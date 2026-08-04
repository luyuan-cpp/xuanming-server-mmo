package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"guild/internal/constants"
	"guild/internal/data"
	base "proto/common/base"
	pb "proto/guild"
	"shared/snowflake"
)

type GuildLogic struct {
	repo           *data.GuildRepo
	snowflake      *snowflake.Node
	onlineResolver *OnlineStatusResolver
}

func NewGuildLogic(repo *data.GuildRepo, sf *snowflake.Node, onlineResolver *OnlineStatusResolver) *GuildLogic {
	return &GuildLogic{repo: repo, snowflake: sf, onlineResolver: onlineResolver}
}

func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

func (l *GuildLogic) CreateGuild(ctx context.Context, req *pb.CreateGuildRequest) (*pb.CreateGuildResponse, error) {
	// Check if player already in a guild
	existingGuildID, err := l.repo.GetPlayerGuildID(ctx, req.PlayerId)
	if err != nil {
		return nil, fmt.Errorf("check existing guild: %w", err)
	}
	if existingGuildID > 0 {
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	}

	now := time.Now().UnixMilli()
	// 失租后发号器被 fence,这里整体失败 —— 绝不能吞掉错误再用 0 或自造 id 建帮,
	// 那会与接管了同一 worker id 的进程发出逐位相同的 guild_id。
	guildID, err := l.snowflake.Generate()
	if err != nil {
		logx.Errorf("CreateGuild: snowflake refused to mint (player=%d): %v", req.PlayerId, err)
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "id generator unavailable")}, nil
	}

	guild := &data.GuildData{
		GuildID:      guildID,
		Name:         req.Name,
		LeaderID:     req.PlayerId,
		Level:        constants.DefaultInitLevel,
		CreateTimeMs: now,
		MaxMembers:   constants.DefaultMaxMembers,
		ZoneID:       req.ZoneId,
		Members: []data.MemberData{
			{
				PlayerID:     req.PlayerId,
				Role:         constants.RoleLeader,
				JoinTimeMs:   now,
				LastActiveMs: now,
			},
		},
	}

	// 公会行 + 会长成员行在同一事务里落库;失败必须报给玩家,不能吞掉 ——
	// 旧实现吞掉成员写入失败仍返回成功,产出「玩家以为建会成功、权威库却查无此人」
	// 的半态,而且这个无人属于的公会永远没人有权解散。
	if err := l.repo.CreateGuild(ctx, guild); errors.Is(err, data.ErrPlayerAlreadyInGuild) {
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	} else if err != nil {
		return nil, fmt.Errorf("create guild: %w", err)
	}
	// 新公会加入排行榜(初始分数 0)。失败可容忍:score 权威在 MySQL,
	// ZSET 缺口由启动时 RebuildRanks 或下次推分自愈。
	if err := l.repo.UpdateGuildScore(ctx, guildID, req.ZoneId, 0); err != nil {
		logx.Errorf("init guild rank score: %v", err)
	}

	return &pb.CreateGuildResponse{Guild: l.toProtoGuild(ctx, guild)}, nil
}

func (l *GuildLogic) GetGuild(ctx context.Context, req *pb.GetGuildRequest) (*pb.GetGuildResponse, error) {
	guild, err := l.repo.GetGuild(ctx, req.GuildId)
	if err != nil {
		return nil, err
	}
	if guild == nil {
		return &pb.GetGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	}
	return &pb.GetGuildResponse{Guild: l.toProtoGuild(ctx, guild)}, nil
}

func (l *GuildLogic) GetPlayerGuild(ctx context.Context, req *pb.GetPlayerGuildRequest) (*pb.GetPlayerGuildResponse, error) {
	guildID, err := l.repo.GetPlayerGuildID(ctx, req.PlayerId)
	if err != nil {
		return nil, err
	}
	if guildID == 0 {
		return &pb.GetPlayerGuildResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not in any guild")}, nil
	}

	guild, err := l.repo.GetGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if guild == nil {
		// 解散与读取并发或历史悬空 membership：只失效映射并重读 MySQL，
		// 不把 nil 传给转换层，也不无条件删除任何权威成员行。
		refreshedGuildID, err := l.repo.RefreshPlayerGuildID(ctx, req.PlayerId)
		if err != nil {
			return nil, err
		}
		if refreshedGuildID == 0 {
			return &pb.GetPlayerGuildResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not in any guild")}, nil
		}
		guild, err = l.repo.GetGuild(ctx, refreshedGuildID)
		if err != nil {
			return nil, err
		}
		if guild == nil {
			return &pb.GetPlayerGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
		}
	}
	return &pb.GetPlayerGuildResponse{Guild: l.toProtoGuild(ctx, guild)}, nil
}

