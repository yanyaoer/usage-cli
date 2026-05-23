package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeJSONL(t *testing.T, path string, lines ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, line := range lines {
		payload, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(payload)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadClaudeEntriesParsesAssistantUsage(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-tmp-demo", "session.jsonl")
	writeJSONL(t, path, map[string]any{
		"type":      "assistant",
		"timestamp": "2026-05-23T01:02:03Z",
		"sessionId": "s1",
		"requestId": "r1",
		"cwd":       "/tmp/demo",
		"costUSD":   0.42,
		"message": map[string]any{
			"id":    "m1",
			"model": "claude-sonnet-4-6-20260101",
			"usage": map[string]any{
				"input_tokens":                100,
				"output_tokens":               20,
				"cache_creation_input_tokens": 10,
				"cache_read_input_tokens":     5,
			},
		},
	})

	entries := LoadClaudeEntries(filepath.Join(home, ".claude", "projects"), time.Now())

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.AgentCategory != AgentClaude || entry.Project != "demo" {
		t.Fatalf("unexpected metadata: %#v", entry)
	}
	if entry.TotalTokens() != 135 || entry.CostUSD == nil || *entry.CostUSD != 0.42 {
		t.Fatalf("unexpected totals: %#v", entry)
	}
}

func TestLoadCodexEntriesUsesLastSessionUsageAndThreadModel(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "sessions", "s.jsonl")
	writeJSONL(t, path,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": "session-new", "timestamp": "2026-05-23T00:00:00Z", "cwd": "/tmp/work"}},
		map[string]any{"type": "event_msg", "timestamp": "2026-05-23T00:01:00Z", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{"input_tokens": 20, "cached_input_tokens": 5, "output_tokens": 7, "reasoning_output_tokens": 3}}}},
	)

	entry, ok := parseCodexSession(path, map[string]string{"session-new": "gpt-5-codex"})

	if !ok {
		t.Fatal("expected codex entry")
	}
	if entry.InputTokens != 15 || entry.OutputTokens != 10 || entry.CacheReadTokens != 5 {
		t.Fatalf("unexpected tokens: %#v", entry)
	}
	if entry.Model != "gpt-5-codex" || entry.AgentCategory != AgentCodex {
		t.Fatalf("unexpected metadata: %#v", entry)
	}
}

func TestLoadAssistantEntriesSupportsPiAndOMPGenericUsage(t *testing.T) {
	home := t.TempDir()
	piPath := filepath.Join(home, ".pi", "sessions", "pi.jsonl")
	ompPath := filepath.Join(home, ".omp", "history", "omp.jsonl")
	writeJSONL(t, piPath, map[string]any{
		"timestamp": "2026-05-23T00:00:00Z",
		"model":     "claude-sonnet-4-6",
		"usage":     map[string]any{"input_tokens": 10, "output_tokens": 2},
	})
	writeJSONL(t, ompPath, map[string]any{
		"created_at": "2026-05-23T00:00:00Z",
		"model":      "openai/gpt-5",
		"usage":      map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
		"cost_usd":   0.12,
	})

	piEntries := LoadAssistantEntries(filepath.Join(home, ".pi"), AgentPi, time.Now())
	ompEntries := LoadAssistantEntries(filepath.Join(home, ".omp"), AgentOMP, time.Now())

	if len(piEntries) != 1 || piEntries[0].AgentCategory != AgentPi || piEntries[0].TotalTokens() != 12 {
		t.Fatalf("unexpected pi entries: %#v", piEntries)
	}
	if len(ompEntries) != 1 || ompEntries[0].AgentCategory != AgentOMP || ompEntries[0].CostUSD == nil || *ompEntries[0].CostUSD != 0.12 {
		t.Fatalf("unexpected omp entries: %#v", ompEntries)
	}
}

func TestParseGenericLineCanOverrideAgentCategory(t *testing.T) {
	entry, ok := parseGenericLine(map[string]any{
		"timestamp":      "2026-05-23T00:00:00Z",
		"agent_category": "pi",
		"model":          "gpt-5",
		"usage":          map[string]any{"input_tokens": 1},
	}, AgentOMP)
	if !ok || entry.AgentCategory != AgentPi {
		t.Fatalf("expected pi override, entry=%#v ok=%v", entry, ok)
	}
}

