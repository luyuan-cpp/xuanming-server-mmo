package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	"guild/internal/data"
	"guild/internal/session"
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
	// homeZones 为 nil = 没配 DataServiceRpc:内部调用照常,客户端来源的请求一律拒绝
	// (没有归属 zone 的真源就不能猜玩家属于哪个区)。
	homeZones HomeZoneLookup
}

// NewGuildLogic。mergeFence / homeZones 可为 nil,语义见字段注释。
//
// 注意 nil 接口的坑:传 (*RedisMergeFence)(nil) 进来**不等于** nil 接口,
// 所以 guild.go 必须显式判空再决定传不传 —— 见那边的接线。
func NewGuildLogic(repo *data.GuildRepo, ids IDMinter, onlineResolver *OnlineStatusResolver,
	mergeFence MergeFence, homeZones HomeZoneLookup) *GuildLogic {
	return &GuildLogic{repo: repo, ids: ids, onlineResolver: onlineResolver, mergeFence: mergeFence, homeZones: homeZones}
}

func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

// ── 调用方身份与 zone ──────────────────────────────────────────
//
// 客户端来源(带 gate 会话)的请求:身份取会话里的 player_id,zone 取 data_service 的
// 归属 zone,请求体里的 player_id / zone_id 一律不信。帮会按 zone 隔离 —— 只能在自己的区
// 建帮,只看得见、加得进自己区的帮,只看自己区的榜;别区的帮会对客户端表现为「不存在」。
// 帮名仍全局唯一(uk_guild,按 name_norm):合服时两区的帮会直接合并,不需要改名。
//
// 内部调用(无会话:GM、运维工具、其它服务)沿用请求体字段,行为与接入客户端之前一致。

type caller struct {
	playerID   uint64
	fromClient bool
}

func callerOf(ctx context.Context, bodyPlayerID uint64) caller {
	playerID, fromClient := session.ClientPlayerID(ctx)
	if !fromClient {
		return caller{playerID: bodyPlayerID}
	}
	if bodyPlayerID != 0 && bodyPlayerID != playerID {
		logx.Errorf("[guild] 请求体 player_id=%d 与会话身份 %d 不一致,以会话为准", bodyPlayerID, playerID)
	}
	return caller{playerID: playerID, fromClient: true}
}

// clientZone 返回客户端来源请求所属的 zone。tip 非 nil = 业务拒绝;err 非 nil = 故障。
func (l *GuildLogic) clientZone(ctx context.Context, playerID uint64) (uint32, *base.TipInfoMessage, error) {
	if l.homeZones == nil {
		logx.Errorf("[guild] home zone lookup not wired (DataServiceRpc missing), refusing client request of player %d", playerID)
		return 0, nil, status.Error(codes.Unavailable, "guild home zone lookup is not configured")
	}
	zoneID, err := l.homeZones.HomeZone(ctx, playerID)
	if err != nil {
		return 0, nil, err
	}
	if zoneID == 0 {
		logx.Infof("[guild] player %d has no home zone mapping, guild request refused "+
			"(run tools/merge_zone -backfill-home-zone for legacy players)", playerID)
		return 0, tipErr(constants.ErrHomeZoneUnknown, "home zone unknown"), nil
	}
	return zoneID, nil, nil
}

// visibleIn:zoneID 为 0 表示不按 zone 过滤(内部调用)。
func visibleIn(guild *data.GuildData, zoneID uint32) bool {
	return zoneID == 0 || guild.ZoneID == zoneID
}

// normalizeGuildName 去掉首尾空白后校验帮名。帮名进全局唯一索引并展示给所有人,
// 不收空名、超长名与控制字符。proto3 的 string 在反序列化时已保证是合法 UTF-8。
func normalizeGuildName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > constants.MaxGuildNameRunes {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	// name_norm 必须可生成且不超长,否则唯一键覆盖不到整个值
	// (docs/design/guild-phase2/01-storage.md §6.3)。失败沿用 kGuildNameInvalid,不新增 tip。
	if _, ok := data.GuildNameNorm(name); !ok {
		return "", false
	}
	return name, true
}

