package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func pairingRecord() setupRecord {
	return setupRecord{SetupState: SetupState{Status: "created", AppID: "app", OwnerOpenID: "actor", BotOpenID: "bot", PairingCode: "BRIDGE_test_code", PairingExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, AppSecret: "private-pair-secret"}
}

func pairingEvent(t *testing.T) *larkim.P2MessageReceiveV1 {
	e := desktopEvent(t, "p2p", false)
	content := `{"text":"BRIDGE_test_code"}`
	e.Event.Message.Content = &content
	return e
}

func TestDesktopFeishuPairOnlyAcceptsCreatorPrivateCode(t *testing.T) {
	r := pairingRecord()
	origin, ok := pairingOrigin(r, pairingEvent(t))
	if !ok || origin != (core.ConversationOrigin{AppID: "app", Tenant: "tenant", Actor: "actor", ChatID: "chat", ChatType: "p2p", MessageID: "message"}) {
		t.Fatal("valid creator pairing rejected")
	}
	for name, mutate := range map[string]func(*larkim.P2MessageReceiveV1){
		"other app":       func(e *larkim.P2MessageReceiveV1) { e.EventV2Base.Header.AppID = "other" },
		"other tenant":    func(e *larkim.P2MessageReceiveV1) { e.EventV2Base.Header.TenantKey = "other" },
		"other creator":   func(e *larkim.P2MessageReceiveV1) { v := "other"; e.Event.Sender.SenderId.OpenId = &v },
		"bot":             func(e *larkim.P2MessageReceiveV1) { v := "app"; e.Event.Sender.SenderType = &v },
		"group":           func(e *larkim.P2MessageReceiveV1) { v := "group"; e.Event.Message.ChatType = &v },
		"attachment":      func(e *larkim.P2MessageReceiveV1) { v := "file"; e.Event.Message.MessageType = &v },
		"missing message": func(e *larkim.P2MessageReceiveV1) { e.Event.Message.MessageId = nil },
		"missing chat":    func(e *larkim.P2MessageReceiveV1) { e.Event.Message.ChatId = nil },
		"wrong code":      func(e *larkim.P2MessageReceiveV1) { v := `{"text":"BRIDGE_wrong"}`; e.Event.Message.Content = &v },
		"ordinary text":   func(e *larkim.P2MessageReceiveV1) { v := `{"text":"hello"}`; e.Event.Message.Content = &v },
		"missing sender":  func(e *larkim.P2MessageReceiveV1) { e.Event.Sender = nil },
		"missing header":  func(e *larkim.P2MessageReceiveV1) { e.EventV2Base.Header = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := pairingEvent(t)
			mutate(e)
			if _, ok := pairingOrigin(r, e); ok {
				t.Fatal("foreign or invalid event paired")
			}
		})
	}
	for _, mutate := range []func(*setupRecord){
		func(r *setupRecord) { r.OwnerOpenID = "" },
		func(r *setupRecord) { r.TenantKey = "other" },
		func(r *setupRecord) { r.PairingCode = "" },
		func(r *setupRecord) { r.PairingExpiresAt = time.Now().Add(-time.Second).UnixMilli() },
	} {
		bad := r
		mutate(&bad)
		if _, ok := pairingOrigin(bad, pairingEvent(t)); ok {
			t.Fatal("invalid pairing scope accepted")
		}
	}
}

func newPairSetup(t *testing.T) *Setup {
	s := newTestSetup(t)
	if err := s.update(func(r *setupRecord) { *r = pairingRecord(); r.PairingCode = ""; r.PairingExpiresAt = 0 }); err != nil {
		t.Fatal(err)
	}
	return s
}

func awaitSetupIdle(t *testing.T, s *Setup) {
	t.Helper()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("pairing child did not finish")
		}
	}
}

