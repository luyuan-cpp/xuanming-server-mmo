package server

// 三个甲级回收入口的双人复核核对(docs/design/admin-identity-and-dual-control.md §5.5、§5.7):
// 口令之后、任何读库写库与判重重放之前核批准记录;被拒的请求一个存储方法都不调,文本不回显请求内容;
// 不带记录只在 AdminAllowUnapproved 打开时受理,每次计数;批准号与批准人进回收审计(理由文本)。

import (
	"context"
	"errors"
	"testing"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/metrics"
	"data_service/internal/store"
	"data_service/internal/svc"
	base "proto/common/base"
	dbpb "proto/common/database"
	"proto/data_service"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	recallProposer   = "ops-alice"
	recallApprover   = "ops-bob"
	recallApprovalID = "7001"
	// recallApprovalTrail 是 recallApproval(recallApprovalID, …) 写进回收审计的样子(logic.approvalTrail)。
	recallApprovalTrail = `approval_id="7001" approver="ops-bob"`
)

// recallApproval 是后台在第二个人批准时生成的批准记录。
func recallApproval(id, proposer string) *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: id, Proposer: proposer, Approver: recallApprover, ApprovedAtMs: 1_800_000_000_000}
}

// recallStoreProbe 是回收库探针:只实现准入之后第一步会调的三个方法并计数;其余方法 nil 嵌入,被调到就 panic,测试当场红。
type recallStoreProbe struct {
	svc.RecallStore
	calls        int
	cancelReason string
}

func (p *recallStoreProbe) ReadBatch(context.Context, uint64) (*store.RecallBatchRow, error) {
	p.calls++
	return nil, store.ErrRecallBatchNotFound
}

func (p *recallStoreProbe) CancelBatch(_ context.Context, _ uint64, _, reason string, _ uint64) error {
	p.calls++
	p.cancelReason = reason
	return store.ErrRecallBatchNotFound
}

func (p *recallStoreProbe) ReadOpState(context.Context, uint64) (store.RecallOpState, error) {
	p.calls++
	return store.RecallOpState{}, store.ErrRecallOpNotFound
}

// txLogProbe 是流水库探针:演练查流水时计数并回读失败(结论走带内码,不往下算计划)。
type txLogProbe struct {
	svc.TransactionLogStore
	calls int
}

func (p *txLogProbe) QueryLog(context.Context, *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error) {
	p.calls++
	return nil, 0, errors.New("transaction log probe")
}

// auditLogProbe 收下回收的拒绝审计行。
type auditLogProbe struct {
	svc.SnapshotStore
	rows []*store.AuditLogRow
}

func (p *auditLogProbe) InsertAuditLog(_ context.Context, row *store.AuditLogRow) error {
	p.rows = append(p.rows, row)
	return nil
}

type recallProbes struct {
	recall *recallStoreProbe
	txlog  *txLogProbe
	audits *auditLogProbe
}

func (p recallProbes) touched() int { return p.recall.calls + p.txlog.calls + len(p.audits.rows) }

// newRecallDualControlServer 装好口令与三个探针。RecallReady 为 false:执行走到「通道没开」就停,留一行拒绝审计。
func newRecallDualControlServer(allowUnapproved bool) (*DataServiceServer, recallProbes) {
	p := recallProbes{recall: &recallStoreProbe{}, txlog: &txLogProbe{}, audits: &auditLogProbe{}}
	s := NewDataServiceServer(&svc.ServiceContext{
		Config:        config.Config{AdminToken: testAdminToken, AdminAllowUnapproved: allowUnapproved},
		Recall:        p.recall,
		TxLogStore:    p.txlog,
		SnapshotStore: p.audits,
	})
	return s, p
}

// executeRecallRequest 是一份入参齐全的执行请求,请求号 7001。
func executeRecallRequest(approval *base.AdminApproval) *data_service.BatchRecallItemsRequest {
	return &data_service.BatchRecallItemsRequest{
		RecallKind: data_service.RecallKind_RECALL_KIND_ITEM, ItemConfigId: 1001, TimeStart: 1_700_000_000, TimeEnd: 1_700_003_600,
		Reason: "工单 #42", Operator: recallProposer, RequestId: 7001, ExpectedPlanDigest: "ab", Approval: approval,
	}
}

