package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	rdb         *redis.Client
	db          *sql.DB
	defaultTTL  time.Duration
	cacheLoadMu sync.Mutex
}

func NewGuildRepo(rdb *redis.Client, db *sql.DB, defaultTTL time.Duration) *GuildRepo {
	return &GuildRepo{
		rdb:        rdb,
		db:         db,
		defaultTTL: defaultTTL,
	}
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

const guildRankKey = "guild_rank" // Redis ZSET: member=guildID, score=rankScore (global)

const (
	guildRankLockKey  = "guild_rank:maintenance_lock"
	guildRankLockTTL  = 5 * time.Minute
	guildRankLockWait = 5 * time.Second
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

var releaseRankLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

func zoneRankKey(zoneID uint32) string {
	return fmt.Sprintf("guild_rank:zone:%d", zoneID)
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
// 锁序 guild(INSERT 新行)→ guild_player_state → guild_member → guild_application,与 §6.1 一致。
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
	if err := r.ensurePlayerStateRow(ctx, leader.PlayerID, guild.CreateTimeMs); err != nil {
		return err
	}

	err := r.inTx(ctx, opCreate, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guild (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
			guild.GuildID, guild.Name, nameNorm, guild.LeaderID, guild.Level,
			guild.Announcement, guild.CreateTimeMs, guild.MaxMembers, guild.ZoneID); err != nil {
			if isDuplicateKeyOn(err, guildNameUniqueKey) {
				return ErrGuildNameTaken
			}
			return fmt.Errorf("insert guild %d: %w", guild.GuildID, err)
		}
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
		// 他会莫名其妙被拉进一个早就忘了的帮会。
		if _, err := tx.ExecContext(ctx, sqlDeletePlayerApplication, leader.PlayerID); err != nil {
			return fmt.Errorf("delete applications of founder %d: %w", leader.PlayerID, err)
		}
		return nil
	})
	if err != nil {
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
// NFKC 对少数兼容字符会展开(如 ㍿),留一倍余量;同时远小于 proto2mysql 的 191 索引前缀,
// 保证唯一键覆盖整个值而不是前缀。
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

// UpdateGuildScore persists the score to MySQL (authoritative) first, then
// updates the global and per-zone ranking ZSETs (read acceleration).
//
// 先 MySQL 后 Redis 的顺序是刻意的:ZSET 丢了可以由 RebuildRanks 重建,
// 反过来「只进了 ZSET 没进 MySQL」的分数在 Redis 故障后就永久消失 ——
// 旧实现只写 ZSET,公会积分没有任何持久化副本。
func (r *GuildRepo) UpdateGuildScore(ctx context.Context, guildID uint64, zoneID uint32, score int64) error {
	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	// 走 inTx 而不是自己 BeginTx:这样才继承 READ COMMITTED(D7)、死锁 / 写冲突重试、
	// 子预算与"COMMIT 结果不明归一到 ErrWriteConflict"。自己开事务会拿到默认隔离级(RR),
	// 并且在 innodb_lock_wait_timeout=1 下把裸 1205 抛给调用方。
	var authoritativeZoneID uint32
	if err := r.inTx(ctx, opScore, func(ctx context.Context, tx *sql.Tx) error {
		var zone uint32
		if err := tx.QueryRowContext(ctx,
			"SELECT zone_id FROM guild WHERE guild_id = ? FOR UPDATE", guildID).Scan(&zone); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGuildGone
			}
			return fmt.Errorf("lock guild %d for score update: %w", guildID, err)
		}
		result, err := tx.ExecContext(ctx,
			"UPDATE guild SET score = ? WHERE guild_id = ?", score, guildID)
		if err != nil {
			return fmt.Errorf("persist score of guild %d: %w", guildID, err)
		}
		if _, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("read score update result for guild %d: %w", guildID, err)
		}
		// 重试契约:结果只在**成功返回前**赋给外层变量,否则重跑会把上一轮的残留带出去。
		authoritativeZoneID = zone
		return nil
	}); err != nil {
		return err
	}
	if zoneID != 0 && zoneID != authoritativeZoneID {
		logx.Errorf("UpdateGuildScore: caller zone %d ignored; guild %d belongs to zone %d",
			zoneID, guildID, authoritativeZoneID)
	}
	if err := r.invalidateGuildCache(ctx, guildID); err != nil {
		return err
	}

	z := redis.Z{Score: float64(score), Member: guildID}
	pipe := r.rdb.Pipeline()
	pipe.ZAdd(ctx, guildRankKey, z)
	if authoritativeZoneID > 0 {
		pipe.ZAdd(ctx, zoneRankKey(authoritativeZoneID), z)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("update rank cache for guild %d: %w", guildID, err)
	}
	return nil
}

