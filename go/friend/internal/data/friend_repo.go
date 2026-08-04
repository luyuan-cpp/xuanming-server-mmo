package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrSenderFriendsFull   = errors.New("sender friend list full")
	ErrAcceptorFriendsFull = errors.New("acceptor friend list full")
	// ErrRequestNotFound:不存在处于 pending 状态的好友申请。
	// AcceptFriend 以此 fail-closed —— 没有申请就不允许建立关系。
	ErrRequestNotFound = errors.New("no pending friend request")
	// ErrFriendCapacityMigrationNotReady:friend_capacity 虽可能已经建表，但历史
	// friend 边尚未与 ready 标记原子提交；此时任何 0 初始化都会破坏硬上限。
	ErrFriendCapacityMigrationNotReady = errors.New("friend capacity migration is not ready")
)

const friendCapacityMigrationKey = "friend_capacity_backfill_v1"

type FriendEntry struct {
	FriendPlayerID uint64 `json:"friend_player_id"`
	SinceMs        int64  `json:"since_ms"`
	LastActiveMs   int64  `json:"last_active_ms"`
}

type FriendRequestEntry struct {
	FromPlayerID  uint64 `json:"from_player_id"`
	ToPlayerID    uint64 `json:"to_player_id"`
	RequestTimeMs int64  `json:"request_time_ms"`
	Status        int32  `json:"status"` // 1=pending, 2=accepted, 3=rejected
}

// FriendRepo provides cache-aside access to friend data.
type FriendRepo struct {
	rdb         *redis.Client
	db          *sql.DB
	defaultTTL  time.Duration
	cacheLoadMu sync.Mutex
}

func NewFriendRepo(rdb *redis.Client, db *sql.DB, defaultTTL time.Duration) *FriendRepo {
	return &FriendRepo{
		rdb:        rdb,
		db:         db,
		defaultTTL: defaultTTL,
	}
}

// RequireFriendCapacityReady 是 friend 写路径的 durable readiness gate。
// CREATE TABLE 在 MySQL 中会隐式提交，所以“表存在”不能证明历史容量已经回填。
func (r *FriendRepo) RequireFriendCapacityReady(ctx context.Context) error {
	return requireFriendCapacityReady(ctx, r.db, false)
}

type migrationStateQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func requireFriendCapacityReady(ctx context.Context, queryer migrationStateQuerier, lock bool) error {
	query := "SELECT state FROM guild_schema_migration WHERE migration_key = ?"
	if lock {
		// 与迁移把 marker 改回 pending 的 UPDATE 互斥；共享锁持有到好友事务提交，
		// 因此 pending 一旦可见，后续事务都无法越过门禁。
		query += " FOR SHARE"
	}
	var state string
	err := queryer.QueryRowContext(ctx, query, friendCapacityMigrationKey).Scan(&state)
	if err == sql.ErrNoRows {
		return friendCapacityMigrationStateError("", false)
	}
	if err != nil {
		return fmt.Errorf("read friend capacity migration gate: %w", err)
	}
	return friendCapacityMigrationStateError(state, true)
}

func friendCapacityMigrationStateError(state string, found bool) error {
	if found && state == "ready" {
		return nil
	}
	if !found {
		state = "missing"
	}
	return fmt.Errorf("%w: state=%s", ErrFriendCapacityMigrationNotReady, state)
}

// ── Redis key helpers ──────────────────────────────────────────

func friendListKey(playerID uint64) string {
	// v2 不读取旧版无 generation-CAS 的缓存，避免升级后继续命中已由旧
	// cache-aside 竞态回填的 30 分钟陈旧快照。
	return fmt.Sprintf("friends:v2:%d", playerID)
}

func pendingRequestsKey(playerID uint64) string {
	return fmt.Sprintf("friend_req:v2:%d", playerID)
}

func onlineKey(playerID uint64) string {
	return fmt.Sprintf("friend:online:%d", playerID)
}

const onlineTTL = 60 * time.Second

