// writer_authority是独立部署入口；常驻服务不得获得本程序的控制凭据。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"math"
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

// 名册至多 1024 个槽位;一代至多写这么多逻辑库,与 authoritysql 的 maxBootGrants 同值。
const rosterSlotLimit = 1024
const bootRequestLimit = 1024

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

// ddlFileList 收集可重复的 -ddl-file,按命令行上的先后执行。一个逻辑库可以有不止一个没有 Go 迁移入口的属主:
// mmorpg 上是网关的区目录表(java/gateway_node 的 schema.sql)和 login 的口令凭据表(go/login/model/password_auth_schema.sql),
// 各带各的建表文件,不合并成一份。
type ddlFileList []string

func (l *ddlFileList) String() string { return strings.Join(*l, ",") }

func (l *ddlFileList) Set(path string) error {
	if path == "" {
		return errors.New("ddl-file must not be empty")
	}
	*l = append(*l, path)
	return nil
}

// loadBusinessDDLFiles 依次读每个 -ddl-file 并校验,返回合在一起的语句。任何一个文件不合格就整体拒绝,一句都不执行。
// 错误里只带文件名,不带目录。
func loadBusinessDDLFiles(paths []string) ([]string, error) {
	var statements []string
	for _, path := range paths {
		loaded, err := loadBusinessDDL(path)
		if err != nil {
			return nil, fmt.Errorf("ddl-file %s: %w", filepath.Base(path), err)
		}
		statements = append(statements, loaded...)
	}
	return statements, nil
}

// loadBusinessDDL 读 prepare-schema 建完闸门表之后要执行的业务表 DDL(-ddl-file)。只给没有 Go 迁移入口的属主用:
// 网关与 login 在 mmorpg 库上没有别的建表入口(docs/design/no-single-node-horizontal-scaling-20261001.md §6.3)。
//
// 拆句见 splitBusinessDDL(引号里的分号不拆)。只认两种幂等语句:CREATE TABLE IF NOT EXISTS 与 INSERT IGNORE INTO,表名不许带库名;
// 其余(USE、DROP、ALTER、DELETE、UPDATE、普通 INSERT……)整份拒绝,一句都不执行 —— Job 重跑必须无害,也不能借这个入口
// 改别的库。
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
	statements, err := splitBusinessDDL(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if err != nil {
		return nil, err
	}
	for _, statement := range statements {
		if err := checkBusinessDDLStatement(statement); err != nil {
			return nil, err
		}
	}
	if len(statements) == 0 {
		return nil, errors.New("ddl-file has no statement to run")
	}
	return statements, nil
}

// splitBusinessDDL 把建表文件拆成语句:去掉 "-- " 注释(整行或行尾),按引号之外的分号拆句,空语句丢掉。
//
// 引号内的分号与 "--" 原样保留:COMMENT '…;…' 这样的列注释是合法 DDL。2026-11-12 之前这里按分号硬拆,
// 网关 schema.sql 的列注释里一出现分号,整份文件就被下游的语句检查拒掉。引号指单引号、双引号字符串与反引号标识符;
// 同一个引号连写两次是转义,字符串里反斜杠转义下一个字符(MySQL 与 TiDB 的缺省词法)。
//
// 不认的写法一律报错,不猜:引号没闭合;引号之外出现 "#" 或 "/*"(块注释里可以藏 /*! … */ 这类可执行内容,建表文件用不着)。
// 按字节扫描对 UTF-8 是安全的:多字节字符的后续字节都不落在 ASCII 范围里。
func splitBusinessDDL(text string) ([]string, error) {
	var statements []string
	var current strings.Builder
	flush := func() {
		if statement := strings.TrimSpace(current.String()); statement != "" {
			statements = append(statements, statement)
		}
		current.Reset()
	}
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\'' || c == '"' || c == '`':
			end, err := quotedEnd(text, i)
			if err != nil {
				return nil, err
			}
			current.WriteString(text[i : end+1])
			i = end
		case c == '-' && startsLineComment(text, i):
			for i < len(text) && text[i] != '\n' {
				i++
			}
			current.WriteByte('\n')
		case c == '#' || (c == '/' && i+1 < len(text) && text[i+1] == '*'):
			return nil, errors.New(`ddl-file supports only "-- " comments outside quoted text`)
		case c == ';':
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return statements, nil
}

// startsLineComment 报告 text[i:] 是不是一个 "--" 行注释的开头:两个连字符之后必须是空白或文件结尾(MySQL 的规则,
// "a--b" 是减负数,不是注释)。
func startsLineComment(text string, i int) bool {
	if i+1 >= len(text) || text[i+1] != '-' {
		return false
	}
	if i+2 == len(text) {
		return true
	}
	next := text[i+2]
	return next == ' ' || next == '\t' || next == '\n' || next == '\r'
}

