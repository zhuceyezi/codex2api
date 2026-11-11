# Codex2API · Aleafy

Fork of [james-6-23/codex2api](https://github.com/james-6-23/codex2api), an OpenAI / Anthropic compatible gateway with an administrator dashboard.

## Build and run

Requires the Go version declared in `go.mod`, Node.js 24 and npm.

```bash
cd frontend
npm ci --no-audit --no-fund
npm run build
cd ..
go build -o codex2api .
cp .env.sqlite.example .env
# Edit .env: DATABASE_PATH, ADMIN_SECRET, CODEX_PORT and CODEX_BIND.
./codex2api
```

Open `/admin/` for administration. Configure upstream accounts and API keys before serving requests. For production, keep `CODEX_BIND=127.0.0.1` and use an HTTPS reverse proxy. See [configuration](docs/CONFIGURATION.md), [deployment](docs/DEPLOYMENT.md), [usage](docs/USAGE.md) and [API reference](docs/API.md).

## Conversation history

Conversation payload retention is on by default for all API keys. In **请求记录**, select **查看对话** to read input messages and the returned answer, or inspect the original payload. **对话留存** configures retention from 1 to 365 days; the default is 7 days. Expired content is immediately hidden and deleted by a background job every 30 minutes. Historical requests recorded before this feature have no payload to display.

Chat Completions, Responses, Anthropic Messages HTTP streams and individual Responses WebSocket turns are captured while forwarding. Payloads are written asynchronously as compressed data in the application database. Reading payloads and changing retention require the existing administrator authentication; API-key self-service routes do not expose content. Authentication headers are not stored. Prompts and replies themselves can contain confidential code and secrets. Each input/output is capped at 8 MiB and visibly marked if truncated. Multipart binary attachments are not retained. Database backups may retain older copies until backup rotation.

## Maintaining this fork

The Aleafy deployment runs `ops/sync_deploy.py` daily at 04:30 Asia/Shanghai. It fetches `upstream/main`, merges it into the fork, applies `ops/repo_policy.py`, builds the frontend and backend, starts an isolated empty-database health check, pushes the integration, and deploys. Code conflicts or failed builds stop the update without replacing the running service. Failed starts restore both the executable and SQLite database. Only the most recent two deployment backups are retained.

```bash
python3 /home/aleafy/code/codex2api-fork/ops/sync_deploy.py
```

The maintenance script targets `/home/aleafy/code/codex-api` and its user-level `codex2api.service`. Adapt these constants before deploying elsewhere. The dashboard's upstream-binary update is disabled, so it cannot overwrite fork features. Maintenance output lives under `~/.local/state/codex2api/updater/`.

Only application code, build/runtime configuration and basic usage documentation are tracked. Verification sources and process artifacts remain local and are excluded during upstream integration.

## License

The upstream project declares the MIT License. Original project: james-6-23/codex2api.
