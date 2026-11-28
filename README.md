# aloop

aloop is a Go framework for running one agent with a standing prompt, a durable inbox, and tools. A signal appends a short summary and a source reference. The Decisions API considers the pending entries together and decides whether to wake the worker. When it wakes, the worker sees the whole snapshot so it can act on related signals together.

For example, a reply that moves a deadline and a daily timer can wake the same agent. The worker can read the email, update the existing task, and send a digest containing the revised date. A reply received during that work waits for the next snapshot.

## Run the personal agent

The supplied command reads Gmail, a local Maildir, or both, and prints notifications to stdout. Each notification also has a durable text file in the configured directory. Set `TODOIST_API_TOKEN` if you want the agent to list, create, and update Todoist tasks. The first Maildir collection includes existing mail in both `new` and `cur`. Choose a Maildir containing the messages you want the agent to handle.

Install Go 1.25 or later and sign in to the Codex CLI with file credential storage. Save this configuration as `aloop.json`, using your own Maildir path, time zone, and Codex home.

```json
{
  "prompt": "Notify me about urgent email. Ignore newsletters. Save routine updates and deadlines for one daily digest.",
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

### Connect Gmail and optional Google Drive

Open the private **Signal inbox**, choose **Connect with Google**, and approve read access to Gmail. Select **Also allow reading linked Google Docs** before connecting if you want the worker to read Docs. The inbox shows the connected account and granted access after Google redirects you back. You can reconnect from the same panel. [Dev signal inbox](https://dev-aloop.kelos.dev/inbox)

The operator configures a Google OAuth application once:

1. In Google Cloud, enable the **Gmail API** and optionally the **Google Drive API**. Configure the consent screen and add your account as a test user if the app is in Testing.
2. Create a **Web application** OAuth client. Add `https://dev-aloop.kelos.dev/oauth/google/callback` as an authorized redirect URI, save, and download the client JSON. [Google Cloud credentials](https://console.cloud.google.com/apis/credentials)
3. In the repository settings, open **Environments → dev → Environment secrets**. Save the downloaded Web application JSON as `GOOGLE_OAUTH_CLIENT_JSON`. Deploy the PR on merge, or rerun **Deploy to Dev** on main after changing the setting.

For local use, set `google_redirect_url` to a registered callback such as `http://localhost:8080/oauth/google/callback`, set `web_listen`, supply the client JSON through `GOOGLE_OAUTH_CLIENT_JSON`, and set the dashboard password. You can omit `maildir` when Google is configured.

The first grant records the current Gmail mailbox position. Subsequent mail provides short signals to triage. The worker can fetch a body with `read_email` and, with Drive consent, export text by Google Doc ID with `read_google_doc`. The configured model providers receive those summaries and any content fetched during a run. Inbox cursors survive restarts. If Gmail expires a cursor, aloop checks inbox messages received since the saved position.