// quotedEnd 返回从 start 处那个引号开始的引用文本的结束引号下标。同一个引号连写两次是转义;
// 字符串(单、双引号)里反斜杠转义下一个字符,反引号标识符里没有反斜杠转义。
func quotedEnd(text string, start int) (int, error) {
	quote := text[start]
	for i := start + 1; i < len(text); i++ {
		switch {
		case text[i] == '\\' && quote != '`':
			i++
		case text[i] == quote:
			if i+1 < len(text) && text[i+1] == quote {
				i++
				continue
			}
			return i, nil
		}
	}
	return 0, errors.New("ddl-file has an unterminated quoted string or identifier")
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
	command := flags.String("command", "", "register、retire、status、retire-removed-slots、prepare-schema或publication只读资格")
	schemaName := flags.String("schema", "", "prepare-schema的显式目标逻辑库")
	var ddlFiles ddlFileList
	flags.Var(&ddlFiles, "ddl-file", "absolute path of business-table DDL that prepare-schema runs in the same logical database after the gate tables (idempotent statements only); repeatable, files run in the order given")
	requestPath := flags.String("request", "", "单个正式Request/维护Receipt protojson绝对路径")
	requestSHA := flags.String("request-sha256", "", "publication原件的固定原字节SHA")
	preparedPath := flags.String("prepared", "", "实际prepared.source.json绝对路径")
	preparedSHA := flags.String("prepared-sha256", "", "实际prepared原字节SHA")
	rosterPath := flags.String("roster", "", "register:槽位名册(SQL_WRITER_BOOT_ROSTER_V2) protojson 绝对路径")
	rosterSHA := flags.String("roster-sha256", "", "已批准槽位名册的原字节SHA256")
	inputRoot := flags.String("input-root", "", "受限init输入根，投影Secret软链解析后不得逃出")
	podName := flags.String("pod-name", "", "register:本 Pod 的名字(K8s metadata.name),末尾的序号选名册里的槽位")
	runtimeOutput := flags.String("runtime-config-output", "", "只输出选中normal完整配置到本Pod私有卷")
	runtimeEnvironmentOutput := flags.String("runtime-env-output", "", "选中roster条目的SQL环境文件私有目录")
	qualificationPath := flags.String("lock-qualification", "", "真实I3原件绝对路径，其SHA等Request12")
	bootOutput := flags.String("boot-output", "", "私有16字段启动grant输出绝对路径")
	receiptOutput := flags.String("receipt-output", "", "已确认正式生命周期收据的私有绝对输出路径")
	deploymentID := flags.String("deployment-id", "", "实际PodUID或本地固定boot标识")
	statefulSet := flags.String("statefulset", "", "retire-removed-slots:这个名册所属的写者 StatefulSet;名册下标就是它的 Pod 序号")
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
			return errors.New("prepare-schema accepts only an explicit -schema, optional -ddl-file flags and a positive -timeout of at most 60s; lifecycle flags are not allowed")
		}
		// 先校验全部 DDL 文件再连库:有一句不合格就一句都不执行,也不先建闸门表。
		businessDDL, err := loadBusinessDDLFiles(ddlFiles)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return prepareSchemaWithDDL(ctx, *schemaName, getenv, businessDDL)
	}
	if *schemaName != "" || len(ddlFiles) != 0 {
		return errors.New("-schema and -ddl-file are only for prepare-schema")
	}
	if *command == "verify-publication-prestart" || *command == "verify-publication-runtime" {
		if flags.NArg() != 0 || *timeout <= 0 || *timeout > 60*time.Second || *rosterPath != "" || *rosterSHA != "" || *podName != "" || *runtimeOutput != "" || *runtimeEnvironmentOutput != "" || *qualificationPath != "" || *bootOutput != "" || *receiptOutput != "" || *deploymentID != "" || *statefulSet != "" {
			return errors.New("publication只读资格不接受生命周期写入/输出参数")
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return runPublicationValidation(ctx, *command, *requestPath, *requestSHA, *preparedPath, *preparedSHA, *inputRoot, getenv, out)
	}
	if *requestSHA != "" || *preparedPath != "" || *preparedSHA != "" {
		return errors.New("publication原件参数不能混入生命周期命令")
	}
	if flags.NArg() != 0 || *timeout <= 0 || *timeout > 60*time.Second {
		return errors.New("必须显式选择命令、唯一正式输入和不超过60秒的正截止")
	}
	switch *command {
	case "register":
		if *requestPath != "" || *receiptOutput != "" || *statefulSet != "" {
			return errors.New("register only reads the slot roster; -request and -receipt-output belong to retire and status, -statefulset to retire-removed-slots")
		}
		options := registerOptions{rosterPath: *rosterPath, rosterSHA: *rosterSHA, inputRoot: *inputRoot, podName: *podName, qualificationPath: *qualificationPath,
			runtimeOutput: *runtimeOutput, runtimeEnvironmentOutput: *runtimeEnvironmentOutput, bootOutput: *bootOutput, deploymentID: *deploymentID}
		if err := options.check(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return runRegister(ctx, options, getenv, out)
	case "retire", "status":
		if *rosterPath != "" || *rosterSHA != "" || *podName != "" || *bootOutput != "" || *deploymentID != "" || *runtimeOutput != "" || *qualificationPath != "" || *runtimeEnvironmentOutput != "" || *statefulSet != "" {
			return errors.New("只有register可以输出启动能力/完整配置/I3资格")
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return runLifecycle(ctx, *command, *requestPath, *inputRoot, *receiptOutput, getenv, out)
	case "retire-removed-slots":
		if *requestPath != "" || *receiptOutput != "" || *podName != "" || *bootOutput != "" || *runtimeOutput != "" || *qualificationPath != "" || *runtimeEnvironmentOutput != "" {
			return errors.New("retire-removed-slots reads only the slot roster of one StatefulSet; it takes no request, pod name, lock qualification or output paths")
		}
		options := retireRemovedSlotsOptions{rosterPath: *rosterPath, rosterSHA: *rosterSHA, inputRoot: *inputRoot, statefulSet: *statefulSet, deploymentID: *deploymentID}
		if err := options.check(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		return runRetireRemovedSlots(ctx, options, getenv, out)
	default:
		return errors.New("必须显式选择命令、唯一正式输入和不超过60秒的正截止")
	}
}

// slotRosterFormat 是写者槽位名册的格式:一条 entry 是一个槽位,下标就是 StatefulSet 序号(writer-roster-exhaustion-20261106.md §5.1)。
const slotRosterFormat = "SQL_WRITER_BOOT_ROSTER_V2"

// bootRecordLimit 是本 Pod 启动记录的大小上限:data_service 一代要注册片库、流水库和全部存档库,一百多份请求也不到 1 MiB。
const bootRecordLimit = 1 << 20

// registerOptions 是 register 的命令行输入,纯校验都在读凭据之前做完。
type registerOptions struct {
	rosterPath, rosterSHA, inputRoot, podName, qualificationPath      string
	runtimeOutput, runtimeEnvironmentOutput, bootOutput, deploymentID string
}

func (o registerOptions) check() error {
	for _, path := range []string{o.rosterPath, o.inputRoot, o.runtimeOutput, o.bootOutput, o.qualificationPath} {
		if !filepath.IsAbs(path) {
			return errors.New("register needs absolute -roster, -input-root, -runtime-config-output, -boot-output and -lock-qualification paths")
		}
	}
	if o.rosterSHA == "" || strings.TrimSpace(o.deploymentID) == "" {
		return errors.New("register needs the approved -roster-sha256 and the real -deployment-id of this pod")
	}
	if o.runtimeEnvironmentOutput != "" && !filepath.IsAbs(o.runtimeEnvironmentOutput) {
		return errors.New("-runtime-env-output must be an absolute private directory")
	}
	_, err := slotOrdinal(o.podName)
	return err
}

// slotOrdinal 取 StatefulSet Pod 名末尾的序号:<名字>-<十进制序号>,不带符号和前导零,最多 4 位(名册至多 1024 个槽位)。
func slotOrdinal(podName string) (int, error) {
	at := strings.LastIndexByte(podName, '-')
	digits := ""
	if at > 0 {
		digits = podName[at+1:]
	}
	if digits == "" || len(digits) > 4 || len(digits) > 1 && digits[0] == '0' {
		return 0, fmt.Errorf("-pod-name %q is not a StatefulSet pod name ending in its ordinal", podName)
	}
	ordinal := 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("-pod-name %q is not a StatefulSet pod name ending in its ordinal", podName)
		}
		ordinal = ordinal*10 + int(c-'0')
	}
	return ordinal, nil
}

// runRegister 把名册里本 Pod 序号的槽位注册成这个 Pod 的一代,写出 grant 集合、选中配置和环境文件。
// 所有候选纯检查在首次SQL副作用之前完成；直接使用正式消息，不造平行roster/请求模型。
func runRegister(ctx context.Context, o registerOptions, getenv func(string) string, out io.Writer) error {
	root, err := resolveInputRoot(o.inputRoot)
	if err != nil {
		return err
	}
	physical, err := physicalAuthoritySHA(getenv)
	if err != nil {
		return err
	}
	roster, err := loadSlotRoster(ctx, o.rosterPath, o.rosterSHA, root, physical)
	if err != nil {
		return err
	}
	ordinal, err := slotOrdinal(o.podName)
	if err != nil {
		return err
	}
	if ordinal >= len(roster.Entries) {
		return fmt.Errorf("pod %s is slot %d but the roster has %d slots: the workload has more replicas than the roster", o.podName, ordinal, len(roster.Entries))
	}
	template := roster.Entries[ordinal]
	qualification, err := readBoundedInput(ctx, o.qualificationPath, root, rosterLimit, false)
	if err != nil {
		return err
	}
	qualificationSum := sha256.Sum256(qualification)
	for _, request := range template.Requests {
		if !bytes.Equal(request.LockQualificationSha256, qualificationSum[:]) {
			return errors.New("真实I3原件SHA与Request12不符，不发启动能力")
		}
	}
	if err = checkOutputPaths(o.bootOutput, o.runtimeOutput); err != nil {
		return err
	}
	if _, err = selectedRuntimeConfig(ctx, template, root); err != nil {
		return err
	}
	if len(template.RuntimeEnvironment) > 0 && o.runtimeEnvironmentOutput == "" {
		return errors.New("正式roster环境输入没有固定私有输出目录")
	}
	if _, err = readRuntimeEnvironment(ctx, template, root); err != nil {
		return err
	}
	for _, variable := range template.RuntimeEnvironment {
		if err = checkOutputPaths(filepath.Join(o.runtimeEnvironmentOutput, variable.Name), ""); err != nil {
			return err
		}
	}
	opener, err := newCatalogOpener(ctx, "MMORPG_SQL_REGISTRAR_DSN", physical, getenv)
	if err != nil {
		return err
	}
	defer opener.close()
	registrar := slotRegistration{
		bootOutput:   o.bootOutput,
		deploymentID: o.deploymentID,
		physical:     physical,
		open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (registrationCatalog, error) {
			return opener.open(request)
		},
		identity:   func(ctx context.Context) (string, error) { return registrarIdentity(ctx, getenv) },
		loadRecord: loadBootRecord,
		saveRecord: saveBootRecord,
		now:        time.Now,
		random:     rand.Reader,
	}
	set, boot, err := registrar.register(ctx, template)
	if err != nil {
		return err
	}
	config, err := selectedRuntimeConfig(ctx, template, root)
	if err != nil {
		return errors.New("选中配置在提交前变化")
	}
	environment, err := readRuntimeEnvironment(ctx, template, root)
	if err != nil {
		return err
	}
	if err = writeConfirmedBytes(o.runtimeOutput, config); err != nil {
		return err
	}
	for _, variable := range template.RuntimeEnvironment {
		if err = writeConfirmedBytes(filepath.Join(o.runtimeEnvironmentOutput, variable.Name), environment[variable.Name]); err != nil {
			return err
		}
	}
	if err = writeConfirmedGrantSet(o.bootOutput, set); err != nil {
		return err
	}
	for _, request := range boot.Requests {
		if _, err = fmt.Fprintf(out, "writer_authority command=register request_id=%s boot_seq=%d confirmed=true\n", hex.EncodeToString(request.RequestId), request.Writer.BootSeq); err != nil {
			return err
		}
	}
	return nil
}

// physicalAuthoritySHA 读部署固定的物理 authority 原件 SHA-256(32 字节)。
func physicalAuthoritySHA(getenv func(string) string) ([]byte, error) {
	physical, err := hex.DecodeString(getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 {
		return nil, errors.New("缺独立物理authority原件SHA")
	}
	return physical, nil
}

// loadSlotRoster 读已批准的槽位名册并核对:原字节 SHA、格式与槽位数、每个槽位的模板(register 与 retire-removed-slots 共用)。
func loadSlotRoster(ctx context.Context, rosterPath, rosterSHA, root string, physical []byte) (*dbpb.SqlWriterBootRoster, error) {
	raw, err := readBoundedInput(ctx, rosterPath, root, rosterLimit, false)
	if err != nil {
		return nil, err
	}
	approved, decodeErr := hex.DecodeString(rosterSHA)
	sum := sha256.Sum256(raw)
	if decodeErr != nil || len(approved) != 32 || !bytes.Equal(sum[:], approved) {
		return nil, errors.New("the roster does not match its approved SHA-256")
	}
	roster := &dbpb.SqlWriterBootRoster{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, roster); err != nil || roster.Format != slotRosterFormat || len(roster.Entries) == 0 || len(roster.Entries) > rosterSlotLimit {
		return nil, fmt.Errorf("the input is not a %s roster with 1..%d slots", slotRosterFormat, rosterSlotLimit)
	}
	if err = validateSlotRoster(roster, physical); err != nil {
		return nil, err
	}
	return roster, nil
}

// selectedRuntimeConfig 读这个槽位的完整运行配置,原字节 SHA 要和名册里的一致。
func selectedRuntimeConfig(ctx context.Context, entry *dbpb.SqlWriterBootRoster_Entry, root string) ([]byte, error) {
	body, err := readBoundedInput(ctx, entry.RuntimeConfigPath, root, runtimeConfigLimit, true)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], entry.RuntimeConfigSha256) {
		return nil, errors.New("roster完整运行配置原bytes SHA不符")
	}
	return body, nil
}

