package main

import (
	"log"

	"pairfob/internal/runtime"
)

// mergeHerdrSessions puts every running Herdr session into one view when
// PAIRFOB_MERGE_SESSIONS=1 (runtime.Merged): the phone lists all of them at
// once, each session's panes notify, and the per-session switcher is off.
// Any other value, or a runtime other than Herdr, keeps rt unchanged.
func mergeHerdrSessions(rt runtime.Runtime) runtime.Runtime {
	if getenv("PAIRFOB_MERGE_SESSIONS", "") != "1" {
		return rt
	}
	herdr, ok := rt.(*runtime.Herdr)
	if !ok {
		log.Printf("runtime sessions_not_merged: PAIRFOB_MERGE_SESSIONS needs the Herdr runtime")
		return rt
	}
	log.Printf("runtime sessions_merged %s", herdr.ConfigRoot)
	return runtime.NewMerged(herdr)
}
