package main

import (
	"flag"
	"fmt"
	"log"
)

const Version = "Oclket_v0.0.1"

func main() {
	var (
		install   = flag.Bool("install", false, "install systemd service and exit")
		uninstall = flag.Bool("uninstall", false, "uninstall systemd service and exit")
		version   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	if *install {
		if err := installService(); err != nil {
			log.Fatalf("install: %v", err)
		}
		return
	}

	if *uninstall {
		if err := uninstallService(); err != nil {
			log.Fatalf("uninstall: %v", err)
		}
		return
	}

	config, err := LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	storage := NewStorage()
	master := NewMasterWithConfig(config, storage)

	if err := master.Start(config.ListenAddress); err != nil {
		log.Fatalf("start node: %v", err)
	}
	defer master.Stop()

	for _, peer := range config.Peers {
		if peer != config.ListenAddress {
			master.Connect(peer)
		}
	}

	log.Printf("node started: listen=%s socket=%s", config.ListenAddress, config.SocketPath)
	socket := NewUSocket(config.SocketPath, config.SocketMode)
	if err := socket.Start(master); err != nil {
		log.Fatalf("start local socket: %v", err)
	}
}
