package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"shared/placement"

	"github.com/zeromicro/go-zero/core/logx"
)

// 对账 leader(D1 第 1b-2 批)在全局库一侧的原语:租约(选主)、每拍的水位推进与库回档检测、leader 冻结映射写者、
// 按片增量扫描与库时钟(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §4、§13.1;
// 底稿 spec_D1_batch1b_scout.md 的「route_reconcile.go(新增,1b-2)」)。Redis 一侧(实例身份、就绪标记、水位键、推送)
// 在 routereconcile 包;本文件不碰 Redis,事务里也不做任何 Redis 读写。
//
// 写入方:route_reconcile(R23 续约 / 抢租约、R25 释放、R27 推进)与 route_meta(R28 leader 冻结)。这四条写语句与
// route_store.go 的写语句一起受 TestRouteMetaAndReconcileWritesOnlyInStoreFiles(AST 守卫)约束;player_route 在本文件只读(R29)。
//
// 租约与 fencing(谁能写):
//   - 判据一律在库时钟上判:leader = 我 且 lease_until_ms >= 库时钟 才算持有;leader 为 NULL、lease_until_ms 为 NULL
//     或 lease_until_ms < 库时钟才可被接管 —— 两个条件互为反面,同一毫秒里不会有两个持有者。
//   - route_reconcile 上的每条写都把判据写进 WHERE:R23 的接管条件、R25 的 leader = ?、R27 的
//     leader = ? AND lease_until_ms >= 库时钟。旧 leader(库里已过期、本地还没察觉)迟到的写受影响 0 行,覆盖不了新 leader 的水位。
//   - R28 写的是 route_meta,WHERE 里放不下租约谓词:它只在已持有 R26(route_reconcile(id=1) FOR UPDATE)行锁、
//     且锁内判定为持有租约的事务里执行。行锁持到提交,期间任何续约 / 接管(R23)/ 解冻(R20)都排在后面,所以
//     「判定持有」到「冻结提交」之间租约不可能易主。冻结是 fail-closed 方向:最坏是一次多余的冻结(要人工 unfreeze),不会写错库。
//
// 锁序(与 route_store.go 文件头不变量 2 合起来看):
//   - 每拍 AdvanceReconcile 与 FreezeByLeader:route_reconcile(R26 FOR UPDATE)→ route_meta(R18 普通读;冻结时 R28 取 X 锁);
//   - 第 1b-1 批解冻 UnfreezeRouteWriters:route_reconcile(R20)→ route_meta(R18 普通读 → R21 → R22),同向;
//   - 反向修库 route_meta(R18x)→ player_route;建池探针守卫 route_meta → player_route;route_tool freeze(R19)只锁 route_meta、
//     单条自动提交;续约 / 释放(R23 / R25)只锁 route_reconcile、单条自动提交;
//   - W1 与导入对 route_meta 只普通读,从不碰 route_reconcile。
//     没有任何一方持着 route_meta 的锁再去等 route_reconcile,所以不成环(真库用例 TestRouteReconcile_TickAndUnfreezeNoDeadlock)。
//     反向修库持 R18x 时 writable 必为 0,每拍在 R18 读到冻结就不发 R28,不会去排那把锁。
//
// 库时钟:R24 / R26 / R30 读 dbNowMsSQL。MySQL 的 NOW() 取语句开始时刻,R26 若排队等锁,读到的 now 会比提交时刻早
// (至多一个锁等待 1s):租约判定因此最多宽松这么多,而 R27 的租约谓词在执行时用库时钟复核;水位按 R26 的 now 推进,只会偏小(保守)。
// TiDB 多个 tidb-server 之间的时钟偏差按不变量 I2(< 1s)处理【未核实】。
//
// cache_verified_ms(cvm)的语义(规格 §4.2 must-2):「updated_at_ms < cvm − IncrementalOverlapMs 的行都已推进缓存,且这段时间
// Redis 身份未变」。本文件只按调用方给的值写,不推算:何时推进、推进到多少由 leader 决定(一轮扫描从起点 S 完整跑完且身份不变,
// 才推进到 min(本拍 now, S));身份变化后它指的是新实例的进度,所以允许变小。
//
// 库回档判据(规格 §13.1 第 2 条):只用水位键(M3)的世代与水位、外加 leader 推送时复读确认的 cache_ahead(FreezeByLeader);
// **不用**就绪标记的水位(scout D4 第一条判据作废:标记水位现在记的是 cvm)。
//
// 超时:含 R28 的事务要等 TiDB 缓存表的读租约(见 RouteFreezeTimeoutMin),所以 AdvanceReconcile / FreezeByLeader 整个调用
// (含 1213 / 9007 的重来)用调用方给的 FreezeTimeout(≥ RouteFreezeTimeoutMin)封顶。**调用方的 ctx 不得比它更早到期**:
// 拿拍间隔(1s)当 ctx 的话,冻结在 TiDB S2+ 上会一直超时、永远冻不成(核实意见 should「TiDB 缓存表」)。
//
// 重试:同 route_store.go 不变量 4,只有 1213 / 9007 整事务重跑(runRouteTx,计 lockRetries),1205 与其它错误原样返回。

