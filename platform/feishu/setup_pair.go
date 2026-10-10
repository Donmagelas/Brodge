package feishu

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// This child performs only pairing; it has no Desktop or model capabilities.
func RunPair(ctx context.Context, input io.Reader, output io.Writer) error {
	var record setupRecord
	if json.NewDecoder(io.LimitReader(input, 16384)).Decode(&record) != nil || record.AppID == "" || record.AppSecret == "" || record.OwnerOpenID == "" || record.PairingCode == "" || record.PairingExpiresAt <= time.Now().UnixMilli() {
		return fmt.Errorf("invalid-pairing-request")
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(record.PairingExpiresAt))
	defer cancel()
	paired := make(chan core.ConversationOrigin, 1)
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		if origin, ok := pairingOrigin(record, event); ok {
			select {
			case paired <- origin:
			default:
			}
		}
		return nil
	})
	handler.Config.Logger = pairLogger{}
	client := larkws.NewClient(record.AppID, record.AppSecret, larkws.WithEventHandler(handler), larkws.WithLogger(pairLogger{}), larkws.WithLogLevel(larkcore.LogLevelError))
	failed := make(chan error, 1)
	go func() { failed <- client.Start(ctx) }()
	select {
	case origin := <-paired:
		return json.NewEncoder(output).Encode(origin)
	case <-failed:
		return fmt.Errorf("pairing-connect-failed")
	case <-ctx.Done():
		return fmt.Errorf("pairing-expired")
	}
}
func pairingOrigin(r setupRecord, event *larkim.P2MessageReceiveV1) (core.ConversationOrigin, bool) {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil || event.Event == nil || event.Event.Sender == nil || event.Event.Message == nil {
		return core.ConversationOrigin{}, false
	}
	s, m := event.Event.Sender, event.Event.Message
	tenant := event.EventV2Base.Header.TenantKey
	if event.EventV2Base.Header.AppID != r.AppID || tenant == "" || (r.TenantKey != "" && r.TenantKey != tenant) || stringValue(s.TenantKey) != tenant || stringValue(s.SenderType) != "user" || s.SenderId == nil || stringValue(m.ChatType) != "p2p" || stringValue(m.MessageType) != "text" || stringValue(m.MessageId) == "" || stringValue(m.ChatId) == "" || time.Now().UnixMilli() >= r.PairingExpiresAt {
		return core.ConversationOrigin{}, false
	}
	actor := stringValue(s.SenderId.OpenId)
	if actor == "" || actor != r.OwnerOpenID {
		return core.ConversationOrigin{}, false
	}
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(stringValue(m.Content)), &body) != nil || r.PairingCode == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Text)), []byte(r.PairingCode)) != 1 {
		return core.ConversationOrigin{}, false
	}
	return core.ConversationOrigin{AppID: r.AppID, Tenant: tenant, Actor: actor, ChatID: stringValue(m.ChatId), ChatType: "p2p", MessageID: stringValue(m.MessageId), ParentID: stringValue(m.ParentId), RootID: stringValue(m.RootId), ThreadID: stringValue(m.ThreadId)}, true
}

// SDK logs can include connection credentials. Only a fixed outcome leaves this child.
type pairLogger struct{}

func (pairLogger) Debug(context.Context, ...interface{}) {}
func (pairLogger) Info(context.Context, ...interface{})  {}
func (pairLogger) Warn(context.Context, ...interface{}) {
	slog.Warn("Feishu connection or event handling warning")
}
func (pairLogger) Error(context.Context, ...interface{}) {
	slog.Error("Feishu connection or event handling failed")
}
