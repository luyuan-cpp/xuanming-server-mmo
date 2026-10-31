package data

// guild_lock_order_mysql_test.go —— B2 事务的并发锁序回归(真库;GUILD_TEST_MYSQL_DSN 未设即 Skip)。
//
// ⚠ 全体 Skip 不代表通过:验收时 DSN 必须真的设上,交付里写明"看到的是 PASS 而不是 SKIP"。
// 选中本文件:`go test ./internal/data -run 'TestGuildLockOrder' -count=3`。
//
// # 覆盖的环(2026-09-21 死锁修复契约 §2.1 第 9 条;friend 会话全仓审计 #2 #6)
//
//   - Review(G1,p,通过) ‖ Review(G2,p,拒绝) ‖ Cancel(G3,p):修复前"通过"按 player_id 删申请经 idx_guild_application_0,
//     先二级后主键,与拒绝 / 撤回的"先主键后二级"在 (G2,p) / (G3,p) 上反序(审计 #2 (a))。
//   - ApplyToGuild(p→G1) ‖ CancelApplication(G2,p) ‖ Disband(G2) ‖ Review(G1,x,过期分支):修复前申请的
//     `SELECT … WHERE player_id = ? FOR UPDATE` 与本人过期行 DELETE 都经 idx_0 / idx_1,与撤回 / 解散按主键删反序
//     (审计 #2 (b)(d));本帮过期行的 LIMIT 删除可能走 idx_1 跨帮(审计 #3,现已移到提交后、每行一个 RC 短事务点删)。
//   - ApplyToGuild(p→G) ‖ CancelApplication(G,p) ×2:在刚被撤回删掉的记录上重插,普通 INSERT 的 S→X 升级
//     会排在排队的撤回后面(取锁规则 H2);插入已改 IODKU。
//   - LeaveGuild(G,p) ‖ Review(G2,p,通过):修复前退帮不拿 p 的状态行,删成员行留下的 uk 项与删申请,
//     和审批方的"插成员撞 uk / 已锁 (G2,p)"成环(契约 P4)。
//   - 同名 CreateGuild 三方,首个因 p1 已入帮回滚:修复前 INSERT guild 在前,首个插入者回滚后另外两人的 S 锁同时授予、
//     再插入时互等(审计 #6)。现在 INSERT guild 是最后一条写。
//   - Disband(G) ‖ Kick / Leave(G 的成员):补上玩家守卫之后三者仍只按 guild → 状态行 → 成员行 → 申请行的同一顺序取锁。
//
// 2026-09-21 第二轮复核补的(完整性审查缺口与 G-OUT1):
//   - ApplyToGuild(p→G1) 惰性删本人过期行 (G4,p) ‖ Review(G4,p) 过期分支(审计 #2 (d))。
//   - CreateGuild(p) ‖ Review(G2,p,拒绝) ‖ Cancel(G3,p)(审计 #2 (c));Leave / Kick 删 p 名下申请 ‖ 别帮拒绝 / 撤回(#2 主形态的删除方)。
//   - 唯一二级索引的两个查重插入者(G-OUT1):同名建帮、uk_guild 上相邻的两个名字、uk_guild_member 上相邻的两个刚离帮玩家
//     (建帮 ‖ 审批通过)。三组都用"占锁事务先删目标行不提交 → 放两路插入者 → 松开"把双方的查重对齐到同一时刻;
//     双方的 S next-key 互挡插入意向(1213)。2026-10-30 撤掉全局插入守卫之后这个环在 MySQL 档上重新可见,由 retryOnDeadlock 吸收,
//     这三组不再要求钩子为空,改为只看业务结局确定;TiDB 没有这个环(guild_manage_repo.go 文件头 (d))。
//   - 建状态行时首个插入者回滚(G-C3 同形):同上,重试吸收,两个补建者都成功、恰好一行。
//
// 2026-09-21 第四轮(完整性复核 G2 / G4):
//   - Disband(G) ‖ Review(G2,p,通过),p 刚离帮、uk_guild_member 上 p 的后继项是 G 的成员 q(G2):审批方查重对 uk(q) 加的
//     S 与解散删 q 的成员行对撞。修复("删成员行前移到删申请之前")之前是环,用读视图挡住 purge、确定性地摆出这个交错;
//   - 踢一个不是本帮成员的 id / 批准一条不存在的申请(G4):不建状态行。
//
// 2026-10-30:占锁事务卡住一名玩家的建帮时,别人的建帮、审批通过、首次建状态行不受影响(不再全服排队)。
//
// 另有两条确定性用例:删成员行的三个事务确实先拿玩家守卫(P4);事务外建状态行不在已存在的行上取锁。
//
// # 判据(契约 P6,选定一种):txDeadlockObserved 钩子的记录为空(上面写明由重试吸收的几组除外)
//
// inTx(CancelApplication、申请后的惰性清理、建状态行的短事务都走它)经 retryOnDeadlock,1213 会被吸收掉重跑,
// 调用方只看得见最终成功(seq 行自 C2 起在 T-D / T-S 的 inTx 里建)。
// 看不见的:TiDB 在语句级内部重试掉的死锁(不回到应用层),那一类要看 INFORMATION_SCHEMA.CLUSTER_DEADLOCKS
// (economy_repo_test.go 的 econDeadlockWatch 已接,本文件的用例是 MySQL 机制的回归)。
// 本文件把钩子换成记录器,断言整组轮次结束后**一次都没有**。它只数本进程里帮会写路径真正撞上的死锁,
// 不受同一 MySQL 实例上别的测试影响,也不需要 PROCESS 权限(与 economy_repo_test.go 的 econDeadlockWatch
// 看 LATEST DETECTED DEADLOCK 是两种判据,各自独立成立)。
// 锁等待超时(1205)不进钩子:它说明有人持锁过久,由各用例对返回值的断言看住(不在允许集合里就红)。
//
// goroutine 里只调被测接口、收集错误,不调 t.Fatal / require;断言全部在主 goroutine 里做。

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
)

const (
	// lockOrderRounds:无编排的对撞轮数。单轮不撞是运气,几十轮都不撞才说明取锁顺序真的一致。
	lockOrderRounds = 30
	// lockOrderStagedRounds:带"先卡住一方"编排的轮数(每轮有几百毫秒的有界等待)。
	lockOrderStagedRounds = 10
	// lockOrderBudget:循环用例自带的总预算。openGuildIntegrationRepo 给的 ctx 只有 20s(含重建 schema),
	// 多轮对撞放在它上面会在慢机器上被截断成"半截通过"。
	lockOrderBudget = 120 * time.Second
	// lockOrderSettle:放行第二批参与者后、松开占锁事务前的等待。只影响修复前能否稳定复现,不影响判据。
	lockOrderSettle = 100 * time.Millisecond
)

// lockOrderDeadlockLog 记录 txDeadlockObserved 收到的每一次死锁 / 写冲突。
type lockOrderDeadlockLog struct {
	mu     sync.Mutex
	events []string
}

// watchGuildTxDeadlocks 把 txDeadlockObserved 换成记录器,用例结束时还原。
// 必须在起任何并发之前调用(替换发生在主 goroutine,之后的 go 语句与它有先后关系,读写不竞争)。
func watchGuildTxDeadlocks(t *testing.T) *lockOrderDeadlockLog {
	t.Helper()
	rec := &lockOrderDeadlockLog{}
	original := txDeadlockObserved
	txDeadlockObserved = func(op string, err error) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.events = append(rec.events, fmt.Sprintf("op=%s: %v", op, err))
	}
	t.Cleanup(func() { txDeadlockObserved = original })
	return rec
}

// assertNone 在全部并发结束之后、主 goroutine 里调用。
func (l *lockOrderDeadlockLog) assertNone(t *testing.T, what string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	assert.Empty(t, l.events, "%s:出现了被重试吸收掉的死锁 / 写冲突(返回值看不出来),修复后必须为 0 次", what)
}

// lockOrderContext 给循环用例一份独立的总预算。
func lockOrderContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderBudget)
	t.Cleanup(cancel)
	return ctx
}

// lockOrderGo 在后台跑 fn,结果从返回的通道取(带缓冲,主 goroutine 提前失败退出时后台不会卡住)。
func lockOrderGo(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return done
}

