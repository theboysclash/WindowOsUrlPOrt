// Package codespace publishes the console on a GitHub Codespaces URL
// (https://<codespace>-<port>.app.github.dev) while the server keeps running
// on this PC. It drives the GitHub CLI:
//
//  1. signs in with the device flow (or a token) with the codespace scope,
//  2. reuses or creates a small relay codespace,
//  3. holds `gh codespace ssh -- -N -R <port>:127.0.0.1:<local>` open so the
//     codespace's port forwards to this PC,
//  4. sets the forwarded port's visibility and reports its URL.
package codespace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
)

type State string

const (
	StateDisabled   State = "disabled"
	StateInstalling State = "installing"
	StateLogin      State = "needs_login"
	StateCreating   State = "creating"
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateError      State = "error"
	StateStopped    State = "stopped"
)

type Status struct {
	State      State  `json:"state"`
	Step       string `json:"step,omitempty"`
	URL        string `json:"url,omitempty"`
	Codespace  string `json:"codespace,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	// LoginCode and LoginURL are the GitHub device-flow code and page the
	// admin must open to authorise the GitHub CLI.
	LoginCode string `json:"login_code,omitempty"`
	LoginURL  string `json:"login_url,omitempty"`
	Error     string `json:"error,omitempty"`
}

const displayName = "vmserver relay"

type Manager struct {
	baseDir string
	dataDir string
	// localPort is the loopback plain-HTTP listener the tunnel forwards to.
	localPort int
	log       *slog.Logger

	mu        sync.Mutex
	cfg       config.Codespace
	state     State
	step      string
	name      string
	url       string
	loginCode string
	loginURL  string
	err       error
	bin       string
	cancel    context.CancelFunc
	done      chan struct{}
	tail      []string
}

func NewManager(cfg config.Codespace, baseDir, dataDir string, localPort int, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, baseDir: baseDir, dataDir: dataDir, localPort: localPort, log: log, state: StateDisabled}
}

// Start launches the whole chain in the background, replacing any previous
// run. Progress is reported via Status.
func (m *Manager) Start(cfg config.Codespace) {
	m.halt()
	m.mu.Lock()
	m.cfg = cfg
	if !cfg.Enabled {
		m.state, m.url, m.err, m.step = StateDisabled, "", nil, ""
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan struct{})
	m.state, m.step, m.url, m.err, m.tail = StateInstalling, "Preparing", "", nil, nil
	m.loginCode, m.loginURL = "", ""
	done := m.done
	m.mu.Unlock()

	go func() {
		defer close(done)
		m.supervise(ctx, cfg)
	}()
}

// halt stops the tunnel but leaves the codespace running.
func (m *Manager) halt() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// Stop closes the tunnel and shuts the relay codespace down so it stops
// using Codespaces hours.
func (m *Manager) Stop() {
	m.halt()
	m.mu.Lock()
	bin, name, cfg := m.bin, m.name, m.cfg
	wasActive := m.state != StateDisabled && m.state != StateStopped
	m.state, m.step, m.loginCode, m.loginURL, m.err = StateStopped, "", "", "", nil
	m.mu.Unlock()
	if !wasActive || bin == "" || name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := m.gh(ctx, bin, cfg, "codespace", "stop", "-c", name); err != nil {
		m.log.Warn("codespace stop", "name", name, "err", err, "out", strings.TrimSpace(out))
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{State: m.state, Step: m.step, Codespace: m.name, Visibility: m.cfg.Visibility}
	if m.state == StateConnected {
		st.URL = m.url
	}
	if m.state == StateLogin {
		st.LoginCode, st.LoginURL = m.loginCode, m.loginURL
	}
	if m.err != nil {
		st.Error = m.err.Error()
	}
	return st
}

// PublicHost is the host name browsers use to reach the forwarded port, or
// "" before a codespace has been chosen.
func (m *Manager) PublicHost() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.url != "" {
		if u, err := url.Parse(m.url); err == nil {
			return u.Host
		}
	}
	if m.name != "" {
		return fallbackHost(m.name, m.cfg.Port)
	}
	return ""
}

// Log returns the most recent GitHub CLI output lines.
func (m *Manager) Log() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.tail...)
}

func fallbackHost(name string, port int) string {
	return fmt.Sprintf("%s-%d.app.github.dev", name, port)
}

func (m *Manager) set(st State, step string, err error) {
	m.mu.Lock()
	m.state, m.step, m.err = st, step, err
	m.mu.Unlock()
}

func (m *Manager) remember(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	m.mu.Lock()
	m.tail = append(m.tail, line)
	if len(m.tail) > 80 {
		m.tail = m.tail[len(m.tail)-80:]
	}
	m.mu.Unlock()
}

func (m *Manager) supervise(ctx context.Context, cfg config.Codespace) {
	fail := func(err error) {
		if ctx.Err() != nil {
			return
		}
		m.log.Warn("codespace sharing failed", "err", err)
		m.set(StateError, "", err)
	}

	if _, err := findSSH(); err != nil {
		fail(err)
		return
	}

	m.set(StateInstalling, "Looking for the GitHub CLI", nil)
	bin, err := m.findBinary(cfg)
	if err != nil {
		m.set(StateInstalling, "Downloading the GitHub CLI", nil)
		if bin, err = m.download(ctx); err != nil {
			fail(err)
			return
		}
	}
	m.mu.Lock()
	m.bin = bin
	m.mu.Unlock()

	if err := m.ensureAuth(ctx, bin, cfg); err != nil {
		fail(err)
		return
	}

	m.set(StateCreating, "Finding your relay codespace", nil)
	name, err := m.ensureCodespace(ctx, bin, cfg)
	if err != nil {
		fail(err)
		return
	}
	m.mu.Lock()
	m.name = name
	m.mu.Unlock()

	backoff := 3 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := m.runTunnel(ctx, bin, cfg, name)
		if ctx.Err() != nil {
			return
		}
		m.log.Warn("codespace tunnel ended, reconnecting", "err", err, "backoff", backoff)
		m.set(StateError, "", fmt.Errorf("tunnel dropped: %v (reconnecting)", err))
		if time.Since(start) > 2*time.Minute {
			backoff = 3 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// --- GitHub CLI plumbing ---

func (m *Manager) command(ctx context.Context, bin string, cfg config.Codespace, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1")
	if cfg.Token != "" {
		cmd.Env = append(cmd.Env, "GH_TOKEN="+cfg.Token)
	}
	if dir, err := findSSH(); err == nil {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	hideConsoleWindow(cmd)
	return cmd
}

// gh runs a short gh command and returns its combined output.
func (m *Manager) gh(ctx context.Context, bin string, cfg config.Codespace, args ...string) (string, error) {
	out, err := m.command(ctx, bin, cfg, args...).CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		m.remember(l)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return string(out), fmt.Errorf("gh %s: %s", strings.Join(args[:min(2, len(args))], " "), msg)
	}
	return string(out), nil
}

var (
	deviceCodeRe = regexp.MustCompile(`one-time code: ([A-Z0-9]{4}-[A-Z0-9]{4})`)
	deviceURLRe  = regexp.MustCompile(`https://github\.com/login/device\S*`)
)

