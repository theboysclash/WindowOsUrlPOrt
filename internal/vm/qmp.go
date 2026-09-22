package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// QMP is a minimal client for QEMU's machine protocol. Responses are matched
// to requests by an incrementing id; asynchronous events are delivered on
// Events.
type QMP struct {
	conn   net.Conn
	enc    *json.Encoder
	mu     sync.Mutex
	nextID int
	// pending maps request ids to the channel awaiting the reply.
	pending map[int]chan qmpResponse
	Events  chan QMPEvent
	closed  chan struct{}
	err     error
}

type QMPEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	ID    *int            `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// DialQMP connects to a QMP TCP socket, retrying until ctx expires.
func DialQMP(ctx context.Context, addr string) (*QMP, error) {
	var conn net.Conn
	var err error
	for {
		d := net.Dialer{Timeout: time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("qmp connect %s: %w", addr, err)
		case <-time.After(200 * time.Millisecond):
		}
	}

	q := &QMP{
		conn:    conn,
		enc:     json.NewEncoder(conn),
		pending: make(map[int]chan qmpResponse),
		Events:  make(chan QMPEvent, 64),
		closed:  make(chan struct{}),
	}

	// The server speaks first with a greeting; read it before negotiating.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	if _, err := r.ReadBytes('\n'); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp greeting: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	go q.readLoop(r)

	if _, err := q.Execute(ctx, "qmp_capabilities", nil); err != nil {
		q.Close()
		return nil, err
	}
	return q, nil
}

func (q *QMP) readLoop(r *bufio.Reader) {
	defer close(q.closed)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			q.mu.Lock()
			q.err = err
			for id, ch := range q.pending {
				close(ch)
				delete(q.pending, id)
			}
			q.mu.Unlock()
			close(q.Events)
			return
		}
		var resp qmpResponse
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		if resp.Event != "" {
			select {
			case q.Events <- QMPEvent{Event: resp.Event, Data: resp.Data}:
			default:
			}
			continue
		}
		if resp.ID == nil {
			continue
		}
		q.mu.Lock()
		ch, ok := q.pending[*resp.ID]
		if ok {
			delete(q.pending, *resp.ID)
		}
		q.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// Execute sends a command and waits for its reply.
func (q *QMP) Execute(ctx context.Context, cmd string, args any) (json.RawMessage, error) {
	q.mu.Lock()
	if q.err != nil {
		q.mu.Unlock()
		return nil, q.err
	}
	q.nextID++
	id := q.nextID
	ch := make(chan qmpResponse, 1)
	q.pending[id] = ch
	req := map[string]any{"execute": cmd, "id": id}
	if args != nil {
		req["arguments"] = args
	}
	err := q.enc.Encode(req)
	q.mu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("qmp connection closed")
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s: %s", cmd, resp.Error.Class, resp.Error.Desc)
		}
		return resp.Return, nil
	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, ctx.Err()
	}
}

// HMP runs a human-monitor command (used for savevm/loadvm which have no QMP
// equivalent) and returns its text output.
func (q *QMP) HMP(ctx context.Context, command string) (string, error) {
	raw, err := q.Execute(ctx, "human-monitor-command", map[string]any{"command-line": command})
	if err != nil {
		return "", err
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
}

func (q *QMP) Close() error {
	err := q.conn.Close()
	<-q.closed
	return err
}
