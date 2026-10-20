# PR Watch

Persistent GitHub PR / GitLab MR watcher for
[Agent of Empires](https://github.com/agent-of-empires/agent-of-empires)
(AoE). It watches the PRs/MRs your sessions work on and pings the owning
session, via AoE's own message channel, when something needs attention:
a review, a comment, CI turning red, a conflict, a merge.

- **Zero setup per PR.** It finds the open PR/MR on each session's git
  branch on its own, seconds after the session goes idle. Agents can also
  register PRs explicitly through an MCP tool.
- **Persistent.** A background worker owns the watch. It survives session
  restarts, wakes a stopped session with a ping, and keeps watching a merged
  or closed PR for 30 days so late reviews still arrive.
- **Quiet.** Several changes in one poll arrive as one message, several PRs of
  one session as one message per poll cycle. Partial approvals, empty bot
  reviews, your own comments and noisy bot comments don't ping.
- **Polite to the APIs.** It spreads the remaining rate-limit budget over the
  live watches instead of polling on a fixed timer.
- **Visible.** Row badges, a home-pane dashboard and a per-session pane show
  CI, review, conflict and merge-queue state at a glance.

## Requirements

- AoE v1.20.0 or newer (plugin `api_version` 15), with the plugin's
  `session.read` and `session.message` capabilities granted.
- Go, in the version `go.mod` names: `aoe plugin install` builds the worker
  from source.
- `git`, and `gh` and/or `glab` logged in on the machine that runs AoE:
  - `gh auth token` supplies the GitHub credential. What `gh auth login`
    grants by default is enough to poll; `dismiss_review` additionally needs
    write access to the PR.
  - The GitLab token comes from `glab config get token --host <host>`, per
    host, at check time. A host with no configured credential is never sent
    one, and a checkout on such a host is not auto-watched.
- Optional: AoE's `session.lifecycle.changed` notification (not in a release
  yet). Without it new sessions are picked up by the 10-minute resync instead
  of immediately.

## Install

```bash
aoe plugin install gh:cwrau/aoe-plugin-pr-watch-mcp
```

Update later with `aoe plugin update io.cwrau.pr-watch-mcp`. Grant the
requested capabilities when asked.

