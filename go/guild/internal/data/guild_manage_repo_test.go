package data

// 帮会管理与审批 repo 的测试(设计 docs/design/guild-phase2/02-management.md §21.3 与 §22)。
//
// 分两层,刻意不混:
//
//  1. **纯函数 / miniredis 层**(本文件前半):不需要 MySQL,任何环境都跑。
//     权限矩阵、错误分类、重试语义、DSN 改写、缓存失效重试、推送冷却都在这一层钉死 ——
//     它们是安全边界(fail-closed)与"不吞错"的落点,没有测试等于这些保证无人看守。
//  2. **真库层**(后半,`GUILD_TEST_MYSQL_DSN` 未设即 Skip):事务的加锁、可见性、
//     唯一索引冲突、锁等待封顶这些行为**只有真 InnoDB 才有**。用假 DB 替身写出来的
//     测试只会验到替身自己的返回值,那正是本批最需要被验的部分。
//
// 三条纪律:
//   - 测试面向导出接口(repo 的方法),不去断言私有步骤的中间态;私有函数只在
//     "它本身就是一条独立契约"时直接测(权限纯函数、错误分类、retryOnDeadlock)。
//   - 不依赖执行顺序:每个真库用例都经 openGuildIntegrationRepo 重建 schema,自带干净状态。
//   - 不依赖真实墙钟:时间一律用 testNowMs 显式传入,过期与否由入参决定,不由机器时钟决定。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
)

// ── 共用夹具 ──────────────────────────────────────────────────

// testNowMs 是所有用例的"当前时刻"。写成常量而不是 time.Now():
// 申请是否过期由 expire_ms 与传入的 now 比较决定,用真实时钟会让同一个用例
// 在慢机器上得到不同结论(AGENTS.md §11.4)。
const testNowMs uint64 = 1_700_000_000_000

// testApplicationTTLMs 与 testRules.TTLMs 同源:GuildRule.application_expire_hours=72。
const testApplicationTTLMs uint64 = 72 * 3_600_000

// testRules 刻意把 MaxPerGuild 压到 2:队列上限的用例只需要 3 个申请人就能触发,
// 不用为了凑满 50 条去造数据。
var testRules = ApplicationRules{
	TTLMs:        testApplicationTTLMs,
	MaxPerPlayer: 3,
	MaxPerGuild:  2,
}

// capOf 造一个"任何等级都返回 n 个长老位"的 OfficerCapFunc。
// 真实实现由 logic 从 GuildLevel 配表查,repo 只认这个接缝。
func capOf(n uint32) OfficerCapFunc {
	return func(uint32) (uint32, bool) { return n, true }
}

// capMissing 模拟 GuildLevel 配表缺该等级行:repo 必须 fail-closed,而不是默认放行。
func capMissing() OfficerCapFunc {
	return func(uint32) (uint32, bool) { return 0, false }
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(ctx, query, args...)
	require.NoError(t, err, query)
}

// seedManagedGuild 直接用 SQL 造一个帮会:帮主 + roles 里的成员。
//
// 刻意绕开 CreateGuild:建帮自己有一整套前置(发号、重名、I2 清申请),
// 用它来造夹具会让"被测的那一步失败"和"夹具没造出来"混在一起。
//
// 名字取 manage-guild-<id>:全小写 ASCII,GuildNameNorm 的规范化结果与原串相同,
// 因此 name_norm 可以直接写同一个值(与生产写入的值一致)。
//
// 注:本包另有一个 integration tag 下的 seedGuild(rank_zone_integration_test.go),
// 参数与语义都不同;两者同时编译,故这里另取名,不复用。
func seedManagedGuild(t *testing.T, ctx context.Context, db *sql.DB,
	guildID uint64, zone, level, maxMembers uint32, leader uint64, roles map[uint64]uint32) {
	t.Helper()
	name := fmt.Sprintf("manage-guild-%d", guildID)
	mustExec(t, ctx, db,
		`INSERT INTO guild (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, 0, 0)`,
		guildID, name, name, leader, level, testNowMs, maxMembers, zone)
	seedMemberRow(t, ctx, db, guildID, leader, constants.RoleLeader)
	for playerID, role := range roles {
		seedMemberRow(t, ctx, db, guildID, playerID, role)
	}
}

func seedMemberRow(t *testing.T, ctx context.Context, db *sql.DB, guildID, playerID uint64, role uint32) {
	t.Helper()
	mustExec(t, ctx, db,
		`INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
		 VALUES (?, ?, ?, ?, ?, 0, 0)`,
		guildID, playerID, role, testNowMs, testNowMs)
}

// seedApplicationRow 直接插一条申请行。
// 需要"已过期"或"申请人已入他帮"这类状态时必须这样造:走 ApplyToGuild 会顺带
// 惰性清理过期行,夹具刚造好就被它删掉了。
func seedApplicationRow(t *testing.T, ctx context.Context, db *sql.DB, guildID, playerID, applyMs, expireMs uint64) {
	t.Helper()
	mustExec(t, ctx, db,
		`INSERT INTO guild_application (guild_id, player_id, apply_ms, expire_ms) VALUES (?, ?, ?, ?)`,
		guildID, playerID, applyMs, expireMs)
}

func playerApplicationCount(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_application WHERE player_id=?", playerID).Scan(&n))
	return n
}

func guildApplicationCount(t *testing.T, ctx context.Context, db *sql.DB, guildID, playerID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_application WHERE guild_id=? AND player_id=?", guildID, playerID).Scan(&n))
	return n
}

func liveApplicationCount(t *testing.T, ctx context.Context, db *sql.DB, playerID, now uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_application WHERE player_id=? AND expire_ms>?", playerID, now).Scan(&n))
	return n
}

// memberRoleOf 读 MySQL 权威 role;found=false 表示没有成员行。
func memberRoleOf(t *testing.T, ctx context.Context, db *sql.DB, guildID, playerID uint64) (uint32, bool) {
	t.Helper()
	var role uint32
	err := db.QueryRowContext(ctx,
		"SELECT role FROM guild_member WHERE guild_id=? AND player_id=?", guildID, playerID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return role, true
}

func officerCount(t *testing.T, ctx context.Context, db *sql.DB, guildID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_member WHERE guild_id=? AND role=?", guildID, constants.RoleOfficer).Scan(&n))
	return n
}

func leaderIDOf(t *testing.T, ctx context.Context, db *sql.DB, guildID uint64) uint64 {
	t.Helper()
	var leaderID uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT leader_id FROM guild WHERE guild_id=?", guildID).Scan(&leaderID))
	return leaderID
}

func guildRowCount(t *testing.T, ctx context.Context, db *sql.DB, guildID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild WHERE guild_id=?", guildID).Scan(&n))
	return n
}

// guildRoles 把快照摊平成 player_id → role,便于一次断言整份成员表。
func guildRoles(guild *GuildData) map[uint64]uint32 {
	out := make(map[uint64]uint32, len(guild.Members))
	for _, m := range guild.Members {
		out[m.PlayerID] = m.Role
	}
	return out
}

// applicantIDs 摊平 ListApplicants 的结果,便于断言"含 / 不含某人"。
func applicantIDs(rows []ApplicantRow) []uint64 {
	out := make([]uint64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.PlayerID)
	}
	return out
}

// runConcurrently 同时发车跑若干个闭包,返回与入参一一对应的错误。
// 用 start 通道统一放行,让它们尽量落在同一时刻 —— 否则串行执行完也叫"并发测试",
// 但什么都没验到。每个 goroutine 只写自己那个下标,切片无需加锁。
func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func countNilErrors(errs []error) int {
	n := 0
	for _, err := range errs {
		if err == nil {
			n++
		}
	}
	return n
}

// assertErrorIn 断言 err 属于给定集合(nil 不算命中,调用方自己先挑掉成功的那一路)。
func assertErrorIn(t *testing.T, err error, allowed ...error) {
	t.Helper()
	for _, want := range allowed {
		if errors.Is(err, want) {
			return
		}
	}
	t.Errorf("错误 %v 不在允许集合 %v 内", err, allowed)
}

// newCacheOnlyRepo 造一个只接了 Redis 的 repo。
// db 传 nil 是故意的:这两个用例只该走 Redis 路径,给个真 DB 反而会把
// "不小心访问了 MySQL"这种回归掩盖成一次正常查询。
func newCacheOnlyRepo(t *testing.T) (*GuildRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewGuildRepo(rdb, nil, time.Minute), mr
}

// ── §21.3 权限纯函数(事务内调用的判定,安全边界) ─────────────

