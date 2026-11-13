package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/roleidentity"
	"shared/serviceauth"
	"trade/internal/commerce"
	"trade/internal/config"
)

// 人工处置入口(manual_contract_ops.go)的准入、目标核对与结论映射。判定本身在 commerce 包,有自己的用例。

const testManualSecret = "manual-resolution-test-secret-0123456789"

// fakeManualStore 回预设的结果,记下收到的请求。
type fakeManualStore struct {
	rows       []*tradepb.TradeCommerceRecord
	listErr    error
	report     commerce.ManualReport
	showErr    error
	result     commerce.ManualResult
	resolveErr error
	requests   []commerce.ManualRequest
	executes   []bool
}

func (f *fakeManualStore) List(context.Context, uint64, uint64, int) ([]*tradepb.TradeCommerceRecord, error) {
	return f.rows, f.listErr
}

func (f *fakeManualStore) Show(context.Context, uint64) (commerce.ManualReport, error) {
	return f.report, f.showErr
}

func (f *fakeManualStore) Resolve(_ context.Context, req commerce.ManualRequest, execute bool) (commerce.ManualResult, error) {
	f.requests = append(f.requests, req)
	f.executes = append(f.executes, execute)
	return f.result, f.resolveErr
}

func newTestManualOps(t *testing.T, store *fakeManualStore) *ManualContractOps {
	t.Helper()
	ops, err := NewManualContractOps(store, testManualSecret, "staging-global", "replica-1")
	require.NoError(t, err)
	return ops
}

func signManual(t *testing.T, method string, in proto.Message, caller, secret string) {
	t.Helper()
	require.NoError(t, serviceauth.Sign(method, in, caller, secret, uint64(time.Now().UnixMilli())))
}

func resolveRequest(t *testing.T, mode tradepb.ManualContractMode, mutate func(*tradepb.ResolveManualContractRequest)) *tradepb.ResolveManualContractRequest {
	t.Helper()
	in := &tradepb.ResolveManualContractRequest{CommerceId: 42, Mode: mode, Action: "refund", ExpectVersion: 7, ExpectTarget: "staging-global",
		Operator: "ops", Reason: "ticket-1"}
	if mutate != nil {
		mutate(in)
	}
	signManual(t, tradepb.TradeAdmin_ResolveManualContract_FullMethodName, in, config.ManualResolutionCaller, testManualSecret)
	return in
}

func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, status.Code(err), "%v", err)
}

// 入口装配:没配变量名就是关闭;配了但密钥太短、环境名不合规、没有合同引擎都回错误(入口保持关闭)。
func TestNewManualContractOpsFromConfig(t *testing.T) {
	getenv := func(string) string { return testManualSecret }
	ops, err := NewManualContractOpsFromConfig(nil, config.CommerceConf{}, getenv)
	require.NoError(t, err)
	require.Nil(t, ops, "没配变量名 = 入口关闭")

	_, err = NewManualContractOpsFromConfig(nil, config.CommerceConf{ManualResolutionSecretEnv: "X", ManualResolutionEnvironment: "staging"}, getenv)
	require.Error(t, err, "资产通道没开就没有合同引擎")

	_, err = NewManualContractOps(&fakeManualStore{}, "short", "staging", "")
	require.Error(t, err, "密钥不足 32 字节")
	_, err = NewManualContractOps(&fakeManualStore{}, testManualSecret, "staging global", "")
	require.Error(t, err, "环境名带空格")
	_, err = NewManualContractOps(&fakeManualStore{}, testManualSecret, "", "")
	require.Error(t, err, "缺环境名")
}

