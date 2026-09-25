//go:build darwin

package main

import (
	"encoding/json"
	"net/netip"
	"testing"
)

func TestUIConnectionStatus(t *testing.T) {
	var d daemonStatus
	if err := json.Unmarshal([]byte(`{"BackendState":"Running","CurrentTailnet":{"Name":"personal.example","MagicDNSSuffix":"mau-newton.ts.net"},"Self":{"TailscaleIPs":["100.100.42.10"]}}`), &d); err != nil {
		t.Fatal(err)
	}
	cfg := companionSettings{"personal.example", "100.100.42.0/24"}
	base := UIStatus{ServiceInstalled: true, ServiceLoaded: true}
	routes := []routeEntry{
		{netip.MustParsePrefix("100.64.0.0/10"), "utun7"},
		{netip.MustParsePrefix(cfg.IPv4Prefix), "utun8"},
		{servicePrefix, "utun8"},
	}
	got := describeConnection(base, d, cfg, "utun8", routes)
	if got.State != "connected" || !got.RouteReady || got.IPv4 != "100.100.42.10" {
		t.Fatal(got)
	}
	got = describeConnection(base, d, cfg, "utun8", routes[:1])
	if got.State != "degraded" || got.RouteReady {
		t.Fatal("missing route shown as connected", got)
	}
	routes = append(routes, routeEntry{netip.MustParsePrefix("100.100.42.42/32"), "utun7"})
	got = describeConnection(base, d, cfg, "utun8", routes)
	if got.State != "degraded" || got.RouteReady {
		t.Fatal("conflicting route shown as connected", got)
	}
	d.CurrentTailnet.Name = "work.example"
	got = describeConnection(base, d, cfg, "utun8", routes[:3])
	if got.State != "degraded" || got.RouteReady {
		t.Fatal("wrong tailnet shown as connected", got)
	}
	d.BackendState = "NeedsLogin"
	got = describeConnection(base, d, cfg, "", nil)
	if got.State != "needs-login" {
		t.Fatal(got)
	}
	d.BackendState = "Stopped"
	if got := describeConnection(base, d, cfg, "", nil); got.State != "degraded" {
		t.Fatal(got)
	}
}
