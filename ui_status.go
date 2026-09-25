//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// UIStatus is the read-only interface used by the unprivileged menu-bar app.
// State describes connectivity AND routing, not just whether a process exists.
type UIStatus struct {
	ServiceInstalled bool   `json:"serviceInstalled"`
	ServiceLoaded    bool   `json:"serviceLoaded"`
	State            string `json:"state"`
	Detail           string `json:"detail"`
	Tailnet          string `json:"tailnet,omitempty"`
	IPv4             string `json:"ipv4,omitempty"`
	Interface        string `json:"interface,omitempty"`
	RouteReady       bool   `json:"routeReady"`
}

type daemonStatus struct {
	BackendState   string
	CurrentTailnet *struct {
		Name           string
		MagicDNSSuffix string
	}
	Self *struct{ TailscaleIPs []string }
}

func trustedServiceFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func describeConnection(s UIStatus, d daemonStatus, cfg companionSettings, iface string, routes []routeEntry) UIStatus {
	s.State = "starting"
	s.Detail = "Waiting for the personal daemon"
	if d.CurrentTailnet != nil {
		s.Tailnet = d.CurrentTailnet.Name
	}
	switch d.BackendState {
	case "NeedsLogin", "NeedsMachineAuth":
		s.State, s.Detail = "needs-login", "Login or device approval required; use the companion CLI"
		return s
	case "Running":
	case "Stopped":
		s.State, s.Detail = "degraded", "Service is loaded but the Tailscale connection is stopped"
		return s
	default:
		return s
	}
	s.State = "degraded"
	if cfg.ExpectedTailnet == "" || s.Tailnet != cfg.ExpectedTailnet {
		s.Detail = "The daemon is not connected to the configured personal tailnet"
		return s
	}
	prefix, err := netip.ParsePrefix(cfg.IPv4Prefix)
	if err != nil {
		s.Detail = "Invalid service routing configuration"
		return s
	}
	if d.Self != nil {
		for _, a := range d.Self.TailscaleIPs {
			ip, err := netip.ParseAddr(a)
			if err == nil && ip.Is4() {
				s.IPv4 = a
				if !prefix.Contains(ip) {
					s.Detail = "Device IP is outside the configured personal pool"
					return s
				}
				break
			}
		}
	}
	if s.IPv4 == "" || !utunName.MatchString(iface) {
		s.Detail = "Connected, waiting for the personal tunnel"
		return s
	}
	s.Interface = iface
	poolReady, serviceReady := false, false
	for _, r := range routes {
		if r.prefix == servicePrefix {
			if r.iface != iface {
				s.Detail = fmt.Sprintf("Routing conflict: %s uses %s", r.prefix, r.iface)
				return s
			}
			serviceReady = true
			continue
		}
		if r.prefix.Bits() >= prefix.Bits() && prefix.Contains(r.prefix.Addr()) && r.iface != iface {
			s.Detail = fmt.Sprintf("Routing conflict: %s uses %s", r.prefix, r.iface)
			return s
		}
		if r.prefix == prefix && r.iface == iface {
			poolReady = true
		}
	}
	if poolReady && serviceReady {
		s.State, s.Detail, s.RouteReady = "connected", "Personal connection, IPv4 routing, and split DNS are ready", true
		return s
	}
	s.Detail = "Connected, waiting for the personal IPv4 and DNS routes"
	return s
}

func collectUIStatus(ctx context.Context) UIStatus {
	s := UIStatus{State: "not-installed", Detail: "Install or update the companion service first"}
	for _, path := range []string{plistPath, installedWrapper, installedDaemon, installedCLI, installedConfig, installedSettings} {
		if !trustedServiceFile(path) {
			return s
		}
	}
	s.ServiceInstalled = true
	_, err := runCommand(ctx, "/bin/launchctl", "print", "system/"+serviceLabel)
	if err != nil {
		// Do not mistake a tooling failure for an unloaded service.
		if strings.Contains(err.Error(), "Could not find service") {
			s.State, s.Detail = "stopped", "Personal service is shut down"
		} else {
			s.State, s.Detail = "unavailable", "Cannot read launchd service status"
		}
		return s
	}
	s.ServiceLoaded = true
	cfg, err := readSettings(installedSettings)
	if err != nil {
		s.State, s.Detail = "degraded", "Cannot read the installed companion settings"
		return s
	}
	out, err := runCommand(ctx, installedCLI, "--socket="+socketPath, "status", "--json")
	if err != nil {
		s.State, s.Detail = "starting", "Service is loaded; waiting for the daemon"
		return s
	}
	var d daemonStatus
	if err := json.Unmarshal(out, &d); err != nil {
		s.State, s.Detail = "degraded", "Cannot decode daemon status"
		return s
	}
	iface := ""
	if d.Self != nil {
		for _, a := range d.Self.TailscaleIPs {
			ip, err := netip.ParseAddr(a)
			if err == nil && ip.Is4() {
				iface, _ = findTunnel(ip)
				break
			}
		}
	}
	out, routeErr := runCommand(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet")
	var routes []routeEntry
	if routeErr == nil {
		routes, routeErr = parseRoutes(out)
	}
	s = describeConnection(s, d, cfg, iface, routes)
	if routeErr != nil && d.BackendState == "Running" {
		s.State, s.Detail, s.RouteReady = "degraded", "Connected; cannot verify the routing table", false
		return s
	}
	if s.RouteReady {
		path := filepath.Join(resolverDirectory, d.CurrentTailnet.MagicDNSSuffix)
		contents, err := os.ReadFile(path)
		if !validMagicDNSSuffix(d.CurrentTailnet.MagicDNSSuffix) || err != nil || string(contents) != string(resolverContents()) || !trustedServiceFile(path) {
			s.State, s.Detail, s.RouteReady = "degraded", "Connected; split DNS is not installed safely", false
		}
	}
	return s
}
