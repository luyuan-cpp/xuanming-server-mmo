package routing

// RouteStore.Mode=db_first 时 W1(RegisterPlayerZone)的编排与发布
// (docs/design/player-route-mapping-durability-proposal.md §4.1-2 / §4.2,第 1a 批规格 c.1)。
//
// 提交点在全局库 player_route(store.RouteStore.RegisterHome),mapping Redis 里的 player:zone /
// player:placement / player:maprev 只是发布出去的副本:库先提交,再用 placement.PublishRouteScript
// 按 maprev 做 set-if-newer,并在**同一条连接**上用 WAITAOF 确认落盘,三步都过才算登记成功。
//
// 合服不改 player_route、也不改这几把键(别名合服,docs/design/player-data-tidb-scaling-plan.md §7.1),W1 不看合服状态。
//
// 本文件不碰 SQL(store 的职责),也不改 redis 模式的任何一行(router.go 里那段 SETNX 脚本)。

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/store"

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// RouteStore 是 db_first 下 W1 的库内提交点。由消费方(本包)定义、只有一个方法:路由器只需要它,
// 单测注入假实现;*store.RouteStore 逐字满足。svc 那边再加一个 Close 组成 svc.PlayerRouteStore。
type RouteStore interface {
	RegisterHome(ctx context.Context, req store.RouteRegisterRequest) (store.RouteRegisterResult, error)
}

// RouterOption 是 NewRouter 的可选装配项。
type RouterOption func(*Router)

// WithRouteStore 注入 db_first 的库内提交点。调用方只在 store 非 nil 时传:把一个 nil 的 *store.RouteStore
// 包进接口会得到「非 nil 接口包着 nil 指针」,registerPlayerZoneDBFirst 的判空拦不住,调用即 panic。
func WithRouteStore(s RouteStore) RouterOption {
	return func(r *Router) { r.routeStore = s }
}

var (
	// ErrInvalidRegistration:player_id 为 0,或 home_zone 不在 [1, store.RouteHomeZoneMax]。只有 db_first 可达
	// (redis 模式与今天逐字相同,照样写入);gRPC 层回 Unavailable + ErrCodeInvalidRequest(码集合不变,规格 g.2 第 14 条)。
	ErrInvalidRegistration = errors.New("route register: invalid player_id or home_zone")

	// ErrRouteStore:库这一段的故障 —— route store 没装配、SQL 错(含 1205)、route_meta 缺失或 writable≠1、
	// 1213/9007 重试用尽、缺行(另包 ErrRouteUnknownToStore)、库里的行过不了校验。gRPC 层回 Unavailable + ErrCodeRouteStoreError。
	ErrRouteStore = errors.New("player_route store failure")

	// ErrRouteUnknownToStore:mapping Redis 认得这名玩家(zone / placement / maprev 任一在),player_route 却没有行
	// —— P1 导入没做,或库被回档。总是与 ErrRouteStore 一起包着返回,零写入,由人处理。
	ErrRouteUnknownToStore = errors.New("the mapping cache knows this player but player_route has no row; run `route_tool import-from-redis` first")

	// ErrRoutePublishConflict:发布脚本发现缓存里是另一份映射,零写入。三种来源:同 rev 而值不同(有写者绕过了脚本,
	// 或库回档后 rev 被复用)、缓存没有 maprev 而遗留值与库不同(B2:不静默覆盖)、缓存 rev 更新而 home 不同(M3:库被回档的信号)。
	ErrRoutePublishConflict = errors.New("route publish refused: the mapping cache holds a conflicting mapping")

	// ErrRoutePublishNotDurable:WAITAOF 没确认到要求的落盘份数,或剩余预算已不够发一次 WAITAOF。
	// 库里的行已提交、缓存可能已写入但未确认落盘(login 那段注释说的「幽灵映射」),调用方重试会落到幂等分支补发。
	ErrRoutePublishNotDurable = errors.New("route publish not confirmed durable by WAITAOF")
)