aloop stores the offline grant in a mode-0600 file beside the inbox database on its persistent volume. Access tokens refresh automatically. If Google expires or revokes the grant, use **Reconnect with Google**. External apps in Testing typically receive seven-day refresh tokens. [Google web authorization](https://developers.google.com/identity/protocols/oauth2/web-server) · [Google token expiration](https://developers.google.com/identity/protocols/oauth2#expiration)

### Web dashboard

Set `"web_listen": "127.0.0.1:8080"` in the configuration and start aloop in continuous mode. Open `http://127.0.0.1:8080` to see pending and handled signal counts, the processing flow, and completed tool actions. Refresh the page to read the current state. Leaving out `web_listen` keeps the HTTP listener disabled.

The overview is public. To enable the personal views, supply `ALOOP_WEB_PASSWORD` and sign in with username `aloop` over localhost or HTTPS. **Signal inbox** shows summaries and deferred signals. **Notifications** shows the 30 most recent messages saved by the agent.

### Chat and notification replies

Open **Chat** in the private dashboard to ask aloop a question. Choose **Reply in chat** on a notification to include that notification and its email references. For example, after a deadline alert you can ask “What exactly did Maya say?” and aloop can fetch the original from Gmail or Maildir. The configured worker receives the conversation context and any source content it reads.

User messages go directly to the same worker that handles signals. Messages and pending replies survive restarts. The transcript updates automatically while the page is open. Chat can answer from the conversation and read linked emails, including emails whose signals have already been handled. Mailbox-wide search and sending mail are outside its tool set.

A notification tied to an email keeps its source reference. A digest can cite multiple email entries through the worker's `email_sources` field. Existing notifications are imported on first startup. An earlier email notification can recover its source from its effect receipt; an earlier timer-only digest has only its saved text.

### Configure triage and the worker

Triage defaults to `openai-decisions` when its configuration is omitted. It calls OpenAI’s `/v1/decisions` endpoint with `gpt-6-luna`. Each named choice question classifies one pending entry as `run`, `dismiss`, or `defer` using the standing prompt, time, and full snapshot as shared input. Snapshots larger than 400 entries use several requests with the same shared input. A refusal or incomplete set of answers leaves the snapshot pending for retry.

By default, aloop uses the returned choice. An optional numeric `triage.run_threshold` from 0 to 1 selects `run` when its probability reaches the threshold. Below the threshold the higher of `dismiss` and `defer` wins, with ties deferred. A lower threshold can trigger more worker runs. Decisions can defer until another signal. The worker and prompted triage providers can also set an explicit retry time.

For Decisions, set `key_env` to the name of an API key variable or set `codex_home` to use the credentials in its `auth.json`. Without either setting, aloop tries `OPENAI_API_KEY`, then `CODEX_HOME` or `~/.codex`. File credentials are reread for each request. When the Codex CLI is installed, aloop asks it to refresh a rejected file login and retries that Decisions request once.

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
- The local bbolt database stores pending summaries and references, source cursors, received IDs, and effect receipts. It also retains the private chat transcript, including notification text, user questions, assistant answers, and source-email references and summaries for handled signals. Original emails, documents, and tasks remain in their source systems. A tool read is transient, but any passage quoted in a notification or answer persists with the transcript.
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

## Dev deployment

After CI passes on a push to `main`, **Deploy to Dev** publishes an amd64/arm64 image to GHCR and rolls out that digest on the Kelos dev cluster through Cloudflare Access. To redeploy the current revision, run `gh workflow run deploy-dev.yaml --ref main`.

The first deployment needs the following setup.

1. Make `DEV_CLUSTER_KUBECONFIG`, `CLOUDFLARE_ACCESS_CLIENT_ID`, and `CLOUDFLARE_ACCESS_CLIENT_SECRET` available to GitHub Actions in the `dev` environment. Use the same kubeconfig format and Cloudflare Access endpoint as Kelos.
2. The deploy workflow creates the dedicated namespace `aloop`. Both deployment and token refresh use it. To use another existing namespace, set the GitHub `dev` environment variable `ALOOP_NAMESPACE`. The cluster needs to provision a 1 GiB volume and pull `ghcr.io/kelos-dev/aloop`. For a private image, configure pull credentials on the namespace's default service account.
3. Set the GitHub `dev` environment secret `CODEX_AUTH_JSON` to a Codex login file. To give the pod its own refreshable login, sign in with `CODEX_HOME="$dev_codex_home" codex login -c 'cli_auth_credentials_store="file"'` using a separate directory, then run `gh secret set CODEX_AUTH_JSON --env dev < "$dev_codex_home/auth.json"`. Optionally set `ALOOP_WEB_PASSWORD` for private views. The workflow applies each supplied key to the Kubernetes secret `aloop-credentials`. If you leave the GitHub password unset, you can manage `ALOOP_WEB_PASSWORD` directly in the Kubernetes Secret. Redeploy after replacing the login or password.
4. Include `kelos-dev/aloop` in the `kelos-secret-manager` GitHub App installation and make `KELOS_SECRET_MANAGER_APP_ID` and `KELOS_SECRET_MANAGER_PRIVATE_KEY` available to the `dev` workflow. It requests an installation token scoped to aloop with **Environments: write** permission.

Decisions triage and the Codex worker share a writable Codex home on the persistent volume. The pod keeps CLI credential refreshes across restarts and imports a replacement login when the supplied file changes. It reads mail delivered to `/var/lib/aloop/Maildir` and schedules an email digest at 09:00 UTC. The same volume stores the inbox database and notifications. Read notifications with `kubectl -n aloop logs deployment/aloop`, substituting your namespace if needed. [Dev agent configuration](deploy/dev/aloop.json)

When moving an existing dev agent from `default` to `aloop`, the first deploy pauses the source agent, copies its credential Secrets, and streams the offline `aloop-data` volume to a new claim. It verifies the copy before transferring NodePort `30086`. A failed copy restarts a previously running source agent. After verification, rerun **Deploy to Dev** to resume an interrupted cutover. The workflow retires the old Deployment and its credentials after the new agent is ready. It retains the source PVC in `default` as a point-in-time backup of the inbox, mail, notifications, and Codex login.

**Refresh Codex Auth** runs daily at 06:00 UTC. To run it manually, use `gh workflow run refresh-codex-auth.yaml --ref main`. It verifies write access, waits for any active Codex call, and renews the pod’s login once. It saves that bundle to the Kubernetes Secret `aloop-codex-recovery` and the GitHub `dev` environment secret `CODEX_AUTH_JSON`. If the GitHub write fails after Kubernetes saves the bundle, rerun the workflow to publish it.

A redeploy with the mirrored secret preserves any newer login on the volume. An empty Codex home restores the matching recovery copy. To switch accounts or recover a revoked login, replace `CODEX_AUTH_JSON` in GitHub and redeploy before refreshing.

The dev hostname forwards to NodePort `30086` on Service `aloop`, which sends HTTP to the agent on port `8080`. The deployment workflow checks the public overview after rollout. [Dev dashboard](https://dev-aloop.kelos.dev/)
