# CAS Architecture

**Last updated:** 2026-05-19
**Verified against:** commit `aa5cc11`

---

## What CAS is

CAS is a terminal shell where conversation and direct manipulation coexist in
the same interface. The left panel is a persistent chat; the right panel is a
live workspace (document, code, or list) that tokens stream into as the model
generates. Once generated, the workspace is yours to edit directly.

The design resolves a split in HCI between agent delegation and direct
manipulation: agents generate, users manipulate.

---

## Binary layout

```
cmd/cas/main.go          Entry point. Wires store → shell → TUI and starts
                         the Bubble Tea event loop.

internal/
  intent/     detect.go  Zero-latency regex classifier. Fires before any LLM
                         call. Maps a user message to a Kind and workspace type.
  agent/      agent.go    Named sub-agents with per-agent contracts. Seven agents:
                         GenerationAgent (create), EditAgent (edit), CombineAgent (combine),
                         ChatAgent (chat), MCPAgent (mcp tool calls), WebAgent (web actions),
                         OrchestratorAgent (multi-workspace coordination).
                         Shell delegates to agents; agents own every LLM call.
              mcp_agent.go  MCPAgent: tool-call planning + execution, autonomy dial.
              web_agent.go  WebAgent: web action planning (answer/navigate/extract), autonomy dial.
              orchestrator.go  OrchestratorAgent: multi-workspace task coordination.
                         plan → execute (via StepExecutor) → summarise.
  contract/   contract.go  Design by Contract enforcement. Pre/post/invariant
                         checks on workspace operations. Fail-closed.
  workspace/  workspace.go  Workspace type and lifecycle (create, update, close,
                         history/undo). Three types: document, code, list.
  shell/      shell.go   Session coordinator. Wires intent → workspace →
                         LLM → store → conductor.
              resolve.go Cross-workspace fuzzy title resolution for combine
                         and context-aware edit.
  llm/        llm.go     Multi-provider LLM bridge. Streaming and non-streaming.
                         Provider: CAS_PROVIDER env (ollama | anthropic | groq | openai | openrouter).
                         Called exclusively by agents. The shell has no remaining LLM call sites.
  store/      store.go   Store interface (SessionStore).
              sqlite.go  SQLiteStore — production persistence at ~/.cas/cas.db.
              memory.go  MemoryStore — in-memory, used in tests.
  runner/     runner.go  Isolated code execution (process-level). Language detection, temp-file
                         write, subprocess with timeout, stdout/stderr capture.
  plugin/     plugin.go  Lua plugin runtime (gopher-lua). Loads ~/.cas/plugins/
                         *.lua. Sandboxed VM; no file I/O, no network.
  conductor/  conductor.go  Behavioral learning. Observes interactions, builds
                         ~/.cas/profile.json, injects context into LLM prompts.
  mcp/        client.go  MCP client layer. Connect(), discoverTools(), Call(), Close().
                         SSE transport via mark3labs/mcp-go.
  webview/    browser.go HTTP fetch + golang.org/x/net/html parser.
                         NewSession(), Navigate(), Fetch(), FormatPageState().
                         Extracts title, headings, links, body text; no browser runtime.

ui/           model.go   Bubble Tea TUI model. Renders chat + workspace panels,
                         handles keyboard, streams tokens into workspace view.

tests/tui/               TUI integration tests. Spawn the real binary in tmux
                         and interact with it as a user would.
```

---

## Request flow

