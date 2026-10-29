package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"data_service/cmd/debugutil"
	"data_service/internal/config"
	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"

	goredis "github.com/redis/go-redis/v9"
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
	// Errors 是导出时查询失败的表(2026-10-19 起导出)。出错的表没有导出行,导入会把它当成「该玩家没有行」。
	Errors []map[string]string `json:"errors"`
}

type fieldEntry struct {
	Kind      string `json:"kind"`
	Size      int    `json:"size"`
	RawBase64 string `json:"raw_base64"`
	Text      string `json:"text"`
}

type tableMatch struct {
	Table       string `json:"table"`
	MatchColumn string `json:"match_column"`
	// Truncated 为 nil 表示旧版导出(没有这个标记,无法判断是否被 -limit 截断),一律拒绝导入。
	Truncated *bool                        `json:"truncated"`
	Rows      []map[string]json.RawMessage `json:"rows"`
}

// fencedPlayerTables 是带行内围栏、由 login 按存盘屏障冷加载的两张表。旧 playerdb 导入只写行、不对齐屏障与 owner_epoch,
// 写进去的玩家本地加载不了,后续存盘还会被围栏判成 stale_owner;这两张表只能经 player-bundle 导入。
var fencedPlayerTables = map[string]bool{"player_database": true, "player_database_1": true}

func main() {
	if err := runDebugImport(); err != nil {
		fmt.Fprintf(os.Stderr, "import failed: %v\n", err)
		os.Exit(1)
	}
}

