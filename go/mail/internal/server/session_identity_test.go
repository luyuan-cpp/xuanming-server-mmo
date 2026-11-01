package server

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	base "proto/common/base"
	mailpb "proto/mail"
	"shared/domainhost"
	table "shared/generated/pb/table"
	"shared/redisslot"
	"shared/rolefence"
	"shared/serverbase"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

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

func TestMailRejectsIncompleteOrRevokedSessionBeforeRepository(t *testing.T) {
	cache := miniredis.RunT(t)
	reader := redis.NewClient(&redis.Options{Addr: cache.Addr()})
	t.Cleanup(func() { _ = reader.Close() })
	cache.Set(redisslot.LoginSessionKey("gate-original:19"), "原卖家")
	for _, tc := range []struct {
		name    string
		session *base.SessionDetails
	}{
		{"缺修订", &base.SessionDetails{SessionId: 7, PlayerId: 8, GateInstanceId: "gate-original", SessionIncarnation: 19}},
		{"缺进程", &base.SessionDetails{SessionId: 7, PlayerId: 8, SessionIncarnation: 19, OwnerRevision: 3}},
		{"缺代次", &base.SessionDetails{SessionId: 7, PlayerId: 8, GateInstanceId: "gate-original", OwnerRevision: 3}},
		{"原连接已撤销", &base.SessionDetails{SessionId: 7, PlayerId: 8, GateInstanceId: "gate-original", SessionIncarnation: 20, OwnerRevision: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := proto.Marshal(tc.session)
			if err != nil {
				t.Fatal(err)
			}
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(domainhost.SessionMetadata, base64.StdEncoding.EncodeToString(raw)))
			// nil仓储保证这些拒绝发生在发号、落库、分配资产seq之前，而非依赖失败伪装拒绝。
			handler := New(nil, nil).WithLoginSessions(reader)
			response, err := domainhost.Guard(ClientMethods(), time.Second)(ctx,
				&mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{9}},
				&grpc.UnaryServerInfo{FullMethod: mailpb.ClientPlayerMail_ClaimMailAttachments_FullMethodName},
				func(bound context.Context, request any) (any, error) {
					return handler.ClaimMailAttachments(bound, request.(*mailpb.ClaimMailAttachmentsRequest))
				})
			if err != nil {
				t.Fatal(err)
			}
			if response.(*mailpb.ClaimMailAttachmentsResponse).GetErrorMessage().GetId() == 0 {
				t.Fatal("失效原会话被允许领取")
			}
		})
	}
}