// lockOrderAwaitLockWaits 等到实例上至少 n 个事务处于锁等待(INFORMATION_SCHEMA.INNODB_TRX,需 PROCESS 权限),
// 最多等 limit。只用来把"先让一方卡住、再放另一方"的交错摆出来 —— 影响的是修复前能否稳定复现,
// 不影响修复后的判据。账号没有 PROCESS 权限时查询报错,退化成固定等待 limit。
func lockOrderAwaitLockWaits(ctx context.Context, db *sql.DB, n int, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var waiting int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.INNODB_TRX WHERE trx_state = 'LOCK WAIT'").Scan(&waiting)
		if err != nil {
			time.Sleep(time.Until(deadline))
			return
		}
		if waiting >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lockOrderFounding 造一份建帮入参(建帮者即帮主)。
func lockOrderFounding(guildID, leaderID uint64, name string, zone uint32, now uint64) *GuildData {
	return &GuildData{
		GuildID: guildID, Name: name, LeaderID: leaderID,
		Level: constants.DefaultInitLevel, MaxMembers: 30, ZoneID: zone, CreateTimeMs: now,
		Members: []MemberData{{PlayerID: leaderID, Role: constants.RoleLeader, JoinTimeMs: now, LastActiveMs: now}},
	}
}

// TestGuildLockOrder_ReviewApproveVersusRejectVersusCancel:同一申请人同时被 G1 批准、被 G2 拒绝、自己撤回 G3。
//
// 修复后的取锁序列(p = 申请人):
//   - 通过:guild(G1) → state(p) → member(G1,审批人) → app(G1,p) → 插 member(G1,p) → app(G2,p) → app(G3,p)(主键升序点删);
//   - 拒绝:guild(G2) → member(G2,审批人) → app(G2,p) → 删它自己的二级项;
//   - 撤回:app(G3,p) → 删它自己的二级项(RC 短事务,见 CancelApplication)。
//
// 三方共享的只有申请行,且每一方对任一申请行都是"先主键后二级";拒绝与撤回各只碰一行 —— 没有第二把可互等的锁。
func TestGuildLockOrder_ReviewApproveVersusRejectVersusCancel(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g1, g2, g3 uint64 = 7601, 7602, 7603
		l1, l2, l3 uint64 = 8601, 8602, 8603
		applicant  uint64 = 8609
		zone       uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g1, zone, 1, 50, l1, nil)
	seedManagedGuild(t, seedCtx, db, g2, zone, 1, 50, l2, nil)
	seedManagedGuild(t, seedCtx, db, g3, zone, 1, 50, l3, nil)

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id=? AND player_id=?", g1, applicant)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE guild_id IN (?, ?, ?) AND player_id=?", g1, g2, g3, applicant)
		for _, guildID := range []uint64{g1, g2, g3} {
			seedApplicationRow(t, ctx, db, guildID, applicant, now, now+testApplicationTTLMs)
		}

		errs := runConcurrently(
			func() error {
				_, err := repo.ReviewApplication(ctx, g1, l1, applicant, true, zone, now)
				return err
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g2, l2, applicant, false, 0, now)
				return err
			},
			func() error { return repo.CancelApplication(ctx, g3, applicant, now) },
		)

		require.NoError(t, errs[0], "round %d:批准一方的申请行没人动,必须成功", round)
		for i, err := range errs[1:] {
			if err != nil {
				// 通过方先删掉了 (G2,p) / (G3,p):拒绝 / 撤回看到的是"这条已经不在了"。
				assertErrorIn(t, err, ErrApplicationNotFound)
				t.Logf("round %d party %d: %v", round, i+1, err)
			}
		}
		role, found := memberRoleOf(t, ctx, db, g1, applicant)
		require.True(t, found, "round %d:申请人应已入 G1", round)
		assert.Equal(t, constants.RoleMember, role)
		assert.Zero(t, playerApplicationCount(t, ctx, db, applicant), "round %d:I2 清干净", round)
	}
	deadlocks.assertNone(t, "Review(G1,p,通过) ‖ Review(G2,p,拒绝) ‖ Cancel(G3,p)")
}

// TestGuildLockOrder_ApplyVersusCancelDisbandAndExpiredReview:四方同时动申请表。
//
// 修复后的取锁序列(p = 申请人,m = G2 的成员,x = G1 的一个过期申请人):
//   - 申请 p→G1:guild(G1) → state(p) → app(G4,p)(本人过期行点删)→ app(G1,p) FOR UPDATE / 插入;
//     提交后逐行 RC 短事务点删 app(G1,x)(本帮过期行,每行单锁);
//   - 撤回 (G2,p):app(G2,p) 点删(RC 短事务);
//   - 解散 G2:guild(G2) → state(l2) → state(m) → member(G2,l2) → member(G2,m)(锁后即删)→ app(G2,p) → app(G4,m)
//     (合并后主键升序)→ 提前截止 → guild(G2);
//   - 审批 G1 对 x(过期分支):guild(G1) → member(G1,l1) → app(G1,x) FOR UPDATE → 点删。
//
// 申请事务的申请行锁集全在 p 名下;解散在 p 名下只碰 (G2,p) 一行,撤回也只碰这一行;审批与提交后清理只在 (G1,x) 相遇,
// 清理那一方每条语句只取一把锁。任意两方之间至多共享一行,不存在"各持一把、互等另一把"。
func TestGuildLockOrder_ApplyVersusCancelDisbandAndExpiredReview(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g1, g2, g4 uint64 = 7611, 7612, 7614
		l1, l2, l4 uint64 = 8611, 8612, 8614
		m          uint64 = 8615 // G2 的普通成员,在 G4 留有残留申请
		applicant  uint64 = 8619
		expiredX   uint64 = 8618 // 在 G1 有一条过期申请
		zone       uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g1, zone, 1, 50, l1, nil)
	seedManagedGuild(t, seedCtx, db, g4, zone, 1, 50, l4, nil)

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		// 每轮重建 G2(上一轮被解散了)与四个人的申请。
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id=?", g2)
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id=?", g2)
		for _, playerID := range []uint64{applicant, expiredX, m} {
			mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", playerID)
		}
		seedManagedGuild(t, ctx, db, g2, zone, 1, 50, l2, map[uint64]uint32{m: constants.RoleMember})
		seedApplicationRow(t, ctx, db, g2, applicant, now, now+testApplicationTTLMs)
		// 本人的过期行(申请事务内惰性清理)。
		seedApplicationRow(t, ctx, db, g4, applicant, now-testApplicationTTLMs, now-1)
		// 成员的残留申请(解散按 I3 删)。
		seedApplicationRow(t, ctx, db, g4, m, now, now+testApplicationTTLMs)
		// 本帮的过期行(申请提交后清理,或审批过期分支删)。
		seedApplicationRow(t, ctx, db, g1, expiredX, now-testApplicationTTLMs, now-1)

		errs := runConcurrently(
			func() error {
				res, err := repo.ApplyToGuild(ctx, g1, applicant, zone, now, testRules)
				if err == nil && !res.Inserted {
					return fmt.Errorf("round %d: apply should insert a new application", round)
				}
				return err
			},
			func() error { return repo.CancelApplication(ctx, g2, applicant, now) },
			func() error {
				_, err := repo.DisbandGuild(ctx, g2, l2, now)
				return err
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g1, l1, expiredX, false, 0, now)
				return err
			},
		)

		require.NoError(t, errs[0], "round %d:申请", round)
		if errs[1] != nil {
			assertErrorIn(t, errs[1], ErrApplicationNotFound) // 解散先删掉了 (G2,p)
		}
		require.NoError(t, errs[2], "round %d:解散", round)
		// 过期分支删行后回 NotFound;提交后的清理先删掉的话,审批锁读不到行,同样 NotFound。
		require.Error(t, errs[3], "round %d:过期申请不能被审批成功", round)
		assertErrorIn(t, errs[3], ErrApplicationNotFound)

		assert.Zero(t, guildRowCount(t, ctx, db, g2), "round %d:G2 已解散", round)
		assert.Zero(t, memberCount(t, ctx, db, m), "round %d", round)
		assert.Equal(t, 1, guildApplicationCount(t, ctx, db, g1, applicant), "round %d:新申请落地", round)
		assert.Zero(t, guildApplicationCount(t, ctx, db, g2, applicant), "round %d", round)
		assert.Zero(t, guildApplicationCount(t, ctx, db, g4, applicant), "round %d:本人过期行被惰性清理", round)
		assert.Zero(t, playerApplicationCount(t, ctx, db, m), "round %d:I3 清掉成员在别帮的申请", round)
		assert.Zero(t, playerApplicationCount(t, ctx, db, expiredX), "round %d:本帮过期行被删", round)
	}
	deadlocks.assertNone(t, "Apply(p→G1) ‖ Cancel(G2,p) ‖ Disband(G2) ‖ Review(G1,x,过期)")
}

