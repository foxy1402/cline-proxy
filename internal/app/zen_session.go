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
	// zenSessSaveBlocked 会话文件读取失败（非 ENOENT）时置位：此时内存表是空的，
	// 任何 save 都会把磁盘上读不到的原文件覆盖掉。宁可本次运行不落盘，也不能
	// 销毁可能有价值的数据（重启后重试读取）。
	zenSessSaveBlocked bool
	// zenNativeUA 官方 CLI 的原生 ai-sdk 形态 UA（会话粘性与轮换列表共用）。
	// opencode 版本按最新 CLI 核对（audit 2026-10-10：1.18.31 → 1.18.35）；
	// ai-sdk / bun 段保持不变——它们对应 TLS 指纹（tls_bun.go 的 Bun hello），
	// 与 opencode 应用版本无关，改动这两段才会与指纹失配。
	zenNativeUA = "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
)

// defaultSessionRotateMinutes 粘性会话的默认轮换周期（分钟）。语义见
// zenConfigData.SessionRotateMinutes：上游对"长期未更新的会话"首个请求会变慢，
// 定期重铸可避免这个延迟；本地铸造零成本（无子进程、无额度消耗）。
const defaultSessionRotateMinutes = 120

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
			log.Printf("zen sessions read failed (%s): %v — refusing to persist this run to avoid clobbering it", zenSessionFile(), err)
			zenSessSaveBlocked = true
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
	replaced, migrated, stamped := 0, 0, 0
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
			// 无创建时间的旧条目：以加载时刻起算轮换时钟，否则老化轮换对它永久失效
			// （CreatedAt==0 被判为"年龄未知"而跳过）。只在真有会话时才补。
			if e.CreatedAt == 0 {
				e.CreatedAt = now
				stamped++
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
	if stamped > 0 {
		log.Printf("zen sessions migrated: %d entry(ies) stamped with a rotation clock (createdAt=load time)", stamped)
	}
	if replaced > 0 || migrated > 0 || stamped > 0 {
		saveZenSessionsLocked()
	}
}

