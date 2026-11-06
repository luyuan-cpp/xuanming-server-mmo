package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	storepb "proto/player_store"
	"shared/accountdirectory"
)

// player_store 的访问(docs/design/player-save-direct-commit.md §6)。
// 存盘本身在 go/db;data_service 只做建角初始行、账本只读、交易冻结证据、判死释放,按 player_route 选库,走存档库连接池。

const (
	playerStoreOwnerKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	// 钱包行:货币、资产通道账本、收件箱账本、限购(docs/design/player-save-direct-commit.md §1.1)。账本读只要它。
	playerStoreWalletKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_WALLET)

	// 展示资料:等级在角色行,职业、名字、外观、性别在资料行。
	playerStoreCharacterKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_CHARACTER)
	playerStoreProfileKind   = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE)

	// 证据读取不是批量导出;超过正常玩家存档预算显式拒绝,避免无限读 blob。
	playerStoreProofMaxBytes = 16 << 20

	// PlayerDisplayBatchLimit 是一次读展示资料的上限。在线目录一批 SCAN 最多 1024 个 key。
	PlayerDisplayBatchLimit = 1024
)

// PlayerAssetRow 是写权行和钱包行(kWallet)的一次一致读。
type PlayerAssetRow struct {
	OwnerEpoch uint64
	SaveSeq    uint64
	// Released:持有者已最终存盘并释放,或者还没有人认领过。
	Released bool
	// Player 只有钱包行的字段(货币、资产通道账本、收件箱账本、限购)和 player_id;没有钱包行时只有 player_id。
	// 背包在另一行(kBag),这里读不到:账本读不需要它。
	Player *dbpb.PlayerDatabase
}

// playerStoreReadLimit 是账本和展示资料读最多占的连接数:不超过池的一半。
func playerStoreReadLimit(maxOpenConn int) int {
	if limit := maxOpenConn / 2; limit > 0 {
		return limit
	}
	return 1
}

