package server

import (
	"context"
	"errors"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/inbox"

	inboxpb "proto/player_inbox"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeConsumer struct {
	err         error
	sawDeadline bool
}

const consumerFixtureSecret = "fixture-only-scene-inbox-signing-secret-000000"

type fakeBattleProducer struct {
	fakeConsumer
	calls int
}

func (f *fakeBattleProducer) EnqueueBattleSettlement(ctx context.Context, r *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error) {
	f.calls++
	return &inboxpb.BattleSettlementInboxResponse{PlayerId: r.PlayerId, BattleId: r.BattleId, Seq: 42, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}, f.err
}
func (f *fakeBattleProducer) LookupBattleSettlement(ctx context.Context, r *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error) {
	return f.EnqueueBattleSettlement(ctx, r)
}

func TestBattleProducerMethodsRequireIndependentSignatureAndBindReply(t *testing.T) {
	const secret = "fixture-only-match-inbox-secret-000000000000"
	t.Setenv("MMORPG_PLAYER_INBOX_SECRET_MATCH", secret)
	store := &fakeBattleProducer{}
	s := newTestConsumer(t, store, time.Second)
	r := &inboxpb.BattleSettlementInboxRequest{PlayerId: 11, BattleId: 21, ExpectedOwnerAccount: "original", ExpectedOwnerRevision: 3, IssuedAtMs: 7, SettlementPayload: []byte{1}}
	method := inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName
	if _, err := s.EnqueueBattleSettlement(context.Background(), r); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("缺签名应拒: %v", err)
	}
	if err := serviceauth.Sign(method, r, "scene", consumerFixtureSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueBattleSettlement(context.Background(), r); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Scene不能签Match生产入口: %v", err)
	}
	if err := serviceauth.Sign(method, r, "match", secret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(r).(*inboxpb.BattleSettlementInboxRequest)
	changed.ExpectedOwnerRevision++
	if _, err := s.EnqueueBattleSettlement(context.Background(), changed); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("改owner原件应拒: %v", err)
	}
	if store.calls != 0 {
		t.Fatal("错误签名不得触存储")
	}
	resp, err := s.EnqueueBattleSettlement(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	v, err := serviceauth.New(map[string]string{"inbox": secret})
	if err != nil {
		t.Fatal(err)
	}
	if caller, err := v.Verify(context.Background(), method+"/reply", resp); err != nil || caller != "inbox" {
		t.Fatalf("回执签名无效: %s %v", caller, err)
	}
	digest, err := serviceauth.Digest(r)
	if err != nil || resp.RequestDigest != digest || resp.Seq != 42 {
		t.Fatal("回执未绑定原请求")
	}
	if _, err = s.LookupBattleSettlement(context.Background(), r); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Enqueue签名不能用于Lookup: %v", err)
	}
	method = inboxpb.PlayerInbox_LookupBattleSettlement_FullMethodName
	if err = serviceauth.Sign(method, r, "match", secret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LookupBattleSettlement(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestBattleProducerDistinguishesOriginalConflictFromUnknownFailure(t *testing.T) {
	const secret = "fixture-only-match-inbox-secret-000000000000"
	t.Setenv("MMORPG_PLAYER_INBOX_SECRET_MATCH", secret)
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"原件永久冲突", inbox.ErrBattleSettlementOriginal, codes.FailedPrecondition},
		{"角色冻结仍可恢复", inbox.ErrOriginalRole, codes.Unavailable},
		{"未知SQL错误", errors.New("fixture DB error"), codes.Unavailable},
		{"提交取消仍未知", context.DeadlineExceeded, codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeBattleProducer{fakeConsumer: fakeConsumer{err: tc.err}}
			s := newTestConsumer(t, store, time.Second)
			for _, method := range []string{inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName, inboxpb.PlayerInbox_LookupBattleSettlement_FullMethodName} {
				req := &inboxpb.BattleSettlementInboxRequest{PlayerId: 11, BattleId: 21, ExpectedOwnerAccount: "original", ExpectedOwnerRevision: 3, IssuedAtMs: 7, SettlementPayload: []byte{1}}
				if err := serviceauth.Sign(method, req, "match", secret, uint64(time.Now().UnixMilli())); err != nil {
					t.Fatal(err)
				}
				var err error
				if method == inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName {
					_, err = s.EnqueueBattleSettlement(context.Background(), req)
				} else {
					_, err = s.LookupBattleSettlement(context.Background(), req)
				}
				if status.Code(err) != tc.code {
					t.Fatalf("原分类=%s，期望=%s", status.Code(err), tc.code)
				}
			}
		})
	}
}

