package svc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	"login/internal/config"
	dspb "proto/data_service"
	"shared/idsegment"
)

// playerIDBizTag 是 id_segment 表里 PlayerId 的业务键。全球唯一号源,不带 zone。
const playerIDBizTag = "player"

// playerIDWarmTimeout 是启动时同步领第一段的预算。只为把「配置错 / data_service 不通」
// 尽早暴露在启动日志里;超时不阻止起服(号段是弱依赖,见 shared/idsegment 包注释)。
const playerIDWarmTimeout = 5 * time.Second

// initPlayerIDMinter 按 IdSegment 配置组装建角发号策略(设计稿 §6.4):
//
//   - Enabled=false:不拨 data_service,PlayerIDMinter 纯走 SnowFlake —— 回滚开关。
//   - Enabled=true,FallbackToSnowflake=false(默认):号段失败 → 建角整体失败。
//   - Enabled=true,FallbackToSnowflake=true:号段失败 → 记 ERROR 后走 SnowFlake。
//
// SnowFlake 本身的接线(login.go 里的槽位申领 / 水位 / fence)一字不动:
// 它既是可选回退,也是解码存量 PlayerId 的依据。
func (s *ServiceContext) initPlayerIDMinter() {
	cfg := config.AppConfig.IdSegment
	if !cfg.Enabled {
		logx.Infof("[id-segment] PlayerId minting: snowflake-only (IdSegment.Enabled=false; "+
			"set IdSegment.Enabled=true + DataServiceRpc to mint from id_segment biz_tag=%s)", playerIDBizTag)
		s.PlayerIDMinter = buildPlayerIDMinter(cfg, nil, s.snowflakePlayerID)
		return
	}
	if !hasRpcTarget(config.AppConfig.DataServiceRpc) {
		// 开了号段却没配 data_service 客户端:静默退回 snowflake 会让运维以为号段已上线,
		// 所以这里直接拒绝起服。
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
	s.PlayerIDMinter = buildPlayerIDMinter(cfg, seg, s.snowflakePlayerID)
	logx.Infof("[id-segment] PlayerId minting: segment biz_tag=%s initial_step=%d step_bounds=[%d, %d] fallback_to_snowflake=%v",
		playerIDBizTag, cfg.StepOrDefault(), cfg.MinStepOrDefault(), cfg.MaxStepOrDefault(), cfg.FallbackToSnowflake)
}

// buildPlayerIDMinter 是策略的纯函数版,单测直接打它(见 player_id_minter_test.go)。
// segment 为 nil 表示号段关闭。
func buildPlayerIDMinter(cfg idsegment.Conf, segment idsegment.Source,
	snowflake func() (uint64, error)) *idsegment.Minter {
	m := &idsegment.Minter{Name: "player_id"}
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

// snowflakePlayerID 把 SnowFlake(在 SetNodeId 里才装上)包成 Minter 的回退。
// 延迟取 s.SnowFlake 是因为 ServiceContext 构造时它还是 nil。
func (s *ServiceContext) snowflakePlayerID() (uint64, error) {
	g := s.SnowFlake
	if g == nil {
		return 0, errors.New("login: snowflake player id generator not initialised (SetNodeId not called)")
	}
	id, err := g.Generate()
	if err != nil {
		return 0, err
	}
	return uint64(id), nil
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
			"— CreatePlayer will retry per request; with FallbackToSnowflake=%v it %s until data_service answers",
			err, config.AppConfig.IdSegment.FallbackToSnowflake,
			map[bool]string{true: "falls back to snowflake", false: "fails closed"}[config.AppConfig.IdSegment.FallbackToSnowflake])
	}
}

// hasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target)。
func hasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}
