package main

import (
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"path/filepath"
	"testing"
)

func TestOnlyRequiredAdaptersAreRegistered(t *testing.T) {
	agents := core.ListRegisteredAgents()
	platforms := core.ListRegisteredPlatforms()
	if len(agents) != 1 || agents[0] != "codex-desktop" {
		t.Fatal("unexpected agents", agents)
	}
	if len(platforms) != 1 || platforms[0] != "feishu" {
		t.Fatal("unexpected platforms", platforms)
	}
}

func TestLauncherSingleInstanceLockAndDefaultProjectlessConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	first, err := launcherLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := launcherLock(path); err == nil {
		f.Close()
		t.Fatal("second instance locked")
	}
	first.Close()
	again, err := launcherLock(path)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	again.Close()
	t.Setenv("HOME", t.TempDir())
	p, err := defaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 || len(cfg.Projects[0].Platforms) != 0 || cfg.Projects[0].Agent.Type != "codex-desktop" {
		t.Fatal("default config wrong")
	}
	before, _ := os.ReadFile(p)
	if _, err = defaultConfig(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("default config overwritten")
	}
}
