package main

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

const (
	maxInputBytes  = int64(1 << 20)
	maxDSNBytes    = int64(64 << 10)
	maxOutputBytes = 16 << 20
	maxRows        = 100000
	maxColumns     = 1024
	maxCellBytes   = 8 << 20
)

// 私有正文和驱动原始错误都不能走普通日志。
type quietLogger struct{}

func (quietLogger) Print(...any) {}

type failure struct {
	Stage    string   `json:"stage"`
	Class    string   `json:"class"`
	Code     uint16   `json:"code,omitempty"`
	SQLState string   `json:"sql_state,omitempty"`
	Cleanup  *failure `json:"cleanup,omitempty"`
}

func (f *failure) Error() string        { return "Controlled SQL file execution failed" }
func fail(stage, class string) *failure { return &failure{Stage: stage, Class: class} }
func classify(stage string, err error) *failure {
	if err == nil {
		return nil
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		state := string(my.SQLState[:])
		for _, c := range state {
			if c < '0' || c > '9' && c < 'A' || c > 'Z' {
				state = ""
				break
			}
		}
		return &failure{Stage: stage, Class: "mysql", Code: my.Number, SQLState: state}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fail(stage, "deadline")
	case errors.Is(err, context.Canceled):
		return fail(stage, "canceled")
	case errors.Is(err, driver.ErrBadConn):
		return fail(stage, "bad_connection")
	case errors.Is(err, driver.ErrSkip):
		return fail(stage, "unsupported_driver_operation")
	default:
		return fail(stage, "unconfirmed_io_or_contract")
	}
}

type options struct {
	dsnFile, sqlFile, sqlSHA, paramsFile, endpoint                      string
	database, mode, outputFile, expectedPrincipal, credentialOutputFile string
	timeout                                                             time.Duration
}

type result struct {
	Mode               string      `json:"mode"`
	SQLSHA256          string      `json:"sql_sha256"`
	TargetOutcome      string      `json:"target_outcome"`
	Principal          string      `json:"principal,omitempty"`
	Columns            []string    `json:"columns"`
	Rows               [][]*string `json:"rows"`
	AffectedRows       *string     `json:"affected_rows,omitempty"`
	CredentialsWritten bool        `json:"credentials_written"`
	Error              *failure    `json:"error,omitempty"`
	CleanupErrors      []*failure  `json:"cleanup_errors,omitempty"`
}

func checkBudget(ctx context.Context, stage string) *failure { return classify(stage, ctx.Err()) }
func privatePath(path string) *failure {
	if !filepath.IsAbs(path) {
		return fail("inputs", "absolute_path_required")
	}
	return classify("private_directory", authoritysql.CheckPrivateControlDirectory(filepath.Dir(filepath.Clean(path))))
}
func absent(path string) *failure {
	_, err := os.Lstat(path)
	if err == nil {
		return fail("outputs", "existing_output_or_attempt")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return classify("outputs", err)
	}
	return nil
}
func readPrivate(ctx context.Context, path string, size int64, stage string) ([]byte, *failure) {
	if f := checkBudget(ctx, stage); f != nil {
		return nil, f
	}
	b, err := authoritysql.ReadPrivateControlFile(path, size)
	if err != nil {
		return nil, classify(stage, err)
	}
	if f := checkBudget(ctx, stage); f != nil {
		return nil, f
	}
	return b, nil
}
func writePrivate(ctx context.Context, path string, body []byte, stage string) *failure {
	if f := checkBudget(ctx, stage); f != nil {
		return f
	}
	if err := authoritysql.WritePrivateControlFile(path, body); err != nil {
		return classify(stage, err)
	}
	return checkBudget(ctx, stage)
}

