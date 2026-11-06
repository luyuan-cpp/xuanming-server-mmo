package store

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"sync"

	base "proto/common/base"
	"shared/dirshard"
	"shared/rolefence"
)

// sqlResidualSteps 是建角残留核账的 SQL(编排见 create_residual.go)。目录 K 片、名字表、存档库都是 data_service 自己的逻辑库,
// 不碰别的服务的库(AGENTS.md §7 第 6 条)。
type sqlResidualSteps struct {
	shards *shardPools // 目录:角色行、路由行、ref、目录行
	stores *PlayerStorePools
	// names 的池和 shards 开在同一组库上(NewCreateResidualAuditor 核过):名字表走名字库自己的池,闸门行锁走目录的池。
	names *PlayerNameStore
}

const residualRoleColumns = "player_id," + ownershipColumns + ",initial_player_commit"

func (st *sqlResidualSteps) scanRoles(ctx context.Context, slot int, after uint64, limit int) ([]residualRole, error) {
	return st.readRoles(ctx, slot, "SELECT "+residualRoleColumns+" FROM account_role WHERE player_id>? AND account<>'' ORDER BY player_id LIMIT ?", after, limit)
}

func (st *sqlResidualSteps) loadRole(ctx context.Context, player uint64) (*residualRole, error) {
	roles, err := st.readRoles(ctx, dirshard.SlotOfPlayer(player, st.shards.k), "SELECT "+residualRoleColumns+" FROM account_role WHERE player_id=?", player)
	if err != nil || len(roles) == 0 {
		return nil, err
	}
	return &roles[0], nil
}

// readRoles 读角色行,再在同一片上补路由行和名字副本。原件解不开的 Role 留 nil,交给判据报出来,不中断整页。
func (st *sqlResidualSteps) readRoles(ctx context.Context, slot int, query string, args ...any) ([]residualRole, error) {
	out, err := readResidualRoleRows(ctx, st.shards.pools[slot], query, args...)
	if err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]uint64, len(out))
	for i := range out {
		ids[i] = out[i].PlayerID
	}
	routes, err := loadRoutes(ctx, st.shards.pools[slot], ids)
	if err != nil {
		return nil, err
	}
	owned, err := readOwnedNorms(ctx, st.names.pools.pools[slot], ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Route, out[i].Routed = routes[out[i].PlayerID]
		out[i].OwnedNorm = owned[out[i].PlayerID]
	}
	return out, nil
}

