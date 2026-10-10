package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The Desktop trial keeps its admin surface on this machine.
func (m *ManagementServer) SetLocalOnly() error {
	if strings.TrimSpace(m.token) == "" {
		return fmt.Errorf("local conversation management requires an admin token")
	}
	m.listenHost = "127.0.0.1"
	return nil
}

func (m *ManagementServer) SetLocalFeishuSetup(project string, setup LocalPlatformSetup) {
	if m.localFeishuSetups == nil {
		m.localFeishuSetups = make(map[string]LocalPlatformSetup)
	}
	m.localFeishuSetups[project] = setup
}

func (m *ManagementServer) handleLocalFeishuSetup(w http.ResponseWriter, r *http.Request, project string) {
	setup := m.localFeishuSetups[project]
	if setup == nil || m.listenHost != "127.0.0.1" || m.token == "" {
		mgmtError(w, http.StatusServiceUnavailable, "local Feishu setup unavailable")
		return
	}
	if r.Method == http.MethodGet {
		mgmtJSON(w, http.StatusOK, setup.State())
		return
	}
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		mgmtError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Action == "enable" {
		if m.enableFeishu == nil {
			mgmtError(w, 503, "connection unavailable")
			return
		}
		if err := m.enableFeishu(project); err != nil {
			mgmtError(w, 400, err.Error())
			return
		}
		mgmtJSON(w, 200, setup.State())
		return
	}
	state, err := setup.Action(body.Action)
	if err != nil {
		mgmtError(w, http.StatusBadRequest, err.Error())
		return
	}
	mgmtJSON(w, http.StatusOK, state)
}

func (m *ManagementServer) handleBoundConversation(w http.ResponseWriter, r *http.Request, e *Engine) {
	bound := e.agent
	switch r.Method {
	case http.MethodGet:
		mgmtJSON(w, http.StatusOK, bound.ConversationState())
	case http.MethodPost:
		var body struct {
			SessionKey string `json:"session_key"`
			Thread     string `json:"thread"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if err := bound.BindConversation(r.Context(), body.SessionKey, body.Thread); err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		mgmtOK(w, "conversation bound")
	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) SetFeishuActivator(fn func(string) error) { m.enableFeishu = fn }
