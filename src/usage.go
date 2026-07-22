package main

// Usage limits footer — surfaces the same subscription usage limits that the
// `clui` TUI shows (Claude Code, Kimi Code, Codex), so a lasso user can see how
// much of their 5-hour / weekly quotas they've burned without leaving the app.
//
// This reads the same on-disk credential files each provider's own CLI writes
// and calls the same usage endpoints clui does, parsing the same fields. Tokens
// that expire (Codex, Kimi) are refreshed proactively and written back, matching
// clui's behaviour — a Kimi access token lives only ~15 minutes, so a polling
// footer would break within one refresh window otherwise. Claude's token comes
// straight from ~/.claude/.credentials.json and is used as-is (no refresh path,
// same as clui).
//
// Everything is best-effort per provider: a provider that has no credentials,
// times out, or errors simply contributes an `err` (or is omitted) rather than
// failing the whole endpoint. Results are cached briefly so multiple browser
// tabs polling don't multiply upstream requests.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const usageUserAgent = "lasso-usage/1"

// usageLimit is one quota window (5-hour block, weekly rolling, …). Percent is
// 0–100 (rounded). ResetsAt is RFC3339 (empty if unknown); the frontend formats
// it relative to now so the countdown stays live between polls. Countdown marks
// short windows the UI renders as "resets in 18m" vs a "resets Mon 09:00" date.
//
// ElapsedPct is how far through this window we are (time since the window
// started ÷ its length), 0–100, or -1 if the window length is unknown. The
// footer draws it as a notch on the usage bar: usage past the notch means you're
// burning faster than the clock and will hit the cap before it resets ("pace").
type usageLimit struct {
	Label      string `json:"label"`
	Percent    int    `json:"percent"`
	ResetsAt   string `json:"resetsAt,omitempty"`
	Countdown  bool   `json:"countdown,omitempty"`
	ElapsedPct int    `json:"elapsedPct"`
}

// usageProvider groups one provider's limits under its name and (optional) plan
// tier, e.g. "Kimi Code" · "Intermediate". Err carries a short reason when the
// provider is configured but its fetch failed, so the UI can show a hint.
type usageProvider struct {
	Name   string       `json:"name"`
	Plan   string       `json:"plan,omitempty"`
	Limits []usageLimit `json:"limits"`
	Err    string       `json:"err,omitempty"`
}

type usagePayload struct {
	Providers []usageProvider `json:"providers"`
	UpdatedAt string          `json:"updatedAt"`
}

var usageHTTP = &http.Client{Timeout: 12 * time.Second}

// usageCache memoizes the assembled payload for a short TTL. clui polls upstream
// every 60s; here several tabs may poll independently, so we collapse them.
var usageCache struct {
	mu      sync.Mutex
	at      time.Time
	payload usagePayload
}

const usageCacheTTL = 25 * time.Second

// usageLastGood remembers the last successful reading per provider so an upstream
// failure (a 429 rate-limit, a timeout) keeps showing the last known numbers
// instead of blanking to an error — the reset countdowns are computed client-side
// from resetsAt, so slightly stale percentages still read fine. It's persisted to
// disk (usageStorePath) so even a cold start lands mid rate-limit window with the
// last numbers already on screen: Anthropic's usage API rate-limits aggressively,
// so the footer must never depend on a live fetch to render. Mirrors clui, which
// keeps cached values and backs off on 429.
var usageLastGood = struct {
	mu     sync.Mutex
	m      map[string]usageProvider
	loaded bool
}{m: map[string]usageProvider{}}

