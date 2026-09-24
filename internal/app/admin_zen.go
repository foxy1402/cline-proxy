package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ Zen Free Model Management API ============

// GET /admin/api/zen/config
func handleZenConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	cfg := getZenConfig()
	data := map[string]any{
		"enabled":         cfg.Enabled,
		"key":             cfg.Key,
		"keys":            cfg.Keys,
		"keyStates":       zenKeyStatus(),
		"baseURL":         cfg.BaseURL,
		"proxies":         cfg.Proxies,
		"proxyStrategy":   cfg.ProxyStrategy,
		"maxConcurrency":  cfg.MaxConcurrency,
		"retries":         cfg.Retries,
		"failover":        cfg.Failover,
		"failoverCount":   cfg.FailoverCount,
		"failoverMinutes": cfg.FailoverMinutes,
		"compaction":      cfg.Compaction,
		"runtime": map[string]any{
			"failoverActive": zenFailedNow(),
			"proxyCooldowns": zenProxyCooldownStatus(),
		},
	}
	if auth, err := GetConsoleAuth(); err == nil && auth != nil && auth.AccessToken != "" {
		data["consoleAuth"] = map[string]any{
			"available": true,
			"tokenMask": maskZenKey(auth.AccessToken),
			"orgID":     auth.ActiveOrgID,
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: data})
}

