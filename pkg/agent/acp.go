package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Shared JSON-RPC types ──

type pendingRPC struct {
	ch     chan rpcResult
	method string
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// ── ACP prompt result ──

type acpPromptResult struct {
	stopReason string
	usage      TokenUsage
}

type acpInitialPromptTurn struct {
	ctx          context.Context
	provider     string
	sessionID    string
	promptBlocks []map[string]any
	client       *acpClient
	turns        *acpTurnController
	turn         *acpRuntimeTurn
	// stopStatus maps the prompt response's stop reason to a Solo turn status
	// and failure message. A nil value reports every settled prompt as
	// completed, which is what the adapters that ignore stop reasons do.
	stopStatus func(stopReason string) (status, message string)
}

func startACPInitialPromptTurn(turn acpInitialPromptTurn) {
	go func() {
		result, err := turn.client.request(turn.ctx, "session/prompt", map[string]any{
			"sessionId": turn.sessionID,
			"prompt":    turn.promptBlocks,
		})
		if err != nil {
			msg := fmt.Sprintf("%s session/prompt failed: %v", turn.provider, err)
			if errors.Is(turn.ctx.Err(), context.DeadlineExceeded) {
				msg = fmt.Sprintf("%s timed out during initial prompt", turn.provider)
			} else if errors.Is(turn.ctx.Err(), context.Canceled) {
				msg = "execution cancelled"
			}
			turn.turns.finish(turn.turn, acpPromptErrorStatus(turn.ctx), msg)
			return
		}

		status, message := "completed", ""
		if turn.stopStatus != nil {
			status, message = turn.stopStatus(extractACPStopReason(result))
		}
		turn.turns.finish(turn.turn, status, message)
	}()
}

// extractACPStopReason reads the standard stopReason of a prompt response.
func extractACPStopReason(result json.RawMessage) string {
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	return r.StopReason
}

func acpPromptErrorStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timed_out"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	return "failed"
}

// acpRuntimeTurn owns all mutable state and output channels for one ACP
// session/prompt request. Providers keep one controller for the lifetime of a
// process so process exit and late callbacks always target the active turn,
// never channels captured by Start.
type acpRuntimeTurn struct {
	mu        sync.Mutex
	msgMu     sync.RWMutex
	msgCh     chan OutputChunk
	resCh     chan *Result
	done      chan struct{}
	output    strings.Builder
	usage     TokenUsage
	model     string
	startedAt time.Time
	finished  bool
}

func newACPRuntimeTurn(model string) *acpRuntimeTurn {
	return &acpRuntimeTurn{
		msgCh:     make(chan OutputChunk, 256),
		resCh:     make(chan *Result, 1),
		done:      make(chan struct{}),
		model:     model,
		startedAt: time.Now(),
	}
}

func (t *acpRuntimeTurn) emit(chunk OutputChunk) {
	t.msgMu.RLock()
	defer t.msgMu.RUnlock()

	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	if chunk.Type == string(MessageText) && chunk.Content != "" {
		t.output.WriteString(chunk.Content)
	}
	t.mu.Unlock()
	if chunk.Context != nil {
		sendContextChunk(t.done, t.msgCh, chunk)
		return
	}
	trySend(t.msgCh, chunk)
}

// setModel names the model this turn reports usage under. A backend that learns
// the exact route from the session configuration sets it once the session
// exists; turns created before that start with the requested model.
func (t *acpRuntimeTurn) setModel(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.model = model
}

func (t *acpRuntimeTurn) recordPromptDone(pr acpPromptResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	// ACP implementations may emit both turn_end and the session/prompt
	// response. Preserve fields already reported when the duplicate terminal
	// signal omits usage instead of replacing them with zeroes.
	if pr.usage.InputTokens != 0 {
		t.usage.InputTokens = pr.usage.InputTokens
	}
	if pr.usage.OutputTokens != 0 {
		t.usage.OutputTokens = pr.usage.OutputTokens
	}
	if pr.usage.CacheReadTokens != 0 {
		t.usage.CacheReadTokens = pr.usage.CacheReadTokens
	}
}

func (t *acpRuntimeTurn) finish(status, errMsg string) bool {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return false
	}
	t.finished = true
	close(t.done)
	result := &Result{
		Status:     status,
		Error:      errMsg,
		Output:     t.output.String(),
		DurationMs: time.Since(t.startedAt).Milliseconds(),
		Usage:      buildACPUsageMap(t.usage, t.model),
	}
	t.mu.Unlock()

	// A context event may be waiting for buffer space. Closing done releases
	// it; msgMu then guarantees no sender overlaps channel closure.
	t.msgMu.Lock()
	close(t.msgCh)
	t.msgMu.Unlock()
	t.resCh <- result
	close(t.resCh)
	return true
}

