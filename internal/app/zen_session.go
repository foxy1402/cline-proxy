package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ zen 会话粘性（sticky session） ============
//
// 背景（2026-10-09 实测结论）：zen 免费层的会话门是无状态的格式检查——
// 会话 ID 匹配 ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$ 即放行。服务端既不检查
// "见过此 ID"，也不检查时间戳新鲜度：本地铸造的合法格式 ID 三次直连通过
// （含复用），而旧 sess_ 随机占位因格式不符必然 403 FreeTierError。
// （早期"只有 CLI 收割的会话能用"的结论是被占位 ID 的坏格式混淆了。）
//
// 因此每个 zen key 绑定一个稳定的本地铸造 ses_ ID（kit.MintZenSessionID，
// 结构与 CLI 铸造字节兼容）+ 固定的 ai-sdk 形态 UA；msg_ 请求 ID 仍每次
// 随机。会话文件持久化到 DATA_DIR/.zen-sessions.json，重启后沿用同一身份。
//
// 会话失效（FreeTier 403）时本地直接换新：refreshZenSession（连续 2 次
// 403 触发，1 分钟 key 级退避防热循环）。若 zen 未来收紧门禁（合法格式
// 也被拒），日志里的换新频率会先于用户感知升高——这就是 tripwire。

type zenSessionEntry struct {
	Session string `json:"session"`
	UA      string `json:"ua"`
	Updated int64  `json:"updated"`
	// Minted 历史字段：曾区分"CLI 收割过"与"本地随机占位"。本地铸造成为
	// 唯一来源后所有条目恒为 true，保留只为兼容旧文件（读入后即不再有意义）。
	Minted bool `json:"minted,omitempty"`
	// CreatedAt 会话创建时间（unix 秒），日志/面板展示年龄用。
	// 旧文件的 harvestedAt 读入时迁移到该字段，下次保存时写出新键名。
	CreatedAt int64 `json:"createdAt,omitempty"`
	// HarvestedAt 旧字段名，仅为读入旧文件保留（unmarshal 兜底），
	// 加载时并入 CreatedAt，不再写回。
	HarvestedAt int64 `json:"harvestedAt,omitempty"`
}

var (
	zenSessMu     sync.Mutex
	zenSessions   = map[string]*zenSessionEntry{} // zen key -> sticky identity
	zenSessLoaded bool
	zenSessPath   string
	// zenNativeUA 官方 CLI 1.18.31 的原生 ai-sdk 形态 UA（会话粘性与
	// 轮换列表共用；与 tls_bun.go 指纹版本耦合——升版本需同步这两处）。
	zenNativeUA = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
)

// zenSessionFile 会话持久化路径（DATA_DIR 优先，容器 volume 挂载点）。
func zenSessionFile() string {
	if zenSessPath == "" {
		zenSessPath = kit.ResolveDataPath(".zen-sessions.json")
	}
	return zenSessPath
}

