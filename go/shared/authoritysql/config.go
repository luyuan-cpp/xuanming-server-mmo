package authoritysql

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
)

type permissionKind uint8

const (
	permissionInvalid permissionKind = iota
	permissionReader
	permissionWriter
	permissionSchema
	permissionOperator
)

// Permission没有可修改的导出字段；零值拒绝，能力只能由装配owner明确选择。
type Permission struct {
	kind      permissionKind
	principal string
	writer    WriterConfig
}
type writerBoot struct {
	grant  *databasepb.SqlWriterBootGrant
	next   atomic.Uint64
	closed atomic.Bool
}

// WriterConfig身份永久固定，调度计数属于同boot共享状态；复制它不再消费grant。
type WriterConfig struct{ boot *writerBoot }

func Reader() Permission { return Permission{kind: permissionReader} }
func Schema(expectedPrincipal string) Permission {
	return Permission{kind: permissionSchema, principal: expectedPrincipal}
}
func Operator(expectedPrincipal string) Permission {
	return Permission{kind: permissionOperator, principal: expectedPrincipal}
}
func Writer(config WriterConfig) Permission {
	return Permission{kind: permissionWriter, writer: config}
}
func (p Permission) Kind() string {
	switch p.kind {
	case permissionReader:
		return "reader"
	case permissionWriter:
		return "writer"
	case permissionSchema:
		return "schema"
	case permissionOperator:
		return "operator"
	default:
		return "invalid"
	}
}
func LoadSchemaPermission(getenv func(string) string) (Permission, error) {
	if getenv == nil {
		return Permission{}, errors.New("缺少SQL schema主体装配")
	}
	p := Schema(getenv("MMORPG_SQL_SCHEMA_PRINCIPAL"))
	return p, p.validate()
}
func LoadOperatorPermission(getenv func(string) string) (Permission, error) {
	if getenv == nil {
		return Permission{}, errors.New("缺少SQL operator主体装配")
	}
	p := Operator(getenv("MMORPG_SQL_OPERATOR_PRINCIPAL"))
	return p, p.validate()
}
func (c WriterConfig) AuthoritySchema() string {
	if c.boot == nil {
		return ""
	}
	return c.boot.grant.AuthoritySchema
}
func (c WriterConfig) Generation() string {
	if c.boot == nil {
		return ""
	}
	return c.boot.grant.Generation
}
func (c WriterConfig) WriterKey() []byte {
	if c.boot == nil {
		return nil
	}
	return bytes.Clone(c.boot.grant.WriterKey)
}
func (c WriterConfig) InstanceID() []byte {
	if c.boot == nil {
		return nil
	}
	return bytes.Clone(c.boot.grant.InstanceId)
}

// Seal 只在全部同boot角色与SQL调用已确定join后使用；它不证明排空，只永久禁用旧handle。
func (c WriterConfig) Seal() {
	if c.boot != nil {
		c.boot.closed.Store(true)
	}
}
func (c WriterConfig) ProtectedSchemas() []string {
	if c.boot == nil {
		return nil
	}
	return append([]string(nil), c.boot.grant.ProtectedSchemas...)
}

