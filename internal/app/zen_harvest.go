package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ zen 会话收割机（session harvester） ============
//
// 背景：zen 免费层按服务端会话绑定——只有服务端"见过"的 sess_ ID 才能通过
// FreeTier 检查，本地随机生成的 sess_ 必 403。网关自身无法凭空造出有效会话，
// 唯一能 mint 新会话的是官方 opencode CLI（`opencode run` 会在服务端注册
// 新 session，见其本地 sqlite/log）。
//
// 方案：镜像内嵌官方 CLI（二进制，见 Dockerfile）；网关调用它跑一条极小请求
//（transient run，不污染用户项目），从其日志里读出刚 mint 的 sess_，存入
// sticky 会话表。触发时机：
//  1. 启动时：key 无 live 会话 → 收割一个（避免首请求必 403）；
//  2. 运行时：某 key 连续 FreeTier 403 达阈值 → 后台收割新会话替换；
//  3. 定时：每 10 分钟检查一次，最久未 mint 超过 ZEN_HARVEST_INTERVAL_HOURS
//     （默认 4h）的 key 补一个；
//  4. 手动：管理面板「Force mint/refresh live session ids」→ 全池 mint，立即见效。
//
// 并行与隔离：**每个 key 一个独立 CLI HOME**（`ZEN_HARVEST_HOME/keys/<hash>`），
// auth.json 与 CLI 日志天然隔离，因此多 key 具备并行的条件（上限
// ZEN_HARVEST_CONCURRENCY）；但**默认 1（串行）**——CLI 是 Bun 二进制，每个
// 进程启动都 burst 一大块 CPU，小实例上并发会把每个 mint 都拖过预算（见
// harvestConcurrency 注释）。同一 key 始终串行（per-key 锁）。
// 早期实现共用单一 HOME + 全局锁：11 个 key 首启要串行数分钟（每个失败点还要
// 3 模型 × 3 次尝试），且每次收割都要覆盖再恢复管理员写在公共 HOME 里的
// auth.json——恢复一旦失败即丢掉管理员的真实凭据。per-key HOME 两个问题
// 一起消失：隔离使并发成为可能，且公共 HOME 完全不被触碰。
//
// 开关：ZEN_HARVEST=0 关闭（默认开启，需 CLI 存在）；ZEN_HARVEST_BIN 指定
// CLI 路径（默认 /app/bin/opencode）；ZEN_HARVEST_HOME 指定 CLI HOME 根
//（默认 /app/.opencode-home）；ZEN_HARVEST_INTERVAL_HOURS 默认 4（见 harvestInterval 注释）；
// ZEN_HARVEST_CONCURRENCY 默认 1（串行）。CLI 认证由收割机自给自足：每次收割把当前
// key 以单 key 形态写入该 key 专属 HOME 的 auth.json，跑完删除（不落盘留存）。

var (
	harvestMu        sync.Mutex
	harvestFails     = map[string]int{} // key -> 连续收割失败次数
	harvestLastTry   = map[string]time.Time{}
	harvestLastSweep = map[string]time.Time{} // key -> 最近一次周期补收尝试
)

// per-key 串行锁：同 key 的两次收割不能并发（会互相踩会话表与日志判读）。
// 不同 key 之间无共享资源（各自 HOME），无需全局锁。
var (
	harvestKeyMu    sync.Mutex
	harvestKeyLocks = map[string]*sync.Mutex{}
)

// 收割候选模型缓存：big-pickle 首选 + 至多 2 个动态免费兜底，缓存 1h。
var (
	mintModelsMu    sync.Mutex
	mintModelsCache []string
	mintModelsAt    time.Time
)

// harvestEnabled 是否启用收割机：显式 ZEN_HARVEST=0 关闭；
// CLI 二进制不存在时自动降级（日志提示一次）。
func harvestEnabled() bool {
	if strings.TrimSpace(os.Getenv("ZEN_HARVEST")) == "0" {
		return false
	}
	if _, err := os.Stat(harvestBin()); err != nil {
		return false
	}
	return true
}

func harvestBin() string {
	if p := strings.TrimSpace(os.Getenv("ZEN_HARVEST_BIN")); p != "" {
		return p
	}
	return "/app/bin/opencode"
}

// harvestHome 容器内 CLI 的 HOME 根：auth.json 与 sqlite 都落在这里，
// 与 DATA_DIR 分开（DATA_DIR 是网关状态，HOME 是 CLI 身份）。
func harvestHome() string {
	if h := strings.TrimSpace(os.Getenv("ZEN_HARVEST_HOME")); h != "" {
		return h
	}
	return "/app/.opencode-home"
}

