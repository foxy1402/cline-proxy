package kit

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func resetZenIDState(ts int64) {
	zenIDMu.Lock()
	zenIDLastTS = ts
	zenIDCtr = 0
	zenIDMu.Unlock()
}

func TestMintZenSessionIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 100; i++ {
		id := MintZenSessionID()
		if len(id) != 30 {
			t.Fatalf("len = %d, want 30: %s", len(id), id)
		}
		if !re.MatchString(id) {
			t.Fatalf("format mismatch: %s", id)
		}
	}
}

func TestMintZenSessionIDUniqueWithinSameMillisecond(t *testing.T) {
	resetZenIDState(time.Now().UnixMilli())
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := MintZenSessionID()
		if seen[id] {
			t.Fatalf("duplicate ID within same ms: %s", id)
		}
		seen[id] = true
	}
}

func TestMintZenSessionIDDescendingOrder(t *testing.T) {
	// CLI 的 descending 变体：后铸 ID 的 hex 时间位更大（位反转），
	// 字典序排在先铸 ID 之前。跨毫秒铸两枚验证。
	first := MintZenSessionID()
	time.Sleep(2 * time.Millisecond)
	second := MintZenSessionID()
	if second >= first {
		t.Fatalf("later ID should sort before earlier one:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestMintZenSessionIDTailIsRandom(t *testing.T) {
	// 尾部 14 字符来自 crypto/rand：同毫秒内的多枚 ID 不应共享尾部。
	resetZenIDState(time.Now().UnixMilli())
	a := MintZenSessionID()
	b := MintZenSessionID()
	if a[16:] == b[16:] {
		t.Fatalf("random tails identical within same ms: %s", a[16:])
	}
}

func TestValidZenSessionID(t *testing.T) {
	good := []string{MintZenSessionID(), "ses_" + "0123456789ab" + "0123456789ABcd"}
	for _, s := range good {
		if !ValidZenSessionID(s) {
			t.Errorf("ValidZenSessionID(%q) = false, want true", s)
		}
	}
	bad := []string{
		"",
		"sess_" + strings.Repeat("a", 25),           // 旧占位前缀
		"ses_0123456789abc",                         // hex 段过短
		"ses_0123456789ABCDEF0123456789",            // hex 段含大写
		"ses_0123456789abcdef0123456789!",           // 尾段含非法字符
		"ses_0123456789abcdef0123456789extra-long",  // 超长
	}
	for _, s := range bad {
		if ValidZenSessionID(s) {
			t.Errorf("ValidZenSessionID(%q) = true, want false", s)
		}
	}
}

// 匿名路径的身份也必须过免费门：session 组件必须是合法 ses_ 格式，
// 否则路由到 zen 时每请求必 403（旧实现 mint "sess_"+26 随机串即此问题）。
func TestFreshZenIdentityMintsValidSession(t *testing.T) {
	re := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 50; i++ {
		sess, req, ua := FreshZenIdentity()
		if !re.MatchString(sess) {
			t.Fatalf("anonymous session not gate-valid: %q", sess)
		}
		if !ValidZenSessionID(sess) {
			t.Fatalf("ValidZenSessionID rejected a freshly minted session: %q", sess)
		}
		if !strings.HasPrefix(req, "msg_") || len(req) != 30 {
			t.Fatalf("request id malformed: %q", req)
		}
		if ua == "" {
			t.Fatal("user-agent must not be empty")
		}
	}
}
