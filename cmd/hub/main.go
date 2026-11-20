package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"autobot-connector/hub"
	"autobot-connector/internal/hubserver"
	"autobot-connector/internal/wire"
)

func serve(args []string, socket string) error {
	if len(args) != 5 {
		return errors.New("usage: hub serve LISTEN CERT KEY STATE_DIR WORK_IMAGE")
	}
	h, err := hubserver.New(args[3], args[4])
	if err != nil {
		return err
	}
	local, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer local.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	public, err := net.Listen("tcp", args[0])
	if err != nil {
		return err
	}
	defer public.Close()
	admin := &http.Server{Handler: h.Admin(), MaxHeaderBytes: 8192, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 4 * time.Second}
	remote := &http.Server{Handler: h.WebSocket(), MaxHeaderBytes: 8192, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 4 * time.Second, WriteTimeout: 4 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	failed := make(chan error, 2)
	go func() { failed <- admin.Serve(local) }()
	go func() { failed <- remote.ServeTLS(public, args[1], args[2]) }()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failed:
		return err
	}
}

func run(args []string) (int, error) {
	socket := os.Getenv("HUB_SOCKET")
	if socket == "" {
		socket = "hub.sock"
	}
	if len(args) == 0 {
		return 1, errors.New("usage: hub serve|code|donors|run|revoke")
	}
	if args[0] == "serve" {
		return 0, serve(args[1:], socket)
	}
	client := hub.New(socket)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	switch {
	case len(args) == 1 && args[0] == "code":
		code, err := client.Code(ctx)
		if err != nil {
			return 1, err
		}
		fmt.Println(code)
	case len(args) == 1 && args[0] == "donors":
		donors, err := client.Donors(ctx)
		if err != nil {
			return 1, err
		}
		return 0, json.NewEncoder(os.Stdout).Encode(donors)
	case len(args) == 2 && args[0] == "revoke":
		return 0, client.Revoke(ctx, args[1])
	case len(args) >= 5 && args[0] == "run" && args[3] == "--":
		var manifest hub.Manifest
		data, err := os.ReadFile(args[2])
		if err != nil {
			return 1, err
		}
		if err = wire.Strict(data, &manifest); err != nil {
			return 1, err
		}
		result, err := client.Run(ctx, args[1], manifest, 10*time.Minute, args[4:], func(event hub.Event) error {
			if event.Chunk != nil {
				_, err := fmt.Fprintf(os.Stdout, "%s %s\n", event.Chunk.Stream, strconv.QuoteToGraphic(string(event.Chunk.Data)))
				return err
			}
			return nil
		})
		if result.Lease != "" {
			if printErr := json.NewEncoder(os.Stdout).Encode(result); printErr != nil {
				return 1, printErr
			}
		}
		if err != nil {
			return 1, err
		}
		if !result.Verified || result.Facts.End != "complete" {
			return 1, errors.New("job did not complete with a verified transcript")
		}
		if len(result.Facts.Commands) != 1 || result.Facts.Commands[0].ExitCode == nil {
			return 1, errors.New("command exit unavailable")
		}
		return *result.Facts.Commands[0].ExitCode, nil
	default:
		return 1, errors.New("usage: hub code | donors | run DONOR MANIFEST -- COMMAND... | revoke DONOR")
	}
	return 0, nil
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		log.Printf("%q", err.Error())
		code = 1
	}
	os.Exit(code)
}
