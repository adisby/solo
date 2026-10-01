package agent

import (
	"log/slog"
	"os"
	"strings"
)

func persistentCapabilities(safeStop CapabilityStatus) BackendCapabilities {
	return BackendCapabilities{
		PersistentConversation: CapabilitySupported,
		ResumeConversation:     CapabilitySupported,
		BusyMessageDelivery:    CapabilityUnsupported,
		SafeStop:               safeStop,
		InteractiveInput:       CapabilityUnsupported,
		TokenUsage:             CapabilitySupported,
	}
}

func oneShotCapabilities() BackendCapabilities {
	return BackendCapabilities{
		PersistentConversation: CapabilityUnsupported,
		ResumeConversation:     CapabilityUnsupported,
		BusyMessageDelivery:    CapabilityUnsupported,
		SafeStop:               CapabilitySupported,
		InteractiveInput:       CapabilityUnsupported,
		TokenUsage:             CapabilitySupported,
	}
}

// init registers all built-in backend adapters into the global registry
// when the agent package is imported. Each adapter carries its metadata
// (display name, binary requirements, protocols) and a factory function
// that constructs Backend instances from BackendConfig.
func init() {
	r := GlobalRegistry()

	// ── claude — Claude Code CLI via stream-json ──────────────────────
	r.Register(claudeMeta("claude", "Claude Code"), claudeFactory)
	r.Register(claudeMeta("local", "Local Claude Code"), claudeFactory)

	// ── codex — Codex CLI via JSON-RPC ───────────────────────────────
	r.Register(AdapterMeta{
		Type:              "codex",
		DisplayName:       "Codex CLI",
		RequiresBinary:    "codex",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"CODEX_BIN"},
		Protocols:         []string{"json-rpc"},
		Capabilities:      persistentCapabilities(CapabilitySupported),
	}, codexFactory)

	// ── opencode — OpenCode CLI via ACP ─────────────────────────────
	r.Register(AdapterMeta{
		Type:              "opencode",
		DisplayName:       "OpenCode CLI",
		RequiresBinary:    "opencode",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"OPENCODE_BIN"},
		Protocols:         []string{"acp"},
		Capabilities:      persistentCapabilities(CapabilityUnsupported),
	}, opencodeFactory)

	// ── cursor — Cursor Agent CLI via stream-json ───────────────────
	r.Register(AdapterMeta{
		Type:              "cursor",
		DisplayName:       "Cursor Agent",
		RequiresBinary:    "cursor-agent",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"CURSOR_BIN"},
		Protocols:         []string{"stream-json"},
		Capabilities:      oneShotCapabilities(),
	}, cursorFactory)

	// ── gemini — Google Gemini CLI via stream-json ──────────────────
	r.Register(AdapterMeta{
		Type:              "gemini",
		DisplayName:       "Gemini CLI",
		RequiresBinary:    "gemini",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"GEMINI_BIN"},
		Protocols:         []string{"stream-json"},
		Capabilities:      oneShotCapabilities(),
	}, geminiFactory)

	// ── kimi — Kimi CLI via ACP ─────────────────────────────────────
	r.Register(AdapterMeta{
		Type:              "kimi",
		DisplayName:       "Kimi CLI",
		RequiresBinary:    "kimi",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"KIMI_BIN"},
		Protocols:         []string{"acp"},
		Capabilities:      persistentCapabilities(CapabilityUnsupported),
	}, kimiFactory)

	// ── kiro — Kiro CLI via ACP ─────────────────────────────────────
	r.Register(AdapterMeta{
		Type:              "kiro",
		DisplayName:       "Kiro CLI",
		RequiresBinary:    "kiro-cli",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"KIRO_BIN"},
		Protocols:         []string{"acp"},
		Capabilities:      persistentCapabilities(CapabilityUnsupported),
	}, kiroFactory)

	// ── copilot — GitHub Copilot CLI via JSONL ──────────────────────
	r.Register(AdapterMeta{
		Type:              "copilot",
		DisplayName:       "GitHub Copilot",
		RequiresBinary:    "copilot",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"COPILOT_BIN"},
		Protocols:         []string{"jsonl"},
		Capabilities:      oneShotCapabilities(),
	}, copilotFactory)

	// ── openclaw — OpenClaw Agent CLI via ACP ───────────────────────
	r.Register(AdapterMeta{
		Type:              "openclaw",
		DisplayName:       "OpenClaw Agent",
		RequiresBinary:    "openclaw",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"OPENCLAW_BIN"},
		Protocols:         []string{"acp"},
		Capabilities:      persistentCapabilities(CapabilityUnsupported),
	}, openclawFactory)

	// ── hermes — Hermes CLI via ACP ─────────────────────────────────
	r.Register(AdapterMeta{
		Type:              "hermes",
		DisplayName:       "Hermes CLI",
		RequiresBinary:    "hermes",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"HERMES_BIN"},
		Protocols:         []string{"acp"},
		Capabilities:      persistentCapabilities(CapabilityUnsupported),
	}, hermesFactory)

	// ── pi — Pi CLI via JSONL ───────────────────────────────────────
	r.Register(AdapterMeta{
		Type:              "pi",
		DisplayName:       "Pi CLI",
		RequiresBinary:    "pi",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"PI_BIN"},
		Protocols:         []string{"jsonl"},
		Capabilities:      oneShotCapabilities(),
	}, piFactory)

	// ── dsh — DeepSeek Harness over ACP or its SDK JSON-RPC runtime ─
	// DSH_BIN normally points at a .js entry point. On POSIX that script runs
	// through its own shebang; Windows has no shebang support, so the script
	// resolver turns it into a node invocation. The metadata follows the
	// transport SOLO_DSH_PROTOCOL selects, because protocols and capabilities
	// differ between them.
	r.RegisterWithScriptResolver(dshMeta(), dshFactory, resolveScriptCommand)
}

