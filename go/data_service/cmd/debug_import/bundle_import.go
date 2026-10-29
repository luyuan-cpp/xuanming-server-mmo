package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"data_service/cmd/debugutil"
	"data_service/internal/config"
	"data_service/internal/routing"
	"data_service/internal/store"
	"data_service/internal/svc"

	basepb "proto/common/base"
	databasepb "proto/common/database"

	"shared/assetop"
	"shared/authoritysql"
	"shared/generated/persistschema"
	"shared/ownerepoch"
	"shared/placement"
	"shared/savebarrier"
	"shared/scenenode"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// ── player-bundle 导入 ─────────────────────────────────────────────────
//
// 目标:把线上一名玩家的数据灌进**本地隔离环境**,让本地 login 走真实的冷加载链路把他加载起来复现问题。
//
// 冷加载(login dataloader parent_commit_barrier.go)对有历史数据的玩家要求三件事同时成立:
//  1. 两张 SQL 行的行内围栏 (fence_owner_epoch, fence_save_seq) 都等于存盘屏障 pending;
//  2. 屏障 hash 的 pending 与两张表的 ACK 相等;
//  3. owner_epoch 不低于 pending 的 epoch。
//
// 所以导入不搬线上的屏障与代次,而是**在本地重铸一个版本** V=(E,S) 同时写进两张行、屏障与 owner_epoch,
// 再删掉本地父快照与子表缓存,让 login 从 SQL 组装父快照。E 取线上与本地观察到的最大代次 + 1:
// 本地残留的旧 DBTask(代次 ≤ 本地旧值)会被行内围栏拒掉,不会覆盖导入的行。
// 「本地观察到的」= 本地 owner_epoch、两张行的围栏、屏障令牌;并且本地行围栏 / 屏障令牌高于本地 owner 时拒绝导入
// (本地 Redis 丢过状态,Kafka 残留的代次无从知道)。两条合起来,残留 DBTask 的代次 ≤ 本地 owner < E 才成立
// (2026-10-22,r2-ds#2;见 admit 与 reforgedEpoch)。
//
// 写入分三段:① Redis 原子复核「不在线、owner 未变」后删父快照与缓存、屏障只写 pending、owner 前进到 E;
// ② 先以稳定维护请求在全局目录拒 OPEN/换修订，再一个事务写两张 SQL 行；③ 补齐两表 ACK；最后登记映射。
// ① 之后任何一步中断,屏障都是 pending≠ACK,login 冷加载与父快照快路径都 fail-closed,旧代次的残留存盘被
// owner 挡住;带 -replace 重跑即可收敛。不会出现半套数据被当成有效存档加载,也不会让本地旧父快照借快路径复活。

type bundleImportOptions struct {
	// 一次装配后显式传到每个SQL池，不能从env再次消费同boot。
	permission   authoritysql.Permission
	dbConfigPath string
	configPath   string
	localZone    uint32
	source       string
	localAccount string
	replace      bool
	dryRun       bool
	allowHosts   string
	// allowUnverifiedTarget:写目标证明不了不是导出来源(身份取不到且无本地标记)时仍放行,逐个打 WARNING(r2-ds#1)。
	allowUnverifiedTarget bool
}

const (
	sourceAuto        = "auto"
	sourceRedisParent = "redis-parent"
	sourceSQL         = "sql"
)

// importPlan 是不依赖本地状态就能算出的部分(纯函数,单测覆盖)。
type importPlan struct {
	playerID uint64
	source   string
	// rows 按物理表名升序:多表同一事务时的取锁顺序。
	rows []planRow
	// baseEpoch / seq:线上观察到的最大代次与最大存盘序号,本地版本在此之上重铸。
	baseEpoch uint64
	seq       uint64
	notes     []string
}

type planRow struct {
	table string
	msg   proto.Message
}