// errUnexpectedWaitAOFReply:WAITAOF 的回复不是两个非负整数。确定性错误(连的不是同一套协议),不重试。
var errUnexpectedWaitAOFReply = errors.New("unexpected WAITAOF reply")

// publishConfig 是发布的运行参数,来自已经 Normalize + Validate 过的 config.RouteStoreConfig。
type publishConfig struct {
	attempts    int
	waitAOF     bool
	numLocal    int
	numReplicas int
	waitTimeout time.Duration
}

func newPublishConfig(c config.RouteStoreConfig) publishConfig {
	return publishConfig{
		attempts:    c.PublishAttempts,
		waitAOF:     c.WaitsForAOF(),
		numLocal:    c.NumLocal(),
		numReplicas: c.NumReplicas(),
		waitTimeout: time.Duration(c.WaitAOFTimeoutMs) * time.Millisecond,
	}
}

// publishDeadlineMargin:WAITAOF 的等待上限 = min(配置值, ctx 剩余 − 本值),给回包与 gRPC 返回留的余量。
const publishDeadlineMargin = 100 * time.Millisecond

// publishRetryBackoffMin / Max:可重试的发布失败(网络错误、落盘份数不足)之后的抖动等待,可被 ctx 取消。
const (
	publishRetryBackoffMin = 10 * time.Millisecond
	publishRetryBackoffMax = 50 * time.Millisecond
)