func (m *Manager) ensureAuth(ctx context.Context, bin string, cfg config.Codespace) error {
	m.set(StateLogin, "Checking GitHub sign-in", nil)
	out, err := m.gh(ctx, bin, cfg, "auth", "status", "--hostname", "github.com")
	if cfg.Token != "" {
		if err != nil {
			return fmt.Errorf("the saved GitHub token was rejected: %w", err)
		}
		return nil
	}
	var args []string
	switch {
	case err == nil && strings.Contains(out, "'codespace'"):
		return nil
	case err == nil:
		args = []string{"auth", "refresh", "--hostname", "github.com", "--scopes", "codespace"}
	default:
		args = []string{"auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web", "--scopes", "codespace"}
	}
	m.set(StateLogin, "Waiting for you to approve the GitHub sign-in", nil)
	if err := m.deviceFlow(ctx, bin, cfg, args); err != nil {
		return fmt.Errorf("GitHub sign-in: %w", err)
	}
	m.mu.Lock()
	m.loginCode, m.loginURL = "", ""
	m.mu.Unlock()
	return nil
}

// deviceFlow runs gh auth login/refresh and surfaces its one-time code.
func (m *Manager) deviceFlow(ctx context.Context, bin string, cfg config.Codespace, args []string) error {
	cmd := m.command(ctx, bin, cfg, args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	// gh waits for Enter before opening the browser when it thinks it can.
	cmd.Stdin = strings.NewReader("\n")
	if err := cmd.Start(); err != nil {
		return err
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait(); pw.Close() }()
	var last string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		m.remember(line)
		last = strings.TrimSpace(line)
		m.mu.Lock()
		if c := deviceCodeRe.FindStringSubmatch(line); c != nil {
			m.loginCode = c[1]
			if m.loginURL == "" {
				m.loginURL = "https://github.com/login/device"
			}
		}
		if u := deviceURLRe.FindString(line); u != "" {
			m.loginURL = u
		}
		m.mu.Unlock()
	}
	if err := <-waitErr; err != nil {
		if last == "" {
			return err
		}
		return errors.New(last)
	}
	return nil
}

// jsonPart drops any warning text gh prints before its JSON output.
func jsonPart(out string) []byte {
	if i := strings.Index(out, "["); i >= 0 {
		return []byte(out[i:])
	}
	return []byte(out)
}

type codespaceInfo struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Repository  string `json:"repository"`
	DisplayName string `json:"displayName"`
}