func parseOptions(args []string) (options, *failure) {
	var o options
	f := flag.NewFlagSet("sql_file_surface", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.Usage = func() {}
	f.StringVar(&o.endpoint, "endpoint", "127.0.0.1:24000", "Explicit canonical loopback endpoint")
	f.StringVar(&o.dsnFile, "dsn-file", "", "私有Go DSN文件")
	f.StringVar(&o.sqlFile, "sql-file", "", "私有单条SQL文件")
	f.StringVar(&o.sqlSHA, "sql-sha256", "", "SQL原字节摘要")
	f.StringVar(&o.paramsFile, "params-file", "", "可选私有字符串或null数组")
	f.StringVar(&o.database, "database", "", "明确选择的库，可空")
	f.StringVar(&o.mode, "mode", "", "exec或query")
	f.StringVar(&o.outputFile, "output-file", "", "全新私有结果文件")
	f.StringVar(&o.expectedPrincipal, "expected-principal", "", "可选精确CURRENT_USER")
	f.StringVar(&o.credentialOutputFile, "credential-output-file", "", "身份探针后私有User和Password映射")
	f.DurationVar(&o.timeout, "timeout", 30*time.Second, "含输入和结果写入的总预算，最多60s")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return o, fail("flags", "invalid_arguments")
	}
	if !validEndpoint(o.endpoint) {
		return o, fail("flags", "invalid_endpoint")
	}
	if o.timeout <= 0 || o.timeout > 60*time.Second || o.mode != "exec" && o.mode != "query" {
		return o, fail("flags", "invalid_mode_or_timeout")
	}
	if o.dsnFile == "" || o.sqlFile == "" || o.outputFile == "" {
		return o, fail("flags", "missing_path")
	}
	if o.database != "" {
		if len(o.database) > 64 {
			return o, fail("flags", "invalid_database")
		}
		for _, c := range o.database {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return o, fail("flags", "invalid_database")
			}
		}
	}
	if !utf8.ValidString(o.expectedPrincipal) || len(o.expectedPrincipal) > 512 || strings.ContainsRune(o.expectedPrincipal, 0) {
		return o, fail("flags", "invalid_principal")
	}
	pin, err := hex.DecodeString(o.sqlSHA)
	if err != nil || len(pin) != sha256.Size {
		return o, fail("flags", "invalid_sql_sha256")
	}
	paths := []string{o.dsnFile, o.sqlFile, o.outputFile, o.outputFile + ".started"}
	if o.paramsFile != "" {
		paths = append(paths, o.paramsFile)
	}
	if o.credentialOutputFile != "" {
		paths = append(paths, o.credentialOutputFile)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if f := privatePath(p); f != nil {
			return o, f
		}
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] {
			return o, fail("paths", "overlapping_input_or_output")
		}
		seen[key] = true
	}
	for _, p := range []string{o.outputFile, o.outputFile + ".started", o.credentialOutputFile} {
		if p != "" {
			if f := absent(p); f != nil {
				return o, f
			}
		}
	}
	return o, nil
}

func readParams(ctx context.Context, path string) ([]driver.NamedValue, *failure) {
	if path == "" {
		return nil, nil
	}
	b, f := readPrivate(ctx, path, maxInputBytes, "params_read")
	if f != nil {
		return nil, f
	}
	if !utf8.Valid(b) || !strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
		return nil, fail("params_read", "string_or_null_array_required")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) > 1024 {
		return nil, fail("params_read", "invalid_params_array")
	}
	out := make([]driver.NamedValue, len(raw))
	for i, item := range raw {
		out[i].Ordinal = i + 1
		if string(item) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(item, &s); err != nil || !utf8.ValidString(s) {
			return nil, fail("params_read", "string_or_null_array_required")
		}
		out[i].Value = s
	}
	return out, nil
}

// 正式 Java fixture producer 固定使用此端点；不接受其他既有数据库。
func validEndpoint(endpoint string) bool {
	return endpoint == "127.0.0.1:24000"
}