```
User message
     │
     ▼
intent.Detect()               ← regex only, no LLM call, no latency
     │
     ├─ KindClose   →  workspace.Manager.Close()
     ├─ KindRun     →  runner.Run(workspace.Content)
     │
     ├─ KindCreate  →  GenerationAgent
     │                   contract.CheckPreconditions()   ← wsType, prompt, title
     │                   llm.Stream()
     │                   contract.CheckPostconditions()  ← non-empty, ≤512 KB
     │                   workspace.Create()
     │
     ├─ KindEdit    →  EditAgent
     │                   contract.CheckPreconditions()   ← wsType, content, request
     │                   llm.Stream()
     │                   contract.CheckPostconditions()  ← non-empty, ≤512 KB, ≥10% original
     │                   workspace.Update()
     │
     ├─ KindCombine →  CombineAgent
     │                   contract.CheckPreconditions()   ← ≥2 sources, all non-empty
     │                   llm.Stream()
     │                   contract.CheckPostconditions()  ← non-empty, ≤512 KB
     │                   workspace.Create()
     │
     ├─ KindOrchestrate → OrchestratorAgent
     │                   contract.CheckPreconditions()   ← instruction, ≥2 workspaces, executor, autonomy
     │                   llm.Complete() → step plan (workspace_id + instruction per step)
     │                   contract.CheckPostconditions()  ← all step workspace_ids exist
     │                   for each step: shell.ExecuteStep() → MCPAgent | WebAgent | EditAgent
     │                   llm.Complete() → one-sentence summary
     │
     ├─ KindIngest  →  MCPAgent (bound to workspace)
     │                   contract.CheckPreconditions()   ← instruction, connection, tools, autonomy
     │                   llm.Complete() → tool selection
     │                   contract.CheckPostconditions()  ← tool name exists on server
     │                   mcp.Connection.Call()  (if autonomy ≠ suggest)
     │
     ├─ KindBrowse  →  WebAgent (bound to workspace)
     │                   contract.CheckPreconditions()   ← instruction, session, page state, autonomy
     │                   llm.Complete() → action selection (answer/navigate/extract)
     │                   contract.CheckPostconditions()  ← navigate_url valid if set
     │                   webview.Session.Fetch()  (if action = navigate)
     │
     ├─ KindReconnect → shell.handleReconnect
     │                   re-establishes a stale mcp/web session from the stored URL
     │                   reconnectMCP / reconnectWeb → refresh content, Connected = true
     │
     └─ KindChat    →  ChatAgent
                         contract.CheckPreconditions()   ← message non-empty, history ≤ 20 turns
                         llm.Stream()
                         contract.CheckPostconditions()  ← reply guaranteed (fallback on empty)
                         → chat reply
                              │
                              ▼
                       conductor.Observe()   ← updates ~/.cas/profile.json
                              │
                              ▼
                       store.Save*()         ← SQLite
```

Plugin commands are checked before intent detection. If the message matches a
registered plugin prefix, the plugin handler fires and the flow short-circuits.

---

## Design by Contract

The contract package implements Bertrand Meyer's Design by Contract (1988) as
the security primitive for workspace operations.

Contracts are constructed and frozen before any LLM call. The model cannot
see, modify, or reason about them. Violations are always fatal to the
operation — no fallback, no retry, no LLM-assisted recovery. The three phases:

- **Preconditions** — checked before the LLM call (e.g. workspace type allowed,
  prompt non-empty)
- **Postconditions** — checked after the LLM call (e.g. content non-empty, size
  within limit, edit result not drastically shorter than original)
- **Invariants** — structural rules that must always hold

Each agent constructs its own contract from `contract.New()`. The contract is
frozen before the LLM call and cannot be modified by the agent or the model.

Per-agent contract rules:

| Agent             | Preconditions                                      | Postconditions                                        |
|-------------------|----------------------------------------------------|-------------------------------------------------------|
| GenerationAgent   | wsType valid, prompt non-empty, title non-empty    | content non-empty, ≤ 512 KB                           |
| EditAgent         | wsType valid, content non-empty, request non-empty | result non-empty, ≤ 512 KB, ≥ 10% of original        |
| CombineAgent      | ≥ 2 sources, all sources non-empty                 | result non-empty, ≤ 512 KB                            |
| ChatAgent         | message non-empty, history ≤ 20 turns              | reply guaranteed (fallback applied before check)      |
| MCPAgent          | instruction non-empty, connection present, ≥1 tool, autonomy valid | selected tool name exists on server    |
| WebAgent          | instruction non-empty, session present, page state present, autonomy valid | navigate_url (if set) is absolute |
| OrchestratorAgent | instruction non-empty, ≥2 workspaces with IDs, executor present, autonomy valid | plan has steps, all step workspace_ids exist |

The 10% truncation guard on EditAgent catches cases where the model returns
only a fragment of the updated document instead of the full content.

**Autonomy dial** (MCPAgent, WebAgent, OrchestratorAgent):

| Value     | Behaviour                                                        |
|-----------|------------------------------------------------------------------|
| `suggest` | LLM plans the action; tool/navigation not executed              |
| `confirm` | Action executed after user approval; surfaced via the TUI `FocusConfirm` state (y: proceed / n: skip / esc: cancel) |
| `run`     | Action executed freely within workspace scope                   |

ChatAgent's postcondition is a guarantee rather than a hard check — if the
model returns an empty reply, the fallback nudge is substituted before the
postcondition runs, ensuring the contract is always satisfied.

---

## Persistence

Single local SQLite database at `~/.cas/cas.db`. No remote component.

The `Store` interface defines five concerns:

| Concern        | Types                                              |
|----------------|----------------------------------------------------|
| Sessions       | `SessionRow`                                       |
| Messages       | `MessageRow`                                       |
| Workspaces     | `WorkspaceRow` (live), `HistoryRow` (versioned)    |
| History        | Undo via `Undo()`, version replay via `ApplyVersion()` |
| Orchestration  | `OrchestrationRunRow`, `OrchestrationStepRow`      |

Two concrete implementations: `SQLiteStore` (production) and `MemoryStore`
(tests). The interface makes them interchangeable.

