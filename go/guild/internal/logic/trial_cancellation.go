package logic

import (
	"context"
	"errors"
	"time"

	matchpb "proto/match"
	"shared/serviceauth"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// signedTrialStarter 给开局请求加 guild 的签名:match 读不到 guild 的库,原件由这个签名担保(docs/design/horizontal-scale-logical-db.md §4)。
type signedTrialStarter struct {
	client TrialBattleStarter
	secret string
}

// NewSignedTrialStarter 装配。secret 和取消用同一把:guild 调 MatchInternal 只有这一把密钥。
func NewSignedTrialStarter(client TrialBattleStarter, secret string) (TrialBattleStarter, error) {
	if client == nil {
		return nil, errors.New("历练开局客户端未装配")
	}
	if _, err := serviceauth.New(map[string]string{"guild": secret}); err != nil {
		return nil, err
	}
	return &signedTrialStarter{client: client, secret: secret}, nil
}

func (s *signedTrialStarter) StartActivityBattle(ctx context.Context, in *matchpb.StartActivityBattleRequest, opts ...grpc.CallOption) (*matchpb.StartActivityBattleResponse, error) {
	if err := serviceauth.Sign(matchpb.MatchInternal_StartActivityBattle_FullMethodName, in, "guild", s.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, err
	}
	return s.client.StartActivityBattle(ctx, in, opts...)
}

type TrialBattleCanceller interface {
	CancelActivityBattle(context.Context, *matchpb.CancelActivityBattleRequest, ...grpc.CallOption) (*matchpb.CancelActivityBattleResponse, error)
}
type TrialCancellation interface {
	CancelOriginal(context.Context, *matchpb.StartActivityBattleRequest) (*matchpb.CancelActivityBattleResponse, error)
}
type signedTrialCancellation struct {
	client   TrialBattleCanceller
	secret   string
	verifier *serviceauth.Verifier
}

func NewTrialCancellation(client TrialBattleCanceller, secret string) (TrialCancellation, error) {
	if client == nil {
		return nil, errors.New("历练取消客户端未装配")
	}
	verifier, err := serviceauth.New(map[string]string{"match": secret})
	if err != nil {
		return nil, err
	}
	return &signedTrialCancellation{client: client, secret: secret, verifier: verifier}, nil
}
func (c *signedTrialCancellation) CancelOriginal(ctx context.Context, original *matchpb.StartActivityBattleRequest) (*matchpb.CancelActivityBattleResponse, error) {
	if original == nil {
		return nil, errors.New("历练取消缺原开战正文")
	}
	in := &matchpb.CancelActivityBattleRequest{Original: proto.Clone(original).(*matchpb.StartActivityBattleRequest)}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	method := matchpb.MatchInternal_CancelActivityBattle_FullMethodName
	if err = serviceauth.Sign(method, in, "guild", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, err
	}
	out, err := c.client.CancelActivityBattle(ctx, in)
	if err != nil {
		return nil, err
	}
	caller, err := c.verifier.Verify(ctx, method+"/reply", out)
	if err != nil || caller != "match" || out.RequestDigest != digest {
		return nil, serviceauth.ErrDenied
	}
	if out.RequestCancelled && out.BattleId != 0 || out.FormationState > 5 {
		return nil, serviceauth.ErrDenied
	}
	return proto.Clone(out).(*matchpb.CancelActivityBattleResponse), nil
}
