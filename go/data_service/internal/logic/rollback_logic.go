package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"data_service/internal/constants"
	"data_service/internal/guildcheck"
	"data_service/internal/metrics"
	"data_service/internal/store"
	"data_service/internal/svc"

	guildpb "proto/guild"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	snapshotTypePreRollback uint32 = 3 // matches SnapshotType.SNAPSHOT_PRE_ROLLBACK
	rollbackTypePlayer      uint32 = 1
	rollbackTypeZone        uint32 = 2
	rollbackTypeServer      uint32 = 3
)

func validateRollbackDependencies(svcCtx *svc.ServiceContext) (uint32, error) {
	if svcCtx == nil {
		return constants.ErrCodeSnapshotDBError, fmt.Errorf("service context is unavailable")
	}
	if svcCtx.SnapshotStore == nil {
		return constants.ErrCodeSnapshotDBError, fmt.Errorf("snapshot/audit store is unavailable")
	}
	if svcCtx.Router == nil {
		return constants.ErrCodeRedis, fmt.Errorf("player data router is unavailable")
	}
	return constants.ErrCodeOK, nil
}

func rollbackFenceErrorCode(err error) uint32 {
	if errors.Is(err, svc.ErrRollbackTargetOnline) {
		return constants.ErrCodePlayerOnline
	}
	return constants.ErrCodeRollbackFailed
}

func validateRollbackRelease(release func(), scope string) (func(), uint32, error) {
	if release == nil {
		return nil, constants.ErrCodeRollbackFailed,
			fmt.Errorf("rollback fence for %s returned a nil release function", scope)
	}
	return release, constants.ErrCodeOK, nil
}

func acquirePlayerRollbackFence(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (func(), uint32, error) {
	if svcCtx.RollbackFence == nil {
		logx.Errorf("[Rollback] player %d rejected: cross-service offline epoch fence is not configured", playerID)
		return nil, constants.ErrCodeNotImplemented, nil
	}
	release, err := svcCtx.RollbackFence.AcquirePlayer(ctx, playerID)
	if err != nil {
		return nil, rollbackFenceErrorCode(err), fmt.Errorf("acquire rollback fence for player %d: %w", playerID, err)
	}
	return validateRollbackRelease(release, fmt.Sprintf("player %d", playerID))
}

func acquireZoneRollbackFence(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) (func(), uint32, error) {
	if svcCtx.RollbackFence == nil {
		logx.Errorf("[Rollback] zone %d rejected: cross-service offline epoch fence is not configured", zoneID)
		return nil, constants.ErrCodeNotImplemented, nil
	}
	release, err := svcCtx.RollbackFence.AcquireZone(ctx, zoneID)
	if err != nil {
		return nil, rollbackFenceErrorCode(err), fmt.Errorf("acquire rollback fence for zone %d: %w", zoneID, err)
	}
	return validateRollbackRelease(release, fmt.Sprintf("zone %d", zoneID))
}

func acquireServerRollbackFence(ctx context.Context, svcCtx *svc.ServiceContext, zoneIDs []uint32) (func(), uint32, error) {
	if svcCtx.RollbackFence == nil {
		logx.Errorf("[Rollback] server rollback rejected: cross-service offline epoch fence is not configured")
		return nil, constants.ErrCodeNotImplemented, nil
	}
	release, err := svcCtx.RollbackFence.AcquireServer(ctx, zoneIDs)
	if err != nil {
		return nil, rollbackFenceErrorCode(err), fmt.Errorf("acquire server rollback fence: %w", err)
	}
	return validateRollbackRelease(release, "server")
}

func insertRollbackAudit(ctx context.Context, svcCtx *svc.ServiceContext, row *store.AuditLogRow) error {
	if svcCtx == nil || svcCtx.SnapshotStore == nil {
		return fmt.Errorf("snapshot/audit store is unavailable")
	}
	if err := svcCtx.SnapshotStore.InsertAuditLog(ctx, row); err != nil {
		return fmt.Errorf("insert rollback audit: %w", err)
	}
	return nil
}

// detachedAuditContext 给 RESULT 审计一个不随调用方取消的 ctx(07 §7.5.3-3)。回档可能已经写了数据,
// 调用方超时 / 断开后入口 ctx 已死,沿用它 RESULT 审计就写不进、只剩一条悬空的 STARTED。
// 自带上限,免得一个挂住的审计库把栅栏永远按住。
func detachedAuditContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackResultAuditTimeout)
}

// rollbackResultReason 拼 RESULT 审计的 Reason。有帮会闸计数时一并写进去(07 §7.5.2:RESULT 审计带拒绝码与分歧条数);
// note 非空时标注这一条是什么情况下写的(例如全服回档中止时"没执行"的 zone)。
func rollbackResultReason(code uint32, gf GuildGateFields, note, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "RESULT code=%d", code)
	if gf.GuildDivergenceCount > 0 || gf.GuildUnprovablePlayerCount > 0 {
		fmt.Fprintf(&b, " guild_divergences=%d guild_unprovable_players=%d", gf.GuildDivergenceCount, gf.GuildUnprovablePlayerCount)
	}
	if note != "" {
		fmt.Fprintf(&b, " (%s)", note)
	}
	fmt.Fprintf(&b, ": %s", reason)
	return b.String()
}

// ── 帮会资产闸(docs/design/guild-phase2/07-rollback-fail-closed.md)──────────────
//
// 回档把玩家的钱包换回快照,而帮会账本不在快照里:捐献的钱回到玩家手里、帮会资金与帮贡却还在 = 复制;
// 商店 / 活动发奖方向反过来 = 玩家白亏。所以任何回档在写任何玩家数据(含回档前安全快照)之前、在栅栏之内,
// 先问 guild "这些玩家自各自快照时刻以来,有没有已经终结为已应用的资产操作":
//   - 有,或有玩家快照早于帮会流水保留期、无法证明 → 默认拒绝(ErrCodeRollbackGuildDivergence),合法放行可过;
//   - 问不到(未配置、不可达、超时、任一页失败、超上限、预算耗尽)→ 拒绝且放行无效(ErrCodeRollbackGuildCheckFailed);
//   - 写完之后等 10s 复查一次,抓检查之后才终结的漏网行(ErrCodeRollbackGuildDivergedAfterWrite,人工补偿)。
//
// 顺序(07 §7.5.2):入参 → 依赖 → [x-admin-token,在 server 层] → 栅栏 → STARTED 审计 → 装配预检 → 计划(只读)
// → 沉降等待 → 检查 → [放行:ACCEPTED 审计 + 逐行 ERROR 日志] → 逐玩家:安全快照 → 写 Redis
// → (换脱钩 ctx)复查等待 → 复查 → RESULT 审计。栅栏一直持到 RESULT 审计之后才释放。
//
// 不依赖墙钟做判断:since 只由快照时刻与余量决定;两段等待走可注入的 svc.GuildCheckSleep;预算用单调时钟的 ctx。

const (
	// guildSettleDelay:栅栏生效后、首次检查前的沉降等待(07 §7.7 处置一)。让"踢下线前一两秒才发生"的操作
	// 先在 guild 侧终结,在零写入阶段就被拒,而不是写完才告警。
	// 30s > 默认参数下"scene 已应用 → guild 拿到证据"的上界约 27s(三次退避 ≤ 8.4s + 四次轮询 ≤ 8s + 四次处理 ≤ 10s)。
	// **依赖 guild 的配表参数** GuildRule.asset_op_retry_base_ms(默认 1000):策划把它调大时 30s 会不够,
	// 不够的后果是退化成写后复查事后告警,不是漏报。不做成配置项:它跟着另一个服务的配表走,给运维一个旋钮
	// 他也不知道该拧到多少。
	guildSettleDelay = 30 * time.Second

	// guildRecheckDelay:最后一笔写完到复查的等待(07 §7.7 处置二):> 投递 1800 + 落库 700 + Finalize 2000 ms,再留一倍。
	guildRecheckDelay = 10 * time.Second

	// guildRecheckBudget:复查调用本身的预算(不含等待)。与等待一起罩在脱钩 ctx 上。
	guildRecheckBudget = 120 * time.Second

	// maxDivergenceSample:响应里最多带多少条分歧样本(按 op_id 升序的前 N 条)。
	maxDivergenceSample = 20

	// rollbackResultAuditTimeout:RESULT 审计在脱钩 ctx 上的独立上限(见 detachedAuditContext)。
	rollbackResultAuditTimeout = 30 * time.Second

	// unprovableLogBatch:不可证明玩家的日志每行最多列多少个 id(= guild 单次调用的玩家上限)。
	unprovableLogBatch = guildcheck.MaxPlayersPerCall
)