// registerPlayerZoneDBFirst 是 RouteStore.Mode=db_first 时的 W1(规格 c.1):
//  1. 参数校验:player_id 为 0 或 home 越界 → ErrInvalidRegistration,不碰 Redis 也不碰库;
//  2. store 没装配 → ErrRouteStore;
//  3. Redis 预检(调库之前一次 MGET)取 zone / placement / maprev:zone 是别的值 → 冲突(解析不了用 redis 模式同一文案),不进库;
//     三键任一在 = 缓存认得这名玩家 → 库里必须已有行(ExistingRowOnly);
//  4. 库内提交(RegisterHome:ODKU / 加锁读),业务结局走 Outcome;
//  5. 发布(set-if-newer 脚本 + WAITAOF)。幂等分支(AlreadySame)同样要发,用来补齐上次失败的发布;
//  6. FilledHomePlacementKept:打与 redis 模式同一条 ERROR,返回 nil。
//
// 失败语义(gRPC 映射见 dataserviceserver.go 的 RegisterPlayerZone):
//   - 冲突 → *HomeZoneConflictError;
//   - 库故障、缺行 → ErrRouteStore(缺行另包 ErrRouteUnknownToStore);参数非法 → ErrInvalidRegistration;
//   - 已提交但发布失败或未确认落盘 → 发布错误(**不**包 ErrRouteStore)。库里的行留下,调用方重试会落到幂等分支补发。
//
// 事务里绝不做 Redis 读写:预检在调库之前,发布在提交之后(store 侧同一条不变量)。
// 每次调用恰好计一次 route_register_total{outcome};Redis 预检本身出错(MGET 失败、MGET 长度不对、
// 既有值解析不了)计 precheck_error —— error_code 只在 gRPC message 里,不单独计这类拒绝在指标上就看不见。
func (r *Router) registerPlayerZoneDBFirst(ctx context.Context, playerID uint64, homeZoneID, storageID uint32) error {
	if playerID == 0 || homeZoneID == 0 || homeZoneID > store.RouteHomeZoneMax {
		metrics.ObserveRouteRegister(metrics.RouteRegisterInvalid)
		return fmt.Errorf("%w: player_id=%d home_zone=%d (want player_id > 0 and home_zone in [1, %d])",
			ErrInvalidRegistration, playerID, homeZoneID, store.RouteHomeZoneMax)
	}
	if r.routeStore == nil {
		metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
		return fmt.Errorf("%w: route store not assembled (RouteStore.Mode=db_first)", ErrRouteStore)
	}

	// 预检:三键一次 MGET。
	vals, err := r.mappingRedis.MgetCtx(ctx, mappingKey(playerID), placement.Key(playerID), placement.MapRevKey(playerID))
	if err != nil {
		metrics.ObserveRouteRegister(metrics.RouteRegisterPrecheckError)
		return fmt.Errorf("mapping precheck for player %d failed: %w", playerID, err)
	}
	if len(vals) != 3 {
		// 防御:按 key 位置对齐取值,长度对不上说明底层语义变了,宁可失败也不能错位(同 MGetPlayerNames)。
		metrics.ObserveRouteRegister(metrics.RouteRegisterPrecheckError)
		return fmt.Errorf("mapping precheck for player %d failed: MGET returned %d values for 3 keys", playerID, len(vals))
	}
	cachedZone, cachedPlacement, cachedRev := vals[0], vals[1], vals[2]
	if cachedZone != "" && cachedZone != strconv.FormatUint(uint64(homeZoneID), 10) {
		existing, parseErr := strconv.ParseUint(cachedZone, 10, 32)
		if parseErr != nil {
			// 与 redis 模式同一文案:值坏了同样不许覆盖,更需要人来看一眼。
			metrics.ObserveRouteRegister(metrics.RouteRegisterPrecheckError)
			return fmt.Errorf("player %d has an unparsable zone mapping %q, refusing to overwrite with %d",
				playerID, cachedZone, homeZoneID)
		}
		metrics.ObserveRouteRegister(metrics.RouteRegisterPrecheckConflict)
		return &HomeZoneConflictError{PlayerID: playerID, Existing: uint32(existing), Requested: homeZoneID}
	}
	// 缓存认得这名玩家,库里就必须已有这一行:悄悄插一行 rev=1 会让之后的发布被缓存里更大的 maprev
	// 判成 Superseded,永远发布不上去(store.RouteRegisterRequest.ExistingRowOnly)。
	known := cachedZone != "" || cachedPlacement != "" || cachedRev != ""

	res, err := r.routeStore.RegisterHome(ctx, store.RouteRegisterRequest{
		PlayerID:        playerID,
		HomeZone:        homeZoneID,
		StorageID:       storageID,
		ExistingRowOnly: known,
	})
	if err != nil {
		metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
		return fmt.Errorf("%w: %w", ErrRouteStore, err)
	}
	switch res.Outcome {
	case store.RouteRegisterConflict:
		metrics.ObserveRouteRegister(metrics.RouteRegisterConflict)
		return &HomeZoneConflictError{PlayerID: playerID, Existing: res.ExistingHome, Requested: homeZoneID}
	case store.RouteRegisterRowMissing:
		metrics.ObserveRouteRegister(metrics.RouteRegisterRefusedUnknownRow)
		logx.Errorf("[RegisterPlayerZone][db_first] mapping Redis knows player %d (zone=%q placement=%q maprev=%q) but player_route has no row; "+
			"refused with zero writes — run `route_tool import-from-redis` first (proposal §7 P1)",
			playerID, cachedZone, cachedPlacement, cachedRev)
		return fmt.Errorf("%w: %w (player %d)", ErrRouteStore, ErrRouteUnknownToStore, playerID)
	case store.RouteRegisterInserted, store.RouteRegisterFilledHome,
		store.RouteRegisterFilledHomePlacementKept, store.RouteRegisterAlreadySame:
		// 发布的是 RegisterHome 回来的行:它与本次请求必须是同一名玩家、同一个 home(这四种结局下 home 都等于请求值),
		// 对不上说明 store 的契约被破坏,发布出去就是把别人的映射写进缓存。
		if res.Row.PlayerID != playerID || res.Row.HomeZone != homeZoneID {
			metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
			return fmt.Errorf("%w: outcome %s returned row player_id=%d home_zone=%d for request player %d zone %d (store contract broken)",
				ErrRouteStore, res.Outcome, res.Row.PlayerID, res.Row.HomeZone, playerID, homeZoneID)
		}
		metrics.ObserveRouteRegister(registerOutcomeLabel(res.Outcome))
	default:
		metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
		return fmt.Errorf("%w: unexpected register outcome %s for player %d", ErrRouteStore, res.Outcome, playerID)
	}

	if err := r.publishRoute(ctx, res.Row); err != nil {
		// 库里这一行已提交:id 已烧掉,缓存还没确认。只进日志,player_id / rev 不做指标 label(AGENTS.md §9);
		// route_tool audit 会把它计成 db_only_unpublished(或 value_mismatch),第 1b 批的预热推送补齐。
		logx.Errorf("[RegisterPlayerZone][db_first] player_route committed but publish failed: player=%d zone=%d rev=%d outcome=%s: %v",
			playerID, homeZoneID, res.Row.Rev, res.Outcome, err)
		return err
	}

	if res.Outcome == store.RouteRegisterFilledHomePlacementKept {
		// 与 redis 模式 zoneRegisteredPlacementKept 同一条日志:落点指向玩家数据**真正所在**的库,不被请求覆盖。
		logx.Errorf("[RegisterPlayerZone] placement record already present, kept as is: player=%d zone=%d requested_storage=%d key=%s",
			playerID, homeZoneID, storageID, placement.Key(playerID))
	}
	return nil
}

