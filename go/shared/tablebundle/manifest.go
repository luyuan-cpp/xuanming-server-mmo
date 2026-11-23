// Package tablebundle 实现 Go 服务端的配置表包校验、启动加载与运行期两阶段热更
// (docs/design/config-table-hot-reload.md §5、§7、§8.1、§8.3、§11)。
package tablebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// ManifestSchemaVersion 是表包机制要求的最低 manifest 版本(新增 schema_digest 与 hot_reload)。
	ManifestSchemaVersion = 2
	// ManifestFilename 是表目录内的清单文件名。
	ManifestFilename = "manifest.json"
)

// ManifestArtifact 对应 manifest.json 中单张表的一个数据文件条目(.json / .pb)。
type ManifestArtifact struct {
	Kind   string `json:"kind"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ManifestTableEntry 对应 manifest.json 中单张表的元数据与产物列表。
type ManifestTableEntry struct {
	Name         string             `json:"name"`
	HotReload    bool               `json:"hot_reload"`
	IDsPermanent bool               `json:"ids_permanent"`
	Rows         int64              `json:"rows"`
	Artifacts    []ManifestArtifact `json:"artifacts"`
}

// BundleManifest 对应 generated/tables/manifest.json(第 2 版)。
// 导表器输出的 JSON 中 tables 是按表名排序的对象数组(与 C++ table_bundle_manager.cpp 同口径),
// 解析后按表名索引到 Tables 映射供校验与差量比对。
type BundleManifest struct {
	SchemaVersion int                           `json:"schema_version"`
	SchemaDigest  string                        `json:"schema_digest"`
	Version       uint64                        `json:"version"`
	ContentDigest string                        `json:"content_digest"`
	Tables        map[string]ManifestTableEntry `json:"tables"`
}

type bundleManifestWire struct {
	SchemaVersion int                  `json:"schema_version"`
	SchemaDigest  string               `json:"schema_digest"`
	Version       uint64               `json:"version"`
	ContentDigest string               `json:"content_digest"`
	Tables        []ManifestTableEntry `json:"tables"`
}

// UnmarshalJSON 按导表器 v2 的 tables 数组格式反序列化,拒绝缺名或重名条目。
func (m *BundleManifest) UnmarshalJSON(data []byte) error {
	var wire bundleManifestWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	tables := make(map[string]ManifestTableEntry, len(wire.Tables))
	for _, entry := range wire.Tables {
		if strings.TrimSpace(entry.Name) == "" {
			return fmt.Errorf("manifest table entry missing name")
		}
		if _, dup := tables[entry.Name]; dup {
			return fmt.Errorf("manifest duplicate table entry %q", entry.Name)
		}
		tables[entry.Name] = entry
	}
	m.SchemaVersion = wire.SchemaVersion
	m.SchemaDigest = wire.SchemaDigest
	m.Version = wire.Version
	m.ContentDigest = wire.ContentDigest
	m.Tables = tables
	return nil
}

// MarshalJSON 按导表器 v2 的确定性排序数组格式写出 tables。
func (m BundleManifest) MarshalJSON() ([]byte, error) {
	names := make([]string, 0, len(m.Tables))
	for name := range m.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]ManifestTableEntry, 0, len(names))
	for _, name := range names {
		entry := m.Tables[name]
		if entry.Name == "" {
			entry.Name = name
		}
		list = append(list, entry)
	}
	return json.Marshal(bundleManifestWire{
		SchemaVersion: m.SchemaVersion,
		SchemaDigest:  m.SchemaDigest,
		Version:       m.Version,
		ContentDigest: m.ContentDigest,
		Tables:        list,
	})
}

// IsLowerHex 检查 s 是否恰好为 expectedLen 位小写十六进制字符。
func IsLowerHex(s string, expectedLen int) bool {
	if len(s) != expectedLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// isSafeFlatFilename 拒绝任何路径分隔符或相对跳转,防止清单指向目录外文件。
func isSafeFlatFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// hashFile 计算文件大小与小写十六进制 SHA-256。
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("not a regular file: %s", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// ArtifactForFormat 按 useBinary 选择该表应核对的数据文件(.pb 或 .json)。
func (e ManifestTableEntry) ArtifactForFormat(useBinary bool) (ManifestArtifact, bool) {
	wantExt := ".json"
	if useBinary {
		wantExt = ".pb"
	}
	for _, a := range e.Artifacts {
		if strings.HasSuffix(strings.ToLower(a.File), wantExt) {
			return a, true
		}
	}
	return ManifestArtifact{}, false
}

// VerifyTableDirManifest 读取并核对表目录中的 manifest.json:
// 校验 schema_version >= 2、schema_digest 匹配(非空时)、content_digest 格式、
// 以及每张表对应格式(.pb / .json)的文件存在性、字节大小与 SHA-256。
func VerifyTableDirManifest(dir string, useBinary bool, expectedSchemaDigest string) (*BundleManifest, error) {
	manifestPath := filepath.Join(dir, ManifestFilename)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	var m BundleManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	if m.SchemaVersion < ManifestSchemaVersion {
		return nil, fmt.Errorf("manifest schema_version=%d < required %d", m.SchemaVersion, ManifestSchemaVersion)
	}
	if !IsLowerHex(m.SchemaDigest, 16) {
		return nil, fmt.Errorf("manifest schema_digest invalid: %q", m.SchemaDigest)
	}
	if expectedSchemaDigest != "" && m.SchemaDigest != expectedSchemaDigest {
		return nil, fmt.Errorf("manifest schema_digest mismatch: bundle=%s binary=%s", m.SchemaDigest, expectedSchemaDigest)
	}
	if !IsLowerHex(m.ContentDigest, 64) {
		return nil, fmt.Errorf("manifest content_digest invalid: %q", m.ContentDigest)
	}
	if len(m.Tables) == 0 {
		return nil, fmt.Errorf("manifest %s contains no tables", manifestPath)
	}

	tableNames := make([]string, 0, len(m.Tables))
	for name := range m.Tables {
		tableNames = append(tableNames, name)
	}
	sort.Strings(tableNames)

	for _, name := range tableNames {
		entry := m.Tables[name]
		art, ok := entry.ArtifactForFormat(useBinary)
		if !ok {
			return nil, fmt.Errorf("manifest table %s missing artifact for useBinary=%v", name, useBinary)
		}
		if !isSafeFlatFilename(art.File) {
			return nil, fmt.Errorf("manifest table %s has unsafe artifact filename %q", name, art.File)
		}
		if !IsLowerHex(art.SHA256, 64) {
			return nil, fmt.Errorf("manifest table %s has invalid sha256 %q", name, art.SHA256)
		}
		fullPath := filepath.Join(dir, art.File)
		actualSize, actualSHA, err := hashFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("verify table %s file %s: %w", name, art.File, err)
		}
		if actualSize != art.Size {
			return nil, fmt.Errorf("table %s file %s size mismatch: got=%d want=%d", name, art.File, actualSize, art.Size)
		}
		if actualSHA != art.SHA256 {
			return nil, fmt.Errorf("table %s file %s sha256 mismatch: got=%s want=%s", name, art.File, actualSHA, art.SHA256)
		}
	}
	return &m, nil
}

// DiffChangedTables 按 useBinary 对应的文件 sha256 比对两份清单,返回发生增删改的表名(字典序)。
func DiffChangedTables(current, candidate *BundleManifest, useBinary bool) []string {
	if candidate == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(candidate.Tables))
	var changed []string
	for name, candEntry := range candidate.Tables {
		seen[name] = struct{}{}
		if current == nil {
			changed = append(changed, name)
			continue
		}
		curEntry, ok := current.Tables[name]
		if !ok {
			changed = append(changed, name)
			continue
		}
		curArt, curOK := curEntry.ArtifactForFormat(useBinary)
		candArt, candOK := candEntry.ArtifactForFormat(useBinary)
		if !curOK || !candOK || curArt.SHA256 != candArt.SHA256 || curArt.File != candArt.File {
			changed = append(changed, name)
		}
	}
	if current != nil {
		for name := range current.Tables {
			if _, ok := seen[name]; !ok {
				changed = append(changed, name)
			}
		}
	}
	sort.Strings(changed)
	return changed
}