// usageBackoff holds, per provider, the earliest time we may call its endpoint
// again. Set when a provider 429s so we stop hammering an API that's already
// telling us to slow down; until it elapses we serve the cached reading instead.
var usageBackoff = struct {
	mu    sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

// rateLimitCooldown is how long a provider is left alone after a 429. Generous on
// purpose: the cached reading covers the gap and a weekly quota barely moves in a
// few minutes, so backing off well beats re-tripping the limit.
const rateLimitCooldown = 5 * time.Minute

func usageInBackoff(name string) bool {
	usageBackoff.mu.Lock()
	defer usageBackoff.mu.Unlock()
	until, ok := usageBackoff.until[name]
	return ok && time.Now().Before(until)
}

func usageNoteRateLimited(name string) {
	usageBackoff.mu.Lock()
	usageBackoff.until[name] = time.Now().Add(rateLimitCooldown)
	usageBackoff.mu.Unlock()
}

func usageStorePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "lasso", "usage-cache.json")
}

// loadUsageStoreLocked seeds the last-good map from disk on first use. Call with
// usageLastGood.mu held.
func loadUsageStoreLocked() {
	if usageLastGood.loaded {
		return
	}
	usageLastGood.loaded = true
	path := usageStorePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m map[string]usageProvider
	if json.Unmarshal(data, &m) == nil {
		for k, v := range m {
			usageLastGood.m[k] = v
		}
	}
}

// saveUsageStoreLocked persists the last-good map. Call with usageLastGood.mu held.
func saveUsageStoreLocked() {
	path := usageStorePath()
	if path == "" {
		return
	}
	data, err := json.Marshal(usageLastGood.m)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, data, 0o644)
}

func serveUsage(w http.ResponseWriter, r *http.Request) {
	usageCache.mu.Lock()
	defer usageCache.mu.Unlock()
	if time.Since(usageCache.at) < usageCacheTTL && usageCache.payload.UpdatedAt != "" {
		writeJSON(w, usageCache.payload)
		return
	}

	// Fetch all three providers concurrently — one slow endpoint shouldn't
	// serialize behind the others.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	fetchers := []func(context.Context) usageProvider{
		fetchClaudeUsage,
		fetchKimiUsage,
		fetchCodexUsage,
	}
	results := make([]usageProvider, len(fetchers))
	var wg sync.WaitGroup
	for i, f := range fetchers {
		wg.Add(1)
		go func(i int, f func(context.Context) usageProvider) {
			defer wg.Done()
			results[i] = f(ctx)
		}(i, f)
	}
	wg.Wait()

	payload := usagePayload{UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	usageLastGood.mu.Lock()
	loadUsageStoreLocked()
	changed := false
	for _, p := range results {
		// Drop providers with no credentials and nothing to say (empty name).
		if p.Name == "" {
			continue
		}
		if p.Err == "" && len(p.Limits) > 0 {
			usageLastGood.m[p.Name] = p
			changed = true
		} else if prev, ok := usageLastGood.m[p.Name]; ok {
			// Live fetch failed (rate-limited, cooling down, timed out) — fall
			// back to the last successful reading rather than an error dash.
			p = prev
		}
		payload.Providers = append(payload.Providers, p)
	}
	if changed {
		saveUsageStoreLocked()
	}
	usageLastGood.mu.Unlock()

	usageCache.at = time.Now()
	usageCache.payload = payload
	writeJSON(w, payload)
}

// ---- small JSON helpers ------------------------------------------------------

