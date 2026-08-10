# ToolTend

English | [简体中文](README.zh-CN.md)

**Bundle lifecycle manager for coding-agent tooling.**

*Keep your coding-agent tooling current.*

ToolTend is a local lifecycle manager for Codex and Claude Code. It groups the CLI, Skill, Hook, App, configuration, and embedded binaries of the same tool product into a Bundle, then manages them with one Release, one policy, and one transaction. When multiple agents share the same physical installation, ToolTend updates it only once.

ToolTend v0.2 does not provide an extension marketplace, online recipes, search, or uninstall support. It also never takes over existing tools automatically during initialization. Component and Binding remain available as low-level discovery evidence and compatibility interfaces, but they are no longer scheduling units for automatic updates.

## Installation

Install an official build from GitHub Releases into `~/.local/bin`. The installer downloads the matching platform asset over GitHub HTTPS and verifies its byte size and SHA-256 against the release manifest:

```bash
curl -fsSL https://raw.githubusercontent.com/z2z23n0/tooltend/main/install.sh | bash
```

Installing from source requires Go 1.22 or later:

```bash
./scripts/install.sh
```

You can also install a development build from the Go module. Development builds do not include the release public key and cannot perform signed self-updates:

```bash
go install github.com/z2z23n0/tooltend/cmd/tooltend@latest
```

Make sure `$(go env GOPATH)/bin` or `~/.local/bin` is in your `PATH`, then run:

```bash
tooltend version
tooltend init
```

`tooltend init` only reads local state until the final confirmation. After confirmation, it installs ToolTend's own Hook, shim, and scheduler, then scans and groups Bundles. It does not migrate runtimes, run external installers, or queue takeover tasks. Every discovered Bundle starts in the `unconfigured` state.

To rebuild an existing ToolTend state completely, preview the reset before confirming it:

```bash
tooltend init --reset-state --dry-run
tooltend init --reset-state --yes
```

Before resetting, ToolTend acquires the global lock, checks managed objects and unfinished journals, and backs up configuration, state, database, data, and infrastructure state to a neighboring `tooltend-backups/<timestamp>/` directory. If any step fails, it restores the previous state and scheduler.

## Bundle model

```text
Bundle
  ├─ BundleRelease: one exact, fully resolved release
  ├─ BundleArtifact: CLI / Skill / Hook / App / Config / Binary
  ├─ Installation: one unique physical installation
  ├─ ConsumerBinding: how Codex / Claude / a project consumes it
  └─ Policy / Transaction / Receipt / Health Check
```

Each Bundle has a fixed lifecycle owner:

| Owner | Behavior |
|---|---|
| `tooltend` | ToolTend performs staging, activation, verification, and rollback |
| `delegated` | Orchestrates npm, mtskills, or an official installer, then verifies and records the result |
| `host-owned` | Managed by Codex/Claude; ToolTend only observes |
| `app-owned` | Managed by the App's own updater; ToolTend only observes |
| `workspace-linked` | Linked to a local repository; observes its commit and health by default |
| `unresolved` | Cannot be identified with high confidence; automatic updates are prohibited |

Built-in `bundle-recipe-v1` recipes ship with the binary. Put local extensions in `~/.config/tooltend/bundles.d/*.toml`; the first configuration requires explicit trust. Commands may declare only static argv and cannot use shell strings.

## How it works

ToolTend does not run an always-on daemon:

```text
SessionStart / ToolUse / daily task / user command
                    │
                    ▼
          tooltend hook / kick
       record a redacted event and return immediately
                    │
                    ▼
       tooltend reconcile --once
  exclusive lock → recovery → scan/group → Bundle transaction → exit
                    │
                    ▼
       Bundle Receipt and health status
```

- The Hook hot path does not access the network, aggregate state, or call a model, and SQLite uses `busy_timeout=0`. It fails open if the database is busy or the input is invalid.
- `kick` starts a single detached, one-shot worker. A global file lock prevents concurrent sessions from updating in parallel.
- macOS uses launchd, while Linux uses a systemd user timer. Each starts `reconcile --once` once per day; neither runs a persistent ToolTend process.
- Every reconcile persists its full run state. After the main task, an independent watchdog checks for missed, failed, or unfinished runs. Failures trigger desktop notifications by default and a follow-up reminder at the next Codex/Claude SessionStart.
- On macOS, the installer uses Xcode Command Line Tools to build and register `ToolTend Notifier.app` in `~/Applications`. You must allow notifications at the first system prompt. ToolTend no longer borrows Script Editor's notification identity, and `tooltend doctor` checks installation and authorization state.
- macOS scheduler output is stored in `~/.local/state/tooltend/logs/` instead of being discarded to `/dev/null`. `tooltend status` and `tooltend doctor` show the most recent complete reconcile result.
- Bundles that have not been configured with `bundles configure` do not check for updates, download files, or invoke installers.
- A Bundle update resolves, verifies, and stages every Artifact before activating each physical Installation. Failures are compensated in reverse order.
- Bundle transactions use a step journal. Interruptions, failures, rollbacks, and health checks all produce auditable Bundle-level Receipts.

## Policies

Every Bundle has one explicit policy:

```toml
mode = "auto" # auto | manual | observe | ignore
```

- `auto`: allowed only when the recipe provides exact resolution, complete staging, activation, health checks, and reliable compensating rollback.
- `manual`: allows update checks, but applying the full Bundle requires confirmation every time.
- `observe`: records only versions, sources, drift, and health; never runs replacement commands.
- `ignore`: preserves discovery evidence but does not check for updates.

