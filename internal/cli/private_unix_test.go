//go:build !windows

package cli_test

import (
	"testing"

	"github.com/forkcloser/xos"
)

// ownerOnly fails the test unless the file is readable by its owner
// alone: mode 0600.
func ownerOnly(t *testing.T, path string) {
	t.Helper()

	info, err := xos.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s is %v", path, info.Mode().Perm())
	}
}
