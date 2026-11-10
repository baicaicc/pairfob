package main

import (
	"testing"
	"time"
)

// scripts/build-kidsbox-daemon.sh stamps "kidsbox-<date>-<commit>". The hosted
// channel must never be able to replace such a build, automatically or from
// the phone's update button.
func TestForkBuildVersionNeverSelfUpdates(t *testing.T) {
	for _, current := range []string{"kidsbox-20261109-abc1234", "kidsbox-20261109-1234567", "kidsbox-20261109-abc1234-dirty"} {
		if releaseVersion.MatchString(current) {
			t.Fatalf("%s reads as a release version", current)
		}
		for _, target := range []string{"1.4.0", "v9.9.9", "2026-12-01"} {
			if newerVersion(target, current) {
				t.Fatalf("%s ordered as newer than %s", target, current)
			}
			note, install := autoUpdateStep(autoUpdateNote{Target: target, FirstSeen: 1}, time.Unix(1, 0).Add(48*time.Hour), current, target, time.Hour)
			if install || note != (autoUpdateNote{}) {
				t.Fatalf("auto update would install %s over %s (note %+v)", target, current, note)
			}
		}
	}
	saved := version
	t.Cleanup(func() { version = saved })
	version = "kidsbox-20261109-abc1234"
	u := &remoteUpdater{available: true, dir: t.TempDir(), run: func(string) error { t.Fatal("update started"); return nil }}
	if _, err := u.Start("op_manual_update_0001", "1.4.0"); err == nil {
		t.Fatal("the manual update path accepted a hosted release")
	}
}
