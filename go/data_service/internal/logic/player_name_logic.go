package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"data_service/internal/store"
	"data_service/internal/svc"

	"shared/playername"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// ── 玩家名字注册表 logic(设计 docs/design/guild-phase2/03-names.md §3.6)────────
//
// 本层只做**编排**,三件事各归其位:
//   - 规则(什么算合法、什么算同一个名字)→ shared/playername,login 与本服务共用同一份;
//   - 持久化与唯一性 → store.PlayerNameStore(唯一性只由 name_norm 上的 UNIQUE KEY 保证);
//   - 加速 → routing 的 player:name:{id} 缓存。
//
// 本层**不写一行 SQL**,也不自己判重。判重一旦在这里出现第二份实现(比如"先查缓存
// 有没有人占"),并发建角下必漏:两个请求会同时看到"没人占"。
//
// 【fail 方向】写侧(Reserve)fail-closed:库不可用就拒绝建角,宁可建不出号,也不能
// 让两个角色拿到同一个名字。读侧(BatchGet)对"查不到"fail-open —— 缺席不是错误,
// 名字只是展示数据;但对"库炸了"仍然返回错误,让调用方自己决定降级成空名还是重试。

var (
	// ErrPlayerNameStoreUnavailable:store 没装配起来(建表失败 / 连不上库)。
	// server 层映射成 Unavailable + ErrCodePlayerNameDBError。
	ErrPlayerNameStoreUnavailable = errors.New("player name store unavailable")

	// ErrPlayerNameConflict:这个 player_id 名下已经登记了**另一个**名字。
	// v1 没有改名功能,所以它只可能来自 player_id 复用(发号器被重置)这类事故。
	// 绝不能"顺手把旧的删了"——旧名字属于一个在役角色。server 层映射成
	// FailedPrecondition + ErrCodePlayerNameConflict,并留 ERROR 日志给人看。
	ErrPlayerNameConflict = errors.New("player already holds a different name")

	// ErrPlayerNameReleaseOutsideWindow:行存在,但登记时刻早于释放窗口下界,
	// 而本次调用没有 admin token。server 层映射成 FailedPrecondition +
	// ErrCodeAdminAuthRequired。这道窗拦的是"拿别人早就建好的号的名字来释放"。
	ErrPlayerNameReleaseOutsideWindow = errors.New("release outside window requires admin token")
)

// ── 指标 ───────────────────────────────────────────────────────
//
// 用 go-zero 的 metric 包(它内部 prom.MustRegister 到默认 registry,而 data_service
// 的 /metrics 用的正是 promhttp.Handler() 默认 gatherer,所以不需要额外注册)。
//
// Namespace/Subsystem 留空、名字一次写全:设计 §3.6 直接给出了 `data_service_` 开头的
// 完整指标名,拆成 Namespace="data_service" 虽然等价,但改 Namespace 常量时会连带改掉
// 一批别的指标名,不如让这三个名字自己独立。
//
// 【低基数纪律】label 只有 op(3 个取值)、result(6 个取值)。**绝不放 player_id 或
// 名字**:那是无上界的维度,会把 Prometheus 的时间序列打爆(AGENTS.md §9)。
var (
	playerNameOpsTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Name: "data_service_player_name_ops_total",
		Help: "玩家名字注册表操作结果。op: reserve|release|batch_get;" +
			"result: ok|taken|invalid|conflict|outside_window|error。",
		Labels: []string{"op", "result"},
	})

	playerNameOpSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Name: "data_service_player_name_op_seconds",
		Help: "玩家名字注册表单次操作耗时(秒),含缓存与 SQL。建角是同步路径," +
			"reserve 的 p99 直接进 CreatePlayer 的耗时预算。",
		Labels: []string{"op"},
		// 桶按"一次本地 SQL"的量级铺:1ms 是理想值,25ms 以内算健康,
		// 超过 250ms 说明全局库开始排队(建角会肉眼可见地卡)。
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	})

	playerNameCacheTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Name: "data_service_player_name_cache_total",
		Help: "BatchGetPlayerName 的缓存命中情况,按 id 计数。" +
			"result: hit(有名字)|negative_hit(缓存记着库里没有)|miss(要回源)。",
		Labels: []string{"result"},
	})
)

