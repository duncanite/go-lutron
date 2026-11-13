//go:build windows

package cli_test

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerOnly fails the test unless the file is readable by its owner
// alone: an access list cut off from the directory's, with one entry,
// the current user's.
func ownerOnly(t *testing.T, path string) {
	t.Helper()

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}

	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}

	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s inherits its directory's access list", path)
	}

	list, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}

	if list == nil || list.AceCount != 1 {
		t.Fatalf("%s: an access list of one entry is wanted, got %+v", path, list)
	}

	var entry *windows.ACCESS_ALLOWED_ACE
	if err = windows.GetAce(list, 0, &entry); err != nil {
		t.Fatal(err)
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}

	if entry == nil {
		t.Fatalf("%s: its access list's one entry could not be read", path)
	}

	if entry.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		!windows.EqualSid((*windows.SID)(unsafe.Pointer(&entry.SidStart)), user.User.Sid) {
		t.Fatalf("%s: its one entry is not the current user's", path)
	}
}
