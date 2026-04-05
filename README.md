# CAS — Conversational Agent Shell (Go)

A terminal shell where conversation generates workspaces and you control them directly.

Single static binary. No runtime dependencies.

## Quick Start

```bash
git clone https://github.com/goweft/cas.git
cd cas
go build -o cas ./cmd/cas

# With Anthropic API (no GPU required)
export CAS_PROVIDER=anthropic
export ANTHROPIC_API_KEY=sk-ant-...
./cas

# With Ollama (local, private)
ollama pull qwen3.5:9b && ollama pull qwen2.5-coder:7b
./cas
```

## Status

Phase 1 in progress — core loop: intent → contract → LLM → workspace → TUI.
