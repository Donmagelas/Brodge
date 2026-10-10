package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func randomLaunchCode() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// One-use browser handoff; the permanent management token never appears in a URL.
func (m *ManagementServer) BrowserURL() string {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.launchCode = randomLaunchCode()
	m.launchExpiry = time.Now().Add(time.Minute)
	return fmt.Sprintf("http://127.0.0.1:%d/#launch=%s", m.port, m.launchCode)
}
func (m *ManagementServer) SetShutdown(fn func()) { m.shutdown = fn }
func (m *ManagementServer) lifecycle(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/api/v1/launch" {
		if r.Method != "POST" {
			mgmtError(w, 405, "POST only")
			return true
		}
		// A browser from any other origin cannot redeem a local launch code.
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			mgmtError(w, 403, "invalid origin")
			return true
		}
		var body struct {
			Code string `json:"code"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil {
			mgmtError(w, 400, "invalid request")
			return true
		}
		m.lifecycleMu.Lock()
		valid := m.launchCode != "" && time.Now().Before(m.launchExpiry) && subtle.ConstantTimeCompare([]byte(body.Code), []byte(m.launchCode)) == 1
		if valid {
			m.launchCode = ""
		}
		m.lifecycleMu.Unlock()
		if !valid {
			mgmtError(w, 401, "expired launch code")
			return true
		}
		mgmtJSON(w, 200, map[string]string{"token": m.token})
		return true
	}
	return false
}
