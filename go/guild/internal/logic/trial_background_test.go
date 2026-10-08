package logic

// 同道历练两个后台循环的单测(06-activities.md §6.40 的 trial_background_test.go 部分)。
//
// 只测单轮函数(owedRewardLoop.round / trialSweepRound):轮内的"现在"取注入时钟,结果由返回的小结与替身上的记录断言,
// 不起定时器、不 sleep。循环外壳(定时、recover、停机)另有一条只验"能停下来"的用例。
// 替身与夹具(fakeTrialStore / trialFixture / seqMinter …)在 activity_trial_test.go。

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"guild/internal/data"
)

// trialSweepConf 是用例共用的巡检节律:结果超时 420s、放弃 3600s(设计默认值)。
func trialSweepConf() TrialBackgroundConf {
	return TrialBackgroundConf{
		OwedInterval:  10 * time.Second,
		SweepInterval: 60 * time.Second,
		ResultOverdue: 420 * time.Second,
		Abandon:       3600 * time.Second,
		Owner:         "guild-test-1",
	}
}

// overdueRow 造一条"登记于 age 之前、仍为 STARTED"的巡检候选。
func overdueRow(battleID uint64, age time.Duration) data.OverdueTrialBattle {
	return data.OverdueTrialBattle{BattleID: battleID, GuildID: trialTestGuildID, CreatedMs: activityTestNowMs() - uint64(age/time.Millisecond)}
}

// ── 巡检器 ────────────────────────────────────────────────────

// TestTrialSweepRound(06 §6.40):三条超时仍为 STARTED 的对局 ——
// 有结果记录的按记录结算(并销账);没有记录且登记已超过放弃时限的判 EXPIRED;没有记录但还没到时限的只计入 overdue。
func TestTrialSweepRound(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	f.store.overdue = []data.OverdueTrialBattle{
		overdueRow(1002, 4000*time.Second), // 没有记录,已超过 3600s → 判过期
		overdueRow(1001, 500*time.Second),  // 有记录 → 结算
		overdueRow(1003, 500*time.Second),  // 没有记录,还没到放弃时限 → 只计数
	}
	f.putResult(t, trialEvent(1001, 42, 43))

	stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

	assert.Equal(t, trialSweepStats{ran: true, recovered: 1, expired: 1, overdue: 1}, stats)
	require.Equal(t, []uint64{activityTestNowMs() - 420_000}, f.store.overdueBefore, "候选 = 登记早于 now − ResultOverdue")
	require.Len(t, f.store.settleInputs, 1)
	assert.Equal(t, uint64(1001), f.store.settleInputs[0].Battle.BattleID, "从结果记录取回的事件走同一条结算路径")
	assert.False(t, f.hasResult(1001), "结算后销账")
	assert.Equal(t, []uint64{1002}, f.store.expired)
}

// TestTrialSweepLease(06 §6.33):多副本靠租约省掉重复劳动 —— 租约被别的实例占着,本轮不查库;
// 自己拿到时写下 owner,有效期略短于巡检间隔(租约不主动释放,必须在下一轮之前自然到期)。
func TestTrialSweepLease(t *testing.T) {
	const leaseKey = "guild:trial:sweep:lease"

	t.Run("租约在别的实例手里:本轮不扫", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		f.store.overdue = []data.OverdueTrialBattle{overdueRow(1002, 4000*time.Second)}
		require.NoError(t, f.lobbies.Set(leaseKey, "another-instance"))

		stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

		assert.Equal(t, trialSweepStats{}, stats)
		assert.Empty(t, f.store.overdueBefore, "没拿到租约不查库")
		assert.Empty(t, f.store.expired)
	})
	t.Run("拿到租约:写下 owner 与有效期", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)

		stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

		assert.True(t, stats.ran)
		owner, err := f.lobbies.Get(leaseKey)
		require.NoError(t, err)
		assert.Equal(t, "guild-test-1", owner)
		assert.Equal(t, 55*time.Second, f.lobbies.TTL(leaseKey), "租约 = 巡检间隔 − 5s")
	})
	t.Run("租约 Redis 不可用:本轮跳过", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		f.store.overdue = []data.OverdueTrialBattle{overdueRow(1002, 4000*time.Second)}
		f.lobbies.Close()

		stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

		assert.False(t, stats.ran)
		assert.Empty(t, f.store.expired)
	})
	t.Run("没有房间依赖时不抢租约,照常巡检", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Lobby, d.Match = nil, nil })
		f.store.overdue = []data.OverdueTrialBattle{overdueRow(1002, 4000*time.Second)}

		stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

		assert.Equal(t, trialSweepStats{ran: true, expired: 1}, stats, "关掉历练之后,在途对局照样要巡检")
		assert.False(t, f.lobbies.Exists(leaseKey))
	})
}

