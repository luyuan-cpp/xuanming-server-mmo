// Package server 是 player_inbox 的 gRPC 适配层:PlayerInbox(scene 调 Fetch / Ack)与 PlayerInboxAdmin(运维)。
// 只做「套整请求预算 → 调 inbox / logic → 故障折成 in-band kPlayerInboxStoreUnavailable」,不含业务判定。
//
// 错误码规定(设计 §1.4):所有响应只用 TipInfoMessage error_message,不带 uint32 error_code;
// handler 一律 `return resp, nil`,由 serverbase 拦截器按 Tip.xlsx 的 fault 列定性。
// 例外:运维口令不对由 AdminAuthInterceptor 回 gRPC PermissionDenied(设计 §11.4 验收)。
package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/inbox"

	base "proto/common/base"
	inboxpb "proto/player_inbox"
	"shared/accountdirectory"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ConsumerStore 是 scene 侧协议需要的存储能力(*inbox.Store 实现它)。
type ConsumerStore interface {
	Fetch(ctx context.Context, req *inboxpb.FetchPlayerInboxRequest) (*inboxpb.FetchPlayerInboxResponse, error)
	Ack(ctx context.Context, req *inboxpb.AckPlayerInboxRequest) (*inboxpb.AckPlayerInboxResponse, error)
}

// PlayerInboxServer 实现 inboxpb.PlayerInboxServer。
type PlayerInboxServer struct {
	inboxpb.UnimplementedPlayerInboxServer
	store    ConsumerStore
	budget   time.Duration
	verifier *serviceauth.Verifier
	producer MatchProducer
}

// MatchProducerSecretEnv 是 Match 结算生产面(EnqueueBattleSettlement / LookupBattleSettlement)签名密钥的环境变量名。
// K8s 只在全球匹配启用时注入(k8s_deploy.ps1 Add-PlayerInboxMatchSecretEnv,Secret domain-service-secrets 的 player-inbox-match)。
const MatchProducerSecretEnv = "MMORPG_PLAYER_INBOX_SECRET_MATCH"

// MatchProducer 是 Match 结算生产面的验签与回执签名材料。零值 = 未装配:两个方法一律回 Unavailable。
// 由装配层(player_inbox.go)构造后传给 NewPlayerInboxServer;构造函数不读环境变量。
type MatchProducer struct {
	verifier *serviceauth.Verifier
	secret   string
}

// NewMatchProducer 用 Match 专用密钥原文构造。缺失或去掉首尾空白后不足 32 字节返回错误;
// 专用面缺 key 只拒这一面,不能把 Scene / GM 密钥借给 Match。
func NewMatchProducer(rawSecret string) (MatchProducer, error) {
	verifier, err := serviceauth.New(map[string]string{"match": rawSecret})
	if err != nil {
		return MatchProducer{}, err
	}
	return MatchProducer{verifier: verifier, secret: rawSecret}, nil
}

// Ready 报告 Match 结算生产面是否已装配签名密钥。
func (p MatchProducer) Ready() bool { return p.verifier != nil }

// NewPlayerInboxServer。budget 是整请求 I/O 预算(config.RequestBudget,= Timeout − 回包余量);
// producer 为零值时 Battle 结算两个方法回 Unavailable(装配层 resolveMatchProducer 已据此打日志并把 gauge 置 0)。
func NewPlayerInboxServer(store ConsumerStore, budget time.Duration, verifier *serviceauth.Verifier, producer MatchProducer) *PlayerInboxServer {
	return &PlayerInboxServer{store: store, budget: budget, verifier: verifier, producer: producer}
}

func NewConsumerVerifier() (*serviceauth.Verifier, error) {
	verifier, err := serviceauth.New(map[string]string{"scene": strings.TrimSpace(os.Getenv(accountdirectory.SceneSecretEnv))})
	if err != nil {
		return nil, errors.New("inbox: 场景原入场签名密钥缺失或不足32字节")
	}
	return verifier, nil
}

func (s *PlayerInboxServer) authorize(ctx context.Context, method string, request proto.Message) error {
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("x-session-detail-bin")) != 0 {
		return status.Error(codes.PermissionDenied, "收件箱消费仅允许场景内部调用")
	}
	if s.verifier == nil {
		return status.Error(codes.Unavailable, "收件箱消费签名未装配")
	}
	caller, err := s.verifier.Verify(ctx, method, request)
	if err != nil || caller != "scene" {
		return status.Error(codes.PermissionDenied, "收件箱场景证明无效")
	}
	return nil
}

func storeUnavailable() *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: constants.ErrStoreUnavailable}
}

