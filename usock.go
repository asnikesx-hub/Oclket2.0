package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type USocket struct {
	path string
	mode os.FileMode
}

func NewUSocket(path string, mode os.FileMode) *USocket {
	return &USocket{
		path: path,
		mode: mode.Perm(),
	}
}

func parseSocketMode(value string) (os.FileMode, error) {
	mode, err := strconv.ParseUint(value, 8, 9)
	if err != nil {
		return 0, fmt.Errorf("expected octal permissions such as 0666: %w", err)
	}
	return os.FileMode(mode), nil
}

func (us *USocket) Start(master *Master) error {
	if info, err := os.Lstat(us.path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("local socket path exists and is not a socket: %s", us.path)
		}
		if conn, err := net.DialTimeout("unix", us.path, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			return fmt.Errorf("local socket is already in use: %s", us.path)
		}
		if err := os.Remove(us.path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", us.path)
	if err != nil {
		return err
	}

	defer listener.Close()
	defer os.Remove(us.path)
	if err := os.Chmod(us.path, us.mode); err != nil {
		return fmt.Errorf("set local socket permissions: %w", err)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		client := NewClient(conn)
		go master.handleLocal(client)
	}
}
