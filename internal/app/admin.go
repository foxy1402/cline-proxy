package app

import (
	"bytes"
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// In-memory OAuth login state for async browser login
var (
	oauthSessions   = make(map[string]*oauthSessionState)
	oauthSessionsMu sync.Mutex
)

type oauthSessionState struct {
	DeviceCode string
	UserCode   string
	AuthURL    string
	CreatedAt  time.Time
	Done       bool
	Success    bool
	Email      string
	Error      string
}

type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeAPI(w http.ResponseWriter, status int, resp apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// adminCORS 管理路由的跨域策略：SPA 是同源的，管理员 API 无需 CORS。
// 不回显 Access-Control-Allow-Origin —— 通配符会让任意网页在无密码模式
// （默认本机部署）下读写 /admin/api/*（含导出 refresh token、删除全部账号）。
// OPTIONS 预检在认证前短路。
func adminCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func registerAdminRoutes(mux *http.ServeMux) {
	// 管理面板认证：ADMIN_PASSWORD 设置后所有 /admin/*（含静态页与全部 API）
	// 需要会话凭证；登录/登出端点本身豁免。adminCORS 在外层保证 OPTIONS
	// 预检在认证前短路。
	auth := adminAuthMiddleware
	mux.HandleFunc("/admin/api/login", adminCORS(handleAdminLogin))
	mux.HandleFunc("/admin/api/logout", adminCORS(handleAdminLogout))
	// 静态页自行区分认证状态：未认证返回独立登录页，认证后返回完整面板
	//（数据全部由下方带 auth 的 API 提供，面板 HTML 本身不含敏感信息）
	mux.HandleFunc("/admin/", adminStaticHandler)
	mux.HandleFunc("/admin/api/accounts", adminCORS(auth(handleAdminAccounts)))
	mux.HandleFunc("/admin/api/accounts/add", adminCORS(auth(handleAdminAccountAdd)))
	mux.HandleFunc("/admin/api/accounts/delete", adminCORS(auth(handleAdminAccountDelete)))
	mux.HandleFunc("/admin/api/accounts/test", adminCORS(auth(handleAdminAccountTest)))
	mux.HandleFunc("/admin/api/oauth/start", adminCORS(auth(handleOAuthStart)))
	mux.HandleFunc("/admin/api/oauth/status", adminCORS(auth(handleOAuthStatus)))
	mux.HandleFunc("/admin/api/sso/import", adminCORS(auth(handleSSOImport)))
	mux.HandleFunc("/admin/api/stats", adminCORS(auth(handleAdminStats)))
	mux.HandleFunc("/admin/api/batch-import", adminCORS(auth(handleBatchImport)))
	mux.HandleFunc("/admin/api/accounts/refresh-all", adminCORS(auth(handleAdminRefreshAll)))
	mux.HandleFunc("/admin/api/accounts/delete-all", adminCORS(auth(handleAdminDeleteAll)))
	mux.HandleFunc("/admin/api/accounts/reset", adminCORS(auth(handleAdminAccountReset)))
	mux.HandleFunc("/admin/api/accounts/export", adminCORS(auth(handleAccountsExport)))
	mux.HandleFunc("/admin/api/logs", adminCORS(auth(handleRequestLogs)))
	mux.HandleFunc("/admin/api/keys", adminCORS(auth(handleAdminGetKeys)))
	mux.HandleFunc("/admin/api/keys/generate", adminCORS(auth(handleAdminGenerateKey)))
	mux.HandleFunc("/admin/api/keys/delete", adminCORS(auth(handleAdminDeleteKey)))
	mux.HandleFunc("/admin/api/models", adminCORS(auth(handleAdminModels)))
	mux.HandleFunc("/admin/api/models/refresh", adminCORS(auth(handleAdminModelsRefresh)))
	mux.HandleFunc("/admin/api/config", adminCORS(auth(handleAdminConfig)))
	mux.HandleFunc("/admin/api/config/update", adminCORS(auth(handleAdminUpdateConfig)))
	mux.HandleFunc("/admin/api/combos", adminCORS(auth(handleCombosList)))
	mux.HandleFunc("/admin/api/combos/create", adminCORS(auth(handleComboCreate)))
	mux.HandleFunc("/admin/api/combos/delete", adminCORS(auth(handleComboDelete)))
	mux.HandleFunc("/admin/api/opencode/config", adminCORS(auth(handleZenConfig)))
	mux.HandleFunc("/admin/api/opencode/config/update", adminCORS(auth(handleZenConfigUpdate)))
	mux.HandleFunc("/admin/api/opencode/models", adminCORS(auth(handleZenModels)))
	mux.HandleFunc("/admin/api/opencode/models/refresh", adminCORS(auth(handleZenModelsRefresh)))
	mux.HandleFunc("/admin/api/opencode/stats", adminCORS(auth(handleZenStats)))
	mux.HandleFunc("/admin/api/opencode/sessions", adminCORS(auth(handleZenSessions)))
	// 旧 zen 路径别名,兼容旧引用
	mux.HandleFunc("/admin/api/zen/config", adminCORS(auth(handleZenConfig)))
	mux.HandleFunc("/admin/api/zen/config/update", adminCORS(auth(handleZenConfigUpdate)))
	mux.HandleFunc("/admin/api/zen/models", adminCORS(auth(handleZenModels)))
	mux.HandleFunc("/admin/api/zen/models/refresh", adminCORS(auth(handleZenModelsRefresh)))
	mux.HandleFunc("/admin/api/zen/stats", adminCORS(auth(handleZenStats)))
	mux.HandleFunc("/admin/api/zen/keys/test", adminCORS(auth(handleZenKeyTest)))
	mux.HandleFunc("/admin/api/zen/sessions", adminCORS(auth(handleZenSessions)))
	mux.HandleFunc("/admin/zen/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		// no-store：登出/改密后不能让缓存的旧面板继续可用
		w.Header().Set("Cache-Control", "no-store")
		// 配置了密码且未认证时只返回独立登录页，不暴露面板 HTML
		if AdminAuthRequired() && !verifySessionToken(sessionFromRequest(r)) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(adminLoginPageHTML))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(adminHTML))
		return
	}
	http.NotFound(w, r)
}

