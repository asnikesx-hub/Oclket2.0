package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

const (
	serviceName         = "oclket2"
	servicePath         = "/etc/systemd/system/" + serviceName + ".service"
	configDirectoryMode = 0750
	configFileMode      = 0600
	serviceRestartSec   = 5
	serviceStartLimit   = 10
	serviceFileLimit    = 65536
	serviceRunUser      = "root"
	serviceRunGroup     = "root"
)

var serviceTemplate = `[Unit]
Description={{.ServiceName}} memory storage
After={{.ServiceName}}.target
Wants={{.ServiceName}}.target

[Service]
Type=simple
ExecStart={{.BinaryPath}}
WorkingDirectory={{.WorkDir}}

Restart=always
RestartSec={{.RestartSec}}

StartLimitBurst={{.StartLimitBurst}}

StandardOutput=journal
StandardError=journal
SyslogIdentifier={{.ServiceName}}

User={{.ServiceUser}}
Group={{.ServiceGroup}}

LimitNOFILE={{.FileLimit}}

[Install]
WantedBy=multi-user.target
`

type serviceData struct {
	ServiceName     string
	BinaryPath      string
	WorkDir         string
	RestartSec      int
	StartLimitBurst int
	ServiceUser     string
	ServiceGroup    string
	FileLimit       int
}

func installService() error {
	if err := ensureDefaultConfig(defaultConfigPath); err != nil {
		return fmt.Errorf("create default config: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("evalsymlinks: %w", err)
	}

	workDir := filepath.Dir(exe)

	data := serviceData{
		ServiceName:     serviceName,
		BinaryPath:      exe,
		WorkDir:         workDir,
		RestartSec:      serviceRestartSec,
		StartLimitBurst: serviceStartLimit,
		ServiceUser:     serviceRunUser,
		ServiceGroup:    serviceRunGroup,
		FileLimit:       serviceFileLimit,
	}

	tmpl, err := template.New("service").Parse(serviceTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("execute template: %w", err)
	}

	if err := os.WriteFile(servicePath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write service: %w", err)
	}

	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}

	if err := runCmd("systemctl", "enable", serviceName); err != nil {
		return fmt.Errorf("enable: %w", err)
	}

	if err := runCmd("systemctl", "restart", serviceName); err != nil {
		return fmt.Errorf("restart: %w", err)
	}

	log.Printf("service %s installed and started", serviceName)
	return nil
}

func ensureDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), configDirectoryMode); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, configFileMode)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}

	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(path)
		}
	}()

	if err := file.Chmod(configFileMode); err != nil {
		return err
	}
	password := make([]byte, peerAuthKeySize)
	if _, err := rand.Read(password); err != nil {
		return fmt.Errorf("generate network password: %w", err)
	}
	content := fmt.Sprintf(
		"OCLKET_LISTEN=%s\nOCLKET_PEERS=\nOCLKET_SOCKET=%s\nOCLKET_SOCKET_MODE=%s\nOCLKET_PASSWORD=%s\n",
		defaultListenAddress,
		defaultSocketPath,
		defaultSocketMode,
		hex.EncodeToString(password),
	)
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	removeOnError = false
	return nil
}

func uninstallService() error {
	if err := runCmd("systemctl", "stop", serviceName); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if err := runCmd("systemctl", "disable", serviceName); err != nil {
		return fmt.Errorf("disable service: %w", err)
	}

	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove service: %w", err)
	}

	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}

	log.Printf("service %s removed", serviceName)
	return nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