// TestGuildLockOrder_ApplyVersusCancelSameGuild:同一玩家对同一帮"撤回 → 再申请 + 连点两次撤回"。
//
// 每轮先顺序撤回一次,留下一条已提交删除、可能还没被 purge 的删除标记记录 (G,p);随后申请与两条撤回同时发车。
// 普通 INSERT 撞删除标记记录要"先 S 后 X",若某条撤回的 X 请求恰好排在两步之间,升级就排在它后面 —— 互等成环
// (取锁规则 H2)。申请的插入现在是 IODKU,查重直接取 X,撤回只会排队。purge 何时发生不可控,所以这是概率性回归;
// 判据仍是钩子零记录。业务结局:申请总是新插入(它在 p 的守卫下读不到活行),撤回要么删到新行、要么 NotFound。
func TestGuildLockOrder_ApplyVersusCancelSameGuild(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g      uint64 = 7671
		leader uint64 = 8671
		p      uint64 = 8679
		zone   uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g, zone, 1, 50, leader, nil)

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", p)
		seedApplicationRow(t, ctx, db, g, p, now, now+testApplicationTTLMs)
		require.NoError(t, repo.CancelApplication(ctx, g, p, now), "round %d:先撤回一次,留下删除标记记录", round)

		errs := runConcurrently(
			func() error {
				res, err := repo.ApplyToGuild(ctx, g, p, zone, now, testRules)
				if err == nil && !res.Inserted {
					return fmt.Errorf("round %d: apply should insert a new application", round)
				}
				return err
			},
			func() error { return repo.CancelApplication(ctx, g, p, now) },
			func() error { return repo.CancelApplication(ctx, g, p, now) },
		)

		require.NoError(t, errs[0], "round %d:申请", round)
		for _, err := range errs[1:] {
			if err != nil {
				assertErrorIn(t, err, ErrApplicationNotFound)
			}
		}
		assert.LessOrEqual(t, guildApplicationCount(t, ctx, db, g, p), 1, "round %d", round)
	}
	deadlocks.assertNone(t, "ApplyToGuild(p→G) ‖ CancelApplication(G,p) ×2(删除标记记录上重插)")
}

// TestGuildLockOrder_LeaveVersusApproveElsewhere:p 退出 G 的同时,G2 批准了 p 的一条残留申请。
//
// 修复前:退帮 guild(G) → member(G,p) → 删成员行(留下 uk(p) 的删除标记)→ 经 idx_0 锁 (G2,p) 的二级项、等主键;
// 审批 guild(G2) → state(p) → app(G2,p) 主键 → 插 member(G2,p) 在 uk(p) 上查重、等退帮 —— 成环。
// 修复后两边的第一把共享锁是 state(p)(退帮:guild(G) → state(p);审批:guild(G2) → state(p)),
// 先到者做完全部写才放,另一方此前只持有自己那一帮的 guild 行,对方用不到 —— 不成环。
// 两种先后的业务结局相同:退帮成功;审批要么撞 uk 1062、要么锁不到已被删的申请,都回 NotFound。
func TestGuildLockOrder_LeaveVersusApproveElsewhere(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g, g2, g3  uint64 = 7621, 7622, 7623
		lg, l2, l3 uint64 = 8621, 8622, 8623
		p          uint64 = 8629
		zone       uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g, zone, 1, 50, lg, nil)
	seedManagedGuild(t, seedCtx, db, g2, zone, 1, 50, l2, nil)
	seedManagedGuild(t, seedCtx, db, g3, zone, 1, 50, l3, nil)

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id IN (?, ?, ?) AND player_id=?", g, g2, g3, p)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", p)
		seedMemberRow(t, ctx, db, g, p, constants.RoleMember)
		// 直接插行:模拟"申请与他帮审批并发"留下的残留(p 已是 G 的成员)。
		seedApplicationRow(t, ctx, db, g2, p, now, now+testApplicationTTLMs)
		seedApplicationRow(t, ctx, db, g3, p, now, now+testApplicationTTLMs)

		errs := runConcurrently(
			func() error {
				_, err := repo.LeaveGuild(ctx, g, p, now)
				return err
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g2, l2, p, true, zone, now)
				return err
			},
		)

		require.NoError(t, errs[0], "round %d:退帮", round)
		require.Error(t, errs[1], "round %d:残留申请不能被批成第二个帮的成员", round)
		assertErrorIn(t, errs[1], ErrApplicationNotFound)
		assert.Zero(t, memberCount(t, ctx, db, p), "round %d:p 已不在任何帮", round)
		assert.Zero(t, playerApplicationCount(t, ctx, db, p), "round %d:I3 / 1062 分支都删干净", round)
	}
	deadlocks.assertNone(t, "LeaveGuild(G,p) ‖ Review(G2,p,通过)")
}

// TestGuildLockOrder_SameNameCreateThreeWay:审计 #6 的原样交错。
//
// 编排:占锁事务先持有 state(p1)(模拟 p1 正被别帮审批通过)→ 放 T1 = CreateGuild(名, p1)(p1 已入帮)并等它卡住 →
// 放 T2 / T3 = CreateGuild(同名, p2 / p3)→ 短暂等待 → 松开占锁事务。
// 修复前 T1 已插了 guild(uk 'x' 项由它隐式持有),T2 / T3 在 'x' 上排队等 S;T1 插成员撞 1062 回滚后两人的 S 同时授予,
// 再插入时互等 → 1213。修复后 T1 卡在 state(p1) 时还没碰 uk_guild,T2 / T3 不等它:先插 guild 的一方建帮成功,另一方排在 uk 项上,
// 对方提交后撞 uk_guild 回 ErrGuildNameTaken;T1 松开后撞成员 1062 回滚。
// 上一轮留下的 uk_guild_member 删除标记项可能让 T2 / T3 的成员插入在 MySQL 上互挡,由重试吸收,所以不看钩子,只看业务结局。
// 判据是确定的:没有 1205 —— 占锁事务至多持有 ~400ms(等锁等待至多 300ms + lockOrderSettle),单次锁等待都短于
// innodb_lock_wait_timeout(1s);真出现 ErrWriteConflict 说明编排超时,要看日志查原因,不能放过。
func TestGuildLockOrder_SameNameCreateThreeWay(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	const (
		home   uint64 = 7631 // p1 已经所在的帮
		leader uint64 = 8631
		p1     uint64 = 8632
		p2     uint64 = 8633
		p3     uint64 = 8634
		zone   uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, home, zone, 1, 50, leader, map[uint64]uint32{p1: constants.RoleMember})
	// 状态行先建好:占锁事务要锁它,CreateGuild 的事务外建行看到它已存在就不再碰(不在它上面取锁)。
	for _, playerID := range []uint64{p1, p2, p3} {
		mustExec(t, seedCtx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}

	for round := 0; round < lockOrderStagedRounds; round++ {
		now := testNowMs + uint64(round)
		name := fmt.Sprintf("lock-order-race-%d", round)
		base := 7640 + uint64(round)*3
		gA, gB, gC := base, base+1, base+2

		createErrs := func() [3]error {
			blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			require.NoError(t, err)
			defer blocker.Rollback() // 下面已显式回滚时这里是空操作;中途失败时兜底放锁
			var locked uint64
			require.NoError(t, blocker.QueryRowContext(ctx,
				"SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE", p1).Scan(&locked))

			t1 := lockOrderGo(func() error { return repo.CreateGuild(ctx, lockOrderFounding(gA, p1, name, zone, now)) })
			lockOrderAwaitLockWaits(ctx, db, 1, 300*time.Millisecond)
			t2 := lockOrderGo(func() error { return repo.CreateGuild(ctx, lockOrderFounding(gB, p2, name, zone, now)) })
			t3 := lockOrderGo(func() error { return repo.CreateGuild(ctx, lockOrderFounding(gC, p3, name, zone, now)) })
			time.Sleep(lockOrderSettle)
			require.NoError(t, blocker.Rollback())
			return [3]error{<-t1, <-t2, <-t3}
		}()

		// T1:p1 已入帮,只能是 ErrPlayerAlreadyInGuild。
		require.Error(t, createErrs[0], "round %d:p1 已入帮,不能再建帮", round)
		assert.ErrorIs(t, createErrs[0], ErrPlayerAlreadyInGuild, "round %d", round)
		winners := 0
		for _, err := range createErrs[1:] {
			if err == nil {
				winners++
				continue
			}
			assert.ErrorIs(t, err, ErrGuildNameTaken, "round %d:输家只能是撞名", round)
		}
		assert.Equal(t, 1, winners, "round %d:同名恰好成一个,errs=%v", round, createErrs)
		assert.Zero(t, guildRowCount(t, ctx, db, gA), "round %d:T1 不能留下帮会行", round)
		assert.Equal(t, 1, memberCount(t, ctx, db, p1), "round %d:p1 仍只在原帮", round)

		// 收尾:把赢家的帮会拆掉,p2 / p3 回到自由身。
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id IN (?, ?)", gB, gC)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id IN (?, ?)", gB, gC)
	}
}

