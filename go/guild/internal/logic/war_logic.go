package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/constants"
	"guild/internal/data"
	"guild/internal/war"
	battlepb "proto/battle"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	rollbackpb "proto/common/rollback"
	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
	matchpb "proto/match"
	scenepb "proto/scene"
	smpb "proto/scene_manager"
	"shared/assetop"
	"shared/authoritysql"
	"shared/currencytype"
	"shared/gameday"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
	"shared/redisslot"
	"shared/serviceauth"
)

// WarBattleStarter 封装帮战直打开局 RPC（生产为带 serviceauth 签名的 MatchInternal.StartGuildWarBattle）。
type WarBattleStarter interface {
	StartGuildWarBattle(ctx context.Context, in *matchpb.StartGuildWarBattleRequest, opts ...grpc.CallOption) (*matchpb.StartGuildWarBattleResponse, error)
}

type signedWarStarter struct {
	inner  matchpb.MatchInternalClient
	secret string
}

func NewSignedWarStarter(inner matchpb.MatchInternalClient, secret string) (WarBattleStarter, error) {
	if inner == nil {
		return nil, errors.New("war battle starter: nil MatchInternalClient")
	}
	if _, err := serviceauth.New(map[string]string{"guild": secret}); err != nil {
		return nil, fmt.Errorf("war battle starter: %w", err)
	}
	return &signedWarStarter{inner: inner, secret: secret}, nil
}

func (s *signedWarStarter) StartGuildWarBattle(ctx context.Context, in *matchpb.StartGuildWarBattleRequest, opts ...grpc.CallOption) (*matchpb.StartGuildWarBattleResponse, error) {
	if in == nil {
		return nil, errors.New("war battle starter: nil request")
	}
	signed := proto.Clone(in).(*matchpb.StartGuildWarBattleRequest)
	signed.Auth = nil
	if err := serviceauth.Sign(matchpb.MatchInternal_StartGuildWarBattle_FullMethodName, signed, "guild", s.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, err
	}
	return s.inner.StartGuildWarBattle(ctx, signed, opts...)
}

// WarDeps 帮会全天帮战 V2 外部依赖，由 guild.go 经 WithWar 一次装配。
type WarDeps struct {
	Repo         *data.WarRepo
	Loop         *assetop.Loop
	OpIDs        IDMinter
	BountyIDs    IDMinter
	Match        WarBattleStarter
	PlayerRedis  redis.UniversalClient
	ControlRedis redis.UniversalClient
	Now          func() time.Time
	SyncBudget   time.Duration
	Lease        time.Duration
	MatchBudget  time.Duration
}

func WithWar(d WarDeps) Option {
	return func(l *GuildLogic) {
		if d.Repo == nil {
			return
		}
		deps := d
		if deps.Now == nil {
			deps.Now = time.Now
		}
		if deps.SyncBudget <= 0 {
			deps.SyncBudget = defaultSyncBudget
		}
		if deps.Lease <= 0 {
			deps.Lease = defaultInsertLease
		}
		if deps.MatchBudget <= 0 {
			deps.MatchBudget = 1500 * time.Millisecond
		}
		l.war = &deps
	}
}

func (l *GuildLogic) warNow() time.Time {
	if l.war != nil && l.war.Now != nil {
		return l.war.Now()
	}
	return time.Now()
}

func (l *GuildLogic) warRule() *tablepb.GuildWarRuleTable {
	r, _ := table.GuildWarRuleTableManagerInstance.FindById(war.RuleRowID)
	return r
}

func (l *GuildLogic) warWeekKey(now time.Time) uint32 {
	return gameday.WeekKey(now)
}

func (l *GuildLogic) warDayKey(now time.Time) uint32 {
	return gameday.DayKey(now)
}

type warBattleSummary struct {
	AttackerGuildName string   `json:"ag"`
	DefenderGuildName string   `json:"dg"`
	AttackerNames     []string `json:"an"`
	DefenderNames     []string `json:"dn"`
	AttackerIDs       []uint64 `json:"ai"`
	DefenderIDs       []uint64 `json:"di"`
	GoldLeyline       bool     `json:"gl"`
	RivalryBonus      bool     `json:"rb"`
	FeudBonus         bool     `json:"fb,omitempty"`
}

