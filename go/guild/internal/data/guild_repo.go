package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"guild/internal/constants"
)

// 写路径哨兵错误:logic 层据此映射到对玩家的提示,而不是笼统的内部错误。
var (
	// ErrGuildGone:目标公会在 MySQL 里不存在(可能刚被解散)。
	ErrGuildGone = errors.New("guild does not exist")
	// ErrGuildFull:事务内按权威行数判定已满。
	ErrGuildFull = errors.New("guild is full")
	// ErrPlayerAlreadyInGuild:唯一索引 uk_player 拒绝了跨公会重复 membership。
	ErrPlayerAlreadyInGuild = errors.New("player already belongs to a guild")
	// ErrAnnouncementForbidden:MySQL 权威 membership 不存在或角色不是 officer/leader。
	ErrAnnouncementForbidden = errors.New("guild announcement update is not authorized")
	// ErrLegacyRankSnapshotMismatch:迁移时 Redis 旧榜并非 MySQL 公会全集，禁止
	// 把缺失项的默认 0 分固化为新权威值。
	ErrLegacyRankSnapshotMismatch = errors.New("legacy guild rank snapshot does not match MySQL guild set")
)

// GuildData is the persistence-layer representation of a guild (stored in Redis + MySQL).
type GuildData struct {
	GuildID      uint64 `json:"guild_id"`
	Name         string `json:"name"`
	LeaderID     uint64 `json:"leader_id"`
	Level        uint32 `json:"level"`
	Announcement string `json:"announcement"`
	CreateTimeMs int64  `json:"create_time_ms"`
	MaxMembers   uint32 `json:"max_members"`
	ZoneID       uint32 `json:"zone_id"`
	// Score 是公会排行分的 MySQL 权威副本;Redis ZSET 只是读加速层,
	// 丢失/分叉后可由 RebuildRanks 从这里全量重建。
	Score   int64        `json:"score"`
	Members []MemberData `json:"members"`
}

