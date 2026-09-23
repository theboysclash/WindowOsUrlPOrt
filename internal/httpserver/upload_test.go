package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/auth"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/vm"
)

func uploadServer(t *testing.T) (*Server, string) {
	dir := t.TempDir()
	cfg := config.Default(dir)
	s := &Server{cfg: cfg, vm: vm.NewManager(cfg.VM, dir, slog.Default()), log: slog.Default()}
	return s, filepath.Join(filepath.Dir(cfg.VM.DiskPath), "isos")
}

func postChunk(s *Server, name string, offset, total int, data []byte) (*httptest.ResponseRecorder, map[string]any) {
	url := fmt.Sprintf("/api/upload/iso?name=%s&offset=%d&total=%d", name, offset, total)
	r := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	r = r.WithContext(withSession(context.Background(), &auth.Session{Username: "admin", Admin: true}))
	w := httptest.NewRecorder()
	s.apiUploadISO(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestUploadISOInChunks(t *testing.T) {
	s, dir := uploadServer(t)
	data := []byte("0123456789abcdef")

	if w, out := postChunk(s, "tiny10.iso", 0, len(data), data[:10]); w.Code != 200 || out["done"] != false {
		t.Fatalf("first chunk: %d %v", w.Code, out)
	}
	// A retried chunk at the same offset must replace, not duplicate, data.
	if w, _ := postChunk(s, "tiny10.iso", 0, len(data), data[:10]); w.Code != 200 {
		t.Fatalf("retry: %d", w.Code)
	}
	w, out := postChunk(s, "tiny10.iso", 10, len(data), data[10:])
	if w.Code != 200 || out["done"] != true {
		t.Fatalf("last chunk: %d %v", w.Code, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "tiny10.iso"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("file content %q err %v", got, err)
	}
	if out["path"] != filepath.Join(dir, "tiny10.iso") {
		t.Fatalf("path %v", out["path"])
	}
}

func TestUploadISORejects(t *testing.T) {
	s, _ := uploadServer(t)
	if w, _ := postChunk(s, "setup.exe", 0, 4, []byte("abcd")); w.Code != 400 {
		t.Fatalf("non-iso accepted: %d", w.Code)
	}
	if w, _ := postChunk(s, "a.iso", 50, 100, []byte("abcd")); w.Code != 409 {
		t.Fatalf("gap accepted: %d", w.Code)
	}
}

func TestCleanISOName(t *testing.T) {
	cases := map[string]string{
		"Tiny10 23H1 x64.iso":           "Tiny10_23H1_x64.iso",
		`C:\Users\me\Downloads\win.ISO`: "win.ISO",
		"../../etc/evil.iso":            "evil.iso",
	}
	for in, want := range cases {
		got, err := cleanISOName(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".iso", "x.exe", "..iso"} {
		if _, err := cleanISOName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
