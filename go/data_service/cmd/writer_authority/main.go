// writer_authority是独立部署入口；常驻服务不得获得本程序的控制凭据。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func run(parent context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("writer_authority", flag.ContinueOnError)
	command := flags.String("command", "", "register、retire、status或publication只读资格")
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
		entries = []*dbpb.SqlWriterBootRoster_Entry{{Request: request, RuntimeConfigPath: *runtimeConfig}}
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
	for _, entry := range entries {
		if entry == nil || entry.Request == nil {
			return errors.New("roster含空entry或正式Request")
		}
		request := proto.Clone(entry.Request).(*dbpb.SqlWriterLifecycleReceipt_Request)
		if err = validateRequestIdentity(request, *command, physical); err != nil {
			return err
		}
		if *command == "register" {
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
			entry.Request = request
			for _, pair := range []struct {
				values map[string]bool
				value  []byte
			}{{seenKeys, request.Writer.WriterKey}, {seenInstances, request.Writer.InstanceId}, {seenPrincipals, request.Writer.SqlPrincipal}, {seenRequests, request.RequestId}} {
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
		first := entries[0].Request
		if request.AuthoritySchema != first.AuthoritySchema || request.Writer.Generation != first.Writer.Generation || !equalStrings(request.ProtectedSchemas, first.ProtectedSchemas) || !bytes.Equal(request.MaintenanceRunId, first.MaintenanceRunId) {
			return errors.New("同roster必须固定同authority/generation/schema全集/恢复run")
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
	first := entries[0].Request
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return err
	}
	db, err := authoritysql.Open(ctx, dsn, first.AuthoritySchema, backend, 2, permission)
	if err != nil {
		return err
	}
	defer db.Close()
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: first.AuthoritySchema, Generation: first.Writer.Generation, ProtectedSchemas: first.ProtectedSchemas, PhysicalAuthoritySHA256: physical, Principal: getenv("MMORPG_SQL_OPERATOR_PRINCIPAL")})
	if err != nil {
		return err
	}
	for _, entry := range entries {
		request := entry.Request
		if err = ctx.Err(); err != nil {
			return err
		}
		var receipt *dbpb.SqlWriterLifecycleReceipt
		if *command == "register" {
			foreign, readErr := catalog.RegistrationConsumedByOtherDeployment(ctx, request)
			if readErr != nil {
				return readErr
			}
			if foreign {
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
			attempt := *bootOutput + "." + hex.EncodeToString(request.RequestId) + ".register.started.protojson"
			fresh, attemptErr := recordAttempt(attempt, request)
			if attemptErr != nil {
				return attemptErr
			}
			if fresh {
				receipt, err = catalog.Register(ctx, request)
			} else {
				// init重启仅查原请求。查无/失联仍UNKNOWN，不再次Register、不新key或新principal。
				receipt, err = catalog.LifecycleReceipt(ctx, request.RequestId)
				if err != nil {
					return errors.New("原注册尝试结局未定，只允许status原件查询")
				}
			}
			if err != nil {
				if errors.Is(err, authoritysql.ErrOutcomeUnknown) {
					return err
				}
				foreign, readErr = catalog.RegistrationConsumedByOtherDeployment(ctx, request)
				if readErr != nil {
					return readErr
				}
				if foreign {
					continue
				}
				return err
			}
			if err = confirmLifecycle(receipt, request); err != nil {
				foreign, readErr = catalog.RegistrationConsumedByOtherDeployment(ctx, request)
				if readErr != nil {
					return readErr
				}
				if foreign {
					continue
				}
				return err
			}
			meta, readErr := catalog.CurrentMeta(ctx)
			if readErr != nil {
				return readErr
			}
			if meta.Writable != 1 || meta.Generation != request.Writer.Generation || meta.WriterEpoch != request.ExpectedEpoch || !bytes.Equal(meta.FreezeRunId, request.MaintenanceRunId) {
				return errors.New("原REGISTER虽确认但当前boot资格已变，HOLD不输出grant")
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
			grant := &dbpb.SqlWriterBootGrant{Format: "SQL_WRITER_BOOT_V1", WriterKey: bytes.Clone(request.Writer.WriterKey), InstanceId: bytes.Clone(request.Writer.InstanceId), Generation: request.Writer.Generation, AuthoritySchema: request.AuthoritySchema, ProtectedSchemas: append([]string(nil), request.ProtectedSchemas...), SqlPrincipal: bytes.Clone(request.Writer.SqlPrincipal), RegistrationDigest: bytes.Clone(request.Writer.RegistrationDigest), DeploymentId: request.DeploymentId, GrantId: bytes.Clone(request.RequestId), GrantedMs: request.Writer.RegisteredMs, Permission: "NORMAL", PhysicalAuthoritySha256: bytes.Clone(physical), MaintenanceRunId: bytes.Clone(request.MaintenanceRunId), LockPrimitive: request.LockPrimitive, LockQualificationSha256: bytes.Clone(request.LockQualificationSha256)}
			if err = writeConfirmedGrant(*bootOutput, grant); err != nil {
				return err
			}
		} else {
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
		}
		if *receiptOutput != "" {
			raw, marshalErr := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(receipt)
			if marshalErr != nil {
				return marshalErr
			}
			if err = writeConfirmedBytes(*receiptOutput, raw); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(out, "writer_authority command=%s request_id=%s confirmed=true\n", *command, hex.EncodeToString(request.RequestId))
		return err
	}
	return errors.New("有限roster全部已由其它明确boot消费，不生成新key或principal")
}

func validateRequestIdentity(request *dbpb.SqlWriterLifecycleReceipt_Request, command string, physical []byte) error {
	if request == nil || request.Writer == nil || len(request.RequestId) != 16 || len(request.Writer.WriterKey) != 16 || len(request.Writer.InstanceId) != 16 || request.Writer.RegisteredMs == 0 || len(request.DeploymentDigest) != 32 || !bytes.Equal(request.DeploymentDigest, physical) || request.ExpectedEpoch == 0 {
		return errors.New("生命周期原件缺固定身份、epoch或独立物理authority摘要")
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
	if !bytes.Equal(receipt.RequestProto, raw) || !bytes.Equal(receipt.RequestDigest, expectedDigest) || receipt.Generation != request.Writer.Generation || receipt.PreviousEpoch != request.ExpectedEpoch || receipt.ResultingEpoch != request.ExpectedEpoch || receipt.CatalogRevision == 0 {
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
			principal := string(entry.Request.Writer.SqlPrincipal)
			at := strings.LastIndexByte(principal, '@')
			if at < 1 || dsn.User != principal[:at] {
				return nil, errors.New("选中SQL环境用户名与同entry永久principal不符")
			}
			protected := false
			for _, schema := range entry.Request.ProtectedSchemas {
				if dsn.DBName == schema {
					protected = true
					break
				}
			}
			if !protected {
				return nil, errors.New("选中DSN库不在固定protected schemas")
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

func writeConfirmedGrant(path string, grant *dbpb.SqlWriterBootGrant) error {
	body, err := authoritysql.ReadPrivateControlFile(path, requestLimit)
	if err == nil {
		old := &dbpb.SqlWriterBootGrant{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, old) != nil || !proto.Equal(old, grant) {
			return errors.New("原grant不是同一正式确认结局")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		body, err = (protojson.MarshalOptions{UseProtoNames: false}).Marshal(grant)
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
	db, err := authoritysql.Open(ctx, dsn, plan.AuthoritySchema, backend, 2, permission)
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
