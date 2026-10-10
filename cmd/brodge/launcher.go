package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/config"
)

func defaultConfig() (string, error) {
	if executable, err := os.Executable(); err == nil {
		hint := filepath.Join(filepath.Dir(filepath.Dir(executable)), "Resources", "config-path")
		if bytes, err := os.ReadFile(hint); err == nil {
			path := strings.TrimSpace(string(bytes))
			if !filepath.IsAbs(path) {
				return "", fmt.Errorf("absolute app configuration required")
			}
			return path, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, "Library", "Application Support", "Brodge")
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(root, "config.toml")
	if _, err = os.Lstat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", err
	}
	cfg := config.Config{DataDir: root, Language: "zh", Management: config.ManagementConfig{Enabled: true, Port: 18789, Token: hex.EncodeToString(b[:])}, Projects: []config.ProjectConfig{{Name: "desktop", Agent: config.AgentConfig{Type: "codex-desktop", Options: map[string]any{"protocol": "local-v11-start-v2", "allowed_threads": []string{}}}}}}
	var buf bytes.Buffer
	if err = toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return path, nil
		}
		return "", err
	}
	_, err = f.Write(buf.Bytes())
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	return path, err
}
func launcherLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".instance", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func openBrowser(url string) error { return exec.Command("open", url).Run() }
func existingBrowser(cfg *config.Config) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", cfg.Management.Port)
	request, err := http.NewRequest("POST", endpoint+"/api/v1/browser", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.Management.Token)
	resp, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) != nil || !body.OK || body.Data.URL == "" {
		return "", fmt.Errorf("port is occupied by another service")
	}
	return body.Data.URL, nil
}
