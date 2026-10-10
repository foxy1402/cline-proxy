package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

// 隔离测试环境：独立 DATA_DIR，并清空进程级会话/失败印记状态。
func setupZenSessionTest(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zen-session-test")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("DATA_DIR", dir)

	zenSessMu.Lock()
	zenSessions = map[string]*zenSessionEntry{}
	zenSessLoaded = true
	zenSessPath = ""
	zenSessSaveBlocked = false
	zenSessMu.Unlock()
	zenSessFailedMu.Lock()
	zenSessFailed = map[string]int{}
	zenSessFailedMu.Unlock()
	return dir
}

// pickZenKey 必须优先挑 live key：只要池里还有合法会话的 key，
// 未命中轮就该选它（异常窗口外的常规语义）。
func TestPickZenKeyPrefersLiveSessions(t *testing.T) {
	setupZenSessionTest(t)
	cfg := getZenConfig()
	cfg2 := *cfg
	cfg2.Enabled = true
	cfg2.Keys = []string{"sk-dead-1", "sk-live-2", "sk-dead-3"}
	setZenConfig(&cfg2)
	t.Cleanup(func() { c := *cfg; setZenConfig(&c) })

	zenKeyMu.Lock()
	zenKeyCool = map[string]time.Time{}
	zenKeyIdx = 0
	zenKeyMu.Unlock()

	zenSessMu.Lock()
	zenSessions["sk-live-2"] = &zenSessionEntry{Session: "ses_" + "0123456789ab" + "0123456789ABcd"}
	zenSessMu.Unlock()

	for i := 0; i < 5; i++ {
		if got := pickZenKey(); got != "sk-live-2" {
			t.Fatalf("pickZenKey = %q; must prefer the only live key", got)
		}
	}
}

// 池里没有 live 会话时仍须返回 key（异常窗口内请求必须发得出去，
// 上游 403 是唯一可用信号，恢复路径随后本地换新会话）。
func TestPickZenKeyStillReturnsWhenNothingLive(t *testing.T) {
	setupZenSessionTest(t)
	cfg := getZenConfig()
	cfg2 := *cfg
	cfg2.Enabled = true
	cfg2.Keys = []string{"sk-a", "sk-b"}
	setZenConfig(&cfg2)
	t.Cleanup(func() { c := *cfg; setZenConfig(&c) })
	zenKeyMu.Lock()
	zenKeyCool = map[string]time.Time{}
	zenKeyMu.Unlock()

	got := pickZenKey()
	if got != "sk-a" && got != "sk-b" {
		t.Fatalf("pickZenKey = %q; must still hand out a key when none is live", got)
	}
}

// 面板端点：key 快照必须报出 live 状态与已存在的会话。
func TestZenSessionsSnapshot(t *testing.T) {
	setupZenSessionTest(t)
	cfg := getZenConfig()
	cfg2 := *cfg
	cfg2.Enabled = true
	cfg2.Keys = []string{"sk-one", "sk-two"}
	setZenConfig(&cfg2)
	t.Cleanup(func() { c := *cfg; setZenConfig(&c) })

	StickyZenIdentity("sk-one") // 首次请求即本地铸造
	snaps := zenKeyStatus()
	if len(snaps) != 2 {
		t.Fatalf("got %d key states, want 2", len(snaps))
	}
	if snaps[0]["sessionLive"] != true {
		t.Fatalf("key #1 must report sessionLive after local minting: %+v", snaps[0])
	}
	if snaps[1]["sessionLive"] != false {
		t.Fatalf("key #2 never requested; sessionLive must be false: %+v", snaps[1])
	}
	if _, ok := snaps[0]["sessionCreatedAt"]; !ok {
		t.Fatalf("key #1 must report sessionCreatedAt: %+v", snaps[0])
	}
}

// StickyZenIdentity 铸造的会话必须通过 zen 的格式门
// ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$，且同 key 复用、跨 key 不同。
func TestStickyZenIdentityMintsValidSession(t *testing.T) {
	setupZenSessionTest(t)
	a1, _, _ := StickyZenIdentity("sk-a")
	a2, _, _ := StickyZenIdentity("sk-a")
	b, _, _ := StickyZenIdentity("sk-b")
	if a1 != a2 {
		t.Fatalf("sticky identity changed between calls: %s -> %s", a1, a2)
	}
	if a1 == b {
		t.Fatal("two keys must not share a session")
	}
	if !zenSessionLive("sk-a") || !zenSessionLive("sk-b") {
		t.Fatal("minted sessions must be live immediately (no placeholder state)")
	}
	if _, _, ua := StickyZenIdentity("sk-a"); ua != zenNativeUA {
		t.Fatalf("UA = %q, want the native ai-sdk UA", ua)
	}
}

