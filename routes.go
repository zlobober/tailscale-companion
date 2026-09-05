package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	socketPath = "/var/run/tailscale-personal.socket"
)

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
	owned           string // interface of the /24 successfully installed by this process
	logf            func(string, ...any)
}

func newRouteManager() *routeManager {
	return &routeManager{run: runCommand, tunnel: findTunnel, logf: log.Printf, expectedTailnet: personalTailnet}
}

func (m *routeManager) snapshot(ctx context.Context) ([]routeEntry, error) {
	out, err := m.run(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet")
	if err != nil {
		return nil, err
	}
	return parseRoutes(out)
}

// cleanup never deletes an unmanaged route or a route now pointing elsewhere.
// The kernel has no owner token: replacement with an identical prefix/interface
// is indistinguishable. Do not run other managers for this prefix concurrently.
func (m *routeManager) cleanup(ctx context.Context) error {
	if m.owned == "" {
		return nil
	}
	routes, err := m.snapshot(ctx)
	if err != nil {
		return err
	}
	present := false
	for _, r := range routes {
		if r.prefix == pool && r.iface == m.owned {
			present = true
		}
	}
	if present {
		if _, err := m.run(ctx, "/sbin/route", "-n", "delete", "-net", personalCIDR, "-interface", m.owned); err != nil {
			return err
		}
		m.logf("Removed owned route %s via %s", personalCIDR, m.owned)
	}
	m.owned = ""
	return nil
}

func (m *routeManager) desiredTunnel(ctx context.Context) (string, error) {
	out, err := m.run(ctx, cliPath, "--socket="+socketPath, "status", "--json")
	if err != nil {
		return "", err
	}
	var status struct {
		BackendState   string
		CurrentTailnet *struct{ Name string }
		Self           *struct{ TailscaleIPs []string }
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", err
	}
	if status.BackendState != "Running" {
		return "", fmt.Errorf("waiting for personal login/connection (%s)", status.BackendState)
	}
	if m.expectedTailnet == "" || status.CurrentTailnet == nil || status.CurrentTailnet.Name != m.expectedTailnet {
		return "", errors.New("refusing routes: independent daemon is not in the personal tailnet")
	}
	if status.Self == nil {
		return "", errors.New("personal daemon has no self node")
	}
	for _, s := range status.Self.TailscaleIPs {
		ip, err := netip.ParseAddr(s)
		if err == nil && ip.Is4() {
			if !pool.Contains(ip) {
				return "", fmt.Errorf("personal node address %s is outside %s", ip, pool)
			}
			iface, err := m.tunnel(ip)
			if err == nil && !utunName.MatchString(iface) {
				return "", fmt.Errorf("invalid tunnel name %q", iface)
			}
			return iface, err
		}
	}
	return "", errors.New("personal daemon has no IPv4 address")
}

func (m *routeManager) step(ctx context.Context) error {
	iface, err := m.desiredTunnel(ctx)
	if err != nil {
		return errors.Join(err, m.cleanup(ctx))
	}
	if m.owned != "" && m.owned != iface {
		if err := m.cleanup(ctx); err != nil {
			return err
		}
	}
	routes, err := m.snapshot(ctx)
	if err != nil {
		return err
	}
	present := false
	for _, r := range routes {
		if r.prefix.Bits() < pool.Bits() || !pool.Contains(r.prefix.Addr()) {
			continue // broad work /10 and default routes remain untouched
		}
		if r.iface != iface {
			return errors.Join(fmt.Errorf("conflict: %s points to %s, not %s", r.prefix, r.iface, iface), m.cleanup(ctx))
		}
		if r.prefix == pool {
			if m.owned != iface {
				return fmt.Errorf("%s via %s already exists and is unmanaged; refusing to adopt it", pool, iface)
			}
			present = true
		}
	}
	if present {
		return nil
	}
	m.owned = "" // a previously owned route may have disappeared with the tunnel
	if _, err := m.run(ctx, "/sbin/route", "-n", "add", "-net", personalCIDR, "-interface", iface); err != nil {
		return err // never use 'change' or delete another process's route on EEXIST
	}
	m.owned = iface
	m.logf("Installed %s via %s (personal tailnet verified)", personalCIDR, iface)
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
				m.logf("Personal routing is ready")
			}
			lastError = message
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