func buildSilverItemBundle(silver uint64, items ...*assetpb.ItemGrant) ([]byte, error) {
	b := &assetpb.AssetBundle{}
	if silver > 0 {
		b.Currencies = append(b.Currencies, &assetpb.CurrencyAmount{
			CurrencyType: currencytype.Gold,
			Amount:       silver,
		})
	}
	for _, it := range items {
		if it != nil && it.GetConfigId() != 0 && it.GetCount() > 0 {
			b.Items = append(b.Items, it)
		}
	}
	if len(b.Currencies) == 0 && len(b.Items) == 0 {
		return nil, nil
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(b)
}

func (l *GuildLogic) readBattleLockID(ctx context.Context, playerID uint64) uint64 {
	if l.war == nil || l.war.PlayerRedis == nil || playerID == 0 {
		return 0
	}
	raw, err := l.war.PlayerRedis.Get(ctx, redisslot.BattleLockKey(playerID)).Result()
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	return id
}

func (l *GuildLogic) resolvePlayerScene(ctx context.Context, zoneID uint32, playerID uint64) (online bool, sceneConfigID uint32, channelNo uint32, sceneID uint64) {
	if l.war == nil || l.war.PlayerRedis == nil || playerID == 0 {
		return false, 0, 0, 0
	}
	raw, err := l.war.PlayerRedis.Get(ctx, redisslot.LocationKey(playerID)).Bytes()
	if err != nil || len(raw) == 0 {
		return false, 0, 0, 0
	}
	loc := &smpb.PlayerLocation{}
	if proto.Unmarshal(raw, loc) != nil || loc.GetSceneId() == 0 {
		return false, 0, 0, 0
	}
	online = true
	sceneID = loc.GetSceneId()
	targetZone := loc.GetZoneId()
	if targetZone == 0 {
		targetZone = zoneID
	}
	if l.war.ControlRedis == nil || targetZone == 0 {
		return online, 0, 0, sceneID
	}
	// 对照分线目录确定精确地图配置号与线号（覆盖 1 天墉城 + 三座灵脉野外图 2/3/5）。
	for _, confID := range []uint32{1, 2, 3, 5} {
		dirKey := fmt.Sprintf("world_channel_directory:%s:%d", redisslot.ZoneControlTag(redisslot.ControlFamilySceneControl, targetZone), confID)
		dirRaw, dirErr := l.war.ControlRedis.Get(ctx, dirKey).Bytes()
		if dirErr != nil || len(dirRaw) == 0 {
			continue
		}
		dir := &scenepb.SceneChannelDirectory{}
		if proto.Unmarshal(dirRaw, dir) != nil {
			continue
		}
		for _, ch := range dir.GetChannels() {
			if ch.GetSceneId() == sceneID {
				return true, confID, ch.GetChannelNo(), sceneID
			}
		}
	}
	return online, 0, 0, sceneID
}

func (l *GuildLogic) resolveRecommendedChannel(ctx context.Context, zoneID, sceneConfigID uint32) (sceneID uint64, channelNo uint32) {
	if l.war == nil || l.war.ControlRedis == nil || zoneID == 0 || sceneConfigID == 0 {
		return 0, 0
	}
	dirKey := fmt.Sprintf("world_channel_directory:%s:%d", redisslot.ZoneControlTag(redisslot.ControlFamilySceneControl, zoneID), sceneConfigID)
	dirRaw, err := l.war.ControlRedis.Get(ctx, dirKey).Bytes()
	if err != nil || len(dirRaw) == 0 {
		return 0, 0
	}
	dir := &scenepb.SceneChannelDirectory{}
	if proto.Unmarshal(dirRaw, dir) != nil {
		return 0, 0
	}
	for _, ch := range dir.GetChannels() {
		if ch.GetState() == scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH || ch.GetState() == scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY {
			return ch.GetSceneId(), ch.GetChannelNo()
		}
	}
	if len(dir.GetChannels()) > 0 {
		return dir.GetChannels()[0].GetSceneId(), dir.GetChannels()[0].GetChannelNo()
	}
	return 0, 0
}

func (l *GuildLogic) syncDeliverWarOp(ctx context.Context, op data.AssetOpRow) pb.GuildAssetOrderStatus {
	if l.war == nil || l.war.Loop == nil || op.OpID == 0 {
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING
	}
	budget, ok := syncBudgetFor(ctx, l.war.SyncBudget)
	if !ok {
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING
	}
	callCtx, cancel := context.WithTimeout(withSyncDelivery(ctx), budget)
	defer cancel()
	ao, err := op.AssetOp()
	if err != nil {
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING
	}
	if _, err := l.war.Loop.ProcessOne(callCtx, ao); err != nil {
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING
	}
	return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED
}

func (l *GuildLogic) resolveWarPlayerGuildID(ctx context.Context, playerID uint64) (uint64, error) {
	g, err := l.repo.ResolvePlayerGuild(ctx, playerID)
	if err != nil || g == nil {
		return 0, err
	}
	return g.GuildID, nil
}

func (l *GuildLogic) requireWarMember(ctx context.Context) (playerID, guildID uint64, guild *data.GuildData, zoneID uint32, tipID uint32, err error) {
	who := callerOf(ctx, 0)
	if err := requireClientSession(who); err != nil {
		return 0, 0, nil, 0, 0, err
	}
	if l.war == nil || l.war.Repo == nil {
		return 0, 0, nil, 0, constants.ErrWarNotOpen, nil
	}
	g, err := l.repo.ResolvePlayerGuild(ctx, who.playerID)
	if err != nil {
		return 0, 0, nil, 0, 0, status.Errorf(codes.Internal, "resolve player guild: %v", err)
	}
	if g == nil || g.GuildID == 0 {
		return who.playerID, 0, nil, 0, constants.ErrNotInGuild, nil
	}
	z := l.repo.FinalZone(g.ZoneID)
	if z == 0 {
		z = g.ZoneID
	}
	return who.playerID, g.GuildID, g, z, 0, nil
}

// ── 1. GetGuildWarOverview ───────────────────────────────────────────────────

func (l *GuildLogic) GetGuildWarOverview(ctx context.Context, _ *pb.GetGuildWarOverviewRequest) (*pb.GetGuildWarOverviewResponse, error) {
	playerID, guildID, guild, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.GetGuildWarOverviewResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	rule := l.warRule()
	_ = l.war.Repo.EnsureZoneLeylines(ctx, zoneID, nowMs)

	leylinesRecs, err := l.war.Repo.ListLeylines(ctx, zoneID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list leylines: %v", err)
	}
	myState, err := l.war.Repo.GetPlayerWarState(ctx, playerID, zoneID, weekKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get player war state: %v", err)
	}
	prog, err := l.war.Repo.GetWeeklyProgress(ctx, guildID, weekKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get weekly progress: %v", err)
	}
	firstWin, _ := l.war.Repo.GetWeeklyProgress(ctx, guildID, 0)
	myGarrison, _ := l.war.Repo.ListGarrison(ctx, guildID)
	rivals, _ := l.war.Repo.ListRivals(ctx, guildID, weekKey)
	rivalSet := make(map[uint64]bool, len(rivals))
	for _, rg := range rivals {
		rivalSet[rg] = true
	}

	goldID, goldEndsAt := war.GoldLeylineForHour(nowMs)
	maxHP := war.DefaultLeylineMaxBannerHP
	minMerit := war.DefaultDividendMinMerit
	divPlayerCap := war.DefaultDividendPlayerWeeklyCap
	divGuildCap := war.DefaultDividendGuildWeeklyCap
	bountyPayLimit := war.DefaultBountyDailyPayoutLimit
	bountyPubLimit := war.DefaultBountyPublicDailyLimit
	bountyCounterTear := war.DefaultBountyCounterKillTear
	garrisonCD := war.DefaultGarrisonRefreshCD
	trackCD := war.DefaultTrackCooldownSec
	if rule != nil {
		if rule.GetLeylineMaxBannerHp() > 0 {
			maxHP = rule.GetLeylineMaxBannerHp()
		}
		if rule.GetDividendMinPersonalMerit() > 0 {
			minMerit = rule.GetDividendMinPersonalMerit()
		}
		if rule.GetDividendPlayerWeeklyCap() > 0 {
			divPlayerCap = rule.GetDividendPlayerWeeklyCap()
		}
		if rule.GetDividendGuildWeeklyCap() > 0 {
			divGuildCap = rule.GetDividendGuildWeeklyCap()
		}
		if rule.GetBountyDailyPayoutLimit() > 0 {
			bountyPayLimit = rule.GetBountyDailyPayoutLimit()
		}
		if rule.GetBountyPublicDailyLimit() > 0 {
			bountyPubLimit = rule.GetBountyPublicDailyLimit()
		}
		if rule.GetBountyCounterKillTear() > 0 {
			bountyCounterTear = rule.GetBountyCounterKillTear()
		}
		if rule.GetGarrisonRefreshCooldownSec() > 0 {
			garrisonCD = rule.GetGarrisonRefreshCooldownSec()
		}
		if rule.GetTrackCooldownSec() > 0 {
			trackCD = rule.GetTrackCooldownSec()
		}
	}

	// 汇总本帮可用帮贡。
	var myContrib uint64
	for _, m := range guild.Members {
		if m.PlayerID == playerID {
			myContrib = m.ContributionBalance
			break
		}
	}

	// 解析相关帮会名与功臣名。
	guildNameCache := map[uint64]string{guildID: guild.Name}
	lookupGuildName := func(gid uint64) string {
		if gid == 0 {
			return "无主（灵脉守卫）"
		}
		if name, ok := guildNameCache[gid]; ok {
			return name
		}
		if g, gErr := l.repo.GetGuild(ctx, gid); gErr == nil && g != nil {
			guildNameCache[gid] = g.Name
			return g.Name
		}
		return fmt.Sprintf("帮会#%d", gid)
	}

	var occupiedLeyline uint32
	leylineViews := make([]*pb.GuildWarLeylineView, 0, len(leylinesRecs))
	for _, lr := range leylinesRecs {
		meta, _ := war.LeylineByID(lr.GetLeylineId())
		if lr.GetOwnerGuildId() == guildID {
			occupiedLeyline = lr.GetLeylineId()
		}
		gCount := uint32(0)
		if lr.GetOwnerGuildId() != 0 {
			if slots, sErr := l.war.Repo.ListGarrison(ctx, lr.GetOwnerGuildId()); sErr == nil {
				gCount = uint32(len(slots))
			}
		}
		recSceneID, recChNo := l.resolveRecommendedChannel(ctx, zoneID, meta.SceneConfigID)
		leylineViews = append(leylineViews, &pb.GuildWarLeylineView{
			LeylineId:            lr.GetLeylineId(),
			Name:                 meta.Name,
			SceneConfigId:        meta.SceneConfigID,
			OwnerGuildId:         lr.GetOwnerGuildId(),
			OwnerGuildName:       lookupGuildName(lr.GetOwnerGuildId()),
			BannerHp:             lr.GetBannerHp(),
			BannerMaxHp:          maxHP,
			ShieldUntilMs:        lr.GetShieldUntilMs(),
			UnderSiege:           lr.GetLastSiegeAlertMs() > 0 && nowMs < lr.GetLastSiegeAlertMs()+120_000,
			IsGoldLeyline:        lr.GetLeylineId() == goldID,
			GarrisonCount:        gCount,
			RecommendedSceneId:   recSceneID,
			RecommendedChannelNo: recChNo,
		})
	}

	// 本周里程碑宝箱视图（4 档）。
	milestoneRows := table.GuildWarMilestoneTableManagerInstance.FindAll()
	milestoneViews := make([]*pb.GuildWarMilestoneView, 0, len(milestoneRows))
	for _, row := range milestoneRows {
		bit := uint32(1) << (row.GetId() - 1)
		claimed := (myState.GetMilestoneClaimedMask() & bit) != 0
		claimable := !claimed && prog.GetGuildMerit() >= row.GetRequiredGuildMerit() && myState.GetWeeklyMerit() >= minMerit
		mv := &pb.GuildWarMilestoneView{
			MilestoneId:        row.GetId(),
			RequiredMerit:      row.GetRequiredGuildMerit(),
			GuildMerit:         prog.GetGuildMerit(),
			Claimed:            claimed,
			Claimable:          claimable,
			RewardPreview:      row.GetRewardPreview(),
			RewardSilver:       row.GetRewardSilver(),
			RewardContribution: row.GetRewardContribution(),
		}
		if row.GetRewardItem1Id() != 0 {
			mv.RewardItems = append(mv.RewardItems, &pb.GuildRewardItem{ItemId: row.GetRewardItem1Id(), Count: row.GetRewardItem1Count()})
		}
		if row.GetRewardItem2Id() != 0 {
			mv.RewardItems = append(mv.RewardItems, &pb.GuildRewardItem{ItemId: row.GetRewardItem2Id(), Count: row.GetRewardItem2Count()})
		}
		milestoneViews = append(milestoneViews, mv)
	}

	// 分红视图。
	claimableDiv := uint32(0)
	if myState.GetWeeklyMerit() >= minMerit && prog.GetDividendPoolCount() > myState.GetDividendClaimedCount() && myState.GetDividendClaimedCount() < divPlayerCap {
		claimableDiv = prog.GetDividendPoolCount() - myState.GetDividendClaimedCount()
		if myState.GetDividendClaimedCount()+claimableDiv > divPlayerCap {
			claimableDiv = divPlayerCap - myState.GetDividendClaimedCount()
		}
	}

	// 动态帮旗功臣 + 建帮首胜 + 宿敌。
	nameIDs := []uint64{}
	if prog.GetMvp1PlayerId() != 0 {
		nameIDs = append(nameIDs, prog.GetMvp1PlayerId())
	}
	if prog.GetMvp2PlayerId() != 0 {
		nameIDs = append(nameIDs, prog.GetMvp2PlayerId())
	}
	if firstWin != nil {
		if firstWin.GetFirstWinPlayer1Id() != 0 {
			nameIDs = append(nameIDs, firstWin.GetFirstWinPlayer1Id())
		}
		if firstWin.GetFirstWinPlayer2Id() != 0 {
			nameIDs = append(nameIDs, firstWin.GetFirstWinPlayer2Id())
		}
	}

	openPlayers, _ := l.war.Repo.ListOpenChallengePlayers(ctx, zoneID, 24)
	streakLeaders, _ := l.war.Repo.ListStreakLeaders(ctx, zoneID, 8)
	bountiesRecs, _ := l.war.Repo.ListActiveBounties(ctx, zoneID, guildID, nowMs)
	for _, op := range openPlayers {
		nameIDs = append(nameIDs, op.GetPlayerId())
	}
	for _, sl := range streakLeaders {
		nameIDs = append(nameIDs, sl.GetPlayerId())
	}
	for _, br := range bountiesRecs {
		nameIDs = append(nameIDs, br.GetTargetPlayerId())
	}
	resolvedNames := l.resolveNames(ctx, nameIDs)
	nameOf := func(pid uint64) string {
		if pid == 0 {
			return ""
		}
		if n := resolvedNames[pid]; n != "" {
			return n
		}
		return fmt.Sprintf("道友%d", pid)
	}

	bannerHonor := &pb.GuildWarBannerHonorView{
		OccupiedLeylineId: occupiedLeyline,
		RivalGuildIds:     rivals,
	}
	for _, rg := range rivals {
		bannerHonor.RivalGuildNames = append(bannerHonor.RivalGuildNames, lookupGuildName(rg))
	}
	if prog.GetMvp1PlayerId() != 0 {
		bannerHonor.WeeklyMvps = append(bannerHonor.WeeklyMvps, nameOf(prog.GetMvp1PlayerId()))
	}
	if prog.GetMvp2PlayerId() != 0 {
		bannerHonor.WeeklyMvps = append(bannerHonor.WeeklyMvps, nameOf(prog.GetMvp2PlayerId()))
	}
	if firstWin != nil && firstWin.GetFirstWinAtMs() > 0 {
		bannerHonor.FirstWinAtMs = firstWin.GetFirstWinAtMs()
		if firstWin.GetFirstWinPlayer1Id() != 0 {
			bannerHonor.FirstWinNames = append(bannerHonor.FirstWinNames, nameOf(firstWin.GetFirstWinPlayer1Id()))
		}
		if firstWin.GetFirstWinPlayer2Id() != 0 {
			bannerHonor.FirstWinNames = append(bannerHonor.FirstWinNames, nameOf(firstWin.GetFirstWinPlayer2Id()))
		}
	}

	// 活跃追杀令映射（供在线列表角标）。
	bountyByTarget := make(map[uint64]*pb.GuildWarBountyRecord, len(bountiesRecs))
	for _, br := range bountiesRecs {
		bountyByTarget[br.GetTargetPlayerId()] = br
	}

	// 在线迎战活人列表（排除同帮、和平免战与离线）。
	onlineIDs := make([]uint64, 0, len(openPlayers))
	for _, op := range openPlayers {
		onlineIDs = append(onlineIDs, op.GetPlayerId())
	}
	onlineMap := map[uint64]bool{}
	if l.onlineResolver != nil && len(onlineIDs) > 0 {
		onlineMap = l.onlineResolver.BatchResolve(ctx, onlineIDs)
	}
	liveTargets := make([]*pb.GuildWarLiveTargetView, 0, len(openPlayers))
	for _, op := range openPlayers {
		if op.GetPlayerId() == playerID || op.GetPeaceMode() != 0 {
			continue
		}
		if l.onlineResolver != nil && !onlineMap[op.GetPlayerId()] {
			continue
		}
		tgid, _ := l.resolveWarPlayerGuildID(ctx, op.GetPlayerId())
		if tgid == 0 || tgid == guildID {
			continue
		}
		bid := l.readBattleLockID(ctx, op.GetPlayerId())
		var activeBountyID uint64
		if b := bountyByTarget[op.GetPlayerId()]; b != nil {
			activeBountyID = b.GetBountyId()
		}
		liveTargets = append(liveTargets, &pb.GuildWarLiveTargetView{
			PlayerId:       op.GetPlayerId(),
			PlayerName:     nameOf(op.GetPlayerId()),
			GuildId:        tgid,
			GuildName:      lookupGuildName(tgid),
			PartySize:      1,
			Streak:         op.GetStreakCount(),
			InBattle:       bid != 0,
			BattleId:       bid,
			IsRivalGuild:   rivalSet[tgid],
			ActiveBountyId: activeBountyID,
		})
	}

	// 连胜悬赏条。
	streakViews := make([]*pb.GuildWarStreakBountyView, 0, len(streakLeaders))
	for _, sl := range streakLeaders {
		tgid, _ := l.resolveWarPlayerGuildID(ctx, sl.GetPlayerId())
		if tgid == 0 || tgid == guildID {
			continue
		}
		jc, js := war.StreakBountyJackpot(sl.GetStreakCount(), sl.GetStreakDistinctCount())
		if jc == 0 && js == 0 {
			continue
		}
		bid := l.readBattleLockID(ctx, sl.GetPlayerId())
		streakViews = append(streakViews, &pb.GuildWarStreakBountyView{
			PlayerId:          sl.GetPlayerId(),
			PlayerName:        nameOf(sl.GetPlayerId()),
			GuildId:           tgid,
			GuildName:         lookupGuildName(tgid),
			Streak:            sl.GetStreakCount(),
			BonusContribution: jc,
			BonusSilver:       js,
			InBattle:          bid != 0,
			BattleId:          bid,
		})
	}

	// 本帮守旗阵槽位。
	garrisonViews := make([]*pb.GuildWarGarrisonSlotView, 0, len(myGarrison))
	for _, gSlot := range myGarrison {
		garrisonViews = append(garrisonViews, &pb.GuildWarGarrisonSlotView{
			SlotIndex:    gSlot.GetSlotIndex(),
			PlayerId:     gSlot.GetPlayerId(),
			PlayerName:   gSlot.GetPlayerName(),
			Level:        gSlot.GetLevel(),
			AppearanceId: gSlot.GetAppearanceId(),
			PetName:      gSlot.GetPetName(),
			PetTableId:   gSlot.GetPetTableId(),
			UpdatedMs:    gSlot.GetUpdatedMs(),
		})
	}

	// 追杀令视图。
	bountyViews := make([]*pb.GuildWarBountyView, 0, len(bountiesRecs))
	for _, br := range bountiesRecs {
		tOnline, tSceneConf, tChan, _ := l.resolvePlayerScene(ctx, zoneID, br.GetTargetPlayerId())
		inSafe := tOnline && war.IsSafeZoneScene(tSceneConf)
		tState, _ := l.war.Repo.GetPlayerWarState(ctx, br.GetTargetPlayerId(), zoneID, weekKey)
		canDirect := br.GetStatus() == 2 && tOnline && !inSafe && (tState == nil || tState.GetPeaceMode() == 0)
		bountyViews = append(bountyViews, &pb.GuildWarBountyView{
			BountyId:           br.GetBountyId(),
			TargetPlayerId:     br.GetTargetPlayerId(),
			TargetName:         nameOf(br.GetTargetPlayerId()),
			TargetGuildId:      br.GetTargetGuildId(),
			TargetGuildName:    lookupGuildName(br.GetTargetGuildId()),
			IssuerGuildId:      br.GetIssuerGuildId(),
			IssuerGuildName:    lookupGuildName(br.GetIssuerGuildId()),
			Tier:               pb.GuildWarBountyTier(br.GetTier()),
			RewardSilver:       br.GetEscrowSilver(),
			PublicBoard:        br.GetPublicBoard() != 0,
			Status:             pb.GuildWarBountyStatus(br.GetStatus()),
			CounterKills:       br.GetCounterKills(),
			CounterKillsToTear: bountyCounterTear,
			ExpiresAtMs:        br.GetExpiresAtMs(),
			LastSceneConfigId:  tSceneConf,
			LastChannelNo:      tChan,
			TargetInSafeZone:   inSafe,
			TargetOnline:       tOnline,
			NextTrackAtMs:      myState.GetLastTrackMs() + uint64(trackCD)*1000,
			CanDirectAttack:    canDirect,
		})
	}

	// 近 30 场帮战大事记 & 近 24h 帮会仇敌录。
	recentRecs, _ := l.war.Repo.ListRecentBattles(ctx, guildID, 30)
	recentLogs := make([]*pb.GuildWarRecentLogView, 0, len(recentRecs))
	enemies := make([]*pb.GuildWarEnemyView, 0, 12)
	seenEnemy := map[uint64]bool{}
	cutoff24h := uint64(0)
	if nowMs > 24*3600*1000 {
		cutoff24h = nowMs - 24*3600*1000
	}
	for _, br := range recentRecs {
		var sum warBattleSummary
		_ = json.Unmarshal(br.GetSummaryPayload(), &sum)
		selfIn := false
		for _, id := range sum.AttackerIDs {
			if id == playerID {
				selfIn = true
				break
			}
		}
		if !selfIn {
			for _, id := range sum.DefenderIDs {
				if id == playerID {
					selfIn = true
					break
				}
			}
		}
		agName := sum.AttackerGuildName
		if agName == "" {
			agName = lookupGuildName(br.GetAttackerGuildId())
		}
		dgName := sum.DefenderGuildName
		if dgName == "" {
			dgName = lookupGuildName(br.GetDefenderGuildId())
		}
		recentLogs = append(recentLogs, &pb.GuildWarRecentLogView{
			BattleId:          br.GetBattleId(),
			WarBattleKind:     pb.GuildWarBattleKind(br.GetWarBattleKind()),
			LeylineId:         br.GetLeylineId(),
			AttackerGuildName: agName,
			DefenderGuildName: dgName,
			AttackerSummary:   strings.Join(sum.AttackerNames, "、"),
			DefenderSummary:   strings.Join(sum.DefenderNames, "、"),
			Outcome:           br.GetOutcome(),
			Rounds:            br.GetTotalRounds(),
			BannerHpDelta:     int32(br.GetBannerHpAfter()) - int32(br.GetBannerHpBefore()),
			BountyOutcomeTag:  br.GetBountyOutcomeTag(),
			SelfParticipated:  selfIn,
			FinishedAtMs:      br.GetFinishedAtMs(),
		})

		// 若本帮战败且发生在 24h 内，自动把对方首名击杀者收入仇敌录。
		if br.GetFinishedAtMs() >= cutoff24h {
			var killerID, killerGuild uint64
			var killerName, victimName string
			if br.GetDefenderGuildId() == guildID && br.GetOutcome() == 1 && len(sum.AttackerIDs) > 0 {
				killerID, killerGuild = sum.AttackerIDs[0], br.GetAttackerGuildId()
				if len(sum.AttackerNames) > 0 {
					killerName = sum.AttackerNames[0]
				}
				if len(sum.DefenderNames) > 0 {
					victimName = sum.DefenderNames[0]
				}
			} else if br.GetAttackerGuildId() == guildID && br.GetOutcome() == 2 && len(sum.DefenderIDs) > 0 {
				killerID, killerGuild = sum.DefenderIDs[0], br.GetDefenderGuildId()
				if len(sum.DefenderNames) > 0 {
					killerName = sum.DefenderNames[0]
				}
				if len(sum.AttackerNames) > 0 {
					victimName = sum.AttackerNames[0]
				}
			}
			if killerID != 0 && killerGuild != 0 && killerGuild != guildID && !seenEnemy[killerID] {
				seenEnemy[killerID] = true
				canPost := bountyByTarget[killerID] == nil
				blockedTip := uint32(0)
				if !canPost {
					blockedTip = constants.ErrWarBountyAlreadyActive
				}
				enemies = append(enemies, &pb.GuildWarEnemyView{
					PlayerId:      killerID,
					PlayerName:    killerName,
					GuildId:       killerGuild,
					GuildName:     lookupGuildName(killerGuild),
					VictimName:    victimName,
					BattleKind:    pb.GuildWarBattleKind(br.GetWarBattleKind()),
					OccurredAtMs:  br.GetFinishedAtMs(),
					CanPostBounty: canPost,
					BlockedTipId:  blockedTip,
				})
			}
		}
	}

	nextRefresh := uint64(0)
	if myState.GetLastGarrisonRefreshMs() > 0 {
		nextRefresh = myState.GetLastGarrisonRefreshMs() + uint64(garrisonCD)*1000
	}
	nextTrack := uint64(0)
	if myState.GetLastTrackMs() > 0 {
		nextTrack = myState.GetLastTrackMs() + uint64(trackCD)*1000
	}

	// 宿敌约战战书实时状态（含到期自动收官播报）与最近一场战报碑。
	var activeFeudView *pb.GuildWarFeudView
	activeFeudRec, justFinishedFeud, _ := l.war.Repo.GetActiveOrPendingFeud(ctx, guildID, nowMs)
	if justFinishedFeud != nil {
		l.broadcastFeudAlert(ctx, pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_FEUD_ENDED, justFinishedFeud)
	}
	if activeFeudRec != nil {
		activeFeudView = toFeudView(activeFeudRec)
	}
	lastFeudSummary := ""
	if lastFin, _ := l.war.Repo.GetLastFinishedFeud(ctx, guildID); lastFin != nil {
		lastFeudSummary = war.FormatFeudSummary(lastFin)
	}
	nextFeudAt := uint64(0)
	if lastIssuedAt, _ := l.war.Repo.GetLastFeudIssuedAt(ctx, guildID); lastIssuedAt > 0 {
		cand := lastIssuedAt + uint64(war.DefaultFeudCooldownSec)*1000
		if cand > nowMs {
			nextFeudAt = cand
		}
	}

	return &pb.GetGuildWarOverviewResponse{
		ServerTimeMs:       nowMs,
		WeekKey:            weekKey,
		GoldLeylineId:      goldID,
		GoldLeylineEndsAtMs: goldEndsAt,
		MyStance: &pb.GuildWarStanceView{
			PeaceMode:             myState.GetPeaceMode() != 0,
			PeaceReadyAtMs:        myState.GetPeaceReadyAtMs(),
			OpenChallenge:         myState.GetOpenChallenge() != 0,
			ProtectUntilMs:        myState.GetProtectUntilMs(),
			MyStreak:              myState.GetStreakCount(),
			MyWeeklyMerit:         myState.GetWeeklyMerit(),
			MyBountyPayoutLimit:   bountyPayLimit,
			BountyProtectUntilMs:  myState.GetBountyProtectUntilMs(),
			NextGarrisonRefreshMs: nextRefresh,
			NextTrackAtMs:         nextTrack,
		},
		BannerHonor:            bannerHonor,
		Leylines:               leylineViews,
		Milestones:             milestoneViews,
		Dividend:               &pb.GuildWarDividendView{ClaimableCount: claimableDiv, ClaimedThisWeek: myState.GetDividendClaimedCount(), WeeklyCap: divPlayerCap, GuildProducedThisWeek: prog.GetDividendPoolCount(), GuildWeeklyCap: divGuildCap, MinPersonalMerit: minMerit},
		StreakBounties:         streakViews,
		LiveTargets:            liveTargets,
		GarrisonSlots:          garrisonViews,
		Enemies:                enemies,
		Bounties:               bountyViews,
		RecentLogs:             recentLogs,
		ActiveBattleId:         l.readBattleLockID(ctx, playerID),
		ContributionBalance:    myContrib,
		PublicBountyDailyLimit: bountyPubLimit,
		ActiveFeud:             activeFeudView,
		LastFeudSummary:        lastFeudSummary,
		NextFeudIssueAtMs:      nextFeudAt,
	}, nil
}

// ── 2. SetGuildWarStance ─────────────────────────────────────────────────────

func (l *GuildLogic) SetGuildWarStance(ctx context.Context, req *pb.SetGuildWarStanceRequest) (*pb.SetGuildWarStanceResponse, error) {
	playerID, _, _, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.SetGuildWarStanceResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	cur, err := l.war.Repo.GetPlayerWarState(ctx, playerID, zoneID, weekKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get player war state: %v", err)
	}
	peace := cur.GetPeaceMode() != 0
	open := cur.GetOpenChallenge() != 0
	if req.SetPeaceMode != nil {
		peace = req.GetSetPeaceMode()
	}
	if req.SetOpenChallenge != nil {
		open = req.GetSetOpenChallenge()
	}
	cd := war.DefaultPeaceCooldownSec
	if rule := l.warRule(); rule != nil && rule.GetPeaceCooldownSec() > 0 {
		cd = rule.GetPeaceCooldownSec()
	}
	updated, err := l.war.Repo.UpsertPlayerStance(ctx, playerID, zoneID, weekKey, peace, open, nowMs, cd)
	if errors.Is(err, data.ErrWarPeaceCooldown) {
		return &pb.SetGuildWarStanceResponse{ErrorMessage: tipErr(constants.ErrWarPeaceCooldown, "")}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update war stance: %v", err)
	}
	return &pb.SetGuildWarStanceResponse{
		ServerTimeMs: nowMs,
		MyStance: &pb.GuildWarStanceView{
			PeaceMode:      updated.GetPeaceMode() != 0,
			PeaceReadyAtMs: updated.GetPeaceReadyAtMs(),
			OpenChallenge:  updated.GetOpenChallenge() != 0,
			ProtectUntilMs: updated.GetProtectUntilMs(),
			MyStreak:       updated.GetStreakCount(),
			MyWeeklyMerit:  updated.GetWeeklyMerit(),
		},
	}, nil
}

// ── 3. RegisterGuildWarGarrison ──────────────────────────────────────────────

func (l *GuildLogic) RegisterGuildWarGarrison(ctx context.Context, req *pb.RegisterGuildWarGarrisonRequest) (*pb.RegisterGuildWarGarrisonResponse, error) {
	playerID, guildID, guild, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.RegisterGuildWarGarrisonResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	rule := l.warRule()
	maxSlots := war.DefaultGarrisonMaxSlots
	cdSec := war.DefaultGarrisonRefreshCD
	if rule != nil {
		if rule.GetGarrisonMaxSlots() > 0 {
			maxSlots = rule.GetGarrisonMaxSlots()
		}
		if rule.GetGarrisonRefreshCooldownSec() > 0 {
			cdSec = rule.GetGarrisonRefreshCooldownSec()
		}
	}

	targetPID := playerID
	withdraw := req.GetAction() == 2
	if req.GetTargetPlayerId() != 0 && req.GetTargetPlayerId() != playerID {
		// 只有帮主/长老能帮别人下阵。
		var myRole uint32
		for _, m := range guild.Members {
			if m.PlayerID == playerID {
				myRole = m.Role
				break
			}
		}
		if constants.Rank(myRole) < constants.RankOfficer {
			return &pb.RegisterGuildWarGarrisonResponse{ErrorMessage: tipErr(constants.ErrRankTooLow, "")}, nil
		}
		targetPID = req.GetTargetPlayerId()
	}

	names := l.resolveNames(ctx, []uint64{targetPID})
	pName := names[targetPID]
	if pName == "" {
		pName = fmt.Sprintf("守旗弟子%d", targetPID)
	}
	// 构造默认守旗快照（待玩家参战或快照刷新时自动携带真实属性，确保 0 秒随时可开打）。
	snapBytes, _ := proto.Marshal(buildGarrisonSnapshot(targetPID, pName, 60, 1))
	rec := &pb.GuildWarGarrisonRecord{
		GuildId:         guildID,
		PlayerId:        targetPID,
		PlayerName:      pName,
		Level:           60,
		SnapshotPayload: snapBytes,
	}
	slots, nextRefresh, err := l.war.Repo.UpsertGarrison(ctx, guildID, targetPID, zoneID, weekKey, withdraw, rec, nowMs, maxSlots, cdSec)
	switch {
	case errors.Is(err, data.ErrWarGarrisonFull):
		return &pb.RegisterGuildWarGarrisonResponse{ErrorMessage: tipErr(constants.ErrWarGarrisonFull, "")}, nil
	case errors.Is(err, data.ErrWarGarrisonCooldown):
		return &pb.RegisterGuildWarGarrisonResponse{ErrorMessage: tipErr(constants.ErrWarGarrisonCooldown, "")}, nil
	case errors.Is(err, data.ErrNotGuildMember):
		return &pb.RegisterGuildWarGarrisonResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "")}, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "upsert garrison: %v", err)
	}
	outSlots := make([]*pb.GuildWarGarrisonSlotView, 0, len(slots))
	for _, s := range slots {
		outSlots = append(outSlots, &pb.GuildWarGarrisonSlotView{
			SlotIndex:    s.GetSlotIndex(),
			PlayerId:     s.GetPlayerId(),
			PlayerName:   s.GetPlayerName(),
			Level:        s.GetLevel(),
			AppearanceId: s.GetAppearanceId(),
			PetName:      s.GetPetName(),
			PetTableId:   s.GetPetTableId(),
			UpdatedMs:    s.GetUpdatedMs(),
		})
	}
	return &pb.RegisterGuildWarGarrisonResponse{
		GarrisonSlots:  outSlots,
		NextRefreshAtMs: nextRefresh,
	}, nil
}

