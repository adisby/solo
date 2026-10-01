package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// SOLO_TEST_DSH_ACP_HELPER=1 makes the test binary run as the DSH ACP agent:
// it speaks standard ACP over stdin/stdout, so the whole backend path (launch,
// initialize, session/new or session/resume, model selection, prompt, cancel,
// teardown) runs against a real subprocess.
//
// SOLO_TEST_DSH_ACP_STATE names the JSON file this fixture reads and writes, so
// a later process sees the sessions an earlier one created. The optional
// variables below change the agent's behavior:
//
//	SOLO_TEST_DSH_ACP_NO_RESUME_CAPABILITY=1  initialize advertises no session/resume
//	SOLO_TEST_DSH_ACP_REJECT_RESUME=1         every session/resume fails
//	SOLO_TEST_DSH_ACP_HOLD_PROMPT=1           the first prompt stays in flight until session/cancel
func TestDshAcpHelperProcess(t *testing.T) {
	if os.Getenv("SOLO_TEST_DSH_ACP_HELPER") != "1" {
		t.Skip("dsh ACP helper process; only runs when re-executed by a dsh ACP backend test")
	}
	runFakeDshAcpRuntime()
	os.Exit(0)
}

// fakeDshAcpRecord is the fixture state one test observes and asserts on.
type fakeDshAcpRecord struct {
	Sessions   []string `json:"sessions"`
	SessionID  string   `json:"session_id"`
	Methods    []string `json:"methods"`
	Prompts    int      `json:"prompts"`
	Model      string   `json:"model"`
	Effort     string   `json:"effort"`
	Cancelled  bool     `json:"cancelled"`
	LastPrompt string   `json:"last_prompt"`
}

// fakeDshAcpStateMu serializes the fixture's reads and writes of its state file.
var fakeDshAcpStateMu sync.Mutex

func readFakeDshAcpRecord(path string) fakeDshAcpRecord {
	fakeDshAcpStateMu.Lock()
	defer fakeDshAcpStateMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return fakeDshAcpRecord{}
	}
	var record fakeDshAcpRecord
	_ = json.Unmarshal(data, &record)
	return record
}

func writeFakeDshAcpRecord(path string, record fakeDshAcpRecord) {
	if path == "" {
		return
	}
	fakeDshAcpStateMu.Lock()
	defer fakeDshAcpStateMu.Unlock()
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// fakeDshAcpConfigOptions is the configuration state the fixture advertises: an
// opaque provider/model pair for the model and effort ids for the reasoning
// effort, matching how DSH encodes them.
func fakeDshAcpConfigOptions() []map[string]any {
	return []map[string]any{
		{
			"id": "model", "name": "Model", "category": "model", "type": "select",
			"currentValue": `["test-provider","test-model"]`,
			"options": []map[string]any{
				{"value": `["test-provider","test-model"]`, "name": "test-model"},
				{"value": `["test-provider","other-model"]`, "name": "other-model"},
			},
		},
		{
			"id": "reasoning_effort", "name": "Reasoning effort", "category": "thought_level", "type": "select",
			"currentValue": "low",
			"options": []map[string]any{
				{"value": "low", "name": "Low"},
				{"value": "high", "name": "High"},
			},
		},
	}
}

// fakeDshAcpUpdates is the ordered update stream one prompt turn publishes.
func fakeDshAcpUpdates(prompt string) []map[string]any {
	return []map[string]any{
		{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "echo: " + prompt}},
		{
			"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "read_file",
			"kind": "other", "status": "in_progress", "rawInput": map[string]any{"path": "a.txt"},
		},
		{
			"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "status": "completed",
			"content": []map[string]any{
				{"type": "content", "content": map[string]any{"type": "text", "text": "file body"}},
			},
		},
		{"sessionUpdate": "usage_update", "used": 900, "size": 1000},
	}
}

// fakeDshAcpTurnUsage is the exact usage one completed turn reports.
func fakeDshAcpTurnUsage() map[string]any {
	return map[string]any{
		"inputTokens": 7, "outputTokens": 3, "totalTokens": 10, "cachedReadTokens": 2,
	}
}