Agents reach the tools through MCP over HTTP on `127.0.0.1:8632` (change it
with `PR_WATCH_MCP_PORT` in the worker's environment). Add it to your agent,
for Claude Code:

```bash
claude mcp add --transport http pr-watch http://127.0.0.1:8632/mcp --scope user
```

The server tells agents how to read the pings and what to do with them (its
MCP `instructions` and tool descriptions are the agent-facing manual), so no
extra skill is needed.

## What you get pinged about

A ping is a message typed into the session, starting with the PR ref:

```text
[pr-watch] owner/repo#42: CI failure
[pr-watch] owner/repo#42, 2 changes:
- REVIEW alice CHANGES_REQUESTED, 1 unresolved thread(s)
- CONFLICT has a merge conflict with its target branch
[pr-watch] 2 updates:

owner/repo#42: CI failure

group/project!17: MERGED
```

| Event              | When                                                                                                 |
| ------------------ | ---------------------------------------------------------------------------------------------------- |
| `REVIEW`           | A review landed (approved, changes requested, commented), with the unresolved thread count.          |
| `REVIEW_DISMISSED` | A review that was there is gone, usually auto-cleared by a push.                                     |
| `COMMENT`          | A new unresolved thread, a new top-level comment, or a reply inside an open thread.                  |
| `CI`               | CI just turned red. Cancelled stale runs after a force-push don't count.                             |
| `CI_RECOVERED`     | CI was failing and is green again.                                                                   |
| `CI_SUCCESS`       | CI just turned green. **Opt-in**: only sent when named in the `events` filter.                       |
| `CONFLICT`         | The PR/MR now conflicts with its target branch.                                                      |
| `MERGE_QUEUE`      | GitHub: the PR was removed from the merge queue without merging.                                     |
| `MERGED`, `CLOSED` | Terminal. Always sent alone, and the row badge records the outcome.                                  |
| `REOPENED`         | A closed PR/MR is open again.                                                                        |
| `PUSH`             | **Opt-in.** The head commit moved: "new commits by alice, head now abc1234", or "force-push, ...".   |
| `REVIEW_REQUESTED` | **Opt-in.** A review was requested from *your* account. Requests for other people never ping.        |
| `READY_FOR_REVIEW` | **Opt-in.** The PR/MR left draft.                                                                    |
| `DRAFTED`          | **Opt-in.** The PR/MR went back to draft.                                                            |
| `THREAD_RESOLVED`  | **Opt-in.** Review threads were resolved: "2 thread(s) resolved, 1 unresolved left".                 |

### Personas

`register` takes `persona`: `author` (the default) for watching your own
PR/MR, or `reviewer` for reviewing someone else's. The persona only picks the
defaults, and an explicit `events` list always wins.

- **author**: everything above except the opt-in kinds, as before.
- **reviewer**: `REVIEW`, `REVIEW_DISMISSED`, `COMMENT`, `PUSH`,
  `REVIEW_REQUESTED`, `READY_FOR_REVIEW`, `DRAFTED`, `THREAD_RESOLVED`,
  `REOPENED`, `MERGED` and `CLOSED`. No CI, conflict or merge queue pings. The
  row badge, home pane and session pane show only the PR and a review chip that
  reads **in progress** until your own account approved, then **approved**.
  Another reviewer's approval does not count.

Auto-discovered watches are always `author`. The review-flow events have no
baseline until a poll has stored the PR's head commit, so a new watch never
floods you with them.

### Watching a repo, an org or a whole platform

`subscribe` watches a scope instead of one PR/MR: a repo, an org or GitLab
group, or a whole platform such as `github.com`. The worker searches the scope
for open PRs/MRs matching your filters and registers each match as an ordinary
watch (reviewer persona by default, silent first poll). You get one `MATCHED`
ping per new match:

```text
[pr-watch] owner/repo#12: MATCHED new PR by alice (https://github.com/owner/repo/pull/12)
```

The ping carries ref, author and URL, **never the title**: on a platform watch
anyone can open a PR with a hostile title, which would be a prompt injection
straight into the agent. Agents are told to fetch details themselves and treat
PR text as untrusted.

- **Scope** is a URL: `https://github.com/owner/repo`, `https://github.com/org`,
  `https://github.com`, a GitLab project, group or host. For GitLab the API
  tells project from group. A **platform** scope needs a person filter, because
  without one it would watch every open PR on the platform.
- **Filters** (all optional, `me` is your own account): `review_requested`
  (a login or a GitHub team), `assignee`, `authors`, `authors_not`, `involved`
  and `mentioned` (GitHub), `reviewed_by`, `review_status` (GitHub), `labels`,
  `labels_not`, `draft`, `base_branch`, `branch` and `branch_not` (globs),
  `repos` and `repos_not` (globs, for org and platform scopes) and
  `updated_within` (`14d`). A filter a provider cannot express is rejected, not
  silently dropped.
- **Lifecycle:** the first scan registers what already matches without a
  ping (`report_existing` announces it instead). A PR that stops matching, say
  because you approved it, stays watched until merged or closed
  (`release_when_unmatched` stops it instead). `max_watches` caps the watches a
  subscription keeps (25, or 10 for a platform; 100 at most) and overflow is
  reported once. Your own PRs are skipped for the reviewer persona. Unsubscribing, or the
  session going away, stops the registered watches.
- **Notify mode** (`mode: "notify"`) is for an orchestrator that only needs to
  know PRs exist. Nothing is watched: no per-PR polling, no badge. It gets
  `MATCHED new PR by alice, draft, not watched: call register to follow it
  (url)` for each new match, then `DRAFTED` / `READY_FOR_REVIEW` when the draft
  flag flips and `MERGED` / `CLOSED` when a tracked PR ends (one cheap state
  lookup, only for PRs that dropped out of the results; `events` can trim
  these). The agent calls `register` on the PRs it wants to follow, and from
  then on that watch reports, so nothing is pinged twice.
- **Cost:** one search per distinct query every two minutes (identical
  subscriptions share it), 1 GraphQL point per GitHub search, plus the usual
  polling of the registered watches.

What stays silent on purpose:

- **Partial approvals.** One approval of two required (GitLab approval rules,
  GitHub branch protection) is tracked and shown, and only the approval that
  completes the requirement pings.
- **Empty `COMMENTED` reviews** with no text and no new thread (bots submit
  these). Approvals and change requests always ping.
- **Your own activity.** Reviews and top-level comments by the worker's own
  account (your `gh`/`glab` identity, which an agent working for you shares)
  never ping you.
- **A new watch's existing activity.** The first poll of a new watch is only
  recorded as the baseline, because what a PR already has is visible on it.
  `register` takes `report_existing: true` to get it as a one-time ping.
  Auto-discovered watches always start silent.
- **Comments matching a comment filter** (see below).

Pings carry a one-line summary only, never the review text or the failing
log. The agent is expected to fetch the details, then analyse and suggest next
steps; it should not act on feedback automatically.

## Tools

| Tool             | Purpose                                                                                                         |
| ---------------- | --------------------------------------------------------------------------------------------------------------- |
| `register`       | Watch one or more PR/MR URLs for a session. Optional `events`, `persona` and `report_existing`. Idempotent.     |
| `unregister`     | Stop specific URLs, or everything a session owns when `pr_urls` is omitted. Rows move to `stopped`, not deleted. |
| `list`           | Registrations across all sessions, optionally filtered by `session_id` and `status`.                           |
| `dismiss_review` | Clear a GitHub review's badge: real dismissal, else re-request the reviewer, else hide locally.                |
| `subscribe`      | Watch a repo, org/group or platform with filters; every match becomes a watch and a `MATCHED` ping.            |
| `unsubscribe`    | Stop a subscription and the watches it registered.                                                              |
| `subscriptions`  | List subscriptions with their filters, watch counts and last scan.                                              |

Every `session_id` takes the bare session id or the raw `$AOE_ARTIFACT_DIR`
value. A registration belongs to the session that made it; several sessions
can watch the same PR/MR independently, and it still costs one provider check.

## How it works

One always-on worker process does four jobs:

1. **MCP server** (above) on loopback.
2. **Auto-discovery.** When a session goes idle (AoE's `session.status.changed`)
   the worker looks at that session's git checkouts: the `origin` remote and
   current branch, read locally with `git`. It asks the provider whether that
   branch has an open PR/MR and registers it. The same pass runs for every
   live session every 5 minutes as a fallback. Only PRs from the checkout's
   own repo are picked up, so a stranger's fork PR with the same branch name
   is not attached. Fork checkouts also look at the other remotes' repos
   (typically `upstream`), restricted to PRs from the checkout's own remotes.
   An explicit `unregister` is never undone by rediscovery.
3. **Poll scheduler.** Reads the rate-limit signal off every API response
   (GitHub GraphQL `rateLimit`, GitLab `RateLimit-*` headers) and spreads the
   remaining budget over the live watches, never planning to use more than
   90% of a pool. Polls are at least 15 seconds apart per watch, and 5
   minutes when a host reports no limit. GitHub PRs are checked in batched
   GraphQL queries of 20. Each poll is diffed against the last-seen state;
   only a real change produces an event.
4. **Notifier.** Sends pings with the host's `sessions.message.send`. The host
   wakes a stopped session and steers the message into a busy agent or queues
   it. Archived sessions are skipped but keep their watches; trashed or
   deleted sessions lose theirs.

A merged or closed PR/MR is checked every 15 minutes for 30 days after it
finished, for late reviews and comments (and a reopen), then left alone.

Registrations live in SQLite at `$XDG_STATE_HOME/pr-watch-mcp/registry.db`
(default `~/.local/state/pr-watch-mcp/registry.db`), so a restart loses
nothing. State is saved before the ping is sent, so a failed save repeats the
poll instead of pinging twice.

## Snoozing

Configure **Snooze rules** in the plugin's AoE settings (TUI or web). Each
rule has a list of URL patterns and a gate command. While the gate exits
non-zero, every watched PR/MR matching one of the rule's patterns is snoozed:
the worker skips the provider call entirely, so nothing is polled and nothing
is notified. When the gate exits 0 again, the next check reports everything
that happened in between in one go.

- **Patterns** match the PR/MR URL without scheme, case-insensitively. `*`
  matches any characters including `/`, `?` exactly one. For example
  `gitlab.example.com/*` or `github.com/my-org/*`.
- **Gate** runs through `sh -c`, once per scheduler tick per distinct command.
  Exit 0 is open, anything else closed. For example
  `systemctl --user is-active --quiet work.target`. A gate that cannot run at
  all (timeout, `sh` missing, command not found) counts as open and is
  logged, so a typo never silently mutes notifications.
- **Every matching rule applies.** A PR/MR is only checked while the gates of
  all rules matching it are open, so rules work as stages: one rule for
  `github.com/my-org/*` gated on work hours and one for `*` gated on "not in
  a break" keeps work PRs quiet outside work hours and during breaks, while
  other PRs are only held back during breaks. Order does not matter. A PR/MR
  matching no rule is never snoozed, and with no rules nothing is.
- Rules apply to every registration alike, explicit or discovered, and
  changes take effect without a restart. A snoozed watch re-checks its gate
  every minute.

## Comment filters

Some bots post top-level comments that are pure noise as a ping, such as
CodeRabbit's "review in progress" placeholder. Configure **Comment filters**
in the plugin's AoE settings to leave them out. A rule has an **author**
(login, case-insensitive, a trailing `[bot]` ignored), a list of **body
contains** texts (case-insensitive substrings), or both: then the author must
match and the body must contain at least one of the texts. A rule with
neither is ignored, so a half-filled rule can't mute everything. Rules apply
only to top-level comments (the Conversation tab), never to reviews or inline
threads.

- Rules are evaluated on the current body at every poll. A filtered comment is
  not stored as seen, so when its author edits it (CodeRabbit turns its
  placeholder into the real review) and it stops matching, it pings as new.
- Removing a rule has the same effect on whatever it was hiding.
- Changes take effect without a restart; with no rules nothing is filtered.

## Limits

- GitHub: 5000 GraphQL points per hour. The scheduler learns what a PR
  really costs from each response and paces itself accordingly. Queries are
  capped at 20 PRs, because GitHub rejects larger ones as too expensive.
- GitLab: one request per distinct MR per poll, a few more when it has
  approvals, discussions or a failing pipeline. An instance without the
  approvals endpoint just shows no approvals.
- A row badge has an 8KB payload cap on the host; past roughly 16 chips it
  switches to compact mode and past the cap it ends with a `+N` chip.
- Pane payloads are capped at 56KB here; long descriptions are trimmed, and
  the pane says so.
- Rate limits are per token, so several machines on the same token share the
  budget without knowing about each other.

## Security

- The MCP server listens on `127.0.0.1` only, and has **no authentication**:
  any local process can list, register and unregister watches, and
  `dismiss_review` acts with your GitHub token. Browsers are kept out: a
  non-loopback `Host` (DNS rebinding) and any cross-site `Origin` or
  `Sec-Fetch-Site` are rejected with 403.
- Credentials are never stored: tokens come from `gh`/`glab` at request time,
  are cached in memory, and are re-read only after a 401. A GitLab token is
  only sent to a host it is configured for.
- PR/MR text (titles, descriptions, comments) is untrusted. Pings carry
  author names and short summaries, descriptions are shown through the host's
  sanitising `markdown` pane block, and the worker never executes anything it
  reads from a PR. The one command it runs on your behalf is the snooze gate
  you configured.
- Auto-discovery runs `git` inside session checkouts to read the remote and
  branch; remote names that look like options are ignored.

## Logs

The worker logs to stderr, which AoE writes to one file per worker start under
`<aoe config dir>/plugin-workers/` (on Linux
`~/.config/agent-of-empires/plugin-workers/`). Newer AoE builds name the files
`<plugin-id>_<worker-uuid>.log`, keep the five newest per plugin, and
`aoe plugin logs io.cwrau.pr-watch-mcp` prints the live one's path, so
`tail -f "$(aoe plugin logs io.cwrau.pr-watch-mcp)"` follows it. v1.20.0 names
them `<worker-uuid>.log` and never prunes; find the live one there with
`rg -l 'pr_url' <dir> | xargs ls -t | head -1`.

The log records every event found (`event`), every ping sent (`ping sent`) or
held back (`ping suppressed`, with the reason `filter`, `self-authored` or
`baseline`), failed sends (`notify failed`), a warning when the worker cannot
resolve its own account, and the rate-limit state after each provider pass.

## Building and development

```bash
CGO_ENABLED=0 go build ./cmd/pr-watch-worker
go run -C magefiles mage.go -w .. check   # fmt, vet, lint, tests
```

No cgo: storage uses `modernc.org/sqlite`, so the result is a single static
binary.

## License

MIT, see [LICENSE](LICENSE).
