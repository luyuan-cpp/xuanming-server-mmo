package server

// GuildInternalServer 的单测,编号对应 07-rollback-fail-closed.md §7.10.2 的 G7–G9(不连库;查询经 AppliedAssetOpLister
// 接缝注入假实现,墙钟经构造参数注入,不读真实时间)。
//
// 指标:go-zero core/metric 的写入在单测进程里被全局开关丢弃,所以这里换掉 observe 记录器,断言"记了哪个 result"。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/data"
	pb "proto/guild"
)

const (
	internalTestNowMs     uint64 = 1_700_000_000_000
	internalTestRetention        = 30 * 24 * time.Hour
	internalTestHourMs    uint64 = 3_600_000
	internalTestDayMs     uint64 = 24 * internalTestHourMs
	// internalTestSinceOK 远在保留期之内,用于入参 / 装配用例。
	internalTestSinceOK = internalTestNowMs - internalTestHourMs
)

// fakeAppliedLister 记录收到的查询并回预设结果。
type fakeAppliedLister struct {
	calls int
	got   data.AppliedOpsQuery
	ops   []*pb.GuildAssetOpBrief
	next  uint64
	err   error
	// 清理水位(0 = 从未清理过)与读水位的错误。
	watermarkMs  uint64
	watermarkErr error
}

func (f *fakeAppliedLister) TerminalCleanupWatermarkMs(context.Context) (uint64, error) {
	return f.watermarkMs, f.watermarkErr
}

func (f *fakeAppliedLister) ListAppliedAssetOpsSince(_ context.Context, q data.AppliedOpsQuery) ([]*pb.GuildAssetOpBrief, uint64, error) {
	f.calls++
	f.got = q
	return f.ops, f.next, f.err
}

type observedCall struct {
	result string
	rows   int
}

// newInternalTestServer 用固定墙钟与记录器装配;ops 必须传 nil 接口或非 nil 实现(见 NewGuildInternalServer)。
func newInternalTestServer(ops AppliedAssetOpLister, retention time.Duration) (*GuildInternalServer, *[]observedCall) {
	s := NewGuildInternalServer(ops, retention, func() time.Time { return time.UnixMilli(int64(internalTestNowMs)) })
	var observed []observedCall
	s.observe = func(result string, rows int) { observed = append(observed, observedCall{result, rows}) }
	return s, &observed
}

func validInternalRequest() *pb.ListAppliedAssetOpsSinceRequest {
	return &pb.ListAppliedAssetOpsSinceRequest{PlayerIds: []uint64{11, 12}, SinceMs: internalTestSinceOK}
}

func requireStatus(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "应返回 gRPC status,得到 %v", err)
	require.Equal(t, want, st.Code(), st.Message())
	return st
}

func sequentialIDs(n int) []uint64 {
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	return ids
}

// TestListAppliedRequestValidation(G7):07 §7.4.2 表逐行。非法入参回 InvalidArgument 且**不碰查询**;
// 合法入参原样透传,limit 0 换成默认 500。
func TestListAppliedRequestValidation(t *testing.T) {
	invalid := []struct {
		name   string
		mutate func(*pb.ListAppliedAssetOpsSinceRequest)
	}{
		{"player_ids 为空", func(r *pb.ListAppliedAssetOpsSinceRequest) { r.PlayerIds = nil }},
		{"player_ids 超过 100", func(r *pb.ListAppliedAssetOpsSinceRequest) {
			r.PlayerIds = sequentialIDs(data.MaxAppliedOpsPlayerIDs + 1)
		}},
		{"player_ids 含 0", func(r *pb.ListAppliedAssetOpsSinceRequest) { r.PlayerIds = []uint64{11, 0} }},
		{"player_ids 有重复", func(r *pb.ListAppliedAssetOpsSinceRequest) { r.PlayerIds = []uint64{11, 12, 11} }},
		{"since_ms 为 0", func(r *pb.ListAppliedAssetOpsSinceRequest) { r.SinceMs = 0 }},
		{"limit 超过 500", func(r *pb.ListAppliedAssetOpsSinceRequest) { r.Limit = data.MaxAppliedOpsPageLimit + 1 }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeAppliedLister{}
			s, observed := newInternalTestServer(lister, internalTestRetention)
			req := validInternalRequest()
			tc.mutate(req)

			resp, err := s.ListAppliedAssetOpsSince(context.Background(), req)

			assert.Nil(t, resp)
			requireStatus(t, err, codes.InvalidArgument)
			assert.Zero(t, lister.calls, "入参校验必须先于任何 SQL")
			assert.Equal(t, []observedCall{{listAppliedResultInvalid, 0}}, *observed)
		})
	}

	valid := []struct {
		name      string
		limit     uint32
		players   []uint64
		wantLimit int
	}{
		{"limit 0 取默认 500", 0, []uint64{11}, data.MaxAppliedOpsPageLimit},
		{"limit 恰好 500", 500, []uint64{11}, 500},
		{"limit 1", 1, []uint64{11}, 1},
		{"player_ids 恰好 100", 7, sequentialIDs(data.MaxAppliedOpsPlayerIDs), 7},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeAppliedLister{}
			s, _ := newInternalTestServer(lister, internalTestRetention)
			req := &pb.ListAppliedAssetOpsSinceRequest{
				ZoneId: 3, PlayerIds: tc.players, SinceMs: internalTestSinceOK, AfterOpId: 42, Limit: tc.limit,
			}

			_, err := s.ListAppliedAssetOpsSince(context.Background(), req)

			require.NoError(t, err)
			require.Equal(t, 1, lister.calls)
			assert.Equal(t, data.AppliedOpsQuery{
				ZoneID: 3, PlayerIDs: tc.players, SinceMs: internalTestSinceOK, AfterOpID: 42, Limit: tc.wantLimit,
			}, lister.got)
		})
	}
}

