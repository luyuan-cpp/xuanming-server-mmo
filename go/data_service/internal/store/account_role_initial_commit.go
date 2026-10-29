package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"

	"google.golang.org/protobuf/reflect/protoreflect"
	base "proto/common/base"
	databasepb "proto/common/database"
	"shared/accountdirectory"
	"shared/generated/persistschema"
	"shared/placement"
	"shared/savebarrier"
)

// 只由正式迁移调用。旧角色只取得默认 0，绝不从现有 1:1 数字推断出生资格。
func ensureInitialPlayerCommitSchema(ctx context.Context, db *sql.DB, schema string) error {
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role' AND COLUMN_NAME='initial_player_commit'", schema).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := db.ExecContext(ctx, "ALTER TABLE account_role ADD COLUMN initial_player_commit BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:7'"); err != nil {
			return err
		}
	}
	return assertInitialPlayerCommitShape(ctx, db, schema)
}

// 初始证据必须核实际完整行，不能只凭可与旧围栏重号的 1:1 元组。
func initialSQLRowMatches(ctx context.Context, tx *sql.Tx, schema, table string, player uint64, fields protoreflect.FieldDescriptors) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM `"+schema+"`.`"+table+"` WHERE player_id=? FOR UPDATE", player)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return false, err
	}
	if len(columns) != fields.Len() {
		return false, nil
	}
	if !rows.Next() {
		return false, rows.Err()
	}
	values := make([]sql.RawBytes, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return false, err
	}
	seen := make(map[string]bool, len(columns))
	for i, name := range columns {
		field := fields.ByName(protoreflect.Name(name))
		if field == nil || seen[name] {
			return false, nil
		}
		seen[name] = true
		var want uint64
		switch name {
		case "player_id":
			want = player
		case "fence_owner_epoch":
			want = savebarrier.InitialOwnerEpoch
		case "fence_save_seq":
			want = savebarrier.InitialSaveSeq
		case "persist_schema_gen":
			gen, e := strconv.ParseUint(string(values[i]), 10, 32)
			if e != nil || gen == 0 || gen > uint64(persistschema.Gen) {
				return false, nil
			}
			continue
		default:
			if len(values[i]) == 0 {
				continue
			}
			switch field.Kind() {
			case protoreflect.MessageKind, protoreflect.BytesKind, protoreflect.StringKind:
				return false, nil
			default:
				if string(values[i]) != "0" {
					return false, nil
				}
			}
			continue
		}
		if string(values[i]) != strconv.FormatUint(want, 10) {
			return false, nil
		}
	}
	for _, name := range []string{"player_id", "fence_owner_epoch", "fence_save_seq", "persist_schema_gen"} {
		if !seen[name] {
			return false, nil
		}
	}
	if rows.Next() {
		return false, errors.New("玩家初始行主键不唯一")
	}
	return rows.Err() == nil, rows.Err()
}

func assertInitialPlayerCommitShape(ctx context.Context, db *sql.DB, schema string) error {
	var dataType, columnType, nullable string
	var defaultValue sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,COLUMN_TYPE,IS_NULLABLE,COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role' AND COLUMN_NAME='initial_player_commit'", schema).Scan(&dataType, &columnType, &nullable, &defaultValue); err != nil {
		return err
	}
	if dataType != "bigint" || !strings.Contains(columnType, "unsigned") || nullable != "NO" || !defaultValue.Valid || defaultValue.String != "0" {
		return errors.New("角色初始持久提交标记列形状不符")
	}
	return nil
}

// ConfigureInitialPlayerCommit 与现有跨库 SQL 证据共用全局权威池。
// 白名单只限制可访问库，实际落点必须来自同一事务锁住的 player_route，绝不由 caller 指定。
func (s *AccountDirectoryStore) ConfigureInitialPlayerCommit(storageIDs []uint32) error {
	if s == nil || len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return accountdirectory.ErrUnavailable
	}
	allowed := make(map[uint32]bool, len(storageIDs))
	for _, id := range storageIDs {
		if _, ok := placement.StoreDBName(id); !ok || allowed[id] {
			return accountdirectory.ErrInvalid
		}
		allowed[id] = true
	}
	s.initialStores = allowed
	s.initialRequired = true
	return nil
}

// lockInitialPlayerRoute 必须在获取角色归属行锁之前调用。
// 复用正式角色路由前缀：中性合服分片 → 路由聚簇键，再允许调用者获取角色 P 和玩家行。
func (s *AccountDirectoryStore) lockInitialPlayerRoute(ctx context.Context, tx *sql.Tx, role *base.AccountSimplePlayer) error {
	if !s.initialRequired {
		return nil // 仅具名 LegacyNonGlobal 构造，不返回持久初始提交回执。
	}
	if len(s.initialStores) == 0 || role.GetZoneId() == 0 {
		return accountdirectory.ErrUnavailable
	}
	// 与冻结/反向修库串行，保证提交时仍可写；不让前置普通读代替提交锁。
	meta, err := readRouteMeta(ctx, tx, routeLockMetaSQL)
	if err != nil {
		return err
	}
	if meta.Frozen() {
		return ErrRouteNotWritable
	}
	return placement.LockRoleRoutes(ctx, tx, s.schema, []uint64{role.PlayerId})
}

func (s *AccountDirectoryStore) insertInitialPlayerRows(ctx context.Context, tx *sql.Tx, role *base.AccountSimplePlayer) (*databasepb.PlayerAllData, error) {
	if !s.initialRequired {
		return nil, nil
	}
	var row RouteRow
	if err := scanRouteRow(tx.QueryRowContext(ctx, routeLockRowSQL, placement.RouteRowShard(role.PlayerId), role.PlayerId), &row, false); err != nil {
		return nil, err
	}
	if row.HomeZone != role.ZoneId || row.Rev == 0 || row.FrozenRunID != "" {
		return nil, accountdirectory.ErrConflict
	}
	storage := row.StorageID
	if !row.HasPlacement() {
		storage = row.HomeZone
	}
	if !s.initialStores[storage] {
		return nil, accountdirectory.ErrUnavailable
	}
	schema, ok := placement.StoreDBName(storage)
	if !ok {
		return nil, accountdirectory.ErrInvalid
	}
	for _, table := range []string{"player_database", "player_database_1"} {
		// 标识符只由权威落点和两个固定表名产生。严格 INSERT：旧行/并发撞键整批回滚，不能 REPLACE/ODKU。
		query := "INSERT INTO `" + schema + "`.`" + table + "` (player_id,fence_owner_epoch,fence_save_seq,persist_schema_gen) VALUES (?,?,?,?)"
		result, err := tx.ExecContext(ctx, query, role.PlayerId, savebarrier.InitialOwnerEpoch, savebarrier.InitialSaveSeq, persistschema.Gen)
		if err != nil {
			var duplicate *mysql.MySQLError
			if errors.As(err, &duplicate) && duplicate.Number == 1062 {
				return nil, accountdirectory.ErrConflict
			}
			return nil, fmt.Errorf("提交玩家初始持久行 %s: %w", table, err)
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return nil, errors.New("玩家初始持久行未严格插入一行")
		}
	}
	return savebarrier.InitialPlayerData(role.PlayerId), nil
}