// usageNum reads a JSON number OR a numeric string ("100"), matching clui's
// json_num — Kimi returns its counts as strings.
func usageNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func homeFile(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func rfc3339ToUTC(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// elapsedPct returns how far through a window of length `window` (ending at
// resetsAt) we currently are, 0–100, or -1 if either input is unknown. This is
// the notch position on the usage bar — the share of the period that has passed,
// which usage should stay under to last until the reset. resetsAt is RFC3339.
func elapsedPct(resetsAt string, window time.Duration) int {
	if resetsAt == "" || window <= 0 {
		return -1
	}
	reset, err := time.Parse(time.RFC3339, resetsAt)
	if err != nil {
		return -1
	}
	start := reset.Add(-window)
	pct := int(float64(time.Now().Sub(start)) / float64(window) * 100)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

func round(f float64) int {
	if f < 0 {
		return 0
	}
	return int(f + 0.5)
}

// ---- Claude Code -------------------------------------------------------------

func fetchClaudeUsage(ctx context.Context) usageProvider {
	p := usageProvider{Name: "Claude Code"}
	token := readClaudeToken()
	if token == "" {
		// No credentials: omit the provider entirely (name cleared).
		return usageProvider{}
	}
	if usageInBackoff(p.Name) {
		// Cooling down after a 429 — skip the call, serve the cached reading.
		p.Err = "cooldown"
		return p
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", usageUserAgent)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	body, status, err := usageDo(req)
	if err != nil {
		p.Err = err.Error()
		return p
	}
	if status == http.StatusTooManyRequests {
		usageNoteRateLimited(p.Name)
	}
	if status != http.StatusOK {
		p.Err = fmt.Sprintf("http %d", status)
		return p
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		p.Err = "bad response"
		return p
	}

	// five_hour / seven_day: percent is the `utilization` field (already 0–100).
	if b := asMap(root["five_hour"]); b != nil {
		if u, ok := usageNum(b["utilization"]); ok {
			reset := rfc3339ToUTC(asString(b["resets_at"]))
			p.Limits = append(p.Limits, usageLimit{
				Label:      "5-Hour Block",
				Percent:    round(u),
				ResetsAt:   reset,
				Countdown:  true,
				ElapsedPct: elapsedPct(reset, 5*time.Hour),
			})
		}
	}
	if b := asMap(root["seven_day"]); b != nil {
		if u, ok := usageNum(b["utilization"]); ok {
			reset := rfc3339ToUTC(asString(b["resets_at"]))
			p.Limits = append(p.Limits, usageLimit{
				Label:      "7-Day Rolling",
				Percent:    round(u),
				ResetsAt:   reset,
				ElapsedPct: elapsedPct(reset, 7*24*time.Hour),
			})
		}
	}
	// weekly_scoped (e.g. Fable): from limits[], percent is `percent`, label is
	// the scope model display_name.
	if arr, ok := root["limits"].([]any); ok {
		for _, it := range arr {
			m := asMap(it)
			if m == nil || asString(m["kind"]) != "weekly_scoped" {
				continue
			}
			pct, ok := usageNum(m["percent"])
			if !ok {
				continue
			}
			label := "Scoped"
			if scope := asMap(m["scope"]); scope != nil {
				if model := asMap(scope["model"]); model != nil {
					if dn := asString(model["display_name"]); dn != "" {
						label = dn
					}
				}
			}
			reset := rfc3339ToUTC(asString(m["resets_at"]))
			p.Limits = append(p.Limits, usageLimit{
				Label:      label + " Weekly",
				Percent:    round(pct),
				ResetsAt:   reset,
				ElapsedPct: elapsedPct(reset, 7*24*time.Hour),
			})
			break
		}
	}
	return p
}

func readClaudeToken() string {
	path := homeFile(".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return ""
	}
	oauth := asMap(root["claudeAiOauth"])
	tok := asString(oauth["accessToken"])
	if !strings.HasPrefix(tok, "sk-ant-oat") {
		return ""
	}
	return tok
}

// ---- Kimi Code ---------------------------------------------------------------

const (
	kimiUsageURL   = "https://api.kimi.com/coding/v1/usages"
	kimiRefreshURL = "https://auth.kimi.com/api/oauth/token"
	kimiClientID   = "17e5f671-d194-4dfb-9706-5516cb48c098"
)

func kimiCredPath() string {
	if h := os.Getenv("KIMI_CODE_HOME"); h != "" {
		return filepath.Join(h, "credentials", "kimi-code.json")
	}
	return homeFile(".kimi-code", "credentials", "kimi-code.json")
}

func fetchKimiUsage(ctx context.Context) usageProvider {
	p := usageProvider{Name: "Kimi Code"}
	path := kimiCredPath()
	creds, err := readJSONFile(path)
	if err != nil {
		return usageProvider{} // no credentials → omit
	}
	if usageInBackoff(p.Name) {
		p.Err = "cooldown"
		return p
	}

	token := asString(creds["access_token"])
	// Proactive refresh: Kimi access tokens are short-lived (~15m). Refresh when
	// within 60s of expiry, matching clui.
	if expUnix, ok := usageNum(creds["expires_at"]); ok {
		if time.Now().Unix()+60 >= int64(expUnix) {
			if newTok := refreshKimiToken(ctx, path, creds); newTok != "" {
				token = newTok
			}
		}
	}
	if token == "" {
		p.Err = "no token"
		return p
	}

	body, status, err := kimiUsageRequest(ctx, token)
	// Reactive refresh once on auth failure.
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if newTok := refreshKimiToken(ctx, path, creds); newTok != "" {
			body, status, err = kimiUsageRequest(ctx, newTok)
		}
	}
	if err != nil {
		p.Err = err.Error()
		return p
	}
	if status == http.StatusTooManyRequests {
		usageNoteRateLimited(p.Name)
	}
	if status != http.StatusOK {
		p.Err = fmt.Sprintf("http %d", status)
		return p
	}

	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		p.Err = "bad response"
		return p
	}

	// Plan tier: user.membership.level → "LEVEL_INTERMEDIATE" → "Intermediate".
	if user := asMap(root["user"]); user != nil {
		if mem := asMap(user["membership"]); mem != nil {
			p.Plan = formatKimiLevel(asString(mem["level"]))
		}
	}

	// 5-hour: widest MINUTE-unit window in limits[], read from its `detail`.
	var best map[string]any
	var bestDur float64
	if arr, ok := root["limits"].([]any); ok {
		for _, it := range arr {
			m := asMap(it)
			win := asMap(m["window"])
			if win == nil || !strings.Contains(asString(win["timeUnit"]), "MINUTE") {
				continue
			}
			dur, _ := usageNum(win["duration"])
			if best == nil || dur > bestDur {
				detail := asMap(m["detail"])
				if detail == nil {
					detail = m
				}
				best = detail
				bestDur = dur
			}
		}
	}
	if best != nil {
		win := time.Duration(bestDur) * time.Minute
		if lim := kimiBlock(best, kimiWindowLabel(bestDur), true, win); lim != nil {
			p.Limits = append(p.Limits, *lim)
		}
	}

	// Weekly: top-level `usage`. Kimi doesn't state the window length here; it's
	// a 7-day quota, so assume a week for the pace notch.
	if u := asMap(root["usage"]); u != nil {
		if lim := kimiBlock(u, "Weekly Limit", false, 7*24*time.Hour); lim != nil {
			p.Limits = append(p.Limits, *lim)
		}
	}
	return p
}

func kimiUsageRequest(ctx context.Context, token string) ([]byte, int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, kimiUsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", usageUserAgent)
	return usageDo(req)
}

// kimiBlock builds a limit from a {limit, used, remaining, resetTime} object.
// percent = used/limit*100; used falls back to limit-remaining. Returns nil if
// limit is missing or zero (clui omits the card). window is the period length,
// used to place the pace notch.
func kimiBlock(detail map[string]any, label string, countdown bool, window time.Duration) *usageLimit {
	limit, ok := usageNum(detail["limit"])
	if !ok || limit <= 0 {
		return nil
	}
	used, ok := usageNum(detail["used"])
	if !ok {
		rem, ok2 := usageNum(detail["remaining"])
		if !ok2 {
			return nil
		}
		used = limit - rem
	}
	reset := rfc3339ToUTC(asString(detail["resetTime"]))
	return &usageLimit{
		Label:      label,
		Percent:    round(used / limit * 100),
		ResetsAt:   reset,
		Countdown:  countdown,
		ElapsedPct: elapsedPct(reset, window),
	}
}

func kimiWindowLabel(durationMinutes float64) string {
	mins := int(durationMinutes)
	if mins > 0 && mins%60 == 0 {
		return fmt.Sprintf("%dh Limit", mins/60)
	}
	return fmt.Sprintf("%dm Limit", mins)
}

func formatKimiLevel(level string) string {
	level = strings.TrimPrefix(level, "LEVEL_")
	if level == "" {
		return ""
	}
	parts := strings.Split(level, "_")
	for i, p := range parts {
		parts[i] = titleWord(p)
	}
	return strings.Join(parts, " ")
}

// refreshKimiToken exchanges the refresh token (form-urlencoded) and writes the
// new tokens back to the credentials file. Returns the new access token, or "".
func refreshKimiToken(ctx context.Context, path string, creds map[string]any) string {
	refresh := asString(creds["refresh_token"])
	if refresh == "" {
		return ""
	}
	form := url.Values{}
	form.Set("client_id", kimiClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, kimiRefreshURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", usageUserAgent)
	body, status, err := usageDo(req)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var resp map[string]any
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	access := asString(resp["access_token"])
	if access == "" {
		return ""
	}
	creds["access_token"] = access
	if rt := asString(resp["refresh_token"]); rt != "" {
		creds["refresh_token"] = rt
	}
	expiresIn := 900.0
	if e, ok := usageNum(resp["expires_in"]); ok {
		expiresIn = e
	}
	creds["expires_in"] = int64(expiresIn)
	creds["expires_at"] = time.Now().Unix() + int64(expiresIn)
	writeJSONFile(path, creds)
	return access
}

// ---- Codex -------------------------------------------------------------------

const (
	codexUsageURL   = "https://chatgpt.com/backend-api/wham/usage"
	codexRefreshURL = "https://auth.openai.com/oauth/token"
	codexClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
)

func codexAuthPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "auth.json")
	}
	return homeFile(".codex", "auth.json")
}