func buildGarrisonSnapshot(playerID uint64, name string, level, classID uint32) *battlepb.BattlePlayerSnapshot {
	if level == 0 {
		level = 50
	}
	if classID == 0 {
		classID = 1
	}
	hp := uint64(3200 + level*85)
	mp := uint64(1200 + level*20)
	return &battlepb.BattlePlayerSnapshot{
		PlayerId:       playerID,
		TeamIndex:      1,
		PlayerName:     name,
		Level:          level,
		ClassId:        classID,
		MaxHealth:      hp,
		MaxMana:        mp,
		PhysicalAttack: uint64(360 + level*14),
		MagicAttack:    uint64(320 + level*12),
		Defense:        uint64(220 + level*8),
		AiControlled:   true,
		BaseAttributes: &componentpb.BaseAttributesComp{
			Health:     hp,
			Mana:       mp,
			Speed:      uint64(85 + level*2),
			Strength:   uint64(120 + level*5),
			Armor:      uint64(100 + level*4),
			Resistance: uint64(90 + level*3),
		},
	}
}

// ── 4. StartGuildWarBattle ───────────────────────────────────────────────────

// clientWarTargetHints 返回该目标种类允许采信的客户端字段(r2-gw#1):只有打守旗阵(LEYLINE_GARRISON,一键开战回落到守旗阵时
// 按它再取一次)采信 leyline_id,只有追杀榜直打(BOUNTY)采信 bounty_id。其余分支一律从 0 起:野外直打按目标所在场景推灵脉,
// 直打活人只由服务端按目标自动挂钩追杀令。这两个号会原样冻结进开战上下文、决定结算时动哪座灵脉的旗、结哪张追杀令,
// 采信客户端值就等于让任意一局帮战去削第三方的旗、领别人的赏金。
func clientWarTargetHints(kind pb.GuildWarTargetKind, req *pb.StartGuildWarBattleRequest) (leylineID uint32, bountyID uint64) {
	switch kind {
	case pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_LEYLINE_GARRISON:
		return req.GetLeylineId(), 0
	case pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_BOUNTY:
		return 0, req.GetBountyId()
	default:
		return 0, 0
	}
}