func newTestConsumer(t *testing.T, store ConsumerStore, budget time.Duration) *PlayerInboxServer {
	t.Helper()
	verifier, err := serviceauth.New(map[string]string{"scene": consumerFixtureSecret})
	if err != nil {
		t.Fatal(err)
	}
	return NewPlayerInboxServer(store, budget, verifier)
}
func callFetch(t *testing.T, server *PlayerInboxServer, request *inboxpb.FetchPlayerInboxRequest) (*inboxpb.FetchPlayerInboxResponse, error) {
	t.Helper()
	if err := serviceauth.Sign("/player_inbox.PlayerInbox/FetchItems", request, "scene", consumerFixtureSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	return server.FetchItems(context.Background(), request)
}
func callAck(t *testing.T, server *PlayerInboxServer, request *inboxpb.AckPlayerInboxRequest) (*inboxpb.AckPlayerInboxResponse, error) {
	t.Helper()
	if err := serviceauth.Sign("/player_inbox.PlayerInbox/AckItems", request, "scene", consumerFixtureSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	return server.AckItems(context.Background(), request)
}

func TestConsumerRejectsUnsignedAndChangedOriginalRequest(t *testing.T) {
	store := &fakeConsumer{}
	server := newTestConsumer(t, store, time.Second)
	request := &inboxpb.FetchPlayerInboxRequest{PlayerId: 7}
	if _, err := server.FetchItems(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("缺场景签名应拒绝: %v", err)
	}
	if err := serviceauth.Sign("/player_inbox.PlayerInbox/FetchItems", request, "scene", consumerFixtureSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	request.PlayerId = 8
	if _, err := server.FetchItems(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("原请求被修改应拒绝: %v", err)
	}
	if store.sawDeadline {
		t.Fatal("验签失败不得进入存储")
	}
}

func (f *fakeConsumer) Fetch(ctx context.Context, req *inboxpb.FetchPlayerInboxRequest) (*inboxpb.FetchPlayerInboxResponse, error) {
	_, f.sawDeadline = ctx.Deadline()
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.FetchPlayerInboxResponse{PlayerId: req.GetPlayerId(), Fence: inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM}, nil
}

func (f *fakeConsumer) Ack(ctx context.Context, req *inboxpb.AckPlayerInboxRequest) (*inboxpb.AckPlayerInboxResponse, error) {
	_, f.sawDeadline = ctx.Deadline()
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.AckPlayerInboxResponse{PlayerId: req.GetPlayerId()}, nil
}

// TestConsumerServerFoldsFaultsInBand:存储故障 in-band 回 kPlayerInboxStoreUnavailable 并原样回显关联字段
// (scene 靠 player_id + domain + correlation_id 找回在途状态,设计 §7.11);gRPC 层永不报错。
func TestConsumerServerFoldsFaultsInBand(t *testing.T) {
	logtest.Discard(t)
	store := &fakeConsumer{err: errors.New("db down")}
	s := newTestConsumer(t, store, 2500*time.Millisecond)
	fetch, err := callFetch(t, s, &inboxpb.FetchPlayerInboxRequest{
		PlayerId: 7, Domain: inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION, CorrelationId: 11,
	})
	if err != nil || fetch.GetErrorMessage().GetId() != constants.ErrStoreUnavailable ||
		fetch.GetPlayerId() != 7 || fetch.GetCorrelationId() != 11 || fetch.GetDomain() == 0 {
		t.Fatalf("FetchItems = %+v, %v", fetch, err)
	}
	if !store.sawDeadline {
		t.Fatal("请求应带整请求预算截止时间")
	}
	ack, err := callAck(t, s, &inboxpb.AckPlayerInboxRequest{PlayerId: 7, CorrelationId: 12})
	if err != nil || ack.GetErrorMessage().GetId() != constants.ErrStoreUnavailable || ack.GetCorrelationId() != 12 {
		t.Fatalf("AckItems = %+v, %v", ack, err)
	}

	store.err = nil
	fetch, err = callFetch(t, s, &inboxpb.FetchPlayerInboxRequest{PlayerId: 8})
	if err != nil || fetch.GetErrorMessage() != nil || fetch.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM {
		t.Fatalf("成功路径应原样透传: %+v, %v", fetch, err)
	}
}

// TestConsumerServerFaultLogsUseSceneNodeIDKey 是评审 R5 的回归:Fetch / Ack 故障的 ERROR 日志用 scene_node_id= 这个键,
// 与围栏 / Ack 异常日志、告警描述(player-inbox-alerts.yaml)和 runbook 同一写法 —— 值班按 |= "scene_node_id=9"
// 查 Loki 时不会漏掉这两条。原实现写的是 scene_node=。
func TestConsumerServerFaultLogsUseSceneNodeIDKey(t *testing.T) {
	logs := logtest.NewCollector(t)
	s := newTestConsumer(t, &fakeConsumer{err: errors.New("db down")}, 2500*time.Millisecond)
	domain := inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION
	if _, err := callFetch(t, s, &inboxpb.FetchPlayerInboxRequest{PlayerId: 7, Domain: domain, SceneNodeId: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := callAck(t, s, &inboxpb.AckPlayerInboxRequest{PlayerId: 7, Domain: domain, SceneNodeId: 9}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{"FetchItems failed", "AckItems failed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志缺 %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "scene_node_id=9"); n != 2 {
		t.Fatalf("两条故障日志都应带 scene_node_id=9,找到 %d 处:\n%s", n, out)
	}
	if strings.Contains(out, "scene_node=") {
		t.Fatalf("不应再出现 scene_node= 这个键:\n%s", out)
	}
}
