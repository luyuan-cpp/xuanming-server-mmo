package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/argon2"
)

const (
	passwordAccountMaxRunes = 191
	passwordMaxBytes        = 1024

	// Argon2id 每次默认占用 64 MiB。默认只允许同一 login 进程同时跑 2 个，
	// 即约 128 MiB KDF 工作集；硬上限 8 防止错误配置制造并发 OOM。
	DefaultPasswordKDFConcurrency = 2
	MaxPasswordKDFConcurrency     = 8
	DefaultPasswordKDFWaitTimeout = 500 * time.Millisecond
	MaxPasswordKDFWaitTimeout     = 5 * time.Second

	defaultArgonMemory      = 64 * 1024
	defaultArgonIterations  = 3
	defaultArgonParallelism = 2
	defaultArgonSaltLength  = 16
	defaultArgonKeyLength   = 32
)

type argon2idParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

var defaultArgon2idParams = argon2idParams{
	memory:      defaultArgonMemory,
	iterations:  defaultArgonIterations,
	parallelism: defaultArgonParallelism,
	saltLength:  defaultArgonSaltLength,
	keyLength:   defaultArgonKeyLength,
}

var ErrPasswordKDFCapacity = errors.New("password KDF capacity unavailable")
var ErrPasswordCredentialUnavailable = errors.New("password credential unavailable")

// PasswordHashStore 是生产口令认证器唯一信任的账号存储接口。返回的
// canonicalAccount 必须来自权威库，不能原样回显客户端输入。
type PasswordHashStore interface {
	LookupPasswordHash(ctx context.Context, account string) (canonicalAccount, encodedHash string, found bool, err error)
}

// SQLPasswordHashStore 从 user_accounts.password 读取 Argon2id PHC 字符串。
// 这个存储只读；存量迁移走离线工具，注册/改密尚未开放，Login RPC 永不写口令。
type SQLPasswordHashStore struct {
	db *sql.DB
}

func NewSQLPasswordHashStore(dsn string, maxOpenConns, maxIdleConns int) (*SQLPasswordHashStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("password auth MySQL DSN is empty")
	}
	parsed, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("password auth MySQL DSN is invalid")
	}
	if parsed.DBName == "" {
		return nil, fmt.Errorf("password auth MySQL DSN must select a database")
	}

	db, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open password auth MySQL: %w", err)
	}
	if maxOpenConns <= 0 {
		maxOpenConns = 20
	}
	if maxIdleConns < 0 {
		maxIdleConns = 0
	}
	if maxIdleConns > maxOpenConns {
		maxIdleConns = maxOpenConns
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping password auth MySQL: %w", err)
	}
	return &SQLPasswordHashStore{db: db}, nil
}

func (s *SQLPasswordHashStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Stats 返回连接池统计,给 /metrics 的连接池收集器用(见 ProductionPasswordProvider.PoolStats);只读内存计数,不做 I/O。
func (s *SQLPasswordHashStore) Stats() sql.DBStats {
	if s == nil || s.db == nil {
		return sql.DBStats{}
	}
	return s.db.Stats()
}

func (s *SQLPasswordHashStore) LookupPasswordHash(ctx context.Context, account string) (string, string, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT account, password FROM user_accounts WHERE account = ? LIMIT 2", account)
	if err != nil {
		return "", "", false, fmt.Errorf("query password hash: %w", err)
	}
	defer rows.Close()

	var canonical string
	var encoded sql.NullString
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", "", false, fmt.Errorf("read password hash: %w", err)
		}
		return "", "", false, nil
	}
	if err := rows.Scan(&canonical, &encoded); err != nil {
		return "", "", false, fmt.Errorf("scan password hash: %w", err)
	}
	if rows.Next() {
		return "", "", false, fmt.Errorf("account %q has duplicate authoritative rows", account)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, fmt.Errorf("read password hash: %w", err)
	}
	return canonical, encoded.String, true, nil
}

// ProductionPasswordProvider 验证权威库中的 Argon2id 哈希。未知账号也执行
// 一次同成本的 Argon2id，以降低账号枚举的时序信号。
type ProductionPasswordProvider struct {
	store          PasswordHashStore
	quota          PasswordFailureQuota
	dummyHash      string
	kdfSlots       chan struct{}
	kdfWaitTimeout time.Duration
	verifyKDF      func(password, encoded string) (bool, error)
}

