// Package hub controls the donor hub through its local Unix socket.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"autobot-connector/internal/hubserver"
	"autobot-connector/internal/wire"
)

type (
	Manifest = wire.Manifest
	Source   = wire.Source
	File     = wire.File
	Donor    = hubserver.Donor
	Event    = hubserver.Event
	Result   = hubserver.Result
)

const Scratch = wire.ScratchCap

type Client struct{ http *http.Client }

func New(socket string) *Client {
	d := &net.Dialer{Timeout: time.Second}
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", socket) },
	}}}
}

func (c *Client) open(ctx context.Context, method, path string, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hub/"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("hub HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return resp, nil
}

func (c *Client) call(ctx context.Context, method, path string, input, output any) error {
	resp, err := c.open(ctx, method, path, input)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(output)
}

func (c *Client) Code(ctx context.Context) (string, error) {
	var out hubserver.Code
	err := c.call(ctx, "POST", "code", nil, &out)
	return out.Code, err
}

func (c *Client) Donors(ctx context.Context) ([]Donor, error) {
	var out []Donor
	err := c.call(ctx, "GET", "donors", nil, &out)
	return out, err
}

func (c *Client) Revoke(ctx context.Context, donor string) error {
	return c.call(ctx, "POST", "revoke", hubserver.Request{Donor: donor}, nil)
}

func (c *Client) Begin(ctx context.Context, donor string, m Manifest, duration time.Duration, scratch int64) (string, error) {
	var out hubserver.Begun
	err := c.call(ctx, "POST", "begin", hubserver.Request{Donor: donor, Manifest: m, Duration: duration, Scratch: scratch}, &out)
	return out.Lease, err
}

func (c *Client) Exec(ctx context.Context, donor, lease string, argv []string, stream func(Event) error) error {
	resp, err := c.open(ctx, "POST", "exec", hubserver.Request{Donor: donor, Lease: lease, Argv: argv})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	d := json.NewDecoder(io.LimitReader(resp.Body, 2*wire.OutputCap))
	for {
		var event Event
		if err := d.Decode(&event); err != nil {
			if err == io.EOF {
				return errors.New("command stream ended before exit")
			}
			return err
		}
		if event.Error != "" {
			return errors.New(event.Error)
		}
		if stream != nil {
			if err := stream(event); err != nil {
				return err
			}
		}
		if event.Exit != nil {
			return nil
		}
	}
}

func (c *Client) Finish(ctx context.Context, donor, lease string) (Result, error) {
	return c.result(ctx, "finish", donor, lease)
}

func (c *Client) Cancel(ctx context.Context, donor, lease string) (Result, error) {
	return c.result(ctx, "cancel", donor, lease)
}

func (c *Client) Wait(ctx context.Context, donor, lease string) (Result, error) {
	return c.result(ctx, "result", donor, lease)
}

func (c *Client) result(ctx context.Context, op, donor, lease string) (Result, error) {
	var out Result
	err := c.call(ctx, "POST", op, hubserver.Request{Donor: donor, Lease: lease}, &out)
	return out, err
}

func (c *Client) Run(ctx context.Context, donor string, manifest Manifest, duration time.Duration, argv []string, stream func(Event) error) (Result, error) {
	id, err := c.Begin(ctx, donor, manifest, duration, Scratch)
	if err != nil {
		return Result{}, err
	}
	if err = c.Exec(ctx, donor, id, argv, stream); err != nil {
		stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result, endErr := c.Cancel(stop, donor, id)
		return result, errors.Join(err, endErr)
	}
	result, err := c.Finish(ctx, donor, id)
	if err != nil {
		// A health stop or deadline may be finalizing while the last exit arrives.
		result, err = c.Wait(ctx, donor, id)
	}
	return result, err
}
