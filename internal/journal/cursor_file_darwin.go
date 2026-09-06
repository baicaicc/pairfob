//go:build darwin

package journal

import (
	"os"
	"syscall"
)

func openCursorNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func cursorFileStable(before, after os.FileInfo) bool {
	a, aok := before.Sys().(*syscall.Stat_t)
	b, bok := after.Sys().(*syscall.Stat_t)
	return aok && bok && a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtimespec == b.Mtimespec && a.Ctimespec == b.Ctimespec
}
