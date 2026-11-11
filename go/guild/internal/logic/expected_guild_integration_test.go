//go:build integration

package logic

// expected_guild_id 栅栏的端到端用例(docs/design/client-request-ordering.md O14 / §6.4,批次 E2):
// 真 MySQL + miniredis,夹具复用 economy_flow_integration_test.go 的 newEconomyFlowEnv(一次性库、真实配表、记录型推送 / 摘要替身)。
//
// 运行(Codex):`go test -tags integration ./internal/logic -run ExpectedGuild -count=1 -v`。GUILD_TEST_MYSQL_DSN 为空时整体 Skip
// (⚠ Skip 不等于通过,验收时要看到 PASS)。
//
// 覆盖五个写 RPC(LeaveGuild / DisbandGuild / KickGuildMember / TransferGuildLeader / SetGuildMemberRole)的同一组契约:
//   - expected_guild_id = 0:行为与加字段之前相同,只计一次 absent;内部调用(无会话)不计;
//   - 与操作者当前帮会一致:照常成功,不计数;
//   - 不一致(在别的帮 / 不在任何帮):回 kGuildNotInGuild,库里一行不改、不推送、不 bump;回包有 guild 字段的附操作者**当前**帮会快照;
//   - 复核与写入之间换帮(两种交错,判据相同):
//     a. 预检读的缓存还指着 G、库里已换到 H(提交后失效还没跑到 / 失败):事务锁住 G 行后找不到本人成员行;
//     b. 请求已在等 G 的 guild 行锁时,占锁事务把操作者从 G 换到 H 并提交:请求拿到锁后找不到本人成员行。
//     两种都只能拒绝,绝不能作用到 H,也不能在 G 上留下写。
//
// 时钟:离帮 / 踢人 / 解散事务用的 nowMs() 是真实时钟,只落在被拒绝时回滚的行上或成功分支的提前截止上,这里不断言它。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	pb "proto/guild"
)

const (
	egGuild       uint64 = 5101 // G:客户端发请求时看到的本人帮会
	egOther       uint64 = 5102 // H:玩家后来所在的帮会
	egActor       uint64 = 7101 // P:发请求的玩家
	egTarget      uint64 = 7102 // T:任免 / 踢人 / 转让的对象
	egHeir        uint64 = 7103 // Q:P 是 G 的帮主时,离开 G 之前把帮主交给他
	egOtherLeader uint64 = 7104 // H 的帮主
	egOtherMember uint64 = 7105 // H 的成员
)

// expectedGuildCase 是一个写 RPC 在本文件里的全部差异。
type expectedGuildCase struct {
	method    string // expected_guild 指标的 method 取值
	actorRole uint32 // P 在 G 里的职位:够执行这一操作的最低职位
	hasView   bool   // 回包有 guild 字段(任免 / 踢人 / 转让)
	run       func(l *GuildLogic, ctx context.Context, expected uint64) (tipID uint32, view *pb.GuildInfo, err error)
	// written 断言成功时 G 上应当留下的那一笔写(反过来也用来证明「被拒时没有这笔写」由快照比对负责)。
	written func(t *testing.T, env *economyFlowEnv)
}

