# io.cwrau.pr-watch

Persistent PR/MR watch service for Agent of Empires: an AoE Tier-1 plugin
worker that polls registered GitHub PRs and GitLab MRs, paces itself
against live rate-limit budgets, and wakes the owning AoE session directly
on review/CI/merge activity.

Design: `docs/superpowers/specs/2026-09-20-pr-watch-mcp-design.md`
Implementation plan: `docs/superpowers/plans/2026-09-20-pr-watch-mcp.md`