// op / result 的取值写成常量:label 值必须是有界集合,散在各处的字面量早晚会被拼错
// 成第 7 个取值(拼错不会报错,只会悄悄多出一条时间序列)。
const (
	playerNameOpReserve  = "reserve"
	playerNameOpRelease  = "release"
	playerNameOpBatchGet = "batch_get"
)

const (
	playerNameResultOK            = "ok"
	playerNameResultTaken         = "taken"
	playerNameResultInvalid       = "invalid"
	playerNameResultConflict      = "conflict"
	playerNameResultOutsideWindow = "outside_window"
	playerNameResultError         = "error"
)

const (
	playerNameCacheHit         = "hit"
	playerNameCacheNegativeHit = "negative_hit"
	playerNameCacheMiss        = "miss"
)

// observePlayerNameOp 返回一个给 defer 用的收尾函数。
//
// result 用**指针**传:各个 return 分支只要给那个变量赋值即可,不用在每个出口重复
// 写一遍指标调用。调用方把它初始化成 playerNameResultError —— 任何忘记赋值的分支
// (包括将来新加的)都会自动记成 error,方向是"宁可虚报故障,不可漏报"。
func observePlayerNameOp(op string, result *string) func() {
	start := time.Now()
	return func() {
		playerNameOpSeconds.ObserveFloat(time.Since(start).Seconds(), op)
		playerNameOpsTotal.Inc(op, *result)
	}
}

// ReservePlayerName 把 raw 归一化后登记到 playerID 名下。
//
// 返回值 result 取 playername.ReserveOK / ReserveTaken / ReserveInvalid,直接进
// ReservePlayerNameResponse.result;owner 仅在 ReserveTaken 时非 0。
//
// 【owner 的使用边界】它只给 login 判"上一次建角的响应丢了、这是同一个人在重试"
// (§3.11)。**禁止下发客户端**:下发等于送一个"按名字查 player_id"的接口。
//
// 【为什么先 Normalize 再看 store】不合规的名字连库都不该碰:那是纯计算,库挂着的
// 时候也能给出确定答案,顺序反过来只会让"名字打错字"在故障期变成"服务不可用"。
func ReservePlayerName(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, raw string) (uint32, uint64, error) {
	metricResult := playerNameResultError
	defer observePlayerNameOp(playerNameOpReserve, &metricResult)()

	if playerID == 0 {
		// 纵深防御:server 层已经挡过一次(§3.1 映射表 → InvalidArgument)。
		// 这里再挡是因为 player_id=0 一旦插进表里,就是一行永远没有主人、
		// 也永远不会被释放的记录,而它照样占着一个名字。
		metricResult = playerNameResultInvalid
		return 0, 0, fmt.Errorf("%w (reserve player_id=0)", store.ErrPlayerNameInvalidArgument)
	}

	// 只做**结构**校验(UTF-8、1–32 字、字符集、敏感词)。
	// 玩法长度(配表 RoleNameRule 的 2–12)由 login 把关:data_service 是全局服务,
	// 不加载 zone 的玩法配表,在这里复述一遍配表数值只会制造第二个真源。
	display, norm, verdict := playername.Normalize(raw, playername.StructuralRules)
	if verdict != playername.VerdictOK {
		metricResult = playerNameResultInvalid
		// 日志不打 raw 原文:它是玩家输入的任意串,直接进日志会把控制字符/换行带进来。
		// display 已经过 NFKC + TrimSpace,Invalid/Empty 时它本来就是空串。
		logx.Infof("[player-name] reserve rejected player_id=%d verdict=%s display=%q",
			playerID, verdict, display)
		return playername.ReserveInvalid, 0, nil
	}

	if svcCtx == nil || svcCtx.PlayerNameStore == nil {
		return 0, 0, ErrPlayerNameStoreUnavailable
	}

	// nowMs 由本层显式给出:释放窗口靠 created_ms 这一列,时间不能藏在库里
	// (库时钟与本进程时钟不同步时,窗口会算错方向)。
	nowMs := uint64(time.Now().UnixMilli())
	outcome, owner, err := svcCtx.PlayerNameStore.Reserve(ctx, playerID, display, norm, nowMs)
	if err != nil {
		return 0, 0, fmt.Errorf("reserve player name for player %d: %w", playerID, err)
	}

	switch outcome {
	case store.ReserveInserted, store.ReserveAlreadyOwned:
		metricResult = playerNameResultOK
		// 走到这里数据**已经提交**(store 的单条 INSERT 自己就是提交点),
		// 所以现在写缓存是安全的:不会缓存一个还可能回滚的名字。
		//
		// 缓存写失败绝不改变返回值:缓存是加速不是真相,让它把一次已经成功的建角
		// 翻成失败,才是真正的故障放大。只留 ERROR 日志 + 指标。
		cachePlayerNameBestEffort(ctx, svcCtx, playerID, display)
		return playername.ReserveOK, 0, nil

	case store.ReserveTaken:
		metricResult = playerNameResultTaken
		return playername.ReserveTaken, owner, nil

	case store.ReserveConflict:
		// store 已经打了带旧 norm 的 ERROR 日志(它比本层多知道一条信息)。
		metricResult = playerNameResultConflict
		return 0, 0, fmt.Errorf("%w: player_id=%d requested %q", ErrPlayerNameConflict, playerID, norm)

	default:
		// store 新增了终局而本层没跟上。当故障处理,不猜语义。
		return 0, 0, fmt.Errorf("unexpected reserve outcome %d for player %d", uint8(outcome), playerID)
	}
}

