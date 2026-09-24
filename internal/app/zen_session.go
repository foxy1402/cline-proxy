package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ zen sticky sessions ============
//
// Background (empirically confirmed 2026-09-17): zen free tier binds sessions
// on the server side by x-opencode-session / prompt_cache_key. A random sess_
// will result in 403 FreeTierError even if formatted correctly; only session
// IDs previously observed and accepted by the server will succeed.
// Replaying a gateway request using a live CLI session returns 200, whereas
// replaying the exact same request with a freshly generated random session returns 403.
//
// Consequently, the gateway no longer mints a new session per request. Each zen key
// is bound to a stable sess_ ID (+ fixed ai-sdk style UA), while msg_ request IDs
// remain uniquely randomized per request. Sessions are persisted to
// DATA_DIR/.zen-sessions.json so that identities survive server restarts.
//
// Rate-limit trade-offs: previously a "fresh identity per request" strategy was
// attempted to avoid session-level rate limits. However, free-tier session binding
// takes precedence — without a live session, requests never reach accounting.
// If a session is rate-limited (429/503), the caller treats it with key cooldown semantics.
// Expired sessions (FreeTier 403) are renewed in the background by the harvester
// (harvestOnForbidden, see zen_harvest.go); local random sess_ IDs always yield 403.

type zenSessionEntry struct {
	Session string `json:"session"`
	UA      string `json:"ua"`
	Updated int64  `json:"updated"`
	// Minted indicates whether this session was minted by the CLI harvester (seen by upstream).
	// False indicates a local random placeholder (fallback during startup race condition).
	Minted bool `json:"minted,omitempty"`
	// HarvestedAt is the timestamp (unix seconds) of the most recent successful harvest.
	// Periodic harvesting uses this value instead of Updated (which updates on every request).
	HarvestedAt int64 `json:"harvestedAt,omitempty"`
}

var (
	zenSessMu      sync.Mutex
	zenSessions    = map[string]*zenSessionEntry{} // zen key -> sticky identity
	zenSessLoaded  bool
	zenSessPath    string
	// zenNativeUA is the native ai-sdk style User-Agent from official CLI 1.18.31
	// (shared by sticky sessions and rotation; coupled with Dockerfile and tls_bun.go).
	zenNativeUA    = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
)

// zenSessionFile returns the persistence path for sessions (DATA_DIR preferred).
func zenSessionFile() string {
	if zenSessPath == "" {
		zenSessPath = kit.ResolveDataPath(".zen-sessions.json")
	}
	return zenSessPath
}

// loadZenSessions loads persisted sessions on startup or first access.
func loadZenSessions() {
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	if zenSessLoaded {
		return
	}
	zenSessLoaded = true
	data, err := os.ReadFile(zenSessionFile())
	if err != nil {
		// ENOENT is normal on first boot (no session file yet); log other errors
		if !os.IsNotExist(err) {
			log.Printf("zen sessions read failed (%s): %v", zenSessionFile(), err)
		}
		return
	}
	var m map[string]*zenSessionEntry
	if err := json.Unmarshal(data, &m); err != nil {
		// On corruption, backup for debugging and start fresh with empty map
		_ = os.Rename(zenSessionFile(), zenSessionFile()+".corrupt")
		log.Printf("zen sessions parse failed, starting fresh (backup: %s.corrupt): %v", zenSessionFile(), err)
		return
	}
	migrated := 0
	for k, e := range m {
		if e != nil && e.Session != "" {
			// Older files lacked the minted field. Recognize existing sessions by prefix:
			// CLI-minted IDs start with ses_*, while local placeholders start with sess_*.
			if !e.Minted && strings.HasPrefix(e.Session, "ses_") {
				e.Minted = true
				if e.HarvestedAt == 0 && e.Updated > 0 {
					e.HarvestedAt = e.Updated
				}
				migrated++
			}
			zenSessions[k] = e
		}
	}
	log.Printf("zen sessions loaded: %d key(s) with sticky identity", len(zenSessions))
	if migrated > 0 {
		log.Printf("zen sessions migrated: %d key(s) marked CLI-minted from session id prefix", migrated)
	}
}

// saveZenSessionsLocked persists current session map (caller must hold zenSessMu).
func saveZenSessionsLocked() {
	data, err := json.MarshalIndent(zenSessions, "", "  ")
	if err != nil {
		return
	}
	tmp := zenSessionFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("zen sessions save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, zenSessionFile()); err != nil {
		log.Printf("zen sessions save failed (rename): %v", err)
	}
}