// GuildGateFields 是三个回档响应共用的帮会闸字段,server 层原样搬进 proto。
type GuildGateFields struct {
	// GuildDivergenceCount:过滤后的分歧总数;ErrCodeRollbackGuildDivergedAfterWrite 时是写后复查新发现的条数。
	GuildDivergenceCount uint32
	// GuildDivergences:按 op_id 升序的前 maxDivergenceSample 条。
	GuildDivergences []guildcheck.GuildDivergence
	// GuildUnprovablePlayerCount:快照早于帮会流水保留期、无法证明的玩家数(07 R2b)。
	GuildUnprovablePlayerCount uint32
}

func guildGateFieldsOf(rows []guildcheck.GuildDivergence, unprovablePlayers int) GuildGateFields {
	return GuildGateFields{
		GuildDivergenceCount:       clampUint32(uint64(len(rows))),
		GuildDivergences:           slices.Clone(rows[:min(len(rows), maxDivergenceSample)]),
		GuildUnprovablePlayerCount: clampUint32(uint64(unprovablePlayers)),
	}
}

// validateGuildAccept:放行三条件里由 logic 验的那一条 —— reason 与 operator 非空(07 §7.5.4)。
// 放行时才必填;未放行时保持现状不校验,不夹带行为变更。x-admin-token 在 server 层验(三个 Rollback* 一律要)。
// 在栅栏与任何审计之前调用:参数不全的放行请求不该让任何玩家被挡在登录外。
func validateGuildAccept(accept bool, reason, operator string) uint32 {
	if accept && (strings.TrimSpace(reason) == "" || strings.TrimSpace(operator) == "") {
		return constants.ErrCodeInvalidRequest
	}
	return constants.ErrCodeOK
}

// guildGateRequest 是一次回档在帮会闸上的身份:日志、ACCEPTED 审计与指标都从这里取。
type guildGateRequest struct {
	scope        string // metrics.RollbackGuildScope*
	rollbackType uint32 // rollbackType*,ACCEPTED 审计用
	playerID     uint64 // 仅单人回档
	zoneID       uint32 // 单人 / 全服回档为 0
	targetTime   uint64
	accept       bool
	reason       string
	operator     string
	caller       string // server 层的 callerIdentity;只进日志
}

// logFields 是 `[Rollback][GuildDivergence]` 日志行的公共字段。operator / reason 是调用方自报的字符串,
// 用 %q 输出,免得换行符伪造出一条假的日志行。不含任何 token。
func (g guildGateRequest) logFields() string {
	s := fmt.Sprintf("scope=%s zone=%d target_time=%d operator=%q caller=%s", g.scope, g.zoneID, g.targetTime, g.operator, g.caller)
	if g.playerID != 0 {
		s += " player=" + strconv.FormatUint(g.playerID, 10)
	}
	return s
}

// guildPlan 是帮会检查的输入(07 §7.2)。
type guildPlan struct {
	sinceMs map[uint64]uint64 // player → since_ms = S_p×1000 − 余量(≥ 1)
	snapAt  map[uint64]uint64 // player → 计划快照时刻 S_p(秒);日志里的 snapshot_created_at
}

func newGuildPlan(snapAt map[uint64]uint64, marginMs int64) (guildPlan, error) {
	plan := guildPlan{sinceMs: make(map[uint64]uint64, len(snapAt)), snapAt: snapAt}
	for pid, createdAt := range snapAt {
		since, err := guildSinceMs(createdAt, marginMs)
		if err != nil {
			return guildPlan{}, fmt.Errorf("player %d: %w", pid, err)
		}
		plan.sinceMs[pid] = since
	}
	return plan, nil
}

// guildSinceMs = 快照时刻(秒)×1000 − 余量(07 §7.2)。秒转毫秒取该秒的起点(下界),方向 = 多报。
// 结果 < 1 时钳到 1:guild 把 since_ms == 0 当漏填拒掉;真实快照不会触发,只为小时间戳不下溢。
// marginMs 由 config.ValidateGuildCheck 保证 > 0。
func guildSinceMs(createdAtSec uint64, marginMs int64) (uint64, error) {
	if createdAtSec > math.MaxUint64/1000 {
		return 0, fmt.Errorf("snapshot created_at %d overflows milliseconds", createdAtSec)
	}
	ms, margin := createdAtSec*1000, uint64(marginMs)
	if ms <= margin {
		return 1, nil
	}
	return ms - margin, nil
}

// guildVerdict 是帮会闸的裁决。code == ErrCodeOK = 可以写(clean 或合法放行)。
// err 只在"故障"(CheckFailed、ACCEPTED 审计写不进)时非 nil;规则拒绝(Divergence)err 为 nil,
// 否则 handler 只回 error_code、分歧计数与样本带不出去。
type guildVerdict struct {
	code     uint32
	err      error
	check    guildcheck.GuildCheckResult
	accepted bool
}

func guildSleep(svcCtx *svc.ServiceContext) func(context.Context, time.Duration) error {
	if svcCtx.GuildCheckSleep != nil {
		return svcCtx.GuildCheckSleep
	}
	return waitContext
}

// waitContext 可取消的等待:ctx 先结束就立即返回 ctx.Err()。
func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// guildUnavailable:检查没做成 → CheckFailed,放行无效(07 R2)。日志关键字 "guild check unavailable" 是告警口径(§7.9.2)。
func guildUnavailable(g guildGateRequest, result string, reason error) guildVerdict {
	logx.Errorf("[Rollback][GuildDivergence] guild check unavailable: %v %s", reason, g.logFields())
	metrics.ObserveRollbackGuildCheck(g.scope, result)
	return guildVerdict{
		code: constants.ErrCodeRollbackGuildCheckFailed,
		err:  fmt.Errorf("guild check unavailable: %w", reason),
	}
}

// guildGatePreflight 在任何等待之前做两项不花时间的检查:接缝已装配、配置合法。
// svcCtx.GuildDivergence == nil 是**拒绝**,不是跳过(与 LoginAdminClient 的 nil 语义相反,测试 D2 钉住)。
// 失败时返回的裁决已记过指标与日志。
func guildGatePreflight(svcCtx *svc.ServiceContext, g guildGateRequest) (guildcheck.GuildDivergenceChecker, *guildVerdict) {
	var reason error
	if svcCtx.GuildDivergence == nil {
		reason = errors.New("not configured")
	} else if err := svcCtx.Config.ValidateGuildCheck(); err != nil {
		reason = fmt.Errorf("invalid config: %w", err)
	}
	if reason != nil {
		v := guildUnavailable(g, metrics.RollbackGuildResultUnavailable, reason)
		return nil, &v
	}
	return svcCtx.GuildDivergence, nil
}

