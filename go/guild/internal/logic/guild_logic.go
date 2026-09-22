package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ApplyPushGate 决定一条**新建**的入帮申请要不要推给审批人:返回 true 才发 APPLICATION_RECEIVED。
// 生产实现是 repo.TryMarkApplyPush(Redis SetNX 冷却,同一 (帮, 申请人) 60 秒至多一次),
// 挡的是"申请 → 撤回 → 申请"这种刷屏;不传 = 总是放行(单测)。
type ApplyPushGate func(ctx context.Context, guildID, playerID uint64) bool

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
	// notifier 是帮会变更推送的唯一出口,默认 NoopNotifier{}(未配 Kafka 或单测)。
	// 它只在 MySQL 提交之后被调用,失败绝不影响 RPC 结果(push.go 文件头纪律)。
	notifier GuildNotifier
	// applyPushGate 见 ApplyPushGate;默认"总是放行"。
	applyPushGate ApplyPushGate
	// playerNames 批量取成员 / 帮主的展示名(player_name_resolver.go)。
	// nil = 没配 DataServiceRpc 或单测:名字一律留空,不 panic。只经 resolveNames 访问。
	playerNames PlayerNameResolver
	// economy 是捐献 / 升级 / 商店的依赖(economy_logic.go 的 EconomyDeps),经 WithEconomy 注入。
	// nil = 未接线:五个经济 RPC 一律回 codes.Unavailable,其余 RPC 不受影响。
	economy *EconomyDeps
}

// Option 是 NewGuildLogic 的可选项。
//
// 用可选项而不是加参数:NewGuildLogic 已经有 16 处调用点(全在测试里),
// 每加一个依赖就改 16 处纯粹是噪音,而且下一批还要再加(B3b 的 PlayerNameResolver、
// B5 的经济依赖)。可选项让"新依赖"对存量调用点完全透明。
type Option func(*GuildLogic)

// WithNotifier 注入推送实现;不传 = NoopNotifier(单测与未配 Kafka 的环境)。
// 传 nil 被忽略而不是覆盖成 nil:notifier 在提交之后被调用,那里不该出现 nil 解引用。
func WithNotifier(n GuildNotifier) Option {
	return func(l *GuildLogic) {
		if n != nil {
			l.notifier = n
		}
	}
}

// WithApplyPushGate 注入"申请推送冷却"判定;不传 = 总是放行。
// 生产由 guild.go 传 repo.TryMarkApplyPush。
func WithApplyPushGate(g ApplyPushGate) Option {
	return func(l *GuildLogic) {
		if g != nil {
			l.applyPushGate = g
		}
	}
}

// WithPlayerNames 注入展示名解析器;不传或传 nil = 名字一律留空(90-consistency.md Y-02:
// 依赖注入只保留函数式 Option,03-names.md 正文里的 SetPlayerNameResolver 不再使用)。
// 生产由 guild.go 在配了 DataServiceRpc 时传 NewDataServicePlayerNames。
//
// 这里的判空只挡得住 nil **接口**;装着 nil 指针的接口由 DataServicePlayerNames.BatchResolve
// 自己的 nil 接收者分支兜住。
func WithPlayerNames(r PlayerNameResolver) Option {
	return func(l *GuildLogic) {
		if r != nil {
			l.playerNames = r
		}
	}
}

