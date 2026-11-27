package data

// 帮战入账的分流与待入账行 guild_war_credit_owed(数据覆写审计 r2 第 4 轮 r2-gw#5 / #6;设计见
// docs/planning/guild-play-v2-implementation-plan.md 末节「帮战结算的入账分流」)。
//
// 帮战结算(SettleWarBattle)一次给一局里的多名玩家入账,追杀令到期收尾(ExpireBounty)给发布者退款。任何一名玩家此刻收不了,
// 都不能让整笔事务失败 —— 那会挡住同局其余所有人的结算,消费者退避重投 20 次后整局进死信。所以按人分流:
//   - 在役、本流还有空位:照常插 GUILD_CREDIT 指令;
//   - 本流未决指令已满(assetop.ErrTooManyPending):不插指令、seq 不前进,这一笔记进本表(同一事务);
//   - 角色护栏副本冻结中(过户投票、寄售中、删角投票):冻结期间不给他插任何资产指令(护栏规则),同样记进本表,解冻后再补;
//   - 角色已删除(墓碑):这一笔放弃,也不碰他的任何行(删角收尾删过他的 seq 行;墓碑之后本库不许再有他的业务写入)。
//
// 本表由待入队物品循环(logic/trial_background.go)逐行转换,与历练的待入队物品同一套写法,只是指令号不另发:
// 行里存的就是结算事务前预发的那个号,转换时原样用作 guild_asset_op.op_id。同一笔因此至多变成一条指令。
//
// 护栏按「目标」口径判(只有角色号,没有原身份):只看冻结与删除,不看归属 —— 与历练结算的候选人、待入队物品转换同一判据
// (role_trade_guard.go withRoleTargets)。过户完成之后副本解冻,这一笔照常入给这名角色。
//
// 锁序:护栏副本(按 player_id 升序,事务里的第一把锁)→ G → M → Q → O → … → 本表(位置 24,全序末尾)。
// 本表每个写者都先持这名玩家的护栏副本行锁:结算 / 到期收尾插新行、转换点删,同一 (player, op_id) 的写者因此串行。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	"shared/assetop"
	"shared/rolefence"
)

const guildWarCreditOwedTable = "guild_war_credit_owed"

const (
	warCreditOwedColumns = "`player_id`,`op_id`,`guild_id`,`kind`,`tx_type`,`ref_id`,`period_key`,`payload`,`source_id`,`created_ms`"

	// sqlInsertWarCreditOwed:结算 / 到期收尾在全序末尾插新行。op_id 是本事务预发的号,与已提交的行不会撞键。
	sqlInsertWarCreditOwed = "INSERT INTO `" + guildWarCreditOwedTable + "` (" + warCreditOwedColumns + ") VALUES (?,?,?,?,?,?,?,?,?,?)"

	// sqlSelectWarCreditOwedPage:循环的翻页普通读,按 (created_ms, player_id, op_id) 升序、键集游标(展开式,同 sqlSelectOwedRewardsPage)。
	sqlSelectWarCreditOwedPage = "SELECT " + warCreditOwedColumns + " FROM `" + guildWarCreditOwedTable + "`" +
		" WHERE `created_ms` > ? OR (`created_ms` = ? AND (`player_id` > ? OR (`player_id` = ? AND `op_id` > ?)))" +
		" ORDER BY `created_ms`, `player_id`, `op_id` LIMIT ?"

	// sqlDeleteWarCreditOwed:完整主键等值点删,不带复核条件。影响 0 行 = 别的副本已转走。
	sqlDeleteWarCreditOwed = "DELETE FROM `" + guildWarCreditOwedTable + "` WHERE `player_id` = ? AND `op_id` = ?"

	// sqlSelectWarCreditOwedExists:转换在持着他的护栏副本与 seq 行之后普通读本行还在不在(本行的删除者都先持这两把锁)。
	sqlSelectWarCreditOwedExists = "SELECT 1 FROM `" + guildWarCreditOwedTable + "` WHERE `player_id` = ? AND `op_id` = ?"
)