// harvestHomeForKey 单个 key 的 CLI HOME（并发隔离的关键）。
//
// 用 key 的 SHA-256 前缀命名，绝不把 key 本身写进路径——路径会出现在日志、
// 进程列表与 `ls` 输出里。同一 key 恒得同一目录（收割是幂等的覆盖写）。
func harvestHomeForKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(harvestHome(), "keys", hex.EncodeToString(sum[:6]))
}

// harvestAuthPath 指定 HOME 的 auth.json 路径（CLI 按 XDG_DATA_HOME 查找）。
func harvestAuthPath(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

// harvestConcurrency 并行收割上限（默认 1，串行）。CLI 是 Bun 运行时，每个
// 进程启动时会 burst 一大块 CPU 与内存；1-2 核的小实例上同时跑几个，单个
// mint 就会被拖到慢于每 key 预算（实测 0.5 核容器里 4 个并发让单次 mint 从
// ~15s 涨到 70-130s），表现为"全部 key 都 no session minted"，看起来像
// 收割完全坏掉。串行慢一点但每个 key 都能在预算内跑完。范围 1..8，只有在
// 实例确有富余 CPU/内存时才调高（ZEN_HARVEST_CONCURRENCY）。
func harvestConcurrency() int {
	var n int
	if v := strings.TrimSpace(os.Getenv("ZEN_HARVEST_CONCURRENCY")); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			n = 0
		}
	}
	if n <= 0 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// harvestBatchTimeout 一批 mint 的整体上限。并发降到 1 之后，一批的耗时是
// "每 key 预算 × key 数"（11 个 key 的失败路径最长 11×150s）——固定 15 分钟
// 会把批次从中间砍断，排在后面的 key 连尝试机会都没有。按实际工作量推算，
// 再给 2 分钟余量，上下夹在 15 分钟与 45 分钟之间。
func harvestBatchTimeout(nKeys int) time.Duration {
	workers := harvestConcurrency()
	if workers < 1 {
		workers = 1
	}
	rounds := (nKeys + workers - 1) / workers
	d := time.Duration(rounds)*harvestKeyBudget() + 2*time.Minute
	if d < 15*time.Minute {
		d = 15 * time.Minute
	}
	if d > 45*time.Minute {
		d = 45 * time.Minute
	}
	return d
}

// harvestSem 全局并发额度（进程内惰性初始化，运行时改环境变量不生效）。
var (
	harvestSemOnce sync.Once
	harvestSem     chan struct{}
)