// TestGuildLockOrder_DisbandVersusKickAndLeave:解散与踢人 / 退帮同帮对撞。
//
// 取锁序列:解散 guild(G) → state(全体成员升序) → member(全体升序,锁后即删) → 申请(合并升序) → 提前截止 → guild;
// 踢人 guild(G) → state(目标) → member(操作者、目标升序) → 申请(目标名下);退帮 guild(G) → state(本人) → member(本人) → 申请。
// 三者都先锁 guild(G),在第一把锁上就串行;补上的玩家守卫都排在 guild 之后、成员行之前,不引入新的先后。
func TestGuildLockOrder_DisbandVersusKickAndLeave(t *testing.T) {
	_, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g        uint64 = 7651
		other    uint64 = 7652 // 成员们留有残留申请的帮
		leader   uint64 = 8651
		officer  uint64 = 8652
		m1       uint64 = 8653
		m2       uint64 = 8654
		m3       uint64 = 8655
		outsider uint64 = 8656 // 向 G 提过申请的外人
		zone     uint32 = 2
	)
	seedManagedGuild(t, ctx, db, other, zone, 1, 50, 8659, nil)
	people := []uint64{leader, officer, m1, m2, m3}

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id=?", g)
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id=?", g)
		for _, playerID := range append([]uint64{outsider}, people...) {
			mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", playerID)
		}
		seedManagedGuild(t, ctx, db, g, zone, 1, 50, leader, map[uint64]uint32{
			officer: constants.RoleOfficer, m1: constants.RoleMember, m2: constants.RoleMember, m3: constants.RoleMember,
		})
		for _, playerID := range []uint64{m1, m2, m3} {
			seedApplicationRow(t, ctx, db, other, playerID, now, now+testApplicationTTLMs)
		}
		seedApplicationRow(t, ctx, db, g, outsider, now, now+testApplicationTTLMs)

		errs := runConcurrently(
			func() error {
				_, err := repo.DisbandGuild(ctx, g, leader, now)
				return err
			},
			func() error {
				_, err := repo.KickMember(ctx, g, leader, m1, now)
				return err
			},
			func() error {
				_, err := repo.LeaveGuild(ctx, g, m2, now)
				return err
			},
			func() error {
				_, err := repo.KickMember(ctx, g, officer, m3, now)
				return err
			},
		)

		require.NoError(t, errs[0], "round %d:解散", round)
		for i, err := range errs[1:] {
			if err != nil {
				// 解散先提交:帮会行已经没了。
				assertErrorIn(t, err, ErrGuildGone)
				t.Logf("round %d party %d: %v", round, i+1, err)
			}
		}
		assert.Zero(t, guildRowCount(t, ctx, db, g), "round %d", round)
		for _, playerID := range people {
			assert.Zero(t, memberCount(t, ctx, db, playerID), "round %d player %d", round, playerID)
			assert.Zero(t, playerApplicationCount(t, ctx, db, playerID), "round %d player %d:I3", round, playerID)
		}
		assert.Zero(t, guildApplicationCount(t, ctx, db, g, outsider), "round %d:本帮待审申请随解散删除", round)
	}
	deadlocks.assertNone(t, "Disband(G) ‖ Kick / Leave(G 的成员)")
}

// TestGuildLockOrder_MemberRemovalHoldsPlayerGuard 确定性地钉住契约 P4:踢人、退帮、解散都在事务内先拿
// 被删成员的 guild_player_state 行锁。占锁事务持有 state(p) 时三者都必须等满锁等待上限(1205 → ErrWriteConflict)
// 而不改任何数据;状态行预先建好,所以等的只可能是事务内的 lockPlayerState,不是事务外建行。
func TestGuildLockOrder_MemberRemovalHoldsPlayerGuard(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g      uint64 = 7661
		leader uint64 = 8661
		p      uint64 = 8662
	)
	seedManagedGuild(t, ctx, db, g, 2, 1, 50, leader, map[uint64]uint32{p: constants.RoleMember})
	for _, playerID := range []uint64{leader, p} {
		mustExec(t, ctx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}

	blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer blocker.Rollback()
	var locked uint64
	require.NoError(t, blocker.QueryRowContext(ctx,
		"SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE", p).Scan(&locked))

	calls := []struct {
		name string
		run  func() error
	}{
		{"LeaveGuild", func() error { _, err := repo.LeaveGuild(ctx, g, p, testNowMs); return err }},
		{"KickMember", func() error { _, err := repo.KickMember(ctx, g, leader, p, testNowMs); return err }},
		{"DisbandGuild", func() error { _, err := repo.DisbandGuild(ctx, g, leader, testNowMs); return err }},
	}
	for _, call := range calls {
		start := time.Now()
		err := call.run()
		elapsed := time.Since(start)
		assert.ErrorIs(t, err, ErrWriteConflict, "%s 必须在 p 的状态行上等锁(契约 P4)", call.name)
		assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond, "%s 没有等满锁等待上限:它没拿玩家守卫", call.name)
		assert.Equal(t, 1, memberCount(t, ctx, db, p), "%s 被挡住时不能改任何数据", call.name)
		assert.Equal(t, 1, guildRowCount(t, ctx, db, g))
	}

	require.NoError(t, blocker.Rollback())
	_, err = repo.LeaveGuild(ctx, g, p, testNowMs)
	require.NoError(t, err, "占锁事务结束后同一操作必须成功")
	assert.Zero(t, memberCount(t, ctx, db, p))
}

// TestGuildLockOrder_EnsureStateRowsDoesNotWaitOnHeldRows:事务外建状态行先普通读、只插缺的行。
// 已存在的行即使正被别的事务 FOR UPDATE(典型:解散持有全体成员的状态行到提交),建行这一步也不等它;
// 缺的行照常建出来。对已存在的行直接 INSERT IGNORE 会取 S 锁,在这里等满 1s 回 ErrWriteConflict ——
// 判据就是 NoError 本身,不另加墙钟上界(慢库 / CI 上会误红,且与 1205 的判据重复)。
func TestGuildLockOrder_EnsureStateRowsDoesNotWaitOnHeldRows(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		held    uint64 = 8671
		missing uint64 = 8672
	)
	mustExec(t, ctx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", held, testNowMs)

	blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer blocker.Rollback()
	var locked uint64
	require.NoError(t, blocker.QueryRowContext(ctx,
		"SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE", held).Scan(&locked))

	require.NoError(t, repo.ensurePlayerStateRows(ctx, opApply, testNowMs, missing, held, missing),
		"已存在且被锁住的行不该让建行等待(等了就是 1205 → ErrWriteConflict)")

	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_player_state WHERE player_id IN (?, ?)", held, missing).Scan(&n))
	assert.Equal(t, 2, n, "缺的行必须建出来,重复的 id 只建一次")
}

// ── 2026-09-21 第二轮复核补的回归 ────────────────────────────────

