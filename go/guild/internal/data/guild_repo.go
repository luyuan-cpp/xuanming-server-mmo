package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"shared/drainproof"
	"shared/placement"
	"shared/rolefence"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"golang.org/x/text/unicode/norm"

	"guild/internal/constants"
)

// 写路径哨兵错误:logic 层据此映射到对玩家的提示,而不是笼统的内部错误。
var (
	// ErrGuildGone:目标公会在 MySQL 里不存在(可能刚被解散)。
	ErrGuildGone = errors.New("guild does not exist")
	// ErrGuildFull:事务内按权威行数判定已满。
	ErrGuildFull = errors.New("guild is full")
	// ErrPlayerAlreadyInGuild:唯一索引 uk_guild_member(player_id)拒绝了跨公会重复 membership。
	ErrPlayerAlreadyInGuild = errors.New("player already belongs to a guild")
	// ErrGuildNameTaken:唯一索引 uk_guild(name_norm)拒绝了重名(帮名全局唯一,不分 zone)。
	ErrGuildNameTaken = errors.New("guild name already taken")
	// ErrGuildZoneMismatch:目标公会不属于入会方要求的 zone(按 zone 隔离的入会)。
	ErrGuildZoneMismatch = errors.New("guild belongs to another zone")
	// ErrAnnouncementForbidden:MySQL 权威 membership 不存在或角色不是 officer/leader。
	ErrAnnouncementForbidden = errors.New("guild announcement update is not authorized")

	// 下面这组由 guild_manage_repo.go 的管理 / 审批事务返回(02-management.md §6.5)。
	// 放在这里而不是那边,是为了让"哨兵错误"只有一个定义点:logic 层的 errors.Is 映射表
	// 照着这一个 var 块写,新增哨兵时不必去两个文件里找。

	// ErrNotGuildMember:操作者在 MySQL 权威表里已经不是该帮成员(缓存映射落后于真相)。
	ErrNotGuildMember = errors.New("operator is not a member of the guild")
	// ErrTargetNotMember:操作目标不在该帮(刚退帮 / 被别人踢了 / 客户端拿的是过期快照)。
	ErrTargetNotMember = errors.New("target is not a member of the guild")
	// ErrRankTooLow:锁内复核的权威 role 档位不足以执行该操作(constants.Rank)。
	ErrRankTooLow = errors.New("operator rank too low")
	// ErrOfficerLimit:长老数已达 GuildLevel[guild.level].max_officers。
	ErrOfficerLimit = errors.New("officer limit reached")
	// ErrLeaderCantLeave:帮主不能退帮 —— 否则帮会会留下无主状态,只能转让或解散。
	ErrLeaderCantLeave = errors.New("leader cannot leave")
	// ErrLeaderMismatch:guild.leader_id 与 role=3 的成员行对不上(同一事实的两份存储已被破坏)。
	// 这时不猜哪份对,直接拒写:继续写只会让两份存储分叉得更远。
	ErrLeaderMismatch = errors.New("guild leader_id disagrees with member roles")
	// ErrGuildLevelConfigMissing:GuildLevel 配表缺该等级行,长老上限无从判定。
	// 配表缺行按错误处理而不是默认放行 —— 默认值会悄悄绕过上限。
	ErrGuildLevelConfigMissing = errors.New("guild level row missing in GuildLevel table")
	// ErrApplicationNotFound:申请不存在、已过期、已被别人处理,或申请人已入他帮 / 归属区与帮会不符。
	ErrApplicationNotFound = errors.New("guild application not found or expired")
	// ErrApplicationLimit:本人待审申请数已达 GuildRule.max_pending_applications_per_player。
	ErrApplicationLimit = errors.New("player pending application limit reached")
	// ErrApplicationQueueFull:目标帮会待审申请数已达 GuildRule.max_pending_applications_per_guild。
	ErrApplicationQueueFull = errors.New("guild pending application queue full")

	// 下面这组由 economy_repo.go 的经济事务返回(05-economy.md §5.16–§5.18)。
	// 帮会不存在 / 不是成员 / 职位不够 / 配表缺行 / 写冲突直接复用上面已有的哨兵,不另造一份同义错误。

	// ErrGuildLevelTooLow:帮会等级低于捐献选项 / 商品要求的等级(事务内读到的权威等级)。
	ErrGuildLevelTooLow = errors.New("guild level too low")
	// ErrDonateLimit:该捐献选项本游戏日次数已用完(计数行的带上限 upsert 判为达上限)。
	ErrDonateLimit = errors.New("daily donate limit reached")
	// ErrShopLimit:该商品本周期限购份数不够本次兑换。
	ErrShopLimit = errors.New("guild shop purchase limit reached")
	// ErrContributionInsufficient:锁内读到的可用帮贡不够本次兑换的总价。
	ErrContributionInsufficient = errors.New("contribution balance insufficient")
	// ErrGuildMaxLevel:当前等级行的 upgrade_cost_funds 为 0,即已是最高级。
	ErrGuildMaxLevel = errors.New("guild already at max level")
	// ErrFundsInsufficient:锁内读到的帮会资金不够升到下一级。
	ErrFundsInsufficient = errors.New("guild funds insufficient")
	// ErrClientRequestReplayed:捐献 / 兑换预留在锁住本人成员行之后,读到同一 (玩家, client_request_id) 已有去重记录 ——
	// 首次请求已经写成了指令(它比本次的事务前查重晚一步提交)。本事务整体回滚、什么都不写;
	// 调用方按去重记录找回首次那笔指令答复,不是业务拒绝,也不是故障。
	ErrClientRequestReplayed = errors.New("guild client request already applied")
)

// GuildData is the persistence-layer representation of a guild (stored in Redis + MySQL).
type GuildData struct {
	GuildID      uint64 `json:"guild_id"`
	Name         string `json:"name"`
	LeaderID     uint64 `json:"leader_id"`
	Level        uint32 `json:"level"`
	Announcement string `json:"announcement"`
	CreateTimeMs uint64 `json:"create_time_ms"`
	MaxMembers   uint32 `json:"max_members"`
	ZoneID       uint32 `json:"zone_id"`
	// Score 是公会排行分的 MySQL 权威副本;Redis ZSET 只是读加速层,
	// 丢失/分叉后可由 RebuildRanks 从这里全量重建。
	Score int64 `json:"score"`
	// Funds 是帮会资金(捐献累加、升级消耗);B1 恒为 0,B5 起写入。
	Funds   uint64       `json:"funds"`
	Members []MemberData `json:"members"`
}

type MemberData struct {
	PlayerID     uint64 `json:"player_id"`
	Role         uint32 `json:"role"`
	JoinTimeMs   uint64 `json:"join_time_ms"`
	LastActiveMs uint64 `json:"last_active_ms"`
	// ContributionTotal 累计帮贡(只增,用于展示与排序);ContributionBalance 是可消费余额(帮会商店扣减)。
	// B1 两列恒为 0,B5 起写入。
	ContributionTotal   uint64 `json:"contribution_total"`
	ContributionBalance uint64 `json:"contribution_balance"`
	Online              bool   `json:"-"` // 仅内存，不入库
}

// GuildRepo provides cache-aside access to guild data.
// Read path:  Redis → (miss) → singleflight → MySQL → write-back Redis
// Write path: Redis + MySQL (sync)
type GuildRepo struct {
	publicationMu    sync.Mutex
	publicationRole  *drainproof.Session
	publicationOwner *drainproof.Session
	rdb              *redis.Client
	db               *sql.DB
	defaultTTL       time.Duration
	cacheLoadMu      sync.Mutex
	// fence 是角色护栏,ConfigureRoleFence 启动时写一次;occupancy 是 match 上的历练占用,BindTrialOccupancy 启动时写一次。之后都只读。
	fence     *rolefence.Guard
	occupancy TrialOccupancy
	// zones 是合服映射缓存,BindMergedZones 在 repo 交给任何调用方之前写一次,之后只读。
	zones *placement.MergedZoneCache
}

func NewGuildRepo(rdb *redis.Client, db *sql.DB, defaultTTL time.Duration) *GuildRepo {
	return &GuildRepo{
		rdb:        rdb,
		db:         db,
		defaultTTL: defaultTTL,
	}
}

// ── 合服解析 ──────────────────────────────────────────────────
//
// 合服只写一条映射,不改写 guild.zone_id(docs/design/player-data-tidb-scaling-plan.md §7.1)。公会行记的是建帮时的原始区,
// 比较两边都先解析到最终区,按区查询用 zone_id IN (ZoneMembers(z))。映射缓存可能滞后一个刷新周期,最坏结果是一次可重试的
// 「不同区」拒绝,记录里存的原始区窗口过后都解析到同一个最终区,不会留下错误状态。

// BindMergedZones 接入合服映射缓存。guild.go 启动时绑定,读不到映射已经拒绝启动;没绑(单测、不看区的工具)等于没有任何合服。
func (r *GuildRepo) BindMergedZones(zones *placement.MergedZoneCache) {
	r.zones = zones
}

// mergedZones 返回当前快照。nil 快照的 Final、Members 按「没有合服」回答;nil repo 只出现在不碰存储的单测里。
func (r *GuildRepo) mergedZones() *placement.MergedZones {
	if r == nil {
		return nil
	}
	return r.zones.Snapshot()
}

// FinalZone 返回 zone 解析合服映射之后的最终区。
func (r *GuildRepo) FinalZone(zone uint32) uint32 {
	return r.mergedZones().Final(zone)
}

