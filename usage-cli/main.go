package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type AgentCategory string

const (
	AgentClaude AgentCategory = "claude"
	AgentCodex  AgentCategory = "codex"
	AgentPi     AgentCategory = "pi"
	AgentOMP    AgentCategory = "omp"
)

type Window string

const (
	WindowDay   Window = "day"
	WindowWeek  Window = "week"
	WindowMonth Window = "month"
)

type TrendRange string

const (
	TrendRangeWeek  TrendRange = "week"
	TrendRangeMonth TrendRange = "month"
)

type UsageEntry struct {
	Timestamp           time.Time
	SessionID           string
	MessageID           string
	RequestID           string
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	CostUSD             *float64
	Project             string
	AgentCategory       AgentCategory
	SourceFile          string
}

func (e UsageEntry) TotalTokens() int64 {
	return e.InputTokens + e.OutputTokens + e.CacheCreationTokens + e.CacheReadTokens
}

type Pricing map[string]ModelPrice

type ModelPrice struct {
	Input         float64
	Output        float64
	CacheCreation float64
	CacheRead     float64
}

type Totals struct {
	Entries               int64
	Tokens                int64
	Cost                  float64
	InputTokens           int64
	OutputTokens          int64
	CacheCreationTokens   int64
	CacheReadTokens       int64
	PromptCacheHitTokens  int64
	PromptCacheMissTokens int64
}

type TrendOptions struct {
	Enabled bool
	From    time.Time
	To      time.Time
}

type DailyTrendRow struct {
	Date   time.Time
	Totals Totals
}

type GroupKey struct {
	Model         string
	AgentCategory AgentCategory
}

type ProjectKey struct {
	Project       string
	AgentCategory AgentCategory
}

type Options struct {
	Home string
	Now  time.Time
}

type CacheStats struct {
	FilesTotal int
	Hits       int
	Misses     int
	Stale      int
	Removed    int
}

func (stats *CacheStats) Add(other CacheStats) {
	stats.FilesTotal += other.FilesTotal
	stats.Hits += other.Hits
	stats.Misses += other.Misses
	stats.Stale += other.Stale
	stats.Removed += other.Removed
}

type LoadResult struct {
	Entries []UsageEntry
	Cache   CacheStats
}

var dateSuffixes = []string{"-20060102", "-2006-01-02"}

const liteLLMPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
const pricingCacheTTL = 7 * 24 * time.Hour

func main() {
	windowFlag := flag.String("view", "day", "view: day, week, month, all, trend")
	rangeFlag := flag.String("range", "week", "trend range: week, month")
	fromFlag := flag.String("from", "", "trend start date, YYYY-MM-DD")
	toFlag := flag.String("to", "", "trend end date, YYYY-MM-DD")
	homeFlag := flag.String("home", "", "home directory override for tests")
	flag.Parse()

	now := time.Now()
	home := *homeFlag
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "usage-go: cannot resolve home: %v\n", err)
			os.Exit(1)
		}
	}

	result := LoadAllEntries(Options{Home: home, Now: now})
	pricing := LoadPricing(home)

	views := []Window{WindowDay, WindowWeek, WindowMonth}
	var trend TrendOptions
	if *windowFlag == "trend" {
		var err error
		trend, err = trendOptionsFor(*rangeFlag, *fromFlag, *toFlag, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "usage-go: %v\n", err)
			os.Exit(2)
		}
		views = nil
	} else if *windowFlag != "all" {
		view := Window(*windowFlag)
		if view != WindowDay && view != WindowWeek && view != WindowMonth {
			fmt.Fprintf(os.Stderr, "usage-go: unsupported view %q\n", *windowFlag)
			os.Exit(2)
		}
		views = []Window{view}
	}

	Render(result.Entries, pricing, views, now, result.Cache, trend)
}

func LoadAllEntries(opts Options) LoadResult {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	var result LoadResult
	appendResult := func(next LoadResult) {
		result.Entries = append(result.Entries, next.Entries...)
		result.Cache.Add(next.Cache)
	}
	appendResult(LoadCachedEntries(filepath.Join(opts.Home, ".claude", "projects"), AgentClaude, opts.Now, opts.Home, func(path string, root string, now time.Time) []UsageEntry { return LoadClaudeFile(path, root) }))
	appendResult(LoadCachedEntries(filepath.Join(opts.Home, ".codex"), AgentCodex, opts.Now, opts.Home, func(path string, root string, now time.Time) []UsageEntry { return LoadCodexFile(path, root) }))
	appendResult(LoadCachedEntries(filepath.Join(opts.Home, ".pi"), AgentPi, opts.Now, opts.Home, func(path string, root string, now time.Time) []UsageEntry { return parseGenericJSONL(path, AgentPi) }))
	appendResult(LoadCachedEntries(filepath.Join(opts.Home, ".omp"), AgentOMP, opts.Now, opts.Home, func(path string, root string, now time.Time) []UsageEntry { return parseGenericJSONL(path, AgentOMP) }))
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Timestamp.Before(result.Entries[j].Timestamp) })
	result.Entries = dedupe(result.Entries)
	return result
}