func configure(ctx context.Context, raw []byte, database, endpoint string) (*mysql.Config, *failure) {
	if !validEndpoint(endpoint) {
		return nil, fail("dsn_parse", "invalid_endpoint")
	}
	if !utf8.Valid(raw) {
		return nil, fail("dsn_parse", "invalid_private_dsn")
	}
	cfg, err := mysql.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fail("dsn_parse", "invalid_private_dsn")
	}
	if cfg.Net != "tcp" || cfg.Addr != endpoint || cfg.User == "" {
		return nil, fail("dsn_parse", "target_not_allowed")
	}
	if cfg.MultiStatements || cfg.InterpolateParams || cfg.AllowAllFiles || cfg.AllowCleartextPasswords || cfg.AllowFallbackToPlaintext || cfg.AllowOldPasswords || cfg.RejectReadOnly || cfg.ClientFoundRows {
		return nil, fail("dsn_parse", "unsafe_option")
	}
	if len(cfg.Params) != 0 || cfg.ConnectionAttributes != "" || cfg.ServerPubKey != "" || cfg.TLS != nil || cfg.TLSConfig != "" && cfg.TLSConfig != "false" {
		return nil, fail("dsn_parse", "unsupported_session_or_transport_option")
	}
	cfg.DBName = database
	cfg.Params = nil
	cfg.MultiStatements = false
	cfg.InterpolateParams = false
	cfg.AllowAllFiles = false
	cfg.AllowCleartextPasswords = false
	cfg.AllowFallbackToPlaintext = false
	cfg.AllowOldPasswords = false
	cfg.RejectReadOnly = false
	cfg.ClientFoundRows = false
	cfg.ColumnsWithAlias = false
	cfg.ParseTime = false
	cfg.Loc = time.UTC
	cfg.MaxAllowedPacket = 16 << 20
	cfg.Logger = quietLogger{}
	// shared intentionally uses mysql v1.9.0, whose public API has no Charset
	// option. Round-trip the validated configuration with final fixed charset
	// options; ParseDSN applies the last occurrence, so caller values cannot win.
	encoded := cfg.FormatDSN()
	separator := "?"
	if strings.Contains(encoded, "?") {
		separator = "&"
	}
	cfg, err = mysql.ParseDSN(encoded + separator + "charset=utf8mb4&collation=utf8mb4_bin")
	if err != nil {
		return nil, fail("dsn_parse", "charset_rejected")
	}
	cfg.Logger = quietLogger{}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 {
		return nil, fail("dsn_parse", "deadline")
	}
	remaining := time.Until(deadline)
	cfg.Timeout = min(2*time.Second, remaining)
	cfg.ReadTimeout = remaining
	cfg.WriteTimeout = remaining
	return cfg, nil
}

func cellString(v driver.Value) (*string, *failure) {
	var s string
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, fail("rows", "non_utf8_value")
		}
		s = x
	case []byte:
		if !utf8.Valid(x) {
			return nil, fail("rows", "non_utf8_value_use_explicit_hex")
		}
		s = string(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case uint64:
		s = strconv.FormatUint(x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fail("rows", "non_finite_value")
		}
		s = strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		s = strconv.FormatBool(x)
	default:
		return nil, fail("rows", "unsupported_driver_value")
	}
	return &s, nil
}

func readRows(ctx context.Context, rows driver.Rows) (columns []string, data [][]*string, issue *failure) {
	if rows == nil {
		return nil, nil, fail("rows", "nil_driver_rows")
	}
	defer func() {
		if err := rows.Close(); err != nil {
			if issue == nil {
				issue = classify("rows_close", err)
			} else {
				issue.Cleanup = classify("rows_close", err)
			}
		}
	}()
	columns = append([]string(nil), rows.Columns()...)
	data = make([][]*string, 0)
	if len(columns) < 1 || len(columns) > maxColumns {
		return nil, nil, fail("rows", "column_limit")
	}
	bytesRead := 0
	for _, s := range columns {
		if !utf8.ValidString(s) {
			return nil, nil, fail("rows", "non_utf8_column")
		}
		bytesRead += len(s)
	}
	for {
		if f := checkBudget(ctx, "rows"); f != nil {
			return nil, nil, f
		}
		values := make([]driver.Value, len(columns))
		err := rows.Next(values)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, classify("rows", err)
		}
		if len(data) >= maxRows {
			return nil, nil, fail("rows", "row_limit")
		}
		// 空值也占内存，不能只按文本字节限制结果。
		bytesRead += len(values)*8 + 24
		if bytesRead > maxCellBytes {
			return nil, nil, fail("rows", "result_byte_limit")
		}
		r := make([]*string, len(values))
		for i, v := range values {
			s, f := cellString(v)
			if f != nil {
				return nil, nil, f
			}
			r[i] = s
			if s != nil {
				bytesRead += len(*s)
			}
		}
		if bytesRead > maxCellBytes {
			return nil, nil, fail("rows", "result_byte_limit")
		}
		data = append(data, r)
	}
	if more, ok := rows.(driver.RowsNextResultSet); ok && more.HasNextResultSet() {
		return nil, nil, fail("rows", "multiple_result_sets_rejected")
	}
	if f := checkBudget(ctx, "rows"); f != nil {
		return nil, nil, f
	}
	return columns, data, nil
}