// WarCreditOwed 是一笔帮战入账:结算 / 到期收尾要插的 GUILD_CREDIT 指令参数,收不了时原样记进 guild_war_credit_owed。
type WarCreditOwed struct {
	PlayerID, OpID, GuildID uint64
	Kind                    pb.GuildAssetOpKind
	TxType                  rollbackpb.TransactionType
	RefID, PeriodKey        uint32
	Payload                 []byte
	// SourceID:帮战结算记 battle_id,到期收尾记 bounty_id(只作审计)。
	SourceID, CreatedMs uint64
}

// WarCreditOwedCursor 是 ListWarCreditOwed 的键集游标:上一页最后一行的 (created_ms, player_id, op_id)。零值 = 从头读。
type WarCreditOwedCursor struct {
	CreatedMs, PlayerID, OpID uint64
}

// Cursor 返回「读过这一行之后」的游标。
func (o WarCreditOwed) Cursor() WarCreditOwedCursor {
	return WarCreditOwedCursor{CreatedMs: o.CreatedMs, PlayerID: o.PlayerID, OpID: o.OpID}
}

// warOpLease 是插指令时的租约:结算持租约(提交后同步投递,LeaseToken = battle_id),到期收尾与转换不持(立刻可被重投循环领走)。
type warOpLease struct {
	nextAttemptMs, untilMs, token uint64
}

// warCreditRecord 造一行帮战入账指令:GUILD_CREDIT、PENDING、ref_count 1;不设截止(钱属于玩家,背包满也只是等)。
func warCreditRecord(c WarCreditOwed, alloc assetop.Alloc, lease warOpLease, nowMs uint64) *pb.GuildAssetOpRecord {
	return &pb.GuildAssetOpRecord{
		OpId:          c.OpID,
		PlayerId:      c.PlayerID,
		Stream:        uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT),
		Seq:           alloc.Seq,
		GuildId:       c.GuildID,
		Kind:          c.Kind,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: lease.nextAttemptMs,
		Payload:       c.Payload,
		RefId:         c.RefID,
		RefCount:      1,
		PeriodKey:     c.PeriodKey,
		CreatedMs:     nowMs,
		UpdatedMs:     nowMs,
		LeaseUntilMs:  lease.untilMs,
		LeaseToken:    lease.token,
		TxType:        uint32(c.TxType),
		StreamEpoch:   alloc.Epoch,
	}
}

// creditFenceVerdict 是一名入账目标此刻的护栏结论(r2-gw#6)。零值 = 照常入账(没装配护栏的独立仓储夹具也按它)。
type creditFenceVerdict uint8

const (
	// creditFenceOpen:未冻结、未删除。
	creditFenceOpen creditFenceVerdict = iota
	// creditFenceFrozen:冻结中(自愈之后仍冻结),不插资产指令,记待入账行。
	creditFenceFrozen
	// creditFenceRemoved:已删除(墓碑),这一笔放弃,不写他的任何行。
	creditFenceRemoved
)

// classifyCreditFence 判锁到的一行护栏副本。healed:本事务已经让 Guard.RunTx 自愈过一次。
// stale=true 时调用方回 *rolefence.StaleError,事务回滚、读权威状态写进副本后整笔重来:副本缺行要自愈;冻结的副本可能只是
// 解冻推送丢了,先自愈一次,第二轮还冻结才算真冻结(与 rolefence.Check 同一顺序)。墓碑不带令牌、版本只增,不会再变回在役,不必自愈。
func classifyCreditFence(fence rolefence.Fence, lockErr error, healed bool) (verdict creditFenceVerdict, stale bool, err error) {
	switch {
	case errors.Is(lockErr, rolefence.ErrMissing):
		return creditFenceOpen, true, nil
	case lockErr != nil:
		return creditFenceOpen, false, lockErr
	case fence.Account == "":
		return creditFenceRemoved, false, nil
	case fence.FreezeToken != 0 && !healed:
		return creditFenceOpen, true, nil
	case fence.FreezeToken != 0:
		return creditFenceFrozen, false, nil
	}
	return creditFenceOpen, false, nil
}

