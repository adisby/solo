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

// newFakeDshAcpBackend points the backend at this test binary, which
// TestDshAcpHelperProcess turns into a scripted DSH ACP agent.
func newFakeDshAcpBackend(t *testing.T) *DshAcpBackend {
	t.Helper()
	backend := NewDshAcpBackend(os.Args[0], slog.New(slog.NewTextHandler(io.Discard, nil)))
	backend.launchArgs = []string{"-test.run=TestDshAcpHelperProcess"}
	return backend
}

// readDshAcpTurn drains one turn's chunks and returns its result.
func readDshAcpTurn(t *testing.T, ps *PersistentSession) ([]OutputChunk, *Result) {
	t.Helper()
	var chunks []OutputChunk
	for chunk := range ps.Messages {
		chunks = append(chunks, chunk)
	}
	select {
	case result := <-ps.Result:
		if result == nil {
			t.Fatal("turn result was nil")
		}
		return chunks, result
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the turn result")
		return nil, nil
	}
}

func chunkText(chunks []OutputChunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		if chunk.Type == string(MessageText) {
			builder.WriteString(chunk.Content)
		}
	}
	return builder.String()
}

func TestDshAcpBackendStartStreamsTurnAndReportsUsage(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	workspace := t.TempDir()
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ps, err := backend.Start(ctx, &ExecuteRequest{
		AgentID:  "agent-1",
		Messages: []Message{{Role: RoleUser, Content: "first"}},
	}, &ExecuteOptions{
		WorkspaceDir: workspace,
		SystemPrompt: "solo operating contract",
		Env:          fakeDshAcpEnv(statePath, nil),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Close(ps)

	if ps.SessionID != "session-acp-1" {
		t.Fatalf("SessionID = %q, want the id session/new returned", ps.SessionID)
	}
	instructions, err := os.ReadFile(filepath.Join(workspace, dshInstructionsFileName))
	if err != nil || string(instructions) != "solo operating contract" {
		t.Fatalf("workspace instructions = %q (err %v), want the system prompt", instructions, err)
	}

	chunks, result := readDshAcpTurn(t, ps)
	if result.Status != "completed" || result.Error != "" {
		t.Fatalf("result = %+v, want a completed turn", result)
	}
	if text := chunkText(chunks); !strings.Contains(text, "echo: ") || !strings.Contains(text, "first") {
		t.Fatalf("text chunks = %q, want the committed assistant message", text)
	}
	var sawToolUse, sawToolResult, sawContext bool
	for _, chunk := range chunks {
		switch chunk.Type {
		case string(MessageToolUse):
			sawToolUse = chunk.Tool != nil && chunk.Tool.Name == "read_file"
		case string(MessageToolResult):
			sawToolResult = chunk.Tool != nil && chunk.Tool.Output == "file body"
		case string(MessageContext):
			sawContext = chunk.Context != nil && chunk.Context.UsedTokens != nil && *chunk.Context.UsedTokens == 900
		}
	}
	if !sawToolUse || !sawToolResult || !sawContext {
		t.Fatalf("chunks = %+v, want tool_use, tool_result and context output", chunks)
	}

	usage, ok := result.Usage["test-model"]
	if !ok || usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.CacheReadTokens != 2 {
		t.Fatalf("result.Usage = %+v, want the turn's counters under the session's current model", result.Usage)
	}

	record := readFakeDshAcpRecord(statePath)
	if countFakeDshAcpMethod(record, "initialize") != 1 || countFakeDshAcpMethod(record, "session/new") != 1 {
		t.Fatalf("methods = %v, want one initialize and one session/new", record.Methods)
	}
	if record.Prompts != 1 {
		t.Fatalf("prompts = %d, want 1", record.Prompts)
	}
}

func TestDshAcpBackendResumesTheStoredSessionAcrossProcesses(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := func() *ExecuteOptions {
		return &ExecuteOptions{WorkspaceDir: t.TempDir(), Env: fakeDshAcpEnv(statePath, nil)}
	}

	first, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "first"}}}, opts())
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, result := readDshAcpTurn(t, first); result.Status != "completed" {
		t.Fatalf("first result = %+v, want completed", result)
	}
	firstSession := first.SessionID
	if err := backend.Close(first); err != nil {
		t.Fatalf("Close: %v", err)
	}

	resumeOpts := opts()
	resumeOpts.ResumeSessionID = firstSession
	second, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "second"}}}, resumeOpts)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer backend.Close(second)

	if second.SessionID != firstSession {
		t.Fatalf("SessionID = %q, want the restored %q", second.SessionID, firstSession)
	}
	if _, result := readDshAcpTurn(t, second); result.Status != "completed" {
		t.Fatalf("second result = %+v, want completed", result)
	}

	record := readFakeDshAcpRecord(statePath)
	if countFakeDshAcpMethod(record, "session/resume") != 1 {
		t.Fatalf("methods = %v, want exactly one session/resume", record.Methods)
	}
	if countFakeDshAcpMethod(record, "session/new") != 1 {
		t.Fatalf("methods = %v, want the second process to reuse the stored session", record.Methods)
	}
}

