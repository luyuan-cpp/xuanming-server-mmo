package server

// GuildInternal:帮会的服务间内部 RPC(proto/guild/guild_internal.proto;设计 docs/design/guild-phase2/07-rollback-fail-closed.md
// §7.3.1、§7.4)。目前只有一个方法 ListAppliedAssetOpsSince,调用方是 data_service 的回档闸(B5d-2b):回档写玩家数据之前
// 先问"这些玩家自快照时刻以来,有没有已经终结为已应用的资产操作",有就拒绝回档。
//
// 与 guild_server.go 的"薄包装、零分支"不同,这里自带入参校验、保留期判定与 gRPC status 映射(07 §7.4.2 明确放在 server 层,
// 先于任何 SQL):它没有业务规则要交给 logic,只有"这次查询能不能证明"这一个判断,而判断的依据(TerminalRetentionDays)
// 是本服务的配置。拒绝一律走 gRPC status、不回 TipInfoMessage(内部 RPC 不占 tip 号段,07 §7.3.1 末条)。
//
// 准入(07 §7.4.4,用户拍板 Q1 = 方案 A):本服务**不加凭据**。session 拦截器是"白名单外一律拒",GuildInternal 的方法
// 不登记进 session.ClientMethods,所以带会话 metadata 的客户端来源调用一律 PermissionDenied;不带会话的按内部调用放行,
// 网络层隔离靠待补的 NetworkPolicy。拦截器链(guild.go buildUnaryInterceptors)对本 service 同样生效,含 killswitch 与整请求预算。
//
// 失败方向:这里的任何非 OK 答复,data_service 都按"问不到"拒绝回档(07 R2),所以本文件的每条异常分支都是 fail-closed;
// 唯一例外是保留期拒绝 FailedPrecondition —— data_service 解析 message 里的 cutoff_ms 钳位重查(07 §7.5.3-2b),
// 因此 message 格式是跨服务契约,见 RetentionRejectedMessagePrefix。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/data"
	pb "proto/guild"
)

// RetentionSafetyMs:保留期判定的安全余量(07 §7.4.2),常量、不进配置。
// 清理各副本有 [0, CleanupIntervalMinutes) 的随机起始偏移,那只会让终态行多活一会儿(方向安全);真正不安全的是副本间墙钟差
// 与翻页期间流逝的时间,1 小时远大于两者。
const RetentionSafetyMs uint64 = 3_600_000

// RetentionRejectedMessagePrefix 是保留期拒绝(FailedPrecondition)的 message 前缀,后接十进制 cutoff_ms,
// 即本次可接受的最小 since_ms。**跨服务契约**:data_service 的回档闸解析它钳位重查,解析失败按 CheckFailed 拒绝
// (07 §7.4.2,方向仍是拒绝)。改格式必须两边同改;guild_internal_server_test.go 的 G8 与 data_service 的 D15
// 各钉一次同一条样例字符串。
const RetentionRejectedMessagePrefix = "since_ms older than terminal retention; cutoff_ms="

// RetentionRejectedMessage 生成保留期拒绝的完整 message,例:"since_ms older than terminal retention; cutoff_ms=1700000000000"。
func RetentionRejectedMessage(cutoffMs uint64) string {
	return RetentionRejectedMessagePrefix + strconv.FormatUint(cutoffMs, 10)
}

// retentionCutoffMs = now − TerminalRetention + RetentionSafetyMs(07 §7.4.2 表第 4 行右端);since_ms 小于它即不可证明。
// now 早于"保留期 − 余量"(只在单测的小时间戳上发生)时取 0:没有任何 since_ms 早于它。
func retentionCutoffMs(nowMs uint64, terminalRetention time.Duration) uint64 {
	retentionMs := uint64(terminalRetention / time.Millisecond)
	if nowMs+RetentionSafetyMs <= retentionMs {
		return 0
	}
	return nowMs + RetentionSafetyMs - retentionMs
}

// provableCutoffMs 是本次可接受的最小 since_ms:按当前配置算出的下界,与"清理实际删到过哪里"两者取较大值。
//
// 只看配置不够:保留期调大之后(例如 30 天 → 60 天),30 天前的终态行早被旧配置清掉了,配置下界却退到 60 天前 ——
// 那 30 天既查不到行、也不报不可证明,回档照常放行 = 复制资产(fail-open)。清理水位(data 包
// asset_op_cleanup_watermark.go)记下清理用过的最大截止时刻,next_attempt_ms 小于它的终态行可能已不在库里。
// 水位同样加 RetentionSafetyMs:清理是"先推水位再删",别的副本可能正拿着稍新的截止在删,余量盖住这段(清理间隔远小于 1 小时)。
// 水位为 0 = 从未清理过,只按配置算。
func provableCutoffMs(nowMs uint64, terminalRetention time.Duration, cleanupWatermarkMs uint64) uint64 {
	cutoff := retentionCutoffMs(nowMs, terminalRetention)
	if cleanupWatermarkMs == 0 {
		return cutoff
	}
	return max(cutoff, cleanupWatermarkMs+RetentionSafetyMs)
}