// acquirePlayerStoreRead 取一个读名额,释放函数必须调用。好友列表、在线目录、队伍视图原来走 Redis,读量一大会把存档库的池占满,
// 挤掉判死和建角的写。等不到名额就按调用方的截止时间失败,展示和账本的调用方都能接受。
func (s *AccountDirectoryStore) acquirePlayerStoreRead(ctx context.Context) (func(), error) {
	select {
	case s.playerStoreReads <- struct{}{}:
		return func() { <-s.playerStoreReads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// playerStorage 按权威路由给出玩家所在的存档库。不在白名单里的落点不读:白名单是部署声明的可访问范围。
// 建角恒写落点,和 go/db 一样不回落 home_zone。没有路由或没有落点回 ErrMissing,不在白名单回 ErrUnavailable。
func (s *AccountDirectoryStore) playerStorage(ctx context.Context, playerID uint64) (uint32, error) {
	route, found, err := s.steps.readRoute(ctx, playerID)
	if err != nil {
		return 0, err
	}
	if !found || !route.HasPlacement() {
		return 0, accountdirectory.ErrMissing
	}
	if !s.steps.storageAllowed(route.StorageID) {
		return 0, accountdirectory.ErrUnavailable
	}
	return route.StorageID, nil
}

// ReadPlayerStoreAsset 强读写权行和钱包行。两行出自同一条语句:判「不在线」和判「账本未见」看的是同一个时刻。
// 返回 (nil, nil) = 没有写权行或路由(角色不存在或未建完)。读不到回错误,不能当成没有:调用方拿它判账本有没有见过一笔资产。
func (s *AccountDirectoryStore) ReadPlayerStoreAsset(ctx context.Context, playerID uint64) (*PlayerAssetRow, error) {
	if s == nil || playerID == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	release, err := s.acquirePlayerStoreRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	storage, err := s.playerStorage(ctx, playerID)
	if err != nil {
		if errors.Is(err, accountdirectory.ErrMissing) {
			return nil, nil
		}
		return nil, err
	}
	db, _, err := s.stores.DB(ctx, storage)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT kind,owner_epoch,save_seq,released,payload FROM player_store WHERE player_id=? AND kind IN (?,?)",
		playerID, playerStoreOwnerKind, playerStoreWalletKind)
	if err != nil {
		return nil, fmt.Errorf("读玩家账本行: %w", err)
	}
	defer rows.Close()
	var result *PlayerAssetRow
	var assetPayload []byte
	for rows.Next() {
		var kind int32
		var epoch, seq uint64
		var released uint32
		var payload []byte
		if err := rows.Scan(&kind, &epoch, &seq, &released, &payload); err != nil {
			return nil, fmt.Errorf("读玩家账本行: %w", err)
		}
		switch kind {
		case playerStoreOwnerKind:
			result = &PlayerAssetRow{OwnerEpoch: epoch, SaveSeq: seq, Released: released != 0}
		case playerStoreWalletKind:
			assetPayload = payload
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读玩家账本行: %w", err)
	}
	if result == nil {
		return nil, nil
	}
	result.Player = &dbpb.PlayerDatabase{}
	if err := proto.Unmarshal(assetPayload, result.Player); err != nil {
		return nil, fmt.Errorf("解析玩家账本行 player_id=%d: %w", playerID, err)
	}
	result.Player.PlayerId = playerID
	return result, nil
}

// ReadPlayerDisplays 批量读展示资料,来自 kCharacter(等级)和 kProfile(职业、名字、外观、性别)两行。按玩家分片读路由,按落点分组读存档库。
// 缺席的 id 不出现在结果里。一行坏了或者一个落点读不了只跳过那些玩家:这是展示数据,不能让一个人或一个库拖垮整页。
func (s *AccountDirectoryStore) ReadPlayerDisplays(ctx context.Context, ids []uint64) (map[uint64]*dspb.PlayerDisplay, error) {
	if s == nil || len(ids) > PlayerDisplayBatchLimit {
		return nil, accountdirectory.ErrInvalid
	}
	unique := make([]uint64, 0, len(ids))
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return map[uint64]*dspb.PlayerDisplay{}, nil
	}
	release, err := s.acquirePlayerStoreRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	routes, err := routesAcrossShards(ctx, s.shards, unique)
	if err != nil {
		return nil, err
	}
	byStorage := make(map[uint32][]uint64)
	for id, row := range routes {
		if !row.HasPlacement() {
			continue
		}
		if !s.steps.storageAllowed(row.StorageID) {
			logx.Errorf("[player-display] storage %d of player %d cannot be read here; skipped", row.StorageID, id)
			continue
		}
		byStorage[row.StorageID] = append(byStorage[row.StorageID], id)
	}
	return s.readDisplaysByStorage(ctx, byStorage), nil
}

// readDisplaysByStorage 逐落点读展示资料。打不开或读不了的落点记日志后跳过,好友列表这类调用方只缺这个落点的玩家。
// 一个落点要么全进结果,要么全不进:读到一半失败时,半份资料(有等级没名字)比没有更糟。
func (s *AccountDirectoryStore) readDisplaysByStorage(ctx context.Context, byStorage map[uint32][]uint64) map[uint64]*dspb.PlayerDisplay {
	storages := make([]uint32, 0, len(byStorage))
	for storage := range byStorage {
		storages = append(storages, storage)
	}
	out := make(map[uint64]*dspb.PlayerDisplay)
	var mu sync.Mutex
	// 回调不回错误:一个落点失败不影响别的落点,结果里也不区分。
	_ = runParallel(len(storages), shardFanOut, func(i int) error {
		players := byStorage[storages[i]]
		found, err := s.readDisplaysOfStorage(ctx, storages[i], players)
		if err != nil {
			logx.Errorf("[player-display] storage %d is unavailable; %d players skipped: %v", storages[i], len(players), err)
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		for player, display := range found {
			out[player] = display
		}
		return nil
	})
	return out
}

func (s *AccountDirectoryStore) readDisplaysOfStorage(ctx context.Context, storage uint32, players []uint64) (map[uint64]*dspb.PlayerDisplay, error) {
	db, _, err := s.stores.DB(ctx, storage)
	if err != nil {
		return nil, err
	}
	return readPlayerDisplaysIn(ctx, db, players)
}

func readPlayerDisplaysIn(ctx context.Context, db *sql.DB, players []uint64) (map[uint64]*dspb.PlayerDisplay, error) {
	args := make([]any, 0, len(players)+2)
	for _, id := range players {
		args = append(args, id)
	}
	args = append(args, playerStoreCharacterKind, playerStoreProfileKind)
	query := "SELECT player_id,kind,payload FROM player_store WHERE player_id IN (?" +
		strings.Repeat(",?", len(players)-1) + ") AND kind IN (?,?)"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("读玩家展示资料: %w", err)
	}
	defer rows.Close()
	out := make(map[uint64]*dspb.PlayerDisplay, len(players))
	for rows.Next() {
		var player uint64
		var kind int32
		var payload []byte
		if err := rows.Scan(&player, &kind, &payload); err != nil {
			return nil, fmt.Errorf("读玩家展示资料: %w", err)
		}
		var row dbpb.PlayerDatabase
		if err := proto.Unmarshal(payload, &row); err != nil {
			logx.Errorf("[player-display] corrupt player_store row skipped: player_id=%d kind=%d: %v", player, kind, err)
			continue
		}
		display := out[player]
		if display == nil {
			display = &dspb.PlayerDisplay{}
			out[player] = display
		}
		switch kind {
		case playerStoreCharacterKind:
			display.Level = row.GetLevelComponent().GetLevel()
		case playerStoreProfileKind:
			display.ClassId = row.GetUint32PbComponent().GetClass()
			display.Name = row.GetProfileComponent().GetName()
			display.AppearanceId = row.GetProfileComponent().GetAppearanceId()
			display.Gender = row.GetProfileComponent().GetGender()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读玩家展示资料: %w", err)
	}
	return out, nil
}

// ReleaseDeadPlayerStoreHolder 在持有节点判死后释放写权:存档库的一个事务里抬代次、清持有者、置 released=1。
// 旧持有者恢复之后的写因 E<Er 被挡住,所以放弃、退款之后不会再有账本提交(规格 §6「判死放弃要改」)。
//
// ownerEpoch 是判死时 location 这一代。库里已经比它新说明新的持有者认领过了,不动。只增、幂等:重试时库里已是 ownerEpoch+1。
// 没有写权行不算错:没有东西要释放。目录里没有要记的东西,所以这里没有第二步。
func (s *AccountDirectoryStore) ReleaseDeadPlayerStoreHolder(ctx context.Context, playerID, ownerEpoch uint64) error {
	if s == nil || playerID == 0 || ownerEpoch == 0 || ownerEpoch == ^uint64(0) {
		return accountdirectory.ErrInvalid
	}
	storage, err := s.playerStorage(ctx, playerID)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	db, _, err := s.stores.DB(ctx, storage)
	if err != nil {
		return err
	}
	return shardTx(ctx, db, func(tx *sql.Tx) error {
		owner, err := lockStoreOwner(ctx, tx, playerID)
		if err != nil || owner == nil {
			return err
		}
		if owner.ownerEpoch > ownerEpoch {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE player_store SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?",
			ownerEpoch+1, []byte{}, playerID, playerStoreOwnerKind); err != nil {
			return fmt.Errorf("释放玩家写权行: %w", err)
		}
		return nil
	})
}