func isBundleFile(raw []byte) bool {
	var probe struct {
		Format string `json:"format"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.Format == debugutil.BundleFormat
}

func parseBundle(raw []byte) (*debugutil.Bundle, error) {
	var b debugutil.Bundle
	// player_id 等 uint64 字段直接按整数解析,不经 float64。
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if b.Format != debugutil.BundleFormat {
		return nil, fmt.Errorf("not a %s file", debugutil.BundleFormat)
	}
	if b.FormatVersion != debugutil.BundleFormatVersion {
		return nil, fmt.Errorf("bundle format_version %d 不受支持(本工具只认 %d),请用同一版本的 debug_fetch 重新导出",
			b.FormatVersion, debugutil.BundleFormatVersion)
	}
	if b.PlayerID == 0 {
		return nil, errors.New("bundle 没有 player_id")
	}
	return &b, nil
}

// buildImportPlan 选数据来源并收集线上版本号。来源:
//   - redis-parent:线上 Redis 父快照(write-behind 下比 SQL 新,包含尚未落库的最后状态);
//   - sql:线上落点库的两张行(最后一次两表都确认的版本);
//   - auto:父快照存在且可解就用它,否则用 SQL。
func buildImportPlan(b *debugutil.Bundle, source string) (*importPlan, error) {
	plan := &importPlan{playerID: b.PlayerID}
	parent, parentErr := bundleParent(b)
	sqlRows, sqlErr := bundleSQLRows(b)

	switch source {
	case sourceAuto, "":
		if parent != nil {
			source = sourceRedisParent
			break
		}
		source = sourceSQL
		if parentErr != nil {
			// 父快照存在却不可用时线上 login 会拒绝它;改按 SQL 导入复现不出这一点,必须说出来。
			plan.notes = append(plan.notes, "父快照不可用("+parentErr.Error()+"),已改按 SQL 行导入;线上 login 会拒绝该父快照,本地复现不出这个拒绝")
		}
	case sourceRedisParent, sourceSQL:
	default:
		return nil, fmt.Errorf("-source 只能是 auto | redis-parent | sql,得到 %q", source)
	}
	plan.source = source

	var rows map[string]proto.Message
	switch source {
	case sourceRedisParent:
		if parent == nil {
			if parentErr == nil {
				parentErr = errors.New("导出包里没有父快照")
			}
			return nil, fmt.Errorf("-source=redis-parent 不可用: %w", parentErr)
		}
		rows = map[string]proto.Message{
			"player_database":   proto.Clone(parent.GetPlayerDatabaseData()),
			"player_database_1": proto.Clone(parent.GetPlayerDatabase_1Data()),
		}
		plan.seq = parent.GetLastSaveSeq()
	case sourceSQL:
		if sqlErr != nil {
			if parentErr != nil {
				return nil, fmt.Errorf("父快照与 SQL 行都不可用: 父快照 %v; SQL %w", parentErr, sqlErr)
			}
			return nil, fmt.Errorf("-source=sql 不可用: %w", sqlErr)
		}
		rows = sqlRows
	}

	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		msg := rows[name]
		if id := playerIDOf(msg); id != b.PlayerID {
			return nil, fmt.Errorf("表 %s 的 player_id=%d 与导出包 player_id=%d 不符", name, id, b.PlayerID)
		}
		// 混版本保护(docs/design/mixed-version-persist-field-protection.md §5.4):行的持久 schema 世代高于本工具时,
		// 本工具不认识新世代的列,写库时会把它们丢掉,却原样拷走声称完整的世代戳 —— 导入结果对后来者「说谎」。
		if gen := persistSchemaGenOf(msg); gen > uint64(persistschema.Gen) {
			return nil, fmt.Errorf("表 %s 的持久 schema 世代 %d 高于本工具的 %d;用不低于该世代的 debug_import 重新导入",
				name, gen, persistschema.Gen)
		}
		plan.rows = append(plan.rows, planRow{table: name, msg: msg})
	}

	// 线上见过的所有代次 / 序号:owner_epoch、屏障三令牌、两张 SQL 行的围栏。
	observe := func(epoch, seq uint64) {
		plan.baseEpoch = max(plan.baseEpoch, epoch)
		plan.seq = max(plan.seq, seq)
	}
	if owner := debugutil.EntryText(debugutil.FindRedisEntry(b.Redis, ownerepoch.OwnerEpochKey(b.PlayerID))); owner != "" {
		epoch, err := ownerepoch.ParseEpoch(owner)
		if err != nil {
			return nil, fmt.Errorf("导出包 owner_epoch 畸形: %w", err)
		}
		observe(epoch, 0)
	}
	barrier := debugutil.FindRedisEntry(b.Redis, savebarrier.Key(b.PlayerID))
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		if token := debugutil.HashText(barrier, field); token != "" {
			epoch, seq, err := savebarrier.Parse(token)
			if err != nil {
				return nil, fmt.Errorf("导出包存盘屏障 %s 畸形: %w", field, err)
			}
			observe(epoch, seq)
		}
	}
	for _, msg := range sqlRows {
		if epoch, seq, ok := debugutil.RowFence(msg); ok {
			observe(epoch, seq)
		}
	}
	if plan.seq == 0 {
		plan.seq = 1
	}

	if source == sourceRedisParent && sqlErr == nil {
		plan.notes = append(plan.notes, "按父快照导入:包含线上尚未落库的最后状态;只想复现 SQL 里的版本用 -source=sql")
	}
	if location := debugutil.FindRedisEntry(b.Redis, scenenode.LocationKey(b.PlayerID)); location != nil && location.Type != "none" {
		plan.notes = append(plan.notes, "导出时玩家在场景里(位置键存在),导入的不一定是最终态")
	}
	return plan, nil
}

func bundleParent(b *debugutil.Bundle) (*databasepb.PlayerAllData, error) {
	entry := debugutil.FindRedisEntry(b.Redis, debugutil.ParentKey(b.PlayerID))
	if entry == nil || entry.Type == "none" || entry.Value == nil {
		return nil, nil
	}
	raw, ok, err := entry.Value.RawBytes()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("父快照没有原字节")
	}
	parent := &databasepb.PlayerAllData{}
	if err := proto.Unmarshal(raw, parent); err != nil {
		return nil, fmt.Errorf("父快照解析失败: %w", err)
	}
	// 与 login beginParentLoad 同一身份校验:缺子表或身份不符的父快照线上会被拒,不能补骨架后照常导入。
	if parent.GetPlayerDatabaseData() == nil || parent.GetPlayerDatabase_1Data() == nil {
		return nil, errors.New("父快照缺少 player_database_data 或 player_database_1_data(线上 login 会以「父快照玩家身份不符」拒绝)")
	}
	if parent.GetPlayerDatabaseData().GetPlayerId() != b.PlayerID || parent.GetPlayerDatabase_1Data().GetPlayerId() != b.PlayerID {
		return nil, errors.New("父快照里的 player_id 与导出包不符(线上 login 会拒绝)")
	}
	return parent, nil
}

// bundleSQLRows 取导出包里登录加载链路读的两张表;两张都要有,缺一张就不能单独复现 SQL 版本。
func bundleSQLRows(b *debugutil.Bundle) (map[string]proto.Message, error) {
	if b.MySQL.Error != "" {
		return nil, fmt.Errorf("导出时落点库读取失败: %s", b.MySQL.Error)
	}
	rows := map[string]proto.Message{}
	for _, table := range debugutil.PlayerTables {
		if !table.LoadPath {
			continue
		}
		name, err := debugutil.PhysicalTableName(table.New())
		if err != nil {
			return nil, err
		}
		var exported *debugutil.TableRow
		for i := range b.MySQL.Tables {
			if b.MySQL.Tables[i].Table == name {
				exported = &b.MySQL.Tables[i]
			}
		}
		if exported == nil || exported.Error != "" || !exported.Found || exported.Row == nil {
			return nil, fmt.Errorf("导出包缺少表 %s 的行", name)
		}
		if len(exported.LiveExtraColumns) > 0 {
			return nil, fmt.Errorf("表 %s 导出时线上有本地 proto 没有的列 %v,这些列没导出;切到线上镜像的 commit 重导",
				name, exported.LiveExtraColumns)
		}
		raw, ok, err := exported.Row.RawBytes()
		if err != nil {
			return nil, fmt.Errorf("表 %s: %w", name, err)
		}
		if !ok {
			return nil, fmt.Errorf("表 %s 没有原字节", name)
		}
		msg := table.New()
		if err := proto.Unmarshal(raw, msg); err != nil {
			return nil, fmt.Errorf("表 %s 行解析失败: %w", name, err)
		}
		rows[name] = msg
	}
	return rows, nil
}

func playerIDOf(msg proto.Message) uint64 {
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("player_id")
	if fd == nil {
		return 0
	}
	return msg.ProtoReflect().Get(fd).Uint()
}

// persistSchemaGenOf 读玩家表的覆盖世代;没有这个字段的表返回 0(按未盖戳处理,不拦)。
func persistSchemaGenOf(msg proto.Message) uint64 {
	fd := msg.ProtoReflect().Descriptor().Fields().ByName("persist_schema_gen")
	if fd == nil {
		return 0
	}
	return msg.ProtoReflect().Get(fd).Uint()
}

// ── 执行 ───────────────────────────────────────────────────────────────

func runBundleImport(ctx context.Context, raw []byte, opts bundleImportOptions) (runErr error) {
	b, err := parseBundle(raw)
	if err != nil {
		return err
	}
	plan, err := buildImportPlan(b, opts.source)
	if err != nil {
		return err
	}
	cfg, err := debugutil.LoadDBConfig(opts.dbConfigPath)
	if err != nil {
		return err
	}
	localZone := opts.localZone
	if localZone == 0 {
		localZone = cfg.ZoneId
	}
	if localZone == 0 {
		return errors.New("本地 zone 未知:db config 没有 ZoneId,用 -zone 指定")
	}
	// 写进 go/db 会读的那个库:本地没有落点记录时 go/db 按 home_zone 派生 zone_{zone}_db(placement.StoreDBName)。
	// db.yaml 写了别的 DBName 时导入与 go/db 必然各走各的库,直接拒绝。
	if !placement.IsZoneStorage(localZone) {
		return fmt.Errorf("本地 zone %d 不是合法的 zone 库编号", localZone)
	}
	targetDB, _ := placement.StoreDBName(localZone)
	if name := strings.TrimSpace(cfg.ServerConfig.Database.DBName); name != "" && name != targetDB {
		return fmt.Errorf("db.yaml 的 DBName=%s 与本地 zone %d 的库 %s 不同:go/db 按 zone 派生库名,导入写进 DBName 会读不到;去掉 DBName 或改 -zone", name, localZone, targetDB)
	}
	dsCfg, err := debugutil.LoadDataServiceConfig(opts.configPath)
	if err != nil {
		return err
	}
	core, mapping := cfg.CoreRedis(), cfg.MappingRedis()

	printBundlePlan(b, plan, localZone, opts)
	fmt.Fprintf(os.Stderr, "  写目标:      MySQL %s/%s;核心 Redis %s;映射登记 %s(data_service MappingRedis)\n",
		cfg.ServerConfig.Database.Hosts, targetDB, core.Describe(), dsCfg.MappingRedis.Host)
	if opts.dryRun {
		fmt.Fprintf(os.Stderr, "\n[DRY RUN] 未写入。确认上面的写目标都是本地隔离环境后加 --dry-run=false 执行。\n")
		return nil
	}

	if opts.permission.Kind() != "writer" {
		return errors.New("实际导入必须显式注入同boot Writer Permission")
	}

	// 写前闸门一:写目标必须是本机(或显式放行的本地服务名)。先按配置校验,再建任何连接。
	if err := debugutil.CheckLocalWriteTarget("MySQL", cfg.ServerConfig.Database.Hosts, opts.allowHosts); err != nil {
		return err
	}
	if err := debugutil.CheckLocalWriteTarget("核心 Redis", core.Hosts, opts.allowHosts); err != nil {
		return err
	}
	if err := checkDataServiceTargets(dsCfg, opts.allowHosts); err != nil {
		return err
	}

	db, _, _, err := debugutil.OpenZoneDBNamedWithPermission(ctx, opts.dbConfigPath, localZone, targetDB, opts.permission)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, db.Close()) }()
	rdb, err := debugutil.OpenRedis(ctx, core)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, rdb.Close()) }()
	mappingClient := rdb
	if mapping != core {
		if mappingClient, err = debugutil.OpenRedis(ctx, mapping); err != nil {
			return fmt.Errorf("go/db 映射 Redis: %w", err)
		}
		defer func() { runErr = errors.Join(runErr, mappingClient.Close()) }()
	}

	// 写前闸门二:目标实例不能就是导出来源。port-forward 之后线上实例也在 127.0.0.1,只看地址拦不住。
	if err := checkNotExportSource(ctx, b.Sources, db, rdb, mappingClient, dsCfg, opts.allowUnverifiedTarget, opts.permission); err != nil {
		return err
	}

	svcCtx, cleanup, err := debugutil.NewServiceContextWithPermissionAndReport(opts.configPath, opts.permission)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, cleanup()) }()

	local, err := inspectLocalPlayer(ctx, db, rdb, mappingClient, svcCtx.Router, plan)
	if err != nil {
		return err
	}
	if err := local.admit(opts.replace, localZone); err != nil {
		return err
	}
	directoryMaxOpen, err := dsCfg.AccountDirectory.PoolMaxOpenConn()
	if err != nil {
		return err
	}
	directoryConfig := svc.MySQLConfigOf(dsCfg)
	directoryConfig.Permission = opts.permission
	directory, err := store.NewAccountDirectoryStore(directoryConfig, true, dsCfg.AccountDirectory.MaxRoles(), directoryMaxOpen)
	if err != nil {
		return fmt.Errorf("目标全局账号目录未就绪: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, directory.Close()) }()
	if err := directory.ConfigureAssetContracts(dsCfg.AccountDirectory.RoleTradeTradeSchema, dsCfg.AccountDirectory.RoleTradeGuildSchema, dsCfg.AccountDirectory.RoleTradeMailSchema, dsCfg.AccountDirectory.RoleTradeMatchSchema, dsCfg.AccountDirectory.RoleTradeInboxSchema); err != nil {
		return err
	}
	account := opts.localAccount
	if account == "" {
		owned, err := directory.Lookup(ctx, plan.playerID)
		if err != nil || owned == nil || owned.Account == "" {
			if err == nil {
				err = errors.New("目标目录归属为空")
			}
			return fmt.Errorf("未指定local-account且目标SQL目录无有效归属，拒绝覆盖；新角色须显式指定目标账号: %w", err)
		}
		account = owned.Account
	}
	importedRole, err := importedAccountRole(b, plan)
	if err != nil {
		return err
	}
	requestID, err := localImportRequestID(b, plan.source, localZone, opts.localAccount)
	if err != nil {
		return err
	}

	epoch, err := local.reforgedEpoch(plan.baseEpoch)
	if err != nil {
		return err
	}
	version := savebarrier.Version(epoch, plan.seq)
	fmt.Fprintf(os.Stderr, "\n本地重铸版本: owner_epoch=%d save_seq=%d (%s)\n", epoch, plan.seq, version)

	// 三段写入,任一段之后中断都让 login fail-closed(屏障 pending 已是新版本、ACK 未齐 → 冷加载与父快照快路径都拒),
	// 且旧代次的残留存盘被新 owner_epoch 挡住;带 -replace 重跑即可收敛。
	if err := stageLocalRedis(ctx, rdb, plan.playerID, localZone, local.ownerRaw, epoch, version); err != nil {
		return fmt.Errorf("第 1 步(Redis 预置屏障)未确认，未开始SQL写入；保留现场按原bundle重试: %w", err)
	}
	// 先撤销旧 owner_revision：旧路由此后不能 Register；已 Register 则 OPEN 闸在写玩家SQL前拒绝。
	// 同一bundle+目标条款只回原维护修订；失败恢复不会无限 bump，之后另过户/导入则拒绝旧包。
	if err := applyAuthorizedLocalImport(func() error {
		_, err := directory.ImportRole(ctx, account, importedRole, opts.replace, requestID)
		return err
	}, func() error { return writeLocalRows(ctx, db, plan, epoch) }, func() error { return commitLocalRedis(ctx, rdb, plan.playerID, version) }); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  [OK] %s: %d 张表已写入\n", targetDB, len(plan.rows))
	fmt.Fprintf(os.Stderr, "  [OK] 核心 Redis: 屏障 / owner_epoch 已对齐,本地父快照与子表缓存已清\n")

	if !local.mapped {
		if err := svcCtx.Router.RegisterPlayerZone(ctx, plan.playerID, localZone, 0); err != nil {
			return fmt.Errorf("登记 player:zone 失败(数据已写入,修好映射后重跑或手工登记): %w", err)
		}
		fmt.Fprintf(os.Stderr, "  [OK] 映射: player %d -> zone %d\n", plan.playerID, localZone)
	}

	fmt.Fprintf(os.Stderr, "\n[OK] 玩家 %d 已导入本地 zone %d(来源 %s)。\n", plan.playerID, localZone, plan.source)
	fmt.Fprintf(os.Stderr, "全局 SQL 已登记目标账号 %s 的维护修订，可 Login 后 EnterGame player_id=%d。\n", account, plan.playerID)
	return nil
}

// stage 已持久安装 pending 后进入此接缝。授权失败零玩家 SQL；SQL失败零ACK。
// 原请求重放可能返回已提交的目录维护修订，调用者只能按原bundle继续恢复。
func applyAuthorizedLocalImport(authorize, writeRows, commit func() error) error {
	if err := authorize(); err != nil {
		return fmt.Errorf("全局SQL目录维护授权失败；未写玩家SQL，pending保留且不ACK: %w", err)
	}
	if err := writeRows(); err != nil {
		return fmt.Errorf("玩家SQL写入失败；目录维护修订已提交，pending保留且不ACK，以原bundle加-replace恢复: %w", err)
	}
	if err := commit(); err != nil {
		return fmt.Errorf("补齐屏障ACK失败；目录与玩家SQL已提交，保留原bundle恢复: %w", err)
	}
	return nil
}

// 空账号选择器也是原条款：重试不能按后来买家的账号生成另一request_id绕过旧收据。
// replace 是恢复控制开关，不进业务身份；SQL中保留首次许可，重试只返回原目录修订。
func localImportRequestID(b *debugutil.Bundle, source string, zone uint32, accountSelector string) ([]byte, error) {
	raw, err := json.Marshal(struct {
		Bundle          *debugutil.Bundle `json:"bundle"`
		Source          string            `json:"source"`
		Zone            uint32            `json:"zone"`
		AccountSelector string            `json:"account_selector"`
	}{b, source, zone, accountSelector})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func printBundlePlan(b *debugutil.Bundle, plan *importPlan, localZone uint32, opts bundleImportOptions) {
	fmt.Fprintf(os.Stderr, "=== 导入预览 (player-bundle) ===\n")
	fmt.Fprintf(os.Stderr, "  玩家:        %d\n", b.PlayerID)
	fmt.Fprintf(os.Stderr, "  导出:        %s by %s,原因 %q\n", b.ExportedAtUTC, b.Operator, b.Reason)
	fmt.Fprintf(os.Stderr, "  线上落点:    home_zone=%d storage=%s(%s)\n", b.Routing.HomeZone, b.Routing.StorageDB, b.Routing.ResolvedBy)
	if b.ServerRevision != "" {
		fmt.Fprintf(os.Stderr, "  线上版本:    %s(本地检出需与之一致,否则 proto 字段可能错位)\n", b.ServerRevision)
	}
	fmt.Fprintf(os.Stderr, "  线上判读:    %s\n", b.Analysis.Barrier.Conclusion)
	for _, warning := range b.Analysis.Warnings {
		fmt.Fprintf(os.Stderr, "  导出警告:    %s\n", warning)
	}
	fmt.Fprintf(os.Stderr, "  数据来源:    %s\n", plan.source)
	fmt.Fprintf(os.Stderr, "  本地目标:    zone %d\n", localZone)
	for _, row := range plan.rows {
		fmt.Fprintf(os.Stderr, "    %-20s %6d bytes\n", row.table, proto.Size(row.msg))
	}
	fmt.Fprintf(os.Stderr, "  线上最大代次 %d,最大存盘序号 %d;本地版本 = max(线上, 本地) + 1\n", plan.baseEpoch, plan.seq)
	if opts.localAccount != "" {
		fmt.Fprintf(os.Stderr, "  本地账号:    %s(写目标全局 SQL 目录)\n", opts.localAccount)
	}
	for _, note := range plan.notes {
		fmt.Fprintf(os.Stderr, "  注意: %s\n", note)
	}
}

// checkNotExportSource 是写前闸门二:每个写目标都要证明自己不是导出来源,证明不了就拒绝(fail-closed)。
// 这里只补开 data_service 的两个探测连接(MappingRedis、全局库);取身份与本地标记、判定都在
// target_identity.go 的 checkBundleWriteTargets(2026-10-22,r2-ds#1 及评审补充)。
func checkNotExportSource(ctx context.Context, src debugutil.BundleSources, db *sql.DB, core, mapping *redis.Client, dsCfg config.Config, allowUnverified bool, permission authoritysql.Permission) (runErr error) {
	targets := bundleWriteTargets{zoneDB: db, coreRedis: core, mappingRedis: mapping}
	if host := strings.TrimSpace(dsCfg.MappingRedis.Host); host != "" {
		client := redis.NewClient(&redis.Options{Addr: host, Password: dsCfg.MappingRedis.Pass})
		defer func() { runErr = errors.Join(runErr, client.Close()) }()
		targets.dataServiceMapping = client
	}
	global, err := debugutil.OpenMySQLWithPermission(ctx, debugutil.GlobalMySQL(dsCfg), 1, permission)
	if err != nil {
		return fmt.Errorf("核对目标全局SQL实例失败: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, global.Close()) }()
	targets.globalDB = global
	return checkBundleWriteTargets(ctx, src, targets, allowUnverified)
}

// localPlayerState 是导入前本地已有的同号状态。
type localPlayerState struct {
	sqlTables  []string
	redisKeys  []string
	onlineKeys []string
	ownerRaw   string
	ownerEpoch uint64
	sqlEpoch   uint64
	// barrierEpoch:本地存盘屏障各版本令牌(pending、两表 ACK、补投意图)里最大的归属代次(r2-ds#2)。
	barrierEpoch uint64
	mapped       bool
	mappedZone   uint32
	// placementRaw / placementPresent:go/db 选库用的映射实例上的 player:placement:<id>。
	placementRaw     string
	placementPresent bool
}

func inspectLocalPlayer(ctx context.Context, db *sql.DB, rdb, mappingClient *redis.Client, router *routing.Router, plan *importPlan) (*localPlayerState, error) {
	state := &localPlayerState{}
	for _, row := range plan.rows {
		newMsg := func() proto.Message { return row.msg.ProtoReflect().Type().New().Interface() }
		stored, found, err := debugutil.ReadPlayerRow(ctx, db, newMsg, plan.playerID)
		if err != nil {
			return nil, fmt.Errorf("读本地 %s 失败(本地表结构与当前检出不一致时先跑本地迁移): %w", row.table, err)
		}
		if found {
			state.sqlTables = append(state.sqlTables, row.table)
			if version := debugutil.RowFenceVersion(stored); version != "" {
				epoch, _, err := savebarrier.Parse(version)
				if err != nil {
					return nil, err
				}
				state.sqlEpoch = max(state.sqlEpoch, epoch)
			}
		}
	}

	id := plan.playerID
	if err := state.readLocalRedisState(ctx, rdb, id); err != nil {
		return nil, err
	}

	placementEntries, err := debugutil.ReadRedisKeys(ctx, mappingClient, []debugutil.RedisKeySpec{
		{Key: placement.Key(id), Kind: debugutil.RedisKindString},
	})
	if err != nil {
		return nil, err
	}
	if placementEntries[0].Type != "none" {
		state.placementPresent = true
		state.placementRaw = debugutil.EntryText(&placementEntries[0])
	}

	zone, err := router.GetPlayerHomeZone(ctx, id)
	switch {
	case err == nil:
		state.mapped, state.mappedZone = true, zone
	case errors.Is(err, routing.ErrHomeZoneNotMapped):
	default:
		return nil, fmt.Errorf("读本地 player:zone 失败: %w", err)
	}
	return state, nil
}

// readLocalRedisState 读本地核心 Redis 里该玩家的父快照、owner_epoch、存盘屏障与在线 / 会话键,填进 s。
// 屏障的版本令牌一并读成 barrierEpoch:admit 拿它判本地 Redis 丢没丢过状态,reforgedEpoch 拿它抬重铸代次(r2-ds#2)。
// 单独成函数是为了让这条读路径能直接用 miniredis 测(删掉 barrierEpoch 的读取,回归用例立刻变红)。
func (s *localPlayerState) readLocalRedisState(ctx context.Context, rdb redis.Cmdable, id uint64) error {
	entries, err := debugutil.ReadRedisKeys(ctx, rdb, []debugutil.RedisKeySpec{
		{Key: debugutil.ParentKey(id), Kind: debugutil.RedisKindString},
		{Key: ownerepoch.OwnerEpochKey(id), Kind: debugutil.RedisKindString},
		{Key: savebarrier.Key(id), Kind: debugutil.RedisKindHash},
		{Key: scenenode.LocationKey(id), Kind: debugutil.RedisKindString},
		{Key: debugutil.LocatorSessionKey(id), Kind: debugutil.RedisKindString},
	})
	if err != nil {
		return err
	}
	for i, entry := range entries {
		if entry.Type == "none" {
			continue
		}
		if i >= 3 {
			s.onlineKeys = append(s.onlineKeys, entry.Key)
			continue
		}
		s.redisKeys = append(s.redisKeys, entry.Key)
	}
	if owner := entries[1]; owner.Type != "none" {
		if owner.Type != "string" || owner.Value == nil {
			return fmt.Errorf("本地 %s 类型异常(%s)", owner.Key, owner.Type)
		}
		s.ownerRaw = owner.Value.Text
		if s.ownerEpoch, err = ownerepoch.ParseEpoch(s.ownerRaw); err != nil {
			return fmt.Errorf("本地 owner_epoch 畸形: %w", err)
		}
	}
	if s.barrierEpoch, err = localBarrierEpoch(&entries[2]); err != nil {
		return err
	}
	return nil
}

// admit 决定能否写入:
//   - 本地同号玩家在线一律拒绝(scene 里的内存态会把导入的数据存回去覆盖);
//   - 本地映射指向别的 zone,或本地落点记录把这个号钉到别的库 / 处于冻结态时拒绝:go/db 会去别处读写,导入的行读不到;
//   - 本地已有同号存档时要 -replace;
//   - 本地存盘痕迹(两张行的围栏、屏障令牌)的代次高于本地 owner_epoch 时拒绝,理由见函数末尾(r2-ds#2)。
func (s *localPlayerState) admit(replace bool, localZone uint32) error {
	if len(s.onlineKeys) > 0 {
		return fmt.Errorf("本地同号玩家在线或会话未释放(%s),先让他下线、等会话过期后再导入", strings.Join(s.onlineKeys, ", "))
	}
	if s.mapped && s.mappedZone != localZone {
		return fmt.Errorf("本地 player:zone 已指向 zone %d(不是本地 zone %d);用 -zone=%d 导入,或先在本地清掉这条映射",
			s.mappedZone, localZone, s.mappedZone)
	}
	if s.placementPresent {
		rec, err := placement.Parse(s.placementRaw)
		switch {
		case err != nil:
			return fmt.Errorf("本地落点记录 %q 畸形,go/db 会对该玩家 fail-closed;先在本地删掉 player:placement 记录: %w", s.placementRaw, err)
		case rec.Frozen:
			return fmt.Errorf("本地落点记录 %q 处于冻结态(搬库中),go/db 的写会一直延后;先在本地结束或清掉这条记录", s.placementRaw)
		case rec.StorageID != localZone:
			dbName, _ := placement.StoreDBName(rec.StorageID)
			return fmt.Errorf("本地落点记录 %q 把该玩家钉在 %s,go/db 会去那里读写,导入 zone %d 的行读不到;先在本地删掉 player:placement 记录", s.placementRaw, dbName, localZone)
		}
	}
	existing := append(append([]string{}, s.sqlTables...), s.redisKeys...)
	if len(existing) > 0 && !replace {
		return fmt.Errorf("本地已有该 player_id 的存档(%s),会被覆盖;确认可以覆盖后加 -replace", strings.Join(existing, ", "))
	}
	// owner_epoch 只经 scene_manager INCR 前进,本地每一次存盘(进 Kafka 的 DBTask、写进行内围栏与屏障的版本)
	// 的代次都不超过当时的 owner_epoch。本地存盘痕迹高过本地 owner,说明本地 Redis 丢过状态(FLUSHALL、单删键)
	// 而 MySQL / Kafka 没清:Kafka 里可能还压着代次更高、在本地 Redis 与 MySQL 都不留痕迹的残留 DBTask,
	// go/db 只按行内围栏裁决,它们会冲掉导入的行,之后本地新登录的存盘再被判 stale_owner。
	// 重铸代次找不到可靠的上界去盖过它们,只能拒绝,让操作者先把本地环境清干净。
	if trace := max(s.sqlEpoch, s.barrierEpoch); trace > s.ownerEpoch {
		return fmt.Errorf("本地存盘痕迹的代次 %d(行内围栏 %d、存盘屏障 %d)高于本地 owner_epoch %d:本地 Redis 丢过状态而 MySQL / Kafka 没清,"+
			"Kafka 里可能有代次更高的残留存盘,导入后会被冲掉。先清本地环境(redis FLUSHALL 的同时清该玩家两张行,"+
			"并执行 dev_tools.ps1 -Command kafka-offset-reset 跳过残留 DBTask)再导入",
			trace, s.sqlEpoch, s.barrierEpoch, s.ownerEpoch)
	}
	return nil
}

// reforgedEpoch 返回本地重铸版本的归属代次:线上与本地见过的全部代次(线上 owner / 屏障 / 行围栏,本地 owner、
// 两张行的围栏、屏障令牌)之上 + 1。admit 已保证本地痕迹不超过本地 owner,所以本地残留的 DBTask 代次都低于结果,
// 会被行内围栏判 stale_owner,覆盖不了导入的行(r2-ds#2)。
func (s *localPlayerState) reforgedEpoch(onlineEpoch uint64) (uint64, error) {
	prior := max(onlineEpoch, s.ownerEpoch, s.sqlEpoch, s.barrierEpoch)
	if prior == math.MaxUint64 {
		return 0, errors.New("本地存盘代次耗尽，拒绝导入")
	}
	return prior + 1, nil
}

// localBarrierVersionFields 是存盘屏障 hash 里装版本令牌的字段:pending、两表 ACK、go/db 补投意图的版本
// (go/db saveoutbox contract.go 的 outbox_version;补投计次 / 终态字段只抄它的值,不会更大)。
var localBarrierVersionFields = []string{"pending", "player_database", "player_database_1", "outbox_version"}

// localBarrierEpoch 取本地屏障全部版本令牌里最大的归属代次;键不存在时返回 0。类型或令牌畸形时报错而不是当成 0:
// 读不懂的屏障可能装着高于 owner 的代次,当成没有会把重铸代次算低。
func localBarrierEpoch(entry *debugutil.RedisEntry) (uint64, error) {
	if entry.Type == "none" {
		return 0, nil
	}
	if entry.Type != "hash" || entry.Hash == nil {
		return 0, fmt.Errorf("本地 %s 类型异常(%s)", entry.Key, entry.Type)
	}
	var epoch uint64
	for _, field := range localBarrierVersionFields {
		token := debugutil.HashText(entry, field)
		if token == "" {
			continue
		}
		fieldEpoch, _, err := savebarrier.Parse(token)
		if err != nil {
			return 0, fmt.Errorf("本地存盘屏障 %s 畸形: %w", field, err)
		}
		epoch = max(epoch, fieldEpoch)
	}
	return epoch, nil
}

// writeLocalRows 在一个 RC 事务里按表名升序替换两张行,行内围栏写成本地版本。
func writeLocalRows(ctx context.Context, db *sql.DB, plan *importPlan, epoch uint64) error {
	tx, err := db.BeginTx(ctx, importTableTxOptions())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range plan.rows {
		msg := proto.Clone(row.msg)
		if err := debugutil.SetRowFence(msg, epoch, plan.seq); err != nil {
			return err
		}
		if err := debugutil.ReplaceFencedPlayerRow(ctx, tx, msg); err != nil {
			return fmt.Errorf("表 %s: %w", row.table, err)
		}
	}
	return tx.Commit()
}

// stageLocalRedisScript 是三段写入的第 1 段,先原子复核预检结论:同号玩家不在线、不在登录链中、owner_epoch 与预检一致。
// 然后删掉本地父快照 / 子表缓存 / 资产证据 / 交接标记 / 旧屏障,屏障只写 pending(不写 ACK)、owner 前进到新代次,
// 并把玩家移出待落库索引。此后 login 见 pending≠ACK 必拒(含父快照快路径),旧代次的存盘被 owner 挡住。
//
// 父快照、owner 与屏障必须在同一实例(login parent_commit_barrier 的前提);多键脚本不支持 Cluster。
var stageLocalRedisScript = redis.NewScript(stageLocalRedisLua)

const stageLocalRedisLua = `
if redis.call('EXISTS', KEYS[8]) == 1 or redis.call('EXISTS', KEYS[9]) == 1 or redis.call('EXISTS', KEYS[11]) == 1 then
    return redis.error_reply('IMPORT_ONLINE local player became online or entered a login chain since preflight')
end
local owner = redis.call('GET', KEYS[7])
if (owner or '') ~= ARGV[1] then
    return redis.error_reply('IMPORT_OWNER_CHANGED owner_epoch changed since preflight')
end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6])
redis.call('HSET', KEYS[6], 'pending', ARGV[3])
redis.call('SET', KEYS[7], ARGV[2])
redis.call('ZREM', KEYS[10], ARGV[4])
return 1
`

func stageLocalRedis(ctx context.Context, rdb redis.Scripter, playerID uint64, localZone uint32, expectedOwner string, epoch uint64, version string) error {
	keys := []string{
		debugutil.ParentKey(playerID),
		debugutil.SubTableCacheKey(&databasepb.PlayerDatabase{}, playerID),
		debugutil.SubTableCacheKey(&databasepb.PlayerDatabase_1{}, playerID),
		assetop.SQLCommitKey(playerID),
		ownerepoch.HandoffKey(playerID),
		savebarrier.Key(playerID),
		ownerepoch.OwnerEpochKey(playerID),
		scenenode.LocationKey(playerID),
		debugutil.LocatorSessionKey(playerID),
		debugutil.SavePendingIndexKey(localZone, playerID),
		fmt.Sprintf("player_locker:%d", playerID),
	}
	return stageLocalRedisScript.Run(ctx, rdb, keys, expectedOwner, fmt.Sprint(epoch), version, fmt.Sprint(playerID)).Err()
}

// commitLocalRedisScript 是第 3 段:SQL 两行已写成版本 V 之后才补齐两表 ACK;pending 已不是 V(期间被改过)时拒绝。
var commitLocalRedisScript = redis.NewScript(commitLocalRedisLua)

const commitLocalRedisLua = `
if redis.call('HGET', KEYS[1], 'pending') ~= ARGV[1] then
    return redis.error_reply('IMPORT_BARRIER_CHANGED pending changed since stage')
