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
| `session.event` → `assistant/chunk` `reasoning-delta` | `thinking` output chunk |
| `session.event` → `assistant/chunk` `tool-call-delta` | `tool_use` output chunk |
| `session.event` → `assistant/chunk` `block-end` (text) | `text` output chunk |
| `session.event` → `assistant/chunk` `usage` | turn `Result.Usage` for the configured model |
| `session.event` → `turn/end` | turn outcome: `completed`, `cancelled`, or `failed` |
| `session.status` → `running` | `status` output chunk (the resting `idle` is not turn output) |
| `shutdown` | graceful close on session teardown |

Reusing one `sessionId` across turns keeps the conversation, so persistent
sessions and resume need no extra protocol. The SDK protocol has no cancel
request, so `Stop` ends the process instead of interrupting a turn.

## Requirements

1. **DSH with an `sdk` profile.** `sdk` is not a shipped profile name; create it
   from the SDK bundle. `@deepseek-ai/dsh-sdk-app` is an incremental layer that
   expects the plugins `dsh-base` provides, so both bundles are needed:

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

   Verify without starting a session:

   ```
   dsh --profile sdk --dump-config
   ```

2. **Session-log compression must match the DSH home.** `dsh-sdk-minimal` pins
   session persistence to `compression: none`, but a DSH home whose sessions were
   written by another profile uses `zstd`, and the persistence layer refuses to
   read a mismatched artifact. Apply an overlay instead of editing the bundle:

   ```yaml
   - id: sessions
     config:
       root: !!js dshHomePath('sessions')
       compression: zstd
   ```

   Pass it with `DSH_PATCH=<path>` (the launcher accepts `--patch` repeatedly),
   and note that an `id:` entry **replaces** the whole config block, so `root`
   must be repeated.

3. **Credentials.** The `deepseek-official` route reads `DEEPSEEK_API_KEY`
   through DSH's credentials service (`$DSH_HOME/.credentials.yaml`, written by
   the DSH web Models page) or from the environment of the launching process.

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
| `no API key for provider route` | The credentials service has no `DEEPSEEK_API_KEY` and the environment does not either. |
| `uses .jsonl.zstd, but this backend is configured for compression "none"` | The session compression overlay is missing (see requirement 2). |
| `$.root missing required value` | A patch override dropped `root`; an `id:` entry replaces the whole config block. |
| `dsh process exited unexpectedly` during initialize | The launcher failed to boot, usually a missing or invalid profile. Run `dsh --profile sdk --dump-config` to see why. |
