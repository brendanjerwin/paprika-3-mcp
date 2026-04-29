# paprika-3-mcp (brendanjerwin fork)

A [Model Context Protocol (MCP)](https://modelcontextprotocol.io/introduction) server that exposes your **Paprika 3** recipes to LLM clients — with high-quality local search.

> Forked from [soggycactus/paprika-3-mcp](https://github.com/soggycactus/paprika-3-mcp). Adds a local Bleve search index, a `search_paprika_recipes` tool, env-var credentials, and a sync loop that uses Paprika's `/sync/status` counter as a delta probe before listing.

## 🚀 What's different in this fork

- **`search_paprika_recipes`** — Bleve full-text query string syntax with English stemming, fielded queries (`name:chili`), phrases, boolean operators, fuzziness, plus `limit` / `min_rating` / `category` filters. Returns ranked hits with highlighted snippets.
- **`get_paprika_recipe`** — fetch a single recipe from the local index by UID.
- **Local Bleve index** at `$XDG_STATE_HOME/paprika-3-mcp/recipes.bleve` (defaults to `~/.local/state/paprika-3-mcp/`). Source of truth for search and resource reads; rebuilt automatically on first run.
- **Background sync loop** that calls `/api/v2/sync/status/` first and skips the (more expensive) full recipe-list diff when the global `recipes` counter hasn't moved. Forces a deep diff every hour as a safety net.
- **Credentials via `PAPRIKA_USERNAME` / `PAPRIKA_PASSWORD` environment variables** — passing them on the command line is no longer supported because the password ended up visible in `ps`.
- **Logs to stderr** instead of `/var/log/paprika-3-mcp/server.log` (which silently failed for non-root users on Linux).

The original tools (`create_paprika_recipe`, `update_paprika_recipe`) still exist; they now write through to the local index immediately.

## 🛠 Installation

Download a prebuilt binary from the [Releases](https://github.com/brendanjerwin/paprika-3-mcp/releases) page, or build from source with `go build ./cmd/paprika-3-mcp`.

```bash
unzip paprika-3-mcp_<version>_linux_amd64.zip
sudo mv paprika-3-mcp /usr/local/bin/
paprika-3-mcp --version
```

## 🤖 Configuration

```json
{
  "mcpServers": {
    "paprika-3": {
      "command": "/usr/local/bin/paprika-3-mcp",
      "env": {
        "PAPRIKA_USERNAME": "<your paprika 3 email>",
        "PAPRIKA_PASSWORD": "<your paprika 3 password>"
      }
    }
  }
}
```

Most agent harnesses prefer to inject `env` from a secret store rather than embedding it in the JSON; do that. The previous `--username` / `--password` mode was removed.

### Flags

| Flag | Default | Notes |
|------|---------|-------|
| `--data-dir` | `$XDG_STATE_HOME/paprika-3-mcp` (or `~/.local/state/paprika-3-mcp`) | Where the Bleve index lives. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. |
| `--version` | — | Print version and exit. |

### Search query syntax

Bleve query-string syntax. Some examples:

```text
pinto bean                 # any of the terms (OR), stemmed
"smoked paprika"           # exact phrase (still stemmed)
name:chili                 # fielded
ingredients:tahini -beef   # required + must-not-have
paprika~                   # fuzzy
```

Filters (passed as separate arguments, not in the query string): `min_rating: 4`, `category: "Mexican"`.

## 📄 License

MIT © 2025 [Lucas Stephens](https://github.com/soggycactus). Fork additions © 2026 Brendan Erwin.