func TestDshAcpBackendFallsBackToANewSessionWhenResumeFails(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	rejectResume := map[string]string{"SOLO_TEST_DSH_ACP_REJECT_RESUME": "1"}
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "first"}}},
		&ExecuteOptions{WorkspaceDir: t.TempDir(), Env: fakeDshAcpEnv(statePath, rejectResume)})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, result := readDshAcpTurn(t, first); result.Status != "completed" {
		t.Fatalf("first result = %+v, want completed", result)
	}
	_ = backend.Close(first)

	second, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "second"}}},
		&ExecuteOptions{
			WorkspaceDir:    t.TempDir(),
			ResumeSessionID: first.SessionID,
			Env:             fakeDshAcpEnv(statePath, rejectResume),
		})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer backend.Close(second)

	if second.SessionID == first.SessionID {
		t.Fatalf("SessionID = %q, want a fresh session after the rejected resume", second.SessionID)
	}
	if _, result := readDshAcpTurn(t, second); result.Status != "completed" {
		t.Fatalf("second result = %+v, want a turn that still completed", result)
	}
	record := readFakeDshAcpRecord(statePath)
	if countFakeDshAcpMethod(record, "session/resume") != 1 || countFakeDshAcpMethod(record, "session/new") != 2 {
		t.Fatalf("methods = %v, want a rejected resume followed by a fresh session", record.Methods)
	}
}

func TestDshAcpBackendFailsLoudWhenResumeIsNotAdvertised(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "resume"}}},
		&ExecuteOptions{
			WorkspaceDir:    t.TempDir(),
			ResumeSessionID: "session-acp-1",
			Env: fakeDshAcpEnv(statePath, map[string]string{
				"SOLO_TEST_DSH_ACP_NO_RESUME_CAPABILITY": "1",
			}),
		})
	if err == nil {
		t.Fatal("expected a resume request against an agent without session/resume to fail")
	}
	if !strings.Contains(err.Error(), "does not advertise session/resume") {
		t.Fatalf("error = %v, want the advertised-capability failure", err)
	}
	if record := readFakeDshAcpRecord(statePath); countFakeDshAcpMethod(record, "session/resume") != 0 {
		t.Fatalf("methods = %v, want no resume attempt", record.Methods)
	}
}

func TestDshAcpBackendAppliesModelAndEffort(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ps, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "first"}}},
		&ExecuteOptions{
			WorkspaceDir: t.TempDir(),
			Model:        "other-model",
			Effort:       "high",
			Env:          fakeDshAcpEnv(statePath, nil),
		})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Close(ps)
	chunks, result := readDshAcpTurn(t, ps)
	if result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", result)
	}
	if len(chunks) == 0 {
		t.Fatal("expected the turn's chunks")
	}

	record := readFakeDshAcpRecord(statePath)
	if record.Model != `["test-provider","other-model"]` {
		t.Fatalf("model = %q, want the advertised opaque value", record.Model)
	}
	if record.Effort != "high" {
		t.Fatalf("effort = %q, want the advertised effort value", record.Effort)
	}
	if _, ok := result.Usage["other-model"]; !ok {
		t.Fatalf("result.Usage = %+v, want the requested model as the usage key", result.Usage)
	}
}

func TestDshAcpBackendRejectsAnUnadvertisedModel(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "first"}}},
		&ExecuteOptions{
			WorkspaceDir: t.TempDir(),
			Model:        "no-such-model",
			Env:          fakeDshAcpEnv(statePath, nil),
		})
	if err == nil {
		t.Fatal("expected an unadvertised model to fail the start")
	}
	if !strings.Contains(err.Error(), "does not offer model") || !strings.Contains(err.Error(), "other-model") {
		t.Fatalf("error = %v, want the failure to name the advertised choices", err)
	}
}

