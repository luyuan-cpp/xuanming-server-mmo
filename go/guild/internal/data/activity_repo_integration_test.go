//go:build integration

package data

// activity_repo_integration_test.go —— 帮会活动 repo(B6a:元宵灯会 / 中秋团圆)的测试。
//
// 覆盖 06-activities.md §6.41 的 I1–I12,外加并发(同玩家多路、同帮多人越过阈值、点灯 ‖ 解散、点灯 ‖ 兑换预留)。
// 死锁是用户硬要求(92-handoff §12.0):并发用例一律用 econWatchDeadlocks 比对 InnoDB 的 LATEST DETECTED DEADLOCK ——
// inTx 会把 1213 吸收掉重跑,只看返回值看不见死锁。
//
// 三条不连库的契约(入参校验先于碰库、语句形状、源码取锁先后)在不带 tag 的 activity_repo_static_test.go:
// 放在本文件里普通 `go test ./...` 编译不到,锁序回归会在日常构建里整段失明。**本文件只放要真库的用例。**
//
// 库:GUILD_IT_MYSQL_DSN(90-consistency Y-15;root 或任何有 CREATE / DROP DATABASE 与 PROCESS 权限的账号,库名被忽略)。
// 每个用例自建一次性库 guild_it_<pid>_<n>、经 schemamigrate 建表、结束 DROP,mmorpg_guild 一行都不碰。
// 未设置即 Skip;**设置了却连不上 / 建不了库是环境问题,直接红,不许当 Skip**。
//
// 跑法(Codex,串行;-p 1 是为了死锁判据不被别的包的并发测试覆盖现场):
//
//	$env:GUILD_IT_MYSQL_DSN = "<有建库权限的账号>@tcp(127.0.0.1:3306)/"   # 口令不写进任何文件
//	cd go/guild; go test -tags=integration -p 1 -count=1 -v -run "TestActivity" ./internal/data
//
// 通过标准:全部 PASS,输出里不得出现 SKIP(06 §6.41:不允许 SKIP 当作通过)。
//
// 纪律同 economy_repo_test.go:时间一律 testNowMs 派生、不读墙钟;并发闭包只调被测接口、写自己那一格结果,
// 不调 t.Fatal / require,断言在返回后由主 goroutine 做。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"

	"shared/assetop"
	tablepb "shared/generated/pb/table"

	"guild/internal/activity"
	"guild/internal/constants"
)

// actHourMs / actDayMs:推时钟用的单位。
const actHourMs uint64 = 3_600_000

const actDayMs uint64 = 24 * actHourMs

// actITBudget:单个用例(含建库建表与并发对撞)的总预算。
const actITBudget = 90 * time.Second

// actFixture 把活动 repo 与经济 repo / 资产 store 挂在同一个 GuildRepo 上(与生产接线一致),
// 嵌入 econFixture 以复用 economy_repo_test.go 的读库助手与并发、死锁判据。
type actFixture struct {
	econFixture
	act *ActivityRepo
}

