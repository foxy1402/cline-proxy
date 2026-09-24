package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ zen endpoint auto-learning ============
//
// Background: Each model in the zen free tier only serves on one native endpoint
// (/chat/completions or /v1/responses; official CLI only queries one endpoint per model).
// The gateway cannot predict a model's endpoint beforehand since the public catalog
// provides no signal (23 of 29 free models have no provider field). The only reliable
// signal is upstream rejection: querying the wrong endpoint yields 500/400 with
// endpoint-specific error signatures.
//
// Mechanism: Models with Upstream=="" first query chat/completions (default path, zero probe overhead).
// If rejected with an endpoint mismatch pattern, learnZenEndpoint marks the model as "responses"
// and persists it (DATA_DIR/.zen-endpoints.json), and immediately retries via the native responses path.
// Learned endpoints survive server restarts, allowing future requests to proceed with zero overhead.
//
// Known responses models (e.g., muse-spark, Upstream="responses") route directly
// to the responses path without probe or retry.
//
// Misclassification guard: Detection strictly evaluates HTTP response codes and
// error body keywords rather than substring-matching raw network errors (e.g., DNS failures).
// A reverse check is also included: if responses returns a chat-specific error, it learns back to chat.

// zenEndpointFile returns persistence path for learned endpoints.
func zenEndpointFile() string {
	return kit.ResolveDataPath(".zen-endpoints.json")
}

// zenHTTPError represents non-2xx responses from zen upstream with status code.
type zenHTTPError struct {
	Status int
	Body   string
	// RateLimited indicates whether this error originated from rate limiting (429, 403, or 502/503).
	// Distinct from session expiration where sessions are minted afresh.
	RateLimited bool
}

func (e *zenHTTPError) Error() string {
	return fmt.Sprintf("zen API %d: %s", e.Status, e.Body)
}