// TestCanKickMatrix 把"谁能踢谁"整张矩阵钉死。
// 两条不变量:职位必须**严格**高于对方(平级互踢会让长老内斗);
// 未知 role 编码(2 是空号)既不能踢人也不能被踢 —— 存量脏数据不该换来权限或豁免。
func TestCanKickMatrix(t *testing.T) {
	cases := []struct {
		actor, target uint32
		want          bool
	}{
		{constants.RoleLeader, constants.RoleOfficer, true},
		{constants.RoleLeader, constants.RoleMember, true},
		{constants.RoleOfficer, constants.RoleMember, true},
		{constants.RoleOfficer, constants.RoleOfficer, false},
		{constants.RoleOfficer, constants.RoleLeader, false},
		{constants.RoleMember, constants.RoleMember, false},
		{constants.RoleLeader, constants.RoleLeader, false},
		{2, constants.RoleMember, false},
		{constants.RoleLeader, 2, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, canKick(tc.actor, tc.target), "actor=%d target=%d", tc.actor, tc.target)
	}
}

// TestCanAssignTransferReview:任免与转让只有帮主能做(否则长老可以自我扩权),
// 审批长老与帮主都能做。未知编码一律为假。
func TestCanAssignTransferReview(t *testing.T) {
	for _, role := range []uint32{constants.RoleMember, constants.RoleOfficer, 2, constants.RoleLeader, 4} {
		wantLeaderOnly := role == constants.RoleLeader
		assert.Equal(t, wantLeaderOnly, canAssignRole(role), "canAssignRole role=%d", role)
		assert.Equal(t, wantLeaderOnly, canTransferLeader(role), "canTransferLeader role=%d", role)

		wantOfficerUp := role == constants.RoleOfficer || role == constants.RoleLeader
		assert.Equal(t, wantOfficerUp, canReviewApplications(role), "canReviewApplications role=%d", role)
	}
}

// TestDemotedLeaderRole:转让之后原帮主的落点。长老位有空就补进去,没空就降成成员 ——
// 契约宁可降级也不超编,超编会让后续任免的上限判定永久失真。
func TestDemotedLeaderRole(t *testing.T) {
	cases := []struct {
		officersAfterTarget, maxOfficers, want uint32
	}{
		{0, 2, constants.RoleOfficer},
		{1, 2, constants.RoleOfficer},
		{2, 2, constants.RoleMember},
		{3, 2, constants.RoleMember},
		{0, 0, constants.RoleMember},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, demotedLeaderRole(tc.officersAfterTarget, tc.maxOfficers),
			"officers=%d cap=%d", tc.officersAfterTarget, tc.maxOfficers)
	}
}

// ── §21.3 错误分类与事务重试 ─────────────────────────────────

// TestMySQLErrClassifiers:1213(死锁)可重试、1205(锁等待超时)不可重试,
// 两者语义不同,分类错了要么白重试要么把可恢复的写报成失败。
// 分类必须穿透 %w 包装 —— 本仓所有 SQL 错误都带上下文包过一层。
func TestMySQLErrClassifiers(t *testing.T) {
	deadlock := &mysqlDriver.MySQLError{Number: 1213, Message: "Deadlock found"}
	lockWait := &mysqlDriver.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	duplicate := &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry"}

	assert.True(t, isDeadlock(deadlock))
	assert.False(t, isLockWaitTimeout(deadlock))
	assert.True(t, isDeadlock(fmt.Errorf("insert member: %w", deadlock)), "必须穿透 %%w 包装")

	assert.True(t, isLockWaitTimeout(lockWait))
	assert.False(t, isDeadlock(lockWait))
	assert.True(t, isLockWaitTimeout(fmt.Errorf("lock guild 7: %w", lockWait)))

	for _, err := range []error{duplicate, errors.New("boom"), nil} {
		assert.False(t, isDeadlock(err), "%v", err)
		assert.False(t, isLockWaitTimeout(err), "%v", err)
	}
}

// TestCommitThenUnwraps:errCommitThen 是"先提交已做完的写,再把 inner 当业务结果返回"。
// 它必须能被 errors.Is 看穿,否则 logic 的哨兵映射表会把惰性清理的结果当成内部错误。
func TestCommitThenUnwraps(t *testing.T) {
	err := error(errCommitThen{inner: ErrApplicationNotFound})
	assert.True(t, errors.Is(err, ErrApplicationNotFound))
	assert.Equal(t, ErrApplicationNotFound.Error(), err.Error())
}

// TestRetryOnDeadlock 直接驱动重试骨架,不碰数据库 —— 这正是把它从 inTx 里抽出来的理由。
func TestRetryOnDeadlock(t *testing.T) {
	deadlock := func() error { return &mysqlDriver.MySQLError{Number: 1213, Message: "Deadlock found"} }
	lockWait := func() error { return &mysqlDriver.MySQLError{Number: 1205, Message: "Lock wait timeout"} }
	duplicate := &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry"}

	t.Run("前两次死锁第三次成功", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(context.Background(), opSetRole, func() error {
			calls++
			if calls <= 2 {
				return deadlock()
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
	})

	t.Run("连续死锁耗尽重试", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(context.Background(), opSetRole, func() error {
			calls++
			return deadlock()
		})
		assert.ErrorIs(t, err, ErrWriteConflict)
		assert.Equal(t, maxTxAttempts, calls)
	})

	t.Run("锁等待超时不重试", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(context.Background(), opKick, func() error {
			calls++
			return lockWait()
		})
		// 锁等待本身已被 innodb_lock_wait_timeout=1 封顶,再等一轮只会白吃同步预算。
		assert.ErrorIs(t, err, ErrWriteConflict)
		assert.Equal(t, 1, calls)
	})

	t.Run("业务错误原样返回", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(context.Background(), opApply, func() error {
			calls++
			return duplicate
		})
		assert.ErrorIs(t, err, duplicate)
		assert.Equal(t, 1, calls)
	})

	t.Run("ctx 已取消时不再退避重试", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := retryOnDeadlock(ctx, opReview, func() error {
			calls++
			return deadlock()
		})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, calls)
	})
}

// TestRetryResultsDoNotAccumulate 钉住 §6.2 的结果变量约定:
// 事务函数里累积的结果必须在闭包内声明、成功返回前才赋给外层。
// 写反了(直接 append 到外层变量)这里会看到 6 条 —— 上一轮回滚掉的数据被带进了响应。
func TestRetryResultsDoNotAccumulate(t *testing.T) {
	var outer []uint64
	calls := 0
	err := retryOnDeadlock(context.Background(), opDisband, func() error {
		var ids []uint64 // 每次尝试从零开始
		ids = append(ids, 1, 2, 3)
		calls++
		if calls == 1 {
			return &mysqlDriver.MySQLError{Number: 1213, Message: "Deadlock found"}
		}
		outer = ids
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, []uint64{1, 2, 3}, outer)
}

// ── §21.3 DSN 改写(§6.2a + 90-consistency Y-20) ──────────────

func TestWithLockWaitTimeout(t *testing.T) {
	const dsn = "u:p@tcp(127.0.0.1:3306)/mmorpg_guild?charset=utf8mb4&parseTime=true&loc=Local"

	out, err := WithLockWaitTimeout(dsn)
	require.NoError(t, err)
	cfg, err := mysqlDriver.ParseDSN(out)
	require.NoError(t, err)
	assert.Equal(t, "1", cfg.Params["innodb_lock_wait_timeout"])
	// 其余连接参数一个都不能被改掉:这条改写是正确性约束,不是"顺手重写 DSN"。
	assert.Equal(t, "mmorpg_guild", cfg.DBName)
	assert.True(t, cfg.ParseTime)
	assert.Equal(t, time.Local, cfg.Loc)
	// charset 在驱动里存在非导出字段(cfg.charsets)上,读不到,只能从格式化结果里核对。
	assert.Contains(t, out, "charset=utf8mb4")

	// 已经带了别的值时必须被覆盖 —— 否则部署里写了 50 就绕过了封顶。
	out, err = WithLockWaitTimeout("u:p@tcp(127.0.0.1:3306)/mmorpg_guild?innodb_lock_wait_timeout=50")
	require.NoError(t, err)
	cfg, err = mysqlDriver.ParseDSN(out)
	require.NoError(t, err)
	assert.Equal(t, "1", cfg.Params["innodb_lock_wait_timeout"])

	// Y-20:clientFoundRows 必须被清掉。它会把 UPDATE 的 RowsAffected 从"实际改动行数"
	// 翻成"匹配行数",本批多处用 RowsAffected==1 做写入自检,翻转后自检永远通过、写丢了也看不见。
	out, err = WithLockWaitTimeout("u:p@tcp(127.0.0.1:3306)/mmorpg_guild?clientFoundRows=true")
	require.NoError(t, err)
	cfg, err = mysqlDriver.ParseDSN(out)
	require.NoError(t, err)
	assert.False(t, cfg.ClientFoundRows)

	// 解析失败时错误文案**不含原文**:DSN 里有口令,不能进日志。
	_, err = WithLockWaitTimeout("user:secret@tcp(127.0.0.1:3306)")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
}

// ── §21.3 提交后缓存失效与推送冷却(miniredis) ────────────────

// TestInvalidateAfterCommitRetriesInBackground:MySQL 已经是真相,失效失败不许把
// 已提交的写报成失败,但也不许"静默丢掉" —— 必须有界重试,耗尽才记一次账。
//
// 断言通过可替换的钩子 invalidateGaveUp 进行,不去读 go-zero 的指标:
// 指标是输出格式,钩子才是行为契约。
func TestInvalidateAfterCommitRetriesInBackground(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	ctx := context.Background()

	var (
		gaveUpMu  sync.Mutex
		gaveUpOps []string
	)
	recordGaveUp := func(op string) {
		gaveUpMu.Lock()
		defer gaveUpMu.Unlock()
		gaveUpOps = append(gaveUpOps, op)
	}
	gaveUpSnapshot := func() []string {
		gaveUpMu.Lock()
		defer gaveUpMu.Unlock()
		return append([]string(nil), gaveUpOps...)
	}

	originalGaveUp, originalDelays := invalidateGaveUp, invalidateRetryDelays
	invalidateGaveUp = recordGaveUp
	// 生产是 100/400/1600ms。压到 20ms 三轮:既留出"恢复后第一次重试就成功"的窗口,
	// 又让"一直失败"的那一组在 60ms 内走完全部重试。
	invalidateRetryDelays = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}
	defer func() { invalidateGaveUp, invalidateRetryDelays = originalGaveUp, originalDelays }()

	// 第一组:同步失效失败,Redis 随后恢复 → 后台重试补上,不计账。
	const (
		healedGuild  uint64 = 7
		healedPlayer uint64 = 42
	)
	require.NoError(t, mr.Set(guildKey(healedGuild), "{}"))
	require.NoError(t, mr.Set(playerGuildKey(healedPlayer), "0"))
	mr.SetError("boom")
	repo.invalidateAfterCommit(ctx, opKick, healedGuild, healedPlayer) // 必须立即返回,不阻塞调用方
	mr.SetError("")                                                    // Redis 恢复:第一次后台重试就该成功

	require.Eventually(t, func() bool {
		return !mr.Exists(guildKey(healedGuild)) && !mr.Exists(playerGuildKey(healedPlayer))
	}, time.Second, 5*time.Millisecond, "后台重试必须把两个键都失效掉")
	assert.Empty(t, gaveUpSnapshot(), "重试成功就不该计账")

	// 第二组:Redis 一直坏着 → 重试耗尽,恰好计一次账,并且仍然不返回错误。
	const (
		lostGuild  uint64 = 8
		lostPlayer uint64 = 43
	)
	require.NoError(t, mr.Set(guildKey(lostGuild), "{}"))
	require.NoError(t, mr.Set(playerGuildKey(lostPlayer), "0"))
	mr.SetError("boom")
	repo.invalidateAfterCommit(ctx, opDisband, lostGuild, lostPlayer)

	require.Eventually(t, func() bool {
		return len(gaveUpSnapshot()) == 1
	}, time.Second, 5*time.Millisecond, "重试耗尽必须记一次 guild_cache_invalidate_failed_total")
	assert.Equal(t, []string{opDisband}, gaveUpSnapshot(), "计账的 op 必须是发起方的 op")
	mr.SetError("")
}