// acpTurnController enforces the ACP contract that a process has at most one
// active prompt turn. It also serializes callback delivery with terminal
// channel closure, preventing late events and process-exit paths from sending
// to channels that belong to an earlier turn.
type acpTurnController struct {
	mu      sync.Mutex
	current *acpRuntimeTurn
}

func (c *acpTurnController) begin(model string) (*acpRuntimeTurn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		return nil, errors.New("ACP turn already in progress")
	}
	turn := newACPRuntimeTurn(model)
	c.current = turn
	return turn, nil
}

func (c *acpTurnController) emit(chunk OutputChunk) {
	c.mu.Lock()
	turn := c.current
	c.mu.Unlock()
	if turn != nil {
		turn.emit(chunk)
	}
}

func (c *acpTurnController) recordPromptDone(pr acpPromptResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		c.current.recordPromptDone(pr)
	}
}

func (c *acpTurnController) finish(turn *acpRuntimeTurn, status, errMsg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if turn == nil || c.current != turn {
		return false
	}
	finished := turn.finish(status, errMsg)
	c.current = nil
	return finished
}

func (c *acpTurnController) failActive(errMsg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		return false
	}
	finished := c.current.finish("failed", errMsg)
	c.current = nil
	return finished
}

// ── ACP Client ──

// acpClient implements the ACP (Agent Communication Protocol) JSON-RPC 2.0
// transport over stdin/stdout. It is shared by Kimi, Kiro, and Hermes backends.
type acpClient struct {
	logger    *slog.Logger
	stdin     interface{ Write([]byte) (int, error) }
	writeMu   sync.Mutex
	mu        sync.Mutex
	nextID    int
	pending   map[int]*pendingRPC
	sessionID string

	cbMu         sync.Mutex // protects onChunk + onPromptDone (replaced per-turn by Send)
	onChunk      func(OutputChunk)
	onPromptDone func(acpPromptResult)

	acceptNotification func(updateType string) bool

	toolMu       sync.Mutex
	pendingTools map[string]*pendingToolCall

	usageMu sync.Mutex
	usage   TokenUsage
}

// setCallbacks replaces the per-turn chunk and prompt-done callbacks.
// It is called from Send to redirect output to the new turn's channels.
// Must NOT be called concurrently with the reader goroutine dispatching
// events for the same turn — the caller (Send) serialises this by waiting
// for the previous turn's Result channel to close before issuing a new
// session/prompt request.
func (c *acpClient) setCallbacks(onChunk func(OutputChunk), onPromptDone func(acpPromptResult)) {
	c.cbMu.Lock()
	c.onChunk = onChunk
	c.onPromptDone = onPromptDone
	c.cbMu.Unlock()
}

func (c *acpClient) invokeOnChunk(chunk OutputChunk) {
	c.cbMu.Lock()
	fn := c.onChunk
	c.cbMu.Unlock()
	if fn != nil {
		fn(chunk)
	}
}

func (c *acpClient) invokeOnPromptDone(pr acpPromptResult) {
	c.cbMu.Lock()
	fn := c.onPromptDone
	c.cbMu.Unlock()
	if fn != nil {
		fn(pr)
	}
}

type pendingToolCall struct {
	toolName string
	input    map[string]any
	argsText string
	emitted  bool
}

func (c *acpClient) writeLine(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.stdin.Write(data)
	return err
}

func (c *acpClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	pr := &pendingRPC{ch: make(chan rpcResult, 1), method: method}
	c.pending[id] = pr
	c.mu.Unlock()

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	data = append(data, '\n')
	if err := c.writeLine(data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case res := <-pr.ch:
		return res.result, res.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *acpClient) closeAllPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, pr := range c.pending {
		pr.ch <- rpcResult{err: err}
		delete(c.pending, id)
	}
}

func (c *acpClient) handleLine(line string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}

	if _, hasID := raw["id"]; hasID {
		if _, hasResult := raw["result"]; hasResult {
			c.handleResponse(raw)
			return
		}
		if _, hasError := raw["error"]; hasError {
			c.handleResponse(raw)
			return
		}
		if _, hasMethod := raw["method"]; hasMethod {
			c.handleAgentRequest(raw)
			return
		}
	}

	if _, hasMethod := raw["method"]; hasMethod {
		c.handleNotification(raw)
	}
}

