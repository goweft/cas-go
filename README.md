<p align="center"><img src="banner.svg" alt="cas" width="100%"></p>

<h1 align="center">CAS</h1>
<p align="center"><strong>Conversational Agent Shell</strong></p>
<p align="center">
  A terminal shell where conversation generates workspaces and you control them directly.<br>
  Single static binary &nbsp;·&nbsp; deterministic contracts &nbsp;·&nbsp; streaming &nbsp;·&nbsp; persistent sessions
</p>

<p align="center">
  <a href="#the-idea">The Idea</a> &nbsp;·&nbsp;
  <a href="#see-it-work">See It Work</a> &nbsp;·&nbsp;
  <a href="#how-it-works">How It Works</a> &nbsp;·&nbsp;
  <a href="#keyboard-reference">Keys</a> &nbsp;·&nbsp;
  <a href="#quick-start">Quick Start</a> &nbsp;·&nbsp;
  <a href="#writing">Writing</a>
</p>

<p align="center">
  <a href="https://github.com/goweft/cas/actions/workflows/ci.yml">
    <img src="https://github.com/goweft/cas/actions/workflows/ci.yml/badge.svg" alt="CI">
  </a>
  <a href="https://github.com/goweft/cas/releases/latest">
    <img src="https://img.shields.io/github/v/release/goweft/cas" alt="Latest release">
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License: Apache 2.0">
  </a>
  <a href="https://go.dev/">
    <img src="https://img.shields.io/badge/Go-1.25+-00ADD8.svg" alt="Go 1.25+">
  </a>
</p>

---

## The Idea

Most AI tools give you a chat window. You type, the model responds, you copy what you need and paste it somewhere else. The conversation and the artifact are separate things in separate places.

CAS is a different arrangement. Conversation is for **generating** things. Once generated, you **control** them directly.

You say *write a project proposal*. A workspace tab opens alongside the chat — tokens streaming into it as the model generates. When generation ends, you edit it directly, ask CAS to make changes, or both. The AI built the artifact. You wield it.

This resolves a debate in HCI running since 1997. Shneiderman argued that direct manipulation gives users control that delegation never can. Maes argued that agents reduce cognitive load that direct manipulation can't scale to. Both were right. CAS addresses it architecturally:

**Agents generate. Users manipulate.**

---

## See It Work

<p align="center"><img src="demo.svg" alt="CAS demo — animated" width="100%"></p>

Tokens stream into the workspace as they are generated. The left panel is persistent conversation. The right panel is the workspace you control directly. Multiple workspaces open as tabs — `[d]` document, `[c]` code, `[l]` list.

---

## How It Works

### Intent detection — zero latency, no LLM call

Every message is classified before any model is invoked:

```
"write a project proposal"       → create workspace (document)
"make a todo list for easter"    → create workspace (list)
"create a python script"         → create workspace (code)
"add a shopping section"         → edit active workspace
"add error handling"             → edit active workspace
"how long should this be?"       → chat reply
"edit it directly"               → chat  ← self-edit exclusion fires first
"close the workspace"            → close active tab
"run it"                         → execute active code workspace
"test this"                      → execute active code workspace
"combine the proposal and checklist" → merge workspaces into new document
```

Pure regex, sub-millisecond. Self-edit phrases are checked before edit patterns so "edit it directly" never triggers an unwanted LLM call.

### Deterministic contracts

Every workspace operation passes through a contract layer before execution:

```go
contract.CheckPreconditions()   // is this operation permitted?
contract.CheckInvariants()      // are all invariants satisfied?
contract.CheckPostconditions()  // did the output meet requirements?
```

Contracts run in Go, external to the model. The model cannot modify, bypass, or reason about them. Any violation fails the operation — fail-closed always. Based on Bertrand Meyer's Design by Contract (1988).

Every LLM call is owned by a named agent (`GenerationAgent`, `EditAgent`, `CombineAgent`, `ChatAgent`, `MCPAgent`, `WebAgent`, `OrchestratorAgent`). Contracts are frozen before the LLM call and checked again after. The shell delegates to agents and has no remaining LLM call sites — it only routes.

