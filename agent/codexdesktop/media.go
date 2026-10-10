package codexdesktop

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const maxMedia = 30 << 20

// Called only by the existing worker, between steps. All snapshot creation and
// reference commits run on that same worker; ingress contains remote keys only.
// Historical references are retained because later turns may still read them.
func (a *Agent) cleanMedia(now time.Time) (int, error) {
	rows, err := a.store.db.Query(`SELECT media,0 FROM inbox UNION ALL SELECT media,0 FROM jobs UNION ALL SELECT body,1 FROM outbox WHERE kind LIKE 'media:%'`)
	if err != nil {
		return 0, err
	}
	refs := map[string]bool{}
	for rows.Next() {
		var encoded string
		var single bool
		if err = rows.Scan(&encoded, &single); err != nil {
			break
		}
		var media []core.ConversationMedia
		if single {
			encoded = "[" + encoded + "]"
		}
		if err = json.Unmarshal([]byte(encoded), &media); err != nil {
			break
		}
		for _, m := range media {
			if m.Path != "" {
				refs[filepath.Clean(m.Path)] = true
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("media-reference-scan-failed")
	}
	cache := filepath.Join(a.dataDir, "media")
	info, err := os.Lstat(cache)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("unsafe-media-cache")
	}
	root, err := os.OpenRoot(cache)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	dirs, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, dir := range dirs {
		hash, e := hex.DecodeString(dir.Name())
		if !dir.IsDir() || e != nil || len(hash) != sha256.Size {
			continue
		}
		files, e := fs.ReadDir(root.FS(), dir.Name())
		if e != nil {
			return removed, e
		}
		for _, file := range files {
			rel := filepath.Join(dir.Name(), file.Name())
			info, e := root.Lstat(rel)
			if e != nil {
				return removed, e
			}
			if !info.Mode().IsRegular() || refs[filepath.Join(cache, rel)] || !info.ModTime().Before(now.Add(-24*time.Hour)) {
				continue
			}
			if e = root.Remove(rel); e != nil {
				return removed, e
			}
			removed++
		}
		// Remove only an empty hash directory, never recursively.
		_ = root.Remove(dir.Name())
	}
	return removed, nil
}

func (a *Agent) snapshotMedia(data []byte, name string) (string, string, error) {
	if len(data) == 0 || len(data) > maxMedia {
		return "", "", fmt.Errorf("empty-or-oversized-resource")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	dir := filepath.Join(a.dataDir, "media", hash)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if os.IsExist(err) {
		existing, e := readMedia(path)
		if e != nil {
			return "", "", e
		}
		s := sha256.Sum256(existing)
		if s != sum {
			return "", "", fmt.Errorf("media-cache-conflict")
		}
		return path, hash, nil
	}
	if err != nil {
		return "", "", err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Chmod(0400)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return "", "", err
	}
	return path, hash, nil
}
func readMedia(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMedia {
		return nil, fmt.Errorf("unsupported-media-file")
	}
	data := make([]byte, info.Size())
	_, err = io.ReadFull(f, data)
	return data, err
}
func (a *Agent) prepareMedia(ctx context.Context, p core.Platform, o core.ConversationOrigin, media []core.ConversationMedia) ([]core.ConversationMedia, error) {
	downloader, ok := p.(core.ConversationMediaPlatform)
	if !ok {
		return nil, fmt.Errorf("media-adapter-unavailable")
	}
	for i, m := range media {
		if m.Kind != "image" && m.Kind != "file" || m.Key == "" {
			return nil, fmt.Errorf("unsupported-resource")
		}
		if m.Path != "" {
			data, err := readMedia(m.Path)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != m.SHA256 {
				return nil, fmt.Errorf("media-cache-conflict")
			}
			continue
		}
		data, err := downloader.DownloadConversationMedia(ctx, o, m)
		if err != nil {
			return nil, err
		}
		m.Path, m.SHA256, err = a.snapshotMedia(data, m.Name)
		if err != nil {
			return nil, err
		}
		media[i] = m
	}
	return media, nil
}
func (a *Agent) rejectMedia(app, tenant, message, source, platform string, cause error) error {
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = a.store.output(tx, "ingress:"+app+":"+tenant+":"+message, "media-error", source, platform, "图片或附件下载失败，本条消息尚未交给 Codex。请重新发送。原因："+cause.Error()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE inbox SET state='done' WHERE app=? AND tenant=? AND message=?`, app, tenant, message); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *Agent) turnInput(j job) ([]any, []any, error) {
	var encoded string
	if err := a.store.db.QueryRow(`SELECT media FROM jobs WHERE id=?`, j.ID).Scan(&encoded); err != nil {
		return nil, nil, err
	}
	var media []core.ConversationMedia
	if err := json.Unmarshal([]byte(encoded), &media); err != nil {
		return nil, nil, err
	}
	prompt := j.Prompt
	attachments := []any{}
	images := []any{}
	for _, m := range media {
		data, err := readMedia(m.Path)
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != m.SHA256 {
			return nil, nil, fmt.Errorf("media-snapshot-changed")
		}
		attachments = append(attachments, wire{"label": m.Name, "path": m.Path, "fsPath": m.Path, "isImageAttachment": m.Kind == "image"})
		prompt += "\n附件 " + marshal(m.Name) + "：" + marshal(m.Path)
		if m.Kind == "image" {
			images = append(images, wire{"type": "localImage", "path": m.Path})
		}
	}
	if prompt == "" {
		prompt = "请查看收到的图片或附件。"
	}
	return append([]any{wire{"type": "text", "text": prompt, "text_elements": []any{}}}, images...), attachments, nil
}

// Keep output snapshots and exact upload keys in the existing durable outbox.
func mediaReferences(media []core.ConversationMedia) string {
	refs := make([]core.ConversationMedia, 0, len(media))
	for _, m := range media {
		// Derived parent attachments were absent from the original event. The
		// immutable source ParentID already participates in event deduplication.
		if m.MessageID != "" {
			continue
		}
		m.Path, m.SHA256 = "", ""
		refs = append(refs, m)
	}
	return marshalMedia(refs)
}
func sameMediaReferences(a, b string) bool {
	var x, y []core.ConversationMedia
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return mediaReferences(x) == mediaReferences(y)
}
func (a *Agent) queueMedia(tx *sql.Tx, j job, kind string, m core.ConversationMedia) error {
	data, err := readMedia(m.Path)
	if err != nil {
		return err
	}
	m.Name = filepath.Base(m.Path)
	m.Path, m.SHA256, err = a.snapshotMedia(data, m.Name)
	if err != nil {
		return err
	}
	return a.store.output(tx, j.ID, kind, j.Source, j.Platform, marshal(m))
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
