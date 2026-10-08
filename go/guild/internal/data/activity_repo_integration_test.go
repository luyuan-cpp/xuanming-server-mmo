//go:build integration

package data

// activity_repo_integration_test.go —— 帮会活动 repo 的真库测试:B6a 的元宵灯会 / 中秋团圆,B6b 的同道历练。
//
// 覆盖 06-activities.md §6.41 的 I1–I23,外加并发(同玩家多路、同帮多人越过阈值、点灯 ‖ 解散、点灯 ‖ 兑换预留;
// 历练:同一局多路结算、结算 ‖ 点灯 ‖ 兑换预留、结算 ‖ 解散、待入队转换 ‖ 兑换预留)。
// 历练部分(I13–I23)与设计正文不同的地方记在文件后半"同道历练"一节的开头。
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
// 历练用例名都以 TestActivityIT_Trial 开头(解散那一条是 TestActivityIT_DisbandDeletesStartedTrialBattles…),上面的 -run 已全部覆盖;
// 只跑历练可用 -run "TestActivityIT_Trial|TestActivityIT_DisbandDeletesStartedTrial"。
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
	"strings"
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

// ── 同道历练(B6b,I13–I23) ────────────────────────────────────
//
// 与 06 §6.41 的差别(都是落码时按 92-handoff §12.2 / §12.4 做的订正,见 activity_repo.go 文件头第 7–11 条):
//   - I12 / I17:历练对局行只在持有该帮 guild 行锁时才写。解散只删本帮**在途**(STARTED)行,已结算 / 已判过期的历史行保留;
//     帮会已解散时结算、标记、判过期都不落任何行(原设计是"单表补插 SETTLED/GUILD_GONE")。
//   - I21 的"跑一轮 OwedRewardLoop"在数据层就是 ConvertOwedReward:循环本身(翻页、计指标)归 logic。
//   - 登记是"锁 guild 行 → 缺行才插"的短事务,不是自动提交 upsert。

// actTrialOpID / actTrialReward:给名单里每个人预备一份物品指令参数,op_id = opBase + 名单下标 + 1。
func actTrialOpID(opBase uint64, index int) uint64 { return opBase + uint64(index) + 1 }

func actTrialReward(t *testing.T, nowMs, opBase uint64, candidates ...uint64) *TrialReward {
	t.Helper()
	payload, err := proto.Marshal(actRewardBundle())
	require.NoError(t, err)
	reward := &TrialReward{Payload: payload, LeaseUntilMs: nowMs + econLeaseMs}
	for i, playerID := range candidates {
		opID := actTrialOpID(opBase, i)
		reward.Ops = append(reward.Ops, TrialRewardOp{PlayerID: playerID, OpID: opID, LeaseToken: opID | 1<<62})
	}
	return reward
}

// actSeedTrialGuild 造一个 zone 2、1 级的帮会:帮主 + 若干普通成员。
func actSeedTrialGuild(t *testing.T, f actFixture, guildID, leader uint64, members ...uint64) {
	t.Helper()
	roles := make(map[uint64]uint32, len(members))
	for _, playerID := range members {
		roles[playerID] = constants.RoleMember
	}
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, roles)
}

// actTrialBattle 经生产读路径读一行对局;行不存在时 found=false。
func actTrialBattle(t *testing.T, f actFixture, battleID uint64) (TrialBattleRow, bool) {
	t.Helper()
	row, found, err := f.act.TrialBattle(f.ctx, battleID)
	require.NoError(t, err)
	return row, found
}

func actRequireTrialBattle(t *testing.T, f actFixture, battleID uint64) TrialBattleRow {
	t.Helper()
	row, found := actTrialBattle(t, f, battleID)
	require.True(t, found, "对局行 %d 应当存在", battleID)
	return row
}

// actSetUsed 直接写一行活动计数(造"今日次数已用若干次")。
func actSetUsed(t *testing.T, f actFixture, playerID uint64, activityID, dayKey, used uint32) {
	t.Helper()
	mustExec(t, f.ctx, f.db,
		"INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?, ?, ?, ?, ?, ?)",
		playerID, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY), activityID, dayKey, used, testNowMs)
}

// actAllOwed 经生产读路径翻页读出全部待入队物品行(顺带验证键集游标能读完、不重不漏)。
func actAllOwed(t *testing.T, f actFixture) []OwedReward {
	t.Helper()
	var (
		all    []OwedReward
		cursor OwedRewardCursor
	)
	for {
		page, err := f.act.ListOwedRewards(f.ctx, cursor, 2)
		require.NoError(t, err)
		all = append(all, page...)
		if len(page) < 2 {
			return all
		}
		cursor = page[len(page)-1].Cursor()
	}
}

// actFillCreditWindow 给玩家的 GUILD_CREDIT 流预置满额未决指令(兑换行),返回这些行所在的纪元:
// 之后该玩家再分 seq 会被守卫拒绝(assetop.ErrTooManyPending)。
func actFillCreditWindow(t *testing.T, f actFixture, playerID, opBase uint64) (epoch uint64) {
	t.Helper()
	epoch = testNowMs - 100_000
	maxPending := uint64(assetop.DefaultLimits.MaxPending)
	mustExec(t, f.ctx, f.db,
		"INSERT INTO guild_player_op_seq (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, ?, ?, ?)",
		playerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), maxPending+1, epoch, testNowMs)
	for i := range maxPending {
		econInsertOp(t, f.econFixture, econShopRecord(t, opBase+i, playerID, epoch, i+1))
	}
	return epoch
}

// actDumpTables 把与活动有关的表逐行逐列拍成文本,给"重放之后所有表逐字节相同"(I14)用。
func actDumpTables(t *testing.T, f actFixture) string {
	t.Helper()
	tables := []struct{ name, orderBy string }{
		{"guild", "guild_id"},
		{"guild_member", "guild_id, player_id"},
		{"guild_player_op_seq", "player_id, stream"},
		{"guild_asset_op", "op_id"},
		{"guild_daily_counter", "player_id, counter_kind, ref_id, period_key"},
		{"guild_activity_progress", "guild_id, activity_id, period_key"},
		{"guild_trial_battle", "battle_id"},
		{"guild_trial_reward_owed", "player_id, battle_id"},
	}
	var dump strings.Builder
	for _, table := range tables {
		rows, err := f.db.QueryContext(f.ctx, "SELECT * FROM "+table.name+" ORDER BY "+table.orderBy)
		require.NoError(t, err, table.name)
		columns, err := rows.Columns()
		require.NoError(t, err, table.name)
		values := make([]sql.RawBytes, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		fmt.Fprintf(&dump, "## %s\n", table.name)
		for rows.Next() {
			require.NoError(t, rows.Scan(dest...), table.name)
			for i, column := range columns {
				fmt.Fprintf(&dump, "%s=%x;", column, []byte(values[i]))
			}
			dump.WriteByte('\n')
		}
		require.NoError(t, rows.Err(), table.name)
		require.NoError(t, rows.Close(), table.name)
	}
	return dump.String()
}

// actTrialWinScenario 是 I13 / I14 共用的场景:4 名候选(乱序给出),其中 exhausted 当日次数已满;对局已登记;胜利结算带物品。
type actTrialWinScenario struct {
	guildID, leader, battleID, opBase uint64
	players                           []uint64 // 传给结算的顺序(故意乱序)
	exhausted                         uint64
	row                               *tablepb.GuildActivityTable
	startMs, nowMs                    uint64
	input                             TrialSettleInput
}

func actSetupTrialWin(t *testing.T, f actFixture) actTrialWinScenario {
	t.Helper()
	s := actTrialWinScenario{
		guildID: 7950, leader: 8950, battleID: 6_950_001, opBase: 9_950_000,
		players: []uint64{8954, 8952, 8951, 8953}, exhausted: 8954,
		row: actTrialRow(), startMs: testNowMs, nowMs: testNowMs + 90_000,
	}
	actSeedTrialGuild(t, f, s.guildID, s.leader, s.players...)
	key := actTrialKey(s.row, s.battleID, s.guildID, 8951, s.startMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, key, s.startMs))
	actSetUsed(t, f, s.exhausted, s.row.GetId(), key.PeriodKey, s.row.GetDailyLimit())
	s.input = TrialSettleInput{
		Battle: key, Activity: s.row, Win: true, Candidates: s.players,
		Reward:       actTrialReward(t, s.nowMs, s.opBase, s.players...),
		FinishedAtMs: s.nowMs - 1_000, NowMs: s.nowMs,
	}
	return s
}

