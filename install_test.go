package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureDefaultConfigCreatesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "oclket", "oclket.env")
	if err := ensureDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var password string
	for _, line := range strings.Split(string(first), "\n") {
		if strings.HasPrefix(line, "OCLKET_PASSWORD=") {
			password = strings.TrimPrefix(line, "OCLKET_PASSWORD=")
			break
		}
	}
	decodedPassword, err := hex.DecodeString(password)
	if err != nil || len(decodedPassword) != peerAuthKeySize {
		t.Fatal("generated config does not contain a valid random network password")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != configFileMode {
			t.Fatalf("config permissions = %#o, want %#o", info.Mode().Perm(), configFileMode)
		}
	}
	if err := ensureDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("existing config was overwritten")
	}
}
