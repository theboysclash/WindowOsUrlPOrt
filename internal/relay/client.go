package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

type State string

const (
	StateDisabled   State = "disabled"
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateError      State = "error"
)

type Status struct {
	State State  `json:"state"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// ParseLink splits the "https://host#key" link printed by the relay.
func ParseLink(link string) (publicURL, key string, err error) {
	link = strings.TrimSpace(link)
	base, key, ok := strings.Cut(link, "#")
	if !ok || key == "" {
		return "", "", errors.New(`relay link must look like https://NAME-8080.app.github.dev#KEY (copy the whole line the relay prints)`)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", "", fmt.Errorf("relay link %q is not an http(s) URL", base)
	}
	return u.Scheme + "://" + u.Host, key, nil
}

// Client is the host side: it keeps a connection to the relay open and serves
// handler on every stream the relay opens.
type Client struct {
	log *slog.Logger

	mu     sync.Mutex
	st     Status
	cancel context.CancelFunc
	done   chan struct{}
}

func NewClient(log *slog.Logger) *Client {
	return &Client{log: log, st: Status{State: StateDisabled}}
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

func (c *Client) set(st Status) {
	c.mu.Lock()
	c.st = st
	c.mu.Unlock()
}

// Start connects to the relay in the background, reconnecting until Stop.
func (c *Client) Start(link string, handler http.Handler) error {
	publicURL, key, err := ParseLink(link)
	if err != nil {
		return err
	}
	c.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.mu.Lock()
	c.cancel, c.done = cancel, done
	c.st = Status{State: StateConnecting, URL: publicURL}
	c.mu.Unlock()

	go func() {
		defer close(done)
		backoff := 2 * time.Second
		for ctx.Err() == nil {
			start := time.Now()
			err := c.session(ctx, publicURL, key, handler)
			if ctx.Err() != nil {
				return
			}
			if time.Since(start) > time.Minute {
				backoff = 2 * time.Second
			}
			msg := "connection lost"
			if err != nil {
				msg = err.Error()
			}
			c.log.Warn("relay", "err", msg, "retry_in", backoff)
			c.set(Status{State: StateError, URL: publicURL, Error: msg})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			c.set(Status{State: StateConnecting, URL: publicURL, Error: msg})
		}
	}()
	return nil
}

func (c *Client) session(ctx context.Context, publicURL, key string, handler http.Handler) error {
	wsURL := "wss" + strings.TrimPrefix(publicURL, "https") + AgentPath
	if strings.HasPrefix(publicURL, "http://") {
		wsURL = "ws" + strings.TrimPrefix(publicURL, "http") + AgentPath
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + key}},
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return errors.New("the relay rejected the key; copy the link again from the Codespace")
			case http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable:
				return fmt.Errorf("relay not reachable (HTTP %d); is the Codespace running and port 8080 public?", resp.StatusCode)
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return errors.New("the relay redirected to a sign-in page; make port 8080 Public in the Codespace")
			}
		}
		return fmt.Errorf("connect to relay: %w", err)
	}
	conn.SetReadLimit(-1)
	nc := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	sess, err := yamux.Server(nc, muxConfig())
	if err != nil {
		nc.Close()
		return err
	}
	defer sess.Close()

	c.log.Info("relay connected", "url", publicURL)
	c.set(Status{State: StateConnected, URL: publicURL})

	hs := &http.Server{
		Handler:           trustRelay(handler),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(c.log.Handler(), slog.LevelDebug),
	}
	stop := context.AfterFunc(ctx, func() { hs.Close() })
	defer stop()
	err = hs.Serve(sess)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, yamux.ErrSessionShutdown) {
		return nil
	}
	return err
}

// trustRelay takes the viewer address the relay attached. Only requests that
// arrived over the authenticated relay session pass through here.
func trustRelay(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := net.ParseIP(r.Header.Get(ClientIPHeader))
		r.Header.Del(ClientIPHeader)
		if ip != nil {
			r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
		} else {
			r.RemoteAddr = "relay:0"
		}
		next.ServeHTTP(w, r)
	})
}

// Stop disconnects from the relay.
func (c *Client) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel, c.done = nil, nil
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	c.set(Status{State: StateDisabled})
}