// TestActivityIT_TrialWinRewardsEligibleMembers(I13):胜利、4 名候选、其中 1 人当日次数已满 ——
// 另 3 人各得帮贡 50、占一次当日次数、各有一行物品指令;帮会资金 +300、当天计资金胜场 1;对局行 SETTLED / WIN / 3 人。
// 次数已满的人什么都没有(但他的 seq 行作为计数守卫已建好、未推进)。闸门拿到的是事务内锁住的 zone。
func TestActivityIT_TrialWinRewardsEligibleMembers(t *testing.T) {
	f := openActivityFixture(t)
	s := actSetupTrialWin(t, f)
	in := s.input
	var fencedZone uint32
	in.Fence = func(_ context.Context, zoneID uint32) error {
		fencedZone = zoneID
		return nil
	}

	registered := actRequireTrialBattle(t, f, s.battleID)
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED, registered.State)
	assert.Equal(t, in.Battle, registered.TrialBattleKey, "登记行逐列等于开战上下文")
	assert.Equal(t, s.startMs, registered.CreatedMs)
	assert.Zero(t, registered.SettledMs)

	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), fencedZone, "合服闸门必须用事务内锁住的 guild.zone_id")
	assert.Equal(t, TrialSettleSettled, res.Status)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN, res.Result)
	rewarded := []uint64{8951, 8952, 8953}
	assert.Equal(t, rewarded, res.Rewarded, "得奖者升序,次数已满的人不在内")
	assert.Empty(t, res.Owed)
	assert.True(t, res.FundsGranted)
	assert.Equal(t, uint32(1), res.CountedWins)

	// 物品指令:每位得奖者一行,op_id 是调用方按人预备的那一个。
	require.Len(t, res.Enqueued, len(rewarded))
	for i, playerID := range rewarded {
		wantOp := actTrialOpID(s.opBase, slices.Index(s.players, playerID))
		got := res.Enqueued[i]
		assert.Equal(t, playerID, got.PlayerID)
		assert.Equal(t, wantOp, got.OpID)
		assert.Equal(t, uint64(1), got.Seq)
		assert.NotZero(t, got.StreamEpoch)
		assert.Equal(t, wantOp|1<<62, got.LeaseToken)

		rec := econRecord(t, f.econFixture, wantOp)
		assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, rec.GetKind())
		assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
		assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD), rec.GetTxType())
		assert.Zero(t, rec.GetDeadlineMs(), "活动发奖永不中止")
		assert.Zero(t, rec.GetContributionDelta(), "帮贡已在结算事务里记完,终结不做对侧账")
		assert.Zero(t, rec.GetFundsDelta())
		assert.Zero(t, rec.GetRefCount())
		assert.Equal(t, playerID, rec.GetPlayerId())
		assert.Equal(t, s.guildID, rec.GetGuildId())
		assert.Equal(t, s.row.GetId(), rec.GetRefId())
		assert.Equal(t, in.Battle.PeriodKey, rec.GetPeriodKey(), "记开战时的游戏日键")
		assert.Equal(t, got.Seq, rec.GetSeq())
		assert.Equal(t, got.StreamEpoch, rec.GetStreamEpoch())
		assert.Equal(t, in.Reward.LeaseUntilMs, rec.GetLeaseUntilMs())
		assert.Equal(t, in.Reward.LeaseUntilMs, rec.GetNextAttemptMs())
		assert.Equal(t, got.LeaseToken, rec.GetLeaseToken())
		assert.Equal(t, s.nowMs, rec.GetCreatedMs())
		assert.Equal(t, in.Reward.Payload, rec.GetPayload())

		actAssertContribution(t, f, s.guildID, playerID, 50, 50, "得奖者帮贡")
		used, found := actUsed(t, f, playerID, s.row.GetId(), in.Battle.PeriodKey)
		require.True(t, found)
		assert.Equal(t, uint32(1), used)
		assert.Equal(t, uint64(2), actCreditNextSeq(t, f, playerID))
	}

	actAssertContribution(t, f, s.guildID, s.exhausted, 0, 0, "次数已满的人不得帮贡")
	used, _ := actUsed(t, f, s.exhausted, s.row.GetId(), in.Battle.PeriodKey)
	assert.Equal(t, s.row.GetDailyLimit(), used, "次数已满的人计数不变")
	assert.Zero(t, actOpCountOfPlayer(t, f, s.exhausted))
	assert.Equal(t, uint64(1), actCreditNextSeq(t, f, s.exhausted), "seq 行作守卫建好了,但没分配就不推进")
	actAssertContribution(t, f, s.guildID, s.leader, 0, 0, "不在候选里的成员不受影响")

	assert.Equal(t, uint64(300), econFunds(t, f.econFixture, s.guildID))
	progress, found := actProgress(t, f, s.guildID, ProgressKey{ActivityID: s.row.GetId(), PeriodKey: in.Battle.GuildPeriodKey})
	require.True(t, found)
	assert.Equal(t, activity.Progress{Count: 1}, progress, "历练没有锁存与资金标记,只计胜场")

	settled := actRequireTrialBattle(t, f, s.battleID)
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, settled.State)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN, settled.Result)
	assert.Equal(t, uint32(3), settled.RewardedCount)
	assert.Equal(t, s.startMs, settled.CreatedMs, "已登记的行保留登记时刻")
	assert.Equal(t, s.nowMs, settled.SettledMs)
	assert.Empty(t, actAllOwed(t, f))
}

// TestActivityIT_TrialReplayIsDuplicate(I14,06 §6.34 T4):同一局再结算一次(换一批 op_id、晚一点的 now)→ Duplicate,
// 所有表与第一次结算之后逐字节相同。登记也可以重放:已结算的局再登记是空操作。
func TestActivityIT_TrialReplayIsDuplicate(t *testing.T) {
	f := openActivityFixture(t)
	s := actSetupTrialWin(t, f)
	first, err := f.act.SettleTrialBattleTx(f.ctx, s.input)
	require.NoError(t, err)
	require.Equal(t, TrialSettleSettled, first.Status)
	before := actDumpTables(t, f)

	replay := s.input
	replay.NowMs = s.nowMs + 30_000
	replay.Reward = actTrialReward(t, replay.NowMs, s.opBase+100, s.players...)
	res, err := f.act.SettleTrialBattleTx(f.ctx, replay)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleResult{Status: TrialSettleDuplicate}, res)

	// 重放的事件即便把胜负说反、候选说少,也改变不了已结算的局。
	lying := replay
	lying.Win, lying.Candidates, lying.Reward = false, s.players[:1], nil
	res, err = f.act.SettleTrialBattleTx(f.ctx, lying)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleDuplicate, res.Status)

	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, s.input.Battle, replay.NowMs), "已结算的局再登记是空操作")
	assert.Equal(t, before, actDumpTables(t, f), "重放不得改动任何一行")
}

// TestActivityIT_TrialDailyFundsCap(I15):每游戏日计资金胜场上限 3 —— 连胜 4 场,前 3 场各 +300,第 4 场帮贡照发、资金不加;
// 第二个游戏日开战的局记在新的一期,资金重新计。
func TestActivityIT_TrialDailyFundsCap(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 7960
		leader  uint64 = 8960
		first   uint64 = 8961
		second  uint64 = 8962
	)
	actSeedTrialGuild(t, f, guildID, leader, first, second)
	row := actTrialRow()
	row.DailyLimit = 10 // 个人次数放宽,只看帮会资金上限

	for i := range uint64(4) {
		now := testNowMs + i*60_000
		res, err := f.act.SettleTrialBattleTx(f.ctx, actTrialWin(row, 6_960_001+i, guildID, now, first, second))
		require.NoError(t, err, "第 %d 场", i+1)
		require.Equal(t, TrialSettleSettled, res.Status)
		assert.Equal(t, []uint64{first, second}, res.Rewarded)
		assert.Equal(t, i < 3, res.FundsGranted, "第 %d 场", i+1)
		assert.Equal(t, uint32(min(i+1, 3)), res.CountedWins, "第 %d 场", i+1)
		assert.Equal(t, min(i+1, 3)*300, econFunds(t, f.econFixture, guildID), "第 %d 场之后的帮会资金", i+1)
		actAssertContribution(t, f, guildID, first, (i+1)*50, (i+1)*50, "帮贡每场都发")
	}
	progress, found := actProgress(t, f, guildID, actKey(row, testNowMs))
	require.True(t, found)
	assert.Equal(t, uint32(3), progress.Count)

	nextDay := testNowMs + actDayMs
	res, err := f.act.SettleTrialBattleTx(f.ctx, actTrialWin(row, 6_960_100, guildID, nextDay, first, second))
	require.NoError(t, err)
	assert.True(t, res.FundsGranted, "新的一期重新计资金")
	assert.Equal(t, uint32(1), res.CountedWins)
	assert.Equal(t, uint64(4*300), econFunds(t, f.econFixture, guildID))
	assert.Equal(t, 2, actProgressRowCount(t, f, guildID), "历练进度行按游戏日一期一行")
}

