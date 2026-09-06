//go:build linux

package daemon

import "testing"

func TestLinuxCursorStartTimeUsesField22AfterClosingParen(t *testing.T) {
	raw := []byte("42 (name with ) paren) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 987654 20")
	start, err := linuxStartTime(raw)
	if err != nil || start != "987654" {
		t.Fatalf("start=%q err=%v", start, err)
	}
	if _, err := linuxStartTime([]byte("malformed")); err == nil {
		t.Fatal("malformed stat was accepted")
	}
}
