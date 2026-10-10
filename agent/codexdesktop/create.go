package codexdesktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Preparing a draft cannot submit it. The durable sending transition happens
// before Submit; any crash or error after it must be reconciled, never retried.
type desktopDraft interface {
	Submit(context.Context) (string, error)
	Close()
}

// These two commands are hidden menu accelerators in the supported Desktop.
// Do not fire their default keys if the user has remapped or reused them.
func checkCreationKeymap(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(strings.TrimSpace(string(data))) == 0) {
		return nil
	}
	var entries []struct {
		Command string  `json:"command"`
		Key     *string `json:"key"`
	}
	if err != nil || json.Unmarshal(data, &entries) != nil {
		return fmt.Errorf("desktop-keybindings-unreadable")
	}
	normalize := func(key string) string {
		parts := strings.Split(strings.ToLower(key), "+")
		for i, p := range parts {
			p = strings.TrimSpace(p)
			switch p {
			case "command", "cmdorctrl", "commandorcontrol", "meta", "super":
				p = "cmd"
			case "option":
				p = "alt"
			}
			parts[i] = p
		}
		sort.Strings(parts)
		return strings.Join(parts, "+")
	}
	for _, e := range entries {
		key := ""
		if e.Key != nil {
			key = normalize(*e.Key)
		}
		for command, expected := range map[string]string{"newProjectlessTask": "alt+cmd+o", "copyDeeplink": "alt+cmd+l"} {
			if (strings.TrimPrefix(e.Command, "-") == command && (e.Command != command || key != expected)) || (e.Command != command && key == expected) {
				return fmt.Errorf("自动创建所需的 Desktop 快捷键已被自定义，请先绑定已有会话；Brodge 不会修改快捷键设置")
			}
		}
	}
	return nil
}

