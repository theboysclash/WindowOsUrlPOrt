// Command vmserver turns the PC it runs on into a web-accessible Windows VM
// host: it boots a QEMU guest and serves a login-protected browser console.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/auth"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/httpserver"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/relay"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tailnet"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tunnel"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/vm"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	exe, _ := os.Executable()
	defaultBase := filepath.Dir(exe)

	var (
		baseDir    = flag.String("dir", defaultBase, "base directory holding config.yaml, data/ and vm/")
		listen     = flag.String("listen", "", "override listen address, e.g. 0.0.0.0:8443")
		noVM       = flag.Bool("no-vm", false, "do not start the VM automatically")
		share      = flag.Bool("share", false, "enable a Cloudflare quick tunnel for this run (public *.trycloudflare.com URL)")
		tailscale  = flag.Bool("tailscale", false, "join your Tailscale network for this run (https://<name>.<tailnet>.ts.net)")
		funnel     = flag.Bool("funnel", false, "with -tailscale: also publish on the public internet via Tailscale Funnel")
		relayLink  = flag.String("relay", "", `connect to a vmrelay (e.g. in a GitHub Codespace): paste the "https://...#key" link it prints; saved for next time. "off" disables it`)
		addUser    = flag.String("add-user", "", "add or update a user as user:password[:admin] and exit")
		resetAdmin = flag.Bool("reset-admin-password", false, "generate a new password for the first admin user and exit")
		printURLs  = flag.Bool("print-urls", false, "print the console URLs and exit")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("vmserver", version)
		return nil
	}

	cfgPath := filepath.Join(*baseDir, "config.yaml")
	cfg, err := config.Load(cfgPath)
	firstRun := false
	if errors.Is(err, config.ErrNotFound) {
		firstRun = true
	} else if err != nil {
		return err
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}

	log := newLogger(cfg.DataDir)
	log.Info("vmserver starting", "version", version, "config", cfgPath, "first_run", firstRun)

	if firstRun || len(cfg.Users) == 0 {
		pw, err := bootstrapAdmin(cfg)
		if err != nil {
			return err
		}
		announceCredentials(cfg, "admin", pw)
	}

	if *addUser != "" {
		return addUserCmd(cfg, *addUser)
	}
	if *relayLink != "" {
		if strings.EqualFold(*relayLink, "off") {
			cfg.Relay.Enabled = false
		} else {
			if _, _, err := relay.ParseLink(*relayLink); err != nil {
				return err
			}
			cfg.Relay = config.Relay{Enabled: true, Link: strings.TrimSpace(*relayLink)}
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	if *resetAdmin {
		for i := range cfg.Users {
			if cfg.Users[i].Admin {
				pw, err := auth.RandomPassword(12)
				if err != nil {
					return err
				}
				hash, err := auth.HashPassword(pw)
				if err != nil {
					return err
				}
				cfg.Users[i].PasswordHash = hash
				if err := cfg.Save(); err != nil {
					return err
				}
				announceCredentials(cfg, cfg.Users[i].Username, pw)
				return nil
			}
		}
		return errors.New("no admin user found")
	}

	am := auth.NewManager(cfg, auth.Options{
		SessionTTL:    time.Duration(cfg.Auth.SessionTTLMinutes) * time.Minute,
		IdleTimeout:   time.Duration(cfg.Auth.IdleTimeoutMinutes) * time.Minute,
		MaxAttempts:   cfg.Auth.MaxLoginAttempts,
		AttemptWindow: time.Duration(cfg.Auth.LoginWindowMinutes) * time.Minute,
		Secure:        cfg.TLS.Mode != config.TLSOff,
	})
	vmm := vm.NewManager(cfg.VM, *baseDir, log)
	tun := tunnel.NewManager(cfg.Tunnel, *baseDir, cfg.DataDir, httpserver.LocalOrigin(cfg), log)
	var selfSigned *tls.Certificate
	if cfg.TLS.Mode != config.TLSFiles {
		if cert, err := httpserver.LoadOrCreateSelfSigned(cfg.DataDir); err == nil {
			selfSigned = &cert
		}
	}
	tail := tailnet.NewManager(cfg.Tailscale, cfg.DataDir, selfSigned, log)
	srv, err := httpserver.New(cfg, am, vmm, tun, tail, log)
	if err != nil {
		return err
	}

	if *printURLs {
		for _, u := range srv.URLs() {
			fmt.Println(u)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.VM.AutoStart && !*noVM {
		go func() {
			st := vmm.Status()
			if !st.DiskExists {
				log.Warn("no disk image yet; log in as admin and open /setup", "path", cfg.VM.DiskPath)
				return
			}
			if !st.QEMUFound {
				log.Warn("QEMU not found; see README for installing it into third_party/qemu")
				return
			}
			if err := vmm.Start(ctx); err != nil {
				log.Error("vm start failed", "err", err)
			}
		}()
	}

	fmt.Println()
	fmt.Println("  VM Server is running. Open one of these URLs from any PC on the network:")
	for _, u := range srv.URLs() {
		fmt.Println("    " + u)
	}
	fmt.Println("  (The certificate is self-signed; accept the browser warning the first time.)")
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe(ctx) }()

	tunCfg := cfg.Tunnel
	if *share {
		tunCfg.Enabled = true
		tunCfg.Mode = "quick"
	}
	if tunCfg.Enabled {
		tun.Start(tunCfg)
		go announceTunnel(ctx, tun)
	}
	tsCfg := cfg.Tailscale
	if *tailscale || *funnel {
		tsCfg.Enabled = true
		tsCfg.Funnel = tsCfg.Funnel || *funnel
	}
	if tsCfg.Enabled {
		tail.Start(tsCfg, srv.Handler())
		go announceTailscale(ctx, tail)
	}
	rc := relay.NewClient(log)
	srv.SetRelay(rc)
	if cfg.Relay.Enabled && cfg.Relay.Link != "" {
		if err := rc.Start(cfg.Relay.Link, srv.Handler()); err != nil {
			log.Error("relay", "err", err)
		} else {
			go announceRelay(ctx, rc)
		}
	}

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	tun.Stop()
	tail.Stop()
	rc.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vmm.Stop(shutdownCtx, 45*time.Second); err != nil && !errors.Is(err, vm.ErrNotRunning) {
		log.Warn("vm stop", "err", err)
	}
	<-serveErr
	return nil
}

// announceTunnel prints the public URL once cloudflared reports it.
func announceTunnel(ctx context.Context, tun *tunnel.Manager) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := tun.Status()
			if st.State == tunnel.StateConnected && st.URL != "" && st.URL != last {
				last = st.URL
				fmt.Println()
				fmt.Println("  Shareable public URL (Cloudflare Tunnel):")
				fmt.Println("    " + st.URL)
				if st.Mode == "quick" {
					fmt.Println("  This address changes every time the server restarts. Use a named tunnel for a fixed one.")
				}
				fmt.Println()
			}
			if st.State == tunnel.StateError && st.Error != "" && st.Error != last {
				last = st.Error
				fmt.Println("  Tunnel problem: " + st.Error)
			}
		}
	}
}