// isConsoleKey checks if a key is an OpenCode Console OAuth token (st_... or active console auth).
// Console tokens natively use CanonicalSessionID() without requiring CLI session harvesting.
func isConsoleKey(key string) bool {
	if key == "" || key == "public" {
		return false
	}
	token := key
	if idx := strings.Index(key, "#"); idx != -1 {
		token = key[:idx]
	}
	if strings.HasPrefix(token, "st_") {
		return true
	}
	if auth, err := GetConsoleAuth(); err == nil && auth != nil && auth.AccessToken != "" && token == auth.AccessToken {
		return true
	}
	return false
}

// zenSessionLive checks whether this key has a live session recognized upstream (CLI minted or Console OAuth).
// false = local random placeholder where upstream requests will return 403; the request path skips it,
// and the harvester uses this flag to decide whether to mint a session.
func zenSessionLive(key string) bool {
	if key == "" || key == "public" {
		return false
	}
	if isConsoleKey(key) {
		return true
	}
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	e := zenSessions[key]
	return e != nil && e.Minted && e.Session != ""
}

// zenLiveKeys performs batch query (single lock) so pickZenKey prioritizes live keys during rotation.
func zenLiveKeys(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	live := make(map[string]bool, len(keys))
	for _, k := range keys {
		if isConsoleKey(k) {
			live[k] = true
			continue
		}
		if e := zenSessions[k]; e != nil && e.Minted && e.Session != "" {
			live[k] = true
		}
	}
	return live
}

// zenSessionSnapshot contains session status for a key (admin dashboard display).
type zenSessionSnapshot struct {
	Minted      bool
	Live        bool
	Session     string
	HarvestedAt int64
}

func zenSessionSnapshotOf(key string) zenSessionSnapshot {
	if isConsoleKey(key) {
		return zenSessionSnapshot{
			Minted:  true,
			Live:    true,
			Session: "console-oauth",
		}
	}
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	e := zenSessions[key]
	if e == nil {
		return zenSessionSnapshot{}
	}
	live := e.Minted && e.Session != ""
	return zenSessionSnapshot{
		Minted:      e.Minted,
		Live:        live,
		Session:     kit.Truncate(e.Session, 12),
		HarvestedAt: e.HarvestedAt,
	}
}

// zenSessionDesc returns a human-readable session description for logs:
// distinguishes between "never minted local placeholder" and "minted but rejected upstream".
func zenSessionDesc(key string) string {
	if isConsoleKey(key) {
		return "console-oauth"
	}
	s := zenSessionSnapshotOf(key)
	switch {
	case !s.Minted && s.Session == "":
		return "no session"
	case !s.Minted:
		return "placeholder (never minted, always 403)"
	case s.HarvestedAt <= 0:
		return "minted (age unknown)"
	default:
		age := time.Since(time.Unix(s.HarvestedAt, 0)).Round(time.Minute)
		return fmt.Sprintf("minted %v ago", age)
	}
}

// StickyZenIdentity retrieves the stable identity bound to a key: session ID and UA
// are reused across requests, while request ID is freshly generated per request.
// Returns (session, request, user-agent).
func StickyZenIdentity(key string) (sess, req, ua string) {
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	e, ok := zenSessions[key]
	if !ok || e.Session == "" {
		e = &zenSessionEntry{
			Session: "sess_" + kit.RandAlphaNum(26),
			UA:      zenNativeUA,
			// Minted=false: random ID is a placeholder during startup race conditions
			// before harvester mints a genuine upstream session.
		}
		zenSessions[key] = e
		saveZenSessionsLocked()
		log.Printf("zen sticky session created for key#%d: %s (unminted placeholder, harvester will mint)", keyIndex(key), kit.Truncate(e.Session, 24))
	}
	e.Updated = time.Now().Unix()
	// Restore UA if missing
	if e.UA == "" {
		e.UA = zenNativeUA
	}
	return e.Session, "msg_" + kit.RandAlphaNum(26), e.UA
}

// ============ OpenCode Console OAuth Authentication & Sessions ============