`MCPAgent` and `WebAgent` operate under an **autonomy dial** (`suggest` / `confirm` / `run`) that governs whether actions are planned only, executed after the user approves the concrete action (the tool name with its full arguments, or the exact URL — never just the instruction that led to it), or executed freely within their workspace scope. The contract enforces that any tool or URL used exists on the bound server or page — the agent cannot reach outside its workspace.

### Three workspace types

| Type | Badge | Model (Ollama) | Model (Anthropic) | Model (Groq) | Model (OpenAI) | Model (OpenRouter) |
|---|---|---|---|---|---|---|
| Document | `[d]` | `qwen3.5:9b` | `claude-sonnet-4-6` | `llama-3.3-70b-versatile` | `gpt-4o` | `meta-llama/llama-3.3-70b-instruct` |
| List | `[l]` | `qwen3.5:9b` | `claude-sonnet-4-6` | `llama-3.3-70b-versatile` | `gpt-4o` | `meta-llama/llama-3.3-70b-instruct` |
| Code | `[c]` | `qwen2.5-coder:7b` | `claude-haiku-4-5-20251001` | `llama-3.3-70b-versatile` | `gpt-4o-mini` | `meta-llama/llama-3.3-70b-instruct` |

### Streaming

A placeholder tab appears immediately on create. Tokens stream into it via a buffered channel feeding one event per Bubble Tea tick. The workspace is live from the first token. No separate loading state.

### Code execution

Say `run it` or `execute` with an active code workspace. CAS detects the language from content (bash, Python, Go, JavaScript, Ruby), writes to a temp file, and executes in an isolated subprocess with:

- Process group isolation — timeout kills the entire tree, not just the leader
- Environment restriction — only `PATH` is inherited, no secrets leak
- 30-second default timeout
- stdout and stderr captured and displayed in the chat panel

This is process-level isolation, not an OS-level sandbox: executed code
runs with your user's file and network access. OS-level sandboxing
(seccomp/namespaces) is tracked as future hardening — see ARCHITECTURE.md.

No LLM call is needed — intent detection routes directly to the runner.

### Plugins

Drop `.lua` files in `~/.cas/plugins/` to add custom commands without recompiling:

```lua
-- ~/.cas/plugins/standup.lua
cas.command("standup", "Daily standup from workspaces", function()
    local ws = cas.workspaces()
    local lines = {}
    for i, w in ipairs(ws) do
        lines[i] = "- " .. w.title .. " (" .. w.type .. ")"
    end
    cas.reply("## Standup\n\n" .. table.concat(lines, "\n"))
end)
```

Type `standup` in the chat and the plugin runs — no LLM call, sub-millisecond.

The Lua VM is sandboxed: no file I/O, no `os.execute`, no network. Plugins interact with CAS through a controlled API: `cas.command()`, `cas.reply()`, `cas.workspaces()`, `cas.active()`.

Ready-to-use plugins live in [`examples/plugins/`](examples/plugins/) — `standup`, `wordcount`/`wc`, `toc`, and `todos`. Copy them into `~/.cas/plugins/` to try them. They're loaded and executed by the test suite on every push, so the examples can't drift from the API.

### Cross-workspace operations

With multiple workspaces open, CAS resolves which one you're addressing by fuzzy-matching title fragments:

```
"update the proposal"            → targets "Project Proposal", not the most recent tab
"add the script code to the report" → edits "Report" with "Script" content as context
"combine the proposal and checklist" → creates a new workspace from both sources
"merge all workspaces"           → synthesizes everything into one document
```

Edits that reference another workspace by name automatically include that workspace's content in the LLM prompt — the model can see both documents when making changes.

### Behavioral learning

A Conductor module observes your usage across sessions and builds `~/.cas/profile.json`:

```json
{
  "ws_types": {"document": 12, "list": 5, "code": 3},
  "doc_types": {"proposal": 4, "report": 3, "note": 2},
  "edit_verbs": {"add": 7, "fix": 2},
  "session_count": 6,
  "workspace_count": 20
}
```

This feeds back into LLM system prompts automatically. More sessions → better context → better output. No configuration required.

### Persistence