func (l *GuildLogic) CreateGuild(ctx context.Context, req *pb.CreateGuildRequest) (*pb.CreateGuildResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	name, ok := normalizeGuildName(req.Name)
	if !ok {
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNameInvalid, "invalid guild name")}, nil
	}
	zoneID := req.ZoneId
	if who.fromClient {
		homeZone, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.CreateGuildResponse{ErrorMessage: tip}, nil
		}
		zoneID = homeZone
	}

	// 合服闸门在铸号与任何写之前:目标 zone 正在合服时连号都不铸。
	// 放在铸号之后会白白烧掉一个 guild_id(号段只进不退),放在建库之后就已经晚了。
	if err := l.checkMergeFence(ctx, zoneID); err != nil {
		return nil, err
	}

	// Check if player already in a guild
	existingGuildID, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
	if err != nil {
		return nil, fmt.Errorf("check existing guild: %w", err)
	}
	if existingGuildID > 0 {
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	}

	now := uint64(time.Now().UnixMilli())
	guildID, tip := l.mintGuildID(ctx, who.playerID)
	if tip != nil {
		return &pb.CreateGuildResponse{ErrorMessage: tip}, nil
	}

	guild := &data.GuildData{
		GuildID:      guildID,
		Name:         name,
		LeaderID:     who.playerID,
		Level:        constants.DefaultInitLevel,
		CreateTimeMs: now,
		MaxMembers:   constants.DefaultMaxMembers,
		ZoneID:       zoneID,
		Members: []data.MemberData{
			{
				PlayerID:     who.playerID,
				Role:         constants.RoleLeader,
				JoinTimeMs:   now,
				LastActiveMs: now,
			},
		},
	}

	// 公会行 + 会长成员行在同一事务里落库;失败必须报给玩家,不能吞掉 ——
	// 旧实现吞掉成员写入失败仍返回成功,产出「玩家以为建会成功、权威库却查无此人」
	// 的半态,而且这个无人属于的公会永远没人有权解散。
	// 重名只能由唯一索引裁决(预查询挡不住并发建同名),代价是这次铸出的 guild_id 作废。
	switch err := l.repo.CreateGuild(ctx, guild); {
	case errors.Is(err, data.ErrPlayerAlreadyInGuild):
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	case errors.Is(err, data.ErrGuildNameTaken):
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNameTaken, "guild name taken")}, nil
	case err != nil:
		return nil, fmt.Errorf("create guild: %w", err)
	}
	// 新公会加入排行榜(初始分数 0)。失败可容忍:score 权威在 MySQL,
	// ZSET 缺口由启动时 RebuildRanks 或下次推分自愈。
	if err := l.repo.UpdateGuildScore(ctx, guildID, zoneID, 0); err != nil {
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
	visibleZone := uint32(0)
	if who := callerOf(ctx, 0); who.fromClient {
		zoneID, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.GetGuildResponse{ErrorMessage: tip}, nil
		}
		visibleZone = zoneID
	}

	guild, err := l.repo.GetGuild(ctx, req.GuildId)
	if err != nil {
		return nil, err
	}
	// 别区的帮会与不存在的帮会同一答复,不向客户端透露"它在别的区"。
	if guild == nil || !visibleIn(guild, visibleZone) {
		return &pb.GetGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	}
	return &pb.GetGuildResponse{Guild: l.toProtoGuild(ctx, guild)}, nil
}

// GetPlayerGuild 不按 zone 过滤:玩家自己的帮会永远看得见(合服会把帮会与归属映射一起搬走)。
func (l *GuildLogic) GetPlayerGuild(ctx context.Context, req *pb.GetPlayerGuildRequest) (*pb.GetPlayerGuildResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	guildID, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
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
		refreshedGuildID, err := l.repo.RefreshPlayerGuildID(ctx, who.playerID)
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
	who := callerOf(ctx, req.PlayerId)
	requiredZone := uint32(0)
	if who.fromClient {
		zoneID, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.JoinGuildResponse{ErrorMessage: tip}, nil
		}
		requiredZone = zoneID
	}

	existingGuildID, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
	if err != nil {
		return nil, err
	}
	if existingGuildID > 0 {
		return &pb.JoinGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	}

	// 存在性、zone 归属、满员判定、成员插入都在 AddMemberInZone 的事务里按权威行完成。
	// 这里不预读缓存判满 / 判 zone:两个并发入会都会读到同一份旧成员列表并双双通过,
	// 合服刚搬走的公会在缓存里也可能还是旧 zone;事务内 FOR UPDATE 才是唯一可靠的判定点。
	switch err := l.repo.AddMemberInZone(ctx, req.GuildId, who.playerID, constants.RoleMember, requiredZone); {
	case errors.Is(err, data.ErrGuildGone), errors.Is(err, data.ErrGuildZoneMismatch):
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
	who := callerOf(ctx, req.PlayerId)
	guildID, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
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
		refreshedGuildID, err := l.repo.RefreshPlayerGuildID(ctx, who.playerID)
		if err != nil {
			return nil, fmt.Errorf("refresh dangling guild mapping: %w", err)
		}
		if refreshedGuildID == 0 {
			return &pb.LeaveGuildResponse{}, nil
		}
		logx.Errorf("player %d guild mapping changed from stale %d to authoritative %d; retrying is required",
			who.playerID, guildID, refreshedGuildID)
		return &pb.LeaveGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "guild membership changed, retry")}, nil
	}
	if guild.LeaderID == who.playerID {
		return &pb.LeaveGuildResponse{ErrorMessage: tipErr(constants.ErrLeaderCantLeave, "leader cannot leave, disband instead")}, nil
	}

	// 只删成员行 + 失效缓存;失败必须报错,不能吞掉后仍返回成功。
	if err := l.repo.RemoveMember(ctx, guildID, who.playerID); err != nil {
		return nil, err
	}
	return &pb.LeaveGuildResponse{}, nil
}

