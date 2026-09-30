package agent

import (
	"os"
	"strings"
	"testing"
)

// pathEntries collects every PATH-like entry, mirroring how Windows matches the
// name case-insensitively. A count above one means buildEnvAt appended a
// duplicate instead of rewriting the inherited variable.
func pathEntries(env []string) []string {
	var found []string
	for _, e := range env {
		if pathEnvKey(e) {
			found = append(found, e)
		}
	}
	return found
}

func childPathFromEnv(t *testing.T, env []string) string {
	t.Helper()
	entries := pathEntries(env)
	if len(entries) == 0 {
		t.Fatal("buildEnvAt produced no PATH entry")
	}
	if len(entries) > 1 {
		t.Fatalf("buildEnvAt produced %d PATH entries: %q", len(entries), entries)
	}
	_, value, _ := strings.Cut(entries[0], "=")
	return value
}

// TestBuildEnvAtPrefixesWorkspaceToPath pins the workspace-first PATH contract
// that lets an Agent run the injected Solo CLI by name.
func TestBuildEnvAtPrefixesWorkspaceToPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	workspace := "/tmp/ws"

	got := childPathFromEnv(t, buildEnvAt(workspace, nil))

	want := workspace + string(os.PathListSeparator) + "/usr/bin"
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}

// TestBuildEnvAtUsesPlatformListSeparator is the regression for the Windows PATH
// bug: buildEnvAt hardcoded ":", so on Windows the entry became
// "<ws>:<ws>;C:\Windows;..." where the drive letter's colon terminated the first
// component and the child could no longer resolve node or the workspace CLI.
func TestBuildEnvAtUsesPlatformListSeparator(t *testing.T) {
	t.Setenv("PATH", `C:\Windows;C:\Program Files\nodejs`)
	workspace := `C:\ws`

	got := childPathFromEnv(t, buildEnvAt(workspace, nil))

	want := workspace + string(os.PathListSeparator) + `C:\Windows;C:\Program Files\nodejs`
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	// The inherited search path must survive intact after the workspace entry.
	if !strings.Contains(got, `C:\Program Files\nodejs`) {
		t.Fatalf("inherited PATH entry lost: %q", got)
	}
}

// TestBuildEnvAtRewritesWindowsSpelledPath is the regression for the second
// Windows trap: the inherited variable is spelled `Path`, so the original
// case-sensitive `PATH=` prefix test missed it and appended a separate PATH,
// leaving the child with two competing variables (the Agent observed
// "<ws>:<ws>;C:\Program Files\..."). Exactly one PATH entry may come back.
func TestBuildEnvAtRewritesWindowsSpelledPath(t *testing.T) {
	// Drop every spelling of the variable so the case under test is the only one.
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if strings.EqualFold(key, "PATH") {
			if err := os.Unsetenv(key); err != nil {
				t.Fatalf("unset %s: %v", key, err)
			}
		}
	}
	// Windows normally spells it Path; a drive letter also exercises the
	// separator, since its own colon would masquerade as a list separator.
	if err := os.Setenv("Path", `C:\Windows;C:\Program Files\nodejs`); err != nil {
		t.Fatalf("set Path: %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("Path") })

	workspace := `C:\ws`
	entries := pathEntries(buildEnvAt(workspace, nil))

	if len(entries) != 1 {
		t.Fatalf("expected exactly one PATH entry, got %d: %q", len(entries), entries)
	}
	_, value, _ := strings.Cut(entries[0], "=")
	want := workspace + string(os.PathListSeparator) + `C:\Windows;C:\Program Files\nodejs`
	if value != want {
		t.Fatalf("PATH = %q, want %q", value, want)
	}
	// The inherited search path must survive; a duplicate PATH would leave the
	// child resolving against a variable the OS may not have chosen.
	if !strings.Contains(value, `C:\Program Files\nodejs`) {
		t.Fatalf("inherited PATH entry lost: %q", value)
	}
}

// TestBuildEnvAtDefaultsToCurrentDirectory covers the no-workspace case used by
// backends that run without a prepared workspace.
func TestBuildEnvAtDefaultsToCurrentDirectory(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")

	got := childPathFromEnv(t, buildEnvAt("", nil))

	if !strings.HasPrefix(got, "./"+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want a .%c prefix", got, os.PathListSeparator)
	}
}