// TestGuildLockOrder_ApplyPurgeOwnExpiredVersusExpiredReview:审计 #2 (d)。
//
// p 在 G4 有一条过期申请;p 向 G1 申请(事务内惰性删本人过期行)的同时,G4 的帮主审批这条过期申请(过期分支删行)。
// 修复前:申请 guild(G1) → state(p) → `DELETE … WHERE player_id = ? AND expire_ms <= ?` 经 idx_0 / idx_1 先锁二级项、
// 再等主键;审批(拒绝分支不拿 p 的状态行,守卫挡不住)guild(G4) → member(G4,l4) → app(G4,p) 主键 FOR UPDATE →
// 按主键删,delete-mark 二级项时等申请方 —— 反序成环。
// 修复后:申请 guild(G1) → state(p) → 普通读候选 → app(G4,p) 主键点锁 → 点删(复核 expire_ms)→ app(G1,p);
// 审批 guild(G4) → member(G4,l4) → app(G4,p) → 删它自己的二级项。两方只共享 (G4,p) 一行,且都是"先主键后二级"。
func TestGuildLockOrder_ApplyPurgeOwnExpiredVersusExpiredReview(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g1, g4 uint64 = 7681, 7684
		l1, l4 uint64 = 8681, 8684
		p      uint64 = 8689
		zone   uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g1, zone, 1, 50, l1, nil)
	seedManagedGuild(t, seedCtx, db, g4, zone, 1, 50, l4, nil)

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", p)
		seedApplicationRow(t, ctx, db, g4, p, now-testApplicationTTLMs, now-1)

		errs := runConcurrently(
			func() error {
				res, err := repo.ApplyToGuild(ctx, g1, p, zone, now, testRules)
				if err == nil && !res.Inserted {
					return fmt.Errorf("round %d: apply should insert a new application", round)
				}
				return err
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g4, l4, p, false, 0, now)
				return err
			},
		)

		require.NoError(t, errs[0], "round %d:申请", round)
		require.Error(t, errs[1], "round %d:过期申请不能被审批成功", round)
		// 审批先删:过期分支删行后回 NotFound;申请先删:审批锁读不到行,同样 NotFound。
		assertErrorIn(t, errs[1], ErrApplicationNotFound)
		assert.Zero(t, guildApplicationCount(t, ctx, db, g4, p), "round %d:过期行被删", round)
		assert.Equal(t, 1, guildApplicationCount(t, ctx, db, g1, p), "round %d:新申请落地", round)
	}
	deadlocks.assertNone(t, "Apply(p→G1) 删本人过期行 (G4,p) ‖ Review(G4,p) 过期分支")
}

// TestGuildLockOrder_CreateVersusRejectAndCancelElsewhere:审计 #2 (c)。
//
// p 建帮(I2 删他名下全部申请)的同时,G2 拒绝他的申请、他自己撤回对 G3 的申请。
// 修复前:建帮 `DELETE … WHERE player_id = ?` 经 idx_0 先锁 (p,G2) / (p,G3) 的二级项、再等主键;拒绝 / 撤回先按主键
// 锁 (G2,p) / (G3,p)、再 delete-mark 同一条二级项 —— 反序成环(拒绝与撤回都不拿 p 的状态行)。
// 修复后:建帮 state(p) → 插 member(new,p) → 普通读候选 → (G2,p)、(G3,p) 主键升序点删 → 插 guild;
// 拒绝 guild(G2) → member(G2,l2) → app(G2,p) → 删;撤回 app(G3,p) → 删(RC 短事务)。任意两方至多共享一行申请,
// 且都是"先主键后二级"。
func TestGuildLockOrder_CreateVersusRejectAndCancelElsewhere(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g2, g3 uint64 = 7692, 7693
		l2, l3 uint64 = 8692, 8693
		p      uint64 = 8699
		zone   uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g2, zone, 1, 50, l2, nil)
	seedManagedGuild(t, seedCtx, db, g3, zone, 1, 50, l3, nil)

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		founded := 7700 + uint64(round) // 每轮一个新帮会 id 与新帮名
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", p)
		seedApplicationRow(t, ctx, db, g2, p, now, now+testApplicationTTLMs)
		seedApplicationRow(t, ctx, db, g3, p, now, now+testApplicationTTLMs)

		errs := runConcurrently(
			func() error {
				return repo.CreateGuild(ctx, lockOrderFounding(founded, p, fmt.Sprintf("lock-order-create-%02d", round), zone, now))
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g2, l2, p, false, 0, now)
				return err
			},
			func() error { return repo.CancelApplication(ctx, g3, p, now) },
		)

		require.NoError(t, errs[0], "round %d:建帮", round)
		for i, err := range errs[1:] {
			if err != nil {
				// 建帮先删掉了那一行:拒绝 / 撤回看到的是"这条已经不在了"。
				assertErrorIn(t, err, ErrApplicationNotFound)
				t.Logf("round %d party %d: %v", round, i+1, err)
			}
		}
		assert.Zero(t, playerApplicationCount(t, ctx, db, p), "round %d:I2 清干净", round)

		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE player_id=?", p)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id=?", founded)
	}
	deadlocks.assertNone(t, "CreateGuild(p) ‖ Review(G2,p,拒绝) ‖ Cancel(G3,p)")
}

// TestGuildLockOrder_MemberRemovalVersusRejectAndCancelElsewhere:审计 #2 主形态的删除方。
//
// G 里一人退帮、一人被踢,两人在 G2 / G3 各有残留申请;同时 G2 拒绝、本人撤回 G3。
// 修复前:退帮 / 被踢 `DELETE … WHERE player_id = ?` 经 idx_0 先二级后主键,与拒绝 / 撤回的"先主键后二级"反序。
// 修复后:退帮 guild(G) → state(p) → member(G,p) 删 → (G2,p)、(G3,p) 主键升序点删 → 提前截止;
// 被踢 guild(G) → state(q) → member(G,lg)、member(G,q) 升序 → q 名下申请主键升序点删 → 提前截止;
// 拒绝 guild(G2) → member(G2,l2) → app(G2,x) → 删;撤回 app(G3,x) → 删。申请行上所有写者都是"先主键后二级",
// 且拒绝 / 撤回各自只碰一行。
func TestGuildLockOrder_MemberRemovalVersusRejectAndCancelElsewhere(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g, g2, g3  uint64 = 7771, 7772, 7773
		lg, l2, l3 uint64 = 8771, 8772, 8773
		leaver     uint64 = 8778
		kicked     uint64 = 8779
		zone       uint32 = 2
	)
	seedManagedGuild(t, seedCtx, db, g, zone, 1, 50, lg, nil)
	seedManagedGuild(t, seedCtx, db, g2, zone, 1, 50, l2, nil)
	seedManagedGuild(t, seedCtx, db, g3, zone, 1, 50, l3, nil)
	people := []uint64{leaver, kicked}

	for round := 0; round < lockOrderRounds; round++ {
		now := testNowMs + uint64(round)
		for _, playerID := range people {
			mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id=? AND player_id=?", g, playerID)
			mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", playerID)
			seedMemberRow(t, ctx, db, g, playerID, constants.RoleMember)
			// 直接插行:模拟"申请与他帮审批并发"留下的残留。
			seedApplicationRow(t, ctx, db, g2, playerID, now, now+testApplicationTTLMs)
			seedApplicationRow(t, ctx, db, g3, playerID, now, now+testApplicationTTLMs)
		}

		errs := runConcurrently(
			func() error {
				_, err := repo.LeaveGuild(ctx, g, leaver, now)
				return err
			},
			func() error {
				_, err := repo.KickMember(ctx, g, lg, kicked, now)
				return err
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g2, l2, leaver, false, 0, now)
				return err
			},
			func() error { return repo.CancelApplication(ctx, g3, leaver, now) },
			func() error {
				_, err := repo.ReviewApplication(ctx, g2, l2, kicked, false, 0, now)
				return err
			},
			func() error { return repo.CancelApplication(ctx, g3, kicked, now) },
		)

		require.NoError(t, errs[0], "round %d:退帮", round)
		require.NoError(t, errs[1], "round %d:踢人", round)
		for i, err := range errs[2:] {
			if err != nil {
				assertErrorIn(t, err, ErrApplicationNotFound)
				t.Logf("round %d party %d: %v", round, i+2, err)
			}
		}
		for _, playerID := range people {
			assert.Zero(t, memberCount(t, ctx, db, playerID), "round %d player %d", round, playerID)
			assert.Zero(t, playerApplicationCount(t, ctx, db, playerID), "round %d player %d:I3", round, playerID)
		}
	}
	deadlocks.assertNone(t, "Leave / Kick(G,p) 删 p 名下申请 ‖ Review(G2,p,拒绝) ‖ Cancel(G3,p)")
}