// lockCreditFences 按 player_id 升序逐个锁入账目标的护栏副本(必须是事务里的第一把锁,先于 guild 行;与 lockRolePrefix 同一锁序),
// 返回每人的结论。与 withRoleTargets 的区别只有一处:一名目标收不了不让整笔失败,由调用方按人分流。
// 要自愈的角色一次收齐(同 rolefence.LockAll),回 *rolefence.StaleError 并把 *healed 置真,Guard.RunTx 自愈后整笔再跑一次。
// 没装配护栏(r.fence == nil,只有独立仓储夹具)时不锁,全部按放行(同 lockRolePrefix)。
func (r *GuildRepo) lockCreditFences(ctx context.Context, tx *sql.Tx, players []uint64, healed *bool) (map[uint64]creditFenceVerdict, error) {
	if r.fence == nil {
		return nil, nil
	}
	ordered := slices.Clone(players)
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	verdicts := make(map[uint64]creditFenceVerdict, len(ordered))
	var stale []uint64
	for _, player := range ordered {
		if player == 0 {
			continue
		}
		fence, lockErr := rolefence.Lock(ctx, tx, player)
		verdict, needHeal, err := classifyCreditFence(fence, lockErr, *healed)
		if err != nil {
			return nil, fmt.Errorf("lock role fence of credit target %d: %w", player, err)
		}
		if needHeal {
			stale = append(stale, player)
			continue
		}
		verdicts[player] = verdict
	}
	if len(stale) != 0 {
		*healed = true
		return nil, &rolefence.StaleError{Players: stale, Reject: ErrWriteConflict}
	}
	return verdicts, nil
}

// playersWithVerdict 按 player_id 升序列出结论为 want 的目标(日志与单测用)。
func playersWithVerdict(verdicts map[uint64]creditFenceVerdict, want creditFenceVerdict) []uint64 {
	var out []uint64
	for player, verdict := range verdicts {
		if verdict == want {
			out = append(out, player)
		}
	}
	slices.Sort(out)
	return out
}

// creditOrOweTx 给一名不是墓碑的入账目标插一条 GUILD_CREDIT 指令(PENDING)。两种情况不插,回要记进待入账表的那一行
// (调用方在全序末尾 insertWarCreditOwedTx):
//   - frozen(护栏副本冻结中):不分 seq —— 分了不插,他的流上就留了一个洞;seq 行照锁,它是同一事务里领赏计数行的写者守卫(C6);
//   - 本流未决指令已满(AllocateSeq 回 ErrTooManyPending):seq 不前进,seq 行锁仍持有。
//
// 墓碑那一笔调用方直接放弃,不要调进来:删角收尾删过他的 seq 行,这里会把它建回来。
// 建 seq 行的守卫是本事务已持的他的护栏副本行锁(他的全部建行者都先锁它)与成员行锁(他还在帮的话)。
func (r *WarRepo) creditOrOweTx(ctx context.Context, tx *sql.Tx, c WarCreditOwed, frozen bool, lease warOpLease, nowMs uint64) (*AssetOpRow, *WarCreditOwed, error) {
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	if err := ensureSeqRowTx(ctx, tx, c.PlayerID, stream, nowMs); err != nil {
		return nil, nil, err
	}
	c.CreatedMs = nowMs
	if frozen {
		if _, err := lockRowExists(ctx, tx, sqlLockSeqGuard, c.PlayerID, uint32(stream)); err != nil {
			return nil, nil, fmt.Errorf("lock %s guard (player=%d) for a fenced war credit: %w", guildPlayerOpSeqTable, c.PlayerID, err)
		}
		return nil, &c, nil
	}
	alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, c.PlayerID, stream, assetop.DefaultLimits, nowMs)
	if errors.Is(err, assetop.ErrTooManyPending) {
		return nil, &c, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rec := warCreditRecord(c, alloc, lease, nowMs)
	if err := insertAssetOp(ctx, tx, rec); err != nil {
		return nil, nil, err
	}
	row := assetOpRowFrom(rec)
	return &row, nil, nil
}

