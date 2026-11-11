// writer_authority是独立部署入口；常驻服务不得获得本程序的控制凭据。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	mysqldriver "github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"path/filepath"
	dbpb "proto/common/database"
	"runtime"
	"shared/authoritysql"
	"shared/dbplacement"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const requestLimit = 16 << 10
const rosterLimit = 16 << 20
const runtimeConfigLimit = 16 << 20
const rosterEntryLimit = 1024

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "writer_authority:", err)
		os.Exit(1)
	}
}

// Java商城没有Go业务迁移入口。部署者在分发NORMAL资格前，用独立Schema身份
// 复用共享闸门迁移；常驻进程不获得这份能力，已有generation和写者记录不改。
func prepareSchema(ctx context.Context, schema string, getenv func(string) string) error {
	return prepareSchemaWithDDL(ctx, schema, getenv, nil)
}

// businessDDLLimit 是 -ddl-file 的大小上限:网关的 schema.sql 只有几 KB,给足余量,又不让误传的大文件读进内存。
const businessDDLLimit = 1 << 20

// loadBusinessDDL 读 prepare-schema 建完闸门表之后要执行的业务表 DDL(-ddl-file)。只给没有 Go 迁移入口的 Java 服务用:
// 网关的 mmorpg 库在 TiDB 上没有别的建表入口(docs/design/no-single-node-horizontal-scaling-20261001.md §6.3)。
//
// 去掉整行注释(--)后按分号拆句。只认两种幂等语句:CREATE TABLE IF NOT EXISTS 与 INSERT IGNORE INTO,表名不许带库名;
// 其余(USE、DROP、ALTER、DELETE、UPDATE、普通 INSERT……)整份拒绝,一句都不执行 —— Job 重跑必须无害,也不能借这个入口
// 改别的库。字符串里带分号的语句会被拆坏,拆出来的片段过不了这道检查,同样整份拒绝。
func loadBusinessDDL(path string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("ddl-file must be an absolute path")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, businessDDLLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > businessDDLLimit {
		return nil, errors.New("ddl-file is larger than the size limit")
	}
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var statements []string
	for _, part := range strings.Split(strings.Join(kept, "\n"), ";") {
		statement := strings.TrimSpace(part)
		if statement == "" {
			continue
		}
		if err := checkBusinessDDLStatement(statement); err != nil {
			return nil, err
		}
		statements = append(statements, statement)
	}
	if len(statements) == 0 {
		return nil, errors.New("ddl-file has no statement to run")
	}
	return statements, nil
}

// checkBusinessDDLStatement 只放行 CREATE TABLE IF NOT EXISTS <表> 与 INSERT IGNORE INTO <表>,表名不带库名。
func checkBusinessDDLStatement(statement string) error {
	words := strings.Fields(statement)
	// head(n) 是前 n 个词(大写、单空格连接);后面至少还要有一个词当表名,否则返回空串。
	head := func(n int) string {
		if len(words) <= n {
			return ""
		}
		return strings.ToUpper(strings.Join(words[:n], " "))
	}
	var table string
	switch {
	case head(5) == "CREATE TABLE IF NOT EXISTS":
		table = words[5]
	case head(3) == "INSERT IGNORE INTO":
		table = words[3]
	default:
		return fmt.Errorf("ddl-file accepts only CREATE TABLE IF NOT EXISTS and INSERT IGNORE INTO, rejected: %.60s", statement)
	}
	if strings.Contains(strings.SplitN(table, "(", 2)[0], ".") {
		return fmt.Errorf("ddl-file table names must not carry a database name: %.60s", statement)
	}
	return nil
}

// prepareSchemaWithDDL 是 prepareSchema 加上可选的业务表 DDL:闸门表照旧走共享的 EnsureCatalogSchema,
// 之后在同一条 Schema 连接上逐句执行 businessDDL(已由 loadBusinessDDL 校验过)。
func prepareSchemaWithDDL(ctx context.Context, schema string, getenv func(string) string, businessDDL []string) error {
	if getenv == nil || schema == "" || strings.TrimSpace(schema) != schema {
		return errors.New("prepare-schema缺少显式目标逻辑库或Schema装配")
	}
	for _, key := range []string{"MMORPG_SQL_WRITER_GRANT_FILE", "MMORPG_SQL_REGISTRAR_DSN", "MMORPG_SQL_MAINTENANCE_DSN"} {
		if getenv(key) != "" {
			return errors.New("prepare-schema拒绝继承NORMAL或其他控制身份")
		}
	}
	dsn := getenv("MMORPG_SQL_SCHEMA_DSN")
	permission, err := authoritysql.LoadSchemaPermission(getenv)
	if err != nil || dsn == "" {
		return errors.New("prepare-schema必须显式提供独立Schema连接和主体")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || parsed.DBName != schema {
		return errors.New("prepare-schema的连接必须明确指向同一目标逻辑库")
	}
	// 同时复用共享标识符校验，不把任意字符串拼进SQL。
	if _, err := authoritysql.DSNForSchema(dsn, schema); err != nil {
		return err
	}
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return err
	}
	placementConfig, err := dbplacement.ConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	// 和其他-migrate入口相同：要求映射而缺配置时拒绝；MOVING期间禁止DDL，
	// 已搬走的库按当前映射连接，不能向旧DSN地址补建一份闸门。
	if err := dbplacement.RequireForMigration(placementConfig, getenv); err != nil {
		return err
	}
	placementOptions, err := dbplacement.ForMigration(ctx, placementConfig, schema)
	if err != nil {
		return err
	}
	db, err := authoritysql.OpenForMigration(ctx, dsn, schema, backend, permission, placementOptions...)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := authoritysql.EnsureCatalogSchema(ctx, db, schema); err != nil {
		return err
	}
	for i, statement := range businessDDL {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("prepare-schema failed at ddl-file statement %d (%.60s): %w", i+1, statement, err)
		}
	}
	return nil
}

