package server

import (
	"context"
	"strings"
	"testing"

	base "proto/common/base"
	loginpb "proto/login"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// LoginAdmin 双人复核拦截器(admin_approval.go)。拦截器只看请求,handler 换成记号:没进 handler 就是没有任何副作用。

const testOperator = "cs-1"

// approvalFor 是一份合格的批准记录:发起人是 proposer,批准人是另一个人。
func approvalFor(proposer string) *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: "900", Proposer: proposer, Approver: "reviewer", ApprovedAtMs: 1}
}

// adminCommand 是一条 LoginAdmin 写命令:方法与按批准记录造请求(操作人都是 testOperator)。
type adminCommand struct {
	method string
	build  func(approval *base.AdminApproval) any
}

func adminWriteCommands() []adminCommand {
	return []adminCommand{
		{loginpb.LoginAdmin_RemovePlayersFromAccounts_FullMethodName, func(a *base.AdminApproval) any {
			return &loginpb.RemovePlayersFromAccountsRequest{PlayerIds: []uint64{7}, Operator: testOperator, Reason: "orphan", Approval: a}
		}},
		{loginpb.LoginAdmin_ScheduleAccountDeletion_FullMethodName, func(a *base.AdminApproval) any {
			return &loginpb.ScheduleAccountDeletionRequest{PlayerId: 7, Operator: testOperator, Reason: "player asked", Approval: a}
		}},
		{loginpb.LoginAdmin_ResetAccountRealName_FullMethodName, func(a *base.AdminApproval) any {
			return &loginpb.ResetAccountRealNameRequest{PlayerId: 7, Operator: testOperator, Reason: "wrong id", Approval: a}
		}},
	}
}

// callApproval 让 req 过双人复核拦截器,回 handler 有没有被调到。
func callApproval(t *testing.T, allowUnapproved bool, method string, req any) (bool, error) {
	t.Helper()
	called := false
	_, err := AdminApprovalInterceptor(allowUnapproved)(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) {
			called = true
			return nil, nil
		})
	return called, err
}

func requireRefused(t *testing.T, called bool, err error, wantMessagePrefix string) {
	t.Helper()
	if called || status.Code(err) != codes.PermissionDenied || !strings.HasPrefix(status.Convert(err).Message(), wantMessagePrefix) {
		t.Fatalf("应当拒绝且不进 handler(%s): called=%t err=%v", wantMessagePrefix, called, err)
	}
}

// 三个写命令不带批准记录一律 PermissionDenied,不进 handler。
func TestAdminApprovalRefusesWriteCommandsWithoutRecord(t *testing.T) {
	for _, c := range adminWriteCommands() {
		t.Run(c.method, func(t *testing.T) {
			called, err := callApproval(t, false, c.method, c.build(nil))
			requireRefused(t, called, err, "dual_control_approval_required")
		})
	}
}

// 合格的记录放行。
func TestAdminApprovalAcceptsAQualifiedRecord(t *testing.T) {
	for _, c := range adminWriteCommands() {
		t.Run(c.method, func(t *testing.T) {
			called, err := callApproval(t, false, c.method, c.build(approvalFor(testOperator)))
			if err != nil || !called {
				t.Fatalf("应当放行: called=%t err=%v", called, err)
			}
		})
	}
}

// 不合格的记录:发起人不是请求的操作人、发起人自批、缺批准时刻。开关打开时也一样拒绝。
func TestAdminApprovalRefusesInvalidRecords(t *testing.T) {
	selfApproved := approvalFor(testOperator)
	selfApproved.Approver = testOperator
	noTime := approvalFor(testOperator)
	noTime.ApprovedAtMs = 0
	for name, approval := range map[string]*base.AdminApproval{
		"发起人不是操作人": approvalFor("cs-2"),
		"发起人自批":    selfApproved,
		"缺批准时刻":    noTime,
	} {
		t.Run(name, func(t *testing.T) {
			for _, c := range adminWriteCommands() {
				for _, allow := range []bool{false, true} {
					called, err := callApproval(t, allow, c.method, c.build(approval))
					requireRefused(t, called, err, "dual_control_approval_invalid: ")
				}
			}
		})
	}
}

