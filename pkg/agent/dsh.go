package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DshBackend drives DeepSeek Harness (DSH) through its SDK runtime: a
// newline-delimited JSON-RPC 2.0 session over stdio, served by the
// `@deepseek-ai/dsh-sdk-jsonrpc-server` plugin inside a `--profile sdk` launch.
//
// The wire contract has three client requests and four server notifications:
//
//	initialize     { cwd, provider, model, reasoningEffort?, maxTokens? }
//	session/prompt { sessionId, contentBlocks } -> { messageId }
//	shutdown       {}
//	<- session.event   { sessionId, event }   one session-log event
//	<- session.status  { sessionId, status }  idle | running
//	<- subagent.started / subagent.finished
//
// A prompt for an unknown sessionId lazily creates the agent+session pair, so a
// single subprocess serves every turn of one Solo agent: reusing the sessionId
// carries the full conversation, and resuming a stored sessionId later in a new
// process rebuilds it from DSH's own session log.
type DshBackend struct {
	executablePath string
	logger         *slog.Logger
	// launchArgs overrides the arguments a launch would otherwise build. Tests
	// use it to run the fixture runtime in place of a real DSH process; a nil
	// value means the standard `--profile sdk` invocation.
	launchArgs []string
}

// dshDefaultProvider and dshDefaultModel match DSH's own default route. Solo's
// agent configuration overrides them per execution.
const (
	dshDefaultProvider = "deepseek-official"
	dshDefaultModel    = "deepseek-flash"
	dshProfile         = "sdk"
)

// NewDshBackend creates a DSH backend. An empty executablePath means "dsh" is
// resolved from PATH; otherwise the value may be a DSH launcher script, which is
// run through node.
func NewDshBackend(executablePath string, logger *slog.Logger) *DshBackend {
	if executablePath == "" {
		executablePath = "dsh"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &DshBackend{executablePath: executablePath, logger: logger}
}

// Name returns "dsh".
func (b *DshBackend) Name() string { return "dsh" }

// ── Process launch ───────────────────────────────────────────────────────────

// dshLaunch resolves the executable and full argument list for one DSH process.
//
// DSH is normally started as `dsh --profile sdk`, but a source checkout only
// exposes the launcher script, so a .js entry point is run through node.
//
// The permission mode is NOT an argument: the launcher rejects
// --permission-mode, so dshEnvironment passes it as DSH_PERMISSION_MODE.
func dshLaunch(executablePath string) (string, []string, error) {
	target := strings.TrimSpace(executablePath)
	args := []string{"--profile", dshProfile}
	if patch := strings.TrimSpace(os.Getenv("DSH_PATCH")); patch != "" {
		args = append(args, "--patch", patch)
	}
	if strings.EqualFold(filepath.Ext(target), ".js") {
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, fmt.Errorf("dsh: %s needs node on PATH: %w", target, err)
		}
		return node, append([]string{target}, args...), nil
	}

	resolved, err := exec.LookPath(target)
	if err != nil {
		return "", nil, fmt.Errorf("dsh executable not found at %q: %w", target, err)
	}
	return resolved, args, nil
}

// resolveLaunch returns the executable and arguments for this backend, honouring
// an injected launch override (used by tests to swap in a fixture runtime).
func (b *DshBackend) resolveLaunch() (string, []string, error) {
	if b.launchArgs != nil {
		resolved, err := exec.LookPath(b.executablePath)
		if err != nil {
			return "", nil, fmt.Errorf("dsh executable not found at %q: %w", b.executablePath, err)
		}
		return resolved, b.launchArgs, nil
	}
	return dshLaunch(b.executablePath)
}

// dshDefaultPermissionMode keeps an unattended Daemon run from blocking on a
// tool-approval prompt.
const dshDefaultPermissionMode = "danger-full-access"