// TestActivityIT_TrialLossOnlySettlesTheRow(I16):失败 / 平局只把对局行写成 SETTLED / LOSS,别的什么都不碰 ——
// 没有帮贡、计数、资金、进度行、指令行,连 seq 行都不建。这一局没登记过,所以对局行是补插的,登记时刻取对局结束时刻。
func TestActivityIT_TrialLossOnlySettlesTheRow(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 7970
		leader   uint64 = 8970
		first    uint64 = 8971
		second   uint64 = 8972
		battleID uint64 = 6_970_001
	)
	actSeedTrialGuild(t, f, guildID, leader, first, second)
	row := actTrialRow()
	in := actTrialWin(row, battleID, guildID, testNowMs, first, second)
	in.Win = false
	in.Reward = actTrialReward(t, testNowMs, 9_970_000, first, second)
	in.FinishedAtMs = testNowMs - 5_000

	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleResult{Status: TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS}, res)

	settled := actRequireTrialBattle(t, f, battleID)
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, settled.State)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS, settled.Result)
	assert.Zero(t, settled.RewardedCount)
	assert.Equal(t, in.FinishedAtMs, settled.CreatedMs, "补登记的行以对局结束时刻作登记时刻")
	assert.Equal(t, testNowMs, settled.SettledMs)
	assert.Equal(t, in.Battle, settled.TrialBattleKey)

	for _, playerID := range []uint64{first, second} {
		actAssertContribution(t, f, guildID, playerID, 0, 0, "输了没有帮贡")
		_, found := actUsed(t, f, playerID, row.GetId(), in.Battle.PeriodKey)
		assert.False(t, found, "输了不占当日次数")
		assert.False(t, actCreditSeqRowExists(t, f, playerID), "不发奖的局不碰 seq 行")
		assert.Zero(t, actOpCountOfPlayer(t, f, playerID))
	}
	assert.Zero(t, econFunds(t, f.econFixture, guildID))
	assert.Zero(t, actProgressRowCount(t, f, guildID))

	// 配表行没了(或类型不符,logic 传 nil):赢了也不发奖,结论记 CONFIG_MISSING。
	missing := actTrialWin(row, battleID+1, guildID, testNowMs, first, second)
	missing.Activity = nil
	res, err = f.act.SettleTrialBattleTx(f.ctx, missing)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleResult{Status: TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING}, res)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING, actRequireTrialBattle(t, f, battleID+1).Result)
	actAssertContribution(t, f, guildID, first, 0, 0, "配表缺行不发奖")
}

// TestActivityIT_TrialGuildGoneWritesNothing(I17,按落码订正):帮会不存在 → 结算回 GuildGone、不落任何行;重放同样。
// 登记回 ErrGuildGone,标记回 TrialMarkGuildGone,判过期回 false —— 没有 guild 行作守卫,就没有任何路径写对局表。
func TestActivityIT_TrialGuildGoneWritesNothing(t *testing.T) {
	f := openActivityFixture(t)
	const (
		ghostGuild uint64 = 7_980_999
		battleID   uint64 = 6_980_001
		player     uint64 = 8981
	)
	row := actTrialRow()
	in := actTrialWin(row, battleID, ghostGuild, testNowMs, player)
	in.Reward = actTrialReward(t, testNowMs, 9_980_000, player)
	before := actDumpTables(t, f)

	for range 2 {
		res, err := f.act.SettleTrialBattleTx(f.ctx, in)
		require.NoError(t, err)
		assert.Equal(t, TrialSettleResult{Status: TrialSettleGuildGone}, res)
	}
	assert.ErrorIs(t, f.act.RegisterTrialBattle(f.ctx, in.Battle, testNowMs), ErrGuildGone)
	outcome, err := f.act.MarkTrialBattlePoison(f.ctx, in.Battle, testNowMs, testNowMs)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkGuildGone, outcome)
	expired, err := f.act.ExpireTrialBattle(f.ctx, battleID, ghostGuild, testNowMs)
	require.NoError(t, err)
	assert.False(t, expired)

	_, found := actTrialBattle(t, f, battleID)
	assert.False(t, found, "帮会不存在时不补插对局行")
	assert.Equal(t, before, actDumpTables(t, f))
}

// TestActivityIT_TrialContextMismatch(I18):登记行属于甲帮,结果事件却说是乙帮 → ContextMismatch,
// 登记行不变,乙帮的人一分奖励都拿不到(乙帮的行也没被锁过、改过)。标记同理回 TrialMarkMismatch。
func TestActivityIT_TrialContextMismatch(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildA   uint64 = 7990
		leaderA  uint64 = 8990
		memberA  uint64 = 8991
		guildB   uint64 = 7991
		leaderB  uint64 = 8995
		memberB  uint64 = 8996
		battleID uint64 = 6_990_001
	)
	actSeedTrialGuild(t, f, guildA, leaderA, memberA)
	actSeedTrialGuild(t, f, guildB, leaderB, memberB)
	row := actTrialRow()
	keyA := actTrialKey(row, battleID, guildA, memberA, testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, keyA, testNowMs))
	before := actDumpTables(t, f)

	forged := actTrialWin(row, battleID, guildB, testNowMs+60_000, memberB)
	forged.Reward = actTrialReward(t, forged.NowMs, 9_990_000, memberB)
	res, err := f.act.SettleTrialBattleTx(f.ctx, forged)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleResult{Status: TrialSettleContextMismatch}, res)

	outcome, err := f.act.MarkTrialBattlePoison(f.ctx, forged.Battle, 0, forged.NowMs)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkMismatch, outcome)
	assert.Error(t, f.act.RegisterTrialBattle(f.ctx, forged.Battle, forged.NowMs), "同一 battle_id 不能再登记给别的帮")
	_, err = f.act.ExpireTrialBattle(f.ctx, battleID, guildB, forged.NowMs)
	assert.Error(t, err, "巡检器的键取自登记行本身,对不上是内部错误")

	assert.Equal(t, before, actDumpTables(t, f), "归属不符的事件不得改动任何一行")
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED, actRequireTrialBattle(t, f, battleID).State)
}

// TestActivityIT_TrialSettleWithoutRegistration(I19,06 §6.34 T1):match 返回之后、登记之前 guild 崩了,这一局没有登记行 ——
// 结算照常发奖,并补插一行 SETTLED。事件没带结束时刻时,登记时刻取结算时刻。
func TestActivityIT_TrialSettleWithoutRegistration(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8000
		leader   uint64 = 9000
		player   uint64 = 9001
		battleID uint64 = 6_000_001
	)
	actSeedTrialGuild(t, f, guildID, leader, player)
	row := actTrialRow()
	in := actTrialWin(row, battleID, guildID, testNowMs, player)
	in.FinishedAtMs = 0
	_, found := actTrialBattle(t, f, battleID)
	require.False(t, found, "前提:没有登记行")

	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleSettled, res.Status)
	assert.Equal(t, []uint64{player}, res.Rewarded)
	assert.Empty(t, res.Enqueued, "本局没配物品")

	settled := actRequireTrialBattle(t, f, battleID)
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, settled.State)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN, settled.Result)
	assert.Equal(t, uint32(1), settled.RewardedCount)
	assert.Equal(t, in.Battle, settled.TrialBattleKey)
	assert.Equal(t, testNowMs, settled.CreatedMs)
	assert.Equal(t, testNowMs, settled.SettledMs)
	actAssertContribution(t, f, guildID, player, 50, 50, "补登记的局照常发奖")
	assert.Equal(t, uint64(1), actCreditNextSeq(t, f, player), "没有物品时 seq 行只作守卫,不推进")
}