// RebuildRanks 始终从 MySQL 权威快照重建全局榜和所有 zone 榜，而不是只在
// ZCARD==0 时运行。临时 key 构造完成后用 MULTI/EXEC 一次切换，因此能修复单项
// 缺失、旧值、zone 榜丢失与解散后的幽灵条目。
func (r *GuildRepo) RebuildRanks(ctx context.Context) error {
	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	rows, err := r.db.QueryContext(ctx, "SELECT guild_id, zone_id, score FROM guild")
	if err != nil {
		return fmt.Errorf("scan guilds for rank rebuild: %w", err)
	}
	defer rows.Close()

	token := uuid.NewString()
	tempGlobalKey := fmt.Sprintf("guild_rank:rebuild:%s:global", token)
	tempZoneKeys := make(map[uint32]string)
	tempKeys := []string{tempGlobalKey}
	pipe := r.rdb.Pipeline()
	rebuilt := 0
	for rows.Next() {
		var guildID uint64
		var zoneID uint32
		var score int64
		if err := rows.Scan(&guildID, &zoneID, &score); err != nil {
			return fmt.Errorf("scan guild row for rank rebuild: %w", err)
		}
		z := redis.Z{Score: float64(score), Member: guildID}
		pipe.ZAdd(ctx, tempGlobalKey, z)
		if zoneID > 0 {
			tempKey, ok := tempZoneKeys[zoneID]
			if !ok {
				tempKey = fmt.Sprintf("guild_rank:rebuild:%s:zone:%d", token, zoneID)
				tempZoneKeys[zoneID] = tempKey
				tempKeys = append(tempKeys, tempKey)
			}
			pipe.ZAdd(ctx, tempKey, z)
		}
		rebuilt++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	defer r.rdb.Del(context.Background(), tempKeys...)
	if rebuilt > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("populate temporary rank zsets: %w", err)
		}
	}

	var oldZoneKeys []string
	iterator := r.rdb.Scan(ctx, 0, "guild_rank:zone:*", 1000).Iterator()
	for iterator.Next(ctx) {
		oldZoneKeys = append(oldZoneKeys, iterator.Val())
	}
	if err := iterator.Err(); err != nil {
		return fmt.Errorf("scan old zone rank keys: %w", err)
	}

	if _, err := r.rdb.TxPipelined(ctx, func(tx redis.Pipeliner) error {
		tx.Del(ctx, guildRankKey)
		if len(oldZoneKeys) > 0 {
			tx.Del(ctx, oldZoneKeys...)
		}
		if rebuilt > 0 {
			tx.Rename(ctx, tempGlobalKey, guildRankKey)
		}
		for zoneID, tempKey := range tempZoneKeys {
			tx.Rename(ctx, tempKey, zoneRankKey(zoneID))
		}
		return nil
	}); err != nil {
		return fmt.Errorf("swap rebuilt rank zsets: %w", err)
	}
	logx.Infof("guild rank ZSETs reconciled from MySQL: %d guilds, %d zones", rebuilt, len(tempZoneKeys))
	return nil
}

