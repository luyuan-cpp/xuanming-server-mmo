package server

import (
	"context"
	"time"

	"data_service/internal/store"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/playername"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RenameRole 处理 scene 发起的角色改名(docs/design/rename-and-content-safety.md §4)。
// 只认 scene 的调用证明(与入场登记共用 RoleAdmissionAuth);名字规范化与字数/字符集校验在这里按调用方带来的规则执行,
// 不合规时仍以 Invalid=true 进入目录事务以便为这个 seq 写下已拒绝记录,挡住同一 seq 的迟到重放。
func (s *DataServiceServer) RenameRole(ctx context.Context, in *dspb.RenameRoleRequest) (*dspb.RenameRoleResponse, error) {
	if s.svcCtx.RoleRenames == nil || s.svcCtx.RoleAdmissionAuth == nil {
		return nil, status.Error(codes.Unavailable, "role rename service is unavailable")
	}
	caller, err := s.svcCtx.RoleAdmissionAuth.Verify(ctx, accountdirectory.MethodPrefix+"RenameRole", in)
	if err != nil || caller != "scene" {
		return nil, status.Error(codes.PermissionDenied, "invalid caller proof for role rename")
	}
	if in.GetPlayerId() == 0 || in.GetSeq() == 0 {
		return nil, status.Error(codes.InvalidArgument, "player_id and seq must be non-zero")
	}
	rules := playername.StructuralRules
	if in.GetMinChars() != 0 || in.GetMaxChars() != 0 {
		rules = playername.Rules{
			MinRunes: int(in.GetMinChars()),
			MaxRunes: int(in.GetMaxChars()),
		}
		if err := rules.Validate(); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid name length rules: %v", err)
		}
	}
	display, norm, verdict := playername.Normalize(in.GetName(), rules)
	req := store.RenameRequest{
		Player:   in.GetPlayerId(),
		Seq:      in.GetSeq(),
		Cooldown: time.Duration(in.GetCooldownSeconds()) * time.Second,
		Hold:     time.Duration(in.GetHoldSeconds()) * time.Second,
	}
	if verdict == playername.VerdictOK {
		req.Display = display
		req.Norm = norm
	} else {
		req.Invalid = true
	}
	out, err := s.svcCtx.RoleRenames.RenameRole(ctx, req)
	if err != nil {
		return nil, directoryError(err)
	}
	return &dspb.RenameRoleResponse{
		Result:        out.Result,
		CurrentSeq:    out.CurrentSeq,
		Name:          out.Name,
		RenamedMs:     out.RenamedMs,
		NextAllowedMs: out.NextAllowedMs,
	}, nil
}
