package main

import (
	"strings"
	"testing"
)

// 每种账号现状与 -create 的组合只有一个结论:该建行、该写一次哈希,或者拒绝并且不碰库。
func TestDecideCredentialAction(t *testing.T) {
	tests := []struct {
		name            string
		matched         int
		hasArgon2idHash bool
		create          bool
		want            credentialAction
		wantErr         string
	}{
		{name: "开通新账号", matched: 0, create: true, want: actionInsert},
		{name: "账号不存在又没给 -create:不替运维猜成开通", matched: 0, wantErr: "pass -create"},
		{name: "给既有、还没有哈希的账号写入", matched: 1, want: actionSetHash},
		{name: "-create 遇到已有账号:不覆盖别人的行", matched: 1, create: true, wantErr: "already exists"},
		{name: "-create 遇到已有哈希的账号", matched: 1, hasArgon2idHash: true, create: true, wantErr: "already exists"},
		{name: "已有 Argon2id 哈希:不改密", matched: 1, hasArgon2idHash: true, wantErr: "does not change passwords"},
		{name: "读到多行:表没有主键,不是凭据表的形状", matched: 2, wantErr: "primary key"},
		{name: "读到多行时 -create 也一样拒绝", matched: 2, create: true, wantErr: "primary key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decideCredentialAction(tt.matched, tt.hasArgon2idHash, tt.create)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
				}
				if got != 0 {
					t.Fatalf("拒绝时不该给出动作: %v", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("action = %v err = %v, want %v", got, err, tt.want)
			}
		})
	}
}

// 参数不合格时在读口令、消费 writer 资格、连库之前就拒绝:库名指到存档库、DSN 没给、账号形状不对。
func TestRunRejectsBadArgumentsBeforeTouchingAnyDependency(t *testing.T) {
	const dsnEnv = "LOGIN_TEST_PASSWORD_ADMIN_DSN"
	t.Setenv(dsnEnv, "writer:secret@tcp(127.0.0.1:1)/mmorpg_zone_1")
	// 没有设 MMORPG_SQL_WRITER_GRANT_FILE 等资格变量:走到消费资格那一步会报别的错,这里的断言按错误文本区分。
	if _, err := run("alice", dsnEnv, "mmorpg", true, true); err == nil || !strings.Contains(err.Error(), `credential database "mmorpg"`) {
		t.Fatalf("DSN 指到存档库必须被拒: %v", err)
	}
	if _, err := run("alice", "LOGIN_TEST_PASSWORD_ADMIN_DSN_UNSET", "mmorpg", true, true); err == nil || !strings.Contains(err.Error(), "missing or empty") {
		t.Fatalf("没给 DSN 必须被拒: %v", err)
	}
	if _, err := run(" alice", dsnEnv, "mmorpg", true, true); err == nil || !strings.Contains(err.Error(), "invalid account") {
		t.Fatalf("账号形状不对必须被拒: %v", err)
	}
	// 错误里不带 DSN:连接串含口令。
	_, err := run("alice", dsnEnv, "mmorpg", true, true)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("错误文本不得泄露连接串: %v", err)
	}
}

func TestReadPasswordLinesRequiresExactlyTwoMatchingLines(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "matching two lines", input: "secret\nsecret\n", want: "secret"},
		{name: "one line", input: "secret\n", wantErr: true},
		{name: "three lines", input: "secret\nsecret\nignored\n", wantErr: true},
		{name: "mismatch", input: "secret\ndifferent\n", wantErr: true},
		{name: "empty but exactly two", input: "\n\n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPasswordLines(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("readPasswordLines error = %v, wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("password = %q, want %q", got, tt.want)
			}
		})
	}
}