// TestSweepLeaseTTL:租约比间隔短 5s;间隔很短时不低于间隔的一半(不会算出 0 或负数)。
func TestSweepLeaseTTL(t *testing.T) {
	assert.Equal(t, 55*time.Second, sweepLeaseTTL(60*time.Second))
	assert.Equal(t, 5*time.Second, sweepLeaseTTL(10*time.Second))
	assert.Equal(t, 3*time.Second, sweepLeaseTTL(6*time.Second))
}

// TestTrialSweepNeverExpiresOnFailedRead:结果记录读不到(Redis 故障)时本轮到此为止 ——
// 读不到不等于没有记录,绝不能据此把一局早该过期的对局判成过期(它的结果可能就躺在那台读不到的 Redis 里)。
func TestTrialSweepNeverExpiresOnFailedRead(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Lobby, d.Match = nil, nil })
	f.store.overdue = []data.OverdueTrialBattle{overdueRow(1002, 4000*time.Second)}
	f.locator.Close()

	stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

	assert.True(t, stats.ran)
	assert.Zero(t, stats.expired)
	assert.Empty(t, f.store.expired)

	t.Run("没接结果记录时巡检器空转", func(t *testing.T) {
		g := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Results = nil })
		g.store.overdue = []data.OverdueTrialBattle{overdueRow(1002, 4000*time.Second)}

		stats := g.l.trialSweepRound(context.Background(), g.deps, trialSweepConf())

		assert.Equal(t, trialSweepStats{}, stats)
		assert.Empty(t, g.store.overdueBefore)
	})
}

// TestTrialSweepPoisonsUnusableRecords:结果记录解不开,或它描述的不是这一局(battle_id 不符 / 不是历练的上下文)——
// 确定性的坏数据。把这一局标成 POISON 并销账;否则它永远停在候选里(有记录就不会被判过期)。
func TestTrialSweepPoisonsUnusableRecords(t *testing.T) {
	foreign := trialEvent(4444, 42, 43) // 记录里写的是另一局
	noContext := trialEvent(1001, 42, 43)
	noContext.ActivityContext = nil

	cases := []struct {
		name string
		put  func(t *testing.T, f *trialFixture)
	}{
		{"记录解不开", func(t *testing.T, f *trialFixture) {
			// 0x08 = 字段 1 的 varint 标签后面什么都没有,proto.Unmarshal 必定失败。
			require.NoError(t, f.locator.Set(trialResultKey(1001), string([]byte{0x08})))
		}},
		{"记录是另一局的", func(t *testing.T, f *trialFixture) {
			// 把另一局(4444)的字节放到本局(1001)的键下。
			raw, err := proto.Marshal(foreign)
			require.NoError(t, err)
			require.NoError(t, f.locator.Set(trialResultKey(1001), string(raw)))
		}},
		{"记录没有历练上下文", func(t *testing.T, f *trialFixture) { f.putResult(t, noContext) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			f.store.overdue = []data.OverdueTrialBattle{overdueRow(1001, 500*time.Second)}
			tc.put(t, f)

			stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

			assert.Equal(t, 1, stats.recovered, "这条记录处理完了")
			assert.Zero(t, f.store.settleCount(), "坏记录不进结算")
			assert.Equal(t, []data.TrialBattleKey{{BattleID: 1001, GuildID: trialTestGuildID}}, f.store.marked,
				"巡检器只有登记行的键,按它标毒")
			assert.False(t, f.hasResult(1001), "标毒后销账")
		})
	}
}

// TestTrialSweepKeepsRecordOnTemporaryFailure:从记录结算时遇到暂时性失败 → 这一局本轮不算处理完,
// 记录原样留着,下一轮再来。
func TestTrialSweepKeepsRecordOnTemporaryFailure(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	f.store.overdue = []data.OverdueTrialBattle{overdueRow(1001, 500*time.Second)}
	f.putResult(t, trialEvent(1001, 42, 43))
	f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
		return data.TrialSettleResult{}, data.ErrWriteConflict
	}

	stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

	assert.Equal(t, trialSweepStats{ran: true}, stats)
	assert.True(t, f.hasResult(1001))
	assert.Empty(t, f.store.marked)
}

