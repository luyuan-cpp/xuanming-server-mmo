package data

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"guild/internal/constants"
)

// RolePurgeResult 是删角收尾清理的结论,提交之后的动作(缓存失效、摘榜、推送、摘要 bump)按它做。
type RolePurgeResult struct {
	Player    uint64   // 被删的角色
	GuildID   uint64   // 他清理前所在的帮;0 = 不在帮,提交之后无事可做
	ZoneID    uint32   // 帮会的归属区(guild 行锁内读到),解散时按它摘榜
	Disbanded bool     // 他是帮主、帮里只剩他:帮会已解散
	Successor uint64   // 他是帮主、帮里还有人:接任的帮主;0 = 没有让位
	Members   []uint64 // 留在帮里的其他成员,player_id 升序(推送与摘要的收件人);解散时为空
}

// guildPurgeKeys 是删角收尾逐行点删的按玩家表:player_id 之后的主键列,按表间锁序排列(tables.go:Q < C < R)。
var guildPurgeKeys = []struct{ table, keys string }{
	{guildPlayerOpSeqTable, "stream"},
	{"guild_daily_counter", "counter_kind, ref_id, period_key"},
	{guildClientRequestTable, "client_request_id"},
}

// sqlSelectSuccessorCandidates:帮主被删时的接任候选,普通读(调用方持 guild 行锁,成员集合与职位都稳定)。
// 不在 SQL 里排序:role 编码不连续,先后按 constants.Rank 在 Go 里比(betterSuccessor)。
const sqlSelectSuccessorCandidates = `SELECT player_id, role, contribution_total, join_time_ms FROM guild_member
	WHERE guild_id = ? AND player_id <> ?`

// PurgeRemovedRole 是 guild 的删角收尾清理(docs/design/account-deletion.md §6.2、§6.3),只做数据库,在角色护栏参与者的
// PurgeRemovedRole 事务里跑:调用方已持这名玩家的护栏副本行锁、核过是墓碑。now 是服务进程时钟毫秒(解散时提前截止捐献用)。
//
// 删他的成员行、状态行、申请、每日计数、seq 行与客户端幂等键;终态 guild_asset_op 是资产审计,历练各表(含 guild_trial_lobby_slot、
// guild_trial_launch_role)是其他成员的活动历史,按 §6.2 保留。帮会没有成员数列,人数一律锁内 COUNT。他是帮主时不让帮会无主:
// 帮里还有人就把帮主交给职位最高、帮贡最多、入帮最早的那一位(handOverGuild),只剩他就整帮解散(disbandLockedGuild,
// 与 DisbandGuild 同一段)。不在申请注销时自动转让:那时让玩家自己选(挂起判定在试探时回 guild_leader);执行时他已经无法操作,
// 冷静期里也可能被人转成帮主,挡住只会让删角永远卡住。
//
// 锁序照退帮:副本 → guild → guild_player_state → guild_member(多行按主键升序)→ guild_application → guild_player_op_seq
// → guild_daily_counter → guild_client_request,一律完整主键点操作(guild_manage_repo.go 文件头 (a)(b))。解散那一支在删 guild 行之前
// 已碰过 asset_op / 活动进度 / 历练对局,之后才删他自己的申请、seq、计数与幂等键:这些行只属于他,墓碑之后能写它们的入口都先锁他的
// 副本、在我们之后被拒,剩下的只有一次只持一行的后台清理,回头取它们不会成环。
// 墓碑之后不锁他副本的只剩踢人、解散、转让与任免(都先锁 guild 行,与这里串行;转让也锁目标的副本,不会再把帮主转给他)。
// 所以状态行、seq 行「建出后永不删除」的前提(ensurePlayerStateRows、ensureSeqRowTx)只对在役角色成立也够用。
// 唯一的边角是与这里并发的踢他:排在 guild 行锁后面,醒来读不到状态行,按内部错误失败一次,重试即得「不在帮」。
func PurgeRemovedRole(ctx context.Context, tx *sql.Tx, player, now uint64) (RolePurgeResult, error) {
	res, err := purgeGuildMembership(ctx, tx, player, now)
	if err != nil {
		return RolePurgeResult{}, err
	}
	// 他的状态行锁在上一步已持有(行在的话),候选集完整(applicationKeysOfPlayers)。
	if err := deleteApplicationsOfPlayer(ctx, tx, player); err != nil {
		return RolePurgeResult{}, fmt.Errorf("purge applications of removed player %d: %w", player, err)
	}
	for _, t := range guildPurgeKeys {
		if err := purgePlayerRows(ctx, tx, t.table, t.keys, player); err != nil {
			return RolePurgeResult{}, fmt.Errorf("purge %s of removed player %d: %w", t.table, player, err)
		}
	}
	return res, nil
}