func (c *acpClient) handleAgentRequest(raw map[string]json.RawMessage) {
	var method string
	_ = json.Unmarshal(raw["method"], &method)

	rawID, ok := raw["id"]
	if !ok {
		return
	}

	var resp map[string]any
	switch method {
	case "session/request_permission":
		optionID := acpPermissionOptionID(raw["params"])
		resp = map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(rawID),
			"result": map[string]any{
				"outcome": map[string]any{
					"outcome":  "selected",
					"optionId": optionID,
				},
			},
		}
		c.logger.Debug("auto-approved agent permission request", "method", method, "option_id", optionID)
	default:
		resp = map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(rawID),
			"error": map[string]any{
				"code":    -32601,
				"message": "method not found: " + method,
			},
		}
		c.logger.Debug("unhandled agent client request", "method", method)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		c.logger.Warn("marshal agent-request response", "method", method, "error", err)
		return
	}
	data = append(data, '\n')
	if err := c.writeLine(data); err != nil {
		c.logger.Warn("write agent-request response", "method", method, "error", err)
	}
}

func (c *acpClient) handleResponse(raw map[string]json.RawMessage) {
	var id int
	if err := json.Unmarshal(raw["id"], &id); err != nil {
		var fid float64
		if err := json.Unmarshal(raw["id"], &fid); err != nil {
			return
		}
		id = int(fid)
	}

	c.mu.Lock()
	pr, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if !ok {
		return
	}

	if errData, hasErr := raw["error"]; hasErr {
		var rpcErr struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(errData, &rpcErr)
		detail := ""
		if len(rpcErr.Data) > 0 && string(rpcErr.Data) != "null" {
			var s string
			if err := json.Unmarshal(rpcErr.Data, &s); err == nil {
				detail = s
			} else {
				detail = string(rpcErr.Data)
			}
		}
		if detail != "" {
			pr.ch <- rpcResult{err: fmt.Errorf("%s: %s (code=%d, data=%s)", pr.method, rpcErr.Message, rpcErr.Code, detail)}
		} else {
			pr.ch <- rpcResult{err: fmt.Errorf("%s: %s (code=%d)", pr.method, rpcErr.Message, rpcErr.Code)}
		}
	} else {
		if pr.method == "session/prompt" {
			c.extractPromptResult(raw["result"])
		}
		pr.ch <- rpcResult{result: raw["result"]}
	}
}

