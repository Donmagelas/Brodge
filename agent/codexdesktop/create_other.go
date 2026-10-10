//go:build !darwin || !cgo

package codexdesktop

import (
	"context"
	"fmt"
	"github.com/chenhg5/cc-connect/core"
)

func prepareDesktopDraft(context.Context, string, []core.ConversationMedia) (desktopDraft, error) {
	return nil, fmt.Errorf("自动新建需要 macOS 的 Brodge.app；当前仍可绑定已有会话")
}
