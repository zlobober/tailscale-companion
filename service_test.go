//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledPaths(t *testing.T) {
	p := pathsForExecutable(installedWrapper)
	if p.daemon != installedDaemon || p.cli != installedCLI || p.config != installedConfig || p.settings != installedSettings {
		t.Fatalf("incorrect installed paths: %+v", p)
	}
	if strings.Contains(p.cli, "homebrew") || strings.Contains(p.daemon, "/Users/") {
		t.Fatal("root service depends on user-writable binaries")
	}
	local := pathsForExecutable("/Users/example/agent/tailscale-companion/tailscale-companion")
	if local.daemon != "/Users/example/tailscale/dist/tailscaled-private" {
		t.Fatal(local)
	}
}

func TestServicePlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.plist")
	if err := os.WriteFile(path, []byte(servicePlist), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("invalid plist: %s: %v", out, err)
	}
	for _, s := range []string{serviceLabel, installedWrapper, serviceLog, "<key>ExitTimeOut</key><integer>45</integer>", "<key>AbandonProcessGroup</key><false/>"} {
		if !strings.Contains(servicePlist, s) {
			t.Fatalf("missing %s", s)
		}
	}
	if strings.Contains(servicePlist, "/Users/") || strings.Contains(servicePlist, "homebrew") {
		t.Fatal("unsafe service definition")
	}
}

func TestMigrationProcessMatching(t *testing.T) {
	p := pathsForExecutable("/Users/example/agent/tailscale-companion/tailscale-companion")
	out := `100 0 /Users/example/agent/tailscale-companion/tailscale-companion routes
101 0 /Users/example/agent/tailscale-companion/../../tailscale/dist/tailscaled-private --private-no-routes --statedir=/var/lib/tailscale-personal --socket=/var/run/tailscale-personal.socket
102 0 sudo -- /Users/example/agent/tailscale-companion/tailscale-companion routes
103 501 /Users/example/agent/tailscale-companion/tailscale-companion routes
104 0 /Users/example/agent/tailscale-companion/tailscale-companion install-service
105 0 /Applications/Tailscale.app/Contents/MacOS/Tailscale
106 0 /Users/example/tailscale/dist/tailscaled-private --statedir=/var/lib/work --socket=/var/run/work.socket
107 0 /Users/example/tailscale/dist/tailscaled-private --socket=/var/run/tailscale-personal.socket
`
	processes, err := parseOldProcesses(out, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 2 || processes[0] != (oldProcess{100, false}) || processes[1] != (oldProcess{101, true}) {
		t.Fatalf("unsafe process selection: %+v", processes)
	}
}
