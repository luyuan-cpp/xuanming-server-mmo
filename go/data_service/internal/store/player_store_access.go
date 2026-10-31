package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	storepb "proto/player_store"
	"shared/accountdirectory"
	"shared/placement"
)

// player_store 的访问(docs/design/player-save-direct-commit.md §6)。
// 存盘本身在 go/db;data_service 只做建角初始行、账本只读、交易冻结证据、判死释放,都走全局权威池,按 player_route 选库。

const (
	playerStoreOwnerKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	playerStoreAssetKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET)

	playerStoreHotKind     = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT)
	playerStoreProfileKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE)

	// 证据读取不是批量导出;超过正常玩家存档预算显式拒绝,避免持目录锁无限读 blob。
	playerStoreProofMaxBytes = 16 << 20

	// PlayerDisplayBatchLimit 是一次读展示资料的上限。在线目录一批 SCAN 最多 1024 个 key。
	PlayerDisplayBatchLimit = 1024
)

// PlayerAssetRow 是写权行和 kAsset 行的一次一致读。
type PlayerAssetRow struct {
	OwnerEpoch uint64
	SaveSeq    uint64
	// Released:持有者已最终存盘并释放,或者还没有人认领过。
	Released bool
	// Player 只有 kAsset 组的字段(账本、货币、背包等)和 player_id;没有 kAsset 行时只有 player_id。
	Player *dbpb.PlayerDatabase
}

// playerStoreReadLimit 是账本和展示资料读最多占的连接数:不超过池的一半。
func playerStoreReadLimit(maxOpenConn int) int {
	if limit := maxOpenConn / 2; limit > 0 {
		return limit
	}
	return 1
}

