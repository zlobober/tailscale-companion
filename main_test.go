//go:build darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigGuards(t *testing.T) {
	data, err := os.ReadFile("tailscaled.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig("tailscaled.example.json"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"acceptDNS", "acceptRoutes", "exitNode", "advertiseRoutes", "advertiseExitNode", "autoUpdate"} {
		t.Run(field, func(t *testing.T) {
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			delete(cfg, field)
			bad, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateConfig(path); err == nil {
				t.Fatal("accepted missing safety preference")
			}
		})
	}
}