// runLifecycle 是 retire 与 status:只有单 Request 形态,逐逻辑库做。
func runLifecycle(ctx context.Context, command, requestPath, inputRoot, receiptOutput string, getenv func(string) string, out io.Writer) error {
	if !filepath.IsAbs(requestPath) {
		return errors.New("Request输入必须绝对路径")
	}
	if command == "retire" && receiptOutput == "" {
		return errors.New("RETIRE必须指定原请求尝试及正式结局的私有receipt-output")
	}
	if receiptOutput != "" {
		if !filepath.IsAbs(receiptOutput) {
			return errors.New("收据输出必须绝对路径")
		}
		if err := checkOutputPaths(receiptOutput, ""); err != nil {
			return err
		}
	}
	if inputRoot == "" {
		inputRoot = filepath.Dir(requestPath)
	}
	root, err := resolveInputRoot(inputRoot)
	if err != nil {
		return err
	}
	raw, err := readBoundedInput(ctx, requestPath, root, requestLimit, false)
	if err != nil {
		return err
	}
	request := &dbpb.SqlWriterLifecycleReceipt_Request{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, request); err != nil {
		return errors.New("输入不是当前正式Request protojson")
	}
	physical, err := hex.DecodeString(getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 {
		return errors.New("缺独立物理authority原件SHA")
	}
	if err = validateRequestIdentity(request, command, physical); err != nil {
		return err
	}
	dsnName := "MMORPG_SQL_REGISTRAR_DSN"
	if command == "retire" {
		dsnName = "MMORPG_SQL_MAINTENANCE_DSN"
	}
	opener, err := newCatalogOpener(ctx, dsnName, physical, getenv)
	if err != nil {
		return err
	}
	defer opener.close()
	catalog, err := opener.open(request)
	if err != nil {
		return err
	}
	var receipt *dbpb.SqlWriterLifecycleReceipt
	if command == "retire" {
		fresh, attemptErr := recordAttempt(receiptOutput+".retire.started.protojson", request)
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
	if err = writeLifecycleReceipt(receiptOutput, receipt); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "writer_authority command=%s request_id=%s confirmed=true\n", command, hex.EncodeToString(request.RequestId))
	return err
}

// catalogOpener 按逻辑库各开一条控制连接,同一个库复用。闸门在各逻辑库自己身上;同集群共用一条控制连接串,
// 库搬到别的集群时地址按映射给(dbplacement),账号在目标集群提前建好。
type catalogOpener struct {
	ctx       context.Context
	dsn       string
	physical  []byte
	principal string
	backend   authoritysql.Backend
	placement dbplacement.Config
	perm      authoritysql.Permission
	catalogs  map[string]*authoritysql.Catalog
	pools     []*sql.DB
}

func newCatalogOpener(ctx context.Context, dsnName string, physical []byte, getenv func(string) string) (*catalogOpener, error) {
	permission, err := authoritysql.LoadOperatorPermission(getenv)
	if err != nil {
		return nil, err
	}
	dsn := getenv(dsnName)
	if dsn == "" {
		return nil, errors.New("缺本命令独立控制DSN")
	}
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return nil, err
	}
	placementConfig, err := dbplacement.ConfigFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	return &catalogOpener{ctx: ctx, dsn: dsn, physical: bytes.Clone(physical), principal: getenv("MMORPG_SQL_OPERATOR_PRINCIPAL"), backend: backend,
		placement: placementConfig, perm: permission, catalogs: make(map[string]*authoritysql.Catalog)}, nil
}