// loadZenSessions 启动时/首次使用时加载持久化会话（只读一次，失败则空跑）。
func loadZenSessions() {
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	if zenSessLoaded {
		return
	}
	zenSessLoaded = true
	data, err := os.ReadFile(zenSessionFile())
	if err != nil {
		// ENOENT 是首启正常路径（还没有会话文件）；其他错误要让运维看见，
		// 否则会静默以空表运行，并在下次 save 时把原文件覆盖掉。
		if !os.IsNotExist(err) {
			log.Printf("zen sessions read failed (%s): %v", zenSessionFile(), err)
		}
		return
	}
	var m map[string]*zenSessionEntry
	if err := json.Unmarshal(data, &m); err != nil {
		// 解析失败先把坏文件改名留证，再以空表启动——与 loadZenConfig 同做法。
		// 不备份的话，紧随其后的 save 会直接覆盖，出问题时无从追查。
		_ = os.Rename(zenSessionFile(), zenSessionFile()+".corrupt")
		log.Printf("zen sessions parse failed, starting fresh (backup: %s.corrupt): %v", zenSessionFile(), err)
		return
	}
	replaced, migrated := 0, 0
	now := time.Now().Unix()
	for k, e := range m {
		if e == nil || e.Session == "" {
			continue
		}
		// 旧 harvestedAt → CreatedAt 一次性迁移（写出时只写新键名）。
		if e.CreatedAt == 0 && e.HarvestedAt > 0 {
			e.CreatedAt = e.HarvestedAt
		}
		if kit.ValidZenSessionID(e.Session) {
			// 合法格式（旧收割条目或本会话铸造的）：原样保留，格式即凭证。
			if !e.Minted {
				e.Minted = true
				migrated++
			}
		} else {
			// sess_* 占位等非法格式：今天就在 403，永远不会自愈，直接换成
			// 本地铸造的新 ID。旧文件若无 CreatedAt 以加载时刻兜底。
			e.Session = kit.MintZenSessionID()
			e.Minted = true
			if e.CreatedAt == 0 {
				e.CreatedAt = now
			}
			replaced++
		}
		zenSessions[k] = e
	}
	log.Printf("zen sessions loaded: %d key(s) with sticky identity", len(zenSessions))
	if replaced > 0 {
		log.Printf("zen sessions migrated: %d invalid-format placeholder(s) replaced with locally minted IDs", replaced)
	}
	if migrated > 0 {
		log.Printf("zen sessions migrated: %d entry(ies) marked minted from session id format", migrated)
	}
	if replaced > 0 || migrated > 0 {
		saveZenSessionsLocked()
	}
}

// saveZenSessionsLocked 持久化当前会话表（调用方持有 zenSessMu）。
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

// zenSessionLive 该 key 是否有一个格式合法的粘性会话。本地铸造后所有条目
// 生而合法；保留批量/单点两个入口是为了 pickZenKey 的两段轮转语义不变。
func zenSessionLive(key string) bool {
	if key == "" || key == "public" {
		return false
	}
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	e := zenSessions[key]
	return e != nil && kit.ValidZenSessionID(e.Session)
}

// zenLiveKeys 批量查询（一次加锁），供 pickZenKey 在轮转时优先挑 live key。
func zenLiveKeys(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	live := make(map[string]bool, len(keys))
	for _, k := range keys {
		if e := zenSessions[k]; e != nil && kit.ValidZenSessionID(e.Session) {
			live[k] = true
		}
	}
	return live
}

// zenSessionSnapshot 每个 key 的会话状态（管理面板展示；session 截断显示）。
type zenSessionSnapshot struct {
	Minted    bool
	Live      bool
	Session   string
	CreatedAt int64
}

func zenSessionSnapshotOf(key string) zenSessionSnapshot {
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	e := zenSessions[key]
	if e == nil {
		return zenSessionSnapshot{}
	}
	live := kit.ValidZenSessionID(e.Session)
	return zenSessionSnapshot{
		Minted:    live, // 格式合法即视为 minted（本地铸造是唯一来源）
		Live:      live,
		Session:   kit.Truncate(e.Session, 12),
		CreatedAt: e.CreatedAt,
	}
}

// zenSessionDesc 供日志使用的会话描述：展示会话年龄——合法格式会话 403
// 意味着寿命/额度窗口到期或门禁收紧，换新频率是门禁变化的 tripwire。
func zenSessionDesc(key string) string {
	s := zenSessionSnapshotOf(key)
	switch {
	case s.Session == "":
		return "no session"
	case s.CreatedAt <= 0:
		return "minted (age unknown)"
	default:
		age := time.Since(time.Unix(s.CreatedAt, 0)).Round(time.Minute)
		return fmt.Sprintf("minted %v ago (local)", age)
	}
}