// runGuildGate:沉降等待 → 检查 → 裁决(07 §7.5.2、§7.5.4、§7.7 处置一)。除放行时的 ACCEPTED 审计外不写任何东西。
// 检查阶段恰好记一个指标 result(归类优先级见 07 §7.9.1)。
func runGuildGate(ctx context.Context, svcCtx *svc.ServiceContext, checker guildcheck.GuildDivergenceChecker, g guildGateRequest, plan guildPlan) guildVerdict {
	// 沉降用入口 ctx:此时还没写任何东西,调用方取消就该停。
	if err := guildSleep(svcCtx)(ctx, guildSettleDelay); err != nil {
		return guildUnavailable(g, metrics.RollbackGuildResultBudget, fmt.Errorf("settle wait interrupted: %w", err))
	}

	// 检查阶段总预算只罩检查本身(沉降之后、第一笔写之前),不罩写阶段。不重试。
	budget := time.Duration(svcCtx.Config.GuildCheckBudgetSeconds) * time.Second
	checkCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	res, err := checker.ListDivergences(checkCtx, plan.sinceMs)
	metrics.ObserveRollbackGuildCheckSeconds(g.scope, time.Since(start))
	if err != nil {
		result := metrics.RollbackGuildResultUnavailable
		switch {
		case errors.Is(err, guildcheck.ErrTooManyDivergences):
			// 放行也不管用:上万行 ERROR 日志既进不全 Loki,也没人能照单补偿。
			result = metrics.RollbackGuildResultTruncated
			err = fmt.Errorf("%w; narrow the scope and roll back in batches", err)
		case checkCtx.Err() != nil:
			result = metrics.RollbackGuildResultBudget
		}
		return guildUnavailable(g, result, err)
	}

	divergent, unprovable := len(res.Divergences), len(res.UnprovablePlayerIDs)
	if divergent == 0 && unprovable == 0 {
		metrics.ObserveRollbackGuildCheck(g.scope, metrics.RollbackGuildResultClean)
		return guildVerdict{code: constants.ErrCodeOK, check: res}
	}
	// 被拒时的归类:有分歧行就是 divergent_rejected(无论是否同时有不可证明玩家);只有不可证明玩家才是 retention。
	rejectedResult := metrics.RollbackGuildResultDivergentRejected
	if divergent == 0 {
		rejectedResult = metrics.RollbackGuildResultRetention
	}

	if !g.accept {
		// 运维要靠 unprovable 日志知道是谁挡住了;分歧行只回计数与样本,不逐行打日志(拒绝是闸在正常工作)。
		logUnprovablePlayers(g, res, plan, false)
		logx.Infof("[Rollback][GuildDivergence] rejected %s total=%d unprovable_players=%d "+
			"(to override: accept_guild_divergence=true + x-admin-token + non-empty reason/operator)",
			g.logFields(), divergent, unprovable)
		metrics.AddRollbackGuildDivergenceRows(g.scope, false, divergent)
		metrics.ObserveRollbackGuildCheck(g.scope, rejectedResult)
		return guildVerdict{code: constants.ErrCodeRollbackGuildDivergence, check: res}
	}

	// 放行三条件:accept(此处)、x-admin-token(server 层)、reason / operator 非空(入口 validateGuildAccept)。
	// 纵深防御:这里再断言一次,免得将来新增的调用点绕过入口校验就能无痕放行。
	if code := validateGuildAccept(g.accept, g.reason, g.operator); code != constants.ErrCodeOK {
		metrics.ObserveRollbackGuildCheck(g.scope, rejectedResult)
		return guildVerdict{code: code, check: res}
	}

	// R3 放行必留痕:ACCEPTED 审计先写,写不进 → 拒绝、零写入(口径同 STARTED 审计)。
	// 审计先于逐行日志:审计失败时不能留下一批"已放行"的日志,否则有人会照着它去补偿一次根本没发生的回档。
	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		PlayerID:     g.playerID,
		ZoneID:       g.zoneID,
		RollbackType: g.rollbackType,
		TargetTime:   g.targetTime,
		Reason:       fmt.Sprintf("GUILD_DIVERGENCE_ACCEPTED count=%d unprovable_players=%d: %s", divergent, unprovable, g.reason),
		Operator:     g.operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		metrics.AddRollbackGuildDivergenceRows(g.scope, false, divergent)
		metrics.ObserveRollbackGuildCheck(g.scope, rejectedResult)
		return guildVerdict{
			code:  constants.ErrCodeSnapshotDBError,
			err:   fmt.Errorf("write guild divergence accepted audit: %w", err),
			check: res,
		}
	}
	// 逐行 ERROR(确保进 Loki,05:774 口径),全部在第一笔玩家数据写之前写完。补偿清单就从这里取。
	logGuildDivergenceRows("accepted", g, res.Divergences, plan)
	logUnprovablePlayers(g, res, plan, true)
	logx.Errorf("[Rollback][GuildDivergence] accepted %s total=%d unprovable_players=%d by_kind=%s reason=%q",
		g.logFields(), divergent, unprovable, divergenceKindSummary(res.Divergences), g.reason)
	metrics.AddRollbackGuildDivergenceRows(g.scope, true, divergent)
	metrics.ObserveRollbackGuildCheck(g.scope, metrics.RollbackGuildResultDivergentAccepted)
	return guildVerdict{code: constants.ErrCodeOK, check: res, accepted: true}
}

// recheckGuildAfterWrite 是写后复查(07 §7.7 处置二),只在至少一个玩家走到写 Redis 之后调用,且必须在栅栏释放之前
// (栅栏一放,玩家登录后新发生的操作同样满足"终结晚于快照",会被当成新分歧误报)。
//
// 用脱钩 ctx(07 §7.5.3-3,先例 shared/assetop 的 settleContext):写阶段可能远超调用方 deadline,调用方超时 / 断开后
// 入口 ctx 已死,沿用它的话等待立即返回、复查必失败 → 每一次这样的回档都被误报成紧急的 post-write,真分歧反而查不出。
// 数据已经写了,这一段必须跑完;它自带 guildRecheckDelay + guildRecheckBudget 的截止时间。
//
// 用同一组 since 再查一次,与检查阶段的 op_id 集合做差;不可证明玩家本身不算新分歧(检查阶段已放行过)。
// 返回 ErrCodeOK,或 ErrCodeRollbackGuildDivergedAfterWrite + 新行(复查没做成时新行为 nil)。数据不自动撤销(Q3 = ①)。
func recheckGuildAfterWrite(ctx context.Context, svcCtx *svc.ServiceContext, checker guildcheck.GuildDivergenceChecker, g guildGateRequest, plan guildPlan, before guildcheck.GuildCheckResult) (uint32, []guildcheck.GuildDivergence) {
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guildRecheckDelay+guildRecheckBudget)
	defer cancel()

	res, err := func() (guildcheck.GuildCheckResult, error) {
		if err := guildSleep(svcCtx)(postCtx, guildRecheckDelay); err != nil {
			return guildcheck.GuildCheckResult{}, fmt.Errorf("recheck wait interrupted: %w", err)
		}
		return checker.ListDivergences(postCtx, plan.sinceMs)
	}()
	if err != nil {
		logx.Errorf("[Rollback][GuildDivergence] post-write recheck failed %s: cannot prove there is no new divergence; "+
			"data is already written — reconcile by hand or undo with pre_rollback_snapshot_id: %v", g.logFields(), err)
		metrics.ObserveRollbackGuildCheck(g.scope, metrics.RollbackGuildResultPostWriteRecheckFailed)
		return constants.ErrCodeRollbackGuildDivergedAfterWrite, nil
	}

	known := make(map[uint64]struct{}, len(before.Divergences))
	for _, d := range before.Divergences {
		known[d.OpID] = struct{}{}
	}
	var fresh []guildcheck.GuildDivergence
	for _, d := range res.Divergences {
		if _, ok := known[d.OpID]; !ok {
			fresh = append(fresh, d)
		}
	}
	if len(fresh) == 0 {
		return constants.ErrCodeOK, nil
	}
	logGuildDivergenceRows("post-write", g, fresh, plan)
	logx.Errorf("[Rollback][GuildDivergence] post-write %s total=%d: data is already written — compensate each op_id by hand "+
		"or undo with pre_rollback_snapshot_id", g.logFields(), len(fresh))
	metrics.ObserveRollbackGuildCheck(g.scope, metrics.RollbackGuildResultPostWriteDivergent)
	return constants.ErrCodeRollbackGuildDivergedAfterWrite, fresh
}

// logGuildDivergenceRows 逐行写分歧(ERROR,一行一条;07 §7.5.4 的固定格式)。tag 是 "accepted" / "post-write"。
// player_id 只进日志,不进指标 label。ACTIVITY_REWARD 的两个 delta 恒为 0,补偿时按 op_id 回查 guild_asset_op.payload。
func logGuildDivergenceRows(tag string, g guildGateRequest, rows []guildcheck.GuildDivergence, plan guildPlan) {
	for _, d := range rows {
		logx.Errorf("[Rollback][GuildDivergence] %s %s op_id=%d player_id=%d guild_id=%d kind=%d status=%d "+
			"funds_delta=%d contribution_delta=%d updated_ms=%d snapshot_created_at=%d",
			tag, g.logFields(), d.OpID, d.PlayerID, d.GuildID, d.Kind, d.Status,
			d.FundsDelta, d.ContributionDelta, d.UpdatedMs, plan.snapAt[d.PlayerID])
	}
}

