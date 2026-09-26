package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	socketPath         = "/var/run/tailscale-personal.socket"
	companionServiceIP = "100.100.100.101"
	resolverDirectory  = "/etc/resolver"
	resolverHeader     = "# Added by tailscale-companion\n"
)

var servicePrefix = netip.MustParsePrefix(companionServiceIP + "/32")

// Service mode uses a root-owned CLI copy, never a user-writable Homebrew binary.
var cliPath = "/opt/homebrew/opt/tailscale/bin/tailscale"

// Populated from ignored local settings before starting any route management.
var personalTailnet string
var personalCIDR = "100.100.42.0/24" // illustrative default for tests; startup requires settings
var pool = netip.MustParsePrefix(personalCIDR)
var utunName = regexp.MustCompile(`^utun[0-9]+$`)

type commandRunner func(context.Context, string, ...string) ([]byte, error)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type routeEntry struct {
	prefix netip.Prefix
	iface  string
}

type routedService struct {
	Name   string `json:"name"`
	IPv4   string `json:"ipv4"`
	prefix netip.Prefix
}

type daemonSelfStatus struct {
	TailscaleIPs []string
	CapMap       map[string][]json.RawMessage
}

type daemonStatus struct {
	BackendState   string
	CurrentTailnet *struct {
		Name           string
		MagicDNSSuffix string
	}
	Self *daemonSelfStatus
}

func serviceRoutesFromStatus(status daemonStatus, nodePool netip.Prefix) ([]routedService, error) {
	if status.Self == nil {
		return nil, nil
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	var routes []routedService
	seen := make(map[netip.Prefix]string)
	for key, values := range status.Self.CapMap {
		if !strings.HasPrefix(key, "services/") {
			continue
		}
		shortName := strings.TrimPrefix(key, "services/")
		if shortName == "" {
			return nil, fmt.Errorf("invalid empty Tailscale Service capability %q", key)
		}
		wantName := "svc:" + shortName
		for _, raw := range values {
			var capability struct {
				Name  string
				Addrs []string
			}
			if err := json.Unmarshal(raw, &capability); err != nil {
				return nil, fmt.Errorf("decode %s capability: %w", wantName, err)
			}
			if capability.Name != wantName {
				return nil, fmt.Errorf("service capability %q names %q", key, capability.Name)
			}
			for _, value := range capability.Addrs {
				ip, err := netip.ParseAddr(value)
				if err != nil {
					return nil, fmt.Errorf("invalid TailVIP %q for %s", value, wantName)
				}
				if !ip.Is4() {
					continue // This companion deliberately manages IPv4 routes only.
				}
				prefix := netip.PrefixFrom(ip, 32)
				if !cgnat.Contains(ip) || nodePool.Contains(ip) || ip.String() == companionServiceIP {
					return nil, fmt.Errorf("unsafe TailVIP %s for %s", ip, wantName)
				}
				if previous, ok := seen[prefix]; ok && previous != wantName {
					return nil, fmt.Errorf("TailVIP %s is assigned to both %s and %s", ip, previous, wantName)
				}
				seen[prefix] = wantName
			}
		}
	}
	for prefix, name := range seen {
		routes = append(routes, routedService{Name: name, IPv4: prefix.Addr().String(), prefix: prefix})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Name != routes[j].Name {
			return routes[i].Name < routes[j].Name
		}
		return routes[i].IPv4 < routes[j].IPv4
	})
	return routes, nil
}

// netstat abbreviates IPv4 prefixes, e.g. 100.64/10 and 192.168.42.
func parseDestination(s string) (netip.Prefix, error) {
	if s == "default" {
		return netip.MustParsePrefix("0.0.0.0/0"), nil
	}
	address, mask, explicit := strings.Cut(s, "/")
	parts := strings.Split(address, ".")
	if len(parts) > 4 {
		return netip.Prefix{}, fmt.Errorf("invalid route destination %q", s)
	}
	bits := len(parts) * 8
	if explicit {
		var err error
		bits, err = strconv.Atoi(mask)
		if err != nil || bits < 0 || bits > 32 {
			return netip.Prefix{}, fmt.Errorf("invalid route mask %q", s)
		}
	}
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	ip, err := netip.ParseAddr(strings.Join(parts, "."))
	if err != nil || !ip.Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid IPv4 route %q", s)
	}
	return netip.PrefixFrom(ip, bits).Masked(), nil
}

