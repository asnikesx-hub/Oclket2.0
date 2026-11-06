package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	valid := Config{
		ListenAddress: ":8445",
		Peers:         []string{"127.0.0.1:8446", "[::1]:8447"},
		SocketPath:    "/tmp/oclket-test.sock",
		SocketMode:    0660,
		Password:      "shared-cluster-secret",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		change func(*Config)
	}{
		{
			name: "invalid listen address",
			change: func(config *Config) {
				config.ListenAddress = "localhost"
			},
		},
		{
			name: "invalid peer port",
			change: func(config *Config) {
				config.Peers = []string{"peer:0"}
			},
		},
		{
			name: "invalid socket path",
			change: func(config *Config) {
				config.SocketPath = strings.Repeat("x", maxUnixSocketPathLen+1)
			},
		},
		{
			name: "invalid socket permissions",
			change: func(config *Config) {
				config.SocketMode = 01000
			},
		},
		{
			name: "socket has no write permission",
			change: func(config *Config) {
				config.SocketMode = 0444
			},
		},
		{
			name: "empty password",
			change: func(config *Config) {
				config.Password = " \t"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.change(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("expected invalid config to be rejected")
			}
		})
	}
}

func TestLoadEnvFileRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oclket.env")
	if err := os.WriteFile(path, []byte("OCLKET_LISTEN=:8445\ninvalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvFile(path); err == nil {
		t.Fatal("expected malformed config line to be rejected")
	}
}
