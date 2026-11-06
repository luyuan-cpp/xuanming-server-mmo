package server

import (
	"context"
	"encoding/base64"
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
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

// requireClaimRejected 按生产顺序经会话准入后领取附件,断言按无效请求拒绝。
// nil仓储保证这些拒绝发生在发号、落库、分配资产seq之前，而非依赖失败伪装拒绝。
func requireClaimRejected(t *testing.T, ctx context.Context) {
	t.Helper()
	handler := New(nil, nil)
	response, err := domainhost.Guard(ClientMethods(), time.Second)(ctx,
		&mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{9}},
		&grpc.UnaryServerInfo{FullMethod: claimMethod},
		func(bound context.Context, request any) (any, error) {
			return handler.ClaimMailAttachments(bound, request.(*mailpb.ClaimMailAttachmentsRequest))
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.(*mailpb.ClaimMailAttachmentsResponse).GetErrorMessage().GetId(); got != uint32(table.CommonError_kInvalidParameter) {
		t.Fatalf("失效原会话被允许领取: tip=%d", got)
	}
}

// withClaimedSession 保留 ctx 里的验签原件,只把透传的会话 metadata 换成另一份声明,模拟业务解码结果与原件不符。
func withClaimedSession(t *testing.T, ctx context.Context, claimed *base.SessionDetails) context.Context {
	t.Helper()
	raw, err := proto.Marshal(claimed)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromIncomingContext(ctx)
	md = md.Copy()
	md.Set(domainhost.SessionMetadata, base64.StdEncoding.EncodeToString(raw))
	return metadata.NewIncomingContext(ctx, md)
}

// 原账号只认 gate 签名原件,不读 login Redis:签了名的完整会话直接捕获原账号与原修订。
func TestMailActorCapturesSignedOriginalAccount(t *testing.T) {
	signed := callerauthtest.OnlineSession("原卖家", 8, 3)
	calls := 0
	_, err := domainhost.Guard(ClientMethods(), time.Second)(callerauthtest.Incoming(t, claimMethod, signed),
		&mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{9}},
		&grpc.UnaryServerInfo{FullMethod: claimMethod},
		func(decoded context.Context, _ any) (any, error) {
			calls++
			bound, player, ok := New(nil, nil).actor(decoded)
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
	// 没有验签原件的自报身份。缺 gate 进程或连接代次的会话本就过不了验签拦截器,到这里也只能是这种。
	for name, mutate := range map[string]func(*base.SessionDetails){
		"未签名": func(*base.SessionDetails) {},
		"缺进程": func(d *base.SessionDetails) { d.GateInstanceId = "" },
		"缺代次": func(d *base.SessionDetails) { d.SessionIncarnation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			session := callerauthtest.OnlineSession("原卖家", 8, 3)
			mutate(session)
			requireClaimRejected(t, callerauthtest.Unverified(t, session))
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
			requireClaimRejected(t, callerauthtest.Incoming(t, claimMethod, session))
		})
	}
}

// 透传的会话声明与验签原件不符时拒绝:不能借另一条连接、另一个账号或另一次归属的签名领附件。
func TestMailRejectsClaimsThatDifferFromSignedOriginal(t *testing.T) {
	signed := callerauthtest.OnlineSession("原卖家", 8, 3)
	for name, mutate := range map[string]func(*base.SessionDetails){
		"新修订":      func(d *base.SessionDetails) { d.OwnerRevision++ },
		"换角色":      func(d *base.SessionDetails) { d.PlayerId++ },
		"换账号":      func(d *base.SessionDetails) { d.AuthenticatedAccount = "别人" },
		"重启后的gate": func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" },
		"新连接代次":    func(d *base.SessionDetails) { d.SessionIncarnation++ },
	} {
		t.Run(name, func(t *testing.T) {
			claimed := proto.Clone(signed).(*base.SessionDetails)
			mutate(claimed)
			requireClaimRejected(t, withClaimedSession(t, callerauthtest.Incoming(t, claimMethod, signed), claimed))
		})
	}
}