`host-owned`, `app-owned`, `workspace-linked`, and `unresolved` Bundles can use only `observe` or `ignore`. Pressing Enter in interactive configuration skips the Bundle, and unselected Bundles always remain `unconfigured`.

## Updates and rollback

Every Bundle update follows this exact sequence:

```text
Resolve the complete BundleRelease and exact Artifact versions
→ download everything, verify integrity, and stage
→ check compatibility, permissions, Hooks, and local modifications
→ update each physical Installation exactly once
→ run derived steps and Bundle/Artifact health checks
→ commit the Receipt; compensate in reverse order on failure
```

Delegated drivers reuse the user's existing authentication environment but never save or print registry tokens, environment variables, or full commands. Default timeouts are 30 seconds for resolution, 5 minutes for installation, and 30 seconds for health checks, with up to 3 retries.

## Discovery evidence

Discovery prioritizes npm `package.json`, Git commits, GitHub Releases, local `.agents/.skill-lock.json`, mtskills source records and signed manifests, App `Info.plist`/code signatures/Sparkle, and repository links. `latest`, example commands, and dependency constraints in Skill documentation are requirement evidence only and are never treated as installed versions.

Codex plugin caches are managed by the Host. ToolTend groups them as `host-owned` observed objects and does not download or replace them. Objects that cannot be grouped with high confidence remain as fallback Bundles and appear only in `bundles list --all`.

## CLI

```text
tooltend init
tooltend init --reset-state --dry-run|--yes
tooltend scan
tooltend status
tooltend bundles list [--all]
tooltend bundles show <bundle>
tooltend bundles configure [--set <bundle>=auto|manual|observe|ignore]
tooltend bundles update [<bundle> | --all] [--stage-only]
tooltend bundles rollback <bundle> [--to <release-or-receipt>]
tooltend bundles history [<bundle>]
tooltend bundles doctor [<bundle>]
tooltend components list
tooltend components show <component>
tooltend policy set <component>
tooltend update [component | --all]
tooltend review [component]
tooltend history [component]
tooltend rollback <component> [--to <receipt-or-version>]
tooltend adopt <component> --source <source> [--subdir <git-relative-path>]
tooltend project init|export|sync
tooltend self status|update
tooltend doctor [--repair]
```

All commands support `--json`, and write operations support `--dry-run`. In non-interactive or JSON mode, a write operation without `--yes` returns `confirmation_required` with a complete preview instead of prompting or writing silently.

The `components`, `policy`, and `adopt` commands, along with single-component `update`, `rollback`, `history`, and `review`, are v0.1 compatibility entry points and emit deprecation warnings. They no longer determine the number of Bundles shown to users or Bundle update state.

JSON output uses a stable V1 envelope:

```json
{
  "schema_version": 1,
  "command": "status",
  "ok": true,
  "data": {},
  "warnings": []
}
```

When an agent submits a review, it must include the candidate ID, candidate hash, a `safe|conflict|uncertain` verdict, risk type, and summary. Judgments for stale candidates or mismatched hashes are invalid.

## Project reproduction

```bash
tooltend project init
tooltend project export
tooltend project sync --dry-run
tooltend project sync --yes
```

- `tooltend.toml` declares sources, component types, target agents, and version channels.
- `tooltend.lock` stores resolved versions/commits and integrity hashes. `project sync` uses them as exact target and artifact-hash constraints, then applies the confirmed preview with transactional compare-and-swap.
- Both files can be committed to a project repository. Secrets, tokens, source trust, and local apply modes are never written to them.

## Local data

ToolTend follows XDG directories by default. Set `TOOLTEND_HOME` to place all ToolTend data under one independent root directory.

| Content | Default location |
|---|---|
| Configuration | `${XDG_CONFIG_HOME:-~/.config}/tooltend/config.toml` |
| SQLite schema v5 state | `${XDG_STATE_HOME:-~/.local/state}/tooltend/state.db` |
| Activation lock | `${XDG_STATE_HOME:-~/.local/state}/tooltend/activation.lock` |
| Objects / staging / generations | `${XDG_DATA_HOME:-~/.local/share}/tooltend/` |
| Stable shims | `~/.local/bin/` |
| Local Bundle recipes | `${XDG_CONFIG_HOME:-~/.config}/tooltend/bundles.d/*.toml` |
| Reset backups | `tooltend-backups/<timestamp>/` next to configuration/state/data |

SQLite uses WAL and creates a backup before schema migration. The database never stores full prompts, transcripts, unparsed raw commands, environment variables, MCP secrets, or registry tokens. Hooks record only normalized package/version values, event types, and irreversible correlation hashes.

## ToolTend self-update

Official releases include raw arm64/amd64 executables for Darwin and Linux, `checksums.txt`, and an Ed25519-signed manifest. The installed self-updater verifies the signature with an embedded public key and also checks the release sequence, platform, SHA-256, and byte size. Any mismatch prevents staging; development builds refuse to self-update.

Homebrew installations only prompt you to run the corresponding `brew upgrade tooltend` and never bypass Homebrew to replace themselves.

## Development

Local development and GitHub Actions use the same validation entry point:

```bash
./scripts/ci.sh
```

The script checks `gofmt` and module consistency, then runs `go vet ./...`, `go test ./...`, and `go build ./...`. CI runs on both macOS and Linux to cover differences in file locking, generation pointers, and system scheduling.

## License

[MIT](LICENSE)
