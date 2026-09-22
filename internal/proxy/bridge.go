package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn adapts a gorilla WebSocket to io.ReadWriter using binary frames, the
// same framing websockify and noVNC use.
type wsConn struct {
	ws  *websocket.Conn
	buf []byte
	wmu sync.Mutex
}

func (c *wsConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		c.buf = data
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Options controls a single bridged session.
type Options struct {
	VNCAddr     string
	VNCPassword string
	ViewOnly    bool
	// SessionValid is polled; when it returns false the bridge is torn down.
	SessionValid func() bool
	// OnTraffic is invoked periodically while data flows (used to keep the
	// HTTP session from idling out while the console is in use).
	OnTraffic func()
}

// Bridge authenticates to the VNC server, completes a password-less handshake
// with the browser and then relays traffic until either side disconnects.
func Bridge(ctx context.Context, ws *websocket.Conn, opts Options) error {
	vnc, err := net.DialTimeout("tcp", opts.VNCAddr, 5*time.Second)
	if err != nil {
		return err
	}
	defer vnc.Close()
	if tc, ok := vnc.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	if err := authToServer(vnc, opts.VNCPassword); err != nil {
		return err
	}
	client := &wsConn{ws: ws}
	if err := handshakeWithClient(client); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 3)

	go func() {
		// server -> browser: opaque copy in reasonably sized frames
		buf := make([]byte, 64*1024)
		for {
			n, err := vnc.Read(buf)
			if n > 0 {
				if _, werr := client.Write(buf[:n]); werr != nil {
					errc <- werr
					return
				}
				if opts.OnTraffic != nil {
					opts.OnTraffic()
				}
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		errc <- copyClientMessages(vnc, client, opts.ViewOnly)
	}()
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if opts.SessionValid != nil && !opts.SessionValid() {
					errc <- errSessionExpired
					return
				}
				_ = ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()

	err = <-errc
	cancel()
	_ = vnc.Close()
	_ = ws.Close()
	if errors.Is(err, io.EOF) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
		return nil
	}
	return err
}

var errSessionExpired = errors.New("session expired")

// IsSessionExpired reports whether the bridge ended because the HTTP session
// timed out.
func IsSessionExpired(err error) bool { return errors.Is(err, errSessionExpired) }
