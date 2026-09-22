package httpserver

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tailnet"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tunnel"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/vm"
)

// Templates are executed with the same data types the handlers use so that a
// renamed field or a pointer-receiver method fails the test instead of
// truncating the page at runtime.
func TestTemplatesRender(t *testing.T) {
	cfg := config.Default(t.TempDir())
	log := slog.Default()
	vmm := vm.NewManager(cfg.VM, t.TempDir(), log)
	s, err := New(cfg, nil, vmm, tunnel.NewManager(cfg.Tunnel, ".", ".", "https://127.0.0.1:8443", log), tailnet.NewManager(cfg.Tailscale, ".", nil, log), log)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{
		"login.html":   loginData{Error: "bad", Next: "/console"},
		"console.html": consoleData{User: "admin", Admin: true, VMStatus: vmm.Status(), SetupNeed: true},
		"setup.html": setupData{
			User: "admin", Config: cfg.VM, Status: vmm.Status(), Tunnel: cfg.Tunnel, TStatus: s.tunnel.Status(),
			Origin: "https://localhost:8443", TS: cfg.Tailscale, TSStatus: s.tail.Status(),
		},
	}
	for name, data := range cases {
		var buf bytes.Buffer
		if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(buf.String(), "</html>") {
			t.Errorf("%s: output truncated", name)
		}
	}
}