// 准入在任何读写之前:入口关闭、验签失败、调用方不对、带客户端会话一律 PermissionDenied,store 一次都没被调。
func TestManualContractOpsAdmission(t *testing.T) {
	store := &fakeManualStore{}
	var disabled *ManualContractOps
	_, err := disabled.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE, nil))
	requireCode(t, err, codes.PermissionDenied)

	ops := newTestManualOps(t, store)
	wrongSecret := &tradepb.ResolveManualContractRequest{CommerceId: 42, Mode: tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW}
	signManual(t, tradepb.TradeAdmin_ResolveManualContract_FullMethodName, wrongSecret, config.ManualResolutionCaller, strings.Repeat("x", 40))
	_, err = ops.ResolveManualContract(context.Background(), wrongSecret)
	requireCode(t, err, codes.PermissionDenied)

	wrongCaller := &tradepb.ResolveManualContractRequest{CommerceId: 42, Mode: tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW}
	signManual(t, tradepb.TradeAdmin_ResolveManualContract_FullMethodName, wrongCaller, "ops-formation", testManualSecret)
	_, err = ops.ResolveManualContract(context.Background(), wrongCaller)
	requireCode(t, err, codes.PermissionDenied)

	// 签名绑定方法:拿 List 的签名调 Resolve 不行。
	crossMethod := &tradepb.ResolveManualContractRequest{CommerceId: 42, Mode: tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW}
	signManual(t, tradepb.TradeAdmin_ListManualContracts_FullMethodName, crossMethod, config.ManualResolutionCaller, testManualSecret)
	_, err = ops.ResolveManualContract(context.Background(), crossMethod)
	requireCode(t, err, codes.PermissionDenied)

	client, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: "account-1", OwnerRevision: 1, Role: &base.AccountSimplePlayer{PlayerId: 1}})
	require.NoError(t, err)
	_, err = ops.ResolveManualContract(client, resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW, nil))
	requireCode(t, err, codes.PermissionDenied)

	require.Empty(t, store.requests, "被拒的调用不碰合同")
}

// 连错环境:expect_target 与本部署的环境名不同就拒绝,不碰合同。
func TestManualContractOpsTargetMismatch(t *testing.T) {
	store := &fakeManualStore{}
	ops := newTestManualOps(t, store)
	out, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE, func(in *tradepb.ResolveManualContractRequest) {
		in.ExpectTarget = "prod-global"
	}))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, out.Verdict)
	require.Equal(t, "target_mismatch", out.Refusal)
	require.Equal(t, "staging-global", out.Target)
	require.Empty(t, store.requests)
}

// commerce 的结论映射成带内 verdict:成功、不存在、拒绝、版本已变、读库失败;DRY_RUN 不写、EXECUTE 写。
func TestManualContractOpsVerdicts(t *testing.T) {
	audit := &tradepb.TradeCommerceManualAction{Action: "refund", VersionBefore: 7, NewLegs: []string{"seller_debit/refund"}}
	cases := []struct {
		name string
		err  error
		want tradepb.ManualContractVerdict
	}{
		{"成功", nil, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK},
		{"不存在", commerce.ErrManualNotFound, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND},
		{"拒绝", fmt.Errorf("%w: 已有发放到账", commerce.ErrManualNotAllowed), tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED},
		{"版本已变", fmt.Errorf("%w(现在是 8)", commerce.ErrManualVersionChanged), tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_VERSION_CHANGED},
		{"读库失败", errors.New("boom"), tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_RETRY},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeManualStore{resolveErr: c.err, result: commerce.ManualResult{Stage: "return", Status: commerce.CancelPending, Audit: audit}}
			ops := newTestManualOps(t, store)
			out, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE, nil))
			require.NoError(t, err)
			require.Equal(t, c.want, out.Verdict)
			require.Equal(t, []bool{true}, store.executes)
			require.Contains(t, out.AuditLine, "[CommerceManual] ")
			require.Equal(t, c.err == nil, strings.Contains(out.AuditLine, "executed=true"))
			if c.err == nil {
				require.True(t, proto.Equal(audit, out.Plan))
				require.Equal(t, "return", out.Stage)
			}
		})
	}

	store := &fakeManualStore{result: commerce.ManualResult{Audit: audit}}
	ops := newTestManualOps(t, store)
	out, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN, nil))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK, out.Verdict)
	require.Equal(t, []bool{false}, store.executes, "DRY_RUN 不写")
	require.Contains(t, out.AuditLine, "executed=false")
}

// 请求里的参数原样交给 commerce;缺口写错在入口就拒绝。
func TestManualContractOpsPassesParameters(t *testing.T) {
	store := &fakeManualStore{}
	ops := newTestManualOps(t, store)
	_, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN, func(in *tradepb.ResolveManualContractRequest) {
		in.Action, in.Parts = "partial", []string{"buyer_receive=instance:7,currency:1"}
	}))
	require.NoError(t, err)
	require.Len(t, store.requests, 1)
	got := store.requests[0]
	require.Equal(t, commerce.ManualPartial, got.Action)
	require.Equal(t, uint64(7), got.ExpectVersion)
	require.Equal(t, "buyer_receive", got.Parts[0].Leg)
	require.Equal(t, commerce.PartialMissing{Instances: []uint64{7}, Currencies: []uint32{1}}, got.Parts[0].Missing)

	out, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN, func(in *tradepb.ResolveManualContractRequest) {
		in.Action, in.Parts = "partial", []string{"buyer_receive=item:10:2"}
	}))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, out.Verdict)

	out, err = ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN, func(in *tradepb.ResolveManualContractRequest) {
		in.Action = "delete"
	}))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, out.Verdict)
	require.Len(t, store.requests, 1, "写错的请求不交给 commerce")
}