// purgeGuildMembership 按退帮的锁序处理成员行与状态行:guild → guild_player_state → guild_member。
// 先锁 guild 行:解散在这把锁下认定成员全集(lockAllMembers),增删成员、改职位的事务也都先锁它;锁住之后普通读到的成员与职位
// 就是提交点的事实(与 disbandRefusal 同一论证),据此决定离帮、让位还是解散,再按需逐行点锁。成员行先普通读定位帮会:
// 锁住 guild 行之前他可能已被踢出或帮会已解散,入新帮在他的副本上被拒。
func purgeGuildMembership(ctx context.Context, tx *sql.Tx, player, now uint64) (RolePurgeResult, error) {
	res := RolePurgeResult{Player: player}
	var guildID uint64
	switch err := tx.QueryRowContext(ctx, sqlSelectMemberGuild, player).Scan(&guildID); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return res, fmt.Errorf("read guild of removed player %d: %w", player, err)
	}
	var guild guildLockRow
	if guildID != 0 {
		var err error
		guild, err = lockGuildRow(ctx, tx, guildID)
		switch {
		case errors.Is(err, ErrGuildGone):
			guildID = 0
		case err != nil:
			return res, err
		}
	}
	// 状态行(锁序位置 2):行在就锁住,最后与其余个人数据一起删。
	var state uint64
	if err := tx.QueryRowContext(ctx, sqlLockPlayerState, player).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("lock guild player state %d: %w", player, err)
	}
	var role uint32
	if guildID != 0 {
		switch err := tx.QueryRowContext(ctx, sqlSelectMemberRole, guildID, player).Scan(&role); {
		case errors.Is(err, sql.ErrNoRows):
			guildID = 0
		case err != nil:
			return res, fmt.Errorf("read role of removed player %d in guild %d: %w", player, guildID, err)
		}
	}
	if guildID != 0 {
		res.GuildID, res.ZoneID = guildID, guild.ZoneID
		if err := leaveGuildOnPurge(ctx, tx, &res, guild, role, now); err != nil {
			return res, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM guild_player_state WHERE player_id = ?", player); err != nil {
		return res, fmt.Errorf("purge guild player state %d: %w", player, err)
	}
	if res.GuildID != 0 && !res.Disbanded {
		members, err := readIDColumn(ctx, tx, sqlSelectMemberIDs, res.GuildID)
		if err != nil {
			return res, fmt.Errorf("read remaining members of guild %d: %w", res.GuildID, err)
		}
		res.Members = members
	}
	return res, nil
}

// leaveGuildOnPurge 让他离开 res.GuildID:普通成员直接删成员行;帮主(两份存储任一说是就算,与退帮同一判据)把帮主交给
// 接任的人,帮里只剩他就整帮解散。前置:已持 guild 行锁与他的状态行锁(行在的话),role 是锁内普通读到的职位。
func leaveGuildOnPurge(ctx context.Context, tx *sql.Tx, res *RolePurgeResult, guild guildLockRow, role uint32, now uint64) error {
	if constants.Rank(role) != constants.RankLeader && guild.LeaderID != res.Player {
		return removeGuildMember(ctx, tx, res.GuildID, res.Player)
	}
	successor, err := pickGuildSuccessor(ctx, tx, res.GuildID, res.Player)
	if err != nil {
		return err
	}
	if successor == 0 {
		res.Disbanded = true
		return disbandSoloGuild(ctx, tx, res.GuildID, res.Player, now)
	}
	res.Successor = successor
	return handOverGuild(ctx, tx, res.GuildID, res.Player, successor)
}

// removeGuildMember 删一名普通成员(不是帮主)的成员行:主键点锁复核之后点删,与踢人、退帮同一写法。
func removeGuildMember(ctx context.Context, tx *sql.Tx, guildID, player uint64) error {
	_, found, err := lockMemberRole(ctx, tx, guildID, player)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("member %d of guild %d vanished under the guild row lock", player, guildID)
	}
	return execExactlyOneRow(ctx, tx, fmt.Sprintf("purge member %d from guild %d", player, guildID),
		sqlDeleteMember, guildID, player)
}

// guildSuccessorCandidate 是让位时的一名候选,读自同一事务、guild 行锁之下。
type guildSuccessorCandidate struct {
	playerID, contribution, joinedMs uint64
	role                             uint32
}

// betterSuccessor 是接任的先后:职位高的先(按 constants.Rank 比,role 编码不连续不能按原值比),累计帮贡多的先,入帮早的先,
// 最后按 player_id 只为结果确定(发号器的 id 没有先后含义)。
func betterSuccessor(a, b guildSuccessorCandidate) int {
	if c := cmp.Compare(constants.Rank(b.role), constants.Rank(a.role)); c != 0 {
		return c
	}
	if c := cmp.Compare(b.contribution, a.contribution); c != 0 {
		return c
	}
	if c := cmp.Compare(a.joinedMs, b.joinedMs); c != 0 {
		return c
	}
	return cmp.Compare(a.playerID, b.playerID)
}

