# ModelRelay CLI (mrl)

A lightweight CLI for chatting with AI models, running agents, and managing ModelRelay resources.

📖 **[Full documentation](https://docs.modelrelay.ai/sdks/cli)**

> **Note**: This repo is mirrored from [modelrelay/modelrelay](https://github.com/modelrelay/modelrelay) (monorepo). The monorepo is the source of truth. Submit issues and PRs there.

## Quick Start

```bash
# Set your API key
export MODELRELAY_API_KEY=mr_sk_...

# Ask a question
mrl "What is 2 + 2?"

# Stream the response
mrl "Write a haiku about coding" --stream

# Show token usage
mrl "Explain recursion" --usage

# Use a specific model
mrl "Hello" --model gpt-5.2

# Pipe text content into the prompt
cat README.md | mrl "summarize this"
echo "What is the capital of France?" | mrl
```

## Installation

### Homebrew (macOS/Linux)

```bash
brew install modelrelay/tap/mrl
```

To upgrade:

```bash
brew upgrade mrl
```

### Manual Download

Download the latest release from [releases.modelrelay.ai](https://releases.modelrelay.ai/mrl/) and add to your PATH.

### From Source

```bash
go install github.com/modelrelay/mrl@latest
```

Or build locally:

```bash
git clone https://github.com/modelrelay/mrl.git
cd mrl && go build -o mrl
```

## Setup

Environment variables:

```bash
export MODELRELAY_API_KEY=mr_sk_...
export MODELRELAY_MODEL=claude-sonnet-4-5  # default model
export MODELRELAY_PROJECT_ID=...           # UUID (optional)
export MODELRELAY_API_BASE_URL=...         # optional
```

Config file (`~/.config/mrl/config.toml`):

```toml
[profiles.default]
api_key = "mr_sk_..."
model = "claude-sonnet-4-5"
base_url = "https://api.modelrelay.ai/api/v1"
project_id = "<uuid>"
output = "table"  # or "json"

# Options for `mrl do` command
allow_all = true
trace = true
# allow = ["git ", "npm "]  # alternative to allow_all
```

Manage config with:

```bash
mrl config set --api-key mr_sk_... --model claude-sonnet-4-5
mrl config set --allow-all --trace  # enable for `mrl do`
mrl config set --profile work --model gpt-5.2
mrl config use work
mrl config show
```

## Commands

### Ask a question (default)

The primary action—just pass a prompt directly:

```bash
mrl "What is the capital of France?"
```

Flags:

| Flag | Description |
|------|-------------|
| `--model` | Override the default model |
| `--system` | Set a system prompt |
| `--stream` | Stream output as it's generated |
| `--usage` | Show token usage after response |
| `-a, --attachment` | Attach a local file (repeatable; use `-` for stdin) |
| `--attachment-type` | Override attachment MIME type |
| `--attach-stdin` | Attach stdin as a file (requires piping data) |

When stdin is piped without attachment flags, it's automatically read as text and combined with the prompt. Use attachment flags (`-a`, `--attachment-type`, `--attach-stdin`) for binary files.

Examples:

```bash
mrl "Explain quantum computing in simple terms"
mrl "Write a poem" --stream
mrl "Summarize this" --system "Be concise" --usage

# Pipe text content (auto-detected)
cat README.md | mrl "summarize this"
echo "What is 2+2?" | mrl
git diff | mrl "explain these changes"

# Attach files
mrl "Summarize this PDF" -a report.pdf
cat notes.pdf | mrl "Extract tables" -a - --attachment-type application/pdf
cat notes.pdf | mrl "Extract tables" --attachment-type application/pdf
cat notes.pdf | mrl "Extract tables" --attach-stdin --attachment-type application/pdf
mrl "Hello" --model gpt-5.2
```

### Execute a task with tools

Run agentic tasks that can execute bash commands:

```bash
# With config: allow_all = true, trace = true
mrl do "commit my changes"
mrl do "run tests and fix failures"

# Or with flags
mrl do "show git status" --allow "git "
mrl do "list all TODO comments" --allow "grep " --allow "find "
mrl do "commit my changes" --allow-all --trace
```

Flags:

| Flag | Description |
|------|-------------|
| `--allow` | Allow bash command prefix (repeatable) |
| `--allow-all` | Allow all bash commands |
| `--max-turns` | Max tool loop turns (default 50) |
| `--trace` | Print commands as they execute |
| `--model` | Override the default model |
| `--system` | Set a system prompt |

Config options (set with `mrl config set`):

| Option | Description |
|--------|-------------|
| `--allow-all` | Allow all bash commands by default |
| `--allow` | Default allowed command prefixes |
| `--trace` | Show commands by default |

By default, no commands are allowed. Use `--allow` to whitelist command prefixes, `--allow-all` to permit any command, or set these in your config.

### Run a local RLM session

Run a local RLM session where Python executes on your machine and LLM calls go through ModelRelay (uses your configured default model unless you pass `--model`):

```bash
# Pipe a file into the local Python sandbox
cat large_dataset.csv | mrl rlm "Summarize the data and compute key stats"

# Attach local files by path
mrl rlm "Summarize the data" -a ./large_dataset.csv

# Multiple files (shell expands globs before mrl runs)
mrl rlm "Summarize all datasets" -a ./data/*.csv -a ./logs/*.json
```

Flags:

| Flag | Description |
|------|-------------|
| `-a, --attachment` | Attach a local file (repeatable; use `-` for stdin) |
| `--attachment-type` | Override attachment MIME type (useful for stdin) |
| `--attach-stdin` | Attach stdin as a file |
| `--max-iterations` | Max code generation cycles (default: 10) |
| `--max-subcalls` | Max llm_query/llm_batch calls (default: 50) |
| `--max-depth` | Max recursion depth (default: 1) |
| `--exec-timeout-ms` | Python execution timeout in ms (0 uses interpreter default) |
| `--python` | Python executable (default: python3) |
| `--max-inline-bytes` | Max inline context bytes (0 uses interpreter default) |
| `--max-total-bytes` | Max total context bytes (0 uses interpreter default) |
| `--inline-text-max-bytes` | Max inline text bytes per file (0 uses default 1MB) |
| `--system` | Custom instructions prepended to the default RLM system prompt |
| `--system-override` | Replace the entire system prompt instead of prepending |

The CLI builds a JSON context from attached files and exposes it as `context` in Python. Small text files are also loaded into `context["files"][i]["text"]` for easier scanning.

### Run an agent

```bash
mrl agent run researcher --input "Analyze Q4 sales"
```

### Test an agent with mocked tools

```bash
mrl agent test researcher \
  --input "Analyze Q4 sales" \
  --mock-tools ./mocks.json \
  --trace
```

### JSON input file

```bash
mrl agent test researcher \
  --input-file ./inputs.json \
  --output ./trace.json \
  --json
```

### Run a local agentic tool loop

Enable the local `bash` tool (deny-by-default) and run a loop:

```bash
mrl agent loop \
  --model claude-sonnet-4-5 \
  --tool bash \
  --bash-allow "git " \
  --input "List recent commits and summarize them"
```

Include `tasks_write` for progress tracking (state handle optional):

```bash
mrl agent loop \
  --model claude-sonnet-4-5 \
  --tool bash \
  --tool tasks_write \
  --state-ttl-sec 86400 \
  --tasks-output ./tasks.json \
  --input "Audit this repo and track your progress"
```

Enable local filesystem tools (`fs.*`):

```bash
mrl agent loop \
  --model claude-sonnet-4-5 \
  --tool fs \
  --input "Search for TODOs in this repo"
```

### Tool manifest (TOML/JSON)

You can load tools from a manifest file. The format is chosen by file extension (`.toml` or `.json`). CLI flags override manifest values.

`tools.toml`:

```toml
tool_root = "."
tools = ["bash", "tasks_write"]
state_ttl_sec = 86400

[bash]
allow = ["git ", "rg "]
timeout = "15s"
max_output_bytes = 64000

[tasks_write]
output = "tasks.json"
print = true

[fs]
ignore_dirs = ["node_modules", ".git"]
search_timeout = "3s"

[[custom]]
name = "custom.echo"
description = "Echo input as JSON"
command = ["cat"]
schema = { type = "object", properties = { message = { type = "string" } }, required = ["message"] }
```

Run with:

```bash
mrl agent loop --model claude-sonnet-4-5 --tools-file ./tools.toml --input "Audit this repo"
```

### List models

```bash
mrl model list
```

Filter by provider/capability and include deprecated:

```bash
mrl model list --provider openai --capability text_generation
mrl model list --include-deprecated --json
```

### Lint a JSON schema

```bash
mrl schema lint ./schema.json
```

Validate provider compatibility:

```bash
mrl schema lint ./schema.json --provider openai
mrl schema lint ./tool-schema.json --provider openai --tool-schema
```

### Version

```bash
mrl version
```

## Resource Commands

### Customers

```bash
mrl customer list
mrl customer get <customer_id>
mrl customer create --external-id user_123 --email user@example.com
```

### Usage

```bash
mrl usage account
```

### Tiers

```bash
mrl tier list
mrl tier get <tier_id>
```

## Output

Table output is the default. Use `--json` for machine-readable output.

## Releasing

To release a new version (from monorepo):

```bash
git tag mrl-v0.3.0 && git push origin mrl-v0.3.0
```

The workflow automatically builds binaries, uploads to R2, and updates the Homebrew tap.

To sync this standalone repo after changes (from monorepo):

```bash
just cli-push-mrl
```