// 本文件的 SQL(编号接 route_store.go 的 R22;RD1–RD4 在 route_reads.go)。抽成常量是为了让契约用例 / 真库用例执行同一段文本。
const (
	// R23 续约 / 抢租约(单条自动提交)。leader 为空、已是自己、或租约已过期(lease_until_ms < 库时钟)才写。
	// 受影响 0 行有两种可能:别人持有有效租约,或自己在同一毫秒里续约、值没变 —— 由 RenewReconcileLease 再读 R24 区分。
	// 实参:leaderID、租约时长(毫秒)、leaderID。
	routeRenewLeaseSQL = "UPDATE route_reconcile SET leader = ?, lease_until_ms = " + dbNowMsSQL + " + ?" +
		" WHERE id = 1 AND (leader IS NULL OR leader = ? OR lease_until_ms IS NULL OR lease_until_ms < " + dbNowMsSQL + ")"

	// R24 一致读租约与库时钟(不加锁)。非 leader 每拍只发它,看起来过期才发 R23(副本多时不抢同一行锁)。
	routePeekLeaseSQL = "SELECT leader, lease_until_ms, " + dbNowMsSQL + " FROM route_reconcile WHERE id = 1"

	// R25 释放:只清自己的(leader = ?)。实参:leaderID。
	routeReleaseLeaseSQL = "UPDATE route_reconcile SET leader = NULL, lease_until_ms = NULL WHERE id = 1 AND leader = ?"

	// R26 每拍事务的第一句:锁对账行,同时读库时钟。
	routeLockReconcileSQL = "SELECT leader, lease_until_ms, watermark_ms, cache_verified_ms, " + dbNowMsSQL +
		" FROM route_reconcile WHERE id = 1 FOR UPDATE"

	// R27 推进水位(只增不减),cvm 只在调用方给出时按给定值写(COALESCE:实参为 NULL 时不动)。带租约谓词(库时钟)。
	// 实参:R26 读到的库时钟、cvm(nil = 不动)、leaderID。
	routeAdvanceReconcileSQL = "UPDATE route_reconcile SET watermark_ms = GREATEST(watermark_ms, ?)," +
		" cache_verified_ms = COALESCE(?, cache_verified_ms)" +
		" WHERE id = 1 AND leader = ? AND lease_until_ms >= " + dbNowMsSQL

	// 危险检测不拥有 writable 控制权限；独立 Catalog owner 使用正式紧急票据。

	// R29 按片增量扫描(提案 §11.2 G1a「后续批次的读方模板」的「1b 增量推送」行):片前缀等值 + (updated_at_ms, player_id) 键集,
	// 走 idx_route_updated (route_shard, updated_at_ms, player_id),ORDER BY 与索引序一致。
	// 写成 `a > x OR (a = x AND b > y)` 而不是行比较:MySQL 不对行比较做 range 优化。
	// 实参:片、游标 updated_at_ms、游标 updated_at_ms、游标 player_id、limit。
	routeScanUpdatedSinceSQL = "SELECT route_shard, player_id, " + routeRowColumnsSQL +
		" FROM player_route WHERE route_shard = ? AND (updated_at_ms > ? OR (updated_at_ms = ? AND player_id > ?))" +
		" ORDER BY updated_at_ms, player_id LIMIT ?"

	// R30 库时钟(epoch 毫秒)。
	routeDBNowSQL = "SELECT " + dbNowMsSQL
)

// leader 标识的形状:route_leader@{洗过的 host,至多 74 字节}:{8 个十六进制字符},最长 13 + 74 + 1 + 8 = 96 字节,
// 等于 route_reconcile.leader / player_route.updated_by 的列宽(核实意见 should:原规格按 78 字节截 host,算出 100 字节)。
const (
	routeLeaderIDPrefix = "route_leader@"

	// routeLeaderHostMaxLen 是 host 部分截断后的上限。
	routeLeaderHostMaxLen = 74

	// routeLeaderSuffixBytes 随机后缀的字节数(十六进制后 8 个字符):同一台机器上的多个进程不同名。
	routeLeaderSuffixBytes = 4

	// routeLeaderIDMaxLen 是 leader 标识的最大长度。
	routeLeaderIDMaxLen = len(routeLeaderIDPrefix) + routeLeaderHostMaxLen + 1 + 2*routeLeaderSuffixBytes

	// routeLeaseMaxMs 是 RenewReconcileLease 接受的租约时长上限。合法区间由 config 校验([3000, 60000]),
	// 这里只挡明显的误用(例如把纳秒当毫秒传进来),免得库时钟 + 租约溢出或一次续约占住几天。
	routeLeaseMaxMs uint64 = 10 * 60 * 1000
)

// 编译期断言:leader 标识最长的形态不超过列宽 routeWriterMaxLen(超过时数组长度为负,编译失败)。
var _ [routeWriterMaxLen - routeLeaderIDMaxLen]struct{}