// GET /admin/api/accounts
func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	accounts := ListAccounts()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":  accounts,
			"total":     len(accounts),
			"poolIndex": poolCurrentIdx(),
		},
	})
}

// POST /admin/api/accounts/add  body: { refreshToken, email }
func handleAdminAccountAdd(w http.ResponseWriter, r *http.Request) {
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
		RefreshToken string `json:"refreshToken"`
		APIToken     string `json:"apiToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.RefreshToken == "" && req.APIToken == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "refreshToken or apiToken is required"})
		return
	}

	// 静态 API key 账号（sk_...）：直接入池，不做校验调用（首次使用时验证，
	// 被吊销的 key 会在 401 时自动标记 expired）
	if req.APIToken != "" {
		email := req.Email
		if email == "" {
			email = "apikey_" + kit.RandHex(3)
		}
		acc := &Account{
			AccountID: "acc_" + kit.RandHex(8),
			Email:     email,
			APIToken:  req.APIToken,
			Status:    "active",
			CreatedAt: time.Now(),
		}
		addAccount(acc)
		log.Printf("API-key account added: %s", email)
		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: fmt.Sprintf("API-key account %s added", email),
			Data: map[string]any{
				"accountId": acc.AccountID,
				"email":     acc.Email,
				"kind":      "api_key",
			},
		})
		return
	}

	// Validate by refreshing
	resp, err := cline.RefreshClineToken(req.RefreshToken)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid refreshToken: " + err.Error()})
		return
	}

	if req.Email == "" {
		req.Email = fmt.Sprintf("user_%d", poolAccountCount()+1)
	}

	acc := &Account{
		AccountID:    "acc_" + kit.RandHex(8),
		Email:        req.Email,
		RefreshToken: req.RefreshToken,
		AccessToken:  "workos:" + resp.Data.AccessToken,
		ExpiresAt:    clineExpiryMs(resp.Data.ExpiresAt),
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}

	addAccount(acc)
	log.Printf("Account added via API: %s", req.Email)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Account %s added", req.Email),
		Data: map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    acc.Status,
		},
	})
}

// POST /admin/api/accounts/delete  body: { accountId }
func handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
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
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	if removeAccount(req.AccountID) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account deleted"})
	} else {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "Account not found"})
	}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	device, err := cline.WorkosDeviceAuth()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	sessionID := fmt.Sprintf("oauth_%d", time.Now().UnixMilli())
	state := &oauthSessionState{
		DeviceCode: device.DeviceCode,
		UserCode:   device.UserCode,
		AuthURL:    authURL,
		CreatedAt:  time.Now(),
	}

	oauthSessionsMu.Lock()
	// 顺带清扫：被遗弃（从未再轮询状态）的会话 15 分钟后过期，防 map 泄漏
	now := time.Now()
	for id, s := range oauthSessions {
		if now.Sub(s.CreatedAt) > 15*time.Minute {
			delete(oauthSessions, id)
		}
	}
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	go func() {
		interval := device.Interval
		if interval < 5 {
			interval = 5
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300
		}

		workosTok, err := cline.PollWorkosToken(device.DeviceCode, interval, expiresIn)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		email := "unknown"
		if reg.Data.UserInfo != nil && reg.Data.UserInfo.Email != "" {
			email = reg.Data.UserInfo.Email
		}

		acc := &Account{
			AccountID:    "acc_" + kit.RandHex(8),
			Email:        email,
			RefreshToken: reg.Data.RefreshToken,
			AccessToken:  "workos:" + reg.Data.AccessToken,
			ExpiresAt:    clineExpiryMs(reg.Data.ExpiresAt),
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", email)
	}()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"sessionId":       sessionID,
			"verificationUri": authURL,
			"userCode":        device.UserCode,
		},
	})
}

// GET /admin/api/oauth/status?sessionId=xxx
func handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "sessionId required"})
		return
	}

	oauthSessionsMu.Lock()
	state, ok := oauthSessions[sessionID]
	if ok {
		// 加锁读快照（后台轮询 goroutine 在写这些字段）；
		// 已结束的会话读走即删，map 不会无限增长
		resp := map[string]any{
			"done":    state.Done,
			"success": state.Success,
		}
		if state.Done {
			resp["email"] = state.Email
			if !state.Success {
				resp["error"] = state.Error
			}
			delete(oauthSessions, sessionID)
		}
		oauthSessionsMu.Unlock()
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
		return
	}
	oauthSessionsMu.Unlock()

	writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
}

// POST /admin/api/sso/import  body: { ssoCookies: string, email?: string }
func handleSSOImport(w http.ResponseWriter, r *http.Request) {
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
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ssoCookies is required"})
		return
	}

	// SSO cookies import - try to use WorkOS device auth (requires browser)
	// For direct SSO cookie conversion, we'd need the WorkOS session cookie
	// to exchange for tokens. This is a placeholder that accepts WorkOS session
	// cookies. In practice, users should use OAuth or direct refreshToken.
	//
	// SSO cookie format expected: workos_session=xxx or similar
	lines := strings.Split(req.SSOCookies, "\n")
	imported := 0
	errors := []string{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := cline.RefreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("token %s...: %v", kit.Truncate(token, 16), err))
				continue
			}
			email := req.Email
			if email == "" {
				email = "sso_user_" + kit.RandHex(4)
			}

			acc := &Account{
				AccountID:    "acc_" + kit.RandHex(8),
				Email:        email,
				RefreshToken: token,
				AccessToken:  "workos:" + resp.Data.AccessToken,
				ExpiresAt:    clineExpiryMs(resp.Data.ExpiresAt),
				Status:       "active",
				CreatedAt:    time.Now(),
			}
			addAccount(acc)
			imported++
		}
	}

	result := map[string]any{
		"imported": imported,
		"failed":   len(errors),
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data:    result,
	})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
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
		Tokens []struct {
			RefreshToken string `json:"refreshToken"`
			APIToken     string `json:"apiToken"`
			Email        string `json:"email"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if len(req.Tokens) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "tokens array is empty"})
		return
	}
	// 每个元素都会同步向上游发一次刷新请求 —— 上限防止一次请求放大成
	// 对 cline 的请求洪流
	if len(req.Tokens) > 500 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "too many tokens (max 500 per batch)"})
		return
	}

	imported := 0
	errors := []string{}

	for _, t := range req.Tokens {
		email := t.Email
		if email == "" {
			email = "batch_" + kit.RandHex(4)
		}
		// 静态 API key：直接入池，不做校验调用（与单个添加接口语义一致）
		if t.APIToken != "" {
			acc := &Account{
				AccountID: "acc_" + kit.RandHex(8),
				Email:     email,
				APIToken:  t.APIToken,
				Status:    "active",
				CreatedAt: time.Now(),
			}
			addAccount(acc)
			imported++
			continue
		}
		if t.RefreshToken == "" {
			continue
		}
		resp, err := cline.RefreshClineToken(t.RefreshToken)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.Email, err))
			continue
		}
		acc := &Account{
			AccountID:    "acc_" + kit.RandHex(8),
			Email:        email,
			RefreshToken: t.RefreshToken,
			AccessToken:  "workos:" + resp.Data.AccessToken,
			ExpiresAt:    clineExpiryMs(resp.Data.ExpiresAt),
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)
		imported++
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// POST /admin/api/accounts/refresh-all
func handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	accounts := append([]*Account(nil), p.Accounts...)
	poolMu.Unlock()
	refreshed, skipped, failed := 0, 0, 0
	for _, a := range accounts {
		// 静态 key（APIToken）没有刷新能力：doRefreshAccountToken 会直接把账号判为
		// expired。启动预热早已按这条规则跳过它们（见 proxy.go 的 prewarm），这里是
		// 同一规则——否则面板上点一次"刷新全部 token"就会作废所有 sk_ 账号。
		if a.APIToken != "" {
			skipped++
			continue
		}
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", a.Email, err)
			failed++
			continue
		}
		refreshed++
	}
	msg := fmt.Sprintf("Refreshed %d token(s)", refreshed)
	if skipped > 0 {
		msg += fmt.Sprintf(", skipped %d static api key account(s)", skipped)
	}
	if failed > 0 {
		msg += fmt.Sprintf(", %d failed", failed)
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: msg,
		Data:    map[string]any{"refreshed": refreshed, "skipped": skipped, "failed": failed},
	})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	// 只清空账号：Keys（/v1 的 API key）与 DefaultModel 必须保留，
	// 否则"删除全部账号"会顺手作废所有已分发的客户端 key
	old := loadPool()
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: old.Keys, DefaultModel: old.DefaultModel, CurrentIdx: 0}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All accounts deleted"})
}

