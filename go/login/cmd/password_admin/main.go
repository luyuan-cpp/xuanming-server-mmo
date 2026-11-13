// password_admin 是口令凭据的受控写入工具。凭据表是 mmorpg 库的 user_accounts(归属与形状见
// internal/logic/pkg/auth/password_schema.go),login 进程自己永不写口令,写入只走这里:
//   - 缺省:给已有、但还没有 Argon2id 哈希的账号写入一次哈希;
//   - -create:开通一个新的口令账号,账号行不存在时建行并写入哈希。
//
// 不提供改密:账号已经有 Argon2id 哈希时拒绝覆盖。要重置,先由 DBA 把那一行的 password 置 NULL(停用),再用缺省方式写入新哈希。
// 明文口令只从终端或 stdin 读,不进命令行参数、日志或 SQL 文件。写入走 login 的一次性 writer 资格(shared/authoritysql),
// 档位取环境变量 MMORPG_SQL_BACKEND(tidb | mysql)。
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"shared/authoritysql"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"golang.org/x/term"

	"login/internal/logic/pkg/auth"
)

func main() {
	account := flag.String("account", "", "account to write a password hash for")
	create := flag.Bool("create", false, "provision a new password account: insert the row when the account does not exist (refused when it exists)")
	dsnEnv := flag.String("dsn-env", "LOGIN_PASSWORD_MIGRATION_DSN", "name of the environment variable holding the DSN of the credential database")
	database := flag.String("database", auth.PasswordAuthDatabase, "logical database holding the credential table; the DSN must select it (production uses the default)")
	passwordStdin := flag.Bool("password-stdin", false, "read the password as two lines from stdin (batch use only; never pass it as an argument)")
	flag.Parse()

	action, err := run(*account, *dsnEnv, *database, *create, *passwordStdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "password_admin failed: %v\n", err)
		os.Exit(1)
	}
	if action == actionInsert {
		fmt.Println("password account created with an Argon2id hash")
		return
	}
	fmt.Println("password hash written for the existing account")
}

// credentialAction 是对一个账号要做的那一笔写入。
type credentialAction int

const (
	// actionInsert:账号行不存在,建行并写入哈希(-create)。
	actionInsert credentialAction = iota + 1
	// actionSetHash:账号行存在、还没有 Argon2id 哈希,写入一次。
	actionSetHash
)

// decideCredentialAction 按账号现状与 -create 决定做什么;不该做的情况返回错误,调用方不碰库。
// matched 是按 account 等值读到的行数(主键保证至多一行;排序规则不区分大小写,所以 Alice 与 alice 是同一行);
// hasArgon2idHash 是那一行已经有 Argon2id 哈希。
func decideCredentialAction(matched int, hasArgon2idHash, create bool) (credentialAction, error) {
	switch {
	case matched > 1:
		return 0, fmt.Errorf("account matched %d rows; the credential table must have account as its primary key", matched)
	case matched == 0 && create:
		return actionInsert, nil
	case matched == 0:
		return 0, errors.New("account does not exist; pass -create to provision a new password account")
	case create:
		return 0, errors.New("account already exists; run without -create to set the password of an existing account that has none")
	case hasArgon2idHash:
		return 0, errors.New("account already has an Argon2id hash; this tool does not change passwords (to reset: set password to NULL for that row, then run again)")
	default:
		return actionSetHash, nil
	}
}

