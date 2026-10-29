package server

// 登录取归属 zone 与账号目录的服务端直方图(开服线 X-3,L-14 / L-18 压测口径):
//   - data_service_home_zone_read_seconds{rpc,result} / data_service_home_zone_batch_ids;
//   - data_service_account_directory_rpc_seconds{rpc,result}。
// result 按 handler 实际回给调用方的 gRPC code 归类。这里钉住两件事:归类与既有错误映射一一对应(拒绝 ≠ 故障 ≠ 缺席),
// 以及每次调用恰好在自己的 rpc 上记一个样本(含鉴权失败等提前返回)。断言一律取增量,不依赖同包其它用例打过的点。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHomeZoneReadResultFollowsReturnedCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"ok", nil, metrics.HomeZoneReadResultOK},
		{"mapping absent", status.Error(codes.NotFound, "no home zone mapping for player 1"), metrics.HomeZoneReadResultNotFound},
		{"mapping redis / global db down", status.Error(codes.Unavailable, "error_code=30"), metrics.HomeZoneReadResultError},
		{"caller canceled", status.FromContextError(context.Canceled).Err(), metrics.HomeZoneReadResultError},
		{"caller deadline", status.FromContextError(context.DeadlineExceeded).Err(), metrics.HomeZoneReadResultError},
		{"non-status error", errors.New("boom"), metrics.HomeZoneReadResultError},
	} {
		if got := homeZoneReadResult(tc.err); got != tc.want {
			t.Fatalf("%s: homeZoneReadResult(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestDirectoryRPCResultFollowsDirectoryErrorMapping:输入直接取 directoryError / admitDirectory 的产物,
// 那张映射一改这里就红,指标归类不会悄悄和应答码分家。
func TestDirectoryRPCResultFollowsDirectoryErrorMapping(t *testing.T) {
	unready := &DataServiceServer{svcCtx: &svc.ServiceContext{}}
	ready := &DataServiceServer{svcCtx: &svc.ServiceContext{AccountDirectory: &serverDirectoryFake{}, AccountDirectoryAuth: directoryVerifier(t)}}
	unsigned := &dspb.ReadAccountDirectoryRequest{Account: "账号甲"}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"ok", directoryError(nil), metrics.AccountDirectoryResultOK},
		{"missing", directoryError(accountdirectory.ErrMissing), metrics.AccountDirectoryResultNotFound},
		{"invalid", directoryError(accountdirectory.ErrInvalid), metrics.AccountDirectoryResultRejected},
		{"full", directoryError(accountdirectory.ErrFull), metrics.AccountDirectoryResultRejected},
		{"conflict", directoryError(accountdirectory.ErrConflict), metrics.AccountDirectoryResultRejected},
		{"unavailable", directoryError(accountdirectory.ErrUnavailable), metrics.AccountDirectoryResultError},
		{"sql failure", directoryError(errors.New("SQL 依赖失败")), metrics.AccountDirectoryResultError},
		{"directory unready", unready.admitDirectory(context.Background(), "ReadAccountDirectory", unsigned), metrics.AccountDirectoryResultError},
		{"bad proof", ready.admitDirectory(context.Background(), "ReadAccountDirectory", unsigned), metrics.AccountDirectoryResultRejected},
		{"caller canceled", status.FromContextError(context.Canceled).Err(), metrics.AccountDirectoryResultError},
	} {
		if got := directoryRPCResult(tc.err); got != tc.want {
			t.Fatalf("%s: directoryRPCResult(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestAccountDirectoryRPCsRecordOneSamplePerCall:五个账号目录 RPC 各自落在自己的 rpc 上,每次调用恰好一个样本;
// 鉴权失败(未签名)的提前返回同样计入,归 rejected。
func TestAccountDirectoryRPCsRecordOneSamplePerCall(t *testing.T) {
	labels := map[string]string{
		"ReadAccountDirectory": metrics.AccountDirectoryRPCRead,
		"CreateAccountRole":    metrics.AccountDirectoryRPCCreate,
		"LookupAccountRole":    metrics.AccountDirectoryRPCLookup,
		"RepairAccountRole":    metrics.AccountDirectoryRPCRepair,
		"RemoveAccountRoles":   metrics.AccountDirectoryRPCRemove,
	}
	cases := directoryRPCCases()
	require.Len(t, cases, len(labels), "every account directory RPC needs an rpc label")
	for _, rpc := range cases {
		label, ok := labels[rpc.name]
		require.True(t, ok, "%s has no account_directory_rpc_seconds rpc label", rpc.name)
		for _, outcome := range []struct {
			name    string
			failure error
			signed  bool
			want    string
		}{
			{"ok", nil, true, metrics.AccountDirectoryResultOK},
			{"missing", accountdirectory.ErrMissing, true, metrics.AccountDirectoryResultNotFound},
			{"conflict", accountdirectory.ErrConflict, true, metrics.AccountDirectoryResultRejected},
			{"sql failure", errors.New("SQL 依赖失败"), true, metrics.AccountDirectoryResultError},
			{"unsigned", nil, false, metrics.AccountDirectoryResultRejected},
		} {
			t.Run(rpc.name+"/"+outcome.name, func(t *testing.T) {
				players := &base.AccountSimplePlayerList{}
				fake := &serverDirectoryFake{players: players, ownership: &dspb.AccountRoleOwnership{}}
				if outcome.failure != nil {
					fake.failure = fmt.Errorf("目录依赖: %w", outcome.failure)
				}
				s := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: fake, AccountDirectoryAuth: directoryVerifier(t)})
				request := rpc.request()
				if outcome.signed {
					signDirectoryRequest(t, rpc.name, request, "login", uint64(time.Now().UnixMilli()))
				}
				before := histogramSamples(t, "data_service_account_directory_rpc_seconds", map[string]string{"rpc": label, "result": outcome.want})
				_, _ = rpc.invoke(context.Background(), s, request)
				after := histogramSamples(t, "data_service_account_directory_rpc_seconds", map[string]string{"rpc": label, "result": outcome.want})
				if after.count != before.count+1 {
					t.Fatalf("account_directory_rpc_seconds{rpc=%q,result=%q} moved by %d, want 1", label, outcome.want, after.count-before.count)
				}
			})
		}
	}
}

// TestHomeZoneRPCsRecordResultAndBatchSize:单查缺席记 home/not_found,调用方已放弃记 home/error;
// 批量查(缺席的 id 只是不出现在 map 里)记 batch/ok,并把本批 id 个数记进 home_zone_batch_ids。
func TestHomeZoneRPCsRecordResultAndBatchSize(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	const family = "data_service_home_zone_read_seconds"
	home := func(result string) map[string]string {
		return map[string]string{"rpc": metrics.HomeZoneRPCHome, "result": result}
	}

	before := histogramSamples(t, family, home(metrics.HomeZoneReadResultNotFound))
	_, err := s.GetPlayerHomeZone(context.Background(), &dspb.GetPlayerHomeZoneRequest{PlayerId: 7101})
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(t, before.count+1, histogramSamples(t, family, home(metrics.HomeZoneReadResultNotFound)).count)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before = histogramSamples(t, family, home(metrics.HomeZoneReadResultError))
	_, err = s.GetPlayerHomeZone(canceled, &dspb.GetPlayerHomeZoneRequest{PlayerId: 7102})
	require.Equal(t, codes.Canceled, status.Code(err))
	require.Equal(t, before.count+1, histogramSamples(t, family, home(metrics.HomeZoneReadResultError)).count)

	batchOK := map[string]string{"rpc": metrics.HomeZoneRPCBatch, "result": metrics.HomeZoneReadResultOK}
	before = histogramSamples(t, family, batchOK)
	idsBefore := histogramSamples(t, "data_service_home_zone_batch_ids", map[string]string{})
	_, err = s.BatchGetPlayerHomeZone(context.Background(), &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{7103, 7104, 7105}})
	require.NoError(t, err)
	require.Equal(t, before.count+1, histogramSamples(t, family, batchOK).count)
	idsAfter := histogramSamples(t, "data_service_home_zone_batch_ids", map[string]string{})
	require.Equal(t, idsBefore.count+1, idsAfter.count)
	require.Equal(t, float64(3), idsAfter.sum-idsBefore.sum)
}

type histogramSnapshot struct {
	count uint64
	sum   float64
}

// histogramSamples 在默认注册表里找 label 集合恰好等于 labels 的那条直方图序列(labels 为空 = 无 label 的直方图)。
// NewDataServiceServer 已预建全部组合,所以序列不存在就是缺陷,直接报错。
func histogramSamples(t *testing.T, family string, labels map[string]string) histogramSnapshot {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			matched := true
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					matched = false
				}
			}
			if matched {
				h := m.GetHistogram()
				return histogramSnapshot{count: h.GetSampleCount(), sum: h.GetSampleSum()}
			}
		}
	}
	t.Fatalf("%s%v not found", family, labels)
	return histogramSnapshot{}
}