func TestDshAcpBackendStopCancelsTheTurnAndKeepsTheSession(t *testing.T) {
	statePath := fakeDshAcpStatePath(t)
	hold := map[string]string{"SOLO_TEST_DSH_ACP_HOLD_PROMPT": "1"}
	backend := newFakeDshAcpBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ps, err := backend.Start(ctx, &ExecuteRequest{Messages: []Message{{Role: RoleUser, Content: "first"}}},
		&ExecuteOptions{WorkspaceDir: t.TempDir(), Env: fakeDshAcpEnv(statePath, hold)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer backend.Close(ps)

	waitForFakeDshAcpRecord(t, statePath, func(record fakeDshAcpRecord) bool { return record.Prompts == 1 })
	if err := ps.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, result := readDshAcpTurn(t, ps); result.Status != "cancelled" {
		t.Fatalf("result = %+v, want a cancelled turn", result)
	}

	state, ok := ps.state.(*dshAcpPersistentState)
	if !ok || !state.IsAlive() {
		t.Fatal("Stop must leave the DSH process and session alive")
	}
	if record := readFakeDshAcpRecord(statePath); !record.Cancelled {
		t.Fatalf("record = %+v, want the fixture to have received session/cancel", record)
	}

	// The session stays usable: the next turn runs on the same DSH session.
	next, err := backend.Send(ctx, ps, []Message{{Role: RoleUser, Content: "second"}})
	if err != nil {
		t.Fatalf("Send after cancel: %v", err)
	}
	if next.SessionID != ps.SessionID {
		t.Fatalf("SessionID = %q, want the same session %q", next.SessionID, ps.SessionID)
	}
	if _, result := readDshAcpTurn(t, next); result.Status != "completed" {
		t.Fatalf("second result = %+v, want completed", result)
	}
	record := readFakeDshAcpRecord(statePath)
	if countFakeDshAcpMethod(record, "session/new") != 1 || record.Prompts != 2 {
		t.Fatalf("record = %+v, want two turns on one DSH session", record)
	}
}

func TestDshAcpBackendLaunchUsesTheAcpProfile(t *testing.T) {
	t.Setenv("DSH_PATCH", "")
	backend := NewDshAcpBackend(os.Args[0], slog.New(slog.NewTextHandler(io.Discard, nil)))

	execPath, args, err := backend.resolveLaunch(&ExecuteOptions{
		ExtraArgs:  []string{"--profile", "evil"},
		CustomArgs: []string{"--no-open", "--patch", "evil.yml"},
	})
	if err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if execPath == "" {
		t.Fatal("expected a resolved executable")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--profile acp") {
		t.Fatalf("args = %v, want the acp profile", args)
	}
	if !strings.Contains(joined, "--no-open") {
		t.Fatalf("args = %v, want the caller's own argument kept", args)
	}
	if strings.Contains(joined, "evil") {
		t.Fatalf("args = %v, want the profile and patch overrides blocked", args)
	}
}

func TestDshMetaFollowsTheSelectedProtocol(t *testing.T) {
	t.Setenv("SOLO_DSH_PROTOCOL", "sdk")
	sdkMeta := dshMeta()
	if len(sdkMeta.Protocols) != 1 || sdkMeta.Protocols[0] != "json-rpc" {
		t.Fatalf("protocols = %v, want json-rpc by default", sdkMeta.Protocols)
	}
	if sdkMeta.Capabilities.SafeStop != CapabilityUnsupported {
		t.Fatalf("safe_stop = %v, want unsupported on the SDK transport", sdkMeta.Capabilities.SafeStop)
	}

	t.Setenv("SOLO_DSH_PROTOCOL", "acp")
	acpMeta := dshMeta()
	if len(acpMeta.Protocols) != 1 || acpMeta.Protocols[0] != "acp" {
		t.Fatalf("protocols = %v, want acp when selected", acpMeta.Protocols)
	}
	if acpMeta.Capabilities.SafeStop != CapabilitySupported {
		t.Fatalf("safe_stop = %v, want supported on the ACP transport", acpMeta.Capabilities.SafeStop)
	}
	if acpMeta.Capabilities.ResumeConversation != CapabilitySupported {
		t.Fatalf("resume = %v, want supported on the ACP transport", acpMeta.Capabilities.ResumeConversation)
	}
	if acpMeta.Capabilities.TokenUsage != CapabilityUnknown {
		t.Fatalf("token_usage = %v, want unknown because usage needs a DSH build that reports it",
			acpMeta.Capabilities.TokenUsage)
	}

	backend, err := dshFactory(BackendConfig{})
	if err != nil {
		t.Fatalf("dshFactory: %v", err)
	}
	if _, ok := backend.(*DshAcpBackend); !ok {
		t.Fatalf("factory returned %T, want the ACP transport when selected", backend)
	}

	t.Setenv("SOLO_DSH_PROTOCOL", "")
	backend, err = dshFactory(BackendConfig{})
	if err != nil {
		t.Fatalf("dshFactory: %v", err)
	}
	if _, ok := backend.(*DshBackend); !ok {
		t.Fatalf("factory returned %T, want the SDK transport by default", backend)
	}
}