// TestActivityIT_TrialStaleCounterRetries(I20):结算读完计数之后、占用之前,有人绕过 seq 行守卫把其中一人的次数写满 ——
// 第一轮占用判到"达上限",整事务回滚重跑;第二轮读到新值,这个人不再得奖,其余人照常。
// 回滚的那一轮不留任何痕迹(帮贡、资金、指令行只发一次)。
func TestActivityIT_TrialStaleCounterRetries(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8010
		leader   uint64 = 9010
		first    uint64 = 9011
		racer    uint64 = 9012
		third    uint64 = 9013
		battleID uint64 = 6_010_001
		opBase   uint64 = 9_010_000
	)
	actSeedTrialGuild(t, f, guildID, leader, first, racer, third)
	row := actTrialRow()
	in := actTrialWin(row, battleID, guildID, testNowMs, first, racer, third)
	in.Reward = actTrialReward(t, testNowMs, opBase, first, racer, third)

	rounds := 0
	trialAfterCounterReadHook = func(context.Context) {
		rounds++
		if rounds == 1 {
			actSetUsed(t, f, racer, row.GetId(), in.Battle.PeriodKey, row.GetDailyLimit())
		}
	}
	t.Cleanup(func() { trialAfterCounterReadHook = nil })
	var retried []string
	originalObserved := txDeadlockObserved
	txDeadlockObserved = func(op string, _ error) { retried = append(retried, op) }
	t.Cleanup(func() { txDeadlockObserved = originalObserved })

	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, 2, rounds, "第一轮回滚、第二轮成功")
	assert.Equal(t, []string{opTrialSettle}, retried, "主动重跑经 inTx 的重试通道,计在 trial_settle 名下")
	assert.Equal(t, TrialSettleSettled, res.Status)
	assert.Equal(t, []uint64{first, third}, res.Rewarded)
	require.Len(t, res.Enqueued, 2)

	actAssertContribution(t, f, guildID, first, 50, 50, "帮贡只发一次")
	actAssertContribution(t, f, guildID, third, 50, 50, "帮贡只发一次")
	actAssertContribution(t, f, guildID, racer, 0, 0, "次数被写满的人不得奖")
	used, _ := actUsed(t, f, racer, row.GetId(), in.Battle.PeriodKey)
	assert.Equal(t, row.GetDailyLimit(), used)
	assert.Zero(t, actOpCountOfPlayer(t, f, racer))
	assert.Equal(t, 1, actOpCountOfPlayer(t, f, first), "回滚的那一轮不留指令行")
	assert.Equal(t, uint64(2), actCreditNextSeq(t, f, first), "seq 只前进一次")
	assert.Equal(t, uint64(300), econFunds(t, f.econFixture, guildID), "资金只发一次")
	assert.Equal(t, uint32(2), actRequireTrialBattle(t, f, battleID).RewardedCount)
}

// TestActivityIT_TrialOwedRewardIsDeferredNotSkipped(I21,06 §6.34 T8):候选人 blocked 的 GUILD_CREDIT 流已有 16 条未决指令 ——
// 结算不给他插指令行、不前进 seq,物品原样记进待入队表,帮贡与次数照发;另一位候选人不受影响。
// 窗口仍满时转换什么都不做,只把诊断计数 +1;腾出空位后转换成一行不持租约、立刻可领的指令(第 17 个 seq),待入队行删除;
// 再转一次发现行已不在。
func TestActivityIT_TrialOwedRewardIsDeferredNotSkipped(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8020
		leader   uint64 = 9020
		free     uint64 = 9021
		blocked  uint64 = 9022
		battleID uint64 = 6_020_001
		opBase   uint64 = 9_020_000
		shopBase uint64 = 9_020_500
		convert1 uint64 = 9_020_901
		convert2 uint64 = 9_020_902
		convert3 uint64 = 9_020_903
	)
	actSeedTrialGuild(t, f, guildID, leader, free, blocked)
	epoch := actFillCreditWindow(t, f, blocked, shopBase)
	maxPending := uint64(assetop.DefaultLimits.MaxPending)
	row := actTrialRow()
	in := actTrialWin(row, battleID, guildID, testNowMs, free, blocked)
	in.Reward = actTrialReward(t, testNowMs, opBase, free, blocked)

	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	require.Equal(t, TrialSettleSettled, res.Status)
	assert.Equal(t, []uint64{free, blocked}, res.Rewarded, "未决已满的人照样得奖,只是物品晚到")
	assert.Equal(t, []uint64{blocked}, res.Owed)
	require.Len(t, res.Enqueued, 1)
	assert.Equal(t, free, res.Enqueued[0].PlayerID)

	blockedOp := actTrialOpID(opBase, 1)
	assert.False(t, econOpExists(t, f.econFixture, blockedOp), "未决已满者不插指令行")
	assert.Equal(t, maxPending+1, actCreditNextSeq(t, f, blocked), "seq 不前进")
	actAssertContribution(t, f, guildID, blocked, 50, 50, "帮贡照发")
	used, found := actUsed(t, f, blocked, row.GetId(), in.Battle.PeriodKey)
	require.True(t, found)
	assert.Equal(t, uint32(1), used, "当日次数照占")

	owed := actAllOwed(t, f)
	require.Len(t, owed, 1)
	want := OwedReward{
		PlayerID: blocked, BattleID: battleID, GuildID: guildID, ActivityID: row.GetId(),
		PeriodKey: in.Battle.PeriodKey, Payload: in.Reward.Payload, Attempts: 0, CreatedMs: testNowMs,
	}
	assert.Equal(t, want, owed[0])
	counts, err := f.act.OwedRewardCounts(f.ctx, blocked)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{row.GetId(): 1}, counts, "视图把它算进待发放")
	counts, err = f.act.OwedRewardCounts(f.ctx, free)
	require.NoError(t, err)
	assert.Empty(t, counts)
	total, err := f.act.CountOwedRewards(f.ctx, 10_000)
	require.NoError(t, err)
	assert.Equal(t, 1, total)

	// 窗口仍满:不转,诊断计数 +1。
	later := testNowMs + 10_000
	status, err := f.act.ConvertOwedReward(f.ctx, owed[0], convert1, later)
	require.NoError(t, err)
	assert.Equal(t, OwedStillFull, status)
	assert.False(t, econOpExists(t, f.econFixture, convert1))
	assert.Equal(t, maxPending+1, actCreditNextSeq(t, f, blocked))
	owed = actAllOwed(t, f)
	require.Len(t, owed, 1)
	assert.Equal(t, uint32(1), owed[0].Attempts)

	// 腾出空位(16 条兑换到账)。
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET status=? WHERE player_id=? AND kind=?",
		int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), blocked, int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP))
	later += 10_000
	status, err = f.act.ConvertOwedReward(f.ctx, owed[0], convert2, later)
	require.NoError(t, err)
	assert.Equal(t, OwedConverted, status)

	rec := econRecord(t, f.econFixture, convert2)
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, rec.GetKind())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD), rec.GetTxType())
	assert.Equal(t, blocked, rec.GetPlayerId())
	assert.Equal(t, guildID, rec.GetGuildId())
	assert.Equal(t, row.GetId(), rec.GetRefId())
	assert.Equal(t, in.Battle.PeriodKey, rec.GetPeriodKey())
	assert.Equal(t, maxPending+1, rec.GetSeq(), "第 17 个 seq")
	assert.Equal(t, epoch, rec.GetStreamEpoch())
	assert.Equal(t, in.Reward.Payload, rec.GetPayload(), "物品包原样转入")
	assert.Zero(t, rec.GetDeadlineMs())
	assert.Zero(t, rec.GetLeaseUntilMs(), "不持租约")
	assert.Zero(t, rec.GetLeaseToken())
	assert.Equal(t, later, rec.GetNextAttemptMs(), "重投循环立刻可领")
	assert.Equal(t, later, rec.GetCreatedMs())
	assert.Equal(t, maxPending+2, actCreditNextSeq(t, f, blocked))
	assert.Empty(t, actAllOwed(t, f), "转换与删待入队行在同一事务里")
	due, err := f.store.ListDue(f.ctx, later, 100)
	require.NoError(t, err)
	assert.Contains(t, due, convert2, "转出来的指令行在到期列表里")

	// 别的实例拿着同一行再转:行已不在,刚插的指令行随事务回滚。
	status, err = f.act.ConvertOwedReward(f.ctx, owed[0], convert3, later+1)
	require.NoError(t, err)
	assert.Equal(t, OwedGone, status)
	assert.False(t, econOpExists(t, f.econFixture, convert3))
	assert.Equal(t, maxPending+2, actCreditNextSeq(t, f, blocked), "回滚的转换不烧 seq")
}

// TestActivityIT_TrialExpiredThenLateResult(I22):巡检器把无结果的局判为 EXPIRED;迟到的结果仍照常结算成 SETTLED / WIN。
// 判过期只作用于 STARTED:重复判、对已结算的局判都是空操作。
func TestActivityIT_TrialExpiredThenLateResult(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8030
		leader   uint64 = 9030
		player   uint64 = 9031
		battleID uint64 = 6_030_001
	)
	actSeedTrialGuild(t, f, guildID, leader, player)
	row := actTrialRow()
	key := actTrialKey(row, battleID, guildID, player, testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, key, testNowMs))

	expireAt := testNowMs + actHourMs
	expired, err := f.act.ExpireTrialBattle(f.ctx, battleID, guildID, expireAt)
	require.NoError(t, err)
	assert.True(t, expired)
	got := actRequireTrialBattle(t, f, battleID)
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_EXPIRED, got.State)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_UNSPECIFIED, got.Result)
	assert.Equal(t, expireAt, got.SettledMs)
	assert.Equal(t, testNowMs, got.CreatedMs)

	expired, err = f.act.ExpireTrialBattle(f.ctx, battleID, guildID, expireAt+1)
	require.NoError(t, err)
	assert.False(t, expired, "已不是 STARTED")
	missing, err := f.act.ExpireTrialBattle(f.ctx, battleID+99, guildID, expireAt)
	require.NoError(t, err)
	assert.False(t, missing, "没有这一行")

	lateAt := expireAt + actHourMs
	late := TrialSettleInput{Battle: key, Activity: row, Win: true, Candidates: []uint64{player}, FinishedAtMs: testNowMs + 60_000, NowMs: lateAt}
	res, err := f.act.SettleTrialBattleTx(f.ctx, late)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleSettled, res.Status)
	assert.Equal(t, []uint64{player}, res.Rewarded)
	got = actRequireTrialBattle(t, f, battleID)
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, got.State)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN, got.Result)
	assert.Equal(t, lateAt, got.SettledMs)
	assert.Equal(t, testNowMs, got.CreatedMs, "迟到结算不改登记时刻")
	actAssertContribution(t, f, guildID, player, 50, 50, "迟到的结果照常发奖")

	expired, err = f.act.ExpireTrialBattle(f.ctx, battleID, guildID, lateAt+1)
	require.NoError(t, err)
	assert.False(t, expired, "已结算的局不会被判回过期")
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, actRequireTrialBattle(t, f, battleID).State)
}

