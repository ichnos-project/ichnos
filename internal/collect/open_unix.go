//go:build unix

package collect

import (
	"os"
	"syscall"
)

// openFlags opens without blocking, so that opening a pipe with no writer
// returns at once and the regular file check can reject it.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK
