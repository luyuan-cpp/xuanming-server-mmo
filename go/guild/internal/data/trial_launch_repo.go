package data

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"guild/internal/constants"
	base "proto/common/base"
	pb "proto/guild"
	matchpb "proto/match"
	"shared/accountdirectory"
	"shared/rolefence"

	"google.golang.org/protobuf/proto"
)

const trialLaunchColumns = "request_id,battle_id,state,original_proto,request_digest,created_ms,updated_ms,cancel_receipt"

// 大厅结束原因(写进视图的 end_tip_id),只由 closeProvenTrialLobby 的两类调用方使用。
const (
	// trialSettledLobbyEndTip:结算 / 标毒同事务关大厅。这一局有结果,客户端按结果显示,不写结束原因。
	trialSettledLobbyEndTip uint32 = 0
	// trialCancelledLobbyEndTip:作废释放(核签的零 battle 取消墓碑,或 formation=5 的发房前中止 / SQL ABORT)写的结束原因
	// (2026-11-05 修复,r2-guild#1 评审第 5 条)。暂用已有的 kGuildTrialServiceBusy:取消墓碑就是「开战没成」,文案准确;
	// 开局后战斗作废还没有专用码 —— Tip.xlsx 加 kGuildTrialBattleAborted 并导表之后,只改这一行。
	trialCancelledLobbyEndTip = constants.ErrTrialServiceBusy
)

func (r *ActivityRepo) TrialLaunchEnabled() bool {
	return r != nil && r.guilds != nil && r.guilds.fence != nil
}

func decodeTrialLaunch(row *pb.GuildTrialLaunchRecord) (*pb.GuildTrialLaunchOriginal, error) {
	if row == nil || row.RequestId == 0 || row.State < 1 || row.State > 3 || row.State == 1 && row.BattleId != 0 || row.State == 2 && row.BattleId == 0 || len(row.OriginalProto) == 0 || len(row.OriginalProto) > 64*1024 {
		return nil, ErrWriteConflict
	}
	sum := sha256.Sum256(row.OriginalProto)
	if !bytes.Equal(sum[:], row.RequestDigest) {
		return nil, ErrWriteConflict
	}
	out := &pb.GuildTrialLaunchOriginal{}
	if proto.Unmarshal(row.OriginalProto, out) != nil || out.RequestId != row.RequestId || out.GuildId == 0 || out.ActivityId == 0 || out.PeriodKey == 0 || out.GuildPeriodKey == 0 || out.InitiatorPlayerId == 0 || out.DungeonId == 0 || len(out.Roster) < minTrialLobbyRoster || len(out.Roster) > maxTrialLobbyRoster {
		return nil, ErrWriteConflict
	}
	seen := map[uint64]bool{}
	var previous uint64
	for _, v := range out.Roster {
		if v.GetPlayerId() == 0 || v.PlayerId <= previous || seen[v.PlayerId] || !accountdirectory.ValidAccount(v.Account) || v.OwnerRevision == 0 {
			return nil, ErrWriteConflict
		}
		seen[v.PlayerId] = true
		previous = v.PlayerId
	}
	if !seen[out.InitiatorPlayerId] {
		return nil, ErrWriteConflict
	}
	return out, nil
}
// scanTrialLaunch 按 trialLaunchColumns 的列序读一行;extra 接在后面,给多选了列的查询用(恢复器认领要多读检查时刻与次数)。
func scanTrialLaunch(row rowScanner, extra ...any) (*pb.GuildTrialLaunchRecord, error) {
	out := &pb.GuildTrialLaunchRecord{}
	dest := append([]any{&out.RequestId, &out.BattleId, &out.State, &out.OriginalProto, &out.RequestDigest, &out.CreatedMs, &out.UpdatedMs, &out.CancelReceipt}, extra...)
	err := row.Scan(dest...)
	return out, err
}
func launchPlayers(original *pb.GuildTrialLaunchOriginal) []uint64 {
	if original == nil {
		return nil
	}
	ids := make([]uint64, 0, len(original.Roster))
	for _, p := range original.Roster {
		ids = append(ids, p.PlayerId)
	}
	return ids
}
func (r *ActivityRepo) TrialLaunch(ctx context.Context, id uint64) (*pb.GuildTrialLaunchRecord, *pb.GuildTrialLaunchOriginal, error) {
	row, err := scanTrialLaunch(r.db.QueryRowContext(ctx, "SELECT "+trialLaunchColumns+" FROM guild_trial_launch WHERE request_id=?", id))
	if err != nil {
		return nil, nil, err
	}
	original, err := decodeTrialLaunch(row)
	return row, original, err
}

