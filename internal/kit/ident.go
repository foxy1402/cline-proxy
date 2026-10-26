package kit

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func WithRetryJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	// 在退避时间上增加 0%~25% 抖动，避免多个请求同时重试。
	jitter := time.Duration(float64(delay) * float64(RandIntn(26)) / 100)
	const maxDuration = time.Duration(1<<63 - 1)
	if delay > maxDuration-jitter {
		return maxDuration
	}
	return delay + jitter
}

// ============================================================================
// 客户端身份轮换: 模拟多个独立 opencode 客户端
// 源码中 x-opencode-session / x-opencode-request / User-Agent 均为客户端随机生成,
// 服务端可能按这些标识记账(实测 "Worker local total request limit reached" 疑似 session 级)。
// 每次请求生成全新身份 = 每次都是"新客户端",从源头规避身份维度的限流。
// ============================================================================

var ZenUserAgents = []string{
	// 2026-10-26 实测：CLI v2.1.4 只发这一个 UA。旧版本轮换会让同一
	// key/会话在不同请求间换 UA 版本，正是上游识别非官方客户端的
	// 信号之一；因此不再轮换旧 UA，只保留 v2 UA。
	"opencode/latest/2.1.4/cli",
}

// mustRand 读满 n 字节加密随机数；crypto/rand 失败说明系统熵源异常，
// 此时身份/防抖动全部失效，宁可 panic 也不静默使用全零字节。
func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
}

func RandHex(n int) string {
	b := make([]byte, n)
	mustRand(b)
	return hex.EncodeToString(b)
}

func RandIntn(n int) int {
	if n <= 0 {
		return 0
	}
	b := make([]byte, 4)
	mustRand(b)
	v := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if v < 0 {
		v = -v
	}
	return v % n
}

// FreshZenIdentity 生成一组全新客户端身份 (session, request, user-agent)。
// session 走本地铸造（MintZenSessionID），与粘性路径同款合法格式
// ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$ —— 匿名路径命中同一道免费门，旧实现
// 的 "sess_"+26 随机串格式不符，必然 403 FreeTierError。
// request 走本地铸造（MintZenRequestID）——2026-10-15 实测新后端对
// x-opencode-request 同样执行 ^msg_[0-9a-f]{12} 格式门，纯随机串必 403。
// UA 仍每次轮换（匿名路径本就模拟多客户端）。
func FreshZenIdentity() (string, string, string) {
	return MintZenSessionID(),
		MintZenRequestID(),
		ZenUserAgents[RandIntn(len(ZenUserAgents))]
}

// RandAlphaNum 生成 n 个大小写字母+数字（匹配官方 CLI 的 sess_/msg_ ID 字符集）。
func RandAlphaNum(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	mustRand(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}