// TestApplyPushGateCooldown:同一 (帮会, 申请人) 在冷却窗口内至多推一次,
// 挡住"申请 → 撤回 → 申请"的刷屏。Redis 出错时选择**不推**:
// 推送只承诺"至多一次",少一条提示远好过抖动期间刷屏。
func TestApplyPushGateCooldown(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	ctx := context.Background()
	const (
		guildID  uint64 = 6001
		playerID uint64 = 7001
	)

	assert.True(t, repo.TryMarkApplyPush(ctx, guildID, playerID), "第一次必须拿到冷却键")
	assert.False(t, repo.TryMarkApplyPush(ctx, guildID, playerID), "窗口内第二次必须被挡")

	// 换一个申请人不受影响:冷却是 (帮会, 申请人) 维度,不是整帮维度。
	assert.True(t, repo.TryMarkApplyPush(ctx, guildID, playerID+1))

	mr.FastForward(ApplyPushCooldown + time.Second)
	assert.True(t, repo.TryMarkApplyPush(ctx, guildID, playerID), "键自然过期后必须重新放行")

	mr.SetError("boom")
	assert.False(t, repo.TryMarkApplyPush(ctx, guildID, playerID+2), "Redis 出错时不推")
	mr.SetError("")
}

// ══════════════════════════════════════════════════════════════
// §22 真库用例(GUILD_TEST_MYSQL_DSN;未设置时 openGuildIntegrationRepo 会 Skip)
// ══════════════════════════════════════════════════════════════

// ── 成员管理(§7) ───────────────────────────────────────────

// TestSetMemberRole_PromoteUntilCap:长老位按 GuildLevel 配表封顶;
// 任免为同一角色是幂等无操作(Changed=false),logic 据此不推送。
func TestSetMemberRole_PromoteUntilCap(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7101
		leader  uint64 = 8101
		m1      uint64 = 8102
		m2      uint64 = 8103
		m3      uint64 = 8104
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		m1: constants.RoleMember, m2: constants.RoleMember, m3: constants.RoleMember,
	})

	res, err := repo.SetMemberRole(ctx, guildID, leader, m1, constants.RoleOfficer, capOf(2))
	require.NoError(t, err)
	assert.True(t, res.Changed)
	require.NotNil(t, res.Guild)
	assert.Equal(t, constants.RoleOfficer, guildRoles(res.Guild)[m1], "快照必须含本次写")

	res, err = repo.SetMemberRole(ctx, guildID, leader, m2, constants.RoleOfficer, capOf(2))
	require.NoError(t, err)
	assert.True(t, res.Changed)

	_, err = repo.SetMemberRole(ctx, guildID, leader, m3, constants.RoleOfficer, capOf(2))
	assert.ErrorIs(t, err, ErrOfficerLimit)
	role, found := memberRoleOf(t, ctx, db, guildID, m3)
	require.True(t, found)
	assert.Equal(t, constants.RoleMember, role, "被上限拒绝后不能留下半写")

	// 幂等:再任命已经是长老的人 —— 成功、无变更、不写库。
	res, err = repo.SetMemberRole(ctx, guildID, leader, m1, constants.RoleOfficer, capOf(2))
	require.NoError(t, err)
	assert.False(t, res.Changed)
	require.NotNil(t, res.Guild)
	assert.Equal(t, 2, officerCount(t, ctx, db, guildID))
}

// TestSetMemberRole_OfficerCannotAssign:长老不能任免,否则长老可以自我扩权。
func TestSetMemberRole_OfficerCannotAssign(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7111
		leader  uint64 = 8111
		officer uint64 = 8112
		member  uint64 = 8113
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})

	_, err := repo.SetMemberRole(ctx, guildID, officer, member, constants.RoleOfficer, capOf(5))
	assert.ErrorIs(t, err, ErrRankTooLow)
	role, found := memberRoleOf(t, ctx, db, guildID, member)
	require.True(t, found)
	assert.Equal(t, constants.RoleMember, role)

	// 帮主的职位也不是任免接口能动的。这一刀先被"只有帮主能任免"挡下;
	// 即便发起者换成帮主,锁内还有一条"目标是帮主就拒"的防御分支 —— role=3 只能由转让产生。
	_, err = repo.SetMemberRole(ctx, guildID, officer, leader, constants.RoleMember, capOf(5))
	assert.ErrorIs(t, err, ErrRankTooLow)
	assert.Equal(t, leader, leaderIDOf(t, ctx, db, guildID))
}

// TestSetMemberRole_StaleCachedRoleIgnored:授权只看 MySQL。
// Redis 里那份"他还是帮主"的快照陈旧一次,就等于越权一次。
func TestSetMemberRole_StaleCachedRoleIgnored(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7121
		leader  uint64 = 8121
		member  uint64 = 8122
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})

	cached, err := repo.GetGuild(ctx, guildID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, constants.RoleLeader, guildRoles(cached)[leader])

	// 绕开 repo 直接在权威库降权,模拟"降职写成功但缓存没失效"。
	mustExec(t, ctx, db, "UPDATE guild_member SET role=? WHERE guild_id=? AND player_id=?",
		constants.RoleMember, guildID, leader)
	stale, err := repo.GetGuild(ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, constants.RoleLeader, guildRoles(stale)[leader], "测试前提:Redis 仍是降权前的快照")

	_, err = repo.SetMemberRole(ctx, guildID, leader, member, constants.RoleOfficer, capOf(5))
	assert.ErrorIs(t, err, ErrRankTooLow)
	role, found := memberRoleOf(t, ctx, db, guildID, member)
	require.True(t, found)
	assert.Equal(t, constants.RoleMember, role)
}

// TestSetMemberRole_MissingLevelConfigFailsClosed:GuildLevel 缺该等级行时
// 不许"当作无上限"放行 —— 配表读不到是配置错误,安全路径一律 fail-closed。
func TestSetMemberRole_MissingLevelConfigFailsClosed(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7131
		leader  uint64 = 8131
		member  uint64 = 8132
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})

	_, err := repo.SetMemberRole(ctx, guildID, leader, member, constants.RoleOfficer, capMissing())
	assert.ErrorIs(t, err, ErrGuildLevelConfigMissing)
	role, found := memberRoleOf(t, ctx, db, guildID, member)
	require.True(t, found)
	assert.Equal(t, constants.RoleMember, role)

	// 降级为成员不查上限,配表缺失不该连"收权"都做不了。
	_, err = repo.SetMemberRole(ctx, guildID, leader, member, constants.RoleMember, capMissing())
	require.NoError(t, err)
}