// registerOutcomeLabel 把四种「已提交 / 已是这一版」的库结局翻成 route_register_total 的 label。
func registerOutcomeLabel(o store.RouteRegisterOutcome) string {
	switch o {
	case store.RouteRegisterInserted:
		return metrics.RouteRegisterInserted
	case store.RouteRegisterFilledHome:
		return metrics.RouteRegisterFilledHome
	case store.RouteRegisterFilledHomePlacementKept:
		return metrics.RouteRegisterFilledHomePlacementKept
	default:
		return metrics.RouteRegisterAlreadySame
	}
}

// publishRoute 把库里提交后的一行发布到 mapping Redis(规格 c.1「publishRoute 的细节」)。
//
// 每次尝试是一个 pipeline,在同一条连接上发 EVAL(PublishRouteScript)与 WAITAOF ——
// 不能用 go-redis 的 WaitAOF:它返回 *IntCmd,而 WAITAOF 回的是两元素数组,会被当成解析错误(规格 d 节末)。
// pipeline 走 r.publishRedis(publish_redis.go:与 mapping Redis 同一实例、独立连接池、不经 go-zero 熔断器;2026-10-20 起,
// 此前走 r.mappingRedis,与读路径共用池与熔断器)。预检 MGET 仍走 mappingRedis:那是普通短读。
//
// 判定:
//   - Redis 错误回复(脚本拒绝坏 rev / 畸形 maprev、WRONGTYPE、OOM,appendonly 关着时 WAITAOF 的 ERR)是确定性的,不重试;
//   - 网络错误(含取连接失败 / 池满等到 ctx 截止)与 WAITAOF 确认份数不足:退避 10–50ms 后**整条 pipeline 一起重发**。
//     只重发 WAITAOF 不行:新连接的写偏移可能比上次写入低,会误报已落盘;脚本在「缓存已是这一版或更新」的分支
//     会把 maprev 原值写回一次(touch),所以重发后的 WAITAOF 仍覆盖缓存此刻的状态;
//   - 回复形状不对 → fail-closed,不重试;Conflict / LegacyConflict、
//     以及 Superseded 而 home 不同(home 写定之后不再改,合服也不改它,
//     所以只可能是库被回档或有写者绕过了脚本)→ ErrRoutePublishConflict;
//   - Applied / Confirmed / home 相同的 Superseded:WAITAOF 满足才算成功。
//
// 重试只有一层(2026-10-20 更正,原文「go-redis 也会按 MaxRetries 重发、两层叠加」只对 go-zero 客户端成立):发布客户端
// MaxRetries=-1,go-redis 不自动重发,重发全由本函数的 PublishAttempts 负责;总时长受 ctx 截止时间约束
// (每次 WAITAOF 的 timeout 都按剩余时间重算),脚本是 set-if-newer + touch,重发幂等。
// 每次调用的最终结果计一次 route_publish_total{result}。
func (r *Router) publishRoute(ctx context.Context, row store.RouteRow) error {
	result := metrics.RoutePublishError
	defer func() { metrics.ObserveRoutePublish(result) }()

	if r.publishRedis == nil {
		// 只在 db_first 下才会走到这里,而 NewRouter 在 db_first 下恒建发布客户端;到这里说明装配被绕过,fail-closed、不重试。
		return fmt.Errorf("route publish for player %d: publish client not assembled (RouteStore.Mode=db_first); player_route row rev=%d committed but unpublished",
			row.PlayerID, row.Rev)
	}

	home := row.HomeValue()
	plc, err := row.PlacementValue()
	if err != nil {
		// 不发布垃圾:一条过不了校验的行写进缓存,go/db 就会按它选错库。
		return fmt.Errorf("%w: player_route row invalid: %w", ErrRouteStore, err)
	}
	rev := placement.FormatRev(row.Rev)
	if row.PlayerID == 0 || row.HomeZone == 0 || !placement.ValidRev(rev) {
		return fmt.Errorf("%w: player_route row invalid: player_id=%d home_zone=%d rev=%d",
			ErrRouteStore, row.PlayerID, row.HomeZone, row.Rev)
	}
	keys := placement.PublishRouteKeys(row.PlayerID)
	deadline, hasDeadline := ctx.Deadline()

	var lastErr error
	for attempt := 1; attempt <= r.publish.attempts; attempt++ {
		if attempt > 1 {
			if waitErr := sleepCtx(ctx, publishRetryBackoff()); waitErr != nil {
				lastErr = fmt.Errorf("%w (retry wait: %w)", lastErr, waitErr)
				break
			}
		}

		var waitMs int64
		if r.publish.waitAOF {
			var ok bool
			waitMs, ok = waitAOFBudgetMs(time.Now(), deadline, hasDeadline, r.publish.waitTimeout, publishDeadlineMargin)
			if !ok {
				// 一条命令都不发:WAITAOF 的 timeout 为 0 表示无限阻塞。
				result = metrics.RoutePublishNotDurable
				return withLastAttempt(fmt.Errorf("%w: player %d rev=%s: budget exhausted before attempt %d/%d",
					ErrRoutePublishNotDurable, row.PlayerID, rev, attempt, r.publish.attempts), lastErr)
			}
		}

		var evalCmd, waitCmd *goredis.Cmd
		_, pipeErr := r.publishRedis.Pipelined(ctx, func(p goredis.Pipeliner) error {
			evalCmd = p.Eval(ctx, placement.PublishRouteScript, keys, rev, home, plc)
			if r.publish.waitAOF {
				waitCmd = p.Do(ctx, "WAITAOF", r.publish.numLocal, r.publish.numReplicas, waitMs)
			}
			return nil
		})
		// 2026-10-20:发布客户端不再挂 go-zero 熔断器,下面「熔断打开」那一支在生产上已不会出现;判据保留:发布客户端上的钩子
		// (单测用它模拟没执行的批)与将来若再挂熔断器都按同一口径处理。取连接失败 / ctx 已结束时 go-redis 把错误回填到每条命令
		// (generalProcessPipeline 的 setCmdsErr),走下面逐条命令的网络错误分支,同样重试。
		// 2026-10-18 更正(原注释「回调根本没被调用(取连接失败、熔断器拒绝)」只对了前一半):回调没被调用只发生在取连接失败时;
		// 熔断打开时回调**照样被调用**、命令照样排进队列,但 go-zero 的 breakerHook.ProcessPipelineHook 直接返回
		// breaker.ErrServiceUnavailable、不调下一层(ctx 已结束时 DoWithAcceptableCtx 先回 ctx.Err(),同理),队列里的命令一条都没
		// 执行,各自的 Err() 为 nil、Val() 为 nil(go-zero v1.10.0 core/stores/redis/breakerhook.go、go-redis v9.18.0 pipeline.go Exec
		// 不回填错误)。按旧判据它们会落到 ClassifyPublishReply(nil) → ErrUnexpectedPublishReply,被当成确定性错误、不再重试。
		// 现在两种情况都按「没执行」处理:可重试的 Redis 不可用(判据同 scene_manager load_refresh.go 的 pipelineBatchRejected)。
		if evalCmd == nil || publishPipelineNotExecuted(pipeErr, evalCmd, waitCmd) {
			if pipeErr == nil {
				pipeErr = errors.New("pipeline callback was not invoked")
			}
			lastErr = fmt.Errorf("route publish pipeline for player %d not executed (connection / circuit breaker / context): %w", row.PlayerID, pipeErr)
			continue
		}
		// 执行过的批不看 pipeErr 本身:它只是第一条失败命令的错误,逐条命令的结果才是判据。

		if evalErr := evalCmd.Err(); evalErr != nil {
			if isRedisErrorReply(evalErr) {
				return fmt.Errorf("route publish script rejected for player %d rev=%s (deterministic, not retried): %w",
					row.PlayerID, rev, evalErr)
			}
			lastErr = fmt.Errorf("route publish script for player %d rev=%s: %w", row.PlayerID, rev, evalErr)
			continue
		}
		reply, err := placement.ClassifyPublishReply(evalCmd.Val(), rev, home, plc)
		if err != nil {
			return fmt.Errorf("route publish for player %d rev=%s: %w", row.PlayerID, rev, err)
		}
		switch {
		case reply.Outcome == placement.PublishConflict:
			result = metrics.RoutePublishConflict
			return publishConflictError(row.PlayerID, rev, home, plc, reply, "same_rev_different_value")
		case reply.Outcome == placement.PublishLegacyConflict:
			result = metrics.RoutePublishLegacyConflict
			return publishConflictError(row.PlayerID, rev, home, plc, reply, "legacy_value_without_maprev")
		case reply.Outcome == placement.PublishSuperseded && reply.CurHome != home:
			result = metrics.RoutePublishConflict
			return publishConflictError(row.PlayerID, rev, home, plc, reply, "superseded_home_differs")
		}

		if waitCmd != nil {
			acks, waitErr := waitCmd.Slice()
			if waitErr != nil {
				if isRedisErrorReply(waitErr) {
					return fmt.Errorf("WAITAOF for player %d rev=%s rejected (deterministic, not retried; needs appendonly yes and Redis >= 7.2): %w",
						row.PlayerID, rev, waitErr)
				}
				lastErr = fmt.Errorf("WAITAOF for player %d rev=%s: %w", row.PlayerID, rev, waitErr)
				continue
			}
			if ackErr := checkWaitAOFReply(acks, r.publish.numLocal, r.publish.numReplicas); ackErr != nil {
				if errors.Is(ackErr, errUnexpectedWaitAOFReply) {
					return fmt.Errorf("WAITAOF for player %d rev=%s: %w", row.PlayerID, rev, ackErr)
				}
				lastErr = fmt.Errorf("%w: player %d rev=%s: %w", ErrRoutePublishNotDurable, row.PlayerID, rev, ackErr)
				continue
			}
		}

		switch reply.Outcome {
		case placement.PublishApplied:
			result = metrics.RoutePublishApplied
		case placement.PublishConfirmed:
			result = metrics.RoutePublishConfirmed
		default: // PublishSuperseded,且 home 相同
			result = metrics.RoutePublishSuperseded
		}
		return nil
	}

	if errors.Is(lastErr, ErrRoutePublishNotDurable) {
		result = metrics.RoutePublishNotDurable
	}
	return fmt.Errorf("route publish for player %d rev=%s failed after %d attempt(s): %w",
		row.PlayerID, rev, r.publish.attempts, lastErr)
}