// dshEnvironment adds the variables DSH needs beyond the inherited environment.
//
// DSH_HOME is passed through so the Daemon and DSH agree on where credentials,
// settings and session logs live.
//
// DSH_PERMISSION_MODE is set because the launcher has no permission flag: the
// mode is read from the environment when a session is created. An operator- or
// caller-supplied value always wins.
func dshEnvironment(extra map[string]string) map[string]string {
	env := map[string]string{}
	for key, value := range extra {
		env[key] = value
	}
	if _, ok := os.LookupEnv("DSH_HOME"); !ok {
		if home, err := os.UserHomeDir(); err == nil {
			env["DSH_HOME"] = filepath.Join(home, ".dsh")
		}
	}
	if _, ok := env["DSH_PERMISSION_MODE"]; !ok {
		env["DSH_PERMISSION_MODE"] = dshDefaultPermissionMode
	}
	return env
}

// dshResolveRoute picks the provider route and model for one execution.
func dshResolveRoute(opts *ExecuteOptions) (provider, model string) {
	provider = strings.TrimSpace(os.Getenv("DSH_PROVIDER"))
	if provider == "" {
		provider = dshDefaultProvider
	}
	model = strings.TrimSpace(os.Getenv("DSH_MODEL"))
	if model == "" {
		model = dshDefaultModel
	}
	if opts != nil {
		if configured := strings.TrimSpace(opts.Model); configured != "" {
			model = configured
		}
	}
	return provider, model
}

// ── Persistent state ─────────────────────────────────────────────────────────

// dshPersistentState is the live state of one DSH subprocess across turns.
type dshPersistentState struct {
	runner    *persistentRunner
	client    *dshClient
	sessionID string
	model     string
	provider  string
	// settled closes when the turn in flight finishes. Internal cleanup waits on
	// it instead of consuming the single value on the result channel, which
	// belongs to the caller.
	settled <-chan struct{}
}

var _ SessionStater = (*dshPersistentState)(nil)

func (s *dshPersistentState) IsAlive() bool         { return s.runner.isAlive() }
func (s *dshPersistentState) SessionID() string     { return s.sessionID }
func (s *dshPersistentState) Done() <-chan struct{} { return s.runner.done }
func (s *dshPersistentState) Notify(msg string) error {
	return s.runner.write([]byte(msg))
}

// turnSettled reports a channel that closes when the state's current turn ends.
func (s *dshPersistentState) turnSettled() <-chan struct{} { return s.settled }

// ── JSON-RPC client ──────────────────────────────────────────────────────────

type dshClient struct {
	logger *slog.Logger
	stdin  interface{ Write([]byte) (int, error) }

	mu      sync.Mutex
	nextID  int
	pending map[int]*pendingRPC

	// callbackMu guards the per-turn sink; a turn owns these while it runs.
	callbackMu   sync.Mutex
	onChunk      func(OutputChunk)
	onTurnDone   func(aborted bool, reason string)
	onTurnFailed func(error)

	sessionID string

	usageMu sync.Mutex
	usage   TokenUsage

	// textSeen tracks whether any assistant text was streamed for the active
	// turn, so a turn that ends without text can still report diagnostics.
	textMu   sync.Mutex
	textSeen bool
}

// dshRequest is one client-to-server JSON-RPC request.
type dshRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// dshFrame is the tolerant view of any inbound line: DSH interleaves its own
// diagnostics, so a line that is not a JSON-RPC frame is skipped by the reader.
type dshFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

type dshInitializeResult struct {
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type dshContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type dshSessionEvent struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Data json.RawMessage `json:"data"`
}

func (c *dshClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	pr := &pendingRPC{ch: make(chan rpcResult, 1), method: method}
	c.pending[id] = pr
	c.mu.Unlock()

	payload, err := json.Marshal(dshRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		c.dropPending(id)
		return nil, err
	}
	if _, err := c.stdin.Write(append(payload, '\n')); err != nil {
		c.dropPending(id)
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case res := <-pr.ch:
		return res.result, res.err
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	}
}

func (c *dshClient) dropPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// closeAllPending fails every in-flight request, which unblocks a turn waiting
// on session/prompt when the process dies mid-turn.
func (c *dshClient) closeAllPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int]*pendingRPC)
	c.mu.Unlock()
	for _, pr := range pending {
		pr.ch <- rpcResult{err: err}
	}
}

// handleLine routes one inbound line. Non-JSON diagnostics and unknown methods
// are ignored: DSH writes its own log lines to the same stream.
func (c *dshClient) handleLine(line string) {
	var frame dshFrame
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return
	}

	if len(frame.ID) > 0 && frame.Method == "" {
		c.handleResponse(frame)
		return
	}
	if frame.Method != "" {
		c.handleNotification(frame)
	}
}

