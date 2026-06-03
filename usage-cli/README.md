# usage-cli

Go CLI for summarizing local LLM usage costs across Claude Code, Codex, Pi, and OMP logs.

## Features

- Reads local usage history from:
  - `~/.claude/projects/**/*.jsonl`
  - `~/.codex/sessions/**/*.jsonl`
  - `~/.pi/**/*.jsonl`
  - `~/.omp/**/*.jsonl`
- Prints day, week, month, or daily trend cost summaries in the terminal.
- Groups spend by model and agent category:
  - `claude`
  - `codex`
  - `pi`
  - `omp`
- Uses recorded `costUSD`/`cost_usd` when present.
- Estimates missing costs from LiteLLM pricing.
- Caches parsed token entries under `~/.cache/llm-usage/`.

## Usage

```bash
go run . --view day
go run . --view week
go run . --view month
go run . --view all
go run . --view trend --range week
go run . --view trend --range month
go run . --view trend --from 2026-05-01 --to 2026-05-31
```

Use another home directory for testing:

```bash
go run . --home /tmp/test-home --view month
```

## Caches

Parsed token entries are cached by source file metadata (`path`, `mtime`, `size`):

```text
~/.cache/llm-usage/claude.json
~/.cache/llm-usage/codex.json
~/.cache/llm-usage/pi.json
~/.cache/llm-usage/omp.json
```

If `XDG_CACHE_HOME` is set, token caches are written to:

```text
$XDG_CACHE_HOME/llm-usage/
```

LiteLLM pricing is cached for 7 days:

```text
~/.cache/llm-usage/litellm_pricing_cache.json
```

To force a full rebuild:

```bash
rm -rf ~/.cache/llm-usage
```

## Pricing

Cost resolution order:

1. Use explicit cost from the log entry when available.
2. Match the model against LiteLLM pricing.
3. Fall back to built-in pricing for common Claude and GPT models.

The CLI also handles common provider/model aliases, such as `azure_openai/gpt-*`, `anthropic/claude-*`, `mimo-v2.5-pro`, and deployment-suffixed Claude model names.

## Development

```bash
go test ./...
go run . --view month
```
