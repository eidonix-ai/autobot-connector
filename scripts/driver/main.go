// The acceptance driver uses the same client as the hub CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"autobot-connector/hub"
)

func run(a []string) (int, error) {
	if len(a) < 4 {
		return 1, errors.New("driver SOCKET DONOR run|exec|finish|stop|wait LEASE [ARGS...]")
	}
	c, donor, lease := hub.New(a[0]), a[1], a[3]
	ctx := context.Background()
	var result hub.Result
	var err error
	switch a[2] {
	case "run":
		if len(a) < 5 {
			return 1, errors.New("driver run needs duration")
		}
		duration, err := time.ParseDuration(a[4])
		if err != nil {
			return 1, err
		}
		var id string
		for i := 0; i < 14; i++ {
			id, err = c.Begin(ctx, donor, hub.Manifest{Salvage: a[5:]}, duration, hub.Scratch)
			if err == nil || (!strings.Contains(err.Error(), "donor unavailable") && !strings.Contains(err.Error(), "donor disconnected")) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err == nil {
			fmt.Println(id)
		}
		return 0, err
	case "begin":
		if len(a) != 5 {
			return 1, errors.New("driver begin needs manifest and duration")
		}
		body, err := os.ReadFile(a[3])
		if err != nil {
			return 1, err
		}
		var m hub.Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			return 1, err
		}
		duration, err := time.ParseDuration(a[4])
		if err != nil {
			return 1, err
		}
		id, err := c.Begin(ctx, donor, m, duration, hub.Scratch)
		if err == nil {
			fmt.Println(id)
		}
		return 0, err
	case "exec":
		code := 1
		err := c.Exec(ctx, donor, lease, a[4:], func(e hub.Event) error {
			if ch := e.Chunk; ch != nil {
				var w io.Writer = os.Stdout
				if ch.Stream == "stderr" {
					w = os.Stderr
				}
				_, err := w.Write(ch.Data)
				return err
			}
			if e.Exit.Error != "" {
				return errors.New(e.Exit.Error)
			}
			if e.Exit.Fact.ExitCode != nil {
				code = *e.Exit.Fact.ExitCode
			}
			return nil
		})
		return code, err
	case "finish":
		result, err = c.Finish(ctx, donor, lease)
	case "stop":
		result, err = c.Cancel(ctx, donor, lease)
	case "wait":
		result, err = c.Wait(ctx, donor, lease)
	default:
		return 1, errors.New("unknown driver operation")
	}
	if err != nil {
		return 1, err
	}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return 1, err
	}
	if !result.Verified {
		return 1, errors.New("unverified result")
	}
	return 0, nil
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		log.Print(err)
		code = 1
	}
	os.Exit(code)
}
