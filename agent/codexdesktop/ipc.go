package codexdesktop

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxFrame = 8 << 20

var errOwnerUnavailable = errors.New("desktop-thread-owner-unavailable")

var methodVersions = map[string]int{"initialize": 0, "thread-owner-discovery": 1, "thread-follower-load-complete-history": 1, "thread-follower-start-turn": 2, "thread-follower-submit-user-input": 1, "thread-follower-steer-turn": 1}

type wire map[string]any

func obj(v any) wire {
	switch m := v.(type) {
	case wire:
		return m
	case map[string]any:
		return wire(m)
	default:
		return nil
	}
}
func str(v any) string { s, _ := v.(string); return s }
func list(v any) []any { s, _ := v.([]any); return s }
func encodeFrame(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || len(b) > maxFrame {
		return nil, fmt.Errorf("invalid-frame-size")
	}
	out := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out, nil
}
func readFrame(r io.Reader) (wire, error) {
	h := make([]byte, 4)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(h)
	if n == 0 || n > maxFrame {
		return nil, fmt.Errorf("invalid-frame-size")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var m wire
	if !utf8.Valid(b) || json.Unmarshal(b, &m) != nil || m == nil {
		return nil, fmt.Errorf("invalid-envelope")
	}
	return m, nil
}
func verifySocket(path string) (uint64, uint64, error) {
	if !filepath.IsAbs(path) {
		return 0, 0, fmt.Errorf("absolute-socket-required")
	}
	uid := uint32(os.Getuid())
	parent := filepath.Dir(path)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return 0, 0, fmt.Errorf("socket-unavailable")
	}
	for _, start := range []string{parent, real} {
		for p := start; ; p = filepath.Dir(p) {
			s, err := os.Lstat(p)
			if err != nil {
				return 0, 0, fmt.Errorf("unsafe-socket-path")
			}
			st, ok := s.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, 0, fmt.Errorf("unix-required")
			}
			if s.Mode()&os.ModeSymlink != 0 {
				if st.Uid != 0 || (p != "/tmp" && p != "/var") {
					return 0, 0, fmt.Errorf("unsafe-symlink")
				}
			} else {
				sticky := st.Uid == 0 && s.Mode()&os.ModeSticky != 0
				if !s.IsDir() || (st.Uid != 0 && st.Uid != uid) || (s.Mode().Perm()&0022 != 0 && !sticky) {
					return 0, 0, fmt.Errorf("unsafe-socket-ancestor")
				}
			}
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	p, err := os.Lstat(parent)
	if err != nil {
		return 0, 0, fmt.Errorf("socket-unavailable")
	}
	ps := p.Sys().(*syscall.Stat_t)
	s, err := os.Lstat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("socket-unavailable")
	}
	st := s.Sys().(*syscall.Stat_t)
	if !p.IsDir() || ps.Uid != uid || p.Mode().Perm()&0022 != 0 || s.Mode()&os.ModeSocket == 0 || st.Uid != uid || s.Mode().Perm()&0022 != 0 {
		return 0, 0, fmt.Errorf("unsafe-socket")
	}
	return uint64(st.Dev), st.Ino, nil
}
func endpoint(explicit string) (string, error) {
	if explicit != "" {
		_, _, err := verifySocket(explicit)
		return explicit, err
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".codex")
	}
	legacy := fmt.Sprintf("ipc-%d.sock", os.Getuid())
	if os.Getuid() == 0 {
		legacy = "ipc.sock"
	}
	for _, p := range []string{filepath.Join(home, "ipc", "ipc.sock"), filepath.Join(os.TempDir(), "codex-ipc", legacy)} {
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		}
		_, _, err := verifySocket(p)
		return p, err
	}
	return "", fmt.Errorf("socket-unavailable")
}

