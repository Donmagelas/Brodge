package feishu

// Registration follows Bro's feishu-setup.ts and the official PersonalAgent flow.
// Credentials remain in a private local record; forwarding is never started here.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const registrationEndpoint = "https://accounts.feishu.cn/oauth/v1/app/registration"
const setupAPI = "https://open.feishu.cn/open-apis"

// SetupState is safe to return to the browser. It never includes credentials.
type SetupState struct {
	Status           string `json:"status"`
	QRURL            string `json:"qr_url,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	AppID            string `json:"app_id,omitempty"`
	OwnerOpenID      string `json:"owner_open_id,omitempty"`
	BotOpenID        string `json:"bot_open_id,omitempty"`
	TenantKey        string `json:"tenant_key,omitempty"`
	ChatID           string `json:"chat_id,omitempty"`
	PairingCode      string `json:"pairing_code,omitempty"`
	PairingExpiresAt int64  `json:"pairing_expires_at,omitempty"`
	Error            string `json:"error,omitempty"`
	CanRetry         bool   `json:"can_retry"`
}
type setupRecord struct {
	SetupState
	DeviceCode string                   `json:"device_code,omitempty"`
	AppSecret  string                   `json:"app_secret,omitempty"`
	Interval   int                      `json:"interval,omitempty"`
	PairOrigin *core.ConversationOrigin `json:"pair_origin,omitempty"`
}
type Setup struct {
	actions    sync.Mutex
	mu         sync.Mutex
	path       string
	record     setupRecord
	client     *http.Client
	cancel     context.CancelFunc
	done       chan struct{}
	closed     bool
	wait       func(context.Context, time.Duration) error
	connection func(context.Context, setupRecord) (core.ConversationOrigin, error)
}

var _ core.LocalPlatformSetup = (*Setup)(nil)

func NewSetup(dir string) (*Setup, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe-setup-directory")
	}
	s := &Setup{path: filepath.Join(dir, "feishu-setup.json"), record: setupRecord{SetupState: SetupState{Status: "idle"}}, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, wait: setupWait}
	info, err = os.Lstat(s.path)
	if err == nil {
		stat, ok = info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("unsafe-setup-file")
		}
		body, e := os.ReadFile(s.path)
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(body, &s.record); e != nil {
			return nil, fmt.Errorf("invalid-setup-record")
		}
		if s.record.Status == "starting" || s.record.Status == "waiting" || s.record.Status == "checking" || s.record.Status == "pairing" {
			s.record.Status, s.record.Error = "error", "interrupted"
			s.record.PairingCode, s.record.PairingExpiresAt = "", 0
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// Pairing runs the official SDK in a short-lived child because its Start method
// blocks forever after connecting and does not expose connection shutdown.
func (s *Setup) receivePair(ctx context.Context, r setupRecord) (core.ConversationOrigin, error) {
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(r.PairingExpiresAt))
	defer cancel()
	if s.connection != nil {
		return s.connection(ctx, r)
	}
	executable, err := os.Executable()
	if err != nil {
		return core.ConversationOrigin{}, fmt.Errorf("pairing-failed")
	}
	cmd := exec.CommandContext(ctx, executable, "desktop-feishu-pair")
	body, _ := json.Marshal(r)
	cmd.Stdin = bytes.NewReader(body)
	var output bytes.Buffer
	cmd.Stdout = &output
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return core.ConversationOrigin{}, fmt.Errorf("pairing-expired")
		}
		return core.ConversationOrigin{}, fmt.Errorf("pairing-failed")
	}
	var origin core.ConversationOrigin
	if json.Unmarshal(output.Bytes(), &origin) != nil {
		return origin, fmt.Errorf("pairing-failed")
	}
	return origin, nil
}
func setupWait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (s *Setup) State() any { s.mu.Lock(); defer s.mu.Unlock(); return s.stateLocked() }
func (s *Setup) PairedOrigin() (core.ConversationOrigin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.PairOrigin == nil || s.record.ChatID == "" {
		return core.ConversationOrigin{}, false
	}
	return *s.record.PairOrigin, true
}

func (s *Setup) stateLocked() SetupState {
	state := s.record.SetupState
	state.CanRetry = s.record.AppSecret != "" || s.record.DeviceCode != ""
	if s.record.AppSecret == "" && s.record.DeviceCode != "" && time.Now().UnixMilli() >= state.ExpiresAt {
		state.Status, state.QRURL, state.CanRetry = "expired", "", false
	}
	return state
}
func (s *Setup) saveLocked(record setupRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".feishu-setup-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	s.record = record
	return nil
}
func (s *Setup) update(fn func(*setupRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.record
	fn(&r)
	return s.saveLocked(r)
}
func (s *Setup) Action(action string) (any, error) {
	s.actions.Lock()
	defer s.actions.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("setup-closed")
	}
	if action == "cancel" {
		cancel, done := s.cancel, s.done
		s.mu.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
		err := s.update(func(r *setupRecord) {
			if r.AppID != "" {
				if r.Status == "pairing" {
					r.Status, r.Error = "created", ""
				} else if r.Status != "created" && r.Status != "paired" {
					r.Status, r.Error = "error", "interrupted"
				}
				r.PairingCode, r.PairingExpiresAt = "", 0
			} else {
				*r = setupRecord{SetupState: SetupState{Status: "cancelled"}}
			}
		})
		return s.State(), err
	}
	if action != "start" && action != "retry" && action != "pair" {
		s.mu.Unlock()
		return nil, fmt.Errorf("invalid-action")
	}
	if s.done != nil {
		state := s.stateLocked()
		s.mu.Unlock()
		return state, nil
	}
	r := s.record
	if action == "pair" {
		if r.AppID == "" || r.AppSecret == "" || r.BotOpenID == "" || r.OwnerOpenID == "" {
			s.mu.Unlock()
			return nil, fmt.Errorf("application-not-ready")
		}
		if r.ChatID != "" {
			state := s.stateLocked()
			s.mu.Unlock()
			return state, nil
		}
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("pairing-failed")
		}
		r.Status, r.Error = "pairing", ""
		r.PairingCode = "BRIDGE_" + hex.EncodeToString(random)
		r.PairingExpiresAt = time.Now().Add(10 * time.Minute).UnixMilli()
	} else if action == "start" {
		if r.AppID != "" {
			s.mu.Unlock()
			return nil, fmt.Errorf("application-already-created")
		}
		if r.DeviceCode != "" && time.Now().UnixMilli() < r.ExpiresAt {
			state := s.stateLocked()
			s.mu.Unlock()
			return state, nil
		}
		r = setupRecord{SetupState: SetupState{Status: "starting"}}
	} else {
		if r.AppSecret == "" && r.DeviceCode == "" {
			s.mu.Unlock()
			return nil, fmt.Errorf("nothing-to-retry")
		}
		if r.AppSecret != "" {
			r.Status = "checking"
		} else {
			r.Status = "waiting"
		}
		r.Error = ""
	}
	if err := s.saveLocked(r); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("save-failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.cancel, s.done = cancel, done
	state := s.stateLocked()
	s.mu.Unlock()
	go func() {
		err := s.run(ctx)
		if err != nil && ctx.Err() == nil {
			if saveErr := s.update(func(r *setupRecord) {
				r.Status, r.Error = "error", err.Error()
				r.PairingCode, r.PairingExpiresAt = "", 0
			}); saveErr != nil {
				s.mu.Lock()
				s.record.Status, s.record.Error = "error", "save-failed"
				s.mu.Unlock()
				slog.Error("Feishu setup state persistence failed")
			}
		}
		s.mu.Lock()
		s.cancel, s.done = nil, nil
		close(done)
		s.mu.Unlock()
		cancel()
	}()
	return state, nil
}
func (s *Setup) Close() error {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}
func (s *Setup) read(ctx context.Context, endpoint, contentType string, body []byte, token string) (map[string]any, error) {
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid-response")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("invalid-response")
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil || result == nil {
		return nil, fmt.Errorf("invalid-response")
	}
	if response.StatusCode != http.StatusOK && result["error"] == nil {
		return nil, fmt.Errorf("network")
	}
	return result, nil
}
func (s *Setup) register(ctx context.Context, form url.Values) (map[string]any, error) {
	return s.read(ctx, registrationEndpoint, "application/x-www-form-urlencoded", []byte(form.Encode()), "")
}
func setupString(v any) string { value, _ := v.(string); return value }
func setupBounded(v any, fallback, max int) int {
	value, ok := v.(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 1 {
		return fallback
	}
	if value > float64(max) {
		return max
	}
	return int(value)
}
func setupMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func (s *Setup) run(ctx context.Context) error {
	s.mu.Lock()
	r := s.record
	s.mu.Unlock()
	if r.Status == "pairing" {
		origin, err := s.receivePair(ctx, r)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if origin.AppID != r.AppID || origin.Tenant == "" || (r.TenantKey != "" && origin.Tenant != r.TenantKey) || origin.ChatID == "" || origin.MessageID == "" || origin.ChatType != "p2p" || origin.Actor == "" || origin.Actor != r.OwnerOpenID || time.Now().UnixMilli() >= r.PairingExpiresAt {
			return fmt.Errorf("pairing-failed")
		}
		return s.update(func(r *setupRecord) {
			r.Status = "paired"
			r.Error = ""
			r.TenantKey = origin.Tenant
			r.ChatID = origin.ChatID
			r.OwnerOpenID = origin.Actor
			r.PairOrigin = &origin
			r.PairingCode, r.PairingExpiresAt = "", 0
		})
	}
	if r.AppSecret != "" {
		return s.check(ctx, r)
	}
	if r.DeviceCode == "" {
		data, err := s.register(ctx, url.Values{"action": {"begin"}, "archetype": {"PersonalAgent"}, "auth_method": {"client_secret"}, "request_user_info": {"open_id tenant_brand"}})
		if err != nil {
			return err
		}
		code, user := setupString(data["device_code"]), setupString(data["user_code"])
		if setupString(data["error"]) != "" || code == "" || user == "" {
			return fmt.Errorf("registration-failed")
		}
		expiry := data["expire_in"]
		if expiry == nil {
			expiry = data["expires_in"]
		}
		r = setupRecord{SetupState: SetupState{Status: "waiting", QRURL: "https://open.feishu.cn/page/cli?user_code=" + url.QueryEscape(user), ExpiresAt: time.Now().Add(time.Duration(setupBounded(expiry, 600, 3600)) * time.Second).UnixMilli()}, DeviceCode: code, Interval: setupBounded(data["interval"], 5, 60)}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.update(func(current *setupRecord) { *current = r }); err != nil {
			return fmt.Errorf("save-failed")
		}
	}
	interval := time.Duration(r.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.UnixMilli(r.ExpiresAt)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for time.Now().Before(deadline) {
		data, err := s.register(pollCtx, url.Values{"action": {"poll"}, "device_code": {r.DeviceCode}})
		if err == nil {
			user := setupMap(data["user_info"])
			brand := setupString(user["tenant_brand"])
			if brand != "" && brand != "feishu" {
				return fmt.Errorf("unsupported-brand")
			}
			app, secret := setupString(data["client_id"]), setupString(data["client_secret"])
			if setupString(data["error"]) == "" && app != "" && secret != "" {
				owner := setupString(user["open_id"])
				if !regexp.MustCompile(`^ou_[A-Za-z0-9_-]+$`).MatchString(owner) {
					owner = ""
				}
				r = setupRecord{SetupState: SetupState{Status: "checking", AppID: app, OwnerOpenID: owner, TenantKey: setupString(user["tenant_key"])}, AppSecret: secret}
				// Preserve returned credentials even if cancellation races with the response.
				if err = s.update(func(current *setupRecord) { *current = r }); err != nil {
					s.mu.Lock()
					s.record = r
					s.mu.Unlock()
					return fmt.Errorf("save-failed")
				}
				return s.check(ctx, r)
			}
			switch setupString(data["error"]) {
			case "access_denied":
				return s.update(func(r *setupRecord) { *r = setupRecord{SetupState: SetupState{Status: "denied"}} })
			case "expired_token", "invalid_grant":
				return s.expire()
			case "slow_down":
				interval += 5 * time.Second
				if interval > 60*time.Second {
					interval = 60 * time.Second
				}
			case "", "authorization_pending":
			default:
				return fmt.Errorf("registration-failed")
			}
		}
		wait := interval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		if err = s.wait(ctx, wait); err != nil {
			return err
		}
	}
	return s.expire()
}
func (s *Setup) expire() error {
	return s.update(func(r *setupRecord) { r.Status = "expired"; r.DeviceCode = ""; r.QRURL = "" })
}
func (s *Setup) check(ctx context.Context, r setupRecord) error {
	body, _ := json.Marshal(map[string]string{"app_id": r.AppID, "app_secret": r.AppSecret})
	auth, err := s.read(ctx, setupAPI+"/auth/v3/tenant_access_token/internal", "application/json", body, "")
	if err != nil {
		return err
	}
	token := setupString(auth["tenant_access_token"])
	if auth["code"] != float64(0) || token == "" {
		return fmt.Errorf("credentials-not-ready")
	}
	info, err := s.read(ctx, setupAPI+"/bot/v3/info", "", nil, token)
	if err != nil {
		return err
	}
	bot := setupMap(info["bot"])
	if bot == nil {
		bot = setupMap(setupMap(info["data"])["bot"])
	}
	id := setupString(bot["open_id"])
	if info["code"] != float64(0) || id == "" {
		return fmt.Errorf("bot-not-ready")
	}
	return s.update(func(r *setupRecord) {
		r.Status = "created"
		if r.PairOrigin != nil && r.ChatID != "" {
			r.Status = "paired"
		}
		r.Error, r.BotOpenID = "", id
	})
}

// PairedPlatform reuses only the application and creator confirmed by pairing.
func (s *Setup) PairedPlatform() (core.Platform, error) {
	s.mu.Lock()
	r := s.record
	s.mu.Unlock()
	if r.PairOrigin == nil || r.ChatID == "" || r.OwnerOpenID == "" || r.AppSecret == "" {
		return nil, fmt.Errorf("private pairing required")
	}
	return New(map[string]any{"app_id": r.AppID, "app_secret": r.AppSecret, "tenant_key": r.PairOrigin.Tenant, "allow_from": r.OwnerOpenID, "allow_chat": r.ChatID})
}
