# DSH backend

Solo runs Agents on [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)
(DSH) through one of two transports, both registered under the `dsh` adapter type:
[ACP](https://agentclientprotocol.com) (`dsh --profile acp`) and the SDK JSON-RPC
runtime (`dsh --profile sdk`). ACP is opt-in through `SOLO_DSH_PROTOCOL=acp`; every
other value keeps the SDK runtime, which is still the default.

The transports are `pkg/agent/dsh_acp.go` and `pkg/agent/dsh.go`. The Daemon
resolves the executable from `DSH_BIN` (or `dsh` on `PATH`), and the adapter's
reported protocols and capabilities follow the selected transport.

## Which transport

| Behaviour | ACP (`SOLO_DSH_PROTOCOL=acp`) | SDK (default) |
|---|---|---|
| Provider session identity | `session/new` mints the DSH session id; `session/resume` restores it | the adapter mints one id per process, so a restart starts a new conversation |
| Idle sleep and wake | the next turn resumes the same DSH session | the next turn starts a new one; Solo replays cold-start context |
| Stop | `session/cancel` cancels the turn and keeps the session usable | ends the process, because the protocol has no cancel request |
| Model and reasoning effort | `session/set_config_option` per session | the `initialize` route, validated at handshake |
| Token usage | reported when the DSH build returns usage from `session/prompt`; context occupancy always arrives through `usage_update` | reported from the committed `assistant/message` |
| Reported capabilities | protocols `acp`, `safe_stop` supported, `token_usage` unknown | protocols `json-rpc`, `safe_stop` unsupported, `token_usage` supported |

Choose ACP when a conversation has to survive the Daemon's idle sleep, a provider
crash, or a Daemon restart. Choose the SDK runtime when the DSH install predates
the ACP profile or when per-turn token accounting matters more than continuity.

## What the ACP integration maps

| ACP wire element | Solo behaviour |
|---|---|
| `initialize` | handshake per process; the response's `agentCapabilities` are read, and a requested resume fails loud when the agent advertises no `session/resume` |
| `session/new { cwd, mcpServers }` | a fresh provider session; `cwd` is the Agent workspace |
| `session/resume { sessionId, cwd, mcpServers }` | restores the stored session, or falls back to exactly one `session/new` when the agent rejects it |
| `session/set_config_option { configId: "model" }` | the Agent's `model_name`, resolved against the values the session advertises |
| `session/set_config_option { configId: "reasoning_effort" }` | the Agent's effort level, with the empty value meaning the provider default |
| `session/prompt { sessionId, prompt }` | one turn; the response's `stopReason` becomes the turn status, and its `usage` becomes `Result.Usage` |
| `session/update` notifications | `agent_message_chunk` and `agent_thought_chunk` become text and thinking chunks, `tool_call`/`tool_call_update` become tool chunks, `usage_update` becomes a context chunk |
| `session/cancel` | `PersistentSession.Stop`; the process and the session stay alive |
| `session/request_permission` | answered with an advertised permit (`allow_always`, then `allow_once`, then a permit-named option); the Daemon is unattended and never escalates to a human |
| stdin end | `Close`; a DSH ACP process exits 0 on end of input |

A turn the agent reports as `cancelled` stays cancelled in Solo, and any other
stop reason names itself in the failure.

## What the SDK integration maps

| DSH wire element | Solo behaviour |
|---|---|
| `initialize { cwd, provider, model, reasoningEffort }` | handshake per process; `cwd` is the Agent workspace |
| `session/prompt { sessionId, contentBlocks }` | one turn; an unknown `sessionId` lazily creates the session |
| `session.event` → `assistant/message` | primary turn output: `reasoning` and `text` content blocks become `thinking` and `text` chunks, `tool-call` becomes `tool_use`, and `usage` becomes the turn's `Result.Usage` |
| `session.event` → `assistant/chunk` | the finer-grained shape, used for turns that end before a message completes |
| `session.event` → `turn/end` | turn outcome: `completed`, `cancelled`, or `failed` |
| `session.status` → `running` | a `status` output chunk |
| `shutdown` | graceful close on session teardown |

Observed against DSH 0.1.6-alpha.2 and re-confirmed on 0.2.0-rc.2: the SDK runtime
reports a completed `assistant/message`, not per-delta chunks. Handling only
`assistant/chunk` produces a turn that completes with no output at all.

## System prompt delivery

Neither transport has a system-prompt field: the ACP `session/prompt` and the SDK
`initialize` accept no such value. The only channel is
`@deepseek-ai/dsh-agent-instructions`, which loads instruction files from the
workspace, so both transports write `opts.SystemPrompt` to **`<cwd>/SOLO.md`**
before launching DSH.

Two properties of that mechanism matter:

- **The name must be a bare file in the session cwd.** Candidate resolution is
  same-directory only, so a subdirectory path passes config validation and then
  never matches.
- **The file must be listed by the profile.** The shipped candidates are
  `AGENTS.md` and `CLAUDE.md`, so the ACP transport writes its own patch layer and
  passes it with `--patch`:

  `$DSH_HOME/solo-acp-overlay.yml`

  ```yaml
  # Generated by Solo for the dsh ACP profile; edits are overwritten on the next launch.
  - id: agent-instructions
    config:
      maxBytes: 65536
      instructionFileCandidates:
        - AGENTS.md
        - CLAUDE.md
        - SOLO.md
  ```

  The write is atomic and idempotent, an identical file is left untouched, and a
  file without Solo's marker line is reported instead of overwritten. The managed
  layer is applied before the operator's `DSH_PATCH` overlay, so an operator layer
  still wins. Workspace `AGENTS.md` and `CLAUDE.md` keep loading alongside
  `SOLO.md`, so the Agent keeps its project knowledge.

Without the overlay, the Agent never receives the operating contract that
`BuildSystemPrompt` assembles — including `solo message send`, the command it is
expected to deliver replies with. The failure is not a clean error: the Agent
reasons about how to reach the channel, hunts for an HTTP endpoint instead, and
burns its whole turn until the 6-minute execution watchdog cancels it with zero
tokens.

## Requirements

1. **DSH with the selected profile.** `acp` and `sdk` are shipped templates that
   initialize on first use. The ACP profile is base-backed, so credentials, session
   persistence, attachments, and the instructions plugin are already mounted; the
   SDK profile needs the overlay described in [Setting up the SDK profile](#setting-up-the-sdk-profile).
2. **Credentials.** The `deepseek-official` route reads `DEEPSEEK_API_KEY` through
   DSH's credentials service (`$DSH_HOME/.credentials.yaml`, written by the DSH web
   Models page) or from the launching environment. The Models page shows the stored
   key read-only; to replace it, revoke the key in the provider console, create a
   new one, and write it into `refs.DEEPSEEK_API_KEY`. Never touch the `secret:`
   field: it is the encryption material for the whole document. DSH refuses to load
   a credentials file that is readable beyond its owner, which a Windows drive
   mounted into Linux reports as mode 777.
3. **Turn usage (ACP only).** A DSH build whose `session/prompt` response carries
   `usage` gives Solo per-turn token accounting. Without it, `token_usage` stays
   `unknown` and every dsh run settles as `usage_unknown`: Solo charges the run's
   reservation instead of an actual count, so a budget stays conservative rather
   than silently unspent, and the run reads as unknown rather than as zero.
   Context occupancy still arrives through `session/update`.

### Setting up the SDK profile

`sdk` is not a shipped profile name; create it from the SDK bundle.
`@deepseek-ai/dsh-sdk-app` builds its own Cordis tree with an `insert:` list and
deliberately does **not** layer over `@deepseek-ai/dsh-base`, so what dsh-base
would otherwise provide is missing and must be restored by an overlay.

`$DSH_HOME/profiles/sdk/package.json`

```json
{
  "name": "dsh-profile-sdk",
  "private": true,
  "dsh": {
    "profile": {
      "bundles": [
        "@deepseek-ai/dsh-base",
        "@deepseek-ai/dsh-sdk-app"
      ]
    }
  }
}
```

`$DSH_HOME/profiles/sdk/cordis.yml` containing `[]`.

`$DSH_HOME/solo-sdk-overlay.yml`, passed as `DSH_PATCH`

```yaml
- insert:
    - id: credentials
      name: '@deepseek-ai/dsh-credentials-local'
```

- **The credentials service is required on every version tested.** Without
  `@deepseek-ai/dsh-credentials-local`, `llm-deepseek`'s
  `apiKeyEnv: DEEPSEEK_API_KEY` has nothing to read from and every turn fails with
  `no API key for provider route "deepseek-official"`, even though `dsh headless`
  works fine with the same home.
- **zstd session-log compression is needed on 0.1.6-alpha.2 only**, where the
  bundle pins session persistence to `compression: none`, which conflicts with a
  home whose logs another profile wrote as `.jsonl.zstd`. On 0.2.0-rc.2 the config
  carries no `compression` key at all, and carrying the older entry there fails
  outright with `patch: entry "sessions" not found`.

An `id:` entry **replaces** that entry's whole config block, so `root` must be
repeated for any entry that is overridden. Check what a given install actually
resolves before trusting either shape:

```bash
dsh --profile sdk --patch "$DSH_HOME/solo-sdk-overlay.yml" --dump-config
```

## Session identity, resume, and retirement

A session lives in `agent_sessions`, keyed by
`(agent_id, provider, external_session_id)`. The Daemon reports the provider id it
discovered, the Server binds it to the run, and the next turn for that Agent
carries it back as `resume_session_id`.

- ACP ids come from `session/new` and are restored by `session/resume`. A resume
  the agent rejects starts exactly one fresh session, and a resume the agent does
  not advertise fails loud rather than silently replacing the conversation.
- The Daemon sleeps an idle Agent session (`AGENT_SESSION_IDLE_TTL`, 30 minutes by
  default) by closing the provider process while keeping that id, so the next turn
  resumes instead of starting over.
- Rows written by the SDK transport were never resumable. Migration
  `000080_retire_dsh_sdk_sessions` closes every `dsh` row that carries a provider
  session id, so dispatch cold-starts instead of attempting a resume that can only
  fail; run history keeps its session references.

## Environment variables

| Variable | Purpose |
|---|---|
| `DSH_BIN` | Path to the DSH launcher. A `.js` entry point is run through `node`; anything else must be an executable on disk or on `PATH`. |
| `SOLO_DSH_PROTOCOL` | `acp` selects the ACP transport; any other value keeps the SDK runtime. |
| `DSH_HOME` | Inherited by DSH so the Daemon and DSH share credentials, settings, and session logs. Defaults to `~/.dsh`. An Agent's `custom_env` value wins over the process environment. |
| `DSH_PERMISSION_MODE` | Tool-approval policy. Solo defaults it to `danger-full-access` because the Daemon runs unattended; the launcher has no permission flag, so this is an environment variable. |
| `DSH_PATCH` | Extra patch overlay applied after Solo's own ACP overlay. |
| `DSH_PROVIDER` / `DSH_MODEL` | Provider route and model defaults for the SDK transport. An Agent's configured model overrides `DSH_MODEL`; on ACP the Agent's model is applied through `session/set_config_option` and must be one the session advertises. |

## Verification

Unit tests drive both transports against fixture runtimes — real subprocesses that
speak the protocol — so no model call is needed:

```bash
go test ./pkg/agent/ -run TestDsh -v
go test ./pkg/agent/ -run 'TestACP|TestStableACP' -v
```

The end-to-end tests boot a real DSH process and spend model tokens, so they are
gated:

```bash
SOLO_E2E_DSH=1 DSH_BIN=/path/to/dsh go test ./pkg/agent/ \
  -run TestDshAcpE2E -v
```

They prove that a second process resumes the session the first one created with a
single durable artifact, and that the workspace instructions reach the model.
Set `SOLO_E2E_DSH_ALLOW_NO_USAGE=1` when the DSH build returns no turn usage.

The make-managed resume targets (`make test-e2e-agent-session-resume`,
`make test-e2e-agent-idle-resume`) exercise the same continuity contract through
the real frontend, API server, Daemon, and PostgreSQL for the shipped provider
defaults.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `does not advertise session/resume` | The installed DSH cannot restore the stored session. Update it, or clear the Agent's stored provider session. |
| `session does not offer model "…"; it offers …` | The Agent's `model_name` is not one the ACP session advertises. Pick one of the listed models. |
| `credentials-local: … is readable beyond its owner (mode 777)` | The credentials file sits on a filesystem that cannot express owner-only modes. Run DSH on a filesystem that can, or export `DEEPSEEK_API_KEY`. |
| `unknown option '--permission-mode'` | The permission mode was passed as a flag. It belongs in `DSH_PERMISSION_MODE`. |
| `no API key for provider route` while `dsh headless` works | The `sdk` profile is missing the `credentials` service (requirement 1). |
| A turn completes with no output and no usage | The SDK adapter only saw `turn/end`; the runtime reported `assistant/message`, which must be handled. |
| `uses .jsonl.zstd, but this backend is configured for compression "none"` | The session compression overlay is missing. Applies to the SDK profile on 0.1.6-alpha.2 only. |
| `patch: entry "sessions" not found` | The zstd overlay was copied to a DSH version where that service does not exist. On 0.2.0-rc.2 it is `session-persistence-jsonl`, and no compression override is needed. |
| `… exists and is not Solo's overlay` | A hand-written `$DSH_HOME/solo-acp-overlay.yml` is in the way. List `SOLO.md` in that file's candidates, or move it aside. |
| `multiple online daemons; computer selection is required` (HTTP 503 from `/api/v1/agent-backends/detect`) | Not a DSH fault. Several Computers are registered, so detection needs an explicit `?computer_id=`. |
| `$.root missing required value` | A patch override dropped `root`; an `id:` entry replaces the whole config block. |
| `dsh process exited unexpectedly` during initialize | The launcher failed to boot, usually a missing or invalid profile. Run `dsh --profile acp --dump-config` to see why. |
