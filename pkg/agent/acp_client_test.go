package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// acpWriterFunc adapts a function to the client's stdin writer.
type acpWriterFunc func([]byte) (int, error)

func (f acpWriterFunc) Write(p []byte) (int, error) { return f(p) }

// newScriptedACPClient builds a real acpClient whose writes are recorded and,
// when respond is non-nil, answered through the client's own line handling.
func newScriptedACPClient(respond func(frame map[string]any) string) (*acpClient, func() []string) {
	var (
		mu     sync.Mutex
		frames []string
	)
	client := &acpClient{
		logger:  slog.Default(),
		pending: map[int]*pendingRPC{},
	}
	client.stdin = acpWriterFunc(func(p []byte) (int, error) {
		mu.Lock()
		frames = append(frames, strings.TrimSpace(string(p)))
		mu.Unlock()
		if respond == nil {
			return len(p), nil
		}
		var frame map[string]any
		if err := json.Unmarshal(p, &frame); err != nil {
			return 0, err
		}
		if reply := respond(frame); reply != "" {
			go client.handleLine(reply)
		}
		return len(p), nil
	})
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), frames...)
	}
}

func replyTo(frame map[string]any, result string) string {
	id, _ := frame["id"].(float64)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, int(id), result)
}

func TestACPClientCancelWritesNotificationWithoutWaiting(t *testing.T) {
	client, frames := newScriptedACPClient(nil)

	if err := client.cancelSession("session-1"); err != nil {
		t.Fatalf("cancelSession: %v", err)
	}

	sent := frames()
	if len(sent) != 1 {
		t.Fatalf("expected one frame, got %d: %v", len(sent), sent)
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(sent[0]), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame["method"] != "session/cancel" {
		t.Fatalf("method = %v, want session/cancel", frame["method"])
	}
	if _, hasID := frame["id"]; hasID {
		t.Fatalf("notification carried an id: %s", sent[0])
	}
	params, _ := frame["params"].(map[string]any)
	if params["sessionId"] != "session-1" {
		t.Fatalf("sessionId = %v, want session-1", params["sessionId"])
	}
}

func TestACPClientInitializeReadsAdvertisedCapabilities(t *testing.T) {
	client, frames := newScriptedACPClient(func(frame map[string]any) string {
		return replyTo(frame, `{"agentCapabilities":{"sessionCapabilities":{"resume":{},"close":{},"list":{}},"promptCapabilities":{"image":true}}}`)
	})

	capabilities, err := client.initialize(context.Background())
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !capabilities.Resume || !capabilities.Close || !capabilities.List || !capabilities.Image {
		t.Fatalf("capabilities = %+v, want resume/close/list/image advertised", capabilities)
	}

	sent := frames()
	if len(sent) != 1 {
		t.Fatalf("expected one frame, got %d", len(sent))
	}
	var frame map[string]any
	_ = json.Unmarshal([]byte(sent[0]), &frame)
	if frame["method"] != "initialize" {
		t.Fatalf("method = %v, want initialize", frame["method"])
	}
	params, _ := frame["params"].(map[string]any)
	if params["protocolVersion"] != float64(1) {
		t.Fatalf("protocolVersion = %v, want 1", params["protocolVersion"])
	}
}

func TestACPAgentCapabilitiesTreatNullAsUnsupported(t *testing.T) {
	capabilities := parseACPAgentCapabilities(json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{"resume":null,"close":{}}}}`))
	if capabilities.Resume {
		t.Fatal("null resume must not advertise resume support")
	}
	if !capabilities.Close {
		t.Fatal("empty close object must advertise close support")
	}
	if capabilities.List || capabilities.Image {
		t.Fatalf("capabilities = %+v, want list and image false", capabilities)
	}

	empty := parseACPAgentCapabilities(json.RawMessage(`{}`))
	if empty.Resume || empty.Close || empty.List || empty.Image {
		t.Fatalf("missing block advertised capabilities: %+v", empty)
	}
	if malformed := parseACPAgentCapabilities(json.RawMessage(`not json`)); malformed != (acpAgentCapabilities{}) {
		t.Fatalf("malformed block advertised capabilities: %+v", malformed)
	}
}

// dshConfigOptions is the configuration state DSH advertises: an opaque
// ["provider","model"] pair for the model and effort ids for the reasoning
// effort.
const dshConfigOptions = `{"configOptions":[
	{"id":"model","name":"Model","category":"model","type":"select",
	 "currentValue":"[\"deepseek-official\",\"deepseek-v4-flash\"]",
	 "options":[
		{"value":"[\"deepseek-official\",\"deepseek-v4-flash\"]","name":"deepseek-v4-flash"},
		{"value":"[\"deepseek-official\",\"deepseek-v4-pro\"]","name":"deepseek-v4-pro"}]},
	{"id":"reasoning_effort","name":"Reasoning effort","category":"thought_level","type":"select",
	 "currentValue":"high",
	 "options":[{"value":"","name":"Default"},{"value":"low","name":"Low"},{"value":"high","name":"High"}]}]}`

func TestACPSessionConfigSelectsAdvertisedValues(t *testing.T) {
	cfg := parseACPSessionConfig(json.RawMessage(dshConfigOptions))
	if len(cfg) != 2 {
		t.Fatalf("parsed %d options, want 2", len(cfg))
	}

	modelCases := []struct {
		name  string
		model string
		want  string
		ok    bool
	}{
		{"display name", "deepseek-v4-pro", `["deepseek-official","deepseek-v4-pro"]`, true},
		{"provider and model", "deepseek-official/deepseek-v4-pro", `["deepseek-official","deepseek-v4-pro"]`, true},
		{"current value by pair", "deepseek-v4-flash", `["deepseek-official","deepseek-v4-flash"]`, true},
		{"wrong provider", "other-official/deepseek-v4-pro", "", false},
		{"unknown model", "gpt-5", "", false},
		{"empty request", "", "", false},
	}
	for _, testCase := range modelCases {
		t.Run("model/"+testCase.name, func(t *testing.T) {
			value, ok := cfg.modelValue(testCase.model)
			if ok != testCase.ok || value != testCase.want {
				t.Fatalf("modelValue(%q) = (%q, %v), want (%q, %v)", testCase.model, value, ok, testCase.want, testCase.ok)
			}
		})
	}

	effortCases := []struct {
		name   string
		effort string
		want   string
		ok     bool
	}{
		{"value", "high", "high", true},
		{"display name", "High", "high", true},
		{"provider default", "Default", "", true},
		{"unknown effort", "turbo", "", false},
		{"empty request", "", "", false},
	}
	for _, testCase := range effortCases {
		t.Run("effort/"+testCase.name, func(t *testing.T) {
			value, ok := cfg.effortValue(testCase.effort)
			if ok != testCase.ok || value != testCase.want {
				t.Fatalf("effortValue(%q) = (%q, %v), want (%q, %v)", testCase.effort, value, ok, testCase.want, testCase.ok)
			}
		})
	}

	empty := parseACPSessionConfig(json.RawMessage(`{"sessionId":"s"}`))
	if _, ok := empty.modelValue("deepseek-v4-pro"); ok {
		t.Fatal("a session without config options must not resolve a model")
	}
	if _, ok := empty.effortValue("high"); ok {
		t.Fatal("a session without config options must not resolve an effort")
	}
	if malformed := parseACPSessionConfig(json.RawMessage(`nope`)); len(malformed) != 0 {
		t.Fatalf("malformed result parsed %d options", len(malformed))
	}
}

func TestACPClientSetConfigOptionRoundTrip(t *testing.T) {
	client, frames := newScriptedACPClient(func(frame map[string]any) string {
		if frame["method"] != "session/set_config_option" {
			return replyTo(frame, `{}`)
		}
		return replyTo(frame, dshConfigOptions[strings.Index(dshConfigOptions, `{"configOptions"`):])
	})

	cfg, err := client.setConfigOption(context.Background(), "session-1", acpModelConfigID, `["deepseek-official","deepseek-v4-pro"]`)
	if err != nil {
		t.Fatalf("setConfigOption: %v", err)
	}
	if len(cfg) != 2 {
		t.Fatalf("returned %d options, want the complete state", len(cfg))
	}

	sent := frames()
	if len(sent) != 1 {
		t.Fatalf("expected one frame, got %d", len(sent))
	}
	var frame map[string]any
	_ = json.Unmarshal([]byte(sent[0]), &frame)
	params, _ := frame["params"].(map[string]any)
	if params["sessionId"] != "session-1" || params["configId"] != "model" {
		t.Fatalf("params = %v, want session-1/model", params)
	}
	if params["value"] != `["deepseek-official","deepseek-v4-pro"]` {
		t.Fatalf("value = %v, want the advertised opaque value", params["value"])
	}
}

func TestACPClientSetConfigOptionReportsAgentRejection(t *testing.T) {
	client, _ := newScriptedACPClient(func(frame map[string]any) string {
		id, _ := frame["id"].(float64)
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"unknown session config option: model"}}`, int(id))
	})

	_, err := client.setConfigOption(context.Background(), "session-1", acpModelConfigID, "x")
	if err == nil {
		t.Fatal("expected the agent rejection to surface")
	}
	if !strings.Contains(err.Error(), "unknown session config option") {
		t.Fatalf("error = %v, want the agent message", err)
	}
}

