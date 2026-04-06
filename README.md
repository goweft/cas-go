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
  <a href="#quick-start">Quick Start</a>
</p>

<p align="center">
  <a href="https://github.com/goweft/cas/actions/workflows/ci.yml">
    <img src="https://github.com/goweft/cas/actions/workflows/ci.yml/badge.svg" alt="CI">
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
```

Pure regex, sub-millisecond. Self-edit phrases are checked before edit patterns so "edit it directly" never triggers an unwanted LLM call.

### Deterministic contracts

Every workspace operation passes through a contract layer before execution:

```go
contract.CheckPreconditions()   // is this operation permitted?
contract.CheckInvariants()      // are all invariants satisfied?
contract.CheckPostconditions()  // did the output meet requirements?
```

Contracts run in Go, external to the model. The model cannot modify, bypass, or reason about them. Any violation fails the operation — fail-closed always. Based on Bertrand Meyer's Design by Contract (1986).

### Three workspace types

| Type | Badge | Model (Ollama) | Model (Anthropic) |
|---|---|---|---|
| Document | `[d]` | `qwen3.5:9b` | `claude-sonnet-4-6` |
| List | `[l]` | `qwen3.5:9b` | `claude-sonnet-4-6` |
| Code | `[c]` | `qwen2.5-coder:7b` | `claude-haiku-4-5-20251001` |

### Streaming

A placeholder tab appears immediately on create. Tokens stream into it via a buffered channel feeding one event per Bubble Tea tick. The workspace is live from the first token. No separate loading state.

### Code execution

Say `run it` or `execute` with an active code workspace. CAS detects the language from content (bash, Python, Go, JavaScript, Ruby), writes to a temp file, and executes in a sandboxed subprocess with:

- Process group isolation — timeout kills the entire tree, not just the leader
- Environment restriction — only `PATH` is inherited, no secrets leak
- 30-second default timeout
- stdout and stderr captured and displayed in the chat panel

No LLM call is needed — intent detection routes directly to the runner.

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

SQLite (WAL mode) at `~/.cas/cas.db`. Sessions, workspaces, and conversation history survive restarts. Previous workspaces restore as tabs on next launch. Full version history per workspace enables multi-step undo.

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
├── contract/    Design by Contract enforcement, fail-closed
├── workspace/   Lifecycle: create, update, undo, close, restore
├── shell/       Session manager: ProcessMessage, StreamMessage
├── llm/         Ollama + Anthropic streaming/sync, model routing
├── runner/      Code execution — sandboxed subprocess, timeout, env isolation
├── store/       Store interface, SQLiteStore (WAL), MemoryStore
└── conductor/   Behavioral learning — observe, profile, user_context
ui/              Bubble Tea TUI: split panel, tabs, streaming, inline edit
tests/tui/       TUI integration tests (spawn real binary via tmux)
cmd/cas/         Entry point: --db, --memory flags
```

**193 tests** across all packages. **8 TUI integration tests** that spawn the real binary in tmux and interact with it as a user would — catching runtime bugs that unit tests miss.

---

## Quick Start

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

### Cloud — Anthropic API (no GPU required)

```bash
export CAS_PROVIDER=anthropic
export ANTHROPIC_API_KEY=sk-ant-...
./cas
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

## References

Shneiderman & Maes (1997). "Direct Manipulation vs. Interface Agents." *Interactions, 4(6).*

Meyer (1988). *Object-Oriented Software Construction.* Prentice Hall. (Design by Contract)

Norman (1986). "Cognitive Engineering." In *User Centered System Design.*

Horvitz (1999). "Principles of Mixed-Initiative User Interfaces." *CHI '99.*
