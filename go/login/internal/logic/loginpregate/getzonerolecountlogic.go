package loginpregatelogic

import (
	"context"
	"time"

	"login/internal/config"
	"login/internal/svc"
	dspb "proto/data_service"
	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// zoneRoleCountTimeout 是转问 data_service 的上限。网关自己的截止时间(login.grpc.timeout-ms)更短时,入站 ctx 先到期。
const zoneRoleCountTimeout = 3 * time.Second

// GetZoneRoleCountLogic 回答本区一共建过多少角色(docs/design/zone-full-handling.md §6)。网关里每个区的负责副本按
// zone_config.role_capacity 定时来问,到了上限就关上本区的创角开关;login 自己不判上限,建角仍只看区服策略里的
// createRoleClosed。只读、可重试,不碰 login 的任何状态;数由 data_service 按合服之后的最终区算(CountZoneRoles)。
type GetZoneRoleCountLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetZoneRoleCountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetZoneRoleCountLogic {
	return &GetZoneRoleCountLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetZoneRoleCount 只回答本 login 所在的区:问到别区回 InvalidArgument(网关选路错了,不能拿别区的数开关本区)。
// data_service 没配或读不到回 Unavailable,网关下一拍再问;绝不回 0,0 会被当成「远没到上限」。
// 网关已放弃(入站 ctx 取消 / 超时)时回它的取消原因。
func (l *GetZoneRoleCountLogic) GetZoneRoleCount(in *loginpb.ZoneRoleCountRequest) (*loginpb.ZoneRoleCountResponse, error) {
	zone := config.AppConfig.Node.ZoneId
	if in.GetZoneId() != zone {
		return nil, status.Errorf(codes.InvalidArgument, "zone role count asked for zone %d at the login of zone %d", in.GetZoneId(), zone)
	}
	if l.svcCtx.DataServiceClient == nil {
		return nil, status.Error(codes.Unavailable, "zone role count unavailable: data_service client not configured")
	}
	ctx, cancel := context.WithTimeout(l.ctx, zoneRoleCountTimeout)
	defer cancel()
	resp, err := l.svcCtx.DataServiceClient.CountZoneRoles(ctx, &dspb.CountZoneRolesRequest{ZoneId: zone})
	if err != nil {
		if ctxErr := l.ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		l.Errorf("GetZoneRoleCount zone=%d: data_service CountZoneRoles failed: %v", zone, err)
		return nil, status.Errorf(codes.Unavailable, "zone role count unavailable: %v", err)
	}
	return &loginpb.ZoneRoleCountResponse{Roles: resp.GetRoles()}, nil
}
