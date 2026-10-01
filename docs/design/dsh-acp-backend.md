# DSH over ACP

Status: implementation design (nothing implemented yet)
Scope: switches the DeepSeek Harness agent runtime's wire protocol from the SDK JSON-RPC runtime (`dsh --profile sdk`) to ACP (`dsh --profile acp`). The provider type stays `dsh`, so the session pool, the dispatch contract, the database schema, and the frontend keep their current shape.

## 1. Problem and evidence

The current adapter cannot continue a conversation across processes:

- `DshBackend.Start` mints `sessionID := uuid.NewString()` and never reads `opts.ResumeSessionID` (`pkg/agent/dsh.go:850`, prompt at `:892`; the file has zero references to the field).
- The SDK protocol has no resume method at all: the server exposes `initialize`, `session/prompt`, `shutdown` and creates every session through `ctx.agents.create(...)` (`E:\dsh\deepseek-harness\packages\sdk\server\src\server.ts:246-292`).
- Verified against the installed DSH 0.1.6-alpha.2: process A prompts `session-X`; process B prompting `session-X` answers
  `{"id":2,"error":{"code":-32603,"message":"session \"session-sdk-dup-probe\" already exists"}}`.

Consequences today:

1. The daemon session pool captures the provider id when it sleeps or crashes (`pkg/agent/session.go:438-447`, `:908-944`) and feeds it back through `ExecuteOptions.ResumeSessionID` on the next turn (`:179-181`, `:689-691`) — the dsh backend discards it, so **every wake after `AGENT_SESSION_IDLE_TTL` (default 30 min, `cmd/daemon/handler.go:38`) or any crash creates a new DSH session artifact** under `$DSH_HOME/sessions/<cwd>/`. Old artifacts accumulate and the in-DSH history is never restored; Solo compensates by replaying `ColdStartMessages`, which costs tokens and breaks prefix-cache continuity.
2. `PersistentSession.Stop` is documented as "cancels the current turn … does NOT kill the underlying subprocess" (`pkg/agent/backend.go:182-184`), but the dsh backend implements it as `runner.forceClose()` (`pkg/agent/dsh.go:1039-1046`), and `dshCapabilities()` therefore declares `SafeStop: unsupported` (`pkg/agent/builtins.go:178-187`).

ACP fixes both, and it is already the protocol of five other Solo adapters (opencode, hermes, kimi, kiro, openclaw — `pkg/agent/builtins.go:53-138`). Verified against the installed DSH:

| Probe | Result |
|---|---|
| process A `session/new {cwd, mcpServers:[]}` | returns a session id plus `configOptions` (model select) |
| process B `session/resume {sessionId, cwd, mcpServers:[]}` | succeeds; the follow-up `session/prompt` runs |
| artifacts under `$DSH_HOME/sessions/<cwd>/<id>/` | still exactly one file — resume appends, it does not fork |
| `dsh --profile acp` with stdin closed | exits 0 in ~30 ms (so `Close` = close stdin is unchanged) |

## 2. Product behavior

- A dsh Agent's conversation survives pool sleep, provider-process crash, and daemon restart: the next message continues the same DSH session (same provider session id, same DSH log file) instead of starting a new one.
- `Stop` interrupts the running turn through `session/cancel` and keeps the session warm; agent deletion still force-kills the process.
- `/api/v1/agent-backends` reports dsh as `protocols: ["acp"]` with `safe_stop: supported`.
- Model and reasoning effort become protocol-level per-session settings (`session/set_config_option`) instead of an environment default.
- **Known regression until DSH emits token usage over ACP**: dsh runs report no token usage (§7). Budget and ledger behavior for dsh runs must be an explicit decision, not a silent zero.
- Unchanged: Channels, Threads, Runs, transcripts, message delivery, agent CRUD, the agent create form.

## 3. Domain model, ownership, and lifecycle

Ownership is unchanged; only the process's protocol changes.

| Entity | Owner | Identity | Lifetime |
|---|---|---|---|
| Solo Agent | server (Postgres) | `agents.id` | durable |
| Scoped conversation | daemon session pool | `sessionKey` (`agent:<id>`, `channel:<cid>:agent:<aid>`, `thinking:<nid>`) | process lifetime, survives sleep/crash as `entry.sessionID` |
| DSH ACP process | pool entry | pid | one per live pool entry; stdio; exits 0 on stdin EOF |
| DSH session (the conversation) | DSH | ACP session id (bare UUID, returned by `session/new`) | durable in `$DSH_HOME/sessions/`; resumed by id |
| Provider session row | server (Postgres) | `(agent_id, provider, external_session_id)` | `active` / `rollover_pending` / `closed` |

