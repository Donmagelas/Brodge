package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/platform/feishu"
)

func init() {
	runLocalFeishuPair = func() error { return feishu.RunPair(context.Background(), os.Stdin, os.Stdout) }
	createBoundPlatformSetup = func(project, dataDir string, agent core.Agent, platforms []core.Platform) (core.LocalPlatformSetup, error) {
		setup, err := feishu.NewSetup(filepath.Join(dataDir, "desktop", project))
		if err != nil {
			return nil, err
		}
		// A single explicitly configured thread gives the pairing an unambiguous
		// target. Multiple allowed threads still require normal Web or /bind choice.
		binder, ok := agent.(interface {
			BindPairedConversation(context.Context, core.Platform, core.ConversationOrigin, string) error
		})
		origin, paired := setup.PairedOrigin()
		if paired {
			for _, p := range platforms {
				if creator, ok := p.(interface {
					SetCreator(core.ConversationOrigin) error
				}); ok {
					if err := creator.SetCreator(origin); err != nil {
						_ = setup.Close()
						return nil, err
					}
				}
			}
		}
		if ok && paired {
			threads, err := agent.ListSessions(context.Background())
			if err != nil {
				_ = setup.Close()
				return nil, err
			}
			target := ""
			if len(threads) == 1 {
				target = threads[0].ID
			}
			// Register authenticated pairing even without a chosen conversation,
			// so Web can bind before the first ordinary message arrives.
			for _, p := range platforms {
				if sender, ok := p.(core.DurableConversationPlatform); ok && sender.AcceptConversationOrigin(origin) {
					if err := binder.BindPairedConversation(context.Background(), p, origin, target); err != nil {
						_ = setup.Close()
						return nil, err
					}
					break
				}
			}
		}
		return setup, nil
	}
}