**Schema migrations** use `PRAGMA user_version` with explicit version gates.
Existing databases upgrade automatically on next start:

- **v1** — `messages.id` migrated from INTEGER to TEXT primary key
- **v2** — `orchestration_runs` and `orchestration_steps` tables added,
  indexed on `session_id` and `run_id` respectively

Every orchestration run and its per-step inputs/outputs are persisted,
making any multi-workspace task fully auditable and replayable via
`LoadOrchestrationRuns(sessionID)` + `LoadOrchestrationSteps(runID)`.

### Session recovery

`mcp` and `web` workspaces hold a live runtime session (MCP connection,
web fetch context) that is **not** persisted — only their last content
snapshot is. On restart, `workspace.Restore()` sets `Workspace.Connected`
to false for these types. The shell prepends a stale notice to their
content, the TUI shows a `[!]` tab badge and a DISCONNECTED status hint,
and the `reconnect` command re-establishes the session from the URL stored
in the workspace content. `document`/`code`/`list` workspaces are always
`Connected` since they have no runtime session.

---

## LLM providers

Provider is selected by `CAS_PROVIDER` environment variable.

| Provider     | Value          | Key env              | Default models                                           |
|--------------|----------------|----------------------|----------------------------------------------------------|
| Ollama       | `ollama`       | —                    | `qwen3.5:9b` (doc/list/chat), `qwen2.5-coder:7b` (code) |
| Anthropic    | `anthropic`    | `ANTHROPIC_API_KEY`  | `claude-sonnet-4-6` (doc/list/chat), `claude-haiku-4-5-20251001` (code) |
| Groq         | `groq`         | `GROQ_API_KEY`       | `llama-3.3-70b-versatile` (all types)                   |
| OpenAI       | `openai`       | `OPENAI_API_KEY`     | `gpt-4o` (doc/list/chat), `gpt-4o-mini` (code)         |
| OpenRouter   | `openrouter`   | `OPENROUTER_API_KEY` | `meta-llama/llama-3.3-70b-instruct` (all types)         |

Model overrides: `CAS_MODEL_{TYPE}` env vars (e.g. `CAS_MODEL_CODE`).

All providers use streaming. The `llm` package exposes `Stream()` and
`Complete()` with identical signatures — provider selection is internal.

Groq, OpenAI, and OpenRouter share a single `openaiCompatComplete` /
`openaiCompatStream` implementation (OpenAI chat completions wire format).
Adding a future OpenAI-compatible provider requires ~10 lines.

`llm.ValidateProvider()` checks the active provider has its key set and
returns a descriptive error naming the missing env var — called at startup
before the TUI opens. `llm.AllProviders()` returns config status for all five;
exposed via `./cas --providers`.

The active provider is displayed in the TUI chat-focus status bar.

---

## Behavioral learning (Conductor)

The Conductor observes every shell interaction and writes a profile to
`~/.cas/profile.json`. After a minimum of 2 messages and 1 workspace, it
derives a natural-language context string (dominant doc types, workspace
preferences, recent edit patterns) and appends it to LLM system prompts.

The Conductor is fail-safe: a corrupt or missing profile never breaks the
shell. Observe errors are logged and discarded.

---

## Code execution (Runner)

The runner detects language from content (shebang, keywords), writes a temp
file, and spawns a subprocess with a 30-second timeout. Supported: Python,
Go, Bash, JavaScript (Node), Ruby.

Sandboxing is currently process-level (timeout, working directory isolation).
OS-level sandbox (seccomp/namespaces) is documented as a future hardening
step.

---

## Lua plugins

Plugins are `.lua` files in `~/.cas/plugins/`. At startup the plugin registry
loads all files into isolated gopher-lua VMs. Each plugin can register
commands via `cas.command(name, desc, handler)`. The sandbox prohibits file
I/O, `os.execute`, and network access. A bad plugin does not block others.

Plugin commands are matched before intent detection — exact match first, then
prefix match (case-insensitive).

---

## Tests

```
go test ./...                                     # 245 unit tests
TUI_INTEGRATION=1 go test -v -tags=integration \
  ./tests/tui/ -timeout 300s                      # 8 TUI integration tests
```

TUI tests spawn the real binary in tmux and interact with it as a user would,
catching runtime issues that unit tests miss. Requires tmux and a running
Ollama instance.

CI runs on GitHub Actions (`.github/workflows/ci.yml`) against Go 1.25.

---

## What does not exist

The following have been claimed in various contexts and are **not** in this
codebase:

- Remote architecture / decoupled session and execution layers
- `ExecutionContext` or `SessionStore` as a transport protocol
- `docs/remote-architecture.md`
- Any "Phase N complete" implementation
- `~/projects/loom` dependency

The store is local SQLite only. There is no server component.
