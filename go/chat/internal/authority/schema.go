// Package authority拥有聊天的SQL准入原件；Redis不参与授权、决定或历史选择。
package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"shared/authoritysql"
	"strings"
)

const DatabaseName = "mmorpg_chat"

// 仅复用既有SendChatRequest/ChatMessage原件；这些列是本域内部元数据，没有平行PB结构。
const sendDDL = `CREATE TABLE IF NOT EXISTS chat_send_authority (
player_id BIGINT UNSIGNED NOT NULL,
request_id VARBINARY(64) NOT NULL,
owner_account VARBINARY(191) NOT NULL,
owner_revision BIGINT UNSIGNED NOT NULL,
request_digest BINARY(32) NOT NULL,
request_payload MEDIUMBLOB NOT NULL,
message_payload MEDIUMBLOB NOT NULL,
stream_key VARBINARY(191) NOT NULL,
state TINYINT UNSIGNED NOT NULL,
accepted_ms BIGINT UNSIGNED NOT NULL,
result_tip INT UNSIGNED NOT NULL,
PRIMARY KEY(player_id,request_id),
KEY idx_chat_stream(stream_key,state,accepted_ms,player_id,request_id),
KEY idx_chat_rate(player_id,state,accepted_ms)
) ENGINE=InnoDB`

const policyDDL = `CREATE TABLE IF NOT EXISTS chat_policy (
id TINYINT UNSIGNED NOT NULL,
policy_hash BINARY(32) NOT NULL,
PRIMARY KEY(id)
) ENGINE=InnoDB`

// Migrate只供显式-migrate入口；普通启动不修表，不把历史结构不明降级成Redis。
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, ddl := range []string{sendDDL, policyDDL} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	var database string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return err
	}
	// 闸门跟着逻辑库走:聊天库自带一份。
	if err := authoritysql.EnsureCatalogSchema(ctx, db, database); err != nil {
		return err
	}
	return CheckSchema(ctx, db)
}

func CheckSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("聊天权威SQL未装配")
	}
	for _, query := range []string{
		"SELECT player_id,request_id,owner_account,owner_revision,request_digest,request_payload,message_payload,stream_key,state,accepted_ms,result_tip FROM chat_send_authority LIMIT 0",
		"SELECT id,policy_hash FROM chat_policy LIMIT 0",
	} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT table_name,index_name,column_name,non_unique,sub_part FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name IN ('chat_send_authority','chat_policy') ORDER BY table_name,index_name,seq_in_index")
	if err != nil {
		return err
	}
	defer rows.Close()
	indexes := map[string][]string{}
	for rows.Next() {
		var table, name, column string
		var nonUnique int
		var prefix sql.NullInt64
		if err = rows.Scan(&table, &name, &column, &nonUnique, &prefix); err != nil {
			return err
		}
		if prefix.Valid || (name == "PRIMARY" && nonUnique != 0) || (strings.HasPrefix(name, "idx_chat_") && nonUnique != 1) {
			return errors.New("聊天权威SQL索引前缀或唯一性不符")
		}
		indexes[table+"/"+name] = append(indexes[table+"/"+name], column)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for name, expected := range map[string]string{
		"chat_send_authority/PRIMARY":         "player_id,request_id",
		"chat_send_authority/idx_chat_stream": "stream_key,state,accepted_ms,player_id,request_id",
		"chat_send_authority/idx_chat_rate":   "player_id,state,accepted_ms",
		"chat_policy/PRIMARY":                 "id",
	} {
		if strings.Join(indexes[name], ",") != expected {
			return errors.New("聊天权威SQL必要索引不符，须显式迁移")
		}
	}
	var nullable int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name IN ('chat_send_authority','chat_policy') AND is_nullable<>'NO'").Scan(&nullable); err != nil {
		return err
	}
	if nullable != 0 {
		return errors.New("聊天权威SQL列不可为NULL")
	}
	// PK必须按原字节区分大小写；修订与时间必须保全uint64，不能等首次写入才发现窄化。
	columns, err := db.QueryContext(ctx, "SELECT table_name,column_name,data_type,column_type,character_maximum_length FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name IN ('chat_send_authority','chat_policy')")
	if err != nil {
		return err
	}
	defer columns.Close()
	shapes := map[string]string{}
	for columns.Next() {
		var table, column, kind, full string
		var length sql.NullInt64
		if err = columns.Scan(&table, &column, &kind, &full, &length); err != nil {
			return err
		}
		shape := kind
		if strings.Contains(full, "unsigned") {
			shape += " unsigned"
		}
		if length.Valid && (kind == "binary" || kind == "varbinary") {
			shape += "/" + fmt.Sprint(length.Int64)
		}
		shapes[table+"/"+column] = shape
	}
	if err = columns.Err(); err != nil {
		return err
	}
	for name, expected := range map[string]string{
		"chat_send_authority/player_id": "bigint unsigned", "chat_send_authority/request_id": "varbinary/64", "chat_send_authority/owner_account": "varbinary/191", "chat_send_authority/owner_revision": "bigint unsigned", "chat_send_authority/request_digest": "binary/32", "chat_send_authority/request_payload": "mediumblob", "chat_send_authority/message_payload": "mediumblob", "chat_send_authority/stream_key": "varbinary/191", "chat_send_authority/state": "tinyint unsigned", "chat_send_authority/accepted_ms": "bigint unsigned", "chat_send_authority/result_tip": "int unsigned", "chat_policy/id": "tinyint unsigned", "chat_policy/policy_hash": "binary/32",
	} {
		if shapes[name] != expected {
			return errors.New("聊天权威SQL字段类型不符，须显式审阅迁移")
		}
	}
	return nil
}
