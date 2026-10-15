# io.cwrau.pr-watch-mcp

Persistent PR/MR watch service for Agent of Empires: an AoE Tier-1 plugin
worker that polls registered GitHub PRs and GitLab MRs, paces itself
against live rate-limit budgets, and wakes the owning AoE session directly
on review/CI/merge activity.

## How it works

One always-on process does four jobs:

1. **MCP-over-HTTP server.** Exposes `register`, `unregister`, `list` and
   `dismiss_review` tools that any AoE session's MCP client can call
   directly. A registration is owned by the session that created it: only
   that session can unregister it or dismiss its reviews, and another
   session cannot take it over — but multiple sessions can independently
   watch the very same PR/MR, each with its own row (the registry's primary
   key is `(pr_url, session_id)`, not `pr_url` alone), its own state, and
   its own notifications; polling still costs one real check per distinct
   PR/MR regardless of how many sessions share it. `list` defaults to every
   registration
   across every session and status, filterable by an optional `session_id`
   and/or `status`. Both `register` and `unregister` take `pr_urls`, a list
   of one or more URLs handled in a single call — `register` registers every
   one under the same `session_id`/`events` filter, reporting a per-URL
   `results` array (`{pr_url, registered, error?}`) so one malformed URL in
   a batch never sinks the rest; the whole call only fails outright if every
   URL in it failed. `unregister`'s `pr_urls` is optional — give it specific
   URLs to stop just those (same per-URL `results` shape,
   `{pr_url, removed}`), or omit it entirely to stop every registration
   `session_id` owns in one call, unchanged from before. Every `session_id`
   input accepts either the bare AoE session id or the full
   `$AOE_ARTIFACT_DIR` value verbatim — the basename is extracted
   server-side, so a caller never has to shell out to `basename` itself.
   `register` also takes an optional `events` filter — a list of
   event kinds (`REVIEW`, `REVIEW_DISMISSED`, `COMMENT`, `CI`, `CI_RECOVERED`, `CI_SUCCESS`,
   `CONFLICT`, `MERGED`, `CLOSED`, `REOPENED`, `MERGE_QUEUE`) to notify on; omitted or empty
   means every kind except the opt-in ones (`CI_SUCCESS`: CI turned green, whatever it was
   before), which only a filter naming them lets through. If a recovery fires both
   `CI_RECOVERED` and `CI_SUCCESS`, the session gets the `CI_RECOVERED` ping only. Re-registering the same `(pr_url, session_id)` pair with a
   different `events` list just replaces the filter in place — no separate
   tool needed to change it. `dismiss_review` clears a GitHub review's badge — a real GitHub
   dismissal if possible, falling back to re-requesting the reviewer,
   falling back to a local-only badge clear when neither GitHub action
   applies or succeeds (always the case for a `COMMENTED` review, which
   GitHub's API can't dismiss at all).
2. **Auto-watch discovery.** No `register` call needed for the PR/MR
   already open on whatever branch a session's workspace happens to be on.
   Discovery is event-driven with a slow fallback. AoE pushes a
   `session.status.changed` notification on every session status change
   (needs the `session.read` capability); when a session goes `Idle` — the
   agent stopped working, so it has likely just pushed and opened a PR —
   the worker waits a few seconds (coalescing a burst of such events) and
   runs one discovery pass for just that session, using its session
   directory (see below; re-listed only if the session isn't in it). Events
   aren't replayed if the worker was down, and a PR
   can be opened outside any session's own turn, so the same pass also runs
   for every live session every 5 minutes (and once at startup) as a
   fallback.

   The worker keeps that session directory (lifecycle, title, checkouts)
   from the host over RPC, never by running the `aoe` CLI: one
   `sessions.list` at startup, kept current by the host's
   `session.lifecycle.changed` notifications (created, archived, trashed,
   renamed, deleted, workspace changed), with a full re-listing every 10
   minutes as the backstop for anything missed, since notifications aren't
   replayed.

   A pass takes the session's git checkouts (source path + current branch,
   from the session summary's `workspace_repos` and worktree, no separate
   directory walk) and resolves each checkout's `origin` remote (a local
   `git remote get-url origin`, no network) to a provider identity — an
   exact `github.com` host is GitHub; any other host is only treated as
   GitLab if a `glab` credential actually resolves for it, since there's no
   way to recognize an arbitrary self-hosted GitLab instance from its URL
   shape alone. That identity is cached per checkout for the worker's whole
   lifetime (only the branch is re-read each pass, since — unlike the
   remote — it changes often): a `glab config get token` probe measured
   ~300ms warm, ~2s cold, real subprocess cost, unlike the ~5ms a plain
   `git` call costs. The branch asked about is the one the PR/MR would
   actually carry as its head ref: normally the local branch name, but a
   branch pushed under a different name (`git push fork local:other`, no
   upstream set) is looked up by that pushed name instead — found via the
   remote-tracking ref git leaves at HEAD, ignoring refs on the default
   branch (and `main`/`master`). Unpushed local commits past that ref fall
   back to the local name until the next push. Then it asks the provider
   (GitHub batched across repos in one aliased GraphQL query, GitLab one
   REST call per checkout) whether that branch already has an open PR/MR.
   Both kinds of pass probe every still-unresolved checkout (of the
   notified session, or of every live session); there's no per-target
   backoff, the pass cadence is the pacing, so a branch without a PR costs
   one probe per 5 minutes plus one per idle event. A hit registers it exactly as if the session had called
   `register` itself — same state, same notifications, same everything from
   that point on, no separate "auto" bookkeeping. Once resolved for a
   branch, that checkout is left alone until the branch changes again, and
   an explicit `unregister` call is never silently undone by a later pass
   rediscovering the same PR/MR (a target already registered, at any
   status, is never re-registered).
   Explicit `register` still works exactly as before, whether to attach an
   `events` filter, re-filter an already-auto-discovered PR/MR, or watch a
   second PR/MR the current branch has nothing to do with.
