package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ManagementServer struct {
	enableFeishu      func(string) error
	port              int
	token, listenHost string
	engines           map[string]*Engine
	localFeishuSetups map[string]LocalPlatformSetup
	server            *http.Server
	started           time.Time
	lifecycleMu       sync.Mutex
	launchCode        string
	launchExpiry      time.Time
	shutdown          func()
}

func NewManagementServer(port int, token string) *ManagementServer {
	return &ManagementServer{port: port, token: token, listenHost: "127.0.0.1", engines: map[string]*Engine{}, localFeishuSetups: map[string]LocalPlatformSetup{}, started: time.Now()}
}
func (m *ManagementServer) RegisterEngine(name string, e *Engine) { m.engines[name] = e }
func (m *ManagementServer) Start() error {
	if err := m.SetLocalOnly(); err != nil {
		return err
	}
	mux := http.NewServeMux()
	m.buildHandler(mux)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.port))
	if err != nil {
		return err
	}
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go m.server.Serve(listener)
	return nil
}
func (m *ManagementServer) Stop() {
	for _, setup := range m.localFeishuSetups {
		_ = setup.Close()
	}
	for _, engine := range m.engines {
		_ = engine.Stop()
	}
	if m.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.server.Shutdown(ctx)
	}
}
func mgmtJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}
func mgmtOK(w http.ResponseWriter, message string) {
	mgmtJSON(w, 200, map[string]string{"message": message})
}
func mgmtError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": message})
}
func (m *ManagementServer) buildHandler(mux *http.ServeMux) {
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if m.lifecycle(w, r) {
			return
		}
		if m.token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+m.token)) != 1 {
			mgmtError(w, 401, "Unauthorized")
			return
		}
		switch r.URL.Path {
		case "/api/v1/shutdown":
			if r.Method != "POST" {
				mgmtError(w, 405, "POST only")
				return
			}
			if m.shutdown == nil {
				mgmtError(w, 503, "shutdown unavailable")
				return
			}
			mgmtOK(w, "Brodge 已退出")
			m.shutdown()
			return
		case "/api/v1/browser":
			if r.Method != "POST" {
				mgmtError(w, 405, "POST only")
				return
			}
			mgmtJSON(w, 200, map[string]string{"url": m.BrowserURL()})
			return
		case "/api/v1/status":
			if r.Method != "GET" {
				mgmtError(w, 405, "GET only")
				return
			}
			connections := []any{}
			for name, e := range m.engines {
				platforms := []PlatformHealthInfo{}
				for _, p := range e.Platforms() {
					if health, ok := p.(PlatformHealth); ok {
						platforms = append(platforms, health.PlatformHealth())
					}
				}
				connections = append(connections, map[string]any{"name": name, "platforms": platforms})
			}
			mgmtJSON(w, 200, map[string]any{"application": "Brodge", "uptime": time.Since(m.started).Seconds(), "connections": connections})
			return
		}
		prefix := "/api/v1/projects/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			mgmtError(w, 404, "Not found")
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		e := m.engines[parts[0]]
		if e == nil {
			mgmtError(w, 404, "Not found")
			return
		}
		if len(parts) == 2 && parts[1] == "conversation" {
			m.handleBoundConversation(w, r, e)
			return
		}
		if len(parts) == 3 && parts[1] == "conversation" && parts[2] == "feishu" {
			m.handleLocalFeishuSetup(w, r, parts[0])
			return
		}
		mgmtError(w, 404, "Not found")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve only the retained UI entrypoints. Removed modules cannot fall back to the SPA.
		entry := r.URL.Path == "/" || r.URL.Path == "/login"
		for name := range m.engines {
			entry = entry || r.URL.Path == "/projects/"+name
		}
		assets := GetWebAssets()
		if assets == nil {
			http.NotFound(w, r)
			return
		}
		if entry {
			bytes, err := fs.ReadFile(assets, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(bytes)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if !(strings.HasPrefix(path, "assets/") || path == "favicon.svg") {
			http.NotFound(w, r)
			return
		}
		http.FileServer(http.FS(assets)).ServeHTTP(w, r)
	})
}
