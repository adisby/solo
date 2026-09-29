package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// icaclsTimeout bounds the ACL hardening call so a stalled icacls cannot block
// pairing or Daemon startup.
const icaclsTimeout = 10 * time.Second

// RestrictFileToOwner rewrites the file's ACL to grant only the current user,
// SYSTEM, and Administrators, and to stop inheriting from the parent.
//
// Windows has no chmod: os.Chmod only toggles the read-only attribute, so
// without this the paired Computer credential would stay readable by every
// account on the machine. icacls ships with Windows and is the same class of
// system tool as the taskkill call in code_gate_process_windows.go.
func RestrictFileToOwner(path string) error {
	return runIcacls(path, false)
}

// RestrictDirToOwner applies the same owner-only ACL to a directory.
func RestrictDirToOwner(path string) error {
	return runIcacls(path, true)
}

// HasOwnerOnlyAccess reports whether only the owning account, SYSTEM, and
// Administrators can read path. Windows has no POSIX mode bits, so this reads
// the file's ACL instead.
func HasOwnerOnlyAccess(path string) bool {
	output, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		return false
	}
	text := string(output)
	// An inherited entry means some parent grant still applies, which is what
	// left the credential readable by every local account before hardening.
	if strings.Contains(text, "(I)") {
		return false
	}
	// Files read "OWNER RIGHTS:(F)", directories "OWNER RIGHTS:(OI)(CI)(F)".
	if !strings.Contains(text, "OWNER RIGHTS:") {
		return false
	}
	for _, broad := range []string{"BUILTIN\\Users", "Everyone", "AUTHENTICATED USERS", "Authenticated Users"} {
		if strings.Contains(text, broad) {
			return false
		}
	}
	return true
}

func runIcacls(path string, directory bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), icaclsTimeout)
	defer cancel()

	// The inheritance flags belong to each grant specification; passing
	// "(OI)(CI)" as its own argument is rejected with exit code 87.
	suffix := ""
	if directory {
		suffix = "(OI)(CI)"
	}
	// SIDs keep the grants independent of the system language.
	grants := []string{
		"*S-1-3-4:" + suffix + "(F)",      // OWNER RIGHTS
		"*S-1-5-18:" + suffix + "(F)",     // LOCAL SYSTEM
		"*S-1-5-32-544:" + suffix + "(F)", // BUILTIN\Administrators
	}
	args := append([]string{path, "/inheritance:r", "/grant:r"}, grants...)

	output, err := exec.CommandContext(ctx, "icacls", args...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("restrict %s: icacls timed out", path)
		}
		return fmt.Errorf("restrict %s: icacls: %w: %s", path, err, trimOutput(output))
	}
	return nil
}

func trimOutput(output []byte) string {
	const limit = 512
	if len(output) > limit {
		return string(output[:limit])
	}
	return string(output)
}
