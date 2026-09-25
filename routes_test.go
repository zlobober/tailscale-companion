package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSystem struct {
	state, tailnet, ip, iface string
	routes                    []routeEntry
	actions                   []string
	failAdd, failDelete       bool
}

func newFake() (*fakeSystem, *routeManager) {
	f := &fakeSystem{
		state: "Running", tailnet: "personal.example", ip: "100.100.42.10", iface: "utun8",
		routes: []routeEntry{
			{netip.MustParsePrefix("0.0.0.0/0"), "en0"},
			{netip.MustParsePrefix("100.64.0.0/10"), "utun7"},
			{netip.MustParsePrefix("100.100.42.10/32"), "utun8"},
		},
	}
	m := &routeManager{
		expectedTailnet: "personal.example",
		run:             f.run,
		tunnel: func(ip netip.Addr) (string, error) {
			if ip.String() != f.ip {
				return "", errors.New("wrong IP")
			}
			return f.iface, nil
		},
		logf: func(string, ...any) {},
	}
	return f, m
}

func (f *fakeSystem) run(_ context.Context, cmd string, args ...string) ([]byte, error) {
	switch cmd {
	case cliPath:
		if strings.Join(args, " ") != "--socket="+socketPath+" status --json" {
			return nil, errors.New("wrong CLI invocation")
		}
		return json.Marshal(map[string]any{
			"BackendState": f.state,
			"CurrentTailnet": map[string]string{
				"Name": f.tailnet, "MagicDNSSuffix": "mau-newton.ts.net",
			},
			"Self": map[string]any{"TailscaleIPs": []string{f.ip}},
		})
	case "/usr/sbin/netstat":
		out := "Routing tables\n\nInternet:\nDestination Gateway Flags Netif Expire\n"
		for _, r := range f.routes {
			out += fmt.Sprintf("%s link#1 UCS %s\n", r.prefix, r.iface)
		}
		return []byte(out), nil
	case "/sbin/route":
		if len(args) != 6 || args[0] != "-n" || args[2] != "-net" || args[4] != "-interface" {
			return nil, errors.New("unsafe route invocation")
		}
		prefix, err := netip.ParsePrefix(args[3])
		if err != nil || (prefix != pool && prefix != servicePrefix) {
			return nil, errors.New("unsafe route prefix")
		}
		f.actions = append(f.actions, args[1]+" "+prefix.String()+" "+args[5])
		switch args[1] {
		case "add":
			if f.failAdd {
				return nil, errors.New("add failed")
			}
			f.routes = append(f.routes, routeEntry{prefix, args[5]})
		case "delete":
			if f.failDelete {
				return nil, errors.New("delete failed")
			}
			for i, r := range f.routes {
				if r.prefix == prefix && r.iface == args[5] {
					f.routes = append(f.routes[:i], f.routes[i+1:]...)
					break
				}
			}
		default:
			return nil, errors.New("unexpected route operation")
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s", cmd)
}

func TestRouteLifecycle(t *testing.T) {
	f, m := newFake()
	ctx := context.Background()
	for range 2 {
		if err := m.step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.actions, ","); got != "add 100.100.42.0/24 utun8,add 100.100.100.101/32 utun8" {
		t.Fatal(got)
	}
	// Tunnel replacement: remove our previous route, then add on the discovered one.
	f.iface = "utun10"
	f.routes[2].iface = "utun10"
	if err := m.step(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.actions, ","); got != "add 100.100.42.0/24 utun8,add 100.100.100.101/32 utun8,delete 100.100.42.0/24 utun8,delete 100.100.100.101/32 utun8,add 100.100.42.0/24 utun10,add 100.100.100.101/32 utun10" {
		t.Fatal(got)
	}
	// Login loss removes the route but never touches work routes.
	f.state = "NeedsLogin"
	if err := m.step(ctx); err == nil {
		t.Fatal("expected waiting status")
	}
	if m.owned != "" || len(f.routes) != 3 || m.ownedPool || m.ownedService {
		t.Fatal("cleanup failed")
	}
	if f.routes[1].iface != "utun7" {
		t.Fatal("work route changed")
	}
	f.state = "Running"
	if err := m.step(ctx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.watch(ctx); err != nil {
		t.Fatal(err)
	}
	if m.owned != "" || len(f.routes) != 3 || m.ownedPool || m.ownedService {
		t.Fatal("signal cleanup failed")
	}
}

func TestRouteConflicts(t *testing.T) {
	for _, tc := range []struct{ name, prefix, iface string }{
		{"foreign aggregate", personalCIDR, "utun7"},
		{"foreign host", "100.100.42.42/32", "utun7"},
		{"foreign subprefix", "100.100.42.128/25", "utun7"},
		{"unmanaged same interface", personalCIDR, "utun8"},
		{"foreign service route", companionServiceIP + "/32", "utun7"},
		{"unmanaged service route", companionServiceIP + "/32", "utun8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, m := newFake()
			f.routes = append(f.routes, routeEntry{netip.MustParsePrefix(tc.prefix), tc.iface})
			if err := m.step(context.Background()); err == nil {
				t.Fatal("conflict accepted")
			}
			if len(f.actions) != 0 || m.owned != "" {
				t.Fatal("modified unmanaged routes")
			}
		})
	}
}

func TestNewConflictRemovesOnlyOwnedRoute(t *testing.T) {
	f, m := newFake()
	if err := m.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.routes = append(f.routes, routeEntry{netip.MustParsePrefix("100.100.42.42/32"), "utun7"})
	if err := m.step(context.Background()); err == nil {
		t.Fatal("new conflict ignored")
	}
	if m.owned != "" || f.routes[len(f.routes)-1].iface != "utun7" {
		t.Fatal("wrong cleanup")
	}
}

func TestRouteIdentityGuards(t *testing.T) {
	for _, change := range []func(*fakeSystem){
		func(f *fakeSystem) { f.tailnet = "work.example" },
		func(f *fakeSystem) { f.ip = "100.80.1.2" },
		func(f *fakeSystem) { f.iface = "en0" },
		func(f *fakeSystem) { f.state = "Stopped" },
	} {
		f, m := newFake()
		change(f)
		if err := m.step(context.Background()); err == nil {
			t.Fatal("unsafe state accepted")
		}
		if len(f.actions) != 0 {
			t.Fatal("route changed")
		}
	}
}

func TestRouteOwnershipAndFailures(t *testing.T) {
	t.Run("failed add not owned", func(t *testing.T) {
		f, m := newFake()
		f.failAdd = true
		if err := m.step(context.Background()); err == nil {
			t.Fatal("missing error")
		}
		if m.owned != "" {
			t.Fatal("claimed failed route")
		}
	})
	t.Run("failed cleanup retried", func(t *testing.T) {
		f, m := newFake()
		if err := m.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.failDelete = true
		if err := m.cleanup(context.Background()); err == nil || m.owned == "" {
			t.Fatal("lost ownership")
		}
		f.failDelete = false
		if err := m.cleanup(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("foreign replacement preserved", func(t *testing.T) {
		f, m := newFake()
		if err := m.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.routes[len(f.routes)-1].iface = "utun7"
		if err := m.cleanup(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(f.actions, ","); got != "add 100.100.42.0/24 utun8,add 100.100.100.101/32 utun8,delete 100.100.42.0/24 utun8" {
			t.Fatal("deleted foreign replacement", f.actions)
		}
	})
	t.Run("missing route restored", func(t *testing.T) {
		f, m := newFake()
		if err := m.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.routes = f.routes[:len(f.routes)-1]
		if err := m.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Join(f.actions, ",") != "add 100.100.42.0/24 utun8,add 100.100.100.101/32 utun8,add 100.100.100.101/32 utun8" {
			t.Fatal(f.actions)
		}
	})
}

func TestSplitDNSLifecycle(t *testing.T) {
	dir := t.TempDir()
	m := &routeManager{resolverDir: dir, dnsSuffix: "mau-newton.ts.net", logf: func(string, ...any) {}}
	path := filepath.Join(dir, m.dnsSuffix)
	if err := m.ensureResolver(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(resolverContents()) {
		t.Fatalf("resolver contents %q: %v", got, err)
	}
	if err := m.ensureResolver(); err != nil {
		t.Fatal("idempotent install failed:", err)
	}
	if err := m.cleanupResolver(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resolver file remains:", err)
	}
}

func TestSplitDNSRefusesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	m := &routeManager{resolverDir: dir, dnsSuffix: "mau-newton.ts.net", logf: func(string, ...any) {}}
	path := filepath.Join(dir, m.dnsSuffix)
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureResolver(); err == nil {
		t.Fatal("replaced foreign resolver file")
	}
	if err := m.cleanupResolver(); err == nil {
		t.Fatal("removed foreign resolver file")
	}
	if got, _ := os.ReadFile(path); string(got) != "nameserver 192.0.2.1\n" {
		t.Fatal("foreign resolver file changed")
	}
	m.dnsSuffix = "../bad.ts.net"
	if err := m.ensureResolver(); err == nil {
		t.Fatal("accepted unsafe suffix")
	}
}

func TestParseRoutes(t *testing.T) {
	out := []byte("Routing tables\n\nInternet:\nDestination Gateway Flags Netif Expire\ndefault 192.168.42.1 UGScg en0\n100.64/10 link#28 UCS utun7\n100.100.42/24 link#29 USc utun8\n100.100.42.10 100.100.42.10 UH utun8\n192.168.42 link#14 UCS en0\n")
	routes, err := parseRoutes(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0.0.0.0/0", "100.64.0.0/10", personalCIDR, "100.100.42.10/32", "192.168.42.0/24"}
	if len(routes) != len(want) {
		t.Fatal(routes)
	}
	for i, r := range routes {
		if r.prefix.String() != want[i] {
			t.Fatalf("%v != %s", r, want[i])
		}
	}
	for _, invalid := range []string{"garbage", "Destination Gateway Flags Netif\nbad link#1 UCS utun8", "Destination Gateway Flags Netif\n100.1/99 link#1 UCS utun8"} {
		if _, err := parseRoutes([]byte(invalid)); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