// SameZone 报告两个原始区是否解析到同一个最终区。两边用同一份快照解析,不会被一次映射刷新夹在中间。
func (r *GuildRepo) SameZone(a, b uint32) bool {
	zones := r.mergedZones()
	return zones.Final(a) == zones.Final(b)
}

// ZoneMembers 返回解析到 zone 所在最终区的全部原始区,含最终区自己,升序。按区查询用它拼 IN。
func (r *GuildRepo) ZoneMembers(zone uint32) []uint32 {
	return r.mergedZones().Members(zone)
}

// ── Redis key helpers ──────────────────────────────────────────

func guildKey(guildID uint64) string {
	// v2 隔离旧版无 generation 的整份覆盖缓存；部署后绝不能直接信任可能由
	// 旧 reader 在写后回填的 guild:{id}。
	return fmt.Sprintf("guild:v2:%d", guildID)
}

func playerGuildKey(playerID uint64) string {
	return fmt.Sprintf("player_guild:v2:%d", playerID)
}

func guildCacheGenerationKey(guildID uint64) string {
	return fmt.Sprintf("guild:v2:cache_generation:%d", guildID)
}

func playerGuildCacheGenerationKey(playerID uint64) string {
	return fmt.Sprintf("player_guild:v2:cache_generation:%d", playerID)
}

// 帮会榜只有分区榜,没有全服榜:客户端只看本区榜,运维和 GM 也不需要(负责人 2026-11-10 定,
// docs/design/gate-online-identity.md §9.6;Java 版同样不提供)。每个最终区两个键,只经下面「Ranking」一节的脚本一起改:
//   - guild_rank:zone:{最终区}    ZSET,member = guild_id,score = 帮会积分;
//   - guild_rank:version:{最终区} HASH,field = guild_id,value = 榜上那个分数对应的 guild.score_version。
//
// 另有每个已解散帮会一个墓碑 guild_rank:tombstone:{guild_id}(带 TTL),挡住解散前提交、解散后才到的旧写入。
// 权威分在 MySQL guild.score / score_version,RebuildRanks 按版本把榜与它对齐。
// ErrRankZoneRequired 是查榜不给区时的错误,只可能是调用方写错了。
var ErrRankZoneRequired = errors.New("guild rank: a zone is required, there is no global guild rank")

const (
	// 榜维护锁只让 RebuildRanks 与 FoldMergedZoneRanks 互斥(同一时刻只有一个副本在搬条目,不重复扫描)。
	// 写分、建帮上榜、解散摘榜都不拿它。
	guildRankLockKey  = "guild_rank:maintenance_lock"
	guildRankLockTTL  = 5 * time.Minute
	guildRankLockWait = 5 * time.Second

	zoneRankKeyPrefix = "guild_rank:zone:"

	// rankTombstoneTTL 是解散墓碑的保留时长。迟到的旧写入只有三个来源:解散前已提交、还没写榜的推分(同一请求内,
	// 受请求超时约束,秒级);启动重建读完快照到合并之间(分钟级);折叠读出源区条目到并进目标区之间(秒级)。
	// 一天远大于三者;过期由 Redis 自己删,不需要清理任务。
	rankTombstoneTTL = 24 * time.Hour

	// rankBatchSize 是榜维护一条 pipeline、一次回库复核、折叠一页的条目数。
	rankBatchSize = 500

	// maxRankVersion:脚本用 Lua 数字(double)比较版本,2^53 以内是精确整数。每次改分只 +1,实际到不了。
	maxRankVersion = 1 << 53
)

// 新帮会的初始分数与版本:CreateGuild 写进 guild 行,AddNewGuildToRank 按同一对值上榜。
const (
	newGuildScore        int64  = 0
	newGuildScoreVersion uint64 = 0
)

var invalidateVersionedCacheScript = redis.NewScript(`
redis.call("INCR", KEYS[1])
redis.call("DEL", KEYS[2])
return 1
`)

var fillVersionedCacheScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then current = "0" end
if current ~= ARGV[1] then return 0 end
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
return 1
`)

// releaseRankLockScript:榜维护锁的"令牌相符才删"(比较后删除),TTL 到期后别人重新拿到的锁不会被迟到的释放删掉。
var releaseRankLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

func zoneRankKey(zoneID uint32) string {
	return fmt.Sprintf("%s%d", zoneRankKeyPrefix, zoneID)
}

// zoneRankVersionKey 是与 zoneRankKey 同区的版本 HASH。刻意不以 guild_rank:zone: 开头:按 guild_rank:zone:* 扫榜键的地方
// (RebuildRanks、RemoveGuildFromRank、tools/data_consistency_check)不会把它当成一张榜。
func zoneRankVersionKey(zoneID uint32) string {
	return fmt.Sprintf("guild_rank:version:%d", zoneID)
}

func rankTombstoneKey(guildID uint64) string {
	return fmt.Sprintf("guild_rank:tombstone:%d", guildID)
}

// ── Read (cache-aside + singleflight) ──────────────────────────

// GetGuild loads a guild by ID. cache_generation prevents this race:
// reader miss -> reads old MySQL -> writer commits + invalidates -> reader writes old snapshot.
// A fill is accepted only when the generation observed before the DB read is still current.
func (r *GuildRepo) GetGuild(ctx context.Context, guildID uint64) (*GuildData, error) {
	if guild, found, err := r.getCachedGuild(ctx, guildID); err != nil || found {
		return guild, err
	}

	// 只压平本实例的同 key miss；跨实例的旧回填由 generation Lua 拦截。
	r.cacheLoadMu.Lock()
	defer r.cacheLoadMu.Unlock()
	if guild, found, err := r.getCachedGuild(ctx, guildID); err != nil || found {
		return guild, err
	}

	generation, err := r.cacheGeneration(ctx, guildCacheGenerationKey(guildID))
	if err != nil {
		return nil, err
	}
	guild, err := r.loadGuildFromMySQL(ctx, guildID)
	if err != nil || guild == nil {
		return guild, err
	}
	payload, err := json.Marshal(guild)
	if err != nil {
		return nil, err
	}
	if _, err := fillVersionedCacheScript.Run(
		ctx,
		r.rdb,
		[]string{guildCacheGenerationKey(guildID), guildKey(guildID)},
		generation,
		payload,
		r.defaultTTL.Milliseconds(),
	).Int(); err != nil {
		return nil, fmt.Errorf("fill guild cache %d: %w", guildID, err)
	}
	return guild, nil
}

// GetPlayerGuildID returns the guild ID a player belongs to (0 if none).
func (r *GuildRepo) GetPlayerGuildID(ctx context.Context, playerID uint64) (uint64, error) {
	val, err := r.rdb.Get(ctx, playerGuildKey(playerID)).Uint64()
	if err == nil {
		return val, nil
	}
	if err != redis.Nil {
		return 0, fmt.Errorf("redis get player guild %d: %w", playerID, err)
	}

	r.cacheLoadMu.Lock()
	defer r.cacheLoadMu.Unlock()
	val, err = r.rdb.Get(ctx, playerGuildKey(playerID)).Uint64()
	if err == nil {
		return val, nil
	}
	if err != redis.Nil {
		return 0, fmt.Errorf("redis recheck player guild %d: %w", playerID, err)
	}

	generation, err := r.cacheGeneration(ctx, playerGuildCacheGenerationKey(playerID))
	if err != nil {
		return 0, err
	}
	guildID, err := r.loadPlayerGuildFromMySQL(ctx, playerID)
	if err != nil {
		return 0, err
	}
	// 0 也缓存，避免无公会玩家每次都打 MySQL；唯一索引保证结果至多一行。
	if _, err := fillVersionedCacheScript.Run(
		ctx,
		r.rdb,
		[]string{playerGuildCacheGenerationKey(playerID), playerGuildKey(playerID)},
		generation,
		strconv.FormatUint(guildID, 10),
		r.defaultTTL.Milliseconds(),
	).Int(); err != nil {
		return 0, fmt.Errorf("fill player guild cache %d: %w", playerID, err)
	}
	return guildID, nil
}

func (r *GuildRepo) getCachedGuild(ctx context.Context, guildID uint64) (*GuildData, bool, error) {
	payload, err := r.rdb.Get(ctx, guildKey(guildID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redis get guild %d: %w", guildID, err)
	}
	guild := &GuildData{}
	if err := json.Unmarshal(payload, guild); err != nil {
		return nil, false, fmt.Errorf("decode cached guild %d: %w", guildID, err)
	}
	return guild, true, nil
}

func (r *GuildRepo) cacheGeneration(ctx context.Context, key string) (string, error) {
	generation, err := r.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "0", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cache generation %s: %w", key, err)
	}
	return generation, nil
}

// ── Write (MySQL 权威,缓存失效制) ──────────────────────────────
//
// 写路径的规矩:**只往 MySQL 写细粒度变更,成功后让缓存失效**,不做
// 「读缓存 → 改内存 → 整份回写」。整份回写没有并发控制,后写者会把
// 先写者已提交的字段(公告、成员列表)静默覆盖回旧值;删缓存则最坏
// 只是多一次 MySQL 冷读,由 GetGuild 的 cache-aside 自愈。

// CreateGuild 在单个事务里写入公会行 + 会长成员行 + 清掉建帮者本人的全部入帮申请。
// 任何一步失败整体回滚 —— 不允许出现「有公会无会长」或「有会长无公会」的半态。
//
// 事务边界:经 inTx(op=create),因此与管理 / 审批事务共用同一套死锁重试与 READ COMMITTED。
// 锁序 guild_player_state(建帮者)→ guild_member(INSERT)→
// guild_application(I2,主键升序逐行点删)→ guild(INSERT 新行,**事务里最后一条写**)。
//
// 新 guild 行排在最后,是全局表序(guild 在最前)的登记例外之一(2026-09-21 死锁修复契约 P1,friend 审计 #6;
// 另一个是审批通过在锁申请行之后插成员行,论证见 ReviewApplication)。
// 旧顺序先 INSERT guild:建帮者 p1 若正被别帮审批通过,事务会带着新 uk_guild('x') 项卡在 guild_player_state(p1) 上;
// 同名的 p2、p3 此时插 guild 做重复检查,都在 'x' 上排队等 S 锁;审批提交后 p1 插成员撞 1062、整事务回滚,
// 'x' 项被清掉,p2 / p3 的 S 锁同时授予,再各自插入时插入意向被对方的 S 挡住 —— 1213。
// 现在 INSERT guild 之后只剩 COMMIT:会让本事务业务回滚的步骤(成员 1062)全在它之前,彼时还没碰过 uk_guild,
// 同名插入者卷不进来;撞 uk_guild 时本事务只是持有已提交重复项上的 S 锁然后回滚。
// 为什么这个例外不成环:新 guild 行的 guild_id 刚发号、提交前别人无从得知,能等它的只有同名插入者(在 uk_guild 项上
// 以 S 锁等),本事务插完 guild 后不再等任何锁。guild_member 没有指向 guild 的外键,先插成员行、后插帮会行在提交前对外不可见,没有半态。
//
// 查重靠唯一索引裁决,撞 1062 回 ErrGuildNameTaken / ErrPlayerAlreadyInGuild,不再先排全局守卫(guild_manage_repo.go 文件头 (d))。
// 建帮同时是 uk_guild 与 uk_guild_member 两个唯一二级索引的查重插入者:MySQL 档上,同名(名字刚被解散的帮用过)、名字在 uk 上
// 相邻的两帮、在 uk_guild_member 上相邻的两个刚离帮玩家,两个查重插入者可能各持 S next-key(R12)、插入意向互相挡住(G-OUT1),
// InnoDB 牺牲一方,inTx 重跑。TiDB 没有这个环。
// 错误语义:ErrGuildNameTaken / ErrPlayerAlreadyInGuild / ErrWriteConflict;其余为内部错误。
// 提交后的缓存失效走 invalidateAfterCommit,**不再**把失效失败当成建帮失败回给客户端。
func (r *GuildRepo) CreateGuild(ctx context.Context, guild *GuildData) error {
	if len(guild.Members) != 1 {
		return fmt.Errorf("create guild %d: expected exactly one founding member, got %d", guild.GuildID, len(guild.Members))
	}
	leader := guild.Members[0]
	// 防御性复算:logic 已在 normalizeGuildName 里校验过,这里保证写库的唯一键值一定存在。
	nameNorm, ok := GuildNameNorm(guild.Name)
	if !ok {
		return fmt.Errorf("create guild %d: name fails normalization", guild.GuildID)
	}
	// 建帮者的串行化状态行必须在**事务外**建好(90-consistency X-10):
	// 对不存在的行做加锁读再插入,在 TiDB 上锁不住后续插入,在 RR 下靠间隙锁互等。
	if err := r.ensurePlayerStateRows(ctx, opCreate, guild.CreateTimeMs, leader.PlayerID); err != nil {
		return err
	}

	if err := r.inTx(withRoleTargets(ctx, leader.PlayerID), opCreate, func(ctx context.Context, tx *sql.Tx) error {
		// 与"审批通过"抢同一个申请人状态行:两者都要给同一玩家插成员行,
		// 由这把行锁串行,不再靠间隙锁。
		if err := lockPlayerState(ctx, tx, leader.PlayerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
		 VALUES (?, ?, ?, ?, ?, 0, 0)`,
			guild.GuildID, leader.PlayerID, leader.Role, leader.JoinTimeMs, leader.LastActiveMs); err != nil {
			if isDuplicateKey(err) {
				return ErrPlayerAlreadyInGuild
			}
			return fmt.Errorf("insert founding member %d: %w", leader.PlayerID, err)
		}
		// 不变式 I2:成员行出现即清该玩家的全部申请(02-management.md §8.1)。
		// 漏了这一步,建帮者日后解散 / 转让退帮时,72h 内的旧申请会按 I1 重新"复活",
		// 他会莫名其妙被拉进一个早就忘了的帮会。普通读候选 + 主键升序点删(guild_manage_repo.go 文件头 (b))。
		if err := deleteApplicationsOfPlayer(ctx, tx, leader.PlayerID); err != nil {
			return fmt.Errorf("delete applications of founder %d: %w", leader.PlayerID, err)
		}
		// 最后一条写(见函数头:P1 的登记例外)。之后只剩 COMMIT。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guild (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, score_version, funds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			guild.GuildID, guild.Name, nameNorm, guild.LeaderID, guild.Level,
			guild.Announcement, guild.CreateTimeMs, guild.MaxMembers, guild.ZoneID, newGuildScore, newGuildScoreVersion); err != nil {
			if isDuplicateKeyOn(err, guildNameUniqueKey) {
				return ErrGuildNameTaken
			}
			return fmt.Errorf("insert guild %d: %w", guild.GuildID, err)
		}
		return nil
	}); err != nil {
		return err
	}
	r.invalidateAfterCommit(ctx, opCreate, guild.GuildID, leader.PlayerID)
	return nil
}