// TestKickMember_Matrix:踢人的职位矩阵 + 被踢者的映射缓存必须失效
// (不失效的话他最长 30 分钟还以为自己在帮里)。
func TestKickMember_Matrix(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID  uint64 = 7141
		leader   uint64 = 8141
		officer1 uint64 = 8142
		officer2 uint64 = 8143
		member   uint64 = 8144
		outsider uint64 = 8145
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		officer1: constants.RoleOfficer, officer2: constants.RoleOfficer, member: constants.RoleMember,
	})

	_, err := repo.KickMember(ctx, guildID, officer1, officer2)
	assert.ErrorIs(t, err, ErrRankTooLow, "平级不能互踢")
	_, err = repo.KickMember(ctx, guildID, officer1, leader)
	assert.ErrorIs(t, err, ErrRankTooLow, "没有人能踢帮主")
	_, err = repo.KickMember(ctx, guildID, leader, outsider)
	assert.ErrorIs(t, err, ErrTargetNotMember)

	// 先把被踢者的映射缓存预热,才能证明踢人之后那个键真的被删了。
	cachedGuildID, err := repo.GetPlayerGuildID(ctx, officer2)
	require.NoError(t, err)
	require.Equal(t, guildID, cachedGuildID)
	require.EqualValues(t, 1, repo.rdb.Exists(ctx, playerGuildKey(officer2)).Val())

	res, err := repo.KickMember(ctx, guildID, leader, officer2)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	require.NotNil(t, res.Guild)
	assert.NotContains(t, guildRoles(res.Guild), officer2, "快照不能再含被踢者")
	assert.Zero(t, memberCount(t, ctx, db, officer2))
	assert.EqualValues(t, 0, repo.rdb.Exists(ctx, playerGuildKey(officer2)).Val(), "被踢者的映射键必须被删")
}

// TestTransferLeader_OldLeaderBecomesOfficerWhenSlotFree:长老位有空,原帮主补进去。
func TestTransferLeader_OldLeaderBecomesOfficerWhenSlotFree(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7151
		leader  uint64 = 8151
		officer uint64 = 8152
		member  uint64 = 8153
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})

	res, err := repo.TransferLeader(ctx, guildID, leader, member, capOf(2))
	require.NoError(t, err)
	assert.True(t, res.Changed)
	require.NotNil(t, res.Guild)

	roles := guildRoles(res.Guild)
	assert.Equal(t, constants.RoleLeader, roles[member])
	assert.Equal(t, constants.RoleOfficer, roles[leader])
	assert.Equal(t, member, res.Guild.LeaderID)
	assert.Equal(t, member, leaderIDOf(t, ctx, db, guildID), "guild.leader_id 与 role=3 必须同事务改写")
}

// TestTransferLeader_OldLeaderBecomesMemberWhenFull:长老位满,原帮主降成员。
// 契约宁可降级也不超编。
func TestTransferLeader_OldLeaderBecomesMemberWhenFull(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7161
		leader  uint64 = 8161
		officer uint64 = 8162
		member  uint64 = 8163
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})

	res, err := repo.TransferLeader(ctx, guildID, leader, member, capOf(1))
	require.NoError(t, err)
	roles := guildRoles(res.Guild)
	assert.Equal(t, constants.RoleLeader, roles[member])
	assert.Equal(t, constants.RoleMember, roles[leader])
	assert.Equal(t, constants.RoleOfficer, roles[officer], "既有长老不因转让被挤掉")
}

// TestTransferLeader_TargetOfficerFreesSlot:目标本来就是长老,转让后他腾出一个位,
// 原帮主正好补进去 —— 这条分支写漏就会在 cap=1 时把原帮主直接降成成员。
func TestTransferLeader_TargetOfficerFreesSlot(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7171
		leader  uint64 = 8171
		officer uint64 = 8172
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{officer: constants.RoleOfficer})

	res, err := repo.TransferLeader(ctx, guildID, leader, officer, capOf(1))
	require.NoError(t, err)
	roles := guildRoles(res.Guild)
	assert.Equal(t, constants.RoleLeader, roles[officer])
	assert.Equal(t, constants.RoleOfficer, roles[leader])
	assert.Equal(t, 1, officerCount(t, ctx, db, guildID), "长老数不能超过 cap")
}

// TestTransferLeader_LeaderIDMismatchFailsClosed:guild.leader_id 与 role=3 对不上时
// 不猜哪份对,直接拒写 —— 在这种状态下转让只会让两份存储分叉得更远。
func TestTransferLeader_LeaderIDMismatchFailsClosed(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7181
		leader  uint64 = 8181
		member  uint64 = 8182
		bogus   uint64 = 8183
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})
	mustExec(t, ctx, db, "UPDATE guild SET leader_id=? WHERE guild_id=?", bogus, guildID)

	_, err := repo.TransferLeader(ctx, guildID, leader, member, capOf(2))
	assert.ErrorIs(t, err, ErrLeaderMismatch)

	assert.Equal(t, bogus, leaderIDOf(t, ctx, db, guildID), "拒写之后数据必须原封不动")
	role, found := memberRoleOf(t, ctx, db, guildID, member)
	require.True(t, found)
	assert.Equal(t, constants.RoleMember, role)
	role, found = memberRoleOf(t, ctx, db, guildID, leader)
	require.True(t, found)
	assert.Equal(t, constants.RoleLeader, role)
}

// TestLeaveGuild:帮主不能退帮(否则帮会变无主状态);退过之后再退是 ErrNotGuildMember。
func TestLeaveGuild(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7191
		leader  uint64 = 8191
		member  uint64 = 8192
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})

	_, err := repo.LeaveGuild(ctx, guildID, leader)
	assert.ErrorIs(t, err, ErrLeaderCantLeave)
	assert.Equal(t, 1, memberCount(t, ctx, db, leader))

	res, err := repo.LeaveGuild(ctx, guildID, member)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	require.NotNil(t, res.Guild)
	assert.NotContains(t, guildRoles(res.Guild), member)

	_, err = repo.LeaveGuild(ctx, guildID, member)
	assert.ErrorIs(t, err, ErrNotGuildMember)
}

// ── 申请(§8) ──────────────────────────────────────────────

// TestApply_RefreshSameGuild:同帮重复申请 = 刷新有效期并成功,不是报错,也不是插第二行。
func TestApply_RefreshSameGuild(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7201
		leader    uint64 = 8201
		applicant uint64 = 8202
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)

	res, err := repo.ApplyToGuild(ctx, guildID, applicant, 0, testNowMs, testRules)
	require.NoError(t, err)
	require.True(t, res.Inserted)
	assert.Equal(t, []uint64{leader}, res.ReviewerIDs)

	var firstExpire uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT expire_ms FROM guild_application WHERE guild_id=? AND player_id=?",
		guildID, applicant).Scan(&firstExpire))

	res, err = repo.ApplyToGuild(ctx, guildID, applicant, 0, testNowMs+1000, testRules)
	require.NoError(t, err)
	assert.False(t, res.Inserted, "刷新不是新申请,logic 据此不重复推送")
	assert.Empty(t, res.ReviewerIDs)

	var secondExpire uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT expire_ms FROM guild_application WHERE guild_id=? AND player_id=?",
		guildID, applicant).Scan(&secondExpire))
	assert.Greater(t, secondExpire, firstExpire)
	assert.Equal(t, 1, playerApplicationCount(t, ctx, db, applicant), "刷新不能插第二行")
}

// TestApply_RefreshWhileGuildFull:满员只拒绝**新**申请人;
// 已在队列里的人刷新有效期仍然成功(否则他会在满员期间被静默挤掉)。
func TestApply_RefreshWhileGuildFull(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7211
		leader  uint64 = 8211
		filler  uint64 = 8212
		p       uint64 = 8213
		q       uint64 = 8214
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 2, leader, nil) // max_members=2,当前 1 人

	res, err := repo.ApplyToGuild(ctx, guildID, p, 0, testNowMs, testRules)
	require.NoError(t, err)
	require.True(t, res.Inserted)
	var firstExpire uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT expire_ms FROM guild_application WHERE guild_id=? AND player_id=?", guildID, p).Scan(&firstExpire))

	seedMemberRow(t, ctx, db, guildID, filler, constants.RoleMember) // 帮会现在满了

	res, err = repo.ApplyToGuild(ctx, guildID, p, 0, testNowMs+5000, testRules)
	require.NoError(t, err)
	assert.False(t, res.Inserted)
	var secondExpire uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT expire_ms FROM guild_application WHERE guild_id=? AND player_id=?", guildID, p).Scan(&secondExpire))
	assert.Greater(t, secondExpire, firstExpire)

	_, err = repo.ApplyToGuild(ctx, guildID, q, 0, testNowMs+5000, testRules)
	assert.ErrorIs(t, err, ErrGuildFull)
	assert.Zero(t, playerApplicationCount(t, ctx, db, q))
}

