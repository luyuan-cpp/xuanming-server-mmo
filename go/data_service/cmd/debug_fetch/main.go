// debug_fetch 把线上一个玩家的存档和相关记录导出成一个 JSON 包(docs/ops/online-debug-data-fetch.md)。
//
// 只读:连接用 authoritysql 的 Reader 权限打开,只发固定的 SELECT。DSN 从环境变量取,工具不打印它。
// 包里有账号名和完整存档,文件只给当前用户读写,不要放进仓库或工单附件。
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"

	"data_service/internal/playerbundle"
	"shared/authoritysql"
)

type options struct {
	dsnEnv     string
	globalDB   string
	player     uint64
	reason     string
	revision   string
	out        string
	limit      int
	timeout    time.Duration
	skipGlobal bool
}

func main() {
	var o options
	flag.StringVar(&o.dsnEnv, "dsn-env", "", "name of the environment variable holding the MySQL/TiDB DSN (required)")
	flag.StringVar(&o.globalDB, "global-db", "mmorpg_global", "global schema holding player_route")
	flag.Uint64Var(&o.player, "player", 0, "player id (required)")
	flag.StringVar(&o.reason, "reason", "", "ticket or report the export is for (required)")
	flag.StringVar(&o.revision, "server-revision", "", "server image revision; decoding uses the local checkout, so check out this revision first")
	flag.StringVar(&o.out, "out", "", "output file (required)")
	flag.IntVar(&o.limit, "limit", 20, "max rows from player_snapshot and transaction_log")
	flag.DurationVar(&o.timeout, "timeout", time.Minute, "overall timeout")
	flag.BoolVar(&o.skipGlobal, "skip-global", false, "export only the route and player_store rows")
	flag.Parse()
	if err := run(o); err != nil {
		log.Fatalf("debug_fetch: %v", err)
	}
}

func run(o options) error {
	if o.dsnEnv == "" || o.player == 0 || o.reason == "" || o.out == "" {
		return errors.New("-dsn-env, -player, -reason and -out are required")
	}
	dsn := os.Getenv(o.dsnEnv)
	if dsn == "" {
		return fmt.Errorf("environment variable %s is empty", o.dsnEnv)
	}
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	global, err := openReader(ctx, dsn, o.globalDB, backend)
	if err != nil {
		return err
	}
	defer global.Close()
	route, found, err := playerbundle.ReadRoute(ctx, global, o.player)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("player %d has no route with a storage target", o.player)
	}
	store, err := openReader(ctx, dsn, route.StoreSchema, backend)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := readStoreRows(ctx, store, o.player)
	if err != nil {
		return err
	}

	bundle := &playerbundle.Bundle{
		Format:         playerbundle.Format,
		PlayerID:       o.player,
		ExportedAtMs:   time.Now().UnixMilli(),
		Reason:         o.reason,
		ServerRevision: o.revision,
		Analysis:       playerbundle.Analyze(route, rows),
		Route:          route,
		Rows:           rows,
	}
	if !o.skipGlobal {
		bundle.Global = readGlobal(ctx, global, o.player, o.limit)
	}
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.out, raw, 0o600); err != nil {
		return err
	}
	fmt.Printf("exported player %d from %s: %d rows, save_seq=%d online=%v frozen=%v warnings=%d -> %s\n", o.player, route.StoreSchema, len(rows),
		bundle.Analysis.SaveSeq, bundle.Analysis.Online, bundle.Analysis.Frozen, len(bundle.Analysis.Warnings), o.out)
	return nil
}

func openReader(ctx context.Context, dsn, schema string, backend authoritysql.Backend) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("cannot parse the DSN") // 解析错误会带上 DSN 原文,不往外打
	}
	cfg.DBName = schema
	return authoritysql.Open(ctx, cfg.FormatDSN(), schema, backend, 2, authoritysql.Reader())
}

// readStoreRows 一条语句读出全部行:写权行和数据行是同一个时刻的。
func readStoreRows(ctx context.Context, store *sql.DB, playerID uint64) ([]playerbundle.Row, error) {
	rows, err := store.QueryContext(ctx, "SELECT kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload FROM player_store WHERE player_id=? ORDER BY kind", playerID)
	if err != nil {
		return nil, fmt.Errorf("read player_store: %w", err)
	}
	defer rows.Close()
	var out []playerbundle.Row
	for rows.Next() {
		var row playerbundle.Row
		var holder []byte
		if err := rows.Scan(&row.Kind, &row.OwnerEpoch, &row.SaveSeq, &row.SchemaGen, &holder, &row.Released, &row.Payload); err != nil {
			return nil, fmt.Errorf("scan player_store: %w", err)
		}
		row.HolderInstance = string(holder)
		row.Decode()
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read player_store: %w", err)
	}
	return out, nil
}

// readGlobal 读账号归属、快照和流水。读不到只记下来:存档已经到手,不能因为这些丢掉整个导出。
func readGlobal(ctx context.Context, global *sql.DB, playerID uint64, limit int) playerbundle.Global {
	var out playerbundle.Global
	read := func(name, query string, args ...any) []map[string]any {
		rows, err := queryMaps(ctx, global, query, args...)
		if err != nil {
			out.Errors = append(out.Errors, name+": "+err.Error())
		}
		return rows
	}
	out.Account = read("account_role", "SELECT account,owner_revision,role_proto FROM account_role WHERE player_id=?", playerID)
	out.Snapshots = read("player_snapshot", "SELECT * FROM player_snapshot WHERE player_id=? ORDER BY created_at DESC LIMIT ?", playerID, limit)
	out.TransactionLog = read("transaction_log",
		"SELECT * FROM (SELECT * FROM transaction_log WHERE from_player=? ORDER BY timestamp_sec DESC LIMIT ?) a "+
			"UNION SELECT * FROM (SELECT * FROM transaction_log WHERE to_player=? ORDER BY timestamp_sec DESC LIMIT ?) b",
		playerID, limit, playerID, limit)
	return out
}

func queryMaps(ctx context.Context, db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = jsonValue(values[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// jsonValue 让二进制列能进 JSON:能当文本读的按文本,其余 base64。
func jsonValue(value any) any {
	raw, ok := value.([]byte)
	if !ok {
		return value
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	return base64.StdEncoding.EncodeToString(raw)
}
