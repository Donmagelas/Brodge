package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestDesktopSdkLogsDoNotExposeMessageOrConnectionCredentials(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	defer slog.SetDefault(previous)
	logger := pairLogger{}
	ctx := context.Background()
	for _, entry := range []string{"payload: private-chat-body", "access_key=private-connection-token", "ticket=private-ticket"} {
		logger.Debug(ctx, entry)
		logger.Info(ctx, entry)
		logger.Warn(ctx, entry)
		logger.Error(ctx, entry)
	}
	for _, sensitive := range []string{"private-chat-body", "private-connection-token", "private-ticket"} {
		if strings.Contains(output.String(), sensitive) {
			t.Fatal("SDK log disclosed sensitive data")
		}
	}
	if !strings.Contains(output.String(), "Feishu connection or event handling failed") {
		t.Fatal("failure signal was lost")
	}
}

func desktopEvent(t *testing.T, chatType string, mention bool) *larkim.P2MessageReceiveV1 {
	t.Helper()
	mentions := []any{}
	if mention {
		mentions = append(mentions, map[string]any{"key": "@_user_1", "id": map[string]string{"open_id": "bot"}})
	}
	body := map[string]any{"schema": "2.0", "header": map[string]string{"app_id": "app", "tenant_key": "tenant"}, "event": map[string]any{
		"sender":  map[string]any{"sender_type": "user", "tenant_key": "tenant", "sender_id": map[string]string{"open_id": "actor"}},
		"message": map[string]any{"message_id": "message", "chat_id": "chat", "chat_type": chatType, "message_type": "text", "content": "{\"text\":\"@_user_1 hello\"}", "mentions": mentions}}}
	b, _ := json.Marshal(body)
	var e larkim.P2MessageReceiveV1
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return &e
}
func TestDesktopIngressValidatesOfficialSdkIdentityAndRealMention(t *testing.T) {
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", platformName: "feishu", allowFrom: "actor", allowChat: "chat", botOpenID: "bot"}
	calls := 0
	p.SetConversationHandler(func(_ context.Context, _ core.Platform, m *core.Message) error {
		calls++
		if m.ConversationOrigin == nil || m.ConversationOrigin.Actor != "actor" || m.Content != "hello" {
			t.Fatal(m)
		}
		return nil
	})
	for _, kind := range []string{"p2p", "group"} {
		if err := p.onDesktopMessage(context.Background(), desktopEvent(t, kind, true)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	_ = p.onDesktopMessage(context.Background(), desktopEvent(t, "group", false))
	if calls != 2 {
		t.Fatal("literal mention accepted")
	}
	for _, mutation := range []func(*larkim.P2MessageReceiveV1){
		func(e *larkim.P2MessageReceiveV1) { e.EventV2Base.Header.AppID = "other" },
		func(e *larkim.P2MessageReceiveV1) { e.EventV2Base.Header.TenantKey = "other" },
		func(e *larkim.P2MessageReceiveV1) { s := "other"; e.Event.Sender.SenderId.OpenId = &s },
		func(e *larkim.P2MessageReceiveV1) { s := "other"; e.Event.Message.ChatId = &s },
		func(e *larkim.P2MessageReceiveV1) { s := "app"; e.Event.Sender.SenderType = &s },
	} {
		e := desktopEvent(t, "group", true)
		mutation(e)
		_ = p.onDesktopMessage(context.Background(), e)
	}
	if calls != 2 {
		t.Fatal("unauthorized ingress admitted")
	}
}
func TestDesktopIngressPersistenceFailureReachesDispatcher(t *testing.T) {
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", allowFrom: "actor", allowChat: "chat"}
	p.SetConversationHandler(func(context.Context, core.Platform, *core.Message) error { return fmt.Errorf("database unavailable") })
	if err := p.onDesktopMessage(context.Background(), desktopEvent(t, "p2p", false)); err == nil {
		t.Fatal("persistence error ACKed")
	}
}
func TestDesktopReplyRefusesForeignOriginBeforeNetwork(t *testing.T) {
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", allowFrom: "actor", allowChat: "chat"}
	_, err := p.ReplyConversation(context.Background(), core.ConversationOrigin{AppID: "other"}, "uuid", "hello")
	if err == nil {
		t.Fatal("foreign reply admitted")
	}
}

func TestDesktopAllowlistRejectsWhitespaceAndWildcard(t *testing.T) {
	for _, scope := range []string{"", " ", " * ", "actor,", "actor,*"} {
		if explicitDesktopScope(scope) {
			t.Fatal("unsafe scope", scope)
		}
	}
	if !explicitDesktopScope("actor, other") {
		t.Fatal("explicit scope rejected")
	}
}

func TestDesktopOfficialSdkDispatcherPreservesDurableFailure(t *testing.T) {
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", allowFrom: "actor", allowChat: "chat"}
	p.SetConversationHandler(func(context.Context, core.Platform, *core.Message) error { return fmt.Errorf("storage failure") })
	d := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(p.onDesktopMessage)
	e := desktopEvent(t, "p2p", false)
	e.EventV2Base.Header.EventType = "im.message.receive_v1"
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Do(context.Background(), b); err == nil {
		t.Fatal("SDK swallowed durable failure")
	}
}
func TestDesktopOfficialSdkReplyUsesOriginalMessageAndUuid(t *testing.T) {
	replies := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"fake-local-token","expire":7200}`)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/messages/original/reply" {
			t.Errorf("wrong route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["uuid"] != "persisted-uuid" || body["reply_in_thread"] != true || body["msg_type"] != "text" || body["content"] != `{"text":"hello"}` {
			t.Errorf("reply body: %#v", body)
		}
		replies++
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"receipt"}}`)
	}))
	defer server.Close()
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "local-test-app", allowFrom: "actor", allowChat: "chat", client: lark.NewClient("local-test-app", "fake-local-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	origin := core.ConversationOrigin{AppID: p.appID, Tenant: "tenant", Actor: "actor", ChatID: "chat", ChatType: "group", MessageID: "original", ThreadID: "topic"}
	id, err := p.ReplyConversation(context.Background(), origin, "persisted-uuid", "hello")
	if err != nil || id != "receipt" || replies != 1 {
		t.Fatal(id, err, replies)
	}
}