func (l *GuildLogic) StartGuildWarBattle(ctx context.Context, req *pb.StartGuildWarBattleRequest) (*pb.StartGuildWarBattleResponse, error) {
	playerID, guildID, guild, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	if l.war.Match == nil {
		return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarServiceBusy, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	rule := l.warRule()
	battleConfigID := uint32(1)
	if rule != nil && rule.GetBattleConfigId() != 0 {
		battleConfigID = rule.GetBattleConfigId()
	}
	_ = l.war.Repo.EnsureZoneLeylines(ctx, zoneID, nowMs)

	// 本人处于和平免战态时禁止主动发起帮战。
	myState, err := l.war.Repo.GetPlayerWarState(ctx, playerID, zoneID, weekKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get self war state: %v", err)
	}
	if myState.GetPeaceMode() != 0 {
		return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarSelfPeaceMode, "")}, nil
	}

	// 检查本人是否正处于战斗中。
	if selfBid := l.readBattleLockID(ctx, playerID); selfBid != 0 {
		return &pb.StartGuildWarBattleResponse{
			ErrorMessage:     tipErr(constants.ErrWarTargetInBattle, ""),
			OffenderPlayerId: playerID,
			OffenderBattleId: selfBid,
		}, nil
	}

	kind := req.GetTargetKind()
	targetPID := req.GetTargetPlayerId()
	// 只信服务端推出来的目标信息(r2-gw#1):客户端的 leyline_id / bounty_id 只在各自的分支采信,其余一律从 0 起由服务端按目标推。
	leylineID, bountyID := clientWarTargetHints(kind, req)

	var matchMode matchpb.MatchMode
	var warKind pb.GuildWarBattleKind
	var defenderGuildID uint64
	var defenderPIDs []uint64
	var aiSnapshots []*battlepb.BattlePlayerSnapshot

	// 解析目标与模式。
	switch kind {
	case pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_BOUNTY:
		b, bErr := l.war.Repo.GetBounty(ctx, bountyID)
		if bErr != nil || !war.BountyHuntable(b, zoneID, guildID, nowMs) {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarBountyNotFound, "")}, nil
		}
		targetPID = b.GetTargetPlayerId()
		// 防守帮按目标此刻所在的帮记(发单之后目标可能换过帮),不按单上的旧帮。
		tgid, gErr := l.resolveWarPlayerGuildID(ctx, targetPID)
		if gErr != nil || tgid == 0 {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarBountyInvalidTarget, "")}, nil
		}
		defenderGuildID = tgid
		if defenderGuildID == guildID || targetPID == playerID {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarSameGuild, "")}, nil
		}
		tOnline, tScene, _, _ := l.resolvePlayerScene(ctx, zoneID, targetPID)
		if !tOnline {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetOffline, "")}, nil
		}
		if war.IsSafeZoneScene(tScene) {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarSafeZone, "")}, nil
		}
		tState, _ := l.war.Repo.GetPlayerWarState(ctx, targetPID, zoneID, weekKey)
		if tState != nil && tState.GetPeaceMode() != 0 {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetPeaceMode, "")}, nil
		}
		if tState != nil && nowMs < tState.GetProtectUntilMs() {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetProtected, "")}, nil
		}
		matchMode = matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP
		warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT
		defenderPIDs = []uint64{targetPID}

	case pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_FIELD_PLAYER,
		pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_LIVE_PLAYER:
		if targetPID == 0 || targetPID == playerID {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrCannotTargetSelf, "")}, nil
		}
		tgid, gErr := l.resolveWarPlayerGuildID(ctx, targetPID)
		if gErr != nil || tgid == 0 {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarBountyInvalidTarget, "")}, nil
		}
		if tgid == guildID {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarSameGuild, "")}, nil
		}
		defenderGuildID = tgid
		tState, _ := l.war.Repo.GetPlayerWarState(ctx, targetPID, zoneID, weekKey)
		if tState != nil && tState.GetPeaceMode() != 0 {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetPeaceMode, "")}, nil
		}
		if tState != nil && nowMs < tState.GetProtectUntilMs() {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetProtected, "")}, nil
		}
		if kind == pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_FIELD_PLAYER {
			// 野外路上任意异帮直打：双方必须都在非天墉城安全区的同一场景分线。
			sOnline, sConf, _, sSceneID := l.resolvePlayerScene(ctx, zoneID, playerID)
			tOnline, tConf, _, tSceneID := l.resolvePlayerScene(ctx, zoneID, targetPID)
			if !tOnline {
				return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetOffline, "")}, nil
			}
			if !sOnline || war.IsSafeZoneScene(sConf) || war.IsSafeZoneScene(tConf) {
				return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarSafeZone, "")}, nil
			}
			if sSceneID != 0 && tSceneID != 0 && sSceneID != tSceneID {
				return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarTargetOffline, "")}, nil
			}
			if meta, ok := war.LeylineByScene(tConf); ok {
				leylineID = meta.ID
				warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_LEYLINE_PVP
			} else {
				warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_FIELD_ROAD
			}
		} else {
			warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_PANEL_PVP
		}
		// 目标身上挂着本帮可接的追杀令(同区、全区榜或本帮发的单)时自动挂钩结单;客户端给的 bounty_id 在这里从不采信。
		if activeB, _ := l.war.Repo.ActiveBountyByTarget(ctx, targetPID, nowMs); war.BountyHuntable(activeB, zoneID, guildID, nowMs) {
			bountyID = activeB.GetBountyId()
			warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT
		}
		matchMode = matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP
		defenderPIDs = []uint64{targetPID}

	case pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_QUICK_AUTO:
		// 0 秒一键快速开战：优先找挂牌在线异帮活人，无活人秒打金脉/敌帮守旗阵。
		openPlayers, _ := l.war.Repo.ListOpenChallengePlayers(ctx, zoneID, 20)
		for _, op := range openPlayers {
			if op.GetPlayerId() == playerID || op.GetPeaceMode() != 0 || nowMs < op.GetProtectUntilMs() {
				continue
			}
			if l.readBattleLockID(ctx, op.GetPlayerId()) != 0 {
				continue
			}
			tgid, _ := l.resolveWarPlayerGuildID(ctx, op.GetPlayerId())
			if tgid != 0 && tgid != guildID {
				targetPID = op.GetPlayerId()
				defenderGuildID = tgid
				defenderPIDs = []uint64{targetPID}
				matchMode = matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP
				warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_PANEL_PVP
				break
			}
		}
		if len(defenderPIDs) == 0 {
			// 无在线活人 → 回落到守旗阵:与 LEYLINE_GARRISON 同样采信客户端指定的灵脉(没指定就打当期 1.5x 金脉),下面照常校验。
			kind = pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_LEYLINE_GARRISON
			leylineID, bountyID = clientWarTargetHints(kind, req)
		}
	}

	if kind == pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_LEYLINE_GARRISON {
		if leylineID == 0 {
			leylineID, _ = war.GoldLeylineForHour(nowMs)
		}
		if _, ok := war.LeylineByID(leylineID); !ok {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarLeylineNotFound, "")}, nil
		}
		leylines, lErr := l.war.Repo.ListLeylines(ctx, zoneID)
		if lErr != nil {
			return nil, status.Errorf(codes.Internal, "list leylines: %v", lErr)
		}
		var targetLeyline *pb.GuildWarLeylineRecord
		for _, lr := range leylines {
			if lr.GetLeylineId() == leylineID {
				targetLeyline = lr
				break
			}
		}
		if targetLeyline == nil {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarLeylineNotFound, "")}, nil
		}
		if targetLeyline.GetOwnerGuildId() == guildID {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarSameGuild, "")}, nil
		}
		defenderGuildID = targetLeyline.GetOwnerGuildId()
		if defenderGuildID != 0 {
			slots, _ := l.war.Repo.ListGarrison(ctx, defenderGuildID)
			for _, s := range slots {
				if len(s.GetSnapshotPayload()) > 0 {
					snap := &battlepb.BattlePlayerSnapshot{}
					if proto.Unmarshal(s.GetSnapshotPayload(), snap) == nil && snap.GetPlayerId() != 0 && snap.GetPlayerId() != playerID {
						snap.TeamIndex = 1
						snap.AiControlled = true
						aiSnapshots = append(aiSnapshots, snap)
						if len(aiSnapshots) >= 3 {
							break
						}
					}
				}
			}
		}
		if len(aiSnapshots) == 0 {
			// 无主灵脉或敌帮未登记守旗阵：派出灵脉镇守道兵（保证 365 天 0 秒可开打）。
			meta, _ := war.LeylineByID(leylineID)
			aiSnapshots = append(aiSnapshots, buildGarrisonSnapshot(900_000_000+uint64(leylineID), meta.Name+"·灵脉镇守使", 55, 1))
		}
		matchMode = matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE
		warKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_GARRISON_SIEGE
	}

	// 计算当期金脉与本周宿敌增益。
	goldID, _ := war.GoldLeylineForHour(nowMs)
	isGold := leylineID != 0 && leylineID == goldID
	isRival := false
	if defenderGuildID != 0 && defenderGuildID != guildID {
		if rivals, rErr := l.war.Repo.ListRivals(ctx, guildID, weekKey); rErr == nil {
			for _, rg := range rivals {
				if rg == defenderGuildID {
					isRival = true
					break
				}
			}
		}
	}

	// 开战请求号是 match 判重的持久键:客户端没带就用帮战发号器(与 op_id 同一个)现发,发不出号整体失败,不落墙钟值(r2-gw#4)。
	reqID := req.GetClientRequestId()
	if reqID == 0 {
		if l.war.OpIDs == nil {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
		minted, idErr := l.war.OpIDs.Mint(ctx)
		if idErr != nil || minted == 0 {
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
		reqID = minted
	}
	matchReq := &matchpb.StartGuildWarBattleRequest{
		BattleConfigId:      battleConfigID,
		MatchMode:           uint32(matchMode),
		AttackerPlayerIds:   []uint64{playerID},
		DefenderPlayerIds:   defenderPIDs,
		AiDefenderSnapshots: aiSnapshots,
		RequestId:           reqID,
		ActivityContext: &battlepb.BattleActivityContext{
			Kind:              battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR,
			GuildId:           guildID,
			InitiatorPlayerId: playerID,
			LaunchRequestId:   reqID,
			GuildWar: &battlepb.GuildWarBattleContext{
				WarBattleKind:   uint32(warKind),
				LeylineId:       leylineID,
				BountyId:        bountyID,
				AttackerGuildId: guildID,
				DefenderGuildId: defenderGuildID,
				ZoneId:          zoneID,
				GoldLeylineBonus: isGold,
				RivalryBonus:    isRival,
			},
		},
	}

	callCtx, cancel := context.WithTimeout(ctx, l.war.MatchBudget)
	defer cancel()
	resp, mErr := l.war.Match.StartGuildWarBattle(callCtx, matchReq)
	if mErr != nil {
		logx.WithContext(ctx).Errorf("[guild] StartGuildWarBattle match RPC failed: %v", mErr)
		return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarServiceBusy, "")}, nil
	}
	if resp.GetReject() != matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE {
		switch resp.GetReject() {
		case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE:
			return &pb.StartGuildWarBattleResponse{
				ErrorMessage:     tipErr(constants.ErrWarTargetInBattle, ""),
				OffenderPlayerId: resp.GetOffenderPlayerId(),
				OffenderBattleId: resp.GetOffenderBattleId(),
			}, nil
		case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE:
			return &pb.StartGuildWarBattleResponse{
				ErrorMessage:     tipErr(constants.ErrWarTargetOffline, ""),
				OffenderPlayerId: resp.GetOffenderPlayerId(),
			}, nil
		default:
			return &pb.StartGuildWarBattleResponse{ErrorMessage: tipErr(constants.ErrWarServiceBusy, "")}, nil
		}
	}

	// 若在攻打敌帮灵脉，异步向防守帮会成员推踢馆警报（非阻断）。
	if leylineID != 0 && defenderGuildID != 0 && defenderGuildID != guildID {
		if defGuild, gErr := l.repo.GetGuild(ctx, defenderGuildID); gErr == nil && defGuild != nil {
			recips := make([]uint64, 0, len(defGuild.Members))
			for _, m := range defGuild.Members {
				recips = append(recips, m.PlayerID)
			}
			meta, _ := war.LeylineByID(leylineID)
			l.notifyWarAlert(ctx, &pb.GuildWarAlertS2C{
				Kind:              pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_LEYLINE_UNDER_SIEGE,
				LeylineId:         leylineID,
				SceneConfigId:     meta.SceneConfigID,
				AttackerGuildName: guild.Name,
				DefenderGuildName: defGuild.Name,
				BattleId:          resp.GetBattleId(),
			}, recips)
		}
	}

	return &pb.StartGuildWarBattleResponse{
		BattleId:      resp.GetBattleId(),
		WarBattleKind: warKind,
	}, nil
}

// ── 5. ClaimGuildWarMilestone ────────────────────────────────────────────────

