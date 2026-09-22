// Package config loads and persists the server configuration that lives in
// config.yaml next to the executable.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// TLSMode selects how the HTTPS listener obtains its certificate.
type TLSMode string

const (
	TLSSelfSigned TLSMode = "self-signed"
	TLSFiles      TLSMode = "files"
	TLSOff        TLSMode = "off"
)

type TLS struct {
	Mode     TLSMode `yaml:"mode"`
	CertFile string  `yaml:"cert_file,omitempty"`
	KeyFile  string  `yaml:"key_file,omitempty"`
}

type User struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
	Admin        bool   `yaml:"admin"`
}

type VM struct {
	// QEMUDir is the directory that holds qemu-system-x86_64(.exe).
	// Empty means "look in third_party/qemu next to the exe, then PATH".
	QEMUDir  string `yaml:"qemu_dir"`
	DiskPath string `yaml:"disk_path"`
	// DiskInterface is "sata" (works with stock Windows media) or "virtio"
	// (faster, needs virtio-win drivers installed in the guest).
	DiskInterface string `yaml:"disk_interface"`
	// ISOPath, when set, is attached as a CD-ROM (used for installation).
	ISOPath string `yaml:"iso_path,omitempty"`
	// VirtioISOPath is the virtio-win driver ISO, attached during install.
	VirtioISOPath string `yaml:"virtio_iso_path,omitempty"`
	RAMMB         int    `yaml:"ram_mb"`
	CPUs          int    `yaml:"cpus"`
	VNCPort       int    `yaml:"vnc_port"`
	QMPPort       int    `yaml:"qmp_port"`
	// Accel is "auto", "whpx", "hvf", "kvm" or "tcg".
	Accel string `yaml:"accel"`
	// AutoStart boots the VM as soon as the server starts.
	AutoStart bool `yaml:"auto_start"`
	// RestartOnCrash relaunches QEMU if it exits unexpectedly.
	RestartOnCrash bool `yaml:"restart_on_crash"`
	// ExtraArgs are appended verbatim to the QEMU command line.
	ExtraArgs []string `yaml:"extra_args,omitempty"`
}

type Auth struct {
	// SessionTTLMinutes is the absolute lifetime of a login session.
	SessionTTLMinutes int `yaml:"session_ttl_minutes"`
	// IdleTimeoutMinutes ends a session that has seen no requests for this long.
	IdleTimeoutMinutes int `yaml:"idle_timeout_minutes"`
	// MaxLoginAttempts per IP within LoginWindowMinutes before lockout.
	MaxLoginAttempts   int `yaml:"max_login_attempts"`
	LoginWindowMinutes int `yaml:"login_window_minutes"`
}

// Tunnel configures the Cloudflare Tunnel (cloudflared) that publishes the
// console on a shareable HTTPS URL without router port forwarding.
type Tunnel struct {
	Enabled bool `yaml:"enabled"`
	// Mode is "quick" (free, random *.trycloudflare.com URL that changes on
	// every start, no account needed) or "named" (stable hostname on your
	// own domain; requires a tunnel token from the Cloudflare Zero Trust
	// dashboard).
	Mode string `yaml:"mode"`
	// Token is the connector token for a named tunnel.
	Token string `yaml:"token,omitempty"`
	// Hostname is the public hostname configured for the named tunnel; used
	// only for display since cloudflared does not report it.
	Hostname string `yaml:"hostname,omitempty"`
	// BinaryPath overrides where to find cloudflared. Empty means look in
	// third_party/cloudflared next to the exe, the data dir, then PATH.
	BinaryPath string `yaml:"binary_path,omitempty"`
	// AutoDownload fetches cloudflared from GitHub releases into the data
	// dir when it cannot be found locally.
	AutoDownload bool `yaml:"auto_download"`
}

type Config struct {
	ListenAddr string `yaml:"listen_addr"`
	// DataDir stores generated certificates, the session secret and logs.
	DataDir string `yaml:"data_dir"`
	TLS     TLS    `yaml:"tls"`
	Auth    Auth   `yaml:"auth"`
	VM      VM     `yaml:"vm"`
	Tunnel  Tunnel `yaml:"tunnel"`
	Users   []User `yaml:"users"`

	path string
	mu   *sync.RWMutex
}

// Lock/Unlock guard mutations of the configuration made while the server is
// running (setup wizard, user management). Readers on the hot path use Lookup.
func (c *Config) Lock()   { c.mu.Lock() }
func (c *Config) Unlock() { c.mu.Unlock() }

