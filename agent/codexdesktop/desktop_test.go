package codexdesktop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type fakeDesktop struct {
	mu              sync.Mutex
	states          map[string]wire
	starts, answers int
	dropStart       bool
	dropSteer       bool
	version         int
	listener        net.Listener
	conns           []net.Conn
}

func idleState(thread string) wire {
	return wire{"id": thread, "hostId": "local", "turns": []any{}, "requests": []any{}, "resumeState": "resumed", "threadRuntimeStatus": wire{"type": "idle"}}
}
func newFake(t *testing.T) (*fakeDesktop, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "db-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "ipc.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0600)
	f := &fakeDesktop{states: map[string]wire{"T1": idleState("T1"), "T2": idleState("T2")}, version: 11, listener: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
	})
	return f, path
}
func (f *fakeDesktop) serve(c net.Conn) {
	defer c.Close()
	for {
		m, err := readFrame(c)
		if err != nil {
			return
		}
		if str(m["type"]) != "request" {
			continue
		}
		method := str(m["method"])
		params := obj(m["params"])
		thread := str(params["conversationId"])
		result := wire{}
		owner := "owner"
		f.mu.Lock()
		switch method {
		case "initialize":
			result = wire{"clientId": "test-client"}
		case "thread-owner-discovery":
			if f.states[thread] == nil {
				f.mu.Unlock()
				return
			}
		case "thread-follower-load-complete-history":
			state := f.states[thread]
			b, _ := json.Marshal(state)
			var snapshot wire
			_ = json.Unmarshal(b, &snapshot)
			broadcast := wire{"type": "broadcast", "method": "thread-stream-state-changed", "version": f.version, "sourceClientId": owner, "targetClientIds": []string{"test-client"}, "params": wire{"conversationId": thread, "hostId": "local", "change": wire{"type": "snapshot", "revision": 1, "conversationState": snapshot}}}
			encoded, _ := encodeFrame(broadcast)
			_, _ = c.Write(encoded)
			result = wire{"revision": 1}
		case "thread-follower-start-turn":
			f.starts++
			request := obj(obj(params["turnStart"])["request"])
			id := fmt.Sprintf("turn%d", f.starts)
			turn := wire{"turnId": id, "status": "inProgress", "params": wire{"clientUserMessageId": request["clientUserMessageId"]}, "items": []any{}}
			f.states[thread]["turns"] = append(list(f.states[thread]["turns"]), turn)
			if f.dropStart {
				f.mu.Unlock()
				return
			}
			result = wire{"result": wire{"turn": wire{"id": id}}}
		case "thread-follower-submit-user-input":
			f.answers++
			result = wire{"ok": true}
		case "thread-follower-steer-turn":
			f.answers++
			turns := list(f.states[thread]["turns"])
			turn := obj(turns[len(turns)-1])
			turn["items"] = append(list(turn["items"]), wire{"type": "steeringUserMessage", "id": "answer-item", "status": "accepted", "targetTurnId": turn["turnId"], "clientUserMessageId": params["clientUserMessageId"], "input": params["input"]})
			if f.dropSteer {
				f.mu.Unlock()
				return
			}
			result = wire{"result": wire{"turnId": turn["turnId"]}}
		default:
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		b, _ := encodeFrame(wire{"type": "response", "requestId": m["requestId"], "method": method, "handledByClientId": owner, "resultType": "success", "result": result})
		for len(b) > 0 {
			n := 7
			if len(b) < n {
				n = len(b)
			}
			if _, err = c.Write(b[:n]); err != nil {
				return
			}
			b = b[n:]
		}
	}
}
func (f *fakeDesktop) finish(thread, text string, nativeError bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	turns := list(f.states[thread]["turns"])
	t := obj(turns[len(turns)-1])
	t["status"] = "completed"
	items := []any{wire{"type": "agentMessage", "phase": "final_answer", "text": text}}
	if nativeError {
		items = append(items, wire{"type": "error", "message": "content_filter; stream disconnected before completion"})
	}
	t["items"] = items
	f.states[thread]["requests"] = []any{}
}

type fakePlatform struct {
	mu      sync.Mutex
	sent    []delivery
	failure error
}
type delivery struct {
	o          core.ConversationOrigin
	uuid, text string
}

func (p *fakePlatform) AcceptConversationOrigin(core.ConversationOrigin) bool { return true }

func (p *fakePlatform) Name() string                    { return "feishu" }
func (p *fakePlatform) Start(core.MessageHandler) error { return nil }
func (p *fakePlatform) Stop() error                     { return nil }
func (p *fakePlatform) ReplyConversation(_ context.Context, o core.ConversationOrigin, id, text string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, delivery{o, id, text})
	if p.failure != nil {
		return "", p.failure
	}
	return "bot-" + id, nil
}
func testAgent(t *testing.T, socket string) *Agent {
	t.Helper()
	a, err := New(map[string]any{"protocol": "local-v11-start-v2", "language": "zh", "allowed_threads": []string{"T1", "T2"}, "state_dir": filepath.Join(t.TempDir(), "state"), "socket": socket})
	if err != nil {
		t.Fatal(err)
	}
	a.timeout = 200 * time.Millisecond
	a.openThread = nil // Unit tests must never navigate the user's Desktop.
	a.prepareNew = func(context.Context, string, []core.ConversationMedia) (desktopDraft, error) {
		return nil, fmt.Errorf("test-ui-unavailable")
	}
	t.Cleanup(func() { _ = a.Stop() })
	return a
}
func incoming(actor, chat, id, body string) *core.Message {
	o := core.ConversationOrigin{AppID: "app", Tenant: "tenant", Actor: actor, ChatID: chat, ChatType: "p2p", MessageID: id}
	return &core.Message{SessionKey: chat + ":" + actor, Content: body, MessageID: id, Platform: "feishu", ConversationOrigin: &o}
}
func accept(t *testing.T, a *Agent, p *fakePlatform, msg *core.Message) {
	t.Helper()
	if err := a.HandleConversation(context.Background(), p, msg); err != nil {
		t.Fatal(err)
	}
	if err := a.consumeInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func step(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func count(t *testing.T, s *store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type fakeDraft struct {
	submit func() (string, error)
	closed bool
}

func (d *fakeDraft) Submit(context.Context) (string, error) { return d.submit() }
func (d *fakeDraft) Close()                                 { d.closed = true }

func uiTurn(thread, prompt string) wire {
	s := idleState(thread)
	s["workspaceKind"] = "projectless"
	s["turns"] = []any{wire{"turnId": "ui-turn-" + thread, "status": "inProgress", "params": wire{"clientUserMessageId": "ui-message-" + thread, "input": []any{wire{"type": "text", "text": prompt}}}, "items": []any{}}}
	return s
}

func TestCUJ_PairBeforeBindingAndSwitchExistingChats(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	o := *incoming("A", "DM", "pair", "code").ConversationOrigin
	if err := a.BindPairedConversation(context.Background(), p, o, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleConversation(context.Background(), p, incoming("A", "DM", "pair", "secret-pair-code")); err != nil {
		t.Fatal(err)
	}
	if count(t, a.store, `SELECT count(*) FROM inbox WHERE state='paired'`) != 1 {
		t.Fatal("pair not registered")
	}
	if err := a.BindConversation(context.Background(), originKey(o), "codex://threads/T1?hostId=local"); err != nil {
		t.Fatal(err)
	}
	accept(t, a, p, incoming("A", "DM", "bind2", "/bind T2"))
	accept(t, a, p, incoming("A", "DM", "list", "/list"))
	accept(t, a, p, incoming("A", "DM", "switch", "/switch T1"))
	accept(t, a, p, incoming("A", "DM", "current", "/current"))
	accept(t, a, p, incoming("A", "DM", "new", "/new"))
	for i := 0; i < 4; i++ {
		step(t, a)
	}
	if count(t, a.store, `SELECT count(*) FROM jobs`) != 0 || f.starts != 0 {
		t.Fatal("command or pairing submitted a model turn")
	}
	var listReply, current string
	for _, r := range p.sent {
		if r.o.MessageID == "list" {
			listReply = r.text
		}
		if r.o.MessageID == "current" {
			current = r.text
		}
	}
	if !strings.Contains(listReply, "T1") || !strings.Contains(listReply, "* T2") || !strings.Contains(current, "T1") {
		t.Fatal("list or current did not track switching")
	}
	if count(t, a.store, `SELECT count(*) FROM binding_threads`) != 2 {
		t.Fatal("switch discarded previous conversation")
	}
}

func TestCUJ_FirstCreationFollowupsAndOriginalSource(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	prepares, sends := 0, 0
	a.prepareNew = func(_ context.Context, prompt string, _ []core.ConversationMedia) (desktopDraft, error) {
		prepares++
		return &fakeDraft{submit: func() (string, error) {
			sends++
			f.mu.Lock()
			f.states["T3"] = uiTurn("T3", prompt)
			f.mu.Unlock()
			return "codex://threads/T3?hostId=local", nil
		}}, nil
	}
	first := incoming("A", "DM", "first", "remember pine47")
	for _, m := range []*core.Message{first, first, incoming("A", "DM", "second", "recall"), incoming("A", "DM", "third", "increment")} {
		if err := a.HandleConversation(context.Background(), p, m); err != nil {
			t.Fatal(err)
		}
	}
	step(t, a)
	if prepares != 1 || sends != 1 || f.starts != 0 || count(t, a.store, `SELECT count(*) FROM jobs WHERE turn='ui-turn-T3' AND state='running'`) != 1 {
		t.Fatal("creation sent the first input twice or failed to adopt it")
	}
	step(t, a)
	if f.starts != 0 {
		t.Fatal("second input overtook first turn")
	}
	f.finish("T3", "remembered", false)
	step(t, a)
	step(t, a)
	if f.starts != 1 {
		t.Fatal("second input did not continue created chat")
	}
	f.finish("T3", "pine47", false)
	step(t, a)
	step(t, a)
	if f.starts != 2 {
		t.Fatal("third input did not continue created chat")
	}
	f.finish("T3", "pine48", false)
	step(t, a)
	if prepares != 1 || sends != 1 || count(t, a.store, `SELECT count(*) FROM jobs WHERE thread='T3' AND state='finished'`) != 3 {
		t.Fatal("incorrect creation/followup count")
	}
	for _, r := range p.sent {
		if r.o.ChatID != "DM" || r.o.Actor != "A" {
			t.Fatal("result left original source")
		}
	}
	if len(p.sent) != 3 {
		t.Fatalf("got %d replies", len(p.sent))
	}
}

func TestCUJ_CreationUnknownSendRecoversWithoutResending(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	sends := 0
	a.prepareNew = func(_ context.Context, prompt string, _ []core.ConversationMedia) (desktopDraft, error) {
		return &fakeDraft{submit: func() (string, error) {
			sends++
			f.mu.Lock()
			f.states["T3"] = uiTurn("T3", prompt)
			f.mu.Unlock()
			return "", fmt.Errorf("lost-ui-link")
		}}, nil
	}
	accept(t, a, p, incoming("A", "DM", "first", "original"))
	step(t, a)
	id := "ingress:app:tenant:first"
	if err := a.store.recover(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		step(t, a)
	}
	if err := a.RetryConversationCreation(context.Background(), id, ""); err == nil {
		t.Fatal("unknown send retried")
	}
	if err := a.RetryConversationCreation(context.Background(), id, "T1"); err == nil {
		t.Fatal("unrelated existing chat adopted")
	}
	if count(t, a.store, `SELECT count(*) FROM bindings`) != 0 {
		t.Fatal("invalid candidate changed binding")
	}
	if err := a.RetryConversationCreation(context.Background(), id, "T3"); err != nil {
		t.Fatal(err)
	}
	f.finish("T3", "result", false)
	step(t, a)
	if sends != 1 || f.starts != 0 || count(t, a.store, `SELECT count(*) FROM jobs WHERE state='finished' AND thread='T3'`) != 1 {
		t.Fatal("recovery resubmitted original")
	}
}

func TestCUJ_CreationWaitsForFirstTurnEvidenceWithoutResending(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	sends := 0
	a.prepareNew = func(_ context.Context, prompt string, _ []core.ConversationMedia) (desktopDraft, error) {
		return &fakeDraft{submit: func() (string, error) {
			sends++
			state := uiTurn("T3", prompt)
			state["turns"] = []any{}
			f.mu.Lock()
			f.states["T3"] = state
			f.mu.Unlock()
			return "T3", nil
		}}, nil
	}
	accept(t, a, p, incoming("A", "DM", "first", "original"))
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE state='create_verifying'`) != 1 || count(t, a.store, `SELECT count(*) FROM bindings`) != 0 {
		t.Fatal("empty initial snapshot was adopted or stopped evidence recovery")
	}
	if err := a.store.recover(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.states["T3"] = uiTurn("T3", "original")
	f.mu.Unlock()
	step(t, a)
	f.finish("T3", "result", false)
	step(t, a)
	if sends != 1 || f.starts != 0 || count(t, a.store, `SELECT count(*) FROM jobs WHERE state='finished' AND thread='T3'`) != 1 {
		t.Fatal("evidence recovery resubmitted or lost first turn")
	}
}

func TestCUJ_CreationBeforeSendFailureAndOtherEntryProgress(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	prepares := 0
	a.prepareNew = func(context.Context, string, []core.ConversationMedia) (desktopDraft, error) {
		prepares++
		return nil, fmt.Errorf("permission-required")
	}
	accept(t, a, p, incoming("A", "DM", "first", "first"))
	step(t, a)
	for i := 0; i < 2; i++ {
		step(t, a)
	}
	if prepares != 1 {
		t.Fatal("failed preparation repeated UI actions")
	}
	accept(t, a, p, incoming("A", "GROUP", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "GROUP", "group-input", "group"))
	step(t, a)
	if f.starts != 1 {
		t.Fatal("failed creation blocked unrelated bound chat")
	}
	id := "ingress:app:tenant:first"
	if err := a.RetryConversationCreation(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	if prepares != 2 {
		t.Fatal("explicit safe retry not processed")
	}
	if _, err := a.store.db.Exec(`UPDATE jobs SET state='create_preparing' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := a.store.recover(); err != nil {
		t.Fatal(err)
	}
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE id=? AND state='create_failed'`, id) != 1 {
		t.Fatal("preparing recovery unsafe")
	}
	if _, err := a.store.db.Exec(`UPDATE jobs SET state='create_sending' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := a.store.recover(); err != nil {
		t.Fatal(err)
	}
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE id=? AND state='create_uncertain'`, id) != 1 {
		t.Fatal("sending recovery unsafe")
	}
}

func TestCUJ_CreationAndManualBindingKeepAcceptedDestination(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	a.prepareNew = func(_ context.Context, prompt string, _ []core.ConversationMedia) (desktopDraft, error) {
		return &fakeDraft{submit: func() (string, error) {
			f.mu.Lock()
			f.states["T3"] = uiTurn("T3", prompt)
			f.mu.Unlock()
			return "T3", nil
		}}, nil
	}
	first := incoming("A", "DM", "first", "initial")
	if err := a.HandleConversation(context.Background(), p, first); err != nil {
		t.Fatal(err)
	}
	if err := a.BindConversation(context.Background(), originKey(*first.ConversationOrigin), "T2"); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM bindings WHERE thread='T2'`) != 1 || count(t, a.store, `SELECT count(*) FROM jobs WHERE thread='T3'`) != 1 {
		t.Fatal("creation overwrote manual binding or first input retargeted")
	}
	accept(t, a, p, incoming("A", "DM", "after", "later"))
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE thread='T2'`) != 1 {
		t.Fatal("later message ignored manual binding")
	}
}

func TestCUJ_CreationRevalidatesAuthorizationBeforeSend(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &scopePlatform{fakePlatform: &fakePlatform{}, allowed: true}
	sends := 0
	a.prepareNew = func(context.Context, string, []core.ConversationMedia) (desktopDraft, error) {
		p.allowed = false
		return &fakeDraft{submit: func() (string, error) { sends++; return "T3", nil }}, nil
	}
	if err := a.HandleConversation(context.Background(), p, incoming("A", "DM", "first", "original")); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	if sends != 0 || count(t, a.store, `SELECT count(*) FROM bindings`) != 0 {
		t.Fatal("revoked origin submitted UI input")
	}
}

func TestFirstCreationEvidenceRejectsChangedInputAndProject(t *testing.T) {
	for _, tc := range []string{"text", "client", "project", "multiple"} {
		t.Run(tc, func(t *testing.T) {
			s := uiTurn("T3", "hello")
			turn := obj(list(s["turns"])[0])
			params := obj(turn["params"])
			switch tc {
			case "text":
				params["input"] = []any{wire{"type": "text", "text": "different"}}
			case "client":
				delete(params, "clientUserMessageId")
			case "project":
				s["workspaceKind"] = "project"
			case "multiple":
				s["turns"] = append(list(s["turns"]), wire{"turnId": "other", "status": "completed", "params": wire{}, "items": []any{}})
			}
			if _, err := firstCreationTurn(s, "T3", "hello", nil); err == nil {
				t.Fatal("uncertain evidence accepted")
			}
		})
	}
}

func TestCreationDefaultShortcutsRejectRemappingAndCollisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keybindings.json")
	if err := checkCreationKeymap(path); err != nil {
		t.Fatal("absent user overrides rejected", err)
	}
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`[]`, true},
		{`[{"command":"newProjectlessTask","key":"Command+Option+O"}]`, true},
		{`[{"command":"copyDeeplink","key":"CmdOrCtrl+Alt+L"}]`, true},
		{`[{"command":"unrelated","key":"Cmd+J"}]`, true},
		{`[{"command":"newProjectlessTask","key":null}]`, false},
		{`[{"command":"copyDeeplink","key":"Cmd+J"}]`, false},
		{`[{"command":"-newProjectlessTask","key":"Command+Alt+O"}]`, false},
		{`[{"command":"unrelated","key":"Option+Command+L"}]`, false},
		{`invalid-json`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkCreationKeymap(path); (err == nil) != tc.ok {
			t.Fatalf("body %s: %v", tc.body, err)
		}
	}
}

func TestCUJ_FirstCreationPreservesMediaAndDoesNotResubmit(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &mediaPlatform{data: []byte("original media bytes")}
	sends := 0
	a.prepareNew = func(_ context.Context, prompt string, media []core.ConversationMedia) (desktopDraft, error) {
		if len(media) != 2 || media[0].Kind != "image" || media[1].Kind != "file" {
			t.Fatal("first input resources lost")
		}
		return &fakeDraft{submit: func() (string, error) {
			sends++
			state := uiTurn("T3", prompt)
			obj(obj(list(state["turns"])[0])["params"])["input"] = []any{wire{"type": "text", "text": "# Files mentioned\n" + media[1].Path + "\n" + prompt}, wire{"type": "localImage", "path": media[0].Path}}
			f.mu.Lock()
			f.states["T3"] = state
			f.mu.Unlock()
			return "T3", nil
		}}, nil
	}
	m := incoming("A", "DM", "first-media", "read both")
	m.Media = []core.ConversationMedia{{Kind: "image", Key: "image-key", Name: "image.png"}, {Kind: "file", Key: "file-key", Name: "file.txt"}}
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	f.finish("T3", "both read", false)
	step(t, a)
	if p.downloads != 2 || sends != 1 || f.starts != 0 || count(t, a.store, `SELECT count(*) FROM jobs WHERE state='finished'`) != 1 {
		t.Fatal("media first turn was lost or repeated")
	}
}

func TestCUJ_TwoNewGroupEntriesKeepIndependentBindings(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	sends := 0
	a.prepareNew = func(_ context.Context, prompt string, _ []core.ConversationMedia) (desktopDraft, error) {
		thread := "T3"
		if prompt == "group two" {
			thread = "T4"
		}
		return &fakeDraft{submit: func() (string, error) {
			sends++
			f.mu.Lock()
			f.states[thread] = uiTurn(thread, prompt)
			f.mu.Unlock()
			return thread, nil
		}}, nil
	}
	for i, chat := range []string{"G1", "G2"} {
		prompt := "group one"
		if i == 1 {
			prompt = "group two"
		}
		m := incoming("A", chat, "first-"+chat, prompt)
		m.ConversationOrigin.ChatType = "group"
		if err := a.HandleConversation(context.Background(), p, m); err != nil {
			t.Fatal(err)
		}
	}
	step(t, a)
	step(t, a)
	if sends != 2 || count(t, a.store, `SELECT count(*) FROM bindings`) != 2 {
		t.Fatal("group creation not independent")
	}
	f.finish("T3", "group-one-result", false)
	f.finish("T4", "group-two-result", false)
	for i := 0; i < 3; i++ {
		step(t, a)
	}
	for _, r := range p.sent {
		if r.text == "group-one-result" && r.o.ChatID != "G1" || r.text == "group-two-result" && r.o.ChatID != "G2" {
			t.Fatal("cross-group result")
		}
	}
	accept(t, a, p, incoming("A", "G2", "cross-switch", "/switch T3"))
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM bindings WHERE origin=? AND thread='T4'`, originKey(*incoming("A", "G2", "x", "").ConversationOrigin)) != 1 {
		t.Fatal("foreign group thread was switched")
	}
}

func TestThreadLinksRequireLocalHost(t *testing.T) {
	for _, s := range []string{"codex://threads/T1?hostId=remote", "codex://someone@threads/T1", "codex://threads/T1/extra", "codex://other/T1"} {
		if _, err := parseThread(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestCUJ_BindChatQuestionSwitchAndOriginalReply(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	e := core.NewEngine("desktop", a, []core.Platform{p})
	receive := func(m *core.Message) {
		e.ReceiveMessage(p, m)
		if err := a.consumeInbox(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	receive(incoming("A", "DM", "import", "/bind T2"))
	receive(incoming("A", "DM", "bind", "/bind codex://threads/T1"))
	receive(incoming("A", "DM", "m1", "你好"))
	step(t, a)
	receive(incoming("A", "DM", "switch", "/switch T2"))
	f.finish("T1", "你想做什么？", false)
	step(t, a)
	for i := 0; i < 5; i++ {
		step(t, a)
	}
	var result delivery
	p.mu.Lock()
	for _, d := range p.sent {
		if strings.Contains(d.text, "你想做什么") {
			result = d
		}
	}
	p.mu.Unlock()
	if result.o.MessageID != "m1" {
		t.Fatalf("wrong result origin: %#v", result)
	}
	answer := incoming("A", "DM", "m2", "写一个函数")
	answer.ConversationOrigin.ParentID = "bot-" + result.uuid
	receive(answer)
	step(t, a)
	var target string
	if err := a.store.db.QueryRow(`SELECT thread FROM jobs ORDER BY rowid DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "T1" {
		t.Fatal("explicit reply did not continue T1")
	}
	var current string
	_ = a.store.db.QueryRow(`SELECT thread FROM bindings`).Scan(&current)
	if current != "T2" {
		t.Fatal("reply changed binding")
	}
	f.mu.Lock()
	starts := f.starts
	f.mu.Unlock()
	if starts != 2 {
		t.Fatalf("starts=%d", starts)
	}
}
func TestDedupConflictRejectsChangedOriginAndBody(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	m := incoming("A", "DM", "same", "hello")
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	other := incoming("B", "other", "same", "hello")
	if err := a.HandleConversation(context.Background(), p, other); err == nil {
		t.Fatal("changed source accepted")
	}
	m.Content = "changed"
	if err := a.HandleConversation(context.Background(), p, m); err == nil {
		t.Fatal("changed body accepted")
	}
	if count(t, a.store, `SELECT count(*) FROM inbox`) != 1 {
		t.Fatal("duplicate persisted")
	}
}
func TestLostStartAckReconcilesWithoutResending(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "msg", "hello"))
	f.mu.Lock()
	f.dropStart = true
	f.mu.Unlock()
	step(t, a)
	if err := a.store.recover(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		step(t, a)
	}
	f.mu.Lock()
	n := f.starts
	f.mu.Unlock()
	if n != 1 {
		t.Fatal("uncertain start resent")
	}
	f.finish("T1", "done", false)
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE state='finished'`) != 1 {
		t.Fatal("accepted turn not reconciled")
	}
}
func TestUncertainWithoutEvidenceBlocksSameThread(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m1", "hello"))
	j, err := a.store.claim("generation1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = a.store.db.Exec(`UPDATE jobs SET state='dispatch' WHERE id=?`, j.ID)
	_ = a.store.recover()
	accept(t, a, p, incoming("A", "DM", "m2", "second"))
	if _, err = a.store.claim("generation2"); err != sql.ErrNoRows {
		t.Fatal("uncertain dispatch released thread")
	}
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE state='uncertain'`) != 1 {
		t.Fatal("not marked uncertain")
	}
}
func TestTwoOriginsSameThreadSerialize(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	for _, actor := range []string{"A", "B"} {
		accept(t, a, p, incoming(actor, actor, "bind"+actor, "/bind T1"))
		accept(t, a, p, incoming(actor, actor, "msg"+actor, "hello"))
	}
	step(t, a)
	step(t, a)
	f.mu.Lock()
	starts := f.starts
	f.mu.Unlock()
	if starts != 1 {
		t.Fatal("concurrent starts")
	}
	f.finish("T1", "first", false)
	step(t, a)
	step(t, a)
	f.mu.Lock()
	starts = f.starts
	f.mu.Unlock()
	if starts != 2 {
		t.Fatal("queue did not advance")
	}
}
func TestCompletedWithErrorDoesNotReportSuccess(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	step(t, a)
	f.finish("T1", "done", true)
	step(t, a)
	var category, body string
	_ = a.store.db.QueryRow(`SELECT error FROM jobs`).Scan(&category)
	_ = a.store.db.QueryRow(`SELECT body FROM outbox WHERE kind='result'`).Scan(&body)
	if category != "content-filter" || !strings.Contains(body, "返回错误") {
		t.Fatal(category, body)
	}
}
func TestNewAndFailedSwitchPreserveBinding(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "new", "/new"))
	accept(t, a, p, incoming("A", "DM", "switch", "/switch unknown"))
	var thread string
	_ = a.store.db.QueryRow(`SELECT thread FROM bindings`).Scan(&thread)
	if thread != "T1" {
		t.Fatal("binding lost")
	}
}
func TestOutboxStableUuidAndExpiredUncertaintyHeld(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{failure: fmt.Errorf("unknown")}
	accept(t, a, p, incoming("A", "DM", "m", "/current"))
	step(t, a)
	_, _ = a.store.db.Exec(`UPDATE outbox SET next_attempt=0`)
	step(t, a)
	p.mu.Lock()
	if len(p.sent) != 2 || p.sent[0].uuid != p.sent[1].uuid {
		t.Fatal("retry UUID changed")
	}
	p.mu.Unlock()
	_, _ = a.store.db.Exec(`UPDATE outbox SET first_attempt=?,next_attempt=0`, time.Now().Unix()-3600)
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE state='reconcile'`) != 1 {
		t.Fatal("expired uncertainty resent")
	}
}
func TestNativeAnswerAckAloneNeverResolves(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	a.nativeAnswers = true
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	step(t, a)
	f.mu.Lock()
	turn := obj(list(f.states["T1"]["turns"])[0])
	f.states["T1"]["requests"] = []any{wire{"id": "request1", "method": "item/tool/requestUserInput", "params": wire{"threadId": "T1", "turnId": turn["turnId"], "questions": []any{wire{"id": "qid", "question": "选哪个？", "options": []any{}}}}}}
	f.mu.Unlock()
	step(t, a)
	accept(t, a, p, incoming("A", "DM", "answer", "ALPHA"))
	step(t, a)
	step(t, a)
	f.mu.Lock()
	answers := f.answers
	f.mu.Unlock()
	if answers != 1 {
		t.Fatal("answer replayed")
	}
	if count(t, a.store, `SELECT count(*) FROM questions WHERE state='resolved'`) != 0 {
		t.Fatal("ACK falsely resolved")
	}
	f.finish("T1", "end", false)
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM questions WHERE state='closed'`) != 1 {
		t.Fatal("terminal did not close request")
	}
}
func TestUnknownProtocolVersionFailsBeforeSend(t *testing.T) {
	f, socket := newFake(t)
	f.mu.Lock()
	f.version = 12
	f.mu.Unlock()
	_, err := attachIPC(context.Background(), socket, "T1", 100*time.Millisecond)
	if err == nil {
		t.Fatal("unknown version accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 {
		t.Fatal("sent on unknown protocol")
	}
}
func TestSocketPermissionsAndFraming(t *testing.T) {
	_, socket := newFake(t)
	if _, _, err := verifySocket(socket); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(socket, 0666)
	if _, _, err := verifySocket(socket); err == nil {
		t.Fatal("unsafe socket accepted")
	}
	_, err := readFrame(strings.NewReader("\x01\x00\x00\x00{"))
	if err == nil {
		t.Fatal("invalid JSON accepted")
	}
	_, err = readFrame(strings.NewReader("\x01\x00"))
	if err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
}
func TestClaimOwnerGenerationFence(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	old, e := a.store.claim("generation1")
	if e != nil {
		t.Fatal(e)
	}
	_ = a.store.recover()
	_, e = a.store.claim("generation2")
	if e != nil {
		t.Fatal(e)
	}
	r, e := a.store.db.Exec(`UPDATE jobs SET state='dispatch' WHERE id=? AND state='claimed' AND owner=?`, old.ID, old.Owner)
	if e != nil {
		t.Fatal(e)
	}
	n, _ := r.RowsAffected()
	if n != 0 {
		t.Fatal("old owner unfenced")
	}
}

func TestCrashRecoveryCommittedIntent(t *testing.T) {
	if os.Getenv("DESKTOP_CRASH_CHILD") == "1" {
		s, err := openStore(os.Getenv("DESKTOP_CRASH_DB"))
		if err != nil {
			os.Exit(3)
		}
		_, err = s.db.Exec(`UPDATE jobs SET state='dispatch' WHERE id='crash'`)
		if err != nil {
			os.Exit(4)
		}
		os.Exit(17)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO jobs(id,origin,source,platform,thread,revision,prompt,client_message,state) VALUES('crash','origin','{}','feishu','T1',1,'hello','client','claimed')`)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecoveryCommittedIntent$")
	cmd.Env = append(os.Environ(), "DESKTOP_CRASH_CHILD=1", "DESKTOP_CRASH_DB="+path)
	err = cmd.Run()
	if err == nil {
		t.Fatal("child did not crash")
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	_ = s.recover()
	if count(t, s, `SELECT count(*) FROM jobs WHERE state='uncertain'`) != 1 {
		t.Fatal("dispatch replay allowed")
	}
}

func TestAcceptedMessageKeepsBindingAcrossWebSwitch(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	m := incoming("A", "DM", "text", "hello")
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	if err := a.BindConversation(context.Background(), originKey(*m.ConversationOrigin), "T2"); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	var thread string
	if err := a.store.db.QueryRow(`SELECT thread FROM jobs`).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	if thread != "T1" {
		t.Fatal("accepted text retargeted", thread)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(list(f.states["T2"]["turns"])) != 0 {
		t.Fatal("wrong Desktop conversation")
	}
}
func TestRapidBindThenTextUsesOrderedBinding(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	for _, m := range []*core.Message{incoming("A", "DM", "import", "/bind T2"), incoming("A", "DM", "bind", "/bind T1"), incoming("A", "DM", "text", "hello"), incoming("A", "DM", "switch", "/switch T2"), incoming("A", "DM", "next", "second")} {
		if err := a.HandleConversation(context.Background(), p, m); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := a.consumeInbox(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE prompt='hello' AND thread='T1'`) != 1 || count(t, a.store, `SELECT count(*) FROM jobs WHERE prompt='second' AND thread='T2'`) != 1 {
		t.Fatal("ordered bindings lost")
	}
}
func addQuestion(f *fakeDesktop, questions []any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	turn := obj(list(f.states["T1"]["turns"])[0])
	f.states["T1"]["requests"] = []any{wire{"id": "request1", "method": "item/tool/requestUserInput", "params": wire{"threadId": "T1", "turnId": turn["turnId"], "questions": questions}}}
}
func startQuestion(t *testing.T, questions []any) (*fakeDesktop, *Agent, *fakePlatform) {
	t.Helper()
	f, socket := newFake(t)
	a := testAgent(t, socket)
	a.nativeAnswers = true
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	step(t, a)
	addQuestion(f, questions)
	step(t, a)
	return f, a, p
}
func TestNativeAnswerDisappearedBeforeSubmissionIsClosed(t *testing.T) {
	f, a, p := startQuestion(t, []any{wire{"id": "q", "question": "choose"}})
	accept(t, a, p, incoming("A", "DM", "answer", "ALPHA"))
	f.mu.Lock()
	f.states["T1"]["requests"] = []any{}
	f.mu.Unlock()
	step(t, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answers != 0 {
		t.Fatal("stale answer submitted")
	}
	if count(t, a.store, `SELECT count(*) FROM questions WHERE state='closed'`) != 1 {
		t.Fatal("stale question open")
	}
}
func TestNativeAnswerEvidenceAndFinalReplyContinueConversation(t *testing.T) {
	f, a, p := startQuestion(t, []any{wire{"id": "q", "question": "choose"}})
	accept(t, a, p, incoming("A", "DM", "answer", "ALPHA"))
	step(t, a)
	f.mu.Lock()
	turn := obj(list(f.states["T1"]["turns"])[0])
	f.states["T1"]["requests"] = []any{}
	turn["items"] = []any{wire{"type": "userInputResponse", "requestId": "request1", "turnId": turn["turnId"], "completed": true, "answers": wire{"q": []any{"ALPHA"}}}}
	f.mu.Unlock()
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM questions WHERE state='resolved'`) != 1 {
		t.Fatal("answer evidence ignored")
	}
	f.finish("T1", "done", false)
	step(t, a)
	for i := 0; i < 8; i++ {
		step(t, a)
	}
	var resultID, questionID string
	if err := a.store.db.QueryRow(`SELECT message FROM outbox WHERE kind='result'`).Scan(&resultID); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.QueryRow(`SELECT message FROM outbox WHERE kind LIKE 'question:%'`).Scan(&questionID); err != nil {
		t.Fatal(err)
	}
	stale := incoming("A", "DM", "stale", "ALPHA")
	stale.ConversationOrigin.ParentID = questionID
	accept(t, a, p, stale)
	if count(t, a.store, `SELECT count(*) FROM jobs`) != 1 {
		t.Fatal("stale native reply became new turn")
	}
	next := incoming("A", "DM", "next", "continue")
	next.ConversationOrigin.ParentID = resultID
	accept(t, a, p, next)
	if count(t, a.store, `SELECT count(*) FROM jobs`) != 2 {
		t.Fatal("ordinary reply after native question rejected")
	}
}
func TestRepeatedQuestionTextUsesQuestionIDsAndRejectsOldReply(t *testing.T) {
	f, a, p := startQuestion(t, []any{wire{"id": "q1", "question": "same"}, wire{"id": "q2", "question": "same"}})
	for i := 0; i < 5; i++ {
		step(t, a)
	}
	var firstID string
	if err := a.store.db.QueryRow(`SELECT message FROM outbox WHERE kind='question:"request1":q1'`).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	answer := incoming("A", "DM", "a1", "ALPHA")
	answer.ConversationOrigin.ParentID = firstID
	accept(t, a, p, answer)
	duplicate := incoming("A", "DM", "old", "WRONG")
	duplicate.ConversationOrigin.ParentID = firstID
	accept(t, a, p, duplicate)
	if count(t, a.store, `SELECT count(*) FROM questions WHERE state='ready'`) != 0 {
		t.Fatal("old reply answered next question")
	}
	accept(t, a, p, incoming("A", "DM", "a2", "BETA"))
	step(t, a)
	var encoded string
	if err := a.store.db.QueryRow(`SELECT answers FROM questions`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if encoded != `{"q1":["ALPHA"],"q2":["BETA"]}` {
		t.Fatal(encoded)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answers != 1 {
		t.Fatal("answer count", f.answers)
	}
}
func TestUnrelatedDesktopOutputNeverDelivered(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	step(t, a)
	f.mu.Lock()
	f.states["T1"]["turns"] = append(list(f.states["T1"]["turns"]), wire{"turnId": "private", "status": "completed", "params": wire{"clientUserMessageId": "someone-else"}, "items": []any{wire{"type": "agentMessage", "text": "PRIVATE_CONTENT"}}})
	f.mu.Unlock()
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE body LIKE '%PRIVATE_CONTENT%'`) != 0 {
		t.Fatal("unrelated result leaked")
	}
}
func TestLongReplyChunksStableAndOrderedAcrossRetry(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{failure: fmt.Errorf("unknown")}
	m := incoming("A", "DM", "m", "hello")
	a.platforms[p.Name()] = p
	text := strings.Repeat("汉", 7200)
	j := job{ID: "long", Source: marshal(*m.ConversationOrigin), Platform: p.Name()}
	if err := a.store.notice(j, "result", text); err != nil {
		t.Fatal(err)
	}
	if err := a.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.db.Exec(`UPDATE outbox SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	if err := a.store.notice(j, "result", text); err != nil {
		t.Fatal(err)
	}
	if err := a.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	if len(p.sent) != 2 || p.sent[0].uuid != p.sent[1].uuid {
		t.Fatal("chunk skipped or UUID changed")
	}
	p.failure = nil
	p.mu.Unlock()
	if _, err := a.store.db.Exec(`UPDATE outbox SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := a.flushOutbox(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) != 5 {
		t.Fatal("chunk delivery count", len(p.sent))
	}
	var assembled string
	for _, d := range p.sent[2:] {
		if len([]rune(d.text)) > 3500 {
			t.Fatal("chunk too large")
		}
		assembled += d.text
	}
	if assembled != text || count(t, a.store, `SELECT count(*) FROM outbox`) != 3 {
		t.Fatal("chunk content changed")
	}
}
func TestRemovedThreadScopeBlocksRecoveredJobBeforeStart(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	if _, err := a.store.db.Exec(`DELETE FROM binding_threads WHERE thread='T1'`); err != nil {
		t.Fatal(err)
	}
	if err := a.store.recover(); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 || count(t, a.store, `SELECT count(*) FROM jobs WHERE state='blocked'`) != 1 {
		t.Fatal("scope removed but executed")
	}
}
func TestPersistenceFailureStopsBeforeDesktopWrite(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "hello"))
	j, err := a.store.claim(a.owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.db.Exec(`CREATE TRIGGER fail_intent BEFORE UPDATE ON jobs WHEN NEW.state='dispatch' BEGIN SELECT RAISE(ABORT,'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = a.dispatch(context.Background(), j); err == nil {
		t.Fatal("persistence failure swallowed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 {
		t.Fatal("write before durable intent")
	}
}

func TestOfflineConstructorAndSingleStateInstance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	opts := map[string]any{"protocol": "local-v11-start-v2", "state_dir": dir, "allowed_threads": []string{"T1"}, "socket": "/does/not/exist"}
	a, err := New(opts)
	if err != nil {
		t.Fatal("offline constructor failed", err)
	}
	defer a.Stop()
	if _, err = New(opts); err == nil {
		t.Fatal("second state consumer accepted")
	}
	for _, name := range []string{"bridge.sqlite", "bridge.sqlite-wal", "bridge.sqlite-shm", "service.lock"} {
		s, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if s.Mode().Perm()&0077 != 0 {
			t.Fatal("state readable by other users", name, s.Mode())
		}
	}
}
func TestSecretNativeQuestionNeverForwarded(t *testing.T) {
	state := idleState("T1")
	turn := wire{"turnId": "turn"}
	state["requests"] = []any{wire{"id": 77, "method": "item/tool/requestUserInput", "params": wire{"threadId": "T1", "turnId": "turn", "questions": []any{wire{"id": "q", "question": "secret", "is_secret": true}}}}}
	if _, err := questionRequests(state, turn); err == nil {
		t.Fatal("secret question forwarded")
	}
}

func TestRecoveredOriginScopeCannotBindExecuteOrDeliver(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &scopePlatform{fakePlatform: &fakePlatform{}, allowed: true}
	m := incoming("A", "DM", "bind", "/bind T1")
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	p.allowed = false
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM bindings`) != 0 {
		t.Fatal("revoked origin bound")
	}
	p.allowed = true
	if err := a.HandleConversation(context.Background(), p, incoming("A", "DM", "current", "/current")); err != nil {
		t.Fatal(err)
	}
	if err := a.consumeInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.allowed = false
	if err := a.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 || len(p.sent) != 0 || count(t, a.store, `SELECT count(*) FROM outbox WHERE state='blocked'`) != 1 {
		t.Fatal("revoked origin executed/delivered")
	}
}

type scopePlatform struct {
	*fakePlatform
	allowed bool
}

func (p *scopePlatform) AcceptConversationOrigin(core.ConversationOrigin) bool { return p.allowed }

func TestDesktopPairedChatBindsWithoutPromptOrReply(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &scopePlatform{fakePlatform: &fakePlatform{}, allowed: true}
	o := *incoming("A", "DM", "pair", "").ConversationOrigin
	if err := a.BindPairedConversation(context.Background(), p, o, "T1"); err != nil {
		t.Fatal(err)
	}
	if err := a.BindPairedConversation(context.Background(), p, o, "T1"); err != nil {
		t.Fatal("same pairing should be idempotent", err)
	}
	if err := a.HandleConversation(context.Background(), p, incoming("A", "DM", "pair", "BRIDGE_pairing_code")); err != nil {
		t.Fatal("redelivered pairing message should be acknowledged", err)
	}
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM jobs`) != 0 || count(t, a.store, `SELECT count(*) FROM outbox`) != 0 || count(t, a.store, `SELECT revision FROM bindings`) != 1 {
		t.Fatal("pairing created model work, reply or changed binding")
	}
	f.mu.Lock()
	starts := f.starts
	f.mu.Unlock()
	if starts != 0 || len(p.sent) != 0 {
		t.Fatal("pairing invoked Desktop or sent a message")
	}
	if err := a.BindPairedConversation(context.Background(), p, o, "T2"); err != nil {
		t.Fatal(err)
	}
	if count(t, a.store, `SELECT count(*) FROM bindings WHERE thread='T1' AND revision=1`) != 1 {
		t.Fatal("pair replay changed current binding")
	}
	accept(t, a, p.fakePlatform, incoming("A", "DM", "first", "hello"))
	if count(t, a.store, `SELECT count(*) FROM jobs WHERE thread='T1' AND prompt='hello'`) != 1 {
		t.Fatal("first ordinary message did not use paired binding")
	}
	p.allowed = false
	if err := a.BindPairedConversation(context.Background(), p, o, "T1"); err == nil {
		t.Fatal("revoked origin accepted")
	}
	p.allowed = true
	o.ChatType = "group"
	if err := a.BindPairedConversation(context.Background(), p, o, "T1"); err == nil {
		t.Fatal("group pairing accepted")
	}
}

func TestDesktopPairingRejectsExistingMessageConflict(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	m := incoming("A", "DM", "existing", "ordinary text")
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	if err := a.BindPairedConversation(context.Background(), p, *m.ConversationOrigin, "T1"); err == nil {
		t.Fatal("existing inbox message overwritten by pairing")
	}
	if count(t, a.store, `SELECT count(*) FROM bindings`) != 0 {
		t.Fatal("failed pairing left a binding")
	}
}

func TestMalformedNativeRequestIDFailsClosed(t *testing.T) {
	for _, id := range []any{wire{"bad": "id"}, []any{"bad"}, float64(-1), float64(1.5), ""} {
		state := idleState("T1")
		turn := wire{"turnId": "turn"}
		state["requests"] = []any{wire{"id": id, "method": "item/tool/requestUserInput", "params": wire{"threadId": "T1", "turnId": "turn", "questions": []any{wire{"id": "q", "question": "choose"}}}}}
		if _, err := questionRequests(state, turn); err == nil {
			t.Fatal("malformed request accepted", id)
		}
	}
}

func TestCUJ_EntryListsAndSwitchCannotImportAnotherEntry(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "dm-bind", "/bind T1"))
	accept(t, a, p, incoming("A", "G1", "g-bind", "/bind T2"))
	accept(t, a, p, incoming("A", "DM", "foreign-switch", "/switch T2"))
	accept(t, a, p, incoming("A", "DM", "dm-list", "/list"))
	var thread, body string
	if err := a.store.db.QueryRow(`SELECT thread FROM bindings WHERE origin=?`, originKey(*incoming("A", "DM", "x", "").ConversationOrigin)).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	if thread != "T1" {
		t.Fatal("cross-entry switch imported a thread")
	}
	if err := a.store.db.QueryRow(`SELECT body FROM outbox WHERE job='ingress:app:tenant:dm-list'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "T1") || strings.Contains(body, "T2") {
		t.Fatal("list escaped entry", body)
	}
	accept(t, a, p, incoming("A", "DM", "dm-import", "/bind T2"))
	accept(t, a, p, incoming("A", "DM", "dm-switch", "/switch T1"))
	if !a.store.linked(originKey(*incoming("A", "DM", "x", "").ConversationOrigin), "T2") {
		t.Fatal("old conversation disappeared")
	}
}

type reactingPlatform struct {
	fakePlatform
	reactions int
}

func (p *reactingPlatform) ReactConversation(context.Context, core.ConversationOrigin, string) (string, error) {
	p.reactions++
	return "", fmt.Errorf("reaction unavailable")
}
func TestCUJ_ReactionFailureQuietTaskFinalOnly(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &reactingPlatform{}
	for _, m := range []*core.Message{incoming("A", "DM", "bind", "/bind T1"), incoming("A", "DM", "m", "hello")} {
		if err := a.HandleConversation(context.Background(), p, m); err != nil {
			t.Fatal(err)
		}
		if err := a.consumeInbox(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		step(t, a)
	}
	f.mu.Lock()
	turn := obj(list(f.states["T1"]["turns"])[0])
	turn["items"] = []any{wire{"type": "agentMessage", "phase": "commentary", "text": "PRIVATE_PROGRESS"}}
	f.mu.Unlock()
	step(t, a)
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE kind IN ('accepted','running') OR body LIKE '%PRIVATE_PROGRESS%'`) != 0 {
		t.Fatal("normal progress emitted")
	}
	f.mu.Lock()
	turn["status"] = "completed"
	turn["items"] = append(list(turn["items"]), wire{"type": "agentMessage", "phase": "final_answer", "text": "FINAL_ONLY"})
	f.mu.Unlock()
	for i := 0; i < 6; i++ {
		step(t, a)
	}
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE body='FINAL_ONLY' AND state='sent'`) != 1 {
		t.Fatal("reaction blocked final")
	}
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE body LIKE '%PRIVATE_PROGRESS%'`) != 0 {
		t.Fatal("commentary leaked")
	}
}

func TestReplySplittingPreservesUnicodeAndStructure(t *testing.T) {
	text := strings.Repeat("一😀二\n", 1500) + "\n```go\n" + strings.Repeat("x", 11000) + "\n```\n[链接](https://example.com)"
	parts := splitReply(text)
	if strings.Join(parts, "") != text {
		t.Fatal("reply lost content")
	}
	for _, p := range parts {
		if len([]rune(p)) > 3500 || len(p) > 10000 {
			t.Fatal("oversized part")
		}
	}
}

type mediaPlatform struct {
	fakePlatform
	downloads, uploads int
	data               []byte
	media              []core.ConversationMedia
	failDownload       bool
	failUpload         bool
}

func (p *mediaPlatform) DownloadConversationMedia(_ context.Context, o core.ConversationOrigin, m core.ConversationMedia) ([]byte, error) {
	p.downloads++
	if o.MessageID == "" || m.Key == "" || p.failDownload {
		return nil, fmt.Errorf("download-failed")
	}
	return p.data, nil
}
func (p *mediaPlatform) UploadConversationMedia(_ context.Context, o core.ConversationOrigin, m core.ConversationMedia) (string, error) {
	p.uploads++
	if p.failUpload {
		return "", fmt.Errorf("upload-unavailable")
	}
	data, err := readMedia(m.Path)
	if err != nil {
		return "", err
	}
	if string(data) != string(p.data) {
		return "", fmt.Errorf("snapshot bytes changed")
	}
	return "uploaded-key", nil
}
func (p *mediaPlatform) ReplyConversationMedia(_ context.Context, o core.ConversationOrigin, id string, m core.ConversationMedia) (string, error) {
	p.media = append(p.media, m)
	if p.failure != nil {
		return "", p.failure
	}
	return "media-" + id, nil
}
func TestCUJ_MediaPersistentInputOutputSnapshotsAndRetry(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &mediaPlatform{data: []byte("exact file bytes")}
	m := incoming("A", "DM", "media", "分析这些附件")
	m.Media = []core.ConversationMedia{{Kind: "image", Key: "image", Name: "测试图片.png"}, {Kind: "file", Key: "file", Name: "../测试文件.txt"}}
	if err := a.store.bind(originKey(*m.ConversationOrigin), "T1"); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	if p.downloads != 0 {
		t.Fatal("ingress blocked on download")
	}
	if err := a.consumeInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal("redelivered downloaded media rejected", err)
	}
	j, err := scanJob(a.store.db.QueryRow(`SELECT ` + jobCols + ` FROM jobs`))
	if err != nil {
		t.Fatal(err)
	}
	input, attachments, err := a.turnInput(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) != 2 || str(obj(input[1])["type"]) != "localImage" || len(attachments) != 2 {
		t.Fatal("media not in real turn input", input, attachments)
	}
	path := str(obj(attachments[1])["fsPath"])
	bytes, err := readMedia(path)
	if err != nil || string(bytes) != string(p.data) {
		t.Fatal("bytes changed", err)
	}
	step(t, a)
	f.mu.Lock()
	starts := f.starts
	f.mu.Unlock()
	if starts != 1 {
		t.Fatal(starts)
	}
	original := filepath.Join(t.TempDir(), "产物.txt")
	if err := os.WriteFile(original, p.data, 0600); err != nil {
		t.Fatal(err)
	}
	f.finish("T1", "已完成\n[产物](<"+original+">)\n结束", false)
	step(t, a)
	if err := os.WriteFile(original, []byte("mutated original"), 0600); err != nil {
		t.Fatal(err)
	}
	p.failure = fmt.Errorf("unknown send")
	for i := 0; i < 4; i++ {
		step(t, a)
	}
	p.failure = nil
	if _, err := a.store.db.Exec(`UPDATE outbox SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		step(t, a)
	}
	if p.uploads != 1 || len(p.media) < 2 || p.media[0].Key != p.media[1].Key {
		t.Fatal("upload key not retained across retry", p.uploads, p.media)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 1 {
		t.Fatal("delivery retry reran model")
	}
}
func TestCUJ_MediaDownloadFailureNeverRunsPartialText(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &mediaPlatform{failDownload: true}
	m := incoming("A", "DM", "media", "text with file")
	m.Media = []core.ConversationMedia{{Kind: "file", Key: "file", Name: "x.txt"}}
	if err := a.store.bind(originKey(*m.ConversationOrigin), "T1"); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleConversation(context.Background(), p, m); err != nil {
		t.Fatal(err)
	}
	step(t, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != 0 || count(t, a.store, `SELECT count(*) FROM jobs`) != 0 {
		t.Fatal("partial message executed")
	}
}
func TestCUJ_NativeOptionNumberMapsToExactAnswer(t *testing.T) {
	_, a, p := startQuestion(t, []any{wire{"id": "q", "question": "选择", "options": []any{wire{"label": "甲", "description": "第一种"}, wire{"label": "乙", "description": "第二种"}}}})
	accept(t, a, p, incoming("A", "DM", "answer", "2"))
	var encoded string
	if err := a.store.db.QueryRow(`SELECT answers FROM questions`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if encoded != `{"q":["乙"]}` {
		t.Fatal("number not mapped", encoded)
	}
}

func TestFinalPhaseIsRequired(t *testing.T) {
	for _, phase := range []string{"", "analysis", "commentary"} {
		r := classify(wire{"status": "completed", "items": []any{wire{"type": "agentMessage", "phase": phase, "text": "PROCESS_MUST_NOT_LEAK"}}})
		if r.Text != "" {
			t.Fatalf("phase %q leaked: %q", phase, r.Text)
		}
	}
	r := classify(wire{"status": "completed", "items": []any{wire{"type": "agentMessage", "phase": "final_answer", "text": "final"}}})
	if r.Text != "final" || r.Category != "" {
		t.Fatal(r)
	}
}

func TestOutputSourceReferenceAndDuplicateArtifact(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &mediaPlatform{data: []byte("result bytes")}
	accept(t, a, &p.fakePlatform, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, &p.fakePlatform, incoming("A", "DM", "m", "produce"))
	j, err := scanJob(a.store.db.QueryRow(`SELECT ` + jobCols + ` FROM jobs`))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "generated.py")
	if err = os.WriteFile(file, p.data, 0600); err != nil {
		t.Fatal(err)
	}
	text := "Source [code](/unreadable/main.go:1) and [line](/unreadable/main.go:42)\n[Report](" + file + ")\nAgain [Second label](" + file + ") done"
	if err = a.finishResult(j, text, ""); err != nil {
		t.Fatal(err)
	}
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE kind LIKE 'media:%'`) != 1 {
		t.Fatal("artifact duplicated or source exported")
	}
	var bodies []string
	rows, err := a.store.db.Query(`SELECT body FROM outbox WHERE job=? AND kind LIKE 'result:%' ORDER BY rowid`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	joined := strings.Join(bodies, "")
	if !strings.Contains(joined, "[code](/unreadable/main.go:1)") || !strings.Contains(joined, "Second label") || !strings.Contains(joined, "done") {
		t.Fatal("content lost", joined)
	}
}

func TestUploadFailureDoesNotStartReplyWindow(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &mediaPlatform{data: []byte("file"), failUpload: true}
	accept(t, a, &p.fakePlatform, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, &p.fakePlatform, incoming("A", "DM", "m", "produce"))
	a.platforms[p.Name()] = p
	j, err := scanJob(a.store.db.QueryRow(`SELECT ` + jobCols + ` FROM jobs`))
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := a.snapshotMedia(p.data, "result.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finishResult(j, "[result]("+path+")", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err = a.flushOutbox(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var first int64
	if err = a.store.db.QueryRow(`SELECT state,first_attempt FROM outbox WHERE kind LIKE 'media:%'`).Scan(&state, &first); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || first != 0 || p.uploads != 1 || len(p.media) != 0 {
		t.Fatal(state, first, p.uploads, p.media)
	}
	p.failUpload = false
	if _, err = a.store.db.Exec(`UPDATE outbox SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	if err = a.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.media) != 1 || p.media[0].Key != "uploaded-key" {
		t.Fatal("upload retry did not deliver", p.media)
	}
}

func TestComplexFinalPreservesExactOriginalAttachment(t *testing.T) {
	_, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "m", "produce"))
	j, err := scanJob(a.store.db.QueryRow(`SELECT ` + jobCols + ` FROM jobs`))
	if err != nil {
		t.Fatal(err)
	}
	original := "| 表 | 值 |\n|---|---|\n| 数据 | 73 |\n$$x^2$$\n::code-comment{title=\"标题\" body=\"说明\" file=\"/tmp/code.go\"}"
	if err = a.finishResult(j, original, ""); err != nil {
		t.Fatal(err)
	}
	var body string
	if err = a.store.db.QueryRow(`SELECT body FROM outbox WHERE kind='media:original'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var m core.ConversationMedia
	if err = json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	data, err := readMedia(m.Path)
	if err != nil || string(data) != original {
		t.Fatal("original lost", err)
	}
	if count(t, a.store, `SELECT count(*) FROM outbox WHERE kind LIKE 'result%' AND body LIKE '%::code-comment%'`) != 0 {
		t.Fatal("directive leaked")
	}
}

func TestReplyEncodedSizeBudget(t *testing.T) {
	original := strings.Repeat("\x00<>\\\"😀", 5000)
	parts := splitReply(original)
	if strings.Join(parts, "") != original {
		t.Fatal("lost content")
	}
	for _, p := range parts {
		inner := marshal(wire{"elements": []any{wire{"tag": "markdown", "content": p}}})
		outer := marshal(wire{"msg_type": "interactive", "content": inner})
		if len(outer) > 28000 {
			t.Fatal("encoded message too large", len(outer))
		}
	}
}

func TestCUJ_AsyncQuestionSelectionAndFreeformContinueOriginalTurn(t *testing.T) {
	for _, answer := range []string{"2", "紫色"} {
		t.Run(answer, func(t *testing.T) {
			f, socket := newFake(t)
			f.dropSteer = answer == "紫色"
			a := testAgent(t, socket)
			a.nativeAnswers = true
			p := &fakePlatform{}
			accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
			accept(t, a, p, incoming("A", "DM", "q", "ask"))
			step(t, a)
			f.mu.Lock()
			turn := obj(list(f.states["T1"]["turns"])[0])
			turn["items"] = []any{wire{"type": "agentMessage", "id": "call-real-async", "phase": "final_answer", "delivery": "async", "text": "颜色？", "questions": []any{wire{"title": "颜色？", "options": []any{"红色", "蓝色"}}, wire{"title": "补充说明？"}}}}
			f.mu.Unlock()
			step(t, a)
			step(t, a)
			if count(t, a.store, `SELECT count(*) FROM questions WHERE state='pending'`) != 1 {
				t.Fatal("async question expired before answer")
			}
			accept(t, a, p, incoming("A", "DM", "answer", answer))
			step(t, a)
			if count(t, a.store, `SELECT count(*) FROM questions WHERE state='pending'`) != 1 || count(t, a.store, `SELECT count(*) FROM outbox WHERE kind LIKE 'question:%' AND body LIKE '补充说明？%'`) != 1 {
				t.Fatal("remaining question not collected in IM")
			}
			f.mu.Lock()
			premature := f.answers
			f.mu.Unlock()
			if premature != 0 {
				t.Fatal("partial form submitted")
			}
			accept(t, a, p, incoming("A", "DM", "answer2", "只在飞书回答"))
			step(t, a)
			f.mu.Lock()
			turn["status"] = "completed"
			turn["items"] = append(list(turn["items"]), wire{"type": "agentMessage", "phase": "final_answer", "text": "done"})
			f.mu.Unlock()
			step(t, a)
			if count(t, a.store, `SELECT count(*) FROM questions WHERE state='resolved'`) != 1 {
				t.Fatal("answer not confirmed")
			}
			f.mu.Lock()
			evidence := asyncAnswerValues(obj(list(f.states["T1"]["turns"])[0]))
			starts, submits := f.starts, f.answers
			f.mu.Unlock()
			expected := answer
			if answer == "2" {
				expected = "蓝色"
			}
			qid := marshal([]any{"request_user_input_async", "call-real-async", 0})
			qid2 := marshal([]any{"request_user_input_async", "call-real-async", 1})
			if evidence[qid].Answer != expected || evidence[qid2].Answer != "只在飞书回答" || starts != 1 || submits != 1 {
				t.Fatal("wrong answer or new turn", evidence, starts, submits)
			}
			step(t, a)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.answers != 1 {
				t.Fatal("duplicate answer")
			}
		})
	}
}

func TestAsyncEvidenceRejectsUnacceptedOrOtherTurnAndHidesQuestionFromFinal(t *testing.T) {
	id := marshal([]any{"request_user_input_async", "call", 0})
	text := asyncReplyOpen + "\n" + marshal([]asyncAnswer{{QuestionItemID: id, Question: "题目", Answer: "回答"}}) + "\n" + asyncReplyClose
	for _, status := range []string{"pending", "rejected"} {
		turn := wire{"turnId": "T", "items": []any{wire{"type": "steeringUserMessage", "status": status, "targetTurnId": "T", "input": []any{wire{"type": "text", "text": text}}}}}
		if len(asyncAnswerValues(turn)) != 0 {
			t.Fatal("unaccepted answer evidence")
		}
	}
	turn := wire{"turnId": "T", "status": "completed", "items": []any{wire{"type": "agentMessage", "phase": "final_answer", "delivery": "async", "text": "问题"}, wire{"type": "agentMessage", "phase": "final_answer", "text": "结果"}, wire{"type": "steeringUserMessage", "status": "accepted", "targetTurnId": "OTHER", "input": []any{wire{"type": "text", "text": text}}}}}
	if len(asyncAnswerValues(turn)) != 0 || classify(turn).Text != "结果" {
		t.Fatal("wrong-turn evidence or question echoed")
	}
}

func TestNativeAnswerNeverSubmittedAfterTurnEnded(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			f, a, p := startQuestion(t, []any{wire{"id": "q", "question": "选择"}})
			accept(t, a, p, incoming("A", "DM", "answer", "答案"))
			f.mu.Lock()
			obj(list(f.states["T1"]["turns"])[0])["status"] = status
			f.mu.Unlock()
			step(t, a)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.answers != 0 || count(t, a.store, `SELECT count(*) FROM questions WHERE state='closed'`) != 1 {
				t.Fatal("answer submitted after turn ended")
			}
		})
	}
}

func TestLongCodeBlockContinuesOnEveryCard(t *testing.T) {
	text := "```go\n" + strings.Repeat("a", 11000) + "\n```\nend"
	parts := renderedReplyParts(text)
	if len(parts) < 3 {
		t.Fatal("fixture did not split")
	}
	for _, part := range parts {
		if strings.Count(part, "```") != 2 {
			t.Fatal("unbalanced card code block")
		}
	}
	if !strings.HasSuffix(parts[len(parts)-1], "end") {
		t.Fatal("tail lost")
	}
}

func TestCUJ_UnloadedBoundThreadOpensBeforeSingleSubmission(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	p := &fakePlatform{}
	accept(t, a, p, incoming("A", "DM", "bind", "/bind T1"))
	accept(t, a, p, incoming("A", "DM", "message", "continue"))
	f.mu.Lock()
	delete(f.states, "T1")
	f.dropStart = true
	f.mu.Unlock()
	opens := 0
	a.openThread = func(_ context.Context, thread string) error {
		if thread != "T1" {
			t.Fatalf("opened wrong thread: %s", thread)
		}
		opens++
		f.mu.Lock()
		f.states[thread] = idleState(thread)
		f.mu.Unlock()
		return nil
	}
	step(t, a)
	step(t, a) // Lost start ACK is reconciled from the original turn.
	f.finish("T1", "continued", false)
	step(t, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if opens != 1 || f.starts != 1 || count(t, a.store, `SELECT count(*) FROM jobs WHERE state='finished' AND thread='T1'`) != 1 {
		t.Fatal("recovery opened or submitted more than once", opens, f.starts)
	}
}

func TestThreadOpeningFailureIsThrottledAndDoesNotSubmit(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	f.mu.Lock()
	delete(f.states, "T1")
	f.mu.Unlock()
	opens := 0
	a.openThread = func(context.Context, string) error { opens++; return fmt.Errorf("open failed") }
	for n := 0; n < 2; n++ {
		if _, err := a.attach(context.Background(), socket, "T1", a.timeout); err == nil {
			t.Fatal("missing recovery error")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if opens != 1 || f.starts != 0 {
		t.Fatal("repeated window open or model submission", opens, f.starts)
	}
}

func TestUnknownDesktopProtocolDoesNotOpenThread(t *testing.T) {
	f, socket := newFake(t)
	a := testAgent(t, socket)
	f.mu.Lock()
	f.version = 999
	f.mu.Unlock()
	opens := 0
	a.openThread = func(context.Context, string) error { opens++; return nil }
	if _, err := a.attach(context.Background(), socket, "T1", a.timeout); err == nil || opens != 0 {
		t.Fatal("protocol mismatch triggered navigation", err, opens)
	}
}

// Opt in only with an explicitly authorized disposable Desktop thread.
func TestLiveOpenAuthorizedDesktopThread(t *testing.T) {
	thread := os.Getenv("BRODGE_TEST_DESKTOP_THREAD")
	if thread == "" {
		t.Skip("requires an explicitly authorized Desktop thread")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := openDesktopThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	var err error
	for ctx.Err() == nil {
		var result map[string]any
		result, err = Preflight(ctx, "", thread)
		if err == nil && result["history_loaded"] == true && result["send_attempted"] == false {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatal("Desktop did not load the authorized thread", err)
}