func (c *dshClient) handleResponse(frame dshFrame) {
	var id int
	if err := json.Unmarshal(frame.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	pr, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return
	}
	if frame.Error != nil {
		message := strings.TrimSpace(frame.Error.Message)
		if message == "" {
			message = "unknown JSON-RPC error"
		}
		pr.ch <- rpcResult{err: fmt.Errorf("%s: %s", pr.method, message)}
		return
	}
	pr.ch <- rpcResult{result: frame.Result}
}

// handleNotification maps DSH notifications onto Solo turn events.
func (c *dshClient) handleNotification(frame dshFrame) {
	switch frame.Method {
	case "session.event":
		var params struct {
			SessionID string          `json:"sessionId"`
			Event     dshSessionEvent `json:"event"`
		}
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			return
		}
		if !c.ownsSession(params.SessionID) {
			return
		}
		c.handleSessionEvent(params.Event)
	case "session.status":
		var params struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			return
		}
		if !c.ownsSession(params.SessionID) {
			return
		}
		// Only the transition into "running" belongs to the active turn. The
		// "idle" that follows turn/end describes the resting agent, and the turn
		// channels are already closed by then, so surfacing it would race the
		// completion signal.
		if params.Status != "running" {
			return
		}
		c.emit(OutputChunk{Type: string(MessageStatus), Content: params.Status, SessionID: params.SessionID})
	}
}

func (c *dshClient) ownsSession(sessionID string) bool {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	return c.sessionID == "" || sessionID == c.sessionID
}

// handleSessionEvent translates one DSH session-log event into Solo output and
// turn termination.
func (c *dshClient) handleSessionEvent(event dshSessionEvent) {
	switch event.Type {
	case "assistant/chunk":
		c.handleAssistantChunk(event.Data)
	case "turn/end":
		var data struct {
			Reason struct {
				Kind  string `json:"kind"`
				Error *struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			} `json:"reason"`
		}
		_ = json.Unmarshal(event.Data, &data)
		kind := strings.TrimSpace(data.Reason.Kind)
		message := ""
		if data.Reason.Error != nil {
			message = strings.TrimSpace(data.Reason.Error.Message)
		}
		c.finishTurn(kind, message)
	}
}

// dshChunk is one assistant stream chunk. DSH emits block-start/delta/block-end
// triples per content block plus usage and finish markers.
type dshChunk struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Delta string `json:"argumentsDelta"`
	Usage *struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
	} `json:"usage"`
	Reason *struct {
		Kind  string `json:"kind"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"reason"`
	Block *dshContentBlockLike `json:"block"`
}

// dshContentBlockLike is the completed content block carried by block-end.
type dshContentBlockLike struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsErr bool   `json:"isError"`
}

