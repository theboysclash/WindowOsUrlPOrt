package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/auth"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/proxy"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tailnet"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tunnel"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/vm"
)

type ctxKey int

const sessionKey ctxKey = 1

func withSession(ctx context.Context, s *auth.Session) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}

func sessionFrom(ctx context.Context) *auth.Session {
	s, _ := ctx.Value(sessionKey).(*auth.Session)
	return s
}

func jsonAttr(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b) //nolint:gosec // only used inside <script> with marshalled data
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, vm.ErrNotRunning), errors.Is(err, vm.ErrAlreadyRunning):
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}

// --- pages ---

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if s.auth.SessionFromRequest(r) != nil {
		http.Redirect(w, r, "/console", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type loginData struct {
	Error string
	Next  string
}

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/console"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" {
		return "/console"
	}
	return next
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.SessionFromRequest(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", loginData{Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next := safeNext(r.Form.Get("next"))
	username := strings.TrimSpace(r.Form.Get("username"))
	password := r.Form.Get("password")
	sess, err := s.auth.Login(w, r, username, password)
	if err != nil {
		s.log.Warn("login failed", "user", username, "remote", r.RemoteAddr, "err", err)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", loginData{Error: err.Error(), Next: next})
		return
	}
	s.log.Info("login ok", "user", sess.Username, "remote", r.RemoteAddr)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := s.auth.SessionFromRequest(r); sess != nil {
		s.control.release(sess.ID)
	}
	s.auth.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type consoleData struct {
	User      string    `json:"user"`
	Admin     bool      `json:"admin"`
	VMStatus  vm.Status `json:"status"`
	SetupNeed bool      `json:"setupNeeded"`
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	st := s.vm.Status()
	s.render(w, "console.html", consoleData{
		User:      sess.Username,
		Admin:     sess.Admin,
		VMStatus:  st,
		SetupNeed: !st.DiskExists,
	})
}

type setupData struct {
	User     string
	Config   config.VM
	Status   vm.Status
	Tunnel   config.Tunnel
	HasToken bool
	TStatus  tunnel.Status
	Origin   string
	TS       config.Tailscale
	HasTSKey bool
	TSStatus tailnet.Status
}

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	// Show the live state: sharing may have been switched on for this run
	// with -share/-tailscale without being saved to config.yaml yet.
	t := s.cfg.Tunnel
	hasToken := t.Token != ""
	t.Token = ""
	tst := s.tunnel.Status()
	t.Enabled = t.Enabled || (tst.State != tunnel.StateDisabled && tst.State != tunnel.StateStopped)
	ts := s.cfg.Tailscale
	hasKey := ts.AuthKey != ""
	ts.AuthKey = ""
	tss := s.tail.Status()
	ts.Enabled = ts.Enabled || (tss.State != tailnet.StateDisabled && tss.State != tailnet.StateStopped)
	ts.Funnel = ts.Funnel || tss.Funnel
	s.render(w, "setup.html", setupData{
		User: sess.Username, Config: s.vm.Config(), Status: s.vm.Status(),
		Tunnel: t, HasToken: hasToken, TStatus: tst,
		Origin: strings.Replace(LocalOrigin(s.cfg), "127.0.0.1", "localhost", 1),
		TS:     ts, HasTSKey: hasKey, TSStatus: tss,
	})
}

// --- WebSocket VNC ---

func (s *Server) handleVNC(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	wantControl := r.URL.Query().Get("mode") != "view"

	vncAddr, vncPass, err := s.vm.VNCCredentials()
	if err != nil {
		writeErr(w, err)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	viewOnly := true
	if wantControl {
		viewOnly = !s.control.acquire(sess, cancel)
	}
	if viewOnly {
		s.control.addViewer(1)
		defer s.control.addViewer(-1)
	} else {
		defer s.control.release(sess.ID)
	}
	s.log.Info("console connected", "user", sess.Username, "viewOnly", viewOnly, "remote", r.RemoteAddr)

	err = proxy.Bridge(ctx, ws, proxy.Options{
		VNCAddr:      vncAddr,
		VNCPassword:  vncPass,
		ViewOnly:     viewOnly,
		SessionValid: func() bool { return s.auth.Valid(sess.ID) },
		OnTraffic:    func() { s.auth.Touch(sess.ID) },
	})
	if err != nil && !proxy.IsSessionExpired(err) && ctx.Err() == nil {
		s.log.Warn("console bridge ended", "user", sess.Username, "err", err)
	}
	s.log.Info("console disconnected", "user", sess.Username)
}

// --- controller lock ---

func (c *controller) acquire(sess *auth.Session, cancel context.CancelFunc) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID != "" && c.sessionID != sess.ID {
		return false
	}
	if c.sessionID == sess.ID && c.cancel != nil {
		// Same session reconnecting (e.g. page reload): drop the old socket.
		c.cancel()
	}
	c.sessionID, c.username, c.since, c.cancel = sess.ID, sess.Username, time.Now(), cancel
	return true
}

func (c *controller) release(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID == sessionID {
		c.sessionID, c.username, c.cancel = "", "", nil
	}
}

// take forcibly transfers control to sess, disconnecting the current holder.
func (c *controller) take(sess *auth.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil && c.sessionID != sess.ID {
		c.cancel()
	}
	// Mark the seat as reserved for this session; the actual socket will
	// re-acquire when the browser reconnects in control mode.
	c.sessionID, c.username, c.since, c.cancel = sess.ID, sess.Username, time.Now(), nil
}

func (c *controller) addViewer(d int) {
	c.mu.Lock()
	c.viewers += d
	c.mu.Unlock()
}

type controlInfo struct {
	Holder  string    `json:"holder,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	Mine    bool      `json:"mine"`
	Viewers int       `json:"viewers"`
}

func (c *controller) info(sessionID string) controlInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return controlInfo{Holder: c.username, Since: c.since, Mine: c.sessionID == sessionID && sessionID != "", Viewers: c.viewers}
}

// --- API ---

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	writeJSON(w, 200, map[string]any{
		"vm":        s.vm.Status(),
		"control":   s.control.info(sess.ID),
		"urls":      s.URLs(),
		"tunnel":    s.tunnel.Status(),
		"tailscale": s.tail.Status(),
	})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	writeJSON(w, 200, map[string]any{"username": sess.Username, "admin": sess.Admin})
}

func (s *Server) apiTakeControl(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	s.control.take(sess)
	s.log.Info("control taken", "user", sess.Username)
	writeJSON(w, 200, s.control.info(sess.ID))
}

func (s *Server) apiReleaseControl(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	s.control.release(sess.ID)
	writeJSON(w, 200, s.control.info(sess.ID))
}

func (s *Server) vmAction(fn func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			s.log.Warn("vm action failed", "path", r.URL.Path, "user", sess.Username, "err", err)
			writeErr(w, err)
			return
		}
		s.log.Info("vm action", "path", r.URL.Path, "user", sess.Username)
		writeJSON(w, 200, s.vm.Status())
	}
}

func (s *Server) apiListSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.vm.ListSnapshots(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if list == nil {
		list = []vm.SnapshotInfo{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) apiCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.vm.SaveSnapshot(ctx, body.Name); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": body.Name})
}

func (s *Server) apiRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.vm.LoadSnapshot(ctx, r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": r.PathValue("name")})
}

func (s *Server) apiDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.vm.DeleteSnapshot(ctx, r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": r.PathValue("name")})
}

// apiSetup creates the disk image (if missing) and stores VM settings. It is
// the first-run wizard's backend.
func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ISOPath       string `json:"iso_path"`
		VirtioISOPath string `json:"virtio_iso_path"`
		DiskInterface string `json:"disk_interface"`
		RAMMB         int    `json:"ram_mb"`
		CPUs          int    `json:"cpus"`
		DiskGB        int    `json:"disk_gb"`
		Accel         string `json:"accel"`
		ClipboardSync *bool  `json:"clipboard_sync"`
		Start         bool   `json:"start"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json"})
		return
	}
	if st := s.vm.Status().State; st == vm.StateRunning || st == vm.StateStarting {
		writeJSON(w, 409, map[string]string{"error": "stop the VM before changing its settings"})
		return
	}
	for _, p := range []string{body.ISOPath, body.VirtioISOPath} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			writeJSON(w, 400, map[string]string{"error": "file not found on server: " + p})
			return
		}
	}

	s.cfg.Lock()
	defer s.cfg.Unlock()
	vmCfg := s.cfg.VM
	vmCfg.ISOPath = body.ISOPath
	vmCfg.VirtioISOPath = body.VirtioISOPath
	if body.DiskInterface != "" {
		vmCfg.DiskInterface = body.DiskInterface
	}
	if body.RAMMB > 0 {
		vmCfg.RAMMB = body.RAMMB
	}
	if body.CPUs > 0 {
		vmCfg.CPUs = body.CPUs
	}
	if body.Accel != "" {
		vmCfg.Accel = body.Accel
	}
	if body.ClipboardSync != nil {
		vmCfg.ClipboardSync = body.ClipboardSync
	}
	if err := config.ValidateVM(&vmCfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.cfg.VM = vmCfg
	s.vm.UpdateConfig(s.cfg.VM)

	if _, err := os.Stat(s.cfg.VM.DiskPath); err != nil {
		if body.DiskGB <= 0 {
			body.DiskGB = 40
		}
		if err := s.vm.CreateDisk(r.Context(), body.DiskGB); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, err)
		return
	}
	if body.Start {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := s.vm.Start(ctx); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 200, s.vm.Status())
}