// NewRouteLeaderID 生成对账 leader 的标识 "route_leader@{host}:{8hex}"。host 只留 [A-Za-z0-9._@:-](列是 ascii,
// 与 NewRouteWriterID 同一套清洗),洗完为空用 "unknown",超过 74 字节截断;后缀取 rnd 的 4 个字节,
// 让同一台机器上的多个进程不同名。产物满足 validRouteWriter,也满足就绪标记 leader 字段的字符集与长度。
// host 与 rnd 都由调用方传入(装配层传 os.Hostname 与 crypto/rand.Reader),本函数不碰任何环境。
func NewRouteLeaderID(host string, rnd io.Reader) (string, error) {
	if rnd == nil {
		return "", fmt.Errorf("%w: route leader id needs a random source", ErrRouteInvalidArgument)
	}
	h := sanitizeRouteWriterPart(host)
	if h == "" {
		h = "unknown"
	}
	if len(h) > routeLeaderHostMaxLen {
		h = h[:routeLeaderHostMaxLen] // 洗过之后全是单字节 ASCII,按字节截不会截断字符
	}
	var suffix [routeLeaderSuffixBytes]byte
	if _, err := io.ReadFull(rnd, suffix[:]); err != nil {
		return "", fmt.Errorf("route leader id: read random suffix: %w", err)
	}
	id := routeLeaderIDPrefix + h + ":" + hex.EncodeToString(suffix[:])
	if !validRouteWriter(id) {
		return "", fmt.Errorf("route leader id %q is not a valid writer id (internal bug)", id)
	}
	return id, nil
}

// ── 租约 ────────────────────────────────────────────────────────────────────────

// RouteLease 是对账租约的一次观测。
type RouteLease struct {
	// Held:Holder 非空,且租约在 NowMs(库时钟)时仍有效(LeaseUntilMs >= NowMs)。
	Held bool
	// Holder 是 route_reconcile.leader(NULL = "")。租约过期之后它仍可能非空(没人接管、也没人释放)。
	Holder string
	// LeaseUntilMs / NowMs 是库时钟的毫秒值,只有经 R24 读回时才非零(Peek,与续约受影响 0 行时的回落读)。
	// 续约直接成功时两者为 0:本地租约截止由调用方按「续约发出时刻的单调时钟 + LeaseMs − Safety」算(scout D6),不依赖库返回的值。
	LeaseUntilMs uint64
	NowMs        uint64
}

// HeldBy 报告 leaderID 此刻是否持有有效租约。
func (l RouteLease) HeldBy(leaderID string) bool {
	return l.Held && leaderID != "" && l.Holder == leaderID
}

// PeekReconcileLease 一致读当前租约(R24,不加锁、不写)。没有 id=1 那一行回 ErrRouteReconcileMissing。
func (s *RouteStore) PeekReconcileLease(ctx context.Context) (RouteLease, error) {
	return peekReconcileLease(ctx, s.db)
}

// peekReconcileLease 用 q(池或事务)跑 R24。
func peekReconcileLease(ctx context.Context, q routeQueryer) (RouteLease, error) {
	var leader sql.NullString
	var until sql.Null[uint64]
	var l RouteLease
	err := q.QueryRowContext(ctx, routePeekLeaseSQL).Scan(&leader, &until, &l.NowMs)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteLease{}, ErrRouteReconcileMissing
	}
	if err != nil {
		return RouteLease{}, fmt.Errorf("read route_reconcile lease: %w", err)
	}
	if l.NowMs == 0 {
		return RouteLease{}, errors.New("read route_reconcile lease: database clock read as 0")
	}
	if leader.Valid {
		l.Holder = leader.String
	}
	if until.Valid {
		l.LeaseUntilMs = until.V
	}
	l.Held = l.Holder != "" && until.Valid && l.LeaseUntilMs >= l.NowMs
	return l, nil
}

