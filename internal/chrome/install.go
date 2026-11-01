// Package chrome installs, launches and talks to a pinned headless Chrome
// whose network access goes through an in-process SSRF-filtering proxy.
package chrome

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Version is the pinned chrome-headless-shell release. Never pin below
// 156.0.8078.70: the shell has no CRLSet and older SPKI blocklists trust
// compromised intermediates. Bump by recomputing every bytes/sha256 from the
// zips listed in Chrome for Testing's known-good-versions-with-downloads.json
// (which carries no checksums).
const Version = "156.0.8078.70"

const downloadHost = "storage.googleapis.com"

type archive struct {
	bytes  int64
	sha256 string
}

var archives = map[string]archive{
	"linux64":     {126_646_931, "a313da592261ff1e342c1e507d315f25b1809006c0508f77ce86151d464d4ccb"},
	"linux-arm64": {126_749_876, "dca11213d2e0b1db52141edd9f92ec423fd52ca25715846790d84fa1d5d16e8a"},
	"mac-arm64":   {103_901_293, "1f848385611f2e58122e68969489712661880fc9a92c9dd20428099f9998ba7b"},
	"mac-x64":     {109_387_968, "ab4cc2949f88439e2241b2712c49268f24982f2d25af87960466cc22bdc2e6e1"},
	"win64":       {126_214_071, "41c6aa97046d05d015b817a95f7b1512c3a1a41b18364f8da46478ae520400b1"},
}

func chromePlatform() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux64"
	case "linux/arm64":
		return "linux-arm64"
	case "darwin/arm64":
		return "mac-arm64"
	case "darwin/amd64":
		return "mac-x64"
	case "windows/amd64", "windows/arm64": // no Windows arm64 build; it runs the x64 one emulated
		return "win64"
	}
	return ""
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "chrome-headless-shell.exe"
	}
	return "chrome-headless-shell"
}

type flight struct {
	done chan struct{}
	bin  string
	err  error
}

var (
	flightMu sync.Mutex
	flights  = map[string]*flight{}
)

// Ensure returns the Chrome binary to launch: $YOAKE_CHROME as-is, else the
// pinned chrome-headless-shell under <stateDir>/chrome/<version>/, installed
// on first use. Concurrent callers share one install, which finishes even if
// the caller that started it gives up. progress (optional) is called from the
// installing goroutine, at most about once per MB.
func Ensure(ctx context.Context, stateDir string, progress func(done, total int64)) (string, error) {
	if p := os.Getenv("YOAKE_CHROME"); p != "" {
		return p, nil
	}
	plat := chromePlatform()
	a, ok := archives[plat]
	if !ok {
		return "", fmt.Errorf("no chrome-headless-shell build for %s/%s; set YOAKE_CHROME to a Chrome binary", runtime.GOOS, runtime.GOARCH)
	}
	chromeDir := filepath.Join(stateDir, "chrome")
	bin := filepath.Join(chromeDir, Version, exeName())
	if installed(bin) {
		return bin, nil
	}

	flightMu.Lock()
	fl, ok := flights[bin]
	if !ok {
		fl = &flight{done: make(chan struct{})}
		flights[bin] = fl
		go func() {
			fl.bin, fl.err = install(context.WithoutCancel(ctx), chromeDir, plat, a, progress)
			flightMu.Lock()
			delete(flights, bin)
			flightMu.Unlock()
			close(fl.done)
		}()
	}
	flightMu.Unlock()

	select {
	case <-fl.done:
		return fl.bin, fl.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func installed(bin string) bool {
	fi, err := os.Stat(bin)
	return err == nil && fi.Mode().IsRegular() && (runtime.GOOS == "windows" || fi.Mode().Perm()&0o111 != 0)
}

func install(ctx context.Context, chromeDir, plat string, a archive, progress func(done, total int64)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	dest := filepath.Join(chromeDir, Version)
	bin := filepath.Join(dest, exeName())
	if installed(bin) {
		return bin, nil
	}
	if err := os.MkdirAll(chromeDir, 0o700); err != nil {
		return "", err
	}
	removeStale(chromeDir)
	// Staging sits beside the destination so publishing is one rename.
	stage, err := os.MkdirTemp(chromeDir, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)

	zipPath := filepath.Join(stage, "download.zip")
	u := fmt.Sprintf("https://%s/chrome-for-testing-public/%s/%s/chrome-headless-shell-%s.zip", downloadHost, Version, plat, plat)
	if err := download(ctx, u, a, zipPath, progress); err != nil {
		return "", err
	}
	unpacked := filepath.Join(stage, "browser")
	if err := extract(zipPath, unpacked, "chrome-headless-shell-"+plat+"/"); err != nil {
		return "", err
	}
	if !installed(filepath.Join(unpacked, exeName())) {
		return "", errors.New("archive does not contain " + exeName())
	}
	if _, err := os.Lstat(dest); err == nil {
		_ = os.RemoveAll(dest) // half-installed leftovers
	}
	if err := os.Rename(unpacked, dest); err != nil {
		if installed(bin) { // another process won the race
			return bin, nil
		}
		return "", err
	}
	return bin, nil
}

func removeStale(chromeDir string) {
	ents, _ := os.ReadDir(chromeDir)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), ".install-") && time.Since(fi.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(chromeDir, e.Name()))
		}
	}
}

// download fetches rawURL into dst, which must not exist, and fails unless it
// is exactly a.bytes long with SHA-256 a.sha256. The stream is capped one byte
// past the pinned size, so an oversized body is cut off, not stored.
func download(ctx context.Context, rawURL string, a archive, dst string, progress func(done, total int64)) error {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true // hash the bytes as published
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" || req.URL.Hostname() != downloadHost {
				return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
	if u, err := url.Parse(rawURL); err != nil || u.Scheme != "https" || u.Hostname() != downloadHost {
		return fmt.Errorf("refusing download from %s", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading chrome: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading chrome: %s", resp.Status)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != a.bytes {
		return errors.New("download size does not match the pinned release")
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	var done, last int64
	n, err := io.Copy(io.MultiWriter(f, h, writerFunc(func(p []byte) {
		done += int64(len(p))
		if progress != nil && done-last >= 1<<20 {
			last = done
			progress(done, a.bytes)
		}
	})), io.LimitReader(resp.Body, a.bytes+1))
	if err != nil {
		return fmt.Errorf("downloading chrome: %w", err)
	}
	if n != a.bytes || hex.EncodeToString(h.Sum(nil)) != a.sha256 {
		return errors.New("download failed its size or SHA-256 check; nothing was installed")
	}
	if progress != nil {
		progress(n, a.bytes)
	}
	return f.Close()
}

type writerFunc func([]byte)

func (w writerFunc) Write(p []byte) (int, error) { w(p); return len(p), nil }

// extract unpacks the entries under prefix into the new directory dst.
// Anything but plain files and directories, or any path that is not local
// to dst, aborts the whole extraction.
func extract(zipPath, dst, prefix string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	unsafe := errors.New("archive contains an unexpected or unsafe entry")
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		local := filepath.FromSlash(rel)
		if !ok || strings.Contains(f.Name, `\`) || f.Flags&1 != 0 ||
			(rel != "" && !filepath.IsLocal(local)) || f.Mode()&os.ModeType&^os.ModeDir != 0 {
			return fmt.Errorf("%w: %q", unsafe, f.Name)
		}
		target := filepath.Join(dst, local)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	perm := f.Mode().Perm() | 0o600
	// O_EXCL: a duplicate entry (or anything pre-existing, symlinks included) fails.
	w, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(w, rc, int64(f.UncompressedSize64)); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