// TestApply_PlayerLimitCountsLiveOnly:每人待审上限只数**未过期**的;
// 过期行在申请时被惰性清掉,不该永久占着名额。
func TestApply_PlayerLimitCountsLiveOnly(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const applicant uint64 = 8221
	guilds := []uint64{7221, 7222, 7223, 7224}
	for i, guildID := range guilds {
		seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, 8300+uint64(i), nil)
	}

	for _, guildID := range guilds[:3] {
		res, err := repo.ApplyToGuild(ctx, guildID, applicant, 0, testNowMs, testRules)
		require.NoError(t, err, "guild %d", guildID)
		require.True(t, res.Inserted)
	}
	_, err := repo.ApplyToGuild(ctx, guilds[3], applicant, 0, testNowMs, testRules)
	assert.ErrorIs(t, err, ErrApplicationLimit)

	// 把第一条改成已过期:名额应当立刻回来,并且那一行会被物理删除。
	mustExec(t, ctx, db, "UPDATE guild_application SET expire_ms=? WHERE guild_id=? AND player_id=?",
		testNowMs-1, guilds[0], applicant)

	res, err := repo.ApplyToGuild(ctx, guilds[3], applicant, 0, testNowMs, testRules)
	require.NoError(t, err)
	assert.True(t, res.Inserted)
	assert.Zero(t, guildApplicationCount(t, ctx, db, guilds[0], applicant), "过期行必须被惰性清理")
	assert.Equal(t, 3, playerApplicationCount(t, ctx, db, applicant))
}

// TestApply_GuildQueueFull:帮会待审队列上限按不变式 I1 计 ——
// 已经入了别的帮的申请人不占本帮名额。
func TestApply_GuildQueueFull(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		target uint64 = 7231
		other  uint64 = 7232
		leader uint64 = 8231
		p1     uint64 = 8232
		p2     uint64 = 8233
		p3     uint64 = 8234
	)
	seedManagedGuild(t, ctx, db, target, 2, 1, 50, leader, nil)
	seedManagedGuild(t, ctx, db, other, 2, 1, 50, 8235, nil)

	for _, p := range []uint64{p1, p2} {
		res, err := repo.ApplyToGuild(ctx, target, p, 0, testNowMs, testRules)
		require.NoError(t, err, "player %d", p)
		require.True(t, res.Inserted)
	}
	_, err := repo.ApplyToGuild(ctx, target, p3, 0, testNowMs, testRules)
	assert.ErrorIs(t, err, ErrApplicationQueueFull)

	// p1 入了别的帮:他那条申请按 I1 已经无效,名额回到队列。
	seedMemberRow(t, ctx, db, other, p1, constants.RoleMember)
	res, err := repo.ApplyToGuild(ctx, target, p3, 0, testNowMs, testRules)
	require.NoError(t, err)
	assert.True(t, res.Inserted)
}

// TestApply_GuildFullAndMember:满员、已入帮、帮会不存在三条拒绝路径。
func TestApply_GuildFullAndMember(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		fullGuild  uint64 = 7241
		openGuild  uint64 = 7242
		fullLeader uint64 = 8241
		openLeader uint64 = 8242
		outsider   uint64 = 8243
	)
	seedManagedGuild(t, ctx, db, fullGuild, 2, 1, 1, fullLeader, nil) // max_members=1:帮主一人即满
	seedManagedGuild(t, ctx, db, openGuild, 2, 1, 50, openLeader, nil)

	_, err := repo.ApplyToGuild(ctx, fullGuild, outsider, 0, testNowMs, testRules)
	assert.ErrorIs(t, err, ErrGuildFull)
	assert.Zero(t, playerApplicationCount(t, ctx, db, outsider))

	_, err = repo.ApplyToGuild(ctx, openGuild, fullLeader, 0, testNowMs, testRules)
	assert.ErrorIs(t, err, ErrPlayerAlreadyInGuild)
	assert.Zero(t, playerApplicationCount(t, ctx, db, fullLeader))

	_, err = repo.ApplyToGuild(ctx, 7249, outsider, 0, testNowMs, testRules)
	assert.ErrorIs(t, err, ErrGuildGone)
}

// TestApply_ReviewerIDs:APPLICATION_RECEIVED 的收件人只有长老与帮主,player_id 升序。
func TestApply_ReviewerIDs(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7251
		leader    uint64 = 8251
		officer   uint64 = 8252
		member1   uint64 = 8253
		member2   uint64 = 8254
		applicant uint64 = 8255
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member1: constants.RoleMember, member2: constants.RoleMember,
	})

	res, err := repo.ApplyToGuild(ctx, guildID, applicant, 0, testNowMs, testRules)
	require.NoError(t, err)
	require.True(t, res.Inserted)
	assert.Equal(t, []uint64{leader, officer}, res.ReviewerIDs, "普通成员不是审批人")
}

// TestCancelApplication:撤回一条有效申请返回 nil;重复撤回与撤回过期行都回 NotFound,
// 但过期行必须被真的删掉(否则它会一直躺在表里)。
func TestCancelApplication(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7261
		leader    uint64 = 8261
		applicant uint64 = 8262
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)

	_, err := repo.ApplyToGuild(ctx, guildID, applicant, 0, testNowMs, testRules)
	require.NoError(t, err)

	require.NoError(t, repo.CancelApplication(ctx, guildID, applicant, testNowMs))
	assert.Zero(t, guildApplicationCount(t, ctx, db, guildID, applicant))

	err = repo.CancelApplication(ctx, guildID, applicant, testNowMs)
	assert.ErrorIs(t, err, ErrApplicationNotFound)

	seedApplicationRow(t, ctx, db, guildID, applicant, testNowMs-100, testNowMs-1)
	err = repo.CancelApplication(ctx, guildID, applicant, testNowMs)
	assert.ErrorIs(t, err, ErrApplicationNotFound)
	assert.Zero(t, guildApplicationCount(t, ctx, db, guildID, applicant), "过期行也要顺手删掉")
}

// TestListApplicantsAppliesI1:列表与计数用同一条判据 I1 —— 未过期,且申请人没有成员行。
// 两者口径不一致时,角标数字和列表长度会对不上。
func TestListApplicantsAppliesI1(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7271
		other   uint64 = 7272
		leader  uint64 = 8271
		live    uint64 = 8272
		expired uint64 = 8273
		joined  uint64 = 8274
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)
	seedManagedGuild(t, ctx, db, other, 2, 1, 50, 8275, nil)

	// 直接插行:走 ApplyToGuild 会顺带清掉过期行,夹具就构造不出来了。
	seedApplicationRow(t, ctx, db, guildID, live, testNowMs-30, testNowMs+testApplicationTTLMs)
	seedApplicationRow(t, ctx, db, guildID, expired, testNowMs-20, testNowMs-1)
	seedApplicationRow(t, ctx, db, guildID, joined, testNowMs-10, testNowMs+testApplicationTTLMs)
	seedMemberRow(t, ctx, db, other, joined, constants.RoleMember)

	rows, err := repo.ListApplicants(ctx, guildID, testNowMs, 50)
	require.NoError(t, err)
	assert.Equal(t, []uint64{live}, applicantIDs(rows))

	count, err := repo.CountLiveApplications(ctx, guildID, testNowMs)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	// limit=0 是"别查了",不是"不限量"。
	rows, err = repo.ListApplicants(ctx, guildID, testNowMs, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// ── 审批(§8.5) ────────────────────────────────────────────

// TestReview_ApproveDeletesAllApplicantRows:不变式 I2 ——
// 成员行一出现就清该玩家在**所有**帮会的申请,否则他日后退帮时旧申请会按 I1"复活"。
func TestReview_ApproveDeletesAllApplicantRows(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g1        uint64 = 7281
		g2        uint64 = 7282
		l1        uint64 = 8281
		l2        uint64 = 8282
		applicant uint64 = 8283
		zone      uint32 = 2
	)
	seedManagedGuild(t, ctx, db, g1, zone, 1, 50, l1, nil)
	seedManagedGuild(t, ctx, db, g2, zone, 1, 50, l2, nil)
	for _, guildID := range []uint64{g1, g2} {
		_, err := repo.ApplyToGuild(ctx, guildID, applicant, zone, testNowMs, testRules)
		require.NoError(t, err, "guild %d", guildID)
	}

	// 预热映射键(此刻缓存的是 0),用来证明通过之后它被失效了。
	cached, err := repo.GetPlayerGuildID(ctx, applicant)
	require.NoError(t, err)
	require.Zero(t, cached)
	require.EqualValues(t, 1, repo.rdb.Exists(ctx, playerGuildKey(applicant)).Val())

	res, err := repo.ReviewApplication(ctx, g1, l1, applicant, true, zone, testNowMs)
	require.NoError(t, err)
	assert.True(t, res.Approved)
	require.NotNil(t, res.Guild)
	assert.Equal(t, constants.RoleMember, guildRoles(res.Guild)[applicant])

	role, found := memberRoleOf(t, ctx, db, g1, applicant)
	require.True(t, found)
	assert.Equal(t, constants.RoleMember, role)
	assert.Zero(t, playerApplicationCount(t, ctx, db, applicant), "I2:两条申请都要没")
	assert.EqualValues(t, 0, repo.rdb.Exists(ctx, playerGuildKey(applicant)).Val(), "映射键必须失效")
}

