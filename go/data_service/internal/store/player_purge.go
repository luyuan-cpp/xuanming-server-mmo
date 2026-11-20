package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"shared/accountdirectory"
)

// 删角收尾清数据(docs/design/account-deletion.md §6)的 data_service 一侧:删存档库里这名玩家的全部 player_store 行(含写权行),
// 再让六个参与者各清各的(协调者 Purge)。快照(player_snapshot)与流水按审计保留期由 RunAuditRetention 删,不在这里(§6.2)。
//
// 为什么墓碑之后可以直接删、不用两阶段:墓碑让所有参与者的护栏副本拒绝这名玩家的业务写入;scene 写盘以入场登记为前提,
// 删角要求这名角色的全部登记已关闭(该实例的实体与迟到回调都已结束),墓碑之后修订号变了,旧世代的登记再也进不来。
// 所以清完不会有迟到的写把数据写回来,失败了也只是留给下一轮,重复执行没有副作用。

// RemovalSettlePurge 是 RemovalSettleFailed 的取值之一:清数据这一步没做完。
const RemovalSettlePurge = "purge_data"

// purgeRemovedRole 清一名已删除角色的数据。先核目录里确是墓碑:在役角色绝不清。没有角色行(从没建成)算清完。
func (s *AccountDirectoryStore) purgeRemovedRole(ctx context.Context, player uint64) error {
	state, err := s.steps.readFence(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-read the removed role: %w", err)
	}
	if state.Account != "" {
		return errors.New("the role is not removed, refusing to purge its data")
	}
	if err := s.purgeRemovedPlayerStore(ctx, player); err != nil {
		return fmt.Errorf("purge player_store rows: %w", err)
	}
	if s.fences == nil || !s.fences.Purge(ctx, state) {
		return errors.New("not every role fence participant purged the removed role yet")
	}
	return nil
}

// purgeRemovedPlayerStore 删这名玩家在落点存档库里的全部行。没有路由或落点 = 从没写过存档,算清完;存档库连接没装配时不清
// (只有单测这样装配,生产 ConfigurePlayerStores 必配)。
func (s *AccountDirectoryStore) purgeRemovedPlayerStore(ctx context.Context, player uint64) error {
	if s.stores == nil {
		return nil
	}
	route, found, err := s.steps.readRoute(ctx, player)
	if err != nil {
		return err
	}
	if !found || route.StorageID == 0 {
		return nil
	}
	db, _, err := s.stores.DB(ctx, route.StorageID)
	if err != nil {
		return err
	}
	return shardTx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM player_store WHERE player_id=?", player)
		return err
	})
}
