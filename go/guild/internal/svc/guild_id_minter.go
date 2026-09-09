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

// guildIDBizTag 是 id_segment 表里 guild_id 的业务键。公会跨 zone,全球唯一号源。
const guildIDBizTag = "guild"

// guildIDWarmTimeout 是启动时同步领第一段的预算。只为把「配置错 / data_service 不通」
// 尽早暴露在启动日志里;超时不阻止起服(号段是弱依赖,见 shared/idsegment 包注释)。
const guildIDWarmTimeout = 5 * time.Second

// initGuildIDSegment 按 IdSegment 配置拨 data_service 并建号段客户端;Enabled=false 什么都不做。
func (s *ServiceContext) initGuildIDSegment() {
	cfg := s.Config.IdSegment
	if !cfg.Enabled {
		logx.Infof("[id-segment] guild_id minting: snowflake-only (IdSegment.Enabled=false; "+
			"set IdSegment.Enabled=true + DataServiceRpc to mint from id_segment biz_tag=%s)", guildIDBizTag)
		return
	}
	if !hasRpcTarget(s.Config.DataServiceRpc) {
		// 开了号段却没配 data_service 客户端:静默退回 snowflake 会让运维以为号段已上线,
		// 所以这里直接拒绝起服。
		panic(fmt.Errorf("IdSegment.Enabled=true but DataServiceRpc has no Etcd.Key / Endpoints / Target " +
			"(guild needs data_service.AllocateIdSegment to mint guild ids)"))
	}
	conn := zrpc.MustNewClient(s.Config.DataServiceRpc)
	s.DataServiceClient = dspb.NewDataServiceClient(conn.Conn())

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
	logx.Infof("[id-segment] guild_id minting: segment biz_tag=%s initial_step=%d step_bounds=[%d, %d] fallback_to_snowflake=%v",
		guildIDBizTag, cfg.StepOrDefault(), cfg.MinStepOrDefault(), cfg.MaxStepOrDefault(), cfg.FallbackToSnowflake)
}

// NewGuildIDMinter 按 IdSegment 配置组装建帮发号策略(设计稿 §6.4):
//
//   - Enabled=false(segment 为 nil):纯 snowflake —— 回滚开关。
//   - Enabled=true,FallbackToSnowflake=false(默认):号段失败 → 建帮整体失败。
//   - Enabled=true,FallbackToSnowflake=true:号段失败 → 记 ERROR 后走 snowflake。
//
// snowflake 是 guild.go 里申领的 shared/snowflake.Node 的 Generate:它的接线(槽位 /
// 水位 / fence)一字不动,既是可选回退,也是解码存量 guild_id 的依据。
func NewGuildIDMinter(cfg idsegment.Conf, segment *idsegment.Client,
	snowflake func() (uint64, error)) *idsegment.Minter {
	var src idsegment.Source
	if segment != nil { // 别把 nil *Client 装进非 nil 接口
		src = segment
	}
	return buildGuildIDMinter(cfg, src, snowflake)
}

// buildGuildIDMinter 是策略的纯函数版,单测直接打它(见 guild_id_minter_test.go)。
func buildGuildIDMinter(cfg idsegment.Conf, segment idsegment.Source,
	snowflake func() (uint64, error)) *idsegment.Minter {
	m := &idsegment.Minter{Name: "guild_id"}
	if !cfg.Enabled || segment == nil {
		m.Fallback = snowflake
		return m
	}
	m.Segment = segment
	if cfg.FallbackToSnowflake {
		m.Fallback = snowflake
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
		mode := "fails closed"
		if s.Config.IdSegment.FallbackToSnowflake {
			mode = "falls back to snowflake"
		}
		logx.Errorf("[id-segment] guild_id first segment not available yet (data_service.AllocateIdSegment): %v "+
			"— CreateGuild will retry per request and %s until data_service answers", err, mode)
	}
}

// hasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target)。
func hasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}
