package guildcheck

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	guildpb "proto/guild"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 测试编号对应 docs/design/guild-phase2/07-rollback-fail-closed.md §7.10.2(D1–D6、D15–D16)。
// 全部经 guildpb.GuildInternalClient 这个真实接缝注入假实现,不依赖墙钟、不连 guild。

// retentionSample 与 guild 侧 G8(go/guild/internal/server/guild_internal_server_test.go)钉的是**同一条**样例串:
// now = 1700000000000、保留 30 天、安全余量 1 小时 → cutoff = 1700000000000 − 2592000000 + 3600000。
const (
	retentionSample   = "since_ms older than terminal retention; cutoff_ms=1697411600000"
	retentionSampleMs = uint64(1697411600000)
)

type recordedCall struct {
	zoneID    uint32
	playerIDs []uint64
	sinceMs   uint64
	afterOpID uint64
	limit     uint32
}

// fakeGuildInternal 记录每次调用;answer 决定答复(n 从 0 起)。
type fakeGuildInternal struct {
	calls  []recordedCall
	answer func(n int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error)
}

func (f *fakeGuildInternal) ListAppliedAssetOpsSince(_ context.Context, in *guildpb.ListAppliedAssetOpsSinceRequest, _ ...grpc.CallOption) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
	f.calls = append(f.calls, recordedCall{
		zoneID:    in.GetZoneId(),
		playerIDs: slices.Clone(in.GetPlayerIds()),
		sinceMs:   in.GetSinceMs(),
		afterOpID: in.GetAfterOpId(),
		limit:     in.GetLimit(),
	})
	return f.answer(len(f.calls)-1, in)
}

// tableAnswer 按 guild 的查询语义(07 §7.4.1)模拟一张 guild_asset_op 表:只含已应用行,
// 终结时刻 = updated_ms;player_id ∈ 请求、updated_ms > since、op_id > after,按 op_id 升序,limit+1 判下一页。
func tableAnswer(rows []*guildpb.GuildAssetOpBrief) func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b *guildpb.GuildAssetOpBrief) int { return cmp.Compare(a.GetOpId(), b.GetOpId()) })
	return func(_ int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
		limit := int(req.GetLimit())
		var page []*guildpb.GuildAssetOpBrief
		for _, r := range sorted {
			if !slices.Contains(req.GetPlayerIds(), r.GetPlayerId()) || r.GetUpdatedMs() <= req.GetSinceMs() || r.GetOpId() <= req.GetAfterOpId() {
				continue
			}
			page = append(page, r)
			if len(page) == limit+1 {
				break
			}
		}
		resp := &guildpb.ListAppliedAssetOpsSinceResponse{}
		if len(page) > limit {
			page = page[:limit]
			resp.NextAfterOpId = page[limit-1].GetOpId()
		}
		resp.Ops = page
		return resp, nil
	}
}

func brief(opID, playerID, updatedMs uint64) *guildpb.GuildAssetOpBrief {
	return &guildpb.GuildAssetOpBrief{
		OpId:      opID,
		PlayerId:  playerID,
		GuildId:   77,
		Kind:      uint32(guildpb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE),
		Status:    uint32(guildpb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED),
		UpdatedMs: updatedMs,
	}
}

func opIDs(rows []GuildDivergence) []uint64 {
	out := make([]uint64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.OpID)
	}
	return out
}

func TestNew_NilClientIsNilChecker(t *testing.T) {
	// 装配点靠"nil 接口 = 未配置 = 拒绝回档";带类型的 nil 指针会让接口不为 nil、在查询时 panic。
	assert.Nil(t, New(nil))
}

func TestListDivergences_EmptyMapMakesNoCall(t *testing.T) {
	f := &fakeGuildInternal{answer: tableAnswer(nil)}
	res, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{})
	require.NoError(t, err)
	assert.Empty(t, res.Divergences)
	assert.Empty(t, res.UnprovablePlayerIDs)
	assert.Empty(t, f.calls)
}

func TestListDivergences_RejectsZeroKeysAndZeroSince(t *testing.T) {
	f := &fakeGuildInternal{answer: tableAnswer(nil)}
	_, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{0: 10})
	require.Error(t, err)
	_, err = New(f).ListDivergences(context.Background(), map[uint64]uint64{5: 0})
	require.Error(t, err)
	assert.Empty(t, f.calls, "调用方 bug 不能发到 guild")
}