func Render(entries []UsageEntry, pricing Pricing, views []Window, now time.Time, cacheStats CacheStats, trend TrendOptions) {
	fmt.Println("usage-go cost summary")
	fmt.Printf("Entry cache: %d files, %d hits, %d misses, %d stale, %d removed\n", cacheStats.FilesTotal, cacheStats.Hits, cacheStats.Misses, cacheStats.Stale, cacheStats.Removed)
	for _, view := range views {
		cutoff := cutoffFor(view, now)
		filtered := filterSince(entries, cutoff)
		total, modelGroups, projectGroups := Aggregate(filtered, pricing)
		fmt.Printf("\n%s (%s)\n", strings.ToUpper(string(view)), cutoff.Format("2006-01-02 15:04"))
		fmt.Printf("Total: %s tokens  $%.4f  %d entries\n", formatInt(total.Tokens), total.Cost, total.Entries)
		fmt.Printf("Prompt cache: hit %s tokens, miss %s tokens\n", formatInt(total.PromptCacheHitTokens), formatInt(total.PromptCacheMissTokens))
		fmt.Println("Model                         Agent   Tokens        Cost      Entries")
		fmt.Println("----------------------------- ------- ------------- --------- -------")
		for _, row := range modelGroups {
			fmt.Printf("%-29.29s %-7s %13s $%8.4f %7d\n",
				row.Key.Model,
				row.Key.AgentCategory,
				formatInt(row.Totals.Tokens),
				row.Totals.Cost,
				row.Totals.Entries,
			)
		}
		if len(modelGroups) == 0 {
			fmt.Println("(no usage found)")
		}
		fmt.Println("")
		fmt.Println("Project                       Agent   Cache Hit  Tokens        Cost      Entries")
		fmt.Println("----------------------------- ------- ---------- ------------- --------- -------")
		for _, row := range projectGroups {
			fmt.Printf("%-29.29s %-7s %9s %13s $%8.4f %7d\n",
				row.Key.Project,
				row.Key.AgentCategory,
				formatPromptCacheRate(row.Totals),
				formatInt(row.Totals.Tokens),
				row.Totals.Cost,
				row.Totals.Entries,
			)
		}
		if len(projectGroups) == 0 {
			fmt.Println("(no usage found)")
		}
	}
	if trend.Enabled {
		RenderDailyTrend(entries, pricing, trend)
	}
}

func trendOptionsFor(rangeValue, fromValue, toValue string, now time.Time) (TrendOptions, error) {
	if now.IsZero() {
		now = time.Now()
	}
	loc := now.Location()
	if loc == nil {
		loc = time.Local
	}
	end := dateStart(now.In(loc))
	switch TrendRange(rangeValue) {
	case TrendRangeWeek:
		// Include today plus the previous six local dates.
		return trendOptionsFromDates(fromValue, toValue, end.AddDate(0, 0, -6), end, loc)
	case TrendRangeMonth:
		// Match the existing month window closely: 30 local dates including today.
		return trendOptionsFromDates(fromValue, toValue, end.AddDate(0, 0, -29), end, loc)
	default:
		return TrendOptions{}, fmt.Errorf("unsupported trend range %q", rangeValue)
	}
}

func trendOptionsFromDates(fromValue, toValue string, defaultFrom, defaultTo time.Time, loc *time.Location) (TrendOptions, error) {
	from := defaultFrom
	to := defaultTo
	var err error
	if toValue != "" {
		to, err = parseDateInLocation(toValue, loc)
		if err != nil {
			return TrendOptions{}, fmt.Errorf("invalid --to date %q, expected YYYY-MM-DD", toValue)
		}
		if fromValue == "" {
			days := int(defaultTo.Sub(defaultFrom).Hours() / 24)
			from = to.AddDate(0, 0, -days)
		}
	}
	if fromValue != "" {
		from, err = parseDateInLocation(fromValue, loc)
		if err != nil {
			return TrendOptions{}, fmt.Errorf("invalid --from date %q, expected YYYY-MM-DD", fromValue)
		}
	}
	if from.After(to) {
		return TrendOptions{}, fmt.Errorf("--from must be on or before --to")
	}
	return TrendOptions{Enabled: true, From: from, To: to}, nil
}

func parseDateInLocation(value string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil {
		return time.Time{}, err
	}
	return dateStart(t), nil
}

func AggregateDailyTrend(entries []UsageEntry, pricing Pricing, opts TrendOptions) []DailyTrendRow {
	if opts.From.IsZero() || opts.To.IsZero() || opts.From.After(opts.To) {
		return nil
	}
	loc := opts.From.Location()
	if loc == nil {
		loc = time.Local
	}
	from := dateStart(opts.From.In(loc))
	to := dateStart(opts.To.In(loc))
	days := int(to.Sub(from).Hours()/24) + 1
	rows := make([]DailyTrendRow, 0, days)
	indexByDate := make(map[string]int, days)
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		indexByDate[day.Format("2006-01-02")] = len(rows)
		rows = append(rows, DailyTrendRow{Date: day})
	}
	for _, entry := range entries {
		day := dateStart(entry.Timestamp.In(loc))
		idx, ok := indexByDate[day.Format("2006-01-02")]
		if !ok {
			continue
		}
		tokens := entry.TotalTokens()
		cost := CalculateCost(entry, pricing)
		addTotals(&rows[idx].Totals, entry, tokens, cost)
	}
	return rows
}