// dshProtocol names the transport the dsh adapter speaks. ACP is the default;
// SOLO_DSH_PROTOCOL=sdk selects the SDK JSON-RPC runtime, which stays shipped
// until the ACP transport has run in production.
func dshProtocol() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SOLO_DSH_PROTOCOL")), "sdk") {
		return "sdk"
	}
	return "acp"
}

// dshMeta describes the dsh adapter for the selected transport.
func dshMeta() AdapterMeta {
	meta := AdapterMeta{
		Type:              "dsh",
		DisplayName:       "DeepSeek Harness",
		RequiresBinary:    "dsh",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"DSH_BIN"},
		Capabilities:      dshCapabilities(),
		Protocols:         []string{"json-rpc"},
	}
	if dshProtocol() == "acp" {
		meta.Protocols = []string{"acp"}
		meta.Capabilities = dshAcpCapabilities()
	}
	return meta
}

// dshCapabilities describes what the SDK JSON-RPC transport integrates, not what
// DSH could do on its own.
//
// Persistent conversation, resume and token usage are supported: the adapter
// reuses one DSH session id across Send calls and reads the turn's usage chunk.
// Resume here means the session pool keeping a live process alive; DSH also
// rebuilds a session from its own log, but the adapter does not yet feed a stored
// session id back on a fresh process, so a restart starts a new conversation.
//
// Busy message delivery and interactive input are unsupported: the SDK protocol
// has no cancel request, so Stop ends the process instead of interrupting a turn,
// and there is no stdin prompt path.
func dshCapabilities() BackendCapabilities {
	return BackendCapabilities{
		PersistentConversation: CapabilitySupported,
		ResumeConversation:     CapabilitySupported,
		BusyMessageDelivery:    CapabilityUnsupported,
		SafeStop:               CapabilityUnsupported,
		InteractiveInput:       CapabilityUnsupported,
		TokenUsage:             CapabilitySupported,
	}
}

// dshAcpCapabilities describes what the ACP transport integrates.
//
// Resume is protocol-level: session/new mints the DSH session id and
// session/resume restores it, so a sleeping or restarted process continues the
// same conversation. Stop cancels the active turn through session/cancel and
// keeps the process, so an interrupted Agent is safe to steer again.
//
// Token usage is unknown rather than supported: ACP carries the turn's counters
// only from a DSH build that returns them, and every turn still reports context
// occupancy through usage_update. Busy message delivery and interactive input
// stay unsupported because ACP admits one prompt per session.
func dshAcpCapabilities() BackendCapabilities {
	return BackendCapabilities{
		PersistentConversation: CapabilitySupported,
		ResumeConversation:     CapabilitySupported,
		BusyMessageDelivery:    CapabilityUnsupported,
		SafeStop:               CapabilitySupported,
		InteractiveInput:       CapabilityUnsupported,
		TokenUsage:             CapabilityUnknown,
	}
}

// claudeMeta builds an AdapterMeta for the claude and local backends.
// They share the same metadata except for their Type and DisplayName fields.
func claudeMeta(typ, displayName string) AdapterMeta {
	return AdapterMeta{
		Type:              typ,
		DisplayName:       displayName,
		RequiresBinary:    "claude",
		DetectCommand:     "--version",
		BinaryOverrideEnv: []string{"CLAUDE_BIN", "CLAUDECODE_BIN"},
		Protocols:         []string{"stream-json"},
		Capabilities:      persistentCapabilities(CapabilityUnsupported),
	}
}

// ── Factory functions ────────────────────────────────────────────────────────

// logOrDefault returns logger if non-nil, otherwise slog.Default.
func logOrDefault(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.Default()
}

// execPathOrDefault resolves the binary path from cfg.ExecPath or an
// environment variable. If both are empty the constructor will fall
// back to its own default (typically the binary name).
func execPathOrDefault(cfgExecPath, envVar string) string {
	if cfgExecPath != "" {
		return cfgExecPath
	}
	return os.Getenv(envVar)
}

func claudeFactory(cfg BackendConfig) (Backend, error) {
	execPath := cfg.ExecPath
	if execPath == "" {
		execPath = os.Getenv("CLAUDE_BIN")
	}
	if execPath == "" {
		execPath = os.Getenv("CLAUDECODE_BIN")
	}
	return NewClaudeBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func codexFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "CODEX_BIN")
	return NewCodexBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func opencodeFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "OPENCODE_BIN")
	return NewOpenCodeBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func cursorFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "CURSOR_BIN")
	return NewCursorBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func geminiFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "GEMINI_BIN")
	return NewGeminiBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func kimiFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "KIMI_BIN")
	return NewKimiBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func kiroFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "KIRO_BIN")
	return NewKiroBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func copilotFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "COPILOT_BIN")
	return NewCopilotBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func openclawFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "OPENCLAW_BIN")
	return NewOpenClawBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func hermesFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "HERMES_BIN")
	return NewHermesBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func piFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "PI_BIN")
	return NewPiBackend(execPath, logOrDefault(cfg.Logger)), nil
}

func dshFactory(cfg BackendConfig) (Backend, error) {
	execPath := execPathOrDefault(cfg.ExecPath, "DSH_BIN")
	logger := logOrDefault(cfg.Logger)
	if dshProtocol() == "acp" {
		return NewDshAcpBackend(execPath, logger), nil
	}
	return NewDshBackend(execPath, logger), nil
}