var invalidateFriendCacheScript = redis.NewScript(`
redis.call("INCR", KEYS[1])
redis.call("DEL", KEYS[2])
return 1
`)

var fillFriendCacheScript = redis.NewScript(`
local generation = redis.call("GET", KEYS[1])
if not generation then generation = "0" end
if generation ~= ARGV[1] then return 0 end
if tonumber(ARGV[3]) > 0 then
    redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
else
    redis.call("SET", KEYS[2], ARGV[2])
end
return 1
`)

func friendCacheGenerationKey(cacheKey string) string {
	return cacheKey + ":generation"
}

// ── Read (cache-aside + singleflight) ──────────────────────────

func (r *FriendRepo) GetFriendList(ctx context.Context, playerID uint64) ([]FriendEntry, error) {
	return loadVersionedFriendCache(
		ctx, r, friendListKey(playerID),
		func(ctx context.Context) ([]FriendEntry, error) { return r.loadFriendListFromMySQL(ctx, playerID) },
	)
}

func (r *FriendRepo) GetPendingRequests(ctx context.Context, playerID uint64) ([]FriendRequestEntry, error) {
	return loadVersionedFriendCache(
		ctx, r, pendingRequestsKey(playerID),
		func(ctx context.Context) ([]FriendRequestEntry, error) {
			return r.loadPendingRequestsFromMySQL(ctx, playerID)
		},
	)
}

// ── Write operations ───────────────────────────────────────────

