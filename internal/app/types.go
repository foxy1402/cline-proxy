package app

import "time"

type Account struct {
	AccountID    string `json:"accountId"`
	Email        string `json:"email"`
	RefreshToken string `json:"refreshToken"`
	// APIToken 静态 API key（sk_...）账号：设置后直接作为 Bearer 使用，
	// 不刷新（无 refresh token）；json 序列化持久化，重启后仍生效。
	// 优先级高于 RefreshToken/AccessToken。
	APIToken        string    `json:"apiToken,omitempty"`
	AccessToken     string    `json:"-"`
	ExpiresAt       int64     `json:"-"`
	Status          string    `json:"status"` // active, cooldown, expired
	LastUsed        time.Time `json:"lastUsed"`
	UsageCount      int64     `json:"usageCount"`      // 本地累计成功调用次数
	UsageCountToday int64     `json:"usageCountToday"` // 本地今日成功调用次数
	UsageDate       string    `json:"usageDate"`       // 本地计数日期 YYYY-MM-DD，跨日自动重置
	TokensTotal     int64     `json:"tokensTotal"`     // 本地累计 token 消耗（prompt+completion）
	TokensToday     int64     `json:"tokensToday"`     // 本地今日 token 消耗
	TokensDate      string    `json:"tokensDate"`      // 今日 token 计数日期 YYYY-MM-DD，跨日自动重置
	CreatedAt       time.Time `json:"createdAt"`
	CooldownUntil   time.Time `json:"cooldownUntil,omitempty"` // 预计冷却结束时间
	LastReason      string    `json:"lastReason,omitempty"`    // 最后一次进入冷却/失效的原因
	// EgressProxy / NoEgress 仅供管理面板展示：该账号绑定的出口（已打码），
	// 或"代理池已用尽、本账号在代理模式下被跳过"。绑定本身持久化在
	// .egress-bindings.json，不进账号池文件。
	EgressProxy string `json:"egressProxy,omitempty"`
	NoEgress    bool   `json:"noEgress,omitempty"`
}

type AccountPool struct {
	Accounts        []*Account `json:"accounts"`
	CurrentIdx      int        `json:"currentIdx"`
	Keys            []string   `json:"keys,omitempty"`
	DefaultModel    string     `json:"defaultModel,omitempty"` // 用户自定义默认模型，持久化
	ClineUseProxies bool       `json:"clineUseProxies,omitempty"` // cline 上游走共享出口代理池（面板开关；CLINE_USE_PROXIES env 为 true 时强制开启）
}

type LoginMethod int

const (
	MethodDeviceOAuth LoginMethod = iota
	MethodRefreshToken
	MethodSSOCookie
)