// NewGuildLogic。mergeFence / homeZones 可为 nil,语义见字段注释。
//
// 注意 nil 接口的坑:传 (*RedisMergeFence)(nil) 进来**不等于** nil 接口,
// 所以 guild.go 必须显式判空再决定传不传 —— 见那边的接线。
func NewGuildLogic(repo *data.GuildRepo, ids IDMinter, onlineResolver *OnlineStatusResolver,
	mergeFence MergeFence, homeZones HomeZoneLookup, opts ...Option) *GuildLogic {
	l := &GuildLogic{
		repo: repo, ids: ids, onlineResolver: onlineResolver,
		mergeFence: mergeFence, homeZones: homeZones,
		notifier:      NoopNotifier{},
		applyPushGate: func(context.Context, uint64, uint64) bool { return true },
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
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
// B2 新增的管理 / 申请 RPC **不开**内部调用口(协议里没有 player_id 字段),见 clientWrite。

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
	if tip := l.mergeFenceTip(ctx, zoneID); tip != nil {
		return &pb.CreateGuildResponse{ErrorMessage: tip}, nil
	}

	// 成员上限读 GuildLevel 第 1 级(默认 30)。读表放在闸门之后、铸号之前:
	// 配表缺行时既不越过闸门,也不白烧一个 guild_id。
	maxMembers, ok := initialMaxMembers()
	if !ok {
		logx.Errorf("[guild] GuildLevel row %d missing, cannot decide the member cap of a new guild", constants.DefaultInitLevel)
		return nil, status.Errorf(codes.Internal, "GuildLevel row %d missing", constants.DefaultInitLevel)
	}

	// 缓存预检只是省一次事务,不是判据:缓存说他已入帮时先用 MySQL 复核一次,
	// 免得一个陈旧映射把玩家永久挡在建帮之外。真正的判定在 CreateGuild 事务的唯一索引上。
	existingGuildID, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
	if err != nil {
		return nil, fmt.Errorf("check existing guild: %w", err)
	}
	if existingGuildID > 0 {
		verified, err := l.repo.VerifyPlayerGuildID(ctx, who.playerID, existingGuildID)
		if err != nil {
			return nil, fmt.Errorf("verify existing guild of player %d: %w", who.playerID, err)
		}
		if verified > 0 {
			return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
		}
	}

	now := nowMs()
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
		MaxMembers:   maxMembers,
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
		// 事务说他已入帮而缓存说没有:以 MySQL 为准,顺手把那个陈旧的 0 映射纠正回来。
		l.verifyMapping(ctx, who.playerID, 0)
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
	case errors.Is(err, data.ErrGuildNameTaken):
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNameTaken, "guild name taken")}, nil
	case errors.Is(err, data.ErrWriteConflict):
		// 写冲突是业务 tip 不是故障:玩家原地重试一次就能成功,回 gRPC 错误会让客户端进重连隔离。
		logx.Infof("[guild] create guild write conflict (player %d): %v", who.playerID, err)
		return &pb.CreateGuildResponse{ErrorMessage: tipErr(constants.ErrBusyRetry, "guild write conflict")}, nil
	case err != nil:
		return nil, fmt.Errorf("create guild: %w", err)
	}
	// 新公会加入排行榜(初始分数 0)。失败可容忍:score 权威在 MySQL,
	// ZSET 缺口由启动时 RebuildRanks 或下次推分自愈。
	if err := l.repo.UpdateGuildScore(ctx, guildID, zoneID, 0); err != nil {
		logx.Errorf("init guild rank score: %v", err)
	}

	// 建帮不推送:此刻帮里只有建帮者自己,而他手上这个响应就是最新状态。
	return &pb.CreateGuildResponse{Guild: l.guildInfoFor(ctx, guild, viewerOf(who))}, nil
}