func (r *FriendRepo) AddFriendRequest(ctx context.Context, fromPlayerID, toPlayerID uint64) error {
	now := time.Now().UnixMilli()
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status)
		 VALUES (?, ?, ?, 1)
		 ON DUPLICATE KEY UPDATE status=1, request_time_ms=VALUES(request_time_ms)`,
		fromPlayerID, toPlayerID, now)
	if err != nil {
		return err
	}
	// generation + Lua CAS 会阻止并发 miss 把写入前读到的旧 DB 快照回填。
	return r.invalidateCaches(ctx, pendingRequestsKey(toPlayerID))
}

func (r *FriendRepo) AcceptFriend(ctx context.Context, fromPlayerID, toPlayerID uint64, maxFriends uint32) error {
	if fromPlayerID == toPlayerID {
		return fmt.Errorf("cannot accept self as friend")
	}
	// 事务外预检只用于避免恶意无申请调用制造无界 capacity 空行；真正的安全
	// 门禁仍是事务内 SELECT ... FOR UPDATE 的二次校验。
	var pending int
	if err := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM friend_request WHERE from_player_id=? AND to_player_id=? AND status=1",
		fromPlayerID, toPlayerID).Scan(&pending); err != nil {
		if err == sql.ErrNoRows {
			return ErrRequestNotFound
		}
		return err
	}
	cacheKeys := []string{
		friendListKey(fromPlayerID),
		friendListKey(toPlayerID),
		pendingRequestsKey(toPlayerID),
	}
	// capacity 行在主事务前逐行、按 player_id 顺序自动提交地确保存在。
	// 把双方 INSERT IGNORE 放在同一事务里时，多个请求各持有自己的新行又争抢
	// 同一个接收者行，真实 InnoDB 会形成 insert-intention 死锁。缺行不能猜 0：
	// ensure 会先过 ready gate，再用普通一致性读从 friend 权威边计算初值。
	if err := r.ensureFriendCapacityRows(ctx, fromPlayerID, toPlayerID); err != nil {
		return err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireFriendCapacityReady(ctx, tx, true); err != nil {
		return err
	}

	// 先用主键锁定并验证申请，但暂不修改 status。若大量申请同时指向一个玩家，
	// 过早的 status=1->2 会让事务并发删除/插入同一 idx_to_player 前缀，真实
	// InnoDB 压测可触发 1213。待下面共享 capacity 行把它们串行化后再更新状态。
	var requestStatus int32
	if err := tx.QueryRowContext(ctx,
		"SELECT status FROM friend_request WHERE from_player_id=? AND to_player_id=? FOR UPDATE",
		fromPlayerID, toPlayerID).Scan(&requestStatus); err != nil {
		if err == sql.ErrNoRows {
			return ErrRequestNotFound
		}
		return err
	}
	if requestStatus != 1 {
		return ErrRequestNotFound
	}

	// 好友数上限由始终存在的 friend_capacity 行保护，不再依赖
	// COUNT ... FOR UPDATE 的 gap-lock 行为（它会随隔离级别变化）。
	// 双方始终按 player_id 升序建行/加锁，避免反向申请同时接受时 ABBA。
	firstID, secondID := fromPlayerID, toPlayerID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT player_id, friend_count FROM friend_capacity WHERE player_id IN (?, ?) ORDER BY player_id FOR UPDATE",
		firstID, secondID)
	if err != nil {
		return err
	}
	counts := make(map[uint64]uint32, 2)
	for rows.Next() {
		var playerID uint64
		var count uint32
		if err := rows.Scan(&playerID, &count); err != nil {
			rows.Close()
			return err
		}
		counts[playerID] = count
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(counts) != 2 {
		return fmt.Errorf("friend capacity rows missing for players %d/%d", firstID, secondID)
	}
	if counts[fromPlayerID] >= maxFriends {
		return ErrSenderFriendsFull
	}
	if counts[toPlayerID] >= maxFriends {
		return ErrAcceptorFriendsFull
	}

	// capacity 锁已经让共享任一玩家的 AcceptFriend 串行化，现在才推进申请
	// 状态，既保留 RowsAffected 的 fail-closed 门禁，也避免二级索引死锁。
	res, err := tx.ExecContext(ctx,
		"UPDATE friend_request SET status=2 WHERE from_player_id=? AND to_player_id=? AND status=1",
		fromPlayerID, toPlayerID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRequestNotFound
	}

	// 分向插入并按实际 RowsAffected 更新各自容量；部分历史单向关系会被补齐，
	// 已存在的方向不会重复计数。
	now := time.Now().UnixMilli()
	for _, edge := range [][2]uint64{{fromPlayerID, toPlayerID}, {toPlayerID, fromPlayerID}} {
		result, err := tx.ExecContext(ctx,
			"INSERT IGNORE INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, ?)",
			edge[0], edge[1], now)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted > 1 {
			return fmt.Errorf("unexpected friend insert count %d for %d -> %d", inserted, edge[0], edge[1])
		}
		if inserted == 1 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE friend_capacity SET friend_count = friend_count + 1 WHERE player_id = ?",
				edge[0]); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return r.invalidateCaches(ctx, cacheKeys...)
}

func (r *FriendRepo) RejectFriend(ctx context.Context, fromPlayerID, toPlayerID uint64) error {
	key := pendingRequestsKey(toPlayerID)
	result, err := r.db.ExecContext(ctx,
		"UPDATE friend_request SET status=3 WHERE from_player_id=? AND to_player_id=? AND status=1",
		fromPlayerID, toPlayerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrRequestNotFound
	}
	return r.invalidateCaches(ctx, key)
}

func (r *FriendRepo) RemoveFriend(ctx context.Context, playerID, targetPlayerID uint64) error {
	if playerID == targetPlayerID {
		return nil
	}
	cacheKeys := []string{friendListKey(playerID), friendListKey(targetPlayerID)}
	if err := r.ensureFriendCapacityRows(ctx, playerID, targetPlayerID); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireFriendCapacityReady(ctx, tx, true); err != nil {
		return err
	}
	firstID, secondID := playerID, targetPlayerID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT player_id FROM friend_capacity WHERE player_id IN (?, ?) ORDER BY player_id FOR UPDATE",
		firstID, secondID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ignored uint64
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, edge := range [][2]uint64{{playerID, targetPlayerID}, {targetPlayerID, playerID}} {
		result, err := tx.ExecContext(ctx,
			"DELETE FROM friend WHERE player_id=? AND friend_player_id=?", edge[0], edge[1])
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted == 1 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE friend_capacity SET friend_count = IF(friend_count > 0, friend_count - 1, 0) WHERE player_id = ?",
				edge[0]); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return r.invalidateCaches(ctx, cacheKeys...)
}

func (r *FriendRepo) invalidateCaches(ctx context.Context, keys ...string) error {
	var result error
	for _, key := range keys {
		if _, err := invalidateFriendCacheScript.Run(
			ctx,
			r.rdb,
			[]string{friendCacheGenerationKey(key), key},
		).Int(); err != nil {
			result = errors.Join(result, fmt.Errorf("invalidate friend cache key %s: %w", key, err))
		}
	}
	return result
}

func (r *FriendRepo) ensureFriendCapacityRows(ctx context.Context, playerIDs ...uint64) error {
	if len(playerIDs) == 0 {
		return nil
	}
	// 即使进程启动时检查过，也在写路径重查：运维重跑迁移会先把状态持久化为
	// pending。服务若未按要求停写，这里仍拒绝制造 0 行。
	if err := r.RequireFriendCapacityReady(ctx); err != nil {
		return err
	}
	firstID, secondID := playerIDs[0], playerIDs[0]
	if len(playerIDs) > 1 {
		secondID = playerIDs[1]
	}
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}
	ids := []uint64{firstID}
	if secondID != firstID {
		ids = append(ids, secondID)
	}
	for _, playerID := range ids {
		// ready 库里的缺行通常是从未有好友的新玩家；若历史库曾半迁移，仍从
		// friend 权威边计算，绝不能猜 0。普通一致性读不持有 gap lock；所有新
		// 写路径都先 ensure 同一 capacity 行，再在事务里加锁并更新计数。
		var authoritativeCount uint32
		if err := r.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM friend WHERE player_id = ?", playerID).Scan(&authoritativeCount); err != nil {
			return fmt.Errorf("count authoritative friends for player %d: %w", playerID, err)
		}
		if _, err := r.db.ExecContext(ctx,
			"INSERT IGNORE INTO friend_capacity (player_id, friend_count) VALUES (?, ?)",
			playerID, authoritativeCount); err != nil {
			return fmt.Errorf("ensure friend capacity row for player %d: %w", playerID, err)
		}
	}
	return nil
}

// AreFriends checks if two players are friends.
func (r *FriendRepo) AreFriends(ctx context.Context, playerID, targetID uint64) (bool, error) {
	friends, err := r.GetFriendList(ctx, playerID)
	if err != nil {
		return false, err
	}
	for _, f := range friends {
		if f.FriendPlayerID == targetID {
			return true, nil
		}
	}
	return false, nil
}

// HasPendingRequest checks if there's already a pending request between two players.
func (r *FriendRepo) HasPendingRequest(ctx context.Context, fromID, toID uint64) (bool, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND to_player_id=? AND status=1",
		fromID, toID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ── Online status (Redis key with TTL) ─────────────────────────

func (r *FriendRepo) SetPlayerOnline(ctx context.Context, playerID uint64, gateNodeID uint32) error {
	return r.rdb.Set(ctx, onlineKey(playerID), gateNodeID, onlineTTL).Err()
}

func (r *FriendRepo) SetPlayerOffline(ctx context.Context, playerID uint64) error {
	return r.rdb.Del(ctx, onlineKey(playerID)).Err()
}

func (r *FriendRepo) BatchGetOnlineStatus(ctx context.Context, playerIDs []uint64) (map[uint64]bool, error) {
	if len(playerIDs) == 0 {
		return map[uint64]bool{}, nil
	}
	pipe := r.rdb.Pipeline()
	cmds := make(map[uint64]*redis.IntCmd, len(playerIDs))
	for _, id := range playerIDs {
		cmds[id] = pipe.Exists(ctx, onlineKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("batch online status: %w", err)
	}
	result := make(map[uint64]bool, len(playerIDs))
	for id, cmd := range cmds {
		result[id] = cmd.Val() > 0
	}
	return result, nil
}

// ── Cache helpers ──────────────────────────────────────────────

// loadVersionedFriendCache 阻止经典 cache-aside 竞态：reader miss 后读到旧
// MySQL，writer 提交并失效缓存，reader 最后才 SET 旧快照。reader 只有在 DB
// 读取前后 generation 未变化时才能回填；写路径提交后原子 INCR+DEL。
func loadVersionedFriendCache[T any](
	ctx context.Context,
	r *FriendRepo,
	cacheKey string,
	loader func(context.Context) (T, error),
) (T, error) {
	var zero T
	readCache := func() (T, bool, error) {
		payload, err := r.rdb.Get(ctx, cacheKey).Bytes()
		if err == redis.Nil {
			return zero, false, nil
		}
		if err != nil {
			return zero, false, fmt.Errorf("read friend cache %s: %w", cacheKey, err)
		}
		var value T
		if err := json.Unmarshal(payload, &value); err != nil {
			return zero, false, fmt.Errorf("decode friend cache %s: %w", cacheKey, err)
		}
		return value, true, nil
	}

	if value, found, err := readCache(); err != nil || found {
		return value, err
	}
	r.cacheLoadMu.Lock()
	defer r.cacheLoadMu.Unlock()
	if value, found, err := readCache(); err != nil || found {
		return value, err
	}

	generation, err := r.rdb.Get(ctx, friendCacheGenerationKey(cacheKey)).Result()
	if err == redis.Nil {
		generation = "0"
	} else if err != nil {
		return zero, fmt.Errorf("read friend cache generation %s: %w", cacheKey, err)
	}
	value, err := loader(ctx)
	if err != nil {
		return zero, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return zero, fmt.Errorf("encode friend cache %s: %w", cacheKey, err)
	}
	if _, err := fillFriendCacheScript.Run(
		ctx,
		r.rdb,
		[]string{friendCacheGenerationKey(cacheKey), cacheKey},
		generation,
		payload,
		r.defaultTTL.Milliseconds(),
	).Int(); err != nil {
		return zero, fmt.Errorf("fill friend cache %s: %w", cacheKey, err)
	}
	return value, nil
}

// ── MySQL queries ──────────────────────────────────────────────

func (r *FriendRepo) loadFriendListFromMySQL(ctx context.Context, playerID uint64) ([]FriendEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT friend_player_id, since_ms FROM friend WHERE player_id = ?", playerID)
	if err != nil {
		return nil, fmt.Errorf("query friends %d: %w", playerID, err)
	}
	defer rows.Close()

	var friends []FriendEntry
	for rows.Next() {
		var f FriendEntry
		if err := rows.Scan(&f.FriendPlayerID, &f.SinceMs); err != nil {
			return nil, fmt.Errorf("scan friend: %w", err)
		}
		friends = append(friends, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if friends == nil {
		friends = []FriendEntry{}
	}
	return friends, nil
}

func (r *FriendRepo) loadPendingRequestsFromMySQL(ctx context.Context, playerID uint64) ([]FriendRequestEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT from_player_id, to_player_id, request_time_ms, status FROM friend_request WHERE to_player_id = ? AND status = 1",
		playerID)
	if err != nil {
		return nil, fmt.Errorf("query pending requests %d: %w", playerID, err)
	}
	defer rows.Close()

	var requests []FriendRequestEntry
	for rows.Next() {
		var req FriendRequestEntry
		if err := rows.Scan(&req.FromPlayerID, &req.ToPlayerID, &req.RequestTimeMs, &req.Status); err != nil {
			return nil, fmt.Errorf("scan friend request: %w", err)
		}
		requests = append(requests, req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if requests == nil {
		requests = []FriendRequestEntry{}
	}
	return requests, nil
}