// 原同意身份来自私有lobby值；首次提交前逐项核原account/revision，未知提交后只重放永久原件。
func (r *ActivityRepo) PrepareTrialLaunch(ctx context.Context, requestID uint64, battle TrialBattleKey, dungeon uint32, roster []*pb.GuildTrialRoleIdentity, nowMs uint64) (*pb.GuildTrialLaunchOriginal, error) {
	if r.guilds == nil || r.guilds.fence == nil {
		return nil, nil
	} // 旧独立仓储夹具；在线装配强制权威。
	if requestID == 0 || dungeon == 0 || len(roster) < minTrialLobbyRoster || len(roster) > maxTrialLobbyRoster {
		return nil, ErrWriteConflict
	}
	if battle.GuildID == 0 || battle.ActivityID == 0 || battle.PeriodKey == 0 || battle.GuildPeriodKey == 0 || battle.InitiatorPlayerID == 0 {
		return nil, ErrWriteConflict
	}
	proposed := &pb.GuildTrialLaunchOriginal{RequestId: requestID, GuildId: battle.GuildID, ActivityId: battle.ActivityID, PeriodKey: battle.PeriodKey, GuildPeriodKey: battle.GuildPeriodKey, InitiatorPlayerId: battle.InitiatorPlayerID, DungeonId: dungeon, CreatedMs: nowMs}
	for _, p := range roster {
		if p == nil {
			return nil, ErrWriteConflict
		}
		proposed.Roster = append(proposed.Roster, proto.Clone(p).(*pb.GuildTrialRoleIdentity))
	}
	sort.Slice(proposed.Roster, func(i, j int) bool { return proposed.Roster[i].PlayerId < proposed.Roster[j].PlayerId })
	initiator := false
	for _, p := range proposed.Roster {
		if p.PlayerId == proposed.InitiatorPlayerId {
			initiator = true
		}
	}
	if !initiator {
		return nil, ErrWriteConflict
	}
	// 首次 Prepare 之前续租并复核票和整队开战,对应旧 requireTrialEntryFree 在事务里读 match 的两条(规格 §4)。
	// 已有原件的重放不再占:Start 可能已经写了票,这时 Acquire 一定拒绝,结论由事务里的原件比对给出。
	var existing uint64
	err := r.db.QueryRowContext(ctx, "SELECT request_id FROM guild_trial_launch WHERE request_id=?", requestID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		err = r.guilds.acquireTrialOccupancy(ctx, requestID, proposed.Roster, trialOccupancyMargin)
	}
	if err != nil {
		return nil, err
	}
	var result *pb.GuildTrialLaunchOriginal
	err = r.guilds.inTx(withRoleTargets(ctx, launchPlayers(proposed)...), opTrialRegister, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := lockTrialGuild(ctx, tx, battle.GuildID); err != nil {
			return err
		}
		if err := r.verifyTrialOwners(ctx, tx, proposed); err != nil {
			return err
		}
		lobby, err := lockTrialLobbyForLaunch(ctx, tx, proposed)
		if err != nil {
			return err
		}
		if lobby.State == 4 {
			return ErrWriteConflict
		}
		for _, p := range proposed.Roster {
			var member uint64
			if err := tx.QueryRowContext(ctx, "SELECT player_id FROM guild_member WHERE guild_id=? AND player_id=? FOR UPDATE", battle.GuildID, p.PlayerId).Scan(&member); err != nil {
				return err
			}
		}
		row, err := scanTrialLaunch(tx.QueryRowContext(ctx, "SELECT "+trialLaunchColumns+" FROM guild_trial_launch WHERE request_id=? FOR UPDATE", requestID))
		if err == nil {
			saved, err := decodeTrialLaunch(row)
			if err != nil {
				return err
			}
			proposed.CreatedMs = saved.CreatedMs
			if row.State == 3 || !proto.Equal(proposed, saved) {
				return ErrWriteConflict
			}
			for _, p := range saved.Roster {
				var account string
				var revision uint64
				var state uint32
				if err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,state FROM guild_trial_launch_role WHERE player_id=? AND request_id=? FOR UPDATE", p.PlayerId, saved.RequestId).Scan(&account, &revision, &state); err != nil {
					return err
				}
				if account != p.Account || revision != p.OwnerRevision || state != 1 {
					return ErrWriteConflict
				}
			}
			result = saved
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if lobby.State != 2 {
			return ErrWriteConflict
		}
		if err := r.guilds.requireTrialEntryFree(ctx, tx, launchPlayers(proposed), requestID); err != nil {
			return err
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposed)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		// 首次检查时刻落在开局许可窗口之后(trialFirstCheckMs):这次提交许可的 Start 可能还在路上,恢复器在那之前不碰它。
		// 实时路径拿到 match 的确定答复而没开成时,再由 ExpediteTrialLaunchCheck 提前到现在。
		sqlNowMs, err := trialSQLNow(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO guild_trial_launch("+trialLaunchColumns+",next_check_ms,realtime_end_tip) VALUES(?,0,1,?,?,?,?,?,?,?)", requestID, raw, sum[:], nowMs, nowMs, []byte{}, trialFirstCheckMs(lobby.LaunchDeadlineMs, sqlNowMs), []byte{})
		if err != nil {
			return err
		}
		for _, p := range proposed.Roster {
			if _, err = tx.ExecContext(ctx, "INSERT INTO guild_trial_launch_role(player_id,request_id,account,owner_revision,state) VALUES(?,?,?,?,1)", p.PlayerId, requestID, p.Account, p.OwnerRevision); err != nil {
				return err
			}
		}
		result = proposed
		return nil
	})
	return result, err
}
func (r *ActivityRepo) verifyTrialOwners(ctx context.Context, tx *sql.Tx, original *pb.GuildTrialLaunchOriginal) error {
	seen := map[uint64]bool{}
	expects := make([]rolefence.Expect, 0, len(original.Roster))
	for _, p := range original.Roster {
		if p.GetPlayerId() == 0 || seen[p.PlayerId] || !accountdirectory.ValidAccount(p.Account) || p.OwnerRevision == 0 {
			return ErrWriteConflict
		}
		seen[p.PlayerId] = true
		expects = append(expects, rolefence.Expect{PlayerID: p.PlayerId, Account: p.Account, OwnerRevision: p.OwnerRevision, Reject: ErrWriteConflict})
	}
	_, err := rolefence.LockAll(ctx, tx, expects...)
	return err
}

