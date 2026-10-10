package app

import (
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

var (
	pool       *AccountPool
	poolMu     sync.Mutex
	poolSaveMu sync.Mutex
	poolPath   string
	poolDirty  bool // 热路径变更标记，由后台 flusher 周期落盘
)

func init() {
	poolPath = kit.ResolveDataPath(".cline-accounts.json")
}

// markPoolDirtyLocked 标记池有待落盘变更（调用方必须持有 poolMu）。
// 热路径（选号/计数）不再每次全量写盘 —— poolMu 是全局锁，磁盘 I/O
// 会把所有在途请求串行化；后台 flusher 每 2s 补一次写。
func markPoolDirtyLocked() {
	poolDirty = true
}

// startPoolFlusher 启动池文件后台落盘循环；进程退出由 flushPoolNow 兜底。
func startPoolFlusher() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			poolMu.Lock()
			dirty := poolDirty
			poolDirty = false
			poolMu.Unlock()
			if dirty {
				savePool()
			}
		}
	}()
}

// flushPoolNow 立即落盘（优雅停机时调用，防止丢最后 2s 的状态变更）。
func flushPoolNow() {
	poolMu.Lock()
	dirty := poolDirty
	poolDirty = false
	poolMu.Unlock()
	if dirty {
		savePool()
	}
}

// kit.ResolveDataPath 数据文件路径解析：优先可执行文件目录，其次当前工作目录。
// go run 运行时编译产物在临时目录，此时应回退到工作目录（项目根）查找数据文件。

func loadPool() *AccountPool {
	poolMu.Lock()
	defer poolMu.Unlock()

	if pool != nil {
		return pool
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		// 损坏的池文件绝不能被空池静默覆盖 —— 先把原文件改名留档，
		// 否则一次崩溃中途写入就会永久丢失全部账号凭证
		stamp := time.Now().Format("20060102-150405")
		if renErr := os.Rename(poolPath, poolPath+".corrupt-"+stamp); renErr == nil {
			log.Printf("SEVERE: account pool file is corrupt JSON; moved to %s.corrupt-%s for manual recovery; starting with an EMPTY pool", poolPath, stamp)
		} else {
			log.Printf("SEVERE: account pool file is corrupt JSON (quarantine rename failed: %v); starting with an EMPTY pool", renErr)
		}
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	pool = &p
	if pool.DefaultModel != "" {
		defaultModel = pool.DefaultModel
	}
	return pool
}

// setDefaultModel 持久化默认模型：更新内存全局并写入账号池文件
func setDefaultModel(modelID string) {
	initModelsCache()
	modelsMu.Lock()
	_, ok := modelsCache[modelID]
	modelsMu.Unlock()
	if !ok {
		return
	}
	defaultModel = modelID
	p := loadPool()
	poolMu.Lock()
	p.DefaultModel = modelID
	poolMu.Unlock()
	savePool()
}

// poolAccountCount 线程安全地返回账号池大小。
func poolAccountCount() int {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return len(p.Accounts)
}

// poolStatusSnapshot 线程安全地返回 (账号总数, 活跃数) 快照。
// Status/Accounts 由池锁保护，任何请求路径的读取都必须走这里。
func poolStatusSnapshot() (total, active int) {
	total, active, _, _ = poolStatusCounts()
	return total, active
}

// poolStatusCounts 线程安全地返回 (总数, active, cooldown, expired) 快照。
func poolStatusCounts() (total, active, cooldown, expired int) {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	for _, a := range p.Accounts {
		total++
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}
	return
}

// poolCurrentIdx 线程安全地读取账号轮转游标。
func poolCurrentIdx() int {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return p.CurrentIdx
}

// poolClineUseProxies 线程安全地读取 cline 走代理池开关。
func poolClineUseProxies() bool {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return p.ClineUseProxies
}

// poolKeysSnapshot 线程安全地返回客户端 API key 列表副本。
func poolKeysSnapshot() []string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return append([]string(nil), p.Keys...)
}

func savePool() {
	poolMu.Lock()
	defer poolMu.Unlock()
	savePoolLocked()
}