// LoadWriterConfig只由进程装配owner调用一次；不读取隐式全局env，不生成身份或注册。
// protojson拒未知/重复字段；marker已存在或写/flush未确定时永远不删除或覆盖。
func LoadWriterConfig(getenv func(string) string) (WriterConfig, error) {
	bad := func() (WriterConfig, error) {
		return WriterConfig{}, errors.New("SQL writer启动grant缺失、漂移或已消费")
	}
	if getenv == nil {
		return bad()
	}
	path := getenv("MMORPG_SQL_WRITER_GRANT_FILE")
	dir := getenv("MMORPG_SQL_WRITER_CONSUME_DIR")
	expected, err := hex.DecodeString(getenv("MMORPG_SQL_WRITER_GRANT_SHA256"))
	if err != nil || len(expected) != sha256.Size || !filepath.IsAbs(path) || !filepath.IsAbs(dir) {
		return bad()
	}
	raw, err := ReadPrivateControlFile(path, 1<<20)
	if err != nil {
		return bad()
	}
	actual := sha256.Sum256(raw)
	if !bytes.Equal(actual[:], expected) {
		return bad()
	}
	grant := &databasepb.SqlWriterBootGrant{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, grant); err != nil {
		return bad()
	}
	physical, err := hex.DecodeString(getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != sha256.Size || !bytes.Equal(physical, grant.PhysicalAuthoritySha256) {
		return bad()
	}
	if grant.Format != "SQL_WRITER_BOOT_V1" || grant.Permission != "NORMAL" || grant.DeploymentId == "" || grant.DeploymentId != getenv("MMORPG_SQL_DEPLOYMENT_ID") ||
		len(grant.WriterKey) != 16 || len(grant.InstanceId) != 16 || len(grant.GrantId) != 16 || len(grant.RegistrationDigest) != 32 || grant.GrantedMs == 0 ||
		!validGeneration(grant.Generation) || !validIdentifier(grant.AuthoritySchema) || !validPrincipal(string(grant.SqlPrincipal)) || !validSchemas(grant.ProtectedSchemas) {
		return bad()
	}
	if len(grant.MaintenanceRunId) != 0 && len(grant.MaintenanceRunId) != 16 || len(grant.LockQualificationSha256) != 32 || (grant.LockPrimitive != databasepb.SqlWriterBootGrant_SELECT_ONLY && grant.LockPrimitive != databasepb.SqlWriterBootGrant_LOCK_TOUCH) {
		return bad()
	}
	if err = consumeGrantMarker(dir, hex.EncodeToString(grant.GrantId)+".consumed", append([]byte(hex.EncodeToString(actual[:])), '\n')); err != nil {
		return bad()
	}
	return WriterConfig{boot: &writerBoot{grant: proto.Clone(grant).(*databasepb.SqlWriterBootGrant)}}, nil
}

type grantMarkerFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// 即使写入失败也收集Sync/Close结局；任何不确定都拒启，绝不删除或重置marker。
func writeGrantMarker(file grantMarkerFile, content []byte) error {
	n, writeErr := file.Write(content)
	if n != len(content) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func (p Permission) validate() error {
	if p.kind == permissionReader {
		return nil
	}
	if p.kind == permissionSchema || p.kind == permissionOperator {
		if validPrincipal(p.principal) {
			return nil
		}
	}
	if p.kind == permissionWriter && p.writer.boot != nil && p.writer.boot.grant != nil && !p.writer.boot.closed.Load() {
		return nil
	}
	return errors.New("权威SQL必须显式提供有效Permission")
}
func validPrincipal(value string) bool {
	return value != "" && len(value) <= 288 && !strings.ContainsAny(value, "\x00\r\n")
}
func validIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
func validGeneration(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func validSchemas(values []string) bool {
	if len(values) == 0 || !sort.StringsAreSorted(values) {
		return false
	}
	for i, value := range values {
		if !validIdentifier(value) || i > 0 && value == values[i-1] {
			return false
		}
	}
	return true
}

// 私有控制文件的单一平台owner；调用方不得先验证再自行重开路径。
// Windows持祖先句柄、核真实NTFS卷与ACL；Unix拒symlink并要求私有文件及不可被他人改写的目录。
func ReadPrivateControlFile(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 || maxBytes > 16<<20 {
		return nil, errors.New("控制文件大小预算无效")
	}
	dir, name, err := privateControlPath(path)
	if err != nil {
		return nil, err
	}
	return readPrivateControlFile(dir, name, maxBytes)
}
func WritePrivateControlFile(path string, content []byte) error {
	if len(content) < 1 || len(content) > 16<<20 {
		return errors.New("控制文件正文大小无效")
	}
	dir, name, err := privateControlPath(path)
	if err != nil {
		return err
	}
	return writePrivateControlFile(dir, name, content)
}
func CheckPrivateControlDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("控制目录须绝对路径")
	}
	return checkPrivateControlDirectory(filepath.Clean(path))
}
func privateControlPath(path string) (string, string, error) {
	if !filepath.IsAbs(path) {
		return "", "", errors.New("控制文件须绝对路径")
	}
	clean := filepath.Clean(path)
	name := filepath.Base(clean)
	if name == "." || name == ".." || name == string(filepath.Separator) || strings.ContainsAny(name, ":\x00") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", "", errors.New("控制文件路径非法")
	}
	return filepath.Dir(clean), name, nil
}
func readControlBytes(file *os.File, maxBytes int64) (raw []byte, err error) {
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			raw = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxBytes {
		return nil, errors.New("控制文件类型或大小不符")
	}
	raw, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) < 1 || int64(len(raw)) > maxBytes {
		return nil, errors.New("控制文件正文大小不符")
	}
	return raw, nil
}