// caller已按全员P升序后持G；既有launch和其ACTIVE投影必须与永久原件完全一致。
func (r *ActivityRepo) lockTrialLaunch(ctx context.Context, tx *sql.Tx, battle TrialBattleKey, original *pb.GuildTrialLaunchOriginal, terminal bool, nowMs uint64) error {
	if r.guilds.fence == nil {
		return nil
	}
	if original == nil || battle.LaunchRequestID == 0 {
		return ErrWriteConflict
	}
	lobby, err := lockTrialLobbyForLaunch(ctx, tx, original)
	if err != nil {
		return err
	}
	row, err := scanTrialLaunch(tx.QueryRowContext(ctx, "SELECT "+trialLaunchColumns+" FROM guild_trial_launch WHERE request_id=? FOR UPDATE", battle.LaunchRequestID))
	if err != nil {
		return err
	}
	saved, err := decodeTrialLaunch(row)
	if err != nil {
		return err
	}
	if !proto.Equal(saved, original) || saved.GuildId != battle.GuildID || saved.ActivityId != battle.ActivityID || saved.PeriodKey != battle.PeriodKey || saved.GuildPeriodKey != battle.GuildPeriodKey || saved.InitiatorPlayerId != battle.InitiatorPlayerID || row.BattleId != 0 && row.BattleId != battle.BattleID {
		return ErrWriteConflict
	}
	if row.State == 3 {
		if terminal {
			return closeProvenTrialLobby(ctx, tx, lobby, trialSettledLobbyEndTip, nil, nowMs)
		}
		return ErrWriteConflict
	}
	if err := r.verifyTrialOwners(ctx, tx, saved); err != nil {
		return err
	}
	for _, p := range saved.Roster {
		var account string
		var revision uint64
		var state uint32
		if err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,state FROM guild_trial_launch_role WHERE player_id=? AND request_id=? FOR UPDATE", p.PlayerId, saved.RequestId).Scan(&account, &revision, &state); err != nil {
			return err
		}
		if account != p.Account || revision != p.OwnerRevision || state != 1 {
			return ErrWriteConflict
		}
	}
	state, roleState := uint32(2), uint32(1)
	if terminal {
		state, roleState = 3, 2
	}
	if _, err = tx.ExecContext(ctx, "UPDATE guild_trial_launch SET battle_id=?,state=?,updated_ms=? WHERE request_id=?", battle.BattleID, state, nowMs, saved.RequestId); err != nil {
		return err
	}
	if terminal {
		for _, p := range saved.Roster {
			if _, err = tx.ExecContext(ctx, "UPDATE guild_trial_launch_role SET state=? WHERE player_id=? AND request_id=?", roleState, p.PlayerId, saved.RequestId); err != nil {
				return err
			}
		}
		return closeProvenTrialLobby(ctx, tx, lobby, trialSettledLobbyEndTip, nil, nowMs)
	}
	return nil
}
func (r *ActivityRepo) trialLaunchOriginal(ctx context.Context, battle *TrialBattleKey) (*pb.GuildTrialLaunchOriginal, error) {
	if r.guilds == nil || r.guilds.fence == nil {
		return nil, nil
	}
	if battle.LaunchRequestID == 0 && battle.BattleID != 0 {
		var id uint64
		if err := r.db.QueryRowContext(ctx, "SELECT request_id FROM guild_trial_launch WHERE battle_id=?", battle.BattleID).Scan(&id); err != nil {
			return nil, err
		}
		battle.LaunchRequestID = id
	}
	_, original, err := r.TrialLaunch(ctx, battle.LaunchRequestID)
	return original, err
}

// 解散只关闭帮会业务，不提供远端开战终局证明；永久原件继续供恢复器核取消。
func releaseGuildTrialLaunches(ctx context.Context, tx *sql.Tx, guildID, nowMs uint64) error {
	// 解散不是Match取消证明；保留所有原launch/role/大厅，后台只能沿签名取消或真实终局释放。
	return nil
}

// ── 恢复器的到期认领(2026-11-05 修复,r2-guild#1 评审第 1、2、6 条)──────────────
//
// 每条在途原件(state 1/2)带一个检查时刻 next_check_ms(SQL 时钟)。恢复器只取到期的行,先用单行 CAS 把检查时刻推后
// (= 认领),认领成功的那个副本才重放这一条的签名取消。由此:
//   - 刚 Prepare、Start 还在路上的原件不到期,恢复器不会抢在 Start 前面写下取消墓碑(第 1 条);
//   - 多副本各自认领不同的行,同一条同一时刻只有一个副本在查,吞吐随副本数增长(第 6 条);
//   - 开局成功、截止前不可能 ABORT 的局按退避拉长检查间隔,不再每副本每 10s 加锁空转一次(第 6 条);
//   - 解不开的行同样被认领(推到退避上限),单独报给调用方,不再整页失败、卡住它后面的原件(第 2 条)。
// 认领只决定「谁、什么时候去查」,不参与正确性:释放仍只凭 TrialCancelReceiptIsTerminal 认定的权威回执,
// 两个副本万一同时查同一条(认领后进程卡顿超过间隔),FinishCancelledTrialLaunch 的行锁与 state3 重放保证只释放一次。
// 宽限只为「Start 结局未知」而留:实时路径拿到 match 的确定答复(业务拒绝,或预算不足根本没发)时,这次 Start 已不在途,
// 它调 ExpediteTrialLaunchCheck 把首次检查提前到现在,下一轮任一副本就认领、取消、释放(2026-11-05 修复,r2-guild#1 复审)。

const (
	// TrialRecoveryGrace:新原件最早在「开局截止、Prepare 提交」两者较晚者之后多久才交给恢复器(实时路径确定答复时除外,
	// 见 ExpediteTrialLaunchCheck)。开局许可只在开局窗口里用:末人同意的那次请求在同一份请求预算(guild Timeout−500ms)里
	// Prepare、再发 Start,单次 Start 不超过 config.MaxMatchRpcTimeoutMs(2s),match 按传播来的截止处理;窗口本身 5s。
	// 再留 30s,实时路径一定已经放弃这次 Start,这时重放取消不会与在途 Start 竞争 —— 否则取消先写下永久墓碑,合法的 Start
	// 被 match 拒成「服务繁忙」,而且副本越多越常见。与占用租约多留的余量同值(trialOccupancyMargin 也要盖住开战窗口与两库时钟差)。
	// 导出只为让 logic 的单测机械核对它盖住开局总预算(开局窗口、请求上限与 match 调用上限都定义在 logic / config,data 看不到)。
	TrialRecoveryGrace = trialOccupancyMargin
	// trialRecheckBase / trialRecheckCap:第 n 次认领(从 0 数)之后到下一次检查的间隔 = base×2^n,封顶 cap,
	// 再加至多 1/5 的随机抖动错开多副本。成局中途失败、发房前中止的局头几次检查就拿到终态;开局成功的局要等 match 截止
	// 之后才可能 ABORT,退避把这段时间里的检查从「每副本每 10s 一次」降到「全体副本合计至多每 2 分钟一次」,
	// 代价是 ABORT 提交之后最多再晚一个 cap(加抖动)才释放。解不开的行直接按 cap 复查。
	trialRecheckBase = 10 * time.Second
	trialRecheckCap  = 2 * time.Minute
	// trialClaimPageMax:一次认领最多看多少行。
	trialClaimPageMax = 100
)