Lifecycle:

1. **Create** — pool miss → `Start` with empty `ResumeSessionID` → `initialize` → `session/new` → id reported to the server through the existing `session` SSE event → `BindProviderSession` inserts the `agent_sessions` row.
2. **Turn** — `session/prompt`; `session/update` notifications become `OutputChunk`s; the prompt response's `stopReason` settles the turn.
3. **Sleep** — idle TTL reached → the pool stores `entry.sessionID` and closes the process (stdin EOF).
4. **Wake** — next tracked turn → `Start` with `ResumeSessionID = entry.sessionID` → `session/resume`; on any failure, one `session/new` fallback (§8).
5. **Crash** — `watchCrash` observes process exit, marks the entry asleep, preserves the id; the next turn resumes it.
6. **Retire / rollover** — unchanged (`RetireAndStartFreshScopedSession`, `agent_run.go:770-982`); the fresh start must return a different id, which the existing check already enforces (`session.go:330-350`).
7. **Delete** — `ForceClose` kills the process; the DSH session log remains on disk (unchanged).

## 4. Data flow of one turn

```
user message
  └─ server: resolveSessionDispatchTx → resume_session_id (agent_run.go:770-982)
      └─ daemon task (daemonTaskRequest.resume_session_id, agent.go:3351-3399)
          └─ GetOrCreateScopedSession → Start(opts.ResumeSessionID) (session.go:167-192, 689-691)
              ├─ write <workspace>/SOLO.md            (system prompt channel, unchanged)
              ├─ spawn dsh --profile acp [--patch <overlay>]
              ├─ initialize → validate agentCapabilities.sessionCapabilities.resume
              ├─ session/resume {sessionId, cwd, mcpServers:[]}   (fallback: session/new)
              ├─ session/set_config_option(model | reasoning_effort)
              └─ session/prompt {sessionId, prompt:[{type:"text",text}]}
                    ├─ ← session/update … → OutputChunk{text|thinking|tool_use|tool_result|context}
                    ├─ ← session/request_permission → auto-allow (unattended daemon)
                    └─ → {stopReason} → Result{status, output, usage?}
          └─ daemon "session" SSE → agent_sessions.external_session_id upsert
```

Wire mapping to keep parity with the SDK adapter (`pkg/agent/dsh.go:503-716`):

| ACP | Solo | Notes |
|---|---|---|
| `initialize` result `agentCapabilities` | adapter negotiation | fail loud if `sessionCapabilities.resume` is absent |
| `session/new` | provider session id | bare UUID; stored through the existing SSE path |
| `session/resume {sessionId, cwd, mcpServers}` | `ResumeSessionID` | must send `cwd`; DSH verifies the canonical workspace |
| `session/set_config_option {configId:"model", value}` | `ExecuteOptions.Model` | value is the opaque string advertised in `options[]` (JSON `["provider","model"]`) |
| `session/set_config_option {configId:"reasoning_effort", value}` | `ExecuteOptions.Effort` | `""` means provider default |
| `session/prompt {sessionId, prompt}` | turn input | text-only in phase 1 (parity with the SDK path; images are a follow-up) |
| `session/update` `agent_message_chunk` / `agent_thought_chunk` | `text` / `thinking` | same shape the existing ACP adapters consume (`pkg/agent/acp.go:582-606`) |
| `session/update` `tool_call` / `tool_call_update` | `tool_use` / `tool_result` | generic lifecycle only |
| `session/update` `usage_update {used,size}` | `context` chunk | context occupancy only, **not** token usage |
| prompt response `{stopReason}` | `Result.Status` | `end_turn` → completed, `cancelled` → cancelled, `error`/`max_tokens` → failed |
| `session/cancel` (client → agent) | `PersistentSession.Stop` | new helper; keeps the process alive |
| `session/close` | not used | `Close` relies on stdin EOF (verified) so a disposal call is unnecessary |
| stdin EOF | `Close` | verified exit 0 in ~30 ms |

## 5. Persistence, identity, and migration

