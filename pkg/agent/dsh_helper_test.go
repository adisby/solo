package agent

// This file is both a test helper and the fake DSH runtime it drives.
//
// TestDshHelperProcess re-executes the test binary as a DSH stand-in when
// SOLO_TEST_DSH_HELPER=1 is set, speaking the SDK JSON-RPC session protocol on
// stdin/stdout. Tests point DSH_BIN at os.Args[0] with that variable set, so the
// whole backend path (launch, handshake, prompt, event mapping, teardown) runs
// against a real subprocess without a model call.
//
// The fake is deliberately strict: it replies with the sessionId the client
// asked for and ignores any request it does not know, so a protocol drift shows
// up as a timeout instead of a silent pass.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestDshHelperProcess must be selected by -test.run when the process is
// re-executed as the fake runtime.
func TestDshHelperProcess(t *testing.T) {
	if os.Getenv("SOLO_TEST_DSH_HELPER") != "1" {
		t.Skip("dsh helper process; only runs when re-executed by a dsh backend test")
	}
	runFakeDshRuntime()
	os.Exit(0)
}

func runFakeDshRuntime() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// A DSH process logs diagnostics on the same stream; the reader must skip
	// these without losing the frames that follow.
	fmt.Fprintln(out, "dsh: fake runtime starting")
	out.Flush()

	turns := 0
	// DSH lazily creates the agent+session pair for an unknown sessionId, and
	// rebuilds a known one from its session log. The fixture mirrors that: turn
	// numbers persist per session id in a file so a second process resumes the
	// same conversation instead of starting over.
	sessions := loadFakeDshSessions()
	defer storeFakeDshSessions(sessions)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			var params struct {
				Cwd      string `json:"cwd"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			_ = json.Unmarshal(req.Params, &params)
			writeFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"id":      rawID(req.ID),
				"result": map[string]any{
					"serverInfo": map[string]any{"name": "deepseek-harness-sdk-runtime", "version": "fake-0.0.1"},
				},
			})

		case "session/prompt":
			var params struct {
				SessionID     string `json:"sessionId"`
				ContentBlocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"contentBlocks"`
			}
			_ = json.Unmarshal(req.Params, &params)
			turns++
			sessions[params.SessionID]++
			sessionTurn := sessions[params.SessionID]

			// Enqueue receipt first, then the streamed turn, exactly like DSH.
			writeFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"id":      rawID(req.ID),
				"result":  map[string]any{"messageId": fmt.Sprintf("fake-user-%d", turns)},
			})

			promptText := ""
			if len(params.ContentBlocks) > 0 {
				promptText = params.ContentBlocks[0].Text
			}
			emitEvent(out, params.SessionID, "assistant/chunk", map[string]any{
				"chunk": map[string]any{"type": "reasoning-delta", "index": 0, "text": "thinking..."},
			})
			emitEvent(out, params.SessionID, "assistant/chunk", map[string]any{
				"chunk": map[string]any{"type": "tool-call-delta", "index": 0, "id": "call-1", "name": "read", "argumentsDelta": `{"path":"a.txt"}`},
			})
			// The text reports the session id and this session's turn number, so a
			// caller can assert both session identity and continued history.
			emitEvent(out, params.SessionID, "assistant/chunk", map[string]any{
				"chunk": map[string]any{
					"type": "block-end", "index": 0,
					"block": map[string]any{
						"type": "text",
						"text": fmt.Sprintf("session=%s turn=%d prompt=%s", params.SessionID, sessionTurn, promptText),
					},
				},
			})
			emitEvent(out, params.SessionID, "assistant/chunk", map[string]any{
				"chunk": map[string]any{
					"type": "usage",
					"usage": map[string]any{
						"inputTokens": 11, "outputTokens": 7, "totalTokens": 18,
						"cacheReadTokens": 0, "cacheWriteTokens": 0, "reasoningTokens": 3,
					},
				},
			})
			writeFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session.status",
				"params":  map[string]any{"sessionId": params.SessionID, "status": "running"},
			})
			emitEvent(out, params.SessionID, "turn/end", map[string]any{
				"turn": turns, "reason": map[string]any{"kind": "completed"},
			})
			writeFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session.status",
				"params":  map[string]any{"sessionId": params.SessionID, "status": "idle"},
			})

		case "shutdown":
			writeFrame(out, map[string]any{"jsonrpc": "2.0", "id": rawID(req.ID), "result": map[string]any{}})
			return

		default:
			// Unknown methods are ignored, mirroring how DSH treats noise.
			continue
		}
	}
}

// rawID re-emits the request id verbatim so string and number ids both work.
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

func writeFrame(out *bufio.Writer, frame map[string]any) {
	payload, err := json.Marshal(frame)
	if err != nil {
		return
	}
	out.Write(payload)
	out.WriteByte('\n')
	out.Flush()
}

func emitEvent(out *bufio.Writer, sessionID, eventType string, data map[string]any) {
	writeFrame(out, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session.event",
		"params": map[string]any{
			"sessionId": sessionID,
			"event":     map[string]any{"type": eventType, "seq": 1, "time": 1, "data": data},
		},
	})
}

// fakeDshStatePath is where the fixture keeps its per-session turn counters. The
// test points SOLO_TEST_DSH_STATE at a temp file so a resumed process sees the
// sessions an earlier process created.
func fakeDshStatePath() string {
	return os.Getenv("SOLO_TEST_DSH_STATE")
}

func loadFakeDshSessions() map[string]int {
	sessions := map[string]int{}
	path := fakeDshStatePath()
	if path == "" {
		return sessions
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return sessions
	}
	_ = json.Unmarshal(raw, &sessions)
	return sessions
}

func storeFakeDshSessions(sessions map[string]int) {
	path := fakeDshStatePath()
	if path == "" {
		return
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}
