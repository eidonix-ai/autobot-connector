package inputs

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"autobot-connector/internal/wire"
)

type Bundle struct {
	*os.File
	Size   int64
	Digest string
	dir    string
}

func (b *Bundle) Close() error { return errors.Join(b.File.Close(), os.RemoveAll(b.dir)) }

type capped struct {
	w    io.Writer
	left int64
}

func (c *capped) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, errors.New("input byte cap exceeded")
	}
	n, err := c.w.Write(p)
	c.left -= int64(n)
	return n, err
}

func file(ctx context.Context, httpClient *http.Client, dst string, f wire.File, limit int64) (int64, error) {
	u, err := url.Parse(f.URL)
	if err != nil {
		return 0, errors.New("invalid input URL")
	}
	if err = publicURL(u); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("file host returned HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return 0, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(&capped{io.MultiWriter(out, hash), limit}, resp.Body)
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return 0, err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != f.SHA256 {
		return 0, fmt.Errorf("digest mismatch for %q", f.Path)
	}
	return n, nil
}

func Build(ctx context.Context, httpClient *http.Client, manifest wire.Manifest, scratch int64) (*Bundle, error) {
	if !manifest.Valid() {
		return nil, errors.New("invalid manifest")
	}
	dir, err := os.MkdirTemp("", "inputs-")
	if err != nil {
		return nil, err
	}
	b := &Bundle{dir: dir}
	keep := false
	defer func() {
		if !keep {
			if b.File != nil {
				b.File.Close()
			}
			os.RemoveAll(dir)
		}
	}()
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		return nil, err
	}
	remaining := min(scratch, wire.BundleCap)
	for _, f := range manifest.Files {
		if pathRoot, _, _ := strings.Cut(f.Path, "/"); pathRoot == "source" || pathRoot == ".bundle" {
			return nil, errors.New("reserved input path")
		}
		n, err := file(ctx, httpClient, filepath.Join(tree, f.Path), f, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= n
	}
	if manifest.Source != nil {
		if err := source(ctx, httpClient, filepath.Join(tree, "source"), *manifest.Source); err != nil {
			return nil, err
		}
	}
	b.File, err = os.Create(filepath.Join(dir, "bundle"))
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	capWriter := &capped{io.MultiWriter(b.File, hash), wire.BundleCap}
	gz := gzip.NewWriter(capWriter)
	tw := tar.NewWriter(gz)
	var files int
	var expanded int64
	err = filepath.WalkDir(tree, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == tree {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(tree, name)
		if err != nil || !wire.Path(relative) {
			return errors.New("invalid bundle path")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		// Allow for page rounding on tmpfs.
		if info.Mode().IsRegular() {
			expanded += (info.Size() + 4095) &^ 4095
		}
		if files > wire.FileCap || expanded > min(scratch, wire.BundleCap) {
			return errors.New("expanded bundle exceeds scratch or file cap")
		}
		link := ""
		switch {
		case info.Mode().IsRegular(), info.IsDir():
		case info.Mode()&os.ModeSymlink != 0:
			link, err = os.Readlink(name)
			if err != nil {
				return err
			}
			if !SafeLink(relative, link) {
				return errors.New("source symlink escapes its tree")
			}
		default:
			return errors.New("unsupported input file")
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name, h.Format = relative, tar.FormatPAX
		h.ModTime, h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}, time.Time{}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(name)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, in, info.Size())
		return errors.Join(err, in.Close())
	})
	if err := errors.Join(err, tw.Close(), gz.Close()); err != nil {
		return nil, err
	}
	b.Size, b.Digest = wire.BundleCap-capWriter.left, fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if expanded+b.Size > scratch {
		return nil, errors.New("input staging exceeds scratch cap")
	}
	if manifest.Bundle != "" && manifest.Bundle != b.Digest {
		return nil, errors.New("bundle digest mismatch")
	}
	if _, err := b.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	keep = true
	return b, nil
}

func SafeLink(name, link string) bool {
	if !strings.HasPrefix(name, "source/") || link == "" || len(link) > 255 ||
		strings.HasPrefix(link, "/") || strings.ContainsRune(link, 0) {
		return false
	}
	target := path.Clean(path.Join(path.Dir(name), link))
	return (target == "source" || strings.HasPrefix(target, "source/")) &&
		!strings.Contains("/"+target+"/", "/.git/")
}
