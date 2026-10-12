package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type setupBindingAgent struct {
	threads []core.AgentSessionInfo
	bound   string
	paired  bool
	fail    bool
}

func (a *setupBindingAgent) Name() string { return "test" }
func (a *setupBindingAgent) Stop() error  { return nil }
func (a *setupBindingAgent) HandleConversation(context.Context, core.Platform, *core.Message) error {
	return nil
}
func (a *setupBindingAgent) ResumeConversations(context.Context, []core.Platform) error { return nil }
func (a *setupBindingAgent) ConversationState() any                                     { return nil }
func (a *setupBindingAgent) BindConversation(context.Context, string, string) error     { return nil }
func (a *setupBindingAgent) UnbindConversation(context.Context, string, string) error   { return nil }
func (a *setupBindingAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return a.threads, nil
}
func (a *setupBindingAgent) BindPairedConversation(_ context.Context, _ core.Platform, _ core.ConversationOrigin, thread string) error {
	if a.fail {
		return fmt.Errorf("pairing-message-conflict")
	}
	a.bound = thread
	a.paired = true
	return nil
}

type setupBindingPlatform struct{ allowed bool }

func (p *setupBindingPlatform) Name() string                    { return "feishu" }
func (p *setupBindingPlatform) Start(core.MessageHandler) error { return nil }
func (p *setupBindingPlatform) Stop() error                     { return nil }
func (p *setupBindingPlatform) ReplyConversation(context.Context, core.ConversationOrigin, string, string) (string, error) {
	return "", nil
}
func (p *setupBindingPlatform) AcceptConversationOrigin(core.ConversationOrigin) bool {
	return p.allowed
}

func TestDesktopSetupStartupBindsOnlyUnambiguousAuthorizedPair(t *testing.T) {
	for _, tc := range []struct {
		name                string
		threads             int
		allowed, fail, want bool
	}{
		{"single", 1, true, false, true}, {"multiple", 2, true, false, false}, {"zero", 0, true, false, false}, {"revoked", 1, false, false, false}, {"conflict", 1, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "desktop", "test")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			o := core.ConversationOrigin{AppID: "app", Tenant: "tenant", Actor: "actor", ChatID: "chat", ChatType: "p2p", MessageID: "pair"}
			body, _ := json.Marshal(map[string]any{"status": "paired", "chat_id": "chat", "pair_origin": o})
			if err := os.WriteFile(filepath.Join(dir, "feishu-setup.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			a := &setupBindingAgent{fail: tc.fail}
			for i := 0; i < tc.threads; i++ {
				a.threads = append(a.threads, core.AgentSessionInfo{ID: fmt.Sprintf("T%d", i+1)})
			}
			s, err := createBoundPlatformSetup("test", root, a, []core.Platform{&setupBindingPlatform{allowed: tc.allowed}})
			if tc.fail {
				if err == nil {
					t.Fatal("binding failure swallowed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if (a.bound == "T1") != tc.want {
				t.Fatal("incorrect startup binding decision")
			}
			if a.paired != tc.allowed {
				t.Fatal("authenticated pairing was not registered")
			}
		})
	}
}
