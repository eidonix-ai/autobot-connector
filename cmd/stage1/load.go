package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"autobot-connector/internal/inputs"
	"autobot-connector/internal/wire"
)

func (r *runner) load(w http.ResponseWriter, req *http.Request) {
	id, digest, size := req.URL.Query().Get("lease"), req.URL.Query().Get("digest"), req.ContentLength
	if !formatSHA(digest) || size < 1 || size > wire.BundleCap {
		http.Error(w, "invalid bundle digest or size", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	j := r.activeLease(id)
	if r.dirty || j == nil || j.Lease != id || j.End != "" || !j.Bundle || j.loading || j.loaded {
		r.mu.Unlock()
		http.Error(w, "lease cannot accept inputs", http.StatusConflict)
		return
	}
	eid, err := r.newExecWithInput(j.Container, []string{"/stage1", "load", digest, strconv.FormatInt(size, 10)})
	if err == nil {
		j.loading = true
	} else {
		j.End = "cancel"
	}
	r.mu.Unlock()
	if err == nil {
		until := minTime(j.Deadline, time.Now().Add(time.Minute))
		http.NewResponseController(w).SetReadDeadline(until)
		err = r.streamInputs(req.Context(), until, eid, req.Body, size)
		r.mu.Lock()
		j.loading = false
		j.loaded = err == nil && j.End == ""
		if err != nil && j.End == "" {
			j.End = "cancel"
		}
		r.mu.Unlock()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (d engine) streamInputs(ctx context.Context, until time.Time, id string, input io.Reader, size int64) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", "/var/run/docker.sock")
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(until)
	req, err := http.NewRequest("POST", "http://docker/v1.55/exec/"+id+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	req.Header.Set("Content-Type", "application/json")
	if err = req.Write(conn); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return fmt.Errorf("Docker stdin upgrade returned HTTP %d", resp.StatusCode)
	}
	resp.Body = io.NopCloser(reader)
	if _, err := io.CopyN(conn, input, size); err != nil {
		return err
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return err
	}
	var detail strings.Builder
	err = readExec(resp, func(_ byte, data []byte) {
		if detail.Len() < 512 {
			detail.Write(data[:min(len(data), 512-detail.Len())])
		}
	})
	if err != nil {
		return err
	}
	code, err := d.exit(id)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("loader exited %d: %s", code, detail.String())
	}
	return nil
}

func load(args []string, in io.Reader) error {
	if len(args) != 2 || !formatSHA(args[0]) {
		return errors.New("invalid loader digest")
	}
	size, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || size <= 0 || size > wire.BundleCap {
		return errors.New("invalid bundle size")
	}
	root, err := os.OpenRoot("/work")
	if err != nil {
		return err
	}
	defer root.Close()
	stash, err := root.OpenFile(".bundle", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer stash.Close()
	if err := root.Remove(".bundle"); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(stash, hash), in, size); err != nil {
		return err
	}
	if fmt.Sprintf("sha256:%x", hash.Sum(nil)) != args[0] {
		return errors.New("loader digest mismatch")
	}
	if _, err := stash.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(stash)
	if err != nil {
		return err
	}
	defer gz.Close()
	tarBytes := &io.LimitedReader{R: gz, N: wire.BundleCap + wire.FileCap*2048}
	tr := tar.NewReader(tarBytes)
	var count int
	var expanded int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
		expanded += (h.Size + 4095) &^ 4095
		if count > wire.FileCap || expanded > wire.BundleCap || h.Size > wire.BundleCap ||
			!wire.Path(h.Name) || strings.ContainsRune(h.Name, 0) || h.Name == ".bundle" {
			return errors.New("invalid or oversized input entry")
		}
		// Parents must already be real directories. In particular, a file can never
		// be written through a symlink staged earlier in the archive.
		for parent := path.Dir(h.Name); parent != "."; parent = path.Dir(parent) {
			fi, err := root.Lstat(parent)
			if err != nil || !fi.IsDir() {
				return errors.New("invalid input parent")
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.Mkdir(h.Name, 0700)
		case tar.TypeReg:
			f, openErr := root.OpenFile(h.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|os.FileMode(h.Mode)&0100)
			if openErr != nil {
				return openErr
			}
			_, err = io.CopyN(f, tr, h.Size)
			err = errors.Join(err, f.Close())
		case tar.TypeSymlink:
			if !inputs.SafeLink(h.Name, h.Linkname) {
				return errors.New("invalid input symlink")
			}
			err = root.Symlink(h.Linkname, h.Name)
		default:
			return errors.New("unsupported input entry")
		}
		if err != nil {
			return err
		}
	}
	if n, err := io.Copy(io.Discard, tarBytes); err != nil || n != 0 || tarBytes.N == 0 {
		return errors.New("trailing or oversized bundle data")
	}
	return nil
}
