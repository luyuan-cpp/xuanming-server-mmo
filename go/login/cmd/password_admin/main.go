// password_admin 是受控的存量口令迁移工具。它只给尚未迁移为 Argon2id 的
// 既有账号写入一次哈希，不提供自注册/再次改密，也不会把明文口令放进
// 命令行参数、日志或 SQL 文件。
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"golang.org/x/term"

	"login/internal/logic/pkg/auth"
)

func main() {
	account := flag.String("account", "", "要迁移口令哈希的既有账号")
	dsnEnv := flag.String("dsn-env", "LOGIN_PASSWORD_MIGRATION_DSN", "保存迁移专用 MySQL DSN 的环境变量名")
	passwordStdin := flag.Bool("password-stdin", false, "从 stdin 读取两行口令（批处理专用，禁止命令行参数）")
	flag.Parse()

	if err := run(*account, *dsnEnv, *passwordStdin); err != nil {
		fmt.Fprintf(os.Stderr, "password_admin failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("password hash migrated for the existing account")
}

func run(account, dsnEnv string, passwordStdin bool) error {
	if err := auth.ValidatePasswordAccount(account); err != nil {
		return fmt.Errorf("invalid account: %w", err)
	}
	if strings.TrimSpace(dsnEnv) == "" {
		return fmt.Errorf("dsn-env must not be empty")
	}
	dsn := os.Getenv(dsnEnv)
	if strings.TrimSpace(dsn) == "" {
		return fmt.Errorf("DSN environment variable %q is missing or empty", dsnEnv)
	}

	password, err := readPasswordTwice(passwordStdin)
	if err != nil {
		return err
	}
	encoded, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	parsedDSN, err := mysqlDriver.ParseDSN(dsn)
	if err != nil || parsedDSN.DBName == "" {
		return fmt.Errorf("migration MySQL DSN is invalid or has no database")
	}
	db, err := sql.Open("mysql", parsedDSN.FormatDSN())
	if err != nil {
		return fmt.Errorf("open MySQL: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping MySQL: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, "SELECT account, password FROM user_accounts WHERE account = ? FOR UPDATE", account)
	if err != nil {
		return fmt.Errorf("lock existing account: %w", err)
	}
	count := 0
	alreadyMigrated := false
	for rows.Next() {
		var canonical string
		var currentHash sql.NullString
		if err := rows.Scan(&canonical, &currentHash); err != nil {
			rows.Close()
			return fmt.Errorf("scan existing account: %w", err)
		}
		count++
		alreadyMigrated = strings.HasPrefix(currentHash.String, "$argon2id$")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read existing account: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close existing account query: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("account must already exist exactly once; matched %d rows (registration is not supported)", count)
	}
	if alreadyMigrated {
		return fmt.Errorf("account already has an Argon2id hash; password change/reset is not supported by this migration tool")
	}
	result, err := tx.ExecContext(ctx, "UPDATE user_accounts SET password = ? WHERE account = ?", encoded, account)
	if err != nil {
		return fmt.Errorf("update password hash: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read update result: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("password update affected %d rows, expected exactly 1", affected)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit password update: %w", err)
	}
	return nil
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
