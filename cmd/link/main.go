package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"autobot-connector/internal/link"
	"autobot-connector/internal/wire"
)

const state = "/link-state"

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: link run|enroll|pause|resume|clear|reset-enrollment")
	}
	switch os.Args[1] {
	case "run":
		threshold := 0
		if v := os.Getenv("DONOR_MAX_TEMPERATURE"); v != "" {
			var err error
			threshold, err = strconv.Atoi(v)
			if err != nil {
				return err
			}
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		return link.Run(ctx, link.Config{
			Hub: os.Getenv("DONOR_HUB"), Certificate: "/hub/certificate.pem", State: state, Socket: "/control/runner.sock",
			Image: os.Getenv("WORK_IMAGE"), NVML: "/nvml", TestHub: os.Getenv("DONOR_TEST_HUB") == "1", MaxTemperature: threshold,
		})
	case "enroll":
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 128))
		if err != nil {
			return err
		}
		code := strings.TrimSpace(string(raw))
		if !wire.Hex(code, 32) {
			return errors.New("invalid enrollment code")
		}
		p := filepath.Join(state, "code")
		if err := os.WriteFile(p+".tmp", []byte(code), 0600); err != nil {
			return err
		}
		return os.Rename(p+".tmp", p)
	case "reset-enrollment":
		for _, name := range []string{"key", "code", "last-offer"} {
			if err := os.Remove(filepath.Join(state, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	case "pause", "clear":
		return os.WriteFile(filepath.Join(state, os.Args[1]), nil, 0600)
	case "resume":
		err := os.Remove(filepath.Join(state, "pause"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	default:
		return errors.New("unknown link operation")
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
