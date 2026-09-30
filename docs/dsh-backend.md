# DSH backend

Solo can run Agents on [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)
(DSH) through its SDK runtime: a newline-delimited JSON-RPC 2.0 session over
stdio, served by the `@deepseek-ai/dsh-sdk-jsonrpc-server` plugin inside a
`--profile sdk` launch.

The adapter is `pkg/agent/dsh.go`. It registers the runtime type `dsh`, and the
Daemon resolves the executable from `DSH_BIN` (or `dsh` on `PATH`).

## What the integration maps

| DSH wire element | Solo behaviour |
|---|---|
| `initialize { cwd, provider, model, reasoningEffort }` | handshake per process; `cwd` is the Agent workspace |
| `session/prompt { sessionId, contentBlocks }` | one turn; an unknown `sessionId` lazily creates the session |
| `session.event` → `assistant/message` | primary turn output: `reasoning` and `text` content blocks become `thinking` and `text` chunks, `tool-call` becomes `tool_use`, and `usage` becomes the turn's `Result.Usage`. The accumulated text also lands in `Result.Output` |
| `session.event` → `assistant/chunk` | the finer-grained shape (`reasoning-delta`, `tool-call-delta`, `block-end`, `usage`, `finish`), used for turns that end before a message completes |
| `session.event` → `turn/end` | turn outcome: `completed`, `cancelled`, or `failed` |
| `session.status` → `running` | `status` output chunk (the resting `idle` is not turn output) |
| `shutdown` | graceful close on session teardown |

Observed against DSH 0.1.6-alpha.2: the SDK runtime reports a **completed
`assistant/message`**, not per-delta chunks. Handling only `assistant/chunk`
produces a turn that completes with no output at all.

Reusing one `sessionId` across turns keeps the conversation, so the persistent
path needs no extra protocol. The adapter always mints a fresh id for a new
process, so a restart starts a new conversation even though DSH could rebuild the
old one from its session log. The SDK protocol has no cancel request, so `Stop`
ends the process instead of interrupting a turn.

## Requirements

1. **DSH with an `sdk` profile.** `sdk` is not a shipped profile name; create it
   from the SDK bundle. `@deepseek-ai/dsh-sdk-app` builds its own Cordis tree with
   an `insert:` list and deliberately does **not** layer over
   `@deepseek-ai/dsh-base`, so two things dsh-base would otherwise provide are
   missing and must be restored by an overlay:

   - **the credentials service.** Without `@deepseek-ai/dsh-credentials-local`,
     `llm-deepseek`'s `apiKeyEnv: DEEPSEEK_API_KEY` has nothing to read from and
     every turn fails with `no API key for provider route "deepseek-official"`,
     even though `dsh headless` works fine with the same home.
   - **zstd session-log compression.** The bundle pins session persistence to
     `compression: none`, which conflicts with a home whose logs another profile
     wrote as `.jsonl.zstd`.

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

   `$DSH_HOME/solo-sdk-overlay.yml` (passed as `DSH_PATCH`)

   ```yaml
   - insert:
       - id: credentials
         name: '@deepseek-ai/dsh-credentials-local'

   - id: sessions
     config:
       root: !!js dshHomePath('sessions')
       compression: zstd
   ```

   Note that an `id:` entry **replaces** that entry's whole config block, which is
   why `root` is repeated for the sessions override.

   Verify without starting a session:

   ```
   dsh --profile sdk --patch "$DSH_HOME/solo-sdk-overlay.yml" --dump-config
   ```

2. **Credentials.** The `deepseek-official` route reads `DEEPSEEK_API_KEY`
   through DSH's credentials service (`$DSH_HOME/.credentials.yaml`, written by
   the DSH web Models page) or from the environment of the launching process.
   The Models page shows the stored key read-only; to replace it, revoke the key
   in the provider console, create a new one, and write it into
   `refs.DEEPSEEK_API_KEY`. Never touch the `secret:` field: it is the encryption
   material for the whole document.

## Environment variables

| Variable | Purpose |
|---|---|
| `DSH_BIN` | Path to the DSH launcher. A `.js` entry point is run through `node`; anything else must be an executable on disk or on `PATH`. |
| `DSH_HOME` | Inherited by DSH so the Daemon and DSH share credentials, settings, and session logs. Defaults to `~/.dsh`. |
| `DSH_PERMISSION_MODE` | Tool-approval policy. Solo defaults it to `danger-full-access` because the Daemon runs unattended; the launcher has no permission flag, so this is an environment variable. |
| `DSH_PATCH` | Extra patch overlay (for example the compression overlay above). |
| `DSH_PROVIDER` / `DSH_MODEL` | Provider route and model. Defaults: `deepseek-official` / `deepseek-flash`. An Agent's configured model overrides `DSH_MODEL`. |

## Verification

Unit tests drive the whole adapter against a fixture runtime — a real
subprocess that speaks the protocol — so no model call is needed:

```bash
go test ./pkg/agent/ -run TestDsh -v
```

The end-to-end test boots a real DSH process and spends model tokens, so it is
gated:

```bash
SOLO_E2E_DSH=1 DSH_BIN=/path/to/dsh go test ./pkg/agent/ \
  -run TestDshBackendExecuteAgainstRealRuntime -v
```

## Troubleshooting

| Symptom | Cause |
|---|---|
| `unknown option '--permission-mode'` | The permission mode was passed as a flag. It belongs in `DSH_PERMISSION_MODE`. |
| `no API key for provider route` while `dsh headless` works | The `sdk` profile is missing the `credentials` service (requirement 1). |
| A turn completes with no output and no usage | The adapter only saw `turn/end`; the runtime reported `assistant/message`, which must be handled (see the mapping table). |
| `uses .jsonl.zstd, but this backend is configured for compression "none"` | The session compression overlay is missing (requirement 1). |
| `$.root missing required value` | A patch override dropped `root`; an `id:` entry replaces the whole config block. |
| `dsh process exited unexpectedly` during initialize | The launcher failed to boot, usually a missing or invalid profile. Run `dsh --profile sdk --dump-config` to see why. |