func fetchCodexUsage(ctx context.Context) usageProvider {
	p := usageProvider{Name: "Codex"}
	path := codexAuthPath()
	auth, err := readJSONFile(path)
	if err != nil {
		return usageProvider{} // no credentials → omit
	}
	if usageInBackoff(p.Name) {
		p.Err = "cooldown"
		return p
	}

	tokens := asMap(auth["tokens"])
	token := asString(tokens["access_token"])
	account := asString(tokens["account_id"])
	if token == "" {
		// Plain API-key installs can't hit the usage endpoint (OAuth only).
		return usageProvider{}
	}

	// Proactive refresh when last_refresh is missing or older than 8 days.
	if codexNeedsRefresh(asString(auth["last_refresh"])) {
		if newTok := refreshCodexToken(ctx, path, auth); newTok != "" {
			token = newTok
		}
	}

	body, status, err := codexUsageRequest(ctx, token, account)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if newTok := refreshCodexToken(ctx, path, auth); newTok != "" {
			token = newTok
			body, status, err = codexUsageRequest(ctx, token, account)
		}
	}
	if err != nil {
		p.Err = err.Error()
		return p
	}
	if status == http.StatusTooManyRequests {
		usageNoteRateLimited(p.Name)
	}
	if status != http.StatusOK {
		p.Err = fmt.Sprintf("http %d", status)
		return p
	}

	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		p.Err = "bad response"
		return p
	}
	p.Plan = formatCodexPlan(asString(root["plan_type"]))

	// Two windows, classified by length: smaller = session (5h), larger = weekly.
	type win struct {
		pct   float64
		secs  int64
		reset int64
	}
	var wins []win
	if rl := asMap(root["rate_limit"]); rl != nil {
		for _, key := range []string{"primary_window", "secondary_window"} {
			m := asMap(rl[key])
			if m == nil {
				continue
			}
			pct, ok := usageNum(m["used_percent"])
			if !ok {
				continue
			}
			secs, _ := usageNum(m["limit_window_seconds"])
			reset, _ := usageNum(m["reset_at"])
			wins = append(wins, win{pct: pct, secs: int64(secs), reset: int64(reset)})
		}
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i].secs < wins[j].secs })

	mk := func(w win, label string, countdown bool) usageLimit {
		reset := ""
		if w.reset > 0 {
			reset = time.Unix(w.reset, 0).UTC().Format(time.RFC3339)
		}
		return usageLimit{
			Label:      label,
			Percent:    round(w.pct),
			ResetsAt:   reset,
			Countdown:  countdown,
			ElapsedPct: elapsedPct(reset, time.Duration(w.secs)*time.Second),
		}
	}
	switch len(wins) {
	case 0:
		// nothing
	case 1:
		if wins[0].secs <= 6*3600 {
			p.Limits = append(p.Limits, mk(wins[0], "Session", true))
		} else {
			p.Limits = append(p.Limits, mk(wins[0], "Weekly", false))
		}
	default:
		p.Limits = append(p.Limits, mk(wins[0], "Session", true))
		p.Limits = append(p.Limits, mk(wins[len(wins)-1], "Weekly", false))
	}
	return p
}