No schema change. `agent_sessions.external_session_id` (nullable, `migrations/000029_create_agent_observability.up.sql:1-15`) already carries the provider id, and the dispatch JSON (`resume_session_id`, `force_fresh_session`, `retire_session_id`, `external_session_id`) does not change.

The migration problem is semantic: existing `provider='dsh'` rows hold ids minted by the SDK runtime, and ACP ids are also bare UUIDs, so they cannot be told apart by shape. A stale id passed to `session/resume` fails (unknown session) and would leave the row `active` while a new row is bound.

Resolution — one additive data migration:

```sql
-- 0000NN_retire_dsh_sdk_sessions.up.sql
UPDATE agent_sessions
   SET status = 'closed'
 WHERE provider = 'dsh'
   AND external_session_id IS NOT NULL
   AND status IN ('active', 'rollover_pending');
```

`resolveSessionDispatchTx` drops a `closed` binding and cold-starts (`internal/server/service/agent_run.go:827-845`), so the first post-upgrade turn starts a clean ACP session instead of attempting a guaranteed-failing resume, and no stale row stays `active`. History is preserved: runs keep their `session_id` FK and the rows keep their ids. The down migration is a documented no-op (the previous statuses cannot be reconstructed safely).

Configuration persistence: Solo writes its own overlay and passes it as a patch, so no profile file is mutated:

```yaml
# $DSH_HOME/solo-acp-overlay.yml  (written atomically at daemon start; idempotent)
- id: agent-instructions          # `id:` replaces the whole config block, so restate maxBytes
  config:
    maxBytes: 65536
    instructionFileCandidates:
      - AGENTS.md
      - CLAUDE.md
      - SOLO.md
```

Launch becomes `dsh --profile acp [--patch <DSH_PATCH>] --patch <solo overlay>`; the Solo-managed patch goes first so a user overlay can still override it. The overlay is required because `session/prompt` has no system-prompt field either, so `SOLO.md` plus `@deepseek-ai/dsh-agent-instructions` remains the only delivery channel (`pkg/agent/dsh.go:157-197`). Verified on the installed DSH: the composed tree then carries `maxBytes: 65536` with `AGENTS.md, CLAUDE.md, SOLO.md`. The `acp` profile itself is a shipped template (`@deepseek-ai/dsh-base` + `@deepseek-ai/dsh-acp-app`) and needs no credentials overlay: the base bundle already mounts `dsh-credentials-local`, `dsh-session-persistence-jsonl` (root only, no compression pin), `dsh-attachment-local`, and `dsh-agent-instructions`.

## 6. Adapter and protocol work

New backend `pkg/agent/dsh_acp.go` (keeping `DshBackend` for one release behind `SOLO_DSH_PROTOCOL=sdk`), registered with the same `Type: "dsh"` so existing agents, revisions, and UI keep working. Required additions in the shared ACP layer (`pkg/agent/acp.go`), none of which the hermes-shaped adapters use yet:

- **Cancel** — `acpClient.cancel(sessionId)` sending the `session/cancel` notification, a `Stop` that settles the turn as `cancelled`, and `Capabilities.SafeStop = supported`. Cancel must work for both `Start`- and `Send`-produced sessions.
- **Config options** — `session/set_config_option` helper plus a mapping from `ExecuteOptions.Model`/`Effort` to the advertised option values. Unknown model or unavailable effort fails the turn with a clear message (parity with the SDK `initialize` handshake, which rejects prompts until the exact route resolves).
- **Resume params** — always send `{sessionId, cwd, mcpServers: []}`; `session/resume` verification of the canonical workspace means `cwd` is part of the resume key.
- **Initialize negotiation** — read `agentCapabilities` and refuse a requested resume when the agent advertises no `sessionCapabilities.resume`, so a stored session is never silently replaced by a fresh one. A first turn that asks for no resume still runs against an agent without resume support.
- **Permission** — answer `session/request_permission` with the first allow-shaped option from the request's own `options[]` instead of the hardcoded `approve_for_session` (`pkg/agent/acp.go:386-395`); the daemon is unattended, so it never escalates to a human.

Capability table for the `dsh` adapter (`pkg/agent/builtins.go:178-187`):