// 403 日志描述：无会话与有会话（带年龄）必须可区分。
func TestZenSessionDescReportsAge(t *testing.T) {
	setupZenSessionTest(t)
	key := "sk-desc"
	if got := zenSessionDesc(key); got != "no session" {
		t.Fatalf("unminted key desc = %q", got)
	}

	zenSessMu.Lock()
	zenSessions[key] = &zenSessionEntry{
		Session:   "ses_" + "0123456789ab" + "0123456789ABcd",
		CreatedAt: time.Now().Add(-5 * time.Hour).Unix(),
	}
	zenSessMu.Unlock()
	got := zenSessionDesc(key)
	if !contains(got, "minted") || !contains(got, "ago") {
		t.Fatalf("minted desc = %q; must report the session age", got)
	}
}

// refreshZenSession：每次调用同步换新（幂等、零成本）；空 key / public 不动；
// 已从配置移除的 key 不重建（防止 prune 后又被复活）。
func TestRefreshZenSessionMintsFreshSession(t *testing.T) {
	setupZenSessionTest(t)
	key := "sk-refresh"
	cfg := getZenConfig()
	cfg2 := *cfg
	cfg2.Keys = []string{key}
	setZenConfig(&cfg2)
	t.Cleanup(func() { c := *cfg; setZenConfig(&c) })

	StickyZenIdentity(key)
	zenSessMu.Lock()
	before := zenSessions[key].Session
	zenSessMu.Unlock()

	refreshZenSession(key)
	zenSessMu.Lock()
	after := zenSessions[key].Session
	zenSessMu.Unlock()
	if after == before {
		t.Fatal("refreshZenSession must mint a new session id")
	}
	if !zenSessionLive(key) {
		t.Fatal("refreshed session must be format-valid/live")
	}
	// 再换一次也必须得到新身份（同步、无退避门槛）
	refreshZenSession(key)
	zenSessMu.Lock()
	third := zenSessions[key].Session
	zenSessMu.Unlock()
	if third == after {
		t.Fatal("second refresh must mint another new session id")
	}
	// 哨兵与空 key 不动作
	refreshZenSession("")
	refreshZenSession("public")
}

// refreshZenSession 不重建已移除的 key：prune 之后该 key 的条目必须保持消失，
// 否则 .zen-sessions.json 会被删掉的旧 key 单调撑大。
func TestRefreshZenSessionSkipsUnconfiguredKey(t *testing.T) {
	setupZenSessionTest(t)
	key := "sk-removed-refresh"
	cfg := getZenConfig()
	cfg2 := *cfg
	cfg2.Keys = []string{"sk-only-active"}
	setZenConfig(&cfg2)
	t.Cleanup(func() { c := *cfg; setZenConfig(&c) })

	refreshZenSession(key)
	zenSessMu.Lock()
	_, exists := zenSessions[key]
	zenSessMu.Unlock()
	if exists {
		t.Fatal("refreshZenSession must not re-create an entry for a key absent from the config")
	}
}

// zenSessionMarkSuccess 清除失败印记：一次成功即该 key 恢复干净。
func TestZenSessionMarkSuccessClearsFailureMark(t *testing.T) {
	setupZenSessionTest(t)
	key := "sk-success"
	zenSessionFailed(key)
	zenSessFailedMu.Lock()
	n := zenSessFailed[key]
	zenSessFailedMu.Unlock()
	if n == 0 {
		t.Fatal("failure mark must be recorded")
	}
	zenSessionMarkSuccess(key)
	zenSessFailedMu.Lock()
	_, still := zenSessFailed[key]
	zenSessFailedMu.Unlock()
	if still {
		t.Fatal("success must clear the failure mark")
	}
}

// 配置移除 key 后 prune 会话与失败印记。
func TestPruneRemovesSessionsAndFailureMarks(t *testing.T) {
	setupZenSessionTest(t)
	k := "sk-removed"
	StickyZenIdentity(k)
	zenSessionFailed(k)

	pruneZenKeyState(map[string]bool{})

	if zenSessionLive(k) {
		t.Fatal("session of a removed key must be pruned")
	}
	zenSessFailedMu.Lock()
	_, marked := zenSessFailed[k]
	zenSessFailedMu.Unlock()
	if marked {
		t.Fatal("failure mark of a removed key must be pruned")
	}
	// 未移除的 key 不受影响
	k2 := "sk-kept"
	StickyZenIdentity(k2)
	sess := zenSessions[k2].Session
	pruneZenKeyState(map[string]bool{k2: true})
	zenSessMu.Lock()
	kept := zenSessions[k2].Session
	zenSessMu.Unlock()
	if kept != sess {
		t.Fatal("session of a still-configured key must survive prune")
	}
}