func RenderDailyTrend(entries []UsageEntry, pricing Pricing, opts TrendOptions) {
	rows := AggregateDailyTrend(entries, pricing, opts)
	var total Totals
	var maxTokens int64
	var maxCost float64
	for _, row := range rows {
		total.Entries += row.Totals.Entries
		total.Tokens += row.Totals.Tokens
		total.Cost += row.Totals.Cost
		total.InputTokens += row.Totals.InputTokens
		total.OutputTokens += row.Totals.OutputTokens
		total.CacheCreationTokens += row.Totals.CacheCreationTokens
		total.CacheReadTokens += row.Totals.CacheReadTokens
		total.PromptCacheHitTokens += row.Totals.PromptCacheHitTokens
		total.PromptCacheMissTokens += row.Totals.PromptCacheMissTokens
		if row.Totals.Tokens > maxTokens {
			maxTokens = row.Totals.Tokens
		}
		if row.Totals.Cost > maxCost {
			maxCost = row.Totals.Cost
		}
	}
	fmt.Printf("\nTREND (%s to %s)\n", opts.From.Format("2006-01-02"), opts.To.Format("2006-01-02"))
	fmt.Printf("Total: %s tokens  $%.4f  %d entries\n", formatInt(total.Tokens), total.Cost, total.Entries)
	fmt.Println("Date         Tokens        Cost      Entries  Token Trend          Cost Trend")
	fmt.Println("----------  ------------- --------- -------  -------------------- --------------------")
	for _, row := range rows {
		fmt.Printf("%s  %13s $%8.4f %7d  %-20s %-20s\n",
			row.Date.Format("2006-01-02"),
			formatInt(row.Totals.Tokens),
			row.Totals.Cost,
			row.Totals.Entries,
			formatIntBar(row.Totals.Tokens, maxTokens, 20),
			formatFloatBar(row.Totals.Cost, maxCost, 20),
		)
	}
	if len(rows) == 0 {
		fmt.Println("(no usage found)")
	}
}

func dateStart(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

func cutoffFor(view Window, now time.Time) time.Time {
	switch view {
	case WindowDay:
		return now.Add(-24 * time.Hour)
	case WindowWeek:
		return now.Add(-7 * 24 * time.Hour)
	case WindowMonth:
		return now.Add(-30 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}

func filterSince(entries []UsageEntry, cutoff time.Time) []UsageEntry {
	out := make([]UsageEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.Timestamp.Before(cutoff) {
			out = append(out, entry)
		}
	}
	return out
}

type GroupRow struct {
	Key    GroupKey
	Totals Totals
}

type ProjectGroupRow struct {
	Key    ProjectKey
	Totals Totals
}

func Aggregate(entries []UsageEntry, pricing Pricing) (Totals, []GroupRow, []ProjectGroupRow) {
	byModel := make(map[GroupKey]Totals)
	byProject := make(map[ProjectKey]Totals)
	var total Totals
	for _, entry := range entries {
		cost := CalculateCost(entry, pricing)
		tokens := entry.TotalTokens()
		total.Entries++
		total.Tokens += tokens
		total.Cost += cost
		total.InputTokens += entry.InputTokens
		total.OutputTokens += entry.OutputTokens
		total.CacheCreationTokens += entry.CacheCreationTokens
		total.CacheReadTokens += entry.CacheReadTokens
		total.PromptCacheHitTokens += entry.CacheReadTokens
		total.PromptCacheMissTokens += entry.InputTokens + entry.CacheCreationTokens

		modelKey := GroupKey{Model: normalizeUnknown(entry.Model), AgentCategory: entry.AgentCategory}
		modelBucket := byModel[modelKey]
		addTotals(&modelBucket, entry, tokens, cost)
		byModel[modelKey] = modelBucket

		projectKey := ProjectKey{Project: normalizeUnknown(entry.Project), AgentCategory: entry.AgentCategory}
		projectBucket := byProject[projectKey]
		addTotals(&projectBucket, entry, tokens, cost)
		byProject[projectKey] = projectBucket
	}

	modelRows := make([]GroupRow, 0, len(byModel))
	for key, totals := range byModel {
		modelRows = append(modelRows, GroupRow{Key: key, Totals: totals})
	}
	sortGroupRows(modelRows)

	projectRows := make([]ProjectGroupRow, 0, len(byProject))
	for key, totals := range byProject {
		projectRows = append(projectRows, ProjectGroupRow{Key: key, Totals: totals})
	}
	sortProjectRows(projectRows)
	return total, modelRows, projectRows
}

func addTotals(bucket *Totals, entry UsageEntry, tokens int64, cost float64) {
	bucket.Entries++
	bucket.Tokens += tokens
	bucket.Cost += cost
	bucket.InputTokens += entry.InputTokens
	bucket.OutputTokens += entry.OutputTokens
	bucket.CacheCreationTokens += entry.CacheCreationTokens
	bucket.CacheReadTokens += entry.CacheReadTokens
	bucket.PromptCacheHitTokens += entry.CacheReadTokens
	bucket.PromptCacheMissTokens += entry.InputTokens + entry.CacheCreationTokens
}

func sortGroupRows(rows []GroupRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Totals.Cost != rows[j].Totals.Cost {
			return rows[i].Totals.Cost > rows[j].Totals.Cost
		}
		if rows[i].Totals.Tokens != rows[j].Totals.Tokens {
			return rows[i].Totals.Tokens > rows[j].Totals.Tokens
		}
		return fmt.Sprint(rows[i].Key) < fmt.Sprint(rows[j].Key)
	})
}

func sortProjectRows(rows []ProjectGroupRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Totals.Cost != rows[j].Totals.Cost {
			return rows[i].Totals.Cost > rows[j].Totals.Cost
		}
		if rows[i].Totals.Tokens != rows[j].Totals.Tokens {
			return rows[i].Totals.Tokens > rows[j].Totals.Tokens
		}
		return fmt.Sprint(rows[i].Key) < fmt.Sprint(rows[j].Key)
	})
}