func (l *GuildLogic) ClaimGuildWarMilestone(ctx context.Context, req *pb.ClaimGuildWarMilestoneRequest) (*pb.ClaimGuildWarMilestoneResponse, error) {
	playerID, guildID, _, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	row, ok := table.GuildWarMilestoneTableManagerInstance.FindById(req.GetMilestoneId())
	if !ok || row == nil {
		return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(constants.ErrWarMilestoneLocked, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	minMerit := war.DefaultDividendMinMerit
	if rule := l.warRule(); rule != nil && rule.GetDividendMinPersonalMerit() > 0 {
		minMerit = rule.GetDividendMinPersonalMerit()
	}

	var items []*assetpb.ItemGrant
	if row.GetRewardItem1Id() != 0 {
		items = append(items, &assetpb.ItemGrant{ConfigId: row.GetRewardItem1Id(), Count: row.GetRewardItem1Count()})
	}
	if row.GetRewardItem2Id() != 0 {
		items = append(items, &assetpb.ItemGrant{ConfigId: row.GetRewardItem2Id(), Count: row.GetRewardItem2Count()})
	}
	bundleBytes, err := buildSilverItemBundle(row.GetRewardSilver(), items...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal milestone reward: %v", err)
	}
	var opID uint64
	if len(bundleBytes) > 0 {
		if l.war.OpIDs == nil {
			return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
		opID, err = l.war.OpIDs.Mint(ctx)
		if err != nil {
			return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
	}

	opRow, hasOp, err := l.war.Repo.ClaimMilestone(ctx, data.ClaimMilestoneInput{
		GuildID:          guildID,
		PlayerID:         playerID,
		ZoneID:           zoneID,
		WeekKey:          weekKey,
		MilestoneID:      row.GetId(),
		RequiredMerit:    row.GetRequiredGuildMerit(),
		MinPersonalMerit: minMerit,
		ContributionGain: row.GetRewardContribution(),
		OpID:             opID,
		BundleBytes:      bundleBytes,
		LeaseUntilMs:     nowMs + uint64(l.war.Lease.Milliseconds()),
		LeaseToken:       opID,
		NowMs:            nowMs,
	})
	switch {
	case errors.Is(err, data.ErrWarMilestoneLocked):
		return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(constants.ErrWarMilestoneLocked, "")}, nil
	case errors.Is(err, data.ErrWarMilestoneClaimed):
		return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(constants.ErrWarMilestoneClaimed, "")}, nil
	case errors.Is(err, assetop.ErrTooManyPending):
		return &pb.ClaimGuildWarMilestoneResponse{ErrorMessage: tipErr(constants.ErrAssetPending, "")}, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "claim war milestone: %v", err)
	}
	orderStatus := pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED
	if hasOp {
		orderStatus = l.syncDeliverWarOp(ctx, opRow)
	}
	return &pb.ClaimGuildWarMilestoneResponse{
		AssetOrderStatus: orderStatus,
		Milestone: &pb.GuildWarMilestoneView{
			MilestoneId:        row.GetId(),
			RequiredMerit:      row.GetRequiredGuildMerit(),
			Claimed:            true,
			Claimable:          false,
			RewardPreview:      row.GetRewardPreview(),
			RewardSilver:       row.GetRewardSilver(),
			RewardContribution: row.GetRewardContribution(),
		},
	}, nil
}

// ── 6. ClaimGuildWarDividend ─────────────────────────────────────────────────

func (l *GuildLogic) ClaimGuildWarDividend(ctx context.Context, _ *pb.ClaimGuildWarDividendRequest) (*pb.ClaimGuildWarDividendResponse, error) {
	playerID, guildID, _, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.ClaimGuildWarDividendResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	rule := l.warRule()
	minMerit := war.DefaultDividendMinMerit
	guildCap := war.DefaultDividendGuildWeeklyCap
	playerCap := war.DefaultDividendPlayerWeeklyCap
	intervalSec := war.DefaultDividendIntervalSec
	if rule != nil {
		if rule.GetDividendMinPersonalMerit() > 0 {
			minMerit = rule.GetDividendMinPersonalMerit()
		}
		if rule.GetDividendGuildWeeklyCap() > 0 {
			guildCap = rule.GetDividendGuildWeeklyCap()
		}
		if rule.GetDividendPlayerWeeklyCap() > 0 {
			playerCap = rule.GetDividendPlayerWeeklyCap()
		}
		if rule.GetDividendIntervalSec() > 0 {
			intervalSec = rule.GetDividendIntervalSec()
		}
	}
	var opID uint64
	if l.war.OpIDs != nil {
		opID, err = l.war.OpIDs.Mint(ctx)
		if err != nil {
			return &pb.ClaimGuildWarDividendResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
	}

	res, err := l.war.Repo.ClaimDividend(ctx, data.ClaimDividendInput{
		GuildID:          guildID,
		PlayerID:         playerID,
		ZoneID:           zoneID,
		WeekKey:          weekKey,
		MinPersonalMerit: minMerit,
		GuildWeeklyCap:   guildCap,
		PlayerWeeklyCap:  playerCap,
		IntervalSec:      intervalSec,
		SilverPerShare:   war.DefaultDividendSilverPerShare,
		ContribPerShare:  war.DefaultDividendContribPerShare,
		OpID:             opID,
		BuildBundle:      func(silver uint64) ([]byte, error) { return buildSilverItemBundle(silver) },
		LeaseUntilMs:     nowMs + uint64(l.war.Lease.Milliseconds()),
		LeaseToken:       opID,
		NowMs:            nowMs,
	})
	switch {
	case errors.Is(err, data.ErrWarMilestoneLocked):
		return &pb.ClaimGuildWarDividendResponse{ErrorMessage: tipErr(constants.ErrWarMilestoneLocked, "")}, nil
	case errors.Is(err, data.ErrWarDividendEmpty):
		return &pb.ClaimGuildWarDividendResponse{ErrorMessage: tipErr(constants.ErrWarDividendEmpty, "")}, nil
	case errors.Is(err, data.ErrWriteConflict):
		return &pb.ClaimGuildWarDividendResponse{ErrorMessage: tipErr(constants.ErrBusyRetry, "")}, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "claim war dividend: %v", err)
	}
	orderStatus := pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED
	if res.HasOp {
		orderStatus = l.syncDeliverWarOp(ctx, res.Op)
	}
	return &pb.ClaimGuildWarDividendResponse{
		ClaimedCount:     res.SharesClaimed,
		AssetOrderStatus: orderStatus,
		Dividend: &pb.GuildWarDividendView{
			ClaimableCount:   0,
			WeeklyCap:        playerCap,
			GuildWeeklyCap:   guildCap,
			MinPersonalMerit: minMerit,
		},
	}, nil
}

// ── 7. PostGuildWarBounty ────────────────────────────────────────────────────

func (l *GuildLogic) PostGuildWarBounty(ctx context.Context, req *pb.PostGuildWarBountyRequest) (*pb.PostGuildWarBountyResponse, error) {
	playerID, guildID, _, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	targetPID := req.GetTargetPlayerId()
	if targetPID == 0 || targetPID == playerID {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarBountyInvalidTarget, "")}, nil
	}
	targetGuildID, err := l.resolveWarPlayerGuildID(ctx, targetPID)
	if err != nil || targetGuildID == 0 {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarBountyInvalidTarget, "")}, nil
	}
	if targetGuildID == guildID {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarSameGuild, "")}, nil
	}
	rule := l.warRule()
	spec, ok := war.ResolveBountyTier(rule, req.GetTier())
	if !ok {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarBountyInvalidTarget, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	dayKey := l.warDayKey(now)
	ttlSec := war.DefaultBountyTTLSec
	pubDailyLimit := war.DefaultBountyPublicDailyLimit
	if rule != nil {
		if rule.GetBountyTtlSec() > 0 {
			ttlSec = rule.GetBountyTtlSec()
		}
		if rule.GetBountyPublicDailyLimit() > 0 {
			pubDailyLimit = rule.GetBountyPublicDailyLimit()
		}
	}

	// bounty_id 是发号器永久 ID:发不出号整体失败,不落墙钟值(AGENTS §7 第 1 条,r2-gw#4)。
	if l.war.BountyIDs == nil {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
	}
	bountyID, idErr := l.war.BountyIDs.Mint(ctx)
	if idErr != nil || bountyID == 0 {
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
	}
	var opID uint64
	var debitBundle []byte
	if spec.CostSilver > 0 {
		if l.war.OpIDs == nil {
			return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
		opID, err = l.war.OpIDs.Mint(ctx)
		if err != nil {
			return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
		}
		debitBundle, err = buildSilverItemBundle(spec.CostSilver)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "marshal bounty debit bundle: %v", err)
		}
	}

	rec, opRow, hasOp, err := l.war.Repo.PostBounty(ctx, data.PostBountyInput{
		BountyID:         bountyID,
		ZoneID:           zoneID,
		WeekKey:          weekKey,
		DayKey:           dayKey,
		IssuerPlayerID:   playerID,
		IssuerGuildID:    guildID,
		TargetPlayerID:   targetPID,
		TargetGuildID:    targetGuildID,
		Tier:             req.GetTier(),
		CostSilver:       spec.CostSilver,
		CostGuildFunds:   spec.CostGuildFunds,
		PoolSilver:       spec.PoolSilver,
		PublicBoard:      req.GetPublicBoard(),
		PublicDailyLimit: pubDailyLimit,
		TTLSec:           ttlSec,
		OpID:             opID,
		DebitBundle:      debitBundle,
		LeaseUntilMs:     nowMs + uint64(l.war.Lease.Milliseconds()),
		LeaseToken:       opID,
		NowMs:            nowMs,
	})
	switch {
	case errors.Is(err, data.ErrWarBountyActive):
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarBountyAlreadyActive, "")}, nil
	case errors.Is(err, data.ErrWarBountyLimit):
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarBountyLimit, "")}, nil
	case errors.Is(err, data.ErrFundsInsufficient):
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrFundsInsufficient, "")}, nil
	case errors.Is(err, data.ErrRankTooLow):
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrRankTooLow, "")}, nil
	case errors.Is(err, assetop.ErrTooManyPending):
		return &pb.PostGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrAssetPending, "")}, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "post war bounty: %v", err)
	}
	if hasOp {
		l.syncDeliverWarOp(ctx, opRow)
	}
	return &pb.PostGuildWarBountyResponse{
		Bounty: &pb.GuildWarBountyView{
			BountyId:           rec.GetBountyId(),
			TargetPlayerId:     rec.GetTargetPlayerId(),
			TargetGuildId:      rec.GetTargetGuildId(),
			IssuerGuildId:      rec.GetIssuerGuildId(),
			Tier:               pb.GuildWarBountyTier(rec.GetTier()),
			RewardSilver:       rec.GetEscrowSilver(),
			PublicBoard:        rec.GetPublicBoard() != 0,
			Status:             pb.GuildWarBountyStatus(rec.GetStatus()),
			CounterKillsToTear: war.DefaultBountyCounterKillTear,
			ExpiresAtMs:        rec.GetExpiresAtMs(),
		},
	}, nil
}

// ── 8. TrackGuildWarBounty ───────────────────────────────────────────────────

func (l *GuildLogic) TrackGuildWarBounty(ctx context.Context, req *pb.TrackGuildWarBountyRequest) (*pb.TrackGuildWarBountyResponse, error) {
	playerID, guildID, guild, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.TrackGuildWarBountyResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	b, err := l.war.Repo.GetBounty(ctx, req.GetBountyId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get bounty: %v", err)
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	if !war.BountyHuntable(b, zoneID, guildID, nowMs) {
		return &pb.TrackGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarBountyNotFound, "")}, nil
	}
	cd := war.DefaultTrackCooldownSec
	if rule := l.warRule(); rule != nil && rule.GetTrackCooldownSec() > 0 {
		cd = rule.GetTrackCooldownSec()
	}
	nextTrackAt, err := l.war.Repo.MarkTrackCooldown(ctx, playerID, zoneID, l.warWeekKey(now), nowMs, cd)
	if errors.Is(err, data.ErrWarTrackCooldown) {
		return &pb.TrackGuildWarBountyResponse{ErrorMessage: tipErr(constants.ErrWarTrackCooldown, "")}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mark track cooldown: %v", err)
	}
	online, sceneConf, chNo, sceneID := l.resolvePlayerScene(ctx, zoneID, b.GetTargetPlayerId())
	inSafe := online && war.IsSafeZoneScene(sceneConf)
	if inSafe {
		sceneID = 0
	} else if online {
		// 追兵锁定行踪时，向被追杀目标推送非阻断警报（只告帮名与地图，反杀博弈感拉满）。
		l.notifyWarAlert(ctx, &pb.GuildWarAlertS2C{
			Kind:              pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_PURSUER_ENTERED_MAP,
			SceneConfigId:     sceneConf,
			ChannelNo:         chNo,
			AttackerGuildName: guild.Name,
			BountyId:          b.GetBountyId(),
			RewardSilver:      b.GetEscrowSilver(),
		}, []uint64{b.GetTargetPlayerId()})
	}
	return &pb.TrackGuildWarBountyResponse{
		TargetOnline:  online,
		InSafeZone:    inSafe,
		SceneConfigId: sceneConf,
		ChannelNo:     chNo,
		NextTrackAtMs: nextTrackAt,
		SceneId:       sceneID,
	}, nil
}

// ── 9. GetGuildWarBattleResult ───────────────────────────────────────────────

func (l *GuildLogic) GetGuildWarBattleResult(ctx context.Context, req *pb.GetGuildWarBattleResultRequest) (*pb.GetGuildWarBattleResultResponse, error) {
	playerID, guildID, _, _, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.GetGuildWarBattleResultResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	rec, err := l.war.Repo.GetWarBattle(ctx, req.GetBattleId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get war battle: %v", err)
	}
	if rec == nil {
		return &pb.GetGuildWarBattleResultResponse{Settled: false, BattleId: req.GetBattleId()}, nil
	}
	var sum warBattleSummary
	_ = json.Unmarshal(rec.GetSummaryPayload(), &sum)
	isAttacker := rec.GetAttackerGuildId() == guildID
	for _, id := range sum.DefenderIDs {
		if id == playerID {
			isAttacker = false
			break
		}
	}
	contrib, silver, funds, merit := rec.GetAttackerContribution(), rec.GetAttackerSilver(), rec.GetAttackerFunds(), rec.GetAttackerMerit()
	if !isAttacker {
		contrib, silver, funds, merit = rec.GetDefenderContribution(), rec.GetDefenderSilver(), rec.GetDefenderFunds(), rec.GetDefenderMerit()
	}
	lostToEnemy := (isAttacker && rec.GetOutcome() == 2) || (!isAttacker && rec.GetOutcome() == 1)
	var candidateID uint64
	var candidateName string
	if lostToEnemy {
		if isAttacker && len(sum.DefenderIDs) > 0 {
			candidateID = sum.DefenderIDs[0]
			if len(sum.DefenderNames) > 0 {
				candidateName = sum.DefenderNames[0]
			}
		} else if !isAttacker && len(sum.AttackerIDs) > 0 {
			candidateID = sum.AttackerIDs[0]
			if len(sum.AttackerNames) > 0 {
				candidateName = sum.AttackerNames[0]
			}
		}
	}
	return &pb.GetGuildWarBattleResultResponse{
		Settled:                   true,
		BattleId:                  rec.GetBattleId(),
		WarBattleKind:             pb.GuildWarBattleKind(rec.GetWarBattleKind()),
		Outcome:                   rec.GetOutcome(),
		Rounds:                    rec.GetTotalRounds(),
		ContributionGain:          contrib,
		SilverGain:                silver,
		GuildFundsGain:            funds,
		MeritGain:                 merit,
		BountyJackpotContribution: rec.GetJackpotContribution(),
		BountyJackpotSilver:       rec.GetJackpotSilver(),
		BountyOutcomeTag:          rec.GetBountyOutcomeTag(),
		DiminishPercent:           rec.GetDiminishPercent(),
		LeylineId:                 rec.GetLeylineId(),
		BannerHpBefore:            rec.GetBannerHpBefore(),
		BannerHpAfter:             rec.GetBannerHpAfter(),
		BannerShielded:            rec.GetBannerShielded() != 0,
		CanPostBounty:             lostToEnemy && candidateID != 0 && rec.GetDefenderGuildId() != 0,
		BountyCandidatePlayerId:   candidateID,
		BountyCandidateName:       candidateName,
		GoldLeylineBonus:          sum.GoldLeyline,
		RivalryBonus:              sum.RivalryBonus,
		FeudBonus:                 sum.FeudBonus,
	}, nil
}

// ── 10. InspectFieldWarTargets（野外三图点选任意异帮玩家） ────────────────────

