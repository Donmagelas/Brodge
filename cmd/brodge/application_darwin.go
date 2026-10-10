//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework AppKit
void brodgeAppRun(void);
void brodgeAppStop(void);
*/
import "C"

import (
	"log/slog"
	"runtime"

	"github.com/chenhg5/cc-connect/config"
)

func init() { runtime.LockOSThread() }

var appReopen = make(chan struct{}, 1)

//export brodgeAppReopen
func brodgeAppReopen() {
	select {
	case appReopen <- struct{}{}:
	default:
	}
}

// AppKit only handles application open/reopen events. The interface stays in
// the user's ordinary browser; all services remain in the same core process.
func runApplication(path string) error {
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- runWithBrowser(path, true); close(done); C.brodgeAppStop() }()
	go func() {
		for {
			select {
			case <-done:
				return
			case <-appReopen:
				cfg, err := config.Load(path)
				if err == nil {
					var url string
					url, err = existingBrowser(cfg)
					if err == nil {
						err = openBrowser(url)
					}
				}
				if err != nil {
					slog.Warn("Brodge is still starting or its browser could not be opened")
				}
			}
		}
	}()
	C.brodgeAppRun()
	return <-result
}