// cachePlayerNameBestEffort 回填正缓存,失败只记日志。抽出来是因为 Reserve 与
// BatchGet 的回填口径必须完全一致(同一个 TTL、同样的"失败不影响结果")。
func cachePlayerNameBestEffort(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, name string) {
	if svcCtx.Router == nil {
		return
	}
	if err := svcCtx.Router.SetPlayerNames(ctx,
		map[uint64]string{playerID: name}, svcCtx.Config.PlayerName.CacheTTL); err != nil {
		logx.Errorf("[player-name] cache set failed for player_id=%d (name is already committed in MySQL, "+
			"read path will refill on next BatchGet): %v", playerID, err)
	}
}

// ReleasePlayerName 条件释放 playerID 名下的 raw 这个名字。
//
// admin=true 表示 server 层已经过了 authorizeAdmin,本次不限登记时间(运维按日志
// 清孤儿);admin=false 是 login 的建角补偿路径,只能删 ReleaseWindow 之内的登记。
//
// 【幂等】行不存在返回 nil。login 的补偿会发两次(立即一次 + 约 10s 后一次,
// 防"Release 先于在途 INSERT 提交"的竞态),第二次必然打空。
func ReleasePlayerName(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, raw string, admin bool) error {
	metricResult := playerNameResultError
	defer observePlayerNameOp(playerNameOpRelease, &metricResult)()

	if playerID == 0 {
		metricResult = playerNameResultInvalid
		return fmt.Errorf("%w (release player_id=0)", store.ErrPlayerNameInvalidArgument)
	}

	_, norm, verdict := playername.Normalize(raw, playername.StructuralRules)
	switch verdict {
	case playername.VerdictOK:
		// 正常路径。
	case playername.VerdictSensitive:
		// **刻意放行**。Reserve 会拒绝敏感词,所以表里本不该有这种名字;但敏感词表
		// 是可替换的(playername.DefaultSensitive),换一版词库之后,早先合法登记的
		// 名字可能变成"敏感"。这时如果连释放都做不了,那条孤儿行就永远清不掉了。
		// 释放只需要 norm 这一个判重键,敏感与否对删除语义没有任何影响。
		logx.Infof("[player-name] releasing a name that the current word list flags as sensitive "+
			"(reserve would reject it today): player_id=%d admin=%v", playerID, admin)
	default:
		// VerdictEmpty / VerdictInvalid:归一化后为空或非法,norm 是空串,
		// 传给 store 只会撞 ErrPlayerNameInvalidArgument。在这里就拒,
		// 对应 §3.1 映射表的 "Release 的 name 归一化后为空或非法 → InvalidArgument"。
		metricResult = playerNameResultInvalid
		return fmt.Errorf("%w (release player_id=%d verdict=%s)",
			store.ErrPlayerNameInvalidArgument, playerID, verdict)
	}

	if svcCtx == nil || svcCtx.PlayerNameStore == nil {
		return ErrPlayerNameStoreUnavailable
	}

	minCreatedMs := releaseWindowFloorMs(svcCtx, admin, time.Now())

	outcome, err := svcCtx.PlayerNameStore.Release(ctx, playerID, norm, minCreatedMs)
	if err != nil {
		return fmt.Errorf("release player name for player %d: %w", playerID, err)
	}

	switch outcome {
	case store.ReleaseDeleted:
		metricResult = playerNameResultOK
		// **先删库再删缓存**,顺序不能反:反过来的话,两步之间的一次读会把库里那个
		// 即将消失的名字重新缓存起来,DEL 就白做了。
		// DEL 失败不算业务失败(库已经是真相),脏值由 CacheTTL 封顶。
		if svcCtx.Router != nil {
			if cacheErr := svcCtx.Router.DelPlayerName(ctx, playerID); cacheErr != nil {
				logx.Errorf("[player-name] cache del failed for player_id=%d after MySQL delete committed "+
					"(stale name visible until CacheTTL expires): %v", playerID, cacheErr)
			}
		}
		logx.Infof("[player-name] released player_id=%d admin=%v", playerID, admin)
		return nil

	case store.ReleaseAbsent:
		// 幂等成功:行本来就不在,或已经被上一次补偿删掉了。
		metricResult = playerNameResultOK
		return nil

	case store.ReleaseOutsideWindow:
		metricResult = playerNameResultOutsideWindow
		// server 层会把它翻成 FailedPrecondition 并打 WARN;这里补上 server 拿不到的
		// 细节(窗口下界),排障时才知道是"窗口太窄"还是"来释放的根本不是刚建的号"。
		logx.Infof("[player-name] release refused: player_id=%d registered before window floor %d ms "+
			"(no admin token); a live character's name is NOT released by the create-player compensation path",
			playerID, minCreatedMs)
		return fmt.Errorf("%w: player_id=%d", ErrPlayerNameReleaseOutsideWindow, playerID)

	default:
		return fmt.Errorf("unexpected release outcome %d for player %d", uint8(outcome), playerID)
	}
}