const (
	// sqlSelectDueTrialLaunches:到期的在途原件,按检查时刻先后取(索引 state,next_check_ms)。普通读、不加锁,认领靠下面的 CAS。
	sqlSelectDueTrialLaunches = "SELECT " + trialLaunchColumns + ",next_check_ms,check_attempts FROM guild_trial_launch WHERE state IN(1,2) AND next_check_ms<=" + trialSQLNowMs + " ORDER BY next_check_ms,request_id LIMIT ?"
	// sqlClaimTrialLaunch:单行 CAS 认领,自动提交、不进事务,只锁这一行(改的 next_check_ms 只在非唯一索引里,索引键自带行句柄,
	// 不另加悲观锁)。读到的检查时刻没变才改成「SQL 现在 + 间隔」:别的副本先认领了,或这期间已经终结(state3),都影响 0 行。
	sqlClaimTrialLaunch = "UPDATE guild_trial_launch SET next_check_ms=" + trialSQLNowMs + "+?,check_attempts=check_attempts+1 WHERE request_id=? AND state IN(1,2) AND next_check_ms=?"
	// sqlExpediteTrialLaunchCheck:把一份还没被认领过的 state1 原件的首次检查提前到 SQL 现在,自动提交、只改调度列。
	// 只对从未认领(check_attempts=0)、仍是 state1 且没有 battle 的原件生效,且只往前提、不往后推:重复调用、
	// 原件已被认领或已登记对局 / 已终结时影响 0 行,调度回到认领与退避。
	sqlExpediteTrialLaunchCheck = "UPDATE guild_trial_launch SET next_check_ms=" + trialSQLNowMs + " WHERE request_id=? AND state=1 AND battle_id=0 AND check_attempts=0 AND next_check_ms>" + trialSQLNowMs
	// sqlRecordTrialLaunchEndTip:记下实时路径的结束原因,自动提交、只改这一列。只写一次(已有记录不覆盖),只对还没登记对局、
	// 没终结的 state1 原件写 —— 与提前检查同一前提;原件已登记对局或已终结时影响 0 行。
	sqlRecordTrialLaunchEndTip = "UPDATE guild_trial_launch SET realtime_end_tip=? WHERE request_id=? AND state=1 AND battle_id=0 AND LENGTH(realtime_end_tip)=0"
)

// trialFirstCheckMs 是新原件的首次检查时刻(SQL 时钟毫秒):开局截止与 Prepare 提交时刻两者较晚者,再加 TrialRecoveryGrace。
// 取较晚者是防 Prepare 因锁排队拖过截止才提交:许可从提交那一刻起才可能被用来发 Start。
func trialFirstCheckMs(launchDeadlineMs, sqlNowMs uint64) uint64 {
	return max(launchDeadlineMs, sqlNowMs) + uint64(TrialRecoveryGrace.Milliseconds())
}

// ExpediteTrialLaunchCheck 由实时开局路径在「match 已确定地没有开成这一局」时调用(2026-11-05 修复,r2-guild#1 复审):
// match 同步回了业务拒绝,或请求预算不足、根本没发 Start。这时这次 Start 已不在途,首次检查不必再等 TrialRecoveryGrace,
// 提前到 SQL 现在,下一轮任一副本就认领它、重放签名取消、凭零 battle 的取消墓碑释放占用 —— 否则每一次业务拒绝都让
// 名单上的人多挂约 35s 不能再建房或同意。
//
// 只改调度、不参与释放判定:释放仍只凭 TrialCancelReceiptIsTerminal 认定的权威回执。所以调用方判断错了(比如 match 回了
// 拒绝却其实开了局)也不丢数据:取消回执不是终态,原件照常按退避等结果。Start 结局未知(调用返回传输错误或超时、
// 回成功却没给 battle_id)时调用方不得调本方法:那时 Start 可能仍在 match 处理中,提前取消会把合法的 Start 拒成「服务繁忙」。
// 幂等;没有这一行(Prepare 未提交)时影响 0 行,返回 nil。
func (r *ActivityRepo) ExpediteTrialLaunchCheck(ctx context.Context, requestID uint64) error {
	if r.guilds == nil || r.guilds.fence == nil {
		return nil
	}
	if requestID == 0 {
		return ErrWriteConflict
	}
	_, err := r.db.ExecContext(ctx, sqlExpediteTrialLaunchCheck, requestID)
	return err
}

