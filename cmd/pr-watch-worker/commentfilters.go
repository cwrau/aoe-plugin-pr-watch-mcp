package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
)

const commentFiltersSettingKey = "comment_filters"

// commentFilters caches the comment_filters setting read from the host. The
// scheduler reads it once per registration check, so get() must never block
// on the host; refreshes happen out of band and swap the cached copy, same
// shape as snoozeRules.
type commentFilters struct {
	mu    sync.RWMutex
	rules []provider.CommentFilter
}

func (c *commentFilters) get() []provider.CommentFilter {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rules
}

func (c *commentFilters) set(rules []provider.CommentFilter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rules = rules
}

// refresh re-reads the setting. On any failure the previous rules stay in
// force: a transient host hiccup must not silently drop the user's filters.
func (c *commentFilters) refresh(ctx context.Context, rpc *hostRPC, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, configGetTimeout)
	defer cancel()

	raw, err := rpc.call(ctx, "config.get", map[string]any{"key": commentFiltersSettingKey})
	if err != nil {
		logger.Warn("read comment_filters setting; keeping previous rules", "error", err)
		return
	}
	rules, err := parseCommentFilters(raw)
	if err != nil {
		logger.Warn("parse comment_filters setting; keeping previous rules", "error", err)
		return
	}
	c.set(rules)
	logger.Info("comment filters loaded", "rules", len(rules))
}

// parseCommentFilters decodes a config.get reply for the comment_filters
// object_list: {"value": [{"_id": "...", "author": "...", "contains": ["...", ...]}]}.
// A null/absent value is simply no rules. Blank texts are dropped, and a row
// with neither an author nor a text is dropped rather than kept as a rule
// that could never match. contains used to be a single string setting and
// the host keeps a stored value as-is until the row is next edited, so a
// plain string is accepted as a one-entry list.
func parseCommentFilters(raw json.RawMessage) ([]provider.CommentFilter, error) {
	var reply struct {
		Value []struct {
			Author   string          `json:"author"`
			Contains json.RawMessage `json:"contains"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode config.get reply: %w", err)
	}
	rules := make([]provider.CommentFilter, 0, len(reply.Value))
	for _, item := range reply.Value {
		texts, err := decodeStringOrList(item.Contains)
		if err != nil {
			return nil, fmt.Errorf("decode contains: %w", err)
		}
		var contains []string
		for _, t := range texts {
			if t = strings.TrimSpace(t); t != "" {
				contains = append(contains, t)
			}
		}
		author := strings.TrimSpace(item.Author)
		if author == "" && len(contains) == 0 {
			continue
		}
		rules = append(rules, provider.CommentFilter{Author: author, Contains: contains})
	}
	return rules, nil
}

// decodeStringOrList reads a JSON string as a one-entry list, a JSON array
// of strings as itself, and absent/null as nothing.
func decodeStringOrList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}