// mergeFenceTip 返回非 nil 表示本次写被合服闸门拒绝(取代原先回 gRPC FailedPrecondition 的 checkMergeFence)。
//
// 三条分支各有理由:
//   - fence 为 nil(没配 MergeMarkerRedis)或 zoneID 为 0(内部调用)→ 放行。闸门是可选加固,
//     不能因为没配就把帮会功能整体关掉;guild.go 启动时已经打过"闸门未生效"的 Info。
//   - 查询报错 → **拒绝**(fail closed)。查不到闸门状态不等于没有闸门,
//     而合服窗口里写出来的帮会数据会落在一个正在搬迁的 zone 上 —— 那是数据事故,
//     拒绝只是一次可重试的失败。
//   - 键存在 → 拒绝。
//
// 为什么回 tip 而不是 gRPC 错误:gRPC 错误会被 serverbase 记成服务端故障,客户端也会
// 进入重连隔离;而"这个区正在合服,等几分钟"是可预期的运维窗口,玩家该看到一句人话。
// 码由配表发号(kGuildZoneMerging),不是手写数字。
func (l *GuildLogic) mergeFenceTip(ctx context.Context, zoneID uint32) *base.TipInfoMessage {
	if l.mergeFence == nil || zoneID == 0 {
		return nil
	}
	merging, err := l.mergeFence.MergeInProgress(ctx, zoneID)
	if err != nil {
		logx.Errorf("[guild] merge fence unreadable for zone %d, refusing (fail closed): %v", zoneID, err)
		return tipErr(constants.ErrZoneMerging, "merge fence unreadable")
	}
	if merging {
		logx.Infof("[guild] refused: zone %d is merging (%s present)", zoneID, MergeFenceKey(zoneID))
		return tipErr(constants.ErrZoneMerging, "zone merging")
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
	who := callerOf(ctx, 0)
	visibleZone := uint32(0)
	if who.fromClient {
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
	return &pb.GetGuildResponse{Guild: l.guildInfoFor(ctx, guild, viewerOf(who))}, nil
}

// GetPlayerGuild 不按 zone 过滤:玩家自己的帮会永远看得见(合服会把帮会与归属映射一起搬走)。
//
// 客户端来源改走 ResolvePlayerGuild:它在"缓存说没入帮"和"快照里没有本人"两种情况下
// 用 MySQL 复核一次。不复核的话,一次失败的缓存失效会让刚入帮的玩家在 30 分钟里
// 看到一个空帮会界面,而且没有任何自愈路径。内部调用保持原缓存路径,不给高频内部读加 MySQL 负载。
func (l *GuildLogic) GetPlayerGuild(ctx context.Context, req *pb.GetPlayerGuildRequest) (*pb.GetPlayerGuildResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	if who.fromClient {
		guild, err := l.repo.ResolvePlayerGuild(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if guild == nil {
			return &pb.GetPlayerGuildResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not in any guild")}, nil
		}
		return &pb.GetPlayerGuildResponse{Guild: l.guildInfoFor(ctx, guild, who.playerID)}, nil
	}

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
	return &pb.GetPlayerGuildResponse{Guild: l.guildInfoFor(ctx, guild, 0)}, nil
}

// LeaveGuild:主动退帮。帮主不能退(先转让或解散),判定在事务内按权威 role + leader_id 做,
// 不再看缓存里的 LeaderID —— 那个值在降权 / 转让之后会说谎整整一个 TTL。
func (l *GuildLogic) LeaveGuild(ctx context.Context, req *pb.LeaveGuildRequest) (*pb.LeaveGuildResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	if who.fromClient {
		zoneID, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.LeaveGuildResponse{ErrorMessage: tip}, nil
		}
		if tip := l.mergeFenceTip(ctx, zoneID); tip != nil {
			return &pb.LeaveGuildResponse{ErrorMessage: tip}, nil
		}
	}

	guildID, tip, err := l.operatorGuild(ctx, who.playerID)
	if err != nil || tip != nil {
		return &pb.LeaveGuildResponse{ErrorMessage: tip}, err
	}
	// now 显式传入(B5):事务内要把本人未结算捐献的截止时间提前到"现在",时钟归调用方(§11.2 显式依赖)。
	res, err := l.repo.LeaveGuild(ctx, guildID, who.playerID, nowMs())
	if errors.Is(err, data.ErrNotGuildMember) || errors.Is(err, data.ErrGuildGone) {
		// 缓存指的帮会里没有他:以 MySQL 复核。真的不在任何帮 = 这次退帮本来就无事可做(幂等成功);
		// 在别的帮 = 缓存过期,让客户端刷新后重来,**绝不**按旧 guild_id 去删别的帮的成员行。
		current, verifyErr := l.repo.VerifyPlayerGuildID(ctx, who.playerID, guildID)
		if verifyErr != nil {
			return nil, fmt.Errorf("verify guild mapping of player %d: %w", who.playerID, verifyErr)
		}
		if current == 0 {
			return &pb.LeaveGuildResponse{}, nil
		}
		logx.Errorf("player %d guild mapping changed from stale %d to authoritative %d; retrying is required",
			who.playerID, guildID, current)
		return &pb.LeaveGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "guild membership changed, retry")}, nil
	}
	if tip, err := l.mapWriteErr(ctx, who.playerID, guildID, guildID, err); err != nil || tip != nil {
		return &pb.LeaveGuildResponse{ErrorMessage: tip}, err
	}
	// 快照已不含退帮者,所以"剩余全体"就是收件人;他自己拿的是这次响应,不必再推。
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_LEFT, guildID, who.playerID, who.playerID, membersExcept(res.Guild))
	return &pb.LeaveGuildResponse{}, nil
}