// TestActivityIT_TrialMarkPoison(I23):标记于 STARTED 行 → SETTLED / POISON、不发奖;于已结算的行 → 保持原样;
// 没有登记行时,上下文齐全就补插一行审计,只有键则无从补插。标记之后同一局的结果再来就是 Duplicate。
func TestActivityIT_TrialMarkPoison(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID   uint64 = 8040
		leader    uint64 = 9040
		player    uint64 = 9041
		started   uint64 = 6_040_001
		settled   uint64 = 6_040_002
		unknown   uint64 = 6_040_003
		keyOnly   uint64 = 6_040_004
		finishAt  uint64 = testNowMs + 30_000
		poisonAt  uint64 = testNowMs + 60_000
		expiredID uint64 = 6_040_005
	)
	actSeedTrialGuild(t, f, guildID, leader, player)
	row := actTrialRow()
	poisoned := pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_POISON
	settledState := pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED

	// STARTED 行。
	startedKey := actTrialKey(row, started, guildID, player, testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, startedKey, testNowMs))
	outcome, err := f.act.MarkTrialBattlePoison(f.ctx, startedKey, finishAt, poisonAt)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkDone, outcome)
	got := actRequireTrialBattle(t, f, started)
	assert.Equal(t, settledState, got.State)
	assert.Equal(t, poisoned, got.Result)
	assert.Zero(t, got.RewardedCount)
	assert.Equal(t, testNowMs, got.CreatedMs, "已登记的行保留登记时刻")
	assert.Equal(t, poisonAt, got.SettledMs)
	// 巡检器手里只有登记行的键:对已标记的行同样是"已结算"。
	outcome, err = f.act.MarkTrialBattlePoison(f.ctx, TrialBattleKey{BattleID: started, GuildID: guildID}, 0, poisonAt+1)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkAlreadySettled, outcome)
	// 标毒之后结果再来:Duplicate,不发奖。
	res, err := f.act.SettleTrialBattleTx(f.ctx, actTrialWin(row, started, guildID, poisonAt+2, player))
	require.NoError(t, err)
	assert.Equal(t, TrialSettleDuplicate, res.Status)
	actAssertContribution(t, f, guildID, player, 0, 0, "标毒的局不发奖")

	// 已正常结算的行:保持原样。
	_, err = f.act.SettleTrialBattleTx(f.ctx, actTrialWin(row, settled, guildID, testNowMs, player))
	require.NoError(t, err)
	before := actRequireTrialBattle(t, f, settled)
	outcome, err = f.act.MarkTrialBattlePoison(f.ctx, before.TrialBattleKey, finishAt, poisonAt)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkAlreadySettled, outcome)
	assert.Equal(t, before, actRequireTrialBattle(t, f, settled))
	actAssertContribution(t, f, guildID, player, 50, 50, "已发的奖不受标记影响")

	// EXPIRED 行也可以标(迟到的结果是毒消息)。
	expiredKey := actTrialKey(row, expiredID, guildID, player, testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, expiredKey, testNowMs))
	_, err = f.act.ExpireTrialBattle(f.ctx, expiredID, guildID, finishAt)
	require.NoError(t, err)
	outcome, err = f.act.MarkTrialBattlePoison(f.ctx, expiredKey, finishAt, poisonAt)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkDone, outcome)
	assert.Equal(t, poisoned, actRequireTrialBattle(t, f, expiredID).Result)

	// 没有登记行、上下文齐全:补插一行审计,登记时刻取对局结束时刻。
	unknownKey := actTrialKey(row, unknown, guildID, player, testNowMs)
	outcome, err = f.act.MarkTrialBattlePoison(f.ctx, unknownKey, finishAt, poisonAt)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkDone, outcome)
	got = actRequireTrialBattle(t, f, unknown)
	assert.Equal(t, settledState, got.State)
	assert.Equal(t, poisoned, got.Result)
	assert.Equal(t, unknownKey, got.TrialBattleKey)
	assert.Equal(t, finishAt, got.CreatedMs)
	assert.Equal(t, poisonAt, got.SettledMs)

	// 没有登记行、只有键:无从补插。
	outcome, err = f.act.MarkTrialBattlePoison(f.ctx, TrialBattleKey{BattleID: keyOnly, GuildID: guildID}, 0, poisonAt)
	require.NoError(t, err)
	assert.Equal(t, TrialMarkMissing, outcome)
	_, found := actTrialBattle(t, f, keyOnly)
	assert.False(t, found)
}

// TestActivityIT_TrialDeterministicFailuresRollBack:三种确定性失败 —— 结果过旧(周期键已落进计数行清理可能触及的范围)、
// 帮贡溢出、资金溢出 —— 都回 ErrActivityPoison,整体回滚(对局行仍是 STARTED,交给 MarkTrialBattlePoison)。
// 过旧只卡发奖的局:同一把旧键的败局照常结算(它不写计数行)。
func TestActivityIT_TrialDeterministicFailuresRollBack(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 8050
		leader  uint64 = 9050
		first   uint64 = 9051
		second  uint64 = 9052
		tooOld  uint64 = 6_050_001
		contrib uint64 = 6_050_002
		funds   uint64 = 6_050_003
		opBase  uint64 = 9_050_000
	)
	actSeedTrialGuild(t, f, guildID, leader, first, second)
	row := actTrialRow()
	started := pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED

	// 九天前开战的局现在才到。
	nowMs := testNowMs + 9*actDayMs
	oldKey := actTrialKey(row, tooOld, guildID, first, testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, oldKey, testNowMs))
	stale := TrialSettleInput{Battle: oldKey, Activity: row, Win: true, Candidates: []uint64{first, second},
		Reward: actTrialReward(t, nowMs, opBase, first, second), FinishedAtMs: testNowMs + 60_000, NowMs: nowMs}
	before := actDumpTables(t, f)
	_, err := f.act.SettleTrialBattleTx(f.ctx, stale)
	assert.ErrorIs(t, err, ErrActivityPoison, "结果过旧")
	assert.NotErrorIs(t, err, ErrTrialInputInvalid)
	assert.Equal(t, before, actDumpTables(t, f), "过旧的胜局什么都不写")
	assert.Equal(t, started, actRequireTrialBattle(t, f, tooOld).State)
	// 同一把旧键的败局不写计数行,不受"过旧"限制。
	staleLoss := stale
	staleLoss.Win = false
	res, err := f.act.SettleTrialBattleTx(f.ctx, staleLoss)
	require.NoError(t, err)
	assert.Equal(t, pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS, res.Result)

	// 帮贡溢出:第二个人的累计帮贡加 50 会回绕。第一个人此前的写一并回滚。
	nearMax := uint64(math.MaxUint64 - 5)
	econSetContribution(t, f.econFixture, guildID, second, nearMax, 0)
	contribIn := actTrialWin(row, contrib, guildID, testNowMs, first, second)
	contribIn.Reward = actTrialReward(t, testNowMs, opBase+10, first, second)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, contribIn.Battle, testNowMs))
	_, err = f.act.SettleTrialBattleTx(f.ctx, contribIn)
	assert.ErrorIs(t, err, ErrActivityPoison, "帮贡溢出")
	actAssertContribution(t, f, guildID, first, 0, 0, "同局其他人的帮贡一并回滚")
	actAssertContribution(t, f, guildID, second, nearMax, 0, "溢出者帮贡不变")
	_, found := actUsed(t, f, first, row.GetId(), contribIn.Battle.PeriodKey)
	assert.False(t, found, "计数一并回滚")
	assert.Zero(t, actOpCountOfPlayer(t, f, first), "指令行一并回滚")
	assert.False(t, actCreditSeqRowExists(t, f, first), "首次建出的 seq 行也随事务回滚")
	assert.Equal(t, started, actRequireTrialBattle(t, f, contrib).State)

	// 资金溢出。
	econSetContribution(t, f.econFixture, guildID, second, 0, 0)
	nearMaxFunds := uint64(math.MaxUint64 - 100)
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", nearMaxFunds, guildID)
	fundsIn := actTrialWin(row, funds, guildID, testNowMs, first, second)
	_, err = f.act.SettleTrialBattleTx(f.ctx, fundsIn)
	assert.ErrorIs(t, err, ErrActivityPoison, "资金溢出")
	assert.Equal(t, nearMaxFunds, econFunds(t, f.econFixture, guildID))
	actAssertContribution(t, f, guildID, first, 0, 0, "资金溢出时帮贡也不能先发出去")
	assert.Zero(t, actProgressRowCount(t, f, guildID), "进度行一并回滚")
	_, found = actTrialBattle(t, f, funds)
	assert.False(t, found, "没登记过的局回滚后不留对局行")
}

