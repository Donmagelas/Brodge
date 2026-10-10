package codexdesktop

import (
	"context"
	"fmt"
	"time"
)

// Preflight is initialize-only unless the caller supplies an explicit thread.
// It never submits a turn, answers a question or reads authentication files.
func Preflight(ctx context.Context, socket, thread string) (map[string]any, error) {
	out := map[string]any{"initialized": false, "history_loaded": false, "send_attempted": false, "account_route_verified": false}
	if thread != "" {
		var err error
		thread, err = parseThread(thread)
		if err != nil {
			return out, err
		}
	}
	path, err := endpoint(socket)
	if err != nil {
		return out, err
	}
	if thread == "" {
		c, err := connectIPC(ctx, path, 5*time.Second)
		if err != nil {
			return out, err
		}
		defer c.abort(fmt.Errorf("detached"))
		ack, err := c.rpc(ctx, "initialize", wire{"clientType": "Brodge-desktop-preflight"}, "")
		if err != nil {
			return out, err
		}
		if str(obj(ack["result"])["clientId"]) == "" {
			return out, fmt.Errorf("invalid-initialize")
		}
		out["initialized"] = true
		return out, nil
	}
	a, err := attachIPC(ctx, path, thread, 5*time.Second)
	if err != nil {
		return out, err
	}
	defer a.c.abort(fmt.Errorf("detached"))
	state, err := a.snapshot(ctx)
	if err != nil {
		return out, err
	}
	observedIdle, err := idle(state, thread)
	if err != nil {
		return out, err
	}
	out["initialized"] = true
	out["history_loaded"] = true
	out["observed_idle"] = observedIdle
	out["authoritative_idle_verified"] = false
	out["pending_requests"] = len(list(state["requests"]))
	return out, nil
}