// announceTailscale prints the login link and, once connected, the tailnet URL.
func announceTailscale(ctx context.Context, tail *tailnet.Manager) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := tail.Status()
			key := string(st.State) + st.URL + st.AuthURL + st.Error
			if key == last {
				continue
			}
			last = key
			switch st.State {
			case tailnet.StateNeedsLogin:
				fmt.Println()
				fmt.Println("  Tailscale: this PC is not connected to a Tailscale account yet.")
				fmt.Println("  Open this link, sign in and approve the machine:")
				fmt.Println("    " + st.AuthURL)
				fmt.Println()
			case tailnet.StateRunning:
				fmt.Println()
				fmt.Println("  Tailscale URL (works from any device signed in to your tailnet):")
				fmt.Println("    " + st.URL)
				if st.Funnel && st.Note == "" {
					fmt.Println("  Funnel is on: the same link also works from the public internet.")
				}
				if st.Note != "" {
					fmt.Println("  Note: " + st.Note)
				}
				fmt.Println()
			case tailnet.StateError:
				fmt.Println("  Tailscale problem: " + st.Error)
			}
		}
	}
}

// announceRelay prints the relay URL each time the connection comes up.
func announceRelay(ctx context.Context, rc *relay.Client) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var last relay.State
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := rc.Status()
			if st.State == last {
				continue
			}
			last = st.State
			switch st.State {
			case relay.StateConnected:
				fmt.Println()
				fmt.Println("  Relay link (GitHub Codespace) - open this on the Chromebook or any other device:")
				fmt.Println("    " + st.URL)
				fmt.Println()
			case relay.StateError:
				fmt.Println("  Relay problem: " + st.Error + " (retrying)")
			}
		}
	}
}

func newLogger(dataDir string) *slog.Logger {
	var w io.Writer = os.Stderr
	f, err := os.OpenFile(filepath.Join(dataDir, "vmserver.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		w = io.MultiWriter(os.Stderr, f)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// bootstrapAdmin writes a fresh config with a random admin password.
func bootstrapAdmin(cfg *config.Config) (string, error) {
	pw, err := auth.RandomPassword(12)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return "", err
	}
	cfg.Users = []config.User{{Username: "admin", PasswordHash: hash, Admin: true}}
	if err := cfg.Save(); err != nil {
		return "", err
	}
	return pw, nil
}

// announceCredentials prints the generated password and also stores it in the
// data directory, because a GUI build has no visible console.
func announceCredentials(cfg *config.Config, user, pw string) {
	msg := fmt.Sprintf("Login: %s\nPassword: %s\n", user, pw)
	fmt.Println()
	fmt.Println("  ================= FIRST-RUN CREDENTIALS =================")
	fmt.Printf("  Username: %s\n  Password: %s\n", user, pw)
	fmt.Println("  Change it after signing in (user menu > Change password).")
	fmt.Println("  =========================================================")
	fmt.Println()
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "initial-credentials.txt"), []byte(msg), 0o600)
}

func addUserCmd(cfg *config.Config, spec string) error {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 || parts[0] == "" || len(parts[1]) < 8 {
		return errors.New("--add-user expects user:password[:admin]; password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(parts[1])
	if err != nil {
		return err
	}
	admin := len(parts) == 3 && parts[2] == "admin"
	if u := cfg.FindUser(parts[0]); u != nil {
		u.PasswordHash, u.Admin = hash, admin
	} else {
		cfg.Users = append(cfg.Users, config.User{Username: parts[0], PasswordHash: hash, Admin: admin})
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("user %q saved (admin=%v)\n", parts[0], admin)
	return nil
}