// openActivityFixture 用 GUILD_IT_MYSQL_DSN 的账号建一次性库,DSN 经 WithLockWaitTimeout(与生产连接池同一套会话变量:
// 锁等待封顶 1s、会话 READ-COMMITTED、ClientFoundRows=false —— 计数 upsert 的 RowsAffected 语义依赖最后这一条)。
func openActivityFixture(t *testing.T) actFixture {
	t.Helper()
	raw := os.Getenv(envDSN)
	if raw == "" {
		t.Skipf("%s 未设置,跳过帮会活动真库测试(Codex 验证时必须设置:SKIP 不算通过)", envDSN)
	}
	ctx, cancel := context.WithTimeout(context.Background(), actITBudget)
	t.Cleanup(cancel)

	adminDSN, err := dsnWithDB(raw, "")
	require.NoError(t, err, "解析 %s", envDSN)
	admin, err := sql.Open("mysql", adminDSN)
	require.NoError(t, err)
	// Cleanup 后进先出:admin 最先登记、最后关闭,DROP DATABASE 那一步还用得上它。
	t.Cleanup(func() { admin.Close() })
	require.NoError(t, admin.PingContext(ctx), "设置了 %s 却连不上:环境问题,不许当 Skip", envDSN)

	name := fmt.Sprintf("%s%d_%d", itDBPrefix, os.Getpid(), itDBCounter.Add(1))
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` DEFAULT CHARACTER SET utf8mb4")
	require.NoError(t, err, "建一次性库 %s(账号需 CREATE / DROP DATABASE 权限)", name)

	itDSN, err := dsnWithDB(raw, name)
	require.NoError(t, err)
	itDSN, err = WithLockWaitTimeout(itDSN)
	require.NoError(t, err)
	db, err := sql.Open("mysql", itDSN)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), itConnTimeout)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Errorf("DROP DATABASE %s 失败(请手工清理): %v", name, err)
		}
	})
	resetGuildSchemaViaMigrate(t, ctx, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	repo := NewGuildRepo(rdb, db, time.Minute)
	econ, err := NewEconomyRepo(repo)
	require.NoError(t, err)
	store, err := NewGuildAssetStore(repo)
	require.NoError(t, err)
	act, err := NewActivityRepo(repo)
	require.NoError(t, err)
	return actFixture{
		econFixture: econFixture{ctx: ctx, db: db, repo: repo, econ: econ, store: store},
		act:         act,
	}
}

// actReunionRow 取 06 §6.2.2 的团圆开发默认行(0/0 常开);用例按需改字段。
// 灯会行 actLanternRow 与 actInput 在不带 tag 的 activity_repo_static_test.go(两种构建都要用)。
func actReunionRow() *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id: 2, Name: "中秋团圆", Type: activity.TypeReunion, Enabled: true, MinGuildLevel: 1,
		PersonalContribution: 30, GuildThreshold: 3, RewardId: 1, DailyLimit: 1,
	}
}

// actScheduled 把一行改成"昨天开始、30 天后结束"的档期行:帮会期键 = 开始那天的游戏日,跨 05:00 不变(U1)。
func actScheduled(row *tablepb.GuildActivityTable) *tablepb.GuildActivityTable {
	row.StartAtMs, row.EndAtMs = testNowMs-26*actHourMs, testNowMs+30*actDayMs
	return row
}

// actRewardBundle 是 Reward 1 合并后的包(两槽物品 1 × 2 → 物品 1 × 4,06 §6.41 I8)。
func actRewardBundle() *assetpb.AssetBundle {
	return &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 1, Count: 4}}}
}

func actReward(t *testing.T, opID, nowMs uint64) *ActivityRewardOp {
	t.Helper()
	payload, err := proto.Marshal(actRewardBundle())
	require.NoError(t, err)
	return &ActivityRewardOp{OpID: opID, Payload: payload, LeaseUntilMs: nowMs + econLeaseMs, LeaseToken: opID | 1<<62}
}

func actDayKey(nowMs uint64) uint32 { return activity.DayKey(time.UnixMilli(int64(nowMs))) }

func actKey(row *tablepb.GuildActivityTable, nowMs uint64) ProgressKey {
	return ProgressKey{ActivityID: row.GetId(), PeriodKey: activity.GuildPeriodKey(row, time.UnixMilli(int64(nowMs)))}
}

// actProgress 经生产读路径(GuildProgress)读一期进度;行不存在时 found=false。
func actProgress(t *testing.T, f actFixture, guildID uint64, key ProgressKey) (activity.Progress, bool) {
	t.Helper()
	got, err := f.act.GuildProgress(f.ctx, guildID, []ProgressKey{key})
	require.NoError(t, err)
	progress, found := got[key]
	return progress, found
}

func actProgressRowCount(t *testing.T, f actFixture, guildID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_activity_progress WHERE guild_id=?", guildID).Scan(&n))
	return n
}

func actUsed(t *testing.T, f actFixture, playerID uint64, activityID, dayKey uint32) (uint32, bool) {
	t.Helper()
	return econCounterUsed(t, f.econFixture, playerID, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY, activityID, dayKey)
}

func actOpCountOfPlayer(t *testing.T, f actFixture, playerID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_asset_op WHERE player_id=?", playerID).Scan(&n))
	return n
}

func actCreditSeqRowExists(t *testing.T, f actFixture, playerID uint64) bool {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_player_op_seq WHERE player_id=? AND stream=?",
		playerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT)).Scan(&n))
	return n > 0
}

func actCreditNextSeq(t *testing.T, f actFixture, playerID uint64) uint64 {
	t.Helper()
	return econNextSeq(t, f.econFixture, playerID, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT)
}

// actAssertContribution 断言帮贡两列。
func actAssertContribution(t *testing.T, f actFixture, guildID, playerID, wantTotal, wantBalance uint64, msg string) {
	t.Helper()
	total, balance, found := econContribution(t, f.econFixture, guildID, playerID)
	require.True(t, found, "%s:成员行应当存在", msg)
	assert.Equal(t, wantTotal, total, "%s:contribution_total", msg)
	assert.Equal(t, wantBalance, balance, "%s:contribution_balance", msg)
}

// actAssertUntouched:一次被拒的参与什么都没留下 —— 今日计数、本期进度行、帮贡。
func actAssertUntouched(t *testing.T, f actFixture, row *tablepb.GuildActivityTable, guildID, playerID, nowMs uint64, msg string) {
	t.Helper()
	_, found := actUsed(t, f, playerID, row.GetId(), actDayKey(nowMs))
	assert.False(t, found, "%s:不应占今日次数", msg)
	_, found = actProgress(t, f, guildID, actKey(row, nowMs))
	assert.False(t, found, "%s:不应留下进度行", msg)
	actAssertContribution(t, f, guildID, playerID, 0, 0, msg)
}

// ── 灯会(I1–I5) ─────────────────────────────────────────────

// TestActivityIT_LanternFirstLight(I1):计数 1、进度 1、帮贡两列各 +20;没有物品就不插指令行,
// 但仍建好并锁住 CREDIT 流的 seq 行作计数守卫(不推进 seq);闸门拿到的是事务内锁住的 zone。
func TestActivityIT_LanternFirstLight(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7601
		leader  uint64 = 8601
		player  uint64 = 8602
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	row := actLanternRow()
	in := actInput(row, guildID, player, testNowMs)
	var fencedZone uint32
	in.Fence = func(_ context.Context, zoneID uint32) error {
		fencedZone = zoneID
		return nil
	}

	res, err := f.act.LightLanternTx(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), fencedZone, "合服闸门必须用事务内锁住的 guild.zone_id")
	assert.False(t, res.Enqueued)
	assert.False(t, res.ReachedNow)
	assert.False(t, res.FundsGranted)
	assert.Equal(t, actDayKey(testNowMs), res.DayKey)
	assert.Equal(t, actDayKey(testNowMs), res.GuildPeriodKey, "0/0 常开行的帮会期键退化为当天游戏日")
	assert.Equal(t, activity.Progress{Count: 1}, res.Progress)

	used, found := actUsed(t, f, player, row.GetId(), res.DayKey)
	require.True(t, found)
	assert.Equal(t, uint32(1), used)
	progress, found := actProgress(t, f, guildID, actKey(row, testNowMs))
	require.True(t, found)
	assert.Equal(t, activity.Progress{Count: 1}, progress)
	actAssertContribution(t, f, guildID, player, 20, 20, "首次点灯")
	assert.Zero(t, econFunds(t, f.econFixture, guildID), "未达阈值不发资金")
	assert.Zero(t, actOpCountOfPlayer(t, f, player), "灯会没有物品奖励,不插指令行")
	assert.Equal(t, uint64(1), actCreditNextSeq(t, f, player), "seq 行作守卫建好了,但没分配就不推进")
}

// TestActivityIT_LanternSecondLightSameDay(I2):同一游戏日第二次 → AlreadyClaimed,全部不变。
func TestActivityIT_LanternSecondLightSameDay(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7611
		leader  uint64 = 8611
		player  uint64 = 8612
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	row := actLanternRow()
	_, err := f.act.LightLanternTx(f.ctx, actInput(row, guildID, player, testNowMs))
	require.NoError(t, err)

	_, err = f.act.LightLanternTx(f.ctx, actInput(row, guildID, player, testNowMs+60_000))
	assert.ErrorIs(t, err, ErrActivityAlreadyClaimed)

	used, _ := actUsed(t, f, player, row.GetId(), actDayKey(testNowMs))
	assert.Equal(t, uint32(1), used)
	progress, _ := actProgress(t, f, guildID, actKey(row, testNowMs))
	assert.Equal(t, uint32(1), progress.Count, "被拒的那次不加进度")
	actAssertContribution(t, f, guildID, player, 20, 20, "被拒的那次不加帮贡")
}

// TestActivityIT_LanternThresholdGrantsFundsOnce(I3):阈值 3,4 人依次点灯 —— 只有第 3 次首次达标并发资金,
// funds 恰好 +500,第 4 次不再加;锁存时刻是第 3 次的 now。
func TestActivityIT_LanternThresholdGrantsFundsOnce(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7621
		leader  uint64 = 8621
	)
	players := []uint64{8622, 8623, 8624, 8625}
	roles := map[uint64]uint32{}
	for _, p := range players {
		roles[p] = constants.RoleMember
	}
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, roles)
	row := actLanternRow()

	var reachedAt uint64
	for i, p := range players {
		now := testNowMs + uint64(i)*1000
		res, err := f.act.LightLanternTx(f.ctx, actInput(row, guildID, p, now))
		require.NoError(t, err, "第 %d 人", i+1)
		third := i == 2
		assert.Equal(t, third, res.ReachedNow, "第 %d 人", i+1)
		assert.Equal(t, third, res.FundsGranted, "第 %d 人", i+1)
		assert.Equal(t, uint32(i+1), res.Progress.Count, "第 %d 人", i+1)
		if third {
			reachedAt = now
		}
		wantFunds := uint64(0)
		if i >= 2 {
			wantFunds = 500
		}
		assert.Equal(t, wantFunds, econFunds(t, f.econFixture, guildID), "第 %d 人之后的帮会资金", i+1)
	}
	progress, found := actProgress(t, f, guildID, actKey(row, testNowMs))
	require.True(t, found)
	assert.Equal(t, activity.Progress{Count: 4, ThresholdReachedMs: reachedAt, FundsGranted: true}, progress)
}

// TestActivityIT_LanternScheduledPeriodSpansGameDays(I3b,U1):档期行第 1 游戏日 3 人点灯达阈值;推到第 2 游戏日,
// 同 3 人再点 —— 个人计数重新从 1 起、都成功;帮会进度累加在同一期(6),资金仍只 +500。
func TestActivityIT_LanternScheduledPeriodSpansGameDays(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7626
		leader  uint64 = 8626
	)
	players := []uint64{8627, 8628, 8629}
	roles := map[uint64]uint32{}
	for _, p := range players {
		roles[p] = constants.RoleMember
	}
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, roles)
	row := actScheduled(actLanternRow())
	day1, day2 := testNowMs, testNowMs+actDayMs
	require.NotEqual(t, actDayKey(day1), actDayKey(day2), "前提:两个时刻落在相邻两个游戏日")
	key := actKey(row, day1)
	require.Equal(t, key, actKey(row, day2), "档期行跨游戏日帮会期键不变")
	require.Equal(t, actDayKey(row.GetStartAtMs()), key.PeriodKey, "帮会期键 = 档期开始那天的游戏日")

	for day, now := range []uint64{day1, day2} {
		for i, p := range players {
			res, err := f.act.LightLanternTx(f.ctx, actInput(row, guildID, p, now+uint64(i)))
			require.NoError(t, err, "第 %d 天第 %d 人", day+1, i+1)
			firstReach := day == 0 && i == 2
			assert.Equal(t, firstReach, res.ReachedNow, "第 %d 天第 %d 人", day+1, i+1)
			assert.Equal(t, firstReach, res.FundsGranted, "第 %d 天第 %d 人", day+1, i+1)
			assert.Equal(t, key.PeriodKey, res.GuildPeriodKey)
			used, found := actUsed(t, f, p, row.GetId(), actDayKey(now))
			require.True(t, found)
			assert.Equal(t, uint32(1), used, "第 %d 天个人次数从 1 起", day+1)
		}
	}
	progress, found := actProgress(t, f, guildID, key)
	require.True(t, found)
	assert.Equal(t, uint32(6), progress.Count, "两天累加在同一期")
	assert.True(t, progress.FundsGranted)
	assert.Equal(t, uint64(500), econFunds(t, f.econFixture, guildID), "本档期资金只发一次")
	assert.Equal(t, 1, actProgressRowCount(t, f, guildID), "两天写的是同一行")
}

// TestActivityIT_LanternConcurrentSamePlayer(I4 / 06 §6.13 C10):同一玩家 10 路并发点灯(带物品奖励,顺带压 Q / O)——
// 恰 1 路成功,其余一律 AlreadyClaimed(不能是写冲突或内部错误);只留 1 行指令、seq 只前进 1;零死锁。
func TestActivityIT_LanternConcurrentSamePlayer(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7631
		leader  uint64 = 8631
		player  uint64 = 8632
		opBase  uint64 = 9_631_000
		workers        = 10
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	row := actLanternRow()
	fns := make([]func() error, workers)
	for i := range fns {
		in := actInput(row, guildID, player, testNowMs)
		in.Reward = actReward(t, opBase+uint64(i), testNowMs)
		fns[i] = func() error {
			_, err := f.act.LightLanternTx(f.ctx, in)
			return err
		}
	}

	watch := econWatchDeadlocks(t, f.econFixture)
	errs := econRunTogether(fns...)
	watch.assertNone(t, f.ctx, "同一玩家 10 路并发点灯")

	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		assert.ErrorIs(t, err, ErrActivityAlreadyClaimed, "第 %d 路:多出来的只能被每日上限拒绝", i)
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, econOpCountInRange(t, f.econFixture, opBase, opBase+workers-1), "被拒的参与不能留下指令行")
	assert.Equal(t, uint64(2), actCreditNextSeq(t, f, player), "seq 只因成功的那一次前进")
	used, _ := actUsed(t, f, player, row.GetId(), actDayKey(testNowMs))
	assert.Equal(t, uint32(1), used)
	progress, _ := actProgress(t, f, guildID, actKey(row, testNowMs))
	assert.Equal(t, uint32(1), progress.Count)
	actAssertContribution(t, f, guildID, player, 20, 20, "只加一次帮贡")
}

// TestActivityIT_LanternConcurrentSameGuild(I5 / C9):阈值 3,3 人各 2 路并发 —— 每人恰成功 1 次,进度 3;
// 恰有一次首次达标并发资金,funds 恰 +500;零死锁。
func TestActivityIT_LanternConcurrentSameGuild(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID   uint64 = 7641
		leader    uint64 = 8641
		perPlayer        = 2
	)
	players := []uint64{8642, 8643, 8644}
	roles := map[uint64]uint32{}
	for _, p := range players {
		roles[p] = constants.RoleMember
	}
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, roles)
	row := actLanternRow()

	var callers []uint64
	for _, p := range players {
		for range perPlayer {
			callers = append(callers, p)
		}
	}
	results := make([]ActivityTxResult, len(callers))
	fns := make([]func() error, len(callers))
	for i, p := range callers {
		in := actInput(row, guildID, p, testNowMs)
		fns[i] = func() error {
			res, err := f.act.LightLanternTx(f.ctx, in)
			results[i] = res
			return err
		}
	}

	watch := econWatchDeadlocks(t, f.econFixture)
	errs := econRunTogether(fns...)
	watch.assertNone(t, f.ctx, "同帮 3 人各 2 路并发点灯")

	perPlayerOK := map[uint64]int{}
	reached, granted := 0, 0
	for i, err := range errs {
		if err != nil {
			assert.ErrorIs(t, err, ErrActivityAlreadyClaimed, "第 %d 路", i)
			continue
		}
		perPlayerOK[callers[i]]++
		if results[i].ReachedNow {
			reached++
		}
		if results[i].FundsGranted {
			granted++
			assert.Equal(t, uint32(3), results[i].Progress.Count, "发资金的那一次读到的必须恰是第 3 盏灯")
		}
	}
	for _, p := range players {
		assert.Equal(t, 1, perPlayerOK[p], "玩家 %d 恰成功一次", p)
		used, _ := actUsed(t, f, p, row.GetId(), actDayKey(testNowMs))
		assert.Equal(t, uint32(1), used, "玩家 %d 的计数", p)
	}
	assert.Equal(t, 1, reached, "首次达标恰一次")
	assert.Equal(t, 1, granted, "资金恰发一次")
	progress, _ := actProgress(t, f, guildID, actKey(row, testNowMs))
	assert.Equal(t, uint32(3), progress.Count)
	assert.True(t, progress.Latched())
	assert.True(t, progress.FundsGranted)
	assert.Equal(t, uint64(500), econFunds(t, f.econFixture, guildID))
}

// TestActivityIT_LanternConcurrentCrossThreshold(C9 的另一面):阈值 3,5 人同时点灯 —— 全部成功、进度 5,
// 越过阈值的只有一次、资金只发一次(不因并发多判一次"首次达标")。
func TestActivityIT_LanternConcurrentCrossThreshold(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7646
		leader  uint64 = 8646
	)
	players := []uint64{8647, 8648, 8649, 8650, 8651}
	roles := map[uint64]uint32{}
	for _, p := range players {
		roles[p] = constants.RoleMember
	}
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, roles)
	row := actLanternRow()

	results := make([]ActivityTxResult, len(players))
	fns := make([]func() error, len(players))
	for i, p := range players {
		in := actInput(row, guildID, p, testNowMs)
		fns[i] = func() error {
			res, err := f.act.LightLanternTx(f.ctx, in)
			results[i] = res
			return err
		}
	}
	watch := econWatchDeadlocks(t, f.econFixture)
	errs := econRunTogether(fns...)
	watch.assertNone(t, f.ctx, "同帮 5 人同时点灯越过阈值")

	reached, granted := 0, 0
	for i, err := range errs {
		require.NoError(t, err, "第 %d 人", i+1)
		if results[i].ReachedNow {
			reached++
		}
		if results[i].FundsGranted {
			granted++
		}
	}
	assert.Equal(t, 1, reached)
	assert.Equal(t, 1, granted)
	progress, _ := actProgress(t, f, guildID, actKey(row, testNowMs))
	assert.Equal(t, uint32(5), progress.Count)
	assert.Equal(t, uint64(500), econFunds(t, f.econFixture, guildID))
}

// ── 团圆(I6–I9) ─────────────────────────────────────────────

// TestActivityIT_ReunionNotObservedRollsBack(I6):未锁存且没凑够人 → ThresholdNotReached,整事务回滚:
// 进度行不存在、不占次数、不加帮贡、不留指令行,事务内新建的 seq 行也随之消失。
func TestActivityIT_ReunionNotObservedRollsBack(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7651
		leader  uint64 = 8651
		player  uint64 = 8652
		opID    uint64 = 9_651_001
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	row := actReunionRow()
	in := actInput(row, guildID, player, testNowMs)
	in.Reward = actReward(t, opID, testNowMs)

	_, err := f.act.ClaimReunionTx(f.ctx, in, false)
	assert.ErrorIs(t, err, ErrActivityThresholdNotReached)
	actAssertUntouched(t, f, row, guildID, player, testNowMs, "未锁存且没凑够人")
	assert.False(t, econOpExists(t, f.econFixture, opID))
	assert.False(t, actCreditSeqRowExists(t, f, player), "事务内新建的 seq 行随回滚消失")
}

// TestActivityIT_ReunionLatchPersists(I7):第一人凑够人领取 → 锁存;之后另一人没凑够人也能领,
// threshold_reached_ms 保持首次值。团圆配了资金时也只在锁存那次发一次。
func TestActivityIT_ReunionLatchPersists(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7661
		leader  uint64 = 8661
		first   uint64 = 8662
		second  uint64 = 8663
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader,
		map[uint64]uint32{first: constants.RoleMember, second: constants.RoleMember})
	row := actReunionRow()
	row.GuildFunds = 100

	res, err := f.act.ClaimReunionTx(f.ctx, actInput(row, guildID, first, testNowMs), true)
	require.NoError(t, err)
	assert.True(t, res.ReachedNow)
	assert.True(t, res.FundsGranted)
	assert.Equal(t, activity.Progress{ThresholdReachedMs: testNowMs, FundsGranted: true}, res.Progress, "团圆不计 progress_count")

	res, err = f.act.ClaimReunionTx(f.ctx, actInput(row, guildID, second, testNowMs+5_000), false)
	require.NoError(t, err, "已锁存:之后领取不必再凑人")
	assert.False(t, res.ReachedNow)
	assert.False(t, res.FundsGranted)

	progress, found := actProgress(t, f, guildID, actKey(row, testNowMs))
	require.True(t, found)
	assert.Equal(t, activity.Progress{ThresholdReachedMs: testNowMs, FundsGranted: true}, progress, "锁存时刻保持首次值")
	assert.Equal(t, uint64(100), econFunds(t, f.econFixture, guildID))
	actAssertContribution(t, f, guildID, first, 30, 30, "第一人")
	actAssertContribution(t, f, guildID, second, 30, 30, "第二人")
}

// TestActivityIT_ReunionLatchSpansGameDays(I7b):档期行第 1 游戏日锁存;第 2 游戏日没凑够人照样能领,
// 第 1 天领过的人第 2 天也能再领(个人次数按游戏日)。
func TestActivityIT_ReunionLatchSpansGameDays(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7666
		leader  uint64 = 8666
		first   uint64 = 8667
		second  uint64 = 8668
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader,
		map[uint64]uint32{first: constants.RoleMember, second: constants.RoleMember})
	row := actScheduled(actReunionRow())
	day1, day2 := testNowMs, testNowMs+actDayMs

	_, err := f.act.ClaimReunionTx(f.ctx, actInput(row, guildID, first, day1), true)
	require.NoError(t, err)
	res, err := f.act.ClaimReunionTx(f.ctx, actInput(row, guildID, second, day2), false)
	require.NoError(t, err, "锁存跨游戏日有效")
	assert.False(t, res.ReachedNow)
	_, err = f.act.ClaimReunionTx(f.ctx, actInput(row, guildID, first, day2+1_000), false)
	require.NoError(t, err, "新游戏日个人次数重置")

	progress, found := actProgress(t, f, guildID, actKey(row, day2))
	require.True(t, found)
	assert.Equal(t, day1, progress.ThresholdReachedMs)
	for _, day := range []uint64{day1, day2} {
		used, found := actUsed(t, f, first, row.GetId(), actDayKey(day))
		require.True(t, found)
		assert.Equal(t, uint32(1), used)
	}
	actAssertContribution(t, f, guildID, first, 60, 60, "两天各领一次")
}

// TestActivityIT_RewardOpRowAndViewReads(I8 + 06 §6.8 第 3 / 7 步):带物品的领取插一行 ACTIVITY_REWARD 指令 ——
// GUILD_CREDIT 流、PENDING、永不中止、不带对侧账增量、payload 是物品 1 × 4;seq 行 next_seq=2。
// 随后经生产读路径看用量与发放状态:只数活动发奖行(同流的兑换行不算),背包满原因、24h 内的被拒原因。
func TestActivityIT_RewardOpRowAndViewReads(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7681
		leader  uint64 = 8681
		player  uint64 = 8682
		opID    uint64 = 9_681_001
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	row := actReunionRow()
	in := actInput(row, guildID, player, testNowMs)
	in.Reward = actReward(t, opID, testNowMs)

	res, err := f.act.ClaimReunionTx(f.ctx, in, true)
	require.NoError(t, err)
	assert.True(t, res.Enqueued)
	assert.Equal(t, uint64(1), res.Seq)
	assert.NotZero(t, res.StreamEpoch)

	rec := econRecord(t, f.econFixture, opID)
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, rec.GetKind())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD), rec.GetTxType())
	assert.Zero(t, rec.GetDeadlineMs(), "活动发奖永不中止")
	assert.Zero(t, rec.GetContributionDelta(), "帮贡已在入队事务里记完,终结不做对侧账")
	assert.Zero(t, rec.GetFundsDelta())
	assert.Zero(t, rec.GetRefCount(), "活动发奖不退任何计数")
	assert.Equal(t, row.GetId(), rec.GetRefId())
	assert.Equal(t, res.DayKey, rec.GetPeriodKey())
	assert.Equal(t, guildID, rec.GetGuildId())
	assert.Equal(t, player, rec.GetPlayerId())
	assert.Equal(t, res.Seq, rec.GetSeq())
	assert.Equal(t, res.StreamEpoch, rec.GetStreamEpoch())
	assert.Equal(t, in.Reward.LeaseUntilMs, rec.GetLeaseUntilMs())
	assert.Equal(t, in.Reward.LeaseUntilMs, rec.GetNextAttemptMs(), "同步投递握着租约期间重投循环不碰它")
	assert.Equal(t, in.Reward.LeaseToken, rec.GetLeaseToken())
	assert.Equal(t, testNowMs, rec.GetCreatedMs())
	var bundle assetpb.AssetBundle
	require.NoError(t, proto.Unmarshal(rec.GetPayload(), &bundle))
	assert.True(t, proto.Equal(actRewardBundle(), &bundle), "payload 应为物品 1 × 4,实际 %v", &bundle)
	assert.Equal(t, uint64(2), actCreditNextSeq(t, f, player))
	actAssertContribution(t, f, guildID, player, 30, 30, "团圆帮贡")

	usage, err := f.act.ActivityUsage(f.ctx, player, res.DayKey)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{row.GetId(): 1}, usage)

	// 同流上一行未决的兑换指令:不能被数进活动的待发放。
	econInsertOp(t, f.econFixture, econShopRecord(t, 9_681_900, player, res.StreamEpoch, 2))
	statuses, err := f.act.RewardStatuses(f.ctx, player, testNowMs)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]RewardStatus{row.GetId(): {PendingCount: 1}}, statuses)

	// 背包满:scene 回 RETRY + assetop.ReasonBagFull,行保持 PENDING、写 last_reason(06 §6.13 C6)。
	// 原因码一律取 assetop.Reason*(92 §12.1),不写数字:码由导表器发号,换号时这里跟着走。
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET last_reason=? WHERE op_id=?", assetop.ReasonBagFull, opID)
	statuses, err = f.act.RewardStatuses(f.ctx, player, testNowMs)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]RewardStatus{row.GetId(): {PendingCount: 1, PendingReasonTipID: assetop.ReasonBagFull}}, statuses)

	// 永久拒绝:终态行 next_attempt_ms = 终结时刻;24h 内显示原因,过了窗口不再显示。
	// 用 ReasonBlocked(货币或物品被封禁 → REJECTED,04 §4.10 原因表):它是真会落成 REJECTED 的码;
	// 冻结(ReasonFrozen)只会回 RETRY、行保持 PENDING,拿它造拒绝行是在测一个不可能出现的状态。
	finishedMs := testNowMs + 1_000
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET status=?, reason_tip_id=?, next_attempt_ms=? WHERE op_id=?",
		int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED), assetop.ReasonBlocked, finishedMs, opID)
	statuses, err = f.act.RewardStatuses(f.ctx, player, finishedMs+1_000)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]RewardStatus{row.GetId(): {LastRejectTipID: assetop.ReasonBlocked}}, statuses)
	statuses, err = f.act.RewardStatuses(f.ctx, player, finishedMs+rejectedRewardWindowMs+1)
	require.NoError(t, err)
	assert.Empty(t, statuses, "超过 24h 的拒绝不再显示")
}

// TestActivityIT_TooManyPendingRollsBack(I9):本人 GUILD_CREDIT 流已有 16 条本纪元未决行 → 领团圆回
// assetop.ErrTooManyPending(logic 回 GuildAssetPending);计数、帮贡、seq、进度都不动,指令行不落。
func TestActivityIT_TooManyPendingRollsBack(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7691
		leader  uint64 = 8691
		player  uint64 = 8692
		opBase  uint64 = 9_691_000
		epoch   uint64 = testNowMs - 100_000
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	maxPending := uint64(assetop.DefaultLimits.MaxPending)
	mustExec(t, f.ctx, f.db,
		"INSERT INTO guild_player_op_seq (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, ?, ?, ?)",
		player, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), maxPending+1, epoch, testNowMs)
	for i := range maxPending {
		econInsertOp(t, f.econFixture, econShopRecord(t, opBase+i, player, epoch, i+1))
	}

	row := actReunionRow()
	in := actInput(row, guildID, player, testNowMs)
	in.Reward = actReward(t, opBase+999, testNowMs)
	_, err := f.act.ClaimReunionTx(f.ctx, in, true)
	assert.ErrorIs(t, err, assetop.ErrTooManyPending)
	actAssertUntouched(t, f, row, guildID, player, testNowMs, "未决行过多")
	assert.False(t, econOpExists(t, f.econFixture, opBase+999))
	assert.Equal(t, maxPending+1, actCreditNextSeq(t, f, player), "seq 不前进")
}

// ── 失败与回滚(I10 / I11 / 业务拒绝) ────────────────────────

// TestActivityIT_OverflowIsPoisonAndRollsBack(I10):资金或帮贡相加会溢出 → ErrActivityPoison,全部回滚。
func TestActivityIT_OverflowIsPoisonAndRollsBack(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7701
		leader  uint64 = 8701
		first   uint64 = 8702
		second  uint64 = 8703
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader,
		map[uint64]uint32{first: constants.RoleMember, second: constants.RoleMember})
	row := actLanternRow()
	row.GuildThreshold = 1 // 第一盏灯就达标、发资金

	nearMaxFunds := uint64(math.MaxUint64 - 100)
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", nearMaxFunds, guildID)
	_, err := f.act.LightLanternTx(f.ctx, actInput(row, guildID, first, testNowMs))
	assert.ErrorIs(t, err, ErrActivityPoison, "资金溢出")
	assert.Equal(t, nearMaxFunds, econFunds(t, f.econFixture, guildID))
	actAssertUntouched(t, f, row, guildID, first, testNowMs, "资金溢出")

	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=0 WHERE guild_id=?", guildID)
	nearMaxContribution := uint64(math.MaxUint64 - 5)
	econSetContribution(t, f.econFixture, guildID, second, nearMaxContribution, 0)
	_, err = f.act.LightLanternTx(f.ctx, actInput(row, guildID, second, testNowMs))
	assert.ErrorIs(t, err, ErrActivityPoison, "帮贡溢出")
	assert.Zero(t, econFunds(t, f.econFixture, guildID), "帮贡溢出时资金也不能先发出去")
	_, found := actUsed(t, f, second, row.GetId(), actDayKey(testNowMs))
	assert.False(t, found)
	_, found = actProgress(t, f, guildID, actKey(row, testNowMs))
	assert.False(t, found)
	actAssertContribution(t, f, guildID, second, nearMaxContribution, 0, "帮贡溢出")
}

// TestActivityIT_MemberOrGuildGone(I11 / C13):预检之后被踢 → ErrNotGuildMember;帮会不存在 → ErrGuildGone。都不留痕。
func TestActivityIT_MemberOrGuildGone(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7711
		leader  uint64 = 8711
		player  uint64 = 8712
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	row := actLanternRow()
	mustExec(t, f.ctx, f.db, "DELETE FROM guild_member WHERE guild_id=? AND player_id=?", guildID, player)

	_, err := f.act.LightLanternTx(f.ctx, actInput(row, guildID, player, testNowMs))
	assert.ErrorIs(t, err, ErrNotGuildMember)
	_, found := actUsed(t, f, player, row.GetId(), actDayKey(testNowMs))
	assert.False(t, found)
	_, found = actProgress(t, f, guildID, actKey(row, testNowMs))
	assert.False(t, found)
	assert.False(t, actCreditSeqRowExists(t, f, player))

	_, err = f.act.LightLanternTx(f.ctx, actInput(row, 999_999, leader, testNowMs))
	assert.ErrorIs(t, err, ErrGuildGone)
}

// TestActivityIT_BusinessRejections:帮会等级不足、入帮未满 N 小时(恰好满算满)、合服闸门拒绝 / 读不出(fail-closed)。
func TestActivityIT_BusinessRejections(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7721
		leader  uint64 = 8721
		player  uint64 = 8722
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})

	highLevel := actLanternRow()
	highLevel.MinGuildLevel = 2
	_, err := f.act.LightLanternTx(f.ctx, actInput(highLevel, guildID, player, testNowMs))
	assert.ErrorIs(t, err, ErrGuildLevelTooLow)
	actAssertUntouched(t, f, highLevel, guildID, player, testNowMs, "帮会等级不足")

	row := actLanternRow()
	for _, fence := range []FenceFunc{
		func(context.Context, uint32) error { return ErrZoneMerging },
		func(context.Context, uint32) error { return errors.New("merge fence unreadable") },
	} {
		in := actInput(row, guildID, player, testNowMs)
		in.Fence = fence
		_, err = f.act.LightLanternTx(f.ctx, in)
		assert.ErrorIs(t, err, ErrZoneMerging, "闸门拒绝或读不出一律按合服中拒绝")
		actAssertUntouched(t, f, row, guildID, player, testNowMs, "合服闸门")
	}

	// seedMemberRow 的 join_time_ms = testNowMs;要求入帮满 1 小时。
	tooEarly := actInput(row, guildID, player, testNowMs+actHourMs-1)
	tooEarly.JoinMinHours = 1
	_, err = f.act.LightLanternTx(f.ctx, tooEarly)
	assert.ErrorIs(t, err, ErrActivityJoinTooRecent)
	actAssertUntouched(t, f, row, guildID, player, tooEarly.NowMs, "入帮未满 1 小时")

	exactly := actInput(row, guildID, player, testNowMs+actHourMs)
	exactly.JoinMinHours = 1
	_, err = f.act.LightLanternTx(f.ctx, exactly)
	require.NoError(t, err, "恰好满 1 小时算满")
}

// ── 解散(I12,X-14) ─────────────────────────────────────────

// TestActivityIT_DisbandDeletesProgressKeepsCountersAndOps(I12):解散删本帮全部进度行(多个活动、多个期),
// 别帮的进度行不动;计数行(按玩家)与活动发奖指令行(物品属于玩家)保留,指令行仍是永不中止的 PENDING。
func TestActivityIT_DisbandDeletesProgressKeepsCountersAndOps(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7731
		leader  uint64 = 8731
		first   uint64 = 8732
		second  uint64 = 8733
		otherID uint64 = 7732
		otherLd uint64 = 8741
		other   uint64 = 8742
		opID    uint64 = 9_731_001
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader,
		map[uint64]uint32{first: constants.RoleMember, second: constants.RoleMember})
	seedManagedGuild(t, f.ctx, f.db, otherID, 2, 1, 50, otherLd, map[uint64]uint32{other: constants.RoleMember})
	lantern, reunion := actLanternRow(), actReunionRow()
	day1, day2 := testNowMs, testNowMs+actDayMs

	withReward := actInput(lantern, guildID, first, day1)
	withReward.Reward = actReward(t, opID, day1)
	_, err := f.act.LightLanternTx(f.ctx, withReward)
	require.NoError(t, err)
	_, err = f.act.LightLanternTx(f.ctx, actInput(lantern, guildID, second, day1))
	require.NoError(t, err)
	_, err = f.act.ClaimReunionTx(f.ctx, actInput(reunion, guildID, first, day1), true)
	require.NoError(t, err)
	_, err = f.act.LightLanternTx(f.ctx, actInput(lantern, guildID, first, day2)) // 常开行第 2 天是新的一期
	require.NoError(t, err)
	_, err = f.act.LightLanternTx(f.ctx, actInput(lantern, otherID, other, day1))
	require.NoError(t, err)
	require.Equal(t, 3, actProgressRowCount(t, f, guildID), "前提:本帮有 3 行进度(两期灯会 + 一期团圆)")

	_, err = f.repo.DisbandGuild(f.ctx, guildID, leader, day2+1_000, nil)
	require.NoError(t, err)

	assert.Zero(t, actProgressRowCount(t, f, guildID), "解散删本帮全部进度行")
	assert.Equal(t, 1, actProgressRowCount(t, f, otherID), "别帮的进度行不动")
	for _, c := range []struct {
		player uint64
		row    *tablepb.GuildActivityTable
		day    uint64
	}{{first, lantern, day1}, {second, lantern, day1}, {first, reunion, day1}, {first, lantern, day2}} {
		used, found := actUsed(t, f, c.player, c.row.GetId(), actDayKey(c.day))
		assert.True(t, found, "计数行按玩家保留,删了就能'解散重建再领'")
		assert.Equal(t, uint32(1), used)
	}
	rec := econRecord(t, f.econFixture, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus(), "物品属于玩家,解散后照常投递")
	assert.Zero(t, rec.GetDeadlineMs(), "提前截止只作用于捐献")

	_, err = f.act.LightLanternTx(f.ctx, actInput(lantern, guildID, first, day2+2_000))
	assert.ErrorIs(t, err, ErrGuildGone)
}

// ── 与别的写事务并发(死锁回归) ────────────────────────────────

// TestActivityIT_ConcurrentLanternVersusDisband:4 名成员点灯(带物品)与帮主解散同时发生,多轮 ——
// 解散必成功;每盏灯要么成功、要么 GuildGone / NotGuildMember;结束后本帮不留进度行,成功者的计数与指令行保留;零死锁。
// 它钉的是 X-14 的位置:删进度行若排到删申请 / 提前截止之前,解散就会持着 P 回头取 A / O。
func TestActivityIT_ConcurrentLanternVersusDisband(t *testing.T) {
	f := openActivityFixture(t)
	const rounds = 5
	row := actLanternRow()

	watch := econWatchDeadlocks(t, f.econFixture)
	for round := range uint64(rounds) {
		guildID := 7800 + round
		leader := 8800 + round*10
		members := []uint64{leader + 1, leader + 2, leader + 3, leader + 4}
		roles := map[uint64]uint32{}
		for _, p := range members {
			roles[p] = constants.RoleMember
		}
		seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, roles)

		fns := []func() error{func() error {
			_, err := f.repo.DisbandGuild(f.ctx, guildID, leader, testNowMs, nil)
			return err
		}}
		opIDs := make([]uint64, len(members))
		for i, p := range members {
			opIDs[i] = 9_800_000 + p
			in := actInput(row, guildID, p, testNowMs)
			in.Reward = actReward(t, opIDs[i], testNowMs)
			fns = append(fns, func() error {
				_, err := f.act.LightLanternTx(f.ctx, in)
				return err
			})
		}
		errs := econRunTogether(fns...)

		require.NoError(t, errs[0], "第 %d 轮:解散本身必须成功", round)
		for i, err := range errs[1:] {
			p := members[i]
			_, counted := actUsed(t, f, p, row.GetId(), actDayKey(testNowMs))
			switch {
			case err == nil:
				assert.True(t, counted, "第 %d 轮玩家 %d:点成功的计数保留", round, p)
				assert.True(t, econOpExists(t, f.econFixture, opIDs[i]), "第 %d 轮玩家 %d:点成功的指令行保留", round, p)
			case errors.Is(err, ErrGuildGone), errors.Is(err, ErrNotGuildMember):
				assert.False(t, counted, "第 %d 轮玩家 %d:被解散挡下的不占次数", round, p)
				assert.False(t, econOpExists(t, f.econFixture, opIDs[i]), "第 %d 轮玩家 %d:被挡下的不留指令行", round, p)
			default:
				t.Errorf("第 %d 轮玩家 %d:只允许成功或帮会已解散,实际 %v", round, p, err)
			}
		}
		assert.Zero(t, actProgressRowCount(t, f, guildID), "第 %d 轮:解散之后本帮不留进度行", round)
	}
	watch.assertNone(t, f.ctx, "点灯 ‖ 解散")
}

// TestActivityIT_ConcurrentLanternVersusShopSamePlayer:同一玩家同时点灯(带物品)与 4 笔兑换预留 ——
// 两类事务都要 GUILD_CREDIT 流的 seq 行(点灯在 G → M 之后、兑换在 M 之后),全部成功、seq 恰为 {1..5};零死锁。
func TestActivityIT_ConcurrentLanternVersusShopSamePlayer(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7901
		leader  uint64 = 8901
		player  uint64 = 8902
		opBase  uint64 = 9_901_000
		shops          = 4
		cost    uint64 = 30
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	econSetContribution(t, f.econFixture, guildID, player, 1000, 1000)

	seqs := make([]uint64, shops+1)
	in := actInput(actLanternRow(), guildID, player, testNowMs)
	in.Reward = actReward(t, opBase, testNowMs)
	fns := []func() error{func() error {
		res, err := f.act.LightLanternTx(f.ctx, in)
		seqs[0] = res.Seq
		return err
	}}
	for i := range shops {
		order := econShopOrder(t, opBase+1+uint64(i), player, guildID, 1, cost, 0, testNowMs)
		fns = append(fns, func() error {
			res, err := f.econ.ReserveShopOrder(f.ctx, order)
			seqs[i+1] = res.Seq
			return err
		})
	}

	watch := econWatchDeadlocks(t, f.econFixture)
	errs := econRunTogether(fns...)
	watch.assertNone(t, f.ctx, "点灯 ‖ 兑换预留(同一玩家)")

	for i, err := range errs {
		require.NoError(t, err, "第 %d 路", i)
	}
	slices.Sort(seqs)
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, seqs, "CREDIT 流 seq 连续无空洞")
	assert.Equal(t, uint64(6), actCreditNextSeq(t, f, player))
	actAssertContribution(t, f, guildID, player, 1000+20, 1000+20-shops*cost, "点灯加帮贡、兑换扣可用帮贡")
}