// TestActivityIT_TrialMembershipAndFence:结算时已不在本帮的候选人没有奖励(战后退帮 / 被踢),也不为他建 seq 行;
// 全员都不在了仍结算成 WIN、0 人得奖。合服闸门拒绝或读不出 → ErrZoneMerging,什么都不写(消费者稍后重试)。
func TestActivityIT_TrialMembershipAndFence(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8060
		leader   uint64 = 9060
		stays    uint64 = 9061
		left     uint64 = 9062
		outsider uint64 = 9069 // 从来不是本帮成员
		battleID uint64 = 6_060_001
	)
	actSeedTrialGuild(t, f, guildID, leader, stays, left)
	row := actTrialRow()
	in := actTrialWin(row, battleID, guildID, testNowMs, stays, left, outsider)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, in.Battle, testNowMs))

	before := actDumpTables(t, f)
	for _, fence := range []FenceFunc{
		func(context.Context, uint32) error { return ErrZoneMerging },
		func(context.Context, uint32) error { return errors.New("merge fence unreadable") },
	} {
		fenced := in
		fenced.Fence = fence
		_, err := f.act.SettleTrialBattleTx(f.ctx, fenced)
		assert.ErrorIs(t, err, ErrZoneMerging, "闸门拒绝或读不出一律按合服中拒绝")
		assert.Equal(t, before, actDumpTables(t, f))
	}

	mustExec(t, f.ctx, f.db, "DELETE FROM guild_member WHERE guild_id=? AND player_id=?", guildID, left)
	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleSettled, res.Status)
	assert.Equal(t, []uint64{stays}, res.Rewarded)
	assert.Equal(t, uint32(1), actRequireTrialBattle(t, f, battleID).RewardedCount)
	for _, gone := range []uint64{left, outsider} {
		_, found := actUsed(t, f, gone, row.GetId(), in.Battle.PeriodKey)
		assert.False(t, found, "玩家 %d 不在帮,不占次数", gone)
		assert.False(t, actCreditSeqRowExists(t, f, gone), "玩家 %d 不在帮,不建 seq 行", gone)
	}

	nobody := actTrialWin(row, battleID+1, guildID, testNowMs, left, outsider)
	res, err = f.act.SettleTrialBattleTx(f.ctx, nobody)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleSettled, res.Status)
	assert.Empty(t, res.Rewarded)
	assert.False(t, res.FundsGranted, "没人得奖的胜场不计帮会资金")
	assert.Equal(t, uint64(300), econFunds(t, f.econFixture, guildID), "只有第一局计了资金")
}

// TestActivityIT_TrialRegisterIsIdempotent:登记只在缺行时插一行;重复登记、登记一局已被判过期的对局都不改任何东西。
func TestActivityIT_TrialRegisterIsIdempotent(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8070
		leader   uint64 = 9070
		player   uint64 = 9071
		battleID uint64 = 6_070_001
	)
	actSeedTrialGuild(t, f, guildID, leader, player)
	row := actTrialRow()
	key := actTrialKey(row, battleID, guildID, player, testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, key, testNowMs))
	first := actRequireTrialBattle(t, f, battleID)
	assert.Equal(t, TrialBattleRow{TrialBattleKey: key, State: pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED, CreatedMs: testNowMs}, first)

	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, key, testNowMs+5_000))
	assert.Equal(t, first, actRequireTrialBattle(t, f, battleID), "重复登记不改登记时刻")

	_, err := f.act.ExpireTrialBattle(f.ctx, battleID, guildID, testNowMs+actHourMs)
	require.NoError(t, err)
	expiredRow := actRequireTrialBattle(t, f, battleID)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, key, testNowMs+2*actHourMs))
	assert.Equal(t, expiredRow, actRequireTrialBattle(t, f, battleID), "登记不会把 EXPIRED 改回 STARTED")
}

// TestActivityIT_TrialSweeperAndRosterReads:巡检候选只含登记早于给定时刻、仍为 STARTED 的局,按登记时刻升序、受 limit 限制;
// 名单核对只返回此刻在本帮的人及其入帮时刻。
func TestActivityIT_TrialSweeperAndRosterReads(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID uint64 = 8080
		leader  uint64 = 9080
		first   uint64 = 9081
		second  uint64 = 9082
		other   uint64 = 8081
		otherLd uint64 = 9090
		otherMb uint64 = 9091
	)
	actSeedTrialGuild(t, f, guildID, leader, first, second)
	actSeedTrialGuild(t, f, other, otherLd, otherMb)
	row := actTrialRow()
	register := func(battleID, guild, initiator, startMs uint64) {
		t.Helper()
		require.NoError(t, f.act.RegisterTrialBattle(f.ctx, actTrialKey(row, battleID, guild, initiator, startMs), startMs))
	}
	register(101, guildID, first, testNowMs+3_000)
	register(102, guildID, first, testNowMs+1_000)
	register(103, other, otherMb, testNowMs+2_000)
	register(104, guildID, first, testNowMs+1_000) // 与 102 同一时刻:按 battle_id 排
	register(105, guildID, first, testNowMs+9_000) // 还没超时
	register(106, guildID, first, testNowMs+500)   // 已结算,不在候选里
	_, err := f.act.SettleTrialBattleTx(f.ctx, actTrialWin(row, 106, guildID, testNowMs+600, first))
	require.NoError(t, err)
	register(107, guildID, first, testNowMs+700) // 已判过期,不在候选里
	_, err = f.act.ExpireTrialBattle(f.ctx, 107, guildID, testNowMs+800)
	require.NoError(t, err)

	overdue, err := f.act.ListOverdueTrialBattles(f.ctx, testNowMs+5_000, 50)
	require.NoError(t, err)
	assert.Equal(t, []OverdueTrialBattle{
		{BattleID: 102, GuildID: guildID, CreatedMs: testNowMs + 1_000},
		{BattleID: 104, GuildID: guildID, CreatedMs: testNowMs + 1_000},
		{BattleID: 103, GuildID: other, CreatedMs: testNowMs + 2_000},
		{BattleID: 101, GuildID: guildID, CreatedMs: testNowMs + 3_000},
	}, overdue)
	limited, err := f.act.ListOverdueTrialBattles(f.ctx, testNowMs+5_000, 2)
	require.NoError(t, err)
	assert.Equal(t, overdue[:2], limited)
	none, err := f.act.ListOverdueTrialBattles(f.ctx, testNowMs+5_000, 0)
	require.NoError(t, err)
	assert.Empty(t, none)

	// seedMemberRow 的 join_time_ms = testNowMs。
	members, err := f.act.TrialRosterMembers(f.ctx, guildID, []uint64{second, first, otherMb, 9_999_999})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint64{first: testNowMs, second: testNowMs}, members, "别帮的人与不存在的人不在返回里")
}

// ── 解散(I12 的历练部分,按落码订正)────────────────────────────

