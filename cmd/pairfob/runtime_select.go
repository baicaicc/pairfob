package main

import (
	"context"
	"strings"
	"time"

	"pairfob/internal/runtime"
)

// openRuntime picks the harness runtime from the process environment.
//
//	PAIRFOB_DEV_FAKE_RUNTIME=1  in-memory fake (development only)
//	PAIRFOB_HERDR_MULTI=1       every live Herdr session under ~/.config/herdr,
//	                            ids qualified as <session>::<id>
//	PAIRFOB_MULTI_SESSION=1     single socket, named sessions allowed per RPC
//	(default)                   the single socket from HERDR_SOCKET_PATH or
//	                            ~/.config/herdr/herdr.sock
func openRuntime() (runtime.Runtime, string, error) {
	if getenv("PAIRFOB_DEV_FAKE_RUNTIME", "") == "1" {
		return runtime.Open(true, false)
	}
	if getenv("PAIRFOB_HERDR_MULTI", "") == "1" {
		return runtime.OpenMulti()
	}
	return runtime.Open(false, getenv("PAIRFOB_MULTI_SESSION", "") == "1")
}

// herdrHealthNote describes the live runtime for `pairfob doctor`.
func herdrHealthNote(rt runtime.Runtime) string {
	multi, ok := rt.(*runtime.Multi)
	if !ok {
		return "on"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions := multi.Sessions(ctx)
	if len(sessions) == 0 {
		return "on (no live sessions)"
	}
	return "on (" + strings.Join(sessions, ", ") + ")"
}