func acquireHarvestSlot(ctx context.Context) error {
	harvestSemOnce.Do(func() {
		harvestSem = make(chan struct{}, harvestConcurrency())
	})
	select {
	case harvestSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseHarvestSlot() { <-harvestSem }

// lockHarvestKey 取得某 key 的串行锁，返回释放函数。
func lockHarvestKey(key string) func() {
	harvestKeyMu.Lock()
	mu, ok := harvestKeyLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		harvestKeyLocks[key] = mu
	}
	harvestKeyMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// harvestInterval 定时收割间隔（默认 4h，最小 1h）。
//
// 为什么是 4 而不是 6：zen 免费层按出口 IP 记账（实测 200 req/5h per IP），
// 服务端只认得"见过的"会话；会话寿命的上界疑似就是这个 5h 额度窗口。间隔设在
// 窗口之上（旧默认 6h）意味着每轮都有一段时间**全池会话已过期**：请求开始 403，
// 只能靠 harvestOnForbidden 逐个补救（每 key 需 2 次连续 403，且同 key 10 分钟
// 冷却），那段时间的失败率与首字节延迟都会抬起来。4h 留出余量：会话过期前就换新，
// 死窗口不再出现。
//
// 不要再往短了调（1h 之类）：收割 run 为了走 CLI 自己的注册握手**直连公网**，
// 出口就是容器自己的 IP——那个 IP 同样按 200/5h 记账（若该 IP 还提供 socks5
// 出口，收割流量会和请求流量抢同一个额度桶）。11 个 key × 每 4h 一轮 ≈ 66 次
// mint/天，相对 960/天 的桶是零头；改成 1h 就是 264 次/天，开始有实际成本。
func harvestInterval() time.Duration {
	var hours int
	if n, err := fmt.Sscanf(strings.TrimSpace(os.Getenv("ZEN_HARVEST_INTERVAL_HOURS")), "%d", &hours); err == nil && n == 1 && hours >= 1 {
		return time.Duration(hours) * time.Hour
	}
	return 4 * time.Hour
}

// harvestKeyBudget 单个 key 的收割总预算（默认 150s）。
//
// 必须封顶：一次收割要遍历至多 3 个候选模型 × 每个 3 次尝试 × 每次 60s 超时，
// 失败路径理论最坏 9 分钟——并行收割时这会把一个 worker 长期占住，11 个失败
// key 就把整个池子拖到半小时以上（面板按钮超时、首启窗口内请求持续 403）。
// 成功路径只需 10-15s，150s 足够，且与 harvestOnForbidden 的既有超时一致。
func harvestKeyBudget() time.Duration {
	var secs int
	if n, err := fmt.Sscanf(strings.TrimSpace(os.Getenv("ZEN_HARVEST_KEY_TIMEOUT_SECONDS")), "%d", &secs); err == nil && n == 1 && secs >= 30 {
		return time.Duration(secs) * time.Second
	}
	return 150 * time.Second
}

// harvestRunFn 执行一次 CLI mint run（测试 seam：注入桩即可在无 CLI 的
// 机器上验证收割逻辑，不必依赖 185MB 的 Bun 二进制）。
// harvestRunResult 一次 CLI mint run 的结果。必须留下 CLI 自己的输出：只有
// "no session minted" 这一句话时，无法区分"CLI 根本没起来""被并发拖慢到超时
// 被杀""上游拒绝"——线上排查只能靠猜（2026-09-18 小实例全池 mint 失败就是
// 这样被误判成"收割机坏了"）。
type harvestRunResult struct {
	ExitCode int
	Err      error
	Elapsed  time.Duration
	Output   string // stdout+stderr 末尾若干字节（已去控制字符、截断）
}

var harvestRunFn = runHarvestCLI

// runHarvestCLI 跑一条最小 run。只为在服务端 mint session，不关心回答内容
// （回答可能因 key 配额 429，但 session 在 run 开始即已创建）。
// 收割 run 不走任何代理：直连公网（容器须能直连 opencode.ai；代理池是给
// 网关上游用的，收割是 CLI 自己的注册握手）。
func runHarvestCLI(ctx context.Context, bin, home, model string) harvestRunResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, "run",
		"--model", model,
		"Reply with exactly: OK")
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"ALL_PROXY=",
		"all_proxy=",
		"NO_PROXY=*",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// CLI 的子孙进程会继承 stdout/stderr：ctx 到期只 kill 直接子进程，若孙进程
	// 还活着，cmd.Run 会一直等管道关闭（实测 `timeout 90` 之后 CLI 仍存活十几
	// 分钟）。WaitDelay 让 Go 在 kill 后最多再等这么久就关掉管道返回，保证单次
	// run 不会拖过预算。
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	res := harvestRunResult{Err: err, Elapsed: time.Since(start), ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	res.Output = harvestOutputTail(stdout.String() + stderr.String())
	return res
}

// harvestOutputTail 把 CLI 输出压成适合进日志/面板的一行：去 ANSI 控制字符、
// 折叠空白、截到 400 字节（够看出错误信息，不会把日志刷爆）。
// 同时redact 掉形如 key 的串：CLI 报错时可能把 auth 内容带出来。
func harvestOutputTail(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0x1b:
			continue // ANSI 转义引导符
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20:
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = harvestKeyPattern.ReplaceAllString(out, "sk-***")
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

// harvestKeyPattern 匹配 zen key 形态的串（sk- 后跟足够长的字符），
// 用于把 CLI 输出里可能夹带的凭据抹掉——日志/面板/错误信息都不该出现 key。
var harvestKeyPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)

// harvestRunTimeout 单次 CLI 尝试的上限。每次尝试各自计时：早期实现三次尝试
// 共用一个 60s ctx，第一次卡住就把后两次的时间吃光，"重试"名存实亡。
func harvestRunTimeout() time.Duration { return 60 * time.Second }

// harvestAttemptNote 把一次尝试的结果整理成日志/错误里的一段（CLI 输出附在冒号后）。
func harvestAttemptNote(res harvestRunResult) string {
	note := fmt.Sprintf("exit=%d err=%v elapsed=%.1fs", res.ExitCode, res.Err, res.Elapsed.Seconds())
	if res.Output != "" {
		note += ": " + res.Output
	}
	return note
}

// harvestNoSessionErr 组装"没拿到会话"的错误。真实 run 与桩都会留下 Elapsed，
// 所以零值结果只能意味着**一次 CLI 都没跑**（每 key 预算在第一次尝试前就耗尽，
// 比如批次超时 / 任务取消正好卡在写 auth.json 与首次尝试之间）。这种情况绝不
// 能报 `exit=0 err=<nil>`——那读起来像"CLI 成功但上游没给会话"，会把人引到
// 上游去查，而这正是本次改动要消掉的那类误导。
func harvestNoSessionErr(models []string, last harvestRunResult) error {
	// 逐字段判断：直接比较整个结构体会去比较 Err 接口，动态类型不可比较时 panic。
	if last.Elapsed == 0 && last.Err == nil && last.ExitCode == 0 && last.Output == "" {
		return fmt.Errorf("harvest: no session minted (no CLI run started: per-key budget %s was already exhausted; tried %s)",
			harvestKeyBudget(), strings.Join(models, ", "))
	}
	return fmt.Errorf("harvest: no session minted (tried %s; last CLI %s)",
		strings.Join(models, ", "), harvestAttemptNote(last))
}

// harvestSession 为指定 key 收割一个新 live 会话：
// 用该 key 的 zen token 写入**该 key 专属** HOME 的 auth.json，跑最小 run，
// 从 CLI 日志中提取本次 mint 的 sess_，写入 sticky 表。
// 返回新 session，失败返回错误（调用方保留旧会话继续）。
func harvestSession(ctx context.Context, key string) (string, error) {
	if key == "" || key == "public" {
		// "public" 是"无 key"哨兵，不是真凭据，无法 mint。
		return "", fmt.Errorf("harvest: no usable key")
	}
	if isConsoleKey(key) {
		return "", fmt.Errorf("harvest: console token does not need session harvesting")
	}
	unlock := lockHarvestKey(key)
	defer unlock()
	if err := acquireHarvestSlot(ctx); err != nil {
		return "", fmt.Errorf("harvest: %w", err)
	}
	defer releaseHarvestSlot()

	// 单 key 总预算：失败路径（9 次 CLI 尝试 × 60s）绝不能占住 worker 9 分钟。
	ctx, cancel := context.WithTimeout(ctx, harvestKeyBudget())
	defer cancel()

	bin := harvestBin()
	home := harvestHomeForKey(key)
	authPath := harvestAuthPath(home)

	// 1. 写认证。per-key HOME 由收割机独占，理论上没有"别人的" auth.json，
	// 但同 key 重复收割会留下上一次的——仍然备份并在结束后恢复/删除，保证
	// 凭据不长期落盘。失败路径也必须恢复：WriteFile 会先截断原文件。
	var prevAuth []byte
	if b, err := os.ReadFile(authPath); err == nil {
		prevAuth = b
	}
	defer func() {
		if prevAuth != nil {
			_ = os.WriteFile(authPath, prevAuth, 0600)
		} else {
			_ = os.Remove(authPath)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(authPath), 0700); err != nil {
		return "", fmt.Errorf("harvest mkdir: %w", err)
	}
	authObj := map[string]any{
		"opencode": map[string]any{"type": "api", "key": key},
	}
	ab, _ := json.Marshal(authObj)
	if err := os.WriteFile(authPath, ab, 0600); err != nil {
		return "", fmt.Errorf("harvest write auth: %w", err)
	}

	// 2. 跑最小 run：transient 会话 + 免费模型。首选 opencode/big-pickle
	//（zen 免费层默认模型别名），失败则依次尝试至多 2 个动态获取的价格 0
	// 模型兜底（big-pickle 未来下架后收割不中断，见 harvestMintModels）。
	before := latestHarvestSession(home)
	sess := ""
	var last harvestRunResult
	models := harvestMintModels()
	for _, model := range models {
		if sess != "" {
			break
		}
		for i := 0; i < 3 && sess == ""; i++ {
			if ctx.Err() != nil {
				break // 每 key 预算耗尽，再试也是立刻失败
			}
			runCtx, cancel := context.WithTimeout(ctx, harvestRunTimeout())
			res := harvestRunFn(runCtx, bin, home, model)
			cancel()
			last = res
			if s := latestHarvestSession(home); s != "" && s != before {
				sess = s
				break
			}
			log.Printf("zen harvest: key#%d attempt %d (%s) minted nothing — %s",
				keyIndex(key), i+1, model, harvestAttemptNote(res))
		}
	}
	if sess == "" {
		return "", harvestNoSessionErr(models, last)
	}

	// 3. 写入 sticky 表（标记 CLI mint 与会话新鲜度：启动扫描不再跳过、
	// 周期收割按 HarvestedAt 判断而非每次请求刷新的 Updated）
	loadZenSessions()
	zenSessMu.Lock()
	e, ok := zenSessions[key]
	if !ok || e == nil {
		e = &zenSessionEntry{UA: zenNativeUA}
		zenSessions[key] = e
	}
	e.Session = sess
	e.Minted = true
	e.HarvestedAt = time.Now().Unix()
	zenSessMu.Unlock()
	saveZenSessions()
	log.Printf("zen harvest: key#%d minted live session %s", keyIndex(key), kit.Truncate(sess, 24))
	return sess, nil
}

// harvestMintModels 收割候选模型（按尝试顺序）：
//  1. opencode/big-pickle —— zen 免费层默认模型别名，硬编码首选；
//  2. 至多 2 个动态获取的价格 0 模型 —— `opencode models`（无认证）在线
//     列表 ∩ 公共目录 api.json 价格门（cost.input==0 && cost.output==0 且
//     status 非 deprecated），与 syncZenModels 的价格门同规则。big-pickle
//     未来下架后自动落到其余免费模型，收割不中断。
//
// 列表缓存 1h（收割低频，模型目录变化以天计）；CLI/目录不可达时回退到
// 当前已知免费模型的静态表（big-pickle 之外挑两个稳定的）。
func harvestMintModels() []string {
	mintModelsMu.Lock()
	defer mintModelsMu.Unlock()
	if len(mintModelsCache) > 0 && time.Since(mintModelsAt) < time.Hour {
		return mintModelsCache
	}
	live := cliModelIDs()
	free := registryFreeModelIDs()
	var fallback []string
	for _, id := range live {
		if id == "opencode/big-pickle" {
			continue // 首选已在前列，不必重复尝试
		}
		bare := strings.TrimPrefix(id, "opencode/")
		if free[id] || free[bare] {
			fallback = append(fallback, id)
			if len(fallback) == 2 {
				break
			}
		}
	}
	if len(fallback) == 0 {
		// 动态获取失败或免费模型全下架：静态兜底当前已知免费模型
		fallback = []string{"opencode/ling-3.0-flash-fin-free", "opencode/mimo-v2.5-free"}
	}
	mintModelsCache = append([]string{"opencode/big-pickle"}, fallback...)
	mintModelsAt = time.Now()
	log.Printf("zen harvest: mint models -> %s", strings.Join(mintModelsCache, ", "))
	return mintModelsCache
}

// cliModelIDs 通过 `opencode models`（无认证）取当前在线模型 ID 列表
// （形如 opencode/xxx 一行一个）。只读操作，用公共 HOME。
func cliModelIDs() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	home := harvestHome()
	cmd := exec.CommandContext(ctx, harvestBin(), "models")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
	// 与 runHarvestCLI 同理：子孙进程继承管道时，ctx 到期只杀得掉直接子进程，
	// CombinedOutput 会一直等管道关闭。这条调用还额外持有 mintModelsMu（收割
	// 模型列表缓存），一旦挂住就是整批 key 全卡在锁上——同样用 WaitDelay 兜底。
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

// registryFreeModelIDs 公共目录 api.json 的价格门：cost 0/0 且非 deprecated
// 的模型 ID 集合（与 syncZenModels 同规则；目录不可达返回空集）。
// 实现复用 fetchZenRegistry（zen.go），避免价格门逻辑漂移。
func registryFreeModelIDs() map[string]bool {
	_, free, _ := fetchZenRegistry()
	return free
}

// latestHarvestSession 从指定 HOME 的 CLI 日志读最新创建的 session ID。
// 路径：$HOME/.local/share/opencode/log/opencode.log，行含
// `message=created id=ses_...`（实测 2026-09-17）。
// per-key HOME 隔离后，这里读到的一定是本 key 自己那次 run 的会话。
func latestHarvestSession(home string) string {
	logPath := filepath.Join(home, ".local", "share", "opencode", "log", "opencode.log")
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	latest := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "message=created id=ses_") {
			continue
		}
		if i := strings.Index(line, "id=ses_"); i >= 0 {
			rest := line[i+3:]
			j := 0
			for j < len(rest) && isSessChar(rest[j]) {
				j++
			}
			if j > 4 {
				latest = rest[:j]
			}
		}
	}
	return latest
}