func (c *acpClient) extractPromptResult(data json.RawMessage) {
	var resp struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens      int64 `json:"inputTokens"`
			OutputTokens     int64 `json:"outputTokens"`
			TotalTokens      int64 `json:"totalTokens"`
			ThoughtTokens    int64 `json:"thoughtTokens"`
			CachedReadTokens int64 `json:"cachedReadTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}

	pr := acpPromptResult{
		stopReason: resp.StopReason,
	}
	if resp.Usage != nil {
		pr.usage = TokenUsage{
			InputTokens:     resp.Usage.InputTokens,
			OutputTokens:    resp.Usage.OutputTokens,
			CacheReadTokens: resp.Usage.CachedReadTokens,
		}
	}

	c.invokeOnPromptDone(pr)
}

func (c *acpClient) handleNotification(raw map[string]json.RawMessage) {
	var method string
	_ = json.Unmarshal(raw["method"], &method)

	if method != "session/update" && method != "session/notification" {
		return
	}

	var params struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if p, ok := raw["params"]; ok {
		_ = json.Unmarshal(p, &params)
	}
	if len(params.Update) == 0 {
		return
	}

	updateType, updateData := normalizeACPUpdate(params.Update)
	if c.acceptNotification != nil && !c.acceptNotification(updateType) {
		return
	}

	switch updateType {
	case "agent_message_chunk":
		c.handleAgentMessage(updateData)
	case "agent_thought_chunk":
		c.handleAgentThought(updateData)
	case "tool_call":
		c.handleToolCallStart(updateData)
	case "tool_call_update":
		c.handleToolCallUpdate(updateData)
	case "usage_update":
		c.handleUsageUpdate(updateData)
	case "turn_end":
		c.extractPromptResult(updateData)
	}
}

func normalizeACPUpdate(data json.RawMessage) (string, json.RawMessage) {
	var updateType struct {
		SessionUpdate string `json:"sessionUpdate"`
		Type          string `json:"type"`
	}
	_ = json.Unmarshal(data, &updateType)
	if updateType.SessionUpdate != "" {
		return normalizeACPUpdateType(updateType.SessionUpdate), data
	}
	if updateType.Type != "" {
		return normalizeACPUpdateType(updateType.Type), data
	}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper) == 1 {
		for k, v := range wrapper {
			return normalizeACPUpdateType(k), v
		}
	}

	return "", data
}

func normalizeACPUpdateType(t string) string {
	key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(t), "_", ""), "-", ""))
	switch key {
	case "agentmessagechunk":
		return "agent_message_chunk"
	case "agentthoughtchunk":
		return "agent_thought_chunk"
	case "toolcall":
		return "tool_call"
	case "toolcallupdate":
		return "tool_call_update"
	case "usageupdate":
		return "usage_update"
	case "turnend", "endturn":
		return "turn_end"
	default:
		return ""
	}
}

func (c *acpClient) handleAgentMessage(data json.RawMessage) {
	var msg struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil || msg.Content.Text == "" {
		return
	}
	c.invokeOnChunk(OutputChunk{Type: string(MessageText), Content: msg.Content.Text})
}

func (c *acpClient) handleAgentThought(data json.RawMessage) {
	var msg struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil || msg.Content.Text == "" {
		return
	}
	c.invokeOnChunk(OutputChunk{Type: string(MessageThinking), Content: msg.Content.Text})
}

func (c *acpClient) handleToolCallStart(data json.RawMessage) {
	var msg struct {
		ToolCallID string            `json:"toolCallId"`
		Name       string            `json:"name"`
		Title      string            `json:"title"`
		Kind       string            `json:"kind"`
		RawInput   map[string]any    `json:"rawInput"`
		Input      map[string]any    `json:"input"`
		Parameters map[string]any    `json:"parameters"`
		Content    []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	toolName := acpToolNameFromTitle(msg.Title, msg.Kind)
	if toolName == "" {
		toolName = msg.Name
	}
	rawInput := msg.RawInput
	if rawInput == nil {
		rawInput = msg.Input
	}
	if rawInput == nil {
		rawInput = msg.Parameters
	}

	if rawInput != nil {
		c.trackTool(msg.ToolCallID, &pendingToolCall{
			toolName: toolName,
			input:    rawInput,
			emitted:  true,
		})
		c.invokeOnChunk(OutputChunk{
			Type: string(MessageToolUse),
			Tool: &ToolInfo{Name: toolName, CallID: msg.ToolCallID, Input: rawInput},
		})
		return
	}

	c.trackTool(msg.ToolCallID, &pendingToolCall{
		toolName: toolName,
		argsText: extractACPToolCallText(msg.Content),
		emitted:  false,
	})
}

func (c *acpClient) handleToolCallUpdate(data json.RawMessage) {
	var msg struct {
		ToolCallID string            `json:"toolCallId"`
		Status     string            `json:"status"`
		Name       string            `json:"name"`
		Title      string            `json:"title"`
		Kind       string            `json:"kind"`
		RawInput   map[string]any    `json:"rawInput"`
		Input      map[string]any    `json:"input"`
		Parameters map[string]any    `json:"parameters"`
		RawOutput  string            `json:"rawOutput"`
		Output     string            `json:"output"`
		Content    []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	rawInput := msg.RawInput
	if rawInput == nil {
		rawInput = msg.Input
	}
	if rawInput == nil {
		rawInput = msg.Parameters
	}
	title := msg.Title
	if title == "" {
		title = msg.Name
	}

	if msg.Status != "completed" && msg.Status != "failed" {
		if pending := c.getPendingTool(msg.ToolCallID); pending != nil && !pending.emitted {
			if text := extractACPToolCallText(msg.Content); text != "" {
				pending.argsText = text
			}
		}
		return
	}

	pending := c.takePendingTool(msg.ToolCallID)
	c.emitDeferredToolUse(pending, msg.ToolCallID, title, msg.Kind, rawInput)

	output := msg.RawOutput
	if output == "" {
		output = msg.Output
	}
	if output == "" {
		output = extractACPToolCallText(msg.Content)
	}
	c.invokeOnChunk(OutputChunk{
		Type: string(MessageToolResult),
		Tool: &ToolInfo{CallID: msg.ToolCallID, Output: output},
	})
}

func (c *acpClient) trackTool(callID string, p *pendingToolCall) {
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	if c.pendingTools == nil {
		c.pendingTools = make(map[string]*pendingToolCall)
	}
	c.pendingTools[callID] = p
}

func (c *acpClient) getPendingTool(callID string) *pendingToolCall {
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	if c.pendingTools == nil {
		return nil
	}
	return c.pendingTools[callID]
}

func (c *acpClient) takePendingTool(callID string) *pendingToolCall {
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	if c.pendingTools == nil {
		return nil
	}
	p := c.pendingTools[callID]
	delete(c.pendingTools, callID)
	return p
}

func (c *acpClient) emitDeferredToolUse(
	p *pendingToolCall,
	callID, updateTitle, updateKind string,
	updateRawInput map[string]any,
) {
	if p != nil && p.emitted {
		return
	}

	var toolName string
	var input map[string]any

	switch {
	case p != nil && p.input != nil:
		toolName = p.toolName
		input = p.input
	case p != nil:
		toolName = p.toolName
		input = parseToolArgsJSON(p.argsText)
	default:
		toolName = acpToolNameFromTitle(updateTitle, updateKind)
		input = updateRawInput
	}

	c.invokeOnChunk(OutputChunk{
		Type: string(MessageToolUse),
		Tool: &ToolInfo{Name: toolName, CallID: callID, Input: input},
	})
}

func parseToolArgsJSON(argsText string) map[string]any {
	argsText = strings.TrimSpace(argsText)
	if argsText == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsText), &m); err == nil {
		return m
	}
	return map[string]any{"text": argsText}
}

func extractACPToolCallText(blocks []json.RawMessage) string {
	var b strings.Builder
	appendPiece := func(piece string) {
		if piece == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(piece)
	}
	for _, raw := range blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			continue
		}
		switch kind.Type {
		case "content":
			var outer struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(raw, &outer); err != nil || len(outer.Content) == 0 {
				continue
			}
			var inner struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(outer.Content, &inner); err != nil {
				continue
			}
			if inner.Type != "text" {
				continue
			}
			appendPiece(inner.Text)
		case "diff":
			var diff struct {
				Path    string `json:"path"`
				OldText string `json:"oldText"`
				NewText string `json:"newText"`
			}
			if err := json.Unmarshal(raw, &diff); err != nil || diff.Path == "" {
				continue
			}
			var piece strings.Builder
			piece.WriteString("--- ")
			piece.WriteString(diff.Path)
			piece.WriteString("\n+++ ")
			piece.WriteString(diff.Path)
			if diff.OldText == "" {
				piece.WriteString("\n(new file, ")
				piece.WriteString(strconv.Itoa(len(diff.NewText)))
				piece.WriteString(" bytes)")
			} else {
				piece.WriteString("\n(edited: ")
				piece.WriteString(strconv.Itoa(len(diff.OldText)))
				piece.WriteString("→ ")
				piece.WriteString(strconv.Itoa(len(diff.NewText)))
				piece.WriteString(" bytes)")
			}
			appendPiece(piece.String())
		}
	}
	return b.String()
}

func (c *acpClient) handleUsageUpdate(data json.RawMessage) {
	var msg struct {
		Usage struct {
			InputTokens      int64 `json:"inputTokens"`
			OutputTokens     int64 `json:"outputTokens"`
			TotalTokens      int64 `json:"totalTokens"`
			CachedReadTokens int64 `json:"cachedReadTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	c.usageMu.Lock()
	if msg.Usage.InputTokens > c.usage.InputTokens {
		c.usage.InputTokens = msg.Usage.InputTokens
	}
	if msg.Usage.OutputTokens > c.usage.OutputTokens {
		c.usage.OutputTokens = msg.Usage.OutputTokens
	}
	if msg.Usage.CachedReadTokens > c.usage.CacheReadTokens {
		c.usage.CacheReadTokens = msg.Usage.CachedReadTokens
	}
	c.usageMu.Unlock()

	if contextEvent := parseACPContextUsage(data); contextEvent != nil {
		c.invokeOnChunk(OutputChunk{Type: string(MessageContext), Context: contextEvent})
	}
}

