package codexdesktop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

type Agent struct {
	mu            sync.Mutex
	store         *store
	dataDir       string
	i18n          *core.I18n
	allowed       []string
	socket        string
	nativeAnswers bool
	timeout, poll time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	platforms     map[string]core.Platform
	owner         string
	lock          *os.File
	started       bool
	attach        func(context.Context, string, string, time.Duration) (*attachment, error)
	openThread    func(context.Context, string) error
	nextOpen      map[string]time.Time
	prepareNew    func(context.Context, string, []core.ConversationMedia) (desktopDraft, error)
	creationMu    sync.Mutex
	creationIndex func(context.Context) ([]string, error)
	metadataRoot  string
	metadataMu    sync.Mutex
	queryPreviews map[string]queryPreview
}

var _ core.Agent = (*Agent)(nil)
var _ core.BoundConversationAgent = (*Agent)(nil)

func init() {
	core.RegisterAgent("codex-desktop", func(opts map[string]any) (core.Agent, error) { return New(opts) })
}
func New(opts map[string]any) (*Agent, error) {
	if protocol := str(opts["protocol"]); protocol != "local-v11-start-v2" {
		return nil, fmt.Errorf("codex-desktop: protocol must be local-v11-start-v2")
	}
	var allowed []string
	switch v := opts["allowed_threads"].(type) {
	case []string:
		allowed = append([]string(nil), v...)
	case []any:
		for _, s := range v {
			allowed = append(allowed, str(s))
		}
	case nil:
	default:
		return nil, fmt.Errorf("codex-desktop: invalid allowed_threads")
	}

	for i, s := range allowed {
		t, err := parseThread(s)
		if err != nil {
			return nil, err
		}
		allowed[i] = t
	}
	data := str(opts["state_dir"])
	if data == "" {
		data = filepath.Join(str(opts["cc_data_dir"]), "desktop", str(opts["cc_project"]))
	}
	if !filepath.IsAbs(data) {
		return nil, fmt.Errorf("state-dir-required")
	}
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	dirInfo, err := os.Lstat(data)
	if err != nil {
		return nil, err
	}
	dirStat, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || !dirInfo.IsDir() || dirStat.Uid != uint32(os.Getuid()) || dirInfo.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe-state-directory")
	}
	lock, err := os.OpenFile(filepath.Join(data, "service.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		lock.Close()
		return nil, err
	}
	lockStat, ok := lockInfo.Sys().(*syscall.Stat_t)
	if !ok || !lockInfo.Mode().IsRegular() || lockStat.Uid != uint32(os.Getuid()) || lockInfo.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, fmt.Errorf("unsafe-state-lock")
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return nil, fmt.Errorf("desktop bridge already running for this state directory")
	}
	s, err := openStore(filepath.Join(data, "bridge.sqlite"))
	if err != nil {
		lock.Close()
		return nil, err
	}
	a := &Agent{store: s, dataDir: data, i18n: core.NewI18n(core.NormalizeLanguageString(str(opts["language"]))), allowed: allowed, socket: str(opts["socket"]), nativeAnswers: opts["native_answers"] == true, timeout: 5 * time.Second, poll: time.Second, platforms: map[string]core.Platform{}, owner: uuid.NewString(), lock: lock, openThread: openDesktopThread, nextOpen: map[string]time.Time{}}
	a.attach = a.attachWithActivation
	a.prepareNew = prepareDesktopDraft
	a.creationIndex = desktopThreadIndex
	a.metadataRoot, _ = desktopStateRoot()
	a.queryPreviews = make(map[string]queryPreview)
	return a, nil
}
func (a *Agent) Name() string { return "codex-desktop" }

func openDesktopThread(ctx context.Context, thread string) error {
	thread, err := parseThread(thread)
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop-thread-opening-requires-macos")
	}
	// Ask the installed Desktop to load its own conversation. No prompt is
	// attached, so opening a link can never submit or repeat a model turn.
	return exec.CommandContext(ctx, "/usr/bin/open", "-g", "-b", "com.openai.codex", "codex://threads/"+thread+"?hostId=local").Run()
}