// acquirePlayerStoreRead 取一个读名额,释放函数必须调用。这些读和登录的悲观事务共用账号目录池(缺省 8 条):好友列表、在线目录、
// 队伍视图原来走 Redis,读量一大进场就会等不到连接。等不到名额就按调用方的截止时间失败,展示和账本的调用方都能接受。
func (s *AccountDirectoryStore) acquirePlayerStoreRead(ctx context.Context) (func(), error) {
	select {
	case s.playerStoreReads <- struct{}{}:
		return func() { <-s.playerStoreReads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// playerStoreSchema 按权威路由给出玩家所在的库。不在白名单里的落点不读:白名单是部署声明的可访问范围。
func (s *AccountDirectoryStore) playerStoreSchema(ctx context.Context, q routeQueryer, playerID uint64) (string, error) {
	routes, err := loadRoutes(ctx, q, []uint64{playerID})
	if err != nil {
		return "", err
	}
	row, ok := routes[playerID]
	// 建角恒写落点,和 go/db 一样不回落 home_zone。
	if !ok || !row.HasPlacement() {
		return "", accountdirectory.ErrMissing
	}
	storage := row.StorageID
	if !s.initialStores[storage] {
		return "", accountdirectory.ErrUnavailable
	}
	schema, ok := placement.StoreDBName(storage)
	if !ok {
		return "", accountdirectory.ErrInvalid
	}
	return schema, nil
}

// ReadPlayerStoreAsset 强读写权行和 kAsset 行。两行出自同一条语句:判「不在线」和判「账本未见」看的是同一个时刻。
// 返回 (nil, nil) = 没有写权行或路由(角色不存在或未建完)。
func (s *AccountDirectoryStore) ReadPlayerStoreAsset(ctx context.Context, playerID uint64) (*PlayerAssetRow, error) {
	if s == nil || playerID == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	release, err := s.acquirePlayerStoreRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	schema, err := s.playerStoreSchema(ctx, s.db, playerID)
	if err != nil {
		if errors.Is(err, accountdirectory.ErrMissing) {
			return nil, nil
		}
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT kind,owner_epoch,save_seq,released,payload FROM `"+schema+"`.`player_store` WHERE player_id=? AND kind IN (?,?)",
		playerID, playerStoreOwnerKind, playerStoreAssetKind)
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
		case playerStoreAssetKind:
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

// ReadPlayerDisplays 批量读展示资料,来自 kHot(等级、职业)和 kProfile(名字、外观、性别)两行。
// 缺席的 id 不出现在结果里。一行坏了或者落点读不了只跳过那一个玩家:这是展示数据,不能让一个人拖垮整页。
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
	out := make(map[uint64]*dspb.PlayerDisplay, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	release, err := s.acquirePlayerStoreRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	routes, err := loadRoutes(ctx, s.db, unique)
	if err != nil {
		return nil, err
	}
	bySchema := make(map[string][]uint64)
	for id, row := range routes {
		if !row.HasPlacement() {
			continue
		}
		storage := row.StorageID
		schema, ok := placement.StoreDBName(storage)
		if !ok || !s.initialStores[storage] {
			logx.Errorf("[player-display] storage %d of player %d cannot be read here; skipped", storage, id)
			continue
		}
		bySchema[schema] = append(bySchema[schema], id)
	}
	for schema, players := range bySchema {
		if err := s.readPlayerDisplaysIn(ctx, schema, players, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *AccountDirectoryStore) readPlayerDisplaysIn(ctx context.Context, schema string, players []uint64, out map[uint64]*dspb.PlayerDisplay) error {
	args := make([]any, 0, len(players)+2)
	for _, id := range players {
		args = append(args, id)
	}
	args = append(args, playerStoreHotKind, playerStoreProfileKind)
	query := "SELECT player_id,kind,payload FROM `" + schema + "`.`player_store` WHERE player_id IN (?" +
		strings.Repeat(",?", len(players)-1) + ") AND kind IN (?,?)"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("读玩家展示资料: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var player uint64
		var kind int32
		var payload []byte
		if err := rows.Scan(&player, &kind, &payload); err != nil {
			return fmt.Errorf("读玩家展示资料: %w", err)
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
		case playerStoreHotKind:
			display.Level = row.GetLevelComponent().GetLevel()
			display.ClassId = row.GetUint32PbComponent().GetClass()
		case playerStoreProfileKind:
			display.Name = row.GetProfileComponent().GetName()
			display.AppearanceId = row.GetProfileComponent().GetAppearanceId()
			display.Gender = row.GetProfileComponent().GetGender()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读玩家展示资料: %w", err)
	}
	return nil
}

// ReleaseDeadPlayerStoreHolder 在持有节点判死后释放写权:同一个事务里抬代次、清持有者、置 released=1。
// 旧持有者恢复之后的写因 E<Er 被挡住,所以放弃、退款之后不会再有账本提交(规格 §6「判死放弃要改」)。
//
// ownerEpoch 是判死时 location 这一代。库里已经比它新说明新的持有者认领过了,不动。幂等:重试时库里已是 ownerEpoch+1。
// 没有写权行不算错:没有东西要释放。
func (s *AccountDirectoryStore) ReleaseDeadPlayerStoreHolder(ctx context.Context, playerID, ownerEpoch uint64) error {
	if s == nil || playerID == 0 || ownerEpoch == 0 || ownerEpoch == ^uint64(0) {
		return accountdirectory.ErrInvalid
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		schema, err := s.playerStoreSchema(ctx, tx, playerID)
		if err != nil {
			if errors.Is(err, accountdirectory.ErrMissing) {
				return nil
			}
			return err
		}
		var current uint64
		err = tx.QueryRowContext(ctx, "SELECT owner_epoch FROM `"+schema+"`.`player_store` WHERE player_id=? AND kind=? FOR UPDATE", playerID, playerStoreOwnerKind).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("锁玩家写权行: %w", err)
		}
		if current > ownerEpoch {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE `"+schema+"`.`player_store` SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?",
			ownerEpoch+1, []byte{}, playerID, playerStoreOwnerKind); err != nil {
			return fmt.Errorf("释放玩家写权行: %w", err)
		}
		return nil
	})
}