func expectedGuildCases() []expectedGuildCase {
	return []expectedGuildCase{
		{
			method: expectedGuildMethodLeave, actorRole: constants.RoleMember,
			run: func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, *pb.GuildInfo, error) {
				resp, err := l.LeaveGuild(ctx, &pb.LeaveGuildRequest{ExpectedGuildId: expected})
				return resp.GetErrorMessage().GetId(), nil, err
			},
			written: func(t *testing.T, env *economyFlowEnv) {
				_, found := egMemberRole(t, env.db, egGuild, egActor)
				assert.False(t, found, "离帮:P 已不在 G")
			},
		},
		{
			method: expectedGuildMethodDisband, actorRole: constants.RoleLeader,
			run: func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, *pb.GuildInfo, error) {
				resp, err := l.DisbandGuild(ctx, &pb.DisbandGuildRequest{ExpectedGuildId: expected})
				return resp.GetErrorMessage().GetId(), nil, err
			},
			written: func(t *testing.T, env *economyFlowEnv) {
				var n int
				require.NoError(t, env.db.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM guild WHERE guild_id = ?", egGuild).Scan(&n))
				assert.Zero(t, n, "解散:G 的帮会行已删")
			},
		},
		{
			method: expectedGuildMethodKick, actorRole: constants.RoleOfficer, hasView: true,
			run: func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, *pb.GuildInfo, error) {
				resp, err := l.KickGuildMember(ctx, &pb.KickGuildMemberRequest{TargetPlayerId: egTarget, ExpectedGuildId: expected})
				return resp.GetErrorMessage().GetId(), resp.GetGuild(), err
			},
			written: func(t *testing.T, env *economyFlowEnv) {
				_, found := egMemberRole(t, env.db, egGuild, egTarget)
				assert.False(t, found, "踢人:T 已不在 G")
			},
		},
		{
			method: expectedGuildMethodTransfer, actorRole: constants.RoleLeader, hasView: true,
			run: func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, *pb.GuildInfo, error) {
				resp, err := l.TransferGuildLeader(ctx, &pb.TransferGuildLeaderRequest{TargetPlayerId: egTarget, ExpectedGuildId: expected})
				return resp.GetErrorMessage().GetId(), resp.GetGuild(), err
			},
			written: func(t *testing.T, env *economyFlowEnv) {
				assert.Equal(t, egTarget, egLeaderOf(t, env.db, egGuild), "转让:G 的帮主已是 T")
			},
		},
		{
			method: expectedGuildMethodSetRole, actorRole: constants.RoleLeader, hasView: true,
			run: func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, *pb.GuildInfo, error) {
				resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{
					TargetPlayerId: egTarget, Role: constants.RoleOfficer, ExpectedGuildId: expected,
				})
				return resp.GetErrorMessage().GetId(), resp.GetGuild(), err
			},
			written: func(t *testing.T, env *economyFlowEnv) {
				role, found := egMemberRole(t, env.db, egGuild, egTarget)
				require.True(t, found)
				assert.Equal(t, constants.RoleOfficer, role, "任免:T 已是长老")
			},
		},
	}
}

// ── 夹具 ──────────────────────────────────────────────────────

// newExpectedGuildEnv 建一次性库并造两个帮:
//   - G:P 按 actorRole 入帮;P 是帮主时 T、Q 是成员,否则 Q 是帮主、T 是成员;
//   - H:帮主 HL + 成员 HM。
//
// 归属 zone 换成加锁的 perPlayerZones:「等锁」用例里被测 RPC 跑在后台协程。
func newExpectedGuildEnv(t *testing.T, actorRole uint32) *economyFlowEnv {
	t.Helper()
	env := newEconomyFlowEnv(t, 1)
	if actorRole == constants.RoleLeader {
		env.seedGuild(t, egGuild, egActor, egTarget, egHeir)
	} else {
		env.seedGuild(t, egGuild, egHeir, egActor, egTarget)
		if actorRole != constants.RoleMember {
			env.setRole(t, egGuild, egActor, actorRole)
		}
	}
	env.seedGuild(t, egOther, egOtherLeader, egOtherMember)
	zones := map[uint64]uint32{}
	for _, id := range []uint64{egActor, egTarget, egHeir, egOtherLeader, egOtherMember} {
		zones[id] = flowZone
	}
	env.logic.homeZones = &perPlayerZones{zones: zones}
	return env
}

// egExecer / egQueryer:*sql.DB 与 *sql.Tx 共有的那一半,让同一段换帮 SQL 既能自动提交执行,也能放进占锁事务。
type egExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type egQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// egMoveActorOut 把 P 移出 G:P 是帮主时先把帮主交给 Q(leader_id 与 role=3 一起改,保持双存储一致)。
// joinOther = true 时 P 随即作为普通成员加入 H。全部是直写 SQL,刻意**不**失效缓存 —— 等同于「提交后失效还没跑到」。
func egMoveActorOut(t *testing.T, q egExecer, actorRole uint32, joinOther bool) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string, args ...any) {
		_, err := q.ExecContext(ctx, query, args...)
		require.NoError(t, err, query)
	}
	if actorRole == constants.RoleLeader {
		exec("UPDATE guild SET leader_id = ? WHERE guild_id = ?", egHeir, egGuild)
		exec("UPDATE guild_member SET role = ? WHERE guild_id = ? AND player_id = ?", constants.RoleLeader, egGuild, egHeir)
	}
	exec("DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?", egGuild, egActor)
	if joinOther {
		exec(`INSERT INTO guild_member
			(guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
			VALUES (?, ?, ?, 1, 1, 0, 0)`, egOther, egActor, constants.RoleMember)
	}
}

