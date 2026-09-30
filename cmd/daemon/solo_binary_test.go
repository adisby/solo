package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSoloBinaryFileNamesCoverPlatformExecutable pins the Windows regression
// that made every task log "solo binary not found" while the CLI sat next to the
// daemon: the lookup only tried the extensionless name, which os.Stat cannot
// resolve to solo.exe.
func TestSoloBinaryFileNamesCoverPlatformExecutable(t *testing.T) {
	names := soloBinaryFileNames()
	if len(names) == 0 {
		t.Fatal("expected at least one candidate file name")
	}
	if runtime.GOOS != "windows" {
		if names[0] != "solo" {
			t.Fatalf("first candidate = %q, want solo", names[0])
		}
		return
	}
	if names[0] != "solo.exe" {
		t.Fatalf("first candidate = %q, want solo.exe (the only runnable name on Windows)", names[0])
	}
	if names[len(names)-1] != "solo" {
		t.Fatalf("last candidate = %q, want the extensionless fallback", names[len(names)-1])
	}
}

// TestSoloWorkspaceFileNameMatchesPlatformExecutable pins the other half of the
// Windows bug: even a resolved CLI was copied into the workspace without an
// extension, where Windows refuses to run it.
func TestSoloWorkspaceFileNameMatchesPlatformExecutable(t *testing.T) {
	want := "solo"
	if runtime.GOOS == "windows" {
		want = "solo.exe"
	}
	if got := soloWorkspaceFileName(); got != want {
		t.Fatalf("soloWorkspaceFileName() = %q, want %q", got, want)
	}
}

// TestRemoveStaleSoloWorkspaceCopyClearsExtensionlessLeftover covers upgrading a
// workspace that an older daemon populated with the unrunnable extensionless copy.
func TestRemoveStaleSoloWorkspaceCopyClearsExtensionlessLeftover(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "solo")
	if err := os.WriteFile(stale, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, soloWorkspaceFileName())
	if err := os.WriteFile(current, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}

	removeStaleSoloWorkspaceCopy(dir)

	if _, err := os.Stat(current); err != nil {
		t.Fatalf("the current copy must survive: %v", err)
	}
	_, err := os.Stat(stale)
	if runtime.GOOS == "windows" {
		if !os.IsNotExist(err) {
			t.Fatalf("stale extensionless copy should be removed on Windows, stat err = %v", err)
		}
		return
	}
	// On POSIX the extensionless name IS the injected copy, so it must remain.
	if err != nil {
		t.Fatalf("POSIX copy must survive: %v", err)
	}
}

func TestResolveSoloBinaryUsesConfiguredExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "solo")
	if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOLO_CLI_BIN", path)

	if got := resolveSoloBinary(); got != path {
		t.Fatalf("resolveSoloBinary() = %q, want %q", got, path)
	}
}

func TestResolveSoloBinaryPrefersBuildCompanionOverInstalledPATH(t *testing.T) {
	root := t.TempDir()
	companion := filepath.Join(root, ".pids", "solo")
	if err := os.MkdirAll(filepath.Dir(companion), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(companion, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	installedDir := filepath.Join(root, "installed")
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installedDir, "solo"), []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	t.Setenv("PATH", installedDir)
	t.Setenv("SOLO_CLI_BIN", "")

	got := resolveSoloBinary()
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	companionInfo, err := os.Stat(companion)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, companionInfo) {
		t.Fatalf("resolveSoloBinary() = %q, want build companion %q", got, companion)
	}
}