// UpdateAnnouncementAuthorized 是 UpdateAnnouncement 的薄包装,保留给不需要快照的调用点。
// 授权复核、事务与缓存失效全在 UpdateAnnouncement 里(guild_manage_repo.go §8.7)。
//
// 语义相对旧实现有一处变化:提交后缓存失效失败**不再**返回错误 —— MySQL 已是真相,
// 把失效失败报成写失败只会让客户端因为 Redis 抖动进入重连隔离,而公告其实已经改成功了。
func (r *GuildRepo) UpdateAnnouncementAuthorized(ctx context.Context, guildID, playerID uint64, announcement string) error {
	_, err := r.UpdateAnnouncement(ctx, guildID, playerID, announcement)
	return err
}

// canSetAnnouncement:长老与帮主可改公告。
// 比 constants.Rank 不比 role 原值 —— role 编码不连续(2 是空号),
// `role >= RoleOfficer` 会把未知编码一起放进来,等于凭空发权限。
func canSetAnnouncement(role uint32) bool {
	return constants.Rank(role) >= constants.RankOfficer
}

func (r *GuildRepo) invalidateGuildCache(ctx context.Context, guildID uint64) error {
	if _, err := invalidateVersionedCacheScript.Run(
		ctx,
		r.rdb,
		[]string{guildCacheGenerationKey(guildID), guildKey(guildID)},
	).Int(); err != nil {
		return fmt.Errorf("invalidate guild cache %d: %w", guildID, err)
	}
	return nil
}

func (r *GuildRepo) invalidatePlayerGuildCache(ctx context.Context, playerID uint64) error {
	if _, err := invalidateVersionedCacheScript.Run(
		ctx,
		r.rdb,
		[]string{playerGuildCacheGenerationKey(playerID), playerGuildKey(playerID)},
	).Int(); err != nil {
		return fmt.Errorf("invalidate player guild cache %d: %w", playerID, err)
	}
	return nil
}

// RefreshPlayerGuildID 只失效缓存并从 MySQL 重读，绝不删除 membership。
// 用于处理 Redis 映射指向已不存在公会的情况：旧缓存可能落后于玩家的新入会。
func (r *GuildRepo) RefreshPlayerGuildID(ctx context.Context, playerID uint64) (uint64, error) {
	if err := r.invalidatePlayerGuildCache(ctx, playerID); err != nil {
		return 0, err
	}
	return r.GetPlayerGuildID(ctx, playerID)
}

// 解散公会的入口是 guild_manage_repo.go 的 DisbandGuild:它在同一事务里按 MySQL 的
// leader_id 复核授权、按不变式 I3 清申请、返回锁内读到的 zone 与成员名单。
// 旧的 DeleteGuild(无授权、分两段写缓存)已删除,不留调用点。

// ── MySQL queries ──────────────────────────────────────────────

// queryer 是 *sql.DB 与 *sql.Tx 的公共读接口。
//
// 它存在的唯一理由:写事务要在最后一次写之后、提交之前读一份**含本次写**的权威快照
// (02-management.md §6.3)。用同一个 loadGuild 走 tx,既避免抄第二份装配逻辑,
// 也避免用提交后的 GetGuild —— 那一步若 Redis 失败,已提交的写会被报成失败。
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (r *GuildRepo) loadGuildFromMySQL(ctx context.Context, guildID uint64) (*GuildData, error) {
	return loadGuild(ctx, r.db, guildID)
}