// releaseWindowFloorMs 算这次释放允许触碰的最早登记时刻(Unix 毫秒,含)。
//
//   - admin=true → 0,不限时间(调用方已过 authorizeAdmin);
//   - 否则 → now - ReleaseWindow。
//
// 【配置漏填时往哪边倒】ReleaseWindow 非正值(config.Normalize() 本该填上默认 10m)
// **绝不能**退化成 0:0 等于"不限时间",任何能发 RPC 的内部调用方都能删掉在役角色的
// 名字,而名字一释放就可能立刻被别人占走,不可回滚。这里照常套公式 —— 窗口为 0 时
// 下界就是"此刻",几乎什么都删不掉。方向是 fail-closed:代价是建角补偿失效、留下
// 孤儿行(运维带 x-admin-token 可清),而不是误删。
func releaseWindowFloorMs(svcCtx *svc.ServiceContext, admin bool, now time.Time) uint64 {
	if admin {
		return 0
	}
	window := svcCtx.Config.PlayerName.ReleaseWindow
	if window <= 0 {
		logx.Errorf("[player-name] PlayerName.ReleaseWindow is %v (expected a positive duration from config defaults); "+
			"conditional release is effectively disabled until it is configured", window)
	}
	nowMs := now.UnixMilli()
	windowMs := window.Milliseconds()
	if nowMs <= windowMs {
		// 只可能出现在把时钟调到 1970 附近、或窗口被配成几十年的机器上。
		// 这里必须显式取 0:无符号减法会把负差回绕成天文数字,下界反而跑到未来去。
		return 0
	}
	return uint64(nowMs - windowMs)
}