// POST /admin/api/zen/config/update
func handleZenConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	cur := getZenConfig()
	var patch struct {
		Enabled         *bool    `json:"enabled"`
		Key             *string  `json:"key"`
		Keys            []string `json:"keys"`
		BaseURL         *string  `json:"baseURL"`
		Proxies         []string `json:"proxies"`
		ProxyStrategy   *string  `json:"proxyStrategy"`
		MaxConcurrency  *int     `json:"maxConcurrency"`
		Retries         *int     `json:"retries"`
		Failover        *bool    `json:"failover"`
		FailoverCount   *int     `json:"failoverCount"`
		FailoverMinutes *int     `json:"failoverMinutes"`
		Compaction      *struct {
			Auto         *bool   `json:"auto"`
			Buffer       *int    `json:"buffer"`
			KeepTokens   *int    `json:"keepTokens"`
			SummaryModel *string `json:"summaryModel"`
			MaxSummary   *int    `json:"maxSummary"`
		} `json:"compaction"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	next := &zenConfigData{
		Enabled:         cur.Enabled,
		Key:             cur.Key,
		Keys:            cur.Keys,
		BaseURL:         cur.BaseURL,
		Proxies:         cur.Proxies,
		ProxyStrategy:   cur.ProxyStrategy,
		MaxConcurrency:  cur.MaxConcurrency,
		Retries:         cur.Retries,
		Failover:        cur.Failover,
		FailoverCount:   cur.FailoverCount,
		FailoverMinutes: cur.FailoverMinutes,
		Compaction:      cur.Compaction,
	}
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if patch.Keys != nil {
		// Multi-key pool full replacement; backward compatible with legacy single key
		if len(patch.Keys) == 0 && (patch.Key == nil || *patch.Key == "") {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "keys list is empty"})
			return
		}
		if len(patch.Keys) > 0 {
			next.Keys = patch.Keys
		}
	}
	if patch.Key != nil && *patch.Key != "" && patch.Keys == nil {
		// Legacy client single key submission -> single element pool
		next.Keys = []string{*patch.Key}
	}
	if patch.BaseURL != nil && *patch.BaseURL != "" {
		next.BaseURL = strings.TrimRight(*patch.BaseURL, "/")
	}
	if patch.Proxies != nil {
		if err := validateProxyList(patch.Proxies); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		next.Proxies = patch.Proxies
	}
	if patch.ProxyStrategy != nil && *patch.ProxyStrategy != "" {
		next.ProxyStrategy = *patch.ProxyStrategy
	}
	if patch.MaxConcurrency != nil && *patch.MaxConcurrency > 0 {
		next.MaxConcurrency = *patch.MaxConcurrency
	}
	if patch.Retries != nil && *patch.Retries >= 0 {
		next.Retries = *patch.Retries
	}
	if patch.Failover != nil {
		next.Failover = *patch.Failover
	}
	if patch.FailoverCount != nil && *patch.FailoverCount > 0 {
		next.FailoverCount = *patch.FailoverCount
	}
	if patch.FailoverMinutes != nil && *patch.FailoverMinutes > 0 {
		next.FailoverMinutes = *patch.FailoverMinutes
	}
	if patch.Compaction != nil {
		base := cur.Compaction
		if patch.Compaction.Buffer != nil {
			base.Buffer = *patch.Compaction.Buffer
		}
		if patch.Compaction.KeepTokens != nil {
			base.KeepTokens = *patch.Compaction.KeepTokens
		}
		if patch.Compaction.SummaryModel != nil {
			base.SummaryModel = *patch.Compaction.SummaryModel
		}
		if patch.Compaction.MaxSummary != nil {
			base.MaxSummary = *patch.Compaction.MaxSummary
		}
		if patch.Compaction.Auto != nil {
			base.Auto = *patch.Compaction.Auto
		}
		next.Compaction = base
	}
	setZenConfig(next)
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: getZenConfig()})
}

// GET /admin/api/opencode/models — returns only free models
func handleZenModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initZenModels()
	zenModelsMu.RLock()
	models := make([]map[string]any, 0, len(zenModels))
	for _, m := range zenModels {
		if !isZenFreeModel(m) {
			continue
		}
		models = append(models, map[string]any{
			"id":        m.ID,
			"aliases":   m.Aliases,
			"context":   m.Context,
			"output":    m.Output,
			"source":    m.Source,
			"upstream":  m.Upstream,
			"toolCall":  m.ToolCall,
			"reasoning": m.Reasoning,
			"attach":    m.Attach,
		})
	}
	zenModelsMu.RUnlock()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"models": models, "count": len(models)}})
}

// POST /admin/api/zen/models/refresh
func handleZenModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	added, err := syncZenModels()
	if err != nil {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: "sync failed: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: fmt.Sprintf("synced, %d new models", added)})
}

// GET /admin/api/zen/stats
func handleZenStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: zenStatsSnapshot()})
}

// GET /admin/api/zen/sessions
// Live session status for each key + progress of current (or last) manual mint task.
func handleZenSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	keys := getZenConfig().Keys
	sessions := make([]map[string]any, 0, len(keys))
	live := 0
	for i, k := range keys {
		s := zenSessionSnapshotOf(k)
		if s.Live {
			live++
		}
		entry := map[string]any{
			"index":     i,
			"keyMask":   maskZenKey(k),
			"noKey":     k == "" || k == "public",
			"live":      s.Live,
			"minted":    s.Minted,
			"session":   s.Session,
			"harvested": "",
		}
		if s.HarvestedAt > 0 {
			entry["harvested"] = time.Unix(s.HarvestedAt, 0).Format(time.RFC3339)
		}
		sessions = append(sessions, entry)
	}
	data := map[string]any{
		"harvestEnabled":    harvestEnabled(),
		"concurrency":       harvestConcurrency(),
		"intervalHours":     int(harvestInterval() / time.Hour),
		"keyTimeoutSeconds": int(harvestKeyBudget() / time.Second),
		"liveCount":         live,
		"total":             len(keys),
		"sessions":          sessions,
		"job":               zenMintJobStatus(),
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: data})
}

// POST /admin/api/zen/keys/test   body: {"index": 0}
// Sends a lightweight probe request for a single zen key (analogous to Cline account Test button).
// The probe is pinned to this key (zenCallOpts.pinKey), without affecting regular rotation;
// a 2xx response clears cooldown on the key.
func handleZenKeyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Index int    `json:"index"`
		Model string `json:"model"` // Optional: specified probe model; empty = auto (big-pickle -> live -> seed)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	cfg := getZenConfig()
	if req.Index < 0 || req.Index >= len(cfg.Keys) {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "key index out of range"})
		return
	}
	key := cfg.Keys[req.Index]
	if key == "" || key == "public" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "nothing to test: the anonymous public key has no credential"})
		return
	}

	result, status := testZenKey(key, req.Index, req.Model)
	// status must be in Data: the dashboard JS reads r.status
	result["status"] = status
	log.Printf("Test zen key #%d (%s): status=%s model=%s reason=%v",
		req.Index+1, maskZenKey(key), status, result["model"], result["reason"])

	writeAPI(w, http.StatusOK, apiResponse{
		Success: status == "active",
		Message: status,
		Data:    result,
	})
}

// testZenKey executes a single-key probe: "Reply with exactly: OK", routing to chat
// or responses based on the model's Upstream field. Returns (result, status).
func testZenKey(key string, index int, modelID string) (map[string]any, string) {
	result := map[string]any{
		"index":   index,
		"keyMask": maskZenKey(key),
	}
	var zm *ZenModel
	if strings.TrimSpace(modelID) != "" {
		// Supports alias and opencode/ prefix matching
		m, ok := resolveZenFreeModel(modelID)
		if !ok {
			result["reason"] = fmt.Sprintf("unknown or non-free zen model: %s", modelID)
			return result, "error"
		}
		zm = m
	} else {
		zm = zenProbeModel()
	}
	if zm == nil {
		result["reason"] = "no zen model available to probe (model catalog empty)"
		return result, "error"
	}
	result["model"] = zm.ID

	params := map[string]any{
		"model":    zm.ID,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: OK"}},
		// Upstream 2xx confirms probe success
		"max_tokens": 64,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	var (
		resp *http.Response
		err  error
	)
	opts := []zenCallOpts{{pinKey: key}}
	if zm.Upstream == "responses" {
		resp, _, err = callZenResponsesAPI(ctx, params, true, opts...)
	} else {
		resp, _, err = callZenAPI(ctx, params, true, opts...)
	}
	result["latencyMs"] = time.Since(start).Milliseconds()

	he := (*zenHTTPError)(nil)
	if err != nil && errors.As(err, &he) {
		// Evaluate rate limit before status code
		if he.RateLimited {
			result["httpStatus"] = he.Status
			result["reason"] = fmt.Sprintf("rate limited (HTTP %d): %s", he.Status, kit.Truncate(he.Body, 300))
			if until, ok := zenKeyCooldownUntil(key); ok {
				result["cooldownUntil"] = until.UTC().Format(time.RFC3339)
				result["remaining"] = formatDuration(time.Until(until))
			}
			return result, "cooldown"
		}
		switch he.Status {
		case http.StatusForbidden:
			result["httpStatus"] = he.Status
			if isConsoleKey(key) {
				result["reason"] = "console authentication rejected (403) — token expired or invalid; run 'opencode console login' or refresh credentials"
			} else {
				result["reason"] = "session rejected (403) — this key's session is no longer live; the harvester was just triggered, use the mint buttons below to retry now"
			}
			return result, "error"
		default:
			result["httpStatus"] = he.Status
			result["reason"] = fmt.Sprintf("API %d: %s", he.Status, kit.Truncate(he.Body, 300))
			return result, "error"
		}
	}
	if err != nil {
		// Network/timeout error
		result["reason"] = "upstream call failed: " + err.Error()
		return result, "error"
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		result["httpStatus"] = resp.StatusCode
		result["reason"] = fmt.Sprintf("API %d: %s", resp.StatusCode, kit.Truncate(string(bodyBytes), 300))
		return result, "error"
	}

	// Success: uncool key
	uncoolZenKey(key)
	result["reason"] = "ok"
	return result, "active"
}

// POST /admin/api/zen/sessions/mint
// Manually mints live sessions for the entire pool. Runs in background.
// body: {"force": true}
func handleZenSessionsMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	if !harvestEnabled() {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "harvester unavailable: opencode CLI not present (ZEN_HARVEST_BIN)"})
		return
	}
	var body struct {
		Force *bool `json:"force"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "read body: " + err.Error()})
			return
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON: " + err.Error()})
				return
			}
		}
	}
	force := body.Force != nil && *body.Force
	started, state := startZenMintJob(getZenConfig().Keys, force)
	msg := "mint job started"
	if !started {
		msg = "mint job already running"
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: msg, Data: state})
}
