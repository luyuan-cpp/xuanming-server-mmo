package logic

import (
	"context"
	"errors"
	"time"

	"guild/internal/constants"
	"guild/internal/data"
	cgpb "proto/content_guard"
	pb "proto/guild"
	scenepb "proto/scene"
	"shared/generated/table"
)

const (
	maxGuildRenameDurationSeconds uint32 = 365 * 86400
	maxGuildRenameCostFunds       uint64 = 1_000_000_000_000
)

// guildRenameRuleParams 从 GuildRule 表第 1 行读取改名配置(启动期 validateGuildTables 已校验上下界)。
func guildRenameRuleParams() (cooldown, hold time.Duration, costFunds uint64) {
	rule, _ := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	if rule == nil {
		return 7 * 24 * time.Hour, 7 * 24 * time.Hour, 1000
	}
	return time.Duration(rule.GetRenameCooldownSeconds()) * time.Second,
		time.Duration(rule.GetNameHoldSeconds()) * time.Second,
		rule.GetRenameCostFunds()
}

// RenameGuild 帮会改名(docs/design/rename-and-content-safety.md §5)。
// 仅帮主可操作;送内容安全服务校验(TEXT_SCENE_GUILD_NAME)后在单个本地事务内改名、扣帮会资金并保护旧名。
func (l *GuildLogic) RenameGuild(ctx context.Context, req *pb.RenameGuildRequest) (*pb.RenameGuildResponse, error) {
	who := callerOf(ctx, 0)
	if err := requireClientSession(who); err != nil {
		return nil, err
	}
	if req.GuildId == 0 {
		return &pb.RenameGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	}
	display, ok := normalizeGuildName(req.Name)
	if !ok {
		return &pb.RenameGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNameInvalid, "invalid guild name")}, nil
	}
	if _, tip, err := l.clientZone(ctx, who.playerID); err != nil {
		return nil, err
	} else if tip != nil {
		return &pb.RenameGuildResponse{ErrorMessage: tip}, nil
	}

	// 内容安全送检在事务之外(不在持锁事务内调 RPC)。
	if tip := l.checkText(ctx, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, who.playerID, display, constants.ErrGuildNameSensitive); tip != nil {
		return &pb.RenameGuildResponse{ErrorMessage: tip}, nil
	}

	cooldown, hold, costFunds := guildRenameRuleParams()
	snapshot, nextAllowedMs, err := l.repo.RenameGuild(
		ctx, req.GuildId, who.playerID, display, cooldown, hold, costFunds, nowMs(),
	)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrGuildNameTaken):
		return &pb.RenameGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNameTaken, "guild name taken")}, nil
	case errors.Is(err, data.ErrGuildRenameCooldown):
		return &pb.RenameGuildResponse{
			ErrorMessage:   tipErr(constants.ErrRenameCooldown, "guild rename cooldown"),
			NextRenameTime: nextAllowedMs / 1000,
		}, nil
	case errors.Is(err, data.ErrGuildRenameSameName):
		return &pb.RenameGuildResponse{ErrorMessage: tipErr(constants.ErrRenameSameName, "guild rename same name")}, nil
	case errors.Is(err, data.ErrFundsInsufficient):
		return &pb.RenameGuildResponse{ErrorMessage: tipErr(constants.ErrFundsInsufficient, "guild funds insufficient")}, nil
	case errors.Is(err, data.ErrRankTooLow):
		return &pb.RenameGuildResponse{ErrorMessage: tipErr(constants.ErrNotLeader, "only leader can rename guild")}, nil
	default:
		if tip, mappedErr := l.mapWriteErr(ctx, who.playerID, req.GuildId, req.GuildId, err); mappedErr != nil || tip != nil {
			return &pb.RenameGuildResponse{ErrorMessage: tip}, mappedErr
		}
	}

	l.notify(ctx, pb.GuildChangeKind_GUILD_CHANGE_KIND_RENAMED, req.GuildId, who.playerID, 0,
		membersExcept(snapshot, who.playerID))
	l.bumpSync(membersPlus(snapshot), scenepb.SyncDomain_SYNC_DOMAIN_GUILD)
	return &pb.RenameGuildResponse{
		Guild:          l.guildInfoFor(ctx, snapshot, who.playerID),
		NextRenameTime: nextAllowedMs / 1000,
	}, nil
}