// D1 分块:250 个玩家 → 3 次首页调用,每次 ≤100 个 id(升序),since = 块内最小值,zone_id 恒 0,limit 显式 500。
func TestListDivergences_D1_ChunksBy100WithChunkMinSince(t *testing.T) {
	since := make(map[uint64]uint64, 250)
	for id := uint64(1); id <= 250; id++ {
		since[id] = 1_000_000 + (251-id)*10 // id 越大 since 越小:块内最小值落在每块最后一个 id 上
	}
	f := &fakeGuildInternal{answer: tableAnswer(nil)}

	_, err := New(f).ListDivergences(context.Background(), since)
	require.NoError(t, err)
	require.Len(t, f.calls, 3)

	wantRanges := [][2]uint64{{1, 100}, {101, 200}, {201, 250}}
	for i, call := range f.calls {
		lo, hi := wantRanges[i][0], wantRanges[i][1]
		require.Len(t, call.playerIDs, int(hi-lo+1), "call %d", i)
		assert.Equal(t, lo, call.playerIDs[0], "call %d", i)
		assert.Equal(t, hi, call.playerIDs[len(call.playerIDs)-1], "call %d", i)
		assert.True(t, slices.IsSorted(call.playerIDs), "call %d", i)
		assert.Equal(t, since[hi], call.sinceMs, "call %d: since 必须是块内最小值", i)
		assert.Zero(t, call.zoneID, "data_service 恒传 zone_id=0(07 §7.4.3)")
		assert.Zero(t, call.afterOpID)
		assert.Equal(t, PageLimit, call.limit)
	}
}

// D3 逐玩家过滤:同块内快照新的玩家,其早于自身 since 的行被滤掉;结果按 op_id 升序。
func TestListDivergences_D3_FiltersByEachPlayersSince(t *testing.T) {
	const a, b = uint64(11), uint64(22)
	f := &fakeGuildInternal{answer: tableAnswer([]*guildpb.GuildAssetOpBrief{
		brief(9, b, 6000), // b 自己的 since 之后 → 留
		brief(3, a, 2000), // a 的 since 之后 → 留
		brief(5, b, 3000), // 晚于块 since(1000)但早于 b 自己的 since(5000)→ 滤掉
	})}

	res, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{a: 1000, b: 5000})
	require.NoError(t, err)
	require.Len(t, f.calls, 1)
	assert.Equal(t, uint64(1000), f.calls[0].sinceMs)
	assert.Equal(t, []uint64{3, 9}, opIDs(res.Divergences))
	assert.Equal(t, uint64(77), res.Divergences[0].GuildID)
	assert.Equal(t, uint32(guildpb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), res.Divergences[0].Status)
	assert.Empty(t, res.UnprovablePlayerIDs)
}

// 翻页:同一块跨三页时翻到 next_after_op_id == 0 为止,无重无漏。
func TestListDivergences_PagesUntilCursorIsZero(t *testing.T) {
	rows := make([]*guildpb.GuildAssetOpBrief, 0, 1201)
	for op := uint64(1); op <= 1201; op++ {
		rows = append(rows, brief(op, 7, 5000+op))
	}
	f := &fakeGuildInternal{answer: tableAnswer(rows)}

	res, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
	require.NoError(t, err)
	require.Len(t, f.calls, 3)
	assert.Equal(t, []uint64{0, 500, 1000}, []uint64{f.calls[0].afterOpID, f.calls[1].afterOpID, f.calls[2].afterOpID})
	require.Len(t, res.Divergences, 1201)
	assert.True(t, slices.IsSortedFunc(res.Divergences, func(x, y GuildDivergence) int { return cmp.Compare(x.OpID, y.OpID) }))
}

