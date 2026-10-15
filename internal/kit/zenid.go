package kit

import (
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Zen 会话 ID 本地铸造（替代 CLI 收割机）
//
// 2026-10-09 实测（直连栈：bunSpecForConn TLS 指纹 + CLI 头）：zen 免费层
// 的会话门是无状态的格式检查 —— 只要 ID 匹配 ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$
// 就放行（本地铸造的合法格式 ID 三次通过，含复用；sess_ 占位 403）。
// 服务端既不检查"见过此 ID"，也不检查时间戳新鲜度。
//
// 铸造算法移植自 opencode SessionID.create() 的 descending 变体：
// "ses_" + 12 个小写 hex（位反转的 (ts_ms<<12|counter) 低 48 位）+ 14 个
// crypto/rand base62 字符。时间位反转使后铸的 ID 字典序排在先铸的之前
// （与 CLI 行为一致）；同毫秒多次铸造由 counter 区分。
// ============================================================================

var (
	zenIDMu     sync.Mutex
	zenIDLastTS int64
	zenIDCtr    uint64
)

// MintZenSessionID 本地铸造一个 zen 免费层接受的会话 ID。
// 格式："ses_" + 12 个小写 hex（位反转的 (ts_ms<<12|counter) 低 48 位）
// + 14 个 crypto/rand base62 字符，总长 30。
func MintZenSessionID() string { return mintDescendingID("ses_") }

// MintZenRequestID 本地铸造一个 zen 免费层接受的请求 ID（x-opencode-request）。
//
// 2026-10-15 实测（新 inference 后端，干净 IP A/B）：迁移后的免费层校验
// 不再只看会话 ID —— x-opencode-request 同样必须匹配
// ^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$。旧实现用 "msg_"+26 位纯随机字母数字，
// 头 12 位几乎必然含非 hex 字符 → 每个请求都 403 FreeTierError（这就是
// 2026-10 中旬 cline-proxy 全量 403 的根因之一）。校验只看格式不看时间戳
// 内容：随机 hex 头、2020 年时间戳编码均实测通过；故与会话共用同一
// descending 铸造（与 CLI 的 MessageID.create() 同构）。
func MintZenRequestID() string { return mintDescendingID("msg_") }

func mintDescendingID(prefix string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	const hexd = "0123456789abcdef"
	zenIDMu.Lock()
	ts := time.Now().UnixMilli()
	if ts != zenIDLastTS {
		zenIDLastTS, zenIDCtr = ts, 0
	}
	zenIDCtr++
	ctr := zenIDCtr
	zenIDMu.Unlock()
	inv := 0xFFFFFFFFFFFF - ((uint64(ts)<<12 | ctr) & 0xFFFFFFFFFFFF)
	var b [30]byte
	copy(b[:4], prefix)
	for i := 0; i < 6; i++ {
		v := byte((inv >> (40 - 8*i)) & 0xFF)
		b[4+2*i], b[4+2*i+1] = hexd[v>>4], hexd[v&0xF]
	}
	var rb [14]byte
	mustRand(rb[:])
	for i, v := range rb {
		b[16+i] = chars[int(v)%62]
	}
	return string(b[:])
}

// ValidZenRequestID 判断请求 ID 是否符合格式门
// ^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$（30 字符）。
func ValidZenRequestID(s string) bool {
	if len(s) != 30 || !strings.HasPrefix(s, "msg_") {
		return false
	}
	return validZenIDTail(s)
}

// ValidZenSessionID 判断会话 ID 是否符合 zen 免费层的格式门
// ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$（30 字符）。加载持久化会话时据此
// 回认/替换条目，请求路径不再依赖 Minted 标记。
func ValidZenSessionID(s string) bool {
	if len(s) != 30 || !strings.HasPrefix(s, "ses_") {
		return false
	}
	return validZenIDTail(s)
}

func validZenIDTail(s string) bool {
	for i := 4; i < 16; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	for i := 16; i < 30; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}