// Lookup implements the auth credential store.
func (c *Config) Lookup(username string) (hash string, admin bool, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u := c.FindUser(username)
	if u == nil {
		return "", false, false
	}
	return u.PasswordHash, u.Admin, true
}

// Path returns the file this configuration was loaded from.
func (c *Config) Path() string { return c.path }

func Default(baseDir string) *Config {
	return &Config{
		mu:         &sync.RWMutex{},
		ListenAddr: "0.0.0.0:8443",
		DataDir:    filepath.Join(baseDir, "data"),
		TLS:        TLS{Mode: TLSSelfSigned},
		Auth: Auth{
			SessionTTLMinutes:  12 * 60,
			IdleTimeoutMinutes: 60,
			MaxLoginAttempts:   8,
			LoginWindowMinutes: 15,
		},
		VM: VM{
			DiskPath:       filepath.Join(baseDir, "vm", "win10.qcow2"),
			DiskInterface:  "sata",
			RAMMB:          4096,
			CPUs:           2,
			VNCPort:        5900,
			QMPPort:        4444,
			Accel:          "auto",
			AutoStart:      true,
			RestartOnCrash: false,
		},
		Tunnel: Tunnel{Enabled: false, Mode: "quick", AutoDownload: true},
	}
}

// Load reads the config at path. If the file does not exist a default
// configuration is returned along with ErrNotFound so the caller can run
// first-time setup.
func Load(path string) (*Config, error) {
	cfg := Default(filepath.Dir(path))
	cfg.path = path
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, ErrNotFound
		}
		return nil, err
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

var ErrNotFound = errors.New("config file not found")

func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config has no path")
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	switch c.TLS.Mode {
	case TLSSelfSigned, TLSOff:
	case TLSFiles:
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return errors.New("tls.cert_file and tls.key_file are required when tls.mode is files")
		}
	default:
		return fmt.Errorf("unknown tls.mode %q", c.TLS.Mode)
	}
	if err := ValidateVM(&c.VM); err != nil {
		return err
	}
	if err := ValidateTunnel(&c.Tunnel); err != nil {
		return err
	}
	if c.Auth.SessionTTLMinutes <= 0 {
		c.Auth.SessionTTLMinutes = 12 * 60
	}
	if c.Auth.IdleTimeoutMinutes <= 0 {
		c.Auth.IdleTimeoutMinutes = 60
	}
	if c.Auth.MaxLoginAttempts <= 0 {
		c.Auth.MaxLoginAttempts = 8
	}
	if c.Auth.LoginWindowMinutes <= 0 {
		c.Auth.LoginWindowMinutes = 15
	}
	return nil
}

// ValidateVM checks and normalises the VM section.
func ValidateVM(c *VM) error {
	if c.RAMMB < 512 {
		return errors.New("vm.ram_mb must be at least 512")
	}
	if c.CPUs < 1 {
		return errors.New("vm.cpus must be at least 1")
	}
	if c.VNCPort < 5900 || c.VNCPort > 5999 {
		return errors.New("vm.vnc_port must be between 5900 and 5999")
	}
	if c.QMPPort <= 0 || c.VNCPort == c.QMPPort {
		return errors.New("vm.qmp_port must be a positive port distinct from vm.vnc_port")
	}
	switch c.DiskInterface {
	case "", "sata":
		c.DiskInterface = "sata"
	case "virtio":
	default:
		return fmt.Errorf("vm.disk_interface must be sata or virtio, got %q", c.DiskInterface)
	}
	switch c.Accel {
	case "", "auto":
		c.Accel = "auto"
	case "whpx", "kvm", "hvf", "tcg":
	default:
		return fmt.Errorf("vm.accel must be auto, whpx, kvm, hvf or tcg, got %q", c.Accel)
	}
	return nil
}

// ValidateTunnel checks and normalises the tunnel section.
func ValidateTunnel(t *Tunnel) error {
	switch t.Mode {
	case "", "quick":
		t.Mode = "quick"
	case "named":
		if t.Enabled && t.Token == "" {
			return errors.New("tunnel.token is required when tunnel.mode is named")
		}
	default:
		return fmt.Errorf("tunnel.mode must be quick or named, got %q", t.Mode)
	}
	return nil
}

// FindUser returns the user with the given name, or nil.
func (c *Config) FindUser(username string) *User {
	for i := range c.Users {
		if c.Users[i].Username == username {
			return &c.Users[i]
		}
	}
	return nil
}