// DisbandGuild:解散帮会。授权改读 MySQL(事务内 FOR UPDATE 的 leader_id),
// 原实现按缓存 LeaderID 判帮主 —— 转让之后那个值在一个 TTL 内还是旧帮主,等于把解散权多发一份。
func (l *GuildLogic) DisbandGuild(ctx context.Context, req *pb.DisbandGuildRequest) (*pb.DisbandGuildResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	if who.fromClient {
		zoneID, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.DisbandGuildResponse{ErrorMessage: tip}, nil
		}
		if tip := l.mergeFenceTip(ctx, zoneID); tip != nil {
			return &pb.DisbandGuildResponse{ErrorMessage: tip}, nil
		}
	}

	guildID, tip, err := l.operatorGuild(ctx, who.playerID)
	if err != nil || tip != nil {
		return &pb.DisbandGuildResponse{ErrorMessage: tip}, err
	}
	// now 显式传入(B5):解散事务把全体成员未结算捐献的截止时间提前到"现在"。
	// 闸门在事务内**再判一次**(friend 审计 #17 第 6 点):上面的 mergeFenceTip 查的是请求者归属区、且在事务外,
	// 与合服置闸之间有检查到使用的窗口,内部调用路径更是完全不查;repo 锁住 guild 行后按行里的 zone_id 调它,
	// 拒绝回 data.ErrZoneMerging,由 mapWriteErr 翻成 kGuildZoneMerging。
	res, err := l.repo.DisbandGuild(ctx, guildID, who.playerID, nowMs(), l.economyFence)
	// 解散是唯一一处 ErrRankTooLow 不回 kGuildRankTooLow 的地方:解散只有帮主能做,
	// 沿用既有的"只有会长可以执行该操作"文案比"职位不足"更贴合玩家看到的按钮。
	if errors.Is(err, data.ErrRankTooLow) {
		return &pb.DisbandGuildResponse{ErrorMessage: tipErr(constants.ErrNotLeader, "not guild leader")}, nil
	}
	if tip, err := l.mapWriteErr(ctx, who.playerID, guildID, guildID, err); err != nil || tip != nil {
		return &pb.DisbandGuildResponse{ErrorMessage: tip}, err
	}

	// 清榜**只能**用事务内读到的 zone。缓存里的 ZoneID 在合服之后会整整一个 TTL 指向源区,
	// 按它 ZREM 会去删一个空 ZSET,把条目永久留在目标区榜上(榜上出现查不到名字的幽灵帮会)。
	// 失败可容忍:RebuildRanks 会自愈。
	if err := l.repo.RemoveGuildFromRank(ctx, guildID, res.ZoneID); err != nil {
		logx.Errorf("remove guild %d (zone %d) from rank: %v", guildID, res.ZoneID, err)
	}

	recipients := make([]uint64, 0, len(res.MemberIDs))
	for _, id := range res.MemberIDs {
		if id != who.playerID {
			recipients = append(recipients, id)
		}
	}
	// target 取 0:解散没有"被作用的那个人"。只提交过申请的玩家不通知(他们的申请行已被删,
	// 下次打开申请列表自然就没了)。
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_DISBANDED, guildID, who.playerID, 0, recipients)
	return &pb.DisbandGuildResponse{}, nil
}