// 所有实际清理在此函数返回前完成；main 的 os.Exit 不跨过资源 defer。
func runDebugImport() error {
	var (
		configPath   = flag.String("config", "etc/data_service.yaml", "path to data_service config (Redis import / player:zone mapping)")
		dbConfigPath = flag.String("db-config", "../db/etc/db.yaml", "path to db config (MySQL import; player-bundle: also core Redis)")
		inputFile    = flag.String("file", "", "path to debug_fetch JSON export file (required)")
		dryRun       = flag.Bool("dry-run", true, "preview changes without writing; use --dry-run=false to apply")
		zoneID       = flag.Uint("zone", 0, "override zone ID for import (player-bundle: local zone, default db config ZoneId)")
		playerID     = flag.Uint64("player", 0, "override player ID for import (default: use value from export; not allowed for player-bundle)")
		timeoutSec   = flag.Int("timeout-seconds", 30, "request timeout in seconds")
		runtimePath  = flag.String("maintenance-drain-plan", "", "固定 NORMAL boot 的正式排空 runtime")
		runtimeDir   = flag.String("maintenance-drain-dir", "", "固定 runtime 私有目录")
		runtimeSHA   = flag.String("maintenance-drain-sha256", "", "固定 runtime 原件 SHA256")
		source       = flag.String("source", sourceAuto, "player-bundle: auto | redis-parent | sql")
		localAccount = flag.String("local-account", "", "player-bundle: local dev_/robot_ account that will own the imported role")
		replace      = flag.Bool("replace", false, "player-bundle: overwrite an existing local save of the same player_id")
		allowHosts   = flag.String("allow-target-host", "", "comma-separated non-loopback hosts allowed as write targets (local docker service names)")
		allowFenced  = flag.Bool("allow-fenced-tables", false, "legacy playerdb: write player_database / player_database_1 rows without aligning the save barrier (the player will NOT load)")

		// r2-ds#1(2026-10-22):写目标证明不了不是导出来源(MySQL 身份取不到 / 无同角色来源可比对、所有 Redis 目标)
		// 且没有本地调试环境标记时,缺省拒绝写入。旧 player / playerdb 导出不记来源身份,每个写目标都要标记或本开关。
		allowUnverifiedTarget = flag.Bool("allow-unverified-target-identity", false, "write even when a target cannot be proven distinct from the export source and carries no local marker (debug_import_local_target table / debug_import:local_target key). player-bundle: TiDB, unmatched MySQL sources and every Redis target need it; legacy player / playerdb exports record no source identity, so every target needs it. Prints a WARNING per target")
	)
	flag.Parse()

	if *inputFile == "" {
		return errors.New("-file is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	raw, err := os.ReadFile(*inputFile)
	if err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	if isBundleFile(raw) {
		if *playerID > 0 {
			return errors.New("player-bundle 不支持 -player 换号导入(blob 内的身份不会跟着改);本地撞号时用 -replace 覆盖")
		}
		return runNormalImport(ctx, *dryRun, *runtimePath, *runtimeDir, *runtimeSHA, func(permission authoritysql.Permission) error {
			return runBundleImport(ctx, raw, bundleImportOptions{
				permission:   permission,
				dbConfigPath: *dbConfigPath, configPath: *configPath, localZone: uint32(*zoneID),
				source: *source, localAccount: *localAccount, replace: *replace,
				dryRun: *dryRun, allowHosts: *allowHosts, allowUnverifiedTarget: *allowUnverifiedTarget,
			})
		})
	}

	data, err := parseExportFile(raw)
	if err != nil {
		return fmt.Errorf("load export: %w", err)
	}

	if *zoneID > 0 {
		data.HomeZoneID = uint32(*zoneID)
		data.ZoneID = uint32(*zoneID)
	}
	if *playerID > 0 && *playerID != data.PlayerID {
		overridePlayerID(data, *playerID)
	}

	if data.Mode != "player" && data.Mode != "playerdb" {
		return fmt.Errorf("unsupported import mode %q (only player and playerdb exports are supported)", data.Mode)
	}
	return runNormalImport(ctx, *dryRun, *runtimePath, *runtimeDir, *runtimeSHA, func(permission authoritysql.Permission) error {
		legacyOpts := legacyImportOptions{
			permission: permission,
			dryRun:     *dryRun, allowHosts: *allowHosts, allowFencedTables: *allowFenced, allowUnverifiedTarget: *allowUnverifiedTarget,
		}
		switch data.Mode {
		case "player":
			fmt.Fprintln(os.Stderr, "注意:player 导出写回的是 data_service 的 player:{id}:<field> 键族,不在登录加载链路上;本地复现玩家请用 player-bundle")
			err = importPlayerRedis(ctx, *configPath, data, legacyOpts)
		case "playerdb":
			err = importPlayerDB(ctx, *dbConfigPath, data, legacyOpts)
		default:
			err = fmt.Errorf("unsupported import mode %q (only player and playerdb exports are supported)", data.Mode)
		}

		return err
	})
}

// runNormalImport 只为真实有限命令装配一次 NORMAL boot；预览不消费 grant，也不制造闭合原件。
func runNormalImport(ctx context.Context, dryRun bool, plan, directory, digest string, call func(authoritysql.Permission) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dryRun {
		if plan != "" || directory != "" || digest != "" {
			return errors.New("只读预览不绑定NORMAL排空runtime")
		}
		return call(authoritysql.Reader())
	}
	var runtime *drainproof.Runtime
	if plan != "" || directory != "" || digest != "" {
		var err error
		runtime, err = drainproof.LoadRuntime(plan, directory, digest)
		if err != nil {
			return err
		}
		if runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil ||
			runtime.Request.RunPhase == databasepb.SqlWriterDrainRun_SQL_RELEASED {
			return drainproof.ErrHold
		}
	}
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return err
	}
	return executeNormalImport(ctx, writer, runtime, call)
}

// SQL/Redis 与 Router 发布全部同步返回；双角色票据覆盖命令及其实际资源清理，不造后台线程。
func executeNormalImport(ctx context.Context, writer authoritysql.WriterConfig, runtime *drainproof.Runtime, call func(authoritysql.Permission) error) (runErr error) {
	if len(writer.InstanceID()) != 16 || len(writer.WriterKey()) != 16 {
		return errors.New("有限导入缺少真实NORMAL boot")
	}
	defer writer.Seal()
	root, relay := drainproof.New(), drainproof.New()
	if runtime != nil {
		if runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
			return drainproof.ErrHold
		}
		if err := runtime.BindBoot(writer.InstanceID(), writer.WriterKey()); err != nil {
			return err
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			root.Hold()
			relay.Hold()
			panic(recovered)
		}
		root.CloseAdmission()
		relay.CloseAdmission()
		// call 已同步返回且其 defer 已结束。超时或未知不能写成功原件。
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, root.Wait(closeCtx), relay.Wait(closeCtx))
		writer.Seal()
		if runErr == nil && runtime != nil {
			runErr = errors.Join(runtime.Write("SOURCE", root), runtime.Write("RELAY", relay))
		}
	}()
	finishRoot, err := root.Enter()
	if err != nil {
		return err
	}
	defer finishRoot()
	finishRelay, err := relay.Enter()
	if err != nil {
		return err
	}
	defer finishRelay()
	runErr = call(authoritysql.Writer(writer))
	if runErr != nil {
		root.Hold()
		relay.Hold()
	}
	return runErr
}

