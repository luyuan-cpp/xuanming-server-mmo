package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	base "proto/common/base"
	rankpb "proto/rank"
	"rank/internal/data"
	"rank/internal/metrics"
	table "shared/generated/pb/table"
	"shared/serviceauth"
)

// RankAdmin 的删除入口(remove / admitApproval)。不起数据库:库换成每次取连接都失败、只计次数的连接器,
// 「拒绝发生在任何读写之前」就是连接次数为 0;放行的请求走到数据层,取不到连接回 kServiceUnavailable。

const testProducerSecret = "rank-server-test-producer-secret-000000"

// countingConnector 是数据库替身:每次取连接都失败并计数。
type countingConnector struct{ connects atomic.Int32 }

var errNoDatabase = errors.New("fixture: no database in server unit tests")

func (c *countingConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects.Add(1)
	return nil, errNoDatabase
}

func (c *countingConnector) Driver() driver.Driver { return failingDriver{} }

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return nil, errNoDatabase }

func newRemoveFixture(t *testing.T, allowUnapproved bool) (*Server, *countingConnector) {
	t.Helper()
	connector := &countingConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	auth, err := serviceauth.New(map[string]string{adminCaller: testProducerSecret, "match": testProducerSecret})
	require.NoError(t, err)
	return New(data.New(db, nil, nil, nil, nil), auth, nil, allowUnapproved), connector
}

func signedRemove(t *testing.T, method, caller string, in *rankpb.RemoveRankEntryRequest) *rankpb.RemoveRankEntryRequest {
	t.Helper()
	require.NoError(t, serviceauth.Sign(method, in, caller, testProducerSecret, uint64(time.Now().UnixMilli())))
	return in
}

// approvalFor 是一份合格的批准记录,批准号是 approvalID;用例的 request_id 都是 900。
func approvalFor(approvalID string) *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: approvalID, Proposer: "ops-alice", Approver: "ops-bob", ApprovedAtMs: 1}
}

// removeCall 是两个删除方法之一:签名用的方法名、计数的 method、按批准记录造请求、调用。
type removeCall struct {
	name   string
	method string
	label  string
	build  func(approval *base.AdminApproval) *rankpb.RemoveRankEntryRequest
	call   func(s *Server, ctx context.Context, in *rankpb.RemoveRankEntryRequest) (*rankpb.RankAdminResponse, error)
}

func removeCalls() []removeCall {
	return []removeCall{
		{name: "RemoveRankEntry", method: rankpb.RankAdmin_RemoveRankEntry_FullMethodName, label: metrics.AdminMethodRemoveEntry,
			build: func(a *base.AdminApproval) *rankpb.RemoveRankEntryRequest {
				return &rankpb.RemoveRankEntryRequest{BoardId: 10, Generation: 1, EntityId: 7, RequestId: 900, Approval: a}
			},
			call: (*Server).RemoveRankEntry},
		{name: "DeleteRankGeneration", method: rankpb.RankAdmin_DeleteRankGeneration_FullMethodName, label: metrics.AdminMethodDeleteGeneration,
			build: func(a *base.AdminApproval) *rankpb.RemoveRankEntryRequest {
				return &rankpb.RemoveRankEntryRequest{BoardId: 10, Generation: 1, RequestId: 900, Approval: a}
			},
			call: (*Server).DeleteRankGeneration},
	}
}

// requireReachedRepo:请求过了批准核对、走到了数据层(取不到连接,回 kServiceUnavailable)。
func requireReachedRepo(t *testing.T, out *rankpb.RankAdminResponse, err error, connector *countingConnector) {
	t.Helper()
	require.NoError(t, err)
	require.EqualValues(t, table.CommonError_kServiceUnavailable, out.GetErrorMessage().GetId())
	require.NotZero(t, connector.connects.Load(), "放行的请求要走到数据层")
}