func (a *Agent) attachWithActivation(ctx context.Context, socket, thread string, timeout time.Duration) (*attachment, error) {
	at, err := attachIPC(ctx, socket, thread, timeout)
	if err == nil || !errors.Is(err, errOwnerUnavailable) || ctx.Err() != nil || a.openThread == nil {
		return at, err
	}
	// Throttle window navigation independently of durable message deduplication.
	a.mu.Lock()
	if time.Now().Before(a.nextOpen[thread]) {
		a.mu.Unlock()
		return nil, err
	}
	a.nextOpen[thread] = time.Now().Add(time.Minute)
	a.mu.Unlock()
	recovery, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = a.openThread(recovery, thread); err != nil {
		return nil, fmt.Errorf("open-desktop-thread: %w", err)
	}
	for {
		at, err = attachIPC(recovery, socket, thread, timeout)
		if err == nil || !errors.Is(err, errOwnerUnavailable) {
			return at, err
		}
		select {
		case <-recovery.Done():
			return nil, err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (a *Agent) permitted(thread string) bool {
	for _, t := range a.allowed {
		if t == thread {
			return true
		}
	}
	var n int
	return a.store.db.QueryRow(`SELECT count(*) FROM binding_threads WHERE thread=?`, thread).Scan(&n) == nil && n > 0
}
func (a *Agent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	out := make([]core.AgentSessionInfo, 0, len(a.allowed))
	for _, id := range a.allowed {
		out = append(out, core.AgentSessionInfo{ID: id, Summary: "Authorized Desktop conversation"})
	}
	return out, nil
}
func (a *Agent) Stop() error {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
	err := a.store.db.Close()
	unlockErr := syscall.Flock(int(a.lock.Fd()), syscall.LOCK_UN)
	closeErr := a.lock.Close()
	return errors.Join(err, unlockErr, closeErr)
}
func (a *Agent) ResumeConversations(ctx context.Context, platforms []core.Platform) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range platforms {
		a.platforms[p.Name()] = p
	}
	if a.started {
		return nil
	}
	if err := a.store.recover(); err != nil {
		return fmt.Errorf("desktop recovery: %w", err)
	}
	a.started = true
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(a.poll)
		defer ticker.Stop()
		var nextCleanup time.Time
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				if now := time.Now(); !now.Before(nextCleanup) {
					nextCleanup = now.Add(time.Hour)
					if removed, err := a.cleanMedia(now); err != nil {
						slog.Warn("desktop media cleanup skipped", "error", err)
					} else if removed > 0 {
						slog.Info("desktop orphan media removed", "count", removed)
					}
				}
				if err := a.step(a.ctx); err != nil {
					slog.Warn("desktop bridge worker", "error", err)
				}
			}
		}
	}()
	return nil
}
func (a *Agent) HandleConversation(ctx context.Context, p core.Platform, msg *core.Message) error {
	if msg.ConversationOrigin == nil || msg.IsPermissionResponse || msg.Recalled {
		return fmt.Errorf("verified conversation origin required")
	}
	sender, ok := p.(core.DurableConversationPlatform)
	if !ok || !sender.AcceptConversationOrigin(*msg.ConversationOrigin) {
		return fmt.Errorf("platform lacks durable reply capability")
	}
	if len(msg.Images) > 0 || len(msg.Files) > 0 || msg.Audio != nil || msg.ModeOverride != "" || len(msg.Content) > 24000 {
		msgCopy := *msg
		msgCopy.Content = "/unsupported-attachment"
		msg = &msgCopy
	}
	for _, m := range msg.Media {
		if m.Path != "" || m.SHA256 != "" || m.MessageID != "" {
			return fmt.Errorf("remote resource reference required")
		}
	}
	a.mu.Lock()
	a.platforms[p.Name()] = p
	a.mu.Unlock()
	return a.store.ingest(p, msg)
}
func (a *Agent) BindConversation(ctx context.Context, key, thread string) error {
	thread, err := parseThread(thread)
	if err != nil {
		return err
	}
	var known int
	if err = a.store.db.QueryRow(`SELECT count(*) FROM inbox WHERE key=?`, key).Scan(&known); err != nil {
		return err
	}
	if known == 0 {
		return fmt.Errorf("入口尚未认证，请先完成飞书私聊配对，或在群内 @ 机器人发送 /bind 会话链接")
	}
	at, err := a.attach(ctx, a.socket, thread, a.timeout)
	if err != nil {
		return err
	}
	at.c.abort(fmt.Errorf("detached"))
	return a.store.bind(key, thread)
}