func CalculateCost(entry UsageEntry, pricing Pricing) float64 {
	if entry.CostUSD != nil {
		return *entry.CostUSD
	}
	key, ok := ResolveModelKey(entry.Model, pricing)
	if !ok {
		return 0
	}
	price := pricing[key]
	cacheCreation := price.CacheCreation
	if cacheCreation == 0 {
		cacheCreation = price.Input * 1.25
	}
	cacheRead := price.CacheRead
	if cacheRead == 0 {
		cacheRead = price.Input * 0.1
	}
	return float64(entry.InputTokens)*price.Input + float64(entry.OutputTokens)*price.Output + float64(entry.CacheCreationTokens)*cacheCreation + float64(entry.CacheReadTokens)*cacheRead
}

func LoadPricing(home string) Pricing {
	pricing := fallbackPricing()
	mergePricing(pricing, LoadLiteLLMPricing(home, http.DefaultClient))
	return pricing
}

func LoadLiteLLMPricing(home string, client *http.Client) Pricing {
	cachePath := filepath.Join(home, ".cache", "llm-usage", "litellm_pricing_cache.json")
	if pricing := readFreshPricingCache(cachePath, time.Now()); len(pricing) > 0 {
		return pricing
	}
	legacy := readPricingCache(filepath.Join(home, ".claude", "pricing_cache.json"))
	pricing := fetchLiteLLMPricing(client)
	if len(pricing) == 0 {
		return legacy
	}
	writePricingCache(cachePath, pricing)
	return pricing
}

func mergePricing(dst Pricing, src Pricing) {
	for model, price := range src {
		dst[model] = price
	}
}

func readFreshPricingCache(cachePath string, now time.Time) Pricing {
	info, err := os.Stat(cachePath)
	if err != nil || now.Sub(info.ModTime()) > pricingCacheTTL {
		return nil
	}
	return readPricingCache(cachePath)
}

func readPricingCache(cachePath string) Pricing {
	file, err := os.Open(cachePath)
	if err != nil {
		return nil
	}
	defer file.Close()
	var raw map[string]map[string]any
	if json.NewDecoder(file).Decode(&raw) != nil {
		return nil
	}
	return normalizePricing(raw)
}

func writePricingCache(cachePath string, pricing Pricing) {
	if os.MkdirAll(filepath.Dir(cachePath), 0o755) != nil {
		return
	}
	raw := make(map[string]map[string]float64, len(pricing))
	for model, price := range pricing {
		raw[model] = map[string]float64{
			"input_cost_per_token":            price.Input,
			"output_cost_per_token":           price.Output,
			"cache_creation_input_token_cost": price.CacheCreation,
			"cache_read_input_token_cost":     price.CacheRead,
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), "litellm_pricing_*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if json.NewEncoder(tmp).Encode(raw) != nil || tmp.Close() != nil {
		return
	}
	if os.Rename(tmpPath, cachePath) != nil {
		return
	}
	ok = true
}