func parseRoutes(out []byte) ([]routeEntry, error) {
	ifaceColumn := -1
	var routes []routeEntry
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "Destination" {
			for i, field := range fields {
				if field == "Netif" {
					ifaceColumn = i
				}
			}
			continue
		}
		if ifaceColumn < 0 {
			continue
		}
		if len(fields) <= ifaceColumn {
			return nil, fmt.Errorf("cannot parse route row: %q", line)
		}
		prefix, err := parseDestination(fields[0])
		if err != nil {
			return nil, err // fail closed rather than overlooking a conflict
		}
		routes = append(routes, routeEntry{prefix, fields[ifaceColumn]})
	}
	if ifaceColumn < 0 {
		return nil, errors.New("netstat output lacks a Netif header")
	}
	return routes, nil
}

func findTunnel(ip netip.Addr) (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var found []string
	for _, iface := range interfaces {
		if !utunName.MatchString(iface.Name) || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			p, err := netip.ParsePrefix(addr.String())
			if err == nil && p.Addr() == ip {
				found = append(found, iface.Name)
			}
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("expected one active utun with %s, found %v", ip, found)
	}
	return found[0], nil
}

type routeManager struct {
	expectedTailnet string
	run             commandRunner
	tunnel          func(netip.Addr) (string, error)
	owned           string // interface shared by routes successfully installed by this process
	ownedPool       bool
	ownedService    bool // companion DNS service route
	ownedVIPs       map[netip.Prefix]bool
	dnsSuffix       string
	resolverDir     string
	logf            func(string, ...any)
}

func newRouteManager() *routeManager {
	return &routeManager{
		run: runCommand, tunnel: findTunnel, logf: log.Printf,
		expectedTailnet: personalTailnet, resolverDir: resolverDirectory,
		ownedVIPs: make(map[netip.Prefix]bool),
	}
}

func (m *routeManager) snapshot(ctx context.Context) ([]routeEntry, error) {
	out, err := m.run(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet")
	if err != nil {
		return nil, err
	}
	return parseRoutes(out)
}

func validMagicDNSSuffix(s string) bool {
	if s == "" || s != strings.ToLower(s) || !strings.HasSuffix(s, ".ts.net") || strings.ContainsAny(s, "/\\:\t\r\n ") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return len(s) <= 253
}

func resolverContents() []byte {
	return []byte(resolverHeader + "nameserver " + companionServiceIP + "\n")
}

func (m *routeManager) resolverPath() (string, error) {
	if m.resolverDir == "" {
		return "", nil
	}
	if !validMagicDNSSuffix(m.dnsSuffix) {
		return "", fmt.Errorf("invalid companion MagicDNS suffix %q", m.dnsSuffix)
	}
	return filepath.Join(m.resolverDir, m.dnsSuffix), nil
}

func (m *routeManager) ensureResolver() error {
	path, err := m.resolverPath()
	if err != nil || path == "" {
		return err
	}
	if info, err := os.Lstat(m.resolverDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(m.resolverDir, 0755); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("resolver path is not a real directory: %s", m.resolverDir)
	} else if m.resolverDir == resolverDirectory {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("resolver directory is not safely root-owned: %s", m.resolverDir)
		}
	}
	want := resolverContents()
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing non-regular resolver file %s", path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(got) != string(want) {
			return fmt.Errorf("refusing to replace unmanaged resolver file %s", path)
		}
		if err := os.Chmod(path, 0644); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := filepath.Join(m.resolverDir, fmt.Sprintf(".tailscale-companion-%d", os.Getpid()))
	fd, err := syscall.Open(tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, writeErr := f.Write(want)
	chmodErr := f.Chmod(0644) // launchd's 0077 umask must not hide status from the menu app
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, chmodErr, syncErr, closeErr); err != nil {
		os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		return fmt.Errorf("install resolver file %s: %w", path, err)
	}
	m.logf("Installed split DNS for %s via %s", m.dnsSuffix, companionServiceIP)
	return nil
}

