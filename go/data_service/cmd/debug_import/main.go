package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"data_service/cmd/debugutil"
)

// ── Export file structures ──────────────────────────────────────

type exportFile struct {
	Mode       string                `json:"mode"`
	PlayerID   uint64                `json:"player_id"`
	HomeZoneID uint32                `json:"home_zone_id"`
	Version    uint64                `json:"version"`
	Fields     map[string]fieldEntry `json:"fields"`

	// playerdb mode
	ZoneID   uint32       `json:"zone_id"`
	Database string       `json:"database"`
	Matches  []tableMatch `json:"matches"`
}

type fieldEntry struct {
	Kind      string `json:"kind"`
	Size      int    `json:"size"`
	RawBase64 string `json:"raw_base64"`
	Text      string `json:"text"`
}

type tableMatch struct {
	Table       string                      `json:"table"`
	MatchColumn string                      `json:"match_column"`
	Rows        []map[string]json.RawMessage `json:"rows"`
}

func main() {
	var (
		configPath   = flag.String("config", "etc/data_service.yaml", "path to data_service config (for Redis import)")
		dbConfigPath = flag.String("db-config", "../db/etc/db.yaml", "path to db config (for MySQL import)")
		inputFile    = flag.String("file", "", "path to debug_fetch JSON export file (required)")
		dryRun       = flag.Bool("dry-run", true, "preview changes without writing; use --dry-run=false to apply")
		zoneID       = flag.Uint("zone", 0, "override zone ID for import (default: use value from export)")
		playerID     = flag.Uint64("player", 0, "override player ID for import (default: use value from export)")
		timeoutSec   = flag.Int("timeout-seconds", 30, "request timeout in seconds")
	)
	flag.Parse()

	if *inputFile == "" {
		fmt.Fprintln(os.Stderr, "error: -file is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	data, err := loadExportFile(*inputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load export: %v\n", err)
		os.Exit(1)
	}

	if *zoneID > 0 {
		data.HomeZoneID = uint32(*zoneID)
		data.ZoneID = uint32(*zoneID)
	}
	if *playerID > 0 {
		data.PlayerID = *playerID
	}

	switch data.Mode {
	case "player":
		err = importPlayerRedis(ctx, *configPath, data, *dryRun)
	case "playerdb":
		err = importPlayerDB(ctx, *dbConfigPath, data, *dryRun)
	default:
		err = fmt.Errorf("unsupported import mode %q (only player and playerdb exports are supported)", data.Mode)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "import failed: %v\n", err)
		os.Exit(1)
	}
}

// ── Player Redis import ─────────────────────────────────────────

func importPlayerRedis(ctx context.Context, configPath string, data *exportFile, dryRun bool) error {
	if data.PlayerID == 0 {
		return errors.New("export file has no player_id (use -player to override)")
	}

	// Decode all fields from base64
	fields := make(map[string][]byte, len(data.Fields))
	for name, entry := range data.Fields {
		if entry.RawBase64 == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(entry.RawBase64)
		if err != nil {
			return fmt.Errorf("decode base64 for field %q: %w", name, err)
		}
		fields[name] = raw
	}

	if len(fields) == 0 {
		return errors.New("no importable fields found (all fields lack raw_base64; re-export with latest debug_fetch)")
	}

	fieldNames := make([]string, 0, len(fields))
	for name := range fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)

	fmt.Fprintf(os.Stderr, "=== Import Preview (Redis) ===\n")
	fmt.Fprintf(os.Stderr, "  Player:    %d\n", data.PlayerID)
	fmt.Fprintf(os.Stderr, "  Zone:      %d\n", data.HomeZoneID)
	fmt.Fprintf(os.Stderr, "  Fields:    %d\n", len(fields))
	for _, name := range fieldNames {
		fmt.Fprintf(os.Stderr, "    %-30s %6d bytes\n", name, len(fields[name]))
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "\n[DRY RUN] No changes written. Use --dry-run=false to apply.\n")
		return nil
	}

	svcCtx, cleanup, err := debugutil.NewServiceContext(configPath)
	if err != nil {
		return err
	}
	defer cleanup()

	// In dev mode, ClientForZone returns devClient for any zone.
	// In cluster mode, we need a valid zone ID.
	client, err := svcCtx.Router.ClientForZone(data.HomeZoneID)
	if err != nil {
		return fmt.Errorf("get Redis client for zone %d: %w", data.HomeZoneID, err)
	}

	// Write fields
	for _, name := range fieldNames {
		key := fmt.Sprintf("player:{%d}:%s", data.PlayerID, name)
		if err := client.Set(ctx, key, fields[name], 0).Err(); err != nil {
			return fmt.Errorf("set field %q: %w", name, err)
		}
	}

	// Reset version to 1
	versionKey := fmt.Sprintf("player:{%d}:__version", data.PlayerID)
	if err := client.Set(ctx, versionKey, "1", 0).Err(); err != nil {
		return fmt.Errorf("set version: %w", err)
	}

	// Register player -> zone mapping.
	// RegisterPlayerZone 是 SETNX 语义:导入到一个**已有别的归属**的 player_id 上会
	// 报错而不是覆盖(值相同则幂等)。对导入工具来说这正是想要的 —— 覆盖会把一名
	// 已经被合服搬到新区的玩家静默送回源区,而这里的失败只是让人换个 -player 重来。
	if data.HomeZoneID > 0 {
		if err := svcCtx.Router.RegisterPlayerZone(ctx, data.PlayerID, data.HomeZoneID); err != nil {
			return fmt.Errorf("register zone mapping: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  Registered mapping: player %d -> zone %d\n", data.PlayerID, data.HomeZoneID)
	}

	fmt.Fprintf(os.Stderr, "\n[OK] Imported %d fields for player %d to local Redis\n", len(fields), data.PlayerID)
	return nil
}