func currentPrincipal(ctx context.Context, conn driver.Conn) (string, *failure) {
	q, ok := conn.(driver.QueryerContext)
	if !ok {
		return "", fail("identity", "query_context_unavailable")
	}
	rows, err := q.QueryContext(ctx, "SELECT CURRENT_USER()", nil)
	if err != nil {
		return "", classify("identity", err)
	}
	_, data, f := readRows(ctx, rows)
	if f != nil {
		return "", f
	}
	if len(data) != 1 || len(data[0]) != 1 || data[0][0] == nil || *data[0][0] == "" {
		return "", fail("identity", "unexpected_principal_result")
	}
	return *data[0][0], nil
}

// 只取得一次物理连接；不使用database/sql池，也不对ErrBadConn或ErrSkip重试。
func perform(ctx context.Context, cfg *mysql.Config, sqlText string, args []driver.NamedValue, o options) (out result, issue *failure) {
	out = result{Mode: o.mode, SQLSHA256: strings.ToLower(o.sqlSHA), TargetOutcome: "not_executed", Columns: []string{}, Rows: [][]*string{}}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return out, classify("connector", err)
	}
	conn, err := connector.Connect(ctx)
	if err != nil {
		return out, classify("connect", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			if issue == nil {
				issue = classify("connection_close", err)
			} else {
				out.CleanupErrors = append(out.CleanupErrors, classify("connection_close", err))
			}
		}
		if f := checkBudget(ctx, "connection_close"); f != nil && issue == nil {
			issue = f
		}
	}()
	out.Principal, issue = currentPrincipal(ctx, conn)
	if issue != nil {
		return out, issue
	}
	if o.expectedPrincipal != "" && out.Principal != o.expectedPrincipal {
		return out, fail("identity", "principal_mismatch")
	}
	if f := checkBudget(ctx, "target"); f != nil {
		return out, f
	}
	var execute func() (driver.Result, error)
	var query func() (driver.Rows, error)
	if len(args) == 0 {
		execer, ok := conn.(driver.ExecerContext)
		queryer, qok := conn.(driver.QueryerContext)
		if !ok || !qok {
			return out, fail("target", "context_driver_interface_unavailable")
		}
		execute = func() (driver.Result, error) { return execer.ExecContext(ctx, sqlText, nil) }
		query = func() (driver.Rows, error) { return queryer.QueryContext(ctx, sqlText, nil) }
	} else {
		prepare, ok := conn.(driver.ConnPrepareContext)
		if !ok {
			return out, fail("prepare", "prepare_context_unavailable")
		}
		stmt, err := prepare.PrepareContext(ctx, sqlText)
		if err != nil {
			return out, classify("prepare", err)
		}
		defer func() {
			if err := stmt.Close(); err != nil {
				if issue == nil {
					issue = classify("statement_close", err)
				} else {
					out.CleanupErrors = append(out.CleanupErrors, classify("statement_close", err))
				}
			}
		}()
		if stmt.NumInput() != len(args) {
			return out, fail("prepare", "parameter_count_mismatch")
		}
		execer, ok := stmt.(driver.StmtExecContext)
		queryer, qok := stmt.(driver.StmtQueryContext)
		if !ok || !qok {
			return out, fail("prepare", "statement_context_interface_unavailable")
		}
		execute = func() (driver.Result, error) { return execer.ExecContext(ctx, args) }
		query = func() (driver.Rows, error) { return queryer.QueryContext(ctx, args) }
	}
	if f := checkBudget(ctx, "target"); f != nil {
		return out, f
	}
	// 发出目标语句之后，只有完整应答和读取/关闭都确认才写confirmed。
	out.TargetOutcome = "unknown"
	if o.mode == "exec" {
		r, err := execute()
		if err != nil {
			return out, classify("target_exec", err)
		}
		if r == nil {
			return out, fail("affected_rows", "nil_driver_result")
		}
		n, err := r.RowsAffected()
		if err != nil || n < 0 {
			return out, fail("affected_rows", "unconfirmed_affected_rows")
		}
		value := strconv.FormatInt(n, 10)
		out.AffectedRows = &value
	} else {
		rows, err := query()
		if err != nil {
			return out, classify("target_query", err)
		}
		out.Columns, out.Rows, issue = readRows(ctx, rows)
		if issue != nil {
			return out, issue
		}
	}
	if f := checkBudget(ctx, "target_complete"); f != nil {
		return out, f
	}
	out.TargetOutcome = "confirmed"
	return out, nil
}

