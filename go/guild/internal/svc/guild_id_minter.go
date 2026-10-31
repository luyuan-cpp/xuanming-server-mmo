package svc

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	dspb "proto/data_service"
	"shared/idsegment"
)

// guildIDBizTag 是发号器里 guild_id 的业务键。公会跨 zone,全球唯一号源。
const guildIDBizTag = "guild"

// guildIDWarmTimeout 是启动时同步领第一段的预算。只为把「配置错 / data_service 不通」
// 尽早暴露在启动日志里;超时不阻止起服(号段是弱依赖,见 shared/idsegment 包注释)。
const guildIDWarmTimeout = 5 * time.Second

// initDataServiceClient 按 DataServiceRpc 建 data_service 客户端。guild 用它做两件事:
// 领 guild_id 号段(initGuildIDSegment)与查玩家归属 zone(logic.DataServiceHomeZone,客户端请求的
// zone 隔离)。没配目标时保持 nil,由两个使用方各自决定拒启(号段)还是拒请求(归属 zone)。
// 配置里 NonBlock: true,data_service 暂未起时不阻塞 guild 起服。
func (s *ServiceContext) initDataServiceClient() {
	if !hasRpcTarget(s.Config.DataServiceRpc) {
		return
	}
	conn := zrpc.MustNewClient(s.Config.DataServiceRpc)
	s.DataServiceClient = dspb.NewDataServiceClient(conn.Conn())
}

// initGuildIDSegment 按 IdSegment 配置建号段客户端;Enabled=false 什么都不做。
func (s *ServiceContext) initGuildIDSegment() {
	cfg := s.Config.IdSegment
	if !cfg.Enabled {
		logx.Infof("[id-segment] guild_id minting: snowflake-only (IdSegment.Enabled=false; "+
			"set IdSegment.Enabled=true + DataServiceRpc to mint from the id issuer, biz_tag=%s)", guildIDBizTag)
		return
	}
	if s.DataServiceClient == nil {
		// 开了号段却没配 data_service 客户端:静默退回 snowflake 会让运维以为号段已上线,
		// 所以这里直接拒绝起服。客户端由 initDataServiceClient 按 DataServiceRpc 建。
		panic(fmt.Errorf("IdSegment.Enabled=true but DataServiceRpc has no Etcd.Key / Endpoints / Target " +
			"(guild needs data_service.AllocateIdSegment to mint guild ids)"))
	}

	seg, err := idsegment.New(
		idsegment.Adapt(s.DataServiceClient.AllocateIdSegment,
			func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
				return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
			}),
		// Step 只是初值:客户端按每段消耗时长在 [MinStep, MaxStep] 内自适应(Leaf 口径),
		// 上限 1000 让 guild 每天重启的浪费可以忽略。
		idsegment.Options{
			BizTag:  guildIDBizTag,
			Step:    cfg.StepOrDefault(),
			MinStep: cfg.MinStepOrDefault(),
			MaxStep: cfg.MaxStepOrDefault(),
		},
	)
	if err != nil {
		panic(fmt.Errorf("guild id segment client: %w", err))
	}
	s.GuildIDSegment = seg
	logx.Infof("[id-segment] guild_id minting: segment biz_tag=%s initial_step=%d step_bounds=[%d, %d]",
		guildIDBizTag, cfg.StepOrDefault(), cfg.MinStepOrDefault(), cfg.MaxStepOrDefault())
}

// NewGuildIDMinter 组装建帮发号:只有号段一条路,号段失败即建帮失败。
// segment 为 nil(IdSegment.Enabled=false)时发不出 id,建帮一律失败。
func NewGuildIDMinter(segment *idsegment.Client) *idsegment.Minter {
	m := &idsegment.Minter{Name: "guild_id"}
	if segment != nil { // 别把 nil *Client 装进非 nil 接口
		m.Segment = segment
	}
	return m
}

// WarmGuildIDSegment 启动时同步领第一段;失败只告警(之后每次 Next 会自己再试)。
func (s *ServiceContext) WarmGuildIDSegment() {
	if s.GuildIDSegment == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guildIDWarmTimeout)
	defer cancel()
	if err := s.GuildIDSegment.Warm(ctx); err != nil {
		logx.Errorf("[id-segment] guild_id first segment not available yet (data_service.AllocateIdSegment): %v "+
			"— CreateGuild will retry per request and fails closed until data_service answers", err)
	}
}

// hasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target)。
func hasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}
