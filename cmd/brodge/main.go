package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

var runLocalFeishuPair func() error
var createBoundPlatformSetup func(string, string, core.Agent, []core.Platform) (core.LocalPlatformSetup, error)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "desktop-check":
			runDesktopCheck(os.Args[2:])
			return
		case "desktop-feishu-pair":
			if runLocalFeishuPair == nil || runLocalFeishuPair() != nil {
				os.Exit(2)
			}
			return
		}
	}
	flags := flag.NewFlagSet("Brodge", flag.ExitOnError)
	path := flags.String("config", "", "local Brodge configuration")
	noBrowser := flags.Bool("no-browser", false, "run without opening a browser")
	_ = flags.Parse(os.Args[1:])
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
	if *path == "" {
		p, err := defaultConfig()
		if err != nil {
			slog.Error("Brodge setup failed", "error", err)
			os.Exit(1)
		}
		*path = p
	}
	var err error
	executable, _ := os.Executable()
	if !*noBrowser && strings.HasSuffix(filepath.Dir(executable), ".app/Contents/MacOS") {
		err = runApplication(*path)
	} else {
		err = runWithBrowser(*path, !*noBrowser)
	}
	if err != nil {
		slog.Error("Brodge stopped", "error", err)
		os.Exit(1)
	}
}
func run(path string) error { return runWithBrowser(path, false) }
func runWithBrowser(path string, browser bool) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	lock, lockErr := launcherLock(path)
	if lockErr != nil {
		url, err := existingBrowser(cfg)
		if err != nil {
			return fmt.Errorf("Brodge instance unavailable: %w", err)
		}
		if browser {
			return openBrowser(url)
		}
		return nil
	}
	defer lock.Close()
	project := cfg.Projects[0]
	opts := project.Agent.Options
	if opts == nil {
		opts = map[string]any{}
	}
	opts["cc_data_dir"] = cfg.DataDir
	opts["cc_project"] = project.Name
	opts["language"] = cfg.Language
	agent, err := core.CreateAgent(project.Agent.Type, opts)
	if err != nil {
		return err
	}
	platforms := []core.Platform{}
	for _, pc := range project.Platforms {
		p, err := core.CreatePlatform(pc.Type, pc.Options)
		if err != nil {
			_ = agent.Stop()
			return err
		}
		platforms = append(platforms, p)
	}
	engine := core.NewEngine(project.Name, agent, platforms)
	defer engine.Stop()
	management := core.NewManagementServer(cfg.Management.Port, cfg.Management.Token)
	management.RegisterEngine(project.Name, engine)
	closed := make(chan struct{})
	var stopOnce sync.Once
	management.SetShutdown(func() { stopOnce.Do(func() { close(closed) }) })
	defer management.Stop()
	if createBoundPlatformSetup != nil {
		setup, err := createBoundPlatformSetup(project.Name, cfg.DataDir, agent, platforms)
		if err != nil {
			return err
		}
		management.SetLocalFeishuSetup(project.Name, setup)
		if factory, ok := setup.(interface{ PairedPlatform() (core.Platform, error) }); ok {
			if len(platforms) == 0 {
				if p, err := factory.PairedPlatform(); err == nil {
					platforms = append(platforms, p)
					engine.ConfigurePlatform(p)
				}
			}
			management.SetFeishuActivator(func(name string) error {
				if name != project.Name {
					return fmt.Errorf("unknown connection")
				}
				p, err := factory.PairedPlatform()
				if err != nil {
					return err
				}
				return engine.ConnectPlatform(p)
			})
		}
	}
	if err := engine.Start(); err != nil {
		return err
	}
	if err := management.Start(); err != nil {
		return err
	}
	slog.Info("Brodge ready", "url", fmt.Sprintf("http://127.0.0.1:%d", cfg.Management.Port))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if browser {
		if err := openBrowser(management.BrowserURL()); err != nil {
			slog.Warn("Brodge browser unavailable")
		}
	}
	select {
	case <-stop:
	case <-closed:
	}
	return nil
}
