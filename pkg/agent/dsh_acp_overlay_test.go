package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestEnsureAcpOverlayCreatesAndKeepsTheManagedFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, dshAcpOverlayName)

	written, err := ensureAcpOverlay(home, testLogger())
	if err != nil {
		t.Fatalf("ensureAcpOverlay: %v", err)
	}
	if written != path {
		t.Fatalf("overlay path = %q, want %q", written, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the overlay: %v", err)
	}
	if !strings.HasPrefix(string(content), dshAcpOverlayMarker) {
		t.Fatalf("overlay does not carry Solo's marker:\n%s", content)
	}
	if !strings.Contains(string(content), "- SOLO.md") || !strings.Contains(string(content), "maxBytes: 65536") {
		t.Fatalf("overlay does not list SOLO.md with the restated maxBytes:\n%s", content)
	}

	// An identical overlay is left untouched: the mtime set here must survive.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("age the overlay: %v", err)
	}
	if _, err := ensureAcpOverlay(home, testLogger()); err != nil {
		t.Fatalf("second ensureAcpOverlay: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the overlay: %v", err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("overlay was rewritten (mtime %s, want %s)", info.ModTime(), past)
	}
}

func TestEnsureAcpOverlayRefusesAForeignFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, dshAcpOverlayName)
	foreign := "# an operator's own overlay\n- id: agent-instructions\n  config:\n    maxBytes: 1024\n"
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatalf("write the foreign overlay: %v", err)
	}

	_, err := ensureAcpOverlay(home, testLogger())
	if err == nil {
		t.Fatal("expected a foreign overlay to be reported, not overwritten")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "SOLO.md") {
		t.Fatalf("error = %v, want it to name the file and the fix", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the foreign overlay: %v", err)
	}
	if string(content) != foreign {
		t.Fatalf("foreign overlay was modified:\n%s", content)
	}
}

func TestEnsureAcpOverlayRefreshesStaleManagedContent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, dshAcpOverlayName)
	stale := dshAcpOverlayMarker + "\n- id: agent-instructions\n  config:\n    maxBytes: 1024\n"
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatalf("write the stale overlay: %v", err)
	}

	if _, err := ensureAcpOverlay(home, testLogger()); err != nil {
		t.Fatalf("ensureAcpOverlay: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the refreshed overlay: %v", err)
	}
	if string(content) != dshAcpOverlayMarker+"\n"+dshAcpOverlayBody {
		t.Fatalf("overlay was not refreshed to the current body:\n%s", content)
	}
}

func TestDshAcpBackendLaunchWritesAndPassesTheOverlay(t *testing.T) {
	t.Setenv("DSH_PATCH", filepath.Join(t.TempDir(), "operator.yml"))
	home := t.TempDir()
	backend := NewDshAcpBackend(os.Args[0], testLogger())

	_, args, err := backend.resolveLaunch(&ExecuteOptions{Env: map[string]string{"DSH_HOME": home}})
	if err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	joined := strings.Join(args, " ")
	overlay := filepath.Join(home, dshAcpOverlayName)
	if !strings.Contains(joined, "--patch "+overlay) {
		t.Fatalf("args = %v, want the managed overlay passed to DSH", args)
	}
	// The operator's own overlay is applied after Solo's, so it still wins.
	if strings.Index(joined, overlay) > strings.Index(joined, "operator.yml") {
		t.Fatalf("args = %v, want the managed overlay before the operator's", args)
	}
	if _, err := os.Stat(overlay); err != nil {
		t.Fatalf("managed overlay missing after launch resolution: %v", err)
	}
}

func TestDshEnvironmentKeepsACallerSuppliedHome(t *testing.T) {
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "process-home"))

	env := dshEnvironment(map[string]string{"DSH_HOME": "/caller-home"})
	if env["DSH_HOME"] != "/caller-home" {
		t.Fatalf("DSH_HOME = %q, want the caller's value to win over the process environment", env["DSH_HOME"])
	}
	inherited := dshEnvironment(nil)
	if inherited["DSH_HOME"] != "" {
		t.Fatalf("DSH_HOME = %q, want the inherited environment to be left to the child", inherited["DSH_HOME"])
	}
	if inherited["DSH_PERMISSION_MODE"] != dshDefaultPermissionMode {
		t.Fatalf("DSH_PERMISSION_MODE = %q, want the unattended default", inherited["DSH_PERMISSION_MODE"])
	}
}