// insertWarCreditOwedTx 在全序末尾插待入账行(锁序位置 24)。结算的幂等闸门 / 到期收尾的状态复核已保证这是首次落定。
func insertWarCreditOwedTx(ctx context.Context, tx *sql.Tx, rows []WarCreditOwed) error {
	for _, o := range rows {
		if _, err := tx.ExecContext(ctx, sqlInsertWarCreditOwed, o.PlayerID, o.OpID, o.GuildID, uint32(o.Kind), uint32(o.TxType),
			o.RefID, o.PeriodKey, o.Payload, o.SourceID, o.CreatedMs); err != nil {
			return fmt.Errorf("insert %s (player=%d op=%d): %w", guildWarCreditOwedTable, o.PlayerID, o.OpID, err)
		}
	}
	return nil
}

// ListWarCreditOwed 按 (created_ms, player_id, op_id) 升序普通读一页待入账行,从 after 之后开始,至多 limit 行。
// 循环要翻页读完(每页用最后一行的 Cursor 作下一页的 after),不能每轮只取最老的一页:队头若是一批长期收不了的玩家
// (背包一直满、角色寄售中),后面的人会被饿死。
func (r *WarRepo) ListWarCreditOwed(ctx context.Context, after WarCreditOwedCursor, limit int) ([]WarCreditOwed, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, sqlSelectWarCreditOwedPage,
		after.CreatedMs, after.CreatedMs, after.PlayerID, after.PlayerID, after.OpID, limit)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", guildWarCreditOwedTable, err)
	}
	defer rows.Close()
	var out []WarCreditOwed
	for rows.Next() {
		var o WarCreditOwed
		var kind, txType uint32
		if err := rows.Scan(&o.PlayerID, &o.OpID, &o.GuildID, &kind, &txType, &o.RefID, &o.PeriodKey, &o.Payload, &o.SourceID, &o.CreatedMs); err != nil {
			return nil, fmt.Errorf("scan %s: %w", guildWarCreditOwedTable, err)
		}
		o.Kind, o.TxType = pb.GuildAssetOpKind(kind), rollbackpb.TransactionType(txType)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", guildWarCreditOwedTable, err)
	}
	return out, nil
}

// WarOwedConvertStatus 是 ConvertWarCreditOwed 的去向。零值无效。
type WarOwedConvertStatus uint8

const (
	// WarOwedConverted:已插指令(PENDING、不持租约、立刻可领,op_id 就是本行的 op_id)并删掉待入账行(已提交)。
	WarOwedConverted WarOwedConvertStatus = iota + 1
	// WarOwedStillFull:他本流未决指令仍满,什么都没写。
	WarOwedStillFull
	// WarOwedFrozen:他的护栏副本自愈之后仍冻结,什么都没写。
	WarOwedFrozen
	// WarOwedAbandoned:角色已删除,删掉待入账行,这一笔放弃(已提交)。
	WarOwedAbandoned
	// WarOwedGone:待入账行已不在(别的副本刚转走),什么都没写。
	WarOwedGone
)

var (
	errWarOwedWindowFull = errors.New("war credit owed: credit window still full")
	errWarOwedFrozen     = errors.New("war credit owed: role fence still frozen")
	errWarOwedRowGone    = errors.New("war credit owed: row already converted")
)