// withBudget 给一次请求的全部 I/O 套同一个更早的截止时间:依赖变慢时先在预算内 in-band 回故障码,
// 不让 go-zero 的超时拦截器抢先回 DeadlineExceeded(同 trade P1-9)。
func withBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

// FetchItems 按 seq 升序拉 PENDING 条目并推进围栏(设计 §7.8)。
func (s *PlayerInboxServer) FetchItems(ctx context.Context, in *inboxpb.FetchPlayerInboxRequest) (*inboxpb.FetchPlayerInboxResponse, error) {
	if err := s.authorize(ctx, "/player_inbox.PlayerInbox/FetchItems", in); err != nil {
		return nil, err
	}
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.Fetch(ctx, in)
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] FetchItems failed player=%d domain=%d epoch=%d scene_node_id=%d: %v",
			in.GetPlayerId(), in.GetDomain(), in.GetOwnerEpoch(), in.GetSceneNodeId(), err)
		return &inboxpb.FetchPlayerInboxResponse{
			ErrorMessage: storeUnavailable(),
			PlayerId:     in.GetPlayerId(), Domain: in.GetDomain(), CorrelationId: in.GetCorrelationId(),
		}, nil
	}
	return resp, nil
}

// AckItems 登记消费方已落盘的结局(设计 §7.7)。
func (s *PlayerInboxServer) AckItems(ctx context.Context, in *inboxpb.AckPlayerInboxRequest) (*inboxpb.AckPlayerInboxResponse, error) {
	if err := s.authorize(ctx, "/player_inbox.PlayerInbox/AckItems", in); err != nil {
		return nil, err
	}
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.Ack(ctx, in)
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] AckItems failed player=%d domain=%d epoch=%d decisions=%d scene_node_id=%d: %v",
			in.GetPlayerId(), in.GetDomain(), in.GetOwnerEpoch(), len(in.GetDecisions()), in.GetSceneNodeId(), err)
		return &inboxpb.AckPlayerInboxResponse{
			ErrorMessage: storeUnavailable(),
			PlayerId:     in.GetPlayerId(), Domain: in.GetDomain(), CorrelationId: in.GetCorrelationId(),
		}, nil
	}
	return resp, nil
}

type battleSettlementStore interface {
	EnqueueBattleSettlement(context.Context, *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error)
	LookupBattleSettlement(context.Context, *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error)
}

func (s *PlayerInboxServer) EnqueueBattleSettlement(ctx context.Context, in *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error) {
	return s.battleSettlement(ctx, in, true)
}
func (s *PlayerInboxServer) LookupBattleSettlement(ctx context.Context, in *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error) {
	return s.battleSettlement(ctx, in, false)
}
func (s *PlayerInboxServer) battleSettlement(ctx context.Context, in *inboxpb.BattleSettlementInboxRequest, enqueue bool) (*inboxpb.BattleSettlementInboxResponse, error) {
	method := inboxpb.PlayerInbox_LookupBattleSettlement_FullMethodName
	if enqueue {
		method = inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("x-session-detail-bin")) != 0 {
		return nil, status.Error(codes.PermissionDenied, "结算生产仅限Match内部调用")
	}
	if !s.producer.Ready() {
		return nil, status.Error(codes.Unavailable, "Match结算密钥未装配")
	}
	caller, err := s.producer.verifier.Verify(ctx, method, in)
	if err != nil || caller != "match" {
		return nil, status.Error(codes.PermissionDenied, "Match结算原件证明无效")
	}
	store, ok := s.store.(battleSettlementStore)
	if !ok {
		return nil, status.Error(codes.Unavailable, "结算存储未装配")
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	call, cancel := withBudget(ctx, s.budget)
	defer cancel()
	var resp *inboxpb.BattleSettlementInboxResponse
	if enqueue {
		resp, err = store.EnqueueBattleSettlement(call, in)
	} else {
		resp, err = store.LookupBattleSettlement(call, in)
	}
	if err != nil || resp == nil {
		if errors.Is(err, inbox.ErrBattleSettlementOriginal) {
			logx.Error("metric=inbox_battle_original_rejected reason=original_conflict")
			return nil, status.Error(codes.FailedPrecondition, "结算原件存在永久冲突")
		}
		logx.Error("metric=inbox_battle_receipt_unconfirmed reason=storage_or_transport")
		return nil, status.Error(codes.Unavailable, "结算原收据尚未确认")
	}
	resp.RequestDigest = digest
	if err = serviceauth.Sign(method+"/reply", resp, "inbox", s.producer.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, status.Error(codes.Unavailable, "结算回执签名不可用")
	}
	return resp, nil
}