type rpcWait struct {
	method, owner string
	ch            chan wire
}
type ipcClient struct {
	conn        net.Conn
	mu          sync.Mutex
	writeMu     sync.Mutex
	pending     map[string]rpcWait
	clientID    string
	failure     error
	onBroadcast func(wire)
	timeout     time.Duration
}

func connectIPC(ctx context.Context, path string, timeout time.Duration) (*ipcClient, error) {
	dev, ino, err := verifySocket(path)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("socket-connect-failed")
	}
	a, b, err := verifySocket(path)
	if err != nil || a != dev || b != ino {
		conn.Close()
		return nil, fmt.Errorf("socket-changed")
	}
	c := &ipcClient{conn: conn, pending: map[string]rpcWait{}, clientID: "initializing-client", timeout: timeout}
	go c.readLoop()
	return c, nil
}
func (c *ipcClient) abort(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
	}
	for id, w := range c.pending {
		close(w.ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.conn.Close()
}
func (c *ipcClient) readLoop() {
	for {
		m, err := readFrame(c.conn)
		if err != nil {
			c.abort(fmt.Errorf("ipc-disconnected"))
			return
		}
		c.mu.Lock()
		if str(m["type"]) == "response" {
			id := str(m["requestId"])
			if w, ok := c.pending[id]; ok {
				w.ch <- m
				delete(c.pending, id)
			}
		}
		listener := c.onBroadcast
		c.mu.Unlock()
		if str(m["type"]) == "broadcast" && listener != nil {
			listener(m)
		}
	}
}
func (c *ipcClient) send(v any) error {
	b, err := encodeFrame(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err = c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return fmt.Errorf("ipc-write-deadline: %w", err)
	}
	for len(b) > 0 {
		n, err := c.conn.Write(b)
		if err != nil {
			return fmt.Errorf("ipc-write-uncertain")
		}
		b = b[n:]
	}
	return nil
}
func (c *ipcClient) rpc(ctx context.Context, method string, params wire, owner string) (wire, error) {
	v, ok := methodVersions[method]
	if !ok {
		return nil, fmt.Errorf("unsupported-operation")
	}
	id := uuid.NewString()
	ch := make(chan wire, 1)
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return nil, err
	}
	source := c.clientID
	if method != "initialize" && source == "initializing-client" {
		c.mu.Unlock()
		return nil, fmt.Errorf("not-initialized")
	}
	c.pending[id] = rpcWait{method, owner, ch}
	c.mu.Unlock()
	m := wire{"type": "request", "requestId": id, "sourceClientId": source, "version": v, "method": method, "params": params, "timeoutMs": int(c.timeout.Milliseconds())}
	if owner != "" {
		m["targetClientId"] = owner
	}
	if err := c.send(m); err != nil {
		c.abort(err)
		return nil, err
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case response, open := <-ch:
		if !open {
			return nil, fmt.Errorf("ipc-disconnected")
		}
		if str(response["method"]) != method || (owner != "" && str(response["handledByClientId"]) != owner) {
			return nil, fmt.Errorf("response-owner-mismatch")
		}
		if str(response["resultType"]) != "success" {
			code := str(response["error"])
			switch code {
			case "no-client-found", "client-disconnected", "unsupported-version", "version-mismatch", "client-cannot-handle-request":
				return nil, fmt.Errorf("%s", code)
			}
			return nil, fmt.Errorf("ipc-request-rejected")
		}
		return response, nil
	case <-timer.C:
		c.abort(fmt.Errorf("ipc-timeout"))
		return nil, fmt.Errorf("ipc-timeout")
	case <-ctx.Done():
		c.abort(fmt.Errorf("ipc-cancelled"))
		return nil, fmt.Errorf("ipc-cancelled")
	}
}

type attachment struct {
	mu               sync.Mutex
	c                *ipcClient
	thread, owner    string
	revision, latest float64
	state            wire
	invalid          bool
}