| Capability | Now | After |
|---|---|---|
| `persistent_conversation` | supported | supported |
| `resume_conversation` | supported (not actually true) | supported (true; protocol-level) |
| `busy_message_delivery` | unsupported | unsupported (ACP admits one prompt per session) |
| `safe_stop` | unsupported | **supported** |
| `interactive_input` | unsupported | unsupported |
| `token_usage` | supported | **unknown until §7 lands** |

## 7. Token usage: the one real regression

- DSH's ACP bridge returns `{ stopReason }` only from `session/prompt` (`packages/acp/acp/src/session.ts:313,327` in the installed 0.1.6-alpha.2 and in 0.1.7-rc.2; the published 0.2.0-rc.2 `@deepseek-ai/dsh-acp` bundle contains no `inputTokens`/`cachedReadTokens` reference at all).
- The only usage-bearing update is context occupancy: `usage_update {used, size}` (`packages/acp/acp/src/updates.ts:87-102`).
- Solo's ACP client already parses token usage from the prompt response (`pkg/agent/acp.go:470-497` reads `inputTokens`, `outputTokens`, `cachedReadTokens`), so the gap is entirely on the DSH side.

Options:

- **A (recommended) — upstream.** Return the turn's committed usage as ACP `PromptResponse.usage`. The facts already exist: `assistant/message` events carry `usage?: TokenUsage` (`packages/core/session/src/types.ts`), and the ACP bridge already reads `event.data.usage` for `usageUpdate`. Accumulate it per turn and map to `{inputTokens, outputTokens, cachedReadTokens, cachedWriteTokens, totalTokens}` at the two return sites. No Solo change is needed afterwards.
- **B (interim, mandatory) — declare it.** Until A lands, the adapter reports `token_usage: unknown`, and every dsh run settles as `usage_unknown` in the budget ledger: Solo charges that run's reservation (`SettleRunTx`) instead of an actual count, which is the existing conservative path for a run that reports nothing. The only thing lost is exact per-run numbers, so a stock DSH needs no Solo change at all.
- **C (rejected) — read the DSH session log tail** (`$DSH_HOME/sessions/<cwd-slug>/<id>/session.v*.jsonl.zstd`). Rejected: format versioning (v3 today, v4 exists in the 0.2.0 line), compression variants, slugged paths, and concurrent writers make it a second, silent source of truth.

Rollout gate: the default protocol stays `sdk` until A lands or the product accepts B.

## 8. Failure recovery

- **Resume miss** (unknown id, foreign cwd, format mismatch): log once, fall back to exactly one `session/new`, report the new id. A resume miss never fails the turn.
- **Resume returns a different id**: adopt the returned id (`resolveResumedSessionID`, `pkg/agent/acp.go:910-916`).
- **Process crash mid-turn**: existing `watchCrash` marks the entry asleep and preserves the id; the next tracked turn restarts and resumes.
- **stdin write failure / early EOF**: the turn fails, the entry goes asleep, the next turn starts a fresh process and resumes by id.
- **Cancel**: `Stop` sends `session/cancel`, waits a bounded time for the turn to settle, and reports `cancelled`; `ForceClose` still kills.
- **Boot failure** (missing `acp` profile or plugin): fail loud and keep the existing `dsh --profile acp --dump-config` troubleshooting hint.
- **Concurrency**: one live writer per DSH session; the pool already serializes turns per session key. One pool entry keeps one process for crash isolation even though ACP permits several sessions per connection.
- **Stale sessions**: covered by the §5 migration plus the existing rollover/retire rules.

## 9. API, frontend, and observability compatibility

- `GET /api/v1/agent-backends` (`internal/server/handler/agent.go:856-863`) starts reporting `protocols: ["acp"]` and the capability changes above. The frontend's `use-backend-meta` hook currently drops `capabilities`, so nothing renders differently by itself.
- `backendFamily()` dispatches on `meta.Protocols[0]` (`pkg/agent/island.go:106-123`); moving dsh to the ACP arm switches displayed tool names from snake_case to Title Case. Intentional, covered by extending `pkg/agent/island_test.go:436`.
- `frontend/lib/agent-runtimes.ts:1` already allows `dsh`; `RuntimeLogo` has no dsh case (stays generic); `MODEL_PRESETS` and `supportsCustomModel` exclude dsh, so the create form only offers the default model — out of scope here, listed as a follow-up.
- No HTTP/WebSocket schema change and no new run event type. Optional: an info-level log plus an observability label when a resume miss forces a fresh session.
- `canRecoverMissingSession` (`internal/server/service/agent.go:3401-3405`) stays Claude-only; the ACP backend handles its own resume misses, so the server-side net is not on the critical path.

