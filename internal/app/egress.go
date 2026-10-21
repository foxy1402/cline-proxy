package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ egress binding: one proxy per upstream identity ============
//
// 背景（2026-10-21 实测）：opencode zen 免费层现在按 api key 记录出口 IP，
// 一个 key 只允许一个"个人 IP"。共享代理池的 round-robin 让每个 key 轮流
// 从所有出口出去 —— 每个 key 都见过 N 个 IP，即使代理本身干净，上游也会
// 把整池 key 判为可疑。对策是把出口从"按请求轮转"改成"按 key 绑定"：
// 一个 key 永远从同一个出口出去。
//
// 容量规则：同一个 namespace 内一个代理只服务一个身份。代理数少于身份数时，
// 多出来的身份没有绑定 —— 它们被 pickZenKey / pickAccountExcluding 过滤掉，
// 而不是退回直连或借用别的出口（那正是要消除的指纹）。
// zen 与 cline 是两个独立 namespace：同一个代理同时服务一个 zen key 和一个
// cline 账号是安全的，两个上游各自只看到属于自己那一个身份。
//
// 持久化：DATA_DIR/.egress-bindings.json（0600，tmp+rename）。绑定必须跨
// 重启保留——每次启动重新分配等于让 key 换 IP，正好重新触发上游告警。
// zen key 在文件里只以 sha256 前 8 字节的 hex 出现，所以绑定表不含明文凭据，
// 可以安全地贴进 issue。cline 账号 ID 是 UUID 而非凭据，保持可读以便排查。

const (
	egressNSZen   = "zen:"
	egressNSCline = "cline:"
)

// egressBinding 单个身份绑定的出口。
type egressBinding struct {
	Proxy string `json:"proxy"`
	Since int64  `json:"since"`
}

// boolPtr 取指针（配置里用 *bool 区分"未提交"与"显式 false"）。
func boolPtr(b bool) *bool { return &b }

var (
	egressMu          sync.Mutex
	egressBindings    = map[string]egressBinding{}
	egressLoaded      bool
	egressSaveBlocked bool
	egressPath        string
)

func egressBindingFile() string {
	if egressPath == "" {
		egressPath = kit.ResolveDataPath(".egress-bindings.json")
	}
	return egressPath
}

// egressID 绑定表的键：namespace + 身份。zen key 是凭据，只写入 sha256
// 前 8 字节的 hex（16 字符），绑定表因此不含明文密钥。cline 账号 ID 不是
// 凭据，原样保留——面板与日志里能直接对上号。
func egressID(ns, id string) string {
	if ns == egressNSZen {
		sum := sha256.Sum256([]byte(id))
		return hex.EncodeToString(sum[:8])
	}
	return id
}

func loadEgressBindings() {
	egressMu.Lock()
	defer egressMu.Unlock()
	if egressLoaded {
		return
	}
	egressLoaded = true
	data, err := os.ReadFile(egressBindingFile())
	if err != nil {
		// ENOENT 是首启正常路径。其它错误要让运维看见，但不能落盘覆盖——
		// 与 .zen-sessions.json 同一条纪律。
		if !os.IsNotExist(err) {
			log.Printf("egress bindings read failed (%s): %v — refusing to persist this run", egressBindingFile(), err)
			egressSaveBlocked = true
		}
		return
	}
	var m map[string]egressBinding
	if err := json.Unmarshal(data, &m); err != nil {
		_ = os.Rename(egressBindingFile(), egressBindingFile()+".corrupt")
		log.Printf("egress bindings parse failed, starting fresh (backup: %s.corrupt): %v", egressBindingFile(), err)
		return
	}
	if m != nil {
		egressBindings = m
	}
}

// saveEgressLocked 持久化绑定表（调用方持有 egressMu）。
func saveEgressLocked() {
	if egressSaveBlocked {
		return
	}
	data, err := json.MarshalIndent(egressBindings, "", "  ")
	if err != nil {
		return
	}
	tmp := egressBindingFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("egress bindings save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, egressBindingFile()); err != nil {
		log.Printf("egress bindings save failed (rename): %v", err)
	}
}