func TestDesktopFeishuPairPersistsOriginWithoutCreatingModelWork(t *testing.T) {
	s := newPairSetup(t)
	s.connection = func(ctx context.Context, r setupRecord) (core.ConversationOrigin, error) {
		if r.PairingCode == "" {
			return core.ConversationOrigin{}, fmt.Errorf("missing code")
		}
		return core.ConversationOrigin{AppID: r.AppID, Tenant: "tenant", Actor: r.OwnerOpenID, ChatID: "chat", ChatType: "p2p", MessageID: "pair-message"}, nil
	}
	if _, err := s.Action("pair"); err != nil {
		t.Fatal(err)
	}
	state := awaitSetup(t, s, "paired")
	awaitSetupIdle(t, s)
	if state.TenantKey != "tenant" || state.ChatID != "chat" || state.PairingCode != "" || state.PairingExpiresAt != 0 {
		t.Fatal("incorrect pairing state")
	}
	public, _ := json.Marshal(state)
	if strings.Contains(string(public), "private-pair-secret") || strings.Contains(string(public), "pair-message") {
		t.Fatal("private setup record exposed")
	}
	if _, err := s.Action("pair"); err != nil {
		t.Fatal("repeated pairing should reuse chat", err)
	}
	_ = s.Close()
	restored, err := NewSetup(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.record.PairOrigin == nil || restored.record.PairOrigin.MessageID != "pair-message" || restored.record.AppSecret != "private-pair-secret" || restored.State().(SetupState).Status != "paired" {
		t.Fatal("paired origin lost on restart")
	}
	restored.client.Transport = setupTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "internal") {
			return setupReply(`{"code":0,"tenant_access_token":"private-token"}`), nil
		}
		return setupReply(`{"code":0,"bot":{"open_id":"bot"}}`), nil
	})
	if _, err := restored.Action("retry"); err != nil {
		t.Fatal(err)
	}
	awaitSetup(t, restored, "paired")
}

func TestDesktopFeishuPairCancelStopsListenerAndRetainsApp(t *testing.T) {
	s := newPairSetup(t)
	entered := make(chan struct{})
	exited := make(chan struct{})
	s.connection = func(ctx context.Context, r setupRecord) (core.ConversationOrigin, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return core.ConversationOrigin{}, ctx.Err()
	}
	state, err := s.Action("pair")
	if err != nil || state.(SetupState).PairingCode == "" {
		t.Fatal("pairing not started")
	}
	<-entered
	code := s.State().(SetupState).PairingCode
	second, err := s.Action("pair")
	if err != nil || second.(SetupState).PairingCode != code {
		t.Fatal("duplicate pairing changed code")
	}
	if _, err = s.Action("cancel"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("listener left running")
	}
	state = s.State()
	if state.(SetupState).Status != "created" || state.(SetupState).PairingCode != "" || state.(SetupState).PairingExpiresAt != 0 || s.record.AppSecret != "private-pair-secret" {
		t.Fatal("cancel lost application or retained code")
	}
}

func TestDesktopFeishuPairRejectsInvalidChildOutputAndExpiry(t *testing.T) {
	for _, name := range []string{"other creator", "empty actor", "group", "other tenant", "expired"} {
		t.Run(name, func(t *testing.T) {
			s := newPairSetup(t)
			_ = s.update(func(r *setupRecord) { r.TenantKey = "tenant" })
			s.connection = func(ctx context.Context, r setupRecord) (core.ConversationOrigin, error) {
				o := core.ConversationOrigin{AppID: r.AppID, Tenant: "tenant", Actor: r.OwnerOpenID, ChatID: "chat", ChatType: "p2p", MessageID: "message"}
				switch name {
				case "other creator":
					o.Actor = "other"
				case "empty actor":
					o.Actor = ""
				case "group":
					o.ChatType = "group"
				case "other tenant":
					o.Tenant = "other"
				case "expired":
					return o, fmt.Errorf("pairing-expired")
				}
				return o, nil
			}
			_, _ = s.Action("pair")
			state := awaitSetup(t, s, "error")
			if state.ChatID != "" || state.PairingCode != "" || s.record.AppSecret != "private-pair-secret" {
				t.Fatal("invalid pairing persisted")
			}
		})
	}
}

func TestDesktopFeishuPairRestartAndInvalidRequests(t *testing.T) {
	s := newPairSetup(t)
	_ = s.update(func(r *setupRecord) {
		r.Status = "pairing"
		r.PairingCode = "old-code"
		r.PairingExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	})
	_ = s.Close()
	restored, err := NewSetup(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	state := restored.State().(SetupState)
	if state.Status != "error" || state.Error != "interrupted" || state.PairingCode != "" || restored.record.AppSecret != "private-pair-secret" {
		t.Fatal("interrupted pairing not recovered safely")
	}
	for _, input := range []string{`{}`, `not-json`, `{"app_id":"app","app_secret":"s","pairing_code":"c","pairing_expires_at":1}`} {
		var output bytes.Buffer
		if RunPair(context.Background(), strings.NewReader(input), &output) == nil || output.Len() != 0 {
			t.Fatal("invalid child request accepted")
		}
	}
	_ = restored.update(func(r *setupRecord) { r.OwnerOpenID = "" })
	if _, err = restored.Action("pair"); err == nil {
		t.Fatal("pairing without creator accepted")
	}
}