// TestReview_ZoneMismatchDeletesApplication:unmerge 只改 guild.zone_id、不动申请表,
// 回滚后残留的跨区申请一旦被批就造出跨区成员。锁内复核 zone 是这条防线。
func TestReview_ZoneMismatchDeletesApplication(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7291
		leader    uint64 = 8291
		applicant uint64 = 8292
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)
	_, err := repo.ApplyToGuild(ctx, guildID, applicant, 2, testNowMs, testRules)
	require.NoError(t, err)

	mustExec(t, ctx, db, "UPDATE guild SET zone_id=3 WHERE guild_id=?", guildID) // 模拟 unmerge 回滚

	_, err = repo.ReviewApplication(ctx, guildID, leader, applicant, true, 2, testNowMs)
	assert.ErrorIs(t, err, ErrApplicationNotFound)
	assert.Zero(t, guildApplicationCount(t, ctx, db, guildID, applicant), "跨区申请必须被删掉,不能留着等下次再批")
	assert.Zero(t, memberCount(t, ctx, db, applicant))
}

// TestReview_Reject:拒绝删申请行、不加成员,并回带快照供客户端刷新。
func TestReview_Reject(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7301
		leader    uint64 = 8301
		applicant uint64 = 8302
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)
	seedApplicationRow(t, ctx, db, guildID, applicant, testNowMs-10, testNowMs+testApplicationTTLMs)

	// 拒绝分支不看申请人归属区(不放人进来就不需要 zone),applicantZone 传 0 合法。
	res, err := repo.ReviewApplication(ctx, guildID, leader, applicant, false, 0, testNowMs)
	require.NoError(t, err)
	assert.False(t, res.Approved)
	require.NotNil(t, res.Guild)
	assert.Zero(t, guildApplicationCount(t, ctx, db, guildID, applicant))
	assert.Zero(t, memberCount(t, ctx, db, applicant))
}

// TestReview_ExpiredCommitsDelete:发现申请已过期时,既要把行真的删掉(提交),又要回 NotFound。
// 直接返回错误会让 defer Rollback 把删除一起撤销,过期行就永远留在表里。
func TestReview_ExpiredCommitsDelete(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7311
		leader    uint64 = 8311
		applicant uint64 = 8312
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)
	seedApplicationRow(t, ctx, db, guildID, applicant, testNowMs-100, testNowMs-1)

	_, err := repo.ReviewApplication(ctx, guildID, leader, applicant, true, 2, testNowMs)
	assert.ErrorIs(t, err, ErrApplicationNotFound)
	assert.Zero(t, guildApplicationCount(t, ctx, db, guildID, applicant), "删除必须被提交")
	assert.Zero(t, memberCount(t, ctx, db, applicant))
}

// TestReview_FullKeepsApplication:人满只是暂时的,不该因为一次审批把申请人的名额烧掉。
func TestReview_FullKeepsApplication(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7321
		leader    uint64 = 8321
		applicant uint64 = 8322
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 1, leader, nil) // max_members=1:帮主一人即满
	seedApplicationRow(t, ctx, db, guildID, applicant, testNowMs-10, testNowMs+testApplicationTTLMs)

	_, err := repo.ReviewApplication(ctx, guildID, leader, applicant, true, 2, testNowMs)
	assert.ErrorIs(t, err, ErrGuildFull)
	assert.Equal(t, 1, guildApplicationCount(t, ctx, db, guildID, applicant), "满员时必须保留申请")
	assert.Zero(t, memberCount(t, ctx, db, applicant))
}

// TestReview_ApplicantJoinedElsewhere:两帮并发审批同一人时,输的一方会撞 uk_guild_member。
// InnoDB 只回滚那条语句,所以可以接着删申请行再提交 —— 对审批者来说"这条已经不能批了"。
func TestReview_ApplicantJoinedElsewhere(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g1        uint64 = 7331
		g2        uint64 = 7332
		l1        uint64 = 8331
		l2        uint64 = 8332
		applicant uint64 = 8333
	)
	seedManagedGuild(t, ctx, db, g1, 2, 1, 50, l1, nil)
	seedManagedGuild(t, ctx, db, g2, 2, 1, 50, l2, nil)
	seedApplicationRow(t, ctx, db, g1, applicant, testNowMs-10, testNowMs+testApplicationTTLMs)
	seedMemberRow(t, ctx, db, g2, applicant, constants.RoleMember)

	_, err := repo.ReviewApplication(ctx, g1, l1, applicant, true, 2, testNowMs)
	assert.ErrorIs(t, err, ErrApplicationNotFound)
	assert.Zero(t, guildApplicationCount(t, ctx, db, g1, applicant))
	_, found := memberRoleOf(t, ctx, db, g1, applicant)
	assert.False(t, found, "不能在 g1 留下成员行")
	role, found := memberRoleOf(t, ctx, db, g2, applicant)
	require.True(t, found, "他在 g2 的成员关系不受影响")
	assert.Equal(t, constants.RoleMember, role)
}

// TestReview_MemberCannotReview:审批权是长老起步;普通成员被拒后申请必须原样留着。
func TestReview_MemberCannotReview(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7341
		leader    uint64 = 8341
		member    uint64 = 8342
		applicant uint64 = 8343
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})
	seedApplicationRow(t, ctx, db, guildID, applicant, testNowMs-10, testNowMs+testApplicationTTLMs)

	_, err := repo.ReviewApplication(ctx, guildID, member, applicant, true, 2, testNowMs)
	assert.ErrorIs(t, err, ErrRankTooLow)
	assert.Equal(t, 1, guildApplicationCount(t, ctx, db, guildID, applicant))
	assert.Zero(t, memberCount(t, ctx, db, applicant))
}

// ── 建帮 / 解散 / 公告(§8.6–§8.8) ───────────────────────────

// TestCreateGuild_DeletesOwnApplications:不变式 I2 的建帮侧。
// 漏了这一步,建帮者日后解散退帮时,72h 内的旧申请会按 I1 复活,把他拉进早就忘了的帮会。
func TestCreateGuild_DeletesOwnApplications(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g1      uint64 = 7351
		g2      uint64 = 7352
		l1      uint64 = 8351
		founder uint64 = 8352
	)
	seedManagedGuild(t, ctx, db, g1, 2, 1, 50, l1, nil)
	res, err := repo.ApplyToGuild(ctx, g1, founder, 2, testNowMs, testRules)
	require.NoError(t, err)
	require.True(t, res.Inserted)

	require.NoError(t, repo.CreateGuild(ctx, &GuildData{
		GuildID: g2, Name: "founder-guild", LeaderID: founder,
		Level: constants.DefaultInitLevel, MaxMembers: 30, ZoneID: 2, CreateTimeMs: testNowMs,
		Members: []MemberData{{
			PlayerID: founder, Role: constants.RoleLeader,
			JoinTimeMs: testNowMs, LastActiveMs: testNowMs,
		}},
	}))

	applicants, err := repo.ListApplicants(ctx, g1, testNowMs, 50)
	require.NoError(t, err)
	assert.Empty(t, applicants)

	// 关键在"解散之后仍然为空":I2 删的是物理行,不是靠 I1 过滤遮住。
	_, err = repo.DisbandGuild(ctx, g2, founder)
	require.NoError(t, err)
	applicants, err = repo.ListApplicants(ctx, g1, testNowMs, 50)
	require.NoError(t, err)
	assert.Empty(t, applicants)
	assert.Zero(t, playerApplicationCount(t, ctx, db, founder))
}

