package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	comppb "proto/common/component"
	databasepb "proto/common/database"
	storepb "proto/player_store"
	"shared/accountdirectory"
	"shared/generated/persistschema"
	"shared/savebarrier"
)

// initialStoreRow 是建角时写进 player_store 的一行。
type initialStoreRow struct {
	kind     storepb.PlayerStoreKind
	released uint32
	payload  []byte
}

// initialPlayerStoreRows 给出新角色的初始行(docs/design/player-save-direct-commit.md §1、§9.2):写权行,以及身份所在的两行。
// 身份在这里一次写全,不再需要 login 入场时往父键补:名字、外观、性别在 kProfile,职业在 kHot。
// 写权行 released=1:新角色没有持有者,不算在线。其余 kind 没有行,加载时按空处理;
// registration_timestamp 留空,首次进场由 scene 判定为新号。
func initialPlayerStoreRows(role *base.AccountSimplePlayer) ([]initialStoreRow, error) {
	deterministic := proto.MarshalOptions{Deterministic: true}
	hot, err := deterministic.Marshal(&databasepb.PlayerDatabase{
		Uint32PbComponent: &comppb.PlayerUint32Comp{Class: role.GetClassId()},
	})
	if err != nil {
		return nil, fmt.Errorf("序列化玩家初始热行: %w", err)
	}
	profile, err := deterministic.Marshal(&databasepb.PlayerDatabase{
		ProfileComponent: &comppb.PlayerProfileComp{
			Name:         role.GetName(),
			AppearanceId: role.GetAppearanceId(),
			Gender:       role.GetGender(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("序列化玩家初始资料行: %w", err)
	}
	return []initialStoreRow{
		{kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER, released: 1, payload: []byte{}},
		{kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT, payload: hot},
		{kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE, payload: profile},
	}, nil
}

// insertInitialPlayerStoreRows 在建角事务里严格插入初始行。schema 只由权威落点产生。
// 严格 INSERT:旧行或并发撞键整批回滚,不能 REPLACE / ODKU。
func insertInitialPlayerStoreRows(ctx context.Context, tx *sql.Tx, schema string, role *base.AccountSimplePlayer) error {
	rows, err := initialPlayerStoreRows(role)
	if err != nil {
		return err
	}
	placeholders := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*8)
	for _, row := range rows {
		placeholders = append(placeholders, "(?,?,?,?,?,?,?,?)")
		// holder_instance、payload 是 NOT NULL:绑定空切片,nil 会被驱动写成 NULL。
		args = append(args, role.PlayerId, int32(row.kind), savebarrier.InitialOwnerEpoch, savebarrier.InitialSaveSeq,
			persistschema.Gen, []byte{}, row.released, row.payload)
	}
	query := "INSERT INTO `" + schema + "`.`player_store` (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES " +
		strings.Join(placeholders, ",")
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == 1062 {
			return accountdirectory.ErrConflict
		}
		return fmt.Errorf("提交玩家初始存档行: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != int64(len(rows)) {
		return errors.New("玩家初始存档行未严格插入")
	}
	return nil
}