func (l *GuildLogic) InspectFieldWarTargets(ctx context.Context, req *pb.InspectFieldWarTargetsRequest) (*pb.InspectFieldWarTargetsResponse, error) {
	playerID, guildID, _, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.InspectFieldWarTargetsResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	weekKey := l.warWeekKey(now)
	myState, _ := l.war.Repo.GetPlayerWarState(ctx, playerID, zoneID, weekKey)
	rivals, _ := l.war.Repo.ListRivals(ctx, guildID, weekKey)
	rivalSet := map[uint64]bool{}
	for _, rg := range rivals {
		rivalSet[rg] = true
	}

	rawIDs := req.GetPlayerIds()
	if len(rawIDs) > 30 {
		rawIDs = rawIDs[:30]
	}
	names := l.resolveNames(ctx, rawIDs)
	out := make([]*pb.FieldWarTargetBrief, 0, len(rawIDs))
	for _, pid := range rawIDs {
		if pid == 0 || pid == playerID {
			continue
		}
		tgid, _ := l.resolveWarPlayerGuildID(ctx, pid)
		gName := ""
		if tgid != 0 {
			if g, gErr := l.repo.GetGuild(ctx, tgid); gErr == nil && g != nil {
				gName = g.Name
			}
		}
		st, _ := l.war.Repo.GetPlayerWarState(ctx, pid, zoneID, weekKey)
		peace := st != nil && st.GetPeaceMode() != 0
		protectUntil := uint64(0)
		streak := uint32(0)
		if st != nil {
			protectUntil = st.GetProtectUntilMs()
			streak = st.GetStreakCount()
		}
		bid := l.readBattleLockID(ctx, pid)
		// 只露查看者有资格接的单(r2-gw#8):仅发单帮可见的追杀令,别帮看到的号、赏金与档位一律为空。
		activeB, _ := l.war.Repo.ActiveBountyByTarget(ctx, pid, nowMs)
		bID, bSilver, bTier := fieldTargetBounty(activeB, zoneID, guildID, nowMs)
		canAttack := tgid != 0 && tgid != guildID && !peace && nowMs >= protectUntil && bid == 0 && (myState == nil || myState.GetPeaceMode() == 0)
		out = append(out, &pb.FieldWarTargetBrief{
			PlayerId:                 pid,
			PlayerName:               names[pid],
			GuildId:                  tgid,
			GuildName:                gName,
			PeaceMode:                peace,
			ProtectUntilMs:           protectUntil,
			Streak:                   streak,
			InBattle:                 bid != 0,
			BattleId:                 bid,
			IsRivalGuild:             rivalSet[tgid],
			ActiveBountyId:           bID,
			ActiveBountyRewardSilver: bSilver,
			ActiveBountyTier:         bTier,
			CanAttackNow:             canAttack,
		})
	}
	return &pb.InspectFieldWarTargetsResponse{Targets: out}, nil
}

// ── 11. IssueGuildWarFeud & RespondGuildWarFeud（2 小时宿敌约战战书） ──────────

func toFeudView(rec *pb.GuildWarFeudRecord) *pb.GuildWarFeudView {
	if rec == nil {
		return nil
	}
	return &pb.GuildWarFeudView{
		FeudId:           rec.GetFeudId(),
		IssuerGuildId:    rec.GetIssuerGuildId(),
		IssuerGuildName:  rec.GetIssuerGuildName(),
		TargetGuildId:    rec.GetTargetGuildId(),
		TargetGuildName:  rec.GetTargetGuildName(),
		Status:           pb.GuildWarFeudStatus(rec.GetStatus()),
		IssuerWins:       rec.GetIssuerWins(),
		TargetWins:       rec.GetTargetWins(),
		IssuerMerit:      rec.GetIssuerMerit(),
		TargetMerit:      rec.GetTargetMerit(),
		CreatedAtMs:      rec.GetCreatedAtMs(),
		AcceptDeadlineMs: rec.GetAcceptDeadlineMs(),
		StartedAtMs:      rec.GetStartedAtMs(),
		EndsAtMs:         rec.GetEndsAtMs(),
		WinnerGuildId:    rec.GetWinnerGuildId(),
	}
}

func (l *GuildLogic) guildMemberIDs(ctx context.Context, guildID uint64) []uint64 {
	if guildID == 0 || l.repo == nil {
		return nil
	}
	g, err := l.repo.GetGuild(ctx, guildID)
	if err != nil || g == nil {
		return nil
	}
	ids := make([]uint64, 0, len(g.Members))
	for _, m := range g.Members {
		if m.PlayerID != 0 {
			ids = append(ids, m.PlayerID)
		}
	}
	return ids
}

func (l *GuildLogic) broadcastFeudAlert(ctx context.Context, kind pb.GuildWarAlertKind, rec *pb.GuildWarFeudRecord) {
	if rec == nil {
		return
	}
	recips := l.guildMemberIDs(ctx, rec.GetTargetGuildId())
	if kind != pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_FEUD_CHALLENGE {
		recips = append(recips, l.guildMemberIDs(ctx, rec.GetIssuerGuildId())...)
	}
	if len(recips) == 0 {
		return
	}
	l.notifyWarAlert(ctx, &pb.GuildWarAlertS2C{
		Kind:              kind,
		AttackerGuildName: rec.GetIssuerGuildName(),
		DefenderGuildName: rec.GetTargetGuildName(),
		FeudId:            rec.GetFeudId(),
		IssuerWins:        rec.GetIssuerWins(),
		TargetWins:        rec.GetTargetWins(),
	}, recips)
}

func (l *GuildLogic) IssueGuildWarFeud(ctx context.Context, req *pb.IssueGuildWarFeudRequest) (*pb.IssueGuildWarFeudResponse, error) {
	playerID, guildID, guild, zoneID, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	if !isGuildOfficerOrLeader(guild, playerID) {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrRankTooLow, "")}, nil
	}
	targetGuildID := req.GetTargetGuildId()
	if targetGuildID == 0 || targetGuildID == guildID {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrWarFeudInvalidTarget, "")}, nil
	}
	targetGuild, err := l.repo.GetGuild(ctx, targetGuildID)
	if err != nil || targetGuild == nil {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrWarFeudInvalidTarget, "")}, nil
	}
	nowMs := uint64(l.warNow().UnixMilli())
	// feud_id 是发号器永久 ID:发不出号整体失败,不拿毫秒时间戳顶替(r2-gw#4)。
	if l.war.BountyIDs == nil {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
	}
	feudID, idErr := l.war.BountyIDs.Mint(ctx)
	if idErr != nil || feudID == 0 {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrIDGenUnavailable, "")}, nil
	}
	rec := &pb.GuildWarFeudRecord{
		FeudId:           feudID,
		ZoneId:           zoneID,
		IssuerGuildId:    guildID,
		IssuerGuildName:  guild.Name,
		TargetGuildId:    targetGuildID,
		TargetGuildName:  targetGuild.Name,
		CreatedAtMs:      nowMs,
		AcceptDeadlineMs: nowMs + uint64(war.DefaultFeudAcceptWindowSec)*1000,
	}
	created, err := l.war.Repo.IssueFeud(ctx, rec, war.DefaultFeudCooldownSec, nowMs)
	if errors.Is(err, data.ErrWarFeudAlreadyActive) {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrWarFeudAlreadyActive, "")}, nil
	}
	if errors.Is(err, data.ErrWarFeudCooldown) {
		return &pb.IssueGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrWarFeudCooldown, "")}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue guild war feud: %v", err)
	}
	l.broadcastFeudAlert(ctx, pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_FEUD_CHALLENGE, created)
	return &pb.IssueGuildWarFeudResponse{
		Feud:          toFeudView(created),
		NextIssueAtMs: nowMs + uint64(war.DefaultFeudCooldownSec)*1000,
	}, nil
}

func (l *GuildLogic) RespondGuildWarFeud(ctx context.Context, req *pb.RespondGuildWarFeudRequest) (*pb.RespondGuildWarFeudResponse, error) {
	playerID, guildID, guild, _, tipID, err := l.requireWarMember(ctx)
	if err != nil {
		return nil, err
	}
	if tipID != 0 {
		return &pb.RespondGuildWarFeudResponse{ErrorMessage: tipErr(tipID, "")}, nil
	}
	if !isGuildOfficerOrLeader(guild, playerID) {
		return &pb.RespondGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrRankTooLow, "")}, nil
	}
	nowMs := uint64(l.warNow().UnixMilli())
	updated, err := l.war.Repo.RespondFeud(ctx, req.GetFeudId(), guildID, req.GetAccept(), war.DefaultFeudDurationSec, nowMs)
	if errors.Is(err, data.ErrWarFeudNotFound) {
		return &pb.RespondGuildWarFeudResponse{ErrorMessage: tipErr(constants.ErrWarFeudNotFound, "")}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "respond guild war feud: %v", err)
	}
	if req.GetAccept() && updated.GetStatus() == uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_ACTIVE) {
		l.broadcastFeudAlert(ctx, pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_FEUD_STARTED, updated)
	}
	return &pb.RespondGuildWarFeudResponse{Feud: toFeudView(updated)}, nil
}

func isGuildOfficerOrLeader(g *data.GuildData, playerID uint64) bool {
	if g == nil || playerID == 0 {
		return false
	}
	if g.LeaderID == playerID {
		return true
	}
	for _, m := range g.Members {
		if m.PlayerID == playerID {
			return constants.Rank(m.Role) >= constants.RankOfficer
		}
	}
	return false
}

// ── Kafka 帮战终局结算入口（SettleGuildWarResult） ────────────────────────────

// warSettleStaleAttempts:结算基准(灵脉快照、追杀令普通读、领赏计数)被并发结算改掉时进程内整笔重读重算的次数上限。
// 同一座灵脉上同时收官的局数有限,3 次之内基本都能落定;仍冲突就把错误交给 Kafka 重投(带退避),不在这里无限转。
const warSettleStaleAttempts = 3

// SettleGuildWarResult 结算一局帮战(match-results 的 GUILD_WAR 对局,至少一次投递,由 guild_war_battle 幂等闸门兜住重复)。
// key 是 battle_id,同一座灵脉的几局会落在不同分区、被不同副本并发结算:事务在行锁内发现基准已变就回 ErrWarSettleStale,
// 这里从头重读重算(r2-gw#3)。返回 error = 什么都没提交,消费者退避重投。
func (l *GuildLogic) SettleGuildWarResult(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
	var err error
	for attempt := 1; attempt <= warSettleStaleAttempts; attempt++ {
		if err = l.settleGuildWarResultOnce(ctx, ev); !errors.Is(err, data.ErrWarSettleStale) {
			return err
		}
		logx.WithContext(ctx).Infof("[GuildWar] settlement basis of battle %d changed concurrently, re-reading and recomputing (attempt %d/%d)",
			ev.GetBattleId(), attempt, warSettleStaleAttempts)
	}
	return err
}