// TestDisbandGuild_AuthorizesByMySQLAndDeletesApplications:解散授权读 MySQL 的 leader_id
// (缓存的 LeaderID 在转让之后最长陈旧 30 分钟,按它授权等于让前帮主还能解散帮会)。
func TestDisbandGuild_AuthorizesByMySQLAndDeletesApplications(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID  uint64 = 7361
		other    uint64 = 7362
		leader   uint64 = 8361
		officer  uint64 = 8362
		member   uint64 = 8363
		outsider uint64 = 8364
		zone     uint32 = 5
	)
	seedManagedGuild(t, ctx, db, guildID, zone, 1, 50, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})
	seedManagedGuild(t, ctx, db, other, zone, 1, 50, 8365, nil)
	seedApplicationRow(t, ctx, db, guildID, outsider, testNowMs-10, testNowMs+testApplicationTTLMs)
	seedApplicationRow(t, ctx, db, other, member, testNowMs-10, testNowMs+testApplicationTTLMs)

	_, err := repo.DisbandGuild(ctx, guildID, officer)
	assert.ErrorIs(t, err, ErrRankTooLow)
	assert.Equal(t, 1, guildRowCount(t, ctx, db, guildID))

	res, err := repo.DisbandGuild(ctx, guildID, leader)
	require.NoError(t, err)
	assert.Equal(t, zone, res.ZoneID, "清榜只能信删除事务里 FOR UPDATE 读到的 zone")
	assert.Equal(t, []uint64{leader, officer, member}, res.MemberIDs, "收件人必须是 player_id 升序")

	assert.Zero(t, guildRowCount(t, ctx, db, guildID))
	for _, playerID := range []uint64{leader, officer, member} {
		assert.Zero(t, memberCount(t, ctx, db, playerID), "player %d", playerID)
	}
	assert.Zero(t, guildApplicationCount(t, ctx, db, guildID, outsider), "本帮的待审申请要清掉")
	assert.Zero(t, playerApplicationCount(t, ctx, db, member), "I3:成员在他帮的申请也要清掉")
}

// TestUpdateAnnouncement_ReturnsSnapshot:公告写回带事务内快照(客户端直接应用,不用再拉一次)。
func TestUpdateAnnouncement_ReturnsSnapshot(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7371
		leader  uint64 = 8371
		officer uint64 = 8372
		member  uint64 = 8373
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})

	guild, err := repo.UpdateAnnouncement(ctx, guildID, officer, "今晚八点集合")
	require.NoError(t, err)
	require.NotNil(t, guild)
	assert.Equal(t, "今晚八点集合", guild.Announcement)
	assert.Len(t, guild.Members, 3, "快照是完整的权威快照,不只是公告字段")

	_, err = repo.UpdateAnnouncement(ctx, guildID, member, "普通成员不该写进去")
	assert.ErrorIs(t, err, ErrAnnouncementForbidden)

	var stored string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT announcement FROM guild WHERE guild_id=?", guildID).Scan(&stored))
	assert.Equal(t, "今晚八点集合", stored)
}

// TestLeaveAndKickDeleteOwnApplications:不变式 I3。
// 成员期间的残留申请按 I1 本来就无效,但物理行还在 —— 不删的话,他离帮后这些行会"复活"。
func TestLeaveAndKickDeleteOwnApplications(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7381
		elseWhere uint64 = 7382
		leader    uint64 = 8381
		leaver    uint64 = 8382
		kicked    uint64 = 8383
		stayer    uint64 = 8384
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		leaver: constants.RoleMember, kicked: constants.RoleMember, stayer: constants.RoleMember,
	})
	seedManagedGuild(t, ctx, db, elseWhere, 2, 1, 50, 8385, nil)

	// 直接插行:模拟"申请与他帮审批并发"留下的竞态残留。
	seedApplicationRow(t, ctx, db, elseWhere, leaver, testNowMs-10, testNowMs+testApplicationTTLMs)
	_, err := repo.LeaveGuild(ctx, guildID, leaver)
	require.NoError(t, err)
	assert.Zero(t, playerApplicationCount(t, ctx, db, leaver), "退帮必须带走残留申请")

	seedApplicationRow(t, ctx, db, elseWhere, kicked, testNowMs-10, testNowMs+testApplicationTTLMs)
	_, err = repo.KickMember(ctx, guildID, leader, kicked)
	require.NoError(t, err)
	assert.Zero(t, playerApplicationCount(t, ctx, db, kicked), "被踢必须带走残留申请")

	seedApplicationRow(t, ctx, db, elseWhere, stayer, testNowMs-10, testNowMs+testApplicationTTLMs)
	seedApplicationRow(t, ctx, db, elseWhere, leader, testNowMs-10, testNowMs+testApplicationTTLMs)
	_, err = repo.DisbandGuild(ctx, guildID, leader)
	require.NoError(t, err)
	assert.Zero(t, playerApplicationCount(t, ctx, db, stayer), "解散必须带走全部成员的残留申请")
	assert.Zero(t, playerApplicationCount(t, ctx, db, leader))
}

// ── 锁等待封顶(§6.2a,M5 的实测项) ─────────────────────────

// TestLockWaitTimeoutBounded 实测 innodb_lock_wait_timeout=1 是否真的生效。
//
// 为什么必须实测:"ctx 取消后服务端等锁的线程不感知断连"是推断。若这条封顶不生效,
// 一个长事务占着 guild 行锁时,该帮后续所有写会连锁等到 InnoDB 默认的 50s,
// 而 RPC 侧只会看到超时,谁都不知道卡在哪。
// 实测耗时打进日志;若封顶不生效(如账号无权设会话变量),按 §30 上报,不得自行调大。
func TestLockWaitTimeoutBounded(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7391
		leader  uint64 = 8391
		member  uint64 = 8392
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})

	// 占住 guild 行锁不提交 —— 这正是 KickMember 的锁序位置 1。
	blocker, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	var level uint32
	require.NoError(t, blocker.QueryRowContext(ctx,
		"SELECT level FROM guild WHERE guild_id=? FOR UPDATE", guildID).Scan(&level))

	start := time.Now()
	_, err = repo.KickMember(ctx, guildID, leader, member)
	elapsed := time.Since(start)
	t.Logf("锁等待实测耗时 %v(innodb_lock_wait_timeout=%ds)", elapsed, LockWaitTimeoutSeconds)
	assert.ErrorIs(t, err, ErrWriteConflict, "1205 必须归一到可重试的忙错误,不是内部错误")
	assert.Less(t, elapsed, 2500*time.Millisecond,
		"必须在 innodb_lock_wait_timeout 内返回,而不是等到 ctx 超时或 InnoDB 默认的 50s")

	require.NoError(t, blocker.Rollback())
	res, err := repo.KickMember(ctx, guildID, leader, member)
	require.NoError(t, err, "占锁事务结束后同一操作必须成功")
	assert.True(t, res.Changed)
}

// ── 陈旧映射自愈(§6.4 的 M2 修订) ───────────────────────────

// TestVerifyPlayerGuildID_StaleZeroHeals:GetPlayerGuildID 连 0 也缓存 30 分钟。
// 审批通过之后若失效连后台重试都失败,刚入帮的玩家会在这半小时里什么帮会操作都做不了。
func TestVerifyPlayerGuildID_StaleZeroHeals(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7401
		leader  uint64 = 8401
		player  uint64 = 8402
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, nil)

	cached, err := repo.GetPlayerGuildID(ctx, player)
	require.NoError(t, err)
	require.Zero(t, cached, "测试前提:未入帮的 0 被缓存下来")

	// 绕开 repo 直接建成员行,模拟"写成功但失效失败"。
	seedMemberRow(t, ctx, db, guildID, player, constants.RoleMember)

	actual, err := repo.VerifyPlayerGuildID(ctx, player, 0)
	require.NoError(t, err)
	assert.Equal(t, guildID, actual)

	healed, err := repo.GetPlayerGuildID(ctx, player)
	require.NoError(t, err)
	assert.Equal(t, guildID, healed, "复核之后普通读路径也必须拿到真相")

	// 映射一致时不该翻代:无谓的 generation INCR 会把别人正在进行的回填全部作废。
	generationKey := playerGuildCacheGenerationKey(player)
	before := repo.rdb.Get(ctx, generationKey).Val()
	actual, err = repo.VerifyPlayerGuildID(ctx, player, guildID)
	require.NoError(t, err)
	assert.Equal(t, guildID, actual)
	assert.Equal(t, before, repo.rdb.Get(ctx, generationKey).Val())
}

// TestResolvePlayerGuild_KickedStaleSnapshot:客户端 GetPlayerGuild 的权威解析。
// 差别只在坏路径:缓存说 0、或快照里没有本人时,用 MySQL 复核一次,
// 而不是把陈旧结论直接回给玩家。
func TestResolvePlayerGuild_KickedStaleSnapshot(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g1     uint64 = 7411
		g2     uint64 = 7412
		l1     uint64 = 8411
		l2     uint64 = 8412
		player uint64 = 8413
	)
	seedManagedGuild(t, ctx, db, g1, 2, 1, 50, l1, map[uint64]uint32{player: constants.RoleMember})

	// 只预热"玩家 → 帮会"映射:模拟踢人之后帮会快照失效成功、玩家映射失效失败。
	// (若把含本人的旧快照也留在缓存里,那是两个键同时陈旧的另一个故障面,
	//  按契约 ResolvePlayerGuild 会把那份快照当成事实,不在本用例范围内。)
	cached, err := repo.GetPlayerGuildID(ctx, player)
	require.NoError(t, err)
	require.Equal(t, g1, cached)

	mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id=? AND player_id=?", g1, player)

	guild, err := repo.ResolvePlayerGuild(ctx, player)
	require.NoError(t, err)
	assert.Nil(t, guild, "被踢之后必须解析成'未入帮',而不是回一份陈旧的帮会")

	// 另一例:缓存停在 0,而他其实已经入了 g2。
	seedManagedGuild(t, ctx, db, g2, 2, 1, 50, l2, nil)
	cached, err = repo.GetPlayerGuildID(ctx, player)
	require.NoError(t, err)
	require.Zero(t, cached, "测试前提:此刻缓存的是 0")

	seedMemberRow(t, ctx, db, g2, player, constants.RoleMember)
	guild, err = repo.ResolvePlayerGuild(ctx, player)
	require.NoError(t, err)
	require.NotNil(t, guild)
	assert.Equal(t, g2, guild.GuildID)
	assert.True(t, guildHasMember(guild, player), "回带的快照必须含本人")
}

