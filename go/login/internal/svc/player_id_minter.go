package svc

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	"login/internal/config"
	dspb "proto/data_service"
	"shared/idsegment"
)

// playerIDBizTag 是发号器里 PlayerId 的业务键。全球唯一号源,不带 zone。
const playerIDBizTag = "player"

// playerIDWarmTimeout 是启动时同步领第一段的预算。只为把「配置错 / data_service 不通」
// 尽早暴露在启动日志里;超时不阻止起服(号段是弱依赖,见 shared/idsegment 包注释)。
const playerIDWarmTimeout = 5 * time.Second

// initPlayerIDMinter 按 IdSegment 配置组装建角发号:只有号段一条路,号段失败即建角失败。
// Enabled=false 时发不出 id,建角一律失败,所以大声记日志。
func (s *ServiceContext) initPlayerIDMinter() {
	cfg := config.AppConfig.IdSegment
	if !cfg.Enabled {
		logx.Errorf("[id-segment] IdSegment.Enabled=false: login cannot mint player ids, CreatePlayer will fail. "+
			"Set IdSegment.Enabled=true + DataServiceRpc (biz_tag=%s)", playerIDBizTag)
		s.PlayerIDMinter = &idsegment.Minter{Name: "player_id"}
		return
	}
	if !hasRpcTarget(config.AppConfig.DataServiceRpc) {
		// 开了号段却没配 data_service 客户端,一个 id 也发不出来,直接拒绝起服。
		panic(fmt.Errorf("IdSegment.Enabled=true but DataServiceRpc has no Etcd.Key / Endpoints / Target " +
			"(login needs data_service.AllocateIdSegment to mint player ids)"))
	}
	conn := zrpc.MustNewClient(config.AppConfig.DataServiceRpc)
	s.DataServiceClient = dspb.NewDataServiceClient(conn.Conn())

	seg, err := idsegment.New(
		idsegment.Adapt(s.DataServiceClient.AllocateIdSegment,
			func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
				return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
			}),
		// Step 只是初值:客户端按每段消耗时长在 [MinStep, MaxStep] 内自适应(Leaf 口径),
		// 上限 1000 让 login 每天重启的浪费可以忽略。
		idsegment.Options{
			BizTag:  playerIDBizTag,
			Step:    cfg.StepOrDefault(),
			MinStep: cfg.MinStepOrDefault(),
			MaxStep: cfg.MaxStepOrDefault(),
		},
	)
	if err != nil {
		panic(fmt.Errorf("player id segment client: %w", err))
	}
	s.PlayerIDSegment = seg
	s.PlayerIDMinter = &idsegment.Minter{Name: "player_id", Segment: seg}
	logx.Infof("[id-segment] PlayerId minting: segment biz_tag=%s initial_step=%d step_bounds=[%d, %d]",
		playerIDBizTag, cfg.StepOrDefault(), cfg.MinStepOrDefault(), cfg.MaxStepOrDefault())
}

// warmPlayerIDSegment 启动时同步领第一段;失败只告警(之后每次 Next 会自己再试)。
func (s *ServiceContext) warmPlayerIDSegment() {
	if s.PlayerIDSegment == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), playerIDWarmTimeout)
	defer cancel()
	if err := s.PlayerIDSegment.Warm(ctx); err != nil {
		logx.Errorf("[id-segment] PlayerId first segment not available yet (data_service.AllocateIdSegment): %v "+
			"— CreatePlayer will retry per request and fails closed until data_service answers", err)
	}
}

// hasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target)。
func hasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}