// logUnprovablePlayers 按块写"不可证明"的玩家(每行 ≤ unprovableLogBatch 个 id;07 §7.5.4)。
// 设计写的是拒绝时 WARN、放行时 ERROR;go-zero 的 logx 没有 WARN 级别,两种都取 ERROR
// (同 dataserviceserver.go playerNameStatus 的先例):拒绝时运维要靠它知道是谁挡住了,淹在 Info 里等于没有。
// 不设上限:逐人没有可补偿的清单,上限只会把"老 zone 永远回不了档"的问题搬回来。
func logUnprovablePlayers(g guildGateRequest, res guildcheck.GuildCheckResult, plan guildPlan, accepted bool) {
	state := "rejected"
	if accepted {
		state = "accepted"
	}
	ids := res.UnprovablePlayerIDs
	for start := 0; start < len(ids); start += unprovableLogBatch {
		batch := ids[start:min(start+unprovableLogBatch, len(ids))]
		oldest := plan.sinceMs[batch[0]]
		parts := make([]string, 0, len(batch))
		for _, id := range batch {
			oldest = min(oldest, plan.sinceMs[id])
			parts = append(parts, strconv.FormatUint(id, 10))
		}
		logx.Errorf("[Rollback][GuildDivergence] unprovable %s %s cutoff_ms=%d oldest_since_ms=%d player_ids=%s",
			state, g.logFields(), res.RetentionCutoffMs, oldest, strings.Join(parts, ","))
	}
}

// divergenceKindSummary 汇总 by_kind=donate:<a>,shop:<b>,activity:<c>,other:<d>。kind 数值取 guild_db.proto 的枚举,不写字面量。
func divergenceKindSummary(rows []guildcheck.GuildDivergence) string {
	var donate, shop, activity, other int
	for _, d := range rows {
		switch d.Kind {
		case uint32(guildpb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE):
			donate++
		case uint32(guildpb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP):
			shop++
		case uint32(guildpb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD):
			activity++
		default:
			other++
		}
	}
	return fmt.Sprintf("donate:%d,shop:%d,activity:%d,other:%d", donate, shop, activity, other)
}

// ── RollbackPlayer ─────────────────────────────────────────────

type RollbackPlayerReq struct {
	PlayerID   uint64
	SnapshotID uint64
	TargetTime uint64
	Scope      uint32   // 0=full, 1=partial
	Fields     []string // only for partial
	Reason     string
	Operator   string
	// AcceptGuildDivergence:明知帮会资产有分歧(或快照早于保留期无法证明)仍要回档(07 §7.5.4)。
	// 要求 Reason / Operator 非空;x-admin-token 由 server 层验。
	AcceptGuildDivergence bool
	// Caller:调用方身份(对端地址 + 自报 user-agent / x-operator),只进帮会闸日志,不是鉴权依据。
	Caller string
}

type RollbackPlayerResp struct {
	ErrorCode             uint32
	SnapshotIDUsed        uint64
	PreRollbackSnapshotID uint64
	FieldsRestored        []string
	GuildGateFields

	// writeAttempted:走到了写 Redis 那一步(结果未知也算)。写后复查的触发条件;不出响应。
	writeAttempted bool
}

func RollbackPlayer(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackPlayerReq) (*RollbackPlayerResp, error) {
	if req == nil || req.PlayerID == 0 {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if req.SnapshotID == 0 && req.TargetTime == 0 {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if code := validateGuildAccept(req.AcceptGuildDivergence, req.Reason, req.Operator); code != constants.ErrCodeOK {
		return &RollbackPlayerResp{ErrorCode: code}, nil
	}
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackPlayerResp{ErrorCode: code}, err
	}

	release, code, err := acquirePlayerRollbackFence(ctx, svcCtx, req.PlayerID)
	if code != constants.ErrCodeOK || err != nil {
		return &RollbackPlayerResp{ErrorCode: code}, err
	}
	defer release()

	// 先写请求意图再做任何修改：审计库不可用时，高风险回档必须零变更。
	// 被帮会闸拒绝的尝试同样留痕(谁、何时、想回到哪),所以 STARTED 在检查之前。
	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		PlayerID:     req.PlayerID,
		RollbackType: rollbackTypePlayer,
		TargetTime:   req.TargetTime,
		Reason:       fmt.Sprintf("STARTED: %s", req.Reason),
		Operator:     req.Operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("write player rollback intent audit: %w", err)
	}

	resp, rollbackErr := rollbackPlayerWithFenceHeld(ctx, svcCtx, req)
	if resp == nil {
		resp = &RollbackPlayerResp{ErrorCode: constants.ErrCodeRollbackFailed}
	}

	// 写后复查报警时数据已经写了:照样算"恢复了一个玩家"。
	restored := rollbackErr == nil &&
		(resp.ErrorCode == constants.ErrCodeOK || resp.ErrorCode == constants.ErrCodeRollbackGuildDivergedAfterWrite)
	var affected, failed uint32
	if restored {
		affected = 1
	} else {
		failed = 1
	}

	auditCtx, cancel := detachedAuditContext(ctx)
	defer cancel()
	if err := insertRollbackAudit(auditCtx, svcCtx, &store.AuditLogRow{
		PlayerID:              req.PlayerID,
		RollbackType:          rollbackTypePlayer,
		SnapshotIDUsed:        resp.SnapshotIDUsed,
		PreRollbackSnapshotID: resp.PreRollbackSnapshotID,
		TargetTime:            req.TargetTime,
		PlayersAffected:       affected,
		PlayersFailed:         failed,
		Reason:                rollbackResultReason(resp.ErrorCode, resp.GuildGateFields, "", req.Reason),
		Operator:              req.Operator,
		CreatedAt:             uint64(time.Now().Unix()),
	}); err != nil {
		resp.ErrorCode = constants.ErrCodeSnapshotDBError
		metrics.ObserveRollback("player", "failed", 0, 0)
		return resp, errors.Join(rollbackErr, fmt.Errorf("write player rollback result audit: %w", err))
	}

	if !restored {
		metrics.ObserveRollback("player", "failed", 0, 0)
		logx.Errorf("[Rollback] player %d failed: err=%v code=%d", req.PlayerID, rollbackErr, resp.ErrorCode)
		return resp, rollbackErr
	}
	metrics.ObserveRollback("player", "ok", 1, 0)

	if resp.ErrorCode == constants.ErrCodeRollbackGuildDivergedAfterWrite {
		logx.Errorf("[Rollback] player %d restored from snapshot %d (pre-rollback=%d) by %s, but the post-write guild recheck flagged it (code=%d): %s",
			req.PlayerID, resp.SnapshotIDUsed, resp.PreRollbackSnapshotID, req.Operator, resp.ErrorCode, req.Reason)
		return resp, nil
	}
	logx.Infof("[Rollback] player %d restored from snapshot %d (pre-rollback=%d) by %s: %s",
		req.PlayerID, resp.SnapshotIDUsed, resp.PreRollbackSnapshotID, req.Operator, req.Reason)

	return resp, nil
}

