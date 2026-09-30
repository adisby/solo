package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dshTestBackend points a DshBackend at the fake runtime in dsh_helper_test.go.
func dshTestBackend(t *testing.T) *DshBackend {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	t.Setenv("SOLO_TEST_DSH_HELPER", "1")
	return &DshBackend{
		executablePath: self,
		// Re-execute this binary as the fixture runtime instead of booting DSH.
		launchArgs: []string{"-test.run=TestDshHelperProcess"},
		logger:     logOrDefault(nil),
	}
}

// collectDshTurn drains one turn and returns its chunks and result.
//
// The delivery contract closes Messages before Result is sent, but Result may be
// observed first because it is written to its channel before the message channel
// is closed. A real consumer therefore has to drain the buffered messages after
// seeing the result; this helper does the same so it cannot lose output.
func collectDshTurn(t *testing.T, messages <-chan OutputChunk, results <-chan *Result) ([]OutputChunk, *Result) {
	t.Helper()
	var chunks []OutputChunk
	var result *Result
	messagesOpen, resultsOpen := true, true
	deadline := time.After(30 * time.Second)

	for (messagesOpen || resultsOpen) && result == nil {
		select {
		case chunk, ok := <-messages:
			if !ok {
				messagesOpen = false
				continue
			}
			chunks = append(chunks, chunk)
		case got, ok := <-results:
			if !ok {
				resultsOpen = false
				continue
			}
			result = got
		case <-deadline:
			t.Fatalf("timed out waiting for the turn to finish (chunks so far: %d)", len(chunks))
		}
	}

	// Drain whatever the producer buffered before it closed the message channel.
	for messagesOpen {
		select {
		case chunk, ok := <-messages:
			if !ok {
				messagesOpen = false
				continue
			}
			chunks = append(chunks, chunk)
		default:
			messagesOpen = false
		}
	}

	if result == nil {
		t.Fatal("turn channels closed without a result")
	}
	return chunks, result
}