SQLite (WAL mode) at `~/.cas/cas.db`. Sessions, workspaces, and conversation history survive restarts. Previous workspaces restore as tabs on next launch. Full version history per workspace enables multi-step undo. Orchestration runs are recorded before their first step executes and finalized as completed or failed afterwards; every attempted step is persisted with its output or its error, so partial and failed runs are auditable, not just successful ones. Schema migrations run automatically via `PRAGMA user_version`.

---

## Keyboard Reference

### Chat panel (default focus)

| Key | Action |
|---|---|
| `Enter` | Send message |
| `Tab` | Switch to workspace panel |
| `←` `→` | Move cursor in input |
| `Home` / `Ctrl+A` | Jump to start of input |
| `End` / `Ctrl+E` | Jump to end of input |
| `Backspace` | Delete character before cursor |
| `Delete` | Delete character after cursor |
| `Ctrl+W` | Delete previous word |
| `Ctrl+K` | Delete to end of line |
| `Ctrl+U` | Delete to start of line |
| `↑` / `↓` | Scroll conversation history |
| `Ctrl+N` | Start a new session |
| `Ctrl+C` | Quit |

### Workspace panel (press `Tab` to focus)

| Key | Action |
|---|---|
| `Tab` | Return to chat panel |
| `Esc` | Return to chat panel |
| `[` / `]` | Previous / next workspace tab |
| `e` | Enter inline edit mode |
| `Ctrl+Z` | Undo last change |
| `Ctrl+E` | Export to `~/cas-exports/` |
| `↑` / `↓` | Scroll workspace content |
| `PgUp` / `PgDn` | Scroll by 10 lines |

### Edit mode (press `e` to enter — amber border)

Full terminal editor via `charmbracelet/bubbles` textarea. All standard cursor movement and editing keys work.

| Key | Action |
|---|---|
| `Esc` | Save and exit edit mode |
| `Ctrl+S` | Save without leaving edit mode |
| `Ctrl+C` | Discard changes and exit |

---

## Architecture

```
internal/
├── intent/      Zero-latency intent detection — regex, no LLM call
├── agent/       Seven named sub-agents, each with a frozen contract
│                GenerationAgent, EditAgent, CombineAgent, ChatAgent,
│                MCPAgent (tool calls), WebAgent (web actions),
│                OrchestratorAgent (multi-workspace coordination)
│                Shell has no remaining LLM call sites — it only routes
├── contract/    Design by Contract enforcement, fail-closed
├── workspace/   Lifecycle: create, update, undo, close, restore
├── shell/       Session manager: ProcessMessage, StreamMessage
├── llm/         Ollama, Anthropic, Groq, OpenAI, OpenRouter — streaming/sync, model routing
├── runner/      Code execution — isolated subprocess, timeout, env isolation
├── plugin/      Lua plugin runtime — sandboxed gopher-lua VM
├── mcp/         MCP client — connect, discover tools, call, close (SSE transport)
├── webview/     HTTP fetch + HTML parser — title, headings, links, body text
├── store/       Store interface, SQLiteStore (WAL), MemoryStore
└── conductor/   Behavioral learning — observe, profile, user_context
ui/              Bubble Tea TUI: split panel, tabs, streaming, inline edit
tests/tui/       TUI integration tests (spawn real binary via tmux)
cmd/cas/         Entry point: --db, --memory, --providers, --version flags
```

**280 tests** across all packages. **8 TUI integration tests** that spawn the real binary in tmux and interact with it as a user would — catching runtime bugs that unit tests miss.