// rollbackPlayerWithFenceHeld:装配预检 → 计划 → 帮会闸 → 执行 → 写后复查。
// 调用方持玩家栅栏、已写 STARTED 审计,并负责 RESULT 审计。ROLLBACK_PARTIAL 同样过闸(07 R6)。
func rollbackPlayerWithFenceHeld(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackPlayerReq) (*RollbackPlayerResp, error) {
	g := guildGateRequest{
		scope:        metrics.RollbackGuildScopePlayer,
		rollbackType: rollbackTypePlayer,
		playerID:     req.PlayerID,
		targetTime:   req.TargetTime,
		accept:       req.AcceptGuildDivergence,
		reason:       req.Reason,
		operator:     req.Operator,
		caller:       req.Caller,
	}
	checker, rejected := guildGatePreflight(svcCtx, g)
	if rejected != nil {
		return &RollbackPlayerResp{ErrorCode: rejected.code}, rejected.err
	}

	// 计划:解析本次实际要用的那份快照(只读),它的 created_at 就是 S_p。不能直接用 target_time:
	// 快照可能比它早几小时,中间那段的操作会漏(07 §7.2)。
	snap, err := resolveSnapshot(ctx, svcCtx, req.PlayerID, req.SnapshotID, req.TargetTime)
	if err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}
	if snap == nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotNotFound}, nil
	}
	plan, err := newGuildPlan(map[uint64]uint64{req.PlayerID: snap.CreatedAt}, svcCtx.Config.GuildClockSkewMarginMs)
	if err != nil {
		v := guildUnavailable(g, metrics.RollbackGuildResultUnavailable, err)
		return &RollbackPlayerResp{ErrorCode: v.code}, v.err
	}

	verdict := runGuildGate(ctx, svcCtx, checker, g, plan)
	gateFields := guildGateFieldsOf(verdict.check.Divergences, len(verdict.check.UnprovablePlayerIDs))
	if verdict.code != constants.ErrCodeOK {
		return &RollbackPlayerResp{ErrorCode: verdict.code, GuildGateFields: gateFields}, verdict.err
	}

	resp, execErr := rollbackSinglePlayer(ctx, svcCtx, req, snap.CreatedAt)
	if resp == nil {
		resp = &RollbackPlayerResp{ErrorCode: constants.ErrCodeRollbackFailed}
	}
	resp.GuildGateFields = gateFields
	if !resp.writeAttempted {
		return resp, execErr
	}

	postCode, fresh := recheckGuildAfterWrite(ctx, svcCtx, checker, g, plan, verdict.check)
	if postCode == constants.ErrCodeOK {
		return resp, execErr
	}
	// 写后分歧比写失败更紧急:数据可能已经写了一部分。原错误进日志,响应带上新行,err 置空让字段带得出去。
	if execErr != nil || resp.ErrorCode != constants.ErrCodeOK {
		logx.Errorf("[Rollback] player %d: write reported code=%d err=%v, and the post-write guild recheck flagged it",
			req.PlayerID, resp.ErrorCode, execErr)
	}
	resp.ErrorCode = postCode
	resp.GuildGateFields = guildGateFieldsOf(fresh, 0)
	return resp, nil
}

// rollbackSinglePlayer 执行一个玩家的回档:解析快照 → 安全快照 → 写 Redis。只能在帮会闸放行之后调用。
// plannedCreatedAt 是检查时用的快照时刻(R5 的下界)。
func rollbackSinglePlayer(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackPlayerReq, plannedCreatedAt uint64) (*RollbackPlayerResp, error) {
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackPlayerResp{ErrorCode: code}, err
	}

	// 1. Resolve target snapshot
	snap, err := resolveSnapshot(ctx, svcCtx, req.PlayerID, req.SnapshotID, req.TargetTime)
	if err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}
	if snap == nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotNotFound}, nil
	}

	// R5(07 §7.5.3-1):执行期实际选中的快照不得早于帮会检查时用的那份 —— 更早就意味着"两份快照之间"的
	// 帮会操作根本没被检查过。执行期的 GetLatestSnapshotBefore 不带 zone 条件,正常只会等于或晚于计划值;
	// 更早只可能是快照被并发删除。该玩家失败、不写(连安全快照都不打)。
	if snap.CreatedAt < plannedCreatedAt {
		logx.Errorf("[Rollback][GuildDivergence] player %d: executing snapshot %d created_at=%d is older than the planned %d (R5); not written",
			req.PlayerID, snap.ID, snap.CreatedAt, plannedCreatedAt)
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeRollbackGuildCheckFailed, SnapshotIDUsed: snap.ID},
			fmt.Errorf("player %d: snapshot %d created_at %d is older than the guild-checked %d", req.PlayerID, snap.ID, snap.CreatedAt, plannedCreatedAt)
	}

	// 2. Deserialize snapshot data
	var sd snapshotData
	if err := json.Unmarshal(snap.Data, &sd); err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("unmarshal snapshot %d: %w", snap.ID, err)
	}

	// 3. Create pre-rollback safety snapshot (so we can undo the rollback if needed)
	preSnap, err := CreatePlayerSnapshot(ctx, svcCtx, &CreateSnapshotReq{
		PlayerID:     req.PlayerID,
		SnapshotType: snapshotTypePreRollback,
		Reason:       fmt.Sprintf("pre-rollback safety snapshot (target snapshot=%d)", snap.ID),
		Operator:     req.Operator,
	})
	if err != nil {
		code := constants.ErrCodeSnapshotDBError
		if preSnap != nil && preSnap.ErrorCode != constants.ErrCodeOK {
			code = preSnap.ErrorCode
		}
		return &RollbackPlayerResp{ErrorCode: code, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d: %w", req.PlayerID, err)
	}
	if preSnap == nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d returned nil response", req.PlayerID)
	}
	if preSnap.ErrorCode != constants.ErrCodeOK {
		return &RollbackPlayerResp{ErrorCode: preSnap.ErrorCode, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d failed with code %d", req.PlayerID, preSnap.ErrorCode)
	}
	if preSnap.SnapshotID == 0 {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d returned snapshot id 0", req.PlayerID)
	}
	preRollbackID := preSnap.SnapshotID

	// 4. Determine which fields to restore
	fieldsToRestore := sd.Fields
	if req.Scope == 1 && len(req.Fields) > 0 {
		// Partial rollback — only restore requested fields
		filtered := make(map[string][]byte, len(req.Fields))
		for _, f := range req.Fields {
			if val, ok := sd.Fields[f]; ok {
				filtered[f] = val
			}
		}
		fieldsToRestore = filtered
	}

	if len(fieldsToRestore) == 0 {
		return &RollbackPlayerResp{
			ErrorCode:             constants.ErrCodeSnapshotNotFound,
			SnapshotIDUsed:        snap.ID,
			PreRollbackSnapshotID: preRollbackID,
		}, nil
	}

	// 5. Write snapshot data to Redis (overwrite current player data)
	// 从这里起数据可能已经写了(出错也可能是写成功后回包丢了):写后复查以此为触发条件。
	saveResp, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID:        req.PlayerID,
		Data:            fieldsToRestore,
		ExpectedVersion: 0, // skip version check — this is an admin override
	})
	if err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeRedis, SnapshotIDUsed: snap.ID,
			PreRollbackSnapshotID: preRollbackID, writeAttempted: true}, err
	}
	if saveResp.ErrorCode != constants.ErrCodeOK {
		return &RollbackPlayerResp{ErrorCode: saveResp.ErrorCode, SnapshotIDUsed: snap.ID,
			PreRollbackSnapshotID: preRollbackID, writeAttempted: true}, nil
	}

	// 6. Collect field names
	restoredFields := make([]string, 0, len(fieldsToRestore))
	for f := range fieldsToRestore {
		restoredFields = append(restoredFields, f)
	}

	return &RollbackPlayerResp{
		SnapshotIDUsed:        snap.ID,
		PreRollbackSnapshotID: preRollbackID,
		FieldsRestored:        restoredFields,
		writeAttempted:        true,
	}, nil
}

// ── RollbackZone ───────────────────────────────────────────────

type RollbackZoneReq struct {
	ZoneID     uint32
	TargetTime uint64
	Reason     string
	Operator   string
	// AcceptGuildDivergence / Caller:语义同 RollbackPlayerReq。
	AcceptGuildDivergence bool
	Caller                string
}

type RollbackZoneResp struct {
	ErrorCode       uint32
	PlayersAffected uint32
	PlayersFailed   uint32
	FailedPlayerIDs []uint64
	// OrphanPlayerIDs 是**候选**名单:zone 内当前存在、但在 target_time 之前没有
	// 任何快照的玩家。它不代表这些角色确实是 target_time 之后创建的,也不代表
	// 它们被删除了 —— 见 reportOrphanCandidates。
	OrphanPlayerIDs []uint64
	// OrphansCleaned 恒为 0:本服务不再自动删除孤儿候选。字段保留是为了不破坏
	// 已有调用方与 proto 兼容性(只减少语义、不改编号)。
	OrphansCleaned uint32
	GuildGateFields
}

func RollbackZone(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackZoneReq) (*RollbackZoneResp, error) {
	if req == nil || req.ZoneID == 0 || req.TargetTime == 0 {
		return &RollbackZoneResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if code := validateGuildAccept(req.AcceptGuildDivergence, req.Reason, req.Operator); code != constants.ErrCodeOK {
		return &RollbackZoneResp{ErrorCode: code}, nil
	}
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackZoneResp{ErrorCode: code}, err
	}

	release, code, err := acquireZoneRollbackFence(ctx, svcCtx, req.ZoneID)
	if code != constants.ErrCodeOK || err != nil {
		return &RollbackZoneResp{ErrorCode: code}, err
	}
	defer release()

	resp, rollbackErr := rollbackZoneWithFenceHeld(ctx, svcCtx, req)
	outcome := "ok"
	restored := rollbackErr == nil &&
		(resp.ErrorCode == constants.ErrCodeOK || resp.ErrorCode == constants.ErrCodeRollbackGuildDivergedAfterWrite)
	if !restored {
		outcome = "failed"
	} else if resp.PlayersFailed > 0 && resp.PlayersAffected > 0 {
		outcome = "partial"
	} else if resp.PlayersFailed > 0 {
		outcome = "failed"
	}
	metrics.ObserveRollback("zone", outcome, resp.PlayersAffected, resp.OrphansCleaned)
	return resp, rollbackErr
}