func (l *GuildLogic) DisbandGuild(ctx context.Context, req *pb.DisbandGuildRequest) (*pb.DisbandGuildResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	guildID, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
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
	if guild == nil || guild.LeaderID != who.playerID {
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
	who := callerOf(ctx, req.PlayerId)
	if len(req.Announcement) > constants.MaxAnnouncementBytes {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrAnnouncementTooLong, "announcement too long")}, nil
	}
	// 权限必须在 MySQL 更新事务内按权威 membership.role 复核；Redis GuildData
	// 可能仍缓存着操作者降权/退会前的 officer 身份，只能做展示，不能授权。
	// 公会 id 来自请求体也无妨:事务要求操作者是该公会的 officer/leader,改不了别人的帮。
	if err := l.repo.UpdateAnnouncementAuthorized(ctx, req.GuildId, who.playerID, req.Announcement); errors.Is(err, data.ErrGuildGone) {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	} else if errors.Is(err, data.ErrAnnouncementForbidden) {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrNoPermission, "no permission")}, nil
	} else if err != nil {
		return nil, err
	}
	return &pb.SetAnnouncementResponse{}, nil
}

// ── 排行榜 ─────────────────────────────────────────────────────

// UpdateGuildScore 只对内部调用开放(session.ClientMethods 不含它)。
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

// GetGuildRank:客户端只看本区榜(zone_id 以归属 zone 覆盖,全服榜 zone_id=0 只对内部调用开放),
// 单页条数夹到 MaxRankPageSize。
func (l *GuildLogic) GetGuildRank(ctx context.Context, req *pb.GetGuildRankRequest) (*pb.GetGuildRankResponse, error) {
	zoneID := req.ZoneId
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = 20
	}
	page := req.Page
	if page == 0 {
		page = 1
	}
	if who := callerOf(ctx, 0); who.fromClient {
		homeZone, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.GetGuildRankResponse{ErrorMessage: tip}, nil
		}
		zoneID = homeZone
		pageSize = min(pageSize, constants.MaxRankPageSize)
	}

	entries, total, err := l.repo.GetGuildRankPage(ctx, zoneID, page, pageSize)
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

// GetGuildRankByGuild:客户端查的是本区榜上的名次;别区的帮会在本区榜上不存在,答复"未上榜"。
func (l *GuildLogic) GetGuildRankByGuild(ctx context.Context, req *pb.GetGuildRankByGuildRequest) (*pb.GetGuildRankByGuildResponse, error) {
	zoneID := req.ZoneId
	if who := callerOf(ctx, 0); who.fromClient {
		homeZone, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.GetGuildRankByGuildResponse{ErrorMessage: tip}, nil
		}
		zoneID = homeZone
	}

	entry, err := l.repo.GetGuildRank(ctx, req.GuildId, zoneID)
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
		// guild.proto 仍是 int64(B2 随客户端重生成统一改 uint64);毫秒时间戳远小于 2^63,转换无损。
		CreateTimeMs: int64(g.CreateTimeMs),
		MaxMembers:   g.MaxMembers,
		ZoneId:       g.ZoneID,
	}
	for _, m := range g.Members {
		info.Members = append(info.Members, &pb.GuildMember{
			PlayerId:     m.PlayerID,
			Role:         m.Role,
			JoinTimeMs:   int64(m.JoinTimeMs),
			LastActiveMs: int64(m.LastActiveMs),
			// B2 把 GuildMember.contribution 拆成 total / balance 之前,这里展示累计帮贡。
			Contribution: m.ContributionTotal,
			Online:       onlineMap[m.PlayerID],
		})
	}
	return info
}
