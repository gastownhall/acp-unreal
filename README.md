# unreal-acp (spike)

`unreal-acp` is an [ACP v1](https://agentclientprotocol.com) agent over stdio. It hosts
[Unreal Agent](https://github.com/unreallabsai/unreal-agent) sessions (library pinned at
`v0.1.1` = `b7c9bf1`) inside one long-lived process. The process owner can be gc, acpx,
or any other ACP client. It does not fork Unreal Agent and has no `replace` directives.
It uses only the library's public seams.

stdout carries JSON-RPC only. Logs go to stderr.

## Build and test

```sh
go build -o bin/unreal-acp ./cmd/unreal-acp
go test ./... -count=1                                # unit + contract + e2e (~20s)
UNREAL_ACP_E2E_RACE=1 go test -race ./e2e/ -count=1   # also race-instruments the agent binary
```

The Go 1.27 toolchain is selected through `GOTOOLCHAIN=auto`. The e2e tests need Linux
because they scan `/proc` to detect orphaned processes.

## Flags (env fallback)

| flag | default | notes |
|---|---|---|
| `--base-url` (`UNREAL_ACP_BASE_URL`) | required | OpenAI Responses-compatible base URL; `/responses` is appended |
| `--model` (`UNREAL_ACP_MODEL`) | required | default model |
| `--models` | | csv of models offered by the `model` config option |
| `--api-key-env` | `UNREAL_ACP_API_KEY` | the NAME of the key variable. The key is read, then that variable and `UNREAL_HARNESS_LLM_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY` and `OLLAMA_API_KEY` are unset before any tool runs. `GC_*` and `BEADS_*` are kept. |
| `--state-dir` | `$XDG_STATE_HOME/unreal-acp` | holds `sessions/`, `meta/`, `locks/` and `operations/`. It is refused if it is inside the cwd or the session workspace. |
| `--permission-mode` | `auto` | `auto` \| `ask` \| `allowlist` |
| `--allow` | | csv of command prefixes that allowlist mode runs without asking. Commands that contain shell control characters (`;&|`, backtick, `$`, `<>()`, newline, backslash) always ask. |
| `--permission-timeout` | `0` (forever) | when it expires the request is treated as `reject_once` |
| `--shell` | `/bin/bash` | never taken from `$SHELL` |
| `--cancel-grace` | `1s` | wait before SIGKILLing the tool process groups of a cancelled turn |
| `--shutdown-budget` | `4s` | keep this below the owner's kill grace (gc: 5s) |
| `--context-window` | `131072` | size reported in `usage_update` |
| `--max-update-text` | `16384` | per-chunk and per-tool-result text cap, so every stdout line stays under 1 MiB |
| `--op-timeout` | `0` | wall clock for each Bash call |
| `--session-id K` / `--resume K` | | bound mode for hosts other than gc (see below) |

`<workspace>/.env` is never loaded.

## Session identity (bound mode)

The first successful `session/new` of a process is bound, checked in this order:

1. **gc launch:** `GC_SESSION_ID` is set. The session id is `gc-<sanitize(GC_SESSION_ID)>-e<GC_CONTINUATION_EPOCH|1>`. The session is opened if it exists and created otherwise, with no replay. A gc restart resumes the conversation, and `gc session reset` bumps the epoch, which starts a fresh session.
2. **`--session-id K`:** create K. It is an error if K already exists.
3. **`--resume K`:** open K without replay. If K is missing, the process exits with code 3 and prints `unknown session` before it answers `initialize`.

In any other case the id is a fresh uuid. `sanitize` maps characters outside
`[A-Za-z0-9-]` to `-`. When it maps anything, it appends `-` plus the first 8 hex digits of
`sha256(raw)`.

Only one process may open a session. The lock is a kernel `flock` on
`<state>/locks/<id>.lock`. A second opener gets `session busy`.

## Method mapping

- `initialize`: protocol 1, `loadSession`, embedded context, and `sessionCapabilities{resume,list,close}`. `_meta.unrealAcp` carries `{harness, permissionMode, steerMethod}`.
- `session/new`, `session/load`, `session/resume`, `session/list`, `session/close`: `load` replays every item and makes zero provider requests. `resume` attaches without replay. `close` cancels the turn, StopHards the generation, drains the operations and unlocks.
- `session/prompt`: `text`, `resource` and `resource_link` blocks are flattened to text. `image` and `audio` are rejected. The response is sent when the scheduling mirror shows the coordinator is quiescent. It carries the summed usage and `_meta.model`, which is the model the provider reports serving.
- `session/cancel` and SIGINT cancel in place, in this order:
  1. The Gate cancels the model request. Partial text is kept.
  2. Outstanding operations are cancelled.
  3. After `--cancel-grace`, the recorded tool process groups are SIGKILLed.
  4. `tool_call_update(failed)` is flushed for any call not yet reported.
  5. The prompt is answered `cancelled`.

  The Gate then mutes requests that only answer cancelled calls, so no tokens are spent until the next prompt.
- `session/set_config_option`: `model` sets the Gate's per-request model override. `thought_level` is sent as an inbox `UpdateSettings` control.
- `_unreal-acp/steer {sessionId, text, mode: queue|interrupt}`: `queue` submits the text at the next response boundary. `interrupt` submits it now, and the coordinator supersedes the in-flight request.
- `session/request_permission` is sent before a gated Bash call starts. The options are `allow_once`, `allow_always`, `reject_once` and `reject_always`. A rejection becomes a synthesized canceled operation, so the model sees the reason.
- SIGTERM, SIGHUP and stdin EOF shut down gracefully within `--shutdown-budget`: turns are cancelled, the manager is drained, the remaining recorded tool groups are SIGKILLed, and the process exits 0 with no orphans.

## gc provider

```toml
[providers.unreal]
supports_acp = true
command      = "/data/projects/acp-spike/unreal-acp/bin/unreal-acp"
acp_command  = "/data/projects/acp-spike/unreal-acp/bin/unreal-acp"
acp_args     = ["--permission-mode", "auto"]
# No session_id_flag/resume_flag: continuity comes from bound mode
# (GC_SESSION_ID + GC_CONTINUATION_EPOCH, injected by gc).
# Pass UNREAL_ACP_BASE_URL, UNREAL_ACP_MODEL, UNREAL_ACP_API_KEY via env.
```

Use `auto` under gc. gc's ACP reader currently drops agent-to-client requests
(`internal/runtime/acp/conn.go`), so a `session/request_permission` would never be answered.
If you do use ask mode under gc, set `--permission-timeout`.

## Layout

- `cmd/unreal-acp`: flags, env scrub, signals and stdio wiring.
- `cmd/livesmoke`: a live-provider smoke client that prints counts only.
- `internal/acpagent`: the SDK Agent, bound mode and prompt flattening.
- `internal/runtime`: the per-session actor, generations, close rule, cancel, replay, close, the state layout and locks, and the contract tests.
- `internal/{gate,tap,ops,mirror,project,fifo}`: the llm.Adapter decorator, the SSE delta tee, the operation.Manager decorator, the scheduling mirror, the pure ACP projection, and the queue.
- `internal/testfake/responses`: the scripted fake of the Responses API. Its `cmd/` serves it for manual smoke tests.
- `e2e/`: subprocess tests E1-E10 plus close and steer.
- `smoke/`: acpx and live smoke artifacts.

Design patterns (Gate, mirror, parked cancel) follow andreylukin/bough (Apache-2.0). The code
here is independently written from the spike POC.