func (l *GuildLogic) settleGuildWarResultOnce(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
	if l.war == nil || l.war.Repo == nil || ev == nil {
		return nil
	}
	actx := ev.GetActivityContext()
	if actx == nil || actx.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR || actx.GetGuildWar() == nil {
		return nil
	}
	gw := actx.GetGuildWar()
	battleID := ev.GetBattleId()
	if battleID == 0 || gw.GetAttackerGuildId() == 0 {
		return nil
	}
	if existing, err := l.war.Repo.GetWarBattle(ctx, battleID); err != nil {
		return err
	} else if existing != nil {
		return nil // 已幂等结算
	}

	now := l.warNow()
	nowMs := uint64(now.UnixMilli())
	if ev.GetFinishedAtMs() > 0 {
		nowMs = ev.GetFinishedAtMs()
	}
	weekKey := l.warWeekKey(time.UnixMilli(int64(nowMs)))
	dayKey := l.warDayKey(time.UnixMilli(int64(nowMs)))
	rule := l.warRule()

	// 收集双方真人成员。
	var attackerIDs, defenderIDs []uint64
	for _, t := range ev.GetTeams() {
		if t.GetTeamIndex() == 0 {
			attackerIDs = append(attackerIDs, t.GetPlayerIds()...)
		} else if t.GetTeamIndex() == 1 {
			defenderIDs = append(defenderIDs, t.GetPlayerIds()...)
		}
	}
	attackerWon := ev.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN
	defenderWon := ev.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN
	isDraw := !attackerWon && !defenderWon

	// 检查两帮是否正处于 2 小时宿敌约战窗口内（前 3 场免递减 + 收益 +25% + 计入比分）。
	activeFeud, _ := l.war.Repo.GetActiveFeudBetween(ctx, gw.GetAttackerGuildId(), gw.GetDefenderGuildId(), nowMs)
	feudActive := activeFeud != nil

	// 30 分钟内同对手递减计数（基于最近帮战记录）。
	diminishPct := uint32(100)
	repeatOpp := false
	if len(attackerIDs) > 0 && len(defenderIDs) > 0 {
		recentCount := uint32(0)
		winMs := uint64(war.DefaultDiminishWindowSec) * 1000
		if rule != nil && rule.GetDiminishWindowSec() > 0 {
			winMs = uint64(rule.GetDiminishWindowSec()) * 1000
		}
		if logs, lErr := l.war.Repo.ListRecentBattles(ctx, gw.GetAttackerGuildId(), 20); lErr == nil {
			for _, lb := range logs {
				if nowMs > lb.GetFinishedAtMs()+winMs {
					continue
				}
				var s warBattleSummary
				if json.Unmarshal(lb.GetSummaryPayload(), &s) == nil &&
					((containsU64(s.AttackerIDs, attackerIDs[0]) && containsU64(s.DefenderIDs, defenderIDs[0])) ||
						(containsU64(s.AttackerIDs, defenderIDs[0]) && containsU64(s.DefenderIDs, attackerIDs[0]))) {
					recentCount++
				}
			}
		}
		diminishPct = war.DiminishPercentWithFeud(recentCount, feudActive)
		repeatOpp = recentCount > 0
	}

	goldBps := war.DefaultGoldLeylineBps
	rivalBps := war.DefaultRivalryBonusBps
	if rule != nil {
		if rule.GetGoldLeylineMultiplierBps() > 0 {
			goldBps = rule.GetGoldLeylineMultiplierBps()
		}
		if rule.GetRivalryBonusBps() > 0 {
			rivalBps = rule.GetRivalryBonusBps()
		}
	}

	warKind := pb.GuildWarBattleKind(gw.GetWarBattleKind())
	atkReward := war.ComputeSideReward(war.BattleRewardInput{
		BattleKind:      warKind,
		Won:             attackerWon,
		Draw:            isDraw,
		DiminishPercent: diminishPct,
		IsGoldLeyline:   gw.GetGoldLeylineBonus(),
		IsRivalGuild:    gw.GetRivalryBonus(),
		IsFeudActive:    feudActive,
		GoldLeylineBps:  goldBps,
		RivalryBonusBps: rivalBps,
		Rule:            rule,
	})
	defReward := war.ComputeSideReward(war.BattleRewardInput{
		BattleKind:      warKind,
		Won:             defenderWon,
		Draw:            isDraw,
		DiminishPercent: diminishPct,
		IsGoldLeyline:   gw.GetGoldLeylineBonus(),
		IsRivalGuild:    gw.GetRivalryBonus(),
		IsFeudActive:    feudActive,
		GoldLeylineBps:  goldBps,
		RivalryBonusBps: rivalBps,
		Rule:            rule,
	})

	// 连胜终结赏金检查（输方连胜 >= 3 且击败 >= 2 不同对手时发给赢方首人），同时记录败者连胜供捷报播报。
	var jackpotContrib, jackpotSilver uint64
	var outcomeTag uint32
	var endedLoserStreak uint32
	if !isDraw && len(attackerIDs) > 0 && len(defenderIDs) > 0 {
		loserID := defenderIDs[0]
		if defenderWon {
			loserID = attackerIDs[0]
		}
		if loserState, sErr := l.war.Repo.GetPlayerWarState(ctx, loserID, gw.GetZoneId(), weekKey); sErr == nil && loserState != nil {
			endedLoserStreak = loserState.GetStreakCount()
			jc, js := war.StreakBountyJackpot(loserState.GetStreakCount(), loserState.GetStreakDistinctCount())
			if jc > 0 || js > 0 {
				jackpotContrib, jackpotSilver = jc, js
				outcomeTag = 1 // 终结连胜
			}
		}
	}

	// 灵脉旗势变化与每帮限占 1 座判定。事务外读快照照旧,算出的 BannerDelta 自带这份快照,结算事务在行锁内逐项比对(r2-gw#3)。
	// 旗势只在本局防守帮就是灵脉此刻的主人时才变(无主灵脉 = 打灵脉守卫,防守帮为 0):两个外帮在别人的灵脉上打架、
	// 或灵脉在开战之后已易主,都不动第三方的旗(r2-gw#1)。
	var bannerDelta *war.BannerDelta
	if gw.GetLeylineId() > 0 {
		_ = l.war.Repo.EnsureZoneLeylines(ctx, gw.GetZoneId(), nowMs)
		leylines, lErr := l.war.Repo.ListLeylines(ctx, gw.GetZoneId())
		if lErr != nil {
			return lErr // 读不到快照就算不了旗势:不提交,交给重投,不能悄悄跳过这一局的旗势变化
		}
		cur, ownsOther := war.FindLeylineBasis(leylines, gw.GetLeylineId(), gw.GetAttackerGuildId())
		if cur != nil && cur.GetOwnerGuildId() == gw.GetDefenderGuildId() {
			d := war.ApplyBannerImpact(
				rule, nowMs,
				cur.GetOwnerGuildId(), cur.GetBannerHp(), cur.GetShieldUntilMs(),
				gw.GetAttackerGuildId(), ownsOther,
				attackerWon, warKind != pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_GARRISON_SIEGE,
				gw.GetGoldLeylineBonus(),
			)
			bannerDelta = &d
			if d.BrokeBanner {
				outcomeTag = 5 // 破旗首功
				jackpotSilver += d.BreakBonusSilver
			}
		}
	}

	// 构造参战人员入账列表。
	credits := make([]data.PlayerSettlementCredit, 0, len(attackerIDs)+len(defenderIDs))
	appendSide := func(ids []uint64, gid uint64, teamIdx uint32, won bool, r war.SideReward, oppID uint64) error {
		for idx, pid := range ids {
			silver := r.Silver
			contrib := r.Contribution
			if won && idx == 0 {
				contrib += jackpotContrib
				silver += jackpotSilver
			}
			var opID uint64
			var bundleBytes []byte
			if silver > 0 && l.war.OpIDs != nil {
				var err error
				opID, err = l.war.OpIDs.Mint(ctx)
				if err != nil {
					return err
				}
				bundleBytes, err = buildSilverItemBundle(silver)
				if err != nil {
					return err
				}
			}
			credits = append(credits, data.PlayerSettlementCredit{
				PlayerID:         pid,
				GuildID:          gid,
				TeamIndex:        teamIdx,
				Won:              won,
				Draw:             isDraw,
				ContributionGain: contrib,
				SilverGain:       silver,
				MeritGain:        r.Merit,
				OpID:             opID,
				CreditBundle:     bundleBytes,
				TxType:           rollbackpb.TransactionType_TX_GUILD_WAR_REWARD,
				OpKind:           pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD,
				OpponentID:       oppID,
				RepeatOpponent:   repeatOpp,
			})
		}
		return nil
	}
	var firstDef, firstAtk uint64
	if len(defenderIDs) > 0 {
		firstDef = defenderIDs[0]
	}
	if len(attackerIDs) > 0 {
		firstAtk = attackerIDs[0]
	}
	if err := appendSide(attackerIDs, gw.GetAttackerGuildId(), 0, attackerWon, atkReward, firstDef); err != nil {
		return err
	}
	if err := appendSide(defenderIDs, gw.GetDefenderGuildId(), 1, defenderWon, defReward, firstAtk); err != nil {
		return err
	}

	// 构造大事记快照。
	allNames := l.resolveNames(ctx, append(append([]uint64(nil), attackerIDs...), defenderIDs...))
	sum := warBattleSummary{
		AttackerIDs:  attackerIDs,
		DefenderIDs:  defenderIDs,
		GoldLeyline:  gw.GetGoldLeylineBonus(),
		RivalryBonus: gw.GetRivalryBonus(),
		FeudBonus:    feudActive,
	}
	if g, _ := l.repo.GetGuild(ctx, gw.GetAttackerGuildId()); g != nil {
		sum.AttackerGuildName = g.Name
	}
	if gw.GetDefenderGuildId() != 0 {
		if g, _ := l.repo.GetGuild(ctx, gw.GetDefenderGuildId()); g != nil {
			sum.DefenderGuildName = g.Name
		}
	} else {
		sum.DefenderGuildName = "灵脉守卫"
	}
	for _, pid := range attackerIDs {
		if n := allNames[pid]; n != "" {
			sum.AttackerNames = append(sum.AttackerNames, n)
		} else {
			sum.AttackerNames = append(sum.AttackerNames, fmt.Sprintf("道友%d", pid))
		}
	}
	for _, pid := range defenderIDs {
		if n := allNames[pid]; n != "" {
			sum.DefenderNames = append(sum.DefenderNames, n)
		} else {
			sum.DefenderNames = append(sum.DefenderNames, fmt.Sprintf("道友%d", pid))
		}
	}
	sumBytes, _ := json.Marshal(sum)

	var hpBefore, hpAfter, shielded uint32
	var atkFunds = atkReward.GuildFunds
	if bannerDelta != nil {
		hpBefore = bannerDelta.HPBefore
		hpAfter = bannerDelta.HPAfter
		if nowMs < bannerDelta.ShieldUntilAfter && !bannerDelta.BrokeBanner {
			shielded = 1
		}
		if bannerDelta.BrokeBanner {
			atkFunds += bannerDelta.BreakBonusFunds
		}
	}

	protectSec := war.DefaultPostBattleProtectSec
	peaceCD := war.DefaultPeaceCooldownSec
	bountyProtect := war.DefaultBountyProtectSec
	counterTear := war.DefaultBountyCounterKillTear
	dailyPayLimit := war.DefaultBountyDailyPayoutLimit
	if rule != nil {
		if rule.GetPostBattleProtectSec() > 0 {
			protectSec = rule.GetPostBattleProtectSec()
		}
		if rule.GetPeaceCooldownSec() > 0 {
			peaceCD = rule.GetPeaceCooldownSec()
		}
		if rule.GetBountyProtectSec() > 0 {
			bountyProtect = rule.GetBountyProtectSec()
		}
		if rule.GetBountyCounterKillTear() > 0 {
			counterTear = rule.GetBountyCounterKillTear()
		}
		if rule.GetBountyDailyPayoutLimit() > 0 {
			dailyPayLimit = rule.GetBountyDailyPayoutLimit()
		}
	}

	rec := &pb.GuildWarBattleRecord{
		BattleId:             battleID,
		ZoneId:               gw.GetZoneId(),
		WarBattleKind:        gw.GetWarBattleKind(),
		LeylineId:            gw.GetLeylineId(),
		BountyId:             gw.GetBountyId(),
		AttackerGuildId:      gw.GetAttackerGuildId(),
		DefenderGuildId:      gw.GetDefenderGuildId(),
		Outcome:              uint32(ev.GetOutcome()),
		TotalRounds:          ev.GetTotalRounds(),
		BannerHpBefore:       hpBefore,
		BannerHpAfter:        hpAfter,
		BannerShielded:       shielded,
		BountyOutcomeTag:     outcomeTag,
		JackpotContribution:  jackpotContrib,
		JackpotSilver:        jackpotSilver,
		DiminishPercent:      diminishPct,
		AttackerContribution: atkReward.Contribution,
		AttackerSilver:       atkReward.Silver,
		AttackerFunds:        atkFunds,
		AttackerMerit:        atkReward.Merit,
		DefenderContribution: defReward.Contribution,
		DefenderSilver:       defReward.Silver,
		DefenderFunds:        defReward.GuildFunds,
		DefenderMerit:        defReward.Merit,
		SummaryPayload:       sumBytes,
		FinishedAtMs:         nowMs,
	}

	settleIn := data.SettleWarBattleInput{
		Record:             rec,
		WeekKey:            weekKey,
		DayKey:             dayKey,
		NowMs:              nowMs,
		ProtectSec:         protectSec,
		PeaceCooldownSec:   peaceCD,
		BountyProtectSec:   bountyProtect,
		DailyPayoutLimit:   dailyPayLimit,
		AttackerGuildFunds: atkFunds,
		DefenderGuildFunds: defReward.GuildFunds,
		AttackerMeritAdd:   atkReward.Merit * uint64(len(attackerIDs)),
		DefenderMeritAdd:   defReward.Merit * uint64(len(defenderIDs)),
		BannerDelta:        bannerDelta,
		PlayerCredits:      credits,
		LeaseUntilMs:       nowMs + uint64(l.war.Lease.Milliseconds()),
		LeaseToken:         battleID,
	}
	// 追杀令(r2-gw#1 / #2):只有开战时服务端挂钩的追杀局才交给第 7 步;去向在事务里按锁内的行判定,赏金与退款指令同事务插入。
	// 指令号事务前预发两枚(领赏 / 撕单退发布者),用不上的丢弃;发号失败整笔返回错误交给重投,不能先结单后补发。
	if warKind == pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT && gw.GetBountyId() != 0 {
		if l.war.OpIDs == nil {
			// 资产通道没装配:发不出赏金也退不了款,追杀令原样保留(到期收尾同样等通道恢复),本局其余结算照常。
			logx.WithContext(ctx).Errorf("[GuildWar] battle %d hooks bounty %d but the asset op id minter is not wired; bounty left untouched",
				battleID, gw.GetBountyId())
		} else {
			rewardOpID, mErr := l.war.OpIDs.Mint(ctx)
			if mErr != nil {
				return mErr
			}
			refundOpID, mErr := l.war.OpIDs.Mint(ctx)
			if mErr != nil {
				return mErr
			}
			settleIn.BountyID = gw.GetBountyId()
			settleIn.BountyFacts = war.BountyFacts{
				WarBattleKind:   warKind,
				ZoneID:          gw.GetZoneId(),
				HunterGuildID:   gw.GetAttackerGuildId(),
				HunterID:        firstAtk,
				TargetID:        firstDef,
				HunterWon:       attackerWon,
				TargetWon:       defenderWon,
				CounterKillTear: counterTear,
				NowMs:           nowMs,
			}
			settleIn.BountyRewardOpID = rewardOpID
			settleIn.BountyRefundOpID = refundOpID
			settleIn.BuildSilverBundle = func(silver uint64) ([]byte, error) { return buildSilverItemBundle(silver) }
		}
	}
	settleOut, err := l.war.Repo.SettleWarBattle(ctx, settleIn)
	if errors.Is(err, data.ErrWarBattleAlreadyDone) {
		return nil
	}
	if err != nil {
		return err
	}
	switch settleOut.BountyHeldReason {
	case data.BountyHeldDailyLimit:
		logx.WithContext(ctx).Infof("[GuildWar] bounty %d kept active: reward recipient %d reached the daily bounty payout limit, battle %d settled without moving the escrow",
			gw.GetBountyId(), settleOut.BountyHeldRecipientID, battleID)
	case data.BountyHeldRecipientRemoved:
		logx.WithContext(ctx).Infof("[GuildWar] bounty %d kept active: reward recipient %d has been removed, battle %d settled without moving the escrow",
			gw.GetBountyId(), settleOut.BountyHeldRecipientID, battleID)
	}
	// 入账分流(r2-gw#5 / #6):一个人收不了只影响他自己那一笔,本局其余照常落定。player_id 只进日志,不进指标 label。
	for _, owed := range settleOut.OwedCredits {
		logx.WithContext(ctx).Infof("[GuildWar] battle %d: credit op %d of player %d deferred to the owed queue (credit window full or role fence frozen)",
			battleID, owed.OpID, owed.PlayerID)
	}
	if len(settleOut.FencedPlayers) > 0 {
		logx.WithContext(ctx).Infof("[GuildWar] battle %d: role fence frozen for players %v, their asset credits wait in the owed queue", battleID, settleOut.FencedPlayers)
	}
	if len(settleOut.RemovedPlayers) > 0 {
		logx.WithContext(ctx).Errorf("[GuildWar] battle %d: players %v have been removed, nothing of this battle is written for them", battleID, settleOut.RemovedPlayers)
	}
	if settleOut.BountyIssuerGuildGone {
		logx.WithContext(ctx).Errorf("[GuildWar] bounty %d torn but its issuer guild no longer exists; the issuer half of the public-funds stake is not refunded (battle %d)",
			gw.GetBountyId(), battleID)
	}

	for _, op := range settleOut.CreatedOps {
		l.syncDeliverWarOp(ctx, op)
	}

	// 计入 2 小时宿敌约战比分（到点自动收官并向两帮播报战果）。
	if feudActive {
		if updatedFeud, justFinished, fErr := l.war.Repo.RecordFeudBattle(ctx, activeFeud.GetFeudId(),
			gw.GetAttackerGuildId(), gw.GetDefenderGuildId(), attackerWon, defenderWon,
			atkReward.Merit*uint64(len(attackerIDs)), defReward.Merit*uint64(len(defenderIDs)), nowMs); fErr == nil && justFinished {
			l.broadcastFeudAlert(ctx, pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_FEUD_ENDED, updatedFeud)
		}
	}

	// 达成 5/8 连胜或终结 >=5 连胜时推送非阻断战区/双帮连胜捷报（含 battle_id 供回味与悬赏追踪）。
	if !isDraw && len(attackerIDs) > 0 && len(defenderIDs) > 0 {
		winnerID, loserID := firstAtk, firstDef
		winnerGuildName, loserGuildName := sum.AttackerGuildName, sum.DefenderGuildName
		if defenderWon {
			winnerID, loserID = firstDef, firstAtk
			winnerGuildName, loserGuildName = sum.DefenderGuildName, sum.AttackerGuildName
		}
		var newStreak uint32
		if ws, sErr := l.war.Repo.GetPlayerWarState(ctx, winnerID, gw.GetZoneId(), weekKey); sErr == nil && ws != nil {
			newStreak = ws.GetStreakCount()
		}
		if war.ShouldBroadcastStreakAlert(newStreak, endedLoserStreak) {
			streakVal := newStreak
			if endedLoserStreak >= war.StreakBroadcastMin && jackpotSilver > 0 {
				streakVal = endedLoserStreak
			}
			alertRecips := append(l.guildMemberIDs(ctx, gw.GetAttackerGuildId()), l.guildMemberIDs(ctx, gw.GetDefenderGuildId())...)
			l.notifyWarAlert(ctx, &pb.GuildWarAlertS2C{
				Kind:              pb.GuildWarAlertKind_GUILD_WAR_ALERT_KIND_STREAK_BOUNTY,
				AttackerGuildName: winnerGuildName,
				DefenderGuildName: loserGuildName,
				ActorName:         allNames[winnerID],
				TargetName:        allNames[loserID],
				BattleId:          battleID,
				RewardSilver:      jackpotSilver,
				StreakCount:       streakVal,
			}, alertRecips)
		}
	}

	// 推帮会变动刷新信号给涉及参战人。
	recipients := append(append([]uint64(nil), attackerIDs...), defenderIDs...)
	l.notifier.Notify(ctx, &pb.GuildChangedS2C{
		Kind:    pb.GuildChangeKind_GUILD_CHANGE_KIND_WAR_UPDATED,
		GuildId: gw.GetAttackerGuildId(),
	}, recipients)

	return nil
}