func codexUsageRequest(ctx context.Context, token, account string) ([]byte, int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", usageUserAgent)
	if account != "" {
		req.Header.Set("ChatGPT-Account-Id", account)
	}
	return usageDo(req)
}

func codexNeedsRefresh(lastRefresh string) bool {
	if lastRefresh == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, lastRefresh)
	if err != nil {
		return true
	}
	return time.Since(t) > 8*24*time.Hour
}

func formatCodexPlan(plan string) string {
	switch strings.ToLower(plan) {
	case "":
		return ""
	case "pro":
		return "Pro 20x"
	case "pro_lite", "prolite":
		return "Pro 5x"
	default:
		parts := strings.FieldsFunc(plan, func(r rune) bool { return r == '_' || r == ' ' })
		for i, p := range parts {
			parts[i] = titleWord(p)
		}
		return strings.Join(parts, " ")
	}
}

// refreshCodexToken exchanges the refresh token (JSON body) and writes the new
// tokens back, preserving other keys. Returns the new access token, or "".
func refreshCodexToken(ctx context.Context, path string, auth map[string]any) string {
	tokens := asMap(auth["tokens"])
	refresh := asString(tokens["refresh_token"])
	if refresh == "" {
		return ""
	}
	reqBody, _ := json.Marshal(map[string]any{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
		"scope":         "openid profile email",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, codexRefreshURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", usageUserAgent)
	body, status, err := usageDo(req)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var resp map[string]any
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	access := asString(resp["access_token"])
	if access == "" {
		return ""
	}
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = access
	if rt := asString(resp["refresh_token"]); rt != "" {
		tokens["refresh_token"] = rt
	}
	if idt := asString(resp["id_token"]); idt != "" {
		tokens["id_token"] = idt
	}
	auth["tokens"] = tokens
	auth["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	writeJSONFile(path, auth)
	return access
}

// ---- shared low-level helpers ------------------------------------------------

func usageDo(req *http.Request) ([]byte, int, error) {
	resp, err := usageHTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes(), resp.StatusCode, nil
}

func readJSONFile(path string) (map[string]any, error) {
	if path == "" {
		return nil, fmt.Errorf("no path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// writeJSONFile rewrites a credential file (mode 0600) with refreshed tokens.
// Best-effort: a failed write just means we refresh again next time.
func writeJSONFile(path string, m map[string]any) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// titleWord lower-cases a word then upper-cases its first rune ("INTERMEDIATE"
// → "Intermediate", "plus" → "Plus").
func titleWord(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	return strings.ToUpper(s[:1]) + s[1:]
}