end
redis.call('HSET', KEYS[1], 'player_database', ARGV[1], 'player_database_1', ARGV[1])
return 1
`

// commitLocalRedis 只碰屏障一把键:不写 player_to_account 反向缓存,角色归属已由第 2 段之前的全局 SQL ImportRole 登记
// (2026-10-22 评审:原先多传的 KEYS[2] / ARGV[2] 脚本并不使用,只会让读者以为这里写了反向索引,已删)。
func commitLocalRedis(ctx context.Context, rdb redis.Scripter, playerID uint64, version string) error {
	return commitLocalRedisScript.Run(ctx, rdb, []string{savebarrier.Key(playerID)}, version).Err()
}

// checkDataServiceTargets 检查 Router.RegisterPlayerZone 会写的目标:映射 Redis,db_first 时还有全局库的 player_route。
func checkDataServiceTargets(c config.Config, allowHosts string) error {
	if err := debugutil.CheckLocalWriteTarget("data_service MappingRedis", c.MappingRedis.Host, allowHosts); err != nil {
		return err
	}
	{
		if err := debugutil.CheckLocalWriteTarget("data_service 全局库(账号目录及路由)", c.SnapshotMySQL.Host, allowHosts); err != nil {
			return err
		}
	}
	return nil
}

// 新导出包保留角色目录 PB 未知字段；旧包只能从玩家身份构造目录，不读取 Redis 反向缓存。
func importedAccountRole(b *debugutil.Bundle, plan *importPlan) (*basepb.AccountSimplePlayer, error) {
	if b.Global != nil && b.Global.AccountRole != nil {
		raw, ok, err := b.Global.AccountRole.RawBytes()
		if err != nil || !ok {
			return nil, fmt.Errorf("SQL角色目录原件缺失或非法")
		}
		row := &databasepb.AccountRole{}
		if err = proto.Unmarshal(raw, row); err != nil || row.PlayerId != plan.playerID {
			return nil, fmt.Errorf("SQL角色目录身份不符")
		}
		role := &basepb.AccountSimplePlayer{}
		if err = proto.Unmarshal(row.RoleProto, role); err != nil || role.PlayerId != plan.playerID {
			return nil, fmt.Errorf("角色目录PB原件不符")
		}
		return role, nil
	}
	for _, row := range plan.rows {
		if player, ok := row.msg.(*databasepb.PlayerDatabase); ok {
			return &basepb.AccountSimplePlayer{PlayerId: plan.playerID, ClassId: player.GetUint32PbComponent().GetClass(),
				Gender: player.GetProfileComponent().GetGender(), Name: player.GetProfileComponent().GetName(), AppearanceId: player.GetProfileComponent().GetAppearanceId()}, nil
		}
	}
	return nil, fmt.Errorf("导入包缺少玩家身份，不能建立SQL目录")
}
