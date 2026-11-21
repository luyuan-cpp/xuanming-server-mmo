package store

import (
	"context"
	"database/sql"
	"fmt"

	base "proto/common/base"

	"google.golang.org/protobuf/proto"
)

// UpdateRoleAppearance 把 scene 已落盘提交的角色永久外观同步到账号目录(account_role.role_proto),
// 使下次登录的选角列表展示最新永久外观(docs/design/character-appearance-switch.md)。
// 外观不属于护栏状态:不动 owner_revision 与 fence_version,在线会话与入场登记不受影响。
func (s *AccountDirectoryStore) UpdateRoleAppearance(ctx context.Context, playerID uint64, appearanceID string) (bool, error) {
	if playerID == 0 || appearanceID == "" {
		return false, fmt.Errorf("player_id and appearance_id must be non-empty")
	}
	var updated bool
	err := s.playerTx(ctx, playerID, func(tx *sql.Tx) error {
		owned, _, err := lockFencedOwnership(ctx, tx, playerID)
		if err != nil {
			return err
		}
		if owned == nil || owned.Account == "" {
			return nil // 角色已删除或不存在,不阻断同步收尾
		}
		role, ok := proto.Clone(owned.GetRole()).(*base.AccountSimplePlayer)
		if !ok || role == nil {
			return nil
		}
		if role.GetAppearanceId() == appearanceID {
			return nil
		}
		role.AppearanceId = appearanceID
		raw, err := proto.Marshal(role)
		if err != nil {
			return err
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_role SET role_proto=?,updated_ms=? WHERE player_id=?", raw, now, playerID); err != nil {
			return err
		}
		updated = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}
