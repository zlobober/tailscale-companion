//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppAndAgentPlists(t *testing.T) {
	for name, content := range map[string]string{
		"Info.plist":  infoPlist,
		"Agent.plist": agentPlist("/Users/example/Applications/A & B.app", "/Users/example/Library/Logs/A & B.log"),
	} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if !strings.Contains(infoPlist, "<key>LSUIElement</key><true/>") {
		t.Fatal("app must be menu-bar-only")
	}
	agent := agentPlist("/Users/example/Applications/Tailscale Companion.app", "/Users/example/Library/Logs/menu.log")
	if strings.Contains(agent, "UserName") || strings.Contains(agent, "tailscaled-private") || strings.Contains(agent, "/Library/LaunchDaemons") {
		t.Fatal("menu agent must neither run as root nor launch the daemon directly")
	}
}