func isSessChar(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// saveZenSessions 对外持久化（harvester 用；内部写已在锁外调用）。
func saveZenSessions() {
	zenSessMu.Lock()
	defer zenSessMu.Unlock()
	saveZenSessionsLocked()
}

// ============ 批量 mint（启动 / 定时 / 面板手动共用一个实现） ============

// zenMintOutcome 单个 key 的 mint 结果（管理面板展示用；Key 字段不外泄到 API）。
type zenMintOutcome struct {
	Index     int
	Key       string
	KeyMask   string
	OK        bool
	Skipped   bool
	Done      bool // 该 key 已处理完（未完成的条目在面板显示 pending）
	Session   string
	Err       string
	ElapsedMS int64
}

// mintZenSessions 并行 mint 一批 key（并发上限 harvestConcurrency）。
// force=false 时跳过已有 live 会话的 key（快且不浪费配额）；force=true 一律重 mint
// （面板「Force」按钮语义；mint 失败不会覆盖旧会话，见 harvestSession）。
// 结果按传入顺序对齐，每个 key 恰好一个 outcome；progress 非空时每完成一个
// key 回调一次（面板进度条）。
func mintZenSessions(ctx context.Context, keys []string, force bool, progress func(idx int, o zenMintOutcome)) []zenMintOutcome {
	out := make([]zenMintOutcome, len(keys))
	todo := make([]int, 0, len(keys))
	for i, k := range keys {
		out[i] = zenMintOutcome{Index: i, Key: k, KeyMask: maskZenKey(k), Done: true}
		if k == "" || k == "public" {
			out[i].Skipped = true
			out[i].Err = "no usable key"
			continue
		}
		if isConsoleKey(k) {
			out[i].Skipped = true
			out[i].Err = "console oauth (no CLI mint needed)"
			continue
		}
		if !force && zenSessionLive(k) {
			out[i].Skipped = true
			continue
		}
		out[i].Done = false
		todo = append(todo, i)
	}
	// 跳过项也报一次进度（含"全部跳过"的情形，所以放在提前返回之前）：
	// 否则调用方（面板任务）永远等不到它们，那些 key 会一直显示 pending，
	// done 计数也永远追不上 total。
	reportSkipped := func() {
		if progress == nil {
			return
		}
		for i := range out {
			if out[i].Done {
				progress(i, out[i])
			}
		}
	}
	if len(todo) == 0 {
		reportSkipped()
		return out
	}
	reportSkipped()

	workers := harvestConcurrency()
	if workers > len(todo) {
		workers = len(todo)
	}
	var next int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt32(&next, 1)) - 1
				if i >= len(todo) {
					return
				}
				idx := todo[i]
				start := time.Now()
				if ctx.Err() != nil {
					out[idx].Err = "canceled"
				} else {
					sess, err := harvestSession(ctx, out[idx].Key)
					out[idx].ElapsedMS = time.Since(start).Milliseconds()
					if err != nil {
						out[idx].Err = err.Error()
					} else {
						out[idx].OK = true
						out[idx].Session = kit.Truncate(sess, 24)
					}
				}
				out[idx].Done = true
				if progress != nil {
					progress(idx, out[idx])
				}
			}
		}()
	}
	wg.Wait()
	return out
}