// TestDshBackendExecuteMapsStreamedChunks covers the DSH -> Solo output mapping:
// reasoning becomes thinking, a tool-call delta becomes tool_use, the completed
// text block becomes text, and turn usage reaches the result.
func TestDshBackendExecuteMapsStreamedChunks(t *testing.T) {
	backend := dshTestBackend(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, &ExecuteRequest{
		AgentID:  "dsh-unit",
		Messages: []Message{{Role: "user", Content: "hello harness"}},
	}, &ExecuteOptions{WorkspaceDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if session.SessionID == "" {
		t.Fatal("Execute returned an empty SessionID")
	}

	chunks, result := collectDshTurn(t, session.Messages, session.Result)

	if result.Status != "completed" {
		t.Fatalf("status = %q (error=%q), want completed", result.Status, result.Error)
	}

	var thinking, text strings.Builder
	toolUse, statuses := 0, map[string]bool{}
	for _, chunk := range chunks {
		switch chunk.Type {
		case string(MessageThinking):
			thinking.WriteString(chunk.Content)
		case string(MessageText):
			text.WriteString(chunk.Content)
		case string(MessageToolUse):
			toolUse++
			if chunk.Tool == nil || chunk.Tool.Name != "read" {
				t.Fatalf("tool_use chunk missing tool info: %+v", chunk.Tool)
			}
			if chunk.Tool.CallID != "call-1" {
				t.Fatalf("tool call id = %q, want call-1", chunk.Tool.CallID)
			}
		case string(MessageStatus):
			statuses[chunk.Content] = true
		}
	}

	if !strings.Contains(thinking.String(), "thinking...") {
		t.Fatalf("reasoning-delta was not mapped to thinking: %q", thinking.String())
	}
	if !strings.Contains(text.String(), "prompt=User: hello harness") {
		t.Fatalf("text block was not mapped to text: %q", text.String())
	}
	if toolUse != 1 {
		t.Fatalf("tool_use chunks = %d, want 1", toolUse)
	}
	if !statuses["running"] {
		t.Fatalf("the running status transition was not surfaced: %v", statuses)
	}
	if statuses["idle"] {
		t.Fatalf("the resting idle status must not be reported as turn output: %v", statuses)
	}

	usage, ok := result.Usage["deepseek-flash"]
	if !ok {
		t.Fatalf("usage missing for the configured model: %v", result.Usage)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v, want input=11 output=7", usage)
	}
}

// TestDshBackendPersistentTurnsReuseSession covers the persistent path: a second
// prompt must reuse the same DSH session id and carry the new prompt text, which
// is what makes conversation memory and resume work without a resume protocol.
func TestDshBackendPersistentTurnsReuseSession(t *testing.T) {
	backend := dshTestBackend(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts := &ExecuteOptions{WorkspaceDir: t.TempDir()}
	first, err := backend.Start(ctx, &ExecuteRequest{
		AgentID:  "dsh-unit",
		Messages: []Message{{Role: "user", Content: "first turn"}},
	}, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, result := collectDshTurn(t, first.Messages, first.Result); result.Status != "completed" {
		t.Fatalf("first turn status = %q (error=%q)", result.Status, result.Error)
	}

	state, ok := first.state.(*dshPersistentState)
	if !ok || state == nil {
		t.Fatal("persistent session state is not a dshPersistentState")
	}
	if !state.IsAlive() {
		t.Fatal("session process is not alive after the first turn")
	}
	if state.SessionID() != first.SessionID {
		t.Fatalf("SessionStater id = %q, want %q", state.SessionID(), first.SessionID)
	}

	second, err := backend.Send(ctx, first, []Message{{Role: "user", Content: "second turn"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("Send changed the session id: %q -> %q", first.SessionID, second.SessionID)
	}

	chunks, result := collectDshTurn(t, second.Messages, second.Result)
	if result.Status != "completed" {
		t.Fatalf("second turn status = %q (error=%q)", result.Status, result.Error)
	}
	var text strings.Builder
	for _, chunk := range chunks {
		if chunk.Type == string(MessageText) {
			text.WriteString(chunk.Content)
		}
	}
	if !strings.Contains(text.String(), "prompt=[user]: second turn") {
		t.Fatalf("second turn did not carry its own prompt: %q", text.String())
	}
	// The fixture counts turns per session, so turn=2 proves the second prompt
	// continued the same DSH conversation instead of creating a new one.
	if !strings.Contains(text.String(), "turn=2") {
		t.Fatalf("second turn did not continue the session history: %q", text.String())
	}
	if !strings.Contains(text.String(), "session="+first.SessionID) {
		t.Fatalf("second turn ran on a different DSH session: %q", text.String())
	}

	if err := backend.Close(second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if state.IsAlive() {
		t.Fatal("session still reports alive after Close")
	}
}

// TestDshBackendFreshStartCreatesANewSession documents the resume boundary: the
// adapter starts a new DSH session per process, because DSH mints the session id
// from the client and Start always generates a fresh one. Continuing a
// conversation across processes needs a stored session id to be fed back into
// initialize/session-prompt, which the adapter does not do yet. Within a process,
// Start+Send reuse one session, which is what the persistent capability covers.
func TestDshBackendFreshStartCreatesANewSession(t *testing.T) {
	t.Setenv("SOLO_TEST_DSH_STATE", filepath.Join(t.TempDir(), "fake-dsh-state.json"))
	backend := dshTestBackend(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := &ExecuteOptions{WorkspaceDir: t.TempDir()}

	start := func(prompt string) (string, *Result) {
		t.Helper()
		session, err := backend.Start(ctx, &ExecuteRequest{
			AgentID:  "dsh-unit",
			Messages: []Message{{Role: "user", Content: prompt}},
		}, opts)
		if err != nil {
			t.Fatalf("Start(%q): %v", prompt, err)
		}
		_, result := collectDshTurn(t, session.Messages, session.Result)
		if result.Status != "completed" {
			t.Fatalf("turn %q status = %q (error=%q)", prompt, result.Status, result.Error)
		}
		if err := backend.ForceClose(session); err != nil {
			t.Fatalf("ForceClose: %v", err)
		}
		return session.SessionID, result
	}

	firstID, _ := start("first process")
	secondID, _ := start("second process")
	if firstID == secondID {
		t.Fatalf("two processes reused session id %q; DSH mints per-client ids", firstID)
	}
}

// TestDshDetectionHonoursDSHBin covers local runtime detection: an operator who
// points the adapter at an explicit launcher must see DSH as available even
// though `dsh` is not on PATH, which is the common case for a source checkout.
func TestDshDetectionHonoursDSHBin(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	t.Setenv("DSH_BIN", self)

	var found *BackendStatus
	for _, status := range GlobalRegistry().Detect() {
		if status.Type == "dsh" {
			candidate := status
			found = &candidate
			break
		}
	}
	if found == nil {
		t.Fatal("the dsh adapter is not registered")
	}
	if !found.Available {
		t.Fatalf("dsh reported unavailable with DSH_BIN set: %s", found.Error)
	}
	if found.Binary != self {
		t.Fatalf("dsh binary = %q, want the DSH_BIN path %q", found.Binary, self)
	}
}

// TestDshDetectionRunsAScriptLauncherThroughNode covers the checkout case: a .js
// launcher must be probed through node, so the reported binary is the interpreter
// rather than an unrunnable script.
func TestDshDetectionRunsAScriptLauncherThroughNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node is not on PATH: %v", err)
	}
	t.Setenv("DSH_BIN", filepath.Join(t.TempDir(), "bin.js"))

	var found *BackendStatus
	for _, status := range GlobalRegistry().Detect() {
		if status.Type == "dsh" {
			candidate := status
			found = &candidate
			break
		}
	}
	if found == nil {
		t.Fatal("the dsh adapter is not registered")
	}
	if !found.Available {
		t.Fatalf("a .js launcher was reported unavailable: %s", found.Error)
	}
	if !strings.Contains(found.Binary, "node") {
		t.Fatalf("dsh binary = %q, want the node interpreter", found.Binary)
	}
}

// TestDshLaunchSelectsNodeForAScriptEntryPoint covers the launcher contract:
// a .js entry point runs through node and carries the sdk profile, and the
// permission mode is never passed as a flag (the launcher rejects it).
func TestDshLaunchSelectsNodeForAScriptEntryPoint(t *testing.T) {
	execPath, args, err := dshLaunch("/opt/dsh/bin/dsh.js")
	if err != nil {
		t.Skipf("node is not on PATH: %v", err)
	}
	if !strings.HasSuffix(execPath, "node") && !strings.HasSuffix(execPath, "node.exe") {
		t.Fatalf("execPath = %q, want the node binary", execPath)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--profile sdk") {
		t.Fatalf("args = %q, want the sdk profile", joined)
	}
	if strings.Contains(joined, "permission-mode") {
		t.Fatalf("args = %q, but the launcher rejects --permission-mode", joined)
	}
}

// TestDshEnvironmentDefaultsPermissionMode guards the unattended default: the
// Daemon must not block on an approval prompt, and a caller value must win.
func TestDshEnvironmentDefaultsPermissionMode(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())

	env := dshEnvironment(nil)
	if env["DSH_PERMISSION_MODE"] != dshDefaultPermissionMode {
		t.Fatalf("permission mode = %q, want %q", env["DSH_PERMISSION_MODE"], dshDefaultPermissionMode)
	}

	env = dshEnvironment(map[string]string{"DSH_PERMISSION_MODE": "workspace-write"})
	if env["DSH_PERMISSION_MODE"] != "workspace-write" {
		t.Fatalf("caller permission mode was overwritten: %q", env["DSH_PERMISSION_MODE"])
	}
}
