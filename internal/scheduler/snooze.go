package scheduler

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// SnoozeRecheckInterval is how long a snoozed registration is pushed out
// before its gate is looked at again. Evaluating a gate is a local command,
// not a rate-limited API call, so this can be short without costing budget;
// it only bounds how long after a gate opens polling resumes.
const SnoozeRecheckInterval = time.Minute

// gateTimeout bounds one gate command so a hung check can't stall a Tick.
const gateTimeout = 10 * time.Second

// SnoozeRule mutes every registration whose URL matches one of Match while
// Gate reports closed. Gate is a shell command: exit 0 means open (poll
// normally), any other exit means closed (snoozed).
type SnoozeRule struct {
	Match []string
	Gate  string
}

// GateFunc evaluates a gate command. A non-nil error means the gate could
// not be evaluated at all (as opposed to evaluating to closed).
type GateFunc func(ctx context.Context, gate string) (open bool, err error)

// ShellGate runs gate with `sh -c`. Exit 0 is open, any other ordinary exit
// is closed. A gate that cannot run at all (shell missing, timeout, or the
// shell's own 126/127 "cannot execute"/"not found") is an error rather than
// "closed", so a typo in a rule never silently mutes notifications.
func ShellGate(ctx context.Context, gate string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()

	err := exec.CommandContext(ctx, "sh", "-c", gate).Run()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := exitErr.ExitCode(); code == 126 || code == 127 {
			return false, err
		}
		return false, nil
	}
	return false, err
}

// isSnoozed reports whether reg is currently muted. Every rule with a
// matching pattern is a stage the registration has to pass: it is muted as
// soon as any of those gates is closed, so e.g. work PRs can be held to a
// work-hours gate while everything is held to a no-break gate. A registration
// no rule matches is never snoozed. A gate that errors fails open — an
// unevaluable gate must not hide events.
func (s *Scheduler) isSnoozed(ctx context.Context, reg registry.Registration) bool {
	if s.Snooze == nil {
		return false
	}
	for _, rule := range s.Snooze() {
		if rule.Gate == "" || !matchesSnooze(rule.Match, reg.PRURL) {
			continue
		}
		if !s.gateOpen(ctx, rule.Gate) {
			return true
		}
	}
	return false
}

// gateOpen evaluates gate at most once per Tick: many registrations commonly
// share one rule, and the answer cannot change within a single pass.
func (s *Scheduler) gateOpen(ctx context.Context, gate string) bool {
	if open, ok := s.gateMemo[gate]; ok {
		return open
	}
	run := s.RunGate
	if run == nil {
		run = ShellGate
	}
	open, err := run(ctx, gate)
	if err != nil {
		s.log().Warn("snooze gate could not be evaluated; treating as open", "gate", gate, "error", err)
		open = true
	}
	if s.gateMemo == nil {
		s.gateMemo = make(map[string]bool)
	}
	s.gateMemo[gate] = open
	return open
}

func matchesSnooze(patterns []string, url string) bool {
	target := normalizeForMatch(url)
	for _, p := range patterns {
		if globMatch(normalizeForMatch(p), target) {
			return true
		}
	}
	return false
}

func normalizeForMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimPrefix(s, "http://")
}

// globMatch matches s against pattern where `*` matches any run of
// characters (including `/`) and `?` matches exactly one.
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