// ============ 后台 mint 任务（面板按钮：立即返回 + 轮询进度） ============

// zenMintJob 一次手动 mint 的进度快照。任务在后台跑，HTTP 立即返回——
// 11 个 key 全量重 mint 要几十秒，同步响应会撞上反向代理的超时。
type zenMintJob struct {
	Force     bool
	StartedAt time.Time
	Total     int
	outcomes  []zenMintOutcome
	done      int32
	finished  bool
}

var (
	zenMintJobMu  sync.Mutex
	zenMintJobCur *zenMintJob
)

// startZenMintJob 启动一次全池 mint。已有任务在跑时返回 false（单飞），
// 不排新任务——重复点击只会干扰进度显示。
func startZenMintJob(keys []string, force bool) (started bool, state map[string]any) {
	zenMintJobMu.Lock()
	if zenMintJobCur != nil && !zenMintJobCur.finished {
		snap := zenMintJobSnapshotLocked(zenMintJobCur)
		zenMintJobMu.Unlock()
		return false, snap
	}
	// 预填占位条目：任务刚开始时面板要能显示"哪些 key 还在排队"，
	// 否则未完成的槽位是零值（index 0、无 keyMask），表格里会全渲染成 #1。
	outcomes := make([]zenMintOutcome, len(keys))
	for i, k := range keys {
		outcomes[i] = zenMintOutcome{Index: i, Key: k, KeyMask: maskZenKey(k)}
	}
	job := &zenMintJob{
		Force:     force,
		StartedAt: time.Now(),
		Total:     len(keys),
		outcomes:  outcomes,
	}
	zenMintJobCur = job
	// 快照必须在临界区内取：goroutine 一起来就会在锁内写 job.outcomes，
	// 在锁外调用 zenMintJobSnapshotLocked 读同一份切片就是数据竞争。
	snap := zenMintJobSnapshotLocked(job)
	zenMintJobMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("zen mint job panicked: %v", r)
			}
			zenMintJobMu.Lock()
			job.finished = true
			zenMintJobMu.Unlock()
		}()
		// 整体上限按实际工作量推算（并发 1 时就是 key 数 × 每 key 预算）。
		ctx, cancel := context.WithTimeout(context.Background(), harvestBatchTimeout(len(keys)))
		defer cancel()
		res := mintZenSessions(ctx, keys, force, func(idx int, o zenMintOutcome) {
			zenMintJobMu.Lock()
			job.outcomes[idx] = o
			zenMintJobMu.Unlock()
			atomic.AddInt32(&job.done, 1)
		})
		ok, fail, skip := 0, 0, 0
		for _, o := range res {
			switch {
			case o.OK:
				ok++
			case o.Skipped:
				skip++
			default:
				fail++
			}
		}
		log.Printf("zen mint job: %d ok, %d failed, %d skipped (force=%v)", ok, fail, skip, force)
	}()
	return true, snap
}

