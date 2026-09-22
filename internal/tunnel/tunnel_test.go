package tunnel

import "testing"

func TestQuickURLRegex(t *testing.T) {
	line := "2026-09-22T16:01:25Z INF |  https://unable-suggest-stating-pure.trycloudflare.com   |"
	if got := quickURLRe.FindString(line); got != "https://unable-suggest-stating-pure.trycloudflare.com" {
		t.Fatalf("got %q", got)
	}
	apiLine := `failed to request quick Tunnel: Post "https://api.trycloudflare.com/tunnel": timeout`
	if got := quickURLRe.FindString(apiLine); got != "https://api.trycloudflare.com" {
		t.Fatalf("api line parsed as %q", got)
	}
}

func TestArgs(t *testing.T) {
	m := NewManager(configFor("quick", ""), ".", ".", "https://127.0.0.1:8443", nil)
	args := m.args(m.cfg)
	if args[0] != "tunnel" || !has(args, "--url") || !has(args, "https://127.0.0.1:8443") || !has(args, "--no-tls-verify") {
		t.Fatalf("quick args: %v", args)
	}
	m = NewManager(configFor("named", "tok"), ".", ".", "https://127.0.0.1:8443", nil)
	args = m.args(m.cfg)
	if !has(args, "run") || !has(args, "--token") || !has(args, "tok") || has(args, "--url") {
		t.Fatalf("named args: %v", args)
	}
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
