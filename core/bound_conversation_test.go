package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

type boundTestAgent struct {
	received, bound    int
	recoveryError      error
	creationRecoveries int
}

func (a *boundTestAgent) Name() string                                             { return "test" }
func (a *boundTestAgent) Stop() error                                              { return nil }
func (a *boundTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *boundTestAgent) HandleConversation(context.Context, Platform, *Message) error {
	a.received++
	return nil
}
func (a *boundTestAgent) ResumeConversations(context.Context, []Platform) error {
	return a.recoveryError
}
func (a *boundTestAgent) ConversationState() any { return map[string]string{"mode": "binding"} }
func (a *boundTestAgent) BindConversation(context.Context, string, string) error {
	a.bound++
	return nil
}
func (a *boundTestAgent) RetryConversationCreation(_ context.Context, id, thread string) error {
	if id != "job" {
		return errors.New("unknown-job")
	}
	a.creationRecoveries++
	return nil
}

type localSetupStub struct {
	actions int
	closed  bool
}

func (s *localSetupStub) State() any { return map[string]string{"status": "paired"} }
func (s *localSetupStub) Action(action string) (any, error) {
	if action != "start" {
		return nil, errors.New("invalid-action")
	}
	s.actions++
	return s.State(), nil
}
func (s *localSetupStub) Close() error { s.closed = true; return nil }

func TestRemovedModulesHaveNoManagementOrWebRoutes(t *testing.T) {
	previous := webAssetsFS
	defer func() { webAssetsFS = previous }()
	RegisterWebAssets(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("bridge")}})
	a := &boundTestAgent{}
	m := NewManagementServer(0, "admin")
	m.RegisterEngine("desktop", NewEngine("desktop", a, nil))
	setup := &localSetupStub{}
	m.SetLocalFeishuSetup("desktop", setup)
	mux := http.NewServeMux()
	m.buildHandler(mux)
	for _, path := range []string{
		"/api/v1/providers", "/api/v1/provider-presets", "/api/v1/skills", "/api/v1/skill-presets", "/api/v1/cron", "/api/v1/settings", "/api/v1/restart", "/api/v1/reload", "/api/v1/setup/feishu/save",
		"/api/v1/projects/desktop/providers", "/api/v1/projects/desktop/skills", "/api/v1/projects/desktop/cron", "/api/v1/projects/desktop/heartbeat", "/api/v1/projects/desktop/sessions", "/api/v1/projects/desktop/workspaces", "/api/v1/projects/desktop/add-platform", "/bridge",
		"/providers", "/skills", "/cron", "/chat", "/system", "/projects",
	} {
		for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
			r := httptest.NewRequest(method, path, strings.NewReader(`{"mode":"yolo"}`))
			r.Header.Set("Authorization", "Bearer admin")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatalf("removed route %s %s: %d", method, path, w.Code)
			}
		}
	}
	if a.bound != 0 || setup.actions != 0 {
		t.Fatal("removed routes performed mutations")
	}
}
func TestRetainedConversationAndPairingRoutesRequireToken(t *testing.T) {
	a := &boundTestAgent{}
	m := NewManagementServer(0, "admin")
	m.RegisterEngine("desktop", NewEngine("desktop", a, nil))
	setup := &localSetupStub{}
	m.SetLocalFeishuSetup("desktop", setup)
	mux := http.NewServeMux()
	m.buildHandler(mux)
	for _, tc := range []struct {
		method, path, body string
		auth               bool
		status             int
	}{
		{"GET", "/api/v1/status", "", false, 401},
		{"GET", "/api/v1/projects/desktop/conversation", "", false, 401},
		{"GET", "/api/v1/projects/desktop/conversation/feishu", "", false, 401},
		{"GET", "/api/v1/status", "", true, 200},
		{"GET", "/api/v1/projects/desktop/conversation", "", true, 200},
		{"POST", "/api/v1/projects/desktop/conversation", `{"session_key":"chat","thread":"T1"}`, true, 200},
		{"POST", "/api/v1/projects/desktop/conversation", `{"action":"recover_creation","id":"job"}`, false, 401},
		{"POST", "/api/v1/projects/desktop/conversation", `{"action":"recover_creation","id":"job"}`, true, 200},
		{"POST", "/api/v1/projects/desktop/conversation", `{"action":"recover_creation","id":"wrong"}`, true, 400},
		{"POST", "/api/v1/projects/desktop/conversation", `{"action":"create"}`, true, 400},
		{"POST", "/api/v1/projects/desktop/conversation/feishu", `{"action":"start"}`, true, 200},
		{"POST", "/api/v1/projects/desktop/conversation/feishu", `{"action":"other"}`, true, 400},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.auth {
			r.Header.Set("Authorization", "Bearer admin")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if a.bound != 1 || setup.actions != 1 || a.creationRecoveries != 1 {
		t.Fatal("retained routes did not execute")
	}
	m.Stop()
	if !setup.closed {
		t.Fatal("setup not closed")
	}
}
func TestDesktopManagementRequiresToken(t *testing.T) {
	if NewManagementServer(0, "").SetLocalOnly() == nil {
		t.Fatal("empty token accepted")
	}
	m := NewManagementServer(0, "admin")
	if m.SetLocalOnly() != nil || m.listenHost != "127.0.0.1" {
		t.Fatal("not loopback")
	}
}
func TestDesktopRecoveryFailureStopsEngineStart(t *testing.T) {
	a := &boundTestAgent{recoveryError: errors.New("storage failure")}
	if NewEngine("desktop", a, nil).Start() == nil {
		t.Fatal("recovery failure ignored")
	}
}

func TestBrowserHandoffOneUseAndAuthenticatedShutdown(t *testing.T) {
	m := NewManagementServer(18789, "private-test-token")
	mux := http.NewServeMux()
	m.buildHandler(mux)
	url := m.BrowserURL()
	code := strings.Split(url, "#launch=")[1]
	if strings.Contains(url, m.token) {
		t.Fatal("permanent token leaked in URL")
	}
	request := func(path, body, origin, auth string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	body := `{"code":"` + code + `"}`
	if request("/api/v1/launch", body, "https://unrelated.example", "") != 403 {
		t.Fatal("cross-origin handoff")
	}
	if request("/api/v1/launch", body, "", "") != 200 || request("/api/v1/launch", body, "", "") != 401 {
		t.Fatal("handoff not single-use")
	}
	closed := false
	m.SetShutdown(func() { closed = true })
	if request("/api/v1/shutdown", "", "", "") != 401 || closed {
		t.Fatal("unauthenticated shutdown")
	}
	if request("/api/v1/shutdown", "", "", m.token) != 200 || !closed {
		t.Fatal("shutdown ignored")
	}
}