func run(parent context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("writer_authority", flag.ContinueOnError)
	command := flags.String("command", "", "register、retire、status、prepare-schema或publication只读资格")
	schemaName := flags.String("schema", "", "prepare-schema的显式目标逻辑库")
	ddlFile := flags.String("ddl-file", "", "absolute path of business-table DDL that prepare-schema runs in the same logical database after the gate tables (idempotent statements only)")
	requestPath := flags.String("request", "", "单个正式Request/维护Receipt protojson绝对路径")
	requestSHA := flags.String("request-sha256", "", "publication原件的固定原字节SHA")
	preparedPath := flags.String("prepared", "", "实际prepared.source.json绝对路径")
	preparedSHA := flags.String("prepared-sha256", "", "实际prepared原字节SHA")
	rosterPath := flags.String("roster", "", "有限正式BootRoster protojson绝对路径")
	rosterSHA := flags.String("roster-sha256", "", "已批准有限roster的原字节SHA256")
	inputRoot := flags.String("input-root", "", "受限init输入根，投影Secret软链解析后不得逃出")
	runtimeConfig := flags.String("runtime-config", "", "单Request的完整normal配置")
	runtimeOutput := flags.String("runtime-config-output", "", "只输出选中normal完整配置到本Pod私有卷")
	runtimeEnvironmentOutput := flags.String("runtime-env-output", "", "选中roster条目的SQL环境文件私有目录")
	qualificationPath := flags.String("lock-qualification", "", "真实I3原件绝对路径，其SHA等Request12")
	bootOutput := flags.String("boot-output", "", "私有16字段启动grant输出绝对路径")
	receiptOutput := flags.String("receipt-output", "", "已确认正式生命周期收据的私有绝对输出路径")
	deploymentID := flags.String("deployment-id", "", "实际PodUID或本地固定boot标识")
	timeout := flags.Duration("timeout", 30*time.Second, "单次尝试截止，不自动重试")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *command == "prepare-schema" {
		mixed := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name != "command" && f.Name != "schema" && f.Name != "timeout" && f.Name != "ddl-file" {
				mixed = true
			}
		})
		if mixed || flags.NArg() != 0 || *timeout <= 0 || *timeout > 60*time.Second {
			return errors.New("prepare-schema accepts only an explicit -schema, an optional -ddl-file and a positive -timeout of at most 60s; lifecycle flags are not allowed")
		}
		// 先校验整份 DDL 再连库:文件有一句不合格就一句都不执行,也不先建闸门表。
		var businessDDL []string
		if *ddlFile != "" {
			statements, err := loadBusinessDDL(*ddlFile)
			if err != nil {
				return err
			}
			businessDDL = statements
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return prepareSchemaWithDDL(ctx, *schemaName, getenv, businessDDL)
	}
	if *schemaName != "" || *ddlFile != "" {
		return errors.New("-schema and -ddl-file are only for prepare-schema")
	}
	if *command == "verify-publication-prestart" || *command == "verify-publication-runtime" {
		if flags.NArg() != 0 || *timeout <= 0 || *timeout > 60*time.Second || *rosterPath != "" || *rosterSHA != "" || *runtimeConfig != "" || *runtimeOutput != "" || *runtimeEnvironmentOutput != "" || *qualificationPath != "" || *bootOutput != "" || *receiptOutput != "" || *deploymentID != "" {
			return errors.New("publication只读资格不接受生命周期写入/输出参数")
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return runPublicationValidation(ctx, *command, *requestPath, *requestSHA, *preparedPath, *preparedSHA, *inputRoot, getenv, out)
	}
	if *requestSHA != "" || *preparedPath != "" || *preparedSHA != "" {
		return errors.New("publication原件参数不能混入生命周期命令")
	}
	if flags.NArg() != 0 || (*command != "register" && *command != "retire" && *command != "status") || *timeout <= 0 || *timeout > 60*time.Second || ((*requestPath == "") == (*rosterPath == "")) {
		return errors.New("必须显式选择命令、唯一正式输入和不超过60秒的正截止")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	if *rosterPath != "" && (*command != "register" || !filepath.IsAbs(*rosterPath) || !filepath.IsAbs(*inputRoot) || !filepath.IsAbs(*runtimeOutput) || *runtimeConfig != "") {
		return errors.New("roster只用于register，必须绝对roster/input-root/runtime输出且不另给runtime-config")
	}
	if (*rosterPath == "") != (*rosterSHA == "") {
		return errors.New("roster和已批准原字节SHA必须同时指定")
	}
	if *requestPath != "" && !filepath.IsAbs(*requestPath) {
		return errors.New("Request输入必须绝对路径")
	}
	if *receiptOutput != "" && !filepath.IsAbs(*receiptOutput) {
		return errors.New("收据输出必须绝对路径")
	}
	if *command == "retire" && *receiptOutput == "" {
		return errors.New("RETIRE必须指定原请求尝试及正式结局的私有receipt-output")
	}
	if *receiptOutput != "" {
		if err := checkOutputPaths(*receiptOutput, ""); err != nil {
			return err
		}
		if *receiptOutput == *bootOutput || *receiptOutput == *runtimeOutput {
			return errors.New("收据输出不得复用启动grant或normal配置路径")
		}
	}
	if *runtimeEnvironmentOutput != "" && (*rosterPath == "" || !filepath.IsAbs(*runtimeEnvironmentOutput)) {
		return errors.New("环境文件输出只用于正式roster，且必须绝对私有目录")
	}
	if *command == "register" && (!filepath.IsAbs(*bootOutput) || !filepath.IsAbs(*qualificationPath) || strings.TrimSpace(*deploymentID) == "") {
		return errors.New("register必须绑定绝对boot输出、真实I3原件及实际deployment-id")
	}
	if *command != "register" && (*bootOutput != "" || *deploymentID != "" || *runtimeOutput != "" || *runtimeConfig != "" || *qualificationPath != "" || *runtimeEnvironmentOutput != "") {
		return errors.New("只有register可以输出启动能力/完整配置/I3资格")
	}
	if *rosterPath == "" && ((*runtimeConfig == "") != (*runtimeOutput == "")) {
		return errors.New("单Request运行配置输入和输出必须同时指定")
	}
	if *inputRoot == "" {
		*inputRoot = filepath.Dir(*requestPath)
	}
	root, err := resolveInputRoot(*inputRoot)
	if err != nil {
		return err
	}
	var entries []*dbpb.SqlWriterBootRoster_Entry
	if *rosterPath != "" {
		raw, readErr := readBoundedInput(ctx, *rosterPath, root, rosterLimit, false)
		if readErr != nil {
			return readErr
		}
		approved, decodeErr := hex.DecodeString(*rosterSHA)
		sum := sha256.Sum256(raw)
		if decodeErr != nil || len(approved) != 32 || !bytes.Equal(sum[:], approved) {
			return errors.New("init实际roster与已批准原字节SHA不符")
		}
		roster := &dbpb.SqlWriterBootRoster{}
		if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, roster); err != nil || roster.Format != "SQL_WRITER_BOOT_ROSTER_V1" || len(roster.Entries) == 0 || len(roster.Entries) > rosterEntryLimit {
			return errors.New("输入不是当前正式有限BootRoster或超1024既有issuer预算")
		}
		entries = roster.Entries
	} else {
		raw, readErr := readBoundedInput(ctx, *requestPath, root, requestLimit, false)
		if readErr != nil {
			return readErr
		}
		request := &dbpb.SqlWriterLifecycleReceipt_Request{}
		if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, request); err != nil {
			return errors.New("输入不是当前正式Request protojson")
		}
		entries = []*dbpb.SqlWriterBootRoster_Entry{{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{request}, RuntimeConfigPath: *runtimeConfig}}
		if *runtimeConfig != "" {
			body, readErr := readBoundedInput(ctx, *runtimeConfig, root, runtimeConfigLimit, true)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(body)
			entries[0].RuntimeConfigSha256 = bytes.Clone(sum[:])
		}
	}
	physical, err := hex.DecodeString(getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 {
		return errors.New("缺独立物理authority原件SHA")
	}
	var qualification []byte
	if *command == "register" {
		body, readErr := readBoundedInput(ctx, *qualificationPath, root, rosterLimit, false)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		qualification = bytes.Clone(sum[:])
		if err = checkOutputPaths(*bootOutput, *runtimeOutput); err != nil {
			return err
		}
	}
	// 所有候选纯检查在首次SQL副作用之前完成；直接使用正式消息，不造平行roster/请求模型。
	seenKeys, seenInstances, seenPrincipals, seenRequests := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	shape := ""
	for _, entry := range entries {
		if entry == nil || len(entry.Requests) == 0 || len(entry.Requests) > rosterEntryLimit {
			return errors.New("roster含空entry或正式Request")
		}
		if *receiptOutput != "" && len(entry.Requests) != 1 {
			return errors.New("收据输出只用于单个逻辑库的Request")
		}
		for index, original := range entry.Requests {
			if original == nil {
				return errors.New("roster含空entry或正式Request")
			}
			request := proto.Clone(original).(*dbpb.SqlWriterLifecycleReceipt_Request)
			if err = validateRequestIdentity(request, *command, physical); err != nil {
				return err
			}
			if *command != "register" {
				continue
			}
			if *rosterPath != "" {
				if request.DeploymentId != "" && request.DeploymentId != "PLACEHOLDER_POD_UID" && request.DeploymentId != *deploymentID {
					return errors.New("roster预绑定部署与实际Pod身份不符")
				}
				request.DeploymentId = *deploymentID
			} else if request.DeploymentId != *deploymentID {
				return errors.New("deployment-id必须包含在同一永久请求内，不能换Pod重发旧身份")
			}
			if request.LockPrimitive != dbpb.SqlWriterBootGrant_SELECT_ONLY && request.LockPrimitive != dbpb.SqlWriterBootGrant_LOCK_TOUCH {
				return errors.New("未选定已资格锁原语")
			}
			if len(request.LockQualificationSha256) != 32 || !bytes.Equal(request.LockQualificationSha256, qualification) {
				return errors.New("真实I3原件SHA与Request12不符，不发启动能力")
			}
			supplied := bytes.Clone(request.Writer.RegistrationDigest)
			request, err = authoritysql.CanonicalRegistrationRequest(request)
			if err != nil {
				return err
			}
			if *rosterPath == "" && (len(supplied) != 32 || !bytes.Equal(supplied, request.Writer.RegistrationDigest)) {
				return errors.New("单Request原注册摘要不符，不能静默改写")
			}
			entry.Requests[index] = request
			if seenRequests[string(request.RequestId)] {
				return errors.New("roster重复writer/instance/principal/request，禁止复用cap")
			}
			seenRequests[string(request.RequestId)] = true
		}
		if err = validateRosterEntry(entry); err != nil {
			return err
		}
		first := entry.Requests[0]
		if *command == "register" {
			for _, pair := range []struct {
				values map[string]bool
				value  []byte
			}{{seenKeys, first.Writer.WriterKey}, {seenInstances, first.Writer.InstanceId}, {seenPrincipals, first.Writer.SqlPrincipal}} {
				if pair.values[string(pair.value)] {
					return errors.New("roster重复writer/instance/principal/request，禁止复用cap")
				}
				pair.values[string(pair.value)] = true
			}
			if *rosterPath != "" && (entry.RuntimeConfigPath == "" || len(entry.RuntimeConfigSha256) != 32) {
				return errors.New("roster entry缺normal完整配置路径/SHA")
			}
			if entry.RuntimeConfigPath != "" {
				body, readErr := readBoundedInput(ctx, entry.RuntimeConfigPath, root, runtimeConfigLimit, true)
				if readErr != nil {
					return readErr
				}
				sum := sha256.Sum256(body)
				if !bytes.Equal(sum[:], entry.RuntimeConfigSha256) {
					return errors.New("roster完整运行配置原bytes SHA不符")
				}
			}
			if len(entry.RuntimeEnvironment) > 0 && *runtimeEnvironmentOutput == "" {
				return errors.New("正式roster环境输入没有固定私有输出目录")
			}
			if _, err = readRuntimeEnvironment(ctx, entry, root); err != nil {
				return err
			}
			for _, variable := range entry.RuntimeEnvironment {
				if err = checkOutputPaths(filepath.Join(*runtimeEnvironmentOutput, variable.Name), ""); err != nil {
					return err
				}
			}
		}
		if current := rosterEntryShape(entry); shape == "" {
			shape = current
		} else if current != shape {
			return errors.New("同roster必须固定同逻辑库集合/generation/恢复run")
		}
	}
	permission, err := authoritysql.LoadOperatorPermission(getenv)
	if err != nil {
		return err
	}
	dsnName := "MMORPG_SQL_REGISTRAR_DSN"
	if *command == "retire" {
		dsnName = "MMORPG_SQL_MAINTENANCE_DSN"
	}
	dsn := getenv(dsnName)
	if dsn == "" {
		return errors.New("缺本命令独立控制DSN")
	}
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return err
	}
	placementConfig, err := dbplacement.ConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	// 闸门在各逻辑库自己身上，按库各连一次。同集群共用这一条控制连接串；库搬到别的集群时地址按映射给（dbplacement），账号在目标集群提前建好。
	catalogs := make(map[string]*authoritysql.Catalog)
	var pools []*sql.DB
	defer func() {
		for _, db := range pools {
			_ = db.Close()
		}
	}()
	openCatalog := func(request *dbpb.SqlWriterLifecycleReceipt_Request) (*authoritysql.Catalog, error) {
		schema := request.AuthoritySchema
		if catalog, ok := catalogs[schema]; ok {
			return catalog, nil
		}
		schemaDSN, err := authoritysql.DSNForSchema(dsn, schema)
		if err != nil {
			return nil, err
		}
		placementOptions, err := dbplacement.ForOneShot(ctx, placementConfig, schema)
		if err != nil {
			return nil, err
		}
		db, err := authoritysql.Open(ctx, schemaDSN, schema, backend, 2, permission, placementOptions...)
		if err != nil {
			return nil, err
		}
		pools = append(pools, db)
		catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: schema, Generation: request.Writer.Generation, ProtectedSchemas: request.ProtectedSchemas, PhysicalAuthoritySHA256: physical, Principal: getenv("MMORPG_SQL_OPERATOR_PRINCIPAL")})
		if err != nil {
			return nil, err
		}
		catalogs[schema] = catalog
		return catalog, nil
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if *command != "register" {
			// retire / status 只有单Request形态：退休逐逻辑库做。
			request := entry.Requests[0]
			catalog, err := openCatalog(request)
			if err != nil {
				return err
			}
			var receipt *dbpb.SqlWriterLifecycleReceipt
			if *command == "retire" {
				fresh, attemptErr := recordAttempt(*receiptOutput+".retire.started.protojson", request)
				if attemptErr != nil {
					return attemptErr
				}
				if fresh {
					receipt, err = catalog.Retire(ctx, request)
				} else {
					receipt, err = catalog.LifecycleReceipt(ctx, request.RequestId)
					if err != nil {
						return errors.New("原RETIRE结局未定；只按原request做status，不重发或换ID")
					}
				}
			} else {
				receipt, err = catalog.LifecycleReceipt(ctx, request.RequestId)
			}
			if err != nil {
				return err
			}
			if err = confirmLifecycle(receipt, request); err != nil {
				return err
			}
			if err = writeLifecycleReceipt(*receiptOutput, receipt); err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "writer_authority command=%s request_id=%s confirmed=true\n", *command, hex.EncodeToString(request.RequestId))
			return err
		}
		registrar := bootRegistration{
			bootOutput: *bootOutput,
			physical:   physical,
			open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (registrationCatalog, error) {
				catalog, err := openCatalog(request)
				if err != nil {
					return nil, err
				}
				return catalog, nil
			},
			recordAttempt:  recordAttempt,
			retractAttempt: retractAttempt,
		}
		consumed, err := registrar.consumed(ctx, entry)
		if err != nil {
			return err
		}
		if consumed {
			continue
		}
		var config []byte
		if entry.RuntimeConfigPath != "" {
			config, err = readBoundedInput(ctx, entry.RuntimeConfigPath, root, runtimeConfigLimit, true)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(config)
			if !bytes.Equal(sum[:], entry.RuntimeConfigSha256) {
				return errors.New("选中配置在提交前变化")
			}
		}
		environment, readErr := readRuntimeEnvironment(ctx, entry, root)
		if readErr != nil {
			return readErr
		}
		set, receipt, consumed, err := registrar.register(ctx, entry)
		if err != nil {
			return err
		}
		if consumed {
			continue
		}
		if len(config) > 0 {
			if err = writeConfirmedBytes(*runtimeOutput, config); err != nil {
				return err
			}
		}
		for _, variable := range entry.RuntimeEnvironment {
			if err = writeConfirmedBytes(filepath.Join(*runtimeEnvironmentOutput, variable.Name), environment[variable.Name]); err != nil {
				return err
			}
		}
		if err = writeConfirmedGrantSet(*bootOutput, set); err != nil {
			return err
		}
		if err = writeLifecycleReceipt(*receiptOutput, receipt); err != nil {
			return err
		}
		for _, request := range entry.Requests {
			if _, err = fmt.Fprintf(out, "writer_authority command=%s request_id=%s confirmed=true\n", *command, hex.EncodeToString(request.RequestId)); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("有限roster全部已由其它明确boot消费，不生成新key或principal")
}

