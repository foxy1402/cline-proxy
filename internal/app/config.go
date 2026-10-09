package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
)

// 环境变量配置层。优先级：显式命令行 flag > 环境变量 > 内置默认值。
//
// 支持的环境变量：
//
//	PORT                       监听端口（main.go 中显式 flag 优先）
//	DATA_DIR                   数据目录（账号池/zen 配置/日志/combos 等，kit.ResolveDataPath）
//	API_KEY                    /v1 上游代理接口的固定 API key；设置后仅此 key 可调用 /v1/*
//	API_KEY_FILE               API key 文件路径（docker secrets），优先于 API_KEY
//	MAX_BODY_MB                单请求体上限 MB，默认 32，超出返回 413
//	ADMIN_PASSWORD             管理面板登录密码；设置后所有 /admin/* 需登录
//	ADMIN_PASSWORD_FILE        密码文件路径（docker secrets），优先于 ADMIN_PASSWORD
//	REQUIRE_ADMIN_AUTH         "false" 时允许公网无密码运行（如反代已做认证），默认强制
//	POOL_STRATEGY              账号池策略 round_robin(默认) / fill / random，覆盖持久化配置
//	LOG_REQUESTS               请求日志开关，默认 true；"false" 完全关闭（含 body 探测）
//	LOG_FILE_MAX_MB            requests.jsonl 大写上限 MB，默认 10，超出清空
//	APPLY_SYSTEM_PROMPT_OVERRIDE  "true" 才启用 override.md 系统提示词替换，默认关闭
//	STREAM_LOG                 "true" 把 Anthropic 流式原始 SSE 落盘（完整对话，慎用）
//	STRICT_MODEL_MATCH         "false" 时未知模型静默回退默认模型（默认显式 400）
//	CLIENT_IP_HEADER           反代部署下取真实客户端 IP 的请求头名（登录限流用）
//	ZEN_KEYS                   opencode zen 多 key，逗号分隔，配置为空时注入
//	ZEN_PIN_KEY                zen key 池固定用第 n 个 key（1 起），排障/单 key 直测用
//	CLINE_ACCOUNTS_SEED_FILE   cline 账号种子文件（[{refreshToken,email}] JSON 数组），
//	                           池为空时启动自动导入
//	CLINE_USE_PROXIES          true 时 cline 上游全部走出口代理池（zen 上游配置
//	                           proxies 后默认走池；combo 也可按别名单独开启）

