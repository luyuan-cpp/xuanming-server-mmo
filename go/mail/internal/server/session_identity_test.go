package server

import (
	"context"
	"errors"
	"testing"
	"time"

	base "proto/common/base"
	mailpb "proto/mail"
	"shared/callerauth/callerauthtest"
	"shared/domainhost"
	table "shared/generated/pb/table"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serverbase"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// claimMethod 是测试链路模拟的客户端方法;gate 的签名绑定方法名。
const claimMethod = mailpb.ClientPlayerMail_ClaimMailAttachments_FullMethodName

// 规格 §3.6:自愈读不到目录时事务已回滚,回非故障 tip,排空证明不能 Hold。真正的故障照旧。
func TestRejectionRoleFenceUnavailableIsNotAFault(t *testing.T) {
	tip := rejection(errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")))
	if tip.GetId() == uint32(table.CommonError_kServiceUnavailable) || serverbase.TipVerdict(tip.GetId()) != serverbase.VerdictBizReject {
		t.Fatalf("tip = %d", tip.GetId())
	}
	if fault := rejection(errors.New("db down")); fault.GetId() != uint32(table.CommonError_kServiceUnavailable) {
		t.Fatalf("真正的故障 tip = %d", fault.GetId())
	}
}

// claimThroughGuard 按生产顺序经入口准入(domainhost.Guard)后领取附件。nil 仓储保证被拒的请求
// 发生在发号、落库、分配资产 seq 之前,而非依赖失败伪装拒绝。
func claimThroughGuard(ctx context.Context) (*mailpb.ClaimMailAttachmentsResponse, error) {
	handler := New(nil, nil)
	response, err := domainhost.Guard(time.Second)(ctx,
		&mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{9}},
		&grpc.UnaryServerInfo{FullMethod: claimMethod},
		func(bound context.Context, request any) (any, error) {
			return handler.ClaimMailAttachments(bound, request.(*mailpb.ClaimMailAttachmentsRequest))
		})
	if response == nil {
		return nil, err
	}
	return response.(*mailpb.ClaimMailAttachmentsResponse), err
}

// 原账号只认 gate 签名原件,不读 login Redis:签了名的完整会话在入口就捕获原账号与原修订。
func TestMailActorUsesSignedOriginalAccount(t *testing.T) {
	signed := callerauthtest.OnlineSession("原卖家", 8, 3)
	calls := 0
	_, err := domainhost.Guard(time.Second)(callerauthtest.Incoming(t, claimMethod, signed),
		&mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{9}},
		&grpc.UnaryServerInfo{FullMethod: claimMethod},
		func(admitted context.Context, _ any) (any, error) {
			calls++
			bound, player, ok := New(nil, nil).actor(admitted)
			captured, found := roleidentity.Original(bound)
			if !ok || player != 8 || !found || captured.GetAccount() != "原卖家" || captured.GetOwnerRevision() != 3 || captured.GetRole().GetPlayerId() != 8 {
				t.Fatalf("签名原件应捕获原账号与原修订: ok=%v player=%d captured=%v", ok, player, captured)
			}
			return nil, nil
		})
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestMailRejectsUnsignedOrIncompleteSessionBeforeRepository(t *testing.T) {
	// 没有验签原件的自报身份。缺 gate 进程或连接代次的会话本就过不了验签拦截器,到这里也只能是这种:Unauthenticated。
	for name, mutate := range map[string]func(*base.SessionDetails){
		"未签名": func(*base.SessionDetails) {},
		"缺进程": func(d *base.SessionDetails) { d.GateInstanceId = "" },
		"缺代次": func(d *base.SessionDetails) { d.SessionIncarnation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			session := callerauthtest.OnlineSession("原卖家", 8, 3)
			mutate(session)
			_, err := claimThroughGuard(callerauthtest.Unverified(t, session))
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("未签名的会话必须在入口被拒: %v", err)
			}
		})
	}
	// gate 签了名也不能凭不完整的会话授予在线角色身份:未绑角色时会话里没有账号与原修订。
	for name, mutate := range map[string]func(*base.SessionDetails){
		"缺修订": func(d *base.SessionDetails) { d.OwnerRevision = 0 },
		"缺账号": func(d *base.SessionDetails) { d.AuthenticatedAccount = "" },
	} {
		t.Run(name, func(t *testing.T) {
			session := callerauthtest.OnlineSession("原卖家", 8, 3)
			mutate(session)
			_, err := claimThroughGuard(callerauthtest.Incoming(t, claimMethod, session))
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("不完整的在线身份必须在入口被拒: %v", err)
			}
		})
	}
	// 不带会话调客户端方法只可能是内部进程写错了:同样在入口拒绝,不进业务。
	if _, err := claimThroughGuard(context.Background()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("无会话调客户端方法必须在入口被拒: %v", err)
	}
}

// 入口准入被绕过时(直接调 handler、ctx 里没有原件)业务层自己也按无效请求拒绝,不进仓储。
func TestMailHandlerRejectsCallsWithoutCapturedOriginal(t *testing.T) {
	response, err := New(nil, nil).ClaimMailAttachments(context.Background(), &mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{9}})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.GetErrorMessage().GetId(); got != uint32(table.CommonError_kInvalidParameter) {
		t.Fatalf("没有原件的请求被允许领取: tip=%d", got)
	}
}