func (m *routeManager) cleanupResolver() error {
	if m.dnsSuffix == "" {
		return nil
	}
	path, err := m.resolverPath()
	if err != nil || path == "" {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-regular resolver file %s", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(got) != string(resolverContents()) {
		return fmt.Errorf("refusing to remove changed resolver file %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.logf("Removed split DNS for %s", m.dnsSuffix)
	return nil
}

// cleanup never deletes an unmanaged route or a route now pointing elsewhere.
// The kernel has no owner token: replacement with an identical prefix/interface
// is indistinguishable. Do not run other managers for these prefixes concurrently.
func (m *routeManager) cleanup(ctx context.Context) error {
	resolverErr := m.cleanupResolver()
	if m.owned == "" {
		return resolverErr
	}
	routes, err := m.snapshot(ctx)
	if err != nil {
		return errors.Join(resolverErr, err)
	}
	present := make(map[netip.Prefix]bool)
	for _, r := range routes {
		if r.iface == m.owned {
			present[r.prefix] = true
		}
	}
	var errs []error
	remove := func(prefix netip.Prefix) bool {
		if present[prefix] {
			if _, err := m.run(ctx, "/sbin/route", "-n", "delete", "-net", prefix.String(), "-interface", m.owned); err != nil {
				errs = append(errs, err)
				return false
			}
			m.logf("Removed owned route %s via %s", prefix, m.owned)
		}
		return true
	}
	if m.ownedPool && remove(pool) {
		m.ownedPool = false
	}
	if m.ownedService && remove(servicePrefix) {
		m.ownedService = false
	}
	for prefix := range m.ownedVIPs {
		if remove(prefix) {
			delete(m.ownedVIPs, prefix)
		}
	}
	if !m.ownedPool && !m.ownedService && len(m.ownedVIPs) == 0 {
		m.owned = ""
	}
	return errors.Join(append([]error{resolverErr}, errs...)...)
}

func (m *routeManager) desiredRoutes(ctx context.Context) (string, []routedService, error) {
	out, err := m.run(ctx, cliPath, "--socket="+socketPath, "status", "--json")
	if err != nil {
		return "", nil, err
	}
	var status daemonStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return "", nil, err
	}
	if m.expectedTailnet == "" || status.CurrentTailnet == nil || status.CurrentTailnet.Name != m.expectedTailnet {
		return "", nil, errors.New("refusing routes: independent daemon is not in the personal tailnet")
	}
	if !validMagicDNSSuffix(status.CurrentTailnet.MagicDNSSuffix) {
		return "", nil, fmt.Errorf("refusing DNS: invalid MagicDNS suffix %q", status.CurrentTailnet.MagicDNSSuffix)
	}
	m.dnsSuffix = status.CurrentTailnet.MagicDNSSuffix
	if status.BackendState != "Running" {
		return "", nil, fmt.Errorf("waiting for personal login/connection (%s)", status.BackendState)
	}
	if status.Self == nil {
		return "", nil, errors.New("personal daemon has no self node")
	}
	services, err := serviceRoutesFromStatus(status, pool)
	if err != nil {
		return "", nil, err
	}
	for _, s := range status.Self.TailscaleIPs {
		ip, err := netip.ParseAddr(s)
		if err == nil && ip.Is4() {
			if !pool.Contains(ip) {
				return "", nil, fmt.Errorf("personal node address %s is outside %s", ip, pool)
			}
			iface, err := m.tunnel(ip)
			if err == nil && !utunName.MatchString(iface) {
				return "", nil, fmt.Errorf("invalid tunnel name %q", iface)
			}
			return iface, services, err
		}
	}
	return "", nil, errors.New("personal daemon has no IPv4 address")
}

func (m *routeManager) step(ctx context.Context) error {
	iface, services, err := m.desiredRoutes(ctx)
	if err != nil {
		return errors.Join(err, m.cleanup(ctx))
	}
	if m.ownedVIPs == nil {
		m.ownedVIPs = make(map[netip.Prefix]bool)
	}
	if m.owned != "" && m.owned != iface {
		if err := m.cleanup(ctx); err != nil {
			return err
		}
	}
	desiredVIPs := make(map[netip.Prefix]routedService)
	for _, service := range services {
		desiredVIPs[service.prefix] = service
	}
	routes, err := m.snapshot(ctx)
	if err != nil {
		return err
	}
	presentPool, presentService := false, false
	presentVIPs := make(map[netip.Prefix]bool)
	for _, r := range routes {
		if r.prefix == servicePrefix {
			if r.iface != iface {
				return errors.Join(fmt.Errorf("conflict: %s points to %s, not %s", r.prefix, r.iface, iface), m.cleanup(ctx))
			}
			if m.owned != iface || !m.ownedService {
				return fmt.Errorf("%s via %s already exists and is unmanaged; refusing to adopt it", servicePrefix, iface)
			}
			presentService = true
			continue
		}
		_, wantedVIP := desiredVIPs[r.prefix]
		if wantedVIP || m.ownedVIPs[r.prefix] {
			if r.iface != iface {
				return errors.Join(fmt.Errorf("conflict: TailVIP %s points to %s, not %s", r.prefix, r.iface, iface), m.cleanup(ctx))
			}
			if m.owned != iface || !m.ownedVIPs[r.prefix] {
				return fmt.Errorf("TailVIP %s via %s already exists and is unmanaged; refusing to adopt it", r.prefix, iface)
			}
			presentVIPs[r.prefix] = true
			continue
		}
		if r.prefix.Bits() < pool.Bits() || !pool.Contains(r.prefix.Addr()) {
			continue // broad work /10 and default routes remain untouched
		}
		if r.iface != iface {
			return errors.Join(fmt.Errorf("conflict: %s points to %s, not %s", r.prefix, r.iface, iface), m.cleanup(ctx))
		}
		if r.prefix == pool {
			if m.owned != iface || !m.ownedPool {
				return fmt.Errorf("%s via %s already exists and is unmanaged; refusing to adopt it", pool, iface)
			}
			presentPool = true
		}
	}
	if m.ownedPool && !presentPool {
		m.ownedPool = false // a previously owned route may have disappeared with the tunnel
	}
	if m.ownedService && !presentService {
		m.ownedService = false
	}
	for prefix := range m.ownedVIPs {
		if _, wanted := desiredVIPs[prefix]; !wanted {
			if present := presentVIPs[prefix]; present {
				if _, err := m.run(ctx, "/sbin/route", "-n", "delete", "-net", prefix.String(), "-interface", iface); err != nil {
					return errors.Join(err, m.cleanup(ctx))
				}
				m.logf("Removed no-longer-authorized TailVIP route %s via %s", prefix, iface)
			}
			delete(m.ownedVIPs, prefix)
			continue
		}
		if !presentVIPs[prefix] {
			delete(m.ownedVIPs, prefix)
		}
	}
	if !m.ownedPool && !m.ownedService && len(m.ownedVIPs) == 0 {
		m.owned = ""
	}
	for _, target := range []struct {
		prefix  netip.Prefix
		present bool
		owned   *bool
	}{{pool, presentPool, &m.ownedPool}, {servicePrefix, presentService, &m.ownedService}} {
		if target.present {
			continue
		}
		if _, err := m.run(ctx, "/sbin/route", "-n", "add", "-net", target.prefix.String(), "-interface", iface); err != nil {
			return errors.Join(err, m.cleanup(ctx)) // never change or delete a foreign route on EEXIST
		}
		m.owned = iface
		*target.owned = true
		m.logf("Installed %s via %s (personal tailnet verified)", target.prefix, iface)
	}
	for _, service := range services {
		if presentVIPs[service.prefix] {
			continue
		}
		if _, err := m.run(ctx, "/sbin/route", "-n", "add", "-net", service.prefix.String(), "-interface", iface); err != nil {
			return errors.Join(err, m.cleanup(ctx))
		}
		m.owned = iface
		m.ownedVIPs[service.prefix] = true
		m.logf("Installed TailVIP route %s for %s via %s", service.prefix, service.Name, iface)
	}
	if err := m.ensureResolver(); err != nil {
		return errors.Join(err, m.cleanup(ctx))
	}
	return nil
}

func (m *routeManager) watch(ctx context.Context) error {
	defer func() { m.logf("Route monitor stopped") }()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		if ctx.Err() != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return m.cleanup(cleanupCtx)
		}
		err := m.step(ctx)
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message != lastError {
			if message != "" {
				m.logf("Route monitor: %s", message)
			} else {
				m.logf("Personal routing and split DNS are ready")
			}
			lastError = message
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