func NewProductionPasswordProvider(store PasswordHashStore, quota PasswordFailureQuota) (*ProductionPasswordProvider, error) {
	return NewProductionPasswordProviderWithLimits(
		store, quota, DefaultPasswordKDFConcurrency, DefaultPasswordKDFWaitTimeout)
}

func NewProductionPasswordProviderWithLimits(store PasswordHashStore, quota PasswordFailureQuota, concurrency int, waitTimeout time.Duration) (*ProductionPasswordProvider, error) {
	if nilPasswordDependency(store) || nilPasswordDependency(quota) {
		return nil, fmt.Errorf("production password auth requires a hash store and failure quota")
	}
	if concurrency == 0 {
		concurrency = DefaultPasswordKDFConcurrency
	}
	if concurrency < 1 || concurrency > MaxPasswordKDFConcurrency {
		return nil, fmt.Errorf("password KDF concurrency must be between 1 and %d", MaxPasswordKDFConcurrency)
	}
	if waitTimeout == 0 {
		waitTimeout = DefaultPasswordKDFWaitTimeout
	}
	if waitTimeout < 0 || waitTimeout > MaxPasswordKDFWaitTimeout {
		return nil, fmt.Errorf("password KDF wait timeout must be between 1ns and %s", MaxPasswordKDFWaitTimeout)
	}
	dummyHash, err := hashPasswordWithParams("invalid-account-dummy-password", defaultArgon2idParams)
	if err != nil {
		return nil, fmt.Errorf("build password timing dummy: %w", err)
	}
	return &ProductionPasswordProvider{
		store:          store,
		quota:          quota,
		dummyHash:      dummyHash,
		kdfSlots:       make(chan struct{}, concurrency),
		kdfWaitTimeout: waitTimeout,
		verifyKDF:      verifyArgon2id,
	}, nil
}

// PoolStats 返回口令库的连接池统计,给 /metrics 的连接池收集器用。存储不是 SQL 池(单测替身)时 ok=false。
// 只读内存计数,不做 I/O,可与 ValidatePassword 并发调用。
func (p *ProductionPasswordProvider) PoolStats() (stats sql.DBStats, ok bool) {
	pooled, ok := p.store.(interface{ Stats() sql.DBStats })
	if !ok {
		return sql.DBStats{}, false
	}
	return pooled.Stats(), true
}

func (p *ProductionPasswordProvider) ValidatePassword(ctx context.Context, account, password string) (*AuthResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPasswordKDFCapacity, err)
	}
	if ValidatePasswordAccount(account) != nil || password == "" || len(password) > passwordMaxBytes {
		if _, err := p.verifyWithLimit(ctx, "invalid-input-dummy-password", p.dummyHash); err != nil {
			return nil, err
		}
		return nil, ErrInvalidCredentials
	}

	canonical, encodedHash, found, err := p.store.LookupPasswordHash(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("password credential store unavailable: %w", err)
	}
	if found && canonical == "" {
		return nil, ErrPasswordCredentialUnavailable
	}
	kind, identity := KnownPasswordAccount, canonical
	if !found {
		kind, identity = UnknownPasswordAccount, account
	}
	attempt, err := p.quota.Begin(ctx, kind, identity)
	if err != nil {
		return nil, err
	}
	finishAttempted := false
	defer func() {
		if !finishAttempted {
			// 仅未进入结算的失败才释放租约。Finish 结果未知不能换成 abort；
			// 独立有界清理不继承已取消的父期限，失败则由 30s 租期恢复。
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), PasswordQuotaBudget)
			defer cancel()
			_ = p.quota.Finish(cleanup, attempt, PasswordAbort)
		}
	}()
	verifyHash := encodedHash
	if !found || encodedHash == "" {
		verifyHash = p.dummyHash
	}
	match, err := p.verifyWithLimit(ctx, password, verifyHash)
	if err != nil {
		if errors.Is(err, ErrPasswordKDFCapacity) {
			return nil, err
		}
		if _, dummyErr := p.verifyWithLimit(ctx, password, p.dummyHash); dummyErr != nil {
			return nil, dummyErr
		}
		return nil, ErrPasswordCredentialUnavailable
	}
	if found && encodedHash == "" {
		return nil, ErrPasswordCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcome := PasswordMismatch
	if found && match {
		outcome = PasswordSuccess
	}
	finishAttempted = true
	if err := p.quota.Finish(ctx, attempt, outcome); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if outcome == PasswordMismatch {
		return nil, ErrInvalidCredentials
	}
	return &AuthResult{Account: canonical}, nil
}