// savePoolLocked 持久化账号池；调用方必须已经持有 poolMu。
// 原子写: 先写临时文件再 rename，崩溃中途写入不会留下截断的 JSON。
func savePoolLocked() {
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()

	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal accounts: %v", err)
		return
	}
	tmp := poolPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("Failed to save accounts: %v", err)
		return
	}
	if err := os.Rename(tmp, poolPath); err != nil {
		log.Printf("Failed to save accounts (rename): %v", err)
	}
}

func addAccount(acc *Account) {
	p := loadPool()
	poolMu.Lock()
	p.Accounts = append(p.Accounts, acc)
	poolMu.Unlock()
	savePool()
}

func removeAccount(accountID string) bool {
	p := loadPool()
	poolMu.Lock()

	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			savePoolLocked()
			poolMu.Unlock()
			return true
		}
	}
	poolMu.Unlock()
	return false
}

func getAccountByID(accountID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if a.AccountID == accountID {
			return a
		}
	}
	return nil
}

// refreshAccountToken 刷新账号 access token。
// 单飞（single-flight）：同一账号的并发 401 只发一次刷新请求 ——
// cline 的 refresh token 是轮换型，两个并发刷新会让后到者拿旧 token
// 换新失败，进而把还活着的账号误标为 expired。
func refreshAccountToken(acc *Account) error {
	refreshFlightMu.Lock()
	if c, ok := refreshFlight[acc]; ok {
		refreshFlightMu.Unlock()
		<-c.done
		return c.err
	}
	c := &refreshFlightCall{done: make(chan struct{})}
	refreshFlight[acc] = c
	refreshFlightMu.Unlock()

	// panic 安全：中途 panic 也要唤醒等待者，否则它们永久阻塞在 <-c.done 上。
	// close 必须在"从表里删除"之前完成：先删后 close 会开出一个窗口，期间的
	// 到达者看不到在飞的调用，于是再发一次刷新 —— 而 cline 的 refresh token
	// 是轮换型的，第二次刷新必然用旧 token 失败，把活着的账号误标 expired。
	func() {
		defer close(c.done)
		c.err = doRefreshAccountToken(acc)
	}()

	refreshFlightMu.Lock()
	delete(refreshFlight, acc)
	refreshFlightMu.Unlock()
	return c.err
}

type refreshFlightCall struct {
	done chan struct{}
	err  error
}

var (
	refreshFlightMu sync.Mutex
	refreshFlight   = map[*Account]*refreshFlightCall{}
)

func doRefreshAccountToken(acc *Account) error {
	if acc.APIToken != "" {
		// 静态 key 无刷新能力 —— 只有在 key 被上游拒绝（401）后才会走到这里，
		// 直接判失效，不再发无意义的刷新请求
		poolMu.Lock()
		acc.Status = "expired"
		acc.LastReason = "api key rejected upstream (static key cannot refresh)"
		savePoolLocked()
		poolMu.Unlock()
		return fmt.Errorf("static api key account cannot refresh")
	}
	resp, err := cline.RefreshClineToken(acc.RefreshToken)
	if err != nil {
		poolMu.Lock()
		if cline.IsAuthRejection(err) {
			// 上游明确拒绝该 refresh token（400/401/403）—— 账号确实失效
			acc.Status = "expired"
			acc.LastReason = "refresh rejected: " + err.Error()
		} else {
			// 网络故障 / 5xx 是瞬时的: 短冷却 5 分钟,不判死账号
			acc.Status = "cooldown"
			acc.CooldownUntil = time.Now().Add(5 * time.Minute)
			acc.LastReason = "refresh transient error: " + err.Error()
		}
		savePoolLocked()
		poolMu.Unlock()
		return fmt.Errorf("token refresh failed: %w", err)
	}
	if resp.Data.AccessToken == "" {
		// 200 但空 token: 上游响应异常,按瞬时故障处理
		markAccountCooldown(acc, "refresh returned empty access token", 5*time.Minute)
		return fmt.Errorf("token refresh returned empty access token")
	}

	poolMu.Lock()
	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = clineExpiryMs(resp.Data.ExpiresAt)
	acc.Status = "active"
	acc.LastReason = ""
	savePoolLocked()
	poolMu.Unlock()
	return nil
}

