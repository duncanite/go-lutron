//go:build windows

package cli

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// private makes the file at path readable by its owner alone. Windows
// keeps one bit of a file's mode, read-only; who may read it is its
// access list. This gives the file a list of one entry, the current user,
// and cuts it off from the directory's. It is called on the file while
// it is still empty, and a rename keeps a file's own list.
func private(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("finding the current user: %w", err)
	}

	list, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("making the access list for %s: %w", path, err)
	}

	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, list, nil,
	)
	if err != nil {
		return fmt.Errorf("restricting %s to its owner: %w", path, err)
	}

	return nil
}