// AppliedAssetOpLister 是本服务读终态资产指令的接缝;生产实现是 *data.GuildAssetStore(asset_op_divergence_repo.go、
// asset_op_cleanup_watermark.go),测试注入假实现。
//   - ListAppliedAssetOpsSince:契约见 data.GuildAssetStore.ListAppliedAssetOpsSince。
//   - TerminalCleanupWatermarkMs:清理水位;0 = 从未清理过。**读不准必须返回 error**(不能回 0),本服务据此回 Unavailable。
type AppliedAssetOpLister interface {
	ListAppliedAssetOpsSince(ctx context.Context, q data.AppliedOpsQuery) ([]*pb.GuildAssetOpBrief, uint64, error)
	TerminalCleanupWatermarkMs(ctx context.Context) (uint64, error)
}

// ── 指标(07 §7.4.5;guild 已配 Prometheus.Host,用 go-zero core/metric;label 只有 result,不带任何 id)──

const (
	listAppliedResultOKEmpty     = "ok_empty"
	listAppliedResultOKRows      = "ok_rows"
	listAppliedResultInvalid     = "invalid"
	listAppliedResultRetention   = "retention"
	listAppliedResultUnavailable = "unavailable"
	listAppliedResultError       = "error"
)

// listAppliedResults 是 result label 的全集,PrimeGuildInternalMetrics 逐个预置。新增取值必须同时加进这里。
var listAppliedResults = []string{
	listAppliedResultOKEmpty, listAppliedResultOKRows, listAppliedResultInvalid,
	listAppliedResultRetention, listAppliedResultUnavailable, listAppliedResultError,
}

var (
	guildInternalListAppliedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "internal",
		Name:      "list_applied_total",
		Help:      "GuildInternal.ListAppliedAssetOpsSince 的调用结果(回档前检查,07 §7.4.5)。unavailable / error 上升 = 回档闸问不到、所有回档被拒;retention 是快照早于终态流水保留期。",
		Labels:    []string{"result"},
	})

	guildInternalListAppliedRows = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "guild",
		Subsystem: "internal",
		Name:      "list_applied_rows",
		Help:      "GuildInternal.ListAppliedAssetOpsSince 成功调用每次返回的行数(单页上限 500)。",
		Buckets:   []float64{0, 1, 5, 20, 100, 500},
	})
)

// PrimeGuildInternalMetrics 把 result 的六个取值预置为 0,让"出现即告警"的 increase(...) 不漏第一次。
//
// **必须在 zrpc.MustNewServer 之后调**(guild.go):go-zero core/metric 的每次写入都过 prometheus.Enabled(),
// 开关由 MustNewServer → SetUp → StartAgent 打开,放进 init() / NewServiceContext 会被直接丢弃(92-handoff §8.1)。
// Prometheus.Host 留空的环境里本函数是空操作,与其它指标同口径。直方图不预置:它不用于"出现即告警"。
func PrimeGuildInternalMetrics() {
	for _, result := range listAppliedResults {
		guildInternalListAppliedTotal.Add(0, result)
	}
}

func observeListApplied(result string, rows int) {
	guildInternalListAppliedTotal.Inc(result)
	if result == listAppliedResultOKEmpty || result == listAppliedResultOKRows {
		guildInternalListAppliedRows.Observe(int64(rows))
	}
}

// ── 服务 ──────────────────────────────────────────────────────

// GuildInternalServer 实现 pb.GuildInternalServer。构造后只读,可并发调用。
type GuildInternalServer struct {
	pb.UnimplementedGuildInternalServer

	ops               AppliedAssetOpLister
	terminalRetention time.Duration
	now               func() time.Time
	// observe 记指标;生产是 observeListApplied,单测换成记录器(go-zero 指标在单测进程里被全局开关丢弃,读不出来)。
	observe func(result string, rows int)
}

// NewGuildInternalServer。
//   - ops:终态资产指令的读接缝;nil = 资产 Store 未装配,调用一律 Unavailable(fail-closed)。
//     **传 nil 接口,不要把 nil 指针装进来**(那样接口不为 nil,会在查询时 panic)。
//   - terminalRetention:终态行保留期,取 AssetOp.TerminalRetentionDays(svc.CleanupConfFrom);≤ 0 视同未配置,一律 Unavailable。
//     清理关着(CleanupEnabled=false)时行实际上不删,按配置值判定只会多拒,方向安全。
//   - now:墙钟,单测注入;nil 取 time.Now。
func NewGuildInternalServer(ops AppliedAssetOpLister, terminalRetention time.Duration, now func() time.Time) *GuildInternalServer {
	if now == nil {
		now = time.Now
	}
	return &GuildInternalServer{ops: ops, terminalRetention: terminalRetention, now: now, observe: observeListApplied}
}

