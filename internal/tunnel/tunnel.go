// Package tunnel runs cloudflared so the console is reachable on a public
// HTTPS URL without opening router ports. Two modes are supported:
//
//   - quick: `cloudflared tunnel --url ...` gives a random
//     https://<words>.trycloudflare.com URL, no account required.
//   - named: `cloudflared tunnel run --token ...` connects a tunnel created in
//     the Cloudflare Zero Trust dashboard, giving a stable hostname.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
)

type State string

const (
	StateDisabled    State = "disabled"
	StateDownloading State = "downloading"
	StateStarting    State = "starting"
	StateConnected   State = "connected"
	StateError       State = "error"
	StateStopped     State = "stopped"
)

type Status struct {
	State     State  `json:"state"`
	Mode      string `json:"mode"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
	Binary    string `json:"binary,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

type Manager struct {
	baseDir string
	dataDir string
	// origin is the local URL cloudflared should forward to.
	origin string
	log    *slog.Logger

	mu        sync.Mutex
	cfg       config.Tunnel
	state     State
	url       string
	err       error
	binary    string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	startedAt time.Time
	tail      []string
}

func NewManager(cfg config.Tunnel, baseDir, dataDir, origin string, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, baseDir: baseDir, dataDir: dataDir, origin: origin, log: log, state: StateDisabled}
}

var quickURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

// findBinary looks for cloudflared in the configured path, the bundle
// directory, the data directory and PATH.
func (m *Manager) findBinary(cfg config.Tunnel) (string, error) {
	name := binaryName()
	candidates := []string{}
	if cfg.BinaryPath != "" {
		candidates = append(candidates, cfg.BinaryPath)
	}
	candidates = append(candidates,
		filepath.Join(m.baseDir, "third_party", "cloudflared", name),
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
	return "", errors.New("cloudflared not found")
}

func downloadURL() (string, error) {
	base := "https://github.com/cloudflare/cloudflared/releases/latest/download/"
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return base + "cloudflared-windows-amd64.exe", nil
	case "windows/386":
		return base + "cloudflared-windows-386.exe", nil
	case "linux/amd64":
		return base + "cloudflared-linux-amd64", nil
	case "linux/arm64":
		return base + "cloudflared-linux-arm64", nil
	}
	return "", fmt.Errorf("no automatic cloudflared download for %s/%s; install it manually", runtime.GOOS, runtime.GOARCH)
}

// download fetches the latest cloudflared release into the data directory.
func (m *Manager) download(ctx context.Context) (string, error) {
	u, err := downloadURL()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(m.dataDir, "bin", binaryName())
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	m.log.Info("downloading cloudflared", "url", u, "dest", dest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download cloudflared: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download cloudflared: HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func (m *Manager) setState(st State, url string, err error) {
	m.mu.Lock()
	m.state = st
	if url != "" {
		m.url = url
	}
	m.err = err
	m.mu.Unlock()
}

// Start launches cloudflared in the background and keeps it running until
// Stop is called. It returns immediately; progress is reported via Status.
func (m *Manager) Start(cfg config.Tunnel) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		m.Stop()
		m.mu.Lock()
	}
	m.cfg = cfg
	if !cfg.Enabled {
		m.state, m.url, m.err = StateDisabled, "", nil
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.state, m.url, m.err, m.tail = StateStarting, "", nil, nil
	m.mu.Unlock()

	go m.supervise(ctx, cfg)
}

func (m *Manager) supervise(ctx context.Context, cfg config.Tunnel) {
	bin, err := m.findBinary(cfg)
	if err != nil {
		if !cfg.AutoDownload {
			m.setState(StateError, "", fmt.Errorf("%w; enable tunnel.auto_download or place it in third_party/cloudflared", err))
			return
		}
		m.setState(StateDownloading, "", nil)
		bin, err = m.download(ctx)
		if err != nil {
			m.setState(StateError, "", err)
			return
		}
	}
	m.mu.Lock()
	m.binary = bin
	m.mu.Unlock()

	backoff := 2 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := m.runOnce(ctx, bin, cfg)
		if ctx.Err() != nil {
			m.setState(StateStopped, "", nil)
			return
		}
		m.log.Warn("cloudflared exited, restarting", "err", err, "backoff", backoff)
		m.setState(StateError, "", fmt.Errorf("cloudflared exited: %v (retrying)", err))
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			m.setState(StateStopped, "", nil)
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (m *Manager) args(cfg config.Tunnel) []string {
	common := []string{"--no-autoupdate", "--loglevel", "info"}
	if cfg.Mode == "named" {
		// The origin (https://localhost:8443 with "No TLS verify") is
		// configured on the Cloudflare side for named tunnels.
		return append([]string{"tunnel"}, append(common, "run", "--token", cfg.Token)...)
	}
	return append([]string{"tunnel"}, append(common, "--url", m.origin, "--no-tls-verify")...)
}

func (m *Manager) runOnce(ctx context.Context, bin string, cfg config.Tunnel) error {
	cmd := exec.CommandContext(ctx, bin, m.args(cfg)...)
	hideConsoleWindow(cmd)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.cmd = cmd
	m.startedAt = time.Now()
	m.state = StateStarting
	m.mu.Unlock()
	m.log.Info("cloudflared started", "mode", cfg.Mode, "bin", bin)

	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		m.remember(line)
		if cfg.Mode == "quick" {
			// cloudflared also logs its API endpoint (api.trycloudflare.com)
			// in error messages; only the assigned hostname is a tunnel URL.
			if u := quickURLRe.FindString(line); u != "" && u != "https://api.trycloudflare.com" {
				m.log.Info("tunnel url", "url", u)
				m.setState(StateConnected, u, nil)
			}
		}
		if strings.Contains(line, "Registered tunnel connection") || strings.Contains(line, "Connection registered") {
			m.mu.Lock()
			if cfg.Mode == "named" {
				m.state = StateConnected
				if cfg.Hostname != "" {
					m.url = "https://" + strings.TrimPrefix(strings.TrimPrefix(cfg.Hostname, "https://"), "http://")
				}
			} else if m.url != "" {
				m.state = StateConnected
			}
			m.err = nil
			m.mu.Unlock()
		}
	}
	return cmd.Wait()
}

func (m *Manager) remember(line string) {
	m.mu.Lock()
	m.tail = append(m.tail, line)
	if len(m.tail) > 50 {
		m.tail = m.tail[len(m.tail)-50:]
	}
	m.mu.Unlock()
}

// Stop terminates cloudflared.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	cmd := m.cmd
	m.cancel, m.cmd = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
	m.setState(StateStopped, "", nil)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{State: m.state, Mode: m.cfg.Mode, URL: m.url, Binary: m.binary}
	if m.state != StateConnected {
		st.URL = ""
	}
	if m.err != nil {
		st.Error = m.err.Error()
	}
	if !m.startedAt.IsZero() && m.state == StateConnected {
		st.StartedAt = m.startedAt.Format(time.RFC3339)
	}
	return st
}

// Log returns the most recent cloudflared output lines.
func (m *Manager) Log() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.tail))
	copy(out, m.tail)
	return out
}
