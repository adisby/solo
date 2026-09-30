package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResolveInternalToken(t *testing.T) {
	t.Run("uses dedicated internal token", func(t *testing.T) {
		if got := resolveInternalToken("internal-secret", "jwt-secret"); got != "internal-secret" {
			t.Fatalf("resolveInternalToken() = %q, want internal-secret", got)
		}
	})

	t.Run("falls back to jwt secret for backward compatibility", func(t *testing.T) {
		if got := resolveInternalToken("", "jwt-secret"); got != "jwt-secret" {
			t.Fatalf("resolveInternalToken() = %q, want jwt-secret", got)
		}
	})
}

func TestDaemonDeclaresContextRolloverCapability(t *testing.T) {
	want := contextRolloverCapability
	for _, capability := range daemonCapabilities() {
		if capability == want {
			return
		}
	}
	t.Fatalf("daemon capabilities %v do not include %q", daemonCapabilities(), want)
}

func TestDaemonPortRecordOnlyRemovesItsOwnPort(t *testing.T) {
	dir := t.TempDir()
	path, err := writeDaemonPortRecord(dir, os.Getpid(), 8081)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "8081" {
		t.Fatalf("port record = %q, want 8081", got)
	}

	// A record naming another port belongs to another Daemon.
	if err := os.WriteFile(path, []byte("49443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDaemonPortRecord(path, 8081); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("another Daemon's port record was removed: %v", err)
	}

	if _, err := writeDaemonPortRecord(dir, os.Getpid(), 8081); err != nil {
		t.Fatal(err)
	}
	if err := removeDaemonPortRecord(path, 8081); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("own port record still exists: %v", err)
	}

	// A record that was never written must not turn shutdown into an error.
	if err := removeDaemonPortRecord("", 8081); err != nil {
		t.Fatalf("removeDaemonPortRecord with no path: %v", err)
	}
}

func TestDaemonProcessRecordOnlyRemovesItsOwnPID(t *testing.T) {
	dir := t.TempDir()
	path, err := writeDaemonProcessRecord(dir, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid()+1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDaemonProcessRecord(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("another process record was removed: %v", err)
	}
	if _, err := writeDaemonProcessRecord(dir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := removeDaemonProcessRecord(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.pid")); !os.IsNotExist(err) {
		t.Fatalf("own process record still exists: %v", err)
	}
}