// ProbeWaitAOF 是 db_first 装配时对 mapping Redis 的一次 WAITAOF 能力探针:按配置的 numlocal / numreplicas
// 发一次 WAITAOF(timeout = WaitAOFTimeoutMs),报错或确认份数不足就返回 error,调用方据此不装配 route store
// (svc.newPlayerRouteStore;Store.Required=true 时 MissingStores 带上 player_route,main 拒启)。
// WaitsForAOF()=false(两项都显式写 0)时什么都不发,直接返回 nil。
//
// 为什么要在启动期挡:WAITAOF 的配置错误(appendonly no 时回 ERR、Redis < 7.2 回 unknown command、
// NumReplicas 写了 1 而拓扑里没有副本)是确定性的,留到请求期就是每一次 W1 都失败;而 go-zero 给 mapping Redis
// 套的熔断器只把 nil / redis.Nil / context.Canceled 当成功,pipeline 回的 Redis 错误回复都记成熔断失败 ——
// 熔断器一开,同一个客户端上无关的 GetPlayerHomeZone、BatchGet、玩家锁也会被按概率丢弃,影响面从建角扩大到登录。
//
// 探针用一个临时的 go-redis 客户端(与发布客户端同一个构造口径 mappingRedisOptions,池 1 条,探完即关),不经 go-zero。
// 2026-10-20(A2-1):此前用 go-zero 的 *redis.Redis,而 go-zero 按地址缓存底层客户端、熔断钩子挂在第一次建客户端时,
// 探针与 Router 共用一个熔断器,探针的失败同样计进 Router 的熔断(2026-10-18 更正登记的风险);现在两边互不相干,该风险销项。
// 代价:启动期对 mapping Redis 多一条短连接(探完即关)。
// 新连接上没有写过东西(WAITAOF 等的是本连接的写偏移),本地确认会立即返回;timeout 仍取配置值而不是 1ms。
func ProbeWaitAOF(ctx context.Context, mapping redis.RedisConf, rc config.RouteStoreConfig) error {
	rc = rc.Normalize()
	if !rc.WaitsForAOF() {
		return nil
	}
	opts, err := mappingRedisOptions(mapping, probeRedisPoolSize, publishRedisReadTimeout(rc))
	if err != nil {
		return fmt.Errorf("WAITAOF probe: mapping redis %s: %w", mapping.Host, err)
	}
	rdb := goredis.NewClient(opts)
	defer func() { _ = rdb.Close() }()
	acks, err := rdb.Do(ctx, "WAITAOF", rc.NumLocal(), rc.NumReplicas(), rc.WaitAOFTimeoutMs).Slice()
	if err != nil {
		// 没执行(连不上、ctx 已结束)单独报:否则会报成「needs appendonly yes」,把运维引错方向。只有 Redis 的错误回复才是能力问题。
		if !isRedisErrorReply(err) {
			return fmt.Errorf("WAITAOF probe not executed on mapping redis %s (connection / context): %w", mapping.Host, err)
		}
		return fmt.Errorf("WAITAOF %d %d %d on mapping redis %s failed (needs appendonly yes and Redis >= 7.2): %w",
			rc.NumLocal(), rc.NumReplicas(), rc.WaitAOFTimeoutMs, mapping.Host, err)
	}
	if err := checkWaitAOFReply(acks, rc.NumLocal(), rc.NumReplicas()); err != nil {
		return fmt.Errorf("WAITAOF probe on mapping redis %s (check RouteStore.WaitAOFNumLocal / WaitAOFNumReplicas against the topology): %w",
			mapping.Host, err)
	}
	return nil
}