func runFakeDshAcpRuntime() {
	statePath := os.Getenv("SOLO_TEST_DSH_ACP_STATE")
	holdPrompt := os.Getenv("SOLO_TEST_DSH_ACP_HOLD_PROMPT") == "1"
	noResume := os.Getenv("SOLO_TEST_DSH_ACP_NO_RESUME_CAPABILITY") == "1"
	rejectResume := os.Getenv("SOLO_TEST_DSH_ACP_REJECT_RESUME") == "1"

	// A DSH process logs diagnostics on the same stream; the reader must skip
	// them without losing the frames that follow.
	fmt.Fprintln(os.Stdout, "dsh: fake ACP runtime starting")

	var (
		mu           sync.Mutex
		record       = readFakeDshAcpRecord(statePath)
		heldPromptID json.RawMessage
		heldTimer    *time.Timer
	)

	writeFrame := func(frame map[string]any) {
		data, err := json.Marshal(frame)
		if err != nil {
			return
		}
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
	respond := func(id json.RawMessage, result any) {
		writeFrame(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	}
	respondError := func(id json.RawMessage, code int, message string) {
		writeFrame(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(id),
			"error": map[string]any{"code": code, "message": message},
		})
	}
	notifyUpdate := func(sessionID string, update map[string]any) {
		writeFrame(map[string]any{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": sessionID, "update": update},
		})
	}
	recordMethod := func(method string) {
		record.Methods = append(record.Methods, method)
		writeFakeDshAcpRecord(statePath, record)
	}
	finishPrompt := func(id json.RawMessage, stopReason string, usage map[string]any) {
		result := map[string]any{"stopReason": stopReason}
		if usage != nil {
			result["usage"] = usage
		}
		respond(id, result)
	}

	handleInitialize := func(id json.RawMessage) {
		mu.Lock()
		recordMethod("initialize")
		mu.Unlock()
		sessionCapabilities := map[string]any{"close": map[string]any{}, "list": map[string]any{}}
		if !noResume {
			sessionCapabilities["resume"] = map[string]any{}
		}
		respond(id, map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": "fake-dsh-acp", "version": "0.0.1"},
			"agentCapabilities": map[string]any{
				"promptCapabilities":  map[string]any{"image": false},
				"sessionCapabilities": sessionCapabilities,
			},
		})
	}

	handleNewSession := func(id json.RawMessage) {
		mu.Lock()
		recordMethod("session/new")
		sessionID := fmt.Sprintf("session-acp-%d", len(record.Sessions)+1)
		record.Sessions = append(record.Sessions, sessionID)
		record.SessionID = sessionID
		writeFakeDshAcpRecord(statePath, record)
		mu.Unlock()
		respond(id, map[string]any{"sessionId": sessionID, "configOptions": fakeDshAcpConfigOptions()})
	}

	handleResumeSession := func(id json.RawMessage, params struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}) {
		mu.Lock()
		recordMethod("session/resume")
		known := record.SessionID
		mu.Unlock()
		if rejectResume || params.SessionID == "" || params.SessionID != known {
			respondError(id, -32602, "unknown session: "+params.SessionID)
			return
		}
		respond(id, map[string]any{"sessionId": known, "configOptions": fakeDshAcpConfigOptions()})
	}

	handleSetConfig := func(id json.RawMessage, params struct {
		ConfigID string `json:"configId"`
		Value    string `json:"value"`
	}) {
		mu.Lock()
		recordMethod("session/set_config_option")
		switch params.ConfigID {
		case "model":
			record.Model = params.Value
		case "reasoning_effort":
			record.Effort = params.Value
		default:
			mu.Unlock()
			respondError(id, -32602, "unknown session config option: "+params.ConfigID)
			return
		}
		writeFakeDshAcpRecord(statePath, record)
		mu.Unlock()
		respond(id, map[string]any{"configOptions": fakeDshAcpConfigOptions()})
	}

	handlePrompt := func(id json.RawMessage, params struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}) {
		mu.Lock()
		recordMethod("session/prompt")
		record.Prompts++
		if len(params.Prompt) > 0 {
			record.LastPrompt = params.Prompt[0].Text
		}
		writeFakeDshAcpRecord(statePath, record)
		mu.Unlock()

		for _, update := range fakeDshAcpUpdates(record.LastPrompt) {
			notifyUpdate(params.SessionID, update)
		}
		// Hold only the first turn: the fixture exists to prove that cancelling a
		// turn leaves the session usable for the next one.
		if !holdPrompt || record.Prompts > 1 {
			finishPrompt(id, "end_turn", fakeDshAcpTurnUsage())
			return
		}
		mu.Lock()
		heldPromptID = id
		heldTimer = time.AfterFunc(3*time.Second, func() {
			mu.Lock()
			pending := heldPromptID
			heldPromptID = nil
			mu.Unlock()
			if pending != nil {
				finishPrompt(pending, "end_turn", fakeDshAcpTurnUsage())
			}
		})
		mu.Unlock()
	}

	handleCancel := func(params struct {
		SessionID string `json:"sessionId"`
	}) {
		mu.Lock()
		recordMethod("session/cancel")
		record.Cancelled = true
		writeFakeDshAcpRecord(statePath, record)
		pending := heldPromptID
		heldPromptID = nil
		if heldTimer != nil {
			heldTimer.Stop()
		}
		mu.Unlock()
		if pending != nil {
			finishPrompt(pending, "cancelled", nil)
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.Method == "" {
			continue
		}
		switch frame.Method {
		case "initialize":
			handleInitialize(frame.ID)
		case "session/new":
			handleNewSession(frame.ID)
		case "session/resume":
			var params struct {
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			handleResumeSession(frame.ID, params)
		case "session/set_config_option":
			var params struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			handleSetConfig(frame.ID, params)
		case "session/prompt":
			var params struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			handlePrompt(frame.ID, params)
		case "session/cancel":
			var params struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			handleCancel(params)
		}
	}
}

// fakeDshAcpStatePath names the fixture's state file for one test.
func fakeDshAcpStatePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "dsh-acp-state.json")
}

// fakeDshAcpEnv points a launched fixture at its state file.
func fakeDshAcpEnv(statePath string, extra map[string]string) map[string]string {
	env := map[string]string{
		"SOLO_TEST_DSH_ACP_HELPER": "1",
		"SOLO_TEST_DSH_ACP_STATE":  statePath,
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
}

// waitForFakeDshAcpRecord waits until the fixture state satisfies match.
func waitForFakeDshAcpRecord(t *testing.T, statePath string, match func(fakeDshAcpRecord) bool) fakeDshAcpRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record := readFakeDshAcpRecord(statePath)
		if match(record) {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for fixture state; last record = %+v", readFakeDshAcpRecord(statePath))
	return fakeDshAcpRecord{}
}

// countFakeDshAcpMethod counts how often the fixture saw one protocol method.
func countFakeDshAcpMethod(record fakeDshAcpRecord, method string) int {
	count := 0
	for _, seen := range record.Methods {
		if seen == method {
			count++
		}
	}
	return count
}