// legacyImportOptions 是旧 player / playerdb 两种导入共用的开关。
type legacyImportOptions struct {
	permission authoritysql.Permission
	dryRun     bool
	allowHosts string
	// allowFencedTables 只对 playerdb:放行两张带行内围栏的表(只写行、不对齐屏障,本地 login 必拒)。
	allowFencedTables bool
	// allowUnverifiedTarget:写目标没有本地调试环境标记时仍放行,逐个打 WARNING。旧导出文件不记来源实例身份,
	// 标记是唯一的证明(2026-10-22,r2-ds#1 评审补充)。
	allowUnverifiedTarget bool
}

// ── Player Redis import ─────────────────────────────────────────

func importPlayerRedis(ctx context.Context, configPath string, data *exportFile, opts legacyImportOptions) (runErr error) {
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

	if opts.dryRun {
		fmt.Fprintf(os.Stderr, "\n[DRY RUN] No changes written. Use --dry-run=false to apply.\n")
		return nil
	}

	// 写前闸门:玩家字段写进 Router 选出的数据实例(DevRedis,或按 zone 的 Regions 集群),映射写进 MappingRedis。
	dsCfg, err := debugutil.LoadDataServiceConfig(configPath)
	if err != nil {
		return err
	}
	if err := checkLegacyRedisTargets(dsCfg, data.HomeZoneID, opts.allowHosts); err != nil {
		return err
	}

	if opts.permission.Kind() != "writer" {
		return errors.New("实际导入缺少同boot NORMAL Permission")
	}
	svcCtx, cleanup, err := debugutil.NewServiceContextWithPermissionAndReport(configPath, opts.permission)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, cleanup()) }()

	// In dev mode, ClientForZone returns devClient for any zone.
	// In cluster mode, we need a valid zone ID.
	client, err := svcCtx.Router.ClientForZone(data.HomeZoneID)
	if err != nil {
		return fmt.Errorf("get Redis client for zone %d: %w", data.HomeZoneID, err)
	}
	// 写前闸门二:地址闸挡不住 port-forward 到 127.0.0.1 的线上实例,而旧导出文件不记来源实例身份,
	// 所以每个写目标都要凭本地调试环境标记证明自己是本地(2026-10-22,r2-ds#1 评审补充)。
	if err := checkLegacyPlayerTargetsMarked(ctx, dsCfg, client, data.HomeZoneID, opts.allowUnverifiedTarget, opts.permission); err != nil {
		return err
	}

	if err := writeLegacyPlayerFields(ctx, client, data.PlayerID, fieldNames, fields); err != nil {
		return err
	}

	// Register player -> zone mapping.
	// RegisterPlayerZone 是 SETNX 语义:导入到一个**已有别的归属**的 player_id 上会
	// 报错而不是覆盖(值相同则幂等)。对导入工具来说这正是想要的 —— 覆盖会把一名
	// 已经被合服搬到新区的玩家静默送回源区,而这里的失败只是让人换个 -player 重来。
	// storage_id 传 0 = 不钉落点记录:导出文件里没有落点信息,导入的玩家有效落点 = home_zone
	// (player-storage-placement.md §1),与引入落点之前的调试导入行为一致。
	if data.HomeZoneID > 0 {
		if err := svcCtx.Router.RegisterPlayerZone(ctx, data.PlayerID, data.HomeZoneID, 0); err != nil {
			return fmt.Errorf("register zone mapping: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  Registered mapping: player %d -> zone %d\n", data.PlayerID, data.HomeZoneID)
	}

	fmt.Fprintf(os.Stderr, "\n[OK] Imported %d fields for player %d to local Redis\n", len(fields), data.PlayerID)
	return nil
}

