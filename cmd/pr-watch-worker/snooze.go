package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/scheduler"
)

const (
	settingsChangedMethod = "plugin.settings.changed"
	snoozeSettingKey      = "snooze"

	// configGetTimeout bounds one config.get round trip so a host that never
	// replies can't wedge the caller.
	configGetTimeout = 10 * time.Second

	// snoozeRefreshEvery is the fallback re-read cadence for a worker that
	// missed a plugin.settings.changed notification.
	snoozeRefreshEvery = 5 * time.Minute
)

// snoozeRules caches the snooze setting read from the host. The scheduler
// reads it once per registration per tick, so rules() must never block on
// the host; refreshes happen out of band and swap the cached copy.
type snoozeRules struct {
	mu    sync.RWMutex
	rules []scheduler.SnoozeRule
}

func (s *snoozeRules) get() []scheduler.SnoozeRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rules
}

func (s *snoozeRules) set(rules []scheduler.SnoozeRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = rules
}

// refresh re-reads the snooze setting. On any failure the previous rules
// stay in force: a transient host hiccup must not silently drop the user's
// snooze configuration (or, worse, un-snooze everything).
func (s *snoozeRules) refresh(ctx context.Context, rpc *hostRPC, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, configGetTimeout)
	defer cancel()

	raw, err := rpc.call(ctx, "config.get", map[string]any{"key": snoozeSettingKey})
	if err != nil {
		logger.Warn("read snooze setting; keeping previous rules", "error", err)
		return
	}
	rules, err := parseSnoozeRules(raw)
	if err != nil {
		logger.Warn("parse snooze setting; keeping previous rules", "error", err)
		return
	}
	s.set(rules)
	logger.Info("snooze rules loaded", "rules", len(rules))
}

// parseSnoozeRules decodes a config.get reply for the snooze object_list:
// {"value": [{"_id": "...", "match": ["pat", ...], "exclude": ["pat", ...],
// "gate": "cmd"}, ...]}. A null/absent value is simply no rules. Items
// missing a gate or any match pattern are dropped rather than failing the
// whole list; exclude is optional.
func parseSnoozeRules(raw json.RawMessage) ([]scheduler.SnoozeRule, error) {
	var reply struct {
		Value []struct {
			Match   []string `json:"match"`
			Exclude []string `json:"exclude"`
			Gate    string   `json:"gate"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode config.get reply: %w", err)
	}
	rules := make([]scheduler.SnoozeRule, 0, len(reply.Value))
	for _, item := range reply.Value {
		gate := strings.TrimSpace(item.Gate)
		patterns := trimmedPatterns(item.Match)
		if gate == "" || len(patterns) == 0 {
			continue
		}
		rules = append(rules, scheduler.SnoozeRule{Match: patterns, Exclude: trimmedPatterns(item.Exclude), Gate: gate})
	}
	return rules, nil
}

// trimmedPatterns trims each pattern and drops the blank ones.
func trimmedPatterns(raw []string) []string {
	var patterns []string
	for _, p := range raw {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}