func (o *catalogOpener) open(request *dbpb.SqlWriterLifecycleReceipt_Request) (*authoritysql.Catalog, error) {
	schema := request.AuthoritySchema
	if catalog, ok := o.catalogs[schema]; ok {
		return catalog, nil
	}
	schemaDSN, err := authoritysql.DSNForSchema(o.dsn, schema)
	if err != nil {
		return nil, err
	}
	placementOptions, err := dbplacement.ForOneShot(o.ctx, o.placement, schema)
	if err != nil {
		return nil, err
	}
	db, err := authoritysql.Open(o.ctx, schemaDSN, schema, o.backend, 2, o.perm, placementOptions...)
	if err != nil {
		return nil, err
	}
	o.pools = append(o.pools, db)
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: schema, Generation: request.Writer.Generation, ProtectedSchemas: request.ProtectedSchemas, PhysicalAuthoritySHA256: o.physical, Principal: o.principal})
	if err != nil {
		return nil, err
	}
	o.catalogs[schema] = catalog
	return catalog, nil
}

func (o *catalogOpener) close() {
	for _, db := range o.pools {
		_ = db.Close()
	}
}

// registrationCatalog 是注册一代用到的目录操作,生产实现 *authoritysql.Catalog。
type registrationCatalog interface {
	LatestBoot(ctx context.Context, principal []byte) (*dbpb.SqlWriterRegistry, error)
	RegistrationReady(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) error
	Register(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error)
	CurrentMeta(ctx context.Context) (*dbpb.RouteMeta, error)
}

