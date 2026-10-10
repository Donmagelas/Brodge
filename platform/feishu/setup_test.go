package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type setupTransport func(*http.Request) (*http.Response, error)

func (f setupTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func setupReply(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func awaitSetup(t *testing.T, s *Setup, status string) SetupState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := s.State().(SetupState)
		if state.Status == status {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %s, got %+v", status, s.State())
	return SetupState{}
}
func newTestSetup(t *testing.T) *Setup {
	t.Helper()
	s, err := NewSetup(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.wait = func(ctx context.Context, d time.Duration) error { return setupWait(ctx, time.Millisecond) }
	return s
}
func TestDesktopFeishuSetupRegistrationAndPrivateRecovery(t *testing.T) {
	s := newTestSetup(t)
	var begins atomic.Int32
	s.client.Transport = setupTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "accounts.feishu.cn" && r.URL.Host != "open.feishu.cn" {
			t.Errorf("unexpected host %s", r.URL.Host)
		}
		body := []byte{}
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		if strings.HasSuffix(r.URL.Path, "registration") {
			values, _ := url.ParseQuery(string(body))
			if values.Get("action") == "begin" {
				begins.Add(1)
				if values.Get("archetype") != "PersonalAgent" || values.Get("request_user_info") != "open_id tenant_brand" {
					t.Error(values)
				}
				return setupReply(`{"device_code":"private-device","user_code":"hello & code","expire_in":60,"interval":1}`), nil
			}
			if values.Get("device_code") != "private-device" {
				t.Error("device mismatch")
			}
			return setupReply(`{"client_id":"cli_new","client_secret":"private-secret","user_info":{"open_id":"ou_creator","tenant_brand":"feishu"}}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "internal") {
			if !strings.Contains(string(body), "private-secret") {
				t.Error("missing credential")
			}
			return setupReply(`{"code":0,"tenant_access_token":"private-access"}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer private-access" {
			t.Error("missing auth")
		}
		return setupReply(`{"code":0,"bot":{"open_id":"ou_bot"}}`), nil
	})
	if _, err := s.Action("start"); err != nil {
		t.Fatal(err)
	}
	state := awaitSetup(t, s, "created")
	if state.AppID != "cli_new" || state.OwnerOpenID != "ou_creator" || state.BotOpenID != "ou_bot" {
		t.Fatal(state)
	}
	public, _ := json.Marshal(s.State())
	for _, secret := range []string{"private-secret", "private-device", "private-access", "app_secret", "device_code"} {
		if strings.Contains(string(public), secret) {
			t.Fatal("credential leaked", secret)
		}
	}
	file, err := os.ReadFile(s.path)
	if err != nil || !strings.Contains(string(file), "private-secret") {
		t.Fatal("credentials not persisted", err)
	}
	info, _ := os.Stat(s.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if _, err = s.Action("start"); err == nil {
		t.Fatal("created duplicate application")
	}
	if begins.Load() != 1 {
		t.Fatal(begins.Load())
	}
	_ = s.Close()
	restored, err := NewSetup(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.State().(SetupState).AppID != "cli_new" {
		t.Fatal("lost application")
	}
}
func TestDesktopFeishuSetupResumeKeepsAppAndDoesNotRegisterAgain(t *testing.T) {
	s := newTestSetup(t)
	var begins atomic.Int32
	var botReady atomic.Bool
	s.client.Transport = setupTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "registration") {
			body := []byte{}
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
			}
			form, _ := url.ParseQuery(string(body))
			if form.Get("action") == "begin" {
				begins.Add(1)
				return setupReply(`{"device_code":"device","user_code":"code","expire_in":60}`), nil
			}
			return setupReply(`{"client_id":"cli_new","client_secret":"secret"}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "internal") {
			return setupReply(`{"code":0,"tenant_access_token":"token"}`), nil
		}
		if !botReady.Load() {
			return setupReply(`{"code":1,"msg":"token=secret"}`), nil
		}
		return setupReply(`{"code":0,"data":{"bot":{"open_id":"ou_bot"}}}`), nil
	})
	_, _ = s.Action("start")
	state := awaitSetup(t, s, "error")
	if state.Error != "bot-not-ready" || state.AppID != "cli_new" || !state.CanRetry {
		t.Fatal(state)
	}
	for {
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		if done == nil {
			break
		}
		<-done
	}
	botReady.Store(true)
	_, err := s.Action("retry")
	if err != nil {
		t.Fatal(err)
	}
	awaitSetup(t, s, "created")
	if begins.Load() != 1 {
		t.Fatal("retry registered new application")
	}
}
func TestDesktopFeishuSetupPendingSlowDownCancelAndExpiry(t *testing.T) {
	s := newTestSetup(t)
	entered := make(chan time.Duration, 1)
	s.wait = func(ctx context.Context, d time.Duration) error { entered <- d; <-ctx.Done(); return ctx.Err() }
	s.client.Transport = setupTransport(func(r *http.Request) (*http.Response, error) {
		body := []byte{}
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		form, _ := url.ParseQuery(string(body))
		if form.Get("action") == "begin" {
			return setupReply(`{"device_code":"device","user_code":"a&b","expire_in":60,"interval":1}`), nil
		}
		return setupReply(`{"error":"slow_down"}`), nil
	})
	_, _ = s.Action("start")
	select {
	case d := <-entered:
		if d != 6*time.Second {
			t.Fatal(d)
		}
	case <-time.After(time.Second):
		t.Fatal("no poll")
	}
	state := s.State().(SetupState)
	if state.QRURL != "https://open.feishu.cn/page/cli?user_code=a%26b" {
		t.Fatal(state)
	}
	_, _ = s.Action("start")
	_, err := s.Action("cancel")
	if err != nil {
		t.Fatal(err)
	}
	if s.State().(SetupState).Status != "cancelled" {
		t.Fatal(s.State())
	}
	if err = s.update(func(r *setupRecord) {
		r.Status = "waiting"
		r.DeviceCode = "expired-device"
		r.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Action("retry")
	if err != nil {
		t.Fatal(err)
	}
	awaitSetup(t, s, "expired")
}
func TestDesktopFeishuSetupRejectsRedirectsAndRedactsPlatformErrors(t *testing.T) {
	for _, body := range []string{`{"error":"unknown","error_description":"private-secret"}`, `not-json-private-secret`, `{"device_code":"d"}`} {
		s := newTestSetup(t)
		s.client.Transport = setupTransport(func(*http.Request) (*http.Response, error) { return setupReply(body), nil })
		_, _ = s.Action("start")
		state := awaitSetup(t, s, "error")
		if strings.Contains(state.Error, "secret") {
			t.Fatal(state)
		}
	}
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	s := newTestSetup(t)
	_, err := s.read(context.Background(), redirect.URL, "application/json", []byte(`{"secret":"local"}`), "")
	if err == nil || reached.Load() {
		t.Fatal("followed redirect", err)
	}
}
func TestDesktopFeishuSetupUnknownBrandDeniedAndInterruptedRecord(t *testing.T) {
	for _, tc := range []struct{ body, status string }{{`{"error":"access_denied"}`, "denied"}, {`{"error":"expired_token"}`, "expired"}, {`{"client_id":"a","client_secret":"s","user_info":{"tenant_brand":"lark"}}`, "error"}} {
		s := newTestSetup(t)
		s.client.Transport = setupTransport(func(r *http.Request) (*http.Response, error) {
			body := []byte{}
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
			}
			if strings.Contains(string(body), "action=begin") {
				return setupReply(`{"device_code":"d","user_code":"u","expire_in":60}`), nil
			}
			return setupReply(tc.body), nil
		})
		_, _ = s.Action("start")
		awaitSetup(t, s, tc.status)
	}
	s := newTestSetup(t)
	err := s.update(func(r *setupRecord) { r.Status = "checking"; r.AppID = "cli_existing"; r.AppSecret = "existing-secret" })
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	restored, err := NewSetup(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	state := restored.State().(SetupState)
	if state.Status != "error" || !state.CanRetry || state.AppID != "cli_existing" {
		t.Fatal(state)
	}
}
func TestDesktopFeishuSetupRejectsUnsafeStorage(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetup(link); err == nil {
		t.Fatal("symlink directory")
	}
	dir := filepath.Join(root, "private")
	_ = os.Mkdir(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "feishu-setup.json"), []byte(`{"status":"created"}`), 0644)
	if _, err := NewSetup(dir); err == nil {
		t.Fatal("public credentials")
	}
	if setupBounded(float64(1000), 5, 60) != 60 || setupBounded(float64(-1), 5, 60) != 5 {
		t.Fatal("bad interval")
	}
}