// egSnapshot 是「有没有写」的判据:两个帮的帮会行(帮主)与全部成员行(职位、帮贡)。
// guild_player_state 不在内:事务外补建状态行是既有行为,与帮会归属无关。
func egSnapshot(t *testing.T, q egQueryer) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	collect := func(query string, format func(*sql.Rows) (string, error)) {
		rows, err := q.QueryContext(ctx, query)
		require.NoError(t, err, query)
		defer rows.Close()
		for rows.Next() {
			line, err := format(rows)
			require.NoError(t, err)
			out = append(out, line)
		}
		require.NoError(t, rows.Err())
	}
	collect("SELECT guild_id, leader_id FROM guild ORDER BY guild_id", func(r *sql.Rows) (string, error) {
		var g, leader uint64
		err := r.Scan(&g, &leader)
		return fmt.Sprintf("guild %d leader=%d", g, leader), err
	})
	collect(`SELECT guild_id, player_id, role, contribution_total, contribution_balance
		FROM guild_member ORDER BY guild_id, player_id`, func(r *sql.Rows) (string, error) {
		var g, p, total, balance uint64
		var role uint32
		err := r.Scan(&g, &p, &role, &total, &balance)
		return fmt.Sprintf("member %d/%d role=%d contribution=%d/%d", g, p, role, total, balance), err
	})
	return out
}