// lockOrderStmt 是占锁事务里执行的一条语句。
type lockOrderStmt struct {
	query string
	args  []any
}

// lockOrderSeedNamedGuild 直接插一行指定名字的帮会(只有 guild 行,不带成员):G-OUT1 的用例只需要它在 uk_guild 上的那一项。
// 名字必须已是规范形(GuildNameNorm 不改它),这样 name_norm 与生产写入的值逐字一致。
func lockOrderSeedNamedGuild(t *testing.T, ctx context.Context, db *sql.DB, guildID, leaderID uint64, name string, zone uint32) {
	t.Helper()
	normalized, ok := GuildNameNorm(name)
	require.True(t, ok, name)
	require.Equal(t, name, normalized, "夹具名字必须已是规范形")
	mustExec(t, ctx, db,
		`INSERT INTO guild (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, 0, 0)`,
		guildID, name, name, leaderID, constants.DefaultInitLevel, testNowMs, 30, zone)
}

// lockOrderStageUniqueInserters 把两个查重插入者的重复检查对齐到同一时刻(G-OUT1 三组用例共用):
// 占锁事务先执行 holds(删掉 uk 上的目标行,不提交)→ 同时放行两路插入者 → 等到至少一方在库里锁等待(或等满上限)→
// 再给另一方 lockOrderSettle 排进来 → 提交占锁事务。
// 两路都排在占锁事务留下的删除标记项上,提交瞬间同时拿到 S next-key,随即互挡插入意向,InnoDB 牺牲一路,inTx 重跑。
// 为什么不会出 1205:占锁事务至多持有 ~400ms(至多 300ms 的等待 + lockOrderSettle),
// 单次锁等待都不超过"占锁时长 + 对方剩余的几条主键语句",远小于 innodb_lock_wait_timeout(1s)。
func lockOrderStageUniqueInserters(t *testing.T, ctx context.Context, db *sql.DB, holds []lockOrderStmt, first, second func() error) [2]error {
	t.Helper()
	blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer blocker.Rollback() // 下面已提交时是空操作;中途失败时兜底放锁
	for _, stmt := range holds {
		_, err := blocker.ExecContext(ctx, stmt.query, stmt.args...)
		require.NoError(t, err, stmt.query)
	}

	a := lockOrderGo(first)
	b := lockOrderGo(second)
	lockOrderAwaitLockWaits(ctx, db, 1, 300*time.Millisecond)
	time.Sleep(lockOrderSettle)
	require.NoError(t, blocker.Commit())
	return [2]error{<-a, <-b}
}

// TestGuildLockOrder_UniqueInsertersSameNameAfterDisband:G-OUT1 同名形态。
//
// 帮 'x'(gOld)刚被删、uk_guild 上的删除标记项还在;两人同时以 'x' 建帮,新帮号一个比 gOld 小、一个比 gOld 大
// (新项分别落在删除标记项前后两段间隙 —— 两种位置都会被对方的 S next-key 挡住)。
// 两方的取锁序列相同:state(p) → 插 member(new,p) → 删 p 名下申请 → 插 guild:uk_guild 查重对 dm('x',gOld)
// 及其后继加 S next-key → 插入意向被对方的 S next-key 挡住,1213。两方不共享任何玩家,状态行守卫管不到。
// InnoDB 牺牲一方,inTx 重跑:另一方已建成,被牺牲的一方重跑时撞 uk 回 ErrGuildNameTaken。
// 判据:恰好一方成功、另一方只能是撞名(没有忙错误的容忍分支)。
func TestGuildLockOrder_UniqueInsertersSameNameAfterDisband(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	const (
		p1   uint64 = 8701
		p2   uint64 = 8702
		zone uint32 = 2
	)
	for _, playerID := range []uint64{p1, p2} {
		mustExec(t, seedCtx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}

	for round := 0; round < lockOrderStagedRounds; round++ {
		now := testNowMs + uint64(round)
		gOld := 7710 + uint64(round)*10
		name := fmt.Sprintf("lock-order-same-%02d", round)
		lockOrderSeedNamedGuild(t, ctx, db, gOld, 8709, name, zone)

		errs := lockOrderStageUniqueInserters(t, ctx, db,
			[]lockOrderStmt{{"DELETE FROM guild WHERE guild_id=?", []any{gOld}}},
			func() error { return repo.CreateGuild(ctx, lockOrderFounding(gOld-1, p1, name, zone, now)) },
			func() error { return repo.CreateGuild(ctx, lockOrderFounding(gOld+1, p2, name, zone, now)) },
		)

		winners := 0
		for _, err := range errs {
			if err == nil {
				winners++
				continue
			}
			assert.ErrorIs(t, err, ErrGuildNameTaken, "round %d:输家只能是撞名", round)
		}
		assert.Equal(t, 1, winners, "round %d:同名恰好成一个,errs=%v", round, errs)
		assert.Equal(t, 1, guildRowCount(t, ctx, db, gOld-1)+guildRowCount(t, ctx, db, gOld+1), "round %d:恰好一行帮会", round)

		// 收尾:拆掉赢家,p1 / p2 回到自由身。留下的 uk_guild_member 删除标记项会让下一轮的成员插入也做一次查重,
		// 正好把成员形态一并压进来(p1 / p2 在 uk 上相邻)。
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE player_id IN (?, ?)", p1, p2)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id IN (?, ?)", gOld-1, gOld+1)
	}
}

// TestGuildLockOrder_UniqueInsertersAdjacentNamesAfterDisband:G-OUT1 不同名、uk_guild 上相邻的形态。
//
// 'a' < 'b' 两帮(gA、gB)都刚被删,删除标记项在 uk_guild 上相邻、中间没有别的项。一人按新号 gA+1(> gA)建 'a',
// 另一人按新号 gB-1(< gB)建 'b':两个新项都落在 dm('a',gA) 与 dm('b',gB) 之间。
// 建 'a' 查重对 dm('a') 及后继 dm('b') 加 S next-key;建 'b' 查重对 dm('b') 及其后继加 S next-key;
// 两方的插入意向都挂在 dm('b') 前的间隙上,互被对方挡住 —— 1213。名字不同,按名字分片的互斥也挡不住它。
// InnoDB 牺牲一方,inTx 重跑,业务结局确定:两帮都建成。判据:两路都返回 nil。
func TestGuildLockOrder_UniqueInsertersAdjacentNamesAfterDisband(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	const (
		p1   uint64 = 8721
		p2   uint64 = 8722
		zone uint32 = 2
	)
	for _, playerID := range []uint64{p1, p2} {
		mustExec(t, seedCtx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}

	for round := 0; round < lockOrderStagedRounds; round++ {
		now := testNowMs + uint64(round)
		gA := 7810 + uint64(round)*10
		gB := gA + 5
		// 同一轮的两个名字之间没有任何别的名字(前缀相同、只差最后一个字母),保证在 uk_guild 上相邻。
		nameA := fmt.Sprintf("lock-order-adj-%02d-a", round)
		nameB := fmt.Sprintf("lock-order-adj-%02d-b", round)
		lockOrderSeedNamedGuild(t, ctx, db, gA, 8729, nameA, zone)
		lockOrderSeedNamedGuild(t, ctx, db, gB, 8728, nameB, zone)

		errs := lockOrderStageUniqueInserters(t, ctx, db,
			[]lockOrderStmt{
				{"DELETE FROM guild WHERE guild_id=?", []any{gA}},
				{"DELETE FROM guild WHERE guild_id=?", []any{gB}},
			},
			func() error { return repo.CreateGuild(ctx, lockOrderFounding(gA+1, p1, nameA, zone, now)) },
			func() error { return repo.CreateGuild(ctx, lockOrderFounding(gB-1, p2, nameB, zone, now)) },
		)

		require.NoError(t, errs[0], "round %d:建 'a'", round)
		require.NoError(t, errs[1], "round %d:建 'b'", round)
		assert.Equal(t, 1, guildRowCount(t, ctx, db, gA+1), "round %d:'a' 建成", round)
		assert.Equal(t, 1, guildRowCount(t, ctx, db, gB-1), "round %d:'b' 建成", round)

		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE player_id IN (?, ?)", p1, p2)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id IN (?, ?)", gA+1, gB-1)
	}
}

