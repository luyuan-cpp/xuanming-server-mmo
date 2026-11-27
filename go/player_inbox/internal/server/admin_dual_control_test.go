package server

// 两个甲级运维写方法的双人复核核对(docs/design/admin-identity-and-dual-control.md §5.5、§5.7):
// 口令拦截器之后、任何读库写库与判重之前核批准记录;被拒的请求不碰存储,文本不回显请求内容;
// 不带记录只在 PlayerInbox.AdminAllowUnapproved 打开时受理,每次计数;批准号与批准人进审计行。

import (
	"strings"
	"testing"

	"player_inbox/internal/inbox"
	"player_inbox/internal/metrics"

	base "proto/common/base"
	inboxpb "proto/player_inbox"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	inboxProposer = "ops_alice"
	inboxApprover = "ops_reviewer"
)

// inboxApproval 是后台在第二个人批准时生成的批准记录。
func inboxApproval(id, proposer string) *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: id, Proposer: proposer, Approver: inboxApprover, ApprovedAtMs: 1_799_999_999_500}
}

// sanctionWith 是带指定批准记录的封锁处置(其余字段同 blockRequest,request_id = 9001)。
func sanctionWith(approval *base.AdminApproval) *inboxpb.GmSanctionPlayerRequest {
	req := blockRequest()
	req.Approval = approval
	return req
}

// voidWith 是带指定批准记录的普通作废(操作人 inboxProposer)。
func voidWith(approval *base.AdminApproval) *inboxpb.VoidPlayerInboxItemRequest {
	return &inboxpb.VoidPlayerInboxItemRequest{PlayerId: 42, Domain: gmDomain, Seq: 3,
		Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED, Operator: inboxProposer, Note: "poison", Approval: approval}
}

// unapprovedCount 读 player_inbox_admin_unapproved_command_total{method}。Register 进程内只生效一次,预建了两个方法的序列;
// 断言一律取增量,不依赖别的用例打过的点。
func unapprovedCount(t *testing.T, method string) float64 {
	t.Helper()
	metrics.Register(nil)
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "player_inbox_admin_unapproved_command_total" {
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
	t.Fatalf("player_inbox_admin_unapproved_command_total{method=%q} 不存在", method)
	return 0
}

// 不带记录、发起人不是操作人、请求号不是批准号(只有 GmSanctionPlayer 有请求号)都拒绝:PermissionDenied、不碰存储、不计数。
func TestAdminWritesRequireDualControlApproval(t *testing.T) {
	cases := map[string]struct {
		method string
		call   func(f *adminFixture) error
	}{
		"GmSanctionPlayer/不带批准记录": {metrics.AdminMethodGmSanctionPlayer, func(f *adminFixture) error {
			_, err := f.server.GmSanctionPlayer(peerCtx(), sanctionWith(nil))
			return err
		}},
		"GmSanctionPlayer/发起人不是操作人": {metrics.AdminMethodGmSanctionPlayer, func(f *adminFixture) error {
			_, err := f.server.GmSanctionPlayer(peerCtx(), sanctionWith(inboxApproval("9001", "ops_mallory")))
			return err
		}},
		"GmSanctionPlayer/请求号不是批准号": {metrics.AdminMethodGmSanctionPlayer, func(f *adminFixture) error {
			_, err := f.server.GmSanctionPlayer(peerCtx(), sanctionWith(inboxApproval("9002", inboxProposer)))
			return err
		}},
		"VoidItem/不带批准记录": {metrics.AdminMethodVoidItem, func(f *adminFixture) error {
			_, err := f.server.VoidItem(peerCtx(), voidWith(nil))
			return err
		}},
		"VoidItem/发起人不是操作人": {metrics.AdminMethodVoidItem, func(f *adminFixture) error {
			_, err := f.server.VoidItem(peerCtx(), voidWith(inboxApproval("5001", "ops_mallory")))
			return err
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAdminFixture(t)
			f.sanction.result = inbox.EnqueueResult{Seq: 1, Created: true}
			before := unapprovedCount(t, c.method)
			err := c.call(f)
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("err = %v, want PermissionDenied", err)
			}
			if msg := status.Convert(err).Message(); strings.Contains(msg, "ops_") || strings.Contains(msg, "工单") {
				t.Errorf("拒绝文本回显了请求内容: %q", msg)
			}
			if len(f.sanction.params) != 0 || len(f.audits) != 0 || len(f.store.sawDeadline) != 0 {
				t.Errorf("被拒的请求碰了存储: enqueue=%v audits=%v store=%v", f.sanction.params, f.audits, f.store.sawDeadline)
			}
			if got := unapprovedCount(t, c.method); got != before {
				t.Errorf("被拒的请求计入了不带记录的受理: %v → %v", before, got)
			}
		})
	}
}