// TestTrialSweepSurvivesPanicInOneBattle:从结果记录结算某一局时 panic,只丢这一局(本轮不算处理完,记录原样留着),
// 排在它后面的候选照常结算、照常判过期。候选按登记时刻升序,出问题的局每轮都排在同一个位置;
// 让它中断整轮,后面的局就永远得不到兜底(06 §6.35a 场景一"被跳过的那一局不再挡住别的对局"靠的就是这一条)。
func TestTrialSweepSurvivesPanicInOneBattle(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	f.store.overdue = []data.OverdueTrialBattle{
		overdueRow(1001, 5000*time.Second), // 有记录,结算时 panic
		overdueRow(1004, 600*time.Second),  // 有记录 → 照常结算
		overdueRow(1002, 4000*time.Second), // 没有记录,已超过放弃时限 → 照常判过期
	}
	f.putResult(t, trialEvent(1001, 42, 43))
	f.putResult(t, trialEvent(1004, 42, 43))
	f.store.settle = func(in data.TrialSettleInput) (data.TrialSettleResult, error) {
		if in.Battle.BattleID == 1001 {
			panic("nil map write in settlement")
		}
		return data.TrialSettleResult{Status: data.TrialSettleSettled}, nil
	}

	stats := f.l.trialSweepRound(context.Background(), f.deps, trialSweepConf())

	assert.Equal(t, trialSweepStats{ran: true, recovered: 1, expired: 1}, stats)
	assert.True(t, f.hasResult(1001), "panic 的那一局没有定论,记录留着下一轮再试")
	assert.False(t, f.hasResult(1004), "排在后面的局照常结算并销账")
	assert.Equal(t, []uint64{1002}, f.store.expired)
}

// TestTrialSweepStopsOnCancel:ctx 已取消时不处理任何候选。
func TestTrialSweepStopsOnCancel(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Lobby, d.Match = nil, nil })
	f.store.overdue = []data.OverdueTrialBattle{overdueRow(1002, 4000*time.Second)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats := f.l.trialSweepRound(ctx, f.deps, trialSweepConf())

	assert.Zero(t, stats.expired)
	assert.Empty(t, f.store.expired)
}

// ── 待入队物品循环 ────────────────────────────────────────────

// owedRow 造一行待入队物品;seq 决定它在表里的先后(created_ms 递增)。
func owedRow(seq int, playerID, battleID uint64) data.OwedReward {
	return data.OwedReward{
		PlayerID: playerID, BattleID: battleID, GuildID: trialTestGuildID, ActivityID: trialTestActivityID,
		PeriodKey: 20260928, Payload: []byte{0x0a, 0x00}, CreatedMs: activityTestNowMs() - 60_000 + uint64(seq),
	}
}

// newOwedLoop 建一个资产通道齐全(Loop + 发号器)的夹具与它的待入队循环。
func newOwedLoop(t *testing.T, minter *seqMinter) (*trialFixture, *owedRewardLoop) {
	t.Helper()
	useTrialTables(t, nil, nil)
	loop, _, _ := newCountingLoop(t)
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Loop, d.OpIDs = loop, minter })
	return f, &owedRewardLoop{d: f.deps}
}

// TestOwedRoundStillFullThenConverted(06 §6.40):某位玩家未决指令仍满 → 他的行原样留着(下一轮再试),
// 不挡后面的人;窗口腾出来之后的那一轮转换成功,行被删掉。每次转换各用一个新发的指令号。
func TestOwedRoundStillFullThenConverted(t *testing.T) {
	minter := &seqMinter{next: 7001}
	f, loop := newOwedLoop(t, minter)
	f.store.owedRows = []data.OwedReward{owedRow(1, 50, 9001), owedRow(2, 51, 9002)}
	stillFull := map[uint64]bool{50: true}
	f.store.convert = func(owed data.OwedReward) (data.OwedConvertStatus, error) {
		if stillFull[owed.PlayerID] {
			return data.OwedStillFull, nil
		}
		return data.OwedConverted, nil
	}

	first := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 2, stillFull: 1, converted: 1}, first)
	require.Len(t, f.store.owedRows, 1, "仍满的那一行不删")
	assert.Equal(t, uint64(50), f.store.owedRows[0].PlayerID)

	stillFull[50] = false
	second := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 1, converted: 1}, second)
	assert.Empty(t, f.store.owedRows)
	require.Len(t, f.store.converted, 3)
	assert.Equal(t, []uint64{7001, 7002, 7003}, []uint64{f.store.converted[0].opID, f.store.converted[1].opID, f.store.converted[2].opID})
	assert.Equal(t, f.nowMs(), f.store.converted[0].nowMs, "转换用注入时钟的现在")
}