func (l *GuildLogic) JoinGuild(ctx context.Context, req *pb.JoinGuildRequest) (*pb.JoinGuildResponse, error) {
	existingGuildID, err := l.repo.GetPlayerGuildID(ctx, req.PlayerId)
	if err != nil {
		return nil, err
	}
	if existingGuildID > 0 {
		return &pb.JoinGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	}

	// 存在性、满员判定、成员插入都在 AddMember 的事务里按权威行数完成。
	// 这里不预读缓存判满:两个并发入会都会读到同一份旧成员列表并双双通过,
	// 事务内 FOR UPDATE + COUNT 才是唯一可靠的判定点。
	switch err := l.repo.AddMember(ctx, req.GuildId, req.PlayerId, constants.RoleMember); {
	case errors.Is(err, data.ErrGuildGone):
		return &pb.JoinGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	case errors.Is(err, data.ErrGuildFull):
		return &pb.JoinGuildResponse{ErrorMessage: tipErr(constants.ErrGuildFull, "guild is full")}, nil
	case errors.Is(err, data.ErrPlayerAlreadyInGuild):
		return &pb.JoinGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	case err != nil:
		return nil, err
	}
	return &pb.JoinGuildResponse{}, nil
}

func (l *GuildLogic) LeaveGuild(ctx context.Context, req *pb.LeaveGuildRequest) (*pb.LeaveGuildResponse, error) {
	guildID, err := l.repo.GetPlayerGuildID(ctx, req.PlayerId)
	if err != nil {
		return nil, err
	}
	if guildID == 0 {
		return &pb.LeaveGuildResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not in any guild")}, nil
	}

	guild, err := l.repo.GetGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if guild == nil {
		// 这里只失效旧 Redis 映射并从 MySQL 重读，绝不能无 guild_id 条件删除
		// membership：玩家可能已加入新公会，只是缓存仍指向已解散的旧公会。
		refreshedGuildID, err := l.repo.RefreshPlayerGuildID(ctx, req.PlayerId)
		if err != nil {
			return nil, fmt.Errorf("refresh dangling guild mapping: %w", err)
		}
		if refreshedGuildID == 0 {
			return &pb.LeaveGuildResponse{}, nil
		}
		logx.Errorf("player %d guild mapping changed from stale %d to authoritative %d; retrying is required",
			req.PlayerId, guildID, refreshedGuildID)
		return &pb.LeaveGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "guild membership changed, retry")}, nil
	}
	if guild.LeaderID == req.PlayerId {
		return &pb.LeaveGuildResponse{ErrorMessage: tipErr(constants.ErrLeaderCantLeave, "leader cannot leave, disband instead")}, nil
	}

	// 只删成员行 + 失效缓存;失败必须报错,不能吞掉后仍返回成功。
	if err := l.repo.RemoveMember(ctx, guildID, req.PlayerId); err != nil {
		return nil, err
	}
	return &pb.LeaveGuildResponse{}, nil
}

func (l *GuildLogic) DisbandGuild(ctx context.Context, req *pb.DisbandGuildRequest) (*pb.DisbandGuildResponse, error) {
	guildID, err := l.repo.GetPlayerGuildID(ctx, req.PlayerId)
	if err != nil {
		return nil, err
	}
	if guildID == 0 {
		return &pb.DisbandGuildResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not in any guild")}, nil
	}

	guild, err := l.repo.GetGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if guild == nil || guild.LeaderID != req.PlayerId {
		return &pb.DisbandGuildResponse{ErrorMessage: tipErr(constants.ErrNotLeader, "not guild leader")}, nil
	}

	// DeleteGuild 从 MySQL 权威表读取该 guild 的实际成员，并在同一事务内按
	// guild_id 删除；不会依据可能陈旧的缓存 Members 去误删已转入别会的玩家。
	if err := l.repo.DeleteGuild(ctx, guildID); err != nil {
		return nil, err
	}
	// 从排行榜移除
	if err := l.repo.RemoveGuildFromRank(ctx, guildID, guild.ZoneID); err != nil {
		logx.Errorf("remove guild %d from rank: %v", guildID, err)
	}
	return &pb.DisbandGuildResponse{}, nil
}

func (l *GuildLogic) SetAnnouncement(ctx context.Context, req *pb.SetAnnouncementRequest) (*pb.SetAnnouncementResponse, error) {
	// 权限必须在 MySQL 更新事务内按权威 membership.role 复核；Redis GuildData
	// 可能仍缓存着操作者降权/退会前的 officer 身份，只能做展示，不能授权。
	if err := l.repo.UpdateAnnouncementAuthorized(ctx, req.GuildId, req.PlayerId, req.Announcement); errors.Is(err, data.ErrGuildGone) {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	} else if errors.Is(err, data.ErrAnnouncementForbidden) {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrNoPermission, "no permission")}, nil
	} else if err != nil {
		return nil, err
	}
	return &pb.SetAnnouncementResponse{}, nil
}

// ── 排行榜 ─────────────────────────────────────────────────────

