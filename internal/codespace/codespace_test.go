package codespace

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
)

// fakeGH mimics the gh subcommands the manager uses and records every call.
const fakeGH = `#!/bin/sh
echo "$*" >> "$CALLS"
case "$1 $2" in
  "auth status") echo "  - Token scopes: 'codespace', 'repo'"; exit 0 ;;
  "codespace list") echo "[]" ;;
  "codespace create") echo "Codespace usage is paid for by you" >&2; echo "fuzzy-relay-abc123" ;;
  "codespace ssh") exec sleep 30 ;;
  "codespace ports")
    if [ "$3" = "visibility" ]; then exit 0; fi
    echo '[{"sourcePort":8443,"browseUrl":"https://fuzzy-relay-abc123-8443.app.github.dev/"}]' ;;
  "codespace stop") exit 0 ;;
esac
`

func TestLaunchFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	if _, err := findSSH(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("CALLS", calls)

	cfg := config.Codespace{Enabled: true, BinaryPath: bin}
	if err := config.ValidateCodespace(&cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, dir, dir, 45678, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Start(cfg)
	deadline := time.Now().Add(20 * time.Second)
	for m.Status().State != StateConnected {
		if time.Now().After(deadline) {
			t.Fatalf("not connected: %+v\nlog: %v", m.Status(), m.Log())
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := m.Status()
	if st.URL != "https://fuzzy-relay-abc123-8443.app.github.dev" || st.Codespace != "fuzzy-relay-abc123" {
		t.Fatalf("status = %+v", st)
	}
	if got := m.PublicHost(); got != "fuzzy-relay-abc123-8443.app.github.dev" {
		t.Fatalf("PublicHost = %q", got)
	}
	m.Stop()

	raw, _ := os.ReadFile(calls)
	log := string(raw)
	for _, want := range []string{
		"codespace create --repo github/codespaces-blank --machine basicLinux32gb",
		"codespace ssh -c fuzzy-relay-abc123 -- -N -T",
		"-R 8443:127.0.0.1:45678",
		"codespace ports visibility 8443:public -c fuzzy-relay-abc123",
		"codespace stop -c fuzzy-relay-abc123",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing call %q in:\n%s", want, log)
		}
	}
	if saved, _ := os.ReadFile(filepath.Join(dir, "codespace-name.txt")); string(saved) != "fuzzy-relay-abc123" {
		t.Errorf("saved name = %q", saved)
	}
}

func TestIsGHEntry(t *testing.T) {
	name := exeName("gh")
	for entry, want := range map[string]bool{
		"gh_2.60.0_linux_amd64/bin/" + name: true,
		"bin/" + name:                       true,
		"gh_2.60.0_linux_amd64/share/gh":    false,
		"LICENSE":                           false,
	} {
		if got := isGHEntry(entry); got != want {
			t.Errorf("isGHEntry(%q) = %v", entry, got)
		}
	}
}
