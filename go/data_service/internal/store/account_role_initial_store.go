package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	comppb "proto/common/component"
	databasepb "proto/common/database"
	storepb "proto/player_store"
	"shared/accountdirectory"
	"shared/generated/persistschema"
)

// initialStoreRow 是建角时写进 player_store 的一行。
type initialStoreRow struct {
	kind     storepb.PlayerStoreKind
	released uint32
	payload  []byte
}

// initialPlayerStoreRows 给出新角色的初始行(docs/design/player-save-direct-commit.md §1、§1.1、§9.2):写权行,以及身份所在的资料行。
// 身份在这里一次写全,不再需要 login 入场时往父键补:职业、名字、外观、性别都在 kProfile。
// 写权行 released=1:新角色没有持有者,不算在线。其余 kind 没有行,加载时按空处理;
// registration_timestamp(也在 kProfile)留空,首次进场由 scene 判定为新号。
func initialPlayerStoreRows(role *base.AccountSimplePlayer) ([]initialStoreRow, error) {
	deterministic := proto.MarshalOptions{Deterministic: true}
	profile, err := deterministic.Marshal(&databasepb.PlayerDatabase{
		Uint32PbComponent: &comppb.PlayerUint32Comp{Class: role.GetClassId()},
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
		{kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE, payload: profile},
	}, nil
}

// upsertInitialPlayerStoreRows 在存档库的一个事务里写初始行,可以重放:行已在且就是这些初始行算成功。
//
// 目录提交之前没人读得到这些行(认领要先有入场登记,登记要先有角色行),所以重放时看到的只可能是上一次尝试写的。
// 看到别的内容就是撞上了不属于这次建角的行,回 ErrConflict,事务回滚,旧行一个字节不改。
func upsertInitialPlayerStoreRows(ctx context.Context, tx *sql.Tx, role *base.AccountSimplePlayer) error {
	rows, err := initialPlayerStoreRows(role)
	if err != nil {
		return err
	}
	placeholders := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*8)
	for _, row := range rows {
		placeholders = append(placeholders, "(?,?,?,?,?,?,?,?)")
		// holder_instance、payload 是 NOT NULL:绑定空切片,nil 会被驱动写成 NULL。
		args = append(args, role.PlayerId, int32(row.kind), accountdirectory.InitialOwnerEpoch, accountdirectory.InitialSaveSeq,
			persistschema.Gen, []byte{}, row.released, row.payload)
	}
	query := "INSERT INTO player_store (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES " +
		strings.Join(placeholders, ",") + " ON DUPLICATE KEY UPDATE player_id=player_id"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	existing, err := readPlayerStoreProofRows(ctx, tx, role.PlayerId, true)
	if err != nil {
		return err
	}
	if !sameInitialRows(existing, rows) {
		return accountdirectory.ErrConflict
	}
	return nil
}

// sameInitialRows 判库里的行是不是这些初始行。schema_gen 只要不超前:上一次尝试可能是旧二进制写的,内容相同就是同一批。
func sameInitialRows(existing []playerStoreProofRow, want []initialStoreRow) bool {
	if len(existing) != len(want) {
		return false
	}
	// 两边都按 kind 升序:readPlayerStoreProofRows 按 kind 排序,initialPlayerStoreRows 的 kind 就是升序。
	for i, row := range want {
		got := existing[i]
		if got.kind != int32(row.kind) || got.ownerEpoch != accountdirectory.InitialOwnerEpoch || got.saveSeq != accountdirectory.InitialSaveSeq ||
			uint64(got.schemaGen) > uint64(persistschema.Gen) || got.released != row.released || len(got.holderInstance) != 0 || !bytes.Equal(got.bytes, row.payload) {
			return false
		}
	}
	return true
}