// ── Player DB (MySQL) import ────────────────────────────────────

func importPlayerDB(ctx context.Context, dbConfigPath string, data *exportFile, dryRun bool) error {
	if data.PlayerID == 0 {
		return errors.New("export file has no player_id (use -player to override)")
	}
	if len(data.Matches) == 0 {
		return errors.New("export file has no table matches to import")
	}

	zoneID := data.ZoneID
	if zoneID == 0 {
		zoneID = data.HomeZoneID
	}

	// Parse all rows first to catch decode errors before writing
	var tables []tableImport
	totalRows := 0

	for _, match := range data.Matches {
		if len(match.Rows) == 0 {
			continue
		}

		// Discover columns from first row
		colSet := map[string]bool{}
		for _, row := range match.Rows {
			for col := range row {
				colSet[col] = true
			}
		}
		cols := make([]string, 0, len(colSet))
		for col := range colSet {
			cols = append(cols, col)
		}
		sort.Strings(cols)

		parsedRows := make([][]any, 0, len(match.Rows))
		for i, row := range match.Rows {
			values := make([]any, len(cols))
			for j, col := range cols {
				raw, ok := row[col]
				if !ok {
					values[j] = nil
					continue
				}
				v, err := resolveColumnValue(raw)
				if err != nil {
					return fmt.Errorf("table %s row %d column %s: %w", match.Table, i, col, err)
				}
				values[j] = v
			}
			parsedRows = append(parsedRows, values)
		}

		tables = append(tables, tableImport{
			Table:       match.Table,
			MatchColumn: match.MatchColumn,
			Columns:     cols,
			Rows:        parsedRows,
		})
		totalRows += len(parsedRows)
	}

	fmt.Fprintf(os.Stderr, "=== Import Preview (MySQL) ===\n")
	fmt.Fprintf(os.Stderr, "  Player:    %d\n", data.PlayerID)
	fmt.Fprintf(os.Stderr, "  Zone:      %d\n", zoneID)
	fmt.Fprintf(os.Stderr, "  Database:  %s\n", data.Database)
	fmt.Fprintf(os.Stderr, "  Tables:    %d\n", len(tables))
	for _, t := range tables {
		fmt.Fprintf(os.Stderr, "    %-30s %d rows (%d cols)\n", t.Table, len(t.Rows), len(t.Columns))
	}
	fmt.Fprintf(os.Stderr, "  Total rows: %d\n", totalRows)

	if dryRun {
		fmt.Fprintf(os.Stderr, "\n[DRY RUN] No changes written. Use --dry-run=false to apply.\n")
		return nil
	}

	db, _, _, err := debugutil.OpenZoneDB(ctx, dbConfigPath, zoneID)
	if err != nil {
		return err
	}
	defer db.Close()

	// 表的处理顺序 = 导出文件里 matches 数组的顺序(JSON 数组,有序;debug_fetch 按 table_name, column_name
	// ORDER BY 产出),不是 Go map 的随机迭代序。而且下面**每张表单独一个事务**、提交后才碰下一张表,
	// 任何时刻一个导入事务只持有一张表上的锁,两个并发导入之间不存在"跨表取锁顺序相反"的环 ——
	// 所以这里不需要再按表名重排。以后若有人把多张表并进同一个事务(比如为了整体原子),
	// 必须先按表名排序再遍历,否则两份表序不同的导入文件会跨表成环。
	for _, t := range tables {
		safeTable, err := debugutil.QuoteIdentifier(t.Table)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [SKIP] table %q: unsafe name\n", t.Table)
			continue
		}
		if err := importTable(ctx, db, safeTable, t, data.PlayerID); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  [OK] %s: deleted old + inserted %d rows\n", t.Table, len(t.Rows))
	}

	fmt.Fprintf(os.Stderr, "\n[OK] Imported %d rows across %d tables for player %d\n", totalRows, len(tables), data.PlayerID)
	return nil
}

// tableImport 是一张表待导入的内容:列名(已排序)与按列序展开的行。
type tableImport struct {
	Table       string
	MatchColumn string
	Columns     []string
	Rows        [][]any // each row = ordered column values
}