// loadGuild 装配一份完整的 GuildData(公会行 + 成员行,成员按 player_id 升序)。
// 公会行不存在返回 (nil, nil);调用方按各自语义决定这是 ErrGuildGone 还是"未入帮"。
func loadGuild(ctx context.Context, q queryer, guildID uint64) (*GuildData, error) {
	row := q.QueryRowContext(ctx,
		"SELECT guild_id, name, leader_id, level, COALESCE(announcement, ''), create_time_ms, max_members, zone_id, score, funds FROM guild WHERE guild_id = ?",
		guildID)

	var guild GuildData
	err := row.Scan(&guild.GuildID, &guild.Name, &guild.LeaderID, &guild.Level,
		&guild.Announcement, &guild.CreateTimeMs, &guild.MaxMembers, &guild.ZoneID, &guild.Score, &guild.Funds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query guild %d: %w", guildID, err)
	}

	// Load members
	rows, err := q.QueryContext(ctx,
		"SELECT player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance FROM guild_member WHERE guild_id = ? ORDER BY player_id",
		guildID)
	if err != nil {
		return nil, fmt.Errorf("query guild members %d: %w", guildID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var m MemberData
		if err := rows.Scan(&m.PlayerID, &m.Role, &m.JoinTimeMs, &m.LastActiveMs, &m.ContributionTotal, &m.ContributionBalance); err != nil {
			return nil, fmt.Errorf("scan guild member: %w", err)
		}
		guild.Members = append(guild.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guild members %d: %w", guildID, err)
	}

	return &guild, nil
}

func (r *GuildRepo) loadPlayerGuildFromMySQL(ctx context.Context, playerID uint64) (uint64, error) {
	var guildID uint64
	err := r.db.QueryRowContext(ctx,
		"SELECT guild_id FROM guild_member WHERE player_id = ?",
		playerID).Scan(&guildID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query player guild %d: %w", playerID, err)
	}
	return guildID, nil
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// MaxGuildNameNormRunes 是 name_norm 的上限:展示名 ≤ constants.MaxGuildNameRunes(24),
// NFKC 对少数兼容字符会展开(如 ㍿),留一倍余量;同时远小于列长 —— proto2mysql v0.2.0 起 name_norm 是整列
// VARCHAR(191),uk_guild 覆盖整列(不再是 191 前缀索引),超过列长的值写不进去,所以上限必须卡在列长之内。
const MaxGuildNameNormRunes = 48

// GuildNameNorm 返回帮名唯一键 name_norm:NFKC → TrimSpace → 小写。
// 唯一性只看它,不依赖 MySQL 表的排序规则(设计 docs/design/guild-phase2/01-storage.md §6.3):
// "青云门" 与 "青云门 "、"ABC" 与 "abc"、"ＡＢＣ" 与 "abc" 都判为重名。
// 空值或超过 MaxGuildNameNormRunes 时返回 false,调用方按帮名非法处理。
func GuildNameNorm(display string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(norm.NFKC.String(display)))
	if n == "" || utf8.RuneCountInString(n) > MaxGuildNameNormRunes {
		return "", false
	}
	return n, true
}

// guildNameUniqueKey 是 guild 表帮名唯一索引名。proto2mysql 按 "uk_"+表名 命名,
// 由 proto/guild/guild_db.proto 的 OptionUniqueKey="name_norm" 生成;表名改了这里必须同改。
const guildNameUniqueKey = "uk_guild"

// isDuplicateKeyOn 判断是否撞了指定的唯一索引。1062 只说明"有重复",要区分是哪个索引只能看消息:
// MySQL 8 / TiDB 形如 "Duplicate entry 'x' for key 'guild.uk_guild'",5.7 不带表名前缀。
// 按**结尾**匹配:帮名本身出现在消息中段,名字里含 "uk_guild" 不能让主键冲突被误判成重名。
func isDuplicateKeyOn(err error, key string) bool {
	var mysqlErr *mysqlDriver.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
		return false
	}
	return strings.HasSuffix(mysqlErr.Message, "'"+key+"'") || strings.HasSuffix(mysqlErr.Message, "."+key+"'")
}

// ── Ranking (Redis ZSET) ───────────────────────────────────────

// RankEntry is a guild's position in the leaderboard.
type RankEntry struct {
	GuildID uint64
	Score   int64
	Rank    uint32 // 1-based
}

// ── 写榜协议:带版本的条件写入 ──────────────────────────────────
//
// guild.score_version 在写 score 的同一事务里 +1。同一帮会的写分在 guild 行锁上串行,所以版本随提交顺序严格递增,
// 一个版本只对应一个已提交的分数。榜上每一项都在同区的版本 HASH 里记着自己的版本,所有写榜都走 rankUpsertScript:
// 版本不比已记的旧才写,帮会已解散(墓碑在)不写。由此:
//   - 写分不拿全服锁:几个副本并发推分、提交之后乱序到达 Redis,榜上留下的总是最后提交的那个分数;
//   - 重建、折叠与写分交错:它们手里快照的版本不比之后提交的推分新,盖不掉它(RebuildRanks、FoldMergedZoneRanks);
//   - 解散之后:先立墓碑再摘榜,迟到的旧写入连同重建、折叠手里的旧快照都被墓碑拒绝(RemoveGuildFromRank)。
//
// 这几个脚本都只碰一个区的两个键,写入时外加该帮会的墓碑键。guild 的 RedisClient 是单实例 *redis.Client,
// 与本文件的缓存代次脚本一样不带 hash tag;要上 Redis Cluster 时连同那两个脚本一起改键名
// (docs/design/guild_ranking_architecture_zh.md §8)。

// rankUpsertScript 是带版本的条件写入,推分、建帮上榜、重建合并、折叠合并都走它。
// 版本相等也写:同一版本只对应一个已提交的分数,重写是幂等的,还能修好被手工改过的分数。
// KEYS[1] = 分区榜,KEYS[2] = 同区版本 HASH,KEYS[3] = 该帮会的墓碑;ARGV[1] = guild_id,ARGV[2] = 分数,ARGV[3] = 版本。
// 返回值见 rankWriteApplied 等三个常量。
var rankUpsertScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[3]) == 1 then return -1 end
local recorded = redis.call("HGET", KEYS[2], ARGV[1])
if recorded and tonumber(recorded) > tonumber(ARGV[3]) then return 0 end
redis.call("ZADD", KEYS[1], ARGV[2], ARGV[1])
redis.call("HSET", KEYS[2], ARGV[1], ARGV[3])
return 1
`)

// rankUpsertScript 的返回值。
const (
	rankWriteApplied   int64 = 1  // 已写入
	rankWriteStale     int64 = 0  // 榜上已是更新的版本,丢弃
	rankWriteDisbanded int64 = -1 // 帮会已解散(墓碑在),丢弃
)

// rankRemoveScript 把一个帮会从一个区摘掉,榜项与版本一起删,不看版本。只用于已解散的帮会:它的任何版本都是幽灵。
// KEYS[1] = 分区榜,KEYS[2] = 同区版本 HASH;ARGV[1] = guild_id。
var rankRemoveScript = redis.NewScript(`
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("HDEL", KEYS[2], ARGV[1])
return 1
`)

// rankRemoveIfVersionScript 只在版本仍是 ARGV[2](读到时的原文,"" = 没记版本)时才摘,返回 1 = 已摘,0 = 版本变了没摘。
// 把条目搬走(折叠、重建摘放错区的项)用它:读完之后又写进来的新版本不会被误删,留到下一轮再搬。
var rankRemoveIfVersionScript = redis.NewScript(`
local recorded = redis.call("HGET", KEYS[2], ARGV[1]) or ""
if recorded ~= ARGV[2] then return 0 end
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("HDEL", KEYS[2], ARGV[1])
return 1
`)

// rankHeadScript 原子读出一个区榜按分数升序的第 0..ARGV[1] 项(ARGV[1] 是末项下标),分数与版本成对、来自同一快照。
// 返回扁平列表 guild_id, 分数, 版本, …;没记版本的项版本为 ""。分数原样返回字符串,不经 Lua 数字。
var rankHeadScript = redis.NewScript(`
local rows = redis.call("ZRANGE", KEYS[1], 0, ARGV[1], "WITHSCORES")
local out = {}
for i = 1, #rows, 2 do
  out[#out + 1] = rows[i]
  out[#out + 1] = rows[i + 1]
  out[#out + 1] = redis.call("HGET", KEYS[2], rows[i]) or ""
end
return out
`)

// rankItem 是写进榜的一项。分数、版本都是十进制原文,原样交给脚本:分数不经 float 往返,版本由脚本比较。
// version 为空表示榜上这一项没记版本(本次改动之前的代码写的),合并时按 0 算,输给任何正式写入。
type rankItem struct {
	guildID uint64
	score   string
	version string
}

// newRankItem 用一次已提交的 (score, score_version) 组成写榜项。
func newRankItem(guildID uint64, score int64, version uint64) (rankItem, error) {
	if version > maxRankVersion {
		return rankItem{}, fmt.Errorf("score version %d of guild %d exceeds the exact integer range of rank scripts", version, guildID)
	}
	return rankItem{guildID: guildID, score: strconv.FormatInt(score, 10), version: strconv.FormatUint(version, 10)}, nil
}

func (it rankItem) upsertArgs() []any {
	version := it.version
	if version == "" {
		version = "0"
	}
	return []any{it.guildID, it.score, version}
}

func rankUpsertKeys(zone uint32, guildID uint64) []string {
	return []string{zoneRankKey(zone), zoneRankVersionKey(zone), rankTombstoneKey(guildID)}
}

func rankEntryKeys(zone uint32) []string {
	return []string{zoneRankKey(zone), zoneRankVersionKey(zone)}
}

// 下面四个把一条写榜命令排进 pipeline,zone 都是已解析的最终区。pipeline 里一律用 EVAL:
// Script.Run 的 EVALSHA 失败回退要先看到这条命令的结果,在 pipeline 里等不到。

func queueRankUpsert(ctx context.Context, pipe redis.Pipeliner, zone uint32, item rankItem) *redis.Cmd {
	return rankUpsertScript.Eval(ctx, pipe, rankUpsertKeys(zone, item.guildID), item.upsertArgs()...)
}

func queueRankRemove(ctx context.Context, pipe redis.Pipeliner, zone uint32, member string) *redis.Cmd {
	return rankRemoveScript.Eval(ctx, pipe, rankEntryKeys(zone), member)
}

func queueRankRemoveIfVersion(ctx context.Context, pipe redis.Pipeliner, zone uint32, member, observed string) *redis.Cmd {
	return rankRemoveIfVersionScript.Eval(ctx, pipe, rankEntryKeys(zone), member, observed)
}

func queueRankTombstone(ctx context.Context, pipe redis.Pipeliner, guildID uint64) *redis.StatusCmd {
	return pipe.Set(ctx, rankTombstoneKey(guildID), "1", rankTombstoneTTL)
}

// 写分事务的两条语句。guild 表上 guild_id 只有 PRIMARY 一条路(guild_lock_plan_mysql_test.go 用 EXPLAIN 钉住)。
const (
	sqlLockGuildScore   = "SELECT zone_id, score_version FROM guild WHERE guild_id = ? FOR UPDATE"
	sqlUpdateGuildScore = "UPDATE guild SET score = ?, score_version = ? WHERE guild_id = ?"
)

// UpdateGuildScore persists the score and bumps score_version in one MySQL transaction (authoritative),
// then writes the guild's per-zone ranking (read acceleration) conditionally on that version.
//
// 先 MySQL 后 Redis 的顺序是刻意的:ZSET 丢了可以由 RebuildRanks 重建,
// 反过来「只进了 ZSET 没进 MySQL」的分数在 Redis 故障后就永久消失 ——
// 旧实现只写 ZSET,公会积分没有任何持久化副本。
//
// 不拿榜维护锁:写榜是带版本的条件写入(见上面「写榜协议」),各帮会、各副本的推分互不等待。
func (r *GuildRepo) UpdateGuildScore(ctx context.Context, guildID uint64, zoneID uint32, score int64) error {
	// 走 inTx 而不是自己 BeginTx:这样才继承 READ COMMITTED(D7)、死锁 / 写冲突重试、
	// 子预算与"COMMIT 结果不明归一到 ErrWriteConflict"。自己开事务会拿到默认隔离级(RR),
	// 并且在 innodb_lock_wait_timeout=1 下把裸 1205 抛给调用方。
	var authoritativeZoneID uint32
	var version uint64
	if err := r.inTx(ctx, opScore, func(ctx context.Context, tx *sql.Tx) error {
		var zone uint32
		var current uint64
		if err := tx.QueryRowContext(ctx, sqlLockGuildScore, guildID).Scan(&zone, &current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGuildGone
			}
			return fmt.Errorf("lock guild %d for score update: %w", guildID, err)
		}
		if current >= maxRankVersion {
			return fmt.Errorf("score version of guild %d is exhausted at %d", guildID, current)
		}
		// 版本每次 +1,这条 UPDATE 一定改到这一行(分数不变也一样),所以按写入自检要求恰好 1 行。
		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("persist score of guild %d", guildID),
			sqlUpdateGuildScore, score, current+1, guildID); err != nil {
			return err
		}
		// 重试契约:结果只在**成功返回前**赋给外层变量,否则重跑会把上一轮的残留带出去。
		authoritativeZoneID, version = zone, current+1
		return nil
	}); err != nil {
		return err
	}
	if zoneID != 0 && !r.SameZone(zoneID, authoritativeZoneID) {
		logx.Errorf("UpdateGuildScore: caller zone %d ignored; guild %d belongs to zone %d",
			zoneID, guildID, authoritativeZoneID)
	}
	if err := r.invalidateGuildCache(ctx, guildID); err != nil {
		return err
	}

	if authoritativeZoneID == 0 {
		// 帮会都按区建(CreateGuild 只用会话的归属区),zone_id=0 的行是坏数据:分数照常落库,不进任何榜。
		logx.Errorf("UpdateGuildScore: guild %d has no zone; score persisted but not ranked", guildID)
		return nil
	}
	return r.publishRank(ctx, guildID, authoritativeZoneID, score, version)
}

// AddNewGuildToRank 让刚建好的帮会以 CreateGuild 写入的初始分数与版本上榜,不开事务、不拿锁。zoneID 是建帮的归属区。
// 初始版本最小,输给之后任何一次推分,所以晚到也盖不掉新分数;帮会已解散时被墓碑挡住。
func (r *GuildRepo) AddNewGuildToRank(ctx context.Context, guildID uint64, zoneID uint32) error {
	if zoneID == 0 {
		logx.Errorf("AddNewGuildToRank: guild %d has no zone; not ranked", guildID)
		return nil
	}
	return r.publishRank(ctx, guildID, zoneID, newGuildScore, newGuildScoreVersion)
}

// publishRank 按版本把一次已提交的写分写进帮会所在最终区的榜,zoneID 是 guild.zone_id(原始区)。
// 被丢弃(榜上已有更新的版本、帮会已解散)是并发下的正常结果,不是错误。
func (r *GuildRepo) publishRank(ctx context.Context, guildID uint64, zoneID uint32, score int64, version uint64) error {
	item, err := newRankItem(guildID, score, version)
	if err != nil {
		return err
	}
	final := r.FinalZone(zoneID)
	result, err := rankUpsertScript.Run(ctx, r.rdb, rankUpsertKeys(final, guildID), item.upsertArgs()...).Int64()
	if err != nil {
		return fmt.Errorf("update rank cache for guild %d: %w", guildID, err)
	}
	switch result {
	case rankWriteApplied:
	case rankWriteStale:
		logx.Infof("guild rank: score of guild %d at version %d not written, zone %d already holds a newer version",
			guildID, version, final)
	case rankWriteDisbanded:
		logx.Infof("guild rank: score of guild %d at version %d not written, the guild has been disbanded", guildID, version)
	default:
		return fmt.Errorf("update rank cache for guild %d: unexpected script result %d", guildID, result)
	}
	return nil
}

// RebuildRanks 在启动时把各分区榜与 MySQL 权威对齐:能修复缺项、旧分、整张榜丢失、解散后的幽灵条目和放错区的条目,
// 合服、撤销合服之后重启一次就回到与映射一致的状态。zone_id=0 的坏行不进任何榜,只计数打日志。
//
// 不再「建临时键、整键 RENAME 覆盖」:那样会把重建期间新写进来的分数盖回快照里的旧值,写分因此要和重建抢同一把锁。
// 现在逐项做条件写,写分不用等它:
//  1. 读一份快照(guild_id, zone_id, score, score_version),每行按版本合并进最终区的榜。榜上已是更新的版本
//     (快照之后又推过分)就不动;帮会已解散(墓碑在)就不写。
//  2. 扫每张分区榜,找出不在快照里、或快照说不归这个区的条目。这些条目**回库复核当前权威**再处理,不凭快照下结论:
//     - 库里已没有:解散后没摘干净的幽灵。立墓碑,再摘;
//     - 在库里、属于这个区:快照之后才建的帮会。按当前权威合并,不删;
//     - 在库里、属于别的区(映射变了)或 zone_id=0:权威分并进它该在的区,再按读到的版本从这里摘,期间新写进来的版本不误删。
//
// 持榜维护锁,只为与折叠、别的副本的重建互斥(同一时刻只有一个副本在搬条目,不重复全表扫描)。
// 正确性不靠这把锁:所有写都是条件写,所有搬走都按版本比较后删除,锁过期(TTL 5 分钟、不续期)也写不坏榜。
func (r *GuildRepo) RebuildRanks(ctx context.Context) error {
	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	snapshot, err := r.loadRankSnapshot(ctx)
	if err != nil {
		return err
	}
	return r.reconcileRanks(ctx, snapshot, r.lookupRankRows)
}

// rankRow 是一个帮会在 MySQL 里的排行权威:原始区、分数与分数版本。
type rankRow struct {
	guildID uint64
	zoneID  uint32
	score   int64
	version uint64
}

func (row rankRow) item() (rankItem, error) {
	return newRankItem(row.guildID, row.score, row.version)
}

// rankRowLookup 按 guild_id 回库读当前权威,查不到的帮会不在结果里。重建摘条目之前用它复核;单测换成内存实现。
type rankRowLookup func(ctx context.Context, guildIDs []uint64) (map[uint64]rankRow, error)

const sqlSelectRankRows = "SELECT guild_id, zone_id, score, score_version FROM guild"

// loadRankSnapshot 读重建用的全表快照(单条语句,语句级一致性读)。
func (r *GuildRepo) loadRankSnapshot(ctx context.Context) ([]rankRow, error) {
	rows, err := r.db.QueryContext(ctx, sqlSelectRankRows)
	if err != nil {
		return nil, fmt.Errorf("scan guilds for rank rebuild: %w", err)
	}
	return scanRankRows(rows)
}

// lookupRankRows 是 rankRowLookup 的 MySQL 实现:按主键 IN 普通读,不加锁(重建只拿它当复核依据,读完就用)。
func (r *GuildRepo) lookupRankRows(ctx context.Context, guildIDs []uint64) (map[uint64]rankRow, error) {
	found := make(map[uint64]rankRow, len(guildIDs))
	if len(guildIDs) == 0 {
		return found, nil
	}
	args := make([]any, len(guildIDs))
	for i, id := range guildIDs {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, sqlSelectRankRows+" WHERE guild_id IN ("+placeholders(len(guildIDs))+")", args...)
	if err != nil {
		return nil, fmt.Errorf("recheck %d guilds for rank rebuild: %w", len(guildIDs), err)
	}
	current, err := scanRankRows(rows)
	if err != nil {
		return nil, err
	}
	for _, row := range current {
		found[row.guildID] = row
	}
	return found, nil
}

func scanRankRows(rows *sql.Rows) ([]rankRow, error) {
	defer rows.Close()
	var out []rankRow
	for rows.Next() {
		var row rankRow
		if err := rows.Scan(&row.guildID, &row.zoneID, &row.score, &row.version); err != nil {
			return nil, fmt.Errorf("scan guild row for rank rebuild: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guild rows for rank rebuild: %w", err)
	}
	return out, nil
}

// rankReconcileStats 只用于重建结束时的一行日志。
type rankReconcileStats struct {
	guilds, zones, unzoned   int // 快照里进榜的帮会数、涉及的最终区数、zone_id=0 的坏行数
	newer, disbanded         int // 快照合并时没写的:榜上已是更新的版本 / 帮会已解散
	ghosts, misplaced, added int // 复核结果:摘掉的幽灵 / 搬走的放错区条目 / 快照之后才进来的(保留)
	garbage                  int // 不是 guild_id 的成员(只可能是手工写进来的)
}

// reconcileRanks 是 RebuildRanks 的 Redis 半段,步骤见 RebuildRanks。快照与复核来源从参数进来,单测不连库。
func (r *GuildRepo) reconcileRanks(ctx context.Context, snapshot []rankRow, lookup rankRowLookup) error {
	zones := r.mergedZones()
	var stats rankReconcileStats

	// 1. 按版本合并快照。home 记下每个帮会按快照该在的最终区,第 2 步据此找可疑条目。
	home := make(map[uint64]uint32, len(snapshot))
	byZone := make(map[uint32][]rankItem)
	for _, row := range snapshot {
		if row.zoneID == 0 {
			stats.unzoned++
			continue
		}
		item, err := row.item()
		if err != nil {
			return err
		}
		final := zones.Final(row.zoneID)
		home[row.guildID] = final
		byZone[final] = append(byZone[final], item)
	}
	if stats.unzoned > 0 {
		logx.Errorf("RebuildRanks: %d guild rows have no zone and are left out of every rank", stats.unzoned)
	}
	stats.guilds, stats.zones = len(home), len(byZone)
	for zone, items := range byZone {
		for batch := range slices.Chunk(items, rankBatchSize) {
			if err := r.mergeRankBatch(ctx, zone, batch, &stats); err != nil {
				return err
			}
		}
	}

	// 2. 扫现有分区榜,可疑条目回库复核。列成员与处理分开:先把一张榜列完再动它,不边扫边删。
	rankZones, err := r.rankZones(ctx)
	if err != nil {
		return err
	}
	for zone := range rankZones {
		members, garbage, err := r.zoneMembers(ctx, zone)
		if err != nil {
			return err
		}
		if err := r.removeGarbageMembers(ctx, zone, garbage); err != nil {
			return err
		}
		stats.garbage += len(garbage)
		var suspects []uint64
		for _, id := range members {
			if final, ok := home[id]; !ok || final != zone {
				suspects = append(suspects, id)
			}
		}
		for batch := range slices.Chunk(suspects, rankBatchSize) {
			if err := r.settleRankSuspects(ctx, zones, zone, batch, lookup, &stats); err != nil {
				return err
			}
		}
	}
	logx.Infof("guild rank reconciled from MySQL: %d guilds in %d zones; snapshot rows not written: %d older than the rank, "+
		"%d disbanded; ghosts removed %d, misplaced entries moved %d, entries added after the snapshot kept %d, "+
		"garbage members removed %d",
		stats.guilds, stats.zones, stats.newer, stats.disbanded, stats.ghosts, stats.misplaced, stats.added, stats.garbage)
	return nil
}

// mergeRankBatch 用一条 pipeline 把一批快照项按版本合并进 zone 的榜。
func (r *GuildRepo) mergeRankBatch(ctx context.Context, zone uint32, items []rankItem, stats *rankReconcileStats) error {
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.Cmd, len(items))
	for i, item := range items {
		cmds[i] = queueRankUpsert(ctx, pipe, zone, item)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("merge snapshot into %s: %w", zoneRankKey(zone), err)
	}
	for _, cmd := range cmds {
		switch result, _ := cmd.Int64(); result {
		case rankWriteStale:
			stats.newer++
		case rankWriteDisbanded:
			stats.disbanded++
		}
	}
	return nil
}

// settleRankSuspects 复核 zone 里的一批可疑条目(不在快照里,或快照说它不归这个区),按当前权威处理,分支见 RebuildRanks。
func (r *GuildRepo) settleRankSuspects(ctx context.Context, zones *placement.MergedZones, zone uint32,
	suspects []uint64, lookup rankRowLookup, stats *rankReconcileStats) error {
	// 版本必须在回库之前读:回库之后才写进来的新版本与这里读到的不同,下面按版本摘时不会误删它。
	observed, err := r.rankVersions(ctx, zone, suspects)
	if err != nil {
		return err
	}
	current, err := lookup(ctx, suspects)
	if err != nil {
		return fmt.Errorf("recheck %d rank entries of %s: %w", len(suspects), zoneRankKey(zone), err)
	}

	// 先写后摘,分两条 pipeline:放错区的项先并进它该在的区,写成功了才从这里摘,中间失败也不会两边都没有。
	writes, removals := r.rdb.Pipeline(), r.rdb.Pipeline()
	for i, id := range suspects {
		member := strconv.FormatUint(id, 10)
		row, ok := current[id]
		switch {
		case !ok:
			// 墓碑与摘除排在同一条 pipeline 里,按序执行,墓碑在前:之后到的旧写入都被拒绝。
			queueRankTombstone(ctx, removals, id)
			queueRankRemove(ctx, removals, zone, member)
			stats.ghosts++
		case row.zoneID != 0 && zones.Final(row.zoneID) == zone:
			item, err := row.item()
			if err != nil {
				return err
			}
			queueRankUpsert(ctx, writes, zone, item)
			stats.added++
		default:
			if row.zoneID != 0 {
				item, err := row.item()
				if err != nil {
					return err
				}
				queueRankUpsert(ctx, writes, zones.Final(row.zoneID), item)
			}
			queueRankRemoveIfVersion(ctx, removals, zone, member, observed[i])
			stats.misplaced++
		}
	}
	if writes.Len() > 0 {
		if _, err := writes.Exec(ctx); err != nil {
			return fmt.Errorf("write rechecked rank entries of %s: %w", zoneRankKey(zone), err)
		}
	}
	if removals.Len() > 0 {
		if _, err := removals.Exec(ctx); err != nil {
			return fmt.Errorf("remove rechecked rank entries of %s: %w", zoneRankKey(zone), err)
		}
	}
	return nil
}

// rankVersions 读 zone 版本 HASH 里这些帮会的版本原文,没记版本的为 ""。
func (r *GuildRepo) rankVersions(ctx context.Context, zone uint32, guildIDs []uint64) ([]string, error) {
	fields := make([]string, len(guildIDs))
	for i, id := range guildIDs {
		fields[i] = strconv.FormatUint(id, 10)
	}
	values, err := r.rdb.HMGet(ctx, zoneRankVersionKey(zone), fields...).Result()
	if err != nil {
		return nil, fmt.Errorf("read versions from %s: %w", zoneRankVersionKey(zone), err)
	}
	versions := make([]string, len(guildIDs))
	for i, value := range values {
		if text, ok := value.(string); ok {
			versions[i] = text
		}
	}
	return versions, nil
}

// zoneMembers 用 ZSCAN 列出一张分区榜的全部成员(SCAN 可能重复返回,这里去重)。
// 不是规范十进制 guild_id 的成员(只可能是手工写进来的)单独放进 garbage。
func (r *GuildRepo) zoneMembers(ctx context.Context, zone uint32) ([]uint64, []string, error) {
	key := zoneRankKey(zone)
	seen := make(map[string]struct{})
	var ids []uint64
	var garbage []string
	var cursor uint64
	for {
		// ZSCAN 的一页是 member、score 交替的扁平列表。
		page, next, err := r.rdb.ZScan(ctx, key, cursor, "", rankBatchSize).Result()
		if err != nil {
			return nil, nil, fmt.Errorf("scan %s: %w", key, err)
		}
		for i := 0; i < len(page); i += 2 {
			member := page[i]
			if _, dup := seen[member]; dup {
				continue
			}
			seen[member] = struct{}{}
			if id, err := strconv.ParseUint(member, 10, 64); err == nil && strconv.FormatUint(id, 10) == member {
				ids = append(ids, id)
			} else {
				garbage = append(garbage, member)
			}
		}
		if next == 0 {
			return ids, garbage, nil
		}
		cursor = next
	}
}

// removeGarbageMembers 摘掉 zone 榜上不是 guild_id 的成员,并打日志:它们不可能由本服务写入。
func (r *GuildRepo) removeGarbageMembers(ctx context.Context, zone uint32, garbage []string) error {
	if len(garbage) == 0 {
		return nil
	}
	logx.Errorf("RebuildRanks: removing %d members of %s that are not guild ids", len(garbage), zoneRankKey(zone))
	pipe := r.rdb.Pipeline()
	for _, member := range garbage {
		queueRankRemove(ctx, pipe, zone, member)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("remove garbage members of %s: %w", zoneRankKey(zone), err)
	}
	return nil
}

// RemoveGuildFromRank 把公会从所有分区榜里摘掉。不拿榜维护锁。
//
// zoneID 是**提示**不是权威:
//
//  1. 公会行还在(降级 / 手工清榜)→ **非锁定**重读一次 MySQL 的 zone_id 当提示
//     (读路径不需要行锁,理由见 authoritativeZoneID;写路径 UpdateGuildScore 仍用 FOR UPDATE),
//     调用方传进来的值只用来记一条不一致日志。帮会还活着,不立墓碑:之后的推分照常让它回到榜上。
//  2. 公会行已删(DisbandGuild 的正常路径)→ 用调用方传进来的值,它必须是
//     DisbandResult.ZoneID(解散事务内 FOR UPDATE 读到的),不能是 guild:v2:{id} 缓存。
//     这时先立墓碑 guild_rank:tombstone:{id}(rankTombstoneTTL)再摘:解散前提交、解散后才到的推分,
//     以及重建、折叠手里的旧快照,都会被条件写入脚本拒绝,不会把帮会写回榜上。
//
// 无论走哪条,最后都扫一遍 guild_rank:zone:* 把该 guildID 从**每一个**分区榜里
// 摘掉:合服之后条目可能还在源区的旧键上(还没折叠),撤销合服之后可能还在目标区的键上(还没重建),
// 只摘最终区那一张会留下一个查不到名字的幽灵条目。代价是一次 SCAN(zone 数量级),换"解散即从所有榜上消失"的确定性。
//
// 为什么不用和写分互斥:墓碑在 SCAN 之前立。墓碑之前执行完的写入留下的条目在 SCAN 开始时已经存在,一定会被扫到、摘掉;
// 墓碑之后的写入都被脚本拒绝。两类之间没有缝。
func (r *GuildRepo) RemoveGuildFromRank(ctx context.Context, guildID uint64, zoneID uint32) error {
	effectiveZone := zoneID
	disbanded := false
	switch authoritativeZone, err := r.authoritativeZoneID(ctx, guildID); {
	case err == nil:
		if zoneID != 0 && !r.SameZone(zoneID, authoritativeZone) {
			logx.Errorf("RemoveGuildFromRank: caller zone %d ignored; guild %d belongs to zone %d",
				zoneID, guildID, authoritativeZone)
		}
		effectiveZone = authoritativeZone
	case errors.Is(err, ErrGuildGone):
		// 行已经删了(解散)。调用方传进来的 zone 来自删除事务,就是权威值。
		disbanded = true
	default:
		return fmt.Errorf("read authoritative zone of guild %d: %w", guildID, err)
	}
	return r.removeFromAllRanks(ctx, guildID, effectiveZone, disbanded)
}

// removeFromAllRanks 是 RemoveGuildFromRank 的 Redis 半段:disbanded 时先立墓碑,再把 guildID 从每张分区榜摘掉。
// zoneID 是帮会的原始区,0 = 不知道(只扫现有的榜)。
func (r *GuildRepo) removeFromAllRanks(ctx context.Context, guildID uint64, zoneID uint32, disbanded bool) error {
	if disbanded {
		// 必须在下面的 SCAN 之前,理由见 RemoveGuildFromRank。
		if err := r.rdb.Set(ctx, rankTombstoneKey(guildID), "1", rankTombstoneTTL).Err(); err != nil {
			return fmt.Errorf("write rank tombstone of guild %d: %w", guildID, err)
		}
	}
	zones, err := r.rankZones(ctx)
	if err != nil {
		return err
	}
	if zoneID > 0 {
		// 分区榜可能因为没人推过分而还不存在;显式带上它,对不存在的键摘除是 no-op。
		zones[r.FinalZone(zoneID)] = struct{}{}
	}
	if len(zones) == 0 {
		return nil // 还没有任何分区榜,也就没有要摘的条目。
	}
	member := strconv.FormatUint(guildID, 10)
	pipe := r.rdb.Pipeline()
	for zone := range zones {
		queueRankRemove(ctx, pipe, zone, member)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("remove guild %d from rank: %w", guildID, err)
	}
	return nil
}

// authoritativeZoneID 读公会当前的 zone_id(建帮时的原始区,合服不改写);行不存在返回 ErrGuildGone。
//
// 这里是**非锁定读**:调用方只是拿这个值做归属判断,读完就用,加 FOR UPDATE 会在
// innodb_lock_wait_timeout=1 下把一次只读查询变成可能抛 1205 的写路径,还平白开了一个事务。
func (r *GuildRepo) authoritativeZoneID(ctx context.Context, guildID uint64) (uint32, error) {
	var zoneID uint32
	if err := r.db.QueryRowContext(ctx,
		"SELECT zone_id FROM guild WHERE guild_id = ?", guildID).Scan(&zoneID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrGuildGone
		}
		return 0, err
	}
	return zoneID, nil
}

// rankZones 列出当前存在分区榜的区(扫 guild_rank:zone:*)。
// SCAN 而不是 KEYS:分区榜数量与 zone 数同量级,但 KEYS 会阻塞整个 Redis。
func (r *GuildRepo) rankZones(ctx context.Context) (map[uint32]struct{}, error) {
	zones := make(map[uint32]struct{})
	iterator := r.rdb.Scan(ctx, 0, zoneRankKeyPrefix+"*", 1000).Iterator()
	for iterator.Next(ctx) {
		key := iterator.Val()
		suffix := strings.TrimPrefix(key, zoneRankKeyPrefix)
		zone, err := strconv.ParseUint(suffix, 10, 32)
		if err != nil || strconv.FormatUint(zone, 10) != suffix {
			// 只可能是手工写进来的键:不认识的键不动它,也不让它挡住这次扫描。
			logx.Errorf("guild rank: key %q is not a zone rank, ignored", key)
			continue
		}
		zones[uint32(zone)] = struct{}{}
	}
	if err := iterator.Err(); err != nil {
		return nil, fmt.Errorf("scan per-zone rank keys: %w", err)
	}
	return zones, nil
}

// FoldMergedZoneRanks 把已合走区的旧榜 guild_rank:zone:{src} 逐项并进最终区的榜,返回这次从旧榜搬走的条目数。
//
// 为什么要有这一步:合服不动公会行,新分数按 Final(guild.zone_id) 写进最终区的榜,但合服之前写的条目还留在源区的键上,
// 没有人再读它。榜是可重建的缓存,权威分在 guild.score;周期任务逐步并过去,不必为一次合服全量重建。
//
// 正确性:
//   - 按版本合并,不用 ZADD NX:每一项都走条件写入脚本并进目标区。目标区已是更新的版本(合服之后推过分)就不动;
//     源区的版本更新(映射缓存还没刷新的副本在合服之后又往源区写过)就由它盖掉目标区的旧分;帮会已解散(墓碑在)就不写。
//   - 先并后摘,按版本摘:一项并进目标区之后,源区那一项只在版本仍是刚读到的那个时才删。读完之后又写进源区的新版本
//     留在原处,下一页或下一轮再并,不会丢。源区的榜空了,键由 Redis 自己删掉,不整键 DEL。
//   - 每页都从榜头重读(ZRANGE 0 N-1,分数与版本在一个脚本里成对读出),不用 ZSCAN 游标:本页摘掉的项不会让下一页漏项。
//   - 逐成员写,不用 ZUNIONSTORE:Redis Cluster 下两个键不在同一个 slot,多键命令会被拒绝。
//   - 中途失败或进程退出:已并的项重并是幂等的条件写,没摘的项下一轮从头再并一遍。
//   - 持 guild_rank:maintenance_lock,只为与 RebuildRanks、别的副本的折叠互斥,不重复搬;正确性不靠它(同 RebuildRanks)。
//
// 稳态下旧键都不存在:先无锁探测,不为空转去抢维护锁。
func (r *GuildRepo) FoldMergedZoneRanks(ctx context.Context) (int, error) {
	zones := r.mergedZones()
	var pending []uint32
	for src := range zones.Merges() {
		exists, err := r.rdb.Exists(ctx, zoneRankKey(src)).Result()
		if err != nil {
			return 0, fmt.Errorf("check rank key of merged zone %d: %w", src, err)
		}
		if exists > 0 {
			pending = append(pending, src)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}
	slices.Sort(pending)

	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	folded := 0
	for _, src := range pending {
		n, err := r.foldZoneRank(ctx, src, zones.Final(src))
		folded += n
		if err != nil {
			return folded, err
		}
	}
	return folded, nil
}

// foldZoneRank 把 src 榜逐项按版本并进 dst 榜,并完一页摘一页(按版本摘)。调用方持榜维护锁。
func (r *GuildRepo) foldZoneRank(ctx context.Context, src, dst uint32) (int, error) {
	folded := 0
	for {
		head, garbage, err := r.rankHead(ctx, src, rankBatchSize)
		if err != nil {
			return folded, err
		}
		if len(head) == 0 && len(garbage) == 0 {
			return folded, nil
		}
		if len(head) > 0 {
			writes := r.rdb.Pipeline()
			for _, item := range head {
				queueRankUpsert(ctx, writes, dst, item)
			}
			if _, err := writes.Exec(ctx); err != nil {
				return folded, fmt.Errorf("fold %s into %s: %w", zoneRankKey(src), zoneRankKey(dst), err)
			}
		}
		// 并进目标区成功之后才从源区摘。并的结果(写入 / 目标区更新 / 已解散)都意味着源区这一项可以摘了。
		removals := r.rdb.Pipeline()
		cmds := make([]*redis.Cmd, len(head))
		for i, item := range head {
			cmds[i] = queueRankRemoveIfVersion(ctx, removals, src, strconv.FormatUint(item.guildID, 10), item.version)
		}
		for _, member := range garbage {
			queueRankRemove(ctx, removals, src, member)
		}
		if _, err := removals.Exec(ctx); err != nil {
			return folded, fmt.Errorf("remove folded entries from %s: %w", zoneRankKey(src), err)
		}
		removed := len(garbage)
		for _, cmd := range cmds {
			if n, _ := cmd.Int64(); n == 1 {
				removed++
			}
		}
		folded += removed
		if removed == 0 {
			// 这一页每一项都在读完之后又被写过:留给下一轮,不在这里空转。
			return folded, nil
		}
	}
}

// rankHead 原子读出 zone 榜头 n 项(分数升序,n ≥ 1)及各自版本。不是规范十进制 guild_id 的成员单独放进 garbage。
func (r *GuildRepo) rankHead(ctx context.Context, zone uint32, n int) ([]rankItem, []string, error) {
	reply, err := rankHeadScript.Run(ctx, r.rdb, rankEntryKeys(zone), n-1).StringSlice()
	if err != nil {
		return nil, nil, fmt.Errorf("read head of %s: %w", zoneRankKey(zone), err)
	}
	if len(reply)%3 != 0 {
		return nil, nil, fmt.Errorf("invalid head reply for %s", zoneRankKey(zone))
	}
	var items []rankItem
	var garbage []string
	for i := 0; i < len(reply); i += 3 {
		member := reply[i]
		id, err := strconv.ParseUint(member, 10, 64)
		if err != nil || strconv.FormatUint(id, 10) != member {
			garbage = append(garbage, member)
			continue
		}
		items = append(items, rankItem{guildID: id, score: reply[i+1], version: reply[i+2]})
	}
	return items, garbage, nil
}

func (r *GuildRepo) acquireRankLock(ctx context.Context) (func(), error) {
	token := uuid.NewString()
	waitTimer := time.NewTimer(guildRankLockWait)
	defer waitTimer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		acquired, err := r.rdb.SetNX(ctx, guildRankLockKey, token, guildRankLockTTL).Result()
		if err != nil {
			return nil, fmt.Errorf("acquire guild rank lock: %w", err)
		}
		if acquired {
			return func() {
				if _, err := releaseRankLockScript.Run(
					context.Background(), r.rdb, []string{guildRankLockKey}, token,
				).Int(); err != nil {
					logx.Errorf("release guild rank lock: %v", err)
				}
			}, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waitTimer.C:
			return nil, fmt.Errorf("timed out waiting for guild rank maintenance lock")
		case <-ticker.C:
		}
	}
}

// 两个脚本各只读一个榜键，Cluster 下不会跨槽；总数/本页及名次/分数来自同一快照。
// 分数字符串原样返回，避免 Redis Lua 把 double 返回值截断成整数。
var guildRankPageScript = redis.NewScript(`
local total = redis.call("ZCARD", KEYS[1])
local size = tonumber(ARGV[2])
if size == 0 then return {total} end
local start = tonumber(ARGV[1])
if start >= total then return {total} end
local stop = math.min(start + size - 1, total - 1)
local rows = redis.call("ZREVRANGE", KEYS[1], start, stop, "WITHSCORES")
table.insert(rows, 1, total)
return rows
`)

var guildRankEntryScript = redis.NewScript(`
local rank = redis.call("ZREVRANK", KEYS[1], ARGV[1])
if not rank then return {} end
return {rank, redis.call("ZSCORE", KEYS[1], ARGV[1])}
`)

// GetGuildRankPage returns a page of the zone's guild rank sorted by score descending.
// page is 1-based; zoneID must be non-zero (ErrRankZoneRequired). A merged-away zone reads the rank of its final zone.
func (r *GuildRepo) GetGuildRankPage(ctx context.Context, zoneID, page, pageSize uint32) ([]RankEntry, uint32, error) {
	if zoneID == 0 {
		return nil, 0, ErrRankZoneRequired
	}
	key := zoneRankKey(r.FinalZone(zoneID))

	// uint32 页码和页长先按 uint64 相乘，超大起点在脚本内先与榜长比较，
	// 为空页时不传给 ZREVRANGE，避免无符号转有符号后环绕回榜首。
	var start uint64
	size := pageSize
	if page == 0 || pageSize == 0 {
		size = 0
	} else {
		start = uint64(page-1) * uint64(pageSize)
		if start > math.MaxInt64 {
			// Redis 的基数/下标都是 int64；该起点必为空页，也不让 Lua
			// 解析超出有符号范围的十进制数（不同 Lua 实现行为不一致）。
			size = 0
		}
	}
	reply, err := guildRankPageScript.Run(ctx, r.rdb, []string{key}, strconv.FormatUint(start, 10), size).Slice()
	if err != nil {
		return nil, 0, fmt.Errorf("read rank page %s: %w", key, err)
	}
	if len(reply) == 0 || len(reply)%2 != 1 {
		return nil, 0, fmt.Errorf("invalid rank page reply for %s", key)
	}
	total, ok := reply[0].(int64)
	if !ok || total < 0 {
		return nil, 0, fmt.Errorf("invalid rank total for %s", key)
	}
	if len(reply) == 1 {
		return nil, uint32(total), nil
	}

	entries := make([]RankEntry, 0, (len(reply)-1)/2)
	for i := 1; i < len(reply); i += 2 {
		guildID, _ := strconv.ParseUint(fmt.Sprintf("%v", reply[i]), 10, 64)
		score, err := parseRankScore(reply[i+1])
		if err != nil {
			return nil, 0, fmt.Errorf("read rank score for guild %d: %w", guildID, err)
		}
		entries = append(entries, RankEntry{
			GuildID: guildID,
			Score:   score,
			Rank:    uint32(start) + uint32((i-1)/2) + 1,
		})
	}
	return entries, uint32(total), nil
}

// GetGuildRank returns a single guild's rank and score on the zone's rank. Rank is 1-based (0 = not ranked).
// zoneID must be non-zero (ErrRankZoneRequired). A merged-away zone reads the rank of its final zone.
func (r *GuildRepo) GetGuildRank(ctx context.Context, guildID uint64, zoneID uint32) (RankEntry, error) {
	if zoneID == 0 {
		return RankEntry{}, ErrRankZoneRequired
	}
	key := zoneRankKey(r.FinalZone(zoneID))

	member := fmt.Sprintf("%d", guildID)

	reply, err := guildRankEntryScript.Run(ctx, r.rdb, []string{key}, member).Slice()
	if err != nil {
		return RankEntry{}, fmt.Errorf("read rank for guild %d: %w", guildID, err)
	}
	if len(reply) == 0 {
		return RankEntry{}, nil // 未上榜仍返回零值，不是存储故障。
	}
	if len(reply) != 2 {
		return RankEntry{}, fmt.Errorf("invalid rank reply for guild %d", guildID)
	}
	rank, ok := reply[0].(int64)
	if !ok || rank < 0 {
		return RankEntry{}, fmt.Errorf("invalid rank position for guild %d", guildID)
	}
	score, err := parseRankScore(reply[1])
	if err != nil {
		return RankEntry{}, fmt.Errorf("read rank score for guild %d: %w", guildID, err)
	}

	return RankEntry{
		GuildID: guildID,
		Score:   score,
		Rank:    uint32(rank) + 1, // 1-based
	}, nil
}

func parseRankScore(raw any) (int64, error) {
	text, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("invalid rank score type %T", raw)
	}
	score, err := strconv.ParseFloat(text, 64)
	return int64(score), err
}

// BindPublication 由实际服务或工具装配同 boot 的 RELAY/MAINTENANCE 角色；必须在交付 repo 前调用。
func (r *GuildRepo) BindPublication(role *drainproof.Session) error {
	r.publicationMu.Lock()
	defer r.publicationMu.Unlock()
	if role == nil || r.publicationRole != nil {
		return drainproof.ErrHold
	}
	r.publicationRole = role
	r.publicationOwner = drainproof.New()
	return nil
}

// ClosePublication 必须在所有能发起 repo 调用的入口已关闭并 join 后调用。
// 私有 owner 关闸不关闭共享角色；已接纳的重试仍完成，调用方把摘要合入自己的正式角色前沿。
func (r *GuildRepo) ClosePublication(ctx context.Context) ([]byte, error) {
	r.publicationMu.Lock()
	owner := r.publicationOwner
	r.publicationMu.Unlock()
	if owner == nil {
		return nil, drainproof.ErrHold
	}
	owner.CloseAdmission()
	return owner.SettledDigest(ctx)
}

func (r *GuildRepo) beginPublication() (finish func(), hold func(), err error) {
	r.publicationMu.Lock()
	owner, role := r.publicationOwner, r.publicationRole
	r.publicationMu.Unlock()
	// 旧未装配构造保留原业务行为，但 ClosePublication 不为其生成正式收据。
	if owner == nil || role == nil {
		return func() {}, func() {}, nil
	}
	roleFinish, err := role.Enter()
	if err != nil {
		role.Hold()
		return nil, nil, err
	}
	ownerFinish, err := owner.Enter()
	if err != nil {
		role.Hold()
		roleFinish()
		return nil, nil, err
	}
	return func() { ownerFinish(); roleFinish() }, func() { owner.Hold(); role.Hold() }, nil
}