// RemoveGuildFromRank 把公会从全局榜和分区榜里摘掉。
//
// zoneID 是**提示**不是权威:
//
//  1. 公会行还在(降级 / 手工清榜)→ **非锁定**重读一次 MySQL 的 zone_id 当提示
//     (读路径不需要行锁,理由见 authoritativeZoneID;写路径 UpdateGuildScore 仍用 FOR UPDATE),
//     调用方传进来的值只用来记一条不一致日志。
//  2. 公会行已删(DisbandGuild 的正常路径)→ 用调用方传进来的值,它必须是
//     DisbandResult.ZoneID(解散事务内 FOR UPDATE 读到的),不能是 guild:v2:{id} 缓存。
//
// 无论走哪条,最后都再扫一遍 guild_rank:zone:* 把该 guildID 从**每一个**分区榜里
// 摘掉。这一刀是给存量数据的:在本次修复之前,清榜用的是可能过期 30 分钟的缓存
// zone,合服后解散的公会会在真正的分区榜里留下一个查不到名字的幽灵条目;
// 只修新写入的路径,那些已经留下的条目永远不会自己消失。代价是一次 SCAN
// (zone 数量级,而且整段已经在榜维护锁里),换"解散即从所有榜上消失"的确定性。
func (r *GuildRepo) RemoveGuildFromRank(ctx context.Context, guildID uint64, zoneID uint32) error {
	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	effectiveZone := zoneID
	switch authoritativeZone, err := r.authoritativeZoneID(ctx, guildID); {
	case err == nil:
		if zoneID != 0 && zoneID != authoritativeZone {
			logx.Errorf("RemoveGuildFromRank: caller zone %d ignored; guild %d belongs to zone %d",
				zoneID, guildID, authoritativeZone)
		}
		effectiveZone = authoritativeZone
	case errors.Is(err, ErrGuildGone):
		// 行已经删了(解散)。调用方传进来的 zone 来自删除事务,就是权威值。
	default:
		return fmt.Errorf("read authoritative zone of guild %d: %w", guildID, err)
	}

	zoneKeys, err := r.allZoneRankKeys(ctx)
	if err != nil {
		return err
	}
	if effectiveZone > 0 {
		// 分区榜可能因为没人推过分而还不存在;显式带上它,ZREM 对不存在的键是 no-op。
		zoneKeys[zoneRankKey(effectiveZone)] = struct{}{}
	}

	pipe := r.rdb.Pipeline()
	pipe.ZRem(ctx, guildRankKey, guildID)
	for key := range zoneKeys {
		pipe.ZRem(ctx, key, guildID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// authoritativeZoneID 读公会当前的 zone_id;行不存在返回 ErrGuildGone。
// MySQL 是归属的唯一真源:guild:v2:{id} 缓存(TTL 30 分钟)在合服之后会有整整一个 TTL 指向旧 zone。
//
// 这里是**非锁定读**:调用方只是拿这个值做归属判断,读完就用,加 FOR UPDATE 既不能
// 阻止它下一毫秒被合服改掉(那是另一个事务的事),又会在 innodb_lock_wait_timeout=1 下
// 把一次只读查询变成可能抛 1205 的写路径,还平白开了一个事务。
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

// allZoneRankKeys 列出当前存在的所有分区榜键(guild_rank:zone:*)。
// SCAN 而不是 KEYS:分区榜数量与 zone 数同量级,但 KEYS 会阻塞整个 Redis。
func (r *GuildRepo) allZoneRankKeys(ctx context.Context) (map[string]struct{}, error) {
	keys := make(map[string]struct{})
	iterator := r.rdb.Scan(ctx, 0, "guild_rank:zone:*", 1000).Iterator()
	for iterator.Next(ctx) {
		keys[iterator.Val()] = struct{}{}
	}
	if err := iterator.Err(); err != nil {
		return nil, fmt.Errorf("scan per-zone rank keys: %w", err)
	}
	return keys, nil
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

// GetGuildRankPage returns a page of guilds sorted by score descending.
// page is 1-based; zoneID=0 means global ranking.
func (r *GuildRepo) GetGuildRankPage(ctx context.Context, zoneID, page, pageSize uint32) ([]RankEntry, uint32, error) {
	key := guildRankKey
	if zoneID > 0 {
		key = zoneRankKey(zoneID)
	}

	total, err := r.rdb.ZCard(ctx, key).Result()
	if err != nil {
		return nil, 0, fmt.Errorf("zcard %s: %w", key, err)
	}
	if total == 0 || page == 0 || pageSize == 0 {
		return nil, uint32(total), nil
	}

	// 请求字段是 uint32:先提升到 uint64 再相乘,避免高页码环绕回榜首。
	// 内部调用的 pageSize 也可能为 MaxUint32,乘积可超过 MaxInt64;
	// 先按实际榜长判断空页,再转换 Redis 使用的有符号下标。
	start := uint64(page-1) * uint64(pageSize)
	if start >= uint64(total) {
		return nil, uint32(total), nil
	}
	stop := min(start+uint64(pageSize)-1, uint64(total)-1)

	members, err := r.rdb.ZRevRangeWithScores(ctx, key, int64(start), int64(stop)).Result()
	if err != nil {
		return nil, 0, fmt.Errorf("zrevrange %s: %w", key, err)
	}

	entries := make([]RankEntry, 0, len(members))
	for i, z := range members {
		guildID, _ := strconv.ParseUint(fmt.Sprintf("%v", z.Member), 10, 64)
		entries = append(entries, RankEntry{
			GuildID: guildID,
			Score:   int64(z.Score),
			Rank:    uint32(start) + uint32(i) + 1,
		})
	}
	return entries, uint32(total), nil
}

// GetGuildRank returns a single guild's rank and score. zoneID=0 means global. Rank is 1-based (0 = not ranked).
func (r *GuildRepo) GetGuildRank(ctx context.Context, guildID uint64, zoneID uint32) (RankEntry, error) {
	key := guildRankKey
	if zoneID > 0 {
		key = zoneRankKey(zoneID)
	}

	member := fmt.Sprintf("%d", guildID)

	// ZREVRANK: 0-based index in descending order
	rank, err := r.rdb.ZRevRank(ctx, key, member).Result()
	if err == redis.Nil {
		return RankEntry{}, nil // not in ranking
	}
	if err != nil {
		return RankEntry{}, fmt.Errorf("zrevrank guild %d: %w", guildID, err)
	}

	score, err := r.rdb.ZScore(ctx, key, member).Result()
	if err != nil {
		return RankEntry{}, fmt.Errorf("zscore guild %d: %w", guildID, err)
	}

	return RankEntry{
		GuildID: guildID,
		Score:   int64(score),
		Rank:    uint32(rank) + 1, // 1-based
	}, nil
}
