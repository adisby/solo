package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestrictFileToOwnerDropsInheritedAccess covers the Windows gap that made
// os.Chmod useless for the paired Computer credential: after hardening, the
// file must no longer carry the inherited grants that let every local account
// read it, and it must still grant the owning account full control.
func TestRestrictFileToOwnerDropsInheritedAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"credential":"secret"}`), 0o600); err != nil {
		t.Fatalf("write credential fixture: %v", err)
	}

	before := icaclsOutput(t, path)
	if !strings.Contains(before, "(I)") {
		t.Skip("fixture has no inherited ACEs to drop")
	}

	if err := RestrictFileToOwner(path); err != nil {
		t.Fatalf("RestrictFileToOwner: %v", err)
	}

	if !HasOwnerOnlyAccess(path) {
		t.Fatalf("HasOwnerOnlyAccess reported a readable credential:\n%s", icaclsOutput(t, path))
	}
	after := icaclsOutput(t, path)
	if strings.Contains(after, "(I)") {
		t.Fatalf("inherited ACEs survived hardening:\n%s", after)
	}
	if !strings.Contains(after, "OWNER RIGHTS:(F)") {
		t.Fatalf("owner no longer has full control:\n%s", after)
	}
	if !strings.Contains(after, "NT AUTHORITY\\SYSTEM:(F)") {
		t.Fatalf("SYSTEM lost full control:\n%s", after)
	}
	if strings.Contains(after, "BUILTIN\\Users") {
		t.Fatalf("every local account can still read the credential:\n%s", after)
	}
}

// TestRestrictDirToOwnerAppliesToChildren checks the directory form used for
// the Daemon state directory.
func TestRestrictDirToOwnerAppliesToChildren(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "daemon")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create fixture dir: %v", err)
	}
	if err := RestrictDirToOwner(dir); err != nil {
		t.Fatalf("RestrictDirToOwner: %v", err)
	}
	if !HasOwnerOnlyAccess(dir) {
		t.Fatalf("HasOwnerOnlyAccess reported a readable directory:\n%s", icaclsOutput(t, dir))
	}
	output := icaclsOutput(t, dir)
	if strings.Contains(output, "(I)") {
		t.Fatalf("inherited ACEs survived hardening:\n%s", output)
	}
	for _, want := range []string{"(OI)", "(CI)", "OWNER RIGHTS:(OI)(CI)(F)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("directory ACL missing %q:\n%s", want, output)
		}
	}
}

func icaclsOutput(t *testing.T, path string) string {
	t.Helper()
	output, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v: %s", path, err, output)
	}
	return string(output)
}
