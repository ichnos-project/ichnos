//go:build unix

package collect

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenRegularRejectsSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "record.dockerbuild")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	for _, path := range []string{fifo, "/dev/null"} {
		t.Run(path, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				f, err := openRegular(path, testLimits())
				if f != nil {
					err = errors.Join(err, f.Close())
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNotRegularFile) {
					t.Fatalf("openRegular(%q) error = %v, want ErrNotRegularFile", path, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("openRegular(%q) blocked", path)
			}
		})
	}
}