// publishConflictError 拼 ErrRoutePublishConflict:带上缓存此刻的值与本次发的值,运维据此判断是谁绕过了脚本。
func publishConflictError(playerID uint64, rev, home, plc string, reply placement.PublishReply, reason string) error {
	return fmt.Errorf("%w: player %d reason=%s cached rev=%q home=%q placement=%q, publishing rev=%s home=%q placement=%q (zero writes)",
		ErrRoutePublishConflict, playerID, reason, reply.CurRev, reply.CurHome, reply.CurPlacement, rev, home, plc)
}

// withLastAttempt 把上一次尝试的错误接在 err 后面;cause 为 nil 时原样返回 err。errors.Is 两边都认。
func withLastAttempt(err, cause error) error {
	if cause == nil {
		return err
	}
	return fmt.Errorf("%w; last attempt: %w", err, cause)
}

// publishPipelineNotExecuted 判断一批已排队的 pipeline 命令是否应按「一条都没执行」处理(纯函数,单测直接测)。
// pipeErr 是 PipelinedCtx 的返回值,cmds 是回调里排的命令(nil 跳过):
//   - pipeErr 为 nil 或 redis.Nil:批已执行,逐条判;
//   - pipeErr 是熔断拒绝(breaker.ErrServiceUnavailable):没执行(钩子不调下一层,命令的 Err() / Val() 都是 nil);
//   - 其它 pipeErr,且没有任何一条命令自己带错:错误没落到任何命令上(ctx 已结束时熔断钩子先回 ctx.Err() 等),没执行;
//   - 其它 pipeErr,且有命令自己带错:批已执行(或 go-redis 已把网络错误回填到每条命令,generalProcessPipeline 的 setCmdsErr),逐条判。
//
// 与 scene_manager internal/logic/load_refresh.go 的 pipelineBatchRejected 同一判据(两个模块各一份:不为几行代码开公共包)。
func publishPipelineNotExecuted(pipeErr error, cmds ...*goredis.Cmd) bool {
	if pipeErr == nil || errors.Is(pipeErr, goredis.Nil) {
		return false
	}
	if errors.Is(pipeErr, breaker.ErrServiceUnavailable) {
		return true
	}
	for _, c := range cmds {
		if c != nil && c.Err() != nil {
			return false
		}
	}
	return true
}