// ── 并发(§8.9) ────────────────────────────────────────────

// concurrentRounds:并发用例的轮数。单轮跑赢是运气,20 轮连续成立才说明串行化真的在起作用。
const concurrentRounds = 20

// TestConcurrentApproveSameApplicant:两个帮会同时批同一个申请人。
// 唯一索引 uk_guild_member 保证只能成一个;输的一方走 1062 分支删申请并回 NotFound。
func TestConcurrentApproveSameApplicant(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g1        uint64 = 7421
		g2        uint64 = 7422
		l1        uint64 = 8421
		l2        uint64 = 8422
		applicant uint64 = 8423
		zone      uint32 = 2
	)
	seedManagedGuild(t, ctx, db, g1, zone, 1, 50, l1, nil)
	seedManagedGuild(t, ctx, db, g2, zone, 1, 50, l2, nil)

	for round := 0; round < concurrentRounds; round++ {
		now := testNowMs + uint64(round)
		// 每轮重置成自由身:上一轮入了帮,这一轮就申请不了了。
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE player_id=?", applicant)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", applicant)
		for _, guildID := range []uint64{g1, g2} {
			_, err := repo.ApplyToGuild(ctx, guildID, applicant, zone, now, testRules)
			require.NoError(t, err, "round %d guild %d", round, guildID)
		}

		errs := runConcurrently(
			func() error {
				_, err := repo.ReviewApplication(ctx, g1, l1, applicant, true, zone, now)
				return err
			},
			func() error {
				_, err := repo.ReviewApplication(ctx, g2, l2, applicant, true, zone, now)
				return err
			},
		)

		require.Equal(t, 1, countNilErrors(errs), "round %d:恰好一方成功,errs=%v", round, errs)
		for _, err := range errs {
			if err != nil {
				assertErrorIn(t, err, ErrApplicationNotFound, ErrWriteConflict)
			}
		}
		assert.Equal(t, 1, memberCount(t, ctx, db, applicant), "round %d:成员行恰好一行", round)
		assert.Zero(t, playerApplicationCount(t, ctx, db, applicant), "round %d:I2 清干净", round)
	}
}

// TestConcurrentApplyRespectsPlayerLimit:同一玩家并发申请多个帮会。
// 串行化靠 guild_player_state 的行锁(X-10),不靠 RR 的间隙锁 ——
// 间隙锁在 TiDB 上不存在,迁库后上限会静默失效。
func TestConcurrentApplyRespectsPlayerLimit(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const applicant uint64 = 8431
	// seeded 的两条申请直接插行占掉名额(不经 ApplyToGuild,免得惰性清理动到夹具);
	// targets 才是本轮并发申请的目标。MaxPerPlayer=3 ⇒ 三个并发里只能进一个。
	seeded := []uint64{7431, 7432}
	targets := []uint64{7433, 7434, 7435}
	for i, guildID := range append(append([]uint64{}, seeded...), targets...) {
		seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, 8440+uint64(i), nil)
	}

	for round := 0; round < concurrentRounds; round++ {
		now := testNowMs + uint64(round)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", applicant)
		for _, guildID := range seeded {
			seedApplicationRow(t, ctx, db, guildID, applicant, now, now+testApplicationTTLMs)
		}

		errs := runConcurrently(
			func() error { _, err := repo.ApplyToGuild(ctx, targets[0], applicant, 0, now, testRules); return err },
			func() error { _, err := repo.ApplyToGuild(ctx, targets[1], applicant, 0, now, testRules); return err },
			func() error { _, err := repo.ApplyToGuild(ctx, targets[2], applicant, 0, now, testRules); return err },
		)

		require.Equal(t, 1, countNilErrors(errs),
			"round %d:MaxPerPlayer=3、已有 2 条,只能再进 1 条,errs=%v", round, errs)
		for _, err := range errs {
			if err != nil {
				assertErrorIn(t, err, ErrApplicationLimit, ErrWriteConflict)
			}
		}
		assert.Equal(t, int(testRules.MaxPerPlayer), liveApplicationCount(t, ctx, db, applicant, now),
			"round %d:有效申请数必须恰好卡在上限", round)
	}
}

// TestConcurrentPromoteRespectsCap:长老位是"帮会级"的资源,
// 并发任免必须在同一把 guild 行锁下串行,否则 cap=1 也能出两个长老。
func TestConcurrentPromoteRespectsCap(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID uint64 = 7441
		leader  uint64 = 8441
		a       uint64 = 8442
		b       uint64 = 8443
	)
	seedManagedGuild(t, ctx, db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		a: constants.RoleMember, b: constants.RoleMember,
	})

	for round := 0; round < concurrentRounds; round++ {
		mustExec(t, ctx, db, "UPDATE guild_member SET role=? WHERE guild_id=? AND player_id IN (?, ?)",
			constants.RoleMember, guildID, a, b)

		errs := runConcurrently(
			func() error {
				_, err := repo.SetMemberRole(ctx, guildID, leader, a, constants.RoleOfficer, capOf(1))
				return err
			},
			func() error {
				_, err := repo.SetMemberRole(ctx, guildID, leader, b, constants.RoleOfficer, capOf(1))
				return err
			},
		)

		require.Equal(t, 1, countNilErrors(errs), "round %d:只能成一个,errs=%v", round, errs)
		for _, err := range errs {
			if err != nil {
				assertErrorIn(t, err, ErrOfficerLimit, ErrWriteConflict)
			}
		}
		assert.Equal(t, 1, officerCount(t, ctx, db, guildID), "round %d", round)
	}
}

// TestConcurrentCreateAndApply:建帮与申请抢同一个玩家。
// 两者都要给他插成员行 / 申请行,由 guild_player_state 的行锁串行(X-10);
// 无论谁先,结束时都不许留下"他既是 G2 帮主、又在申请 G1"的状态(I2 + I3)。
func TestConcurrentCreateAndApply(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		g1     uint64 = 7451
		l1     uint64 = 8451
		player uint64 = 8452
		zone   uint32 = 2
	)
	seedManagedGuild(t, ctx, db, g1, zone, 1, 50, l1, nil)

	for round := 0; round < concurrentRounds; round++ {
		now := testNowMs + uint64(round)
		founded := 7460 + uint64(round) // 每轮一个新帮会 id 与新帮名(帮名全服唯一)
		mustExec(t, ctx, db, "DELETE FROM guild_member WHERE player_id=?", player)
		mustExec(t, ctx, db, "DELETE FROM guild_application WHERE player_id=?", player)
		mustExec(t, ctx, db, "DELETE FROM guild WHERE guild_id=?", founded)

		founding := &GuildData{
			GuildID: founded, Name: fmt.Sprintf("race-guild-%d", round), LeaderID: player,
			Level: constants.DefaultInitLevel, MaxMembers: 30, ZoneID: zone, CreateTimeMs: now,
			Members: []MemberData{{
				PlayerID: player, Role: constants.RoleLeader,
				JoinTimeMs: now, LastActiveMs: now,
			}},
		}
		errs := runConcurrently(
			func() error { return repo.CreateGuild(ctx, founding) },
			func() error { _, err := repo.ApplyToGuild(ctx, g1, player, zone, now, testRules); return err },
		)
		createErr, applyErr := errs[0], errs[1]

		if applyErr != nil {
			assertErrorIn(t, applyErr, ErrPlayerAlreadyInGuild, ErrWriteConflict)
		}
		if createErr != nil {
			// 建帮唯一可接受的失败是忙错误;其余都说明有别的东西抢走了这个玩家。
			assertErrorIn(t, createErr, ErrWriteConflict)
			continue
		}

		live, err := repo.CountLiveApplications(ctx, g1, now)
		require.NoError(t, err)
		assert.Zero(t, live, "round %d:建帮成功就不该还有本人的有效申请(I2)", round)

		res, err := repo.DisbandGuild(ctx, founded, player)
		require.NoError(t, err, "round %d", round)
		assert.Equal(t, []uint64{player}, res.MemberIDs)

		applicants, err := repo.ListApplicants(ctx, g1, now, 50)
		require.NoError(t, err)
		assert.NotContains(t, applicantIDs(applicants), player, "round %d:解散之后旧申请不许复活", round)
		assert.Zero(t, playerApplicationCount(t, ctx, db, player),
			"round %d:I3 删的是物理行,不能只靠 I1 过滤遮住", round)
	}
}