// TestGuildLockOrder_UniqueInsertersAdjacentMembers:G-OUT1 成员形态(建帮 ‖ 审批通过)。
//
// p1 < p2 在 uk_guild_member 上相邻、同属 gOld;占锁事务删掉两人的成员行不提交。随后 p1 建帮(新帮号 gHi > gOld)、
// gLo(< gOld)批准 p2 —— 两人的新 uk 项 (p1,gHi)、(p2,gLo) 都落在 dm(p1,gOld) 与 dm(p2,gOld) 之间。
// 建帮 state(p1) → 插 member(gHi,p1):查重对 dm(p1) 及后继 dm(p2) 加 S next-key;审批 guild(gLo) → state(p2) →
// member(gLo,审批人) → app(gLo,p2) → 插 member(gLo,p2):查重对 dm(p2) 及其后继加 S next-key;两方的插入意向都挂在
// dm(p2) 前的间隙上,互被对方挡住 —— 1213。两方不共享任何玩家或帮会,状态行守卫管不到。
// InnoDB 牺牲一方,inTx 重跑,业务结局确定:两人各自入帮。判据:两路都返回 nil。
func TestGuildLockOrder_UniqueInsertersAdjacentMembers(t *testing.T) {
	seedCtx, db, repo := openGuildIntegrationRepo(t)
	ctx := lockOrderContext(t)
	const (
		p1   uint64 = 8750
		p2   uint64 = 8751 // 与 p1 在 uk 上相邻:两人之间没有别的 player_id(各轮的帮主都取 86xx)
		zone uint32 = 2
	)
	for _, playerID := range []uint64{p1, p2} {
		mustExec(t, seedCtx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}

	for round := 0; round < lockOrderStagedRounds; round++ {
		now := testNowMs + uint64(round)
		base := 7900 + uint64(round)*10
		gLo, gOld, gHi := base+1, base+5, base+9
		// 一人只能在一个帮:每轮换一对帮主。
		leaderLo, leaderOld := 8600+uint64(round)*2, 8601+uint64(round)*2
		seedManagedGuild(t, ctx, db, gLo, zone, 1, 50, leaderLo, nil)
		seedManagedGuild(t, ctx, db, gOld, zone, 1, 50, leaderOld, map[uint64]uint32{
			p1: constants.RoleMember, p2: constants.RoleMember,
		})
		seedApplicationRow(t, ctx, db, gLo, p2, now, now+testApplicationTTLMs)

		errs := lockOrderStageUniqueInserters(t, ctx, db,
			[]lockOrderStmt{
				{"DELETE FROM guild_member WHERE guild_id=? AND player_id=?", []any{gOld, p1}},
				{"DELETE FROM guild_member WHERE guild_id=? AND player_id=?", []any{gOld, p2}},
			},
			func() error {
				return repo.CreateGuild(ctx, lockOrderFounding(gHi, p1, fmt.Sprintf("lock-order-member-%02d", round), zone, now))
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, gLo, leaderLo, p2, true, zone, now)
				return err
			},
		)

		require.NoError(t, errs[0], "round %d:p1 建帮", round)
		require.NoError(t, errs[1], "round %d:gLo 批准 p2", round)
		role, found := memberRoleOf(t, ctx, db, gHi, p1)
		assert.True(t, found, "round %d:p1 应已是新帮帮主", round)
		assert.Equal(t, constants.RoleLeader, role)
		_, found = memberRoleOf(t, ctx, db, gLo, p2)
		assert.True(t, found, "round %d:p2 应已入 gLo", round)
		assert.Zero(t, playerApplicationCount(t, ctx, db, p2), "round %d:I2 清干净", round)

		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE player_id IN (?, ?)", p1, p2)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", p2)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id=?", gHi)
	}
}