// reconcileEgressBindings 按当前 key/账号/代理列表重算绑定并落盘。
// 幂等：已有绑定只要代理还在池里就原样保留（这是"key 不换 IP"的全部意义）。
func reconcileEgressBindings() {
	loadEgressBindings()
	cfg := getZenConfig()
	accounts := poolAccountIDs()

	egressMu.Lock()
	defer egressMu.Unlock()
	zenAdded, zenDropped := assignEgressLocked(egressNSZen, cfg.Keys, cfg.Proxies)
	clineAdded, clineDropped := assignEgressLocked(egressNSCline, accounts, cfg.Proxies)
	if zenAdded+zenDropped+clineAdded+clineDropped > 0 {
		saveEgressLocked()
	}
}

// assignEgressLocked 为一个 namespace 重算绑定，返回 (新分配, 失效丢弃) 数量。
//
// 分配顺序：先按配置顺序为已绑定身份登记代理占用，再把未绑定身份依次填入
// 空闲代理。这样新增一个代理只会给下一个空身份用，既有 key 的出口不动。
func assignEgressLocked(ns string, ids, proxies []string) (assigned, dropped int) {
	valid := make(map[string]bool, len(proxies))
	for _, p := range proxies {
		if p != "" {
			valid[p] = true
		}
	}
	// 1) 代理被移出池 → 该绑定失效（占位也已不存在）。
	for k, b := range egressBindings {
		if !strings.HasPrefix(k, ns) {
			continue
		}
		if !valid[b.Proxy] {
			delete(egressBindings, k)
			dropped++
		}
	}
	if len(valid) == 0 {
		// 无代理 = 直连模式：绑定没有意义，清空以免代理重新加回来时
		// 沿用一批陈旧（且可能已被别人占用）的分配。
		for k := range egressBindings {
			if strings.HasPrefix(k, ns) {
				delete(egressBindings, k)
			}
		}
		return 0, dropped
	}
	// 2) 已绑定身份先占位（按配置顺序，先到先得，结果稳定）。
	taken := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !egressEligibleID(id) {
			continue
		}
		if b, ok := egressBindings[ns+egressID(ns, id)]; ok {
			taken[b.Proxy] = true
		}
	}
	// 3) 未绑定身份填空闲代理；池满则保持未绑定（= 该身份被过滤掉）。
	for _, id := range ids {
		if !egressEligibleID(id) {
			continue
		}
		bk := ns + egressID(ns, id)
		if _, ok := egressBindings[bk]; ok {
			continue
		}
		next := ""
		for _, p := range proxies {
			if p == "" || taken[p] {
				continue
			}
			next = p
			break
		}
		if next == "" {
			continue
		}
		egressBindings[bk] = egressBinding{Proxy: next, Since: time.Now().Unix()}
		taken[next] = true
		assigned++
	}
	return assigned, dropped
}

// egressEligibleID 该身份是否需要独占一个出口。
// 空 ID 与 zen 的匿名哨兵 "public" 不算身份：没有 key 就无法被上游关联，
// 给它们分配代理只会白占名额——恰好挤掉一个真实 key 的出口。
func egressEligibleID(id string) bool {
	return id != "" && id != "public"
}

// egressProxyFor 查一个身份绑定的出口。
func egressProxyFor(ns, id string) (string, bool) {
	if !egressEligibleID(id) {
		return "", false
	}
	loadEgressBindings()
	egressMu.Lock()
	defer egressMu.Unlock()
	b, ok := egressBindings[ns+egressID(ns, id)]
	if !ok || b.Proxy == "" {
		return "", false
	}
	return b.Proxy, true
}

func egressProxyForZen(key string) (string, bool)  { return egressProxyFor(egressNSZen, key) }
func egressProxyForCline(id string) (string, bool) { return egressProxyFor(egressNSCline, id) }

// pinProxyPerKeyEnabled 面板开关（默认开）。nil 表示旧配置没有这个键。
func pinProxyPerKeyEnabled() bool {
	p := getZenConfig().PinProxyPerKey
	return p == nil || *p
}

// egressBindingActive 绑定策略是否真的生效：必须同时"开启"且"有代理"。
// 没有代理时退回直连（历史行为）——否则默认配置（proxies 为空）会让每个
// zen key 都被判为"无可用出口"，整条 zen 链路直接失效。
func egressBindingActive() bool {
	return pinProxyPerKeyEnabled() && len(getZenConfig().Proxies) > 0
}