// envStr 读取环境变量并去除首尾空白，未设置或为空返回 ""。
func envStr(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// envBool 解析布尔环境变量：1/true/yes/on（大小写不敏感）为 true，
// 0/false/no/off 为 false。未设置或无法识别的值返回 (false, false)，
// 调用方回落到各自默认值 —— 拼写错误不会意外关闭安全开关（fail closed）。
func envBool(key string) (bool, bool) {
	v := strings.ToLower(envStr(key))
	if v == "" {
		return false, false
	}
	switch v {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	fmt.Printf("  WARNING: unrecognized boolean %q for %s, using default\n", v, key)
	return false, false
}

// envInt 解析整数环境变量，无效或未设置返回 (0, false)。
func envInt(key string) (int, bool) {
	v := envStr(key)
	if v == "" {
		return 0, false
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// envList 解析逗号分隔的环境变量为去空白、去空项的列表。
func envList(key string) []string {
	raw := envStr(key)
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// APIKeyEnv 返回 /v1 接口的固定 API key：API_KEY_FILE（docker secrets，
// 整个文件内容去除首尾空白）优先，其次 API_KEY。均未设置返回 ""。
// 文件已配置却不可读/为空时返回不可匹配的哨兵（fail closed，见 readSecretFileEnv）。
func APIKeyEnv() string {
	if v, ok := readSecretFileEnv("API_KEY_FILE", "/v1 API key"); ok {
		return v
	}
	return envStr("API_KEY")
}

// sealedSecretValue 哨兵：secret 文件已配置但读不到（权限变更、secret 轮换、
// 挂载丢失）或内容为空时，用它代替"空值"，让依赖"空=未配置"的判定全部失效为
// fail-closed 而不是 fail-open。
//
// 两处失效点（都只在启动时检查过一次，运行中失效不会被发现）：
//   - ADMIN_PASSWORD_FILE 读不到 → 密码为空 → AdminAuthRequired()=false →
//     公网面板静默免认证。
//   - API_KEY_FILE 读不到 → key 为空 → /v1 回落到"动态 key 列表为空则放行" →
//     公网 /v1 静默免认证。
//
// 每进程随机而非固定串：固定串等于一个公开密码，知道这个常量就能在读不到文件时
// 照样通过校验。随机值没有任何来源可提交，因此请求一律 401/拒绝。
var sealedSecretValue = func() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 熵源不可用属极端情况：退化为含 NUL 的值（表单与 JSON 都无法承载），
		// 而不是一个可被猜测的普通字符串
		return "\x00sealed-secret\x00"
	}
	return hex.EncodeToString(b)
}()

var (
	sealedLogMu     sync.Mutex
	sealedLoggedFor = map[string]bool{}
)

// logSealedOnce 同一 secret 只记一次日志：AdminPasswordEnv 每个管理请求都会重新
// 读文件，挂载损坏时按请求刷屏没有意义。
func logSealedOnce(what, detail string) {
	sealedLogMu.Lock()
	first := !sealedLoggedFor[detail]
	sealedLoggedFor[detail] = true
	sealedLogMu.Unlock()
	if first {
		log.Printf("secret file: %s (%s); refusing to treat it as \"not configured\" — "+
			"requests needing it stay rejected until the file is readable again", detail, what)
	}
}

// readSecretFileEnv 读取 fileEnvVar 指向的 secret 文件。
// ok=false 表示该 FILE 变量未配置，调用方回退到普通环境变量；
// ok=true 时返回值一定非空（要么是文件内容，要么是不可匹配的哨兵）。
func readSecretFileEnv(fileEnvVar, what string) (string, bool) {
	path := envStr(fileEnvVar)
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logSealedOnce(what, fmt.Sprintf("%s=%s unreadable: %v", fileEnvVar, path, err))
		return sealedSecretValue, true
	}
	if v := strings.TrimSpace(string(data)); v != "" {
		return v, true
	}
	logSealedOnce(what, fmt.Sprintf("%s=%s is empty", fileEnvVar, path))
	return sealedSecretValue, true
}

// AdminPasswordEnv 解析管理密码：ADMIN_PASSWORD_FILE（docker secrets，整个文件
// 内容去除首尾空白）优先，其次 ADMIN_PASSWORD。均未设置返回 ""。
// 文件已配置却不可读/为空时返回不可匹配的哨兵（fail closed，见 readSecretFileEnv）。
func AdminPasswordEnv() string {
	if v, ok := readSecretFileEnv("ADMIN_PASSWORD_FILE", "admin panel password"); ok {
		return v
	}
	return envStr("ADMIN_PASSWORD")
}

// ApplyEnvConfig 在包初始化后、启动前应用环境变量到内存配置。
// 由 StartProxy 在最早期调用。
func ApplyEnvConfig() {
	// POOL_STRATEGY 覆盖内存中的账号池策略（该配置本身不落盘，重启即回到默认）
	if s := envStr("POOL_STRATEGY"); s != "" {
		switch s {
		case "round_robin", "fill", "random":
			proxyConfigMu.Lock()
			proxyConfig.Strategy = s
			proxyConfigMu.Unlock()
		default:
			fmt.Printf("  WARNING: invalid POOL_STRATEGY %q, using round_robin\n", s)
		}
	}
}

// isLoopbackHost 判断监听 host 是否仅本机可达。
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// CheckPublicExposure 公网暴露安全检查（fail closed）：
// 监听非回环地址时必须设置 API_KEY 与 ADMIN_PASSWORD，否则拒绝启动。
// REQUIRE_ADMIN_AUTH=false 可显式豁免管理密码检查（例如反代已做认证）。
func CheckPublicExposure(host string) error {
	if isLoopbackHost(host) {
		return nil
	}
	if APIKeyEnv() == "" {
		return fmt.Errorf("refusing to start: listening on %q (public) without API_KEY; "+
			"set API_KEY env so only your key can call /v1, or bind -host 127.0.0.1 for local use", host)
	}
	if AdminPasswordEnv() == "" && !adminAuthExplicitlyDisabled() {
		return fmt.Errorf("refusing to start: listening on %q (public) without ADMIN_PASSWORD; "+
			"set ADMIN_PASSWORD env, or REQUIRE_ADMIN_AUTH=false if a reverse proxy handles auth, "+
			"or bind -host 127.0.0.1 for local use", host)
	}
	return nil
}

func adminAuthExplicitlyDisabled() bool {
	v, ok := envBool("REQUIRE_ADMIN_AUTH")
	return ok && !v
}

// AdminAuthRequired 判断管理面板是否需要登录认证：
// 设置了 ADMIN_PASSWORD(_FILE) 即启用；未设置时仅本机访问不启用。
func AdminAuthRequired() bool {
	return AdminPasswordEnv() != ""
}

// LogRequestsEnabled LOG_REQUESTS 是否启用，默认 true。
func LogRequestsEnabled() bool {
	v, ok := envBool("LOG_REQUESTS")
	if !ok {
		return true
	}
	return v
}

// LogFileMaxBytes requests.jsonl 落盘文件大小上限，默认 10MB。
func LogFileMaxBytes() int64 {
	if n, ok := envInt("LOG_FILE_MAX_MB"); ok && n > 0 {
		return int64(n) << 20
	}
	return 10 << 20
}

// MaxRequestBodyBytes 单个请求体的字节上限（MAX_BODY_MB，默认 32MB），
// 在日志中间件以 http.MaxBytesReader 包裹，防止公网匿名超大 body 撑爆内存。
func MaxRequestBodyBytes() int64 {
	if n, ok := envInt("MAX_BODY_MB"); ok && n > 0 {
		return int64(n) << 20
	}
	return 32 << 20
}

// SystemPromptOverrideEnabled APPLY_SYSTEM_PROMPT_OVERRIDE 是否启用 override.md
// 系统提示词替换，默认 false（编码 IDE / Agent 保留自己的提示词）。
func SystemPromptOverrideEnabled() bool {
	v, _ := envBool("APPLY_SYSTEM_PROMPT_OVERRIDE")
	return v
}

// StreamLogEnabled STREAM_LOG=true 时把 Anthropic 流式路径的原始 SSE 事件
// 落盘到 cline-proxy-stream.log（完整对话内容、无大小上限），默认关闭。
func StreamLogEnabled() bool {
	v, _ := envBool("STREAM_LOG")
	return v
}

// ClineUseProxiesEnv CLINE_USE_PROXIES=true 时 cline 上游全部走出口代理池
// （默认 false；cline 上游也可通过 combo 的 useProxies 开关按别名启用）。
func ClineUseProxiesEnv() bool {
	v, _ := envBool("CLINE_USE_PROXIES")
	return v
}

// clineProxiesEnabled cline 上游是否走共享出口代理池：
// 管理面板的持久化开关或 CLINE_USE_PROXIES env 任一开启即启用（env 优先）。
// 代理列表本身由 zen 配置的 proxies 提供 —— 两个上游共用同一个池。
func clineProxiesEnabled() bool {
	return ClineUseProxiesEnv() || poolClineUseProxies()
}

// StrictModelMatchEnv STRICT_MODEL_MATCH 控制未知模型名的处理（默认 true）：
// 开启时对既不在免费模型表也不是 zen 模型的名字返回 400；关闭时沿用旧行为，
// 静默回退到默认模型 —— IDE 配错模型名时会拿到"另一个模型"的 200 响应，
// 排查成本极高，因此默认显式报错。
func StrictModelMatchEnv() bool {
	v, ok := envBool("STRICT_MODEL_MATCH")
	if ok {
		return v
	}
	return true
}