func (a *Agent) creationMedia(j job) ([]core.ConversationMedia, error) {
	var raw string
	if err := a.store.db.QueryRow(`SELECT media FROM jobs WHERE id=?`, j.ID).Scan(&raw); err != nil {
		return nil, err
	}
	var media []core.ConversationMedia
	if err := json.Unmarshal([]byte(raw), &media); err != nil {
		return nil, err
	}
	for _, m := range media {
		data, err := readMedia(m.Path)
		if err != nil {
			return nil, fmt.Errorf("creation-media-unavailable")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != m.SHA256 {
			return nil, fmt.Errorf("creation-media-changed")
		}
	}
	return media, nil
}

func creationPrompt(j job) string {
	if strings.TrimSpace(j.Prompt) == "" {
		return "请查看收到的图片或附件。"
	}
	return j.Prompt
}

func (a *Agent) creationAllowed(j job) bool {
	var origin core.ConversationOrigin
	if json.Unmarshal([]byte(j.Source), &origin) != nil {
		return false
	}
	a.mu.Lock()
	p := a.platforms[j.Platform]
	a.mu.Unlock()
	sender, ok := p.(core.DurableConversationPlatform)
	return ok && sender.AcceptConversationOrigin(origin)
}

func (a *Agent) creationError(j job, state string, cause error) error {
	if _, err := a.store.db.Exec(`UPDATE jobs SET state=?,error=? WHERE id=?`, state, cause.Error(), j.ID); err != nil {
		return err
	}
	text := "自动新建尚未发送首条消息。请在 Brodge 查看原因，处理后点重试创建。"
	if state != "create_failed" {
		text = "自动新建的发送结果待确认，不会重复发送。请把刚创建的 Desktop 会话链接填入 Brodge，确认原会话后接续。"
	}
	return a.store.notice(j, state, text+"\n"+cause.Error())
}

func (a *Agent) advanceCreation(ctx context.Context) error {
	a.creationMu.Lock()
	defer a.creationMu.Unlock()
	rows, err := a.store.db.Query(`SELECT ` + jobCols + ` FROM jobs WHERE state IN ('create_pending','create_verifying') ORDER BY rowid`)
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
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	created := false
	for _, j := range jobs {
		if j.State == "create_pending" {
			if created {
				continue
			}
			created = true
		}
		if err := a.advanceOneCreation(ctx, j); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) advanceOneCreation(ctx context.Context, j job) error {
	var err error
	if !a.creationAllowed(j) {
		_, err = a.store.db.Exec(`UPDATE jobs SET state='blocked',error='authorization-scope-changed' WHERE id=?`, j.ID)
		return err
	}
	if j.State == "create_pending" {
		media, err := a.creationMedia(j)
		if err != nil {
			return a.creationError(j, "create_failed", err)
		}
		if _, err = a.store.db.Exec(`UPDATE jobs SET state='create_preparing',error='' WHERE id=? AND state='create_pending'`, j.ID); err != nil {
			return err
		}
		draft, err := a.prepareNew(ctx, creationPrompt(j), media)
		if err != nil {
			return a.creationError(j, "create_failed", err)
		}
		defer draft.Close()
		if ctx.Err() != nil {
			return a.creationError(j, "create_failed", ctx.Err())
		}
		if !a.creationAllowed(j) {
			return a.creationError(j, "create_failed", fmt.Errorf("authorization-scope-changed"))
		}
		if _, err = a.store.db.Exec(`UPDATE jobs SET state='create_sending' WHERE id=?`, j.ID); err != nil {
			return err
		}
		link, err := draft.Submit(ctx)
		if err != nil {
			return a.creationError(j, "create_uncertain", err)
		}
		j.Thread, err = parseThread(link)
		if err != nil {
			return a.creationError(j, "create_uncertain", fmt.Errorf("creation-link-unavailable"))
		}
		if _, err = a.store.db.Exec(`UPDATE jobs SET state='create_verifying',thread=? WHERE id=?`, j.Thread, j.ID); err != nil {
			return err
		}
	}
	if err := a.verifyCreation(ctx, j); err != nil {
		state := "create_uncertain"
		if err.Error() == "creation-history-unavailable" {
			state = "create_verifying"
		}
		return a.creationError(j, state, err)
	}
	return nil
}

// Only Desktop's first turn, with the original text and every input resource,
// can supply the real client message ID. A recently visible thread is not proof.
func (a *Agent) verifyCreation(ctx context.Context, j job) error {
	at, err := a.attach(ctx, a.socket, j.Thread, a.timeout)
	if err != nil {
		return fmt.Errorf("creation-history-unavailable")
	}
	defer at.c.abort(fmt.Errorf("detached"))
	state, err := at.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("creation-history-unavailable")
	}
	media, err := a.creationMedia(j)
	if err != nil {
		return err
	}
	turn, err := firstCreationTurn(state, j.Thread, creationPrompt(j), media)
	if err != nil {
		return err
	}
	if !a.creationAllowed(j) {
		return fmt.Errorf("authorization-scope-changed")
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO binding_threads VALUES(?,?)`, j.Origin, j.Thread); err != nil {
		return err
	}
	// A manual binding made while creation ran remains the current selection.
	if _, err = tx.Exec(`INSERT INTO bindings VALUES(?,?,1) ON CONFLICT(origin) DO NOTHING`, j.Origin, j.Thread); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE jobs SET thread=?,client_message=?,turn=?,state='running',error='',revision=1 WHERE id=? AND state IN ('create_verifying','create_uncertain')`, j.Thread, str(obj(turn["params"])["clientUserMessageId"]), str(turn["turnId"]), j.ID); err != nil {
		return err
	}
	var origin core.ConversationOrigin
	if err = json.Unmarshal([]byte(j.Source), &origin); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE inbox SET state='done',bound_thread=?,bound_revision=1 WHERE app=? AND tenant=? AND message=? AND state='creating'`, j.Thread, origin.AppID, origin.Tenant, origin.MessageID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE outbox SET state='skipped' WHERE job=? AND kind IN ('create_failed','create_uncertain','create_verifying') AND state='ready'`, j.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func firstCreationTurn(state wire, thread, prompt string, media []core.ConversationMedia) (wire, error) {
	turns, err := turnsOf(state, thread)
	if err != nil {
		return nil, err
	}
	if kind := str(state["workspaceKind"]); kind != "" && kind != "projectless" {
		return nil, fmt.Errorf("creation-is-not-projectless")
	}
	if len(turns) == 2 && marshal(turns[0]) == marshal(turns[1]) {
		turns = turns[:1]
	}
	if len(turns) == 0 {
		return nil, fmt.Errorf("creation-history-unavailable")
	}
	if len(turns) != 1 {
		return nil, fmt.Errorf("creation-first-turn-not-confirmed")
	}
	t := turns[0]
	p := obj(t["params"])
	if str(t["turnId"]) == "" || str(p["clientUserMessageId"]) == "" {
		return nil, fmt.Errorf("creation-turn-identity-missing")
	}
	var texts []string
	var imagePaths []string
	for _, v := range list(p["input"]) {
		i := obj(v)
		switch str(i["type"]) {
		case "text":
			texts = append(texts, str(i["text"]))
		case "localImage":
			imagePaths = append(imagePaths, str(i["path"]))
		default:
			return nil, fmt.Errorf("creation-input-schema-unsupported")
		}
	}
	text := strings.Join(texts, "\n")
	if len(media) == 0 {
		if text != prompt || len(imagePaths) != 0 {
			return nil, fmt.Errorf("creation-input-mismatch")
		}
	} else {
		// Native Desktop adds its own attachment preamble to the original text.
		if text != prompt && !strings.HasSuffix(text, "\n"+prompt) {
			return nil, fmt.Errorf("creation-input-mismatch")
		}
		images := 0
		used := make([]bool, len(imagePaths))
		for _, m := range media {
			if m.Kind == "file" {
				if !strings.Contains(text, m.Path) {
					return nil, fmt.Errorf("creation-file-evidence-missing")
				}
				continue
			}
			images++
			found := false
			for index, path := range imagePaths {
				if used[index] {
					continue
				}
				data, e := readMedia(path)
				if e != nil {
					continue
				}
				sum := sha256.Sum256(data)
				if hex.EncodeToString(sum[:]) == m.SHA256 {
					found = true
					used[index] = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("creation-image-evidence-missing")
			}
		}
		if len(imagePaths) != images {
			return nil, fmt.Errorf("creation-image-count-mismatch")
		}
	}
	return t, nil
}

// Only a failure before Submit can be retried. Unknown submissions require an
// explicit candidate link and exact first-turn evidence instead.
func (a *Agent) RetryConversationCreation(ctx context.Context, id, thread string) error {
	a.creationMu.Lock()
	defer a.creationMu.Unlock()
	j, err := scanJob(a.store.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if err != nil {
		return err
	}
	if !a.creationAllowed(j) {
		return fmt.Errorf("authorization-scope-changed")
	}
	if thread == "" {
		if j.State != "create_failed" {
			return fmt.Errorf("只能重试明确尚未发送的创建；发送结果不确定时请填写已创建的会话链接")
		}
		_, err = a.store.db.Exec(`UPDATE jobs SET state='create_pending',error='' WHERE id=? AND state='create_failed'`, id)
		return err
	}
	if j.State != "create_uncertain" && j.State != "create_verifying" {
		return fmt.Errorf("creation-not-awaiting-confirmation")
	}
	j.Thread, err = parseThread(thread)
	if err != nil {
		return err
	}
	return a.verifyCreation(ctx, j)
}