3. **Poll scheduler.** Keeps one due-heap per provider, reads the
   rate-limit signal off every real API response (GitHub's GraphQL
   `rateLimit` field and `X-RateLimit-*` headers, GitLab's `RateLimit-*`
   headers) and spreads the remaining budget across the live registrations,
   never planning to use more than 90% of a pool. Checks are floored at 15s
   per registration and fall back to 5 minutes when a host reports no
   rate-limit signal at all. Each poll diffs the PR/MR's current review
   count, CI bucket, unresolved-thread count, and top-level comment count
   against the last-seen state; only a real change produces an event.
   Top-level comments (the Conversation tab, e.g. a bot's walkthrough
   summary) are tracked separately from inline review threads — confirmed
   live, a plain top-level comment with no review or inline thread attached
   at all (github.com/agent-of-empires/agent-of-empires/pull/4132) never
   pinged before this existed, since only reviews and review threads were
   ever fetched. A new review landing in the same tick a new unresolved
   thread or top-level comment appears suppresses that `COMMENT` event —
   confirmed live, a `COMMENTED` review whose only content is an inline
   comment fires both with the same href, so it's the same activity, not
   two, and a review submission commonly posts its own top-level walkthrough
   comment alongside it too — leaving just `REVIEW` to report it. A `REVIEW` event, or a
   top-level-comment `COMMENT` event, whose
   author is the worker's own authenticated account (the human's `gh`/`glab`
   identity — an agent acting on their behalf shares the same credential,
   so GitHub/GitLab can't tell the two apart) is dropped before
   notifying, unconditionally, not behind a setting: the registration's
   state and badge still update normally, only the ping — which would
   otherwise just be the worker telling you about your own action — is
   suppressed. Confirmed live: a self-authored top-level comment (an agent
   noting a stale label needed no action) pinged same as anyone else's —
   self-filtering only ever covered REVIEW until this existed. A
   thread-based `COMMENT` (a new unresolved review thread) isn't covered
   yet — neither provider's thread data carries a per-thread author today.
   The identity is resolved once per checker instance and
   cached (a GraphQL `viewer { login }` query for GitHub, one
   `GET /api/v4/user` per GitLab host), not looked up on every poll. A
   review that was present at the last check and is simply gone at this one
   — with nothing newer replacing it — fires `REVIEW_DISMISSED` instead of
   staying silent: GitHub's "dismiss stale approvals on push" branch
   protection and GitLab's "reset approvals on push" project setting both
   clear a review with no event of their own, so without this the
   registration's state (and badge) would just quietly revert to "no
   review" with nothing to explain why. CI going green again after a
   failure fires `CI_RECOVERED` (once). The failing run is usually rerun
   after a fix push, so the poll that finally sees success typically follows
   one that saw pending; a sticky per-registration "CI was failing" flag,
   set by a failure and cleared by success, carries the memory across that
   gap. A `CI` failure the registration's filter hides still sets the flag,
   so the recovery ping is independent of the failure ping.
4. **Notifier.** Calls the host's `sessions.message.send` RPC (capability
   `session.message`) on a state-change event. The host wakes a stopped
   session and steers the message into a busy agent or queues it, like
   typing into the session. Archived sessions are skipped (the host
   refuses them) but keep their registration; trashed or deleted sessions
   have theirs dropped. A merged or closed PR/MR sends a final
   notification and then deregisters itself.

Registrations live in a local SQLite file
(`$XDG_STATE_HOME/pr-watch-mcp/registry.db`, default
`~/.local/state/pr-watch-mcp/registry.db`), so the worker can restart
without losing state. It used to sit under the config dir; an existing
`~/.config/pr-watch-mcp/registry.db` is moved over once on first start of
the new version (never overwriting a database already at the new path).

