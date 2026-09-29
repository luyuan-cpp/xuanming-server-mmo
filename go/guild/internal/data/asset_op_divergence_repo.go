package data

// 回档前检查用的只读查询:"这些玩家自某时刻以来,有没有已经终结为已应用的资产操作"
// (docs/design/guild-phase2/07-rollback-fail-closed.md §7.4.1,B5d-2a)。
// 调用方是 guild 的内部 RPC GuildInternal.ListAppliedAssetOpsSince(internal/server/guild_internal_server.go),
// 再往上是 data_service 的回档闸(B5d-2b)。
//
// 死锁纪律(92-handoff §12.2 / §12.4):本文件只有**一条非锁定普通读**,不开事务、不带 FOR UPDATE / FOR SHARE,
// 所以不进任何取锁序列,不与 asset_store.go / economy_repo.go 的写者成环(RC 下普通读只读最新已提交版本,不加行锁)。
// asset_op_divergence_repo_test.go 的 G6 静态守卫同时钉住"语句不带锁定子句"与"本文件不开事务"。
//
// 查询正确性建立在 asset_store.go 文件头第 1 条上:Finalize 与 ResolveManually 都同写 next_attempt_ms = now,
// 所以终态行的 next_attempt_ms 就是终结时刻。那条一旦回退,这里按 next_attempt_ms 过滤就会漏行(fail-open);
// 测试 G5 / G5b 经真实 ResolveManually / Finalize 钉住它。
//
// 游标与排序(07 §7.4.1):谓词可走 idx_guild_asset_op_0 (status, next_attempt_ms) 或 idx_guild_asset_op_2
// (player_id 前缀)的范围扫描,排序按主键 op_id、接受一次 filesort。单个 after_op_id 表达不了索引序游标;
// op_id 唯一不可变,按它翻页稳定。**不写索引提示**:验收只认 EXPLAIN 的 key ∈ {idx_0, idx_2}(07 §7.10.3 步 7),
// 选了 PRIMARY 才按 07 §7.4.1 的退路加 USE INDEX,没有 EXPLAIN 证据之前不加(AGENTS §11.3)。
//
// 状态值一律绑定 pb.GuildAssetOpStatus 的生成常量,不写数字字面量(guild_db.proto 文件头约定,G6 守卫)。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "proto/guild"
)

// 一次查询的规模上限(07 §7.4.2)。server 层按它们做入参校验并回 InvalidArgument;
// 本文件再兜一次,越界直接报错不碰库 —— IN 占位符与 LIMIT 的规模是这条查询有界的前提。
const (
	// MaxAppliedOpsPlayerIDs:player_ids 的 IN 占位符上限,沿用 90 X-14 统一的 100,不另立。
	MaxAppliedOpsPlayerIDs = 100
	// MaxAppliedOpsPageLimit:单页行数上限;请求 limit=0 时也取它。
	MaxAppliedOpsPageLimit = 500
)

// AppliedOpsQuery 是 ListAppliedAssetOpsSince 的查询条件(server 层已校验、已把 limit 的 0 换成默认值)。
type AppliedOpsQuery struct {
	// ZoneID:0 = 不按 zone 收窄;非 0 时 LEFT JOIN guild,保留本 zone 与已解散帮会(guild 行已删)的行(07 §7.4.3)。
	ZoneID uint32
	// PlayerIDs:1..MaxAppliedOpsPlayerIDs 个。去零、去重由 server 层保证(重复只会让 IN 列表冗余,不改结果)。
	PlayerIDs []uint64
	// SinceMs:只返回 next_attempt_ms 严格大于它的终态行。
	SinceMs uint64
	// AfterOpID:游标,只返回 op_id 严格大于它的行;首页 0。
	AfterOpID uint64
	// Limit:本页至多返回的行数,1..MaxAppliedOpsPageLimit。
	Limit int
}

// 语句分段拼接:zone 过滤是可选的两段,player_id 的 IN 占位符个数随请求变。
// 列序即 appliedOpsColumnCount 个 Scan 目标的顺序(scanAppliedOp)。
const (
	sqlAppliedOpsSelect = "SELECT o.`op_id`, o.`player_id`, o.`guild_id`, o.`stream`, o.`kind`, o.`status`," +
		" o.`funds_delta`, o.`contribution_delta`, o.`updated_ms` FROM " + guildAssetOpTable + " o"
	// 仅 zone_id != 0:LEFT JOIN 而不是 INNER JOIN —— 帮会解散会删 guild 行,INNER JOIN 会丢掉"入账后解散"的行(07 §7.4.3 第 2 条)。
	sqlAppliedOpsZoneJoin = " LEFT JOIN guild g ON g.guild_id = o.`guild_id`"
	// 两个状态占位符依次绑 APPLIED、APPLIED_PARTIAL(appliedOpsArgs)。
	sqlAppliedOpsWhereHead = " WHERE o.`status` IN (?, ?) AND o.`next_attempt_ms` > ? AND o.`player_id` IN ("
	sqlAppliedOpsWhereTail = ") AND o.`op_id` > ?"
	// 仅 zone_id != 0:g.guild_id IS NULL = 帮会已解散,照样保留。
	sqlAppliedOpsZoneFilter = " AND (g.zone_id = ? OR g.guild_id IS NULL)"
	// 多取一行(limit+1)只用来判断"还有没有下一页",不返回给调用方。
	sqlAppliedOpsOrderLimit = " ORDER BY o.`op_id` ASC LIMIT ?"
)