// dryRunRecallRequest 是同一份过滤条件的演练:不带请求号与摘要。
func dryRunRecallRequest(approval *base.AdminApproval) *data_service.BatchRecallItemsRequest {
	req := executeRecallRequest(approval)
	req.DryRun, req.RequestId, req.ExpectedPlanDigest = true, 0, ""
	return req
}

// recallAdminCalls 按 method 调四个甲级入口,入参齐全,只有批准记录由调用方给。
func recallAdminCalls() map[string]func(*DataServiceServer, *base.AdminApproval) error {
	return map[string]func(*DataServiceServer, *base.AdminApproval) error{
		metrics.AdminMethodBatchRecallItems: func(s *DataServiceServer, a *base.AdminApproval) error {
			_, err := s.BatchRecallItems(adminCtx(testAdminToken), executeRecallRequest(a))
			return err
		},
		metrics.AdminMethodCancelRecallBatch: func(s *DataServiceServer, a *base.AdminApproval) error {
			_, err := s.CancelRecallBatch(adminCtx(testAdminToken), &data_service.CancelRecallBatchRequest{
				RequestId: 55, Operator: recallProposer, Reason: "误操作", Approval: a})
			return err
		},
		metrics.AdminMethodResolveRecallOp: func(s *DataServiceServer, a *base.AdminApproval) error {
			_, err := s.ResolveRecallOp(adminCtx(testAdminToken), &data_service.ResolveRecallOpRequest{
				OpId: 9, FinalStatus: uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_ABORTED), Operator: recallProposer, Reason: "工单 #43", Approval: a})
			return err
		},
		// 入参齐全时第一步查流水(txLogProbe 计数并回读失败,结论走带内码)。
		metrics.AdminMethodRegisterRecallReviewHandled: func(s *DataServiceServer, a *base.AdminApproval) error {
			_, err := s.RegisterRecallReviewHandled(adminCtx(testAdminToken), &data_service.RegisterRecallReviewHandledRequest{
				RecallKind: data_service.RecallKind_RECALL_KIND_ITEM, ItemConfigId: 1001, PlayerId: 42, CorrelationId: 9, TxIds: []uint64{1, 2},
				ReviewReason: data_service.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER, Operator: recallProposer, Reason: "工单 #44", Approval: a})
			return err
		},
	}
}