func parseACPContextUsage(data json.RawMessage) *ContextEvent {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	used := decodeContextToken(fields["used"])
	size := decodeContextToken(fields["size"])
	if used == nil && size == nil {
		return nil
	}

	return &ContextEvent{
		Type:         "usage",
		UsedTokens:   used,
		WindowTokens: size,
		Accuracy:     "estimated",
	}
}

// ── Helpers ──

func extractACPSessionID(result json.RawMessage) string {
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	return r.SessionID
}

func resolveResumedSessionID(requested string, response json.RawMessage) (string, bool) {
	got := extractACPSessionID(response)
	if got == "" {
		return requested, false
	}
	return got, got != requested
}

// notify writes a JSON-RPC notification: a method call with no id and no
// response. It is how a client asks the agent to act without awaiting a result.
func (c *acpClient) notify(method string, params any) error {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", method, err)
	}
	if err := c.writeLine(append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}
	return nil
}

// cancelSession asks the agent to stop the session's active turn. The process
// and the session stay alive: the in-flight session/prompt settles with
// stopReason "cancelled" and the session accepts later prompts.
func (c *acpClient) cancelSession(sessionID string) error {
	return c.notify("session/cancel", map[string]any{"sessionId": sessionID})
}

// resumeSession restores a persisted session for one workspace and returns its
// identity and advertised configuration. The workspace belongs to the resume
// identity: the agent verifies it before composing the restored session, so a
// caller must pass the directory the session was created in. A response naming a
// different session id adopts that id.
func (c *acpClient) resumeSession(ctx context.Context, sessionID, cwd string) (string, acpSessionConfig, error) {
	result, err := c.request(ctx, "session/resume", map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if err != nil {
		return "", nil, err
	}
	resumed, _ := resolveResumedSessionID(sessionID, result)
	return resumed, parseACPSessionConfig(result), nil
}

// acpAgentCapabilities is the capability block an agent advertises in its
// initialize result. Each session capability is advertised as an object, so its
// presence is the support statement.
type acpAgentCapabilities struct {
	Resume bool
	Close  bool
	List   bool
	Image  bool
}

// initialize performs the ACP handshake and returns the agent's advertised
// capabilities. A caller that must restore a persisted session reads
// acpAgentCapabilities.Resume instead of assuming session/resume exists.
func (c *acpClient) initialize(ctx context.Context) (acpAgentCapabilities, error) {
	result, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "solo-agent-sdk",
			"version": "1.0.0",
		},
		"clientCapabilities": map[string]any{},
	})
	if err != nil {
		return acpAgentCapabilities{}, err
	}
	return parseACPAgentCapabilities(result), nil
}