// 旧版本会话文件迁移：合法格式的 ses_* 保留（格式即凭证），sess_* 占位
// 直接换成新铸造 ID——占位今天就在 403，永远不会自愈。
func TestLegacySessionFileMigration(t *testing.T) {
	dir := setupZenSessionTest(t)
	now := time.Now().Unix()
	// 前两条是真实的 30 字符合法 ID（4+12 小写 hex+14 base62），钉住 keep 分支；
	// 第三条是 4+26 的旧 sess_ 占位，钉住 replace 分支。
	validLive := "ses_" + "0123456789ab" + "0123456789ABcd" // 30 字符合法
	validOld := "ses_" + "000000000001" + "zyxwvu98765432"  // 30 字符合法，旧时间位
	if !kit.ValidZenSessionID(validLive) || !kit.ValidZenSessionID(validOld) {
		t.Fatalf("fixture IDs must be format-valid: %q %q", validLive, validOld)
	}
	legacy := `{
  "sk-legacy-live": {"session": "` + validLive + `", "ua": "opencode/1.18.31", "updated": ` + itoa(now) + `, "harvestedAt": ` + itoa(now) + `},
  "sk-legacy-old": {"session": "` + validOld + `", "ua": "opencode/1.18.31", "updated": ` + itoa(now-30*24*3600) + `},
  "sk-legacy-placeholder": {"session": "sess_placeholder000000000000", "ua": "opencode/1.18.31", "updated": ` + itoa(now) + `}
}`
	if err := os.WriteFile(filepath.Join(dir, ".zen-sessions.json"), []byte(legacy), 0600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	zenSessMu.Lock()
	zenSessions = map[string]*zenSessionEntry{}
	zenSessLoaded = false
	zenSessPath = ""
	zenSessMu.Unlock()
	loadZenSessions()

	if !zenSessionLive("sk-legacy-live") {
		t.Fatal("legacy ses_* session must survive migration (format-valid)")
	}
	if !zenSessionLive("sk-legacy-old") {
		t.Fatal("aged legacy ses_* session is still format-valid (live), just old")
	}
	if !zenSessionLive("sk-legacy-placeholder") {
		t.Fatal("legacy sess_* placeholder must be replaced with a locally minted ID and become live")
	}

	zenSessMu.Lock()
	liveEntry := zenSessions["sk-legacy-live"]
	oldEntry := zenSessions["sk-legacy-old"]
	if liveEntry == nil || oldEntry == nil {
		zenSessMu.Unlock()
		t.Fatal("kept entries missing after load")
	}
	keptLive, keptOld := liveEntry.Session, oldEntry.Session
	mig := liveEntry.CreatedAt
	repl := zenSessions["sk-legacy-placeholder"].Session
	zenSessMu.Unlock()
	// keep 分支真实命中：两条合法 ID 原样保留（未被替换、未被重新铸造）
	if keptLive != validLive || keptOld != validOld {
		t.Fatalf("format-valid entries must be kept verbatim, got %q / %q", keptLive, keptOld)
	}
	if mig != now {
		t.Fatalf("migrated entry should inherit harvestedAt as CreatedAt, got %d want %d", mig, now)
	}
	if repl == "sess_placeholder000000000000" || !zenSessionLive("sk-legacy-placeholder") {
		t.Fatalf("placeholder was not replaced with a minted ID: %q", repl)
	}
	if !kit.ValidZenSessionID(repl) {
		t.Fatalf("replacement must be a valid minted ID, got %q", repl)
	}
	// 空 UA 回填：旧文件缺 ua 时请求会带空 UA，破坏指纹伪装
	zenSessMu.Lock()
	zenSessions["sk-legacy-live"].UA = ""
	zenSessMu.Unlock()
	_, _, ua := StickyZenIdentity("sk-legacy-live")
	if ua != zenNativeUA {
		t.Fatalf("empty UA must be backfilled, got %q", ua)
	}
}

// 坏文件先备份再空跑：否则紧随其后的 save 会直接覆盖，出问题时无从追查。
func TestCorruptSessionFileIsBackedUp(t *testing.T) {
	dir := setupZenSessionTest(t)
	path := filepath.Join(dir, ".zen-sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	zenSessMu.Lock()
	zenSessions = map[string]*zenSessionEntry{}
	zenSessLoaded = false
	zenSessPath = ""
	zenSessMu.Unlock()
	loadZenSessions()

	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt session file must be kept as .corrupt: %v", err)
	}
	zenSessMu.Lock()
	n := len(zenSessions)
	zenSessMu.Unlock()
	if n != 0 {
		t.Fatalf("corrupt file must start with an empty table, got %d entries", n)
	}
}

// 掩码不得泄露短 key（kit.Truncate 对短串原样返回）。
func TestMaskZenKeyNeverLeaksShortKey(t *testing.T) {
	if m := maskZenKey("sk-1"); contains(m, "sk-1") {
		t.Fatalf("short key leaked in mask: %q", m)
	}
	if m := maskZenKey("sk-abcdefghijklmn"); m != "sk-abc…" {
		t.Fatalf("long key mask: got %q", m)
	}
	if m := maskZenKey(""); m != "-" {
		t.Fatalf("empty key mask: got %q", m)
	}
	if m := maskZenKey("public"); !contains(m, "public") {
		t.Fatalf("public sentinel mask: got %q", m)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
