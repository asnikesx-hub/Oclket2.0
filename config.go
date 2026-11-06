package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	defaultConfigPath    = "/etc/oclket/oclket.env"
	defaultListenAddress = ":8445"
	defaultSocketPath    = "/tmp/oclket.sock"
	defaultSocketMode    = "0666"
	maxUnixSocketPathLen = 107
	peerAuthKeySize      = 32
	peerIOTimeout        = 10 * time.Second
	peerConnectRetry     = 5 * time.Second
	peerReconnectDelay   = time.Second
	storageJanitorPeriod = time.Minute
	storageJanitorShards = 8
	storageJanitorBatch  = 5000
)

var supportedConfigKeys = map[string]struct{}{
	"OCLKET_ENV":         {},
	"OCLKET_LISTEN":      {},
	"OCLKET_PEERS":       {},
	"OCLKET_SOCKET":      {},
	"OCLKET_SOCKET_MODE": {},
	"OCLKET_PASSWORD":    {},
}

type Config struct {
	ListenAddress string
	Peers         []string
	SocketPath    string
	SocketMode    os.FileMode
	Password      string
}

func LoadConfig() (Config, error) {
	configPath := envOrDefault("OCLKET_ENV", defaultConfigPath)
	if err := LoadEnvFile(configPath); err != nil {
		return Config{}, fmt.Errorf("load config %q: %w", configPath, err)
	}

	socketMode, err := parseSocketMode(envOrDefault("OCLKET_SOCKET_MODE", defaultSocketMode))
	if err != nil {
		return Config{}, fmt.Errorf("OCLKET_SOCKET_MODE: %w", err)
	}

	var peers []string
	for _, peer := range strings.Split(os.Getenv("OCLKET_PEERS"), ",") {
		peer = strings.TrimSpace(peer)
		if peer != "" {
			peers = append(peers, peer)
		}
	}

	config := Config{
		ListenAddress: envOrDefault("OCLKET_LISTEN", defaultListenAddress),
		Peers:         peers,
		SocketPath:    envOrDefault("OCLKET_SOCKET", defaultSocketPath),
		SocketMode:    socketMode,
		Password:      envOrDefault("OCLKET_PASSWORD", "nopassword"),
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if err := validateTCPAddress("OCLKET_LISTEN", c.ListenAddress, true, true); err != nil {
		return err
	}
	for _, peer := range c.Peers {
		if err := validateTCPAddress("OCLKET_PEERS", peer, false, false); err != nil {
			return err
		}
	}
	if c.SocketPath == "" || len(c.SocketPath) > maxUnixSocketPathLen ||
		strings.IndexByte(c.SocketPath, 0) >= 0 {
		return fmt.Errorf("OCLKET_SOCKET must be a valid Unix socket path of at most %d bytes", maxUnixSocketPathLen)
	}
	if c.SocketMode&^os.FileMode(0777) != 0 {
		return fmt.Errorf("OCLKET_SOCKET_MODE must contain only octal permission bits (0000-0777)")
	}
	if c.SocketMode.Perm()&0222 == 0 {
		return fmt.Errorf("OCLKET_SOCKET_MODE must grant write access to at least one user")
	}
	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("OCLKET_PASSWORD is required")
	}
	return nil
}

func validateTCPAddress(name, address string, allowEmptyHost, allowZeroPort bool) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address: %w", name, err)
	}
	if host == "" && !allowEmptyHost {
		return fmt.Errorf("%s contains an address with an empty host", name)
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil || (port == 0 && !allowZeroPort) {
		return fmt.Errorf("%s contains an invalid TCP port", name)
	}
	return nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if strings.HasPrefix(key, "OCLKET_") {
			if _, supported := supportedConfigKeys[key]; !supported {
				return fmt.Errorf("%s:%d: unsupported setting %s", path, lineNumber, key)
			}
		}
		if len(value) > 0 && (value[0] == '"' || value[0] == '\'') {
			quote := value[0]
			if len(value) < 2 || value[len(value)-1] != quote {
				return fmt.Errorf("%s:%d: unmatched quote", path, lineNumber)
			}
			value = value[1 : len(value)-1]
		}

		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("%s:%d: set %s: %w", path, lineNumber, key, err)
			}
		}
	}
	return scanner.Err()
}