func (c *dshClient) handleAssistantChunk(raw json.RawMessage) {
	var data struct {
		Chunk dshChunk `json:"chunk"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return
	}
	chunk := data.Chunk

	switch chunk.Type {
	case "reasoning-delta":
		if chunk.Text == "" {
			return
		}
		c.emit(OutputChunk{Type: string(MessageThinking), Content: chunk.Text})
	case "tool-call-delta":
		// Arguments stream in fragments; the tool name arrives with the first.
		c.emit(OutputChunk{
			Type: string(MessageToolUse),
			Tool: &ToolInfo{Name: chunk.Name, CallID: chunk.ID, Input: dshToolInput(chunk.Delta)},
		})
	case "block-end":
		if chunk.Block == nil {
			return
		}
		// Only the completed text block is emitted: DSH streams text as
		// block-start -> deltas -> block-end? (it does not emit text deltas
		// today), so the block is the text payload. Reasoning and tool blocks
		// were already reported through their deltas.
		if chunk.Block.Type == "text" && chunk.Block.Text != "" {
			c.markText()
			c.emit(OutputChunk{Type: string(MessageText), Content: chunk.Block.Text})
		}
	case "usage":
		if chunk.Usage != nil {
			c.usageMu.Lock()
			c.usage = TokenUsage{
				InputTokens:      chunk.Usage.InputTokens,
				OutputTokens:     chunk.Usage.OutputTokens,
				CacheReadTokens:  chunk.Usage.CacheReadTokens,
				CacheWriteTokens: chunk.Usage.CacheWriteTokens,
			}
			c.usageMu.Unlock()
		}
	case "finish":
		// The authoritative turn boundary is `turn/end`; this marker only
		// reports how the model call ended, so it is not terminal here.
	}
}

// dshToolInput parses a streamed tool-argument fragment. Fragments are not
// necessarily complete JSON, so anything unparseable is kept as raw text.
func dshToolInput(fragment string) map[string]any {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(fragment), &parsed); err != nil {
		return map[string]any{"_raw": fragment}
	}
	return parsed
}

func (c *dshClient) markText() {
	c.textMu.Lock()
	c.textSeen = true
	c.textMu.Unlock()
}

func (c *dshClient) tookText() bool {
	c.textMu.Lock()
	defer c.textMu.Unlock()
	return c.textSeen
}

func (c *dshClient) emit(chunk OutputChunk) {
	c.callbackMu.Lock()
	sink := c.onChunk
	c.callbackMu.Unlock()
	if sink != nil {
		sink(chunk)
	}
}

// finishTurn converts a DSH turn reason into the turn outcome.
func (c *dshClient) finishTurn(kind, message string) {
	c.callbackMu.Lock()
	done := c.onTurnDone
	failed := c.onTurnFailed
	c.callbackMu.Unlock()

	switch kind {
	case "completed":
		if done != nil {
			done(false, "")
		}
	case "aborted", "interrupted":
		if done != nil {
			done(true, "")
		}
	case "max-tokens", "blocked":
		if failed != nil {
			if message == "" {
				message = "turn ended: " + kind
			}
			failed(fmt.Errorf("%s", message))
		}
	default:
		if failed != nil {
			if message == "" {
				message = "turn ended: " + kind
			}
			failed(fmt.Errorf("%s", message))
		}
	}
}

// prepareTurn installs this turn's sink and clears per-turn bookkeeping. Callers
// must not start a turn while another is running.
func (c *dshClient) prepareTurn(
	onChunk func(OutputChunk),
	onDone func(aborted bool, reason string),
	onFailed func(error),
) {
	c.callbackMu.Lock()
	c.onChunk = onChunk
	c.onTurnDone = onDone
	c.onTurnFailed = onFailed
	c.callbackMu.Unlock()

	c.textMu.Lock()
	c.textSeen = false
	c.textMu.Unlock()

	c.usageMu.Lock()
	c.usage = TokenUsage{}
	c.usageMu.Unlock()
}

// turnUsage returns the token usage DSH reported for the active turn.
func (c *dshClient) turnUsage() TokenUsage {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	return c.usage
}

// ── Prompt assembly ──────────────────────────────────────────────────────────

// dshContentBlocks renders a Solo prompt as DSH content blocks.
func dshContentBlocks(prompt string) []dshContentBlock {
	return []dshContentBlock{{Type: "text", Text: prompt}}
}

// dshInitializeParams is the process-wide handshake payload.
func dshInitializeParams(cwd string, opts *ExecuteOptions) map[string]any {
	provider, model := dshResolveRoute(opts)
	params := map[string]any{
		"cwd":      cwd,
		"provider": provider,
		"model":    model,
	}
	if opts != nil {
		if effort := strings.TrimSpace(opts.Effort); effort != "" {
			params["reasoningEffort"] = effort
		}
	}
	return params
}

// ── Turn plumbing shared by Start/Send/Execute ───────────────────────────────

// dshTurn bundles one turn's delivery channels and completion.
type dshTurn struct {
	msgCh        chan OutputChunk
	resCh        chan *Result
	deliveryDone chan struct{}
	settled      chan struct{}
	finishOnce   sync.Once
	startedAt    time.Time
}

func newDshTurn() *dshTurn {
	return &dshTurn{
		msgCh:        make(chan OutputChunk, 256),
		resCh:        make(chan *Result, 1),
		deliveryDone: make(chan struct{}),
		settled:      make(chan struct{}),
		startedAt:    time.Now(),
	}
}

func (t *dshTurn) finish(result *Result) {
	t.finishOnce.Do(func() {
		if result.DurationMs == 0 {
			result.DurationMs = time.Since(t.startedAt).Milliseconds()
		}
		t.resCh <- result
		close(t.deliveryDone)
		close(t.msgCh)
		close(t.resCh)
		// settled lets internal cleanup wait for completion without consuming the
		// single value on resCh, which belongs to the caller.
		close(t.settled)
	})
}

func (t *dshTurn) emit(chunk OutputChunk) {
	if chunk.Context != nil {
		sendContextChunk(t.deliveryDone, t.msgCh, chunk)
		return
	}
	trySend(t.msgCh, chunk)
}

// Start launches a persistent DSH process and issues the first prompt.
func (b *DshBackend) Start(ctx context.Context, req *ExecuteRequest, opts *ExecuteOptions) (*PersistentSession, error) {
	execPath, args, err := b.resolveLaunch()
	if err != nil {
		return nil, err
	}
	b.logger.Info("dsh: starting persistent session", "exec", execPath, "args", args)

	runner, err := startPersistent(ctx, execPath, args, opts.WorkspaceDir, dshEnvironment(opts.Env), b.logger)
	if err != nil {
		return nil, err
	}

	provider, model := dshResolveRoute(opts)
	sessionID := uuid.NewString()
	turn := newDshTurn()
	client := &dshClient{
		logger:    b.logger,
		stdin:     runner.stdin,
		pending:   make(map[int]*pendingRPC),
		sessionID: sessionID,
	}
	client.prepareTurn(
		turn.emit,
		func(aborted bool, _ string) {
			if aborted {
				turn.finish(&Result{Status: "cancelled", Error: "turn was aborted"})
				return
			}
			turn.finish(dshTurnResult(client, model))
		},
		func(err error) { turn.finish(&Result{Status: "failed", Error: err.Error()}) },
	)

	go b.readLoop(runner, client)

	// Handshake. The cwd is the agent workspace so DSH's file tools and its
	// session log both land there.
	cwd := strings.TrimSpace(opts.WorkspaceDir)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	initCtx, cancelInit := context.WithTimeout(context.Background(), 60*time.Second)
	raw, err := client.request(initCtx, "initialize", dshInitializeParams(cwd, opts))
	cancelInit()
	if err != nil {
		runner.close()
		return nil, fmt.Errorf("dsh persistent initialize: %w", err)
	}
	var initResult dshInitializeResult
	if err := json.Unmarshal(raw, &initResult); err != nil || initResult.ServerInfo.Name == "" {
		runner.close()
		return nil, fmt.Errorf("dsh persistent initialize: unexpected result %s", strings.TrimSpace(string(raw)))
	}
	b.logger.Info("dsh: initialized", "server", initResult.ServerInfo.Name, "version", initResult.ServerInfo.Version, "provider", provider, "model", model)

	if err := b.prompt(ctx, client, sessionID, buildPrompt(req, opts)); err != nil {
		runner.close()
		return nil, err
	}

	state := &dshPersistentState{
		runner:    runner,
		client:    client,
		sessionID: sessionID,
		model:     model,
		provider:  provider,
		settled:   turn.settled,
	}

	return &PersistentSession{
		Messages:  turn.msgCh,
		Result:    turn.resCh,
		Stop:      dshStop(runner),
		SessionID: sessionID,
		state:     state,
	}, nil
}

// Send delivers another prompt on the same DSH session.
func (b *DshBackend) Send(ctx context.Context, ps *PersistentSession, messages []Message) (*PersistentSession, error) {
	state, ok := ps.state.(*dshPersistentState)
	if !ok || state == nil {
		return nil, fmt.Errorf("dsh: invalid session state")
	}
	if !state.runner.isAlive() {
		return nil, fmt.Errorf("dsh: session process has exited")
	}

	turn := newDshTurn()
	state.settled = turn.settled
	state.client.prepareTurn(
		turn.emit,
		func(aborted bool, _ string) {
			if aborted {
				turn.finish(&Result{Status: "cancelled", Error: "turn was aborted"})
				return
			}
			turn.finish(dshTurnResult(state.client, state.model))
		},
		func(err error) { turn.finish(&Result{Status: "failed", Error: err.Error()}) },
	)

	if err := b.prompt(ctx, state.client, state.sessionID, buildPromptFromMessages(messages)); err != nil {
		return nil, err
	}

	return &PersistentSession{
		Messages:  turn.msgCh,
		Result:    turn.resCh,
		Stop:      dshStop(state.runner),
		SessionID: state.sessionID,
		state:     state,
	}, nil
}

// Close terminates the DSH session, asking it to shut down first.
func (b *DshBackend) Close(ps *PersistentSession) error {
	state, ok := ps.state.(*dshPersistentState)
	if !ok || state == nil {
		return fmt.Errorf("dsh: invalid session state")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := state.client.request(shutdownCtx, "shutdown", nil); err != nil {
		// A refused shutdown is not fatal: closing stdin still ends the process.
		b.logger.Warn("dsh: shutdown request failed", "error", err)
	}
	cancel()
	err := state.runner.close()
	b.logger.Info("dsh: persistent session closed", "session_id", state.sessionID, "reaped", state.runner.exited.Load())
	return err
}

// ForceClose kills the DSH subprocess without a graceful shutdown.
func (b *DshBackend) ForceClose(ps *PersistentSession) error {
	state, ok := ps.state.(*dshPersistentState)
	if !ok || state == nil {
		return fmt.Errorf("dsh: invalid session state")
	}
	err := state.runner.forceClose()
	b.logger.Info("dsh: persistent session ended", "session_id", state.sessionID, "reaped", state.runner.exited.Load())
	return err
}

// prompt sends one session/prompt request. DSH acknowledges the enqueue
// immediately; the turn's content arrives as session.event notifications.
func (b *DshBackend) prompt(ctx context.Context, client *dshClient, sessionID, text string) error {
	promptCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := client.request(promptCtx, "session/prompt", map[string]any{
		"sessionId":     sessionID,
		"contentBlocks": dshContentBlocks(text),
	}); err != nil {
		return fmt.Errorf("dsh session/prompt: %w", err)
	}
	return nil
}

// readLoop consumes the process output until EOF and fails the active turn.
func (b *DshBackend) readLoop(runner *persistentRunner, client *dshClient) {
	scanner := bufio.NewScanner(runner.stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		client.handleLine(line)
	}
	processErr := fmt.Errorf("dsh process exited unexpectedly")
	if err := scanner.Err(); err != nil {
		processErr = fmt.Errorf("dsh process output failed: %w", err)
	}
	client.closeAllPending(processErr)
	runner.cancel()
	runner.finish()

	client.callbackMu.Lock()
	failed := client.onTurnFailed
	client.callbackMu.Unlock()
	if failed != nil {
		failed(processErr)
	}
}

// dshTurnResult builds the completed-turn result, including token usage.
func dshTurnResult(client *dshClient, model string) *Result {
	result := &Result{Status: "completed"}
	usage := client.turnUsage()
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
		return result
	}
	if strings.TrimSpace(model) == "" {
		model = dshDefaultModel
	}
	result.Usage = map[string]TokenUsage{model: usage}
	return result
}

// dshStop implements PersistentSession.Stop. Because the DSH SDK protocol has no
// cancel request, the only way to stop a running turn is to end the process; the
// session is then reported as no longer alive so callers reopen one. Callers that
// need to keep the conversation should let the turn finish instead.
func dshStop(runner *persistentRunner) func() error {
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() { err = runner.forceClose() })
		return err
	}
}

// Execute runs one prompt in a fresh DSH process and returns its stream.
func (b *DshBackend) Execute(ctx context.Context, req *ExecuteRequest, opts *ExecuteOptions) (*Session, error) {
	ps, err := b.Start(ctx, req, opts)
	if err != nil {
		return nil, err
	}

	state, _ := ps.state.(*dshPersistentState)
	session := &Session{
		Messages:  ps.Messages,
		Result:    ps.Result,
		SessionID: ps.SessionID,
		Stop: func() error {
			if state != nil {
				_ = state.runner.forceClose()
			}
			return nil
		},
	}

	// A one-shot execution owns the process: close it once the turn settles.
	// Waiting on the result channel here would race the caller for the single
	// value it carries.
	go func() {
		if state != nil {
			<-state.turnSettled()
			_ = state.runner.close()
		}
	}()

	return session, nil
}