// rollbackZoneWithFenceHeld 只能在上层持有 zone 级跨服务离线 epoch 栅栏时调用。
// 它不会自行做一次有 TOCTOU 窗口的“在线查询”。
//
// 顺序:STARTED 审计 → 装配预检 → 计划(整份清单 + 每人快照时刻,只读)→ 沉降 → 帮会检查
// → 逐玩家执行 → 写后复查 → RESULT 审计。整份清单查完才写第一个玩家(R4)。
func rollbackZoneWithFenceHeld(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackZoneReq) (*RollbackZoneResp, error) {
	g := guildGateRequest{
		scope:        metrics.RollbackGuildScopeZone,
		rollbackType: rollbackTypeZone,
		zoneID:       req.ZoneID,
		targetTime:   req.TargetTime,
		accept:       req.AcceptGuildDivergence,
		reason:       req.Reason,
		operator:     req.Operator,
		caller:       req.Caller,
	}
	zr, err := beginZoneRollback(ctx, svcCtx, req)
	if err != nil {
		return &RollbackZoneResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}

	checker, rejected := guildGatePreflight(svcCtx, g)
	if rejected != nil {
		return zr.complete(ctx, svcCtx, rejected.code, rejected.err)
	}
	if code, err := zr.plan(ctx, svcCtx); code != constants.ErrCodeOK {
		return zr.complete(ctx, svcCtx, code, err)
	}
	plan, err := newGuildPlan(zr.snapAt, svcCtx.Config.GuildClockSkewMarginMs)
	if err != nil {
		v := guildUnavailable(g, metrics.RollbackGuildResultUnavailable, err)
		return zr.complete(ctx, svcCtx, v.code, v.err)
	}

	verdict := runGuildGate(ctx, svcCtx, checker, g, plan)
	zr.resp.GuildGateFields = guildGateFieldsOf(verdict.check.Divergences, len(verdict.check.UnprovablePlayerIDs))
	if verdict.code != constants.ErrCodeOK {
		return zr.complete(ctx, svcCtx, verdict.code, verdict.err)
	}

	code, execErr := zr.execute(ctx, svcCtx)
	if zr.wrote {
		postCode, fresh := recheckGuildAfterWrite(ctx, svcCtx, checker, g, plan, verdict.check)
		if postCode != constants.ErrCodeOK {
			if execErr != nil || code != constants.ErrCodeOK {
				logx.Errorf("[Rollback] zone %d: execution reported code=%d err=%v, and the post-write guild recheck flagged it",
					req.ZoneID, code, execErr)
			}
			code, execErr = postCode, nil
			zr.resp.GuildGateFields = guildGateFieldsOf(fresh, 0)
		}
	}
	return zr.complete(ctx, svcCtx, code, execErr)
}

// zoneRollback 是一个 zone 的回档:计划(清单 + 每人快照时刻,只读)与执行结果。
// RollbackZone 用一个;RollbackAll 先为全部 zone 建好计划、合并过一次帮会闸,再逐个执行(R4)。
// 生命周期:beginZoneRollback(STARTED 审计)→ plan → execute → writeResult(RESULT 审计)。
// 一旦 begin 成功,无论走到哪一步,都必须 writeResult,不留悬空的 STARTED。
type zoneRollback struct {
	req       *RollbackZoneReq
	playerIDs []uint64          // 升序,执行顺序
	snapAt    map[uint64]uint64 // player → 计划快照时刻 S_p(秒):帮会检查的起点、R5 的下界
	resp      RollbackZoneResp
	wrote     bool   // 至少一个玩家走到了写 Redis:写后复查的触发条件
	note      string // RESULT 审计的附注(例如全服回档中止时"没执行")
}

// beginZoneRollback 写 zone 的 STARTED 审计:先写意图再做任何事,审计库不可用时零变更;被拒绝的尝试同样留痕。
func beginZoneRollback(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackZoneReq) (*zoneRollback, error) {
	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		ZoneID:       req.ZoneID,
		RollbackType: rollbackTypeZone,
		TargetTime:   req.TargetTime,
		Reason:       fmt.Sprintf("STARTED: %s", req.Reason),
		Operator:     req.Operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		return nil, fmt.Errorf("write zone rollback intent audit: %w", err)
	}
	logx.Infof("[Rollback] zone=%d target_time=%d operator=%s reason=%s accept_guild_divergence=%t",
		req.ZoneID, req.TargetTime, req.Operator, req.Reason, req.AcceptGuildDivergence)
	return &zoneRollback{req: req}, nil
}

// plan 取清单与每人快照时刻(一条 SQL,只读;07 §7.5.3-1)。清单用快照当时的 zone_id。
func (zr *zoneRollback) plan(ctx context.Context, svcCtx *svc.ServiceContext) (uint32, error) {
	times, err := svcCtx.SnapshotStore.GetSnapshotPlayerTimesByZone(ctx, zr.req.ZoneID, zr.req.TargetTime)
	if err != nil {
		logx.Errorf("[Rollback] zone %d: failed to list players: %v", zr.req.ZoneID, err)
		return constants.ErrCodeSnapshotDBError, err
	}
	zr.snapAt = times
	zr.playerIDs = make([]uint64, 0, len(times))
	for pid := range times {
		zr.playerIDs = append(zr.playerIDs, pid)
	}
	slices.Sort(zr.playerIDs)
	logx.Infof("[Rollback] zone %d: found %d players to rollback", zr.req.ZoneID, len(zr.playerIDs))
	return constants.ErrCodeOK, nil
}

// execute 逐玩家回档 + 报告孤儿候选。只能在帮会闸放行之后调用。
// 单个玩家失败只计入 PlayersFailed、继续下一个(原样);孤儿扫描失败是整个 zone 的错误。
func (zr *zoneRollback) execute(ctx context.Context, svcCtx *svc.ServiceContext) (uint32, error) {
	// NOTE: 帮会 / 好友的**关系**数据不随回档恢复(guild / friend 是独立的 Go 服务,玩家 blob 里没有
	// guild_id 或好友引用),但帮会**资产**不是"会自己愈合的小不一致":回档会把捐献的钱还给玩家而帮会
	// 资金与帮贡还在(复制),或把商店买到的物品抹掉而帮贡不回(白亏)。这部分由上面的帮会资产闸处理:
	// 有分歧默认拒绝,放行必留痕、事后按 op_id 人工补偿,见 docs/design/guild-phase2/07-rollback-fail-closed.md。

	// ── Phase 1: Rollback player data from individual snapshots ─
	for _, pid := range zr.playerIDs {
		resp, err := rollbackSinglePlayer(ctx, svcCtx, &RollbackPlayerReq{
			PlayerID:   pid,
			TargetTime: zr.req.TargetTime,
			Reason:     fmt.Sprintf("zone %d rollback: %s", zr.req.ZoneID, zr.req.Reason),
			Operator:   zr.req.Operator,
		}, zr.snapAt[pid])
		if resp.writeAttempted {
			zr.wrote = true
		}
		if err != nil || resp.ErrorCode != constants.ErrCodeOK {
			zr.resp.PlayersFailed++
			zr.resp.FailedPlayerIDs = append(zr.resp.FailedPlayerIDs, pid)
			logx.Errorf("[Rollback] zone %d player %d failed: err=%v code=%d",
				zr.req.ZoneID, pid, err, resp.ErrorCode)
			continue
		}
		zr.resp.PlayersAffected++
	}

	// ── Phase 2: 报告孤儿候选(只报告,不删除)────────────────
	snapshotPlayerSet := make(map[uint64]bool, len(zr.playerIDs))
	for _, pid := range zr.playerIDs {
		snapshotPlayerSet[pid] = true
	}
	orphanIDs, err := reportOrphanCandidates(ctx, svcCtx, zr.req.ZoneID, snapshotPlayerSet)
	if err != nil {
		return constants.ErrCodeRedis, err
	}
	zr.resp.OrphanPlayerIDs = orphanIDs
	// “无快照”不能提前等价成“zone 不存在”。先扫描当前映射，才能把整个
	// 无快照 zone 的玩家全部作为候选返回。只有快照与当前映射都为空时才是空 zone。
	if len(zr.playerIDs) == 0 && len(orphanIDs) == 0 {
		return constants.ErrCodeZoneNotFound, nil
	}
	// OrphansCleaned 恒为 0:本服务不再据此删除任何玩家数据,见 reportOrphanCandidates。
	logx.Infof("[Rollback] zone %d complete: affected=%d failed=%d orphans_cleaned=0",
		zr.req.ZoneID, zr.resp.PlayersAffected, zr.resp.PlayersFailed)
	return constants.ErrCodeOK, nil
}

