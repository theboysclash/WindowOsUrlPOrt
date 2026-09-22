package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeVNC implements just enough of RFB 3.8 with VNC authentication to
// exercise the proxy: handshake, ServerInit, and echoing every client message
// back wrapped in a fake FramebufferUpdate so the test can see what got
// through the filter.
func fakeVNC(t *testing.T, password string, got chan<- []byte) (addr string, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte(rfbVersion))
				ver := make([]byte, 12)
				io.ReadFull(c, ver)
				c.Write([]byte{1, secTypeVNCAuth})
				choice := make([]byte, 1)
				io.ReadFull(c, choice)
				challenge := []byte("0123456789abcdef")
				c.Write(challenge)
				resp := make([]byte, 16)
				io.ReadFull(c, resp)
				want, _ := vncEncrypt(password, challenge)
				if string(want) != string(resp) {
					c.Write([]byte{0, 0, 0, 1})
					reason := "Authentication failed"
					l := make([]byte, 4)
					binary.BigEndian.PutUint32(l, uint32(len(reason)))
					c.Write(l)
					c.Write([]byte(reason))
					return
				}
				c.Write([]byte{0, 0, 0, 0})
				init := make([]byte, 1)
				io.ReadFull(c, init)
				name := "fake"
				si := make([]byte, 24)
				binary.BigEndian.PutUint16(si[0:], 640)
				binary.BigEndian.PutUint16(si[2:], 480)
				binary.BigEndian.PutUint32(si[20:], uint32(len(name)))
				c.Write(append(si, []byte(name)...))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						cp := make([]byte, n)
						copy(cp, buf[:n])
						got <- cp
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func wsPair(t *testing.T, opts Options) (*websocket.Conn, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = Bridge(context.Background(), ws, opts)
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws, func() { ws.Close(); srv.Close() }
}

// wsReader reassembles a byte stream from WebSocket frames, keeping any
// bytes beyond what the caller asked for.
type wsReader struct {
	ws   *websocket.Conn
	rest []byte
}

func (r *wsReader) readN(t *testing.T, n int) []byte {
	r.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for len(r.rest) < n {
		_, m, err := r.ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (have %d/%d bytes)", err, len(r.rest), n)
		}
		r.rest = append(r.rest, m...)
	}
	out := r.rest[:n]
	r.rest = r.rest[n:]
	return out
}

func clientHandshake(t *testing.T, ws *websocket.Conn) {
	r := &wsReader{ws: ws}
	if v := r.readN(t, 12); string(v) != rfbVersion {
		t.Fatalf("version %q", v)
	}
	ws.WriteMessage(websocket.BinaryMessage, []byte(rfbVersion))
	if sec := r.readN(t, 2); sec[0] != 1 || sec[1] != secTypeNone {
		t.Fatalf("security types %v", sec)
	}
	ws.WriteMessage(websocket.BinaryMessage, []byte{secTypeNone})
	if res := r.readN(t, 4); binary.BigEndian.Uint32(res) != 0 {
		t.Fatalf("security result %v", res)
	}
	ws.WriteMessage(websocket.BinaryMessage, []byte{1})
	si := r.readN(t, 24)
	if binary.BigEndian.Uint16(si) != 640 {
		t.Fatalf("ServerInit width %d", binary.BigEndian.Uint16(si))
	}
	if name := r.readN(t, int(binary.BigEndian.Uint32(si[20:]))); string(name) != "fake" {
		t.Fatalf("desktop name %q", name)
	}
}

func TestBridgeForwardsInput(t *testing.T) {
	got := make(chan []byte, 16)
	addr, stop := fakeVNC(t, "s3cret", got)
	defer stop()
	ws, done := wsPair(t, Options{VNCAddr: addr, VNCPassword: "s3cret"})
	defer done()
	clientHandshake(t, ws)

	key := []byte{msgKeyEvent, 1, 0, 0, 0, 0, 0, 0x41}
	ws.WriteMessage(websocket.BinaryMessage, key)
	select {
	case b := <-got:
		if string(b) != string(key) {
			t.Fatalf("server got %v want %v", b, key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("key event not forwarded")
	}
}

func TestBridgeViewOnlyDropsInput(t *testing.T) {
	got := make(chan []byte, 16)
	addr, stop := fakeVNC(t, "s3cret", got)
	defer stop()
	ws, done := wsPair(t, Options{VNCAddr: addr, VNCPassword: "s3cret", ViewOnly: true})
	defer done()
	clientHandshake(t, ws)

	ws.WriteMessage(websocket.BinaryMessage, []byte{msgKeyEvent, 1, 0, 0, 0, 0, 0, 0x41})
	ws.WriteMessage(websocket.BinaryMessage, []byte{msgPointerEvent, 1, 0, 5, 0, 5})
	ws.WriteMessage(websocket.BinaryMessage, []byte{msgClientCutText, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'})
	fbur := []byte{msgFramebufferUpdateRequest, 0, 0, 0, 0, 0, 0, 10, 0, 10}
	ws.WriteMessage(websocket.BinaryMessage, fbur)

	select {
	case b := <-got:
		if string(b) != string(fbur) {
			t.Fatalf("server got %v, want only the update request %v", b, fbur)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("update request not forwarded")
	}
}

func TestBridgeWrongPassword(t *testing.T) {
	got := make(chan []byte, 16)
	addr, stop := fakeVNC(t, "s3cret", got)
	defer stop()
	ws, done := wsPair(t, Options{VNCAddr: addr, VNCPassword: "nope"})
	defer done()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("expected the bridge to close the socket on auth failure")
	}
}

func TestClientMessageSizes(t *testing.T) {
	cases := []struct {
		msg  []byte
		want int
	}{
		{[]byte{msgSetPixelFormat, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}, 19},
		{[]byte{msgSetEncodings, 0, 0, 2, 0, 0, 0, 7, 0, 0, 0, 5}, 11},
		{[]byte{msgClientCutText, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}, 10},
		{[]byte{msgQEMU, 0, 0, 1, 0, 0, 0, 0x41, 0, 0, 0, 0x1e}, 11},
		{[]byte{msgSetDesktopSize, 0, 3, 32, 2, 88, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 32, 2, 88, 0, 0, 0, 0}, 23},
	}
	for _, c := range cases {
		r := &countingReader{Reader: strings.NewReader(string(c.msg[1:]))}
		n, err := clientMessageSize(c.msg[0], r)
		if err != nil {
			t.Fatalf("type %d: %v", c.msg[0], err)
		}
		if n != c.want {
			t.Errorf("type %d: size %d want %d", c.msg[0], n, c.want)
		}
	}
}