// POST /admin/api/accounts/reset  body: { accountId }
// 检测限流并解除：向上游发送探测请求。若上游仍限流（429）则保持冷却，
// 重置无效；若探测成功则清除冷却、恢复正常状态，并重置今日统计。
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
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
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)

	if status == "active" {
		// 探测通过：解除冷却并重置今日统计
		resetTodayUsage(acc)
		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: "Check passed: upstream not rate-limited; cooldown lifted and today's stats reset",
			Data:    result,
		})
		return
	}

	// 仍限流/失效：保持冷却，重置无效
	msg := "Upstream still rate-limited; reset ineffective, staying in cooldown"
	if status == "expired" {
		msg = "Token expired; reset ineffective"
	} else if status == "error" {
		msg = "Probe error; try again later"
	}
	// 恢复时刻与剩余时长都不在此处拼接：面板按浏览器本地时区渲染
	// data.cooldownUntil（RFC3339；服务器格式化只会给出容器时区=UTC 的读数，
	// 同一时刻在面板里会显示成两个不同的钟点），remaining 由面板统一附加一次。
	writeAPI(w, http.StatusOK, apiResponse{
		Success: false,
		Message: msg,
		Data:    result,
	})
}

// POST /admin/api/accounts/test  body: { accountId }
// 用指定账号发送一个 max_tokens=1 的极小探测请求，验证该账号是否可用。
// 如果命中 429/INFERENCE_CAP_ERROR，自动标记冷却并返回预计恢复时间。
func handleAdminAccountTest(w http.ResponseWriter, r *http.Request) {
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
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)
	reason, _ := result["reason"].(string)
	log.Printf("Test account %s: status=%s reason=%s", truncateEmail(acc.Email), status, reason)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: status == "active",
		Message: status,
		Data:    result,
	})
}