For the design rationale behind this layout — why the shell makes no LLM calls, how contracts
are enforced, how workspaces are modelled — see [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Quick Start

### Install

Download a prebuilt binary for Linux, macOS, or Windows from the
[latest release](https://github.com/goweft/cas/releases/latest), or install
with a Go toolchain:

```bash
go install github.com/goweft/cas/cmd/cas@latest
```

### Build from source

```bash
git clone https://github.com/goweft/cas.git
cd cas
go build -o cas ./cmd/cas
```

### Local inference — Ollama

```bash
ollama pull qwen3.5:9b
ollama pull qwen2.5-coder:7b
./cas
```

Ollama doesn't have to be local: set `OLLAMA_BASE_URL` to point CAS at a
GPU box over Tailscale — see [`docs/deployment.md`](docs/deployment.md).

### Cloud — Anthropic API (no GPU required)

```bash
export CAS_PROVIDER=anthropic
export ANTHROPIC_API_KEY=sk-ant-...
./cas
```

### Cloud — Groq (free tier, fastest inference)

```bash
export CAS_PROVIDER=groq
export GROQ_API_KEY=gsk_...
./cas
```

### Cloud — OpenAI

```bash
export CAS_PROVIDER=openai
export OPENAI_API_KEY=sk-...
./cas
```

### Cloud — OpenRouter (access hundreds of models with one key)

```bash
export CAS_PROVIDER=openrouter
export OPENROUTER_API_KEY=sk-or-...
./cas

# Use a specific model via override
CAS_MODEL_CODE=anthropic/claude-3-5-haiku ./cas
```

### List configured providers

```bash
./cas --providers
```

### Ingest an MCP server

```bash
# In chat: connects and opens a workspace with the server's tools listed
ingest https://mcp.linear.app/sse

# Then ask the agent to use a tool:
# "list my open issues"  →  MCPAgent reasons, selects tool, calls it
```

### Browse a web page

```bash
# In chat: fetches the page and opens a workspace with its content
browse https://golang.org

# Then ask the agent to work with it:
# "summarise the main sections"
# "navigate to https://golang.org/doc/install"
# "extract all links"
```

### Coordinate across workspaces

```bash
# With two ingested workspaces open (e.g. Linear + GitHub MCP servers):
# "read the linear issue and open a github PR for it"
# → OrchestratorAgent decomposes into steps, executes each in sequence,
#   passes each step's output as context to the next
```

In confirm mode the TUI pauses before each side effect and prompts for
approval (`y` proceed / `n` skip / `esc` cancel). For MCP and web steps
the prompt is the concrete action the agent has planned and the contract
has validated — the tool name and its full arguments, or the exact URL —
not the step's instruction. Every run is recorded before its first step
and finalized as completed or failed; each attempted step is persisted
with its output or its error, so partial and failed runs are auditable
too.

### Reconnect a stale workspace

```bash
# After a restart, mcp/web workspaces show a [!] badge — their live
# session ended but their content was preserved. To restore:
reconnect              # reconnects the first disconnected workspace
reconnect linear       # reconnects a workspace by title
```

### Flags

```
./cas                   # restore last session (default)
./cas --memory          # ephemeral session, no persistence
./cas --db /path/to.db  # custom database path
```

### Override model routing

```bash
CAS_MODEL_CODE=qwen3.5:27b ./cas        # use 27b for code workspaces
CAS_MODEL_DOCUMENT=qwen3:14b ./cas      # use 14b for documents
```

### Tests

```bash
go test ./...

# TUI integration tests — requires tmux + Ollama running
TUI_INTEGRATION=1 go test -v -tags=integration ./tests/tui/ -timeout 300s
```

---

## Export

`Ctrl+E` in workspace focus writes the active tab to `~/cas-exports/`:

- Documents and lists → `.md`
- Code → extension detected from content (`.py`, `.go`, `.sh`, `.js`, `.rb`, `.txt`)

The directory is created automatically if it doesn't exist.

---

## Writing

Long-form background on the design, in order:

- [I Replaced My AI Chat Interface With a Terminal Shell](https://dev.to/goweft/i-replaced-my-ai-chat-interface-with-a-terminal-shell-5aoh)
  — why a terminal shell, and how conversation and direct manipulation share one window.
- [Seven Agents, Zero Trust: How I Made an Agentic Shell Safe by Design](https://dev.to/goweft/seven-agents-zero-trust-how-i-made-an-agentic-shell-safe-by-design-3ed6)
  — the safety model: named agents, frozen contracts, fail-closed enforcement, the autonomy dial.

## References

Shneiderman & Maes (1997). "Direct Manipulation vs. Interface Agents." *Interactions, 4(6).*

Meyer (1988). *Object-Oriented Software Construction.* Prentice Hall. (Design by Contract)

Norman (1986). "Cognitive Engineering." In *User Centered System Design.*

Horvitz (1999). "Principles of Mixed-Initiative User Interfaces." *CHI '99.*
