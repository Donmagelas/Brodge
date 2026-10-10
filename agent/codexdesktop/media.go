package codexdesktop

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/chenhg5/cc-connect/core"
)

const maxMedia = 30 << 20

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
	for i := range media {
		media[i].Path = ""
		media[i].SHA256 = ""
	}
	return marshalMedia(media)
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
