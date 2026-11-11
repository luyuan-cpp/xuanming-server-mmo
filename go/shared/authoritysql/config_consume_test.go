package authoritysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	pb "proto/common/database"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := CheckPrivateControlDirectory(dir); err != nil {
		t.Skip("平台临时目录不是私有目录: ", err)
	}
	return dir
}

// 进程还没写过就退出没有东西丢:依赖没就绪时同一个 Pod 重启接着用这份。写过之后这个 boot 不能再起。
func TestWriterGrantIsConsumedOnlyBeforeFirstWrite(t *testing.T) {
	dir := privateTempDir(t)
	physical := bytes.Repeat([]byte{9}, 32)
	set := &pb.SqlWriterBootGrantSet{Format: GrantSetFormat, Grants: []*pb.SqlWriterBootGrant{{
		Format: "SQL_WRITER_BOOT_V1", WriterKey: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), Generation: "ab",
		AuthoritySchema: "a", ProtectedSchemas: []string{"a"}, SqlPrincipal: []byte("writer@%"), RegistrationDigest: make([]byte, 32),
		DeploymentId: "boot-1", GrantId: bytes.Repeat([]byte{3}, 16), GrantedMs: 1, Permission: "NORMAL", PhysicalAuthoritySha256: physical,
		LockPrimitive: pb.SqlWriterBootGrant_LOCK_TOUCH, LockQualificationSha256: make([]byte, 32),
	}}}
	raw, err := protojson.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "grant.json")
	if err = WritePrivateControlFile(path, raw); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	env := map[string]string{"MMORPG_SQL_WRITER_GRANT_FILE": path, "MMORPG_SQL_WRITER_GRANT_SHA256": hex.EncodeToString(digest[:]),
		"MMORPG_SQL_WRITER_CONSUME_DIR": dir, "MMORPG_SQL_DEPLOYMENT_ID": "boot-1", "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256": hex.EncodeToString(physical)}
	load := func() (map[string]WriterConfig, error) {
		return LoadWriterConfigs(func(key string) string { return env[key] })
	}
	if _, err = load(); err != nil {
		t.Fatal(err)
	}
	configs, err := load()
	if err != nil {
		t.Fatalf("没写过,重启还能用这份: %v", err)
	}
	if err = configs["a"].boot.marker.consume(); err != nil {
		t.Fatal(err)
	}
	if err = configs["a"].boot.marker.consume(); err != nil {
		t.Fatalf("同一个进程重复消费是幂等的: %v", err)
	}
	if _, err = load(); err == nil {
		t.Fatal("写过之后这个 boot 不能再起")
	}
}

func TestWriterDriverConsumesGrantBeforeFirstWrite(t *testing.T) {
	dir := privateTempDir(t)
	marker := filepath.Join(dir, "grant.consumed")
	permission := writerUnitPermission()
	permission.writer.boot.marker = &grantMarker{dir: dir, name: "grant.consumed", content: []byte("digest\n")}
	rows, err := writerFixtureWith(&writerFakeWire{sets: 1}, permission).QueryContext(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if _, err = os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("纯读不消费: %v", err)
	}

	if _, err = writerFixtureWith(&writerFakeWire{}, permission).ExecContext(context.Background(), "UPDATE t SET n=n+1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(marker); err != nil {
		t.Fatalf("写之前要落盘: %v", err)
	}
}

// COMMIT 没拿到结果才算提交结局未知。语句发出去之后失败,事务回滚了,不算。
// R6(2026-11-10)起,自动事务只发过纯 DML 时语句失败判「提交前中止」,根本不是结局未知;
// 别的阶段的结局未知(比如执行过可能隐式提交的语句之后)也不能被认成提交结局未知。
func TestCommitOutcomeUnknownIsTellable(t *testing.T) {
	_, err := writerFixture(&writerFakeWire{commitErr: driver.ErrBadConn}).ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
	if !errors.Is(err, ErrOutcomeUnknown) || !IsCommitOutcomeUnknown(err) {
		t.Fatalf("COMMIT 失败: %v", err)
	}
	if IsCommitOutcomeUnknown(fmt.Errorf("wrapped: %w", err)) != true {
		t.Fatal("包一层也要认得")
	}
	_, err = writerFixture(&writerFakeWire{execErr: driver.ErrBadConn}).ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
	if !IsAbortedBeforeCommit(err) || errors.Is(err, ErrOutcomeUnknown) || IsCommitOutcomeUnknown(err) {
		t.Fatalf("statement failure before COMMIT must be aborted-before-commit, not a commit outcome: %v", err)
	}
	if IsCommitOutcomeUnknown(unknown("业务发送后", driver.ErrBadConn)) {
		t.Fatal("non-commit unknown outcome must not be reported as a commit outcome")
	}
	if IsCommitOutcomeUnknown(errors.New("other")) || IsCommitOutcomeUnknown(nil) {
		t.Fatal("别的错误不算")
	}
}

// 标记写不成就不发这条语句:确定没写。
func TestWriterDriverDoesNotWriteWithoutMarker(t *testing.T) {
	permission := writerUnitPermission()
	permission.writer.boot.marker = &grantMarker{dir: filepath.Join(t.TempDir(), "no-such-directory"), name: "grant.consumed", content: []byte("digest\n")}
	w := &writerFakeWire{}
	c := writerFixtureWith(w, permission)
	if _, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil); err == nil {
		t.Fatal("标记写不成不能写")
	}
	if len(w.events) != 0 {
		t.Fatalf("不能有任何语句发出去: %v", w.events)
	}
}
