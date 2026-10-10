package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/chenhg5/cc-connect/agent/codexdesktop"
)

func runDesktopCheck(args []string) {
	flags := flag.NewFlagSet("desktop-check", flag.ExitOnError)
	socket := flags.String("socket", "", "absolute local Desktop socket")
	thread := flags.String("thread", "", "explicit existing thread for read-only state check")
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	result, err := codexdesktop.Preflight(context.Background(), *socket, *thread)
	if err != nil {
		result["error"] = err.Error()
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
	if err != nil {
		os.Exit(2)
	}
}