// 摘角色的 dry_run 只核对不写:不带记录放行,带了不合格的照样拒绝。
func TestAdminApprovalDryRunNeedsNoRecordButChecksAPresentOne(t *testing.T) {
	method := loginpb.LoginAdmin_RemovePlayersFromAccounts_FullMethodName
	dryRun := func(a *base.AdminApproval) any {
		return &loginpb.RemovePlayersFromAccountsRequest{PlayerIds: []uint64{7}, Operator: testOperator, Reason: "orphan", DryRun: true, Approval: a}
	}
	if called, err := callApproval(t, false, method, dryRun(nil)); err != nil || !called {
		t.Fatalf("dry_run 不要求批准记录: called=%t err=%v", called, err)
	}
	called, err := callApproval(t, false, method, dryRun(approvalFor("cs-2")))
	requireRefused(t, called, err, "dual_control_approval_invalid: ")
}

// 只有开关打开才接受不带记录的写命令,每次按方法计数。
func TestAdminApprovalAllowUnapprovedAcceptsAndCounts(t *testing.T) {
	for _, c := range adminWriteCommands() {
		t.Run(c.method, func(t *testing.T) {
			counter := unapprovedCommandTotal.WithLabelValues(strings.TrimPrefix(c.method, adminMethodPrefix))
			before := testutil.ToFloat64(counter)
			called, err := callApproval(t, true, c.method, c.build(nil))
			if err != nil || !called {
				t.Fatalf("开关打开时应当放行: called=%t err=%v", called, err)
			}
			if got := testutil.ToFloat64(counter); got != before+1 {
				t.Fatalf("不带记录被接受要计数: before=%v after=%v", before, got)
			}
			// 带了合格记录的不计数。
			if _, err := callApproval(t, true, c.method, c.build(approvalFor(testOperator))); err != nil {
				t.Fatalf("合格记录应当放行: %v", err)
			}
			if got := testutil.ToFloat64(counter); got != before+1 {
				t.Fatalf("带了记录的不计数: before=%v after=%v", before, got)
			}
		})
	}
}

// 只管 LoginAdmin;LoginAdmin 下没归类的请求一律拒绝(开关打开也一样),新方法不会漏挂。
func TestAdminApprovalScope(t *testing.T) {
	if called, err := callApproval(t, false, loginpb.ClientPlayerLogin_Login_FullMethodName, &loginpb.LoginRequest{}); err != nil || !called {
		t.Fatalf("不该拦别的服务: called=%t err=%v", called, err)
	}
	called, err := callApproval(t, true, adminMethodPrefix+"FutureCommand", &loginpb.LoginRequest{})
	requireRefused(t, called, err, "FutureCommand is not classified")
}

// LoginAdmin 的每个方法的请求类型都在 approvalOf 里归了类。
func TestAdminApprovalClassifiesEveryLoginAdminMethod(t *testing.T) {
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(loginpb.LoginAdmin_ServiceDesc.ServiceName))
	if err != nil {
		t.Fatalf("find %s: %v", loginpb.LoginAdmin_ServiceDesc.ServiceName, err)
	}
	methods := desc.(protoreflect.ServiceDescriptor).Methods()
	if methods.Len() == 0 {
		t.Fatal("LoginAdmin declares no method")
	}
	for i := 0; i < methods.Len(); i++ {
		input := methods.Get(i).Input()
		mt, err := protoregistry.GlobalTypes.FindMessageByName(input.FullName())
		if err != nil {
			t.Fatalf("find %s: %v", input.FullName(), err)
		}
		if _, _, _, classified := approvalOf(mt.New().Interface()); !classified {
			t.Errorf("%s(%s) 没在 approvalOf 里归类:双人复核拦截器会一律拒绝它", methods.Get(i).Name(), input.FullName())
		}
	}
}