func TestAggregateGroupsByModelAndAgentCategory(t *testing.T) {
	pricing := Pricing{"gpt-5": {Input: 1, Output: 2, CacheRead: 0.5}}
	entries := []UsageEntry{
		{Timestamp: time.Now(), Model: "openai/gpt-5", AgentCategory: AgentCodex, InputTokens: 1, OutputTokens: 2, CacheReadTokens: 4},
		{Timestamp: time.Now(), Model: "openai/gpt-5", AgentCategory: AgentOMP, InputTokens: 3},
	}

	total, rows := Aggregate(entries, pricing)

	if total.Tokens != 10 || total.Cost != 10 {
		t.Fatalf("unexpected total: %#v", total)
	}
	if len(rows) != 2 {
		t.Fatalf("expected separate agent_category groups, got %#v", rows)
	}
}

func TestResolveModelKeyNormalizesProviderAndDateSuffix(t *testing.T) {
	pricing := Pricing{"claude-sonnet-4-6": {Input: 1}}
	key, ok := ResolveModelKey("anthropic/claude-sonnet-4-6-20260101", pricing)

	if !ok || key != "claude-sonnet-4-6" {
		t.Fatalf("unexpected key %q ok=%v", key, ok)
	}
}

func TestResolveModelKeyFallsBackToSlashSuffix(t *testing.T) {
	pricing := Pricing{"gpt-5.5": {Input: 1}, "azure/gpt-5.3-codex": {Input: 2}}

	key, ok := ResolveModelKey("openai/gpt-5.5", pricing)
	if !ok || key != "gpt-5.5" {
		t.Fatalf("unexpected openai suffix key %q ok=%v", key, ok)
	}

	key, ok = ResolveModelKey("azure_openai/gpt-5.3-codex", pricing)
	if !ok || key != "azure/gpt-5.3-codex" {
		t.Fatalf("unexpected azure suffix key %q ok=%v", key, ok)
	}
}

func TestResolveModelKeyFuzzyMatchesProviderVersions(t *testing.T) {
	pricing := Pricing{
		"anthropic/claude-opus-4": {Input: 1},
		"openai/gpt-5":            {Input: 2},
	}

	key, ok := ResolveModelKey("anthropic/claude-opus-4.7", pricing)
	if !ok || key != "anthropic/claude-opus-4" {
		t.Fatalf("unexpected claude fuzzy key %q ok=%v", key, ok)
	}

	key, ok = ResolveModelKey("openai/gpt-5.5", pricing)
	if !ok || key != "openai/gpt-5" {
		t.Fatalf("unexpected gpt fuzzy key %q ok=%v", key, ok)
	}
}

func TestResolveModelKeyFuzzyStripsDeploymentSuffixes(t *testing.T) {
	pricing := Pricing{"claude-opus-4-7": {Input: 1}, "anthropic/claude-opus-4": {Input: 2}}

	key, ok := ResolveModelKey("claude-opus-4-7-005-sgp-no-co", pricing)
	if !ok || key != "claude-opus-4-7" {
		t.Fatalf("unexpected deployment key %q ok=%v", key, ok)
	}

	key, ok = ResolveModelKey("claude-opus-4-7-005-sgp-no-co", Pricing{"anthropic/claude-opus-4": {Input: 2}})
	if !ok || key != "anthropic/claude-opus-4" {
		t.Fatalf("unexpected fallback deployment key %q ok=%v", key, ok)
	}
}