func (l *GuildLogic) SetAnnouncement(ctx context.Context, req *pb.SetAnnouncementRequest) (*pb.SetAnnouncementResponse, error) {
	who := callerOf(ctx, req.PlayerId)
	if len(req.Announcement) > constants.MaxAnnouncementBytes {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrAnnouncementTooLong, "announcement too long")}, nil
	}
	if who.fromClient {
		zoneID, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil {
			return nil, err
		}
		if tip != nil {
			return &pb.SetAnnouncementResponse{ErrorMessage: tip}, nil
		}
		if tip := l.mergeFenceTip(ctx, zoneID); tip != nil {
			return &pb.SetAnnouncementResponse{ErrorMessage: tip}, nil
		}
	}

	// 权限必须在 MySQL 更新事务内按权威 membership.role 复核；Redis GuildData
	// 可能仍缓存着操作者降权/退会前的 officer 身份，只能做展示，不能授权。
	// 公会 id 来自请求体也无妨:事务要求操作者是该公会的 officer/leader,改不了别人的帮。
	snapshot, err := l.repo.UpdateAnnouncement(ctx, req.GuildId, who.playerID, req.Announcement)
	if errors.Is(err, data.ErrAnnouncementForbidden) {
		return &pb.SetAnnouncementResponse{ErrorMessage: tipErr(constants.ErrNoPermission, "no permission")}, nil
	}
	if tip, err := l.mapWriteErr(ctx, who.playerID, req.GuildId, req.GuildId, err); err != nil || tip != nil {
		return &pb.SetAnnouncementResponse{ErrorMessage: tip}, err
	}

	// 响应带的是事务内快照,不再提交后回读缓存:回读会在失效失败时返回旧公告,
	// 玩家会以为自己没改成功而反复重试。
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ANNOUNCEMENT_CHANGED, req.GuildId, who.playerID, 0,
		membersExcept(snapshot, who.playerID))
	return &pb.SetAnnouncementResponse{Guild: l.guildInfoFor(ctx, snapshot, viewerOf(who))}, nil
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
	// DisbandGuild 清榜时只按事务内读到的 zone 调 RemoveGuildFromRank,若这里允许
	// req.ZoneId 把分数写进别的 zone 的 ZSET,解散后那个条目将永久残留
	// (榜单上出现查不到名字的幽灵公会)。
	if req.ZoneId > 0 && req.ZoneId != guild.ZoneID {
		logx.Errorf("UpdateGuildScore: requested zone %d ignored, guild %d belongs to zone %d",
			req.ZoneId, req.GuildId, guild.ZoneID)
	}
	// UpdateGuildScore 自 B2s 起走统一事务入口 inTx,于是多了一条 ErrWriteConflict 的返回:
	// 死锁重试耗尽、锁等待超时、子预算到期、提交结果不明四条路径都归一到它。
	// **写冲突必须回业务 tip**,不能包成 gRPC 错误 —— 本服务的纪律是 gRPC 错误会让客户端进入
	// 重连隔离,而两个人同时改同一个帮会是正常玩法,原地重试一次就能成功。
	switch err := l.repo.UpdateGuildScore(ctx, req.GuildId, guild.ZoneID, req.Score); {
	case err == nil:
	case errors.Is(err, data.ErrGuildGone):
		return &pb.UpdateGuildScoreResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild not found")}, nil
	case errors.Is(err, data.ErrWriteConflict):
		logx.Infof("[guild] guild %d score write conflict, asking the caller to retry: %v", req.GuildId, err)
		return &pb.UpdateGuildScoreResponse{ErrorMessage: tipErr(constants.ErrBusyRetry, "guild write conflict")}, nil
	default:
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
		// 帮主名是展示字段:取不到就留空(resolveNames fail-open),不让查名次失败。
		pbEntry.LeaderName = l.resolveNames(ctx, []uint64{guild.LeaderID})[guild.LeaderID]
	}

	return &pb.GetGuildRankByGuildResponse{Entry: pbEntry}, nil
}

// enrichRankEntries fills in guild name/level/member_count from cache for a page of rank entries.
//
// 帮主名整页**只取一次**:循环里只收集 leader_id,循环后一次批量查询再回填。
// 逐条查会把一页 50 条的榜单放大成 50 次 data_service 往返。
func (l *GuildLogic) enrichRankEntries(ctx context.Context, entries []data.RankEntry) ([]*pb.GuildRankEntry, error) {
	result := make([]*pb.GuildRankEntry, 0, len(entries))
	leaderIDs := make([]uint64, 0, len(entries))
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
			leaderIDs = append(leaderIDs, guild.LeaderID)
		}
		result = append(result, pbEntry)
	}
	// 读不到帮会的条目 LeaderId 为 0:它没进 leaderIDs,resolver 也按契约丢 0,names[0] 是空串,无需特判。
	names := l.resolveNames(ctx, leaderIDs)
	for _, pbEntry := range result {
		pbEntry.LeaderName = names[pbEntry.LeaderId]
	}
	return result, nil
}