// testAccount 对单个账号执行轻量探测请求，返回详细结果与最终状态。
// 测试按钮是"升级版重置"：无论账号当前是 active/cooldown/expired，
// 都会尝试刷新 Token 并发起一次真实探测；成功则清除所有异常状态。
// 返回的 status: active / cooldown / expired / error
func testAccount(acc *Account) (map[string]any, string) {
	// 状态快照必须在池锁内取：探测可能持续数秒，期间刷新协程/其他请求会改写
	// 同一结构体，锁外读是数据竞争。
	poolMu.Lock()
	prevStatus := acc.Status
	poolMu.Unlock()

	// 取 token（expired/cooldown 也尝试刷新，测试按钮不因状态直接拒绝）
	token, err := ensureAccountToken(acc)
	if err != nil {
		reason := "token refresh failed: " + err.Error()
		poolMu.Lock()
		acc.LastReason = reason
		acc.Status = "expired"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     reason,
			"prevStatus": prevStatus,
		}, "expired"
	}

	// 构造极小探测请求：max_tokens=1, 单条用户消息。探测请求需与正常代理请求
	// 使用相同的模型选择、流式策略和任务 ID，否则部分模型会返回空响应。
	probeModel := getDefaultModel()
	sessionID := fmt.Sprintf("test_%d", time.Now().UnixMilli())
	probeBody := map[string]any{
		"model":            probeModel,
		"max_tokens":       1,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	}
	if modelNeedsStream(probeModel) {
		probeBody["stream"] = true
	}
	bodyJSON, _ := json.Marshal(probeBody)

	req, err := http.NewRequest("POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    "error",
			"reason":    "build request: " + err.Error(),
		}, "error"
	}
	req.Header = clineHeaders(token, sessionID)

	resp, err := kit.HTTPClient.Do(req)
	if err != nil {
		// 网络错误：5 分钟短冷却（恢复时间取返回值，避免锁外读账号字段）
		reason := "network error: " + err.Error()
		until := markAccountCooldown(acc, reason, 5*time.Minute)
		return map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    "cooldown",
			"reason":    reason,
			// RFC3339（带时区）：面板用 new Date(...).toLocaleString() 渲染成浏览器本地时间；
			// 服务器格式化只能给出容器时区读数，同一时刻会显示成两个钟点
			"cooldownUntil": until.UTC().Format(time.RFC3339),
			"remaining":     formatDuration(time.Until(until)),
		}, "cooldown"
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	if resp.StatusCode == 429 {
		duration := parseInferenceCapDuration(bodyStr)
		if duration <= 0 {
			duration = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		reason := "429: " + kit.Truncate(bodyStr, 500)
		until := markAccountCooldown(acc, reason, duration)
		log.Printf("Test hit 429 on %s, cooldown %v", truncateEmail(acc.Email), duration)
		return map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    "cooldown",
			"reason":    reason,
			// RFC3339（带时区）：面板用 new Date(...).toLocaleString() 渲染成浏览器本地时间；
			// 服务器格式化只能给出容器时区读数，同一时刻会显示成两个钟点
			"cooldownUntil": until.UTC().Format(time.RFC3339),
			"remaining":     formatDuration(time.Until(until)),
			"httpStatus":    resp.StatusCode,
		}, "cooldown"
	}

	if resp.StatusCode == 401 {
		poolMu.Lock()
		acc.Status = "expired"
		acc.LastReason = "401 unauthorized"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     "401 unauthorized",
			"httpStatus": resp.StatusCode,
		}, "expired"
	}

	if resp.StatusCode != 200 {
		// 其它错误：不强制冷却，按一次失败处理
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "error",
			"reason":     fmt.Sprintf("API %d: %s", resp.StatusCode, kit.Truncate(bodyStr, 300)),
			"httpStatus": resp.StatusCode,
		}, "error"
	}

	// 成功：清除所有异常状态（冷却/过期/原因），并递增使用计数
	poolMu.Lock()
	acc.Status = "active"
	acc.LastReason = ""
	acc.CooldownUntil = time.Time{}
	poolMu.Unlock()
	bumpUsage(acc)
	return map[string]any{
		"accountId":  acc.AccountID,
		"email":      acc.Email,
		"status":     "active",
		"reason":     "ok",
		"httpStatus": resp.StatusCode,
		"prevStatus": prevStatus,
	}, "active"
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int(d / time.Second)
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs > 0 && days == 0 && hours == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