## Snoozing

Configure **Snooze rules** in the plugin's AoE settings (TUI or web). Each
rule has a list of URL patterns and a gate command. While the gate exits
non-zero, every watched PR/MR whose URL matches one of the rule's patterns is
snoozed: the worker skips the provider call entirely, so nothing is polled
and nothing is notified. When the gate exits 0 again, the next check diffs
against the state from before the snooze and reports everything that
happened in between in one go.

- **Patterns** match the PR/MR URL without scheme, case-insensitively. `*`
  matches any characters including `/`, `?` matches exactly one. For example
  `gitlab.example.com/*` or `github.com/my-org/*`.
- **Gate** runs through `sh -c`, once per scheduler tick per distinct
  command. Exit 0 means open (watch normally), any other exit means closed
  (snoozed). For example `systemctl --user is-active --quiet work.target`
  snoozes while that target is inactive. A gate that cannot run at all
  (timeout, `sh` missing, command not found) is treated as open and logged,
  so a typo never silently mutes notifications.
- **Every matching rule applies.** A PR/MR is only checked while the gates
  of all rules matching it are open, so rules work as stages: for example one
  rule for `github.com/my-org/*` gated on work hours and one for `*` gated on
  "not in a break" keeps work PRs quiet outside work hours and during breaks,
  while other PRs are only held back during breaks. The order of the rules
  does not matter. A PR/MR matching no rule is never snoozed. With no rules
  configured (the default) nothing is snoozed.
- Rules apply to every registration alike, whether it was registered with the
  `register` tool or found by auto-watch discovery. Changes take effect
  without restarting the worker.
- A snoozed registration is re-checked against its gate every minute.

## Comment filters

Some bots post top-level comments that are pure noise as a ping, e.g.
CodeRabbit's "review in progress" placeholder. Configure **Comment filters**
in the plugin's AoE settings (TUI or web) to leave them out. A rule has an
**author** (login/username, case-insensitive, a trailing `[bot]` ignored), a
list of **body contains** texts (case-insensitive substrings), or both. When
both are set, the author must match and the body must contain at least one
of the texts, so one rule can cover several phrases for the same author
without repeating it. A row with neither an author nor a non-blank text is
ignored, so a half-filled rule can't mute everything. Rules only apply to
top-level comments (the Conversation tab): never to reviews, `REVIEW` events
or inline threads.

- **Evaluated on the current body, every poll.** Everything is fetched, the
  rules are applied, and only then are comments counted and compared with
  the last-seen state. A filtered comment is not stored as seen, so when its
  author edits it (CodeRabbit turns its placeholder into the real review)
  and it stops matching, it counts as a new comment and pings.
- Removing a rule has the same effect on whatever it was hiding: those
  comments show up as new on the next check.
- Rules apply to every registration alike, and changes take effect without
  restarting the worker. With no rules configured (the default) nothing is
  filtered.

## Requirements

- AoE v1.20.0 or newer (plugin `api_version` 15): `sessions.list` summaries,
  `sessions.message.send`, `session.status.changed`, the pane `markdown` block
  and `string_list` settings all ship in it. The worker's `session.read` and
  `session.message` capabilities must be granted. The worker does not run the
  `aoe` CLI.
- `session.lifecycle.changed` is not in a release yet. Without it the worker
  still works: new sessions are picked up by the 10-minute resync instead of
  immediately.
- Go (the version in `go.mod`): `aoe plugin install` builds the worker from
  source.
- `gh`, `glab` and `git` on `PATH`. `gh auth token` supplies the
  GitHub credential; the GitLab token is resolved per-host via `glab config
  get token --host <host>` at check time, so a host with no configured
  credential is never sent one (also how auto-watch discovery decides
  whether an unrecognized host is a configured GitLab instance worth
  probing at all). `git remote get-url origin` is auto-watch discovery's
  only other local dependency.
- The calling session's MCP client must be configured to reach the
  worker's HTTP port, which defaults to `8632` and is overridable via
  `PR_WATCH_MCP_PORT`. The server binds to `127.0.0.1` only.

## Building

```bash
CGO_ENABLED=0 go build ./cmd/pr-watch-worker
```

No cgo: storage uses `modernc.org/sqlite`, so the result is a single
static binary.

## Logs

The worker logs to stderr, which AoE writes to
`<aoe config dir>/plugin-workers/<worker-id>.log` (one file per worker start;
on Linux `~/.config/agent-of-empires/plugin-workers/`). Find the live one with
`rg -l 'pr_url' <dir> | xargs ls -t | head -1`. It records every event found
(`event`), every ping sent (`ping sent`) or held back (`ping suppressed`, with
the reason `filter` or `self-authored`), failed sends (`notify failed`) and
the rate-limit state after each provider pass.
