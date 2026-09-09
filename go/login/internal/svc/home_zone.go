package svc

import (
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	"login/internal/config"
	"login/internal/logic/pkg/homezone"
	dspb "proto/data_service"
)

// initHomeZoneResolver 给 CreatePlayer / Login / EnterGame 装上 player:zone 映射的客户端。
//
// 必须在 initPlayerIDMinter 之后调用:号段开着时它已经拨好了 DataServiceClient,
// 这里直接复用同一条连接;号段关着(IdSegment.Enabled=false)但 DataServiceRpc
// 配了目标时,这里单独拨一次 —— 否则合服后的 zone 修正会随号段的回滚开关一起
// 静默失效。两者都没有时 Resolver.Client 为 nil:读路径按 ErrUnavailable 走
// 「保留存量 zone」,不拒绝登录;写路径(CreatePlayer 的 RegisterPlayerZone)会
// 让建角失败 —— 所以这里按 ERROR 而不是 WARN 报,运维必须看到。
func (s *ServiceContext) initHomeZoneResolver() {
	if s.DataServiceClient == nil && hasRpcTarget(config.AppConfig.DataServiceRpc) {
		conn := zrpc.MustNewClient(config.AppConfig.DataServiceRpc)
		s.DataServiceClient = dspb.NewDataServiceClient(conn.Conn())
	}
	hz := config.AppConfig.HomeZone
	s.HomeZone = homezone.New(s.DataServiceClient, hz.RoleListLookupTimeout, hz.EnterLookupTimeout, hz.RegisterTimeout)
	if s.DataServiceClient == nil {
		logx.Errorf("[home-zone] DataServiceRpc not configured: CreatePlayer will be REJECTED (player:zone mapping " +
			"is mandatory); role list zone_id and EnterGame zone routing fall back to creation-time / login zone")
		return
	}
	logx.Infof("[home-zone] resolver ready: refresh_role_list=%v redirect_on_enter=%v "+
		"role_list_timeout=%s enter_timeout=%s register_timeout=%s",
		!hz.RefreshRoleListDisabled, hz.RedirectOnEnterEnabled,
		s.HomeZone.RoleListTimeout, s.HomeZone.EnterTimeout, s.HomeZone.RegisterTimeout)
}
