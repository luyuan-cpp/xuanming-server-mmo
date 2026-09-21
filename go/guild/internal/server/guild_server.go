package server

import (
	"context"

	"guild/internal/logic"
	base "proto/common/base"
	pb "proto/guild"
)

// GuildServer 是 gRPC 入口薄包装:每个方法只把请求转给对应 logic,不写业务。
// 身份校验、合服闸门、事务与推送全部在 logic 层,这里保持零分支——
// 一旦这里开始出现 if,同一条规则就会在 server 与 logic 各存一份。
type GuildServer struct {
	pb.UnimplementedGuildServiceServer
	logic *logic.GuildLogic
}

func NewGuildServer(l *logic.GuildLogic) *GuildServer {
	return &GuildServer{logic: l}
}

func (s *GuildServer) CreateGuild(ctx context.Context, req *pb.CreateGuildRequest) (*pb.CreateGuildResponse, error) {
	return s.logic.CreateGuild(ctx, req)
}

func (s *GuildServer) GetGuild(ctx context.Context, req *pb.GetGuildRequest) (*pb.GetGuildResponse, error) {
	return s.logic.GetGuild(ctx, req)
}

func (s *GuildServer) GetPlayerGuild(ctx context.Context, req *pb.GetPlayerGuildRequest) (*pb.GetPlayerGuildResponse, error) {
	return s.logic.GetPlayerGuild(ctx, req)
}

func (s *GuildServer) LeaveGuild(ctx context.Context, req *pb.LeaveGuildRequest) (*pb.LeaveGuildResponse, error) {
	return s.logic.LeaveGuild(ctx, req)
}

func (s *GuildServer) DisbandGuild(ctx context.Context, req *pb.DisbandGuildRequest) (*pb.DisbandGuildResponse, error) {
	return s.logic.DisbandGuild(ctx, req)
}

func (s *GuildServer) SetAnnouncement(ctx context.Context, req *pb.SetAnnouncementRequest) (*pb.SetAnnouncementResponse, error) {
	return s.logic.SetAnnouncement(ctx, req)
}

// ── 成员管理(B2)────────────────────────────────────────────────

func (s *GuildServer) SetGuildMemberRole(ctx context.Context, req *pb.SetGuildMemberRoleRequest) (*pb.SetGuildMemberRoleResponse, error) {
	return s.logic.SetGuildMemberRole(ctx, req)
}

func (s *GuildServer) KickGuildMember(ctx context.Context, req *pb.KickGuildMemberRequest) (*pb.KickGuildMemberResponse, error) {
	return s.logic.KickGuildMember(ctx, req)
}

func (s *GuildServer) TransferGuildLeader(ctx context.Context, req *pb.TransferGuildLeaderRequest) (*pb.TransferGuildLeaderResponse, error) {
	return s.logic.TransferGuildLeader(ctx, req)
}

// ── 入帮申请(B2;申请制取代了原先的直接入帮)────────────────────

func (s *GuildServer) ApplyJoinGuild(ctx context.Context, req *pb.ApplyJoinGuildRequest) (*pb.ApplyJoinGuildResponse, error) {
	return s.logic.ApplyJoinGuild(ctx, req)
}

func (s *GuildServer) CancelGuildApplication(ctx context.Context, req *pb.CancelGuildApplicationRequest) (*pb.CancelGuildApplicationResponse, error) {
	return s.logic.CancelGuildApplication(ctx, req)
}

func (s *GuildServer) ListMyGuildApplications(ctx context.Context, req *pb.ListMyGuildApplicationsRequest) (*pb.ListMyGuildApplicationsResponse, error) {
	return s.logic.ListMyGuildApplications(ctx, req)
}

func (s *GuildServer) ListGuildApplications(ctx context.Context, req *pb.ListGuildApplicationsRequest) (*pb.ListGuildApplicationsResponse, error) {
	return s.logic.ListGuildApplications(ctx, req)
}

func (s *GuildServer) ReviewGuildApplication(ctx context.Context, req *pb.ReviewGuildApplicationRequest) (*pb.ReviewGuildApplicationResponse, error) {
	return s.logic.ReviewGuildApplication(ctx, req)
}

// ── 帮会经济(B5:捐献 / 升级 / 商店)──────────────────────────────

func (s *GuildServer) GetGuildDonateOptions(ctx context.Context, req *pb.GetGuildDonateOptionsRequest) (*pb.GetGuildDonateOptionsResponse, error) {
	return s.logic.GetGuildDonateOptions(ctx, req)
}

func (s *GuildServer) DonateToGuild(ctx context.Context, req *pb.DonateToGuildRequest) (*pb.DonateToGuildResponse, error) {
	return s.logic.DonateToGuild(ctx, req)
}

func (s *GuildServer) UpgradeGuild(ctx context.Context, req *pb.UpgradeGuildRequest) (*pb.UpgradeGuildResponse, error) {
	return s.logic.UpgradeGuild(ctx, req)
}

func (s *GuildServer) GetGuildShop(ctx context.Context, req *pb.GetGuildShopRequest) (*pb.GetGuildShopResponse, error) {
	return s.logic.GetGuildShop(ctx, req)
}

func (s *GuildServer) BuyGuildShopGoods(ctx context.Context, req *pb.BuyGuildShopGoodsRequest) (*pb.BuyGuildShopGoodsResponse, error) {
	return s.logic.BuyGuildShopGoods(ctx, req)
}

// NotifyGuildChanged 只是推送的 message id 占位(下行走 Kafka gate PushToPlayerEvent,
// 与 match 的 NotifyChallenge* 同形)。客户端调用在 session 拦截器就被 PermissionDenied
// 挡下,内部调用也无意义,所以这里恒返回空,不进 logic。
func (s *GuildServer) NotifyGuildChanged(context.Context, *pb.GuildChangedS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}

// ── Ranking ───────────────────────────────────────────────────

func (s *GuildServer) UpdateGuildScore(ctx context.Context, req *pb.UpdateGuildScoreRequest) (*pb.UpdateGuildScoreResponse, error) {
	return s.logic.UpdateGuildScore(ctx, req)
}

func (s *GuildServer) GetGuildRank(ctx context.Context, req *pb.GetGuildRankRequest) (*pb.GetGuildRankResponse, error) {
	return s.logic.GetGuildRank(ctx, req)
}

func (s *GuildServer) GetGuildRankByGuild(ctx context.Context, req *pb.GetGuildRankByGuildRequest) (*pb.GetGuildRankByGuildResponse, error) {
	return s.logic.GetGuildRankByGuild(ctx, req)
}
