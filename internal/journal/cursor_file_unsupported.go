//go:build !darwin && !linux

package journal

import (
	"errors"
	"os"
)

func openCursorNoFollow(string) (*os.File, error) {
	return nil, errors.New("Cursor private file inspection is unsupported on this platform")
}

func cursorFileStable(os.FileInfo, os.FileInfo) bool { return false }
