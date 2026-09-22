package data

// guild_lock_plan_mysql_test.go —— B2(管理 / 审批 / 申请 / 建帮 / 解散)锁定语句的执行计划确定性回归
// (真库;GUILD_TEST_MYSQL_DSN 未设即 Skip,库名白名单照 openGuildIntegrationRepo)。
//
// ⚠ 全体 Skip 不代表通过:验收时 DSN 必须真的设上,交付里写明"看到的是 PASS 而不是 SKIP"。
// 选中本用例:`go test ./internal/data -run 'TestGuildLockingStatement'`。
//
// # 为什么要有它
//
// 2026-09-21 死锁修复契约 P3 / P6(依据:docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §3、§6.2;
// friend 会话全仓审计 #2 #3):锁集由执行计划决定,不由 SQL 文本决定。B2 的锁定语句现在全部写成完整主键等值的点操作,
// guild_member 上的锁定 SELECT / UPDATE 另带 `FORCE INDEX (PRIMARY)`(主键之外另有 uk_guild_member(player_id),
// WHERE 同时钉死了它);本用例对**生产代码里的 SQL 常量本身**跑 EXPLAIN,钉住"计划确实是主键":
// 提示子句被误删、某个版本不认它、或单表 DELETE(不收索引提示)被规划到二级索引上,这里都会红。
// 并发用例只能按概率撞上反序,EXPLAIN 每次都答得出来。
//
// # 做法(与 economy_lock_plan_mysql_test.go 同一套,直接复用它的 lockPlanExplain / lockPlanInline / lockPlanPrimaryKeyLen)
//
//   - 断言 key = PRIMARY,key_len = 主键全部列的字节数之和(现算:guild 8、guild_player_state 8、
//     guild_member 16、guild_application 16)。
//   - SELECT … FOR UPDATE 断言 type = const;按主键的 UPDATE / DELETE 走单表范围优化器,显示 range(rows=1),
//     某些版本显示 const —— 两者都接受,index / ALL / ref 一律红。
//   - 夹具只有被查的那几行(小表是最坏情况,不灌数据帮优化器),且每条语句的参数与夹具行逐列对得上:
//     MySQL 的 const 判定在优化期就去读这一行,读不到或其余条件不成立时 table / key 都是 NULL,用例会因夹具而红。
//
// # 不在本用例覆盖范围的
//
//   - 候选**普通读**(按 player_id / guild_id 找申请、成员 id、状态行是否存在、每人待审计数、审批通过前的申请预读):不加锁,
//     走哪个索引只影响性能不影响锁集。"不带锁定子句"由下面的 TestGuildLockingStatementShapes 静态钉住。
//   - INSERT(建状态行、插成员 / 申请 / 帮会):EXPLAIN INSERT 不给访问路径,锁由插入行的主键 / 唯一键值决定。
//   - TransferLeader 的 `UPDATE guild SET leader_id = ? WHERE guild_id = ?`、UpdateGuildScore 的两条 guild 语句:
//     guild 表上 guild_id 只有 PRIMARY 一条路可走,且本批未改动。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
)

// guildLockPlanCase 是一条被核对的 B2 生产 SQL。字段含义同 economy_lock_plan_mysql_test.go 的 lockPlanCase;
// 单独一个类型,免得两个批次的用例表互相牵动。
type guildLockPlanCase struct {
	name      string
	sql       string
	args      []any
	table     string
	wantTypes []string
}