type MemberData struct {
	PlayerID     uint64 `json:"player_id"`
	Role         uint32 `json:"role"`
	JoinTimeMs   int64  `json:"join_time_ms"`
	LastActiveMs int64  `json:"last_active_ms"`
	Contribution uint64 `json:"contribution"`
	Online       bool   `json:"-"` // 仅内存，不入库
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
	guildRankLockKey       = "guild_rank:maintenance_lock"
	guildScoreMigrationKey = "guild_score_redis_backfill_v1"
	guildRankLockTTL       = 5 * time.Minute
	guildRankLockWait      = 5 * time.Second
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

// CreateGuild 在单个事务里写入公会行 + 会长成员行。
// 任何一步失败整体回滚 —— 不允许出现「有公会无会长」或「有会长无公会」的半态。
func (r *GuildRepo) CreateGuild(ctx context.Context, guild *GuildData) error {
	if len(guild.Members) != 1 {
		return fmt.Errorf("create guild %d: expected exactly one founding member, got %d", guild.GuildID, len(guild.Members))
	}
	leader := guild.Members[0]

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO guild (guild_id, name, leader_id, level, announcement, create_time_ms, max_members, zone_id, score)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		guild.GuildID, guild.Name, guild.LeaderID, guild.Level,
		guild.Announcement, guild.CreateTimeMs, guild.MaxMembers, guild.ZoneID); err != nil {
		return fmt.Errorf("insert guild %d: %w", guild.GuildID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		guild.GuildID, leader.PlayerID, leader.Role, leader.JoinTimeMs, leader.LastActiveMs); err != nil {
		if isDuplicateKey(err) {
			return ErrPlayerAlreadyInGuild
		}
		return fmt.Errorf("insert founding member %d: %w", leader.PlayerID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return errors.Join(
		r.invalidateGuildCache(ctx, guild.GuildID),
		r.invalidatePlayerGuildCache(ctx, leader.PlayerID),
	)
}

// AddMember 在单个事务里完成「锁公会行 → 权威行数判满 → 插入成员」。
// 满员判定必须在事务内数 guild_member 行:读缓存里 len(Members) 的旧实现在两个
// 并发入会时会双双通过检查,把公会挤超编。
func (r *GuildRepo) AddMember(ctx context.Context, guildID, playerID uint64, role uint32) error {
	now := time.Now().UnixMilli()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// FOR UPDATE 锁住公会行:并发 AddMember 在此串行化,行数判定不再有竞态窗口;
	// 同时兼做存在性校验(公会刚被解散 → ErrGuildGone)。
	var maxMembers uint32
	err = tx.QueryRowContext(ctx,
		"SELECT max_members FROM guild WHERE guild_id = ? FOR UPDATE", guildID).Scan(&maxMembers)
	if err == sql.ErrNoRows {
		return ErrGuildGone
	}
	if err != nil {
		return fmt.Errorf("lock guild %d: %w", guildID, err)
	}

	var memberCount uint32
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_member WHERE guild_id = ?", guildID).Scan(&memberCount); err != nil {
		return fmt.Errorf("count members of guild %d: %w", guildID, err)
	}
	if memberCount >= maxMembers {
		return ErrGuildFull
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		guildID, playerID, role, now, now); err != nil {
		if isDuplicateKey(err) {
			return ErrPlayerAlreadyInGuild
		}
		return fmt.Errorf("insert member %d into guild %d: %w", playerID, guildID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return errors.Join(
		r.invalidateGuildCache(ctx, guildID),
		r.invalidatePlayerGuildCache(ctx, playerID),
	)
}

// RemoveMember 删除成员行并失效相关缓存。成员行本就不存在也算成功(幂等)。
func (r *GuildRepo) RemoveMember(ctx context.Context, guildID, playerID uint64) error {
	if _, err := r.db.ExecContext(ctx,
		"DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?", guildID, playerID); err != nil {
		return fmt.Errorf("remove member %d from guild %d: %w", playerID, guildID, err)
	}
	return errors.Join(
		r.invalidateGuildCache(ctx, guildID),
		r.invalidatePlayerGuildCache(ctx, playerID),
	)
}

// UpdateAnnouncementAuthorized 在一个 MySQL 事务内锁定公会与操作者 membership，
// 以权威 role 复核授权后才更新公告。Redis GuildData 只用于读加速，绝不参与授权。
func (r *GuildRepo) UpdateAnnouncementAuthorized(ctx context.Context, guildID, playerID uint64, announcement string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 与 AddMember/DeleteGuild/UpdateGuildScore 保持统一锁序：先 guild，再 member。
	var lockedGuildID uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT guild_id FROM guild WHERE guild_id = ? FOR UPDATE", guildID).Scan(&lockedGuildID); err != nil {
		if err == sql.ErrNoRows {
			return ErrGuildGone
		}
		return fmt.Errorf("lock guild %d for announcement: %w", guildID, err)
	}

	var role uint32
	if err := tx.QueryRowContext(ctx,
		"SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE",
		guildID, playerID).Scan(&role); err != nil {
		if err == sql.ErrNoRows {
			return ErrAnnouncementForbidden
		}
		return fmt.Errorf("lock announcement operator %d in guild %d: %w", playerID, guildID, err)
	}
	if !canSetAnnouncement(role) {
		return ErrAnnouncementForbidden
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE guild SET announcement = ? WHERE guild_id = ?", announcement, guildID); err != nil {
		return fmt.Errorf("update announcement of guild %d: %w", guildID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return r.invalidateGuildCache(ctx, guildID)
}

func canSetAnnouncement(role uint32) bool {
	return role == constants.RoleOfficer || role == constants.RoleLeader
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

func (r *GuildRepo) DeleteGuild(ctx context.Context, guildID uint64) error {
	memberIDs, err := r.deleteGuildFromMySQL(ctx, guildID)
	if err != nil {
		return err
	}
	cacheErr := r.invalidateGuildCache(ctx, guildID)
	for _, playerID := range memberIDs {
		cacheErr = errors.Join(cacheErr, r.invalidatePlayerGuildCache(ctx, playerID))
	}
	return cacheErr
}

// ── MySQL queries ──────────────────────────────────────────────

func (r *GuildRepo) loadGuildFromMySQL(ctx context.Context, guildID uint64) (*GuildData, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT guild_id, name, leader_id, level, announcement, create_time_ms, max_members, zone_id, score FROM guild WHERE guild_id = ?",
		guildID)

	var guild GuildData
	err := row.Scan(&guild.GuildID, &guild.Name, &guild.LeaderID, &guild.Level,
		&guild.Announcement, &guild.CreateTimeMs, &guild.MaxMembers, &guild.ZoneID, &guild.Score)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query guild %d: %w", guildID, err)
	}

	// Load members
	rows, err := r.db.QueryContext(ctx,
		"SELECT player_id, role, join_time_ms, last_active_ms, contribution FROM guild_member WHERE guild_id = ?",
		guildID)
	if err != nil {
		return nil, fmt.Errorf("query guild members %d: %w", guildID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var m MemberData
		if err := rows.Scan(&m.PlayerID, &m.Role, &m.JoinTimeMs, &m.LastActiveMs, &m.Contribution); err != nil {
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

func (r *GuildRepo) deleteGuildFromMySQL(ctx context.Context, guildID uint64) ([]uint64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 与 AddMember/UpdateGuildScore 保持统一锁序：先 guild 行，再 membership。
	// 反向锁序会在并发入会/解散时形成可避免的 InnoDB deadlock。
	var lockedGuildID uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT guild_id FROM guild WHERE guild_id = ? FOR UPDATE", guildID).Scan(&lockedGuildID); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrGuildGone
		}
		return nil, err
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT player_id FROM guild_member WHERE guild_id = ? FOR UPDATE", guildID)
	if err != nil {
		return nil, err
	}
	var memberIDs []uint64
	for rows.Next() {
		var playerID uint64
		if err := rows.Scan(&playerID); err != nil {
			rows.Close()
			return nil, err
		}
		memberIDs = append(memberIDs, playerID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM guild_member WHERE guild_id = ?", guildID); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM guild WHERE guild_id = ?", guildID)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrGuildGone
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return memberIDs, nil
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
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

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var authoritativeZoneID uint32
	if err := tx.QueryRowContext(ctx,
		"SELECT zone_id FROM guild WHERE guild_id = ? FOR UPDATE", guildID).Scan(&authoritativeZoneID); err != nil {
		if err == sql.ErrNoRows {
			return ErrGuildGone
		}
		return fmt.Errorf("lock guild %d for score update: %w", guildID, err)
	}
	if zoneID != 0 && zoneID != authoritativeZoneID {
		logx.Errorf("UpdateGuildScore: caller zone %d ignored; guild %d belongs to zone %d",
			zoneID, guildID, authoritativeZoneID)
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE guild SET score = ? WHERE guild_id = ?", score, guildID)
	if err != nil {
		return fmt.Errorf("persist score of guild %d: %w", guildID, err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read score update result for guild %d: %w", guildID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
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

// MigrateLegacyRankScores 在 schema migration 标记仍为 pending 时，把旧版唯一
// 权威 Redis guild_rank 分数一次性回填到 MySQL。完成标记与更新同事务提交，重跑幂等。
func (r *GuildRepo) MigrateLegacyRankScores(ctx context.Context) error {
	var state string
	if err := r.db.QueryRowContext(ctx,
		"SELECT state FROM guild_schema_migration WHERE migration_key = ?", guildScoreMigrationKey).Scan(&state); err != nil {
		return fmt.Errorf("read guild score migration state: %w", err)
	}
	if state == "done" {
		return nil
	}

	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	entries, err := r.rdb.ZRangeWithScores(ctx, guildRankKey, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("read legacy guild rank for migration: %w", err)
	}
	legacyScores := make(map[uint64]int64, len(entries))
	for _, entry := range entries {
		guildID, err := strconv.ParseUint(fmt.Sprint(entry.Member), 10, 64)
		if err != nil {
			return fmt.Errorf("parse legacy rank guild id %v: %w", entry.Member, err)
		}
		if math.Trunc(entry.Score) != entry.Score {
			return fmt.Errorf("legacy rank score for guild %d is not an integer: %v", guildID, entry.Score)
		}
		if _, duplicate := legacyScores[guildID]; duplicate {
			return fmt.Errorf("%w: duplicate parsed guild_id=%d in Redis members", ErrLegacyRankSnapshotMismatch, guildID)
		}
		legacyScores[guildID] = int64(entry.Score)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Redis lock 已串行化多实例，但仍在事务内复查标记，保证重启/重入安全。
	if err := tx.QueryRowContext(ctx,
		"SELECT state FROM guild_schema_migration WHERE migration_key = ? FOR UPDATE",
		guildScoreMigrationKey).Scan(&state); err != nil {
		return err
	}
	if state == "done" {
		return tx.Commit()
	}

	// migration pending 时，旧 guild_rank 必须是 MySQL 公会集合的完整快照。
	// 尤其 MySQL 非空而 ZSET 为空不能被解释成“大家都是 0 分”，否则一次 Redis
	// 丢数据就会在标 done 后永久固化。FOR UPDATE 在核对/回填期间冻结现有公会行。
	rows, err := tx.QueryContext(ctx, "SELECT guild_id FROM guild ORDER BY guild_id FOR UPDATE")
	if err != nil {
		return fmt.Errorf("lock guild ids for rank migration: %w", err)
	}
	mysqlGuildIDs := make([]uint64, 0, len(legacyScores))
	for rows.Next() {
		var guildID uint64
		if err := rows.Scan(&guildID); err != nil {
			rows.Close()
			return fmt.Errorf("scan guild id for rank migration: %w", err)
		}
		mysqlGuildIDs = append(mysqlGuildIDs, guildID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	redisGuildIDs := make([]uint64, 0, len(legacyScores))
	for guildID := range legacyScores {
		redisGuildIDs = append(redisGuildIDs, guildID)
	}
	missingInRedis, ghostInRedis := diffGuildIDSets(mysqlGuildIDs, redisGuildIDs)
	if len(missingInRedis) > 0 || len(ghostInRedis) > 0 {
		return fmt.Errorf(
			"%w: mysql_guilds=%d redis_unique_guilds=%d missing_in_redis=%s ghost_in_redis=%s",
			ErrLegacyRankSnapshotMismatch,
			len(mysqlGuildIDs),
			len(redisGuildIDs),
			summarizeGuildIDs(missingInRedis, 50),
			summarizeGuildIDs(ghostInRedis, 50),
		)
	}

	backfilled := 0
	for guildID, score := range legacyScores {
		result, err := tx.ExecContext(ctx,
			"UPDATE guild SET score = ? WHERE guild_id = ?", score, guildID)
		if err != nil {
			return fmt.Errorf("backfill score for guild %d: %w", guildID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected > 0 {
			backfilled++
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE guild_schema_migration SET state='done', completed_at=CURRENT_TIMESTAMP WHERE migration_key=?",
		guildScoreMigrationKey); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	logx.Infof("legacy guild rank migration completed: redis_entries=%d changed_rows=%d", len(entries), backfilled)
	return nil
}

func diffGuildIDSets(mysqlGuildIDs, redisGuildIDs []uint64) (missingInRedis, ghostInRedis []uint64) {
	mysqlSet := make(map[uint64]struct{}, len(mysqlGuildIDs))
	redisSet := make(map[uint64]struct{}, len(redisGuildIDs))
	for _, guildID := range mysqlGuildIDs {
		mysqlSet[guildID] = struct{}{}
	}
	for _, guildID := range redisGuildIDs {
		redisSet[guildID] = struct{}{}
	}
	for guildID := range mysqlSet {
		if _, found := redisSet[guildID]; !found {
			missingInRedis = append(missingInRedis, guildID)
		}
	}
	for guildID := range redisSet {
		if _, found := mysqlSet[guildID]; !found {
			ghostInRedis = append(ghostInRedis, guildID)
		}
	}
	slices.Sort(missingInRedis)
	slices.Sort(ghostInRedis)
	return missingInRedis, ghostInRedis
}

func summarizeGuildIDs(ids []uint64, limit int) string {
	if len(ids) <= limit {
		return fmt.Sprint(ids)
	}
	return fmt.Sprintf("%v ... (%d total)", ids[:limit], len(ids))
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

// RemoveGuildFromRank removes a guild from both global and per-zone ranking ZSETs.
func (r *GuildRepo) RemoveGuildFromRank(ctx context.Context, guildID uint64, zoneID uint32) error {
	release, err := r.acquireRankLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	pipe := r.rdb.Pipeline()
	pipe.ZRem(ctx, guildRankKey, guildID)
	if zoneID > 0 {
		pipe.ZRem(ctx, zoneRankKey(zoneID), guildID)
	}
	_, err = pipe.Exec(ctx)
	return err
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

	start := int64((page - 1) * pageSize)
	stop := start + int64(pageSize) - 1

	members, err := r.rdb.ZRevRangeWithScores(ctx, key, start, stop).Result()
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