// parseACPAgentCapabilities reads the capability block of an initialize result.
// A missing or malformed block advertises nothing.
func parseACPAgentCapabilities(result json.RawMessage) acpAgentCapabilities {
	var r struct {
		AgentCapabilities struct {
			SessionCapabilities struct {
				Resume *json.RawMessage `json:"resume"`
				Close  *json.RawMessage `json:"close"`
				List   *json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return acpAgentCapabilities{}
	}
	return acpAgentCapabilities{
		Resume: acpCapabilityPresent(r.AgentCapabilities.SessionCapabilities.Resume),
		Close:  acpCapabilityPresent(r.AgentCapabilities.SessionCapabilities.Close),
		List:   acpCapabilityPresent(r.AgentCapabilities.SessionCapabilities.List),
		Image:  r.AgentCapabilities.PromptCapabilities.Image,
	}
}

// acpCapabilityPresent reports whether an advertised capability is a value
// rather than a missing or null field.
func acpCapabilityPresent(raw *json.RawMessage) bool {
	return raw != nil && string(*raw) != "null"
}

// acpConfigOption is one session configuration choice advertised by session/new,
// session/resume, or session/set_config_option. Its values are opaque to the
// client: the agent owns their encoding, and DSH encodes the model option as a
// JSON ["provider","model"] pair.
type acpConfigOption struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Category     string           `json:"category"`
	Type         string           `json:"type"`
	CurrentValue string           `json:"currentValue"`
	Options      []acpConfigValue `json:"options"`
}

// acpConfigValue is one selectable value of an acpConfigOption.
type acpConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// acpSessionConfig is a session's advertised configuration state by option id.
type acpSessionConfig map[string]acpConfigOption

// acpModelConfigID and acpEffortConfigID are the standard configuration option
// ids ACP defines for the model and the reasoning effort.
const (
	acpModelConfigID  = "model"
	acpEffortConfigID = "reasoning_effort"
)

// acpModelPair is an opaque model option value decoded as a provider/model pair.
type acpModelPair struct {
	provider string
	model    string
}

// parseACPSessionConfig reads the configOptions field carried by every session
// lifecycle result. A result without that field yields an empty configuration.
func parseACPSessionConfig(result json.RawMessage) acpSessionConfig {
	var r struct {
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return acpSessionConfig{}
	}
	cfg := make(acpSessionConfig, len(r.ConfigOptions))
	for _, option := range r.ConfigOptions {
		cfg[option.ID] = option
	}
	return cfg
}

// modelValue resolves a requested model to the exact value the session
// advertised. It matches the model element of an opaque JSON pair such as DSH's
// ["provider","model"], an advertised display name, or an exact value;
// "provider/model" pins the provider and therefore requires a pair match.
func (cfg acpSessionConfig) modelValue(model string) (string, bool) {
	option, ok := cfg[acpModelConfigID]
	if !ok || model == "" {
		return "", false
	}
	provider, id := "", model
	if slash := strings.IndexByte(model, '/'); slash >= 0 {
		provider, id = model[:slash], model[slash+1:]
	}
	matchesPair := func(value string) bool {
		pair, ok := decodeACPModelPair(value)
		return ok && pair.model == id && (provider == "" || pair.provider == provider)
	}
	for _, candidate := range option.Options {
		if matchesPair(candidate.Value) {
			return candidate.Value, true
		}
		// A display name or literal value cannot pin a provider, so a
		// provider-qualified request accepts only the pair match above.
		if provider == "" && (candidate.Name == id || candidate.Value == model) {
			return candidate.Value, true
		}
	}
	if option.CurrentValue != "" && matchesPair(option.CurrentValue) {
		return option.CurrentValue, true
	}
	return "", false
}

// effortValue resolves a requested reasoning effort to the exact value the
// session advertised, matching the value first and the display name second, so
// both "high" and "High" resolve.
func (cfg acpSessionConfig) effortValue(effort string) (string, bool) {
	option, ok := cfg[acpEffortConfigID]
	if !ok || effort == "" {
		return "", false
	}
	for _, candidate := range option.Options {
		if candidate.Value == effort || strings.EqualFold(candidate.Name, effort) {
			return candidate.Value, true
		}
	}
	return "", false
}

// currentModelName names the session's current model for usage reporting: the
// model element of an opaque JSON pair, or the advertised display name of the
// current value.
func (cfg acpSessionConfig) currentModelName() string {
	option, ok := cfg[acpModelConfigID]
	if !ok || option.CurrentValue == "" {
		return ""
	}
	if pair, ok := decodeACPModelPair(option.CurrentValue); ok {
		return pair.model
	}
	for _, candidate := range option.Options {
		if candidate.Value == option.CurrentValue && candidate.Name != "" {
			return candidate.Name
		}
	}
	return ""
}

// decodeACPModelPair decodes an opaque model value as a provider/model pair.
func decodeACPModelPair(value string) (acpModelPair, bool) {
	var parts []string
	if err := json.Unmarshal([]byte(value), &parts); err != nil || len(parts) != 2 {
		return acpModelPair{}, false
	}
	return acpModelPair{provider: parts[0], model: parts[1]}, true
}

// setConfigOption applies one advertised session configuration option and
// returns the complete resulting state. An unknown id or value is the agent's
// to reject, and its message travels back in the error.
func (c *acpClient) setConfigOption(ctx context.Context, sessionID, configID, value string) (acpSessionConfig, error) {
	result, err := c.request(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	})
	if err != nil {
		return nil, err
	}
	return parseACPSessionConfig(result), nil
}