func egMemberRole(t *testing.T, db *sql.DB, guildID, playerID uint64) (uint32, bool) {
	t.Helper()
	var role uint32
	err := db.QueryRowContext(context.Background(),
		"SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ?", guildID, playerID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return role, true
}

func egLeaderOf(t *testing.T, db *sql.DB, guildID uint64) uint64 {
	t.Helper()
	var leader uint64
	require.NoError(t, db.QueryRowContext(context.Background(),
		"SELECT leader_id FROM guild WHERE guild_id = ?", guildID).Scan(&leader))
	return leader
}

// egWarmMapping 让 P 的映射缓存读进「G」(预检看到的就是它),之后的直写 SQL 不会失效它。
func egWarmMapping(t *testing.T, env *economyFlowEnv) {
	t.Helper()
	cached, err := env.logic.repo.GetPlayerGuildID(context.Background(), egActor)
	require.NoError(t, err)
	require.Equal(t, egGuild, cached, "夹具:换帮之前缓存必须指着 G")
}

// egAssertRejected 是「不一致即拒绝」的共同判据:kGuildNotInGuild(导表常量)、gRPC 层成功、库里一行不改、
// 不推送、不 bump、计一次 mismatch;回包有 guild 字段的,附操作者当前帮会的快照(wantView = 0 表示不在任何帮、不填)。
func egAssertRejected(t *testing.T, env *economyFlowEnv, c expectedGuildCase, counts *expectedGuildCounts,
	tipID uint32, view *pb.GuildInfo, err error, before []string, wantView uint64) {
	t.Helper()
	require.NoError(t, err, "不在预期帮是业务拒绝,不能回 gRPC 错误(会让客户端进重连隔离)")
	assert.Equal(t, constants.ErrNotInGuild, tipID)
	assert.Equal(t, before, egSnapshot(t, env.db), "被拒的请求不能在 G 或 H 上留下任何写")
	assert.Zero(t, env.notifier.total(), "被拒不推送")
	assert.Zero(t, env.syncBumps.count(), "被拒不 bump 落地恢复摘要")
	assert.Equal(t, 1, counts.of(c.method, expectedGuildOutcomeMismatch))
	assert.Zero(t, counts.of(c.method, expectedGuildOutcomeAbsent))
	if !c.hasView {
		return
	}
	if wantView == 0 {
		assert.Nil(t, view, "不在任何帮:不附快照")
		return
	}
	require.NotNil(t, view, "回包要附操作者当前所在帮会的快照,客户端据此直接纠正界面")
	assert.Equal(t, wantView, view.GetGuildId())
}

// ── 用例 ──────────────────────────────────────────────────────

// TestExpectedGuildZeroKeepsLegacyBehavior:旧客户端(expected_guild_id = 0)按服务端此刻查到的本人帮会执行,结果与加字段之前相同,
// 每次计一次 absent;无会话调用在身份闸就被拒,走不到计数。
func TestExpectedGuildZeroKeepsLegacyBehavior(t *testing.T) {
	for _, c := range expectedGuildCases() {
		t.Run(c.method, func(t *testing.T) {
			env := newExpectedGuildEnv(t, c.actorRole)
			counts := watchExpectedGuild(t)

			tipID, _, err := c.run(env.logic, clientCtx(egActor), 0)

			require.NoError(t, err)
			require.Zero(t, tipID)
			c.written(t, env)
			assert.Equal(t, 1, counts.of(c.method, expectedGuildOutcomeAbsent))
			assert.Equal(t, 1, counts.total())
		})
	}

	// 无会话的退帮以前按请求体身份执行(docs/design/gate-online-identity.md §7.5),现在被身份闸拒绝:
	// 成员行原样保留,也不计 absent(那只衡量客户端来源)。
	t.Run("无会话调用被拒且不计数", func(t *testing.T) {
		env := newExpectedGuildEnv(t, constants.RoleMember)
		counts := watchExpectedGuild(t)

		_, err := env.logic.LeaveGuild(context.Background(), &pb.LeaveGuildRequest{})

		require.Equal(t, codes.PermissionDenied, status.Code(err))
		_, found := egMemberRole(t, env.db, egGuild, egActor)
		assert.True(t, found, "被拒的退帮不能删成员行")
		assert.Zero(t, counts.total(), "absent 只衡量客户端来源")
	})
}

// TestExpectedGuildMatchSucceeds:带上的 expected_guild_id 就是操作者此刻所在的帮 → 照常执行,结果与 expected = 0 相同,不计数。
func TestExpectedGuildMatchSucceeds(t *testing.T) {
	for _, c := range expectedGuildCases() {
		t.Run(c.method, func(t *testing.T) {
			env := newExpectedGuildEnv(t, c.actorRole)
			counts := watchExpectedGuild(t)

			tipID, view, err := c.run(env.logic, clientCtx(egActor), egGuild)

			require.NoError(t, err)
			require.Zero(t, tipID)
			c.written(t, env)
			if c.hasView {
				assert.Equal(t, egGuild, view.GetGuildId(), "成功回包仍是事务内的 G 快照")
			}
			assert.Zero(t, counts.total(), "一致的请求不计:它与按方法的请求总数之差就是 absent")
		})
	}
}

// TestExpectedGuildMismatchRejectedWithoutWrite:操作者此刻在别的帮(H)或不在任何帮,请求带的是 G → 拒绝,哪个帮都不写。
// 缓存是新鲜的(没有预热),由预检拦下:不进事务、不在 G 上取任何锁、不给 G 的成员建状态行。
func TestExpectedGuildMismatchRejectedWithoutWrite(t *testing.T) {
	for _, c := range expectedGuildCases() {
		t.Run(c.method+"/在别的帮", func(t *testing.T) {
			env := newExpectedGuildEnv(t, c.actorRole)
			egMoveActorOut(t, env.db, c.actorRole, true)
			before := egSnapshot(t, env.db)
			counts := watchExpectedGuild(t)

			tipID, view, err := c.run(env.logic, clientCtx(egActor), egGuild)

			egAssertRejected(t, env, c, counts, tipID, view, err, before, egOther)
		})
		t.Run(c.method+"/不在任何帮", func(t *testing.T) {
			env := newExpectedGuildEnv(t, c.actorRole)
			egMoveActorOut(t, env.db, c.actorRole, false)
			before := egSnapshot(t, env.db)
			counts := watchExpectedGuild(t)

			tipID, view, err := c.run(env.logic, clientCtx(egActor), egGuild)

			egAssertRejected(t, env, c, counts, tipID, view, err, before, 0)
		})
	}
}

// TestExpectedGuildSwitchAfterPrecheck(交错 a):预检读到的缓存还指着 G(换帮已提交、失效还没跑到),所以请求带着 G 进了事务;
// 事务锁住 G 的 guild 行后找不到本人成员行 → 拒绝。解散那一支走 data.disbandRefusal(leader_id 已不是他,补读成员行)。
// 拒绝时顺手按 MySQL 纠正映射,所以附带的快照是 H 而不是陈旧的 G。
func TestExpectedGuildSwitchAfterPrecheck(t *testing.T) {
	for _, c := range expectedGuildCases() {
		t.Run(c.method, func(t *testing.T) {
			env := newExpectedGuildEnv(t, c.actorRole)
			egWarmMapping(t, env)
			egMoveActorOut(t, env.db, c.actorRole, true)
			before := egSnapshot(t, env.db)
			counts := watchExpectedGuild(t)

			tipID, view, err := c.run(env.logic, clientCtx(egActor), egGuild)

			egAssertRejected(t, env, c, counts, tipID, view, err, before, egOther)
			cached, err := env.logic.repo.GetPlayerGuildID(context.Background(), egActor)
			require.NoError(t, err)
			assert.Equal(t, egOther, cached, "拒绝时要把陈旧映射纠正到 MySQL 的真相")
		})
	}
}

// TestExpectedGuildSwitchWhileWaitingForGuildLock(交错 b):请求已过预检(缓存指着 G)、正排在 G 的 guild 行锁上,
// 占锁事务在这时把 P 从 G 换到 H 并提交。请求拿到锁后看到的是提交后的成员关系,只能拒绝。
//
// 判据只看结局,与轮询是否真的观察到锁等待无关:即使请求在占锁事务提交之后才进事务(账号没有 PROCESS 权限、轮询退化成固定等待),
// 它看到的也是同一个已提交状态,结局相同。占锁事务内的快照(含它自己未提交的换帮)就是「请求不写」时的终态。
//
// 锁等待上限是 innodb_lock_wait_timeout=1(同生产 DSN):占锁事务从观察到等待到提交只有几条语句,远小于 1s;
// 真超了请求会回 kGuildBusyRetry,本用例以「tip 不是 kGuildNotInGuild」失败,看失败信息即可区分。
func TestExpectedGuildSwitchWhileWaitingForGuildLock(t *testing.T) {
	for _, c := range expectedGuildCases() {
		t.Run(c.method, func(t *testing.T) {
			env := newExpectedGuildEnv(t, c.actorRole)
			egWarmMapping(t, env)
			counts := watchExpectedGuild(t)

			ctx := context.Background()
			blocker, err := env.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			require.NoError(t, err)
			defer blocker.Rollback()
			var locked uint64
			require.NoError(t, blocker.QueryRowContext(ctx,
				"SELECT guild_id FROM guild WHERE guild_id = ? FOR UPDATE", egGuild).Scan(&locked))

			type result struct {
				tipID uint32
				view  *pb.GuildInfo
				err   error
			}
			done := make(chan result, 1)
			go func() {
				tipID, view, err := c.run(env.logic, clientCtx(egActor), egGuild)
				done <- result{tipID, view, err}
			}()

			egAwaitLockWait(ctx, env.db, 500*time.Millisecond)
			egMoveActorOut(t, blocker, c.actorRole, true)
			final := egSnapshot(t, blocker)
			require.NoError(t, blocker.Commit())

			var got result
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("被测 RPC 10s 内没有返回")
			}
			egAssertRejected(t, env, c, counts, got.tipID, got.view, got.err, final, egOther)
		})
	}
}