// ConvertWarCreditOwed 把一行待入账转成资产指令(r2-gw#5 / #6)。
//
// 事务 op=war_credit_owed:锁他的护栏副本(要自愈就由 Guard.RunTx 自愈后重来)→ 墓碑:点删本行放弃 / 冻结:回滚留着 →
// 建 seq 行(已有则不写)→ AllocateSeq(锁 Q;仍满:回滚留着)→ 普通读本行还在不在(不在 = 别的副本已转走,回滚)→
// 插 O(op_id = 本行的 op_id,不另发号)→ 完整主键点删本行。指令号是预发的,本行已被转走时直接插会撞主键,所以先判再插;
// 本行的删除者(转换)都先持他的护栏副本与 seq 行,持着这两把锁之后的普通读就是权威的。同一笔因此至多变成一条指令。
func (r *WarRepo) ConvertWarCreditOwed(ctx context.Context, owed WarCreditOwed, nowMs uint64) (WarOwedConvertStatus, error) {
	if owed.PlayerID == 0 || owed.OpID == 0 || len(owed.Payload) == 0 || nowMs == 0 {
		return 0, fmt.Errorf("war credit owed: needs non-zero player (%d), op id (%d), payload and now (%d)", owed.PlayerID, owed.OpID, nowMs)
	}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	var fenceHealed, abandoned bool
	err := r.guilds.inTx(ctx, "war_credit_owed", func(ctx context.Context, tx *sql.Tx) error {
		abandoned = false
		verdicts, err := r.guilds.lockCreditFences(ctx, tx, []uint64{owed.PlayerID}, &fenceHealed)
		if err != nil {
			return err
		}
		switch verdicts[owed.PlayerID] {
		case creditFenceRemoved:
			abandoned = true
			return deleteWarCreditOwedTx(ctx, tx, owed)
		case creditFenceFrozen:
			return errWarOwedFrozen
		}
		if err := ensureSeqRowTx(ctx, tx, owed.PlayerID, stream, nowMs); err != nil {
			return err
		}
		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, owed.PlayerID, stream, assetop.DefaultLimits, nowMs)
		if errors.Is(err, assetop.ErrTooManyPending) {
			return errWarOwedWindowFull
		}
		if err != nil {
			return err
		}
		if found, err := lockRowExists(ctx, tx, sqlSelectWarCreditOwedExists, owed.PlayerID, owed.OpID); err != nil {
			return fmt.Errorf("read %s (player=%d op=%d): %w", guildWarCreditOwedTable, owed.PlayerID, owed.OpID, err)
		} else if !found {
			return errWarOwedRowGone
		}
		if err := insertAssetOp(ctx, tx, warCreditRecord(owed, alloc, warOpLease{nextAttemptMs: nowMs}, nowMs)); err != nil {
			return err
		}
		return deleteWarCreditOwedTx(ctx, tx, owed)
	})
	switch {
	case err == nil && abandoned:
		return WarOwedAbandoned, nil
	case err == nil:
		return WarOwedConverted, nil
	case errors.Is(err, errWarOwedRowGone):
		return WarOwedGone, nil
	case errors.Is(err, errWarOwedWindowFull):
		return WarOwedStillFull, nil
	case errors.Is(err, errWarOwedFrozen):
		return WarOwedFrozen, nil
	}
	return 0, err
}

// deleteWarCreditOwedTx 完整主键点删一行待入账;影响 0 行回 errWarOwedRowGone。
func deleteWarCreditOwedTx(ctx context.Context, tx *sql.Tx, owed WarCreditOwed) error {
	res, err := tx.ExecContext(ctx, sqlDeleteWarCreditOwed, owed.PlayerID, owed.OpID)
	if err != nil {
		return fmt.Errorf("delete %s (player=%d op=%d): %w", guildWarCreditOwedTable, owed.PlayerID, owed.OpID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete %s (player=%d op=%d): read rows affected: %w", guildWarCreditOwedTable, owed.PlayerID, owed.OpID, err)
	}
	switch affected {
	case 1:
		return nil
	case 0:
		return errWarOwedRowGone
	default:
		return fmt.Errorf("delete %s (player=%d op=%d): deleted %d rows", guildWarCreditOwedTable, owed.PlayerID, owed.OpID, affected)
	}
}
