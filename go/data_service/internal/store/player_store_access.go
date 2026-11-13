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
	"shared/dirshard"
)

// player_store 的访问(docs/design/player-save-direct-commit.md §6)。
// 存盘本身在 go/db;data_service 只做建角初始行、账本与物品获取封禁表只读、交易冻结证据、判死释放,按 player_route 选库,走存档库连接池。

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

// PlayerProfileRow 是写权行和资料行(kProfile)的一次一致读。
type PlayerProfileRow struct {
	// Player 只有资料行的字段(职业、名字副本、外观、性别、近期战斗结果、物品获取封禁等)和 player_id;
	// 没有资料行时只有 player_id。
	Player *dbpb.PlayerDatabase
}

// playerStoreOwnerState 是一致读从写权行带出的三列。
type playerStoreOwnerState struct {
	epoch    uint64
	saveSeq  uint64
	released bool
}

// readSlots 是一个连接池里留给账本、展示读的名额,最多占池的一半。
//
// 这些读和更要紧的工作共用连接池:目录分片的池上有登录的悲观事务,存档库的池上有建角、判死释放、交易冻结证据。
// 好友列表、在线目录、队伍视图原来走 Redis,读量一大,不限的话会排满池的等待队列,登录和判死排到它们后面。
// 所以留一半给别的工作;等不到名额的读按调用方的截止时间失败,展示和账本的调用方都能接受。
//
// 名额跟着池走,一个池一份:目录有 K 片、存档库有 K 个,各有各的池,读不同库的请求互不等待。
// 2026-11-12 之前全进程只有一份,按一个池的一半发。K8s 把池按 K 摊到每库 2 条之后名额只剩 1,
// 一个实例的账本和展示读全排成一队,加库、加片都不会变快。
// 没有另开只读池:那要每个库、每一片多一组连接,而一半的名额已经保证读挤不掉写。
type readSlots chan struct{}

// newReadSlots 按池的连接上限发名额。
func newReadSlots(maxOpenConn int) readSlots {
	return make(readSlots, readSlotLimit(maxOpenConn))
}

// readSlotLimit 是 maxOpenConn 条连接的池里读最多占几条:一半,至少 1 条。
func readSlotLimit(maxOpenConn int) int {
	if limit := maxOpenConn / 2; limit > 0 {
		return limit
	}
	return 1
}

