package codespace

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
)

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// findSSH locates the OpenSSH client that gh codespace ssh drives.
func findSSH() (string, error) {
	if p, err := exec.LookPath(exeName("ssh")); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		p := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", "ssh.exe")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", errors.New(`the OpenSSH client is not installed: Settings > Apps > Optional features > Add "OpenSSH Client", then try again`)
	}
	return "", errors.New("the OpenSSH client (ssh) is not installed")
}

// findBinary looks for gh in the configured path, the bundle directory,
// the data directory and PATH.
func (m *Manager) findBinary(cfg config.Codespace) (string, error) {
	name := exeName("gh")
	var candidates []string
	if cfg.BinaryPath != "" {
		candidates = append(candidates, cfg.BinaryPath)
	}
	candidates = append(candidates,
		filepath.Join(m.baseDir, "third_party", "gh", name),
		filepath.Join(m.baseDir, name),
		filepath.Join(m.dataDir, "bin", name),
	)
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", errors.New("gh not found")
}

// releaseSuffix is the tail of the gh release asset name for this platform.
func releaseSuffix() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64", "windows/386", "windows/arm64":
		return "_windows_" + runtime.GOARCH + ".zip", nil
	case "linux/amd64", "linux/arm64", "linux/386":
		return "_linux_" + runtime.GOARCH + ".tar.gz", nil
	case "darwin/amd64", "darwin/arm64":
		return "_macOS_" + runtime.GOARCH + ".zip", nil
	}
	return "", fmt.Errorf("no automatic GitHub CLI download for %s/%s; install gh manually", runtime.GOOS, runtime.GOARCH)
}

// download fetches the latest GitHub CLI release into the data directory.
func (m *Manager) download(ctx context.Context) (string, error) {
	suffix, err := releaseSuffix()
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/cli/cli/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vmserver")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up GitHub CLI release: %w", err)
	}
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	err = json.NewDecoder(resp.Body).Decode(&rel)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("look up GitHub CLI release: HTTP %d", resp.StatusCode)
	}
	assetURL := ""
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, "gh_") && strings.HasSuffix(a.Name, suffix) {
			assetURL = a.URL
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("no GitHub CLI release asset ending in %s", suffix)
	}

	m.log.Info("downloading GitHub CLI", "url", assetURL)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download GitHub CLI: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download GitHub CLI: HTTP %d", resp.StatusCode)
	}
	binDir := filepath.Join(m.dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(binDir, "gh-*.download")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if _, err := io.Copy(archive, resp.Body); err != nil {
		return "", fmt.Errorf("download GitHub CLI: %w", err)
	}

	dest := filepath.Join(binDir, exeName("gh"))
	if strings.HasSuffix(suffix, ".zip") {
		err = extractZip(archive, dest)
	} else {
		_, _ = archive.Seek(0, io.SeekStart)
		err = extractTarGz(archive, dest)
	}
	if err != nil {
		return "", fmt.Errorf("unpack GitHub CLI: %w", err)
	}
	return dest, nil
}

// isGHEntry matches the executable inside a release archive (…/bin/gh).
func isGHEntry(name string) bool {
	return path.Base(name) == exeName("gh") && path.Base(path.Dir(name)) == "bin"
}

func extractZip(f *os.File, dest string) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return err
	}
	for _, e := range zr.File {
		if !isGHEntry(e.Name) {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeExecutable(rc, dest)
	}
	return errors.New("gh executable not found in archive")
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return errors.New("gh executable not found in archive")
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && isGHEntry(h.Name) {
			return writeExecutable(tr, dest)
		}
	}
}

func writeExecutable(r io.Reader, dest string) error {
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
