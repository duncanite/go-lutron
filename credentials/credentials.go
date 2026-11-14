// Package credentials keeps what pairing with a bridge gave as three
// files in a directory: the client's key, its certificate, and the root
// the bridge's certificate chains to. The key is private, readable by
// its owner alone: by its mode, and on Windows, where a mode does not
// say who may read a file, by an access list of the current user alone.
//
// Writing them is in two steps, around the pairing itself. Reserve,
// before the bridge's button is asked for, makes the directory and the
// key's file, empty and already private, and keeps that file open; Save,
// after, writes the key through that open file and renames it into
// place. So the key is never in a file another could open, a place that
// cannot hold a private file is found out before the bridge signs, and a
// file another put under the reserved name meanwhile is refused.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/forkcloser/xos"
	"github.com/mycophonic/primordium/filesystem"

	"github.com/duncanite/go-lutron/leap"
)

// The files' names in the directory.
const (
	KeyFile         = "key.pem"
	CertificateFile = "certificate.pem"
	RootFile        = "root.pem"

	// reservedSuffix names the file the key is written into before it
	// takes the key's name.
	reservedSuffix = ".new"
	keyPermissions = 0o600
)

// The errors, to test with errors.Is.
var (
	// ErrExists: the directory already holds credentials.
	ErrExists = errors.New("credentials are already there")
	// ErrReplaced: the file reserved for the key is no longer the one
	// under its name; nothing was written.
	ErrReplaced = errors.New(
		"the file made for the key is no longer the one by its name; nothing was written to it",
	)
)

// Load reads the three files from dir.
func Load(dir string) (*leap.Credentials, error) {
	var (
		credentials leap.Credentials
		err         error
	)

	for name, into := range map[string]*[]byte{
		KeyFile: &credentials.Key, CertificateFile: &credentials.Certificate, RootFile: &credentials.Root,
	} {
		if *into, err = xos.ReadFile(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("reading the credentials: %w", err)
		}
	}

	return &credentials, nil
}

// Present returns ErrExists, naming the file, when dir holds any of the
// three files.
func Present(dir string) error {
	for _, name := range []string{KeyFile, CertificateFile, RootFile} {
		if _, err := xos.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%w: %s", ErrExists, filepath.Join(dir, name))
		}
	}

	return nil
}

// Reservation is a place made for credentials before they exist: the
// directory, and the key's file, empty, private and open.
type Reservation struct {
	dir  string
	file *os.File
}

// Reserve makes dir and, in it, the file the key will be written into:
// empty and its owner's alone before it holds anything. The caller ends
// the reservation with Save, or with Discard when no credentials came.
func Reserve(dir string) (*Reservation, error) {
	if err := os.MkdirAll(dir, filesystem.DirPermissionsDefault); err != nil {
		return nil, fmt.Errorf("making %s: %w", dir, err)
	}

	path := filepath.Join(dir, KeyFile+reservedSuffix)

	// One left by a run that died: its mode and its list are not known.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clearing %s: %w", path, err)
	}

	file, err := xos.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyPermissions)
	if err != nil {
		return nil, fmt.Errorf("making %s: %w", path, err)
	}

	if err = private(path); err != nil {
		_ = file.Close()
		_ = os.Remove(path)

		return nil, err
	}

	return &Reservation{dir: dir, file: file}, nil
}

// Save writes the credentials into the reserved place. The certificate
// and the root go first, atomically; the key is written through the
// reserved file's handle and that file renamed to the key's name,
// keeping its mode and its access list, so the key's file appears last,
// whole and private. A reserved name that no longer names the reserved
// file is refused with ErrReplaced: the rename would put that other file
// in the key's place.
func (r *Reservation) Save(credentials *leap.Credentials) error {
	for name, data := range map[string][]byte{
		CertificateFile: credentials.Certificate, RootFile: credentials.Root,
	} {
		path := filepath.Join(r.dir, name)
		if err := filesystem.WriteFile(path, data, filesystem.FilePermissionsDefault); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}

	held, err := r.file.Stat()
	if err != nil {
		return fmt.Errorf("writing the key: %w", err)
	}

	named, err := os.Lstat(r.file.Name())
	if err != nil || !os.SameFile(held, named) {
		return fmt.Errorf("%w: %s", ErrReplaced, r.file.Name())
	}

	_, err = r.file.Write(credentials.Key)
	if err == nil {
		err = r.file.Sync()
	}

	if closeErr := r.file.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return fmt.Errorf("writing the key: %w", err)
	}

	key := filepath.Join(r.dir, KeyFile)
	if err = xos.Rename(r.file.Name(), key); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}

	return nil
}

// Discard ends a reservation: the reserved file is closed and removed.
// After Save it finds nothing left to do, so it can be deferred.
func (r *Reservation) Discard() {
	_ = r.file.Close()
	_ = os.Remove(r.file.Name())
}