// appliedOpsSQL 按玩家数与是否按 zone 收窄拼出完整语句。只拼结构,不拼任何值(值全部走占位符)。
func appliedOpsSQL(playerCount int, byZone bool) string {
	var b strings.Builder
	b.WriteString(sqlAppliedOpsSelect)
	if byZone {
		b.WriteString(sqlAppliedOpsZoneJoin)
	}
	b.WriteString(sqlAppliedOpsWhereHead)
	b.WriteString(placeholders(playerCount))
	b.WriteString(sqlAppliedOpsWhereTail)
	if byZone {
		b.WriteString(sqlAppliedOpsZoneFilter)
	}
	b.WriteString(sqlAppliedOpsOrderLimit)
	return b.String()
}

// appliedOpsArgs 与 appliedOpsSQL 的占位符一一对应。
func appliedOpsArgs(q AppliedOpsQuery) []any {
	args := make([]any, 0, len(q.PlayerIDs)+6)
	args = append(args,
		uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED),
		uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL),
		q.SinceMs)
	for _, id := range q.PlayerIDs {
		args = append(args, id)
	}
	args = append(args, q.AfterOpID)
	if q.ZoneID != 0 {
		args = append(args, q.ZoneID)
	}
	return append(args, q.Limit+1)
}

// ListAppliedAssetOpsSince 返回 PlayerIDs 名下、终结时刻(next_attempt_ms)严格晚于 SinceMs、状态为
// APPLIED / APPLIED_PARTIAL、op_id 严格大于 AfterOpID 的行,按 op_id 升序至多 Limit 行;
// 第二个返回值是下一页游标:还有下一页时 = 本页最后一行的 op_id,否则 0(已查尽)。
//
// 契约:
//   - 非锁定读、不开事务(见文件头);不保证跨页快照一致 —— 翻页期间才终结、op_id 小于游标的行本轮看不到,
//     由 data_service 的写后复查兜住(07 §7.4.1 末条、§7.7)。
//   - 超时只来自调用方 ctx(RPC 的整请求预算,guild.go requestBudgetInterceptor);本方法不另设子预算:
//     它不是 assetop.Store 的后台方法,调用方在等答复,超时即整次调用失败,data_service 按"问不到"拒绝回档(07 R2)。
//   - 入参越界(空 / 超上限的玩家列表、limit 越界、since 为 0)在碰库之前报错,属编程错误,不是业务拒绝。
func (s *GuildAssetStore) ListAppliedAssetOpsSince(ctx context.Context, q AppliedOpsQuery) ([]*pb.GuildAssetOpBrief, uint64, error) {
	if err := validateAppliedOpsQuery(q); err != nil {
		return nil, 0, fmt.Errorf("list applied %s: %w", guildAssetOpTable, err)
	}

	rows, err := s.db.QueryContext(ctx, appliedOpsSQL(len(q.PlayerIDs), q.ZoneID != 0), appliedOpsArgs(q)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list applied %s: %w", guildAssetOpTable, err)
	}
	defer rows.Close()

	var ops []*pb.GuildAssetOpBrief
	for rows.Next() {
		op, err := scanAppliedOp(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan applied %s: %w", guildAssetOpTable, err)
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate applied %s: %w", guildAssetOpTable, err)
	}

	// 取回 limit+1 行 = 还有下一页:丢掉多取的那一行,游标 = 第 limit 行的 op_id。
	if len(ops) > q.Limit {
		ops = ops[:q.Limit]
		return ops, ops[q.Limit-1].GetOpId(), nil
	}
	return ops, 0, nil
}

func validateAppliedOpsQuery(q AppliedOpsQuery) error {
	switch {
	case len(q.PlayerIDs) == 0:
		return errors.New("player_ids is empty")
	case len(q.PlayerIDs) > MaxAppliedOpsPlayerIDs:
		return fmt.Errorf("%d player_ids, limit %d", len(q.PlayerIDs), MaxAppliedOpsPlayerIDs)
	case q.Limit < 1 || q.Limit > MaxAppliedOpsPageLimit:
		return fmt.Errorf("limit %d out of [1, %d]", q.Limit, MaxAppliedOpsPageLimit)
	case q.SinceMs == 0:
		return errors.New("since_ms is 0")
	default:
		return nil
	}
}

// scanAppliedOp 按 sqlAppliedOpsSelect 的列序扫一行。stream / kind / status 在库里是整数列,直接扫成 uint32,
// 与 GuildAssetOpBrief 的字段类型一致(07 §7.3.1:不 import guild_db.proto 的枚举)。
func scanAppliedOp(row rowScanner) (*pb.GuildAssetOpBrief, error) {
	op := &pb.GuildAssetOpBrief{}
	if err := row.Scan(&op.OpId, &op.PlayerId, &op.GuildId, &op.Stream, &op.Kind, &op.Status,
		&op.FundsDelta, &op.ContributionDelta, &op.UpdatedMs); err != nil {
		return nil, err
	}
	return op, nil
}
