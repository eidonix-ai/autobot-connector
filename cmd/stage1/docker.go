package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const leaseLabel = "org.flare.donor.lease"

type engine struct{ *http.Client }

func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
}

type responseError struct {
	status int
	body   string
}

func (e *responseError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

func missing(err error) bool {
	var e *responseError
	return errors.As(err, &e) && e.status == http.StatusNotFound
}

func request(ctx context.Context, client *http.Client, method, path string, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return nil, err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, &responseError{resp.StatusCode, string(data)}
	}
	return resp, nil
}

func (d engine) call(method, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	resp, err := request(ctx, d.Client, method, "/v1.55"+path, input)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(output)
	}
	return nil
}

type container struct {
	ID, Image, AppArmorProfile string
	State                      struct {
		Running, OOMKilled bool
		Pid                int
	}
}

func (d engine) inspect(id string) (container, error) {
	var c container
	err := d.call("GET", "/containers/"+id+"/json", nil, &c)
	return c, err
}

func (d engine) labelled() ([]struct {
	ID     string
	Labels map[string]string
}, error) {
	var list []struct {
		ID     string
		Labels map[string]string
	}
	filter := url.QueryEscape(fmt.Sprintf(`{"label":[%q]}`, leaseLabel))
	err := d.call("GET", "/containers/json?all=1&filters="+filter, nil, &list)
	return list, err
}

func (d engine) remove(id string) error {
	err := d.call("DELETE", "/containers/"+id+"?force=1&v=1", nil, nil)
	if missing(err) {
		return nil
	}
	return err
}

func (d engine) newExec(id string, argv []string) (string, error) {
	return d.createExec(id, argv, false)
}

func (d engine) newExecWithInput(id string, argv []string) (string, error) {
	return d.createExec(id, argv, true)
}

func (d engine) createExec(id string, argv []string, input bool) (string, error) {
	var result struct{ ID string }
	err := d.call("POST", "/containers/"+id+"/exec", map[string]any{
		"Cmd": argv, "User": "1000:1000", "WorkingDir": "/work",
		"AttachStdout": true, "AttachStderr": true, "AttachStdin": input,
	}, &result)
	return result.ID, err
}

func (d engine) openExec(ctx context.Context, id string) (*http.Response, error) {
	return request(ctx, d.Client, "POST", "/v1.55/exec/"+id+"/start", map[string]bool{"Detach": false, "Tty": false})
}

func readExec(resp *http.Response, emit func(byte, []byte)) error {
	defer resp.Body.Close()
	var header [8]byte
	buf := make([]byte, 32<<10)
	for {
		if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if header[0] != 1 && header[0] != 2 {
			return fmt.Errorf("Docker exec stream %d", header[0])
		}
		for left := int64(binary.BigEndian.Uint32(header[4:])); left > 0; {
			n := min(left, int64(len(buf)))
			if _, err := io.ReadFull(resp.Body, buf[:n]); err != nil {
				return err
			}
			emit(header[0], buf[:n])
			left -= n
		}
	}
}

func (d engine) exit(id string) (int, error) {
	var result struct {
		ExitCode *int
		Running  bool
	}
	err := d.call("GET", "/exec/"+id+"/json", nil, &result)
	if err != nil {
		return 0, err
	}
	if result.Running || result.ExitCode == nil {
		return 0, errors.New("command exit is unavailable")
	}
	return *result.ExitCode, nil
}

func (d engine) fixed(ctx context.Context, container string, argv ...string) ([]byte, error) {
	id, err := d.newExec(container, argv)
	if err != nil {
		return nil, err
	}
	var out, stderr bytes.Buffer
	overflow := false
	resp, err := d.openExec(ctx, id)
	if err != nil {
		return nil, err
	}
	err = readExec(resp, func(stream byte, data []byte) {
		dst := &out
		if stream == 2 {
			dst = &stderr
		}
		if dst.Len()+len(data) > salvageCap {
			overflow = true
			return
		}
		dst.Write(data)
	})
	if err != nil {
		return nil, err
	}
	if overflow {
		return nil, errors.New("fixed command exceeded output cap")
	}
	code, err := d.exit(id)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("fixed command exited %d: %s", code, &stderr)
	}
	return out.Bytes(), nil
}
