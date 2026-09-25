# acp-unreal

`acp-unreal` is an [Agent Client Protocol](https://agentclientprotocol.com) (ACP v1)
agent over stdio that hosts [Unreal Agent](https://github.com/unreallabsai/unreal-agent)
sessions. Any ACP client can drive it: [Gas City](https://github.com/gastownhall/gascity)
(`gc`), [acpx](https://www.npmjs.com/package/acpx), or an editor.

Unreal Agent is a Go library for coding agents that talk to an OpenAI
Responses-compatible model API and run shell tools. It ships a library and a one-shot
runner, not an ACP server. `acp-unreal` adds the long-lived process around it:
streaming, cancel, tool approvals, resumable sessions, and clean shutdown. It uses
only the library's public API (pinned at `v0.1.1`), with no fork and no `replace`
directives.

stdout carries only JSON-RPC. Logs go to stderr.

## Install

```sh
go install github.com/gastownhall/acp-unreal/cmd/acp-unreal@v0.1.0
acp-unreal --version
```

Go 1.27 or newer is required (`GOTOOLCHAIN=auto` downloads it). The process-hygiene
features (the detached-descendant sweep, `/proc` checks) are Linux-only; see [Limitations](#limitations).

Minimal run against any Responses-compatible endpoint:

```sh
export ACP_UNREAL_API_KEY=...          # or use --api-key-file
acp-unreal --base-url https://ollama.com/v1 --model gpt-oss:120b
```

## Flags

| flag (env fallback) | default | notes |
|---|---|---|
| `--base-url` (`ACP_UNREAL_BASE_URL`) | required | OpenAI Responses-compatible base URL; `/responses` is appended |
| `--model` (`ACP_UNREAL_MODEL`) | required | the launch model. It applies to every session that has no client override (see [Models](#models-and-reasoning-effort)) |
| `--models` | | comma-separated models offered by the `model` config option. When set, `session/set_config_option` accepts only these (plus `--model`) |
| `--api-key-env` | `ACP_UNREAL_API_KEY` | the NAME of the variable holding the API key |
| `--api-key-file` | | read the key from this file instead. The file must not be readable or writable by group or others (`chmod 600`) |
| `--scrub-env` | | extra variable names removed from the environment tools inherit |
| `--keep-env` | | variable names kept even though their names look like credentials |
| `--state-dir` | `$XDG_STATE_HOME/acp-unreal` (else `~/.local/state/acp-unreal`) | holds `sessions/`, `meta/`, `locks/` and `operations/`. Refused if it is inside the cwd or the session workspace |
| `--permission-mode` | `auto` | `auto` \| `ask` \| `allowlist` |
| `--allow` | | comma-separated command prefixes that `allowlist` mode runs without asking |
| `--permission-timeout` | `0` (wait forever) | when it expires the request counts as `reject_once` |
| `--shell` | `/bin/bash` | absolute path; never taken from `$SHELL` |
| `--cancel-grace` | `1s` | wait before SIGKILLing the tool process groups of a cancelled turn |
| `--shutdown-budget` | `4s` | keep it below the owner's kill grace (gc: 5s) |
| `--op-timeout` | `0` (unbounded) | wall clock per Bash call. A command that outlives it by `--cancel-grace` is SIGKILLed |
| `--context-window` | `131072` | size reported in `usage_update` |
| `--max-update-text` | `16384` | cap per text chunk, tool result and tool input, so every stdout line stays far below 1 MiB |
| `--max-attempts` | `3` | provider attempts per model request |
| `--prompt-cache-key` | `false` | send the session id as `prompt_cache_key` |
| `--system-prompt` | short coding-agent preamble | the working directory is appended |
| `--session-id K` / `--resume K` | | bound mode for hosts other than gc (see below) |
| `--version` | | print the version and exit |

`<workspace>/.env` is never loaded.

## Sessions

### Bound mode

The first successful `session/new` of a process is bound. Checked in this order:

1. **gc launch.** `GC_SESSION_ID` is set. The session id is
   `gc-<sanitize(GC_SESSION_ID)>-e<GC_CONTINUATION_EPOCH|1>`. The session is opened if
   it exists and created otherwise, with no replay. A gc restart resumes the
   conversation, and `gc session reset` bumps the epoch, which starts a fresh session.
   `GC_SESSION_ID` is ignored when `ACP_UNREAL_PARENT_PID` is set. `acp-unreal`
   exports that variable to its tools, so an `acp-unreal` started by a tool does not
   try to bind to its parent's session.
2. **`--session-id K`.** Create K. It is an error if K exists.
3. **`--resume K`.** Open K without replay. If K is missing, the process prints
   `unknown session` and exits with code 3 before it answers `initialize`.

Otherwise the id is a fresh UUID. `sanitize` maps characters outside `[A-Za-z0-9-]`
to `-`. When it maps anything, it appends `-` and the first 8 hex digits of
`sha256(raw)`.

Only one process may open a session. The lock is a kernel `flock` on
`<state>/locks/<id>.lock`, so a crashed process never leaves a stale lock. A second
opener gets `session busy`.

### Models and reasoning effort

The session's model is the launch `--model` unless an ACP client picked one with
`session/set_config_option` (`model`). Only such an explicit choice is stored with the
session. So when an operator changes `--model` (for example a gc pack option), existing
sessions pick it up on their next start.

`thought_level` offers `default` (send no reasoning effort) plus `low`, `medium`,
`high`, `xhigh` and `max`. Both options carry their ACP `category` (`model`,
`thought_level`), and `currentValue` is the value actually sent.

### Message ids

Every `agent_message_chunk`, `agent_thought_chunk` and `user_message_chunk` carries a
UUID `messageId`. Agent message ids are name-based UUIDs (version 5) over the session
id, the provider's response id and the kind. The live stream and a `session/load`
replay therefore use the same id for the same message, and ids never repeat across
prompts, loads or restarts. A client-supplied `messageId` on `session/prompt` is kept
when it is a UUID and returned as `userMessageId`.

## Method mapping

- **`initialize`**: protocol 1, `loadSession`, embedded context, and
  `sessionCapabilities{resume, list, close}`. `_meta.acpUnreal` carries
  `{harness, permissionMode, steerMethod}`.
- **`session/new`, `session/load`, `session/resume`**: `load` replays every item and
  makes no provider request. `resume` attaches without replay.
- **`session/list`**: pages of 100 sessions ordered by id, with `nextCursor`.
- **`session/close`**: cancels the turn, stops the coordinator, drains the tools and
  releases the lock. The process stays up.
- **`session/prompt`**: `text`, `resource` and `resource_link` blocks are flattened
  to text. `image` and `audio` are rejected. The response is sent when the session is
  quiescent. It carries the summed `usage` and `_meta.model`, the model the provider
  reports serving.
- **`session/cancel`**: see [Cancel](#cancel).
- **`session/set_config_option`**: `model` and `thought_level`.
- **`_acp-unreal/steer {sessionId, text, mode: queue|interrupt}`**: `queue` delivers
  the text at the next response boundary. `interrupt` delivers it now, and the
  in-flight model request is superseded.
- **`session/request_permission`**: sent before a gated Bash call starts (see
  [Permission modes](#permission-modes)).
- **Not supported**: `session/set_mode` (method not found), `authenticate` is a no-op
  (credentials come from the environment or `--api-key-file`), MCP servers (see
  [Limitations](#limitations)).

Before any `tool_call_update`, the client has seen a `tool_call` for that
`toolCallId` on the same connection. This matters after a bound-mode restart: the next
turn reports the interrupted call, and it is announced first.

## Permission modes

| mode | behavior |
|---|---|
| `auto` | every command runs without asking |
| `ask` | every Bash call sends `session/request_permission` with `allow_once`, `allow_always`, `reject_once` and `reject_always` |
| `allowlist` | commands matching an `--allow` prefix run; others ask. A command with shell control characters (`;&|`, backtick, `$`, `<>()`, newline, backslash) always asks |

A rejection or a timeout becomes a cancelled tool call whose text tells the model what
happened, and the command never runs. `allow_always` and `reject_always` last for the
session in this process.

## Cancel

`session/cancel` and SIGINT cancel the turn in place:

1. The in-flight model request is cancelled. Streamed text is kept.
2. Outstanding tool calls are cancelled; a call still waiting for approval is
   withdrawn (the client sees `$/cancel_request` for the permission request).
3. After `--cancel-grace`, the recorded tool process groups are SIGKILLed, so a tool
   that ignores SIGTERM cannot hold the turn open.
4. `tool_call_update(failed)` is sent for any call not yet reported.
5. The prompt is answered with `stopReason: cancelled`. It is never answered with a
   JSON-RPC error, even when a provider error happened earlier in the turn.

No tokens are spent after a cancel until the next prompt. The requests that would only
answer the cancelled calls are muted.

## Signals and shutdown

| event | effect |
|---|---|
| SIGINT | cancel every active turn; the process stays up (this is gc's cooperative interrupt) |
| SIGTERM, SIGHUP, stdin EOF | graceful shutdown within `--shutdown-budget`: cancel turns, stop the coordinators, drain the tools, SIGKILL any remaining tool process groups, stop detached descendants (below), exit 0 |
| client death (stdin EOF with stdout and stderr broken) | the same graceful shutdown. SIGPIPE is caught, so writes fail with `EPIPE` instead of killing the process before it cleans up |

**Which descendants are stopped.** Every process `acp-unreal` starts inherits
`ACP_UNREAL_OWNERS`, a colon-separated list that includes a random token of this
`acp-unreal` process. A tool descendant that leaves its tool's process group (a
`setsid` daemon, `tmux new -d`) is not in a group `acp-unreal` kills, so at shutdown
`acp-unreal` scans `/proc` (Linux) for live processes whose environment carries its
token and, when `GC_SESSION_ID` is set, the same `GC_SESSION_ID`. Each one gets
SIGTERM, then SIGKILL if it is still running 500ms later. Single processes are
signalled, never whole process groups.

This is the same rule gc's session orphan sweep uses. gc removes `GC_SESSION_ID` from
the city infrastructure it detaches on purpose (a supervisor respawned by `gc start`,
the managed Dolt server and its watchdog), so a tool that starts those does not get
them killed when the agent stops. Outside gc there is no such hand-off: a process a
tool detached on purpose is stopped unless it drops `ACP_UNREAL_OWNERS` from its
environment. A process that clears its environment (`env -i`) is not found.
`acp-unreal` is not a subreaper, so detached descendants are re-parented to init (or
the nearest subreaper) and never become zombies of `acp-unreal`.

If `acp-unreal` itself is SIGKILLed, running tools survive in their own process
groups. They carry `GC_SESSION_ID`, so under gc the orphan sweep can reap them.

## Security model

- **Credentials.** Tools inherit the agent's environment, and any same-uid process can
  read `/proc/<pid>/environ` of its ancestors. `os.Unsetenv` does not change that file,
  so at startup `acp-unreal` re-execs itself (same pid and argv) with a scrubbed
  environment and receives the key over an inherited pipe. `--api-key-file` keeps the
  key out of every environment. The scrub removes:
  - the `--api-key-env` variable, `UNREAL_HARNESS_LLM_API_KEY`, `OPENAI_API_KEY`,
    `OPENROUTER_API_KEY`, `OLLAMA_API_KEY` and the `--scrub-env` names; and
  - any variable whose name has a credential component (`API_KEY`, `APIKEY`,
    `ACCESS_KEY`, `SECRET`, `PASSWORD`, `TOKEN`; trailing digits ignored, so
    `OLLAMA_API_KEY2` matches),
  - except `GC_*`, `BEADS_*` (tools need `GC_INSTANCE_TOKEN` and `BEADS_HOLDER_TOKEN`)
    and the `--keep-env` names.
- **Residual risk.** A same-uid process can still read the environment of any other
  ancestor that holds the key: a `sh -c` wrapper that does not `exec` its last command
  (on Debian and Ubuntu `/bin/sh` is dash, which does not), or a supervisor that
  expanded the key into its own environment. Launch with `exec acp-unreal` (the gc
  pack does) and prefer `--api-key-file`. Same-uid is not a security boundary; run
  untrusted work under a separate uid or in a sandbox.
- **State.** Session logs, tool output captures and locks live in `--state-dir`,
  outside the workspace. Tool results shown to the client name it `<state-dir>`. The
  state dir is readable by same-uid tools.
- **Logs.** stderr never contains the key or the `Authorization` header.

## Gas City integration

This repository ships a gc pack, [`pack/pack.toml`](pack/pack.toml), which declares the
`unreal` provider:

- `supports_acp = true`, `acp_command = "exec acp-unreal"` (see the security model).
- `upstream_env` maps an agent upstream's `base_url` and `api_key` onto
  `ACP_UNREAL_BASE_URL` and `ACP_UNREAL_API_KEY`.
- `prompt_mode = "none"`: prompts travel only over ACP. gc delivers a session's
  initial message as the first `session/prompt`. `acp-unreal` refuses positional
  arguments (exit 2), so a message appended to the command is never dropped silently.
- Options: `model` (open: any id is passed as `--model <id>`, default `gpt-oss:120b`),
  `permission_mode` (`auto`, `ask`, `allowlist`), `allow` (open: the comma-separated
  prefixes passed as `--allow`, used by `allowlist`) and `permission_timeout` (open: a
  Go duration, default `10m`; `0` waits forever). A gc that does not answer
  `session/request_permission` would leave an `ask` or `allowlist` turn waiting
  forever, which is why the default timeout is finite: when it expires the command
  is rejected and the turn goes on.
- No `session_id_flag` or `resume_flag`: [bound mode](#bound-mode) keys the
  conversation on `GC_SESSION_ID` and `GC_CONTINUATION_EPOCH`.

[`examples/gascity/city.toml`](examples/gascity/city.toml) is a minimal city: it
imports the pack, declares an upstream with `api_key = "$OLLAMA_API_KEY"` (expanded by
the controller, never inlined), and one agent with `provider = "unreal"`,
`session = "acp"`. With `acp-unreal` on the controller's `PATH`:

```sh
cd examples/gascity
export OLLAMA_API_KEY=...
gc start
```

### Which gc version

Basic sessions (start, prompt, streamed output, restart that resumes, reset that
starts fresh) work with current gc. Some features need gc changes that are in review:

| feature | gc change | without it |
|---|---|---|
| tool approvals in `ask` mode (pending interaction + respond) | `session/request_permission` as a pending interaction (a1b, PR pending) | use `permission_mode = "auto"`; gc cannot answer the request |
| gc answers agent requests it does not serve | `-32601` for unsupported agent requests (a1a, PR pending) | a request gc drops is never answered |
| a `/stop` or interrupt that waits for the turn to settle | `session/cancel` interrupt (a3a) and settling `/stop` (c3), PRs pending | gc's SIGINT is a cooperative cancel: the turn is answered `cancelled`, but gc does not wait for it |
| turn completion and idle waits | a2a, PR pending | |
| configurable stop grace | [#6542](https://github.com/gastownhall/gascity/pull/6542) | gc SIGKILLs after 5s; keep `--shutdown-budget` below it |
| transcripts | [#6544](https://github.com/gastownhall/gascity/pull/6544) (capture) and a4b (read), PR pending | no gc transcript for ACP sessions |
| orphan sweep after an agent crash | [#6543](https://github.com/gastownhall/gascity/pull/6543) | tools of a SIGKILLed agent survive |
| session reset over the API | [#6593](https://github.com/gastownhall/gascity/pull/6593) | use `gc session reset` |

## Other ACP clients

**acpx** (verified with acpx 0.19.2, headless):

```sh
npx -y acpx@0.19.2 --format json --approve-all --cwd "$PWD" \
  --agent 'acp-unreal --base-url https://ollama.com/v1 --model gpt-oss:120b --state-dir /tmp/acp-unreal-state' \
  exec 'hello'
```

acpx's `--approve-all` and `--deny-all` answer only the requests the agent sends, so
add `--permission-mode ask` to the agent command line to be asked.

**Zed** (not verified), in `settings.json`:

```json
{
  "agent_servers": {
    "Unreal Agent": {
      "command": "acp-unreal",
      "args": ["--base-url", "https://ollama.com/v1", "--model", "gpt-oss:120b", "--permission-mode", "ask"],
      "env": { "ACP_UNREAL_API_KEY": "..." }
    }
  }
}
```

**Toad** (not verified): `toad acp "acp-unreal --base-url https://ollama.com/v1 --model gpt-oss:120b"`.

## Limitations

- **MCP servers are ignored.** `mcpServers` in `session/new` is accepted and logged,
  but no MCP connection is made. The ACP spec says agents MUST support stdio MCP
  servers, so this is a known deviation. Unreal Agent has no MCP client.
- **No per-operation output cap.** The library captures full tool output to files in
  the state dir. What reaches the client and the model is bounded, but disk use is not.
- **No `session/set_mode`.** Permission modes are launch flags.
- **Linux for detached descendants.** Finding detached descendants reads `/proc`,
  and the e2e tests need Linux. On other platforms tool process groups are still
  killed, but `setsid`-detached descendants can outlive the agent.

## Troubleshooting

| symptom | cause and fix |
|---|---|
| `--state-dir ... is inside the working directory` | move the state dir out of the workspace (the default under `$XDG_STATE_HOME` is fine) |
| `session busy` | another process holds the session lock. Under gc, a nested `acp-unreal` without `ACP_UNREAL_PARENT_PID` would hit this; current versions set it for tools |
| `unknown session`, exit code 3 | `--resume K` names a session that does not exist in this state dir |
| `--api-key-file ... must not be accessible by group or others` | `chmod 600` the key file |
| a permission request is never answered under gc | the running gc does not serve `session/request_permission` yet; use `--permission-mode auto` or set `--permission-timeout` |
| provider errors | the prompt is answered with a JSON-RPC error after `--max-attempts`; the session stays usable. stderr has the provider message (never the key) |

## Development

```sh
go test ./... -count=1                                   # unit, contract and e2e tests
go test -race ./... -count=1
ACP_UNREAL_E2E_RACE=1 go test -race ./e2e/ -count=3      # also race-instruments the agent binary
```

Layout:

- `cmd/acp-unreal`: flags, credential re-exec, signals and stdio wiring.
- `cmd/livesmoke`: a live-provider smoke client that prints counts only.
- `internal/acpagent`: the ACP agent, bound mode and prompt flattening.
- `internal/runtime`: the per-session actor (generations, turn close rule, cancel,
  replay, close), the state layout and locks, and the library contract tests.
- `internal/gate`, `tap`, `ops`, `mirror`, `project`, `fifo`: the model-adapter
  decorator, the SSE delta tee, the operation-manager decorator, the scheduling mirror,
  the ACP projection and a queue.
- `internal/credenv`, `internal/sweep`: the credential scrub policy and the
  shutdown sweep of detached descendants.
- `internal/testfake/responses`: a scripted fake of the Responses API (`cmd/` serves it
  for manual smoke tests).
- `e2e/`: subprocess tests.
- `pack/`, `examples/gascity/`: the gc pack and an example city.

CI pins `unreal-agent` to `v0.1.1` and runs the contract tests, which fail if a
library behavior this adapter relies on changes. A weekly non-blocking job runs them
against the latest `unreal-agent`.

## Attribution

- [Unreal Agent](https://github.com/unreallabsai/unreal-agent), Copyright (c) 2026
  Unreal Labs, MIT License. The HTTP transport settings in `internal/tap` are adapted
  from it.
- The Gate, scheduling-mirror and parked-cancel designs follow
  [andreylukin/bough](https://github.com/andreylukin/bough) (Apache-2.0). No bough code
  is included.

See [NOTICE](NOTICE). acp-unreal is released under the [MIT License](LICENSE).