// saveZenSessionsLocked 持久化当前会话表（调用方持有 zenSessMu）。
func saveZenSessionsLocked() {
	// 读取阶段失败（非 ENOENT）时内存表不可信，落盘会覆盖磁盘上读不到的文件。
	if zenSessSaveBlocked {
		return
	}
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

// sessionRotateInterval 会话轮换周期；0（或负）表示关闭轮换。
func sessionRotateInterval() time.Duration {
	n := getZenConfig().SessionRotateMinutes
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Minute
}

// mintZenSessionEntryLocked 就地刷新条目的会话：新 ID + 新创建时间 + 合法标记，
// 并在 UA 缺失时补上客户端指纹。调用方须持有 zenSessMu。
// 请求路径与后台巡检共用，避免两处铸造逻辑漂移。
func mintZenSessionEntryLocked(e *zenSessionEntry, now time.Time) {
	e.Session = kit.MintZenSessionID()
	e.CreatedAt = now.Unix()
	e.Minted = true
	if e.UA == "" {
		e.UA = zenNativeUA
	}
}

// StickyZenIdentity 取 key 绑定的稳定身份：会话 ID 与 UA 跨请求复用，
// 请求 ID 每次全新（与官方 CLI 语义一致：同会话内多 msg_）。
// 返回 (session, request, user-agent)。
//
// 会话老化轮换：条目年龄超过 SessionRotateMinutes 时重铸（同步、零成本）。
// 主路径是后台巡检（startZenSessionRotator）——它保证空闲 key 也会按周期换新，
// 这里的同款判断只是兜底：巡检未运行（未启用/未启动）时请求仍能自愈。
func StickyZenIdentity(key string) (sess, req, ua string) {
	loadZenSessions()
	// 轮换周期先读（会取 zenConfigMu），再进 zenSessMu：与 zenKeyStatus 同约定，
	// 两把锁不嵌套，避免今后任一路径反向加锁时死锁。
	iv := sessionRotateInterval()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	now := time.Now()
	e, ok := zenSessions[key]
	if !ok || !kit.ValidZenSessionID(e.Session) {
		if e == nil {
			e = &zenSessionEntry{}
			zenSessions[key] = e
		}
		mintZenSessionEntryLocked(e, now)
		saveZenSessionsLocked()
		log.Printf("zen sticky session minted locally for key#%d: %s", keyIndex(key), kit.Truncate(e.Session, 24))
	} else if iv > 0 && e.CreatedAt > 0 && now.Sub(time.Unix(e.CreatedAt, 0)) >= iv {
		// 老化轮换：重铸会话（保留 UA），并清掉 403 失败印记——新会话是干净状态。
		age := now.Sub(time.Unix(e.CreatedAt, 0)).Round(time.Minute)
		mintZenSessionEntryLocked(e, now)
		saveZenSessionsLocked()
		zenSessFailedMu.Lock()
		delete(zenSessFailed, key)
		zenSessFailedMu.Unlock()
		log.Printf("zen session rotated after %v for key#%d: %s", age, keyIndex(key), kit.Truncate(e.Session, 24))
	}
	e.Updated = now.Unix()
	// 补 UA：旧版本文件或外部改写的条目可能缺这一项，空 UA 发出会被上游
	// 按非 CLI 流量处理。UA 属于客户端指纹，须与会话铸造时一致。
	if e.UA == "" {
		e.UA = zenNativeUA
	}
	return e.Session, "msg_" + kit.RandAlphaNum(26), e.UA
}

// pruneZenKeyState 配置变更后清理已移除 key 的运行时状态（会话粘性 + 403 印记）。
// valid 为当前有效 key 集合。
func pruneZenKeyState(valid map[string]bool) {
	zenSessFailedMu.Lock()
	for k := range zenSessFailed {
		if !valid[k] {
			delete(zenSessFailed, k)
		}
	}
	zenSessFailedMu.Unlock()

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
//
// 语义（重构后，替代旧的"连续 2 次 403 + 后台异步换新"）：
//   - 请求路径收到 FreeTier 403 后同步换一次会话（本请求内至多一次，由调用点的
//     sessionRetried 标志保证），随后在**同一 key** 上重试——不再跨 key 扇出。
//     跨 key 扇出是错的：403 针对的是 key/会话，换到别的 key 只会把同一批坏
//     状态扩散到全池，而不是绕过它。
//   - 同 key 换新后仍 403 ⇒ 该 key 会话/额度窗口确实不可用，短暂冷却该 key 并
//     快速失败，不发第三种请求形态。
//   - pinKey（面板 Test）探测完全不触碰会话状态、不冷却、不重试：探测结论只属于
//     那次点击，绝不能污染生产 key 状态。
//
// 若 zen 收紧门禁（合法格式也被拒），冷却+快速失败会让 403 更早浮现，而换新
// 频率仍是日志里的门禁变化信号。

// zenSessionMarkSuccess 2xx 后清除该 key 的 403 印记（成功即该 key 现在可用，
// 后续偶发 403 重新按"本请求内换新一次"处理）。调用点在两条上游调用路径的 200
// 分支；对未标记的 key 是无害 no-op。
func zenSessionMarkSuccess(key string) {
	if key == "" {
		return
	}
	zenSessFailedMu.Lock()
	delete(zenSessFailed, key)
	zenSessFailedMu.Unlock()
}

// zenSessFailedMu/zenSessFailed 记录"换新后仍 403"的 key（tripwire/面板观测）。
// 只增不用于限流决策——限流决策在调用点用本请求的 sessionRetried 标志完成。
var (
	zenSessFailedMu sync.Mutex
	zenSessFailed   = map[string]int{}
)

// zenSessionFailed 记录该 key 在最近一次请求里"换新后仍 403"的失败印记，
// 供面板/日志观测（tripwire）。返回是否是新印记。
func zenSessionFailed(key string) {
	if key == "" {
		return
	}
	zenSessFailedMu.Lock()
	zenSessFailed[key]++
	zenSessFailedMu.Unlock()
}

// refreshZenSession 同步把该 key 的粘性会话换成本地铸造的新 ID（新会话、保留 UA）。
// 幂等且零成本（无子进程、无额度消耗）：FreeTier 403 的调用点在本请求内至多调用
// 一次，然后同 key 重试。key 为空/"public" 或无 key 配置时不动（哨兵无凭据）；
// 已从配置移除的 key 也不重建——否则 pruneZenKeyState 刚清掉的条目会被这里复活，
// 让 .zen-sessions.json 单调增长。
func refreshZenSession(key string) {
	if key == "" || key == "public" {
		return
	}
	if !zenKeyConfigured(key) {
		return
	}
	loadZenSessions()
	zenSessMu.Lock()
	e, ok := zenSessions[key]
	if !ok {
		// key 未在会话表里（例如首次即 403）：建一个新条目即可，下次请求复用。
		e = &zenSessionEntry{}
		zenSessions[key] = e
	}
	mintZenSessionEntryLocked(e, time.Now())
	saveZenSessionsLocked()
	sess := e.Session
	zenSessMu.Unlock()
	log.Printf("zen session refreshed locally for key#%d: %s", keyIndex(key), kit.Truncate(sess, 24))
}

// zenKeyConfigured key 是否仍在当前 zen key 池中。
func zenKeyConfigured(key string) bool {
	for _, k := range getZenConfig().Keys {
		if k == key {
			return true
		}
	}
	return false
}

// ============ 后台会话巡检（关键：空闲 key 也要保活/轮换） ============
//
// 为什么必须有后台巡检：pickZenKey 的第一轮只挑"已经有合法会话"的 key（见
// zen.go），而会话是在 key 被选中之后、由 StickyZenIdentity 惰性铸造的。这两者
// 互为条件就会自锁——key#1 被选中→铸造→永远满足第一轮，key#2/3 永不被选中→
// 永不铸造→永远被跳过。删除收割机后这个不变量就没人维护了：轮转事实上塌缩到
// 第一个 key，既让其余 key 的额度闲置，也让它们的会话永不轮换（用户看到的
// "过 2 小时不自动换会话"）。巡检每 key 只做一个 O(1) 的本地铸造/换新，不发
// 上游请求、不消耗额度、无子进程。
//
// 巡检同时覆盖两种状态：
//   - 无会话/格式非法的 key：立即铸造（让第一轮轮转重新认得它）；
//   - 年龄超过 SessionRotateMinutes 的 key：重铸（新会话，清 403 印记）。
//
// SessionRotateMinutes=0 时关闭"老化轮换"，但仍会为缺会话的 key 补铸——
// 否则轮转自锁问题会以另一种形式回来（空闲 key 依旧被饿死）。

// zenSessionRotateTick 巡检的基础节拍。轮换周期是分钟级，用 1 分钟轮询即可，
// 每分钟只做一次 map 遍历 + 少数几次本地铸造，开销可忽略。
const zenSessionRotateTick = time.Minute

// rotateZenSessionsOnce 巡检一轮：为池中每个配置 key 补齐/轮换会话。
// 返回 (新铸造数, 轮换数)，供日志与测试断言。
func rotateZenSessionsOnce() (minted, rotated int) {
	keys := getZenConfig().Keys
	if len(keys) == 0 {
		return 0, 0
	}
	iv := sessionRotateInterval()
	loadZenSessions()
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	now := time.Now()
	var rotatedKeys []string
	for _, k := range keys {
		if k == "" || k == "public" {
			continue // 哨兵无凭据，铸造会话只会污染文件
		}
		e := zenSessions[k]
		if e == nil {
			e = &zenSessionEntry{}
			zenSessions[k] = e
		}
		if !kit.ValidZenSessionID(e.Session) {
			mintZenSessionEntryLocked(e, now)
			e.Updated = now.Unix()
			minted++
			continue
		}
		if iv > 0 && e.CreatedAt > 0 && now.Sub(time.Unix(e.CreatedAt, 0)) >= iv {
			mintZenSessionEntryLocked(e, now)
			e.Updated = now.Unix()
			rotated++
			rotatedKeys = append(rotatedKeys, k)
		}
	}
	if minted > 0 || rotated > 0 {
		saveZenSessionsLocked()
	}
	if len(rotatedKeys) > 0 {
		// 轮换过的 key 清掉 403 失败印记（与请求路径语义一致）：新会话是干净状态。
		zenSessFailedMu.Lock()
		for _, k := range rotatedKeys {
			delete(zenSessFailed, k)
		}
		zenSessFailedMu.Unlock()
	}
	return minted, rotated
}

// startZenSessionRotator 启动后台会话巡检（每分钟一次），保证所有配置 key 都有
// 合法且新鲜的会话。与 startZenModelsRefresher 同形态：常驻 goroutine，进程退出
// 即结束；第一轮在启动后立即执行，冷启动时就把整个池的会话建起来。
func startZenSessionRotator() {
	go func() {
		if minted, rotated := rotateZenSessionsOnce(); minted > 0 || rotated > 0 {
			log.Printf("zen session rotator: initial pass minted=%d rotated=%d", minted, rotated)
		}
		ticker := time.NewTicker(zenSessionRotateTick)
		defer ticker.Stop()
		for range ticker.C {
			if !getZenConfig().Enabled {
				continue
			}
			minted, rotated := rotateZenSessionsOnce()
			if minted > 0 || rotated > 0 {
				log.Printf("zen session rotator: minted=%d rotated=%d", minted, rotated)
			}
		}
	}()
}