## 10. Rollout, rollback, docs

- Ship the ACP backend behind `SOLO_DSH_PROTOCOL` (`sdk` default, `acp` opt-in) in the first release, with the full test suite green for both.
- Flip the default once §7 is decided; delete `pkg/agent/dsh.go` and the switch in the following release.
- Rollback: set `SOLO_DSH_PROTOCOL=sdk`. Because the migration closed the old rows, the SDK path cold-starts instead of resuming — degraded but safe, no corruption.
- Rewrite `docs/dsh-backend.md`: the wire table, the `sdk`-profile requirement section, the environment table, and most of the troubleshooting table change; the `SOLO.md` section stays but its reason becomes "ACP has no system-prompt field either".

## 11. Validation (real components, no mocks)

1. **Fake ACP runtime** — rewrite `pkg/agent/dsh_helper_test.go`'s scripted runtime from SDK JSON-RPC to ACP (`initialize`, `session/new`, `session/resume`, `session/prompt`, `session/update`), preserving its strictness property ("a request it does not know becomes a timeout, not a silent pass", `:11-13`).
2. **Shared ACP contract** — add a `dsh` row to `TestStableACPPersistentProviderTurnContract` (`pkg/agent/turn_contract_test.go:308-325`), which already drives a real `/bin/sh` ACP server.
3. **Backend behaviour** — mirror `TestDshBackendFreshStartCreatesANewSession` (`pkg/agent/dsh_test.go:229`) with the opposite assertion: a stored id resumes, one artifact, no new id.
4. **Pool plumbing** — extend `pkg/agent/session_test.go:270` and `:327` with a dsh case proving `Start` receives the stored id and that a resume miss falls back to `session/new` exactly once.
5. **Registry** — add the `dsh` row to the capability matrix (`pkg/agent/builtins_test.go:103`) and `TestBuiltins_PersistentBackend` (`:161`).
6. **Real DSH, gated** — extend `pkg/agent/dsh_e2e_test.go` (`SOLO_E2E_DSH=1` + `DSH_BIN`): turn 1 completed; a second process resumes the same id; the artifact count under the session directory stays 1.
7. **Stack E2E through make** — parameterize `frontend/e2e/agent-result-delivery.spec.ts` with an `SOLO_E2E_AGENT_PROVIDER` (default `claude`) so the same specs run with `model_provider: 'dsh'`, then run `make test-e2e-agent-session-resume` (restart continuity) and `make test-e2e-agent-idle-resume` (`AGENT_SESSION_IDLE_TTL=3s`). Assert, against live Postgres, that `agent_sessions.external_session_id` is unchanged across sleep/wake and that exactly one `active` dsh row exists.
8. **Runtime metadata** — extend `frontend/e2e/runtime-detection.spec.ts:91` with a dsh assertion; run `make test-e2e-m9`.
9. **Usage regression, explicitly** — a test that documents today's behavior (dsh runs carry no usage until DSH emits it) so the gap stays visible instead of silent.

All service-driven validation runs on the make-managed stack (`make rebuild`); services are never started directly.

## 12. Execution steps

| Phase | Work | Done when |
|---|---|---|
| 0 | DSH-side verification only: boot `dsh --profile acp`, confirm `--dump-config` shows credentials / agent-instructions / no compression pin, confirm the Solo overlay adds `SOLO.md`, confirm `session/cancel` and the permission option ids | the overlay file is fixed and every ACP method Solo needs is exercised once by hand |
| 1 | `pkg/agent/acp.go`: cancel, `set_config_option`, resume params, initialize negotiation, permission option selection + unit tests | `go test ./pkg/agent/ -run TestACP -v` |
| 2 | `pkg/agent/dsh_acp.go` + registration behind `SOLO_DSH_PROTOCOL`; rewrite the fake runtime; add the contract and backend tests | `go test ./pkg/agent/ -run 'TestDsh|TestStableACP|TestBuiltins' -v` |
| 3 | Gated real-DSH E2E proving cross-process resume and a single artifact | `SOLO_E2E_DSH=1 DSH_BIN=… go test ./pkg/agent/ -run TestDshAcpE2E -v` |
| 4 | Data migration retiring SDK-era dsh sessions; dispatch/log checks | `go test ./internal/server/service/ -run 'TestAgentSession|TestSessionDispatch'` against real Postgres |
| 5 | Overlay provisioning in the daemon + rewrite `docs/dsh-backend.md` | a fresh daemon start writes the overlay idempotently and the docs match the shipped behavior |
| 6 | Stack E2E (real frontend + API + Postgres + daemon) for resume and idle-resume with a dsh agent | `make rebuild && make test-e2e-agent-session-resume && make test-e2e-agent-idle-resume` |
| 7 | Flip `SOLO_DSH_PROTOCOL` default (only after the §7 decision); delete the SDK backend afterwards | both protocols green in CI, then the SDK path removed |

