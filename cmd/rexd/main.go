package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"rex/config"
	"rex/internal/daemon"
)

func main() {
	log.SetFlags(log.Ltime)

	socketPath := daemon.SocketPath()
	cfgPath := config.DefaultPath()

	// Remove socket on clean exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Remove(socketPath)
		os.Exit(0)
	}()

	srv := daemon.NewServer(socketPath, cfgPath)
	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "rexd: %v\n", err)
		os.Exit(1)
	}
}
