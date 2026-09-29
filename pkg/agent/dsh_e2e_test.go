package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDshBackendExecuteAgainstRealRuntime drives a real DSH process through the
// SDK JSON-RPC session and asserts that a one-shot turn completes with text.
//
// Gated because it boots a real harness and spends model tokens:
//
//	SOLO_E2E_DSH=1 DSH_BIN=<path to dsh launcher> go test ./pkg/agent/ -run TestDshBackendExecuteAgainstRealRuntime -v
//
// DSH_HOME must point at a profile root that has an `sdk` profile; the test
// never writes to the default one.
func TestDshBackendExecuteAgainstRealRuntime(t *testing.T) {
	if os.Getenv("SOLO_E2E_DSH") != "1" {
		t.Skip("set SOLO_E2E_DSH=1 (and DSH_BIN) to run against a real DSH runtime")
	}
	execPath := strings.TrimSpace(os.Getenv("DSH_BIN"))
	if execPath == "" {
		t.Fatal("DSH_BIN must point at the dsh launcher")
	}

	backend := NewDshBackend(execPath, nil)
	if backend.Name() != "dsh" {
		t.Fatalf("Name() = %q, want dsh", backend.Name())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	workspace := t.TempDir()
	session, err := backend.Execute(ctx, &ExecuteRequest{
		AgentID:  "dsh-e2e",
		Messages: []Message{{Role: "user", Content: "Reply with exactly the token DSH_OK and nothing else."}},
	}, &ExecuteOptions{
		WorkspaceDir: workspace,
		Env:          map[string]string{"DSH_TELEMETRY_DISABLED": "1"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var text strings.Builder
	deadline := time.After(4 * time.Minute)
	for session.Messages != nil || session.Result != nil {
		select {
		case chunk, ok := <-session.Messages:
			if !ok {
				session.Messages = nil
				continue
			}
			if chunk.Type == string(MessageText) || chunk.Type == string(MessageThinking) {
				text.WriteString(chunk.Content)
			}
		case result, ok := <-session.Result:
			if !ok {
				session.Result = nil
				continue
			}
			if result.Status != "completed" {
				t.Fatalf("turn status = %q (error=%q)", result.Status, result.Error)
			}
			if strings.TrimSpace(text.String()) == "" && result.Output == "" {
				t.Fatalf("completed turn produced no text (usage=%v init=%v)", result.Usage, result.InitInfo)
			}
			t.Logf("dsh turn completed: usage=%v text=%.120q", result.Usage, text.String())
			return
		case <-deadline:
			t.Fatalf("timed out waiting for the DSH turn (partial text=%q)", text.String())
		}
	}
}