// acpPermissionOptionID chooses the option that grants the requested tool call.
// Agents advertise their own option ids, so selection reads the request: a
// session-wide permit wins, then a one-shot permit, then any option whose id or
// name marks a permit. The unattended Daemon never escalates to a human, so the
// fallback keeps the id older DSH releases accept.
func acpPermissionOptionID(params json.RawMessage) string {
	const fallback = "approve_for_session"
	var req struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return fallback
	}
	for _, option := range req.Options {
		if option.Kind == "allow_always" && option.OptionID != "" {
			return option.OptionID
		}
	}
	for _, option := range req.Options {
		if option.Kind == "allow_once" && option.OptionID != "" {
			return option.OptionID
		}
	}
	for _, option := range req.Options {
		if option.OptionID == "" {
			continue
		}
		lowered := strings.ToLower(option.OptionID + " " + option.Name)
		if strings.Contains(lowered, "allow") || strings.Contains(lowered, "approve") {
			return option.OptionID
		}
	}
	return fallback
}

// buildACPUsageMap returns a usage map keyed by the given model, or nil if
// there are no tokens. It is shared by all ACP-family backends (hermes,
// kimi, kiro, openclaw, opencode) — they all wrap their per-turn usage
// the same way before emitting it to the daemon.
func buildACPUsageMap(usage TokenUsage, model string) map[string]TokenUsage {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 {
		return nil
	}
	return map[string]TokenUsage{model: usage}
}