// writeResult 定下 zone 的响应码并写 RESULT 审计(脱钩 ctx)。审计写不进 → 响应码改成 SnapshotDBError 并返回该错误。
func (zr *zoneRollback) writeResult(ctx context.Context, svcCtx *svc.ServiceContext, code uint32) error {
	zr.resp.ErrorCode = code
	auditCtx, cancel := detachedAuditContext(ctx)
	defer cancel()
	if err := insertRollbackAudit(auditCtx, svcCtx, &store.AuditLogRow{
		ZoneID:          zr.req.ZoneID,
		RollbackType:    rollbackTypeZone,
		TargetTime:      zr.req.TargetTime,
		PlayersAffected: zr.resp.PlayersAffected,
		PlayersFailed:   zr.resp.PlayersFailed,
		OrphansCleaned:  0,
		Reason:          rollbackResultReason(code, zr.resp.GuildGateFields, zr.note, zr.req.Reason),
		Operator:        zr.req.Operator,
		CreatedAt:       uint64(time.Now().Unix()),
	}); err != nil {
		zr.resp.ErrorCode = constants.ErrCodeSnapshotDBError
		return fmt.Errorf("write zone %d rollback result audit: %w", zr.req.ZoneID, err)
	}
	return nil
}

// complete 是单 zone 回档的收尾:RESULT 审计 + 返回响应。审计失败的错误与原错误合并返回。
func (zr *zoneRollback) complete(ctx context.Context, svcCtx *svc.ServiceContext, code uint32, err error) (*RollbackZoneResp, error) {
	if auditErr := zr.writeResult(ctx, svcCtx, code); auditErr != nil {
		return &zr.resp, errors.Join(err, auditErr)
	}
	return &zr.resp, err
}

// reportOrphanCandidates 列出「zone 内当前存在、但在 target_time 之前没有任何快照」
// 的玩家,**只报告,不做任何删除**。
//
// 为什么改成只报告(这里原来会硬删,是一条 P0):
//
//	判定依据 GetSnapshotPlayerIDsByZone 查的是 `player_snapshot WHERE zone_id=? AND
//	created_at<=?`,也就是"该玩家在这个 zone 有一份 target_time 之前的快照"。
//	而快照**只在显式触发时才产生** —— CreatePlayerSnapshot / CreateEventSnapshot 的
//	GM 调用,加上回档自己打的 pre-rollback 安全快照;全仓没有任何周期性快照任务。
//	于是"没有快照"根本不等于"target_time 之后才创建":
//	  * 从没触发过快照事件的普通老玩家 —— 绝大多数玩家都是这一类;
//	  * 快照已被 DeleteOldSnapshots 按保留期删掉的老玩家;
//	  * 从别的 zone 迁过来、历史快照的 zone_id 还停在旧 zone 的玩家。
//
//	旧实现对这些人执行 DeletePlayerData(DeleteZoneMapping=true) 并调 login
//	RemovePlayersFromAccounts 把角色从账号里摘掉,而且**不走** rollbackSinglePlayer
//	的 pre-rollback 安全快照 —— 删完无从恢复。一次例行的 zone 回档就能把整个 zone
//	的老玩家抹掉。
//
// 要恢复自动清理,前提是拿到**权威的角色创建时间**(login/account 记录,或玩家数据
// 里的 created_at 字段)并按它判定,而不是拿快照存在性当代理;在那之前这里 fail-closed。
// 候选名单仍然通过响应的 OrphanPlayerIDs 返回,供人工核对后走单独的删除工具。
// (清单现由 GetSnapshotPlayerTimesByZone 给出,过滤条件与 GetSnapshotPlayerIDsByZone 相同。)
func reportOrphanCandidates(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, snapshotPlayerSet map[uint64]bool) ([]uint64, error) {
	currentPlayers, err := svcCtx.Router.GetAllPlayerIDsInZone(ctx, zoneID)
	if err != nil {
		logx.Errorf("[Rollback] zone %d: failed to scan current players for orphan report: %v", zoneID, err)
		return nil, fmt.Errorf("scan zone %d orphan candidates: %w", zoneID, err)
	}

	var orphanIDs []uint64
	for _, pid := range currentPlayers {
		if snapshotPlayerSet[pid] {
			continue // 在 target_time 之前有快照,肯定不是新建角色
		}
		orphanIDs = append(orphanIDs, pid)
	}

	if len(orphanIDs) > 0 {
		logx.Errorf("[Rollback] zone %d: %d players have no snapshot at or before target_time. "+
			"NOT deleted — snapshot absence does not prove the character was created after target_time. "+
			"Candidate IDs are returned in OrphanPlayerIDs for manual review.",
			zoneID, len(orphanIDs))
	}
	return orphanIDs, nil
}

// ── RollbackAll (full server) ──────────────────────────────────

type RollbackAllReq struct {
	TargetTime uint64
	Reason     string
	Operator   string
	// AcceptGuildDivergence / Caller:语义同 RollbackPlayerReq。
	AcceptGuildDivergence bool
	Caller                string
}

type RollbackAllResp struct {
	ErrorCode       uint32
	ZonesProcessed  uint32
	PlayersAffected uint32
	PlayersFailed   uint32
	GuildGateFields
}

func RollbackAll(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackAllReq) (*RollbackAllResp, error) {
	if req == nil || req.TargetTime == 0 {
		return &RollbackAllResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if code := validateGuildAccept(req.AcceptGuildDivergence, req.Reason, req.Operator); code != constants.ErrCodeOK {
		return &RollbackAllResp{ErrorCode: code}, nil
	}
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackAllResp{ErrorCode: code}, err
	}

	zoneIDs := svcCtx.Router.AllZoneIDs()
	// AllZoneIDs 来自 map 遍历,顺序不定;固定成升序,审计与日志的先后可复现。
	slices.Sort(zoneIDs)
	release, code, err := acquireServerRollbackFence(ctx, svcCtx, zoneIDs)
	if code != constants.ErrCodeOK || err != nil {
		return &RollbackAllResp{ErrorCode: code}, err
	}
	defer release()

	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		RollbackType: rollbackTypeServer,
		TargetTime:   req.TargetTime,
		Reason:       fmt.Sprintf("STARTED: %s", req.Reason),
		Operator:     req.Operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		return &RollbackAllResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("write server rollback intent audit: %w", err)
	}

	logx.Infof("[Rollback] FULL SERVER rollback target_time=%d operator=%s reason=%s accept_guild_divergence=%t",
		req.TargetTime, req.Operator, req.Reason, req.AcceptGuildDivergence)

	resp, rollbackErr := rollbackAllWithFenceHeld(ctx, svcCtx, req, zoneIDs)

	auditCtx, cancel := detachedAuditContext(ctx)
	defer cancel()
	if err := insertRollbackAudit(auditCtx, svcCtx, &store.AuditLogRow{
		RollbackType:    rollbackTypeServer,
		TargetTime:      req.TargetTime,
		PlayersAffected: resp.PlayersAffected,
		PlayersFailed:   resp.PlayersFailed,
		Reason:          rollbackResultReason(resp.ErrorCode, resp.GuildGateFields, "", req.Reason),
		Operator:        req.Operator,
		CreatedAt:       uint64(time.Now().Unix()),
	}); err != nil {
		resp.ErrorCode = constants.ErrCodeSnapshotDBError
		metrics.ObserveRollback("server", "failed", resp.PlayersAffected, 0)
		return resp, errors.Join(rollbackErr, fmt.Errorf("write server rollback result audit: %w", err))
	}

	restored := rollbackErr == nil &&
		(resp.ErrorCode == constants.ErrCodeOK || resp.ErrorCode == constants.ErrCodeRollbackGuildDivergedAfterWrite)
	if !restored {
		metrics.ObserveRollback("server", "failed", resp.PlayersAffected, 0)
		logx.Errorf("[Rollback] FULL SERVER failed: code=%d zones=%d affected=%d failed=%d err=%v",
			resp.ErrorCode, resp.ZonesProcessed, resp.PlayersAffected, resp.PlayersFailed, rollbackErr)
		return resp, rollbackErr
	}

	logx.Infof("[Rollback] FULL SERVER complete: code=%d zones=%d affected=%d failed=%d",
		resp.ErrorCode, resp.ZonesProcessed, resp.PlayersAffected, resp.PlayersFailed)

	rbOutcome := "ok"
	if resp.PlayersFailed > 0 && resp.PlayersAffected == 0 {
		rbOutcome = "failed"
	} else if resp.PlayersFailed > 0 {
		rbOutcome = "partial"
	}
	metrics.ObserveRollback("server", rbOutcome, resp.PlayersAffected, 0)

	return resp, nil
}