// TestOwedRoundSkipsRestOfFullPlayer:同一位玩家本轮已判"仍满",他名下其余的行本轮不再尝试(同一个窗口,结论相同)。
func TestOwedRoundSkipsRestOfFullPlayer(t *testing.T) {
	f, loop := newOwedLoop(t, &seqMinter{next: 1})
	f.store.owedRows = []data.OwedReward{owedRow(1, 50, 9001), owedRow(2, 50, 9002), owedRow(3, 51, 9003)}
	f.store.convert = func(owed data.OwedReward) (data.OwedConvertStatus, error) {
		if owed.PlayerID == 50 {
			return data.OwedStillFull, nil
		}
		return data.OwedConverted, nil
	}

	stats := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 3, stillFull: 1, converted: 1}, stats)
	assert.Len(t, f.store.converted, 2, "50 的第二行没有再开事务")
}

// TestOwedRoundMintFailureEndsRoundAndRetriesSameRow:发号失败 → 本轮到此为止(换一行也发不出号);
// 游标停在这一行之前,下一轮从它重试,不会被跳过。
func TestOwedRoundMintFailureEndsRoundAndRetriesSameRow(t *testing.T) {
	minter := &seqMinter{err: errors.New("segment fenced")}
	f, loop := newOwedLoop(t, minter)
	f.store.owedRows = []data.OwedReward{owedRow(1, 50, 9001), owedRow(2, 51, 9002)}

	failed := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 1}, failed)
	assert.Empty(t, f.store.converted)
	assert.Len(t, f.store.owedRows, 2)

	minter.err, minter.next = nil, 1
	recovered := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 2, converted: 2}, recovered)
	require.Len(t, f.store.converted, 2)
	assert.Equal(t, uint64(9001), f.store.converted[0].owed.BattleID, "上一轮没发出号的那一行先重试")
}

// TestOwedRoundRowOutcomes:单行转换出错只记一次失败、接着看下一行;别的副本已转走(Gone)不算失败。
func TestOwedRoundRowOutcomes(t *testing.T) {
	f, loop := newOwedLoop(t, &seqMinter{next: 1})
	f.store.owedRows = []data.OwedReward{owedRow(1, 50, 9001), owedRow(2, 51, 9002), owedRow(3, 52, 9003)}
	f.store.convert = func(owed data.OwedReward) (data.OwedConvertStatus, error) {
		switch owed.PlayerID {
		case 50:
			return 0, data.ErrWriteConflict
		case 51:
			return data.OwedGone, nil
		default:
			return data.OwedConverted, nil
		}
	}

	stats := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 3, failed: 1, gone: 1, converted: 1}, stats)
	require.Len(t, f.store.owedRows, 1, "出错的那一行留着,等游标转回来再试")
	assert.Equal(t, uint64(50), f.store.owedRows[0].PlayerID)
}

// TestOwedRoundSkippedWhenChannelUnavailable:资产通道关闭、或发号器没接线时不转换(转了没人投递 / 发不出号),
// 行原样留着;积压量照常上报。
func TestOwedRoundSkippedWhenChannelUnavailable(t *testing.T) {
	cases := []struct {
		name               string
		withLoop, withMint bool
	}{
		{"资产通道关闭", false, true},
		{"发号器未接线", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			assetLoop, _, _ := newCountingLoop(t)
			f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) {
				if tc.withLoop {
					d.Loop = assetLoop
				}
				if tc.withMint {
					d.OpIDs = &seqMinter{next: 1}
				}
			})
			f.store.owedRows = []data.OwedReward{owedRow(1, 50, 9001)}
			loop := &owedRewardLoop{d: f.deps}

			stats := loop.round(context.Background())

			assert.Equal(t, owedRoundStats{skipped: true}, stats)
			assert.Empty(t, f.store.listCalls)
			assert.Empty(t, f.store.converted)
			assert.Len(t, f.store.owedRows, 1)
			assert.Equal(t, 1, f.store.countCalls, "积压量照常上报")
		})
	}
}