func (l *GuildLogic) UpdateGuildScore(ctx context.Context, req *pb.UpdateGuildScoreRequest) (*pb.UpdateGuildScoreResponse, error) {
	guild, err := l.repo.GetGuild(ctx, req.GuildId)
	if err != nil {
		return nil, err
	}
	if guild == nil {
		return &pb.UpdateGuildScoreResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	}
	// 分区榜恒用公会自己的 zone_id,**不接受请求方覆盖**:
	// DisbandGuild 清榜时只按 guild.ZoneID 调 RemoveGuildFromRank,若这里允许
	// req.ZoneId 把分数写进别的 zone 的 ZSET,解散后那个条目将永久残留
	// (榜单上出现查不到名字的幽灵公会)。
	if req.ZoneId > 0 && req.ZoneId != guild.ZoneID {
		logx.Errorf("UpdateGuildScore: requested zone %d ignored, guild %d belongs to zone %d",
			req.ZoneId, req.GuildId, guild.ZoneID)
	}
	if err := l.repo.UpdateGuildScore(ctx, req.GuildId, guild.ZoneID, req.Score); errors.Is(err, data.ErrGuildGone) {
		return &pb.UpdateGuildScoreResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	} else if err != nil {
		return nil, fmt.Errorf("update guild score: %w", err)
	}
	return &pb.UpdateGuildScoreResponse{}, nil
}

func (l *GuildLogic) GetGuildRank(ctx context.Context, req *pb.GetGuildRankRequest) (*pb.GetGuildRankResponse, error) {
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = 20
	}
	page := req.Page
	if page == 0 {
		page = 1
	}

	entries, total, err := l.repo.GetGuildRankPage(ctx, req.ZoneId, page, pageSize)
	if err != nil {
		return nil, fmt.Errorf("get guild rank page: %w", err)
	}

	pbEntries, err := l.enrichRankEntries(ctx, entries)
	if err != nil {
		return nil, err
	}

	return &pb.GetGuildRankResponse{
		Entries:    pbEntries,
		TotalCount: total,
		Page:       page,
		PageSize:   pageSize,
	}, nil
}

func (l *GuildLogic) GetGuildRankByGuild(ctx context.Context, req *pb.GetGuildRankByGuildRequest) (*pb.GetGuildRankByGuildResponse, error) {
	entry, err := l.repo.GetGuildRank(ctx, req.GuildId, req.ZoneId)
	if err != nil {
		return nil, err
	}
	if entry.Rank == 0 {
		return &pb.GetGuildRankByGuildResponse{ErrorMessage: tipErr(constants.ErrNotRanked, "guild not ranked")}, nil
	}

	guild, err := l.repo.GetGuild(ctx, entry.GuildID)
	if err != nil {
		return nil, err
	}

	pbEntry := &pb.GuildRankEntry{
		GuildId: entry.GuildID,
		Score:   entry.Score,
		Rank:    entry.Rank,
	}
	if guild != nil {
		pbEntry.Name = guild.Name
		pbEntry.LeaderId = guild.LeaderID
		pbEntry.Level = guild.Level
		pbEntry.MemberCount = uint32(len(guild.Members))
	}

	return &pb.GetGuildRankByGuildResponse{Entry: pbEntry}, nil
}

// enrichRankEntries fills in guild name/level/member_count from cache for a page of rank entries.
func (l *GuildLogic) enrichRankEntries(ctx context.Context, entries []data.RankEntry) ([]*pb.GuildRankEntry, error) {
	result := make([]*pb.GuildRankEntry, 0, len(entries))
	for _, e := range entries {
		pbEntry := &pb.GuildRankEntry{
			GuildId: e.GuildID,
			Score:   e.Score,
			Rank:    e.Rank,
		}
		guild, err := l.repo.GetGuild(ctx, e.GuildID)
		if err != nil {
			logx.Errorf("enrich rank entry guild %d: %v", e.GuildID, err)
		} else if guild != nil {
			pbEntry.Name = guild.Name
			pbEntry.LeaderId = guild.LeaderID
			pbEntry.Level = guild.Level
			pbEntry.MemberCount = uint32(len(guild.Members))
		}
		result = append(result, pbEntry)
	}
	return result, nil
}

// ── Proto conversion ───────────────────────────────────────────

func (l *GuildLogic) toProtoGuild(ctx context.Context, g *data.GuildData) *pb.GuildInfo {
	if g == nil {
		return nil
	}
	memberIDs := make([]uint64, 0, len(g.Members))
	for _, m := range g.Members {
		memberIDs = append(memberIDs, m.PlayerID)
	}
	onlineMap := l.onlineResolver.BatchResolve(ctx, memberIDs)

	info := &pb.GuildInfo{
		GuildId:      g.GuildID,
		Name:         g.Name,
		LeaderId:     g.LeaderID,
		Level:        g.Level,
		Announcement: g.Announcement,
		CreateTimeMs: g.CreateTimeMs,
		MaxMembers:   g.MaxMembers,
		ZoneId:       g.ZoneID,
	}
	for _, m := range g.Members {
		info.Members = append(info.Members, &pb.GuildMember{
			PlayerId:     m.PlayerID,
			Role:         m.Role,
			JoinTimeMs:   m.JoinTimeMs,
			LastActiveMs: m.LastActiveMs,
			Contribution: m.Contribution,
			Online:       onlineMap[m.PlayerID],
		})
	}
	return info
}
