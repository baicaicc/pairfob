//go:build !darwin && !linux

package daemon

import (
	"context"
	"errors"

	"pairfob/internal/journal"
)

func nativeCursorProcessIdentity(context.Context, int) (*journal.CursorProcessIdentity, error) {
	return nil, errors.New("Cursor process identity inspection is unsupported on this platform")
}