func (a *attachment) observe(m wire) {
	a.mu.Lock()
	defer a.mu.Unlock()
	method := str(m["method"])
	p := obj(m["params"])
	if method == "ipc-connection-reset" || method == "client-status-changed" && str(p["clientId"]) == a.owner && str(p["status"]) == "disconnected" {
		a.invalid = true
		return
	}
	if method != "thread-stream-state-changed" || str(m["sourceClientId"]) != a.owner || str(p["conversationId"]) != a.thread || str(p["hostId"]) != "local" {
		return
	}
	if targets, exists := m["targetClientIds"]; exists {
		found := false
		for _, v := range list(targets) {
			if str(v) == a.c.clientID {
				found = true
			}
		}
		if !found {
			return
		}
	}
	change := obj(p["change"])
	revision, ok := change["revision"].(float64)
	if m["version"] != float64(11) || !ok || revision < 0 || revision != float64(int64(revision)) {
		a.invalid = true
		return
	}
	if revision > a.latest {
		a.latest = revision
	}
	if str(change["type"]) == "snapshot" && obj(change["conversationState"]) != nil && revision >= a.revision {
		a.state = obj(change["conversationState"])
		a.revision = revision
	}
}
func (a *attachment) snapshot(ctx context.Context) (wire, error) {
	ack, err := a.c.rpc(ctx, "thread-follower-load-complete-history", wire{"conversationId": a.thread}, a.owner)
	if err != nil {
		return nil, err
	}
	rev, ok := obj(ack["result"])["revision"].(float64)
	if !ok || rev < 0 || rev != float64(int64(rev)) {
		return nil, fmt.Errorf("invalid-history-ack")
	}
	deadline := time.Now().Add(a.c.timeout)
	for {
		a.mu.Lock()
		valid := !a.invalid && a.state != nil && a.revision >= rev && a.revision >= a.latest
		state := a.state
		invalid := a.invalid
		a.mu.Unlock()
		if invalid {
			return nil, fmt.Errorf("unsupported-state-or-owner-lost")
		}
		if valid {
			if _, err := turnsOf(state, a.thread); err != nil {
				return nil, err
			}
			return state, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("snapshot-stale")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func attachIPC(ctx context.Context, socket, thread string, timeout time.Duration) (*attachment, error) {
	path, err := endpoint(socket)
	if err != nil {
		return nil, err
	}
	c, err := connectIPC(ctx, path, timeout)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			c.abort(fmt.Errorf("detached"))
		}
	}()
	hello, err := c.rpc(ctx, "initialize", wire{"clientType": "Brodge-codex-desktop"}, "")
	if err != nil {
		return nil, err
	}
	id := str(obj(hello["result"])["clientId"])
	if id == "" {
		return nil, fmt.Errorf("invalid-initialize")
	}
	c.mu.Lock()
	c.clientID = id
	c.mu.Unlock()
	owner, err := c.rpc(ctx, "thread-owner-discovery", wire{"hostId": "local", "conversationId": thread}, "")
	if err != nil {
		switch err.Error() {
		case "no-client-found", "client-disconnected", "client-cannot-handle-request", "ipc-timeout", "ipc-disconnected":
			return nil, fmt.Errorf("%w: %w", errOwnerUnavailable, err)
		default:
			return nil, err
		}
	}
	ownerID := str(owner["handledByClientId"])
	if ownerID == "" {
		return nil, errOwnerUnavailable
	}
	a := &attachment{c: c, thread: thread, owner: ownerID, revision: -1, latest: -1}
	c.mu.Lock()
	c.onBroadcast = a.observe
	c.mu.Unlock()
	if err := c.send(wire{"type": "broadcast", "method": "thread-stream-following-changed", "version": 1, "sourceClientId": id, "targetClientIds": []string{ownerID}, "params": wire{"conversationId": thread, "hostId": "local", "following": true}}); err != nil {
		return nil, err
	}
	if _, err := a.snapshot(ctx); err != nil {
		return nil, err
	}
	failed = false
	return a, nil
}