// ListAppliedAssetOpsSince:判定顺序 = 入参(InvalidArgument)→ 装配(Unavailable)→ 清理水位(读不到 → Unavailable)
// → 保留期(FailedPrecondition)→ 查询。全部判定先于任何 SQL(07 §7.4.2);水位是一次 Redis GET。
func (s *GuildInternalServer) ListAppliedAssetOpsSince(ctx context.Context, req *pb.ListAppliedAssetOpsSinceRequest) (*pb.ListAppliedAssetOpsSinceResponse, error) {
	limit, err := validateListAppliedRequest(req)
	if err != nil {
		s.observe(listAppliedResultInvalid, 0)
		// 调用方(data_service)的编程错误,不是运行期常态:ERROR 级,便于第一时间发现契约漂移。
		logx.WithContext(ctx).Errorf("[GuildInternal] ListAppliedAssetOpsSince 入参非法: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.ops == nil || s.terminalRetention <= 0 {
		s.observe(listAppliedResultUnavailable, 0)
		logx.WithContext(ctx).Errorf("[GuildInternal] ListAppliedAssetOpsSince 不可用:资产 Store 已装配=%t 终态保留期=%v",
			s.ops != nil, s.terminalRetention)
		return nil, status.Error(codes.Unavailable, "guild asset op store is not available")
	}

	// 水位先于保留期判定读:拒绝里回带的 cutoff_ms 必须已经把水位算进去,否则 data_service 钳位重查会被水位再拒一次。
	watermark, err := s.ops.TerminalCleanupWatermarkMs(ctx)
	if err != nil {
		s.observe(listAppliedResultUnavailable, 0)
		logx.WithContext(ctx).Errorf("[GuildInternal] ListAppliedAssetOpsSince 不可用:读不到终态指令清理水位,无法判断哪段流水还能证明: %v", err)
		return nil, status.Error(codes.Unavailable, "guild asset op cleanup watermark is not available")
	}
	cutoff := provableCutoffMs(uint64(s.now().UnixMilli()), s.terminalRetention, watermark)
	if req.GetSinceMs() < cutoff {
		// 预期内的答复:快照早于终态流水保留期,这段已无法证明。data_service 据 cutoff_ms 钳位重查,不打 ERROR。
		s.observe(listAppliedResultRetention, 0)
		logx.WithContext(ctx).Infof("[GuildInternal] ListAppliedAssetOpsSince since_ms=%d 早于可证明下界 cutoff_ms=%d(清理水位 %d)",
			req.GetSinceMs(), cutoff, watermark)
		return nil, status.Error(codes.FailedPrecondition, RetentionRejectedMessage(cutoff))
	}

	ops, next, err := s.ops.ListAppliedAssetOpsSince(ctx, data.AppliedOpsQuery{
		ZoneID:    req.GetZoneId(),
		PlayerIDs: req.GetPlayerIds(),
		SinceMs:   req.GetSinceMs(),
		AfterOpID: req.GetAfterOpId(),
		Limit:     limit,
	})
	if err != nil {
		s.observe(listAppliedResultError, 0)
		// 详情(含库错误)只进日志,不回给调用方;player_id 不进日志,只记规模。
		logx.WithContext(ctx).Errorf("[GuildInternal] ListAppliedAssetOpsSince 查询失败 players=%d zone_id=%d: %v",
			len(req.GetPlayerIds()), req.GetZoneId(), err)
		return nil, storeErrorStatus(err)
	}

	result := listAppliedResultOKEmpty
	if len(ops) > 0 {
		result = listAppliedResultOKRows
	}
	s.observe(result, len(ops))
	return &pb.ListAppliedAssetOpsSinceResponse{Ops: ops, NextAfterOpId: next}, nil
}

// validateListAppliedRequest 实现 07 §7.4.2 表的前三行;返回生效的 limit(0 → 默认 500)。
// player_ids 必填且有上限:内部 RPC 无凭据,不给"按 zone 扫全表"的口子(07 §7.4.2 第一条说明)。
func validateListAppliedRequest(req *pb.ListAppliedAssetOpsSinceRequest) (int, error) {
	ids := req.GetPlayerIds()
	if len(ids) == 0 {
		return 0, errors.New("player_ids is required")
	}
	if len(ids) > data.MaxAppliedOpsPlayerIDs {
		return 0, fmt.Errorf("player_ids has %d entries, limit %d", len(ids), data.MaxAppliedOpsPlayerIDs)
	}
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return 0, errors.New("player_ids must not contain 0")
		}
		if _, dup := seen[id]; dup {
			return 0, errors.New("player_ids must not contain duplicates")
		}
		seen[id] = struct{}{}
	}
	if req.GetSinceMs() == 0 {
		return 0, errors.New("since_ms is required")
	}
	switch limit := req.GetLimit(); {
	case limit == 0:
		return data.MaxAppliedOpsPageLimit, nil
	case limit > data.MaxAppliedOpsPageLimit:
		return 0, fmt.Errorf("limit %d exceeds %d", limit, data.MaxAppliedOpsPageLimit)
	default:
		return int(limit), nil
	}
}

// storeErrorStatus 把查询错误映射成 gRPC status。超时 / 取消保留原语义,其余一律 Internal;库错误原文不外发。
func storeErrorStatus(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "list applied asset ops timed out")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "list applied asset ops canceled")
	default:
		return status.Error(codes.Internal, "list applied asset ops failed")
	}
}