func TestCreatorPairingRejectsOtherIdentity(t *testing.T) {
	p := &Platform{appID: "app", desktopTenant: "tenant", allowFrom: "actor"}
	if err := p.SetCreator(core.ConversationOrigin{AppID: "app", Tenant: "tenant", Actor: "actor", ChatType: "p2p"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCreator(core.ConversationOrigin{AppID: "other", Tenant: "tenant", Actor: "other", ChatType: "p2p"}); err == nil {
		t.Fatal("foreign creator accepted")
	}
	if _, err := New(map[string]any{"app_id": "a", "app_secret": "s", "tenant_key": "t", "allow_from": "a,b", "allow_chat": "c"}); err == nil {
		t.Fatal("multiple creators enabled")
	}
}
func TestOfficialSDKResourceReactionMarkdownUploadAndMediaReply(t *testing.T) {
	data := []byte("exact-test-bytes")
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		requests[path]++
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(path, "tenant_access_token") {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"local-resource-token","expire":7200}`)
			return
		}
		switch path {
		case "/open-apis/im/v1/messages/original/reactions":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["reaction_type"].(map[string]any)["emoji_type"] != "OK" {
				t.Error(body)
			}
			fmt.Fprint(w, `{"code":0,"data":{"reaction_id":"reaction"}}`)
		case "/open-apis/im/v1/messages/original/resources/resource":
			if r.Method != "GET" || r.URL.Query().Get("type") != "file" {
				t.Error(r.URL)
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="test.txt"`)
			w.Write(data)
		case "/open-apis/im/v1/files", "/open-apis/im/v1/images":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			field := "file"
			if strings.HasSuffix(path, "images") {
				field = "image"
			}
			f, _, err := r.FormFile(field)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			actual, _ := io.ReadAll(f)
			if !bytes.Equal(data, actual) {
				t.Error("upload changed bytes")
			}
			if field == "file" && (r.FormValue("file_name") != "测试.txt" || r.FormValue("file_type") != "stream") {
				t.Error("filename or type lost")
			}
			fmt.Fprint(w, `{"code":0,"data":{"file_key":"file-key","image_key":"image-key"}}`)
		case "/open-apis/im/v1/messages/original/reply":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["uuid"] != "fixed" {
				t.Error("missing fixed UUID")
			}
			var content map[string]any
			json.Unmarshal([]byte(body["content"].(string)), &content)
			switch body["msg_type"] {
			case "file":
				if content["file_key"] != "file-key" {
					t.Error(content)
				}
			case "image":
				if content["image_key"] != "image-key" {
					t.Error(content)
				}
			case "interactive":
				if !strings.Contains(body["content"].(string), "markdown") {
					t.Error(content)
				}
			default:
				t.Error(body)
			}
			fmt.Fprint(w, `{"code":0,"data":{"message_id":"receipt"}}`)
		default:
			t.Error("unexpected API", path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p := &Platform{desktopBound: true, appID: "resource-test", desktopTenant: "tenant", allowFrom: "actor", allowChat: "chat", client: lark.NewClient("resource-test", "local-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	o := core.ConversationOrigin{AppID: p.appID, Tenant: "tenant", ChatType: "p2p", ChatID: "chat", Actor: "actor", MessageID: "original"}
	if _, err := p.ReactConversation(context.Background(), o, "OK"); err != nil {
		t.Fatal(err)
	}
	actual, err := p.DownloadConversationMedia(context.Background(), o, core.ConversationMedia{Kind: "file", Key: "resource"})
	if err != nil || !bytes.Equal(data, actual) {
		t.Fatal("resource failed", err)
	}
	path := filepath.Join(t.TempDir(), "测试.txt")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"file", "image"} {
		m := core.ConversationMedia{Kind: kind, Name: "测试.txt", Path: path}
		m.Key, err = p.UploadConversationMedia(context.Background(), o, m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.ReplyConversationMedia(context.Background(), o, "fixed", m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = p.ReplyConversation(context.Background(), o, "fixed", "**标题**\n```go\nfmt.Println(1)\n```"); err != nil {
		t.Fatal(err)
	}
	if requests["/open-apis/im/v1/messages/original/reply"] != 3 {
		t.Fatal(requests)
	}
}
