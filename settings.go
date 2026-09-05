package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

type companionSettings struct {
	ExpectedTailnet string `json:"expectedTailnet"`
	IPv4Prefix      string `json:"ipv4Prefix"`
}

func readSettings(path string) (companionSettings, error) {
	var cfg companionSettings
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read local settings (copy companion.example.json first): %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.ExpectedTailnet) == "" || strings.TrimSpace(cfg.ExpectedTailnet) != cfg.ExpectedTailnet {
		return cfg, errors.New("expectedTailnet must explicitly name the companion tailnet")
	}
	prefix, err := netip.ParsePrefix(cfg.IPv4Prefix)
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() < cgnat.Bits() || !cgnat.Contains(prefix.Addr()) {
		return cfg, errors.New("ipv4Prefix must be a canonical IPv4 subnet within 100.64.0.0/10")
	}
	for _, reserved := range []string{"100.100.0.0/24", "100.100.100.0/24", "100.115.92.0/23"} {
		if prefix.Overlaps(netip.MustParsePrefix(reserved)) {
			return cfg, fmt.Errorf("ipv4Prefix overlaps Tailscale-reserved range %s", reserved)
		}
	}
	return cfg, nil
}

func applySettings(path string) error {
	cfg, err := readSettings(path)
	if err != nil {
		return err
	}
	personalTailnet, personalCIDR = cfg.ExpectedTailnet, cfg.IPv4Prefix
	pool = netip.MustParsePrefix(cfg.IPv4Prefix)
	return nil
}

func configuredHostname(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var cfg struct{ Hostname string }
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	if strings.TrimSpace(cfg.Hostname) == "" {
		return "", errors.New("set hostname in tailscaled.json before login")
	}
	return cfg.Hostname, nil
}