func run(args []string) *failure {
	started := time.Now()
	o, issue := parseOptions(args)
	if issue != nil {
		return issue
	}
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(o.timeout))
	defer cancel()
	if issue = checkBudget(ctx, "inputs"); issue != nil {
		return issue
	}
	// marker在任何连接前CreateNew；保留失败现场，不允许同输出名复活这次尝试。
	marker, _ := json.Marshal(map[string]any{"started": true, "mode": o.mode, "sql_sha256": strings.ToLower(o.sqlSHA)})
	if issue = writePrivate(ctx, o.outputFile+".started", marker, "attempt_create"); issue != nil {
		return issue
	}
	out := result{Mode: o.mode, SQLSHA256: strings.ToLower(o.sqlSHA), TargetOutcome: "not_executed", Columns: []string{}, Rows: [][]*string{}}
	var cfg *mysql.Config
	var sqlText string
	var params []driver.NamedValue
	body := func() *failure {
		raw, f := readPrivate(ctx, o.dsnFile, maxDSNBytes, "dsn_read")
		if f != nil {
			return f
		}
		defer clear(raw)
		cfg, f = configure(ctx, raw, o.database, o.endpoint)
		if f != nil {
			return f
		}
		sqlBytes, f := readPrivate(ctx, o.sqlFile, maxInputBytes, "sql_read")
		if f != nil {
			return f
		}
		sum := sha256.Sum256(sqlBytes)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), o.sqlSHA) {
			return fail("sql_read", "sql_sha256_mismatch")
		}
		if !utf8.Valid(sqlBytes) || strings.ContainsRune(string(sqlBytes), 0) || strings.TrimSpace(string(sqlBytes)) == "" {
			return fail("sql_read", "invalid_sql_bytes")
		}
		sqlText = string(sqlBytes)
		params, f = readParams(ctx, o.paramsFile)
		if f != nil {
			return f
		}
		if o.credentialOutputFile != "" {
			probe := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sqlText), ";"))
			if o.mode != "query" || len(params) != 0 || !strings.EqualFold(probe, "SELECT CURRENT_USER()") {
				return fail("credential_output", "identity_probe_required")
			}
		}
		var f2 *failure
		out, f2 = perform(ctx, cfg, sqlText, params, o)
		if f2 != nil {
			return f2
		}
		if o.credentialOutputFile != "" {
			// 凭据仅走现有私有控制文件，绝不进入普通结果或日志。
			credentials, err := json.Marshal(map[string]string{"User": cfg.User, "Password": cfg.Passwd})
			if err != nil {
				return fail("credential_output", "encoding_failed")
			}
			defer clear(credentials)
			if f := writePrivate(ctx, o.credentialOutputFile, credentials, "credential_output"); f != nil {
				return f
			}
			out.CredentialsWritten = true
		}
		return nil
	}
	issue = body()
	out.Error = issue
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > maxOutputBytes {
		return fail("result_output", "result_encoding_or_size")
	}
	// 同步NTFS API不可由context中断；前后都核同一截止，逾期退出1且原件留存。
	if f := writePrivate(ctx, o.outputFile, encoded, "result_output"); f != nil {
		return f
	}
	return issue
}

func main() {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "Controlled SQL file execution aborted; preserve private originals; do not retry.")
			os.Exit(1)
		}
	}()
	if run(os.Args[1:]) != nil {
		fmt.Fprintln(os.Stderr, "Controlled SQL file execution failed; preserve attempt marker and private originals; do not retry.")
		os.Exit(1)
	}
	fmt.Println("Controlled SQL file execution completed; results are in private files only.")
}