func (a *Agent) UnbindConversation(ctx context.Context, key, thread string) error {
	thread, err := parseThread(thread)
	if err != nil || key == "" {
		return fmt.Errorf("请选择要解除的会话绑定")
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM bindings WHERE origin=? AND thread=?`, key, thread); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM binding_threads WHERE origin=? AND thread=?`, key, thread); err != nil {
		return err
	}
	return tx.Commit()
}

// BindPairedConversation records an already verified private pairing message and
// binds it without treating the pairing code as a prompt or sending a reply.
func (a *Agent) BindPairedConversation(ctx context.Context, p core.Platform, origin core.ConversationOrigin, thread string) error {
	sender, ok := p.(core.DurableConversationPlatform)
	if !ok || !sender.AcceptConversationOrigin(origin) || origin.ChatType != "p2p" || origin.Actor == "" || origin.ChatID == "" || origin.MessageID == "" {
		return fmt.Errorf("verified private pairing origin required")
	}
	a.mu.Lock()
	a.platforms[p.Name()] = p
	a.mu.Unlock()
	var err error
	if thread != "" {
		thread, err = parseThread(thread)
		if err != nil || !a.permitted(thread) {
			return fmt.Errorf("thread not in allowed_threads")
		}
	}
	var recordedSource, recordedState string
	e := a.store.db.QueryRow(`SELECT source,state FROM inbox WHERE app=? AND tenant=? AND message=?`, origin.AppID, origin.Tenant, origin.MessageID).Scan(&recordedSource, &recordedState)
	if e == nil {
		if recordedSource != marshal(origin) || recordedState != "paired" {
			return fmt.Errorf("pairing-message-conflict")
		}
		return nil
	}
	if e != sql.ErrNoRows {
		return e
	}
	if thread != "" {
		at, err := a.attach(ctx, a.socket, thread, a.timeout)
		if err != nil {
			return err
		}
		at.c.abort(fmt.Errorf("detached"))
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key, source := originKey(origin), marshal(origin)
	var existing, body, state string
	err = tx.QueryRow(`SELECT source,body,state FROM inbox WHERE app=? AND tenant=? AND message=?`, origin.AppID, origin.Tenant, origin.MessageID).Scan(&existing, &body, &state)
	if err == nil {
		if existing != source || body != "" || state != "paired" {
			return fmt.Errorf("pairing-message-conflict")
		}
	} else if err == sql.ErrNoRows {
		_, err = tx.Exec(`INSERT INTO inbox(app,tenant,message,source,key,platform,body,state) VALUES(?,?,?,?,?,?,'','paired')`, origin.AppID, origin.Tenant, origin.MessageID, source, key, p.Name())
		if err != nil {
			return err
		}
	} else {
		return err
	}
	if thread == "" {
		return tx.Commit()
	}
	var existingThread string
	err = tx.QueryRow(`SELECT thread FROM bindings WHERE origin=?`, key).Scan(&existingThread)
	if err == nil && existingThread != thread {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO binding_threads VALUES(?,?)`, key, thread); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO bindings VALUES(?,?,1) ON CONFLICT(origin) DO NOTHING`, key, thread); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *Agent) ConversationState() any {
	out := map[string]any{"allowed_threads": a.allowed, "native_answers_enabled": a.nativeAnswers, "capabilities": map[string]bool{"existing_binding": true, "new": false, "attachments": true, "native_answers": a.nativeAnswers}, "bindings": []any{}, "conversations": []any{}, "messages": []any{}, "outbox": []any{}}
	out["accessibility_granted"] = desktopAccessibilityGranted()
	query := func(name, sql string, read func(*sql.Rows) (any, error)) {
		rows, err := a.store.db.Query(sql)
		if err != nil {
			out["error"] = "state-unavailable"
			slog.Warn("desktop state query", "error", err)
			return
		}
		var entries []any
		for rows.Next() {
			value, e := read(rows)
			if e != nil {
				err = e
				break
			}
			entries = append(entries, value)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			out["error"] = "state-unavailable"
			slog.Warn("desktop state rows", "error", err)
			return
		}
		if entries != nil {
			out[name] = entries
		}
	}
	query("bindings", `SELECT i.source,i.platform,COALESCE(b.thread,''),COALESCE(b.revision,0) FROM inbox i LEFT JOIN bindings b ON b.origin=i.key GROUP BY i.key ORDER BY i.rowid`, func(rows *sql.Rows) (any, error) {
		var source, platform, thread string
		var rev int
		if err := rows.Scan(&source, &platform, &thread, &rev); err != nil {
			return nil, err
		}
		var o core.ConversationOrigin
		if err := json.Unmarshal([]byte(source), &o); err != nil {
			return nil, err
		}
		return map[string]any{"session_key": originKey(o), "chat": o.ChatID, "chat_type": o.ChatType, "actor": o.Actor, "thread": thread, "revision": rev, "source": o, "platform": platform}, nil
	})
	query("conversations", `SELECT origin,thread FROM binding_threads ORDER BY rowid`, func(rows *sql.Rows) (any, error) {
		var key, thread string
		err := rows.Scan(&key, &thread)
		return map[string]string{"session_key": key, "thread": thread}, err
	})
	query("messages", `SELECT id,origin,thread,state,error FROM (
SELECT id,origin,thread,state,error,rowid AS position,
ROW_NUMBER() OVER (PARTITION BY origin,thread ORDER BY rowid DESC) AS recent FROM jobs
) WHERE state LIKE 'create_%' OR recent<=30 ORDER BY position DESC`, func(rows *sql.Rows) (any, error) {
		var id, key, thread, state, e string
		err := rows.Scan(&id, &key, &thread, &state, &e)
		return map[string]string{"id": id, "session_key": key, "thread": thread, "state": state, "error": e}, err
	})
	query("outbox", `SELECT id,state FROM outbox WHERE state!='sent' ORDER BY rowid LIMIT 50`, func(rows *sql.Rows) (any, error) {
		var id, state string
		err := rows.Scan(&id, &state)
		return map[string]string{"id": id, "state": state}, err
	})
	a.addConversationLabels(out)
	return out
}
func (a *Agent) step(ctx context.Context) error {
	if err := a.flushOutbox(ctx); err != nil {
		return err
	}
	if err := a.consumeInbox(ctx); err != nil {
		return err
	}
	if err := a.advanceCreation(ctx); err != nil {
		return err
	}
	if err := a.revalidateJobs(); err != nil {
		return err
	}
	j, err := a.store.claim(a.owner)
	if err == nil {
		if err = a.dispatch(ctx, j); err != nil {
			return err
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	rows, err := a.store.db.Query(`SELECT ` + jobCols + ` FROM jobs WHERE state IN ('dispatch','running','waiting','uncertain')`)
	if err != nil {
		return err
	}
	var jobs []job
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			rows.Close()
			return e
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		if err = a.monitor(ctx, j); err != nil {
			return err
		}
	}
	return a.flushOutbox(ctx)
}

func (a *Agent) revalidateJobs() error {
	rows, err := a.store.db.Query(`SELECT id,thread,source,platform,state FROM jobs WHERE state NOT IN ('finished','blocked')`)
	if err != nil {
		return err
	}
	type scope struct{ id, thread, source, platform, state string }
	var scopes []scope
	for rows.Next() {
		var s scope
		if err = rows.Scan(&s.id, &s.thread, &s.source, &s.platform, &s.state); err != nil {
			rows.Close()
			return err
		}
		scopes = append(scopes, s)
	}
	rows.Close()
	for _, s := range scopes {
		var origin core.ConversationOrigin
		if err = json.Unmarshal([]byte(s.source), &origin); err != nil {
			return err
		}
		a.mu.Lock()
		p := a.platforms[s.platform]
		a.mu.Unlock()
		platform, ok := p.(core.DurableConversationPlatform)
		if !ok || !platform.AcceptConversationOrigin(origin) || (s.thread != "" && !strings.HasPrefix(s.state, "create_") && !a.store.linked(originKey(origin), s.thread)) {
			_, err = a.store.db.Exec(`UPDATE jobs SET state='blocked',error='authorization-scope-changed' WHERE id=?`, s.id)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *Agent) dispatch(ctx context.Context, j job) error {
	at, err := a.attach(ctx, a.socket, j.Thread, a.timeout)
	if err != nil {
		_, persistErr := a.store.db.Exec(`UPDATE jobs SET state='queued',owner='',error=? WHERE id=? AND owner=? AND state='claimed'`, err.Error(), j.ID, j.Owner)
		return persistErr
	}
	defer at.c.abort(fmt.Errorf("detached"))
	state, err := at.snapshot(ctx)
	if err == nil {
		var isIdle bool
		isIdle, err = idle(state, j.Thread)
		if !isIdle && err == nil {
			err = fmt.Errorf("desktop-busy")
		}
	}
	if err != nil {
		_, persistErr := a.store.db.Exec(`UPDATE jobs SET state='queued',owner='',error=? WHERE id=? AND owner=? AND state='claimed'`, err.Error(), j.ID, j.Owner)
		return persistErr
	}
	input, attachments, err := a.turnInput(j)
	if err != nil {
		return a.finishResult(j, "附件缓存不可用，本轮未提交给 Codex。请重新发送附件。", "media-input-unavailable")
	}
	result, err := a.store.db.Exec(`UPDATE jobs SET state='dispatch',error='' WHERE id=? AND state='claimed' AND owner=?`, j.ID, j.Owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return nil
	}
	ack, err := at.c.rpc(ctx, "thread-follower-start-turn", wire{"conversationId": j.Thread, "turnStart": wire{"request": wire{"threadId": j.Thread, "input": input, "clientUserMessageId": j.ClientMessage}, "context": wire{"inheritThreadSettings": true, "attachments": attachments}}}, at.owner)
	if err != nil {
		_, persistErr := a.store.db.Exec(`UPDATE jobs SET state='uncertain',error=? WHERE id=?`, err.Error(), j.ID)
		return persistErr
	}
	turnID := str(obj(obj(obj(ack["result"])["result"])["turn"])["id"])
	if turnID == "" {
		_, err = a.store.db.Exec(`UPDATE jobs SET state='uncertain',error='unknown-start-ack' WHERE id=?`, j.ID)
		return err
	}
	_, err = a.store.db.Exec(`UPDATE jobs SET state='running',turn=? WHERE id=?`, turnID, j.ID)
	return err
}
func (a *Agent) monitor(ctx context.Context, j job) error {
	at, err := a.attach(ctx, a.socket, j.Thread, a.timeout)
	if err != nil {
		_, persistErr := a.store.db.Exec(`UPDATE jobs SET error=? WHERE id=?`, err.Error(), j.ID)
		return persistErr
	}
	defer at.c.abort(fmt.Errorf("detached"))
	state, err := at.snapshot(ctx)
	if err != nil {
		return err
	}
	turn, err := ownedTurn(state, j.Thread, j.ClientMessage, j.Turn)
	if err != nil {
		_, persistErr := a.store.db.Exec(`UPDATE jobs SET state='uncertain',error=? WHERE id=?`, err.Error(), j.ID)
		return persistErr
	}
	if turn == nil {
		return nil
	}
	if j.Turn == "" {
		j.Turn = str(turn["turnId"])
		if _, err = a.store.db.Exec(`UPDATE jobs SET turn=?,state='running' WHERE id=?`, j.Turn, j.ID); err != nil {
			return err
		}
	}
	r := classify(turn)
	if r.Terminal {
		// A fast model may finish between polls. Reconcile the accepted answer
		// from its final snapshot before closing the remaining questions.
		if err = a.submitAnswer(ctx, j, at, state, turn); err != nil {
			return err
		}
		text := r.Text
		if r.Category != "" {
			text = a.i18n.Tf(core.MsgDesktopError, r.Category, text)
		}
		if text == "" {
			if questions, e := questionRequests(state, turn); e == nil && len(questions) > 0 {
				text = "Codex 在收到答案前结束了本轮，问题已失效。需要继续时请再发一条消息。"
				r.Category = "question-ended"
			} else {
				text = a.i18n.T(core.MsgDesktopEmpty)
			}
		}
		return a.finishResult(j, text, r.Category)
	}
	requests, err := questionRequests(state, turn)
	if err != nil {
		return a.store.notice(j, "desktop-input", a.i18n.T(core.MsgDesktopDesktopInput))
	}
	if err = a.closeStaleQuestions(j, requests); err != nil {
		return err
	}
	if len(requests) > 0 {
		if err = a.persistQuestions(j, requests); err != nil {
			return err
		}
	} else if len(list(state["requests"])) > 0 {
		if err = a.store.notice(j, "desktop-input", a.i18n.T(core.MsgDesktopDesktopInput)); err != nil {
			return err
		}
	} else {
		if _, err = a.store.db.Exec(`UPDATE jobs SET state='running' WHERE id=? AND state='waiting'`, j.ID); err != nil {
			return err
		}
	}
	return a.submitAnswer(ctx, j, at, state, turn)
}
func (a *Agent) closeStaleQuestions(j job, requests []wire) error {
	rows, err := a.store.db.Query(`SELECT request FROM questions WHERE job=? AND state='pending'`, j.ID)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		present := false
		for _, v := range requests {
			if marshal(v["id"]) == id {
				present = true
			}
		}
		if !present {
			stale = append(stale, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range stale {
		if _, err = a.store.db.Exec(`UPDATE questions SET state='closed' WHERE job=? AND request=? AND state='pending'`, j.ID, id); err != nil {
			return err
		}
	}
	return nil
}
func (a *Agent) persistQuestions(j job, requests []wire) error {
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range requests {
		_, err = tx.Exec(`INSERT OR IGNORE INTO questions(job,request,body) VALUES(?,?,?)`, j.ID, marshal(r["id"]), marshal(r))
		if err != nil {
			return err
		}
		var recordedBody string
		if err = tx.QueryRow(`SELECT body FROM questions WHERE job=? AND request=?`, j.ID, marshal(r["id"])).Scan(&recordedBody); err != nil {
			return err
		}
		if recordedBody != marshal(r) {
			return fmt.Errorf("question-identity-conflict")
		}
		var answers map[string][]string
		var encoded string
		if err = tx.QueryRow(`SELECT answers FROM questions WHERE job=? AND request=?`, j.ID, marshal(r["id"])).Scan(&encoded); err != nil {
			return err
		}
		if encoded != "" {
			if err = json.Unmarshal([]byte(encoded), &answers); err != nil {
				return err
			}
		}
		var q wire
		for _, v := range list(obj(r["params"])["questions"]) {
			candidate := obj(v)
			if _, done := answers[str(candidate["id"])]; !done {
				q = candidate
				break
			}
		}
		if q == nil {
			continue
		}
		text := questionText(q)
		if !a.nativeAnswers {
			text += "\n" + a.i18n.T(core.MsgDesktopNativeDisabled)
		}
		if err = a.store.output(tx, j.ID, "question:"+marshal(r["id"])+":"+str(q["id"]), j.Source, j.Platform, text); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE jobs SET state='waiting' WHERE id=?`, j.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func questionText(q wire) string {
	text := str(q["question"])
	for i, v := range list(q["options"]) {
		option := obj(v)
		text += fmt.Sprintf("\n%d. %s", i+1, str(option["label"]))
		if description := str(option["description"]); description != "" {
			text += " — " + description
		}
	}
	return text + "\n回复编号、选项名，或填写自己的答案。"
}
func (a *Agent) submitAnswer(ctx context.Context, j job, at *attachment, state, turn wire) error {
	rows, err := a.store.db.Query(`SELECT request,body,answers,state FROM questions WHERE job=? AND state IN ('ready','dispatch','uncertain')`, j.ID)
	if err != nil {
		return err
	}
	type answer struct{ id, body, answers, state string }
	var records []answer
	for rows.Next() {
		var r answer
		if err = rows.Scan(&r.id, &r.body, &r.answers, &r.state); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	rows.Close()
	for _, r := range records {
		var requestID any
		if err = json.Unmarshal([]byte(r.id), &requestID); err != nil {
			return err
		}
		var answers map[string][]string
		if err = json.Unmarshal([]byte(r.answers), &answers); err != nil {
			return err
		}
		var request wire
		if err = json.Unmarshal([]byte(r.body), &request); err != nil {
			return err
		}
		isAsync := str(request["method"]) == "request_user_input_async"
		if (!isAsync && answerEvidence(state, turn, requestID, answers)) || (isAsync && asyncAnswerEvidence(turn, request, answers)) {
			if _, err = a.store.db.Exec(`UPDATE questions SET state='resolved' WHERE job=? AND request=?`, j.ID, r.id); err != nil {
				return err
			}
			continue
		}
		present := false
		pending, e := questionRequests(state, turn)
		if e != nil {
			return e
		}
		for _, req := range pending {
			if req["id"] == requestID && str(req["method"]) == str(request["method"]) && str(obj(req["params"])["turnId"]) == j.Turn {
				present = true
			}
		}
		if !present {
			if _, err = a.store.db.Exec(`UPDATE questions SET state='closed' WHERE job=? AND request=?`, j.ID, r.id); err != nil {
				return err
			}
			continue
		}
		if r.state != "ready" || !a.nativeAnswers || str(turn["status"]) != "inProgress" {
			continue
		}
		result, e := a.store.db.Exec(`UPDATE questions SET state='dispatch' WHERE job=? AND request=? AND state='ready'`, j.ID, r.id)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			continue
		}
		wireAnswers := wire{}
		for q, v := range answers {
			wireAnswers[q] = wire{"answers": v}
		}
		if isAsync {
			values := []asyncAnswer{}
			for _, v := range list(obj(request["params"])["questions"]) {
				q := obj(v)
				answer := answers[str(q["id"])]
				if len(answer) != 1 {
					return fmt.Errorf("invalid-async-answer")
				}
				values = append(values, asyncAnswer{QuestionItemID: str(q["id"]), Question: str(q["question"]), Answer: answer[0]})
			}
			text := asyncReplyOpen + "\n" + marshal(values) + "\n" + asyncReplyClose
			clientID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(j.ID+":"+r.id)).String()
			_, err = at.c.rpc(ctx, "thread-follower-steer-turn", wire{"conversationId": j.Thread, "clientUserMessageId": clientID, "input": []any{wire{"type": "text", "text": text, "text_elements": []any{}}}, "restoreMessage": wire{"id": clientID, "cwd": state["cwd"], "context": wire{"prompt": text, "turnTrigger": "send_user_message_async_question", "addedFiles": []any{}, "fileAttachments": []any{}, "imageAttachments": []any{}}}, "attachments": []any{}}, at.owner)
		} else {
			_, err = at.c.rpc(ctx, "thread-follower-submit-user-input", wire{"conversationId": j.Thread, "requestId": requestID, "response": wire{"answers": wireAnswers}}, at.owner)
		}
		if err != nil {
			if _, err = a.store.db.Exec(`UPDATE questions SET state='uncertain' WHERE job=? AND request=?`, j.ID, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *Agent) flushOutbox(ctx context.Context) error {
	var id, source, platform, body, state, kind string
	var first, next int64
	err := a.store.db.QueryRow(`SELECT id,source,platform,body,state,first_attempt,next_attempt,kind FROM outbox o WHERE state IN ('ready','uncertain') AND next_attempt<=? AND NOT EXISTS(SELECT 1 FROM outbox prior WHERE prior.job=o.job AND prior.rowid<o.rowid AND prior.state NOT IN ('sent','skipped') AND prior.kind!='reaction') ORDER BY rowid LIMIT 1`, time.Now().Unix()).Scan(&id, &source, &platform, &body, &state, &first, &next, &kind)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if first != 0 && time.Now().Unix()-first >= 3500 {
		_, err = a.store.db.Exec(`UPDATE outbox SET state='reconcile' WHERE id=?`, id)
		return err
	}
	a.mu.Lock()
	p := a.platforms[platform]
	a.mu.Unlock()
	sender, ok := p.(core.DurableConversationPlatform)
	if !ok {
		return nil
	}
	var origin core.ConversationOrigin
	if json.Unmarshal([]byte(source), &origin) != nil {
		return fmt.Errorf("invalid-outbox-origin")
	}
	if !sender.AcceptConversationOrigin(origin) {
		_, err = a.store.db.Exec(`UPDATE outbox SET state='blocked' WHERE id=?`, id)
		return err
	}
	var media core.ConversationMedia
	var mediaPlatform core.ConversationMediaPlatform
	if strings.HasPrefix(kind, "media:") {
		mediaPlatform, ok = p.(core.ConversationMediaPlatform)
		if !ok || json.Unmarshal([]byte(body), &media) != nil {
			_, err = a.store.db.Exec(`UPDATE outbox SET state='blocked' WHERE id=?`, id)
			return err
		}
		if data, e := readMedia(media.Path); e != nil || digest(data) != media.SHA256 {
			_, err = a.store.db.Exec(`UPDATE outbox SET state='blocked' WHERE id=?`, id)
			return err
		}
		if media.Key == "" {
			// Uploading has no visible message side effect. Its time and failures
			// must not consume the reply UUID's deduplication window.
			uploadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			key, uploadErr := mediaPlatform.UploadConversationMedia(uploadCtx, origin, media)
			cancel()
			if uploadErr != nil || key == "" {
				status := "ready"
				var delivery *core.DeliveryError
				if errors.As(uploadErr, &delivery) && delivery.Code != 99991400 && delivery.Code != 99991401 {
					status = "blocked"
				}
				_, err = a.store.db.Exec(`UPDATE outbox SET state=?,next_attempt=? WHERE id=?`, status, time.Now().Unix()+15, id)
				return err
			}
			media.Key = key
			if _, err = a.store.db.Exec(`UPDATE outbox SET body=? WHERE id=?`, marshal(media), id); err != nil {
				return err
			}
		}
	}
	if first == 0 {
		first = time.Now().Unix()
	}
	_, err = a.store.db.Exec(`UPDATE outbox SET state='sending',first_attempt=? WHERE id=?`, first, id)
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var receipt string
	var sendErr error
	if kind == "reaction" {
		if reactor, ok := p.(core.ConversationReactor); ok {
			receipt, sendErr = reactor.ReactConversation(sendCtx, origin, body)
		} else {
			receipt = "unsupported"
		}
	} else if strings.HasPrefix(kind, "media:") {
		receipt, sendErr = mediaPlatform.ReplyConversationMedia(sendCtx, origin, id, media)
	} else {
		receipt, sendErr = sender.ReplyConversation(sendCtx, origin, id, body)
	}
	cancel()
	if sendErr == nil && receipt != "" {
		_, err = a.store.db.Exec(`UPDATE outbox SET state='sent',message=? WHERE id=?`, receipt, id)
		return err
	}
	status := "uncertain"
	var delivery *core.DeliveryError
	if errors.As(sendErr, &delivery) {
		switch delivery.Code {
		case 230020:
			status = "ready"
		case 230049:
			status = "uncertain"
		default:
			status = "blocked"
		}
	}
	_, err = a.store.db.Exec(`UPDATE outbox SET state=?,next_attempt=? WHERE id=?`, status, time.Now().Unix()+15, id)
	return err
}