// proxyIndex 返回代理在池中的下标（冷却表按索引记录）；不在池中返回 -1。
func proxyIndex(p string) int {
	if p == "" {
		return -1
	}
	for i, x := range getZenConfig().Proxies {
		if x == p {
			return i
		}
	}
	return -1
}

// zenEgress 取该 key 本次上游尝试要用的出口。
// 绑定生效时用该 key 的固定出口（返回的索引用于代理冷却与日志）；
// 未启用绑定时保留按请求轮转的旧行为。
//
// "public"（匿名、无凭据哨兵）不受绑定约束：没有 key 可被关联，也就没有
// "一 key 一 IP"的要求，仍按池轮转分摊负载。
//
// 绑定生效但该 key 没有出口时返回错误，绝不退回直连：直连会把一个额外 IP
// 暴露给上游，正是这套机制要消除的指纹。pickZenKey 已过滤掉这类 key，
// 但 pinKey（面板 Test）与 ZEN_PIN_KEY 绕过轮转，必须在这里同样挡住。
func zenEgress(key string) (proxy string, idx int, err error) {
	if !egressBindingActive() || key == "" || key == "public" {
		p, i := pickUpstreamProxy()
		return p, i, nil
	}
	p, ok := egressProxyForZen(key)
	if !ok {
		return "", -1, fmt.Errorf("zen key #%d has no egress proxy assigned (one proxy serves one key; %d proxies configured, %d keys) — add proxies, or turn off pinProxyPerKey",
			keyIndex(key), len(getZenConfig().Proxies), len(getZenConfig().Keys))
	}
	return p, proxyIndex(p), nil
}

// poolAccountIDs 按池内顺序返回账号 ID（绑定分配顺序即此顺序）。
func poolAccountIDs() []string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	ids := make([]string, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		ids = append(ids, a.AccountID)
	}
	return ids
}

// egressCapacity 返回 (已绑定身份数, 身份总数)。
func egressCapacity(ns string, ids []string) (assigned, total int) {
	loadEgressBindings()
	egressMu.Lock()
	defer egressMu.Unlock()
	for _, id := range ids {
		if _, ok := egressBindings[ns+egressID(ns, id)]; ok {
			assigned++
		}
	}
	return assigned, len(ids)
}

// egressShortfallError 说明"为什么没有可用身份"以及怎么修。
// 这是绑定策略下最常见的问题（代理比 key 少），必须有可操作的提示，
// 否则用户只看到 500/503 而不知道为什么 key 突然不用了。
func egressShortfallError(label, ns string, ids []string) error {
	assigned, total := egressCapacity(ns, ids)
	return fmt.Errorf("%d of %d %s identities have an egress proxy assigned (one proxy serves one identity; %d proxies configured) — add proxies, or turn off pinProxyPerKey",
		assigned, total, label, len(getZenConfig().Proxies))
}

// egressKeyState 单个 key 的出口快照（面板用）。
type egressKeyState struct {
	proxy string // 绑定的代理（未打码，调用方按需掩码）
	index int    // 池内下标，-1 表示无
	bound bool   // 绑定策略是否处于生效状态（用来区分"关闭"与"没分到"）
}

// zenEgressSnapshotOf 批量取 key 的出口快照。在 zenKeyMu 之外调用。
func zenEgressSnapshotOf(keys []string) []egressKeyState {
	active := egressBindingActive()
	out := make([]egressKeyState, len(keys))
	for i, k := range keys {
		out[i].bound = active
		out[i].index = -1
		if !active || k == "" || k == "public" {
			continue
		}
		if p, ok := egressProxyForZen(k); ok {
			out[i].proxy = p
			out[i].index = proxyIndex(p)
		}
	}
	return out
}

// zenEgressSnapshot 汇总接口：给面板判断"代理够不够分"。
func zenEgressSnapshot() map[string]any {
	cfg := getZenConfig()
	assigned, total := egressCapacity(egressNSZen, cfg.Keys)
	return map[string]any{
		"enabled":    pinProxyPerKeyEnabled(),
		"active":     egressBindingActive(),
		"proxies":    len(cfg.Proxies),
		"assigned":   assigned,
		"identities": total,
		"unassigned": total - assigned,
	}
}