// slotRegistration 把名册里的一个槽位注册成这个 Pod 的一代(writer-roster-exhaustion-20261106.md §5.3)。
// 一代在每个逻辑库各注册一份,不在一个事务里,结局这样分:
//
//	预检时冻结或不符     什么都没写,不写启动记录;init 退出,下次重新生成这一代。
//	提交时冻结           事务已回滚,确定没写;启动记录留着,init 重跑重发同样的请求。
//	部分库已注册         init 重跑读启动记录,还是同一代;已注册的库收据在,Register 原样返回,没注册的接着注册。
//	结局未知             同上,原样重发:Register 在锁内先查收据,boot_seq CAS 挡住陈旧请求,同一请求最多落地一次。
//	Pod 没了             同序号的新 Pod 是下一代,在每个库的注册事务里把这一代退休,不用人工。
type slotRegistration struct {
	bootOutput   string
	deploymentID string
	physical     []byte
	open         func(*dbpb.SqlWriterLifecycleReceipt_Request) (registrationCatalog, error)
	// identity 回登记这一代的工作负载身份(registered_by):K8s 里是本 Pod 的 ServiceAccount 连同 UID,集群外是空串(registrarIdentity)。
	// 只在现场生成一代时调一次;init 重跑读启动记录,不再问。
	identity   func(ctx context.Context) (string, error)
	loadRecord func(path string) (*dbpb.SqlWriterBootRoster_Entry, error)
	saveRecord func(path string, entry *dbpb.SqlWriterBootRoster_Entry) error
	now        func() time.Time
	random     io.Reader
}

// bootRecordPath 是本 Pod 的启动记录:和 grant 在同一个私有 emptyDir,Pod 在它就在,Pod 换了它跟着没。
func bootRecordPath(bootOutput string) string { return bootOutput + ".register.started.protojson" }