// clineExpiryMs 解析过期时间并预留 60s 提前量。ParseExpiry 解析失败返回 0，
// 直接使用会得到 ExpiresAt=-60000 → 每个请求都触发刷新（刷新风暴 + 每请求
// 落盘一次）；此时回退到保守的 55 分钟（cline token 生命周期约 1h）。
func clineExpiryMs(expiresAt any) int64 {
	exp := cline.ParseExpiry(expiresAt)
	if exp <= 0 {
		return time.Now().UnixMilli() + 55*60_000
	}
	return exp - 60_000
}

// maxCooldown 冷却时长上限：上游给的间隔（Retry-After / 错误文本解析）
// 可能是离谱的大值，封顶 24h，防止把账号/key/代理冷却到事实上永久下线。
const maxCooldown = 24 * time.Hour

func pickAccount() *Account {
	p := loadPool()
	poolMu.Lock()

	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		// 自动解除已到期的冷却
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && time.Now().After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		if a.Status == "active" {
			active = append(active, a)
		}
	}

	if len(active) == 0 {
		poolMu.Unlock()
		return nil
	}

	cfg := getProxyConfig()

	var acc *Account
	switch cfg.Strategy {
	case "fill":
		// Always pick the first available (fill)
		acc = active[0]
	case "random":
		// Random selection
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default: // round_robin
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}

	markPoolDirtyLocked()
	poolMu.Unlock()
	return acc
}

// pickAccountExcluding 取一个 active 账号,跳过 exclude 中的账号 ID（并跳过
// 无凭证账号）。供 callClineAPI 的跨账号 failover 使用: 429/5xx 后换一个
// 不同的账号重试,而不是把整个请求判死。
func pickAccountExcluding(exclude map[string]bool) *Account {
	p := loadPool()
	poolMu.Lock()

	active := make([]*Account, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && time.Now().After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		if a.Status != "active" {
			continue
		}
		if exclude[a.AccountID] {
			continue
		}
		if a.APIToken == "" && a.RefreshToken == "" {
			continue
		}
		active = append(active, a)
	}

	if len(active) == 0 {
		poolMu.Unlock()
		return nil
	}

	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = active[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default: // round_robin
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}

	markPoolDirtyLocked()
	poolMu.Unlock()
	return acc
}

// pickAccountAny 取任意一个仍持有凭证的账号（含冷却中的），供与推理配额
// 无关的只读调用使用（如 /ai/cline/recommended-models 免费模型列表）。
// 推理额度耗尽（429→cooldown）不影响这类接口：实测 4 个账号全部 429 时
// 模型列表接口仍 200；只走 pickAccount() 会因 "no active accounts" 整轮
// 不同步，模型列表于是只剩种子兜底，已下线的模型会一直挂着。
func pickAccountAny() *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	var fallback *Account
	for _, a := range p.Accounts {
		if a.APIToken == "" && a.RefreshToken == "" {
			continue
		}
		if a.Status == "active" {
			return a
		}
		if fallback == nil {
			fallback = a
		}
	}
	return fallback
}

func ensureAccountToken(acc *Account) (string, error) {
	// 静态 API key 账号：token 即凭证，永不过期、永不刷新
	if acc.APIToken != "" {
		return acc.APIToken, nil
	}
	// 快照读: AccessToken/ExpiresAt 由刷新协程在 poolMu 下写入
	poolMu.Lock()
	tok := acc.AccessToken
	exp := acc.ExpiresAt
	poolMu.Unlock()
	if tok != "" && time.Now().UnixMilli() < exp {
		return tok, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	poolMu.Lock()
	tok = acc.AccessToken
	poolMu.Unlock()
	return tok, nil
}

func ListAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()

	// 自动解除已到期的冷却，确保返回的列表是最新状态
	usageDate := time.Now().Format("2006-01-02")
	for _, a := range p.Accounts {
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && time.Now().After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		if a.UsageDate != usageDate {
			a.UsageDate = usageDate
			a.UsageCountToday = 0
		}
		if a.TokensDate != usageDate {
			a.TokensDate = usageDate
			a.TokensToday = 0
		}
	}
	result := make([]*Account, len(p.Accounts))
	for i, a := range p.Accounts {
		// Don't expose tokens
		result[i] = &Account{
			AccountID:       a.AccountID,
			Email:           a.Email,
			Status:          a.Status,
			LastUsed:        a.LastUsed,
			UsageCount:      a.UsageCount,
			UsageCountToday: a.UsageCountToday,
			UsageDate:       a.UsageDate,
			TokensTotal:     a.TokensTotal,
			TokensToday:     a.TokensToday,
			TokensDate:      a.TokensDate,
			CreatedAt:       a.CreatedAt,
			CooldownUntil:   a.CooldownUntil,
			LastReason:      a.LastReason,
		}
	}
	markPoolDirtyLocked()
	poolMu.Unlock()
	return result
}