// D4 任一页 error → 整体 error(第二页失败时第一页的结果也不能当数用);Unimplemented(旧版 guild)同样是 error。
func TestListDivergences_D4_AnyPageFailureFailsWhole(t *testing.T) {
	rows := make([]*guildpb.GuildAssetOpBrief, 0, 600)
	for op := uint64(1); op <= 600; op++ {
		rows = append(rows, brief(op, 7, 9000))
	}
	table := tableAnswer(rows)
	f := &fakeGuildInternal{answer: func(n int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
		if n == 1 {
			return nil, status.Error(codes.Unavailable, "guild restarting")
		}
		return table(n, req)
	}}
	res, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(errors.Unwrap(err)))
	assert.Empty(t, res.Divergences, "出错时不回半份结果")
	assert.Len(t, f.calls, 2)

	for _, code := range []codes.Code{codes.Unimplemented, codes.PermissionDenied, codes.InvalidArgument, codes.Internal, codes.DeadlineExceeded} {
		f := &fakeGuildInternal{answer: func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
			return nil, status.Error(code, "no")
		}}
		_, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
		require.Error(t, err, "code=%s 必须是 error", code)
		assert.Len(t, f.calls, 1, "code=%s 不重试", code)
	}
}

// 第二块失败:第一块查到的行同样作废(调用方零写入由 logic 的 D11 钉住)。
func TestListDivergences_SecondChunkFailureFailsWhole(t *testing.T) {
	since := make(map[uint64]uint64, 150)
	for id := uint64(1); id <= 150; id++ {
		since[id] = 1000
	}
	table := tableAnswer([]*guildpb.GuildAssetOpBrief{brief(1, 3, 5000)})
	f := &fakeGuildInternal{answer: func(n int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
		if n == 1 {
			return nil, status.Error(codes.Unavailable, "down")
		}
		return table(n, req)
	}}
	_, err := New(f).ListDivergences(context.Background(), since)
	require.Error(t, err)
	assert.Len(t, f.calls, 2)
}

// D5 过滤后 > MaxDivergenceRows → ErrTooManyDivergences;恰好等于上限不算超。
func TestListDivergences_D5_TooManyRows(t *testing.T) {
	build := func(n uint64) []*guildpb.GuildAssetOpBrief {
		rows := make([]*guildpb.GuildAssetOpBrief, 0, n)
		for op := uint64(1); op <= n; op++ {
			rows = append(rows, brief(op, 7, 5000))
		}
		return rows
	}

	_, err := New(&fakeGuildInternal{answer: tableAnswer(build(MaxDivergenceRows + 1))}).
		ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTooManyDivergences)

	res, err := New(&fakeGuildInternal{answer: tableAnswer(build(MaxDivergenceRows))}).
		ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
	require.NoError(t, err)
	assert.Len(t, res.Divergences, MaxDivergenceRows)
}

// D6 总预算耗尽:ctx 已到点 → error(可 errors.Is DeadlineExceeded),一次 RPC 都不发;
// 下游答 DeadlineExceeded(zrpc 客户端单次超时)同样是 error。
func TestListDivergences_D6_BudgetExhausted(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	f := &fakeGuildInternal{answer: tableAnswer(nil)}
	_, err := New(f).ListDivergences(expired, map[uint64]uint64{7: 1000})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, f.calls)

	f = &fakeGuildInternal{answer: func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
		return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	}}
	_, err = New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
	require.Error(t, err)
}

// D15 保留期钳位:块内 3 个玩家、其中 1 个 since < cutoff;首次答 FailedPrecondition(与 guild G8 同一条样例串)
// → 第二次调用 since_ms == cutoff、仍是首页;UnprovablePlayerIDs == [该玩家];其余玩家的行照常按各自 since 过滤,
// 不可证明玩家在保留期内的那段照列不误。
func TestListDivergences_D15_RetentionClampRequeriesOnce(t *testing.T) {
	const old, mid, fresh = uint64(101), uint64(102), uint64(103)
	since := map[uint64]uint64{
		old:   retentionSampleMs - 100_000, // 早于保留期下界:不可证明
		mid:   retentionSampleMs + 100_000,
		fresh: retentionSampleMs + 200_000,
	}
	table := tableAnswer([]*guildpb.GuildAssetOpBrief{
		brief(1, old, retentionSampleMs+50_000),    // 下界之后:照列
		brief(2, mid, retentionSampleMs+50_000),    // 早于 mid 自己的 since:滤掉
		brief(3, fresh, retentionSampleMs+300_000), // 留
	})
	f := &fakeGuildInternal{answer: func(n int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
		if n == 0 {
			return nil, status.Error(codes.FailedPrecondition, retentionSample)
		}
		return table(n, req)
	}}

	res, err := New(f).ListDivergences(context.Background(), since)
	require.NoError(t, err)
	require.Len(t, f.calls, 2)
	assert.Equal(t, since[old], f.calls[0].sinceMs)
	assert.Equal(t, retentionSampleMs, f.calls[1].sinceMs, "钳位重查必须用 guild 带回的 cutoff_ms")
	assert.Zero(t, f.calls[1].afterOpID, "钳位重查从首页开始")
	assert.Equal(t, []uint64{old}, res.UnprovablePlayerIDs)
	assert.Equal(t, retentionSampleMs, res.RetentionCutoffMs)
	assert.Equal(t, []uint64{1, 3}, opIDs(res.Divergences))
}

