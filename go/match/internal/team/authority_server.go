package team

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"match/internal/authority"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	teampb "proto/team"
	"shared/accountdirectory"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const teamMembershipMethod = "/teampb.TeamAuthority/ReadMembership"

type AuthorityServer struct {
	teampb.UnimplementedTeamAuthorityServer
	svc      *svc.ServiceContext
	verifier *serviceauth.Verifier
	secret   string
}

func NewAuthorityServer(s *svc.ServiceContext) (*AuthorityServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Authority.CheckTeamRouteContract(ctx); err != nil {
		return nil, errors.New("team: SQL路由契约缺失或不可读")
	}
	secret := strings.TrimSpace(os.Getenv(accountdirectory.SceneSecretEnv))
	verifier, err := serviceauth.New(map[string]string{"scene": secret})
	if err != nil {
		return nil, errors.New("team: 场景SQL队伍读取签名密钥缺失或不足32字节")
	}
	return &AuthorityServer{svc: s, verifier: verifier, secret: secret}, nil
}

func (s *AuthorityServer) ReadMembership(ctx context.Context, request *teampb.ReadTeamMembershipRequest) (*teampb.ReadTeamMembershipResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("x-session-detail-bin")) != 0 {
		return nil, status.Error(codes.PermissionDenied, "仅允许场景内部调用")
	}
	if _, ok := ctxkeys.GetSessionDetails(ctx); ok {
		return nil, status.Error(codes.PermissionDenied, "仅允许场景内部调用")
	}
	caller, err := s.verifier.Verify(ctx, teamMembershipMethod, request)
	if err != nil || caller != "scene" {
		return nil, status.Error(codes.PermissionDenied, "场景调用证明无效")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	member, record, err := s.svc.Authority.ReadTeamAdmission(ctx, request.GetAdmission())
	if err != nil {
		if errors.Is(err, authority.ErrOriginalPlayer) {
			return nil, status.Error(codes.FailedPrecondition, "原入场世代已失效")
		}
		return nil, status.Error(codes.Unavailable, "SQL队伍原件读取失败")
	}
	digest, err := serviceauth.Digest(request)
	if err != nil {
		return nil, status.Error(codes.Internal, "请求摘要失败")
	}
	response := &teampb.ReadTeamMembershipResponse{TeamId: member.TeamId, MembershipEpoch: member.Epoch, RequestDigest: digest}
	if record != nil {
		data := &teampb.TeamAuthorityData{}
		if proto.Unmarshal(record.Payload, data) != nil {
			return nil, status.Error(codes.DataLoss, "SQL队伍原件损坏")
		}
		response.Version, response.Record = record.Version, data.Record
	}
	if err = serviceauth.Sign(teamMembershipMethod+"/reply", response, "team", s.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, status.Error(codes.Internal, "响应签名失败")
	}
	return response, nil
}