func TestGuildLockingStatementsArePrimaryKeyPointLookups(t *testing.T) {
	ctx, db, _ := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7901
		leader    uint64 = 8901
		member    uint64 = 8902
		applicant uint64 = 8903
	)
	expire := testNowMs + testApplicationTTLMs

	// 夹具:guild 一行、guild_member 帮主 + 被查成员两行(seedManagedGuild 必带帮主)、
	// guild_player_state 被查的一行、guild_application 被查的一行(未过期)。
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})
	mustExec(t, ctx, db, `INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)`, member, testNowMs)
	seedApplicationRow(t, ctx, db, guildID, applicant, testNowMs, expire)

	lockRead := []string{"const"}
	pointWrite := []string{"range", "const"}
	cases := []guildLockPlanCase{
		// guild(锁序位置 1)
		{"sqlLockGuild", sqlLockGuild, []any{guildID}, "guild", lockRead},
		{"sqlUpdateAnnouncement", sqlUpdateAnnouncement, []any{"lock-plan", guildID}, "guild", pointWrite},
		{"sqlDeleteGuild", sqlDeleteGuild, []any{guildID}, "guild", pointWrite},

		// guild_player_state(位置 2):玩家守卫。
		{"sqlLockPlayerState", sqlLockPlayerState, []any{member}, "guild_player_state", lockRead},
		// 同一条语句锁全局插入守卫(lockGlobalInsertGuard,player_id = 0):建帮 / 审批通过 / 建状态行短事务的第一把状态行锁。
		// 哨兵行由 resetGuildSchemaViaMigrate 按启动顺序建好;行不在时 const 判定读不到行,table / key 为 NULL,用例会红。
		{"sqlLockPlayerState(全局插入守卫)", sqlLockPlayerState, []any{globalInsertGuardPlayerID}, "guild_player_state", lockRead},

		// guild_member(位置 3):主键与 uk_guild_member 的列同时被钉死,必须选主键。
		{"sqlLockMemberRole", sqlLockMemberRole, []any{guildID, member}, "guild_member", lockRead},
		{"sqlUpdateMemberRole", sqlUpdateMemberRole, []any{constants.RoleOfficer, guildID, member}, "guild_member", pointWrite},
		// 单表 DELETE 不收索引提示,只能靠这条断言钉住它走 PRIMARY(契约 P3)。
		{"sqlDeleteMember", sqlDeleteMember, []any{guildID, member}, "guild_member", pointWrite},

		// guild_application(位置 4):锁定读 / 刷新 / 三种点删。
		{"sqlSelectApplicationExpire", sqlSelectApplicationExpire, []any{guildID, applicant}, "guild_application", lockRead},
		{"sqlRefreshApplication", sqlRefreshApplication,
			[]any{testNowMs, expire, guildID, applicant}, "guild_application", pointWrite},
		{"sqlDeleteApplication", sqlDeleteApplication, []any{guildID, applicant}, "guild_application", pointWrite},
		// expire_ms <= ? 对夹具行成立(传入 expire 本身)。这条最该防的是被规划到 idx_guild_application_1 的
		// expire_ms 范围上(审计 #3 的旧形状),锁到别帮的过期行。
		{"sqlDeleteExpiredApplication", sqlDeleteExpiredApplication,
			[]any{guildID, applicant, expire}, "guild_application", pointWrite},
		// expire_ms > ? 对夹具行成立(传入 testNowMs)。
		{"sqlCancelApplication", sqlCancelApplication,
			[]any{guildID, applicant, testNowMs}, "guild_application", pointWrite},
	}

	keyLen := map[string]string{}
	for _, table := range []string{"guild", "guild_player_state", "guild_member", "guild_application"} {
		keyLen[table] = lockPlanPrimaryKeyLen(t, ctx, db, table)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := lockPlanInline(t, tc.sql, tc.args...)
			plan := lockPlanExplain(t, ctx, db, stmt)
			// 先确认计划行确实落在这张表上:夹具行缺失或其余条件不成立时 table / key 都是 NULL,Extra 会写原因。
			require.Equal(t, tc.table, plan["table"],
				"%s 的计划行不是 %s(Extra=%q):夹具行缺失或参数与夹具对不上,见文件头。SQL: %s",
				tc.name, tc.table, plan["Extra"], stmt)
			assert.Equal(t, "PRIMARY", plan["key"],
				"%s 没有走主键(possible_keys=%q type=%q):锁定语句必须是完整主键等值,"+
					"走二级索引就是'先二级后主键',与按主键删行的事务反序成环。SQL: %s",
				tc.name, plan["possible_keys"], plan["type"], stmt)
			assert.Equal(t, keyLen[tc.table], plan["key_len"],
				"%s 没有用满主键全部列(key=%q):锁集会扩到同前缀的其它行。SQL: %s", tc.name, plan["key"], stmt)
			assert.Contains(t, tc.wantTypes, plan["type"],
				"%s 的 access type=%q,期望 %v(index / ALL 会逐行加锁扫过别人的行)。SQL: %s",
				tc.name, plan["type"], tc.wantTypes, stmt)
		})
	}
}