// importTable 在**一个** READ COMMITTED 事务里对一张表做"DELETE 该玩家旧行 + INSERT 导出行"并提交。
// safeTable 是已经过 QuoteIdentifier 的表名;列名与 match 列在这里校验。
//
// 拆成独立函数是为了让"开导入事务"这个调用点能被单测直接覆盖(main_test.go 用记录 TxOptions 的假驱动跑它):
// 有人把下面的 BeginTx 改回 BeginTx(ctx, nil)(= 库默认 RR,见 importTableTxOptions 的成环分析),单测立刻变红。
func importTable(ctx context.Context, db *sql.DB, safeTable string, t tableImport, playerID uint64) error {
	var err error
	safeCols := make([]string, len(t.Columns))
	for i, col := range t.Columns {
		safeCols[i], err = debugutil.QuoteIdentifier(col)
		if err != nil {
			return fmt.Errorf("table %s: unsafe column name %q", t.Table, col)
		}
	}

	// DELETE existing rows for this player, then INSERT
	safeMatchCol, err := debugutil.QuoteIdentifier(t.MatchColumn)
	if err != nil {
		return fmt.Errorf("table %s: unsafe match column %q", t.Table, t.MatchColumn)
	}

	// 显式 READ COMMITTED,不用库默认的 RR:RR 下"DELETE 未命中 + INSERT"会在主键间隙上成环,见 importTableTxOptions。
	tx, err := db.BeginTx(ctx, importTableTxOptions())
	if err != nil {
		return fmt.Errorf("begin tx for table %s: %w", t.Table, err)
	}

	delSQL := fmt.Sprintf("DELETE FROM %s WHERE %s = ?", safeTable, safeMatchCol)
	if _, err := tx.ExecContext(ctx, delSQL, playerID); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete from %s: %w", t.Table, err)
	}

	placeholders := strings.Repeat("?,", len(t.Columns))
	placeholders = placeholders[:len(placeholders)-1]
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		safeTable,
		strings.Join(safeCols, ", "),
		placeholders,
	)

	for _, row := range t.Rows {
		if _, err := tx.ExecContext(ctx, insertSQL, row...); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert into %s: %w", t.Table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit table %s: %w", t.Table, err)
	}
	return nil
}

// importTableTxOptions 是每张表"DELETE 该玩家旧行 + INSERT 导出行"那个事务的选项:READ COMMITTED
// (go-sql-driver 在 START TRANSACTION 前发 SET TRANSACTION ISOLATION LEVEL,只作用于这一个事务)。
//
// 用库的默认 RR 会成环(审计 #13):玩家行不存在时,RR 下 DELETE 未命中也要在主键间隙上拿 X 间隙锁,
// 两个并发导入(不同玩家但落在同一段间隙,例如 player_id 都大于表内最大值;或同一玩家导两次)各拿到
// 同一段间隙锁(间隙锁彼此兼容),随后各自 INSERT 申请插入意向锁,又都被对方的间隙锁挡住 → 1213。
// RC 下 DELETE 未命中不加间隙锁;match_column 没有索引要全表扫时,不匹配的行在判定后立即放锁,
// 两条成环路径都消失。DELETE 与 INSERT 仍在同一个事务里原子提交,导出行比库里少时多余的行照样被删。
//
// 同一玩家被两个人同时导入同一张单行表时,RC 下后到者只会在主键记录上单向等先到者提交(之后要么
// 覆盖先到者的行,要么撞 1062 整张表回滚),不成环。剩下一种有界情形:match_column 不是主键、同一玩家
// 有多行、且两份导出的行序不同,两个并发导入仍可能在各自插入的主键上交叉等待而 1213 —— 那本身就是
// "同一玩家被并发导入两次"的操作失误,败者整张表回滚、不写半截;要彻底杜绝得把同库导入串行化
// (GET_LOCK),本工具没有做。
//
// 前提:RC 要求 binlog_format 为 ROW/MIXED(STATEMENT 下 DELETE 报 1665)。MySQL 8 默认 ROW,
// deploy/k8s/manifests/infra/mysql.yaml 也显式写了 binlog_format = ROW。
func importTableTxOptions() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelReadCommitted}
}

// ── Shared helpers ──────────────────────────────────────────────

func loadExportFile(path string) (*exportFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	var data exportFile
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	if data.Mode == "" {
		return nil, errors.New("export file missing 'mode' field")
	}
	return &data, nil
}

// resolveColumnValue turns a JSON-decoded cell back into a Go value
// suitable for sql.Exec. renderedValue objects (with raw_base64) become []byte.
func resolveColumnValue(raw json.RawMessage) (any, error) {
	if string(raw) == "null" {
		return nil, nil
	}

	// Try to parse as object with raw_base64 (renderedValue)
	var rv struct {
		RawBase64 string `json:"raw_base64"`
		Kind      string `json:"kind"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(raw, &rv); err == nil && rv.Kind != "" {
		if rv.RawBase64 != "" {
			return base64.StdEncoding.DecodeString(rv.RawBase64)
		}
		if rv.Kind == "text" {
			return rv.Text, nil
		}
	}

	// Try string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Try number
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		// Preserve integer values
		if f == float64(int64(f)) {
			return int64(f), nil
		}
		return f, nil
	}

	// Try bool
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}

	// Fallback: store as string
	return string(raw), nil
}