// isRedisErrorReply 判断 err 是不是 Redis 回的错误回复(确定性错误)。网络错误、ctx 错误、熔断拒绝都不是。
func isRedisErrorReply(err error) bool {
	var re goredis.Error
	return errors.As(err, &re)
}

// checkWaitAOFReply 判断 WAITAOF 的回复 [numlocal, numreplicas] 是否满足要求的份数。
// 份数不足回普通错误(调用方按 ErrRoutePublishNotDurable 重试);形状不对回 errUnexpectedWaitAOFReply(不重试)。
// 纯函数,单测直接测。
func checkWaitAOFReply(vals []any, numLocal, numReplicas int) error {
	if len(vals) != 2 {
		return fmt.Errorf("%w: %d elements %v, want [numlocal, numreplicas]", errUnexpectedWaitAOFReply, len(vals), vals)
	}
	local, localOK := vals[0].(int64)
	replicas, replicasOK := vals[1].(int64)
	if !localOK || !replicasOK || local < 0 || replicas < 0 {
		return fmt.Errorf("%w: %v", errUnexpectedWaitAOFReply, vals)
	}
	if local < int64(numLocal) || replicas < int64(numReplicas) {
		return fmt.Errorf("WAITAOF acked numlocal=%d numreplicas=%d, want at least %d and %d", local, replicas, numLocal, numReplicas)
	}
	return nil
}