// TestGuildLockOrder_EnsureStateRowFirstInserterRollback:建状态行的 G-C3 同形。
//
// 首插者在事务里插了 state(p) 未提交;两个请求 B、C 同时补建同一行(前置普通读看不见未提交的行,都要建);首插者回滚。
// B、C 都排在首插者的记录上等 S,回滚后两者的 S 锁在 RC 下被继承为后继记录上的间隙 S,插入意向互相挡住,InnoDB 牺牲其一(1213)。
// 建行的短事务走 inTx,被牺牲的一方重跑,语句幂等。
// 判据:首插者回滚后 B、C 都成功,表里恰好一行。
// 不会 1205:首插者至多持有 500ms;B、C 的单次锁等待都小于 1s。
func TestGuildLockOrder_EnsureStateRowFirstInserterRollback(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const player uint64 = 8674

	first, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer first.Rollback() // 下面已显式回滚时是空操作
	_, err = first.ExecContext(ctx, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", player, testNowMs)
	require.NoError(t, err)

	b := lockOrderGo(func() error { return repo.ensurePlayerStateRows(ctx, opApply, testNowMs, player) })
	c := lockOrderGo(func() error { return repo.ensurePlayerStateRows(ctx, opApply, testNowMs, player) })
	// 两个锁等待:两个补建者都在首插者的记录上。锁等待上限是 1s:最多等到 500ms 就回滚首插者。
	lockOrderAwaitLockWaits(ctx, db, 2, 500*time.Millisecond)
	require.NoError(t, first.Rollback())

	assert.NoError(t, <-b, "首插者回滚后补建者 B 必须成功")
	assert.NoError(t, <-c, "首插者回滚后补建者 C 必须成功")
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_player_state WHERE player_id=?", player).Scan(&n))
	assert.Equal(t, 1, n, "两个补建者合起来恰好建出一行")
}

// TestGuildLockOrder_DisbandVersusApproveUniqueSuccessor:完整性复核 G2 —— "解散时删成员行前移到删申请之前"所修之环的确定性回归。
//
// 形状:p 刚离开 gOld(uk_guild_member 上留着删除标记项 (p,gOld)),q = p+1 是 G 的成员,于是 uk 上 p 之后第一条记录就是 (q,G);
// p 在 G 与 G2 各有一条申请。G 解散 ‖ G2 批准 p。
//   - 修复前的解散:guild(G) → state(全员) → member(全员)锁 → **删申请**(持有 (G,p))→ 提前截止 → **删成员**(delete-mark
//     (q,G) 要 X);审批通过:guild(G2) → state(p) → member(G2,l2) → app(G2,p) → 插 member(G2,p):查重对 (p,gOld) 及其
//     后继 (q,G) 加 S → 删 p 名下申请时 (G,p) 被解散持有。解散等审批方在 (q,G) 上的 S,审批方等解散持有的 (G,p) —— 1213。
//     只有语句顺序在防这个环。
//   - 修复后的解散先删成员行(持 (q,G) 的 X)再删申请:审批方查重时在 (q,G) 上等解散,手里没有解散要的任何锁 —— 单向等待。
//
// 编排(确定性):
//  1. 一个 RR 事务先做一次一致性读建出读视图,挡住 purge —— 否则 (p,gOld) 的删除标记项随时可能被清掉,查重根本不扫 (q,G);
//  2. 删掉 member(gOld,p)(自动提交);
//  3. 占锁事务点锁 q 在 G 的一笔未决捐献的 op 行:解散会停在"提前截止"(修复前:申请已删、成员未删;修复后:两者都已删);
//  4. 放行解散,等它在 op 行上锁等待;再放行审批通过,等第二个锁等待(修复后在 (q,G) 上,修复前在 (G,p) 上);
//  5. 提交占锁事务:修复前解散随即去 delete-mark (q,G)、撞上审批方的 S → 1213;修复后解散提交、审批方接着做完。
//
// 判据:钩子严格为零;两边都成功;G 已不在、q 无帮、p 入了 G2、p 名下申请清干净。
// 不会 1205:占锁事务至多持有约 700ms(两次至多 300ms 的等待 + lockOrderSettle),小于 innodb_lock_wait_timeout(1s)。
func TestGuildLockOrder_DisbandVersusApproveUniqueSuccessor(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		g    uint64 = 7671 // 解散的帮
		g2   uint64 = 7672 // 批准 p 的帮
		gOld uint64 = 7673 // p 刚离开的帮
		lg   uint64 = 8671
		l2   uint64 = 8672
		lOld uint64 = 8673
		p    uint64 = 8790
		q    uint64 = 8791 // uk_guild_member 上紧挨在 p 之后:两人之间没有别的 player_id,各帮主都小于 p
		zone uint32 = 2
		opID uint64 = 9_671_001
	)
	seedManagedGuild(t, ctx, db, g, zone, 1, 50, lg, map[uint64]uint32{q: constants.RoleMember})
	seedManagedGuild(t, ctx, db, g2, zone, 1, 50, l2, nil)
	seedManagedGuild(t, ctx, db, gOld, zone, 1, 50, lOld, map[uint64]uint32{p: constants.RoleMember})
	// 状态行先建好:两边事务外建行都只做普通读,编排里的锁等待只来自事务内。
	for _, playerID := range []uint64{lg, q, p} {
		mustExec(t, ctx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}
	seedApplicationRow(t, ctx, db, g, p, testNowMs, testNowMs+testApplicationTTLMs)
	seedApplicationRow(t, ctx, db, g2, p, testNowMs, testNowMs+testApplicationTTLMs)
	// q 在 G 的一笔未决捐献(截止在 testNowMs 之后):解散的提前截止要点改它。
	mustExec(t, ctx, db, sqlInsertAssetOp,
		assetOpInsertArgs(econDonateRecord(opID, q, g, testNowMs, 1, econDonatePayload(t, 100)))...)

	reader, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer reader.Rollback()
	var seen int
	require.NoError(t, reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_member").Scan(&seen), "建读视图")
	mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id=? AND player_id=?", gOld, p)

	blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer blocker.Rollback() // 下面已提交时是空操作
	var locked uint64
	require.NoError(t, blocker.QueryRowContext(ctx, sqlLockAssetOp, opID).Scan(&locked))

	disband := lockOrderGo(func() error { _, err := repo.DisbandGuild(ctx, g, lg, testNowMs); return err })
	lockOrderAwaitLockWaits(ctx, db, 1, 300*time.Millisecond)
	approve := lockOrderGo(func() error {
		_, err := repo.ReviewApplication(ctx, g2, l2, p, true, zone, testNowMs)
		return err
	})
	lockOrderAwaitLockWaits(ctx, db, 2, 300*time.Millisecond)
	time.Sleep(lockOrderSettle)
	require.NoError(t, blocker.Commit())

	require.NoError(t, <-disband, "解散")
	require.NoError(t, <-approve, "G2 批准 p")
	require.NoError(t, reader.Rollback())

	assert.Zero(t, guildRowCount(t, ctx, db, g), "G 已解散")
	assert.Zero(t, memberCount(t, ctx, db, q), "q 随解散离帮")
	role, found := memberRoleOf(t, ctx, db, g2, p)
	require.True(t, found, "p 应已入 G2")
	assert.Equal(t, constants.RoleMember, role)
	assert.Zero(t, playerApplicationCount(t, ctx, db, p), "I2 / I3:p 名下申请清干净")
	deadlocks.assertNone(t, "Disband(G) ‖ Review(G2,p,通过),uk 上 p 的后继是 G 的成员")
}

// TestGuildLockOrder_UniqueInsertersDoNotQueueGlobally:撤掉全局插入守卫的判据(guild_manage_repo.go 文件头 (d))。
//
// 占锁事务持有一名玩家的状态行,他的建帮卡在那一行上;与此同时别的玩家建帮、审批通过、首次建状态行、新玩家申请都必须立即成功。
// 守卫还在的话卡住的建帮持着它,这几条都会等满 1s 锁等待回 ErrWriteConflict。
func TestGuildLockOrder_UniqueInsertersDoNotQueueGlobally(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g         uint64 = 7961
		gStuck    uint64 = 7962
		gOther    uint64 = 7964
		leader    uint64 = 8961
		stuck     uint64 = 8962 // 建帮卡在自己的状态行上
		other     uint64 = 8963
		applicant uint64 = 8964
		fresh     uint64 = 8965 // 没有状态行
		newcomer  uint64 = 8966 // 没有状态行,直接申请
		zone      uint32 = 2
	)
	seedManagedGuild(t, ctx, db, g, zone, 1, 50, leader, nil)
	for _, playerID := range []uint64{stuck, other, applicant} {
		mustExec(t, ctx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", playerID, testNowMs)
	}
	seedApplicationRow(t, ctx, db, g, applicant, testNowMs, testNowMs+testApplicationTTLMs)

	blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer blocker.Rollback() // 下面已显式回滚时是空操作
	var locked uint64
	require.NoError(t, blocker.QueryRowContext(ctx, sqlLockPlayerState, stuck).Scan(&locked))
	stuckDone := lockOrderGo(func() error {
		return repo.CreateGuild(ctx, lockOrderFounding(gStuck, stuck, "no-queue-stuck", zone, testNowMs))
	})
	lockOrderAwaitLockWaits(ctx, db, 1, 300*time.Millisecond)

	require.NoError(t, repo.CreateGuild(ctx, lockOrderFounding(gOther, other, "no-queue-other", zone, testNowMs)), "别人的建帮不排队")
	_, err = repo.ReviewApplication(ctx, g, leader, applicant, true, zone, testNowMs)
	require.NoError(t, err, "审批通过不排队")
	require.NoError(t, repo.ensurePlayerStateRows(ctx, opApply, testNowMs, fresh), "首次建状态行不排队")
	_, err = repo.ApplyToGuild(ctx, g, newcomer, zone, testNowMs, testRules)
	require.NoError(t, err, "新玩家申请不排队")

	require.NoError(t, blocker.Rollback())
	// 慢机器上它可能已经等满 1s 回 ErrWriteConflict,那也是这名玩家自己的事;要紧的是上面几条没被它拖住。
	if err := <-stuckDone; err != nil {
		require.ErrorIs(t, err, ErrWriteConflict)
	}

	var sentinels int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_player_state WHERE player_id=0").Scan(&sentinels))
	assert.Zero(t, sentinels, "不再有 player_id=0 的哨兵行")
}

// TestGuildLockOrder_PrecheckAnswersWithoutStateRow(完整性复核 G4):踢一个不是本帮成员的 id、批准一条不存在的申请 ——
// 事务外普通读预判就答复,不建状态行。答复的优先级与锁内一致:帮会不在 → 操作者不在 → 目标 / 申请不在。
func TestGuildLockOrder_PrecheckAnswersWithoutStateRow(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g         uint64 = 7971
		leader    uint64 = 8971
		stranger  uint64 = 8972 // 从没出现过的玩家 id
		outsider  uint64 = 8973 // 不是本帮成员的"操作者"
		noSuchGid uint64 = 7972
		zone      uint32 = 2
	)
	seedManagedGuild(t, ctx, db, g, zone, 1, 50, leader, nil)
	_, err := repo.KickMember(ctx, g, leader, stranger, testNowMs)
	assert.ErrorIs(t, err, ErrTargetNotMember, "踢非成员:预读答复")
	_, err = repo.KickMember(ctx, g, outsider, stranger, testNowMs)
	assert.ErrorIs(t, err, ErrNotGuildMember, "操作者不在本帮的优先级高于目标不在")
	_, err = repo.KickMember(ctx, noSuchGid, leader, stranger, testNowMs)
	assert.ErrorIs(t, err, ErrGuildGone, "帮会不在的优先级最高")
	_, err = repo.ReviewApplication(ctx, g, leader, stranger, true, zone, testNowMs)
	assert.ErrorIs(t, err, ErrApplicationNotFound, "批准不存在的申请:预读答复")
	_, err = repo.ReviewApplication(ctx, g, outsider, stranger, true, zone, testNowMs)
	assert.ErrorIs(t, err, ErrNotGuildMember, "审批人不在本帮的优先级高于申请不在")
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_player_state WHERE player_id=?", stranger).Scan(&n))
	assert.Zero(t, n, "不给不存在的目标 / 申请人建状态行")
}
