package relay

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelayEndToEnd(t *testing.T) {
	rs := NewServer("secret-key", quietLog())
	ts := httptest.NewServer(rs)
	defer ts.Close()

	// Offline before the host connects.
	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "not connected") {
		t.Fatalf("offline: got %d %q", resp.StatusCode, body)
	}

	app := http.NewServeMux()
	app.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "host="+r.Host+" remote="+r.RemoteAddr+" leaked="+r.Header.Get(ClientIPHeader))
	})
	app.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		c.Write(r.Context(), typ, append([]byte("echo:"), msg...))
	})

	cl := NewClient(quietLog())
	if err := cl.Start(ts.URL+"#secret-key", app); err != nil {
		t.Fatal(err)
	}
	defer cl.Stop()
	waitFor(t, "host connected", rs.Connected)
	if st := cl.Status(); st.State != StateConnected || st.URL != ts.URL {
		t.Fatalf("client status %+v", st)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/login", nil)
	req.Header.Set("X-Forwarded-Host", "demo-8080.app.github.dev")
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.7, 10.1.2.3")
	req.Header.Set(ClientIPHeader, "1.2.3.4")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	want := "host=demo-8080.app.github.dev remote=203.0.113.7:0 leaked="
	if string(body) != want {
		t.Fatalf("got %q, want %q", body, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wc, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wc.CloseNow()
	if err := wc.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := wc.Read(ctx)
	if err != nil || string(msg) != "echo:hi" {
		t.Fatalf("websocket echo: %q %v", msg, err)
	}

	// Host goes away: relay shows the offline page again.
	cl.Stop()
	waitFor(t, "host disconnected", func() bool { return !rs.Connected() })
}

func TestRelayRejectsWrongKey(t *testing.T) {
	rs := NewServer("right", quietLog())
	ts := httptest.NewServer(rs)
	defer ts.Close()

	cl := NewClient(quietLog())
	if err := cl.Start(ts.URL+"#wrong", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	defer cl.Stop()
	waitFor(t, "error state", func() bool { return cl.Status().State == StateError })
	if st := cl.Status(); !strings.Contains(st.Error, "rejected the key") {
		t.Fatalf("unexpected error %q", st.Error)
	}
	if rs.Connected() {
		t.Fatal("relay accepted a wrong key")
	}
}

func TestParseLink(t *testing.T) {
	u, k, err := ParseLink("  https://abc-8080.app.github.dev/#KEY123 ")
	if err != nil || u != "https://abc-8080.app.github.dev" || k != "KEY123" {
		t.Fatalf("got %q %q %v", u, k, err)
	}
	for _, bad := range []string{"https://abc.app.github.dev", "ftp://x#k", "#k", "nothing"} {
		if _, _, err := ParseLink(bad); err == nil {
			t.Errorf("ParseLink(%q) should fail", bad)
		}
	}
}