// markAccountCooldown 将账号置为冷却状态，并记录预计恢复时间。
// duration 为冷却时长；duration<=0 时使用默认冷却。
// 返回设置的恢复时间（调用方用它代替锁外读取 acc.CooldownUntil）。
func markAccountCooldown(acc *Account, reason string, duration time.Duration) time.Time {
	if acc == nil {
		return time.Time{}
	}
	if duration <= 0 {
		duration = 18 * time.Hour // 默认 18 小时（Cline 免费额度每日重置）
	}
	if duration > maxCooldown {
		duration = maxCooldown
	}
	poolMu.Lock()
	acc.Status = "cooldown"
	acc.CooldownUntil = time.Now().Add(duration)
	acc.LastReason = reason
	savePoolLocked()
	until := acc.CooldownUntil
	poolMu.Unlock()
	return until
}

// bumpUsage 递增本地成功调用计数（含今日计数），自动处理跨日重置。
func bumpUsage(acc *Account) {
	if acc == nil {
		return
	}

	poolMu.Lock()
	now := time.Now()
	today := now.Format("2006-01-02")
	if acc.UsageDate != today {
		acc.UsageDate = today
		acc.UsageCountToday = 0
	}
	acc.UsageCountToday++
	acc.UsageCount++
	acc.LastUsed = now
	markPoolDirtyLocked()
	poolMu.Unlock()
}

// resetTodayUsage 仅重置本地今日调用计数，不影响累计调用次数。
func resetTodayUsage(acc *Account) {
	if acc == nil {
		return
	}

	poolMu.Lock()
	acc.UsageDate = time.Now().Format("2006-01-02")
	acc.UsageCountToday = 0
	acc.TokensDate = time.Now().Format("2006-01-02")
	acc.TokensToday = 0
	savePoolLocked()
	poolMu.Unlock()
}

// recordAccountTokens 记录账号本次请求消耗的 token（prompt+completion），
// 自动处理跨日重置，累计值不重置。tokens<=0 时忽略。
func recordAccountTokens(acc *Account, tokens int64) {
	if acc == nil || tokens <= 0 {
		return
	}

	poolMu.Lock()
	today := time.Now().Format("2006-01-02")
	if acc.TokensDate != today {
		acc.TokensDate = today
		acc.TokensToday = 0
	}
	acc.TokensToday += tokens
	acc.TokensTotal += tokens
	markPoolDirtyLocked()
	poolMu.Unlock()
}

// describePoolStatus 汇总当前账号池状态，用于错误诊断。
func describePoolStatus() string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	total := len(p.Accounts)
	if total == 0 {
		return "pool is empty, use --add-account or admin API to add accounts"
	}

	active, cooldown, expired := 0, 0, 0
	var nextRecover *time.Time
	for _, a := range p.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
			if !a.CooldownUntil.IsZero() {
				if nextRecover == nil || a.CooldownUntil.Before(*nextRecover) {
					t := a.CooldownUntil
					nextRecover = &t
				}
			}
		case "expired":
			expired++
		}
	}

	s := fmt.Sprintf("total=%d active=%d cooldown=%d expired=%d", total, active, cooldown, expired)
	if cooldown > 0 && nextRecover != nil {
		s += fmt.Sprintf(", earliest recover at %s", nextRecover.Format("2006-01-02 15:04:05"))
	}
	return s
}

func AddAccountFromDeviceAuth() (*Account, error) {
	fmt.Println("\n=== Add New Cline Account (OAuth) ===")

	device, err := cline.WorkosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Println("  3. Log in with Google, GitHub, or email")

	_ = cline.OpenBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

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
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if reg.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
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
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}