// zenMintJobSnapshotLocked 组装进度快照（调用方持 zenMintJobMu）。
func zenMintJobSnapshotLocked(job *zenMintJob) map[string]any {
	results := make([]map[string]any, 0, len(job.outcomes))
	for _, o := range job.outcomes {
		results = append(results, map[string]any{
			"index":     o.Index,
			"keyMask":   o.KeyMask,
			"ok":        o.OK,
			"skipped":   o.Skipped,
			"done":      o.Done,
			"session":   o.Session,
			"error":     o.Err,
			"elapsedMs": o.ElapsedMS,
		})
	}
	return map[string]any{
		"running":   !job.finished,
		"force":     job.Force,
		"startedAt": job.StartedAt.Format(time.RFC3339),
		"total":     job.Total,
		"done":      int(atomic.LoadInt32(&job.done)),
		"results":   results,
		"finished":  job.finished,
	}
}

// zenMintJobStatus 当前（或最后一次）mint 任务进度；从未跑过返回 nil。
func zenMintJobStatus() map[string]any {
	zenMintJobMu.Lock()
	defer zenMintJobMu.Unlock()
	if zenMintJobCur == nil {
		return nil
	}
	return zenMintJobSnapshotLocked(zenMintJobCur)
}

// ============ 触发路径 ============

