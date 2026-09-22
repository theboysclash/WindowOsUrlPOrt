package vm

import (
	"strings"
	"sync"
)

// ring keeps the last n lines of QEMU output for display in the UI. It
// implements io.Writer so it can be wired directly to the process.
type ring struct {
	mu   sync.Mutex
	max  int
	buf  []string
	part string
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.part + string(p)
	parts := strings.Split(s, "\n")
	r.part = parts[len(parts)-1]
	for _, l := range parts[:len(parts)-1] {
		r.push(strings.TrimRight(l, "\r"))
	}
	return len(p), nil
}

func (r *ring) push(l string) {
	if l == "" {
		return
	}
	r.buf = append(r.buf, l)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

func (r *ring) add(l string) {
	r.mu.Lock()
	r.push(l)
	r.mu.Unlock()
}

func (r *ring) reset() {
	r.mu.Lock()
	r.buf, r.part = nil, ""
	r.mu.Unlock()
}

func (r *ring) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.buf))
	copy(out, r.buf)
	return out
}

func (r *ring) tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.part != "" {
		r.push(r.part)
		r.part = ""
	}
	if n > len(r.buf) {
		n = len(r.buf)
	}
	return strings.Join(r.buf[len(r.buf)-n:], " | ")
}
