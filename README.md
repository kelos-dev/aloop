# aloop

aloop is a Go framework for running one agent with a standing prompt, a durable inbox, and tools. A signal appends a short summary and a source reference. The Decisions API considers the pending entries together and decides whether to wake the worker. When it wakes, the worker sees the whole snapshot so it can act on related signals together.

For example, a reply that moves a deadline and a daily timer can wake the same agent. The worker can read the email, update the existing task, and send a digest containing the revised date. A reply received during that work waits for the next snapshot.

## Run the personal agent

The supplied command reads a local Maildir, manages tasks through Todoist, and prints notifications to stdout. Each notification also has a durable text file in the configured directory. The first collection includes existing mail in both `new` and `cur`. Choose a Maildir containing the messages you want the agent to handle.

Install Go 1.25 or later and provide a Todoist token in `TODOIST_API_TOKEN`. Save this configuration as `aloop.json`, using your own Maildir path, time zone, and credentials.

```json
{
  "prompt": "Help me manage deadlines. Ignore newsletters. Check my current tasks before creating or updating one. When the daily digest is due, finish related updates first, then notify me of my current list.",
  "maildir": "/home/me/Mail/INBOX",
  "inbox": "./inbox.db",
  "notifications": "./notifications",
  "timezone": "America/New_York",
  "daily_at": "09:00",
  "triage": {
    "provider": "openai-decisions",
    "codex_home": "/home/me/.codex"
  },
  "worker": {
    "provider": "codex",
    "codex_home": "/home/me/.codex"
  }
}
```

```sh
go run ./cmd/aloop -config aloop.json -once
go run ./cmd/aloop -config aloop.json
```

The first command collects available signals and runs one snapshot, including any selected tool actions. The second keeps running. The poll interval defaults to 30 seconds. On the first poll at or after the configured local time, the scheduler emits that day's digest signal. Its stable date key prevents duplicates across restarts. Both models receive the current time in the configured time zone.

### Configure triage and the worker

Triage defaults to `openai-decisions` when its configuration is omitted. It calls OpenAI’s `/v1/decisions` endpoint with `gpt-6-luna`. Each named choice question classifies one pending entry as `run`, `dismiss`, or `defer` using the standing prompt, time, and full snapshot as shared input. Snapshots larger than 400 entries use several requests with the same shared input. A refusal or incomplete set of answers leaves the snapshot pending for retry.

By default, aloop uses the returned choice. An optional numeric `triage.run_threshold` from 0 to 1 selects `run` when its probability reaches the threshold. Below the threshold the higher of `dismiss` and `defer` wins, with ties deferred. A lower threshold can trigger more worker runs. Decisions can defer until another signal. The worker and prompted triage providers can also set an explicit retry time.

For Decisions, set `key_env` to the name of an API key variable or set `codex_home` to use the credentials in its `auth.json`. Without either setting, aloop tries `OPENAI_API_KEY`, then `CODEX_HOME` or `~/.codex`. File credentials are reread for each request. Keep the Codex login current when using its access token.

Choose a worker provider from the table. Explicitly selecting one of these for triage uses a prompted JSON completion.

| Provider | Configuration and credentials |
| --- | --- |
| `claude-code` | Install a Claude Code CLI supporting `--safe-mode`. Generate a token with `claude setup-token` and supply it through `CLAUDE_CODE_OAUTH_TOKEN`. Set `"provider": "claude-code"` and optionally `model`. |
| `codex` | Install Codex CLI supporting `exec --ignore-user-config` and sign in using file credential storage. Set `codex_home` to the absolute directory containing `auth.json`, or use `CODEX_HOME` or the default `~/.codex`. The CLI manages the file. Optionally set `model`. |
| `openai-compatible` | Set `base_url` and `model` for an endpoint supporting Chat Completions. The client reads the key from `OPENAI_API_KEY` by default. Use `key_env` to select a different variable, or leave the key unset for a local endpoint. |

aloop checks tool names, arguments, and inbox IDs before dispatching calls. The CLIs run in temporary working directories. For Codex, aloop reads the CLI’s model catalog and applies tool settings to a temporary copy so the model returns JSON with its native tools disabled. aloop executes the registered tools.

## Build an agent

Open one inbox with `aloop.OpenInbox(path)`. Construct the agent with `aloop.New(inbox, standingPrompt, location, decisions.Decide, workerModel, tools...)`. Start `agent.Run(ctx, retryInterval)` and let signal producers append entries concurrently. Continuous mode coalesces arrivals for up to 200 milliseconds before taking its snapshot. A producer can also group related signals in one atomic append.

```go
err := inbox.Append(
    aloop.Entry{ID: "mail:E42", Source: "email", Ref: "E42", Summary: "Maya moved the expense report deadline to Monday"},
    aloop.Entry{ID: "digest:2026-11-26", Source: "schedule", Ref: "2026-11-26", Summary: "Daily digest due"},
)
```

Use a stable, source-qualified ID per occurrence. A later reply gets its own ID. Supply a tool's name, description, required text parameters and Go function in an `aloop.Tool`. Read tools can fetch current data using the event reference. Register a tool with `Effect: true` for a user-visible action. Return a compact external reference from effect tools and use `Call.Key` as the destination's idempotency key. The personal agent under `internal/personal` demonstrates both kinds of tools.

## Scheduling and recovery

- If triage selects no work, aloop settles its dismissals and deferrals. If any entry needs work, the worker receives the whole snapshot and the recommendations. It resolves every entry so a timer and the records needed for its digest can be handled together. Deferred entries stay pending until another signal or a specified time makes the inbox eligible again. Deferring an entry again without specifying a new time preserves a wake time still outstanding at the start of the run. Duplicate delivery of a handled entry does not wake the agent.
- The worker can use multiple tools before reporting which entries are done or deferred. Only IDs from the worker's batch can be resolved. A single worker runs at a time and has a limit of 32 model steps per run.
- The local bbolt database stores pending summaries and references, received IDs, and effect receipts. Mail bodies and the authoritative task list stay in Maildir and Todoist. Read-tool results are scoped to the worker run.
- Each effect tool may act once per entry. Retrying the same call returns its saved result. A conflicting call fails. A destination that honors the stable operation key also covers the crash window between a successful external write and the local receipt.
- Tools can return an `aloop.ToolError` with an actionable message that is safe for the worker to see. The worker can recover or defer the affected entry and continue other work. Model, storage and other unhandled errors leave unfinished entries pending. Continuous mode retries on the next wake or poll. One-shot mode reports the error and exits.

## Development

| Command | Result |
| --- | --- |
| `make verify` | Check Go formatting, module files, and `go vet`. |
| `make build` | Build the static executable at `bin/aloop`. |
| `make test` | Run unit and component tests. |
| `make test-e2e` | Exercise the CLI across process restarts and run the email-to-digest scenario with local fixtures. |
| `make image` | Build and load `ghcr.io/kelos-dev/aloop:latest` with Docker Buildx for the local Linux architecture. |

To exercise live Decisions triage and a signed-in Codex worker, run `ALOOP_LIVE_CODEX=1 make test-e2e`. This opt-in test sends synthetic inbox summaries to Decisions and runs the registered tools through a Codex worker using your account. It checks triage decisions, a task update against a local Todoist fixture, and notifications written to disk.

The image includes Claude Code and Codex and runs as the non-root `node` user. Mount the configuration, Maildir, and writable state and notification directories at the paths in your configuration. Supply credentials at runtime. Use `REGISTRY` and `VERSION` to change the image tag. To publish a multi-platform image, set `IMAGE_PLATFORMS=linux/amd64,linux/arm64 PUSH=true` on the image target.