// periodicBatchMu 保证同一时间只有一个"定时补收"批次在跑。检查间隔已缩到
// 10 分钟，而一个批次（11 个 key、含失败重试）最坏可能跑到十几分钟——不挡住
// 会叠起第二个批次：stale 名单在批次开始前算好，上一批还没写回 HarvestedAt，
// 于是同一批 key 被重复收割（白烧出口 IP 的额度）。
var periodicBatchMu sync.Mutex

// startZenHarvester 启动定时收割循环：每 10 分钟检查一次，为"从未 mint 成功"
// 或"最久未 mint 超过间隔"的 key 补收（并行，受 harvestConcurrency 约束）。
// 从未 mint 成功的 key 按 interval 节流，不会每个 tick 重试。
//
// 检查频率必须明显高于 harvestInterval：间隔到与真正执行之间会差一个 tick，
// 4h 目标配 1h ticker 实际落在 4h-5h——正好顶到 5h 额度窗口的边。10 分钟
// ticker 把误差压到 10 分钟，刷新稳稳发生在窗口之内。
func startZenHarvester() {
	go func() {
		// 启动时先给无会话的 key 收割（错开 10s，避免与 model sync 抢资源）
		time.Sleep(10 * time.Second)
		harvestMissingSessions()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if !harvestEnabled() {
				continue
			}
			cfg := getZenConfig()
			if !cfg.Enabled {
				continue
			}
			interval := harvestInterval()
			if interval < time.Hour {
				interval = time.Hour
			}
			stale := periodicSweepCandidates(cfg.Keys, interval)
			if len(stale) == 0 {
				continue
			}
			if !periodicBatchMu.TryLock() {
				continue // 上一批还没跑完，不必排队（下一 tick 会重算 stale）
			}
			// 盖章必须在拿到批次之后：TryLock 失败时这些 key 根本没被试过，
			// 提前盖上会让它们平白推迟一个 interval 才再入选。
			markSweepTried(stale)
			ctx, cancel := context.WithTimeout(context.Background(), harvestBatchTimeout(len(stale)))
			// force=true：这些 key 是"按新鲜度挑出来的"，正是要重 mint。
			mintZenSessions(ctx, stale, true, nil)
			cancel()
			periodicBatchMu.Unlock()
		}
	}()
}

// periodicSweepCandidates 周期补收名单：从未 mint 成功或最久未 mint 超过
// interval 的 key。
//
// 新鲜度只看 HarvestedAt（CLI 实际 mint 成功的时间），不能回退用 Updated：
// 后者每次请求都刷新，对活跃 key 恒新，等于"被请求过的 key 永不周期收割"；
// 而从未 mint 的本地随机占位恰恰是一直 403 的那一类，必须留在名单里。
// HarvestedAt==0 的 key 用 sweepDue 按 interval 节流，避免持续失败的 key
// 被 10 分钟一个的 ticker 变成无限重试、反复烧出口 IP 的额度。
func periodicSweepCandidates(keys []string, interval time.Duration) []string {
	var stale []string
	for _, k := range keys {
		if k == "" || k == "public" {
			continue
		}
		loadZenSessions()
		zenSessMu.Lock()
		e := zenSessions[k]
		var fresh int64
		if e != nil {
			fresh = e.HarvestedAt
		}
		zenSessMu.Unlock()
		if fresh > 0 && time.Since(time.Unix(fresh, 0)) < interval {
			continue
		}
		if !sweepDue(k, interval) {
			continue
		}
		stale = append(stale, k)
	}
	return stale
}