// rollbackAllWithFenceHeld:先为**全部** zone 写 STARTED 审计并取清单,合并过一次帮会闸,全部通过才开始写第一个 zone
// (R4:逐 zone 边查边写会出现 zone 1 已回档、zone 2 被拒的半截全服回档);写完后一次等待、合并复查。
//
// 调用方持 server 级栅栏(已覆盖全部 zone,不再逐 zone Acquire)、已写 server 级 STARTED 审计,并负责 server 级 RESULT。
// 帮会闸对全服是**一次**合并检查(scope=server):同一玩家出现在多个 zone 的快照清单里时取最早的快照时刻(多查,方向安全);
// 分歧行上限、指标 result、ACCEPTED 审计都按一次回档 RPC 计。
func rollbackAllWithFenceHeld(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackAllReq, zoneIDs []uint32) (*RollbackAllResp, error) {
	g := guildGateRequest{
		scope:        metrics.RollbackGuildScopeServer,
		rollbackType: rollbackTypeServer,
		targetTime:   req.TargetTime,
		accept:       req.AcceptGuildDivergence,
		reason:       req.Reason,
		operator:     req.Operator,
		caller:       req.Caller,
	}
	resp := &RollbackAllResp{}
	checker, rejected := guildGatePreflight(svcCtx, g)
	if rejected != nil {
		resp.ErrorCode = rejected.code
		return resp, rejected.err
	}

	zones := make([]*zoneRollback, 0, len(zoneIDs))
	// abort:计划或帮会闸没过。已写 STARTED 的 zone 逐个补一条 RESULT(带中止码、零写入),不留悬空的 STARTED。
	abort := func(code uint32, err error) (*RollbackAllResp, error) {
		resp.ErrorCode = code
		for _, zr := range zones {
			zr.note = "not executed: server rollback aborted before any write"
			if auditErr := zr.writeResult(ctx, svcCtx, code); auditErr != nil {
				logx.Errorf("[Rollback] server rollback abort: %v", auditErr)
				resp.ErrorCode = constants.ErrCodeSnapshotDBError
				err = errors.Join(err, auditErr)
			}
		}
		return resp, err
	}

	// ── 计划:逐 zone STARTED 审计 + 清单与每人快照时刻(只读)────
	merged := make(map[uint64]uint64)
	for _, zoneID := range zoneIDs {
		zr, err := beginZoneRollback(ctx, svcCtx, &RollbackZoneReq{
			ZoneID:                zoneID,
			TargetTime:            req.TargetTime,
			Reason:                fmt.Sprintf("server rollback: %s", req.Reason),
			Operator:              req.Operator,
			AcceptGuildDivergence: req.AcceptGuildDivergence,
			Caller:                req.Caller,
		})
		if err != nil {
			return abort(constants.ErrCodeSnapshotDBError, fmt.Errorf("zone %d: %w", zoneID, err))
		}
		zones = append(zones, zr)
		if code, err := zr.plan(ctx, svcCtx); code != constants.ErrCodeOK {
			return abort(code, fmt.Errorf("zone %d: %w", zoneID, err))
		}
		for pid, at := range zr.snapAt {
			if cur, ok := merged[pid]; !ok || at < cur {
				merged[pid] = at
			}
		}
	}
	plan, err := newGuildPlan(merged, svcCtx.Config.GuildClockSkewMarginMs)
	if err != nil {
		v := guildUnavailable(g, metrics.RollbackGuildResultUnavailable, err)
		return abort(v.code, v.err)
	}

	// ── 帮会闸:全部 zone 合并检查一次 ────
	verdict := runGuildGate(ctx, svcCtx, checker, g, plan)
	resp.GuildGateFields = guildGateFieldsOf(verdict.check.Divergences, len(verdict.check.UnprovablePlayerIDs))
	if verdict.code != constants.ErrCodeOK {
		return abort(verdict.code, verdict.err)
	}

	// ── 执行:全部计划 + 帮会闸都过了才写第一个 zone ────
	var rollbackErr error
	resultCode := constants.ErrCodeOK
	executed := 0
	for _, zr := range zones {
		code, zoneErr := zr.execute(ctx, svcCtx)
		executed++
		zr.resp.ErrorCode = code
		resp.PlayersAffected += zr.resp.PlayersAffected
		resp.PlayersFailed += zr.resp.PlayersFailed
		if zoneErr != nil {
			resultCode = code
			if resultCode == constants.ErrCodeOK {
				resultCode = constants.ErrCodeRollbackFailed
			}
			rollbackErr = fmt.Errorf("zone %d failed during server rollback: %w", zr.req.ZoneID, zoneErr)
			logx.Errorf("[Rollback] %v", rollbackErr)
			// 任一 zone 的存储前置失败都不能被“继续下一个”吞掉。
			break
		}
		resp.ZonesProcessed++
	}

	// ── 写后复查:一次等待,合并复查(仍持 server 级栅栏)────
	postCode := constants.ErrCodeOK
	var fresh []guildcheck.GuildDivergence
	if slices.ContainsFunc(zones, func(zr *zoneRollback) bool { return zr.wrote }) {
		postCode, fresh = recheckGuildAfterWrite(ctx, svcCtx, checker, g, plan, verdict.check)
	}
	// 把写后问题归到具体 zone:复查没做成时所有写过的 zone 都算;有新行时只算清单里含该玩家的 zone。
	flagged := func(zr *zoneRollback) bool {
		if postCode == constants.ErrCodeOK || !zr.wrote {
			return false
		}
		if fresh == nil {
			return true
		}
		return slices.ContainsFunc(fresh, func(d guildcheck.GuildDivergence) bool {
			_, ok := zr.snapAt[d.PlayerID]
			return ok
		})
	}

	// ── zone 级 RESULT 审计 ────
	var auditErrs error
	for i, zr := range zones {
		code := zr.resp.ErrorCode
		if i >= executed {
			zr.note = "not executed: server rollback stopped at an earlier zone"
			code = resultCode
		}
		if flagged(zr) {
			code = postCode
		}
		if auditErr := zr.writeResult(ctx, svcCtx, code); auditErr != nil {
			logx.Errorf("[Rollback] %v", auditErr)
			auditErrs = errors.Join(auditErrs, auditErr)
		}
	}

	// 响应码的优先级与单人 / 单 zone 一致:审计写不进 > 写后分歧 > 执行失败。
	switch {
	case auditErrs != nil:
		resultCode = constants.ErrCodeSnapshotDBError
		rollbackErr = errors.Join(rollbackErr, auditErrs)
	case postCode != constants.ErrCodeOK:
		// 写后分歧比 zone 执行失败更紧急:数据已经写了一部分。原错误已在上面记过日志,
		// 这里置空 err,让分歧计数与样本带得出去。
		if rollbackErr != nil {
			logx.Errorf("[Rollback] FULL SERVER: execution stopped with %v, and the post-write guild recheck flagged it", rollbackErr)
		}
		resultCode, rollbackErr = postCode, nil
		resp.GuildGateFields = guildGateFieldsOf(fresh, 0)
	}
	resp.ErrorCode = resultCode
	return resp, rollbackErr
}