// RenewReconcileLease 续约或抢租约(R23):leader 为空、已是 leaderID、或租约已过期时,把租约改成 leaderID、截止到库时钟 + leaseMs。
// 结果用 HeldBy(leaderID) 判断是否持有:
//   - 受影响 1 行 → 持有(LeaseUntilMs / NowMs 为 0,见 RouteLease);
//   - 受影响 0 行 → 再读一次 R24(同一毫秒里续约、值没变时也是 0 行),按读到的结果返回;
//
// leaderID 必须过 validRouteWriter(用 NewRouteLeaderID 生成),leaseMs 必须在 [1, routeLeaseMaxMs]。
// 没有 id=1 那一行回 ErrRouteReconcileMissing。
func (s *RouteStore) RenewReconcileLease(ctx context.Context, leaderID string, leaseMs uint64) (RouteLease, error) {
	if !validRouteWriter(leaderID) {
		return RouteLease{}, fmt.Errorf("%w: leader id %q must be 1..%d bytes of [A-Za-z0-9._@:-] (use NewRouteLeaderID)",
			ErrRouteInvalidArgument, leaderID, routeWriterMaxLen)
	}
	if leaseMs == 0 || leaseMs > routeLeaseMaxMs {
		return RouteLease{}, fmt.Errorf("%w: lease %dms not in [1, %d]", ErrRouteInvalidArgument, leaseMs, routeLeaseMaxMs)
	}
	res, err := s.db.ExecContext(ctx, routeRenewLeaseSQL, leaderID, leaseMs, leaderID)
	if err != nil {
		return RouteLease{}, fmt.Errorf("renew route_reconcile lease for %s: %w", leaderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RouteLease{}, fmt.Errorf("renew route_reconcile lease for %s rows affected: %w", leaderID, err)
	}
	switch n {
	case 1:
		return RouteLease{Held: true, Holder: leaderID}, nil
	case 0:
		return peekReconcileLease(ctx, s.db)
	default:
		return RouteLease{}, fmt.Errorf("renew route_reconcile lease for %s: 受影响行数 %d 不在 {1=已续约, 0=别人持有或值未变} 之内", leaderID, n)
	}
}

// ReleaseReconcileLease 释放 leaderID 持有的租约(R25,只清自己的)。released=false 表示租约已不归它(已被接管、或从未持有),
// 不是错误。关停时尽力而为:失败不重试,租约到期自然失效(规格 §4.2「关停顺序」)。
func (s *RouteStore) ReleaseReconcileLease(ctx context.Context, leaderID string) (bool, error) {
	if !validRouteWriter(leaderID) {
		return false, fmt.Errorf("%w: leader id %q must be 1..%d bytes of [A-Za-z0-9._@:-]", ErrRouteInvalidArgument, leaderID, routeWriterMaxLen)
	}
	res, err := s.db.ExecContext(ctx, routeReleaseLeaseSQL, leaderID)
	if err != nil {
		return false, fmt.Errorf("release route_reconcile lease of %s: %w", leaderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("release route_reconcile lease of %s rows affected: %w", leaderID, err)
	}
	switch n {
	case 1:
		return true, nil
	case 0:
		return false, nil
	default:
		return false, fmt.Errorf("release route_reconcile lease of %s: 受影响行数 %d 不在 {1=已释放, 0=不是持有者} 之内", leaderID, n)
	}
}

// ── 每拍:推进水位与回档检测 ─────────────────────────────────────────────────────

// ReconcileTickOutcome 是一拍(AdvanceReconcile)或一次 leader 冻结(FreezeByLeader)在库里的终局。
type ReconcileTickOutcome uint8

const (
	// ReconcileTickAdvanced 持有租约、未冻结、没有回档证据:水位已推进(R27 已提交;值没变时零写入)。
	ReconcileTickAdvanced ReconcileTickOutcome = iota + 1
	// ReconcileTickFrozen 本次调用把 route_meta.writable 从 1 改成了 0(R28 已提交),FreezeReason 给出原因。水位与 cvm 不动。
	ReconcileTickFrozen
	// ReconcileTickAlreadyFrozen route_meta.writable 本来就不是 1(人工冻结、上一拍已冻结、或别人抢先冻结):零写入。
	ReconcileTickAlreadyFrozen
	// ReconcileTickLeaseLost leaderID 不持有有效租约:零写入,调用方立即降级。
	ReconcileTickLeaseLost
)

// String 仅供日志与指标 label(低基数,四个固定值)。
func (o ReconcileTickOutcome) String() string {
	switch o {
	case ReconcileTickAdvanced:
		return "advanced"
	case ReconcileTickFrozen:
		return "frozen"
	case ReconcileTickAlreadyFrozen:
		return "already_frozen"
	case ReconcileTickLeaseLost:
		return "lease_lost"
	default:
		return "unknown"
	}
}

// RouteFreezeReason 是 leader 冻结映射写者的原因。
type RouteFreezeReason uint8

// 各取值:
//   - RouteFreezeNone:没有冻结(只出现在 Outcome 不是 Frozen 的结果里);
//   - RouteFreezeWatermarkRegressed:水位键的水位 > 库水位 + 容差(库丢了已提交的写,M3);
//   - RouteFreezeGenerationMismatch:水位键的世代 ≠ route_meta.generation(库被重置或换了一份库,M3);
//   - RouteFreezeCacheAhead:推送时复读确认缓存 rev 大于库 rev(FreezeByLeader,scout D3)。
const (
	RouteFreezeNone RouteFreezeReason = iota
	RouteFreezeWatermarkRegressed
	RouteFreezeGenerationMismatch
	RouteFreezeCacheAhead
	routeFreezeReasonCount
)

var routeFreezeReasonNames = [routeFreezeReasonCount]string{"none", "watermark_regressed", "generation_mismatch", "cache_ahead"}

// String 仅供日志与指标 label(低基数)。
func (r RouteFreezeReason) String() string {
	if r >= routeFreezeReasonCount {
		return "unknown"
	}
	return routeFreezeReasonNames[r]
}

// ReconcileTickRequest 是一拍的入参。
type ReconcileTickRequest struct {
	// LeaderID 是本 leader 的标识(NewRouteLeaderID)。
	LeaderID string
	// WatermarkKey 是本拍读到的 mapping:route:watermark(M3)。nil = 键不在、畸形或版本更新(调用方只传解得开的 v1 值)。
	WatermarkKey *placement.RouteWatermark
	// ToleranceMs 是水位倒退的容差(config RollbackToleranceMs,负责人 10-17 拍板缺省 0)。
	ToleranceMs uint64
	// SetCacheVerifiedMs 非 nil 时把 cache_verified_ms 按给定值写(可以变小,见文件头 cvm 的语义);nil = 不动。
	// 给定值大于本事务读到的库时钟时按库时钟写(只会更保守),结果里的 CacheVerifiedMs 是实际写入值。只在 Advanced 时写。
	SetCacheVerifiedMs *uint64
	// FreezeTimeout 是整个调用的时限(含 1213 / 9007 的重来),必须 ≥ RouteFreezeTimeoutMin(见文件头「超时」)。
	FreezeTimeout time.Duration
}

func (r ReconcileTickRequest) validate() error {
	if err := validateRouteLeaderArgs(r.LeaderID, r.FreezeTimeout); err != nil {
		return err
	}
	if r.WatermarkKey != nil {
		// 用编码器的校验口径(v、世代 1..32 个十六进制字符、水位 > 0):一个手工拼出来的零值会让「世代不符」判成回档。
		if _, err := placement.EncodeRouteWatermark(*r.WatermarkKey); err != nil {
			return fmt.Errorf("%w: watermark key: %w", ErrRouteInvalidArgument, err)
		}
	}
	return nil
}

// validateRouteLeaderArgs 是 AdvanceReconcile 与 FreezeByLeader 共有的入参校验。
func validateRouteLeaderArgs(leaderID string, freezeTimeout time.Duration) error {
	if !validRouteWriter(leaderID) {
		return fmt.Errorf("%w: leader id %q must be 1..%d bytes of [A-Za-z0-9._@:-] (use NewRouteLeaderID)",
			ErrRouteInvalidArgument, leaderID, routeWriterMaxLen)
	}
	if freezeTimeout < RouteFreezeTimeoutMin {
		return fmt.Errorf("%w: freeze timeout %v < %v (TiDB cached-table writes wait for the read lease)",
			ErrRouteInvalidArgument, freezeTimeout, RouteFreezeTimeoutMin)
	}
	return nil
}

// ReconcileTickResult 是一拍(或一次 leader 冻结)的结果。
type ReconcileTickResult struct {
	Outcome ReconcileTickOutcome
	// FreezeReason 只在 Outcome == ReconcileTickFrozen 时非 none。
	FreezeReason RouteFreezeReason
	// Generation 是本事务读到的 route_meta.generation(调用方据此写水位键与就绪标记)。
	Generation string
	// WatermarkMs / CacheVerifiedMs 是本次调用之后 route_reconcile 里的值:Advanced 时是已提交的新值
	// (水位 = max(原水位, NowMs)),其余结局是原值(没有写)。
	WatermarkMs     uint64
	CacheVerifiedMs uint64
	// NowMs 是 R26 读到的库时钟。
	NowMs uint64
}

// routeReconcileRow 是 R26 读到的对账行。lease_until_ms 为 NULL 记为 0(库时钟远大于 0,0 一定判过期)。
type routeReconcileRow struct {
	Leader          string
	LeaseUntilMs    uint64
	WatermarkMs     uint64
	CacheVerifiedMs uint64
	NowMs           uint64
}

// heldBy 在本行读到的库时钟上判 leaderID 是否持有有效租约(与 RouteLease.Held、R27 的谓词同一口径)。
func (r routeReconcileRow) heldBy(leaderID string) bool {
	return leaderID != "" && r.Leader == leaderID && r.LeaseUntilMs >= r.NowMs
}

// reconcileDecision 是 decideReconcileDB 的结论。
type reconcileDecision struct {
	Outcome      ReconcileTickOutcome
	FreezeReason RouteFreezeReason
	// WatermarkMs / CacheVerifiedMs:Advanced 时是 R27 提交后应有的值,其余结局是原值。
	WatermarkMs     uint64
	CacheVerifiedMs uint64
	// SetCacheVerified:R27 是否写 cvm(否则绑 NULL、COALESCE 保持原值)。
	SetCacheVerified bool
	// CacheVerifiedClamped:调用方给的 cvm 大于库时钟,按库时钟写了(只记日志)。
	CacheVerifiedClamped bool
}

// decideReconcileDB 是每拍的判定(纯函数,规格 §4.3 第 4 步与 §13.1 第 2 条)。判定顺序:
//  1. 租约:leaderID 在 R26 的库时钟上不持有有效租约 → LeaseLost(零写入);
//  2. route_meta.writable != 1 → AlreadyFrozen(零写入:冻结期库可能正处在回档之后,推进水位等于替它作证);
//  3. 水位键世代 ≠ 库世代 → Frozen(generation_mismatch);
//  4. 水位键水位 > 库水位 + 容差 → Frozen(watermark_regressed)(减法写法,不会溢出);
//  5. 否则 Advanced:水位 = max(库水位, 库时钟);cvm 只在给出时写 min(给定值, 库时钟)。
//
// 冻结时不写水位也不写 cvm:GREATEST 会把回档的证据洗掉。就绪标记的水位不参与判定(§13.1 第 2 条)。
func decideReconcileDB(row routeReconcileRow, meta RouteMeta, key *placement.RouteWatermark, toleranceMs uint64,
	leaderID string, setCacheVerifiedMs *uint64) reconcileDecision {
	d := reconcileDecision{WatermarkMs: row.WatermarkMs, CacheVerifiedMs: row.CacheVerifiedMs}
	switch {
	case !row.heldBy(leaderID):
		d.Outcome = ReconcileTickLeaseLost
		return d
	case meta.Frozen():
		d.Outcome = ReconcileTickAlreadyFrozen
		return d
	}
	if key != nil {
		switch {
		case key.Generation != meta.Generation:
			d.Outcome, d.FreezeReason = ReconcileTickFrozen, RouteFreezeGenerationMismatch
			return d
		case key.WatermarkMs > row.WatermarkMs && key.WatermarkMs-row.WatermarkMs > toleranceMs:
			d.Outcome, d.FreezeReason = ReconcileTickFrozen, RouteFreezeWatermarkRegressed
			return d
		}
	}
	d.Outcome = ReconcileTickAdvanced
	d.WatermarkMs = max(row.WatermarkMs, row.NowMs)
	if setCacheVerifiedMs != nil {
		d.SetCacheVerified = true
		d.CacheVerifiedMs = min(*setCacheVerifiedMs, row.NowMs)
		d.CacheVerifiedClamped = *setCacheVerifiedMs > row.NowMs
	}
	return d
}

// unchangedFrom 报告 Advanced 的结论相对 R26 读到的行是不是「什么都不变」(R27 受影响 0 行的合法解释之一)。
func (d reconcileDecision) unchangedFrom(row routeReconcileRow) bool {
	return d.WatermarkMs == row.WatermarkMs && d.CacheVerifiedMs == row.CacheVerifiedMs
}

// result 按结论与本事务读到的值装出结果。
func (d reconcileDecision) result(row routeReconcileRow, meta RouteMeta) ReconcileTickResult {
	return ReconcileTickResult{
		Outcome:         d.Outcome,
		FreezeReason:    d.FreezeReason,
		Generation:      meta.Generation,
		WatermarkMs:     d.WatermarkMs,
		CacheVerifiedMs: d.CacheVerifiedMs,
		NowMs:           row.NowMs,
	}
}

// AdvanceReconcile 跑一拍的库内部分(规格 §4.3 第 4 步)。一个事务:
//
//	R26 锁对账行、读库时钟 → R18 普通读 route_meta → decideReconcileDB
//	  LeaseLost / AlreadyFrozen → 回滚(零写入)
//	  Frozen                    → R28(writable 1 → 0)→ COMMIT;R28 受影响 0 行(别人抢先冻结)→ 回滚,AlreadyFrozen
//	  Advanced                  → R27(带租约谓词)→ COMMIT;R27 受影响 0 行 → 事务内再读 R24:
//	                              不再持有 → 回滚,LeaseLost;仍持有且结论本就是「值不变」→ 照常提交;否则 fail-closed 报错
//
// 只有 1213 / 9007 整事务重跑;整个调用受 req.FreezeTimeout 约束(见文件头「超时」)。
// 错误只表示故障(入参非法 ErrRouteInvalidArgument、缺行 ErrRouteReconcileMissing / ErrRouteMetaMissing、SQL 错误);
// 租约丢失/已冻结走 Outcome；新危险返回 RouteEmergencyError，不能把观察误报为已冻结。
// COMMIT 出错时结果不确定,按 err 返回;下一拍重跑是安全的(R27 幂等、R28 只从 1 翻到 0)。
func (s *RouteStore) AdvanceReconcile(ctx context.Context, req ReconcileTickRequest) (ReconcileTickResult, error) {
	if err := req.validate(); err != nil {
		return ReconcileTickResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, req.FreezeTimeout)
	defer cancel()
	var res ReconcileTickResult
	err := s.runRouteTx(ctx, "route_reconcile advance leader="+req.LeaderID, func() error {
		var attemptErr error
		res, attemptErr = s.advanceReconcileTx(ctx, req)
		return attemptErr
	})
	if err != nil {
		return ReconcileTickResult{}, err
	}
	return res, nil
}

// advanceReconcileTx 一次每拍事务的尝试。
func (s *RouteStore) advanceReconcileTx(ctx context.Context, req ReconcileTickRequest) (ReconcileTickResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconcileTickResult{}, fmt.Errorf("begin route_reconcile advance tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	row, meta, err := lockReconcileAndReadMeta(ctx, tx)
	if err != nil {
		return ReconcileTickResult{}, err
	}
	d := decideReconcileDB(row, meta, req.WatermarkKey, req.ToleranceMs, req.LeaderID, req.SetCacheVerifiedMs)
	res := d.result(row, meta)

	switch d.Outcome {
	case ReconcileTickLeaseLost, ReconcileTickAlreadyFrozen:
		return res, nil // 零写入,由 defer 回滚
	case ReconcileTickFrozen:
		// normal 只观察危险；控制表只能由持正式票据的独立 operator 修改。
		return res, &RouteEmergencyError{Observation: res}
	case ReconcileTickAdvanced:
		var cvmArg any
		if d.SetCacheVerified {
			cvmArg = d.CacheVerifiedMs
		}
		upd, err := tx.ExecContext(ctx, routeAdvanceReconcileSQL, row.NowMs, cvmArg, req.LeaderID)
		if err != nil {
			return ReconcileTickResult{}, fmt.Errorf("advance route_reconcile watermark to %d: %w", d.WatermarkMs, err)
		}
		n, err := upd.RowsAffected()
		if err != nil {
			return ReconcileTickResult{}, fmt.Errorf("advance route_reconcile watermark rows affected: %w", err)
		}
		switch n {
		case 1:
		case 0:
			// 0 行有两种解释:R26 之后库时钟走过了租约截止(R27 的谓词用执行时的库时钟),或值本来就不变
			// (连接不带 CLIENT_FOUND_ROWS,没改值的行不计数)。行被 R26 锁着,leader / 租约不会变,只有库时钟在走:再读一次就分得清。
			lease, err := peekReconcileLease(ctx, tx)
			if err != nil {
				return ReconcileTickResult{}, err
			}
			if !lease.HeldBy(req.LeaderID) {
				lost := reconcileDecision{Outcome: ReconcileTickLeaseLost, WatermarkMs: row.WatermarkMs, CacheVerifiedMs: row.CacheVerifiedMs}
				return lost.result(row, meta), nil
			}
			if !d.unchangedFrom(row) {
				return ReconcileTickResult{}, fmt.Errorf("advance route_reconcile: affected 0 rows while %s still holds the lease and "+
					"the row should change (watermark %d -> %d, cache_verified_ms %d -> %d); the row lock semantics are broken",
					req.LeaderID, row.WatermarkMs, d.WatermarkMs, row.CacheVerifiedMs, d.CacheVerifiedMs)
			}
		default:
			return ReconcileTickResult{}, fmt.Errorf("advance route_reconcile: 受影响行数 %d 不在 {1=已推进, 0=租约到期或值未变} 之内", n)
		}
		if d.CacheVerifiedClamped {
			logx.Infof("[RouteStore] route_reconcile advance: cache_verified_ms %d is ahead of the database clock %d, written as %d "+
				"(clock skew between database servers?)", *req.SetCacheVerifiedMs, row.NowMs, d.CacheVerifiedMs)
		}
	default:
		return ReconcileTickResult{}, fmt.Errorf("route_reconcile advance: unexpected decision %s (internal bug)", d.Outcome)
	}

	if err := tx.Commit(); err != nil {
		return ReconcileTickResult{}, fmt.Errorf("commit route_reconcile advance (outcome %s unknown; the next tick reruns it safely): %w", d.Outcome, err)
	}
	committed = true
	return res, nil
}

// FreezeByLeader 由持有租约的 leader 冻结映射写者(给 cache_ahead 用:推送时复读确认缓存比库新)。事务形状同 AdvanceReconcile:
// R26 → R18 → 不持有租约 → LeaseLost；已冻结 → AlreadyFrozen；否则返回真实危险观察，事务回滚且不改控制表。
// reason 必须是三个冻结原因之一;freezeTimeout 的约束同 ReconcileTickRequest.FreezeTimeout。
func (s *RouteStore) FreezeByLeader(ctx context.Context, leaderID string, reason RouteFreezeReason, freezeTimeout time.Duration) (ReconcileTickResult, error) {
	if err := validateRouteLeaderArgs(leaderID, freezeTimeout); err != nil {
		return ReconcileTickResult{}, err
	}
	if reason == RouteFreezeNone || reason >= routeFreezeReasonCount {
		return ReconcileTickResult{}, fmt.Errorf("%w: leader freeze reason %d", ErrRouteInvalidArgument, reason)
	}
	ctx, cancel := context.WithTimeout(ctx, freezeTimeout)
	defer cancel()
	var res ReconcileTickResult
	err := s.runRouteTx(ctx, "route_reconcile leader freeze leader="+leaderID, func() error {
		var attemptErr error
		res, attemptErr = s.freezeByLeaderTx(ctx, leaderID, reason)
		return attemptErr
	})
	if err != nil {
		return ReconcileTickResult{}, err
	}
	return res, nil
}

// freezeByLeaderTx 一次 leader 冻结事务的尝试。
func (s *RouteStore) freezeByLeaderTx(ctx context.Context, leaderID string, reason RouteFreezeReason) (ReconcileTickResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconcileTickResult{}, fmt.Errorf("begin route_reconcile leader freeze tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	row, meta, err := lockReconcileAndReadMeta(ctx, tx)
	if err != nil {
		return ReconcileTickResult{}, err
	}
	d := reconcileDecision{WatermarkMs: row.WatermarkMs, CacheVerifiedMs: row.CacheVerifiedMs}
	switch {
	case !row.heldBy(leaderID):
		d.Outcome = ReconcileTickLeaseLost
		return d.result(row, meta), nil
	case meta.Frozen():
		d.Outcome = ReconcileTickAlreadyFrozen
		return d.result(row, meta), nil
	}
	d.Outcome, d.FreezeReason = ReconcileTickFrozen, reason
	res := d.result(row, meta)
	return res, &RouteEmergencyError{Observation: res}
}

// lockReconcileAndReadMeta 是每拍 / leader 冻结事务的前两句:R26 锁对账行(含库时钟),R18 普通读 route_meta。
// 锁序 route_reconcile → route_meta(见文件头)。缺行分别回 ErrRouteReconcileMissing / ErrRouteMetaMissing。
func lockReconcileAndReadMeta(ctx context.Context, tx *sql.Tx) (routeReconcileRow, RouteMeta, error) {
	var leader sql.NullString
	var until sql.Null[uint64]
	var row routeReconcileRow
	err := tx.QueryRowContext(ctx, routeLockReconcileSQL).Scan(&leader, &until, &row.WatermarkMs, &row.CacheVerifiedMs, &row.NowMs)
	if errors.Is(err, sql.ErrNoRows) {
		return routeReconcileRow{}, RouteMeta{}, ErrRouteReconcileMissing
	}
	if err != nil {
		return routeReconcileRow{}, RouteMeta{}, fmt.Errorf("lock route_reconcile(id=%d): %w", routeMetaRowID, err)
	}
	if row.NowMs == 0 {
		return routeReconcileRow{}, RouteMeta{}, errors.New("lock route_reconcile: database clock read as 0")
	}
	if leader.Valid {
		row.Leader = leader.String
	}
	if until.Valid {
		row.LeaseUntilMs = until.V
	}
	meta, err := readRouteMeta(ctx, tx, routeReadMetaSQL)
	if err != nil {
		return routeReconcileRow{}, RouteMeta{}, err
	}
	return row, meta, nil
}

// RouteEmergencyError 是实际强读所得危险观察，不是已冻结或已排空收据。
// 装配 owner 必须关闭本 boot 接纳并保存原件；独立 operator 重新核验证据后申请紧急票据。
type RouteEmergencyError struct{ Observation ReconcileTickResult }

func (e *RouteEmergencyError) Error() string {
	return fmt.Sprintf("路由危险需独立维护授权: reason=%s generation=%s watermark_ms=%d observed_ms=%d", e.Observation.FreezeReason, e.Observation.Generation, e.Observation.WatermarkMs, e.Observation.NowMs)
}

// ── 按片增量扫描与库时钟 ─────────────────────────────────────────────────────────

// RouteUpdatedCursor 是 ScanRoutesUpdatedSince 的键集游标 = 上一页最后一行的 (updated_at_ms, player_id)。
// 每个行片一个游标(按片循环 s = 0..placement.RouteRowShards-1,提案 §11.2 G1a 模板)。
type RouteUpdatedCursor struct {
	UpdatedAtMs uint64
	PlayerID    uint64
}

// RouteUpdatedSince 返回「updated_at_ms >= sinceMs」的起点游标 {sinceMs, 0}:
// R29 展开成 updated_at_ms > sinceMs OR (updated_at_ms = sinceMs AND player_id > 0),player_id 为 0 的行不合法。
// sinceMs 由调用方算(cvm − Overlap 之类的减法要饱和到 0,规格 §4.2 must-1)。
func RouteUpdatedSince(sinceMs uint64) RouteUpdatedCursor {
	return RouteUpdatedCursor{UpdatedAtMs: sinceMs}
}

// RouteUpdatedCursorAt 返回 row 的增量游标位置。
func RouteUpdatedCursorAt(row RouteRow) RouteUpdatedCursor {
	return RouteUpdatedCursor{UpdatedAtMs: row.UpdatedAtMs, PlayerID: row.PlayerID}
}

// Less 按 (updated_at_ms, player_id) 比较,与 R29 的 ORDER BY 同序。
func (c RouteUpdatedCursor) Less(o RouteUpdatedCursor) bool {
	if c.UpdatedAtMs != o.UpdatedAtMs {
		return c.UpdatedAtMs < o.UpdatedAtMs
	}
	return c.PlayerID < o.PlayerID
}

// ScanRoutesUpdatedSince 在行片 shard 上按 (updated_at_ms, player_id) 键集翻页(R29,事务外一致读):
// 返回游标之后的至多 limit 行,按 (updated_at_ms, player_id) 升序。下一页从 RouteUpdatedCursorAt(最后一行) 接着翻;
// 一页不足 limit 行即这一片翻完。
//
// shard 必须 < placement.RouteRowShards、limit 在 [1, RouteBatchLimit],否则 ErrRouteInvalidArgument、不发语句
// (片越界的查询会静默回空页,调用方会当作扫完)。每一行都校验:库里存的片等于 player_id 的片(scanRouteRow)、
// 也等于入参片(否则 ErrRouteShardMismatch);键严格大于游标且严格递增 —— 任一不符整页作废(fail-closed:
// 错的一页交给 leader,就是一段静默漏推的行)。
func (s *RouteStore) ScanRoutesUpdatedSince(ctx context.Context, shard uint32, after RouteUpdatedCursor, limit int) ([]RouteRow, error) {
	if shard >= placement.RouteRowShards {
		return nil, fmt.Errorf("%w: incremental scan shard %d not in [0, %d)", ErrRouteInvalidArgument, shard, placement.RouteRowShards)
	}
	if limit < 1 || limit > RouteBatchLimit {
		return nil, fmt.Errorf("%w: incremental scan limit %d not in [1, %d]", ErrRouteInvalidArgument, limit, RouteBatchLimit)
	}
	rows, err := s.db.QueryContext(ctx, routeScanUpdatedSinceSQL, uint64(shard), after.UpdatedAtMs, after.UpdatedAtMs, after.PlayerID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan player_route shard %d updated after (%d, %d): %w", shard, after.UpdatedAtMs, after.PlayerID, err)
	}
	defer rows.Close()

	out := make([]RouteRow, 0, limit)
	prev := after
	for rows.Next() {
		var row RouteRow
		if err := scanRouteRow(rows, &row, true); err != nil {
			return nil, fmt.Errorf("scan player_route row of shard %d: %w", shard, err)
		}
		if got := placement.RouteRowShard(row.PlayerID); got != shard {
			return nil, fmt.Errorf("%w: incremental scan of shard %d returned player_id=%d of shard %d",
				ErrRouteShardMismatch, shard, row.PlayerID, got)
		}
		cur := RouteUpdatedCursorAt(row)
		if !prev.Less(cur) {
			return nil, fmt.Errorf("scan player_route shard %d: row (updated_at_ms, player_id)=(%d, %d) not after (%d, %d) (keyset order broken)",
				shard, cur.UpdatedAtMs, cur.PlayerID, prev.UpdatedAtMs, prev.PlayerID)
		}
		prev = cur
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate player_route shard %d incremental scan: %w", shard, err)
	}
	if len(out) > limit {
		return nil, fmt.Errorf("scan player_route shard %d returned %d rows for limit %d", shard, len(out), limit)
	}
	return out, nil
}

// DBNowMs 读一次库时钟(R30,epoch 毫秒)。leader 用它给就绪标记的 set_at_ms 与长任务的起点取值,不用本机时钟。
func (s *RouteStore) DBNowMs(ctx context.Context) (uint64, error) {
	var now uint64
	if err := s.db.QueryRowContext(ctx, routeDBNowSQL).Scan(&now); err != nil {
		return 0, fmt.Errorf("read database clock: %w", err)
	}
	if now == 0 {
		return 0, errors.New("read database clock: read as 0")
	}
	return now, nil
}