// Global proxy config (mutable via API)
var (
	proxyConfig   = defaultProxyConfig()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/3.0.50",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "3.0.50",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "3.0.50",
			"X-CORE-VERSION":     "0.0.70",
		},
	}
}

func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	proxyConfig = c
}

// validHeaderName 仅接受 token 字符（字母/数字/连字符），阻止头部注入。
func validHeaderName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validHeaderValue 拒绝 CR/LF 及其他控制字符（可见 ASCII 与空格/制表除外）。
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	// poolKeysSnapshot: 在池锁内取副本，避免与并发的 key 增删竞态
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": poolKeysSnapshot()}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	// key 只能来自 crypto/rand —— 时间戳生成的 key 可预测，
	// 而它把守着公网暴露的 /v1 接口
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "keygen failed"})
		return
	}
	key := "cline_" + hex.EncodeToString(b)
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
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
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	cfg := getProxyConfig()
	address := r.Host
	if address == "" {
		address = proxyListenAddress
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":         address,
		"strategy":        cfg.Strategy,
		"version":         "go-1.1",
		"poolPath":        poolPath,
		"defaultModel":    getDefaultModel(),
		"headers":         cfg.Headers,
		"clineUseProxies": poolClineUseProxies(),
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel?, clineUseProxies? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
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
		Strategy        string            `json:"strategy"`
		Headers         map[string]string `json:"headers"`
		DefaultModel    string            `json:"defaultModel"`
		ClineUseProxies *bool             `json:"clineUseProxies"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	// 写时复制：getProxyConfig 返回的是被请求路径并发读取的活配置，
	// 直接原地改 Strategy/Headers 会与读方竞态（concurrent map read/write
	// 直接 panic 进程）；先验证全部字段，再整体替换
	cfg := getProxyConfig()
	newCfg := &proxyConfigData{Strategy: cfg.Strategy, Headers: map[string]string{}}
	for k, v := range cfg.Headers {
		newCfg.Headers[k] = v
	}
	changed := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			newCfg.Strategy = req.Strategy
			changed = true
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.Headers != nil {
		for k, v := range req.Headers {
			// 自定义头最终会被 Set 到上游请求上：注入 CR/LF 或控制字符
			// 可能造成请求走私，格式不合法直接拒绝
			if !validHeaderName(k) || !validHeaderValue(v) {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: fmt.Sprintf("invalid header %q: name allows letters/digits/hyphen, value must not contain control characters", k)})
				return
			}
			newCfg.Headers[k] = v
		}
		changed = true
	}

	if req.DefaultModel != "" {
		initModelsCache()
		modelsMu.Lock()
		_, ok := modelsCache[req.DefaultModel]
		modelsMu.Unlock()
		if !ok {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "unknown model: " + req.DefaultModel})
			return
		}
		setDefaultModel(req.DefaultModel)
		changed = true
	}

	// cline 走共享出口代理池的开关：持久化到池文件，重启后保留。
	// CLINE_USE_PROXIES env 为 true 时 env 优先（见 clineProxiesEnabled）。
	if req.ClineUseProxies != nil {
		p := loadPool()
		poolMu.Lock()
		p.ClineUseProxies = *req.ClineUseProxies
		poolMu.Unlock()
		savePool()
		changed = true
	}

	if changed {
		setProxyConfig(newCfg)
		cfg = newCfg
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":     cfg.Strategy,
		"headers":      cfg.Headers,
		"defaultModel": defaultModel,
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	ensureModelsFresh()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models":   getFreeModels(),
		"lastSync": modelsLastSync,
	}})
}

// POST /admin/api/models/refresh
func handleAdminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initModelsCache()
	modelsMu.Lock()
	syncing := modelsSyncing
	modelsMu.Unlock()
	if syncing {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "sync already running"})
		return
	}
	go syncModelsOnce()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model sync started"})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	total, active, cooldown, expired := poolStatusCounts()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    total,
			"active":   active,
			"cooldown": cooldown,
			"expired":  expired,
			"strategy": getProxyConfig().Strategy,
			"version":  "go-1.1",
		},
	})
}

// GET /admin/api/accounts/export 导出全部账号 refreshToken（JSON 文件下载）
func handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	// 池锁内构建导出项：Accounts 元素字段的并发读写由 poolMu 保护
	poolMu.Lock()
	items := make([]map[string]any, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		item := map[string]any{
			"email": a.Email,
		}
		// 两类账号都能完整导出：OAuth 账号带 refreshToken（可直接回导入），
		// 静态 key 账号带 apiToken —— 导入端两者都认
		if a.APIToken != "" {
			item["apiToken"] = a.APIToken
		} else {
			item["refreshToken"] = a.RefreshToken
		}
		items = append(items, item)
	}
	poolMu.Unlock()
	data, _ := json.MarshalIndent(items, "", "  ")
	// 凭证响应绝不允许被中间层缓存；nosniff 防止 MIME 嗅探误判
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cline-accounts-export.json"`)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /admin/api/logs 最近请求日志（对话/调用历史）
func handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	logs := LoadRequestLogs()
	if logs == nil {
		logs = []RequestLog{}
	}
	// 倒序返回（最新在前）
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"logs": logs,
		},
	})
}
