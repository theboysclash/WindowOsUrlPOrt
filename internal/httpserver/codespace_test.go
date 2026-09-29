package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/auth"
)

func TestCodespaceForwarded(t *testing.T) {
	var got *http.Request
	h := codespaceForwarded(func() string { return "x-8443.app.github.dev" }, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r }))

	r := httptest.NewRequest("GET", "http://127.0.0.1:5555/login", nil)
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if got.Host != "x-8443.app.github.dev" {
		t.Errorf("Host = %q", got.Host)
	}
	if ip := auth.ClientIP(got); ip != "203.0.113.9" {
		t.Errorf("ClientIP = %q", ip)
	}
	if !originMatchesHost("https://x-8443.app.github.dev", got.Host) {
		t.Error("origin check would reject the public URL")
	}
}