func fetchLiteLLMPricing(client *http.Client) Pricing {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequest(http.MethodGet, liteLLMPricingURL, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("User-Agent", "usage-cli/0.1")
	response, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	var raw map[string]map[string]any
	if json.NewDecoder(response.Body).Decode(&raw) != nil {
		return nil
	}
	return normalizePricing(raw)
}

func normalizePricing(raw map[string]map[string]any) Pricing {
	pricing := make(Pricing, len(raw))
	for model, info := range raw {
		price := ModelPrice{
			Input:         asFloat(info["input_cost_per_token"]),
			Output:        asFloat(info["output_cost_per_token"]),
			CacheCreation: asFloat(info["cache_creation_input_token_cost"]),
			CacheRead:     asFloat(info["cache_read_input_token_cost"]),
		}
		if price.Input != 0 || price.Output != 0 || price.CacheCreation != 0 || price.CacheRead != 0 {
			pricing[model] = price
		}
	}
	return pricing
}

func fallbackPricing() Pricing {
	return Pricing{
		"claude-opus-4-6":   {Input: 5e-6, Output: 25e-6, CacheCreation: 6.25e-6, CacheRead: 0.5e-6},
		"claude-opus-4-7":   {Input: 5e-6, Output: 25e-6, CacheCreation: 6.25e-6, CacheRead: 0.5e-6},
		"claude-sonnet-4-6": {Input: 3e-6, Output: 15e-6, CacheCreation: 3.75e-6, CacheRead: 0.3e-6},
		"gpt-5":             {Input: 1.25e-6, Output: 10e-6, CacheRead: 0.125e-6},
		"gpt-5-codex":       {Input: 1.25e-6, Output: 10e-6, CacheRead: 0.125e-6},
	}
}

func ResolveModelKey(model string, pricing Pricing) (string, bool) {
	for _, candidate := range modelCandidates(model) {
		if _, ok := pricing[candidate]; ok {
			return candidate, true
		}
	}

	best := ""
	for _, candidate := range modelCandidates(model) {
		for key := range pricing {
			if modelKeyMatches(key, candidate) && betterDirectModelMatch(best, key) {
				best = key
			}
		}
	}
	if best != "" {
		return best, true
	}
	for _, alias := range modelAliases(model) {
		if _, ok := pricing[alias]; ok {
			return alias, true
		}
		for key := range pricing {
			if modelKeyMatches(key, alias) && betterDirectModelMatch(best, key) {
				best = key
			}
		}
	}
	if best != "" {
		return best, true
	}
	for _, candidate := range modelCandidates(model) {
		candidateBase := fuzzyModelBase(candidate)
		for key := range pricing {
			keyCandidate := NormalizeModelName(key)
			keyBase := fuzzyModelBase(keyCandidate)
			if candidateBase != "" && candidateBase == keyBase && betterModelMatch(best, key) {
				best = key
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

func modelCandidates(model string) []string {
	seen := map[string]struct{}{}
	candidates := make([]string, 0, 4)
	add := func(value string) {
		value = NormalizeModelName(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		candidates = append(candidates, value)
	}
	add(model)
	trimmed := strings.TrimSpace(model)
	if idx := strings.LastIndexByte(trimmed, '/'); idx >= 0 && idx+1 < len(trimmed) {
		add(trimmed[idx+1:])
	}
	if idx := strings.LastIndexByte(trimmed, '.'); idx >= 0 && idx+1 < len(trimmed) {
		add(trimmed[idx+1:])
	}
	return candidates
}

func modelKeyMatches(key string, candidate string) bool {
	if prefixModelMatch(key, candidate) || prefixModelMatch(candidate, key) {
		return true
	}
	if idx := strings.LastIndexByte(key, '/'); idx >= 0 && idx+1 < len(key) {
		suffix := key[idx+1:]
		return prefixModelMatch(suffix, candidate) || prefixModelMatch(candidate, suffix)
	}
	return false
}

func prefixModelMatch(value string, prefix string) bool {
	return strings.HasPrefix(value, prefix) && (len(value) == len(prefix) || value[len(prefix)] == '-')
}

func modelAliases(model string) []string {
	normalized := NormalizeModelName(model)
	switch normalized {
	case "gpt-5.5-pro":
		return []string{"gpt-5.5-pro", "azure/gpt-5.5-pro"}
	case "gpt-5.3-codex":
		return []string{"gpt-5.3-codex", "azure/gpt-5.3-codex", "github_copilot/gpt-5.3-codex", "chatgpt/gpt-5.3-codex"}
	case "claude-haiku-4-5":
		return []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001", "anthropic.claude-haiku-4-5-20251001-v1:0"}
	case "mimo-v2.5-pro":
		return []string{"openrouter/xiaomi/mimo-v2.5-pro", "mimo-v2.5-pro"}
	case "deepseek-v4-pro":
		return []string{"deepseek/deepseek-v4-pro", "deepseek-v4-pro", "deepseek/deepseek-v3.2", "deepseek-v3.2"}
	default:
		return nil
	}
}

func fuzzyModelBase(model string) string {
	model = NormalizeModelName(model)
	if idx := strings.LastIndexByte(model, '/'); idx >= 0 && idx+1 < len(model) {
		model = model[idx+1:]
	}
	if idx := strings.LastIndexByte(model, '.'); idx >= 0 && idx+1 < len(model) && strings.HasPrefix(model[idx+1:], "claude-") {
		model = model[idx+1:]
	}
	model = strings.ReplaceAll(model, ".", "-")
	parts := strings.Split(model, "-")
	for len(parts) > 0 {
		last := parts[len(parts)-1]
		if last == "" || isNumericModelPart(last) || isRegionModelPart(last) {
			parts = parts[:len(parts)-1]
			continue
		}
		break
	}
	return strings.Join(parts, "-")
}

func isNumericModelPart(value string) bool {
	hasDigit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			hasDigit = true
			continue
		}
		if r == '.' {
			continue
		}
		return false
	}
	return hasDigit
}

func isRegionModelPart(value string) bool {
	switch value {
	case "sgp", "us", "eu", "jp", "kr", "cn", "global", "no", "co", "thinking":
		return true
	default:
		return false
	}
}

func betterModelMatch(current string, next string) bool {
	return current == "" || len(next) > len(current) || (len(next) == len(current) && next < current)
}

func betterDirectModelMatch(current string, next string) bool {
	return current == "" || len(next) < len(current) || (len(next) == len(current) && next < current)
}

func NormalizeModelName(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"openai/", "anthropic/", "bedrock/", "azure/", "azure_openai/", "vertex_ai/", "vertex/", "google/", "xiaomi/"} {
		if strings.HasPrefix(normalized, prefix) {
			normalized = normalized[len(prefix):]
			break
		}
	}
	if idx := strings.LastIndexByte(normalized, '/'); idx >= 0 && idx+1 < len(normalized) {
		suffix := normalized[idx+1:]
		if strings.HasPrefix(suffix, "gpt-") || strings.HasPrefix(suffix, "claude-") || strings.HasPrefix(suffix, "mimo-") || strings.HasPrefix(suffix, "deepseek-") {
			normalized = suffix
		}
	}
	if idx := strings.LastIndexByte(normalized, '.'); idx >= 0 && idx+1 < len(normalized) && strings.HasPrefix(normalized[idx+1:], "claude-") {
		normalized = normalized[idx+1:]
	}
	for _, layout := range dateSuffixes {
		if t, err := time.Parse(layout, normalized[max(0, len(normalized)-len(layout)):]); err == nil && !t.IsZero() {
			return normalized[:len(normalized)-len(layout)]
		}
	}
	return normalized
}

type CacheEntry struct {
	Path    string       `json:"path"`
	ModTime int64        `json:"mod_time"`
	Size    int64        `json:"size"`
	Entries []UsageEntry `json:"entries"`
}

type TokenCache struct {
	Files map[string]CacheEntry `json:"files"`
}

type EntryLoader func(string, string, time.Time) []UsageEntry

func LoadCachedEntries(root string, category AgentCategory, now time.Time, home string, loader EntryLoader) LoadResult {
	cachePath := tokenCachePath(home, category)
	cache := readTokenCache(cachePath)
	paths := jsonlPaths(root)
	seenPaths := make(map[string]struct{}, len(paths))
	var result LoadResult
	result.Cache.FilesTotal = len(paths)
	entries := make([]UsageEntry, 0)
	changed := false
	for _, path := range paths {
		seenPaths[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		cached, ok := cache.Files[path]
		if ok && cached.ModTime == info.ModTime().UnixNano() && cached.Size == info.Size() {
			result.Cache.Hits++
			entries = append(entries, cached.Entries...)
			continue
		}
		if ok {
			result.Cache.Stale++
		} else {
			result.Cache.Misses++
		}
		loaded := loader(path, root, now)
		for i := range loaded {
			loaded[i].SourceFile = path
		}
		cache.Files[path] = CacheEntry{Path: path, ModTime: info.ModTime().UnixNano(), Size: info.Size(), Entries: loaded}
		entries = append(entries, loaded...)
		changed = true
	}
	for path := range cache.Files {
		if _, ok := seenPaths[path]; !ok {
			delete(cache.Files, path)
			result.Cache.Removed++
			changed = true
		}
	}
	if changed {
		writeTokenCache(cachePath, cache)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	result.Entries = dedupe(entries)
	return result
}

func tokenCachePath(home string, category AgentCategory) string {
	if cacheHome := os.Getenv("XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(cacheHome, "llm-usage", string(category)+".json")
	}
	return filepath.Join(home, ".cache", "llm-usage", string(category)+".json")
}

func readTokenCache(path string) TokenCache {
	file, err := os.Open(path)
	if err != nil {
		return TokenCache{Files: map[string]CacheEntry{}}
	}
	defer file.Close()
	var cache TokenCache
	if json.NewDecoder(file).Decode(&cache) != nil || cache.Files == nil {
		return TokenCache{Files: map[string]CacheEntry{}}
	}
	return cache
}

func writeTokenCache(path string, cache TokenCache) {
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "entries_*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if json.NewEncoder(tmp).Encode(cache) != nil || tmp.Close() != nil {
		return
	}
	if os.Rename(tmpPath, path) != nil {
		return
	}
	ok = true
}

func jsonlPaths(root string) []string {
	paths := make([]string, 0)
	walkJSONL(root, func(path string, d fs.DirEntry) {
		paths = append(paths, path)
	})
	sort.Strings(paths)
	return paths
}

func LoadClaudeEntries(projectsDir string, now time.Time) []UsageEntry {
	var entries []UsageEntry
	walkJSONL(projectsDir, func(path string, d fs.DirEntry) {
		entries = append(entries, LoadClaudeFile(path, projectsDir)...)
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	return entries
}

func LoadClaudeFile(path string, projectsDir string) []UsageEntry {
	entries := make([]UsageEntry, 0)
	project := projectFromClaudePath(projectsDir, path)
	loadJSONLines(path, func(data map[string]any) {
		entry, ok := parseClaudeLine(data, project)
		if ok {
			entries = append(entries, entry)
		}
	})
	return entries
}

func parseClaudeLine(data map[string]any, project string) (UsageEntry, bool) {
	if asString(data["type"]) != "assistant" {
		return UsageEntry{}, false
	}
	message := asMap(data["message"])
	usage := asMap(message["usage"])
	if usage == nil {
		return UsageEntry{}, false
	}
	timestamp, ok := parseTimestamp(data["timestamp"])
	if !ok {
		return UsageEntry{}, false
	}
	entry := UsageEntry{
		Timestamp:           timestamp,
		SessionID:           asString(data["sessionId"]),
		MessageID:           asString(message["id"]),
		RequestID:           asString(data["requestId"]),
		Model:               defaultString(asString(message["model"]), "unknown"),
		InputTokens:         asInt(usage["input_tokens"]),
		OutputTokens:        asInt(usage["output_tokens"]),
		CacheCreationTokens: asInt(usage["cache_creation_input_tokens"]),
		CacheReadTokens:     asInt(usage["cache_read_input_tokens"]),
		CostUSD:             asOptionalFloat(data["costUSD"]),
		Project:             project,
		AgentCategory:       AgentClaude,
	}
	if cwd := asString(data["cwd"]); cwd != "" {
		entry.Project = filepath.Base(filepath.Clean(os.ExpandEnv(cwd)))
	}
	return entry, entry.TotalTokens() > 0
}

func LoadCodexEntries(root string, now time.Time) []UsageEntry {
	sessionsDir := filepath.Join(root, "sessions")
	var entries []UsageEntry
	walkJSONL(sessionsDir, func(path string, d fs.DirEntry) {
		entries = append(entries, LoadCodexFile(path, root)...)
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	return entries
}

func LoadCodexFile(path string, root string) []UsageEntry {
	models := loadCodexThreadModels(filepath.Join(root, "state_5.sqlite"))
	if entry, ok := parseCodexSession(path, models); ok {
		return []UsageEntry{entry}
	}
	return nil
}

func parseCodexSession(path string, models map[string]string) (UsageEntry, bool) {
	var sessionID, sessionTimestamp, project, lastTimestamp string
	var lastUsage map[string]any
	loadJSONLines(path, func(data map[string]any) {
		switch asString(data["type"]) {
		case "session_meta":
			payload := asMap(data["payload"])
			sessionID = asString(payload["id"])
			sessionTimestamp = asString(payload["timestamp"])
			project = projectFromCWD(asString(payload["cwd"]))
		case "event_msg":
			payload := asMap(data["payload"])
			if asString(payload["type"]) != "token_count" {
				return
			}
			usage := asMap(asMap(payload["info"])["total_token_usage"])
			if usage != nil {
				lastUsage = usage
				lastTimestamp = asString(data["timestamp"])
			}
		}
	})
	if sessionID == "" || lastUsage == nil {
		return UsageEntry{}, false
	}
	timestamp, ok := parseTimestamp(lastTimestamp)
	if !ok {
		timestamp, ok = parseTimestamp(sessionTimestamp)
	}
	if !ok {
		return UsageEntry{}, false
	}
	cached := asInt(lastUsage["cached_input_tokens"])
	input := asInt(lastUsage["input_tokens"]) - cached
	if input < 0 {
		input = 0
	}
	output := asInt(lastUsage["output_tokens"]) + asInt(lastUsage["reasoning_output_tokens"])
	model := defaultString(models[sessionID], "unknown")
	return UsageEntry{Timestamp: timestamp, SessionID: sessionID, MessageID: sessionID, Model: model, InputTokens: input, OutputTokens: output, CacheReadTokens: cached, Project: defaultString(project, "unknown"), AgentCategory: AgentCodex}, input+output > 0
}

func LoadAssistantEntries(root string, category AgentCategory, now time.Time) []UsageEntry {
	var entries []UsageEntry
	for _, dir := range []string{"projects", "sessions", "history"} {
		walkJSONL(filepath.Join(root, dir), func(path string, d fs.DirEntry) {
			entries = append(entries, parseGenericJSONL(path, category)...)
		})
	}
	walkJSONL(root, func(path string, d fs.DirEntry) {
		entries = append(entries, parseGenericJSONL(path, category)...)
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	return dedupe(entries)
}

func parseGenericJSONL(path string, category AgentCategory) []UsageEntry {
	var entries []UsageEntry
	loadJSONLines(path, func(data map[string]any) {
		if entry, ok := parseGenericLine(data, category); ok {
			entries = append(entries, entry)
		}
	})
	return entries
}

func parseGenericLine(data map[string]any, category AgentCategory) (UsageEntry, bool) {
	timestamp, ok := firstTimestamp(data, "timestamp", "created_at", "createdAt", "time")
	if !ok {
		if ts := asFloat(data["created"]); ts > 0 {
			timestamp = time.Unix(int64(ts), 0).UTC()
			ok = true
		}
	}
	if !ok {
		return UsageEntry{}, false
	}
	usage := firstMap(data, "usage", "token_usage", "total_token_usage")
	if usage == nil {
		message := asMap(data["message"])
		usage = firstMap(message, "usage", "token_usage", "total_token_usage")
	}
	if usage == nil {
		payload := asMap(data["payload"])
		usage = firstMap(payload, "usage", "token_usage", "total_token_usage")
		if usage == nil {
			info := asMap(payload["info"])
			usage = firstMap(info, "usage", "token_usage", "total_token_usage")
		}
	}
	if usage == nil {
		return UsageEntry{}, false
	}
	model := firstString(data, "model", "model_name", "modelName")
	if model == "" {
		message := asMap(data["message"])
		model = firstString(message, "model", "model_name", "modelName")
	}
	if model == "" {
		model = "unknown"
	}
	category = detectAgentCategory(data, category)
	cached := asInt(firstValue(usage, "cached_input_tokens", "cache_read_input_tokens", "cacheRead"))
	input := asInt(firstValue(usage, "input_tokens", "prompt_tokens", "input"))
	if asInt(usage["cached_input_tokens"]) > 0 || asInt(usage["cacheRead"]) > 0 {
		input -= cached
		if input < 0 {
			input = 0
		}
	}
	output := asInt(firstValue(usage, "output_tokens", "completion_tokens", "output")) + asInt(usage["reasoning_output_tokens"])
	entry := UsageEntry{Timestamp: timestamp, SessionID: firstString(data, "sessionId", "session_id", "conversation_id", "thread_id"), MessageID: firstString(data, "messageId", "message_id", "id"), RequestID: firstString(data, "requestId", "request_id"), Model: model, InputTokens: input, OutputTokens: output, CacheCreationTokens: asInt(firstValue(usage, "cache_creation_input_tokens", "cacheWrite", "cache_write_input_tokens", "cacheWriteTokens")), CacheReadTokens: cached, CostUSD: genericCost(data, usage), Project: projectFromCWD(firstString(data, "cwd", "workspace", "project", "project_path", "projectPath")), AgentCategory: category}
	return entry, entry.TotalTokens() > 0 || entry.CostUSD != nil
}

func genericCost(data map[string]any, usage map[string]any) *float64 {
	if cost := asOptionalFloat(firstValue(data, "costUSD", "cost_usd", "cost")); cost != nil {
		return cost
	}
	costMap := asMap(usage["cost"])
	if costMap != nil {
		return asOptionalFloat(costMap["total"])
	}
	return nil
}

func detectAgentCategory(data map[string]any, fallback AgentCategory) AgentCategory {
	for _, key := range []string{"agent_category", "agentCategory", "agent", "source", "client"} {
		value := strings.ToLower(strings.TrimSpace(asString(data[key])))
		switch value {
		case "pi":
			return AgentPi
		case "omp":
			return AgentOMP
		case "claude":
			return AgentClaude
		case "codex":
			return AgentCodex
		}
	}
	return fallback
}

func loadCodexThreadModels(dbPath string) map[string]string {
	models := map[string]string{}
	if _, err := os.Stat(dbPath); err != nil {
		return models
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return models
	}
	defer db.Close()
	rows, err := db.Query("SELECT id, model FROM threads WHERE model IS NOT NULL")
	if err != nil {
		return models
	}
	defer rows.Close()
	for rows.Next() {
		var id, model string
		if rows.Scan(&id, &model) == nil && id != "" && model != "" {
			models[id] = model
		}
	}
	return models
}

func dedupe(entries []UsageEntry) []UsageEntry {
	seen := make(map[string]struct{}, len(entries))
	out := entries[:0]
	for _, entry := range entries {
		key := dedupKey(entry)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	return out
}

func dedupKey(entry UsageEntry) string {
	if entry.MessageID != "" || entry.RequestID != "" {
		return fmt.Sprintf("%s:message:%s:%s", entry.AgentCategory, entry.MessageID, entry.RequestID)
	}
	return fmt.Sprintf("%s:entry:%s:%s:%s:%d:%d:%d:%d", entry.AgentCategory, entry.SessionID, entry.Timestamp.Format(time.RFC3339Nano), entry.Model, entry.InputTokens, entry.OutputTokens, entry.CacheCreationTokens, entry.CacheReadTokens)
}

func walkJSONL(root string, fn func(string, fs.DirEntry)) {
	info, err := os.Stat(root)
	if err != nil {
		return
	}
	if !info.IsDir() {
		if strings.HasSuffix(root, ".jsonl") {
			fn(root, nil)
		}
		return
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fn(path, d)
		return nil
	})
}

func loadJSONLines(path string, fn func(map[string]any)) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var data map[string]any
		if json.Unmarshal(scanner.Bytes(), &data) == nil {
			fn(data)
		}
	}
}

func parseTimestamp(value any) (time.Time, bool) {
	s, ok := value.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	layouts := []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func firstTimestamp(data map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if t, ok := parseTimestamp(data[key]); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func projectFromClaudePath(projectsDir, jsonlPath string) string {
	rel, err := filepath.Rel(projectsDir, jsonlPath)
	if err != nil {
		return "unknown"
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) == 0 || parts[0] == "." {
		return "unknown"
	}
	decoded := strings.Trim(strings.ReplaceAll(parts[0], "-", string(os.PathSeparator)), string(os.PathSeparator))
	base := filepath.Base(decoded)
	if base == "." || base == string(os.PathSeparator) || base == "" {
		return "unknown"
	}
	return base
}

func projectFromCWD(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	clean := filepath.Clean(os.ExpandEnv(cwd))
	base := filepath.Base(clean)
	if base == "." || base == string(os.PathSeparator) || base == "" {
		return "unknown"
	}
	return base
}

func asMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func firstMap(data map[string]any, keys ...string) map[string]any {
	if data == nil {
		return nil
	}
	for _, key := range keys {
		if m := asMap(data[key]); m != nil {
			return m
		}
	}
	return nil
}

func firstValue(data map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			return value
		}
	}
	return nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := asString(data[key]); s != "" {
			return s
		}
	}
	return ""
}

func asString(value any) string {
	s, _ := value.(string)
	return s
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func normalizeUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func asInt(value any) int64 {
	switch v := value.(type) {
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 {
			return int64(v)
		}
	case int64:
		if v > 0 {
			return v
		}
	case int:
		if v > 0 {
			return int64(v)
		}
	}
	return 0
}

func asFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v
		}
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

func asOptionalFloat(value any) *float64 {
	v := asFloat(value)
	if v == 0 {
		return nil
	}
	return &v
}

func formatInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem == 0 {
		rem = 3
	}
	b.WriteString(s[:rem])
	for i := rem; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func formatPromptCacheRate(totals Totals) string {
	denominator := totals.PromptCacheHitTokens + totals.PromptCacheMissTokens
	if denominator <= 0 {
		return "--"
	}
	return fmt.Sprintf("%.1f%%", float64(totals.PromptCacheHitTokens)*100/float64(denominator))
}

func formatIntBar(value, maxValue int64, width int) string {
	if maxValue <= 0 {
		return strings.Repeat(".", width)
	}
	return formatBar(float64(value), float64(maxValue), width)
}

func formatFloatBar(value, maxValue float64, width int) string {
	if maxValue <= 0 {
		return strings.Repeat(".", width)
	}
	return formatBar(value, maxValue, width)
}

func formatBar(value, maxValue float64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := int(math.Round(value / maxValue * float64(width)))
	if value > 0 && filled == 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("#", filled) + strings.Repeat(".", width-filled)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
