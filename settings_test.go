package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, tailnet, prefix string
		valid                 bool
	}{
		{"valid", "personal.example", "100.100.42.0/24", true},
		{"empty identity", "", "100.100.42.0/24", false},
		{"whitespace identity", " personal.example ", "100.100.42.0/24", false},
		{"outside CGNAT", "personal.example", "192.168.1.0/24", false},
		{"host bits", "personal.example", "100.100.42.10/24", false},
		{"IPv6", "personal.example", "fd7a:115c:a1e0::/48", false},
		{"reserved DNS", "personal.example", "100.100.100.0/24", false},
		{"contains reserved", "personal.example", "100.64.0.0/10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "companion.json")
			data, err := json.Marshal(companionSettings{tc.tailnet, tc.prefix})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = readSettings(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	if _, err := readSettings("companion.example.json"); err == nil {
		t.Fatal("template should require an explicit tailnet")
	}
}