// TestActivityIT_DisbandDeletesStartedTrialBattlesKeepsHistoryAndOwed:解散删本帮**在途**(STARTED)对局行;
// 已结算 / 已判过期的历史行、别帮的行、待入队物品行都保留。解散之后这一帮的结果再来回 GuildGone,不落任何行。
func TestActivityIT_DisbandDeletesStartedTrialBattlesKeepsHistoryAndOwed(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8100
		leader   uint64 = 9100
		free     uint64 = 9101
		blocked  uint64 = 9102
		otherID  uint64 = 8101
		otherLd  uint64 = 9110
		otherMb  uint64 = 9111
		settled  uint64 = 6_100_001
		expired  uint64 = 6_100_002
		inFlight uint64 = 6_100_003
		another  uint64 = 6_100_004
		foreign  uint64 = 6_100_005
		opBase   uint64 = 9_100_000
	)
	actSeedTrialGuild(t, f, guildID, leader, free, blocked)
	actSeedTrialGuild(t, f, otherID, otherLd, otherMb)
	actFillCreditWindow(t, f, blocked, 9_100_500)
	row := actTrialRow()

	win := actTrialWin(row, settled, guildID, testNowMs, free, blocked)
	win.Reward = actTrialReward(t, testNowMs, opBase, free, blocked)
	res, err := f.act.SettleTrialBattleTx(f.ctx, win)
	require.NoError(t, err)
	require.Equal(t, []uint64{blocked}, res.Owed, "前提:有一行待入队物品")
	for _, battleID := range []uint64{expired, inFlight, another} {
		require.NoError(t, f.act.RegisterTrialBattle(f.ctx, actTrialKey(row, battleID, guildID, free, testNowMs), testNowMs))
	}
	_, err = f.act.ExpireTrialBattle(f.ctx, expired, guildID, testNowMs+actHourMs)
	require.NoError(t, err)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, actTrialKey(row, foreign, otherID, otherMb, testNowMs), testNowMs))

	_, err = f.repo.DisbandGuild(f.ctx, guildID, leader, testNowMs+2*actHourMs, nil)
	require.NoError(t, err)

	for _, battleID := range []uint64{inFlight, another} {
		_, found := actTrialBattle(t, f, battleID)
		assert.False(t, found, "在途对局行 %d 随解散删除", battleID)
	}
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, actRequireTrialBattle(t, f, settled).State, "已结算的历史行保留")
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_EXPIRED, actRequireTrialBattle(t, f, expired).State, "已判过期的历史行保留")
	assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED, actRequireTrialBattle(t, f, foreign).State, "别帮的在途行不动")
	overdue, err := f.act.ListOverdueTrialBattles(f.ctx, testNowMs+actDayMs, 50)
	require.NoError(t, err)
	assert.Equal(t, []OverdueTrialBattle{{BattleID: foreign, GuildID: otherID, CreatedMs: testNowMs}}, overdue,
		"解散的帮不在巡检器的扫描里留任何行")

	owed := actAllOwed(t, f)
	require.Len(t, owed, 1, "待入队物品属于玩家,解散不删")
	assert.Equal(t, blocked, owed[0].PlayerID)
	assert.True(t, econOpExists(t, f.econFixture, actTrialOpID(opBase, 0)), "已入队的物品指令照常保留")

	// 在途的那一局结果迟到:帮会已不在,不发奖也不落行。
	late := actTrialWin(row, inFlight, guildID, testNowMs+3*actHourMs, free)
	late.Battle = actTrialKey(row, inFlight, guildID, free, testNowMs)
	res, err = f.act.SettleTrialBattleTx(f.ctx, late)
	require.NoError(t, err)
	assert.Equal(t, TrialSettleResult{Status: TrialSettleGuildGone}, res)
	_, found := actTrialBattle(t, f, inFlight)
	assert.False(t, found)

	// 解散之后,待入队物品在窗口腾出后照样能转(玩家已不在任何帮)。
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET status=? WHERE player_id=? AND kind=?",
		int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), blocked, int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP))
	status, err := f.act.ConvertOwedReward(f.ctx, owed[0], opBase+900, testNowMs+4*actHourMs)
	require.NoError(t, err)
	assert.Equal(t, OwedConverted, status)
	assert.Equal(t, guildID, econRecord(t, f.econFixture, opBase+900).GetGuildId(), "指令行记的仍是发奖时的帮会")
}

// ── 历练的并发(死锁回归)────────────────────────────────────────

// TestActivityIT_TrialConcurrentDuplicateSettlesOnce(06 §6.34 T5):同一局的结果被 6 路同时结算(battle 重发、Kafka 重放、
// 巡检器与消费者撞在一起)—— 恰好一路结算,其余 Duplicate;奖励只发一次;零死锁。
func TestActivityIT_TrialConcurrentDuplicateSettlesOnce(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8200
		leader   uint64 = 9200
		battleID uint64 = 6_200_001
		opBase   uint64 = 9_200_000
		routes          = 6
	)
	players := []uint64{9201, 9202, 9203}
	actSeedTrialGuild(t, f, guildID, leader, players...)
	row := actTrialRow()
	key := actTrialKey(row, battleID, guildID, players[0], testNowMs)
	require.NoError(t, f.act.RegisterTrialBattle(f.ctx, key, testNowMs))

	statuses := make([]TrialSettleStatus, routes)
	fns := make([]func() error, routes)
	for i := range routes {
		in := TrialSettleInput{Battle: key, Activity: row, Win: true, Candidates: players,
			Reward: actTrialReward(t, testNowMs, opBase+uint64(i)*100, players...), FinishedAtMs: testNowMs, NowMs: testNowMs + uint64(i)}
		fns[i] = func() error {
			res, err := f.act.SettleTrialBattleTx(f.ctx, in)
			statuses[i] = res.Status
			return err
		}
	}
	watch := econWatchDeadlocks(t, f.econFixture)
	errs := econRunTogether(fns...)
	watch.assertNone(t, f.ctx, "同一局并发结算")

	settledRoutes := 0
	for i, err := range errs {
		require.NoError(t, err, "第 %d 路", i)
		switch statuses[i] {
		case TrialSettleSettled:
			settledRoutes++
		case TrialSettleDuplicate:
		default:
			t.Errorf("第 %d 路:只允许 Settled 或 Duplicate,实际 %d", i, statuses[i])
		}
	}
	assert.Equal(t, 1, settledRoutes, "恰好一路结算")
	for _, playerID := range players {
		actAssertContribution(t, f, guildID, playerID, 50, 50, "帮贡只发一次")
		assert.Equal(t, 1, actOpCountOfPlayer(t, f, playerID), "物品指令只有一行")
		assert.Equal(t, uint64(2), actCreditNextSeq(t, f, playerID))
		used, _ := actUsed(t, f, playerID, row.GetId(), key.PeriodKey)
		assert.Equal(t, uint32(1), used)
	}
	assert.Equal(t, uint64(300), econFunds(t, f.econFixture, guildID), "资金只计一次")
	assert.Equal(t, uint32(3), actRequireTrialBattle(t, f, battleID).RewardedCount)
}

// TestActivityIT_TrialConcurrentWithLanternAndShop:同一帮的 4 名成员,同时发生一局历练结算(带物品)、每人一盏灯(带物品)、
// 每人一笔兑换预留,多轮 —— 三类事务都要成员行与 GUILD_CREDIT 流的 seq 行,结算还要一次锁住全部四人。
// 全部成功;每人 CREDIT 流恰好分到 seq {1,2,3};帮贡、资金对得上;零死锁。
// 它钉的是结算"M 全部 → Q 全部 → O"的取锁次序:与单人事务的"M → Q → O"交错时不得成环。
func TestActivityIT_TrialConcurrentWithLanternAndShop(t *testing.T) {
	f := openActivityFixture(t)
	const (
		rounds        = 4
		cost   uint64 = 30
	)
	trial, lantern := actTrialRow(), actLanternRow()
	lantern.GuildThreshold = 100 // 不让灯会资金干扰资金断言

	watch := econWatchDeadlocks(t, f.econFixture)
	for round := range uint64(rounds) {
		guildID := 8300 + round
		leader := 9300 + round*10
		members := []uint64{leader + 1, leader + 2, leader + 3, leader + 4}
		actSeedTrialGuild(t, f, guildID, leader, members...)
		for _, playerID := range members {
			econSetContribution(t, f.econFixture, guildID, playerID, 1000, 1000)
		}
		opBase := 9_300_000 + round*1_000
		battleID := 6_300_001 + round

		settle := actTrialWin(trial, battleID, guildID, testNowMs, members...)
		settle.Reward = actTrialReward(t, testNowMs, opBase, members...)
		var settled TrialSettleResult
		fns := []func() error{func() error {
			res, err := f.act.SettleTrialBattleTx(f.ctx, settle)
			settled = res
			return err
		}}
		for i, playerID := range members {
			light := actInput(lantern, guildID, playerID, testNowMs)
			light.Reward = actReward(t, opBase+100+uint64(i), testNowMs)
			order := econShopOrder(t, opBase+200+uint64(i), playerID, guildID, 1, cost, 0, testNowMs)
			fns = append(fns,
				func() error {
					_, err := f.act.LightLanternTx(f.ctx, light)
					return err
				},
				func() error {
					_, err := f.econ.ReserveShopOrder(f.ctx, order)
					return err
				})
		}
		errs := econRunTogether(fns...)

		for i, err := range errs {
			require.NoError(t, err, "第 %d 轮第 %d 路", round, i)
		}
		require.Equal(t, TrialSettleSettled, settled.Status, "第 %d 轮", round)
		assert.Equal(t, members, settled.Rewarded, "第 %d 轮", round)
		assert.Len(t, settled.Enqueued, len(members), "第 %d 轮", round)
		for _, playerID := range members {
			assert.Equal(t, uint64(4), actCreditNextSeq(t, f, playerID), "第 %d 轮玩家 %d:三行指令,seq 连续", round, playerID)
			assert.Equal(t, 3, actOpCountOfPlayer(t, f, playerID), "第 %d 轮玩家 %d", round, playerID)
			actAssertContribution(t, f, guildID, playerID, 1000+50+20, 1000+50+20-cost, "历练 +50、点灯 +20、兑换扣 30")
		}
		assert.Equal(t, uint64(300), econFunds(t, f.econFixture, guildID), "第 %d 轮", round)
	}
	watch.assertNone(t, f.ctx, "历练结算 ‖ 点灯 ‖ 兑换预留")
}