// acquire 取一个名额,返回的释放函数必须调用。
func (r readSlots) acquire(ctx context.Context) (func(), error) {
	select {
	case r <- struct{}{}:
		return func() { <-r }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acquireDirectoryRead 占玩家所在目录分片的一个读名额,读路由的那一条查询用。
func (s *AccountDirectoryStore) acquireDirectoryRead(ctx context.Context, playerID uint64) (func(), error) {
	return s.directoryReads[dirshard.SlotOfPlayer(playerID, len(s.directoryReads))].acquire(ctx)
}

// readPlayerStorage 是账本读用的 playerStorage:读路由的那一条查询占目录分片的读名额。
// 判死、建角直接调 playerStorage,不占名额,名额就是为了不让读挤掉它们。
func (s *AccountDirectoryStore) readPlayerStorage(ctx context.Context, playerID uint64) (uint32, error) {
	release, err := s.acquireDirectoryRead(ctx, playerID)
	if err != nil {
		return 0, err
	}
	defer release()
	return s.playerStorage(ctx, playerID)
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
	owner, player, err := s.readOwnerAndRow(ctx, playerID, playerStoreWalletKind, "wallet")
	if err != nil || owner == nil {
		return nil, err
	}
	return &PlayerAssetRow{OwnerEpoch: owner.epoch, SaveSeq: owner.saveSeq, Released: owner.released, Player: player}, nil
}

// ReadPlayerStoreProfile 强读写权行和资料行,两行出自同一条语句。返回约定同 ReadPlayerStoreAsset:
// (nil, nil) = 没有写权行或路由;读不到回错误,不能当成没有 —— 运维查物品获取封禁表时会把「没有」读成「没封」。
func (s *AccountDirectoryStore) ReadPlayerStoreProfile(ctx context.Context, playerID uint64) (*PlayerProfileRow, error) {
	owner, player, err := s.readOwnerAndRow(ctx, playerID, playerStoreProfileKind, "profile")
	if err != nil || owner == nil {
		return nil, err
	}
	return &PlayerProfileRow{Player: player}, nil
}

// readOwnerAndRow 在一条语句里强读写权行和一个数据行(kind),数据行解析成只含那一行字段的 PlayerDatabase。
// 返回 (nil, nil, nil) = 没有写权行或路由;写权行在、数据行不在时 player 只有 player_id。
// 只读,不加锁;两条查询各占所在连接池的一个读名额(readSlots),不同时占两个。what 是行名(英文,只进错误文案;错误文案会进日志,AGENTS §2)。
func (s *AccountDirectoryStore) readOwnerAndRow(ctx context.Context, playerID uint64, kind int32, what string) (*playerStoreOwnerState, *dbpb.PlayerDatabase, error) {
	if s == nil || playerID == 0 {
		return nil, nil, accountdirectory.ErrInvalid
	}
	storage, err := s.readPlayerStorage(ctx, playerID)
	if err != nil {
		if errors.Is(err, accountdirectory.ErrMissing) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	db, release, err := s.stores.ReadDB(ctx, storage)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, "SELECT kind,owner_epoch,save_seq,released,payload FROM player_store WHERE player_id=? AND kind IN (?,?)",
		playerID, playerStoreOwnerKind, kind)
	if err != nil {
		return nil, nil, fmt.Errorf("read player_store %s row: %w", what, err)
	}
	defer rows.Close()
	var owner *playerStoreOwnerState
	var payload []byte
	for rows.Next() {
		var rowKind int32
		var epoch, seq uint64
		var released uint32
		var data []byte
		if err := rows.Scan(&rowKind, &epoch, &seq, &released, &data); err != nil {
			return nil, nil, fmt.Errorf("read player_store %s row: %w", what, err)
		}
		switch rowKind {
		case playerStoreOwnerKind:
			owner = &playerStoreOwnerState{epoch: epoch, saveSeq: seq, released: released != 0}
		case kind:
			payload = data
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read player_store %s row: %w", what, err)
	}
	if owner == nil {
		return nil, nil, nil
	}
	player := &dbpb.PlayerDatabase{}
	if err := proto.Unmarshal(payload, player); err != nil {
		return nil, nil, fmt.Errorf("decode player_store %s row player_id=%d: %w", what, playerID, err)
	}
	player.PlayerId = playerID
	return owner, player, nil
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
	routes, err := s.readDisplayRoutes(ctx, unique)
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
	out := s.readDisplaysByStorage(ctx, byStorage)
	// 调用方已经超时或取消:结果里可能缺了没等到名额的落点,不能当成「这些人没有资料」交回去。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// readDisplayRoutes 批量读路由,按玩家分片拆组,每片的那一条查询占这一片的读名额。
// 任一片失败(含等不到名额)整批报错:半份路由会让调用方把查不到的人当成没有角色。
func (s *AccountDirectoryStore) readDisplayRoutes(ctx context.Context, ids []uint64) (map[uint64]RouteRow, error) {
	out := make(map[uint64]RouteRow, len(ids))
	var mu sync.Mutex
	err := s.shards.eachPlayerGroup(ids, func(db *sql.DB, players []uint64) error {
		// 一组里的玩家都在同一片,拿第一个定名额。
		release, err := s.acquireDirectoryRead(ctx, players[0])
		if err != nil {
			return err
		}
		defer release()
		rows, err := loadRoutes(ctx, db, players)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for id, row := range rows {
			out[id] = row
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
			// 调用方已经超时或取消时不是这个落点的问题,不记「落点不可用」;ReadPlayerDisplays 会把 ctx 的错误交回去。
			if ctx.Err() == nil {
				logx.Errorf("[player-display] storage %d is unavailable; %d players skipped: %v", storages[i], len(players), err)
			}
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

// readDisplaysOfStorage 读一个落点上这些玩家的展示资料,占这个库的一个读名额。
func (s *AccountDirectoryStore) readDisplaysOfStorage(ctx context.Context, storage uint32, players []uint64) (map[uint64]*dspb.PlayerDisplay, error) {
	db, release, err := s.stores.ReadDB(ctx, storage)
	if err != nil {
		return nil, err
	}
	defer release()
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

// ReleaseDeadPlayerStoreHolder 只按代次释放写权:存档库的一个事务里抬代次、清持有者、置 released=1。
// 旧持有者恢复之后的写因 E<Er 被挡住,所以放弃、退款之后不会再有账本提交(规格 §6「判死放弃要改」)。
// LeaveScene 判死释放不走这里,走 ReleaseDeadPlayerStoreHolderIfUnchanged(还要比持有者与 claim_seq);
// 这里只剩无资格撤销之后的补释放(scene_manager unqualified_holder_revoke.go),那条路自己用 Redis 围栏挡住了旧持有者。
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
