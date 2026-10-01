package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDshAcpE2EResumesOneSessionAcrossProcesses drives a real `dsh --profile acp`
// process over the ACP transport and proves the regression that transport exists
// to fix: a second process restores the stored session instead of creating
// another one, and the session keeps exactly one durable artifact.
//
// It spends model tokens, needs a real DSH install with credentials, and needs a
// DSH whose ACP profile returns turn usage, so it runs only when explicitly
// enabled:
//
//	SOLO_E2E_DSH=1 DSH_BIN=/path/to/dsh/lib/bin.js go test ./pkg/agent/ \
//	  -run TestDshAcpE2EResumesOneSessionAcrossProcesses -v
//
// SOLO_E2E_DSH_ALLOW_NO_USAGE=1 relaxes the usage assertion for a DSH build that
// does not report it yet.
func TestDshAcpE2EResumesOneSessionAcrossProcesses(t *testing.T) {
	if os.Getenv("SOLO_E2E_DSH") != "1" {
		t.Skip("set SOLO_E2E_DSH=1 to run the real DSH ACP end-to-end test")
	}
	bin := strings.TrimSpace(os.Getenv("DSH_BIN"))
	if bin == "" {
		t.Fatal("DSH_BIN must point at the dsh launcher for this test")
	}
	home := strings.TrimSpace(os.Getenv("DSH_HOME"))
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve the Harness home: %v", err)
		}
		home = filepath.Join(resolved, ".dsh")
	}
	expectUsage := os.Getenv("SOLO_E2E_DSH_ALLOW_NO_USAGE") != "1"

	backend := NewDshAcpBackend(bin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	workspace := t.TempDir()

	first, err := backend.Start(ctx, &ExecuteRequest{
		AgentID:  "agent-e2e",
		Messages: []Message{{Role: RoleUser, Content: "Reply with exactly: pong"}},
	}, &ExecuteOptions{WorkspaceDir: workspace})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	firstSession := first.SessionID
	if firstSession == "" {
		t.Fatal("session/new reported no session id")
	}
	_, firstResult := readDshAcpTurn(t, first)
	if firstResult.Status != "completed" {
		t.Fatalf("first turn = %+v, want completed", firstResult)
	}
	if expectUsage && len(firstResult.Usage) == 0 {
		t.Fatalf("first turn reported no usage; the DSH install must return usage from session/prompt " +
			"(set SOLO_E2E_DSH_ALLOW_NO_USAGE=1 to accept a DSH build without it)")
	}
	if err := backend.Close(first); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	artifacts := dshSessionArtifacts(t, home, firstSession)
	if len(artifacts) != 1 {
		t.Fatalf("session artifacts = %v, want exactly one for %s", artifacts, firstSession)
	}

	// A second process must restore that session rather than create another.
	second, err := backend.Start(ctx, &ExecuteRequest{
		AgentID:  "agent-e2e",
		Messages: []Message{{Role: RoleUser, Content: "Reply with exactly: pong again"}},
	}, &ExecuteOptions{WorkspaceDir: workspace, ResumeSessionID: firstSession})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer backend.Close(second)
	if second.SessionID != firstSession {
		t.Fatalf("second session = %q, want the restored %q", second.SessionID, firstSession)
	}
	_, secondResult := readDshAcpTurn(t, second)
	if secondResult.Status != "completed" {
		t.Fatalf("second turn = %+v, want completed", secondResult)
	}

	if resumed := dshSessionArtifacts(t, home, firstSession); len(resumed) != 1 {
		t.Fatalf("session artifacts after resume = %v, want the same single artifact", resumed)
	}
}

// dshSessionArtifacts lists the durable log files of one DSH session. The
// session directory also holds the writer's lock file, which is not an artifact
// of the conversation, so only the session log generations are counted.
func dshSessionArtifacts(t *testing.T, home, sessionID string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", sessionID, "*"))
	if err != nil {
		t.Fatalf("glob the session directory: %v", err)
	}
	files := make([]string, 0, len(matches))
	for _, match := range matches {
		if !strings.HasPrefix(filepath.Base(match), "session.v") {
			continue
		}
		info, err := os.Stat(match)
		if err != nil || info.IsDir() {
			continue
		}
		files = append(files, match)
	}
	if len(files) == 0 {
		t.Fatalf("no session artifacts under %s for %s", home, sessionID)
	}
	return files
}