// register 注册这一代的全部逻辑库,回写资格集合与这一代的完整请求。
func (r slotRegistration) register(ctx context.Context, template *dbpb.SqlWriterBootRoster_Entry) (*dbpb.SqlWriterBootGrantSet, *dbpb.SqlWriterBootRoster_Entry, error) {
	boot, fresh, err := r.boot(ctx, template)
	if err != nil {
		return nil, nil, err
	}
	// 第一份写下去之前逐库预检,把注定失败的情况挡在写任何东西之前。
	for _, request := range boot.Requests {
		catalog, err := r.open(request)
		if err != nil {
			return nil, nil, err
		}
		if err := catalog.RegistrationReady(ctx, request); err != nil {
			return nil, nil, fmt.Errorf("logical database %s cannot register boot %d of this slot yet: %w", request.AuthoritySchema, request.Writer.BootSeq, err)
		}
	}
	// 先记下这一代再注册:之后 init 重跑都重发同样的请求,不再生成新身份。
	if fresh {
		if err := r.saveRecord(bootRecordPath(r.bootOutput), boot); err != nil {
			return nil, nil, err
		}
	}
	set := &dbpb.SqlWriterBootGrantSet{Format: authoritysql.GrantSetFormat}
	for _, request := range boot.Requests {
		catalog, err := r.open(request)
		if err != nil {
			return nil, nil, err
		}
		receipt, err := catalog.Register(ctx, request)
		if authoritysql.IsWriterFrozen(err) {
			return nil, nil, fmt.Errorf("the writer gate of %s froze during registration, nothing was written for it, retry: %w", request.AuthoritySchema, err)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("register boot %d of this slot in %s: %w", request.Writer.BootSeq, request.AuthoritySchema, err)
		}
		if err = confirmLifecycle(receipt, request); err != nil {
			return nil, nil, err
		}
		meta, err := catalog.CurrentMeta(ctx)
		if err != nil {
			return nil, nil, err
		}
		if meta.Writable != 1 {
			return nil, nil, fmt.Errorf("the writer gate of %s is frozen, retry: %w", request.AuthoritySchema, authoritysql.ErrWriterFrozen)
		}
		if meta.Generation != request.Writer.Generation || !bytes.Equal(meta.FreezeRunId, request.MaintenanceRunId) {
			return nil, nil, fmt.Errorf("the registration in %s is confirmed but the gate generation or maintenance run changed since, no grant is written", request.AuthoritySchema)
		}
		set.Grants = append(set.Grants, bootGrant(request, r.physical))
	}
	return set, boot, nil
}