// waitAOFBudgetMs 算本次 WAITAOF 的 timeout(毫秒):min(configured, 截止前剩余 − margin),
// **先截断成整毫秒再判断**。结果小于 1 时 ok=false,调用方不得发送 —— WAITAOF 的 timeout 为 0 表示无限阻塞(规格 m5)。
// 没有截止时间时取 configured。时间由参数注入,纯函数。
func waitAOFBudgetMs(now, deadline time.Time, hasDeadline bool, configured, margin time.Duration) (int64, bool) {
	budget := configured
	if hasDeadline {
		if remaining := deadline.Sub(now) - margin; remaining < budget {
			budget = remaining
		}
	}
	ms := budget.Milliseconds()
	if ms < 1 {
		return 0, false
	}
	return ms, true
}

// publishRetryBackoff 在 [publishRetryBackoffMin, publishRetryBackoffMax] 内均匀取一个等待时长:
// 同时失败的几个请求错开重来,不在同一瞬间再撞一次刚恢复的连接(同 store.jitteredBackoff)。
func publishRetryBackoff() time.Duration {
	return publishRetryBackoffMin + rand.N(publishRetryBackoffMax-publishRetryBackoffMin+1)
}

// sleepCtx 可取消的等待:ctx 先结束就立即返回 ctx.Err()(同 store.sleepCtx;那个未导出,这里不为两行代码开一个公共包)。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