// registrationCatalog 是注册一个 boot 用到的目录操作,生产实现 *authoritysql.Catalog。
type registrationCatalog interface {
	RegistrationConsumedByOtherDeployment(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (bool, error)
	RegistrationReady(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) error
	Register(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error)
	LifecycleReceipt(ctx context.Context, id []byte) (*dbpb.SqlWriterLifecycleReceipt, error)
	CurrentMeta(ctx context.Context) (*dbpb.RouteMeta, error)
}

// bootRegistration 把名册的一条 entry 注册下去。一条 entry 是一个 boot,每个逻辑库一份 Request,不在一个事务里,所以结局要分清:
//
//	闸门冻结(预检或提交时)   确定没写。不留尝试记录,不算用掉这条 entry,init 重启接着来。
//	部分库已注册,进程还活着   重试时已注册的按原请求查收据,没注册的接着注册。同一个 Pod 的 deployment-id 不变,不算别的 boot 消费。
//	部分库已注册,Pod 没了     新 Pod 看到这条 entry 被别的 boot 消费,换下一条。已注册的那几份是没有进程的写者,和注册完就崩的 Pod 一样,按手册退休。
//	结局未知                  留尝试记录,不重发,只能查原请求(规格 §2)。
//
// 注册第一份之前逐库预检,把注定失败的情况挡在写任何东西之前,所以部分注册只剩预检和提交之间撞上冻结这一种。
type bootRegistration struct {
	bootOutput     string
	physical       []byte
	open           func(*dbpb.SqlWriterLifecycleReceipt_Request) (registrationCatalog, error)
	recordAttempt  func(string, *dbpb.SqlWriterLifecycleReceipt_Request) (bool, error)
	retractAttempt func(string) error
}

// consumed 报告这条 entry 是不是被别的 boot 用过:任一份被别的 boot 消费,整条就算用掉。
func (r bootRegistration) consumed(ctx context.Context, entry *dbpb.SqlWriterBootRoster_Entry) (bool, error) {
	consumed := false
	for _, request := range entry.Requests {
		catalog, err := r.open(request)
		if err != nil {
			return false, err
		}
		foreign, err := catalog.RegistrationConsumedByOtherDeployment(ctx, request)
		if err != nil {
			return false, err
		}
		consumed = consumed || foreign
	}
	return consumed, nil
}

// register 注册这条 entry 的全部逻辑库,回写资格集合和最后一份收据。consumed=true 表示这条 entry 被别的 boot 抢先用了,换下一条。
func (r bootRegistration) register(ctx context.Context, entry *dbpb.SqlWriterBootRoster_Entry) (set *dbpb.SqlWriterBootGrantSet, receipt *dbpb.SqlWriterLifecycleReceipt, consumed bool, resultErr error) {
	for _, request := range entry.Requests {
		catalog, err := r.open(request)
		if err != nil {
			return nil, nil, false, err
		}
		if err := catalog.RegistrationReady(ctx, request); err != nil {
			return nil, nil, false, fmt.Errorf("logical database %s cannot register yet, nothing was written: %w", request.AuthoritySchema, err)
		}
	}
	set = &dbpb.SqlWriterBootGrantSet{Format: authoritysql.GrantSetFormat}
	for _, request := range entry.Requests {
		catalog, err := r.open(request)
		if err != nil {
			return nil, nil, false, err
		}
		attempt := r.bootOutput + "." + hex.EncodeToString(request.RequestId) + ".register.started.protojson"
		fresh, attemptErr := r.recordAttempt(attempt, request)
		if attemptErr != nil {
			return nil, nil, false, attemptErr
		}
		if fresh {
			receipt, err = catalog.Register(ctx, request)
			if authoritysql.IsWriterFrozen(err) {
				// 事务已回滚,确定没有落地。尝试记录不能留:留着的话重试按「结局未定」处理,这条 entry 就废了。
				if retractErr := r.retractAttempt(attempt); retractErr != nil {
					return nil, nil, false, errors.Join(err, retractErr)
				}
				return nil, nil, false, fmt.Errorf("the writer gate of %s froze during registration, nothing was written for it, retry: %w", request.AuthoritySchema, err)
			}
		} else {
			// init重启仅查原请求。查无/失联仍UNKNOWN，不再次Register、不新key或新principal。
			receipt, err = catalog.LifecycleReceipt(ctx, request.RequestId)
			if err != nil {
				return nil, nil, false, errors.New("原注册尝试结局未定，只允许status原件查询")
			}
		}
		if err != nil {
			if errors.Is(err, authoritysql.ErrOutcomeUnknown) {
				return nil, nil, false, err
			}
			foreign, readErr := catalog.RegistrationConsumedByOtherDeployment(ctx, request)
			if readErr != nil {
				return nil, nil, false, readErr
			}
			if foreign {
				return nil, nil, true, nil
			}
			return nil, nil, false, err
		}
		if err = confirmLifecycle(receipt, request); err != nil {
			foreign, readErr := catalog.RegistrationConsumedByOtherDeployment(ctx, request)
			if readErr != nil {
				return nil, nil, false, readErr
			}
			if foreign {
				return nil, nil, true, nil
			}
			return nil, nil, false, err
		}
		meta, readErr := catalog.CurrentMeta(ctx)
		if readErr != nil {
			return nil, nil, false, readErr
		}
		if meta.Writable != 1 {
			return nil, nil, false, fmt.Errorf("the writer gate of %s is frozen, retry: %w", request.AuthoritySchema, authoritysql.ErrWriterFrozen)
		}
		if meta.Generation != request.Writer.Generation || !bytes.Equal(meta.FreezeRunId, request.MaintenanceRunId) {
			return nil, nil, false, errors.New("原REGISTER虽确认但当前boot资格已变，HOLD不输出grant")
		}
		set.Grants = append(set.Grants, &dbpb.SqlWriterBootGrant{Format: "SQL_WRITER_BOOT_V1", WriterKey: bytes.Clone(request.Writer.WriterKey), InstanceId: bytes.Clone(request.Writer.InstanceId), Generation: request.Writer.Generation, AuthoritySchema: request.AuthoritySchema, ProtectedSchemas: append([]string(nil), request.ProtectedSchemas...), SqlPrincipal: bytes.Clone(request.Writer.SqlPrincipal), RegistrationDigest: bytes.Clone(request.Writer.RegistrationDigest), DeploymentId: request.DeploymentId, GrantId: bytes.Clone(request.RequestId), GrantedMs: request.Writer.RegisteredMs, Permission: "NORMAL", PhysicalAuthoritySha256: bytes.Clone(r.physical), MaintenanceRunId: bytes.Clone(request.MaintenanceRunId), LockPrimitive: request.LockPrimitive, LockQualificationSha256: bytes.Clone(request.LockQualificationSha256)})
	}
	return set, receipt, false, nil
}

// retractAttempt 撤掉一次确定没有落地的注册尝试。
func retractAttempt(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeLifecycleReceipt(path string, receipt *dbpb.SqlWriterLifecycleReceipt) error {
	if path == "" {
		return nil
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(receipt)
	if err != nil {
		return err
	}
	return writeConfirmedBytes(path, raw)
}

// validateRosterEntry:一条 entry 是一个 boot。它要写的每个逻辑库各一份 Request，身份和账号都相同，逻辑库不重复。
func validateRosterEntry(entry *dbpb.SqlWriterBootRoster_Entry) error {
	first := entry.Requests[0]
	schemas := make(map[string]bool, len(entry.Requests))
	for _, request := range entry.Requests {
		if schemas[request.AuthoritySchema] {
			return errors.New("同一boot的Request逻辑库重复")
		}
		schemas[request.AuthoritySchema] = true
		if !bytes.Equal(request.Writer.WriterKey, first.Writer.WriterKey) || !bytes.Equal(request.Writer.InstanceId, first.Writer.InstanceId) || !bytes.Equal(request.Writer.SqlPrincipal, first.Writer.SqlPrincipal) {
			return errors.New("同一boot在各逻辑库的writer身份和账号必须相同")
		}
	}
	return nil
}

// rosterEntryShape 是一条 entry 的逻辑库集合：名册里的候选 boot 都要写同样的库。
func rosterEntryShape(entry *dbpb.SqlWriterBootRoster_Entry) string {
	parts := make([]string, 0, len(entry.Requests))
	for _, request := range entry.Requests {
		parts = append(parts, request.AuthoritySchema+"/"+request.Writer.Generation+"/"+hex.EncodeToString(request.MaintenanceRunId))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func validateRequestIdentity(request *dbpb.SqlWriterLifecycleReceipt_Request, command string, physical []byte) error {
	if request == nil || request.Writer == nil || len(request.RequestId) != 16 || len(request.Writer.WriterKey) != 16 || len(request.Writer.InstanceId) != 16 || request.Writer.RegisteredMs == 0 || len(request.DeploymentDigest) != 32 || !bytes.Equal(request.DeploymentDigest, physical) {
		return errors.New("生命周期原件缺固定身份或独立物理authority摘要")
	}
	// 名册是预先渲染的,cutover 和回滚都会推进 epoch,所以 REGISTER 不带(规格 §2)。RETIRE 带,它要和同 epoch 的维护 run 对账。
	if (request.Kind == dbpb.SqlWriterLifecycleReceipt_REGISTER) != (request.ExpectedEpoch == 0) {
		return errors.New("REGISTER must not carry an epoch and RETIRE must")
	}
	// 闸门只保护它所在的逻辑库。
	if len(request.ProtectedSchemas) != 1 || request.ProtectedSchemas[0] != request.AuthoritySchema {
		return errors.New("生命周期原件的受保护库必须恰好是闸门所在的逻辑库")
	}
	if (command == "register" && request.Kind != dbpb.SqlWriterLifecycleReceipt_REGISTER) || (command == "retire" && request.Kind != dbpb.SqlWriterLifecycleReceipt_RETIRE) {
		return errors.New("命令与原件kind不符")
	}
	if command != "register" && len(request.Writer.RegistrationDigest) != 32 {
		return errors.New("查询/退休须绑定原REGISTER摘要")
	}
	if command == "retire" && request.ExpectedCatalogRevision == 0 {
		return errors.New("RETIRE须绑定非零current catalog revision")
	}
	return nil
}
func confirmLifecycle(receipt *dbpb.SqlWriterLifecycleReceipt, request *dbpb.SqlWriterLifecycleReceipt_Request) error {
	if receipt == nil || receipt.Kind != request.Kind || !bytes.Equal(receipt.RequestId, request.RequestId) {
		return errors.New("永久生命周期结局与原请求身份不符")
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	expectedDigest := digest[:]
	if request.Kind == dbpb.SqlWriterLifecycleReceipt_REGISTER {
		expectedDigest = request.Writer.RegistrationDigest
	}
	if !bytes.Equal(receipt.RequestProto, raw) || !bytes.Equal(receipt.RequestDigest, expectedDigest) || receipt.Generation != request.Writer.Generation || receipt.PreviousEpoch == 0 || receipt.ResultingEpoch != receipt.PreviousEpoch || receipt.CatalogRevision == 0 {
		return errors.New("永久生命周期结局与完整原件/摘要/epoch不一致")
	}
	// REGISTER 的收据记落地时的实际 epoch,请求里没有可比的。
	if request.Kind == dbpb.SqlWriterLifecycleReceipt_RETIRE && receipt.PreviousEpoch != request.ExpectedEpoch {
		return errors.New("永久生命周期结局与完整原件/摘要/epoch不一致")
	}
	if request.ExpectedCatalogRevision != 0 && (request.ExpectedCatalogRevision == ^uint64(0) || receipt.CatalogRevision != request.ExpectedCatalogRevision+1) {
		return errors.New("永久生命周期目录版本不是原请求的单步后继")
	}
	return nil
}

// 只承载仓库实际SQL调用者读取的变量，既不解释shell文本，也不允许控制凭据进入normal。
func readRuntimeEnvironment(ctx context.Context, entry *dbpb.SqlWriterBootRoster_Entry, root string) (map[string][]byte, error) {
	if len(entry.RuntimeEnvironment) > 5 {
		return nil, errors.New("roster SQL环境超过实际五变量白名单")
	}
	values := make(map[string][]byte, len(entry.RuntimeEnvironment))
	for _, variable := range entry.RuntimeEnvironment {
		if variable == nil || len(variable.ValueSha256) != 32 || variable.ValuePath == "" {
			return nil, errors.New("roster环境缺正式私有文件路径/SHA")
		}
		isDSN := false
		switch variable.Name {
		case "CHAT_AUTHORITY_DSN", "MMORPG_GUILD_SQL_DSN", "MMORPG_MAIL_SQL_DSN", "MMORPG_RANK_SQL_DSN":
			isDSN = true
		case "MATCH_MYSQL_PASSWORD":
		default:
			return nil, errors.New("roster环境不是实际SQL白名单，禁止解释任意变量")
		}
		if _, duplicate := values[variable.Name]; duplicate {
			return nil, errors.New("roster SQL环境名重复")
		}
		value, err := readBoundedInput(ctx, variable.ValuePath, root, 64<<10, true)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(value)
		if !bytes.Equal(sum[:], variable.ValueSha256) || !utf8.Valid(value) || bytes.IndexAny(value, "\x00\r\n") >= 0 {
			return nil, errors.New("SQL环境原字节SHA或文本边界不符")
		}
		if isDSN {
			dsn, parseErr := mysqldriver.ParseDSN(string(value))
			if parseErr != nil {
				return nil, errors.New("SQL环境不是有效DSN")
			}
			principal := string(entry.Requests[0].Writer.SqlPrincipal)
			at := strings.LastIndexByte(principal, '@')
			if at < 1 || dsn.User != principal[:at] {
				return nil, errors.New("选中SQL环境用户名与同entry永久principal不符")
			}
			protected := false
			for _, request := range entry.Requests {
				if dsn.DBName == request.AuthoritySchema {
					protected = true
					break
				}
			}
			if !protected {
				return nil, errors.New("选中DSN库不是这个boot要写的逻辑库")
			}
		}
		values[variable.Name] = value
	}
	return values, nil
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func resolveInputRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("input-root必须为绝对受限目录")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("input-root不是已有目录")
	}
	return filepath.Clean(resolved), nil
}
func readBoundedInput(ctx context.Context, path, root string, limit int64, private bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if runtime.GOOS == "windows" {
		rel, err := filepath.Rel(root, filepath.Clean(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("init输入逃出固定input-root")
		}
		return authoritysql.ReadPrivateControlFile(path, limit)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("init输入软链/路径逃出固定input-root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("init原件不是预算内普通文件")
	}
	if private && info.Mode().Perm()&0007 != 0 {
		return nil, errors.New("normal秘密配置禁止world可读，Secret须0440/0600且仅挂init")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > limit {
		return nil, errors.New("init读取期间原件超预算")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}
func checkOutputPaths(boot, runtime string) error {
	if !filepath.IsAbs(boot) || (runtime != "" && !filepath.IsAbs(runtime)) || runtime == boot || runtime == boot+".sha256" {
		return errors.New("grant/选中配置输出须不同绝对路径")
	}
	for _, path := range []string{boot, runtime} {
		if path == "" {
			continue
		}
		if err := authoritysql.CheckPrivateControlDirectory(filepath.Dir(path)); err != nil {
			return errors.New("启动输出不是已有私有非软链目录")
		}
	}
	return nil
}
func recordAttempt(path string, request *dbpb.SqlWriterLifecycleReceipt_Request) (bool, error) {
	raw, err := authoritysql.ReadPrivateControlFile(path, requestLimit)
	if err == nil {
		old := &dbpb.SqlWriterLifecycleReceipt_Request{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, old) != nil || !proto.Equal(old, request) {
			return false, errors.New("私有attempt与本原请求冲突，不能重放/换部署")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	body, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(request)
	if err != nil {
		return false, err
	}
	if err = writePrivateExclusive(path, body); err != nil {
		return false, err
	}
	return true, nil
}

func writeConfirmedBytes(path string, body []byte) error {
	old, err := authoritysql.ReadPrivateControlFile(path, int64(len(body)))
	if err == nil {
		if !bytes.Equal(old, body) {
			return errors.New("已存在私有输出与原确认结局冲突")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writePrivateExclusive(path, body)
}

func writeConfirmedGrantSet(path string, set *dbpb.SqlWriterBootGrantSet) error {
	body, err := authoritysql.ReadPrivateControlFile(path, 1<<20)
	if err == nil {
		old := &dbpb.SqlWriterBootGrantSet{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, old) != nil || !proto.Equal(old, set) {
			return errors.New("原grant不是同一正式确认结局")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		body, err = (protojson.MarshalOptions{UseProtoNames: false}).Marshal(set)
		if err != nil {
			return err
		}
		if err = writePrivateExclusive(path, body); err != nil {
			return err
		}
	} else {
		return err
	}
	sum := sha256.Sum256(body)
	return writeConfirmedBytes(path+".sha256", []byte(hex.EncodeToString(sum[:])+"\n"))
}

func writePrivateExclusive(path string, body []byte) error {
	return authoritysql.WritePrivateControlFile(path, body)
}

// 只读资格与私有控制文件是两个显式步骤；这里绝不替应用生成prepared或SOURCE已开ACK。
func decodePinnedPublication(raw []byte, want string, target proto.Message) error {
	expected, err := hex.DecodeString(want)
	sum := sha256.Sum256(raw)
	if err != nil || len(expected) != 32 || len(raw) == 0 || len(raw) > rosterLimit || !bytes.Equal(sum[:], expected) {
		return errors.New("publication原件大小/SHA不符")
	}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, target); err != nil {
		return errors.New("publication原件不是正式protojson")
	}
	return nil
}
func readPublicationInput(ctx context.Context, path, root, want string, target proto.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return errors.New("publication输入必须绝对路径")
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("publication输入逃出固定私有根")
	}
	raw, err := authoritysql.ReadPrivateControlFile(path, rosterLimit)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return decodePinnedPublication(raw, want, target)
}
func runPublicationValidation(ctx context.Context, command, requestPath, requestSHA, preparedPath, preparedSHA, inputRoot string, getenv func(string) string, out io.Writer) error {
	if !filepath.IsAbs(inputRoot) || !filepath.IsAbs(requestPath) {
		return errors.New("publication必须固定绝对input-root与request")
	}
	if err := authoritysql.CheckPrivateControlDirectory(inputRoot); err != nil {
		return err
	}
	if command == "verify-publication-runtime" {
		if !filepath.IsAbs(preparedPath) || preparedSHA == "" {
			return errors.New("开放资格必须绑定实际prepared原件及SHA")
		}
	} else if preparedPath != "" || preparedSHA != "" {
		return errors.New("prestart不接受伪造prepared")
	}
	request := &dbpb.SqlWriterMaintenanceReceipt{}
	if err := readPublicationInput(ctx, requestPath, inputRoot, requestSHA, request); err != nil {
		return err
	}
	plan := &dbpb.SqlWriterMaintenanceReceipt_Plan{}
	if err := proto.Unmarshal(request.PlanProto, plan); err != nil {
		return errors.New("publication原件Plan无效")
	}
	var ready *dbpb.SqlWriterMaintenanceReceipt_IssuerReadiness
	if command == "verify-publication-runtime" {
		ready = &dbpb.SqlWriterMaintenanceReceipt_IssuerReadiness{}
		if err := readPublicationInput(ctx, preparedPath, inputRoot, preparedSHA, ready); err != nil {
			return err
		}
	}
	physical, err := hex.DecodeString(getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 {
		return errors.New("publication缺独立物理authority原件SHA")
	}
	permission, err := authoritysql.LoadOperatorPermission(getenv)
	if err != nil {
		return err
	}
	dsn := getenv("MMORPG_SQL_MAINTENANCE_DSN")
	if dsn == "" {
		return errors.New("publication缺独立维护控制DSN")
	}
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return err
	}
	schemaDSN, err := authoritysql.DSNForSchema(dsn, plan.AuthoritySchema)
	if err != nil {
		return err
	}
	placementConfig, err := dbplacement.ConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	placementOptions, err := dbplacement.ForOneShot(ctx, placementConfig, plan.AuthoritySchema)
	if err != nil {
		return err
	}
	db, err := authoritysql.Open(ctx, schemaDSN, plan.AuthoritySchema, backend, 2, permission, placementOptions...)
	if err != nil {
		return err
	}
	defer db.Close()
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: plan.AuthoritySchema, Generation: request.Generation, ProtectedSchemas: plan.ProtectedSchemas, PhysicalAuthoritySHA256: physical, Principal: getenv("MMORPG_SQL_OPERATOR_PRINCIPAL")})
	if err != nil {
		return err
	}
	if command == "verify-publication-prestart" {
		_, err = catalog.VerifyPublicationPrestart(ctx, request)
	} else {
		_, err = catalog.VerifyPublicationRuntime(ctx, request, ready)
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, "publication verification confirmed\n")
	return err
}
