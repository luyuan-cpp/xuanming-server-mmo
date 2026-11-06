package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	rankpb "proto/rank"
	"rank/internal/data"
	"shared/domainhost"
	table "shared/generated/pb/table"
	"shared/serviceauth"
)

type Server struct {
	rankpb.UnimplementedClientPlayerRankServer
	rankpb.UnimplementedRankAdminServer
	repo  *data.Repo
	auth  *serviceauth.Verifier
	names NameReader
}

type NameReader interface {
	BatchGetPlayerName(context.Context, *dspb.BatchGetPlayerNameRequest, ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error)
}

func New(repo *data.Repo, auth *serviceauth.Verifier, names NameReader) *Server {
	return &Server{repo: repo, auth: auth, names: names}
}
func (s *Server) enrich(ctx context.Context, entries []*rankpb.RankEntry, self *rankpb.RankEntry) error {
	if s.names == nil {
		return errors.New("排行榜权威名称服务未装配")
	}
	seen := map[uint64]bool{}
	request := &dspb.BatchGetPlayerNameRequest{}
	all := append(append([]*rankpb.RankEntry(nil), entries...), self)
	for _, entry := range all {
		if entry != nil && !seen[entry.EntityId] {
			seen[entry.EntityId] = true
			request.PlayerIds = append(request.PlayerIds, entry.EntityId)
		}
	}
	if len(request.PlayerIds) == 0 {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	names, err := s.names.BatchGetPlayerName(bounded, request)
	if err != nil {
		return err
	}
	for _, entry := range all {
		if entry != nil {
			entry.Name = names.GetNames()[entry.EntityId]
		}
	}
	return nil
}
func rejection(err error) *base.TipInfoMessage {
	if err == nil {
		return nil
	}
	code := uint32(table.CommonError_kServiceUnavailable)
	switch {
	case errors.Is(err, data.ErrInvalid):
		code = uint32(table.CommonError_kInvalidParameter)
	case errors.Is(err, data.ErrNotFound):
		code = uint32(table.RankError_kRankBoardNotFound)
	case errors.Is(err, data.ErrClosed):
		code = uint32(table.RankError_kRankGenerationClosed)
	case errors.Is(err, data.ErrConflict):
		code = uint32(table.RankError_kRankRequestConflict)
	case errors.Is(err, data.ErrCursor):
		code = uint32(table.RankError_kRankSettlementPending)
	case errors.Is(err, data.ErrDenied):
		code = uint32(table.CommonError_kInvalidParameter)
	}
	return &base.TipInfoMessage{Id: code}
}
func (s *Server) caller(ctx context.Context, method string, request proto.Message) (string, error) {
	caller, err := s.auth.Verify(ctx, method, request)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "内部调用无权访问")
	}
	return caller, nil
}
func (s *Server) GetRankPage(ctx context.Context, in *rankpb.GetRankPageRequest) (*rankpb.GetRankPageResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	// 新的通用榜公开读面只开放全服榜；其它scope不能靠客户端伪造scope_id越权。
	if in.GetBoard().GetScope() != rankpb.RankScope_RANK_SCOPE_GLOBAL {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(data.ErrDenied)}, nil
	}
	result, err := s.repo.Page(ctx, player, in)
	if err != nil {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(err)}, nil
	}
	if err = s.enrich(ctx, result.Entries, result.Self); err != nil {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) GetRankAround(ctx context.Context, in *rankpb.GetRankAroundRequest) (*rankpb.GetRankAroundResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok || in.GetBoard().GetScope() != rankpb.RankScope_RANK_SCOPE_GLOBAL {
		return &rankpb.GetRankAroundResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	result, err := s.repo.Around(ctx, player, in)
	if err != nil {
		return &rankpb.GetRankAroundResponse{ErrorMessage: rejection(err)}, nil
	}
	if err = s.enrich(ctx, result.Entries, result.Self); err != nil {
		return &rankpb.GetRankAroundResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) OpenRankGeneration(ctx context.Context, in *rankpb.OpenRankGenerationRequest) (*rankpb.OpenRankGenerationResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_OpenRankGeneration_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Open(ctx, caller, in)
	if err != nil {
		return &rankpb.OpenRankGenerationResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) SubmitRankScores(ctx context.Context, in *rankpb.SubmitRankScoresRequest) (*rankpb.SubmitRankScoresResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_SubmitRankScores_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Submit(ctx, caller, in)
	if err != nil {
		return &rankpb.SubmitRankScoresResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) FreezeRankGeneration(ctx context.Context, in *rankpb.FreezeRankGenerationRequest) (*rankpb.FreezeRankGenerationResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_FreezeRankGeneration_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Freeze(ctx, caller, in)
	if err != nil {
		return &rankpb.FreezeRankGenerationResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) RemoveRankEntry(ctx context.Context, in *rankpb.RemoveRankEntryRequest) (*rankpb.RankAdminResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_RemoveRankEntry_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	return &rankpb.RankAdminResponse{ErrorMessage: rejection(s.repo.Remove(ctx, caller, in, false))}, nil
}
func (s *Server) DeleteRankGeneration(ctx context.Context, in *rankpb.RemoveRankEntryRequest) (*rankpb.RankAdminResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_DeleteRankGeneration_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	return &rankpb.RankAdminResponse{ErrorMessage: rejection(s.repo.Remove(ctx, caller, in, true))}, nil
}