// readResidualRoleRows 读完就关游标,之后才去读路由和名字副本。
func readResidualRoleRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]residualRole, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []residualRole
	for rows.Next() {
		var r residualRole
		var raw []byte
		var rev, listing, prepare, initial uint64
		if err := rows.Scan(&r.PlayerID, &r.Account, &raw, &rev, &listing, &prepare, &initial); err != nil {
			return nil, err
		}
		r.OwnerRevision = rev
		r.InitialCommit = initial == 1
		r.Busy = listing != 0 || prepare != 0
		r.Removing = prepare != 0 && rolefence.IsRemovalToken(prepare)
		if owned, err := decodeOwnership(r.PlayerID, r.Account, raw, rev, listing, prepare); err == nil {
			r.Role = owned.Role
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readOwnedNorms(ctx context.Context, db *sql.DB, players []uint64) (map[uint64]string, error) {
	out := make(map[uint64]string, len(players))
	for chunk := range slices.Chunk(players, residualLookupChunk) {
		rows, err := db.QueryContext(ctx, "SELECT player_id,name_norm FROM player_name_owner WHERE player_id IN ("+placeholders(len(chunk))+")", residualIDArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var player uint64
			var norm string
			if err := rows.Scan(&player, &norm); err != nil {
				rows.Close()
				return nil, err
			}
			out[player] = norm
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// presentRefs 按账号片分组,每组一条 (account, player_id) IN 查询。
func (st *sqlResidualSteps) presentRefs(ctx context.Context, keys []residualRefKey) (map[residualRefKey]bool, error) {
	out := make(map[residualRefKey]bool, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	bySlot := make(map[int][]residualRefKey)
	for _, key := range keys {
		slot := dirshard.SlotOfAccount(key.account, st.shards.k)
		bySlot[slot] = append(bySlot[slot], key)
	}
	slots := slices.Sorted(maps.Keys(bySlot))
	var mu sync.Mutex
	err := runParallel(len(slots), shardFanOut, func(i int) error {
		db := st.shards.pools[slots[i]]
		for chunk := range slices.Chunk(bySlot[slots[i]], residualLookupChunk) {
			args := make([]any, 0, 2*len(chunk))
			for _, key := range chunk {
				args = append(args, []byte(key.account), key.player)
			}
			rows, err := db.QueryContext(ctx, "SELECT account,player_id FROM account_role_ref WHERE (account,player_id) IN ("+residualPairPlaceholders(len(chunk))+")", args...)
			if err != nil {
				return err
			}
			var found []residualRefKey
			for rows.Next() {
				var account []byte
				var player uint64
				if err := rows.Scan(&account, &player); err != nil {
					rows.Close()
					return err
				}
				found = append(found, residualRefKey{account: string(account), player: player})
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			mu.Lock()
			for _, key := range found {
				out[key] = true
			}
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nameHolders 按名字片分组,每组一条 name_norm IN 查询。读的是名字行本身:名字归谁、绑没绑定都只看这一行。
func (st *sqlResidualSteps) nameHolders(ctx context.Context, norms []string) (map[string]residualNameRow, error) {
	out := make(map[string]residualNameRow, len(norms))
	if len(norms) == 0 {
		return out, nil
	}
	bySlot := make(map[int][]string)
	for _, norm := range norms {
		slot := dirshard.SlotOfName(norm, st.names.pools.k)
		bySlot[slot] = append(bySlot[slot], norm)
	}
	slots := slices.Sorted(maps.Keys(bySlot))
	var mu sync.Mutex
	err := runParallel(len(slots), shardFanOut, func(i int) error {
		db := st.names.pools.pools[slots[i]]
		for chunk := range slices.Chunk(bySlot[slots[i]], residualLookupChunk) {
			args := make([]any, len(chunk))
			for j, norm := range chunk {
				args[j] = norm
			}
			rows, err := db.QueryContext(ctx, "SELECT name_norm,player_id,bound_ms FROM player_name WHERE name_norm IN ("+placeholders(len(chunk))+")", args...)
			if err != nil {
				return err
			}
			found := make(map[string]residualNameRow)
			for rows.Next() {
				var norm string
				var holder, boundMs uint64
				if err := rows.Scan(&norm, &holder, &boundMs); err != nil {
					rows.Close()
					return err
				}
				found[norm] = residualNameRow{Holder: holder, Bound: boundMs != 0}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			mu.Lock()
			maps.Copy(out, found)
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (st *sqlResidualSteps) storageAllowed(storage uint32) bool { return st.stores.Allowed(storage) }

// storeKinds 只读主键 (player_id, kind),不读载荷。
func (st *sqlResidualSteps) storeKinds(ctx context.Context, storage uint32, players []uint64) (map[uint64][]int32, error) {
	db, _, err := st.stores.DB(ctx, storage)
	if err != nil {
		return nil, err
	}
	out := make(map[uint64][]int32, len(players))
	for chunk := range slices.Chunk(players, residualLookupChunk) {
		rows, err := db.QueryContext(ctx, "SELECT player_id,kind FROM player_store WHERE player_id IN ("+placeholders(len(chunk))+")", residualIDArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var player uint64
			var kind int32
			if err := rows.Scan(&player, &kind); err != nil {
				rows.Close()
				return nil, err
			}
			out[player] = append(out[player], kind)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// describePlayers 按玩家片分组读角色行的归属和路由行的建角时刻。
func (st *sqlResidualSteps) describePlayers(ctx context.Context, players []uint64) (map[uint64]residualPlayer, error) {
	out := make(map[uint64]residualPlayer, len(players))
	var mu sync.Mutex
	err := st.shards.eachPlayerGroup(players, func(db *sql.DB, group []uint64) error {
		live := make(map[uint64]bool, len(group))
		rows, err := db.QueryContext(ctx, "SELECT player_id,account FROM account_role WHERE player_id IN ("+placeholders(len(group))+")", residualIDArgs(group)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var player uint64
			var account []byte
			if err := rows.Scan(&player, &account); err != nil {
				rows.Close()
				return err
			}
			live[player] = len(account) > 0
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		routes, err := loadRoutes(ctx, db, group)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, player := range group {
			out[player] = residualPlayer{Live: live[player], CreatedAtMs: routes[player].CreatedAtMs}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// restoreRef 在账号片的一个事务里补 ACTIVE ref:锁闸门行,锁目录行(没有就建,恢复点可能早于这个账号第一次登录),核这条 ref 不在、
// 加上它不超过硬上限,再写。写用 activateRefSQL:行在且挂着别的过户时不覆盖,和建角第 4 步同一条语句。
// 补 ref 永远安全:ref 只是索引,多一条陈旧的由读路径过滤、后台清理;少一条角色就从列表里消失。所以不需要和角色行同事务。
func (st *sqlResidualSteps) restoreRef(ctx context.Context, account string, player uint64, maxRoles int) (refRestore, error) {
	var out refRestore
	err := shardTx(ctx, st.shards.forAccount(account), func(tx *sql.Tx) error {
		if err := requireGateOpenTx(ctx, tx); err != nil {
			return err
		}
		if err := lockDirectory(ctx, tx, account, true); err != nil {
			return err
		}
		refs, err := readRefs(ctx, tx, account)
		if err != nil {
			return err
		}
		switch {
		case slices.ContainsFunc(refs, func(ref roleRef) bool { return ref.PlayerID == player }):
			out = refPresent
			return nil
		case len(refs) >= maxRefRows:
			out = refAtHardCap
			return nil
		case len(refs) >= maxRoles:
			out = refRestoredOverCap
		default:
			out = refRestored
		}
		_, err = tx.ExecContext(ctx, activateRefSQL, []byte(account), player, refStateActive, newRefNonce(), uint64(0), uint64(0), uint64(0), uint64(0))
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

// restoreName 重新登记名字,复用建角的登记(Reserve),结局口径也相同;登记好之后置为已绑定(Bind,和建角提交之后同一步):
// 修之前 repair 刚重读并复核过角色行,角色确已提交。Reserve、Bind 都是自动提交的单条语句(名字行、名字副本),
// 所以闸门行锁开在旁边的事务里,持到全部提交之后才放(withGatesHeld)。Reserve 撞上的结局原样回;
// 绑定时发现名字归了别人(两步之间被抢)回 Taken,这个 id 名下另有名字回 Conflict,都什么都没改。
func (st *sqlResidualSteps) restoreName(ctx context.Context, player uint64, display, norm string, nowMs uint64) (ReserveOutcome, uint64, error) {
	var outcome ReserveOutcome
	var holder uint64
	err := withGatesHeld(ctx, st.nameGates(player, norm), func() error {
		var err error
		outcome, holder, err = st.names.Reserve(ctx, player, display, norm, nowMs)
		if err != nil || (outcome != ReserveInserted && outcome != ReserveAlreadyOwned) {
			return err
		}
		bound, boundHolder, err := st.names.Bind(ctx, player, display, norm, nowMs)
		if err != nil {
			return err
		}
		switch bound {
		case BindTaken:
			outcome, holder = ReserveTaken, boundHolder
		case BindConflict:
			outcome, holder = ReserveConflict, 0
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return outcome, holder, nil
}

// nameGates 是名字登记要写的库:名字行在名字片,名字副本在玩家片。按片号升序,同一片只锁一次,多个实例同时跑也不会互等成环。
func (st *sqlResidualSteps) nameGates(player uint64, norm string) []*sql.DB {
	slots := []int{dirshard.SlotOfName(norm, st.shards.k), dirshard.SlotOfPlayer(player, st.shards.k)}
	slices.Sort(slots)
	slots = slices.Compact(slots)
	dbs := make([]*sql.DB, len(slots))
	for i, slot := range slots {
		dbs[i] = st.shards.pools[slot]
	}
	return dbs
}

// withGatesHeld 在每个库上开一个事务锁住闸门行、核它开着,全部锁住之后才调 write,write 返回后才回滚放锁。
// 维护身份不过写者闸门;write 用别的连接发自动提交的写,这些写都在放锁之前提交,而冻结要先拿到这把锁,所以只能排在它们之后。
// 闸门事务本身什么都不写。调用方按固定顺序给库(nameGates),锁的顺序就固定。
func withGatesHeld(ctx context.Context, dbs []*sql.DB, write func() error) error {
	txs := make([]*sql.Tx, 0, len(dbs))
	defer func() {
		for i := len(txs) - 1; i >= 0; i-- {
			_ = txs[i].Rollback()
		}
	}()
	for _, db := range dbs {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		txs = append(txs, tx)
		if err := requireGateOpenTx(ctx, tx); err != nil {
			return err
		}
	}
	return write()
}

// restoreInitialRows 在存档库的一个事务里重写初始行:锁闸门行,锁住这名玩家的行核它一行都没有,再写建角时那几行
// (upsertInitialPlayerStoreRows,和建角第 2 步同一个函数,写完读回核对)。有写权行回 storePresent,有别的行没有写权行回 storePartial,都不写。
// 角色行没提交时没人读得到这些行,所以和目录分库写不会让人读到半截;认领要先有角色行和入场登记。
func (st *sqlResidualSteps) restoreInitialRows(ctx context.Context, storage uint32, role *base.AccountSimplePlayer) (storeRestore, error) {
	db, _, err := st.stores.DB(ctx, storage)
	if err != nil {
		return 0, err
	}
	var out storeRestore
	err = shardTx(ctx, db, func(tx *sql.Tx) error {
		if err := requireGateOpenTx(ctx, tx); err != nil {
			return err
		}
		kinds, err := lockStoreKinds(ctx, tx, role.GetPlayerId())
		if err != nil {
			return err
		}
		switch {
		case slices.Contains(kinds, playerStoreOwnerKind):
			out = storePresent
			return nil
		case len(kinds) > 0:
			out = storePartial
			return nil
		}
		if err := upsertInitialPlayerStoreRows(ctx, tx, role); err != nil {
			return err
		}
		out = storeRestored
		return nil
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

// lockStoreKinds 锁住这名玩家现有的行,回它们的 kind。读完就关游标:同一事务后面还要发语句。
func lockStoreKinds(ctx context.Context, tx *sql.Tx, player uint64) ([]int32, error) {
	rows, err := tx.QueryContext(ctx, "SELECT kind FROM player_store WHERE player_id=? FOR UPDATE", player)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var kinds []int32
	for rows.Next() {
		var kind int32
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}
	return kinds, rows.Err()
}

func residualIDArgs(ids []uint64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// residualPairPlaceholders 是 n 个 (?,?)。
func residualPairPlaceholders(n int) string {
	if n == 0 {
		return ""
	}
	out := make([]byte, 0, 6*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, "(?,?)"...)
	}
	return string(out)
}