// 审计行里调用方填的自由文本一律引起来:操作人填成 `x executed=true` 也伪造不出字段。
func TestManualContractOpsAuditQuotesFreeText(t *testing.T) {
	store := &fakeManualStore{resolveErr: fmt.Errorf("%w: 已有发放到账", commerce.ErrManualNotAllowed)}
	ops := newTestManualOps(t, store)
	out, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE, func(in *tradepb.ResolveManualContractRequest) {
		in.Operator, in.Reason = "x executed=true", "y verdict=OK"
	}))
	require.NoError(t, err)
	require.Contains(t, out.AuditLine, `operator="x executed=true"`)
	require.Contains(t, out.AuditLine, `reason="y verdict=OK"`)
	require.True(t, strings.HasSuffix(out.AuditLine, "executed=false"))
}

// SHOW 把报告原样填进回包;不存在回 NOT_FOUND;读失败回 RETRY。
func TestManualContractOpsShow(t *testing.T) {
	record := &tradepb.TradeCommerceRecord{CommerceId: 42, Kind: 3, Status: commerce.Manual, Version: 7}
	store := &fakeManualStore{report: commerce.ManualReport{
		Record:   record,
		Legs:     []commerce.ManualLegFact{{Leg: &tradepb.TradeCommerceLeg{Role: "seller_debit", OpId: 11}, OpStatus: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, FinalMs: 99}},
		Parties:  []commerce.ManualPartyFact{{Party: "seller", Player: 1, ContractRevision: 1, CurrentRevision: 2, Changed: true}},
		Verdicts: []commerce.ManualVerdict{{Action: commerce.ManualRefund, Allowed: true, Detail: "stage=return"}},
	}}
	ops := newTestManualOps(t, store)
	out, err := ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW, nil))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK, out.Verdict)
	require.True(t, proto.Equal(record, out.Record))
	require.Equal(t, uint64(99), out.Legs[0].FinalMs)
	require.True(t, out.Parties[0].Changed)
	require.Equal(t, "refund", out.Actions[0].Action)
	require.Empty(t, store.requests, "SHOW 不走处置")

	store.showErr = commerce.ErrManualNotFound
	out, err = ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW, nil))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND, out.Verdict)

	store.showErr = errors.New("boom")
	out, err = ops.ResolveManualContract(context.Background(), resolveRequest(t, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW, nil))
	require.NoError(t, err)
	require.Equal(t, tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_RETRY, out.Verdict)
}

// 列表:limit 越界回 InvalidArgument;满页才给下一页游标;读失败带内 retry。
func TestManualContractOpsList(t *testing.T) {
	rows := []*tradepb.TradeCommerceRecord{{CommerceId: 1, UpdatedMs: 10}, {CommerceId: 2, UpdatedMs: 20}}
	store := &fakeManualStore{rows: rows}
	ops := newTestManualOps(t, store)
	list := func(limit uint32) (*tradepb.ListManualContractsResponse, error) {
		in := &tradepb.ListManualContractsRequest{Limit: limit}
		signManual(t, tradepb.TradeAdmin_ListManualContracts_FullMethodName, in, config.ManualResolutionCaller, testManualSecret)
		return ops.ListManualContracts(context.Background(), in)
	}
	_, err := list(0)
	requireCode(t, err, codes.InvalidArgument)
	_, err = list(commerce.ManualListPageMax + 1)
	requireCode(t, err, codes.InvalidArgument)

	out, err := list(2)
	require.NoError(t, err)
	require.Len(t, out.Contracts, 2)
	require.Equal(t, uint64(20), out.NextAfterUpdatedMs)
	require.Equal(t, uint64(2), out.NextAfterCommerceId)

	out, err = list(5)
	require.NoError(t, err)
	require.Zero(t, out.NextAfterCommerceId, "不满一页:没有下一页")

	store.listErr = errors.New("boom")
	out, err = list(5)
	require.NoError(t, err)
	require.True(t, out.Retry)
}