// boot 回本 Pod 的这一代:启动记录在就用它(必须是同一槽位模板、本 Pod 生成的),不在就现场生成,fresh=true。
// 新一代的 boot_seq 取各库这个账号已有的最大号 + 1 里最大的那个,各库同一个号。
func (r slotRegistration) boot(ctx context.Context, template *dbpb.SqlWriterBootRoster_Entry) (*dbpb.SqlWriterBootRoster_Entry, bool, error) {
	record, err := r.loadRecord(bootRecordPath(r.bootOutput))
	if err == nil {
		if err = recordMatchesTemplate(record, template, r.deploymentID); err != nil {
			return nil, false, err
		}
		return record, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	bootSeq := uint64(1)
	for _, request := range template.Requests {
		catalog, err := r.open(request)
		if err != nil {
			return nil, false, err
		}
		latest, err := catalog.LatestBoot(ctx, request.Writer.SqlPrincipal)
		if err != nil {
			return nil, false, err
		}
		if latest == nil || latest.BootSeq < bootSeq {
			continue
		}
		if latest.BootSeq == math.MaxUint64 {
			return nil, false, errors.New("the boot sequence of this slot is exhausted")
		}
		bootSeq = latest.BootSeq + 1
	}
	registeredBy, err := r.identity(ctx)
	if err != nil {
		return nil, false, err
	}
	minted, err := mintBoot(template, r.deploymentID, registeredBy, bootSeq, r.now(), r.random)
	if err != nil {
		return nil, false, err
	}
	return minted, true, nil
}

// mintBoot 按槽位模板生成一代:writer_key、instance_id 每代新,各库同一个 boot_seq,request_id 每库一个,然后规范化出注册摘要。
// registeredBy 是登记这一代的工作负载身份,各库相同,算进摘要:登记之后改不了(缩容收尾按它判归属,§5.9)。
func mintBoot(template *dbpb.SqlWriterBootRoster_Entry, deploymentID, registeredBy string, bootSeq uint64, now time.Time, random io.Reader) (*dbpb.SqlWriterBootRoster_Entry, error) {
	writerKey, err := randomID(random)
	if err != nil {
		return nil, err
	}
	instanceID, err := randomID(random)
	if err != nil {
		return nil, err
	}
	boot := proto.Clone(template).(*dbpb.SqlWriterBootRoster_Entry)
	for index, request := range boot.Requests {
		if request.RequestId, err = randomID(random); err != nil {
			return nil, err
		}
		request.DeploymentId = deploymentID
		request.RegisteredBy = registeredBy
		request.Writer.WriterKey = bytes.Clone(writerKey)
		request.Writer.InstanceId = bytes.Clone(instanceID)
		request.Writer.RegisteredMs = uint64(now.UnixMilli())
		request.Writer.BootSeq = bootSeq
		canonical, err := authoritysql.CanonicalRegistrationRequest(request)
		if err != nil {
			return nil, err
		}
		boot.Requests[index] = canonical
	}
	if err = validateRosterEntry(boot); err != nil {
		return nil, err
	}
	return boot, nil
}

func randomID(random io.Reader) ([]byte, error) {
	id := make([]byte, 16)
	if _, err := io.ReadFull(random, id); err != nil {
		return nil, err
	}
	return id, nil
}

// stripIdentity 去掉注册器现场生成的字段,剩下的就是名册里的模板。
func stripIdentity(request *dbpb.SqlWriterLifecycleReceipt_Request) *dbpb.SqlWriterLifecycleReceipt_Request {
	stripped := proto.Clone(request).(*dbpb.SqlWriterLifecycleReceipt_Request)
	stripped.RequestId = nil
	stripped.DeploymentId = ""
	stripped.RegisteredBy = ""
	if stripped.Writer != nil {
		stripped.Writer.WriterKey = nil
		stripped.Writer.InstanceId = nil
		stripped.Writer.RegistrationDigest = nil
		stripped.Writer.RegisteredMs = 0
		stripped.Writer.BootSeq = 0
	}
	return stripped
}

// recordMatchesTemplate 核启动记录就是这份模板在本 Pod 上生成的一代:去掉身份字段之后逐字相同,部署是本 Pod,
// 各库身份一致,每份请求都是规范化过的原件。名册换了就会换 Pod(名册 SHA 在 Pod 模板里),对不上说明记录不是这个 Pod 的。
func recordMatchesTemplate(record, template *dbpb.SqlWriterBootRoster_Entry, deploymentID string) error {
	refuse := errors.New("the boot record of this pod does not belong to this roster slot and pod")
	if record == nil || len(record.Requests) != len(template.Requests) {
		return refuse
	}
	outer, wanted := proto.Clone(record).(*dbpb.SqlWriterBootRoster_Entry), proto.Clone(template).(*dbpb.SqlWriterBootRoster_Entry)
	outer.Requests, wanted.Requests = nil, nil
	if !proto.Equal(outer, wanted) {
		return refuse
	}
	first := record.Requests[0]
	for index, request := range record.Requests {
		if request == nil || request.Writer == nil || first.Writer == nil || request.DeploymentId != deploymentID {
			return refuse
		}
		if !bytes.Equal(request.Writer.WriterKey, first.Writer.WriterKey) || !bytes.Equal(request.Writer.InstanceId, first.Writer.InstanceId) || request.Writer.BootSeq != first.Writer.BootSeq || request.RegisteredBy != first.RegisteredBy {
			return refuse
		}
		canonical, err := authoritysql.CanonicalRegistrationRequest(request)
		if err != nil || !proto.Equal(canonical, request) || !proto.Equal(stripIdentity(request), template.Requests[index]) {
			return refuse
		}
	}
	return nil
}

// loadBootRecord 读本 Pod 的启动记录;还没有时回 os.ErrNotExist。
func loadBootRecord(path string) (*dbpb.SqlWriterBootRoster_Entry, error) {
	raw, err := authoritysql.ReadPrivateControlFile(path, bootRecordLimit)
	if err != nil {
		return nil, err
	}
	entry := &dbpb.SqlWriterBootRoster_Entry{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, entry); err != nil {
		return nil, errors.New("the boot record of this pod is not a roster entry")
	}
	return entry, nil
}

// saveBootRecord 独占写启动记录,已经有了就失败,不覆盖。
func saveBootRecord(path string, entry *dbpb.SqlWriterBootRoster_Entry) error {
	body, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(entry)
	if err != nil {
		return err
	}
	return writePrivateExclusive(path, body)
}

// bootGrant 是一份已确认注册在本 Pod 上的写资格。
func bootGrant(request *dbpb.SqlWriterLifecycleReceipt_Request, physical []byte) *dbpb.SqlWriterBootGrant {
	return &dbpb.SqlWriterBootGrant{Format: "SQL_WRITER_BOOT_V1", WriterKey: bytes.Clone(request.Writer.WriterKey), InstanceId: bytes.Clone(request.Writer.InstanceId), Generation: request.Writer.Generation,
		AuthoritySchema: request.AuthoritySchema, ProtectedSchemas: append([]string(nil), request.ProtectedSchemas...), SqlPrincipal: bytes.Clone(request.Writer.SqlPrincipal),
		RegistrationDigest: bytes.Clone(request.Writer.RegistrationDigest), DeploymentId: request.DeploymentId, GrantId: bytes.Clone(request.RequestId), GrantedMs: request.Writer.RegisteredMs,
		Permission: "NORMAL", PhysicalAuthoritySha256: bytes.Clone(physical), MaintenanceRunId: bytes.Clone(request.MaintenanceRunId), LockPrimitive: request.LockPrimitive,
		LockQualificationSha256: bytes.Clone(request.LockQualificationSha256)}
}

// validateSlotRoster 核整份槽位名册:每个槽位都是合法模板,各槽位账号互不相同,写同一组逻辑库。
// 两个槽位共用一个账号会互相换代,谁都写不稳,所以这里就拒绝。
func validateSlotRoster(roster *dbpb.SqlWriterBootRoster, physical []byte) error {
	principals := make(map[string]bool, len(roster.Entries))
	shape := ""
	for index, entry := range roster.Entries {
		if err := validateSlotEntry(entry, physical); err != nil {
			return fmt.Errorf("slot %d of the roster: %w", index, err)
		}
		principal := string(entry.Requests[0].Writer.SqlPrincipal)
		if principals[principal] {
			return fmt.Errorf("slot %d of the roster reuses the SQL account of another slot", index)
		}
		principals[principal] = true
		if current := rosterEntryShape(entry); shape == "" {
			shape = current
		} else if current != shape {
			return errors.New("every slot of a roster must write the same logical databases with the same generation and maintenance run")
		}
	}
	return nil
}

// validateSlotEntry 核一个槽位:每个要写的逻辑库一份 REGISTER 模板,同一账号、库不重复,带完整运行配置的路径和 SHA。
func validateSlotEntry(entry *dbpb.SqlWriterBootRoster_Entry, physical []byte) error {
	if entry == nil || len(entry.Requests) == 0 || len(entry.Requests) > bootRequestLimit {
		return errors.New("a slot needs one REGISTER template per logical database it writes")
	}
	for _, request := range entry.Requests {
		if err := validateSlotTemplate(request, physical); err != nil {
			return err
		}
	}
	if err := validateRosterEntry(entry); err != nil {
		return err
	}
	if entry.RuntimeConfigPath == "" || len(entry.RuntimeConfigSha256) != 32 {
		return errors.New("roster entry缺normal完整配置路径/SHA")
	}
	return nil
}

// validateSlotTemplate 核一份 REGISTER 模板:身份字段必须为空(注册器现场生成),其余固定字段齐全。
// REGISTER 不带 epoch 与目录版本:名册是预先渲染的,cutover 和回滚都会推进 epoch(规格 §2)。
func validateSlotTemplate(request *dbpb.SqlWriterLifecycleReceipt_Request, physical []byte) error {
	if request == nil || request.Writer == nil || request.Kind != dbpb.SqlWriterLifecycleReceipt_REGISTER {
		return errors.New("a slot carries a request that is not a REGISTER template")
	}
	writer := request.Writer
	if len(request.RequestId) != 0 || request.DeploymentId != "" || request.RegisteredBy != "" || len(writer.WriterKey) != 0 || len(writer.InstanceId) != 0 || len(writer.RegistrationDigest) != 0 || writer.RegisteredMs != 0 || writer.BootSeq != 0 {
		return errors.New("a REGISTER template must leave request_id, deployment_id, registered_by, writer_key, instance_id, registration_digest, registered_ms and boot_seq empty: the registrar makes them for each boot")
	}
	if request.ExpectedEpoch != 0 || request.ExpectedCatalogRevision != 0 || len(request.SuccessorRequestId) != 0 || writer.State != dbpb.SqlWriterRegistry_ACTIVE || len(writer.RetirementRequestId) != 0 || len(writer.RetirementDigest) != 0 || writer.RetiredMs != 0 {
		return errors.New("a REGISTER template carries epoch, catalog revision, successor or retirement fields")
	}
	if len(request.DeploymentDigest) != 32 || !bytes.Equal(request.DeploymentDigest, physical) {
		return errors.New("生命周期原件缺固定身份或独立物理authority摘要")
	}
	// 闸门只保护它所在的逻辑库。
	if len(request.ProtectedSchemas) != 1 || request.ProtectedSchemas[0] != request.AuthoritySchema {
		return errors.New("生命周期原件的受保护库必须恰好是闸门所在的逻辑库")
	}
	principal := string(writer.SqlPrincipal)
	if principal == "" || len(principal) > 288 || strings.ContainsAny(principal, "\x00\r\n") || writer.Generation == "" {
		return errors.New("a REGISTER template needs the SQL account of the slot and the generation of the logical database")
	}
	if request.LockPrimitive != dbpb.SqlWriterBootGrant_SELECT_ONLY && request.LockPrimitive != dbpb.SqlWriterBootGrant_LOCK_TOUCH {
		return errors.New("未选定已资格锁原语")
	}
	if len(request.LockQualificationSha256) != 32 || len(request.MaintenanceRunId) != 0 && len(request.MaintenanceRunId) != 16 {
		return errors.New("a REGISTER template needs the lock qualification SHA-256 and at most one 16-byte maintenance run")
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

// validateRequestIdentity 是 retire 与 status 的单 Request 输入核对:身份、物理 authority、epoch 与命令相符。注册只走槽位名册。
func validateRequestIdentity(request *dbpb.SqlWriterLifecycleReceipt_Request, command string, physical []byte) error {
	if request == nil || request.Writer == nil || len(request.RequestId) != 16 || len(request.Writer.WriterKey) != 16 || len(request.Writer.InstanceId) != 16 || request.Writer.RegisteredMs == 0 || request.Writer.BootSeq == 0 || len(request.DeploymentDigest) != 32 || !bytes.Equal(request.DeploymentDigest, physical) {
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
	if command == "retire" && request.Kind != dbpb.SqlWriterLifecycleReceipt_RETIRE {
		return errors.New("命令与原件kind不符")
	}
	if len(request.Writer.RegistrationDigest) != 32 {
		return errors.New("查询/退休须绑定原REGISTER摘要")
	}
	if command == "retire" && request.ExpectedCatalogRevision == 0 {
		return errors.New("RETIRE须绑定非零current catalog revision")
	}
	// 换代退休只由新一代的注册事务写,不从命令行发起。
	if command == "retire" && len(request.SuccessorRequestId) != 0 {
		return errors.New("a supersession RETIRE is written only by the registration of the next boot, not by the retire command")
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
