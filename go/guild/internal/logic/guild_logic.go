package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	"guild/internal/data"
	base "proto/common/base"
	pb "proto/guild"
)

// IDMinter 是 guild_id 的发号入口(*shared/idsegment.Minter 满足;单测用假实现)。
// 号段优先、是否回退 snowflake 的策略在 svc.NewGuildIDMinter 里定,logic 层只管
// 「拿不到号就整体失败」。
type IDMinter interface {
	Mint(ctx context.Context) (uint64, error)
}

type GuildLogic struct {
	repo           *data.GuildRepo
	ids            IDMinter
	onlineResolver *OnlineStatusResolver
	// mergeFence 为 nil = 没配 MergeMarkerRedis = 闸门不生效(见 merge_fence.go)。
	// 用接口而不是 *redis.Client:单测要能注入"永远封锁"和"永远报错"两种替身。
	mergeFence MergeFence
}

// NewGuildLogic。mergeFence 可为 nil:合服闸门是可选加固,没配就跳过检查
// (guild.go 启动时会打一条 Info 说明它没生效)。
//
// 注意 nil 接口的坑:传 (*RedisMergeFence)(nil) 进来**不等于** nil 接口,
// 所以 NewRedisMergeFence 在客户端为空时返回 nil 指针后,guild.go 必须显式判空
// 再决定传不传 —— 见那边的接线。
func NewGuildLogic(repo *data.GuildRepo, ids IDMinter, onlineResolver *OnlineStatusResolver, mergeFence MergeFence) *GuildLogic {
	return &GuildLogic{repo: repo, ids: ids, onlineResolver: onlineResolver, mergeFence: mergeFence}
}

func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

func (l *GuildLogic) CreateGuild(ctx context.Context, req *pb.CreateGuildRequest) (*pb.CreateGuildResponse, error) {
	// 合服闸门在**最前面**:目标 zone 正在合服时连号都不铸。
	// 放在铸号之后会白白烧掉一个 guild_id(号段只进不退),放在建库之后就已经晚了。
	if err := l.checkMergeFence(ctx, req.ZoneId); err != nil {
		return nil, err
	}

	// Check if player already in a guild
	existingGuildID, err := l.repo.GetPlayerGuildID(ctx, req.PlayerId)
	if err != nil {
		return nil, fmt.Errorf("check existing guild: %w", err)
	}
	if existingGuildID > 0 {
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	}

	now := time.Now().UnixMilli()
	guildID, tip := l.mintGuildID(ctx, req.PlayerId)
	if tip != nil {
		return &pb.CreateGuildResponse{ErrorMessage: tip}, nil
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

// ErrZoneMerging 表示目标 zone 正处在合服维护窗口,建帮被闸门拒绝。
// 用 errors.Is 可判;跨 gRPC 时是 codes.FailedPrecondition。
var ErrZoneMerging = errors.New("zone merge in progress")

// checkMergeFence 返回非 nil 表示本次建帮被合服闸门拒绝。
//
// 三条分支各有理由:
//   - fence 为 nil(没配 MergeMarkerRedis)→ 放行。闸门是可选加固,不能因为没配
//     就把建帮功能整体关掉;guild.go 启动时已经打过"闸门未生效"的 Info。
//   - 查询报错 → **拒绝**(fail closed)。查不到闸门状态不等于没有闸门,
//     而合服窗口里建出来的公会会永远留在一个已下线的 zone —— 那是数据事故,
//     拒绝只是一次可重试的失败。
//   - 键存在 → 拒绝。
//
// 为什么返回 error 而不是 TipInfoMessage:见 constants 包末尾那段 —— tip 码只能
// 由配表发号,本次没有新码可用,借别的段会撞上 TestNoHandWrittenTipCodes 的护栏。
// FailedPrecondition 至少让调用方能把"暂时不能建"与"服务挂了"分开。
func (l *GuildLogic) checkMergeFence(ctx context.Context, zoneID uint32) error {
	if l.mergeFence == nil || zoneID == 0 {
		return nil
	}
	merging, err := l.mergeFence.MergeInProgress(ctx, zoneID)
	if err != nil {
		logx.Errorf("CreateGuild: merge fence unreadable for zone %d, refusing (fail closed): %v", zoneID, err)
		return status.Errorf(codes.FailedPrecondition,
			"%v: zone %d merge fence unreadable, guild creation is paused", ErrZoneMerging, zoneID)
	}
	if merging {
		logx.Infof("CreateGuild: refused, zone %d is merging (%s present)", zoneID, MergeFenceKey(zoneID))
		return status.Errorf(codes.FailedPrecondition,
			"%v: zone %d is merging, guild creation is paused", ErrZoneMerging, zoneID)
	}
	return nil
}

// mintGuildID 是建帮的发号步骤。单独抽出来只为可测:CreateGuild 其余每一步都要 MySQL。
//
// 号段优先(设计稿 §6),失败时按 IdSegment.FallbackToSnowflake 决定回退还是整体失败 ——
// 绝不能吞掉错误再用 0 或自造 id 建帮:snowflake 那条路上,那会与接管了同一 worker id
// 的进程发出逐位相同的 guild_id。失败沿用既有的 ErrIDGenUnavailable(Tip 表里已标 fault,
// serverbase 会把它记成服务端故障),不另发新码。
func (l *GuildLogic) mintGuildID(ctx context.Context, playerID uint64) (uint64, *base.TipInfoMessage) {
	if l.ids == nil {
		// 接线错误(guild.go 一定会装上它),按不可用处理而不是 nil 解引用崩掉整个进程。
		logx.Errorf("CreateGuild: id minter not wired (player=%d)", playerID)
		return 0, tipErr(constants.ErrIDGenUnavailable, "id generator unavailable")
	}
	id, err := l.ids.Mint(ctx)
	if err != nil {
		logx.Errorf("CreateGuild: id generator refused to mint (player=%d): %v", playerID, err)
		return 0, tipErr(constants.ErrIDGenUnavailable, "id generator unavailable")
	}
	return id, nil
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
	// 它同时返回删除事务里 FOR UPDATE 读到的 zone_id —— 清榜只能用这一个值。
	authoritativeZone, err := l.repo.DeleteGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	// 从排行榜移除。**绝不用 guild.ZoneID**:那是 guild:v2:{id} 缓存里的值,TTL 30 分钟;
	// 合服刚把公会搬到目标 zone 时它还是源 zone,按它 ZREM 会去删一个空 ZSET,
	// 把条目永久留在目标 zone 的榜上(榜里出现一个查不到名字的幽灵公会)。
	if err := l.repo.RemoveGuildFromRank(ctx, guildID, authoritativeZone); err != nil {
		logx.Errorf("remove guild %d (zone %d) from rank: %v", guildID, authoritativeZone, err)
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
