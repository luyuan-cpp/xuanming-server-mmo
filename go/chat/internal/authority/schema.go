// Package authority拥有聊天的SQL准入原件；Redis不参与授权、决定或历史选择。
package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"shared/authoritysql"
	"strings"

	"chat/internal/constants"
)

const DatabaseName = "mmorpg_chat"

// 仅复用既有SendChatRequest/ChatMessage原件；这些列是本域内部元数据，没有平行PB结构。
//
// 保留期(2026-11-12):原件保留 constants.OriginalRetentionDays 天,到期由 TiDB 的 TTL 任务在库里删除。
//   - 运行账号仍然只有 SELECT / INSERT:删除不经过应用,被攻破的 chat 进程改不了、删不了已登记的原件;
//   - TTL 只认 DATE / DATETIME / TIMESTAMP 列,所以另存一列 accepted_at(与 accepted_ms 同一时刻,UTC,见 store.go 的 acceptedAt);
//     不用 TIMESTAMP 是因为它到 2038 年为止;
//   - 属性写在 TiDB 专用注释里(SHOW CREATE TABLE 自己就这样输出):MySQL 当普通注释忽略,开发用 MySQL 时表照建、只是不删;
//   - 每天扫一次就够(保留期按天计);TTL 只会晚删,不会早删。
//
// 搬库(tools/dbmove)期间目标集群上这张表要先关 TTL,切换后再开(docs/design/chat-world-channel-fanout.md §5):
// 两边各自跑 TTL 任务时删的行不同步,固定快照上的校验和会对不上。
var sendDDL = `CREATE TABLE IF NOT EXISTS chat_send_authority (
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
accepted_at DATETIME(3) NOT NULL,
PRIMARY KEY(player_id,request_id),
KEY idx_chat_stream(stream_key,state,accepted_ms,player_id,request_id),
KEY idx_chat_rate(player_id,state,accepted_ms)
) ENGINE=InnoDB ` + retentionAttributes

// retentionAttributes 是 chat_send_authority 的 TTL 属性。天数取自 constants.OriginalRetentionDays(同时钉在策略行里)。
var retentionAttributes = fmt.Sprintf("/*T![ttl] TTL=`accepted_at` + INTERVAL %d DAY */ /*T![ttl] TTL_ENABLE='ON' */ /*T![ttl] TTL_JOB_INTERVAL='24h' */", constants.OriginalRetentionDays)

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
		"SELECT player_id,request_id,owner_account,owner_revision,request_digest,request_payload,message_payload,stream_key,state,accepted_ms,result_tip,accepted_at FROM chat_send_authority LIMIT 0",
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
		if kind == "datetime" {
			shape = full // 带小数秒位数,如 datetime(3)
		}
		shapes[table+"/"+column] = shape
	}
	if err = columns.Err(); err != nil {
		return err
	}
	for name, expected := range map[string]string{
		"chat_send_authority/player_id": "bigint unsigned", "chat_send_authority/request_id": "varbinary/64", "chat_send_authority/owner_account": "varbinary/191", "chat_send_authority/owner_revision": "bigint unsigned", "chat_send_authority/request_digest": "binary/32", "chat_send_authority/request_payload": "mediumblob", "chat_send_authority/message_payload": "mediumblob", "chat_send_authority/stream_key": "varbinary/191", "chat_send_authority/state": "tinyint unsigned", "chat_send_authority/accepted_ms": "bigint unsigned", "chat_send_authority/result_tip": "int unsigned", "chat_send_authority/accepted_at": "datetime(3)", "chat_policy/id": "tinyint unsigned", "chat_policy/policy_hash": "binary/32",
	} {
		if shapes[name] != expected {
			return errors.New("聊天权威SQL字段类型不符，须显式审阅迁移")
		}
	}
	return nil
}

// RetentionEnabled 报告 chat_send_authority 上按 accepted_at 的 TTL 属性是否在、是否开着(TiDB)。
// 只读 SHOW CREATE TABLE,运行账号有 SELECT 就能执行。MySQL 没有 TTL(属性写在 TiDB 专用注释里,被忽略),恒为 false,
// 调用方只在 TiDB 档上据此告警。
// 不并进 CheckSchema 拒启:保留期没生效的后果是表只增不减,数据没有错;拒启会把一次属性漂移变成全服聊天不可用。
func RetentionEnabled(ctx context.Context, db *sql.DB) (bool, error) {
	var table, ddl string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE chat_send_authority").Scan(&table, &ddl); err != nil {
		return false, err
	}
	return hasRetentionAttributes(ddl), nil
}

// hasRetentionAttributes 在建表语句里找「按 accepted_at、保留 OriginalRetentionDays 天的 TTL」且「TTL 开着」。
// 先去掉空白与反引号、统一大写再找:不同版本的 TiDB 输出的空格与引号不完全一样。
func hasRetentionAttributes(ddl string) bool {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "`", "").Replace(ddl))
	want := fmt.Sprintf("TTL=ACCEPTED_AT+INTERVAL%dDAY", constants.OriginalRetentionDays)
	return strings.Contains(normalized, want) && strings.Contains(normalized, "TTL_ENABLE='ON'")
}