func (m *Manager) savedNamePath() string { return filepath.Join(m.dataDir, "codespace-name.txt") }

// ensureCodespace returns the codespace to relay through: the configured
// one, the one created last time, or a newly created one.
func (m *Manager) ensureCodespace(ctx context.Context, bin string, cfg config.Codespace) (string, error) {
	out, err := m.gh(ctx, bin, cfg, "codespace", "list", "--json", "name,state,repository,displayName")
	if err != nil {
		return "", err
	}
	var list []codespaceInfo
	if err := json.Unmarshal(jsonPart(out), &list); err != nil {
		return "", fmt.Errorf("read codespace list: %w", err)
	}
	exists := func(n string) bool {
		for _, c := range list {
			if c.Name == n {
				return true
			}
		}
		return false
	}
	if cfg.Name != "" {
		if !exists(cfg.Name) {
			return "", fmt.Errorf("codespace %q not found on your GitHub account", cfg.Name)
		}
		return cfg.Name, nil
	}
	if raw, err := os.ReadFile(m.savedNamePath()); err == nil {
		if n := strings.TrimSpace(string(raw)); n != "" && exists(n) {
			return n, nil
		}
	}
	for _, c := range list {
		if c.DisplayName == displayName && strings.EqualFold(c.Repository, cfg.Repo) {
			_ = os.WriteFile(m.savedNamePath(), []byte(c.Name), 0o600)
			return c.Name, nil
		}
	}

	m.set(StateCreating, "Creating a codespace from "+cfg.Repo+" (first time only, about a minute)", nil)
	out, err = m.gh(ctx, bin, cfg, "codespace", "create",
		"--repo", cfg.Repo, "--machine", cfg.Machine, "--display-name", displayName,
		"--idle-timeout", "240m", "--default-permissions")
	if err != nil {
		return "", err
	}
	name := ""
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.ContainsAny(l, " \t") {
			name = l
		}
	}
	if name == "" {
		return "", fmt.Errorf("gh codespace create did not report a name: %s", strings.TrimSpace(out))
	}
	_ = os.WriteFile(m.savedNamePath(), []byte(name), 0o600)
	m.log.Info("codespace created", "name", name, "repo", cfg.Repo)
	return name, nil
}

// runTunnel holds the reverse SSH tunnel open until it drops or ctx ends.
func (m *Manager) runTunnel(ctx context.Context, bin string, cfg config.Codespace, name string) error {
	m.set(StateConnecting, "Starting the codespace and opening the tunnel", nil)
	cmd := m.command(ctx, bin, cfg, "codespace", "ssh", "-c", name, "--",
		"-N", "-T",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-R", fmt.Sprintf("%d:127.0.0.1:%d", cfg.Port, m.localPort))
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	m.log.Info("codespace tunnel started", "codespace", name, "port", cfg.Port, "local", m.localPort)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait(); pw.Close() }()

	pubCtx, cancelPub := context.WithCancel(ctx)
	defer cancelPub()
	go m.publish(pubCtx, bin, cfg, name)

	var last string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		m.remember(sc.Text())
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	err := <-waitErr
	if last != "" {
		return errors.New(last)
	}
	if err == nil {
		err = errors.New("ssh exited")
	}
	return err
}

// publish waits for the forwarded port to appear in the codespace, applies
// the configured visibility and records the URL.
func (m *Manager) publish(ctx context.Context, bin string, cfg config.Codespace, name string) {
	port := strconv.Itoa(cfg.Port)
	var lastErr error
	for i := 0; i < 40; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		if _, err := m.gh(ctx, bin, cfg, "codespace", "ports", "visibility", port+":"+cfg.Visibility, "-c", name); err != nil {
			lastErr = err
			continue
		}
		u := "https://" + fallbackHost(name, cfg.Port)
		if out, err := m.gh(ctx, bin, cfg, "codespace", "ports", "-c", name, "--json", "sourcePort,browseUrl"); err == nil {
			var ports []struct {
				SourcePort int    `json:"sourcePort"`
				BrowseURL  string `json:"browseUrl"`
			}
			if json.Unmarshal(jsonPart(out), &ports) == nil {
				for _, p := range ports {
					if p.SourcePort == cfg.Port && strings.HasPrefix(p.BrowseURL, "https://") {
						u = strings.TrimRight(p.BrowseURL, "/")
					}
				}
			}
		}
		m.mu.Lock()
		m.url, m.state, m.step, m.err = u, StateConnected, "", nil
		m.mu.Unlock()
		m.log.Info("codespace url", "url", u, "visibility", cfg.Visibility)
		return
	}
	if ctx.Err() == nil && lastErr != nil {
		m.set(StateError, "", fmt.Errorf("tunnel is up but the port could not be published: %w", lastErr))
	}
}