// checkLegacyRedisTargets 检查旧 player 模式会写的全部实例:Router 的数据实例(配了 DevRedis 恒用它,
// 否则是覆盖 homeZone 的 Regions 集群的每个节点)与映射登记目标(checkDataServiceTargets)。
func checkLegacyRedisTargets(c config.Config, homeZone uint32, allowHosts string) error {
	if strings.TrimSpace(c.DevRedis.Host) != "" {
		if err := debugutil.CheckLocalWriteTarget("data_service DevRedis", c.DevRedis.Host, allowHosts); err != nil {
			return err
		}
	} else {
		for _, region := range c.Regions {
			for _, zone := range region.Zones {
				if zone != homeZone {
					continue
				}
				for _, addr := range region.Redis.Addrs {
					if err := debugutil.CheckLocalWriteTarget("data_service Regions 集群", addr, allowHosts); err != nil {
						return err
					}
				}
			}
		}
	}
	return checkDataServiceTargets(c, allowHosts)
}

// checkLegacyPlayerTargetsMarked 是旧 player 模式的写前闸门二(2026-10-22,r2-ds#1 评审补充)。会写的实例:
// 玩家字段写进的数据 Redis(dataRedis,DevRedis 或 Regions 集群);homeZone>0 时还要登记映射,写 MappingRedis,
// RouteStore 为 db_first 时还先写全局库的 player_route。旧导出文件不记来源身份,每个实例都只能凭本地调试环境标记
// 证明自己是本地,否则要 -allow-unverified-target-identity 显式放行。
func checkLegacyPlayerTargetsMarked(ctx context.Context, c config.Config, dataRedis goredis.Cmdable, homeZone uint32, allowUnverified bool, permissions ...authoritysql.Permission) (runErr error) {
	permission := authoritysql.Reader()
	if len(permissions) > 1 {
		return errors.New("重复SQL Permission装配")
	}
	if len(permissions) == 1 {
		permission = permissions[0]
	}
	targets := []importTarget{redisImportTarget(ctx, "data_service 数据 Redis", dataRedis)}
	if homeZone > 0 {
		mapping := goredis.NewClient(&goredis.Options{Addr: c.MappingRedis.Host, Password: c.MappingRedis.Pass})
		defer func() { runErr = errors.Join(runErr, mapping.Close()) }()
		targets = append(targets, redisImportTarget(ctx, "data_service MappingRedis", mapping))
		if c.RouteStore.Normalize().DBFirst() {
			global, err := debugutil.OpenMySQLWithPermission(ctx, debugutil.GlobalMySQL(c), 1, permission)
			if err != nil {
				return fmt.Errorf("核对目标全局库(player_route)失败: %w", err)
			}
			defer func() { runErr = errors.Join(runErr, global.Close()) }()
			targets = append(targets, mysqlImportTarget(ctx, roleGlobalDB, "data_service 全局库(player_route)", global))
		}
	}
	return admitWriteTargets(nil, targets, allowUnverified)
}

// writeLegacyPlayerFields 把导出字段写回 data_service 的 player:{id}:<field> 键族,并把版本重置为 "1"。
//
// 同一个 MULTI 里先删字段索引 player:{id}:__fields(go/data_service/internal/logic/data_logic.go 的 fieldIndexKey):
// 本工具不走 saveFieldsScript、不维护索引,还把版本重置成 "1";索引若留着且戳恰好是 "1",data_service 会把它当成
// 完整索引,导入的字段从全量读与删除里消失,之后的存盘还会把这个不完整的索引固化。删掉之后该玩家在 data_service 里是
// 「有版本、无索引」,全量读 / 删除退回 SCAN,saveFieldsScript 在这种状态下也不会再建索引。
// 所有键共享 {id} hash-tag,同一 slot,Cluster 下同样可以一个 MULTI 完成。
func writeLegacyPlayerFields(ctx context.Context, client goredis.Cmdable, playerID uint64, fieldNames []string, fields map[string][]byte) error {
	_, err := client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.Del(ctx, fmt.Sprintf("player:{%d}:__fields", playerID))
		for _, name := range fieldNames {
			pipe.Set(ctx, fmt.Sprintf("player:{%d}:%s", playerID, name), fields[name], 0)
		}
		pipe.Set(ctx, fmt.Sprintf("player:{%d}:__version", playerID), "1", 0)
		return nil
	})
	if err != nil {
		return fmt.Errorf("write player fields: %w", err)
	}
	return nil
}

// ── Player DB (MySQL) import ────────────────────────────────────