// TestListAppliedPassesRowsAndCursorThrough:成功答复原样带回行与游标,并按有无行记 ok_rows / ok_empty。
func TestListAppliedPassesRowsAndCursorThrough(t *testing.T) {
	row := &pb.GuildAssetOpBrief{OpId: 900, PlayerId: 11, GuildId: 5,
		Status: uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), UpdatedMs: internalTestNowMs}
	lister := &fakeAppliedLister{ops: []*pb.GuildAssetOpBrief{row}, next: 900}
	s, observed := newInternalTestServer(lister, internalTestRetention)

	resp, err := s.ListAppliedAssetOpsSince(context.Background(), validInternalRequest())

	require.NoError(t, err)
	require.Len(t, resp.GetOps(), 1)
	assert.True(t, proto.Equal(row, resp.GetOps()[0]))
	assert.Equal(t, uint64(900), resp.GetNextAfterOpId())

	lister.ops, lister.next = nil, 0
	resp, err = s.ListAppliedAssetOpsSince(context.Background(), validInternalRequest())
	require.NoError(t, err)
	assert.Empty(t, resp.GetOps())
	assert.Zero(t, resp.GetNextAfterOpId())

	assert.Equal(t, []observedCall{{listAppliedResultOKRows, 1}, {listAppliedResultOKEmpty, 0}}, *observed)
}

// TestListAppliedRetentionBoundary(G8):保留期 30 天、余量 1 小时。
// since = now − 30d + 59min 拒(FailedPrecondition),+61min 过,恰好等于下界也过;拒绝的 message **逐字**等于样例,
// cutoff_ms = now − 30d + 60min。样例字符串与 data_service 的 D15 用同一条(跨服务契约,改格式两边同改)。
func TestListAppliedRetentionBoundary(t *testing.T) {
	const sample = "since_ms older than terminal retention; cutoff_ms=1697411600000"
	cutoff := internalTestNowMs - 30*internalTestDayMs + internalTestHourMs
	require.Equal(t, uint64(1_697_411_600_000), cutoff, "样例里的数就是公式右端")
	assert.Equal(t, sample, RetentionRejectedMessage(cutoff))

	lister := &fakeAppliedLister{}
	s, observed := newInternalTestServer(lister, internalTestRetention)
	req := validInternalRequest()
	req.SinceMs = internalTestNowMs - 30*internalTestDayMs + 59*60_000

	resp, err := s.ListAppliedAssetOpsSince(context.Background(), req)

	assert.Nil(t, resp)
	st := requireStatus(t, err, codes.FailedPrecondition)
	assert.Equal(t, sample, st.Message())
	assert.Zero(t, lister.calls, "不可证明就不查")
	assert.Equal(t, []observedCall{{listAppliedResultRetention, 0}}, *observed)

	for _, since := range []uint64{cutoff, internalTestNowMs - 30*internalTestDayMs + 61*60_000} {
		req.SinceMs = since
		_, err = s.ListAppliedAssetOpsSince(context.Background(), req)
		require.NoError(t, err, "since=%d", since)
	}
	assert.Equal(t, 2, lister.calls)
}

// TestListAppliedCleanupWatermarkRaisesTheFloor:保留期调大之后(这里按 60 天配置),旧配置(30 天)清理留下的水位
// 才是真正的可证明下界 —— 只看配置会把"已被删掉的那 30 天"当成可证明且没有分歧(fail-open)。
// 下界 = max(配置下界, 水位 + 1 小时余量);拒绝里回带的 cutoff_ms 已含水位,钳位重查不会被水位再拒一次。
func TestListAppliedCleanupWatermarkRaisesTheFloor(t *testing.T) {
	const retention60d = 60 * 24 * time.Hour
	watermark := internalTestNowMs - 30*internalTestDayMs // 旧配置最后一轮清理的截止
	floor := watermark + internalTestHourMs
	require.Greater(t, floor, retentionCutoffMs(internalTestNowMs, retention60d), "本用例里水位下界必须高于配置下界")
	assert.Equal(t, floor, provableCutoffMs(internalTestNowMs, retention60d, watermark))
	assert.Equal(t, retentionCutoffMs(internalTestNowMs, retention60d), provableCutoffMs(internalTestNowMs, retention60d, 0), "从未清理过:只按配置算")
	assert.Equal(t, retentionCutoffMs(internalTestNowMs, internalTestRetention),
		provableCutoffMs(internalTestNowMs, internalTestRetention, internalTestNowMs-40*internalTestDayMs), "水位比配置下界旧:配置说了算")

	lister := &fakeAppliedLister{watermarkMs: watermark}
	s, observed := newInternalTestServer(lister, retention60d)
	req := validInternalRequest()
	req.SinceMs = internalTestNowMs - 45*internalTestDayMs // 配置(60 天)内,但早于水位:那段流水已经删了

	resp, err := s.ListAppliedAssetOpsSince(context.Background(), req)

	assert.Nil(t, resp)
	st := requireStatus(t, err, codes.FailedPrecondition)
	assert.Equal(t, RetentionRejectedMessage(floor), st.Message())
	assert.Zero(t, lister.calls, "不可证明就不查")
	assert.Equal(t, []observedCall{{listAppliedResultRetention, 0}}, *observed)

	req.SinceMs = floor
	_, err = s.ListAppliedAssetOpsSince(context.Background(), req)
	require.NoError(t, err, "恰好等于下界可证明")
	assert.Equal(t, 1, lister.calls)
}