// unapprovedCount 读 data_service_admin_unapproved_command_total{method}。NewDataServiceServer 已预建三个 method 的序列;
// 断言一律取增量,不依赖同包其它用例打过的点。
func unapprovedCount(t *testing.T, method string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != "data_service_admin_unapproved_command_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" && l.GetValue() == method {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("data_service_admin_unapproved_command_total{method=%q} not found", method)
	return 0
}

// 不带记录、发起人不是操作人、请求号不是批准号(只有执行带请求号)都拒绝:PermissionDenied、不碰存储、不计数。
func TestRecallAdminCommandsRequireDualControlApproval(t *testing.T) {
	for method, call := range recallAdminCalls() {
		rejected := map[string]*base.AdminApproval{
			"不带批准记录":   nil,
			"发起人不是操作人": recallApproval(recallApprovalID, "ops-mallory"),
		}
		if method == metrics.AdminMethodBatchRecallItems {
			rejected["请求号不是批准号"] = recallApproval("7999", recallProposer)
		}
		for name, approval := range rejected {
			t.Run(method+"/"+name, func(t *testing.T) {
				s, probes := newRecallDualControlServer(false)
				before := unapprovedCount(t, method)
				err := call(s, approval)
				assertStatusCode(t, err, codes.PermissionDenied, constants.ErrCodeAdminAuthRequired)
				msg := status.Convert(err).Message()
				require.NotContains(t, msg, "ops-", "拒绝文本不回显请求内容")
				require.NotContains(t, msg, "工单")
				require.Zero(t, probes.touched(), "被拒的请求不碰存储")
				require.Equal(t, before, unapprovedCount(t, method), "被拒的请求不算不带记录的受理")
			})
		}
	}
}

// 合格的批准记录放行,结论走带内码(探针被调到);不算不带记录的受理。
func TestRecallAdminCommandsPassWithAValidApproval(t *testing.T) {
	for method, call := range recallAdminCalls() {
		t.Run(method, func(t *testing.T) {
			s, probes := newRecallDualControlServer(false)
			before := unapprovedCount(t, method)
			require.NoError(t, call(s, recallApproval(recallApprovalID, recallProposer)))
			require.NotZero(t, probes.touched(), "合格的批准记录应当放行")
			require.Equal(t, before, unapprovedCount(t, method))
		})
	}
}

// 批准号与批准人进回收审计:执行被拒(通道没开)的审计行、取消审计行的理由都带上两项。
func TestRecallAdminCommandsRecordTheApproval(t *testing.T) {
	s, probes := newRecallDualControlServer(false)
	resp, err := s.BatchRecallItems(adminCtx(testAdminToken), executeRecallRequest(recallApproval(recallApprovalID, recallProposer)))
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeRecallDisabled, resp.GetErrorCode())
	require.Len(t, probes.audits.rows, 1)
	require.Contains(t, probes.audits.rows[0].Reason, recallApprovalTrail)
	require.Equal(t, recallProposer, probes.audits.rows[0].Operator)

	_, err = s.CancelRecallBatch(adminCtx(testAdminToken), &data_service.CancelRecallBatchRequest{
		RequestId: 55, Operator: recallProposer, Reason: "误操作", Approval: recallApproval(recallApprovalID, recallProposer)})
	require.NoError(t, err)
	require.Equal(t, `误操作 [`+recallApprovalTrail+`]`, probes.recall.cancelReason)
}

// 只有 AdminAllowUnapproved 打开时才受理不带记录的命令,每次计数;带了记录就必须合格,开关开着也一样。
func TestRecallAdminCommandsWithoutApprovalOnlyWhenExplicitlyAllowed(t *testing.T) {
	for method, call := range recallAdminCalls() {
		t.Run(method, func(t *testing.T) {
			s, probes := newRecallDualControlServer(true)
			before := unapprovedCount(t, method)
			require.NoError(t, call(s, nil))
			require.NotZero(t, probes.touched(), "开关打开时不带记录照常受理")
			require.Equal(t, before+1, unapprovedCount(t, method))

			s, probes = newRecallDualControlServer(true)
			assertStatusCode(t, call(s, recallApproval(recallApprovalID, "ops-mallory")), codes.PermissionDenied, constants.ErrCodeAdminAuthRequired)
			require.Zero(t, probes.touched())
			require.Equal(t, before+1, unapprovedCount(t, method), "不合格的记录不算不带记录的受理")
		})
	}
}

// 演练不要求批准记录,也不算不带记录的受理;带了就照样核两人与发起人,请求号不必是批准号(演练没有请求号)。
func TestBatchRecallDryRunNeedsNoApproval(t *testing.T) {
	s, probes := newRecallDualControlServer(false)
	before := unapprovedCount(t, metrics.AdminMethodBatchRecallItems)
	_, err := s.BatchRecallItems(adminCtx(testAdminToken), dryRunRecallRequest(nil))
	require.NoError(t, err)
	require.Equal(t, 1, probes.txlog.calls, "演练照常查流水")
	require.Equal(t, before, unapprovedCount(t, metrics.AdminMethodBatchRecallItems))

	_, err = s.BatchRecallItems(adminCtx(testAdminToken), dryRunRecallRequest(recallApproval(recallApprovalID, recallProposer)))
	require.NoError(t, err)
	require.Equal(t, 2, probes.txlog.calls)

	_, err = s.BatchRecallItems(adminCtx(testAdminToken), dryRunRecallRequest(recallApproval(recallApprovalID, "ops-mallory")))
	assertStatusCode(t, err, codes.PermissionDenied, constants.ErrCodeAdminAuthRequired)
	require.Equal(t, 2, probes.txlog.calls, "带了不合格的记录,演练也拒绝")
}
