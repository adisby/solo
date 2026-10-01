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

// dshAcpE2EInstructionMarker is the codeword the instruction check asks the model
// to answer with, proving the managed overlay made DSH load the workspace
// instructions file.
const dshAcpE2EInstructionMarker = "OVERLAY-LOADED"

// dshAcpE2ETarget resolves the real DSH launcher and Harness home both
// end-to-end tests need, skipping unless they are explicitly enabled.
func dshAcpE2ETarget(t *testing.T) (bin, home string) {
	t.Helper()
	if os.Getenv("SOLO_E2E_DSH") != "1" {
		t.Skip("set SOLO_E2E_DSH=1 to run the real DSH ACP end-to-end tests")
	}
	bin = strings.TrimSpace(os.Getenv("DSH_BIN"))
	if bin == "" {
		t.Fatal("DSH_BIN must point at the dsh launcher for these tests")
	}
	home = strings.TrimSpace(os.Getenv("DSH_HOME"))
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve the Harness home: %v", err)
		}
		home = filepath.Join(resolved, ".dsh")
	}
	return bin, home
}

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
	bin, home := dshAcpE2ETarget(t)
	expectUsage := os.Getenv("SOLO_E2E_DSH_ALLOW_NO_USAGE") != "1"

	backend := NewDshAcpBackend(bin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	workspace := t.TempDir()

	first, err := backend.Start(ctx, &ExecuteRequest{
		AgentID:  "agent-e2e",
		Messages: []Message{{Role: RoleUser, Content: "Reply with exactly: pong"}},
	}, &ExecuteOptions{WorkspaceDir: workspace, SystemPrompt: dshAcpE2EInstructionMarker})
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

// TestDshAcpE2ELoadsTheWorkspaceInstructions proves the managed overlay carries
// Solo's system prompt into a real agent: the workspace instructions file tells
// the model a codeword, and the codeword can only come back if DSH loaded that
// file, which the profile does only because the overlay names it.
func TestDshAcpE2ELoadsTheWorkspaceInstructions(t *testing.T) {
	bin, _ := dshAcpE2ETarget(t)

	backend := NewDshAcpBackend(bin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ps, err := backend.Start(ctx, &ExecuteRequest{
		AgentID: "agent-instructions-e2e",
		Messages: []Message{{
			Role:    RoleUser,
			Content: "What is the codeword? Reply with the codeword only.",
		}},
	}, &ExecuteOptions{
		WorkspaceDir: t.TempDir(),
		SystemPrompt: "You are a test agent. When the user asks for the codeword, reply with exactly " +
			dshAcpE2EInstructionMarker + " and nothing else.",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Close(ps)

	_, result := readDshAcpTurn(t, ps)
	if result.Status != "completed" {
		t.Fatalf("result = %+v, want a completed turn", result)
	}
	if !strings.Contains(result.Output, dshAcpE2EInstructionMarker) {
		t.Fatalf("answer = %q, want it to contain %q, which only the workspace instructions carry",
			result.Output, dshAcpE2EInstructionMarker)
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