// TestListAppliedUnavailableWhenWatermarkUnreadable:读不到清理水位就无法判断哪段流水还在 → Unavailable、不查库。
// data_service 对 Unavailable 按"问不到"拒绝回档且放行无效;把读失败当成 0 才是 fail-open。
func TestListAppliedUnavailableWhenWatermarkUnreadable(t *testing.T) {
	lister := &fakeAppliedLister{watermarkErr: errors.New("redis: connection refused")}
	s, observed := newInternalTestServer(lister, internalTestRetention)

	resp, err := s.ListAppliedAssetOpsSince(context.Background(), validInternalRequest())

	assert.Nil(t, resp)
	st := requireStatus(t, err, codes.Unavailable)
	assert.NotContains(t, st.Message(), "connection refused", "底层错误原文不外发")
	assert.Zero(t, lister.calls)
	assert.Equal(t, []observedCall{{listAppliedResultUnavailable, 0}}, *observed)
}

// TestRetentionCutoffNeverUnderflows:now 早于"保留期 − 余量"时下界取 0,不回绕成一个巨大的数把所有调用都拒掉。
func TestRetentionCutoffNeverUnderflows(t *testing.T) {
	assert.Zero(t, retentionCutoffMs(1000, internalTestRetention))
	assert.Equal(t, uint64(1), retentionCutoffMs(30*internalTestDayMs-internalTestHourMs+1, internalTestRetention))
}

// TestListAppliedUnavailableWhenNotWired(G9):资产 Store 未装配、或保留期未配置 → Unavailable,不查、不判保留期。
func TestListAppliedUnavailableWhenNotWired(t *testing.T) {
	t.Run("Store 未装配", func(t *testing.T) {
		s, observed := newInternalTestServer(nil, internalTestRetention)
		resp, err := s.ListAppliedAssetOpsSince(context.Background(), validInternalRequest())
		assert.Nil(t, resp)
		requireStatus(t, err, codes.Unavailable)
		assert.Equal(t, []observedCall{{listAppliedResultUnavailable, 0}}, *observed)
	})
	t.Run("保留期未配置", func(t *testing.T) {
		lister := &fakeAppliedLister{}
		s, observed := newInternalTestServer(lister, 0)
		_, err := s.ListAppliedAssetOpsSince(context.Background(), validInternalRequest())
		requireStatus(t, err, codes.Unavailable)
		assert.Zero(t, lister.calls)
		assert.Equal(t, []observedCall{{listAppliedResultUnavailable, 0}}, *observed)
	})
}

// TestListAppliedStoreErrorsFailClosed:查询失败一律非 OK(data_service 据此拒绝回档),超时保留 DeadlineExceeded,
// 其余 Internal 且不外发库错误原文。
func TestListAppliedStoreErrorsFailClosed(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{fmt.Errorf("list applied guild_asset_op: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{fmt.Errorf("wrap: %w", context.Canceled), codes.Canceled},
		{errors.New("Error 1146 (42S02): Table 'mmorpg_guild.guild_asset_op' doesn't exist"), codes.Internal},
	}
	for _, tc := range cases {
		lister := &fakeAppliedLister{err: tc.err}
		s, observed := newInternalTestServer(lister, internalTestRetention)

		resp, err := s.ListAppliedAssetOpsSince(context.Background(), validInternalRequest())

		assert.Nil(t, resp)
		st := requireStatus(t, err, tc.want)
		assert.NotContains(t, st.Message(), "mmorpg_guild", "库错误原文不外发")
		assert.Equal(t, []observedCall{{listAppliedResultError, 0}}, *observed)
	}
}

// TestListAppliedResultLabelsAreFixed:result 的取值全集与 07 §7.4.5 一致、无重复 —— PrimeGuildInternalMetrics 按它预置。
func TestListAppliedResultLabelsAreFixed(t *testing.T) {
	assert.ElementsMatch(t,
		[]string{"ok_empty", "ok_rows", "invalid", "retention", "unavailable", "error"},
		listAppliedResults)
}