// D16 message 解析不出 cutoff_ms → error;钳位重查仍 FailedPrecondition → error;下界不晚于块 since(自相矛盾)→ error;
// 非首页的 FailedPrecondition 也不钳位,直接 error。
func TestListDivergences_D16_RetentionFailuresAreErrors(t *testing.T) {
	cases := map[string]func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error){
		"message 解析不出": func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "retention exceeded")
		},
		"钳位重查仍被拒": func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, retentionSample)
		},
		"下界不晚于 since": func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "since_ms older than terminal retention; cutoff_ms=1000")
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeGuildInternal{answer: answer}
			_, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
			require.Error(t, err)
			assert.LessOrEqual(t, len(f.calls), 2, "每块至多钳位一次")
		})
	}

	t.Run("非首页被拒", func(t *testing.T) {
		rows := make([]*guildpb.GuildAssetOpBrief, 0, 600)
		for op := uint64(1); op <= 600; op++ {
			rows = append(rows, brief(op, 7, retentionSampleMs+10))
		}
		table := tableAnswer(rows)
		f := &fakeGuildInternal{answer: func(n int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
			if n == 1 {
				return nil, status.Error(codes.FailedPrecondition, retentionSample)
			}
			return table(n, req)
		}}
		_, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
		require.Error(t, err)
		assert.Len(t, f.calls, 2)
	})
}

// guild 答复自相矛盾(越界 player_id、游标不前进、行不升序)都按查不成处理,而不是死循环或静默吞掉。
func TestListDivergences_InconsistentGuildAnswersAreErrors(t *testing.T) {
	type answerFn = func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error)
	cases := map[string]struct {
		answer    answerFn
		wantCalls int
	}{
		"越界 player_id": {
			answer: func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
				return &guildpb.ListAppliedAssetOpsSinceResponse{Ops: []*guildpb.GuildAssetOpBrief{brief(1, 999, 5000)}}, nil
			},
			wantCalls: 1,
		},
		"行不升序": {
			answer: func(int, *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
				return &guildpb.ListAppliedAssetOpsSinceResponse{Ops: []*guildpb.GuildAssetOpBrief{brief(5, 7, 5000), brief(4, 7, 5000)}}, nil
			},
			wantCalls: 1,
		},
		"游标不前进": {
			// 第一页正常给出游标 1;第二页又回游标 1:不前进,不能死循环到预算耗尽。
			answer: func(n int, _ *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
				return &guildpb.ListAppliedAssetOpsSinceResponse{
					Ops:           []*guildpb.GuildAssetOpBrief{brief(uint64(n+1), 7, 5000)},
					NextAfterOpId: 1,
				}, nil
			},
			wantCalls: 2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeGuildInternal{answer: tc.answer}
			_, err := New(f).ListDivergences(context.Background(), map[uint64]uint64{7: 1000})
			require.Error(t, err)
			assert.Len(t, f.calls, tc.wantCalls)
		})
	}
}

func TestParseRetentionCutoffMs(t *testing.T) {
	got, err := ParseRetentionCutoffMs(retentionSample)
	require.NoError(t, err)
	assert.Equal(t, retentionSampleMs, got)

	for _, bad := range []string{
		"",
		retentionRejectedMessagePrefix,
		retentionRejectedMessagePrefix + "abc",
		retentionRejectedMessagePrefix + "12 ",
		retentionRejectedMessagePrefix + "-1",
		retentionRejectedMessagePrefix + "0",
		"x" + retentionSample,
		"since_ms older than terminal retention, cutoff_ms=1697411600000",
	} {
		_, err := ParseRetentionCutoffMs(bad)
		assert.Error(t, err, "%q 必须解析失败", bad)
	}
}