// TestGuildLockingStatementShapes 静态钉住 B2 SQL 常量的形状(不连库,任何环境都跑)。
//
// EXPLAIN 用例只能在有库时跑;这一层保证"改 SQL 时手滑"在任何环境都红:
//   - guild_member 的锁定 SELECT / UPDATE 必须带 FORCE INDEX (PRIMARY);
//   - 锁定读与点写 / 点删的定位必须是完整主键等值(复核条件可以另带);
//   - 候选普通读不许带任何锁定子句 —— 一旦带上,它就又成了"经二级索引加锁"(审计 #2 的旧形状)。
func TestGuildLockingStatementShapes(t *testing.T) {
	type namedSQL struct{ name, query string }

	forcePrimary := []namedSQL{
		{"sqlLockMemberRole", sqlLockMemberRole},
		{"sqlUpdateMemberRole", sqlUpdateMemberRole},
	}
	for _, s := range forcePrimary {
		assert.Contains(t, s.query, "guild_member FORCE INDEX (PRIMARY)", "%s 必须强制走主键:%s", s.name, s.query)
	}

	fullPrimaryKey := []namedSQL{
		{"sqlLockMemberRole", sqlLockMemberRole},
		{"sqlUpdateMemberRole", sqlUpdateMemberRole},
		{"sqlDeleteMember", sqlDeleteMember},
		{"sqlSelectApplicationExpire", sqlSelectApplicationExpire},
		{"sqlRefreshApplication", sqlRefreshApplication},
		{"sqlDeleteApplication", sqlDeleteApplication},
		{"sqlDeleteExpiredApplication", sqlDeleteExpiredApplication},
		{"sqlCancelApplication", sqlCancelApplication},
	}
	for _, s := range fullPrimaryKey {
		assert.Contains(t, s.query, "WHERE guild_id = ? AND player_id = ?", "%s 必须按完整主键定位:%s", s.name, s.query)
	}
	assert.Contains(t, sqlLockGuild, "WHERE guild_id = ? FOR UPDATE")
	assert.Contains(t, sqlDeleteGuild, "WHERE guild_id = ?")
	assert.Contains(t, sqlLockPlayerState, "WHERE player_id = ? FOR UPDATE")
	// 申请插入查重必须直接取 X(IODKU),不许退回普通 INSERT 的"先 S 后 X"升级(取锁规则 H2)。
	assert.Contains(t, sqlInsertApplication, "ON DUPLICATE KEY UPDATE apply_ms = apply_ms")
	// 申请后顺带清理的候选读由调用方传上限(purgeExpiredApplicationsPerApply),不许写死一个大数拖长回包。
	assert.True(t, strings.HasSuffix(sqlSelectExpiredApplicantsOfGuild, "ORDER BY player_id LIMIT ?"), sqlSelectExpiredApplicantsOfGuild)
	assert.Positive(t, purgeExpiredApplicationsPerApply)

	plainReads := []namedSQL{
		{"existing player states", sqlSelectExistingPlayerStatesHead + placeholders(2) + sqlSelectExistingPlayerStatesTail},
		{"application keys of players", sqlSelectApplicationKeysOfPlayersHead + placeholders(2) + sqlSelectApplicationKeysOfPlayersTail},
		{"sqlSelectApplicantsOfGuild", sqlSelectApplicantsOfGuild},
		{"sqlSelectExpiredApplicationsOfPlayer", sqlSelectExpiredApplicationsOfPlayer},
		{"sqlSelectExpiredApplicantsOfGuild", sqlSelectExpiredApplicantsOfGuild},
		{"sqlCountPendingApplicationsOfPlayer", sqlCountPendingApplicationsOfPlayer},
		{"sqlSelectMemberIDs", sqlSelectMemberIDs},
		{"sqlSelectMemberGuild", sqlSelectMemberGuild},
		{"sqlCountMembers", sqlCountMembers},
		{"sqlCountLiveApplicationsOfGuild", sqlCountLiveApplicationsOfGuild},
		// 审批通过前的预读(完整性复核 G4):只判申请在不在,决定要不要建状态行 / 取全局插入守卫;锁内仍以 FOR UPDATE 复核。
		{"sqlSelectApplicationExists", sqlSelectApplicationExists},
	}
	for _, s := range plainReads {
		upper := strings.ToUpper(s.query)
		for _, clause := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
			assert.NotContains(t, upper, clause, "%s 是候选 / 计数普通读,不许加锁:%s", s.name, s.query)
		}
	}
}