## 13. Open decisions

1. **Token usage (§7)** — pursue the upstream `PromptResponse.usage` patch (recommended), or accept unknown usage for dsh runs now? This gates phase 7.
2. **Default flip timing** — ship ACP opt-in first (recommended) or make it the default immediately?
3. **Per-Agent protocol switch in the UI** — recommended: no; keep the env kill-switch only.
4. **`session_recovery` generalization** — recommended: defer; the ACP backend self-heals resume misses.

Decisions taken: token usage follows option A, ACP ships opt-in first (`SOLO_DSH_PROTOCOL`, default `sdk`), and the ACP tool-name presentation change is accepted.

## 14. Handoff status

Delivered before this handoff:

- Decision A is implemented upstream, tested, documented, and exercised against a live ACP turn: a settled `session/prompt` returns `usage`. It sits on branch `feat/acp-prompt-turn-usage` in the Harness checkout, so a Solo run against stock DSH still receives no usage until that change ships.
- Phases 0 and 1: the ACP profile, overlay, resume, cancel and permission verification, and the shared ACP client helpers.
- Phase 2: the ACP transport itself, its fixture runtime, the shared provider-turn contract row, the capability table, and the `SOLO_DSH_PROTOCOL` switch.
- Phase 3: the real-DSH end-to-end test that resumes one session across two processes with a single durable artifact.
- Phase 4: migration `000080_retire_dsh_sdk_sessions`, plus a throwaway-database test that applies the shipped migrations around it and a dispatch test that pins a retired session cold-starting.
- Phase 5: the ACP transport writes `$DSH_HOME/solo-acp-overlay.yml` before every launch (atomic, idempotent, refuses a foreign file, applied before `DSH_PATCH`), and `docs/dsh-backend.md` documents both transports.

Prerequisites for the remaining phases:

| Requirement | Why |
|---|---|
| A Go toolchain with working cgo, plus `make` | `make rebuild` owns the service lifecycle; services are never started directly |
| PostgreSQL and the frontend toolchain | phases 4 and 6 assert UI and database state |
| `DEEPSEEK_API_KEY` or `$DSH_HOME/.credentials.yaml` | phases 3 and 6 run real model turns |
| A DSH install carrying the `usage` change | otherwise phase 3 asserts the response without usage |

Remaining phases and their entry points:

1. **Phase 6** — `make rebuild`, then `make test-e2e-agent-session-resume` and `make test-e2e-agent-idle-resume` against a `dsh` agent.
2. **Phase 7** — flip `SOLO_DSH_PROTOCOL` and delete the SDK backend.

Carried-forward unknowns:

- Permission option ids are read from the request (`allow_always`, then `allow_once`, then a permit-named option). DSH advertises `allow-once` / `reject-once` and treats every other answer as a rejection, which the phase-1 selection satisfies; it is unit-tested but not yet exercised against a live approval.
- `session/cancel` is wired through the client helper and the ACP backend's Stop; the live cancel path runs in the fixture only, not yet against a real approval or interruption.
- Phases 4 to 7 have no code yet.

Running the phase-3 acceptance:

```sh
SOLO_E2E_DSH=1 \
DSH_BIN=/path/to/dsh/lib/bin.js \
DSH_HOME=/path/to/harness-home \
go test ./pkg/agent/ -run TestDshAcpE2EResumesOneSessionAcrossProcesses -v
```

The DSH install must return turn usage from `session/prompt`; set `SOLO_E2E_DSH_ALLOW_NO_USAGE=1` for one that does not. A Harness home reached through a Windows drive mount can fail the credentials file's owner-only check, so point `DSH_HOME` at a filesystem that reports real modes.
