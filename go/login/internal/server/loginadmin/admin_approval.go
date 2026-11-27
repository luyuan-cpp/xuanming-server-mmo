package server

import (
	"context"
	"strings"

	base "proto/common/base"
	loginpb "proto/login"
	"shared/dualcontrol"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LoginAdmin 的双人复核核对(docs/design/admin-identity-and-dual-control.md §3 甲级、§5.5)。
//
// 三个方法都是甲级:注销、摘孤儿角色(写不可逆墓碑)、重置实名。请求要带后台(java/gm_admin)签发的批准记录,
// 由 shared/dualcontrol 核对两人不同、发起人就是请求里的 operator。摘角色的 dry_run 只核对不写,不要求记录,带了照样核。
// 三个方法都没有请求号,重放天然收敛(删过的回 NOT_FOUND、已在冷静期的回原状态、已清的实名回 had_real_name=false),所以不核批准号。
//
// 做成拦截器、排在口令闸门之后:拒绝发生在 handler 之前,不碰目录、Redis 或任何下游;新加的 LoginAdmin 方法没在 approvalOf 里归类,
// 一律拒绝,不会漏挂。不带记录只在部署显式打开 AdminAllowUnapproved(本机开发)时接受,每次都打 ERROR 并计数,告警据此触发。

// unapprovedCommandTotal 计没带批准记录却被接受的 LoginAdmin 写命令;只在 AdminAllowUnapproved 打开时增长,集群里应当恒为 0。
// method 只取 LoginAdmin 的方法名,封闭集合。
var unapprovedCommandTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "login_admin_unapproved_command_total",
	Help: "LoginAdmin commands accepted without a dual-control approval record (only while AdminAllowUnapproved is on).",
}, []string{"method"})

// AdminApprovalInterceptor 返回 LoginAdmin 的双人复核拦截器。allowUnapproved 是配置 AdminAllowUnapproved,打开时启动打一行 ERROR。
func AdminApprovalInterceptor(allowUnapproved bool) grpc.UnaryServerInterceptor {
	if allowUnapproved {
		logx.Errorf("[LoginAdmin] AdminAllowUnapproved is on: LoginAdmin commands without a dual-control approval record are accepted; keep it off outside local development")
	}
	// 先建出每个方法的序列(值 0):计数第一次出现就是 1 时,告警的 increase 看不到这一次。
	for _, m := range loginpb.LoginAdmin_ServiceDesc.Methods {
		unapprovedCommandTotal.WithLabelValues(m.MethodName)
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, adminMethodPrefix) {
			return handler(ctx, req)
		}
		if err := admitApproval(ctx, strings.TrimPrefix(info.FullMethod, adminMethodPrefix), req, allowUnapproved); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// admitApproval 核对一条 LoginAdmin 请求的批准记录,nil 才放行。拒绝一律 PermissionDenied(确定的拒绝),文本与 trade 的带内原因码相同。
// 日志不回显请求里的自由文本:到这里 operator 还没经过 handler 的长度校验。
func admitApproval(ctx context.Context, method string, req any, allowUnapproved bool) error {
	approval, operator, required, classified := approvalOf(req)
	if !classified {
		logx.WithContext(ctx).Errorf("[LoginAdmin] %s refused: the request is not classified for dual control peer=%s", method, peerAddr(ctx))
		return status.Errorf(codes.PermissionDenied, "%s is not classified for dual control", method)
	}
	if approval == nil {
		if !required {
			return nil
		}
		if !allowUnapproved {
			logx.WithContext(ctx).Errorf("[LoginAdmin] %s refused: dual-control approval record is missing peer=%s", method, peerAddr(ctx))
			return status.Error(codes.PermissionDenied, "dual_control_approval_required")
		}
		unapprovedCommandTotal.WithLabelValues(method).Inc()
		logx.WithContext(ctx).Errorf("[LoginAdmin] %s accepted without a dual-control approval record peer=%s (AdminAllowUnapproved is on)", method, peerAddr(ctx))
		return nil
	}
	if err := dualcontrol.Require(approval, operator); err != nil {
		logx.WithContext(ctx).Errorf("[LoginAdmin] %s refused: %v peer=%s", method, err, peerAddr(ctx))
		return status.Error(codes.PermissionDenied, "dual_control_approval_invalid: "+err.Error())
	}
	return nil
}

// approvalOf 取请求里的批准记录与操作人。required:没带记录时要不要拒绝。classified 为 false 表示不认识的请求(新方法还没归类)。
func approvalOf(req any) (approval *base.AdminApproval, operator string, required, classified bool) {
	switch in := req.(type) {
	case *loginpb.RemovePlayersFromAccountsRequest:
		return in.GetApproval(), in.GetOperator(), !in.GetDryRun(), true
	case *loginpb.ScheduleAccountDeletionRequest:
		return in.GetApproval(), in.GetOperator(), true, true
	case *loginpb.ResetAccountRealNameRequest:
		return in.GetApproval(), in.GetOperator(), true, true
	}
	return nil, "", false, false
}
