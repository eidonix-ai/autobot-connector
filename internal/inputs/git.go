package inputs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"autobot-connector/internal/wire"
)

var repoPath = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\.git$`)

func source(ctx context.Context, client *http.Client, dir string, src wire.Source) error {
	origin, err := url.Parse(src.URL)
	if err != nil || publicURL(origin) != nil || origin.Host != "github.com" ||
		!repoPath.MatchString(origin.Path) || origin.RawPath != "" || origin.RawQuery != "" {
		return errors.New("invalid GitHub repository URL")
	}
	localhost, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		return err
	}
	token := "/" + wire.ID()
	var gate sync.Mutex
	remaining := int64(wire.BundleCap)
	srv := http.Server{ReadHeaderTimeout: 2 * time.Second, MaxHeaderBytes: 8192, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := *origin
		switch {
		case r.Method == "GET" && r.URL.Path == token+"/info/refs" && r.URL.RawQuery == "service=git-upload-pack":
			u.Path += "/info/refs"
			u.RawQuery = r.URL.RawQuery
		case r.Method == "POST" && r.URL.Path == token+"/git-upload-pack" && r.URL.RawQuery == "":
			u.Path += "/git-upload-pack"
		default:
			http.NotFound(w, r)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), http.MaxBytesReader(w, r.Body, 65536))
		if err == nil {
			req.ContentLength = r.ContentLength
			if r.Method == "GET" {
				req.Body = nil
			}
			req.Header.Set("Git-Protocol", r.Header.Get("Git-Protocol"))
			req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
			var resp *http.Response
			resp, err = client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
				w.WriteHeader(resp.StatusCode)
				gate.Lock()
				n, err := io.Copy(&capped{w, remaining}, resp.Body)
				remaining -= n
				gate.Unlock()
				if err != nil {
					panic(http.ErrAbortHandler)
				}
				return
			}
		}
		http.Error(w, "GitHub request failed", http.StatusBadGateway)
	})}
	// Git reaches only this adapter. The adapter owns the fixed origin and the checked TLS dial.
	go srv.Serve(localhost)
	defer srv.Close()
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{
			"-c", "core.hooksPath=/dev/null", "-c", "core.logAllRefUpdates=false", "-c", "transfer.fsckObjects=true",
			"-c", "http.followRedirects=false", "-c", "protocol.file.allow=never", "-C", dir,
		}, args...)...)
		cmd.WaitDelay = 2 * time.Second
		cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %.512s", args[0], err, strings.ReplaceAll(string(out), token, "/git"))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("init", "--quiet", "--template="); err != nil {
		return err
	}
	if _, err := git("fetch", "--quiet", "--no-tags", "--depth=1", "http://"+localhost.Addr().String()+token, src.Commit); err != nil {
		return err
	}
	head, err := git("rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil || head != src.Commit {
		return errors.New("GitHub commit does not match pin")
	}
	if _, err := git("fsck", "--strict", "--no-reflogs", head); err != nil {
		return err
	}
	tree, err := git("ls-tree", "-r", "-z", head)
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(tree, "\x00") {
		if strings.HasPrefix(line, "160000 ") {
			return errors.New("source has unpinned submodules")
		}
	}
	if _, err := git("checkout", "--quiet", "--detach", head); err != nil {
		return err
	}
	// Transient fetch metadata includes the adapter address and the one-use path.
	return os.Remove(dir + "/.git/FETCH_HEAD")
}
