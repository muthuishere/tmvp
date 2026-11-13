//go:build windows

package secure

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// applyOwnerOnly replaces path's DACL with a protected one (no inheritance
// from the parent) granting full access to the current user and SYSTEM
// only, inherited by children (OICI) — the Windows analogue of 0700/0600.
func applyOwnerOnly(path string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user SID: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)", tu.User.Sid.String()))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
