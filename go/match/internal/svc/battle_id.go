package svc

import (
	"context"
	"errors"
	"time"

	dspb "proto/data_service"
	"shared/idsegment"
)

// BattleIDBizTag 是发号器里 battle_id / challenge_id / team_id 共用的业务键。
// 三种 id 共用一个号段所以互不重复;都是打散的计数器值,不能解析、不能比大小。
const BattleIDBizTag = "battle"

// battleIDMintTimeout 是发一个 id 的预算。手里有号时不等待;两段都用完、data_service 又不通才会等到这么久。
const battleIDMintTimeout = 2 * time.Second

// BattleIDGenerator 发 battle_id / challenge_id / team_id。
// 发不出号时返回错误,调用方必须整体失败,不许用 0 顶替。
type BattleIDGenerator interface {
	Generate() (uint64, error)
}

// segmentBattleIDs 是 BattleIDGenerator 的号段实现。调用点(gather、撮合循环、建队)不传 ctx,所以自带时限。
type segmentBattleIDs struct {
	segment *idsegment.Client
}

func (g segmentBattleIDs) Generate() (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), battleIDMintTimeout)
	defer cancel()
	return g.segment.Next(ctx)
}

// NewBattleIDSegment 按 IdSegment 配置建号段客户端。不做 IO,第一段在首次发号时才领。
// 不在启动时 Warm:K8s 上 match 先于 data-service 部署,预领必失败,而领段结局未知会让这一轮启动的
// 维护排空出不了收据(idsegment.Client.DrainAndReport)。真要发号时 zone 已经起来了。
// 没开或没配 data_service 时报错:发不出 id 的 match 什么都做不了,调用方应拒绝起服。
func NewBattleIDSegment(cfg idsegment.Conf, client dspb.DataServiceClient) (*idsegment.Client, error) {
	if !cfg.Enabled {
		return nil, errors.New("IdSegment.Enabled must be true (match mints battle_id, challenge_id and team_id from the id issuer)")
	}
	if client == nil {
		return nil, errors.New("IdSegment.Enabled=true but DataServiceRpc has no Etcd.Key / Endpoints / Target (match needs data_service.AllocateIdSegment)")
	}
	return idsegment.New(
		idsegment.Adapt(client.AllocateIdSegment,
			func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
				return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
			}),
		idsegment.Options{
			BizTag:  BattleIDBizTag,
			Step:    cfg.StepOrDefault(),
			MinStep: cfg.MinStepOrDefault(),
			MaxStep: cfg.MaxStepOrDefault(),
		},
	)
}