// unapprovedCount 从进程默认 registry 读 rank_admin_unapproved_command_total{method}。
func unapprovedCount(t *testing.T, method string) float64 {
	t.Helper()
	metrics.Register()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "rank_admin_unapproved_command_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "method" && label.GetValue() == method {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("rank_admin_unapproved_command_total{method=%q} is not registered", method)
	return 0
}

// 调用方 admin 不带批准记录:PermissionDenied,不碰库(判重键也就没占)。
func TestRemoveByAdminWithoutApprovalIsRefusedBeforeTheDatabase(t *testing.T) {
	for _, c := range removeCalls() {
		t.Run(c.name, func(t *testing.T) {
			s, connector := newRemoveFixture(t, false)
			_, err := c.call(s, context.Background(), signedRemove(t, c.method, adminCaller, c.build(nil)))
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.Equal(t, "dual_control_approval_required", status.Convert(err).Message())
			require.Zero(t, connector.connects.Load(), "拒绝发生在任何读写之前")
		})
	}
}

// 合格的记录(request_id 就是批准号)放行到数据层。
func TestRemoveByAdminWithQualifiedApprovalReachesTheRepo(t *testing.T) {
	for _, c := range removeCalls() {
		t.Run(c.name, func(t *testing.T) {
			s, connector := newRemoveFixture(t, false)
			out, err := c.call(s, context.Background(), signedRemove(t, c.method, adminCaller, c.build(approvalFor("900"))))
			requireReachedRepo(t, out, err, connector)
		})
	}
}

// 请求号不是批准号、发起人自批:拒绝,不碰库。开关打开也一样;match 带了不合格的记录同样拒绝。
func TestRemoveRefusesInvalidApprovals(t *testing.T) {
	selfApproved := approvalFor("900")
	selfApproved.Approver = selfApproved.Proposer
	for name, approval := range map[string]*base.AdminApproval{
		"请求号不是批准号": approvalFor("901"),
		"发起人自批":    selfApproved,
	} {
		t.Run(name, func(t *testing.T) {
			for _, c := range removeCalls() {
				for _, caller := range []string{adminCaller, "match"} {
					for _, allow := range []bool{false, true} {
						s, connector := newRemoveFixture(t, allow)
						_, err := c.call(s, context.Background(), signedRemove(t, c.method, caller, c.build(approval)))
						require.Equal(t, codes.PermissionDenied, status.Code(err), "%s caller=%s allow=%t", c.name, caller, allow)
						require.True(t, strings.HasPrefix(status.Convert(err).Message(), "dual_control_approval_invalid: "), status.Convert(err).Message())
						require.Zero(t, connector.connects.Load(), "拒绝发生在任何读写之前")
					}
				}
			}
		})
	}
}

// match 摘已删角色是自动路径:不带批准记录照常走到数据层,也不计数。
func TestRemoveByMatchNeedsNoApproval(t *testing.T) {
	for _, c := range removeCalls() {
		t.Run(c.name, func(t *testing.T) {
			before := unapprovedCount(t, c.label)
			s, connector := newRemoveFixture(t, false)
			out, err := c.call(s, context.Background(), signedRemove(t, c.method, "match", c.build(nil)))
			requireReachedRepo(t, out, err, connector)
			require.Equal(t, before, unapprovedCount(t, c.label))
		})
	}
}

// 只有开关打开才接受 admin 不带记录的删除,每次按方法计数。
func TestRemoveByAdminWithoutApprovalOnlyWhenAllowed(t *testing.T) {
	for _, c := range removeCalls() {
		t.Run(c.name, func(t *testing.T) {
			before := unapprovedCount(t, c.label)
			s, connector := newRemoveFixture(t, true)
			out, err := c.call(s, context.Background(), signedRemove(t, c.method, adminCaller, c.build(nil)))
			requireReachedRepo(t, out, err, connector)
			require.Equal(t, before+1, unapprovedCount(t, c.label))
		})
	}
}