func TestACPPermissionOptionIDSelectsAnAdvertisedPermit(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{
			name:   "dsh one-shot permit",
			params: `{"sessionId":"s","options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject-once","name":"Reject","kind":"reject_once"}]}`,
			want:   "allow-once",
		},
		{
			name:   "session-wide permit wins",
			params: `{"options":[{"optionId":"allow-once","kind":"allow_once"},{"optionId":"allow-always","kind":"allow_always"}]}`,
			want:   "allow-always",
		},
		{
			name:   "permit named without a kind",
			params: `{"options":[{"optionId":"approve_for_session","name":"Always allow"}]}`,
			want:   "approve_for_session",
		},
		{
			name:   "reject-only options fall back",
			params: `{"options":[{"optionId":"no","kind":"reject_once"}]}`,
			want:   "approve_for_session",
		},
		{
			name:   "no options fall back",
			params: `{"sessionId":"s"}`,
			want:   "approve_for_session",
		},
		{
			name:   "malformed params fall back",
			params: `not json`,
			want:   "approve_for_session",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := acpPermissionOptionID(json.RawMessage(testCase.params)); got != testCase.want {
				t.Fatalf("acpPermissionOptionID() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestACPPermissionResponseAnswersWithTheSelectedOption(t *testing.T) {
	client, frames := newScriptedACPClient(nil)
	params := `{"sessionId":"s","options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject-once","kind":"reject_once"}]}`
	raw := map[string]json.RawMessage{
		"id":     json.RawMessage(`7`),
		"method": json.RawMessage(`"session/request_permission"`),
		"params": json.RawMessage(params),
	}

	client.handleAgentRequest(raw)

	sent := frames()
	if len(sent) != 1 {
		t.Fatalf("expected one response frame, got %d", len(sent))
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(sent[0]), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	result, _ := frame["result"].(map[string]any)
	outcome, _ := result["outcome"].(map[string]any)
	if outcome["outcome"] != "selected" || outcome["optionId"] != "allow-once" {
		t.Fatalf("outcome = %v, want the selected allow-once option", outcome)
	}
	if frame["id"] != float64(7) {
		t.Fatalf("id = %v, want 7", frame["id"])
	}
}

func TestACPClientSetConfigOptionHonorsContextCancellation(t *testing.T) {
	client, _ := newScriptedACPClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := client.setConfigOption(ctx, "session-1", acpEffortConfigID, "high"); err == nil {
		t.Fatal("expected the cancelled request to fail")
	}
	if len(client.pending) != 0 {
		t.Fatalf("cancelled request left %d pending entries", len(client.pending))
	}
}

func TestACPClientResumeSessionSendsTheWorkspace(t *testing.T) {
	client, frames := newScriptedACPClient(func(frame map[string]any) string {
		return replyTo(frame, `{"sessionId":"session-restored","configOptions":[{"id":"model","options":[{"value":"[\"p\",\"m\"]","name":"m"}]}]}`)
	})

	resumed, cfg, err := client.resumeSession(context.Background(), "session-1", "/work/space")
	if err != nil {
		t.Fatalf("resumeSession: %v", err)
	}
	if resumed != "session-restored" {
		t.Fatalf("resumed id = %q, want the id the agent returned", resumed)
	}
	if _, ok := cfg.modelValue("m"); !ok {
		t.Fatalf("resumed session configuration was not parsed: %+v", cfg)
	}

	sent := frames()
	if len(sent) != 1 {
		t.Fatalf("expected one frame, got %d", len(sent))
	}
	var frame map[string]any
	_ = json.Unmarshal([]byte(sent[0]), &frame)
	if frame["method"] != "session/resume" {
		t.Fatalf("method = %v, want session/resume", frame["method"])
	}
	params, _ := frame["params"].(map[string]any)
	if params["sessionId"] != "session-1" || params["cwd"] != "/work/space" {
		t.Fatalf("params = %v, want the requested session and workspace", params)
	}
	if _, ok := params["mcpServers"].([]any); !ok {
		t.Fatalf("mcpServers = %v, want an empty array", params["mcpServers"])
	}
}

func TestACPClientResumeSessionKeepsTheRequestedIDWhenUnnamed(t *testing.T) {
	client, _ := newScriptedACPClient(func(frame map[string]any) string {
		return replyTo(frame, `{}`)
	})

	resumed, cfg, err := client.resumeSession(context.Background(), "session-1", "/work/space")
	if err != nil {
		t.Fatalf("resumeSession: %v", err)
	}
	if resumed != "session-1" {
		t.Fatalf("resumed id = %q, want the requested id", resumed)
	}
	if len(cfg) != 0 {
		t.Fatalf("configuration = %+v, want empty", cfg)
	}
}

func TestACPClientResumeSessionSurfacesUnknownSession(t *testing.T) {
	client, _ := newScriptedACPClient(func(frame map[string]any) string {
		id, _ := frame["id"].(float64)
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"unknown session"}}`, int(id))
	})

	if _, _, err := client.resumeSession(context.Background(), "session-gone", "/work/space"); err == nil {
		t.Fatal("expected an unknown session to fail the resume")
	} else if !strings.Contains(err.Error(), "unknown session") {
		t.Fatalf("error = %v, want the agent message", err)
	}
}
