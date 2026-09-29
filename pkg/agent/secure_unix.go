//go:build !windows

package agent

import "os"

// RestrictFileToOwner makes a file readable only by its owner.
func RestrictFileToOwner(path string) error {
	return os.Chmod(path, 0o600)
}

// RestrictDirToOwner makes a directory accessible only by its owner.
func RestrictDirToOwner(path string) error {
	return os.Chmod(path, 0o700)
}

// HasOwnerOnlyAccess reports whether only the owner can read or write path.
// Windows has no POSIX mode bits, so there the check reads the file's ACL.
func HasOwnerOnlyAccess(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().Perm()&0o077 == 0
}