// BatchGetPlayerName 批量查展示名。
//
// 语义(与 BatchGetPlayerHomeZone 同口径):缺席的 id **不出现**在返回的 map 里,
// 这不是错误 —— 早于本功能建的角色、已释放的名字都属于这一类,调用方按空名展示。
//
// 三层:缓存(正/负)→ 回源查库 → 回填。回填全部 best-effort:写缓存失败只影响
// 下一次的命中率,不影响这一次的结果。
func BatchGetPlayerName(ctx context.Context, svcCtx *svc.ServiceContext, ids []uint64) (map[uint64]string, error) {
	metricResult := playerNameResultError
	defer observePlayerNameOp(playerNameOpBatchGet, &metricResult)()

	// 去重 + 去 0。**必须在限额判定之前**:上游(帮会成员列表)很容易把同一个
	// 帮主 id 重复塞进来,按原始长度拒绝会把一个合法请求判成超限。
	// 保持首次出现的顺序,排障时能和调用方的日志对上。
	unique := make([]uint64, 0, len(ids))
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	if len(unique) > store.PlayerNameBatchLimit {
		metricResult = playerNameResultInvalid
		return nil, fmt.Errorf("%w (%d distinct ids, limit %d)",
			store.ErrPlayerNameBatchTooLarge, len(unique), store.PlayerNameBatchLimit)
	}

	result := make(map[uint64]string, len(unique))
	if len(unique) == 0 {
		metricResult = playerNameResultOK
		return result, nil
	}

	// ── 第一层:缓存 ──
	var (
		hits   map[uint64]string
		absent map[uint64]bool
	)
	if svcCtx != nil && svcCtx.Router != nil {
		h, a, err := svcCtx.Router.MGetPlayerNames(ctx, unique)
		if err != nil {
			// Redis 挂了只意味着"全部未命中",不该让读接口整体失败:
			// 真相在 MySQL,回源一次就是了(代价是这段时间 SQL 压力变大)。
			logx.Errorf("[player-name] cache mget failed for %d ids (degraded to all-miss): %v", len(unique), err)
		} else {
			hits, absent = h, a
		}
	}

	missing := make([]uint64, 0, len(unique))
	for _, id := range unique {
		if name, ok := hits[id]; ok {
			result[id] = name
			playerNameCacheTotal.Inc(playerNameCacheHit)
			continue
		}
		if absent[id] {
			// 负缓存命中:库里确实没有,本轮不回源,也不放进结果。
			playerNameCacheTotal.Inc(playerNameCacheNegativeHit)
			continue
		}
		playerNameCacheTotal.Inc(playerNameCacheMiss)
		missing = append(missing, id)
	}

	if len(missing) == 0 {
		metricResult = playerNameResultOK
		return result, nil
	}

	// ── 第二层:回源 ──
	if svcCtx == nil || svcCtx.PlayerNameStore == nil {
		return nil, ErrPlayerNameStoreUnavailable
	}
	fromDB, err := svcCtx.PlayerNameStore.BatchGet(ctx, missing)
	if err != nil {
		// 不回半份结果:调用方无法分辨"这个人没名字"和"这个人的名字这次没查出来",
		// 半份结果会被当成"确实没名字"缓存/展示出去(§3.1 映射表同口径)。
		return nil, fmt.Errorf("batch get player names (%d ids): %w", len(missing), err)
	}

	// ── 第三层:回填(best-effort)──
	found := make(map[uint64]string, len(fromDB))
	stillAbsent := make([]uint64, 0, len(missing))
	for _, id := range missing {
		// name=="" 当成缺席:空名字写进正缓存会和"未命中"混淆(MGET 对缺键也返回空串)。
		// 库里本不该有空 name(Reserve 的 display 非空),真有就是脏数据,按缺席处理。
		if name, ok := fromDB[id]; ok && name != "" {
			result[id] = name
			found[id] = name
			continue
		}
		stillAbsent = append(stillAbsent, id)
	}

	if svcCtx.Router != nil {
		if len(found) > 0 {
			if cacheErr := svcCtx.Router.SetPlayerNames(ctx, found, svcCtx.Config.PlayerName.CacheTTL); cacheErr != nil {
				logx.Errorf("[player-name] cache refill failed for %d names: %v", len(found), cacheErr)
			}
		}
		if len(stillAbsent) > 0 {
			// 负缓存用 SET NX,永远盖不掉同一时刻刚写进来的真名(§3.5 的时序分析)。
			if cacheErr := svcCtx.Router.SetPlayerNamesAbsent(ctx, stillAbsent,
				svcCtx.Config.PlayerName.NegativeCacheTTL); cacheErr != nil {
				logx.Errorf("[player-name] negative cache refill failed for %d ids: %v", len(stillAbsent), cacheErr)
			}
		}
	}

	metricResult = playerNameResultOK
	return result, nil
}