// apiEject detaches installation media so the next boot goes straight to the
// installed system.
func (s *Server) apiEject(w http.ResponseWriter, r *http.Request) {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	s.cfg.VM.ISOPath = ""
	s.cfg.VM.VirtioISOPath = ""
	s.vm.UpdateConfig(s.cfg.VM)
	if err := s.cfg.Save(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "media detached; takes effect on next VM start"})
}

// apiTunnel updates the Cloudflare Tunnel settings and (re)starts cloudflared.
func (s *Server) apiTunnel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled      *bool  `json:"enabled"`
		Mode         string `json:"mode"`
		Token        string `json:"token"`
		Hostname     string `json:"hostname"`
		AutoDownload *bool  `json:"auto_download"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json"})
		return
	}
	s.cfg.Lock()
	defer s.cfg.Unlock()
	t := s.cfg.Tunnel
	if body.Enabled != nil {
		t.Enabled = *body.Enabled
	}
	if body.Mode != "" {
		t.Mode = body.Mode
	}
	// An empty token in the request keeps the stored one so it never has to
	// be re-entered (and is never echoed back to the browser).
	if body.Token != "" {
		t.Token = strings.TrimSpace(body.Token)
	}
	t.Hostname = strings.TrimSpace(body.Hostname)
	if body.AutoDownload != nil {
		t.AutoDownload = *body.AutoDownload
	}
	if err := config.ValidateTunnel(&t); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.cfg.Tunnel = t
	if err := s.cfg.Save(); err != nil {
		writeErr(w, err)
		return
	}
	sess := sessionFrom(r.Context())
	s.log.Info("tunnel settings changed", "user", sess.Username, "enabled", t.Enabled, "mode", t.Mode)
	s.tunnel.Start(t)
	writeJSON(w, 200, s.tunnel.Status())
}

// apiTailscale updates the embedded Tailscale node settings and restarts it.
func (s *Server) apiTailscale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled    *bool   `json:"enabled"`
		Hostname   string  `json:"hostname"`
		Funnel     *bool   `json:"funnel"`
		AuthKey    string  `json:"auth_key"`
		ControlURL *string `json:"control_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json"})
		return
	}
	s.cfg.Lock()
	defer s.cfg.Unlock()
	t := s.cfg.Tailscale
	if body.Enabled != nil {
		t.Enabled = *body.Enabled
	}
	if body.Hostname != "" {
		t.Hostname = body.Hostname
	}
	if body.Funnel != nil {
		t.Funnel = *body.Funnel
	}
	if body.AuthKey != "" {
		t.AuthKey = strings.TrimSpace(body.AuthKey)
	}
	if body.ControlURL != nil {
		t.ControlURL = strings.TrimSpace(*body.ControlURL)
	}
	if err := config.ValidateTailscale(&t); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.cfg.Tailscale = t
	if err := s.cfg.Save(); err != nil {
		writeErr(w, err)
		return
	}
	sess := sessionFrom(r.Context())
	s.log.Info("tailscale settings changed", "user", sess.Username, "enabled", t.Enabled, "funnel", t.Funnel)
	// Restart asynchronously: stopping the node can take a few seconds.
	go s.tail.Start(t, s.Handler())
	writeJSON(w, 200, map[string]any{"state": "starting", "enabled": t.Enabled, "funnel": t.Funnel, "hostname": t.Hostname})
}

func (s *Server) apiTunnelLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": s.tunnel.Status(), "log": s.tunnel.Log()})
}

func (s *Server) apiAddUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json"})
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || len(body.Password) < 8 {
		writeJSON(w, 400, map[string]string{"error": "username required and password must be at least 8 characters"})
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if u := s.cfg.FindUser(body.Username); u != nil {
		u.PasswordHash, u.Admin = hash, body.Admin
	} else {
		s.cfg.Users = append(s.cfg.Users, config.User{Username: body.Username, PasswordHash: hash, Admin: body.Admin})
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": body.Username})
}

func (s *Server) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || len(body.Password) < 8 {
		writeJSON(w, 400, map[string]string{"error": "password must be at least 8 characters"})
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.cfg.Lock()
	defer s.cfg.Unlock()
	u := s.cfg.FindUser(sess.Username)
	if u == nil {
		writeJSON(w, 404, map[string]string{"error": "user not found"})
		return
	}
	u.PasswordHash = hash
	if err := s.cfg.Save(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "password changed"})
}