// StickyZenIdentity 取 key 绑定的稳定身份：会话 ID 与 UA 跨请求复用，
// 请求 ID 每次全新（与官方 CLI 语义一致：同会话内多 msg_）。
// 返回 (session, request, user-agent)。
func StickyZenIdentity(key string) (sess, req, ua string) {
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	e, ok := zenSessions[key]
	if !ok || e.Session == "" {
		e = &zenSessionEntry{
			Session:   kit.MintZenSessionID(),
			UA:        zenNativeUA,
			CreatedAt: time.Now().Unix(),
		}
		zenSessions[key] = e
		saveZenSessionsLocked()
		log.Printf("zen sticky session minted locally for key#%d: %s", keyIndex(key), kit.Truncate(e.Session, 24))
	}
	e.Updated = time.Now().Unix()
	// 补 UA：旧版本文件或外部改写的条目可能缺这一项，空 UA 发出会被上游
	// 按非 CLI 流量处理。UA 属于客户端指纹，须与会话铸造时一致。
	if e.UA == "" {
		e.UA = zenNativeUA
	}
	return e.Session, "msg_" + kit.RandAlphaNum(26), e.UA
}

// pruneZenKeyState 配置变更后清理已移除 key 的运行时状态（会话粘性 + 403 恢复计数）。
// valid 为当前有效 key 集合。
func pruneZenKeyState(valid map[string]bool) {
	zenRecoverMu.Lock()
	for k := range zenRecoverFails {
		if !valid[k] {
			delete(zenRecoverFails, k)
		}
	}
	for k := range zenRecoverLastAt {
		if !valid[k] {
			delete(zenRecoverLastAt, k)
		}
	}
	zenRecoverMu.Unlock()

	zenSessMu.Lock()
	removed := 0
	for k := range zenSessions {
		if !valid[k] {
			delete(zenSessions, k)
			removed++
		}
	}
	if removed > 0 {
		saveZenSessionsLocked()
	}
	zenSessMu.Unlock()
	if removed > 0 {
		log.Printf("zen sessions pruned: %d removed key(s)", removed)
	}
}

// ============ 403 恢复：本地换新会话 ============

var (
	zenRecoverMu     sync.Mutex
	zenRecoverFails  = map[string]int{}   // key -> 连续 FreeTier 403 次数
	zenRecoverLastAt = map[string]int64{} // key -> 上次换新尝试时刻（unix 秒）
)

// zenRefreshBackoff 单 key 连续换新的最小间隔：防 403 热循环，远短于旧
// 收割机的 10 分钟——本地铸造零成本，换新只需要一次内存写 + 文件保存。
const zenRefreshBackoff = time.Minute

// zenSessionMarkSuccess 2xx 后清零该 key 的连续 403 计数（调用点在两条
// 上游调用路径的 200 分支）。
func zenSessionMarkSuccess(key string) {
	if key == "" {
		return
	}
	zenRecoverMu.Lock()
	delete(zenRecoverFails, key)
	zenRecoverMu.Unlock()
}

// refreshZenSession FreeTier 403 的恢复动作：连续 2 次 403（且过了 key 级
// 退避）就把该 key 的粘性会话换成本地铸造的新 ID。异步调用（go refresh…），
// 不阻塞请求路径；与收割机不同，这里没有子进程、没有额度消耗。
// 若 zen 收紧门禁（合法格式也被拒），换新只会继续 403——换新频率升高本身
// 就是日志里的门禁变化信号。
func refreshZenSession(key string) {
	if key == "" || key == "public" {
		return
	}
	zenRecoverMu.Lock()
	n := zenRecoverFails[key] + 1
	zenRecoverFails[key] = n
	last := zenRecoverLastAt[key]
	now := time.Now()
	due := now.Sub(time.Unix(last, 0)) >= zenRefreshBackoff
	refresh := n >= 2 && due
	if refresh {
		zenRecoverLastAt[key] = now.Unix()
	}
	zenRecoverMu.Unlock()
	if !refresh {
		return
	}
	loadZenSessions()
	zenSessMu.Lock()
	e, ok := zenSessions[key]
	if !ok {
		e = &zenSessionEntry{UA: zenNativeUA}
		zenSessions[key] = e
	}
	e.Session = kit.MintZenSessionID()
	e.CreatedAt = now.Unix()
	e.Minted = true
	saveZenSessionsLocked()
	sess := e.Session
	zenSessMu.Unlock()
	log.Printf("zen session refreshed locally for key#%d after %d consecutive 403(s): %s",
		keyIndex(key), n, kit.Truncate(sess, 24))
}
