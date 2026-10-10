package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func init() { core.RegisterPlatform("feishu", New) }

type replyContext struct{ messageID, chatID, sessionKey string }
type Platform struct {
	mu                                                                             sync.RWMutex
	platformName, appID, appSecret, desktopTenant, allowFrom, allowChat, botOpenID string
	desktopBound                                                                   bool
	client                                                                         *lark.Client
	conversationHandler                                                            func(context.Context, core.Platform, *core.Message) error
	cancel                                                                         context.CancelFunc
	connected                                                                      bool
	failure                                                                        string
}

func New(opts map[string]any) (core.Platform, error) {
	p := &Platform{platformName: "feishu", desktopBound: true}
	p.appID, _ = opts["app_id"].(string)
	p.appSecret, _ = opts["app_secret"].(string)
	p.desktopTenant, _ = opts["tenant_key"].(string)
	p.allowFrom, _ = opts["allow_from"].(string)
	p.allowChat, _ = opts["allow_chat"].(string)
	if p.appID == "" || p.appSecret == "" || p.desktopTenant == "" {
		return nil, fmt.Errorf("feishu: app credentials and tenant required")
	}
	if len(strings.Split(p.allowFrom, ",")) != 1 || !explicitDesktopScope(p.allowFrom) || !explicitDesktopScope(p.allowChat) {
		return nil, fmt.Errorf("feishu: explicit user and chat scope required")
	}
	p.client = lark.NewClient(p.appID, p.appSecret, lark.WithLogger(pairLogger{}), lark.WithLogLevel(larkcore.LogLevelError), lark.WithHttpClient(&http.Client{Timeout: 15 * time.Second}))
	return p, nil
}
func (p *Platform) Name() string { return p.platformName }
func (p *Platform) Start(core.MessageHandler) error {
	if p.conversationHandler == nil {
		return fmt.Errorf("feishu: durable ingress required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()
	// Bot identity is needed only for group @ filtering. Failure keeps groups closed.
	infoCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	id, err := p.fetchBotOpenID(infoCtx)
	stop()
	p.mu.Lock()
	p.botOpenID = id
	p.mu.Unlock()
	if err != nil {
		slog.Warn("Feishu bot identity unavailable; group messages require recovery")
		go p.recoverBotIdentity(ctx)
	}
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(p.onDesktopMessage)
	handler.Config.Logger = pairLogger{}
	client := larkws.NewClient(p.appID, p.appSecret, larkws.WithEventHandler(handler), larkws.WithLogger(connectionLogger{p}), larkws.WithLogLevel(larkcore.LogLevelInfo))
	go func() {
		if err := client.Start(ctx); err != nil {
			p.mu.Lock()
			p.connected = false
			p.failure = "connection-failed"
			p.mu.Unlock()
			slog.Error("Feishu connection failed")
		}
	}()
	// The official SDK remains in Start after connecting. Process exit owns final shutdown.
	return nil
}
func (p *Platform) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	p.connected = false
	return nil
}
func (p *Platform) getBotOpenID() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.botOpenID }
func (p *Platform) fetchBotOpenID(ctx context.Context) (string, error) {
	resp, err := p.client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("bot-info-unavailable")
	}
	var result struct {
		Code int `json:"code"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if json.Unmarshal(resp.RawBody, &result) != nil || result.Code != 0 || result.Bot.OpenID == "" {
		return "", fmt.Errorf("bot-info-unavailable")
	}
	return result.Bot.OpenID, nil
}
func (p *Platform) recoverBotIdentity(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			id, err := p.fetchBotOpenID(call)
			cancel()
			if err == nil {
				p.mu.Lock()
				p.botOpenID = id
				p.mu.Unlock()
				return
			}
		}
	}
}
func (p *Platform) PlatformHealth() core.PlatformHealthInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	reason := p.failure
	if reason == "" && p.botOpenID == "" {
		reason = "group-bot-identity-unavailable"
	}
	return core.PlatformHealthInfo{Name: p.Name(), Connected: p.connected, Degraded: reason != "", DegradedReason: reason}
}

// The SDK connection signal updates health; its raw log arguments never leave this adapter.
type connectionLogger struct{ p *Platform }

func (l connectionLogger) Debug(context.Context, ...interface{}) {}
func (l connectionLogger) Info(_ context.Context, args ...interface{}) {
	for _, arg := range args {
		msg, ok := arg.(string)
		if !ok {
			continue
		}
		if strings.HasPrefix(msg, "connected to ") || strings.HasPrefix(msg, "disconnected to ") {
			l.p.mu.Lock()
			l.p.connected = strings.HasPrefix(msg, "connected to ")
			l.p.failure = ""
			l.p.mu.Unlock()
		}
	}
}
func (l connectionLogger) Warn(ctx context.Context, args ...interface{})  { pairLogger{}.Warn(ctx) }
func (l connectionLogger) Error(ctx context.Context, args ...interface{}) { pairLogger{}.Error(ctx) }
func isBotMentioned(mentions []*larkim.MentionEvent, id string) bool {
	for _, m := range mentions {
		if m != nil && m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == id {
			return true
		}
	}
	return false
}
func stripMentions(text string, mentions []*larkim.MentionEvent, id string) string {
	for _, m := range mentions {
		if m == nil || m.Key == nil {
			continue
		}
		value := ""
		if !(m.Id != nil && stringValue(m.Id.OpenId) == id) && m.Name != nil {
			value = "@" + *m.Name
		}
		text = strings.ReplaceAll(text, *m.Key, value)
	}
	return strings.TrimSpace(text)
}
func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