// pickGuildSuccessor 在帮主被删时选接任的人:除他以外的成员里按 betterSuccessor 取第一名;帮里只剩他一人回 0。
func pickGuildSuccessor(ctx context.Context, tx *sql.Tx, guildID, player uint64) (uint64, error) {
	rows, err := tx.QueryContext(ctx, sqlSelectSuccessorCandidates, guildID, player)
	if err != nil {
		return 0, fmt.Errorf("read successor candidates of guild %d: %w", guildID, err)
	}
	defer rows.Close()
	var candidates []guildSuccessorCandidate
	for rows.Next() {
		var c guildSuccessorCandidate
		if err := rows.Scan(&c.playerID, &c.role, &c.contribution, &c.joinedMs); err != nil {
			return 0, fmt.Errorf("scan successor candidate of guild %d: %w", guildID, err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate successor candidates of guild %d: %w", guildID, err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	return slices.MinFunc(candidates, betterSuccessor).playerID, nil
}

// handOverGuild 把帮主之位交给 successor 并删掉他的成员行,写法照 TransferLeader:成员行按主键升序两次点锁(lockMemberPair),
// 先改 guild 行的 leader_id,再改继任者的职位,最后删他的成员行;事后断言恰好一个帮主,对不上整笔回滚(TransferLeader 第 9 步)。
func handOverGuild(ctx context.Context, tx *sql.Tx, guildID, player, successor uint64) error {
	if _, _, err := lockMemberPair(ctx, tx, guildID, player, successor); err != nil {
		return fmt.Errorf("lock removed leader %d and successor %d of guild %d: %w", player, successor, guildID, err)
	}
	if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("set leader of guild %d", guildID),
		`UPDATE guild SET leader_id = ? WHERE guild_id = ?`, successor, guildID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sqlUpdateMemberRole, constants.RoleLeader, guildID, successor); err != nil {
		return fmt.Errorf("promote successor %d to leader of guild %d: %w", successor, guildID, err)
	}
	if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("purge removed leader %d from guild %d", player, guildID),
		sqlDeleteMember, guildID, player); err != nil {
		return err
	}
	leaders, err := countMembersByRole(ctx, tx, guildID, constants.RoleLeader)
	if err != nil {
		return err
	}
	if leaders != 1 {
		return fmt.Errorf("hand over guild %d: expected exactly 1 leader after write, got %d", guildID, leaders)
	}
	return nil
}

// disbandSoloGuild 帮主是帮里最后一个人:按 DisbandGuild 的同一段解散。lockAllMembers 缺状态行即拒,老成员可能从没建过这一行;
// 他已是墓碑,能给他建行的入口都在他的副本上被拒,所以破例在事务里补(行已在时是空操作),随后与其余个人数据一起删掉。
// 删角收尾只跑在装配了角色护栏的参与者服务里,等同 DisbandGuild 的 r.fence != nil。
func disbandSoloGuild(ctx context.Context, tx *sql.Tx, guildID, player, now uint64) error {
	if _, err := tx.ExecContext(ctx, sqlEnsurePlayerState, player, now); err != nil {
		return fmt.Errorf("ensure guild player state %d before disbanding guild %d: %w", player, guildID, err)
	}
	_, err := disbandLockedGuild(ctx, tx, guildID, now, true)
	return err
}

// FinishRolePurge 是删角收尾提交之后的动作,只在清理事务提交成功之后调(logic 经 rolefence.OnCommit 登记):失效这一帮的快照与他的
// 所属帮映射;帮会解散了再按锁内读到的归属区把它从各分区榜摘掉(与 DisbandGuild 的 logic 同一口径,失败只记日志,RebuildRanks 会自愈)。
// 都是可丢的副作用:缓存最长陈旧一个 TTL,授权只看 MySQL。
func (r *GuildRepo) FinishRolePurge(ctx context.Context, res RolePurgeResult) {
	if res.GuildID == 0 {
		return
	}
	r.invalidateAfterCommit(ctx, opRolePurge, res.GuildID, res.Player)
	if !res.Disbanded {
		return
	}
	if err := r.RemoveGuildFromRank(ctx, res.GuildID, res.ZoneID); err != nil {
		logx.Errorf("[guild] role purge: remove disbanded guild %d (zone %d) from rank: %v", res.GuildID, res.ZoneID, err)
	}
}

// purgePlayerRows 普通读出这名玩家在 table 里的主键(player_id 之后接 keys 列),再按主键升序逐行完整主键点删。
// 墓碑之后只剩后台清理还会删这些行(先点锁、一次一行),候选就是全集;点删影响 0 行是被清理抢先,不算错。
func purgePlayerRows(ctx context.Context, tx *sql.Tx, table, keys string, player uint64) error {
	columns := strings.Split(keys, ", ")
	rows, err := tx.QueryContext(ctx, "SELECT "+keys+" FROM "+table+" WHERE player_id = ? ORDER BY "+keys, player)
	if err != nil {
		return err
	}
	var victims [][]any
	for rows.Next() {
		key := make([]uint64, len(columns))
		dest := make([]any, len(key))
		for i := range key {
			dest[i] = &key[i]
		}
		if err = rows.Scan(dest...); err != nil {
			rows.Close()
			return err
		}
		args := []any{player}
		for _, value := range key {
			args = append(args, value)
		}
		victims = append(victims, args)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	remove := "DELETE FROM " + table + " WHERE player_id = ? AND " + strings.Join(columns, " = ? AND ") + " = ?"
	for _, args := range victims {
		if _, err = tx.ExecContext(ctx, remove, args...); err != nil {
			return err
		}
	}
	return nil
}