// ConsoleAuth represents an OpenCode Console OAuth session.
type ConsoleAuth struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenExpiry  int64     `json:"token_expiry"` // milliseconds
	ActiveOrgID  string    `json:"active_org_id"`
	Email        string    `json:"email"`
	LastUpdated  time.Time `json:"last_updated"`
}

var (
	consoleAuthMu sync.RWMutex
	cachedConsole *ConsoleAuth
)

func consoleCacheFile() string {
	return kit.ResolveDataPath(".opencode-console.json")
}

// GetConsoleAuth retrieves active Console credentials, refreshing from opencode db if expired or missing.
func GetConsoleAuth() (*ConsoleAuth, error) {
	consoleAuthMu.RLock()
	if cachedConsole != nil && isConsoleAuthValid(cachedConsole) {
		auth := *cachedConsole
		consoleAuthMu.RUnlock()
		return &auth, nil
	}
	consoleAuthMu.RUnlock()

	consoleAuthMu.Lock()
	defer consoleAuthMu.Unlock()

	if cachedConsole != nil && isConsoleAuthValid(cachedConsole) {
		auth := *cachedConsole
		return &auth, nil
	}

	auth, err := loadConsoleAuth()
	if err != nil {
		if diskAuth := readConsoleAuthDisk(); diskAuth != nil && diskAuth.AccessToken != "" {
			cachedConsole = diskAuth
			res := *diskAuth
			return &res, nil
		}
		return nil, err
	}

	cachedConsole = auth
	saveConsoleAuthDisk(auth)
	res := *auth
	return &res, nil
}

func isConsoleAuthValid(a *ConsoleAuth) bool {
	if a == nil || a.AccessToken == "" {
		return false
	}
	if a.TokenExpiry <= 0 {
		return true
	}
	nowMs := time.Now().UnixMilli()
	return a.TokenExpiry > (nowMs + 120_000)
}

func readConsoleAuthDisk() *ConsoleAuth {
	data, err := os.ReadFile(consoleCacheFile())
	if err != nil || len(data) == 0 {
		return nil
	}
	var a ConsoleAuth
	if err := json.Unmarshal(data, &a); err != nil {
		return nil
	}
	return &a
}

func saveConsoleAuthDisk(a *ConsoleAuth) {
	if a == nil {
		return
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return
	}
	tmp := consoleCacheFile() + ".tmp"
	_ = os.WriteFile(tmp, data, 0600)
	_ = os.Rename(tmp, consoleCacheFile())
}

func loadConsoleAuth() (*ConsoleAuth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	query := "SELECT a.access_token, a.refresh_token, a.token_expiry, s.active_org_id, a.email FROM account a JOIN account_state s ON a.id = s.active_account_id LIMIT 1;"
	cmd := exec.CommandContext(ctx, "opencode", "db", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("opencode db failed (%w): %s", err, strings.TrimSpace(string(out)))
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, errors.New("opencode db returned no active account rows")
	}

	row := strings.Split(strings.TrimRight(lines[1], "\r"), "\t")
	if len(row) < 4 {
		return nil, fmt.Errorf("opencode db returned unexpected row format: %q", lines[1])
	}

	accessToken := strings.TrimSpace(row[0])
	refreshToken := strings.TrimSpace(row[1])
	tokenExpiry, _ := strconv.ParseInt(strings.TrimSpace(row[2]), 10, 64)
	activeOrgID := strings.TrimSpace(row[3])
	email := ""
	if len(row) > 4 {
		email = strings.TrimSpace(row[4])
	}

	if accessToken == "" {
		return nil, errors.New("opencode db active account has empty access_token")
	}

	auth := &ConsoleAuth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenExpiry:  tokenExpiry,
		ActiveOrgID:  activeOrgID,
		Email:        email,
		LastUpdated:  time.Now(),
	}
	return auth, nil
}

// CanonicalSessionID generates a session ID conforming to OpenCode's regex:
// ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$
func CanonicalSessionID() string {
	hexPart := make([]byte, 6)
	_, _ = rand.Read(hexPart)
	h := hex.EncodeToString(hexPart)

	const alnum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	alnumBytes := make([]byte, 14)
	randBytes := make([]byte, 14)
	_, _ = rand.Read(randBytes)
	for i := 0; i < 14; i++ {
		alnumBytes[i] = alnum[int(randBytes[i])%len(alnum)]
	}

	return fmt.Sprintf("ses_%s%s", h, string(alnumBytes))
}
