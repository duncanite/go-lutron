package credentials_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forkcloser/xos"
	"github.com/mycophonic/primordium/filesystem"

	"github.com/duncanite/go-lutron/credentials"
	"github.com/duncanite/go-lutron/leap"
)

// TestReserveSaveLoad: the two steps around a pairing, what Load reads
// back, and what a directory that holds credentials says.
func TestReserveSaveLoad(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "made", "on", "the", "way")

	if err := credentials.Present(dir); err != nil {
		t.Fatalf("a directory that is not there: %v", err)
	}

	if _, err := credentials.Load(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading from a directory that is not there: %v", err)
	}

	reserved, err := credentials.Reserve(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer reserved.Discard()

	// Reserved is not present: nothing is there to use yet.
	if err = credentials.Present(dir); err != nil {
		t.Fatalf("a reservation alone: %v", err)
	}

	given := &leap.Credentials{Key: []byte("the key"), Certificate: []byte("the certificate"), Root: []byte("the root")}

	if err = reserved.Save(given); err != nil {
		t.Fatal(err)
	}

	loaded, err := credentials.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if string(loaded.Key) != "the key" || string(loaded.Certificate) != "the certificate" ||
		string(loaded.Root) != "the root" {
		t.Fatalf("loaded: %q, %q, %q", loaded.Key, loaded.Certificate, loaded.Root)
	}

	if err = credentials.Present(dir); !errors.Is(err, credentials.ErrExists) {
		t.Fatalf("a directory that holds credentials: %v", err)
	}

	// A reservation given up leaves nothing behind.
	empty := filepath.Join(t.TempDir(), "given up")

	abandoned, err := credentials.Reserve(empty)
	if err != nil {
		t.Fatal(err)
	}

	abandoned.Discard()

	if left, err := xos.ReadDir(empty); err != nil ||
		len(left) != 0 {
		t.Fatalf("after a reservation given up the directory holds %v (%v)", left, err)
	}
}

// TestPresent: any one of the three files makes a directory one that
// holds credentials, and the error names the file.
func TestPresent(t *testing.T) {
	t.Parallel()

	for _, name := range []string{credentials.KeyFile, credentials.CertificateFile, credentials.RootFile} {
		dir := t.TempDir()
		path := filepath.Join(dir, name)

		if err := filesystem.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := credentials.Present(dir)
		if err == nil {
			t.Fatalf("%s alone: the directory reads as empty", name)
		}

		if !errors.Is(err, credentials.ErrExists) || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s alone: %v", name, err)
		}
	}
}