// ValidatePasswordAccount 是运行时和离线迁移工具共享的账号形状门禁，防止
// 两条路径对空白、UTF-8 或 VARCHAR(191) 边界的理解发生漂移。
func ValidatePasswordAccount(account string) error {
	if account == "" || account != strings.TrimSpace(account) || !utf8.ValidString(account) {
		return errors.New("account must be valid UTF-8, non-empty, and have no surrounding whitespace")
	}
	if utf8.RuneCountInString(account) > passwordAccountMaxRunes {
		return fmt.Errorf("account exceeds %d characters", passwordAccountMaxRunes)
	}
	return nil
}

func (p *ProductionPasswordProvider) verifyWithLimit(ctx context.Context, password, encoded string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%w: %w", ErrPasswordKDFCapacity, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.kdfWaitTimeout)
	defer cancel()
	select {
	case p.kdfSlots <- struct{}{}:
		defer func() { <-p.kdfSlots }()
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("%w: %w", ErrPasswordKDFCapacity, err)
		}
	case <-waitCtx.Done():
		return false, fmt.Errorf("%w: %w", ErrPasswordKDFCapacity, waitCtx.Err())
	}

	match, err := p.verifyKDF(password, encoded)
	if ctx.Err() != nil {
		return false, fmt.Errorf("%w: %w", ErrPasswordKDFCapacity, ctx.Err())
	}
	return match, err
}

// HashPassword 生成可直接写入 user_accounts.password 的 Argon2id PHC 字符串。
// 它只供受控管理/迁移工具使用，Login RPC 本身没有注册或改密能力。
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	if len(password) > passwordMaxBytes {
		return "", fmt.Errorf("password exceeds %d bytes", passwordMaxBytes)
	}
	return hashPasswordWithParams(password, defaultArgon2idParams)
}

func hashPasswordWithParams(password string, params argon2idParams) (string, error) {
	salt := make([]byte, params.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, params.keyLength)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, params.memory, params.iterations, params.parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

func verifyArgon2id(password, encoded string) (bool, error) {
	params, salt, expected, err := parseArgon2id(encoded)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func parseArgon2id(encoded string) (argon2idParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return argon2idParams{}, nil, nil, errors.New("unsupported password hash format")
	}

	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return argon2idParams{}, nil, nil, errors.New("invalid Argon2id parameters")
	}
	values := make(map[string]uint64, 3)
	for _, field := range fields {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			return argon2idParams{}, nil, nil, errors.New("invalid Argon2id parameter")
		}
		value, err := strconv.ParseUint(kv[1], 10, 32)
		if err != nil {
			return argon2idParams{}, nil, nil, errors.New("invalid Argon2id parameter value")
		}
		if _, duplicate := values[kv[0]]; duplicate {
			return argon2idParams{}, nil, nil, errors.New("duplicate Argon2id parameter")
		}
		values[kv[0]] = value
	}
	memory, mok := values["m"]
	iterations, tok := values["t"]
	parallelism, pok := values["p"]
	if !mok || !tok || !pok || len(values) != 3 {
		return argon2idParams{}, nil, nil, errors.New("missing Argon2id parameter")
	}
	// 同时设上下界：拒绝弱哈希，也防止被污染的数据库记录诱导进程分配
	// 不受控内存或执行超长时间。
	if memory < 19*1024 || memory > 256*1024 || iterations < 2 || iterations > 10 || parallelism < 1 || parallelism > 16 {
		return argon2idParams{}, nil, nil, errors.New("Argon2id parameters outside policy")
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return argon2idParams{}, nil, nil, errors.New("invalid Argon2id salt")
	}
	expected, err := b64.DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return argon2idParams{}, nil, nil, errors.New("invalid Argon2id digest")
	}

	return argon2idParams{
		memory:      uint32(memory),
		iterations:  uint32(iterations),
		parallelism: uint8(parallelism),
		keyLength:   uint32(len(expected)),
	}, salt, expected, nil
}