// TestActivityIT_TrialConcurrentSettleVersusDisband:一局已登记的历练结果到达与帮主解散同时发生,多轮 ——
// 解散必成功;结算要么先成(对局行 SETTLED,作为历史行留下)、要么回 GuildGone(在途行已随解散删除,不落行、不留指令行);
// 两种先后之下本帮都不留 STARTED 行;零死锁。它钉的是 X-14 的位置:删对局行排在提前截止之后、删 guild 行之前。
func TestActivityIT_TrialConcurrentSettleVersusDisband(t *testing.T) {
	f := openActivityFixture(t)
	const rounds = 6
	row := actTrialRow()

	watch := econWatchDeadlocks(t, f.econFixture)
	for round := range uint64(rounds) {
		guildID := 8400 + round
		leader := 9400 + round*10
		members := []uint64{leader + 1, leader + 2, leader + 3}
		actSeedTrialGuild(t, f, guildID, leader, members...)
		battleID := 6_400_001 + round
		opBase := 9_400_000 + round*100
		in := actTrialWin(row, battleID, guildID, testNowMs, members...)
		in.Reward = actTrialReward(t, testNowMs, opBase, members...)
		require.NoError(t, f.act.RegisterTrialBattle(f.ctx, in.Battle, testNowMs))

		var settled TrialSettleResult
		errs := econRunTogether(
			func() error {
				_, err := f.repo.DisbandGuild(f.ctx, guildID, leader, testNowMs, nil)
				return err
			},
			func() error {
				res, err := f.act.SettleTrialBattleTx(f.ctx, in)
				settled = res
				return err
			})
		require.NoError(t, errs[0], "第 %d 轮:解散本身必须成功", round)
		require.NoError(t, errs[1], "第 %d 轮:结算不得报错", round)

		got, found := actTrialBattle(t, f, battleID)
		switch settled.Status {
		case TrialSettleSettled:
			require.True(t, found, "第 %d 轮:先结算的局留下历史行", round)
			assert.Equal(t, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, got.State, "第 %d 轮", round)
			for i := range members {
				assert.True(t, econOpExists(t, f.econFixture, actTrialOpID(opBase, i)), "第 %d 轮:已入队的物品属于玩家,解散后保留", round)
			}
		case TrialSettleGuildGone:
			assert.False(t, found, "第 %d 轮:在途行随解散删除,迟到的结果不补行", round)
			for i := range members {
				assert.False(t, econOpExists(t, f.econFixture, actTrialOpID(opBase, i)), "第 %d 轮:帮会已解散不发物品", round)
			}
		default:
			t.Errorf("第 %d 轮:只允许 Settled 或 GuildGone,实际 %d", round, settled.Status)
		}
	}
	overdue, err := f.act.ListOverdueTrialBattles(f.ctx, testNowMs+actDayMs, 50)
	require.NoError(t, err)
	assert.Empty(t, overdue, "解散之后不留任何 STARTED 行")
	watch.assertNone(t, f.ctx, "历练结算 ‖ 解散")
}

// TestActivityIT_TrialConcurrentOwedConvert:两个实例同时转同一行待入队物品,同一玩家还有一笔兑换预留在抢同一条 seq 流 ——
// 恰好一个转换成功、另一个发现行已不在;待入队行只变成一行指令;seq 连续;零死锁。
func TestActivityIT_TrialConcurrentOwedConvert(t *testing.T) {
	f := openActivityFixture(t)
	const (
		guildID  uint64 = 8500
		leader   uint64 = 9500
		blocked  uint64 = 9501
		battleID uint64 = 6_500_001
		opBase   uint64 = 9_500_000
		cost     uint64 = 30
	)
	actSeedTrialGuild(t, f, guildID, leader, blocked)
	actFillCreditWindow(t, f, blocked, 9_500_500)
	econSetContribution(t, f.econFixture, guildID, blocked, 1000, 1000)
	maxPending := uint64(assetop.DefaultLimits.MaxPending)
	row := actTrialRow()
	in := actTrialWin(row, battleID, guildID, testNowMs, blocked)
	in.Reward = actTrialReward(t, testNowMs, opBase, blocked)
	res, err := f.act.SettleTrialBattleTx(f.ctx, in)
	require.NoError(t, err)
	require.Equal(t, []uint64{blocked}, res.Owed)
	owed := actAllOwed(t, f)
	require.Len(t, owed, 1)
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET status=? WHERE player_id=? AND kind=?",
		int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), blocked, int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP))

	later := testNowMs + 10_000
	statuses := make([]OwedConvertStatus, 2)
	order := econShopOrder(t, opBase+300, blocked, guildID, 1, cost, 0, later)
	watch := econWatchDeadlocks(t, f.econFixture)
	errs := econRunTogether(
		func() error {
			status, err := f.act.ConvertOwedReward(f.ctx, owed[0], opBase+100, later)
			statuses[0] = status
			return err
		},
		func() error {
			status, err := f.act.ConvertOwedReward(f.ctx, owed[0], opBase+200, later)
			statuses[1] = status
			return err
		},
		func() error {
			_, err := f.econ.ReserveShopOrder(f.ctx, order)
			return err
		})
	watch.assertNone(t, f.ctx, "待入队转换 ‖ 待入队转换 ‖ 兑换预留")

	for i, err := range errs {
		require.NoError(t, err, "第 %d 路", i)
	}
	slices.Sort(statuses)
	assert.Equal(t, []OwedConvertStatus{OwedConverted, OwedGone}, statuses, "恰好一个转换成功")
	assert.Empty(t, actAllOwed(t, f))
	converted := 0
	for _, opID := range []uint64{opBase + 100, opBase + 200} {
		if econOpExists(t, f.econFixture, opID) {
			converted++
		}
	}
	assert.Equal(t, 1, converted, "待入队行只变成一行指令")
	assert.Equal(t, maxPending+3, actCreditNextSeq(t, f, blocked), "转换一行 + 兑换一行,回滚的那次不烧 seq")
}

// TestActivityIT_TrialTablesShape 钉住历练两表经 schemamigrate 建出来的键与索引(与 asset_tables_shape_test.go 同一做法)。
// 为什么对着真库断言:索引名是 proto2mysql 按声明顺序发的 idx_<表>_<n>(从 0 起);解散的候选读靠 (guild_id, state)、
// 巡检靠 (state, created_ms)、待入队翻页靠 (created_ms),列序错了它们会退回去扫全表;而已存在的表 schemamigrate
// 不会补建普通索引(只告警),所以这两张表第一次建就必须是对的。枚举列必须是整数列:业务 SQL 把生成常量当整数绑定。
func TestActivityIT_TrialTablesShape(t *testing.T) {
	f := openActivityFixture(t)
	type indexShape struct {
		columns []string
		unique  bool
	}
	want := map[string]map[string]indexShape{
		guildTrialBattleTable: {
			"PRIMARY":                  {[]string{"battle_id"}, true},
			"idx_guild_trial_battle_0": {[]string{"guild_id", "state"}, false},
			"idx_guild_trial_battle_1": {[]string{"state", "created_ms"}, false},
		},
		guildTrialRewardOwedTable: {
			"PRIMARY":                       {[]string{"player_id", "battle_id"}, true},
			"idx_guild_trial_reward_owed_0": {[]string{"created_ms"}, false},
		},
	}
	for table, wantIndexes := range want {
		rows, err := f.db.QueryContext(f.ctx,
			"SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE FROM INFORMATION_SCHEMA.STATISTICS"+
				" WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? ORDER BY INDEX_NAME, SEQ_IN_INDEX", table)
		require.NoError(t, err, table)
		got := map[string]indexShape{}
		for rows.Next() {
			var indexName, column string
			var nonUnique int
			require.NoError(t, rows.Scan(&indexName, &column, &nonUnique))
			shape := got[indexName]
			shape.columns = append(shape.columns, column)
			shape.unique = nonUnique == 0
			got[indexName] = shape
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		assert.Equal(t, wantIndexes, got, "%s 的索引集合、列序与唯一性", table)
	}
	for _, col := range []struct{ table, column string }{
		{guildTrialBattleTable, "state"},
		{guildTrialBattleTable, "settle_result"},
	} {
		var dataType string
		require.NoError(t, f.db.QueryRowContext(f.ctx,
			"SELECT DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
			col.table, col.column).Scan(&dataType), "%s.%s", col.table, col.column)
		assert.Equal(t, "int", dataType, "%s.%s 应为整数列", col.table, col.column)
	}
}