// RecordTrialLaunchEndReason 由实时开局路径在「match 已确定地没有开成这一局」时、提前检查之前调用(2026-11-11,r2-guild#1 残余):
// 把回给发起人的结束原因(tip id 与参数,如「某人离线」)记在 launch 原件上。原件在途时大厅结束不了(endPendingLobby 拒绝),
// 大厅由恢复器凭取消墓碑释放时才关;没有这条记录,名单上其他人看到的是默认的「服务繁忙」。只影响展示,不参与释放判定;
// 写失败调用方只打日志,释放照常,只是原因退回默认。幂等,只写一次;没有这一行(Prepare 未提交)时影响 0 行,返回 nil。
func (r *ActivityRepo) RecordTrialLaunchEndReason(ctx context.Context, requestID uint64, tip *base.TipInfoMessage) error {
	if r.guilds == nil || r.guilds.fence == nil {
		return nil
	}
	if requestID == 0 || tip.GetId() == 0 {
		return ErrWriteConflict
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(tip)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, sqlRecordTrialLaunchEndTip, raw, requestID)
	return err
}

// cancelledTrialLobbyEnd 是作废释放写进大厅的结束原因与参数。这一局没登记对局(零 battle 的取消墓碑:Start 被 match 拒绝或
// 根本没发)且实时路径记下了原因(realtime_end_tip)时用它,名单上所有人看到与发起人同一个具体原因;否则 trialCancelledLobbyEndTip。
// 调用方已持这一行的 FOR UPDATE 锁。原字节解不开或 tip 为 0 时退回默认(只影响展示,不让坏的展示数据挡住释放)。
func cancelledTrialLobbyEnd(ctx context.Context, tx *sql.Tx, requestID uint64, receipt *matchpb.CancelActivityBattleResponse) (uint32, []string, error) {
	if receipt.GetBattleId() != 0 {
		return trialCancelledLobbyEndTip, nil, nil
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, "SELECT realtime_end_tip FROM guild_trial_launch WHERE request_id=?", requestID).Scan(&raw); err != nil {
		return 0, nil, err
	}
	tip := &base.TipInfoMessage{}
	if len(raw) == 0 || proto.Unmarshal(raw, tip) != nil || tip.GetId() == 0 {
		return trialCancelledLobbyEndTip, nil, nil
	}
	return tip.GetId(), tip.GetParameters(), nil
}

// trialRecheckDelay 是第 attempts 次认领(从 0 数)之后到下一次检查的间隔。jitter 取 [0,1),由调用方给(单测可固定)。
func trialRecheckDelay(attempts uint32, jitter float64) time.Duration {
	delay := trialRecheckBase
	for i := uint32(0); i < attempts && delay < trialRecheckCap; i++ {
		delay *= 2
	}
	delay = min(delay, trialRecheckCap)
	return delay + time.Duration(jitter*float64(delay)/5)
}

// DueTrialLaunches 是恢复器一次认领的结果。只有本副本 CAS 成功的行才会出现在这里。
type DueTrialLaunches struct {
	// Originals:可解码的原件,本副本已认领,应当马上重放它们的签名取消。
	Originals []*pb.GuildTrialLaunchOriginal
	// Undecodable:解不开的行的 request_id(摘要不符、名单越界、原件损坏、状态与 battle_id 矛盾)。
	// 行原样保留、等人工处理,认领时已按 trialRecheckCap 推后,不再占住到期队列的队头。
	Undecodable []uint64
	// More:这一页读满了 limit 行,可能还有到期的行没看(调用方预算够就再认领一页)。
	More bool
}