// TestOwedRoundPagesAndResumes:积压超过单轮上限时,一轮只看 owedMaxRowsPerRound 行;下一轮从游标处接着看,
// 读到表尾回到表头。队头是一批长期仍满的玩家时,排在后面的人不会被饿死。
func TestOwedRoundPagesAndResumes(t *testing.T) {
	f, loop := newOwedLoop(t, &seqMinter{next: 1})
	const total = owedMaxRowsPerRound + 30
	rows := make([]data.OwedReward, 0, total)
	for i := 1; i <= total; i++ {
		rows = append(rows, owedRow(i, uint64(1000+i), uint64(9000+i)))
	}
	// 替身删行时会原地改动切片,给它一份拷贝,rows 留作断言的参照。
	f.store.owedRows = slices.Clone(rows)
	// 前 owedMaxRowsPerRound 位玩家一直仍满;排在最后的 30 位可以转换。
	f.store.convert = func(owed data.OwedReward) (data.OwedConvertStatus, error) {
		if owed.PlayerID <= uint64(1000+owedMaxRowsPerRound) {
			return data.OwedStillFull, nil
		}
		return data.OwedConverted, nil
	}

	first := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: owedMaxRowsPerRound, stillFull: owedMaxRowsPerRound}, first, "第一轮被封顶,全是仍满的队头")
	require.Len(t, f.store.listCalls, owedMaxRowsPerRound/owedPageSize)
	assert.Equal(t, data.OwedRewardCursor{}, f.store.listCalls[0], "第一轮从表头读起")
	assert.Equal(t, rows[owedPageSize-1].Cursor(), f.store.listCalls[1], "翻页用上一页最后一行作游标")

	callsBefore := len(f.store.listCalls)
	second := loop.round(context.Background())

	assert.Equal(t, owedRoundStats{scanned: 30, converted: 30}, second, "第二轮从游标处接着看,排在后面的人得到处理")
	assert.Equal(t, rows[owedMaxRowsPerRound-1].Cursor(), f.store.listCalls[callsBefore])

	callsBefore = len(f.store.listCalls)
	loop.round(context.Background())
	assert.Equal(t, data.OwedRewardCursor{}, f.store.listCalls[callsBefore], "读到表尾后回到表头")
}

// TestOwedRoundStopsOnCancel:ctx 已取消时不读不转。
func TestOwedRoundStopsOnCancel(t *testing.T) {
	f, loop := newOwedLoop(t, &seqMinter{next: 1})
	f.store.owedRows = []data.OwedReward{owedRow(1, 50, 9001)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats := loop.round(ctx)

	assert.Equal(t, owedRoundStats{}, stats)
	assert.Empty(t, f.store.listCalls)
	assert.Empty(t, f.store.converted)
}

// ── 循环外壳 ──────────────────────────────────────────────────

// TestTrialBackgroundConfValidate:间隔必须为正;放弃时限必须大于超时判定;租约 owner 不能为空。
func TestTrialBackgroundConfValidate(t *testing.T) {
	require.NoError(t, trialSweepConf().validate())

	cases := []struct {
		name   string
		mutate func(c *TrialBackgroundConf)
	}{
		{"待入队间隔为 0", func(c *TrialBackgroundConf) { c.OwedInterval = 0 }},
		{"巡检间隔为负", func(c *TrialBackgroundConf) { c.SweepInterval = -time.Second }},
		{"超时判定为 0", func(c *TrialBackgroundConf) { c.ResultOverdue = 0 }},
		{"放弃时限不大于超时判定", func(c *TrialBackgroundConf) { c.Abandon = c.ResultOverdue }},
		{"owner 为空", func(c *TrialBackgroundConf) { c.Owner = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := trialSweepConf()
			tc.mutate(&c)

			assert.Error(t, c.validate())
		})
	}
}

// TestStartTrialBackgroundStops:未接线 / 配置非法时不启动并报错;正常启动后 stop 取消并等两个循环退出。
// 间隔取一小时:首轮延迟在 [0, 1h) 内随机,stop 之前几乎不可能开始第一轮;即使开始了,stop 也只是等它跑完。
// 兜底等待只防整个测试进程挂死,不是结论的一部分。
func TestStartTrialBackgroundStops(t *testing.T) {
	_, err := NewGuildLogic(nil, nil, nil, nil, nil).StartTrialBackground(trialSweepConf())
	require.Error(t, err, "活动依赖未装配时不启动")

	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	bad := trialSweepConf()
	bad.Owner = ""
	_, err = f.l.StartTrialBackground(bad)
	require.Error(t, err, "配置非法时不启动")

	conf := trialSweepConf()
	conf.OwedInterval, conf.SweepInterval = time.Hour, time.Hour
	stop, err := f.l.StartTrialBackground(conf)
	require.NoError(t, err)

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Minute):
		t.Fatal("stop 没有返回:后台循环没有响应取消")
	}
}
