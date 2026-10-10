//go:build darwin && cgo

package codexdesktop

/*
#cgo LDFLAGS: -framework AppKit -framework ApplicationServices
#include <stdlib.h>
#include <stdint.h>
uintptr_t brodgePrepareDraft(const char *, const char *, char **);
char *brodgeSubmitDraft(uintptr_t, char **);
void brodgeCloseDraft(uintptr_t);
int brodgeAccessibilityGranted(void);
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/chenhg5/cc-connect/core"
)

type nativeDraft struct{ handle C.uintptr_t }

func desktopAccessibilityGranted() bool { return C.brodgeAccessibilityGranted() != 0 }

func prepareDesktopDraft(ctx context.Context, text string, media []core.ConversationMedia) (desktopDraft, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".codex")
	}
	if err := checkCreationKeymap(filepath.Join(root, "keybindings.json")); err != nil {
		return nil, err
	}
	paths := []string{}
	for _, m := range media {
		paths = append(paths, m.Path)
	}
	encoded, _ := json.Marshal(paths)
	prompt, files := C.CString(text), C.CString(string(encoded))
	defer C.free(unsafe.Pointer(prompt))
	defer C.free(unsafe.Pointer(files))
	var message *C.char
	h := C.brodgePrepareDraft(prompt, files, &message)
	if message != nil {
		defer C.free(unsafe.Pointer(message))
		return nil, fmt.Errorf("%s", C.GoString(message))
	}
	if h == 0 {
		return nil, fmt.Errorf("desktop-creation-unavailable")
	}
	return &nativeDraft{h}, nil
}

func (d *nativeDraft) Submit(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var message *C.char
	link := C.brodgeSubmitDraft(d.handle, &message)
	if link != nil {
		defer C.free(unsafe.Pointer(link))
	}
	if message != nil {
		defer C.free(unsafe.Pointer(message))
		return "", fmt.Errorf("%s", C.GoString(message))
	}
	if link == nil {
		return "", fmt.Errorf("desktop-creation-link-unavailable")
	}
	return C.GoString(link), nil
}
func (d *nativeDraft) Close() { C.brodgeCloseDraft(d.handle); d.handle = 0 }