// ── 追杀令到期收尾(r2-gw#2) ────────────────────────────────────────────────

const (
	// warBountyExpiryInterval:到期收尾的节拍。它挂在待入队物品循环上(每个 stable 副本都跑、默认 10 秒一轮),按轮数折成约 1 分钟一次。
	warBountyExpiryInterval = time.Minute
	// warBountyExpiryBatch:一轮最多看多少张;没看完的下一轮从游标处接着看。
	warBountyExpiryBatch = 100
	// warBountyExpirySettleGrace:过期多久之后才收尾。结算按对局结束时刻判到期:结束在到期之前、结算消息晚到的那一局
	// (消费者退避重投到转死信约 7.5 分钟)要先落定,否则猎手赢了却被到期退款抢先。多等的这一刻钟只推迟退款,不改金额。
	warBountyExpirySettleGrace = 15 * time.Minute
)

// warBountyExpiryEveryRounds 把收尾节拍折成宿主循环的轮数(至少每轮一次)。
func warBountyExpiryEveryRounds(hostInterval time.Duration) int {
	if hostInterval <= 0 || hostInterval >= warBountyExpiryInterval {
		return 1
	}
	return int(warBountyExpiryInterval / hostInterval)
}

// warBountyExpiryStats 是一轮到期收尾的小结(日志与单测用)。failed 不含「写闸门冻结」(确定没写,下一轮再来)。
type warBountyExpiryStats struct {
	scanned, expired, skipped, failed int
}

// expireDueGuildWarBounties 跑一轮追杀令到期收尾:从游标处普通读一页「ACTIVE 且过期已超宽限」的追杀令,每张一个事务置 EXPIRED
// 并原路退回托管(data.WarRepo.ExpireBounty:行锁 + 状态复核,多副本同时跑也只退一次)。status=1(扣款还没终结)的不在候选里,
// 扣款终结成 ACTIVE 之后的下一轮再处理。返回下一轮的游标:读到表尾回到表头;某张这一轮退不成(发布者未决指令已满、库抖动)
// 就跳过,等游标转回来再试,不挡后面的。发号失败本轮到此为止(号段故障换一张也好不了),游标停在这一张之前。
func (l *GuildLogic) expireDueGuildWarBounties(ctx context.Context, after data.BountyExpiryCursor) (data.BountyExpiryCursor, warBountyExpiryStats) {
	next := after
	var stats warBountyExpiryStats
	if l.war == nil || l.war.Repo == nil {
		return next, stats
	}
	nowMs := uint64(l.warNow().UnixMilli())
	graceMs := uint64(warBountyExpirySettleGrace.Milliseconds())
	if nowMs <= graceMs {
		return next, stats
	}
	page, err := l.war.Repo.ListDueBounties(ctx, nowMs-graceMs, after, warBountyExpiryBatch)
	if err != nil {
		if ctx.Err() == nil {
			stats.failed++
			logx.Errorf("[GuildWar] list due bounties failed, expiry round ends: %v", err)
		}
		return next, stats
	}
	for _, b := range page {
		if ctx.Err() != nil {
			return next, stats
		}
		stats.scanned++
		in := data.ExpireBountyInput{Bounty: b, NowMs: nowMs}
		if silver, _ := war.BountyExpiryRefund(b); silver > 0 {
			if l.war.OpIDs == nil {
				stats.skipped++
				logx.Errorf("[GuildWar] bounty %d expired but the asset op id minter is not wired; silver refund deferred", b.GetBountyId())
				next = data.BountyExpiryCursor{ExpiresAtMs: b.GetExpiresAtMs(), BountyID: b.GetBountyId()}
				continue
			}
			opID, mErr := l.war.OpIDs.Mint(ctx)
			if mErr != nil || opID == 0 {
				stats.failed++
				logx.Errorf("[GuildWar] mint refund op id for expired bounty %d failed, expiry round ends: id=%d err=%v", b.GetBountyId(), opID, mErr)
				return next, stats
			}
			bundle, bErr := buildSilverItemBundle(silver)
			if bErr != nil {
				stats.failed++
				logx.Errorf("[GuildWar] build refund bundle for expired bounty %d failed: %v", b.GetBountyId(), bErr)
				next = data.BountyExpiryCursor{ExpiresAtMs: b.GetExpiresAtMs(), BountyID: b.GetBountyId()}
				continue
			}
			in.RefundOpID, in.RefundBundle = opID, bundle
		}
		res, err := l.war.Repo.ExpireBounty(ctx, in)
		switch {
		case err != nil:
			if ctx.Err() == nil && !authoritysql.IsWriterFrozen(err) {
				stats.failed++
			}
			logx.Errorf("[GuildWar] expire bounty %d failed, will retry when the cursor comes back: %v", b.GetBountyId(), err)
		case res.Expired:
			stats.expired++
			if res.IssuerGuildGone {
				logx.Errorf("[GuildWar] bounty %d expired but its issuer guild %d no longer exists; the public-funds stake is not refunded",
					b.GetBountyId(), b.GetIssuerGuildId())
			}
			if res.RefundOwed {
				logx.Infof("[GuildWar] bounty %d expired; the silver refund to issuer %d waits in the owed queue (credit window full or role fence frozen)",
					b.GetBountyId(), b.GetIssuerPlayerId())
			}
			if res.RefundAbandoned {
				logx.Errorf("[GuildWar] bounty %d expired but issuer %d has been removed; the silver refund is abandoned",
					b.GetBountyId(), b.GetIssuerPlayerId())
			}
		}
		next = data.BountyExpiryCursor{ExpiresAtMs: b.GetExpiresAtMs(), BountyID: b.GetBountyId()}
	}
	if len(page) < warBountyExpiryBatch {
		next = data.BountyExpiryCursor{}
	}
	if stats.expired > 0 || stats.skipped > 0 || stats.failed > 0 {
		logx.Infof("[GuildWar] bounty expiry round: scanned=%d expired=%d skipped=%d failed=%d", stats.scanned, stats.expired, stats.skipped, stats.failed)
	}
	return next, stats
}

// ── 终态追杀令的保留期清理(r2-gw#7) ──────────────────────────────────────────

const (
	// warBountyTerminalRetention:终态追杀令(结单、撕单、到期退回、扣款作废)到期之后再留多久才删。
	// 取 30 天:远长于结果消息可能被重投的窗口(match-results 保留 7 天,死信修好后重放)—— 何况重投时这一行是终态还是已删,
	// 结算的结论都一样(都不动追杀令);并与资产指令终态行的缺省保留期(AssetOp.TerminalRetentionDays = 30)对齐,
	// 预扣 / 领赏 / 退款指令还查得到的时候,对应的追杀令行也还在,对账两头都有据。
	warBountyTerminalRetention = 30 * 24 * time.Hour
	// warBountyPurgeBatch:一轮最多删多少行(与到期收尾同一节拍,约每分钟一轮;没删完的下一轮接着删)。
	warBountyPurgeBatch = 200
)

// purgeTerminalGuildWarBounties 删一批到期已超保留期的终态追杀令。只是清理:失败只记日志,不 Hold 排空(没删的行原样留着,
// 下一轮再删;删一行是自动提交的单行语句,重做无害)。
func (l *GuildLogic) purgeTerminalGuildWarBounties(ctx context.Context) {
	if l.war == nil || l.war.Repo == nil {
		return
	}
	nowMs := uint64(l.warNow().UnixMilli())
	retentionMs := uint64(warBountyTerminalRetention.Milliseconds())
	if nowMs <= retentionMs {
		return
	}
	deleted, err := l.war.Repo.PurgeTerminalBounties(ctx, nowMs-retentionMs, warBountyPurgeBatch)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("[GuildWar] purge terminal bounties failed after deleting %d, will retry next round: %v", deleted, err)
		}
		return
	}
	if deleted > 0 {
		logx.Infof("[GuildWar] purged %d terminal bounties that expired before %d", deleted, nowMs-retentionMs)
	}
}

// ── 帮战待入账行转换(r2-gw#5 / #6) ──────────────────────────────────────────

// warCreditOwedStats 是一轮帮战待入账行转换的小结(日志与单测用)。failed 不含「写闸门冻结」(确定没写,下一轮再来)。
type warCreditOwedStats struct {
	scanned, converted, stillFull, frozen, abandoned, gone, failed int
}

// convertWarCreditOwed 跑一轮帮战待入账行转换:从游标处翻页读 guild_war_credit_owed,逐行转成资产指令
// (data.WarRepo.ConvertWarCreditOwed:护栏副本 → 分 seq → 插指令 → 删行,同一事务;指令号就是行里预发的那个)。
// 与历练的待入队物品循环同一套规矩(trial_background.go owedRewardLoop):每个副本都跑、靠事务里的行锁而不是互斥保证只转一次;
// 一轮至多看 owedMaxRowsPerRound 行,看不完下一轮从游标接着看,读到表尾回到表头;某人本轮已判仍满 / 仍冻结,他名下其余行本轮不再试。
// 仍满、仍冻结都是确定的「什么都没写」,不算失败(不 Hold 排空):寄售中的角色可能冻好几天,算失败的话排空就永远证明不了。
func (l *GuildLogic) convertWarCreditOwed(ctx context.Context, after data.WarCreditOwedCursor) (data.WarCreditOwedCursor, warCreditOwedStats) {
	next := after
	var stats warCreditOwedStats
	if l.war == nil || l.war.Repo == nil {
		return next, stats
	}
	nowMs := uint64(l.warNow().UnixMilli())
	waiting := make(map[uint64]struct{})
	for stats.scanned < owedMaxRowsPerRound {
		if ctx.Err() != nil {
			return next, stats
		}
		page, err := l.war.Repo.ListWarCreditOwed(ctx, next, owedPageSize)
		if err != nil {
			if ctx.Err() == nil {
				stats.failed++
				logx.Errorf("[GuildWar] list owed war credits failed, round ends: %v", err)
			}
			return next, stats
		}
		for _, owed := range page {
			if ctx.Err() != nil {
				return next, stats
			}
			stats.scanned++
			if _, skip := waiting[owed.PlayerID]; !skip {
				status, err := l.war.Repo.ConvertWarCreditOwed(ctx, owed, nowMs)
				switch {
				case err != nil:
					if ctx.Err() == nil && !authoritysql.IsWriterFrozen(err) {
						stats.failed++
					}
					logx.Errorf("[GuildWar] convert owed war credit op %d of player %d failed, will retry on a later round: %v", owed.OpID, owed.PlayerID, err)
				case status == data.WarOwedConverted:
					stats.converted++
				case status == data.WarOwedStillFull:
					stats.stillFull++
					waiting[owed.PlayerID] = struct{}{}
				case status == data.WarOwedFrozen:
					stats.frozen++
					waiting[owed.PlayerID] = struct{}{}
				case status == data.WarOwedAbandoned:
					stats.abandoned++
					logx.Errorf("[GuildWar] owed war credit op %d (source %d) abandoned: player %d has been removed", owed.OpID, owed.SourceID, owed.PlayerID)
				case status == data.WarOwedGone:
					stats.gone++
				}
			}
			next = owed.Cursor()
		}
		if len(page) < owedPageSize {
			next = data.WarCreditOwedCursor{}
			break
		}
	}
	if stats.converted > 0 || stats.abandoned > 0 || stats.failed > 0 {
		logx.Infof("[GuildWar] owed war credit round: scanned=%d converted=%d still_full=%d frozen=%d abandoned=%d gone=%d failed=%d",
			stats.scanned, stats.converted, stats.stillFull, stats.frozen, stats.abandoned, stats.gone, stats.failed)
	}
	return next, stats
}

// fieldTargetBounty 是野外点选面板上能给这名查看者看的追杀令(r2-gw#8):与追杀榜(ListActiveBounties)、开战挂钩、追踪同一口径
// (war.BountyHuntable:同区、ACTIVE 未过期、全区江湖榜或本帮发的单)。仅发单帮可见的单对别帮一律当没有,号、赏金、档位都不露。
func fieldTargetBounty(b *pb.GuildWarBountyRecord, zoneID uint32, viewerGuildID, nowMs uint64) (bountyID, rewardSilver uint64, tier pb.GuildWarBountyTier) {
	if !war.BountyHuntable(b, zoneID, viewerGuildID, nowMs) {
		return 0, 0, pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_UNSPECIFIED
	}
	return b.GetBountyId(), b.GetEscrowSilver(), pb.GuildWarBountyTier(b.GetTier())
}

func containsU64(slice []uint64, target uint64) bool {
	for _, v := range slice {
		if v == target {
			return true
		}
	}
	return false
}