// TestExpectedGuildLegacyLeaveFollowsCurrentGuild 钉住字段要防的那件事,以及它为什么只对新客户端生效:
// P 已从 G 换到 H(缓存是新鲜的)。带着 G 的晚到退帮被拒,H 原封不动;同样的退帮不带字段(旧客户端)就按「此刻所在的帮」执行,
// 把 P 从 H 退掉 —— 这是旧行为,不改,absent 计数就是在量它还剩多少。
func TestExpectedGuildLegacyLeaveFollowsCurrentGuild(t *testing.T) {
	env := newExpectedGuildEnv(t, constants.RoleMember)
	egMoveActorOut(t, env.db, constants.RoleMember, true)
	counts := watchExpectedGuild(t)

	withField, err := env.logic.LeaveGuild(clientCtx(egActor), &pb.LeaveGuildRequest{ExpectedGuildId: egGuild})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNotInGuild, withField.GetErrorMessage().GetId())
	_, stillInOther := egMemberRole(t, env.db, egOther, egActor)
	assert.True(t, stillInOther, "带 expected_guild_id 的晚到退帮不能把玩家从新帮会移出")

	legacy, err := env.logic.LeaveGuild(clientCtx(egActor), &pb.LeaveGuildRequest{})
	require.NoError(t, err)
	require.Nil(t, legacy.GetErrorMessage())
	_, stillInOther = egMemberRole(t, env.db, egOther, egActor)
	assert.False(t, stillInOther, "旧客户端:按服务端此刻查到的帮会执行(行为不变)")
	assert.Equal(t, 1, counts.of(expectedGuildMethodLeave, expectedGuildOutcomeMismatch))
	assert.Equal(t, 1, counts.of(expectedGuildMethodLeave, expectedGuildOutcomeAbsent))
}

// egAwaitLockWait 等到实例上至少有一个事务处于锁等待(INFORMATION_SCHEMA.INNODB_TRX,需 PROCESS 权限),最多等 limit。
// 与 data 包 lockOrderAwaitLockWaits 同一写法(_test.go 不跨包导出,这里留一份最小的):账号没有权限时退化成固定等待 limit。
// 它只影响「交错 b」能否稳定摆出来,不影响判据(见 TestExpectedGuildSwitchWhileWaitingForGuildLock)。
func egAwaitLockWait(ctx context.Context, db *sql.DB, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var waiting int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.INNODB_TRX WHERE trx_state = 'LOCK WAIT'").Scan(&waiting)
		if err != nil {
			time.Sleep(time.Until(deadline))
			return
		}
		if waiting >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
