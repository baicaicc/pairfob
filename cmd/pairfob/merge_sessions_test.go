package main

import (
	"testing"

	"pairfob/internal/runtime"
)

func TestMergeHerdrSessionsPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		merged      bool
	}{
		{name: "unset"},
		{name: "explicit off", value: "0"},
		{name: "unknown value", value: "true"},
		{name: "explicit on", value: "1", merged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAIRFOB_MERGE_SESSIONS", tc.value)
			herdr := runtime.NewHerdr("/tmp/herdr.sock")
			got := mergeHerdrSessions(herdr)
			merged, ok := got.(*runtime.Merged)
			if ok != tc.merged {
				t.Fatalf("mergeHerdrSessions returned %T, merged=%v", got, tc.merged)
			}
			if ok && (merged.Herdr() != herdr || !herdr.Multi) {
				t.Fatal("merged view must wrap the same adapter with session discovery on")
			}
		})
	}
}

func TestMergeHerdrSessionsLeavesOtherRuntimes(t *testing.T) {
	t.Setenv("PAIRFOB_MERGE_SESSIONS", "1")
	fake := runtime.NewFake()
	if got := mergeHerdrSessions(fake); got != runtime.Runtime(fake) {
		t.Fatalf("a non-Herdr runtime must pass through, got %T", got)
	}
}