func importPlayerDB(ctx context.Context, dbConfigPath string, data *exportFile, opts legacyImportOptions) (runErr error) {
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

	if len(data.Errors) > 0 {
		return fmt.Errorf("导出时有 %d 张表查询失败(%v),这些表没有导出行;修好权限 / 超时后重新导出", len(data.Errors), data.Errors)
	}
	for _, match := range data.Matches {
		if len(match.Rows) == 0 {
			continue
		}
		if fencedPlayerTables[match.Table] && !opts.allowFencedTables {
			return fmt.Errorf("table %s 带行内围栏,旧 playerdb 导入不对齐存盘屏障,导入后本地 login 会拒绝该玩家;请改用 debug_fetch -mode player-bundle 导出再导入(确需只写行时加 -allow-fenced-tables)", match.Table)
		}
		// 导入先删掉该玩家在这张表的全部行再插回导出的行:导出被 -limit 截断时会静默丢行。
		if match.Truncated == nil {
			return fmt.Errorf("table %s 的导出没有 truncated 标记(旧版 debug_fetch),无法判断是否被 -limit 截断;用新版 debug_fetch 重新导出", match.Table)
		}
		if *match.Truncated {
			return fmt.Errorf("table %s 的导出被 -limit 截断,导入会丢行;调大 debug_fetch -limit 重新导出", match.Table)
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

	if opts.dryRun {
		fmt.Fprintf(os.Stderr, "\n[DRY RUN] No changes written. Use --dry-run=false to apply.\n")
		return nil
	}

	cfg, err := debugutil.LoadDBConfig(dbConfigPath)
	if err != nil {
		return err
	}
	if err := debugutil.CheckLocalWriteTarget("MySQL", cfg.ServerConfig.Database.Hosts, opts.allowHosts); err != nil {
		return err
	}
	if opts.permission.Kind() != "writer" {
		return errors.New("实际导入缺少同boot NORMAL Permission")
	}
	db, _, _, err := debugutil.OpenZoneDBNamedWithPermission(ctx, dbConfigPath, zoneID, "", opts.permission)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, db.Close()) }()

	// 写前闸门二(2026-10-22,r2-ds#1 评审补充):线上 TiDB / MySQL 经 port-forward 到 127.0.0.1 时地址闸放行,
	// 本模式会把该玩家的其它表 DELETE + INSERT 成导出时的旧状态(带 -allow-fenced-tables 时连两张围栏表也写)。
	// 旧导出文件不记来源实例身份,目标库只能凭本地调试环境标记证明自己是本地。
	if err := admitWriteTargets(nil, []importTarget{mysqlImportTarget(ctx, roleZoneDB, "MySQL 落点库", db)}, opts.allowUnverifiedTarget); err != nil {
		return err
	}

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
	return parseExportFile(raw)
}

func parseExportFile(raw []byte) (*exportFile, error) {
	var data exportFile
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	if data.Mode == "" {
		return nil, errors.New("export file missing 'mode' field")
	}
	return &data, nil
}

// overridePlayerID 让 -player 换号导入前后一致:DELETE 用新号,行里的匹配列也改成新号。
// 原来只改了 DELETE 条件,行内仍是旧号,结果删的是 B、插进去的是 A。blob 内嵌的身份不改,换号只适合查看。
func overridePlayerID(data *exportFile, playerID uint64) {
	data.PlayerID = playerID
	encoded := json.RawMessage(strconv.FormatUint(playerID, 10))
	for i := range data.Matches {
		column := data.Matches[i].MatchColumn
		for _, row := range data.Matches[i].Rows {
			if _, ok := row[column]; ok {
				row[column] = encoded
			}
		}
	}
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

	// Try number。按十进制字面量解析,不经 float64:存量雪花 player_id 约 2.8e17,超过 2^53 后
	// float64 会把它舍入成另一名玩家的号(审计 data-service#1)。
	if number, ok := jsonNumber(raw); ok {
		if v, err := strconv.ParseInt(string(number), 10, 64); err == nil {
			return v, nil
		}
		if v, err := strconv.ParseUint(string(number), 10, 64); err == nil {
			return v, nil
		}
		if v, err := number.Float64(); err == nil {
			return v, nil
		}
	}

	// Try bool
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}

	// Fallback: store as string
	return string(raw), nil
}

// jsonNumber 判断 raw 是否是一个 JSON 数字字面量,是则原样返回(不做任何数值转换)。
func jsonNumber(raw json.RawMessage) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	number, ok := value.(json.Number)
	return number, ok
}