// run 做一笔口令写入并返回做了哪一种。顺序:先做不碰外部依赖的校验,再读口令,最后才消费一次性 writer 资格并连库 ——
// 参数、库名或档位写错不能白白烧掉一份 boot grant。
func run(account, dsnEnv, database string, create, passwordStdin bool) (credentialAction, error) {
	if err := auth.ValidatePasswordAccount(account); err != nil {
		return 0, fmt.Errorf("invalid account: %w", err)
	}
	if strings.TrimSpace(dsnEnv) == "" {
		return 0, errors.New("dsn-env must not be empty")
	}
	dsn := os.Getenv(dsnEnv)
	if strings.TrimSpace(dsn) == "" {
		return 0, fmt.Errorf("DSN environment variable %q is missing or empty", dsnEnv)
	}
	parsedDSN, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return 0, errors.New("credential database DSN is invalid")
	}
	// 口令凭据全服一份,在 mmorpg(缺省的 -database)。DSN 指到别的库(比如存档库里那张同名旧表)直接拒绝。
	if database == "" || parsedDSN.DBName != database {
		return 0, fmt.Errorf("the DSN must select the credential database %q", database)
	}
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", authoritysql.BackendEnv, err)
	}

	password, err := readPasswordTwice(passwordStdin)
	if err != nil {
		return 0, err
	}
	encoded, err := auth.HashPassword(password)
	if err != nil {
		return 0, err
	}

	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return 0, fmt.Errorf("the one-time SQL writer grant is unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := authoritysql.Open(ctx, parsedDSN.FormatDSN(), database, backend, 1, authoritysql.Writer(writer))
	if err != nil {
		return 0, fmt.Errorf("open credential database: %w", err)
	}
	defer db.Close()
	// 表不是 password_auth_schema.sql 的形状就不写:没有主键的表上「一个账号只有一行」不成立,
	// 写进去的哈希可能对不上登录时读到的那一行。
	if err := auth.VerifyPasswordTableShape(ctx, db, database); err != nil {
		return 0, err
	}
	return writeCredential(ctx, db, account, encoded, create)
}

// writeCredential 在一个事务里读出账号现状、决定并执行那一笔写入。
// 先 FOR UPDATE 再写:行已存在时锁住它,判定与写入之间哈希不会被另一次运行改掉。行不存在时 RC 下没有间隙锁,
// 两次并发的 -create 都会走到 INSERT,后到的一方撞主键唯一约束失败,不会出现两行。
func writeCredential(ctx context.Context, db *sql.DB, account, encoded string, create bool) (credentialAction, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, "SELECT account, password FROM user_accounts WHERE account = ? FOR UPDATE", account)
	if err != nil {
		return 0, fmt.Errorf("lock account row: %w", err)
	}
	matched := 0
	hasArgon2idHash := false
	for rows.Next() {
		var canonical string
		var currentHash sql.NullString
		if err := rows.Scan(&canonical, &currentHash); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan account row: %w", err)
		}
		matched++
		hasArgon2idHash = strings.HasPrefix(currentHash.String, "$argon2id$")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read account row: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close account query: %w", err)
	}

	action, err := decideCredentialAction(matched, hasArgon2idHash, create)
	if err != nil {
		return 0, err
	}
	var result sql.Result
	if action == actionInsert {
		result, err = tx.ExecContext(ctx, "INSERT INTO user_accounts (account, password) VALUES (?, ?)", account, encoded)
	} else {
		result, err = tx.ExecContext(ctx, "UPDATE user_accounts SET password = ? WHERE account = ?", encoded, account)
	}
	if err != nil {
		return 0, fmt.Errorf("write password hash: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read write result: %w", err)
	}
	if affected != 1 {
		return 0, fmt.Errorf("password write affected %d rows, expected exactly 1", affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit password write: %w", err)
	}
	return action, nil
}

func readPasswordTwice(fromStdin bool) (string, error) {
	if fromStdin {
		return readPasswordLines(os.Stdin)
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal; use -password-stdin for an intentional pipe")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password confirmation: %w", err)
	}
	if string(first) != string(second) {
		return "", fmt.Errorf("password confirmation does not match")
	}
	return string(first), nil
}

func readPasswordLines(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 4096)
	values := make([]string, 0, 2)
	for scanner.Scan() {
		if len(values) == 2 {
			return "", fmt.Errorf("password-stdin accepts exactly two input lines")
		}
		values = append(values, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read password stdin: %w", err)
	}
	if len(values) != 2 {
		return "", fmt.Errorf("password-stdin requires exactly two input lines")
	}
	if values[0] != values[1] {
		return "", fmt.Errorf("password confirmation does not match")
	}
	return values[0], nil
}