// acpToolNameFromTitle normalises an ACP tool title (and optional kind
// hint) into a canonical snake_case identifier used across the daemon
// and UI.
//
// Each ACP backend emits slightly different title strings — Hermes
// sends "execute code" with a structured kind, Kimi / Kiro send
// server-specific labels like "Bash" or "Read File" with no kind. The
// optional extras slice lets each backend append its own title→name
// mappings without forking this function. extras entries are matched
// case-insensitively against the trimmed title and (when present) the
// text before the first ":".
func acpToolNameFromTitle(title string, kind string, extras ...map[string]string) string {
	lookupExtras := func(s string) (string, bool) {
		lower := strings.ToLower(strings.TrimSpace(s))
		if lower == "" {
			return "", false
		}
		for _, m := range extras {
			if v, ok := m[lower]; ok {
				return v, true
			}
		}
		return "", false
	}

	switch title {
	case "execute code":
		return "execute_code"
	}

	if v, ok := lookupExtras(title); ok {
		return v
	}

	if idx := strings.Index(title, ":"); idx > 0 {
		name := strings.TrimSpace(title[:idx])
		if v, ok := lookupExtras(name); ok {
			return v
		}
		switch {
		case name == "terminal":
			return "terminal"
		case name == "read":
			return "read_file"
		case name == "write":
			return "write_file"
		case strings.HasPrefix(name, "patch"):
			return "patch"
		case name == "search":
			return "search_files"
		case name == "web search":
			return "web_search"
		case name == "extract":
			return "web_extract"
		case name == "delegate":
			return "delegate_task"
		case name == "analyze image":
			return "vision_analyze"
		}
		return name
	}

	switch kind {
	case "read":
		return "read_file"
	case "edit":
		return "write_file"
	case "execute":
		return "terminal"
	case "search":
		return "search_files"
	case "fetch":
		return "web_search"
	case "think":
		return "thinking"
	default:
		if title != "" {
			return title
		}
		return kind
	}
}

// ── Provider-error sniffing ──

type acpProviderErrorSniffer struct {
	provider string
	mu       sync.Mutex
	remains  []byte
	lines    []string
	seen     map[string]bool
	terminal bool
}

var acpErrorHeaderRe = regexp.MustCompile(`(?:⚠️|❌|\[ERROR\]).*(?:BadRequestError|AuthenticationError|RateLimitError|HTTP [0-9]{3}|Non-retryable|API call failed)`)

var acpErrorDetailRe = regexp.MustCompile(`(?:Error:|detail:|Details:)\s*(.+)`)

var acpTerminalErrorRe = regexp.MustCompile(`(?:❌|\[ERROR\]|after \d+ retr|Non-retryable|BadRequestError|AuthenticationError)`)

var acpAgentOutputTerminalRe = regexp.MustCompile(`API call failed after \d+ retr(?:y|ies)`)

const acpMaxErrorLines = 8

func newACPProviderErrorSniffer(provider string) *acpProviderErrorSniffer {
	return &acpProviderErrorSniffer{provider: provider, seen: map[string]bool{}}
}

func (s *acpProviderErrorSniffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data := append(s.remains, p...)
	nl := strings.LastIndexByte(string(data), '\n')
	var complete string
	if nl < 0 {
		s.remains = append(s.remains[:0], data...)
		return len(p), nil
	}
	complete = string(data[:nl])
	s.remains = append(s.remains[:0], data[nl+1:]...)

	for _, line := range strings.Split(complete, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !(acpErrorHeaderRe.MatchString(line) || acpErrorDetailRe.MatchString(line)) {
			continue
		}
		if acpTerminalErrorRe.MatchString(line) {
			s.terminal = true
		}
		if s.seen[line] {
			continue
		}
		s.seen[line] = true
		s.lines = append(s.lines, line)
		if len(s.lines) > acpMaxErrorLines {
			s.lines = s.lines[len(s.lines)-acpMaxErrorLines:]
		}
	}
	return len(p), nil
}

func (s *acpProviderErrorSniffer) message() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messageLocked()
}

func (s *acpProviderErrorSniffer) terminalMessage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.terminal {
		return ""
	}
	return s.messageLocked()
}

func (s *acpProviderErrorSniffer) messageLocked() string {
	prefix := s.provider + " provider error: "
	for _, line := range s.lines {
		if m := acpErrorDetailRe.FindStringSubmatch(line); m != nil {
			detail := strings.TrimSpace(m[1])
			if detail != "" {
				return prefix + detail
			}
		}
	}
	for _, line := range s.lines {
		if acpErrorHeaderRe.MatchString(line) {
			return prefix + line
		}
	}
	return ""
}

func promoteACPResultOnProviderError(finalStatus, finalError, finalOutput string, sniffer *acpProviderErrorSniffer) (string, string) {
	if finalStatus != "completed" {
		return finalStatus, finalError
	}
	if msg := sniffer.terminalMessage(); msg != "" {
		return "failed", msg
	}
	if acpAgentOutputTerminalRe.MatchString(finalOutput) {
		msg := sniffer.message()
		if msg == "" {
			msg = sniffer.provider + " provider error: " + acpAgentOutputTerminalRe.FindString(finalOutput)
		}
		return "failed", msg
	}
	if finalOutput == "" {
		if msg := sniffer.message(); msg != "" {
			return "failed", msg
		}
	}
	return finalStatus, finalError
}
