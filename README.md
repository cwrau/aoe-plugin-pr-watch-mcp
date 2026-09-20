# io.cwrau.pr-watch

Persistent PR/MR watch service for Agent of Empires: an AoE Tier-1 plugin
worker that polls registered GitHub PRs and GitLab MRs, paces itself
against live rate-limit budgets, and wakes the owning AoE session directly
on review/CI/merge activity.

## How it works

One always-on process does three jobs:

1. **MCP-over-HTTP server.** Exposes `register`, `unregister` and `list`
   tools that any AoE session's MCP client can call directly. A
   registration is owned by the session that created it: only that session
   can unregister it, and another session cannot take it over.
2. **Poll scheduler.** Keeps one due-heap per provider, reads the
   rate-limit signal off every real API response (GitHub's GraphQL
   `rateLimit` field and `X-RateLimit-*` headers, GitLab's `RateLimit-*`
   headers) and spreads the remaining budget across the live registrations,
   never planning to use more than 90% of a pool. Checks are floored at 30s
   per registration and fall back to 5 minutes when a host reports no
   rate-limit signal at all. Each poll diffs the PR/MR's current review
   count, CI bucket and unresolved-thread count against the last-seen
   state; only a real change produces an event.
3. **Notifier.** Shells out to `aoe send <session_id> "<message>"` on a
   state-change event, which auto-revives a stopped session. Archived
   sessions are skipped but keep their registration; trashed or deleted
   sessions have theirs dropped. A merged or closed PR/MR sends a final
   notification and then deregisters itself.

Registrations live in a local SQLite file, so the worker can restart
without losing state.

## Requirements

- `gh`, `glab` and `aoe` on `PATH`. `gh auth token` supplies the GitHub
  credential; the GitLab token is resolved per-host via `glab config get
  token --host <host>` at check time, so a host with no configured
  credential is never sent one. `aoe` is used both to classify session
  lifecycle (`aoe list --json --all`) and to deliver notifications
  (`aoe send`).
- The calling session's MCP client must be configured to reach the
  worker's HTTP port, which defaults to `8632` and is overridable via
  `PR_WATCH_MCP_PORT`. The server binds to `127.0.0.1` only.

## Building

```bash
CGO_ENABLED=0 go build ./cmd/pr-watch-worker
```

No cgo: storage uses `modernc.org/sqlite`, so the result is a single
static binary.