// 合格的批准记录放行,批准号与批准人进审计行(GmSanctionPlayer 经入箱事务的审计回调,VoidItem 经 VoidParams)。
func TestAdminWritesPassWithAValidApprovalAndRecordIt(t *testing.T) {
	f := newAdminFixture(t)
	f.sanction.result = inbox.EnqueueResult{Seq: 123, Created: true, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}
	resp, err := f.server.GmSanctionPlayer(peerCtx(), sanctionWith(inboxApproval("9001", inboxProposer)))
	if err != nil || resp.GetErrorMessage() != nil || resp.GetSeq() != 123 || len(f.sanction.params) != 1 {
		t.Fatalf("GmSanctionPlayer = %+v, %v(enqueue=%v)", resp, err, f.sanction.params)
	}
	if len(f.audits) != 1 || f.audits[0].GetApprovalId() != "9001" || f.audits[0].GetApprover() != inboxApprover ||
		f.audits[0].GetOperator() != inboxProposer {
		t.Fatalf("审计行 = %+v,应带批准号 9001 与批准人 %s", f.audits, inboxApprover)
	}

	void, err := f.server.VoidItem(peerCtx(), voidWith(inboxApproval("5001", inboxProposer)))
	if err != nil || !void.GetVoidRequested() {
		t.Fatalf("VoidItem = %+v, %v", void, err)
	}
	if p := f.store.voidParams; p.ApprovalID != "5001" || p.Approver != inboxApprover || p.Operator != inboxProposer {
		t.Fatalf("VoidParams = %+v,批准号与批准人要交给存储层写审计", p)
	}
}

// 只有 AdminAllowUnapproved 打开时才受理不带记录的写命令,每次计数,审计行的批准号与批准人为空;带了记录就必须合格,开关开着也一样。
func TestAdminWritesWithoutApprovalOnlyWhenExplicitlyAllowed(t *testing.T) {
	f := newAdminFixture(t)
	f.server.allowUnapproved = true
	f.sanction.result = inbox.EnqueueResult{Seq: 7, Created: true}
	sanctionBefore := unapprovedCount(t, metrics.AdminMethodGmSanctionPlayer)
	voidBefore := unapprovedCount(t, metrics.AdminMethodVoidItem)

	if resp, err := f.server.GmSanctionPlayer(peerCtx(), sanctionWith(nil)); err != nil || resp.GetErrorMessage() != nil || resp.GetSeq() != 7 {
		t.Fatalf("GmSanctionPlayer = %+v, %v", resp, err)
	}
	if len(f.audits) != 1 || f.audits[0].GetApprovalId() != "" || f.audits[0].GetApprover() != "" {
		t.Fatalf("不带记录受理的审计行 = %+v,批准号与批准人应为空", f.audits)
	}
	if _, err := f.server.VoidItem(peerCtx(), voidWith(nil)); err != nil || f.store.voidParams.PlayerID != 42 {
		t.Fatalf("VoidItem err = %v, VoidParams = %+v", err, f.store.voidParams)
	}
	if got := unapprovedCount(t, metrics.AdminMethodGmSanctionPlayer); got != sanctionBefore+1 {
		t.Errorf("GmSanctionPlayer 不带记录的受理计数 %v → %v, want +1", sanctionBefore, got)
	}
	if got := unapprovedCount(t, metrics.AdminMethodVoidItem); got != voidBefore+1 {
		t.Errorf("VoidItem 不带记录的受理计数 %v → %v, want +1", voidBefore, got)
	}

	f.store.voidParams = inbox.VoidParams{}
	if _, err := f.server.VoidItem(peerCtx(), voidWith(inboxApproval("5001", "ops_mallory"))); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("开关开着也要拒绝不合格的批准记录: %v", err)
	}
	if f.store.voidParams.PlayerID != 0 {
		t.Fatalf("被拒的作废碰了存储: %+v", f.store.voidParams)
	}
	if got := unapprovedCount(t, metrics.AdminMethodVoidItem); got != voidBefore+1 {
		t.Errorf("不合格的记录不算不带记录的受理: %v → %v", voidBefore+1, got)
	}
}