func TestResolveModelKeyUsesKnownAliases(t *testing.T) {
	pricing := Pricing{
		"gpt-5.5-pro":                     {Input: 1},
		"gpt-5.3-codex":                   {Input: 1},
		"claude-haiku-4-5-20251001":       {Input: 1},
		"openrouter/xiaomi/mimo-v2.5-pro": {Input: 1},
		"deepseek/deepseek-v3.2":          {Input: 1},
	}
	cases := map[string]string{
		"openai/gpt-5.5-pro":         "gpt-5.5-pro",
		"openai/gpt-5.3-codex":       "gpt-5.3-codex",
		"azure_openai/gpt-5.3-codex": "gpt-5.3-codex",
		"claude-haiku-4-5":           "claude-haiku-4-5-20251001",
		"xiaomi/mimo-v2.5-pro":       "openrouter/xiaomi/mimo-v2.5-pro",
		"mimo-v2.5-pro":              "openrouter/xiaomi/mimo-v2.5-pro",
		"deepseek/deepseek-v4-pro":   "deepseek/deepseek-v3.2",
	}
	for model, expected := range cases {
		key, ok := ResolveModelKey(model, pricing)
		if !ok || key != expected {
			t.Fatalf("%s resolved to %q ok=%v, want %q", model, key, ok, expected)
		}
	}
}

func TestFetchLiteLLMPricingNormalizesPricing(t *testing.T) {
	client := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != liteLLMPricingURL {
			t.Fatalf("unexpected url: %s", req.URL.String())
		}
		body := `{"openai/gpt-5":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002,"cache_read_input_token_cost":0.0000001}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})

	pricing := fetchLiteLLMPricing(&http.Client{Transport: client})

	price, ok := pricing["openai/gpt-5"]
	if !ok || price.Input != 0.000001 || price.Output != 0.000002 || price.CacheRead != 0.0000001 {
		t.Fatalf("unexpected pricing: %#v", pricing)
	}
}

func TestLoadLiteLLMPricingUsesFreshCache(t *testing.T) {
	home := t.TempDir()
	cachePath := filepath.Join(home, ".cache", "llm-usage", "litellm_pricing_cache.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(`{"cached-model":{"input_cost_per_token":0.5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	client := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("fresh cache should avoid network fetch")
		return nil, nil
	})

	pricing := LoadLiteLLMPricing(home, &http.Client{Transport: client})

	if pricing["cached-model"].Input != 0.5 {
		t.Fatalf("unexpected cached pricing: %#v", pricing)
	}
}

func TestLoadCachedEntriesWritesAndReusesCache(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "logs")
	path := filepath.Join(root, "session.jsonl")
	writeJSONL(t, path, map[string]any{"timestamp": "2026-05-23T00:00:00Z", "model": "gpt-5", "usage": map[string]any{"input_tokens": 1}})
	calls := 0
	loader := func(path string, root string, now time.Time) []UsageEntry {
		calls++
		return parseGenericJSONL(path, AgentOMP)
	}

	first := LoadCachedEntries(root, AgentOMP, time.Now(), home, loader)
	second := LoadCachedEntries(root, AgentOMP, time.Now(), home, loader)

	if calls != 1 {
		t.Fatalf("expected one loader call, got %d", calls)
	}
	if len(first) != 1 || len(second) != 1 || second[0].InputTokens != 1 || second[0].SourceFile != path {
		t.Fatalf("unexpected cached entries first=%#v second=%#v", first, second)
	}
	if _, err := os.Stat(filepath.Join(home, ".cache", "llm-usage", "omp.json")); err != nil {
		t.Fatalf("expected cache file: %v", err)
	}
}

func TestLoadCachedEntriesInvalidatesChangedFile(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "logs")
	path := filepath.Join(root, "session.jsonl")
	writeJSONL(t, path, map[string]any{"timestamp": "2026-05-23T00:00:00Z", "model": "gpt-5", "usage": map[string]any{"input_tokens": 1}})
	LoadCachedEntries(root, AgentOMP, time.Now(), home, func(path string, root string, now time.Time) []UsageEntry {
		return parseGenericJSONL(path, AgentOMP)
	})

	writeJSONL(t, path, map[string]any{"timestamp": "2026-05-23T00:00:00Z", "model": "gpt-5", "usage": map[string]any{"input_tokens": 3}})
	entries := LoadCachedEntries(root, AgentOMP, time.Now(), home, func(path string, root string, now time.Time) []UsageEntry {
		return parseGenericJSONL(path, AgentOMP)
	})

	if len(entries) != 1 || entries[0].InputTokens != 3 {
		t.Fatalf("expected changed cache entry, got %#v", entries)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