// isWrongEndpoint checks if upstream error matches wrong-endpoint signatures.
func isWrongEndpoint(err error) bool {
	var he *zenHTTPError
	if !errors.As(err, &he) {
		return false // Network, timeout, or cancellation errors are not endpoint mismatches
	}
	if he.Status == http.StatusInternalServerError {
		return true
	}
	if he.Status == http.StatusServiceUnavailable && strings.Contains(strings.ToLower(he.Body), "endpoint is unavailable") {
		return true
	}
	switch he.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
	default:
		return false
	}
	msg := strings.ToLower(he.Body)
	// Rate limits, quotas, and session errors trigger normal retry, not endpoint learning
	for _, kw := range []string{
		"freetier", "free tier", "rate limit", "ratelimit", "too many",
		"overloaded", "busy", "quota", "credit", "payment", "billing",
		"limit reached", "resourceexhausted", "session",
	} {
		if strings.Contains(msg, kw) {
			return false
		}
	}
	// Wrong endpoint keywords
	for _, kw := range []string{
		"not found", "no such", "unknown model", "unsupported",
		"invalid endpoint", "wrong endpoint", "use /v1/responses",
		"use /chat/completions", "endpoint", "endpoint is unavailable",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// isWrongEndpointResponses checks for reverse signature: responses endpoint reporting to use chat.
func isWrongEndpointResponses(err error) bool {
	var he *zenHTTPError
	if !errors.As(err, &he) {
		return false
	}
	if he.Status != http.StatusBadRequest && he.Status != http.StatusNotFound && he.Status != http.StatusInternalServerError {
		return false
	}
	msg := strings.ToLower(he.Body)
	for _, kw := range []string{
		"use /chat/completions", "use /v1/chat", "invalid_endpoint",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// learnZenEndpoint learns and persists the native endpoint for a model ("responses" or "").
func learnZenEndpoint(modelID, upstream string) {
	initZenModels()
	zenModelsMu.Lock()
	m, ok := zenModels[modelID]
	if ok && m != nil && m.Upstream != upstream {
		next := *m
		next.Upstream = upstream
		zenModels[modelID] = &next
		log.Printf("zen endpoint learned: model=%s upstream=%q (persisted)", modelID, upstream)
	}
	zenModelsMu.Unlock()
	saveZenEndpoints()
}

// loadZenEndpointsFile reads learned endpoints file (returns nil if empty or invalid).
func loadZenEndpointsFile() map[string]string {
	data, err := os.ReadFile(zenEndpointFile())
	if err != nil || len(data) == 0 {
		return nil
	}
	var learned map[string]string
	if err := json.Unmarshal(data, &learned); err != nil {
		log.Printf("zen endpoints parse failed, ignoring: %v", err)
		return nil
	}
	return learned
}

// applyZenEndpoints applies learned endpoints to models map (caller does not hold lock). Returns applied count.
func applyZenEndpoints(learned map[string]string) int {
	if len(learned) == 0 {
		return 0
	}
	initZenModels()
	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	n := 0
	for id, up := range learned {
		if up != "responses" && up != "chat" && up != "" {
			continue
		}
		m, ok := zenModels[id]
		if !ok || m == nil || m.Upstream == up {
			continue
		}
		next := *m
		next.Upstream = up
		zenModels[id] = &next
		n++
	}
	return n
}

// loadZenEndpoints restores learned endpoints on startup.
func loadZenEndpoints() {
	learned := loadZenEndpointsFile()
	if n := applyZenEndpoints(learned); n > 0 {
		log.Printf("zen endpoints loaded: %d learned override(s)", n)
	}
}

// reapplyLearnedEndpoints reapplies learned endpoints after catalog synchronization.
func reapplyLearnedEndpoints() {
	learned := loadZenEndpointsFile()
	if n := applyZenEndpoints(learned); n > 0 {
		log.Printf("zen endpoints reapplied: %d learned override(s) after catalog sync", n)
	}
}

// saveZenEndpoints persists learned endpoints where Upstream is non-empty.
var zenEndpointSaveMu sync.Mutex

func saveZenEndpoints() {
	zenEndpointSaveMu.Lock()
	defer zenEndpointSaveMu.Unlock()

	zenModelsMu.RLock()
	learned := map[string]string{}
	for id, m := range zenModels {
		if m != nil && (m.Upstream == "responses" || m.Upstream == "chat") && m.Source != "seed" {
			learned[id] = m.Upstream
		}
	}
	zenModelsMu.RUnlock()
	data, err := json.MarshalIndent(learned, "", "  ")
	if err != nil {
		return
	}
	tmp := zenEndpointFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("zen endpoints save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, zenEndpointFile()); err != nil {
		log.Printf("zen endpoints save failed (rename): %v", err)
	}
}

// AutoVerifyZenModels probes any active free models that haven't been verified yet.
func AutoVerifyZenModels(ctx context.Context) (int, int) {
	initZenModels()
	zenModelsMu.RLock()
	var toProbe []string
	for id, m := range zenModels {
		if isZenFreeModel(m) && (m.Upstream == "" || m.Upstream == "unknown") {
			toProbe = append(toProbe, id)
		}
	}
	zenModelsMu.RUnlock()

	verified := 0
	failed := 0

	for _, id := range toProbe {
		select {
		case <-ctx.Done():
			return verified, failed
		default:
		}
		endpoint, err := probeZenModelEndpoint(ctx, id)
		if err == nil && endpoint != "" {
			verified++
		} else {
			failed++
		}
	}

	if verified > 0 || failed > 0 {
		log.Printf("zen probe auto-verification complete: %d verified, %d failed", verified, failed)
	}
	return verified, failed
}

func probeZenModelEndpoint(ctx context.Context, modelID string) (string, error) {
	params := map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{"role": "user", "content": "ping"},
		},
		"max_tokens": 5,
	}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Step 1: Probe chat/completions first
	resp1, _, err1 := callZenAPI(probeCtx, params, false)
	if err1 == nil && resp1 != nil && resp1.StatusCode == http.StatusOK {
		resp1.Body.Close()
		learnZenEndpoint(modelID, "chat")
		log.Printf("zen probe: model %s verified on chat/completions (HTTP 200)", modelID)
		return "chat", nil
	}
	if resp1 != nil {
		resp1.Body.Close()
	}

	// Step 2: Fall back to responses
	resp2, _, err2 := callZenResponsesAPI(probeCtx, params, false)
	if err2 == nil && resp2 != nil && resp2.StatusCode == http.StatusOK {
		resp2.Body.Close()
		learnZenEndpoint(modelID, "responses")
		log.Printf("zen probe: model %s fallback verified on responses (HTTP 200)", modelID)
		return "responses", nil
	}
	if resp2 != nil {
		resp2.Body.Close()
	}

	return "", fmt.Errorf("model %s failed on both endpoints: chat err=%v, responses err=%v", modelID, err1, err2)
}

// PruneLearnedEndpoints removes entries for models that no longer exist in desired catalog.
func PruneLearnedEndpoints(desired map[string]bool) {
	if len(desired) == 0 {
		return
	}
	zenEndpointSaveMu.Lock()
	defer zenEndpointSaveMu.Unlock()

	learned := loadZenEndpointsFile()
	if len(learned) == 0 {
		return
	}

	changed := false
	cleaned := make(map[string]string)
	for id, ep := range learned {
		if desired[id] {
			cleaned[id] = ep
		} else {
			log.Printf("zen probe: pruned deprecated model %s from learned endpoints", id)
			changed = true
		}
	}

	if changed {
		data, err := json.MarshalIndent(cleaned, "", "  ")
		if err != nil {
			return
		}
		tmp := zenEndpointFile() + ".tmp"
		_ = os.WriteFile(tmp, data, 0600)
		_ = os.Rename(tmp, zenEndpointFile())
	}
}