// ClaimDueTrialLaunches 认领至多 limit 条到期的在途原件(见本节说明)。
// 出错时返回出错之前已认领的部分:它们的检查时刻已经推后,调用方照常处理,否则要等到下一次检查时刻。
func (r *ActivityRepo) ClaimDueTrialLaunches(ctx context.Context, limit int) (DueTrialLaunches, error) {
	var out DueTrialLaunches
	if r.guilds.fence == nil {
		return out, nil
	}
	if limit < 1 || limit > trialClaimPageMax {
		return out, ErrWriteConflict
	}
	type dueRow struct {
		record      *pb.GuildTrialLaunchRecord
		nextCheckMs uint64
		attempts    uint32
	}
	rows, err := r.db.QueryContext(ctx, sqlSelectDueTrialLaunches, limit)
	if err != nil {
		return out, err
	}
	var page []dueRow
	for rows.Next() {
		row := dueRow{}
		if row.record, err = scanTrialLaunch(rows, &row.nextCheckMs, &row.attempts); err != nil {
			rows.Close()
			return out, err
		}
		page = append(page, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.More = len(page) == limit
	for _, row := range page {
		original, decodeErr := decodeTrialLaunch(row.record)
		delay := trialRecheckCap
		if decodeErr == nil {
			delay = trialRecheckDelay(row.attempts, rand.Float64())
		}
		claimed, err := r.claimTrialLaunch(ctx, row.record.RequestId, row.nextCheckMs, delay)
		if err != nil {
			return out, err
		}
		switch {
		case !claimed:
			// 别的副本先认领了,或这期间已终结。
		case decodeErr != nil:
			out.Undecodable = append(out.Undecodable, row.record.RequestId)
		default:
			out.Originals = append(out.Originals, original)
		}
	}
	return out, nil
}

// claimTrialLaunch 用读到的检查时刻做 CAS,把它推后 delay。返回 true = 本副本认领到了这一行。
func (r *ActivityRepo) claimTrialLaunch(ctx context.Context, requestID, observedNextCheckMs uint64, delay time.Duration) (bool, error) {
	result, err := r.db.ExecContext(ctx, sqlClaimTrialLaunch, uint64(delay.Milliseconds()), requestID, observedNextCheckMs)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// TrialCancelReceiptIsTerminal 判一份已验签的 match 取消回执能否终结原 launch 与参与者 ACTIVE 占用
// (2026-10-23 修复，审计 r2-guild#1；恢复器 recoverOne 与 FinishCancelledTrialLaunch 共用这一条规则)。只认两种权威终态：
//
//   - 零 battle 的永久取消墓碑(RequestCancelled 且 BattleId==0)：原 Start 从未成局；
//   - formation=5 且不是 COMPLETE：发房前中止(没有 Create 原件)，或开局成功(formation=4)后战斗不出结果，
//     match 在权威截止后与 COMPLETE 争同一主键、原子提交的 SQL ABORT(同一事务把原票置 4)。
//     有 Create 原件却没有 SQL ABORT 的 state5，match 直接报错、不签回执(direct_cancel.go)。
//
// formation=1..4(含开局成功、截止前还没出结果)、RPC 超时或失败都不是终态，占用保留到下一轮 —— 一次超时就释放，
// 同一批角色可能被这一局与下一局同时占用。formation=5 却带 COMPLETE 是自相矛盾的回执：COMPLETE 的局要经结果事件结算，
// 不能当作废按 LOSS 收尾，资产路径 fail-closed。
func TrialCancelReceiptIsTerminal(receipt *matchpb.CancelActivityBattleResponse) bool {
	if receipt == nil {
		return false
	}
	if receipt.RequestCancelled && receipt.BattleId == 0 {
		return true
	}
	return receipt.FormationState == 5 && receipt.BattleId != 0 &&
		receipt.TerminalDecision != matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE
}

// receipt仅由显式注入且验证match回复签名/原请求摘要的适配器传入，客户端无此入口。
// 终局证明与ACTIVE解除同事务保存；未知提交重读原launch，不发新Start/新名单。
// 多副本同时拿到同一份终态回执时只有一次真正迁移：原 launch 行 FOR UPDATE，已是 state3 的重放只关闭大厅、不再改任何行。
// 同一事务里:大厅写 ENDED 并带结束原因(cancelledTrialLobbyEnd:实时路径记下的业务拒绝原因,没有则 trialCancelledLobbyEndTip);
// 帮会还在时对局行经 settleCancelledTrialBattle 记 SETTLED/LOSS,帮会已解散时不碰对局行。锁序 P→G→大厅/slot→launch→role→T。
func (r *ActivityRepo) FinishCancelledTrialLaunch(ctx context.Context, original *pb.GuildTrialLaunchOriginal, receipt *matchpb.CancelActivityBattleResponse, nowMs uint64) error {
	if original == nil || !TrialCancelReceiptIsTerminal(receipt) {
		return ErrWriteConflict
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return err
	}
	err = r.guilds.inTx(withRoleTargets(ctx, launchPlayers(original)...), opTrialMark, func(ctx context.Context, tx *sql.Tx) error {
		// 已解散帮会也须消费可信取消证明；G缺失时不补造帮会或奖项。
		_, err := lockTrialGuild(ctx, tx, original.GuildId)
		guildGone := errors.Is(err, ErrGuildGone)
		if err != nil && !guildGone {
			return err
		}
		lobby, err := lockTrialLobbyForLaunch(ctx, tx, original)
		if err != nil {
			return err
		}
		row, err := scanTrialLaunch(tx.QueryRowContext(ctx, "SELECT "+trialLaunchColumns+" FROM guild_trial_launch WHERE request_id=? FOR UPDATE", original.RequestId))
		if err != nil {
			return err
		}
		saved, err := decodeTrialLaunch(row)
		if err != nil {
			return err
		}
		if !proto.Equal(saved, original) || row.BattleId != 0 && row.BattleId != receipt.BattleId {
			return ErrWriteConflict
		}
		endTip, endParams, err := cancelledTrialLobbyEnd(ctx, tx, saved.RequestId, receipt)
		if err != nil {
			return err
		}
		if row.State == 3 {
			return closeProvenTrialLobby(ctx, tx, lobby, endTip, endParams, nowMs)
		}
		if err := r.verifyTrialOwners(ctx, tx, saved); err != nil {
			return err
		}
		for _, p := range saved.Roster {
			var account string
			var revision uint64
			var state uint32
			if err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,state FROM guild_trial_launch_role WHERE player_id=? AND request_id=? FOR UPDATE", p.PlayerId, saved.RequestId).Scan(&account, &revision, &state); err != nil {
				return err
			}
			if account != p.Account || revision != p.OwnerRevision || state != 1 {
				return ErrWriteConflict
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE guild_trial_launch SET battle_id=?,state=3,cancel_receipt=?,updated_ms=? WHERE request_id=?", receipt.BattleId, raw, nowMs, saved.RequestId); err != nil {
			return err
		}
		for _, p := range saved.Roster {
			if _, err = tx.ExecContext(ctx, "UPDATE guild_trial_launch_role SET state=2 WHERE player_id=? AND request_id=?", p.PlayerId, saved.RequestId); err != nil {
				return err
			}
		}
		if receipt.BattleId != 0 && !guildGone {
			if err := settleCancelledTrialBattle(ctx, tx, original, receipt.BattleId, nowMs); err != nil {
				return err
			}
		}
		return closeProvenTrialLobby(ctx, tx, lobby, endTip, endParams, nowMs)
	})
	if err == nil {
		r.guilds.releaseTrialOccupancy(ctx, original.RequestId, launchPlayers(original))
	}
	return err
}

// settleCancelledTrialBattle 把作废的这一局记成 SETTLED/LOSS(2026-11-05 修复,r2-guild#1 评审第 4 条)。
// 前置:调用方持有 original.GuildId 的 G 行锁(帮会存在)—— 与结算、标毒、巡检同一个写入口:先 readTrialGate 普通读
// (核 guild_id,别帮的行回 errTrialForeignGuild;已 SETTLED 回错),再 writeTrialBattleState 按读到的状态点改
// STARTED / EXPIRED → SETTLED/LOSS,缺行补一行 SETTLED/LOSS(与标毒补行同形,成为这一局的终态闸门)。
// 帮会已解散时调用方不调本函数:解散同事务删了在途行,剩下的历史行此后没有任何路径再写(guild_manage_repo 的解散不变量)。
func settleCancelledTrialBattle(ctx context.Context, tx *sql.Tx, original *pb.GuildTrialLaunchOriginal, battleID, nowMs uint64) error {
	battle := TrialBattleKey{BattleID: battleID, GuildID: original.GuildId, ActivityID: original.ActivityId, PeriodKey: original.PeriodKey,
		GuildPeriodKey: original.GuildPeriodKey, InitiatorPlayerID: original.InitiatorPlayerId, LaunchRequestID: original.RequestId}
	gate, err := readTrialGate(ctx, tx, battle)
	if errors.Is(err, errTrialAlreadySettled) {
		// COMPLETE 与 ABORT 在 match 同一主键上互斥,结算与标毒又在同一事务终结 launch;走到这里说明有一方违反了不变量,
		// 宁可不释放也不覆盖一份已结算的结果(fail-closed)。
		return fmt.Errorf("trial battle %d is already settled while its launch %d is still in flight, keeping occupancy: %w", battleID, original.RequestId, err)
	}
	if err != nil {
		return err
	}
	return writeTrialBattleState(ctx, tx, battle, gate, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED,
		pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS, 0, original.CreatedMs, nowMs)
}