// ── Proto conversion ───────────────────────────────────────────

// toProtoGuild 把权威快照转成客户端结构。
//
// 与请求者无关的字段全在这里填;只给长老 / 帮主看的待审数在 guildInfoFor 里补(§12.4)。
//
// 成员 name / leader_name 来自 data_service 的名字注册表(B3b,player_name_resolver.go):
// 展示字段、不入库,取不到就留空,绝不让整次读失败。成员 id 与帮主 id **合并成一次**批量查询 ——
// 帮主正常情况下就在成员里(resolver 自己去重),单独追加是为了在"leader_id 与成员表矛盾"的
// 坏数据下也能显示帮主名。
//
// 与在线状态的 MGET 顺序执行(先在线、后取名):正常路径上并行只省几十毫秒,却要多起一个协程并处理
// 它的收尾。两步的时间上界**不对称**,排障时别想当然:
//   - 取名有自己的上限 DefaultPlayerNameLookupTimeout(800ms),且走 gRPC、受 ctx 约束,
//     实际最多等 min(800ms, 整请求剩余预算);
//   - 在线 MGET **没有**独立超时:OnlineStatusResolver 直接拿请求 ctx 调 MGet,而 locator Redis 客户端
//     (svc/servicecontext.go)没开 ContextTimeoutEnabled —— go-redis v9 此时的套接字读**不看** ctx 截止时间,
//     每次尝试只受默认 3s ReadTimeout 约束,ctx 只在取连接与重试退避处生效。
//
// 所以 locator Redis 卡住时,是在线这一步先把整请求预算(3500ms)吃掉,轮到取名时 ctx 多半已经到期。
// DataServicePlayerNames.BatchResolve 为此在发 RPC 之前先判 ctx:已结束就直接回空 map、不计
// guild_player_name_lookup_failed_total,免得把 locator Redis 的故障记到 data_service 头上。
func (l *GuildLogic) toProtoGuild(ctx context.Context, g *data.GuildData) *pb.GuildInfo {
	if g == nil {
		return nil
	}
	// 容量多留 1:下面取名时追加帮主 id 不触发重新分配。
	memberIDs := make([]uint64, 0, len(g.Members)+1)
	for _, m := range g.Members {
		memberIDs = append(memberIDs, m.PlayerID)
	}
	onlineMap := l.onlineResolver.BatchResolve(ctx, memberIDs)
	names := l.resolveNames(ctx, append(memberIDs, g.LeaderID))

	info := &pb.GuildInfo{
		GuildId:      g.GuildID,
		Name:         g.Name,
		LeaderId:     g.LeaderID,
		LeaderName:   names[g.LeaderID],
		Level:        g.Level,
		Announcement: g.Announcement,
		CreateTimeMs: g.CreateTimeMs,
		MaxMembers:   g.MaxMembers,
		ZoneId:       g.ZoneID,
		Funds:        g.Funds,
	}
	officerCount := uint32(0)
	for _, m := range g.Members {
		if m.Role == constants.RoleOfficer {
			officerCount++
		}
		info.Members = append(info.Members, &pb.GuildMember{
			PlayerId:            m.PlayerID,
			Name:                names[m.PlayerID],
			Role:                m.Role,
			JoinTimeMs:          m.JoinTimeMs,
			LastActiveMs:        m.LastActiveMs,
			ContributionTotal:   m.ContributionTotal,
			ContributionBalance: m.ContributionBalance,
			Online:              onlineMap[m.PlayerID],
		})
	}
	info.OfficerCount = officerCount

	// 长老上限与升级花费是**展示**字段:查不到就留 0 并记 ERROR,不让整次读失败。
	// 真正的任免上限另走 officerCapFromTable,那条路缺行时是 fail-closed 的。
	if maxOfficers, upgradeCost, ok := levelDisplay(g.Level); ok {
		info.MaxOfficers = maxOfficers
		info.UpgradeCostFunds = upgradeCost
	} else {
		logx.Errorf("[guild] GuildLevel row %d missing while rendering guild %d", g.Level, g.GuildID)
	}
	return info
}