// harvestMissingSessions 启动时为无 live 会话的 key 收割（并行）。
func harvestMissingSessions() {
	if !harvestEnabled() {
		return
	}
	cfg := getZenConfig()
	if !cfg.Enabled {
		return
	}
	var missing []string
	for _, k := range cfg.Keys {
		if k == "" || k == "public" {
			continue
		}
		// 只跳过 CLI mint 过的会话；本地随机占位（启动竞态窗口内
		// StickyZenIdentity 创建）必须收割，否则该 key 永久 403。
		if zenSessionLive(k) {
			continue
		}
		missing = append(missing, k)
	}
	if len(missing) == 0 {
		return
	}
	log.Printf("zen harvest: minting %d key(s) at startup (concurrency %d)", len(missing), harvestConcurrency())
	ctx, cancel := context.WithTimeout(context.Background(), harvestBatchTimeout(len(missing)))
	defer cancel()
	failed := 0
	for _, o := range mintZenSessions(ctx, missing, false, nil) {
		if !o.OK && !o.Skipped {
			failed++
			log.Printf("zen harvest: key#%d startup harvest failed (%s), will retry on 403/later", o.Index+1, o.Err)
		}
	}
	// 整批全灭几乎总是环境问题（小实例上并发太快把 CLI 饿死、CLI 起不来），
	// 而不是 key 或上游的问题——把最可能的那条排查线索直接写进日志。
	if failed == len(missing) && len(missing) > 1 {
		log.Printf("zen harvest: all %d key(s) failed to mint — if this instance has few cores/RAM, keep ZEN_HARVEST_CONCURRENCY=1 (default); the CLI bursts CPU+memory per run", len(missing))
	}
}

// harvestOnForbidden 某 key 连续 FreeTier 403 时调用：阈值（默认连续 2 次）
// 达到后后台收割新会话。调用方以 `go harvestOnForbidden(key)` 触发，失败不阻塞请求。
// 前 1 次 403 只计数不收割（避免瞬态 403 触发 CLI 子进程）；连续第 2 次
// 才真正收割——403 路径不再做本地随机轮换（随机 sess_ 必 403，见
// zen_session.go 顶部注释），恢复唯一靠这里 mint 真会话。
func harvestOnForbidden(key string) {
	if !harvestEnabled() {
		return
	}
	if key == "" || key == "public" || isConsoleKey(key) {
		return
	}

	// 计数、阈值、冷却判定与时间戳必须在一个临界区内完成。拆成两段会让同一
	// 会话失效引发的 N 个并发 403 全部读到"还没收割过"，于是 N 个 goroutine
	// 各起一次 CLI 收割，把额度位排成一条长队。
	harvestMu.Lock()
	harvestFails[key]++
	n := harvestFails[key]
	if n < 2 || time.Since(harvestLastTry[key]) < 10*time.Minute {
		harvestMu.Unlock()
		return
	}
	harvestLastTry[key] = time.Now()
	harvestMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if _, err := harvestSession(ctx, key); err != nil {
		log.Printf("zen harvest: key#%d harvest failed (%v)", keyIndex(key), err)
		return
	}
	harvestMu.Lock()
	harvestFails[key] = 0
	harvestMu.Unlock()
}

// harvestMarkSuccess key 成功 200 后清零其连续失败计数。
func harvestMarkSuccess(key string) {
	harvestMu.Lock()
	delete(harvestFails, key)
	harvestMu.Unlock()
}

// sweepDue 周期补收的按 key 节流：距离上次补收尝试不足 interval 就不重复试。
// 只用于 HarvestedAt==0 的 key（从未 mint 成功），否则每 10 分钟一个 ticker
// 会把"这个 key 一直 mint 不上"变成每 10 分钟一次的无限重试。
func sweepDue(key string, interval time.Duration) bool {
	harvestMu.Lock()
	defer harvestMu.Unlock()
	last, ok := harvestLastSweep[key]
	return !ok || time.Since(last) >= interval
}

// markSweepTried 记录本批补收的尝试时间（在 mint 之前调用：这批可能跑十几分钟，
// 期间不该再从下一个 tick 挑出同一批 key）。
func markSweepTried(keys []string) {
	now := time.Now()
	harvestMu.Lock()
	defer harvestMu.Unlock()
	for _, k := range keys {
		harvestLastSweep[k] = now
	}
}

// pruneZenKeyState 配置变更后清理已移除 key 的运行时状态（会话粘性 + 收割计数）。
// valid 为当前有效 key 集合。
func pruneZenKeyState(valid map[string]bool) {
	harvestMu.Lock()
	for k := range harvestFails {
		if !valid[k] {
			delete(harvestFails, k)
		}
	}
	for k := range harvestLastTry {
		if !valid[k] {
			delete(harvestLastTry, k)
		}
	}
	for k := range harvestLastSweep {
		if !valid[k] {
			delete(harvestLastSweep, k)
		}
	}
	harvestMu.Unlock()

	// harvestKeyLocks 刻意不清理：这里的 per-key 锁可能正被在途 mint 持有，
	// 删掉条目后 lockHarvestKey 会为同一 key 新建一把锁——两个 goroutine
	// 就能同时 mint 同一 key（共用 per-key HOME，auth.json 与 CLI 日志互相
	// 覆盖，mint 结果串味）。条目数等于历史配置过的 key 数，量级可忽略。

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
