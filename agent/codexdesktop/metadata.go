package codexdesktop

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type threadMetadata struct{ title, query string }
type queryPreview struct {
	size     int64
	modified time.Time
	text     string
}

// Commands need names only, not a scan of each conversation's message history.
// Read the same optional Desktop metadata as Web; an unavailable name falls
// back to the ID so the conversation remains identifiable and switchable.
func (a *Agent) commandThreadTitles(ctx context.Context, ids ...string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	metadata := readDesktopMetadata(ctx, a.metadataRoot, ids, nil)
	titles := make(map[string]string, len(ids))
	for _, id := range ids {
		title := metadata[id].title
		if title == "" {
			title = id
		}
		titles[id] = title
	}
	return titles
}

// Labels are optional, read-only data. A missing index or failed name lookup
// must not affect bindings or activate a Desktop conversation.
func (a *Agent) addConversationLabels(out map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for _, value := range out["bindings"].([]any) {
		b := value.(map[string]any)
		origin := b["source"].(core.ConversationOrigin)
		a.mu.Lock()
		platform := a.platforms[b["platform"].(string)]
		a.mu.Unlock()
		delete(b, "source")
		delete(b, "platform")
		label := "私聊"
		if origin.ChatType == "group" {
			label = "群聊（名称暂不可用）"
			if namer, ok := platform.(core.ConversationChatNamer); ok {
				if name, _ := namer.ConversationChatName(ctx, origin); strings.TrimSpace(name) != "" {
					label = strings.TrimSpace(name) + " · 群聊"
				}
			}
		}
		b["display_name"] = label + " · 我"
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, section := range []string{"conversations", "messages"} {
		for _, value := range out[section].([]any) {
			id := value.(map[string]string)["thread"]
			if id != "" && !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	}
	// Parsing unchanged history is cached; titles are read again so Desktop
	// renames appear on the next refresh. This cache never participates in ingress.
	a.metadataMu.Lock()
	metadataCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	metadata := readDesktopMetadata(metadataCtx, a.metadataRoot, ids, a.queryPreviews)
	a.metadataMu.Unlock()
	for _, section := range []string{"conversations", "messages"} {
		for _, value := range out[section].([]any) {
			entry := value.(map[string]string)
			m := metadata[entry["thread"]]
			if m.title == "" {
				m.title = "未命名会话"
			}
			entry["title"] = m.title
			if section == "conversations" {
				entry["latest_query"] = m.query
			}
		}
	}
}

func readDesktopMetadata(ctx context.Context, root string, ids []string, cache map[string]queryPreview) map[string]threadMetadata {
	result := map[string]threadMetadata{}
	if root == "" || len(ids) == 0 {
		return result
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(root, "state_5.sqlite"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return result
	}
	defer db.Close()
	// Older Desktop versions have title but no separate user-facing name.
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(threads)`)
	if err != nil {
		return result
	}
	hasName := false
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if rows.Scan(&cid, &name, &typ, &nn, &def, &pk) != nil {
			rows.Close()
			return result
		}
		if name == "name" {
			hasName = true
		}
	}
	rows.Close()
	title := "COALESCE(title,'')"
	if hasName {
		title = "COALESCE(NULLIF(name,''),title,'')"
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err = db.QueryContext(ctx, `SELECT id,`+title+`,COALESCE(rollout_path,'') FROM threads WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, path string
		if rows.Scan(&id, &name, &path) != nil {
			continue
		}
		m := threadMetadata{title: previewText(name, 100)}
		// Read only the rollout referenced by this known thread, within Desktop's
		// history directories. Never follow an arbitrary path or inspect auth files.
		if cache != nil && historyPath(root, path) {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				old, ok := cache[path]
				if ok && old.size == info.Size() && old.modified.Equal(info.ModTime()) {
					m.query = old.text
				} else {
					if text, err := lastUserQuery(ctx, path, info.Size()); err == nil {
						m.query = previewText(text, 240)
						cache[path] = queryPreview{info.Size(), info.ModTime(), m.query}
					}
				}
			}
		}
		result[id] = m
	}
	return result
}

func historyPath(root, path string) bool {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(rel, "sessions"+string(filepath.Separator)) || strings.HasPrefix(rel, "archived_sessions"+string(filepath.Separator))
}

// Walk backward in chunks, stopping at the most recent real user message.
// A long assistant/tool turn cannot be mistaken for the last user query.
func lastUserQuery(ctx context.Context, path string, end int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var suffix []byte
	for end > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		start := end - 64*1024
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, end-start)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return "", err
		}
		lines := bytes.Split(append(chunk, suffix...), []byte{'\n'})
		first := 1
		if start == 0 {
			first = 0
		}
		for i := len(lines) - 1; i >= first; i-- {
			if text := rolloutUserText(lines[i]); text != "" {
				return text, nil
			}
		}
		suffix = lines[0]
		// Skip a single oversized record (for example an embedded image), keeping
		// memory bounded while continuing to older complete records.
		if len(suffix) > 16*1024*1024 {
			suffix = nil
		}
		end = start
	}
	return "", nil
}

func rolloutUserText(line []byte) string {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Message string `json:"message"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil {
		return ""
	}
	p := record.Payload
	var text string
	var media string
	if record.Type == "event_msg" && p.Type == "user_message" {
		text = p.Message
	}
	if record.Type == "response_item" && p.Type == "message" && p.Role == "user" {
		for _, c := range p.Content {
			if c.Type == "input_image" || c.Type == "image" {
				media = "[图片]"
			}
			if c.Type == "input_file" {
				media = "[文件]"
			}
			if c.Type == "input_text" || c.Type == "text" {
				text += c.Text + "\n"
			}
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = media
	}
	// Desktop adds these context messages; they are not questions from the user.
	if strings.HasPrefix(text, "# AGENTS.md instructions") || strings.HasPrefix(text, "<environment_context>") {
		return ""
	}
	return text
}

// Preview the persisted incoming message, not the conversation's latest query.
// Media paths/keys stay private; media-only messages still have a useful label.
func messagePreview(prompt, rawMedia string) string {
	var media []core.ConversationMedia
	var labels []string
	if json.Unmarshal([]byte(rawMedia), &media) == nil {
		for _, m := range media {
			if m.Kind == "image" {
				labels = append(labels, "[图片]")
			} else {
				name := previewText(filepath.Base(strings.ReplaceAll(m.Name, "\\", "/")), 60)
				if name == "" || name == "." || name == "/" {
					labels = append(labels, "[附件]")
				} else {
					labels = append(labels, "[附件："+name+"]")
				}
			}
		}
	}
	text := previewText(strings.Join(labels, " ")+" "+previewText(prompt, 160), 160)
	if text == "" {
		return "（无文字消息）"
	}
	return text
}

func previewText(text string, limit int) string {
	// Native first-message wrappers contain paths before the user's request.
	if strings.HasPrefix(text, "# Files mentioned by the user:") {
		if _, request, ok := strings.Cut(text, "## My request:"); ok {
			text = request
		}
	}
	// Our own attachment suffix is transport metadata, not the user's question.
	if at := strings.Index(text, "\n附件 \""); at >= 0 {
		text = text[:at]
	}
	// Desktop appends these image-location markers to the recorded user text.
	hadImage := false
	for {
		start := strings.Index(text, "<image name=[Image #")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "</image>")
		if end < 0 {
			break
		}
		text = text[:start] + " " + text[start+end+len("</image>"):]
		hadImage = true
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" && hadImage {
		text = "[图片]"
	}
	chars := []rune(text)
	if len(chars) > limit {
		return string(chars[:limit]) + "…"
	}
	return text
}
