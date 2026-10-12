package codexdesktop

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMessagePreviewTextAndMedia(t *testing.T) {
	for _, tc := range []struct{ prompt, media, want string }{
		{"\n你好\t世界", "[]", "你好 世界"},
		{"", `[{"kind":"image","key":"private-key","path":"/private/cache/image.png"}]`, "[图片]"},
		{"", `[{"kind":"file","name":"/private/cache/计划.pdf","path":"/private/cache/file","key":"private-key"}]`, "[附件：计划.pdf]"},
		{"", `[{"kind":"file"}]`, "[附件]"},
		{"", "[]", "（无文字消息）"},
		{"仍显示原消息", "invalid-json", "仍显示原消息"},
		{"# Files mentioned by the user:\n/private/cache/report\n## My request:\n看一下报告", "[]", "看一下报告"},
	} {
		if got := messagePreview(tc.prompt, tc.media); got != tc.want {
			t.Fatalf("preview %q, want %q", got, tc.want)
		}
	}
	got := messagePreview(strings.Repeat("中文😀", 100), "[]")
	if !utf8.ValidString(got) || len([]rune(got)) != 161 || !strings.HasSuffix(got, "…") {
		t.Fatal("invalid or unbounded Unicode preview", got)
	}
}

func TestDisplayMetadataFallsBackWithoutReadingUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE threads(id TEXT,title TEXT,rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "unrelated.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"private unrelated content"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	title := "# Files mentioned by the user:\n/private/file.pdf\n## My request:\n核对这份 PDF"
	if _, err = db.Exec(`INSERT INTO threads VALUES('T1',?,?); INSERT INTO threads VALUES('unbound','do not display',?)`, title, outside, outside); err != nil {
		t.Fatal(err)
	}
	m := readDesktopMetadata(context.Background(), root, []string{"T1"}, map[string]queryPreview{})
	if len(m) != 1 || m["T1"].title != "核对这份 PDF" || m["T1"].query != "" {
		t.Fatal(m)
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sessions", "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if historyPath(root, link) {
		t.Fatal("followed history symlink outside Desktop")
	}
}

func TestLatestQueryPreviewParsesUserMessagesOnly(t *testing.T) {
	if got := previewText("看一下图片\n<image name=[Image #1] path=\"/private/image.png\">\n</image>", 240); got != "看一下图片" {
		t.Fatal("image transport metadata leaked", got)
	}
	for _, tc := range []struct{ line, want string }{
		{`{"type":"event_msg","payload":{"type":"user_message","message":"真实问题"}}`, "真实问题"},
		{`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image"}]}}`, "[图片]"},
		{`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"不是用户问题"}]}}`, ""},
		{`{"type":"event_msg","payload":{"type":"agent_message","message":"不是用户问题"}}`, ""},
		{`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>metadata</environment_context>"}]}}`, ""},
		{`{"type":"event_msg","payload":`, ""},
	} {
		if got := rolloutUserText([]byte(tc.line)); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
	if got := previewText(strings.Repeat("问", 250), 240); len([]rune(got)) != 241 || !strings.HasSuffix(got, "…") {
		t.Fatal("invalid Unicode truncation")
	}
}
