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
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", platformName: "feishu", allowFrom: "actor", botOpenID: "bot"}
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
		func(e *larkim.P2MessageReceiveV1) { s := ""; e.Event.Message.ChatId = &s },
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
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", allowFrom: "actor"}
	p.SetConversationHandler(func(context.Context, core.Platform, *core.Message) error { return fmt.Errorf("database unavailable") })
	if err := p.onDesktopMessage(context.Background(), desktopEvent(t, "p2p", false)); err == nil {
		t.Fatal("persistence error ACKed")
	}
}

func TestGroupPostImageResolvesAuthenticatedMentionPlaceholder(t *testing.T) {
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", platformName: "feishu", allowFrom: "actor", botOpenID: "bot"}
	var received []*core.Message
	p.SetConversationHandler(func(_ context.Context, _ core.Platform, m *core.Message) error {
		received = append(received, m)
		return nil
	})
	e := desktopEvent(t, "group", true)
	kind := "post"
	body := `{"title":"","content":[[{"tag":"at","user_id":"@_user_1","style":[]},{"tag":"text","text":" read this image","style":[]}],[{"tag":"img","image_key":"image-key","width":680,"height":360}]]}`
	e.Event.Message.MessageType, e.Event.Message.Content = &kind, &body
	if err := p.onDesktopMessage(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Content != "read this image" || len(received[0].Media) != 1 || received[0].Media[0].Key != "image-key" {
		t.Fatal("actual Feishu post shape lost text or image")
	}
	// A made-up body placeholder cannot borrow another real mention.
	body = strings.ReplaceAll(body, "@_user_1", "@_user_2")
	if err := p.onDesktopMessage(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[1].Content != "/unsupported-attachment" || len(received[1].Media) != 0 {
		t.Fatal("unresolved mention accepted")
	}
	e.Event.Message.Mentions = nil
	if err := p.onDesktopMessage(context.Background(), e); err != nil || len(received) != 2 {
		t.Fatal("body mention bypassed group identity check", err)
	}
}

func TestCreatorCanUseAnyChatWithoutGroupAllowlist(t *testing.T) {
	// Legacy allow_chat must no longer restrict where the creator can use the bot.
	platform, err := New(map[string]any{"app_id": "app", "app_secret": "secret", "tenant_key": "tenant", "allow_from": "actor", "allow_chat": "old-private-chat"})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	p.botOpenID = "bot"
	var received []*core.Message
	p.SetConversationHandler(func(_ context.Context, _ core.Platform, m *core.Message) error {
		if !p.AcceptConversationOrigin(*m.ConversationOrigin) {
			t.Fatal("accepted ingress cannot be replied to")
		}
		received = append(received, m)
		return nil
	})
	for _, chat := range []string{"private", "group-one", "group-two"} {
		kind := "group"
		if chat == "private" {
			kind = "p2p"
		}
		e := desktopEvent(t, kind, kind == "group")
		e.Event.Message.ChatId = &chat
		if err := p.onDesktopMessage(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		other := "other-user"
		e.Event.Sender.SenderId.OpenId = &other
		if err := p.onDesktopMessage(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 3 || received[0].SessionKey == received[1].SessionKey || received[1].SessionKey == received[2].SessionKey {
		t.Fatal("creator chats were rejected, mixed, or another user was admitted")
	}
	e := desktopEvent(t, "group", false)
	if err := p.onDesktopMessage(context.Background(), e); err != nil || len(received) != 3 {
		t.Fatal("group input without a real bot mention was admitted", err)
	}
	for _, m := range received {
		origin := *m.ConversationOrigin
		origin.Actor = "other-user"
		if p.AcceptConversationOrigin(origin) {
			t.Fatal("reply accepted another user")
		}
		origin.Actor, origin.ChatID = "actor", ""
		if p.AcceptConversationOrigin(origin) {
			t.Fatal("reply accepted missing chat")
		}
	}
	if _, err := New(map[string]any{"app_id": "app", "app_secret": "secret", "tenant_key": "tenant", "allow_from": "actor"}); err != nil {
		t.Fatal("group allowlist is still required", err)
	}
}
func TestDesktopReplyRefusesForeignOriginBeforeNetwork(t *testing.T) {
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", allowFrom: "actor"}
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
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "app", allowFrom: "actor"}
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
	p := &Platform{desktopBound: true, desktopTenant: "tenant", appID: "local-test-app", allowFrom: "actor", client: lark.NewClient("local-test-app", "fake-local-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
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
	p := &Platform{desktopBound: true, appID: "resource-test", desktopTenant: "tenant", allowFrom: "actor", client: lark.NewClient("resource-test", "local-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
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

func TestOfficialSDKQuotedAttachmentChecksSourceAndDownloadsParent(t *testing.T) {
	message := map[string]any{"message_id": "parent", "chat_id": "chat", "msg_type": "file", "sender": map[string]any{"id": "actor", "id_type": "open_id", "sender_type": "user", "tenant_key": "tenant"}, "body": map[string]string{"content": `{"file_key":"resource","file_name":"报告.pdf"}`}}
	resources := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"local-quote-token","expire":7200}`)
		case "/open-apis/im/v1/messages/parent":
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{message}}})
		case "/open-apis/im/v1/messages/parent/resources/resource":
			resources++
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
			fmt.Fprint(w, "PDF_BYTES")
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p := &Platform{desktopBound: true, appID: "quote-test", desktopTenant: "tenant", allowFrom: "actor", botOpenID: "bot", client: lark.NewClient("quote-test", "local-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	o := core.ConversationOrigin{AppID: p.appID, Tenant: "tenant", ChatType: "group", ChatID: "chat", Actor: "actor", MessageID: "request", ParentID: "parent"}
	media, err := p.ResolveConversationMedia(context.Background(), o)
	if err != nil || len(media) != 1 || media[0].MessageID != "parent" || media[0].Name != "报告.pdf" {
		t.Fatal(media, err)
	}
	data, err := p.DownloadConversationMedia(context.Background(), o, media[0])
	if err != nil || string(data) != "PDF_BYTES" || resources != 1 {
		t.Fatal("wrong attachment bytes or message", err)
	}
	media[0].MessageID = "foreign"
	if _, err = p.DownloadConversationMedia(context.Background(), o, media[0]); err == nil || resources != 1 {
		t.Fatal("foreign resource parent accepted")
	}
	for _, field := range []string{"message_id", "chat_id"} {
		old := message[field]
		message[field] = "other"
		if _, err = p.ResolveConversationMedia(context.Background(), o); err == nil {
			t.Fatal("foreign parent accepted", field)
		}
		message[field] = old
	}
	message["sender"].(map[string]any)["id"] = "other-user"
	if _, err = p.ResolveConversationMedia(context.Background(), o); err == nil {
		t.Fatal("another group member's attachment accepted")
	}
	message["sender"] = map[string]any{"id": p.appID, "id_type": "app_id", "sender_type": "app"}
	if _, err = p.ResolveConversationMedia(context.Background(), o); err != nil {
		t.Fatal("own bot artifact rejected", err)
	}
	message["msg_type"] = "interactive"
	if media, err = p.ResolveConversationMedia(context.Background(), o); err != nil || len(media) != 0 {
		t.Fatal("ordinary question reply treated as media", err)
	}
}
