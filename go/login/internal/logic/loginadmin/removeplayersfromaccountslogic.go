package loginadminlogic

import (
	"context"
	"errors"

	"login/internal/logic/admin"
	"login/internal/svc"
	"proto/login"
	"shared/accountdirectory"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type RemovePlayersFromAccountsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRemovePlayersFromAccountsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemovePlayersFromAccountsLogic {
	return &RemovePlayersFromAccountsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RemovePlayersFromAccounts 摘孤儿角色,判定和结局语义见 admin.RemovePlayersFromAccounts。鉴权在拦截器(x-admin-token)。
func (l *RemovePlayersFromAccountsLogic) RemovePlayersFromAccounts(in *login.RemovePlayersFromAccountsRequest) (*login.RemovePlayersFromAccountsResponse, error) {
	out, err := admin.RemovePlayersFromAccounts(l.ctx, l.svcCtx, &admin.RemovePlayersFromAccountsReq{
		PlayerIDs: in.GetPlayerIds(),
		Operator:  in.GetOperator(),
		Reason:    in.GetReason(),
		DryRun:    in.GetDryRun(),
		PeerAddr:  peerAddrOf(l.ctx),
	})
	switch {
	case errors.Is(err, accountdirectory.ErrInvalid):
		return nil, status.Error(codes.InvalidArgument, "player_ids must be at most 100 entries; operator and reason are required")
	case errors.Is(err, accountdirectory.ErrUnavailable):
		return nil, status.Error(codes.Unavailable, "account directory is not ready")
	case err != nil:
		return nil, status.Error(codes.Internal, "remove players failed")
	}
	resp := &login.RemovePlayersFromAccountsResponse{
		RemovedCount:   out.RemovedCount,
		NotFoundCount:  out.NotFoundCount,
		FailedCount:    out.FailedCount,
		NotOrphanCount: out.NotOrphanCount,
		Results:        make([]*login.RemovePlayerResult, 0, len(out.Results)),
	}
	for _, r := range out.Results {
		resp.Results = append(resp.Results, &login.RemovePlayerResult{PlayerId: r.PlayerID, Outcome: r.Outcome})
	}
	return resp, nil
}

func peerAddrOf(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}
