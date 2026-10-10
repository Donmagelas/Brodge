package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `data_dir = ".state"
language = "zh"
[management]
enabled = true
token = "${BRIDGE_TEST_TOKEN}"
[[projects]]
name = "desktop"
[projects.agent]
type = "codex-desktop"
[projects.agent.options]
protocol = "local-v11-start-v2"
allowed_threads = ["T1"]
[[projects.platforms]]
type = "feishu"
[projects.platforms.options]
app_secret = "${BRIDGE_TEST_SECRET}"
`

func TestConfigPreservesLocalBridgeAndRejectsRemovedRuntime(t *testing.T) {
	t.Setenv("BRIDGE_TEST_TOKEN", "local-test")
	t.Setenv("BRIDGE_TEST_SECRET", "local-secret")
	for _, tc := range []struct {
		name, body string
		allowed    bool
	}{
		{"current", testConfig, true},
		{"provider", testConfig + "\n[[providers]]\nname='old'", false},
		{"cron", testConfig + "\n[cron]\nenabled=true", false},
		{"tts", testConfig + "\n[tts]\nenabled=true", false},
		{"other-agent", strings.Replace(testConfig, "codex-desktop", "claudecode", 1), false},
		{"future-im-not-enabled", strings.Replace(testConfig, `type = "feishu"`, `type = "weixin"`, 1), false},
		{"unsafe-name", strings.Replace(testConfig, `name = "desktop"`, `name = "../desktop"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.allowed {
				t.Fatal("unexpected config acceptance", err)
			}
			if tc.allowed && (cfg.Management.Token != "local-test" || cfg.Projects[0].Platforms[0].Options["app_secret"] != "local-secret" || !filepath.IsAbs(cfg.DataDir)) {
				t.Fatal("local config compatibility lost")
			}
		})
	}
}
