// Package bundle 实现配置表包的打包、安全解包、清单第 2 版校验与只增不删 ID 回滚守卫
// (docs/design/config-table-hot-reload.md §5、§7、§9)。
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxBundleBytes 是单个表包压缩或解包后的硬上限(256 MiB,设计 §7)。
	MaxBundleBytes = 256 * 1024 * 1024
	// MaxBundleFiles 是单个表包允许包含的最大文件数(1024,设计 §7)。
	MaxBundleFiles = 1024
	// ManifestFilename 是表包根目录的清单文件名。
	ManifestFilename = "manifest.json"
	// MinManifestSchemaVersion 是表包要求的最低 manifest schema_version。
	MinManifestSchemaVersion = 2
)

// Artifact 对应 manifest.json 中单张表的一份数据产物(.json 或 .pb)。
type Artifact struct {
	Kind   string `json:"kind"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// TableEntry 对应 manifest.json 的 tables 数组里的一项。
type TableEntry struct {
	Name         string     `json:"name"`
	HotReload    bool       `json:"hot_reload"`
	IDsPermanent bool       `json:"ids_permanent"`
	Rows         int64      `json:"rows"`
	Artifacts    []Artifact `json:"artifacts"`
}

// Manifest 对应 generated/tables/manifest.json(第 2 版)。
// 导表器写出的 tables 是按表名排序的数组,每项带 name(tools/data_table_exporter/core/manifest.py);
// 这里解析后按表名建索引。与 go/shared/tablebundle、C++ table_bundle_manager.cpp 读的是同一种形状,
// 不能直接按 map 解:那样真实的清单一份都读不进来。
type Manifest struct {
	SchemaVersion int
	SchemaDigest  string
	Version       uint64
	ContentDigest string
	Tables        map[string]TableEntry
}

type manifestWire struct {
	SchemaVersion int          `json:"schema_version"`
	SchemaDigest  string       `json:"schema_digest"`
	Version       uint64       `json:"version"`
	ContentDigest string       `json:"content_digest"`
	Tables        []TableEntry `json:"tables"`
}

// UnmarshalJSON 读 tables 数组并按表名建索引;缺表名或表名重复的清单直接拒绝。
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var wire manifestWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	tables := make(map[string]TableEntry, len(wire.Tables))
	for _, entry := range wire.Tables {
		if strings.TrimSpace(entry.Name) == "" {
			return errors.New("manifest table entry missing name")
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

// MarshalJSON 按导表器的形状写回:tables 是按表名排序的数组。
func (m Manifest) MarshalJSON() ([]byte, error) {
	names := make([]string, 0, len(m.Tables))
	for name := range m.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]TableEntry, 0, len(names))
	for _, name := range names {
		entry := m.Tables[name]
		entry.Name = name
		list = append(list, entry)
	}
	return json.Marshal(manifestWire{
		SchemaVersion: m.SchemaVersion,
		SchemaDigest:  m.SchemaDigest,
		Version:       m.Version,
		ContentDigest: m.ContentDigest,
		Tables:        list,
	})
}

// PackResult 是 Pack 成功产出的归档文件与摘要信息。
type PackResult struct {
	ArchivePath   string
	SHA256Path    string
	BundleSHA256  string
	SchemaDigest  string
	ContentDigest string
	Version       uint64
	SizeBytes     int64
}

// ObjectKey 返回对象存储中的表包对象键(设计 §5.2):tables/<schema_digest>/<content_digest>.tar.gz。
func ObjectKey(schemaDigest, contentDigest string) string {
	return fmt.Sprintf("tables/%s/%s.tar.gz", schemaDigest, contentDigest)
}

// ObjectSHA256Key 返回与表包同目录的 .sha256 校验文件键。
func ObjectSHA256Key(schemaDigest, contentDigest string) string {
	return ObjectKey(schemaDigest, contentDigest) + ".sha256"
}

// IsLowerHex 检查 s 是否恰好为 n 位小写十六进制。
func IsLowerHex(s string, n int) bool {
	if len(s) != n {
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

func isSafeFlatName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// HashFile 计算文件字节数与小写 SHA-256。
func HashFile(path string) (int64, string, error) {
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

// VerifyDirManifest 核对目录内 manifest.json(第 2 版)以及其中登记的**全部**产物(.json 与 .pb)。
func VerifyDirManifest(dir, expectedSchemaDigest, expectedContentDigest string) (*Manifest, error) {
	manifestPath := filepath.Join(dir, ManifestFilename)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	if m.SchemaVersion < MinManifestSchemaVersion {
		return nil, fmt.Errorf("manifest schema_version=%d < %d", m.SchemaVersion, MinManifestSchemaVersion)
	}
	if !IsLowerHex(m.SchemaDigest, 16) {
		return nil, fmt.Errorf("manifest schema_digest invalid: %q", m.SchemaDigest)
	}
	if expectedSchemaDigest != "" && m.SchemaDigest != expectedSchemaDigest {
		return nil, fmt.Errorf("manifest schema_digest mismatch: got=%s want=%s", m.SchemaDigest, expectedSchemaDigest)
	}
	if !IsLowerHex(m.ContentDigest, 64) {
		return nil, fmt.Errorf("manifest content_digest invalid: %q", m.ContentDigest)
	}
	if expectedContentDigest != "" && m.ContentDigest != expectedContentDigest {
		return nil, fmt.Errorf("manifest content_digest mismatch: got=%s want=%s", m.ContentDigest, expectedContentDigest)
	}
	if len(m.Tables) == 0 {
		return nil, fmt.Errorf("manifest %s has no tables", manifestPath)
	}

	names := make([]string, 0, len(m.Tables))
	for name := range m.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := m.Tables[name]
		if len(entry.Artifacts) == 0 {
			return nil, fmt.Errorf("manifest table %s has no artifacts", name)
		}
		for _, art := range entry.Artifacts {
			if !isSafeFlatName(art.File) {
				return nil, fmt.Errorf("manifest table %s unsafe filename %q", name, art.File)
			}
			if !IsLowerHex(art.SHA256, 64) {
				return nil, fmt.Errorf("manifest table %s invalid sha256 %q", name, art.SHA256)
			}
			sz, sha, err := HashFile(filepath.Join(dir, art.File))
			if err != nil {
				return nil, fmt.Errorf("verify table %s file %s: %w", name, art.File, err)
			}
			if sz != art.Size {
				return nil, fmt.Errorf("table %s file %s size mismatch: got=%d want=%d", name, art.File, sz, art.Size)
			}
			if sha != art.SHA256 {
				return nil, fmt.Errorf("table %s file %s sha256 mismatch: got=%s want=%s", name, art.File, sha, art.SHA256)
			}
		}
	}
	return &m, nil
}

// Pack 校验 tablesDir 后将其平铺打成确定性 tar.gz(tables-<content_digest>.tar.gz + .sha256)。
func Pack(tablesDir, outDir string) (*PackResult, error) {
	m, err := VerifyDirManifest(tablesDir, "", "")
	if err != nil {
		return nil, fmt.Errorf("verify source tables dir: %w", err)
	}

	// 收集 manifest.json 与全部登记的平铺产物,按文件名排序确保归档字节确定。
	fileSet := map[string]struct{}{ManifestFilename: {}}
	for _, entry := range m.Tables {
		for _, art := range entry.Artifacts {
			fileSet[art.File] = struct{}{}
		}
	}
	if len(fileSet) > MaxBundleFiles {
		return nil, fmt.Errorf("bundle file count %d exceeds limit %d", len(fileSet), MaxBundleFiles)
	}
	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	archiveName := fmt.Sprintf("tables-%s.tar.gz", m.ContentDigest)
	archivePath := filepath.Join(outDir, archiveName)
	shaPath := archivePath + ".sha256"

	tmpFile, err := os.CreateTemp(outDir, ".pack-*.tar.gz")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	mw := io.MultiWriter(tmpFile, hasher)
	gzw, err := gzip.NewWriterLevel(mw, gzip.BestCompression)
	if err != nil {
		_ = tmpFile.Close()
		return nil, err
	}
	gzw.ModTime = time.Unix(0, 0).UTC()
	tw := tar.NewWriter(gzw)

	var totalRaw int64
	for _, name := range files {
		srcPath := filepath.Join(tablesDir, name)
		data, err := os.ReadFile(srcPath)
		if err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			_ = tmpFile.Close()
			return nil, fmt.Errorf("read %s: %w", srcPath, err)
		}
		totalRaw += int64(len(data))
		if totalRaw > MaxBundleBytes {
			_ = tw.Close()
			_ = gzw.Close()
			_ = tmpFile.Close()
			return nil, fmt.Errorf("uncompressed bundle size %d exceeds limit %d", totalRaw, MaxBundleBytes)
		}
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			_ = tmpFile.Close()
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			_ = tmpFile.Close()
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gzw.Close()
		_ = tmpFile.Close()
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		_ = tmpFile.Close()
		return nil, err
	}
	info, err := tmpFile.Stat()
	if err != nil {
		_ = tmpFile.Close()
		return nil, err
	}
	if info.Size() > MaxBundleBytes {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("compressed bundle size %d exceeds limit %d", info.Size(), MaxBundleBytes)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, err
	}

	bundleSHA := hex.EncodeToString(hasher.Sum(nil))
	if err := renameReplace(tmpPath, archivePath); err != nil {
		return nil, err
	}
	cleanup = false

	shaLine := fmt.Sprintf("%s  %s\n", bundleSHA, archiveName)
	if err := os.WriteFile(shaPath, []byte(shaLine), 0o644); err != nil {
		return nil, err
	}
	return &PackResult{
		ArchivePath:   archivePath,
		SHA256Path:    shaPath,
		BundleSHA256:  bundleSHA,
		SchemaDigest:  m.SchemaDigest,
		ContentDigest: m.ContentDigest,
		Version:       m.Version,
		SizeBytes:     info.Size(),
	}, nil
}

// VerifyAndExtract 严格校验归档大小与 SHA-256 后解包到临时目录,再逐文件核对 manifest v2,
// 全部通过后原子改名到 <destBundlesDir>/<content_digest>(设计 §7)。
func VerifyAndExtract(tarGzPath, expectedBundleSHA, expectedSchemaDigest, expectedContentDigest, destBundlesDir string) (string, *Manifest, error) {
	if !IsLowerHex(expectedBundleSHA, 64) {
		return "", nil, fmt.Errorf("expected bundle_sha256 invalid: %q", expectedBundleSHA)
	}
	if expectedContentDigest != "" && !IsLowerHex(expectedContentDigest, 64) {
		return "", nil, fmt.Errorf("expected content_digest invalid: %q", expectedContentDigest)
	}
	archiveSize, actualSHA, err := HashFile(tarGzPath)
	if err != nil {
		return "", nil, fmt.Errorf("hash archive %s: %w", tarGzPath, err)
	}
	if archiveSize > MaxBundleBytes {
		return "", nil, fmt.Errorf("archive size %d exceeds max %d", archiveSize, MaxBundleBytes)
	}
	if actualSHA != expectedBundleSHA {
		return "", nil, fmt.Errorf("archive sha256 mismatch: got=%s want=%s", actualSHA, expectedBundleSHA)
	}

	if err := os.MkdirAll(destBundlesDir, 0o755); err != nil {
		return "", nil, err
	}
	stageDir, err := os.MkdirTemp(destBundlesDir, ".extract-*")
	if err != nil {
		return "", nil, err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	f, err := os.Open(tarGzPath)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return "", nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	fileCount := 0
	var totalWritten int64
	seenFiles := map[string]struct{}{}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("read tar entry: %w", err)
		}
		fileCount++
		if fileCount > MaxBundleFiles {
			return "", nil, fmt.Errorf("archive file count exceeds limit %d", MaxBundleFiles)
		}
		if hdr.Typeflag != tar.TypeReg {
			return "", nil, fmt.Errorf("archive entry %q has forbidden type %d (only regular files allowed)", hdr.Name, hdr.Typeflag)
		}
		if !isSafeFlatName(hdr.Name) {
			return "", nil, fmt.Errorf("archive entry %q has forbidden path or traversal", hdr.Name)
		}
		if _, dup := seenFiles[hdr.Name]; dup {
			return "", nil, fmt.Errorf("archive contains duplicate entry %q", hdr.Name)
		}
		seenFiles[hdr.Name] = struct{}{}
		if hdr.Size < 0 || totalWritten+hdr.Size > MaxBundleBytes {
			return "", nil, fmt.Errorf("archive entry %q exceeds max bundle bytes %d", hdr.Name, MaxBundleBytes)
		}

		dstPath := filepath.Join(stageDir, hdr.Name)
		out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return "", nil, err
		}
		remain := MaxBundleBytes - totalWritten
		n, copyErr := io.Copy(out, io.LimitReader(tr, remain+1))
		closeErr := out.Close()
		if copyErr != nil {
			return "", nil, fmt.Errorf("extract %q: %w", hdr.Name, copyErr)
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
		if n > remain {
			return "", nil, fmt.Errorf("archive uncompressed bytes exceed limit %d", MaxBundleBytes)
		}
		totalWritten += n
	}

	manifest, err := VerifyDirManifest(stageDir, expectedSchemaDigest, expectedContentDigest)
	if err != nil {
		return "", nil, fmt.Errorf("extracted bundle manifest check failed: %w", err)
	}

	finalDir := filepath.Join(destBundlesDir, manifest.ContentDigest)
	if _, statErr := os.Stat(finalDir); statErr == nil {
		// 并发/重复同步时目标目录已存在:复核完好则直接复用并清理临时目录。
		if _, verifyErr := VerifyDirManifest(finalDir, expectedSchemaDigest, manifest.ContentDigest); verifyErr == nil {
			return finalDir, manifest, nil
		}
		_ = os.RemoveAll(finalDir)
	}
	if err := os.Rename(stageDir, finalDir); err != nil {
		return "", nil, fmt.Errorf("promote extracted bundle %s: %w", finalDir, err)
	}
	cleanupStage = false
	return finalDir, manifest, nil
}

// PermanentIDLoss 描述回滚时将在只增不删表(cfg_ids_permanent)中消失的行 id(设计 §9)。
type PermanentIDLoss struct {
	Table      string
	MissingIDs []uint64
}

// CheckPermanentIDRollback 比对旧包(当前线上)与目标包(回滚目标)的所有 ids_permanent 表,
// 列出目标包中缺失的 id(设计 §9:命中时发布工具须要求 -force 才允许回滚)。
func CheckPermanentIDRollback(currentDir, targetDir string) ([]PermanentIDLoss, error) {
	curManifest, err := VerifyDirManifest(currentDir, "", "")
	if err != nil {
		return nil, fmt.Errorf("verify current dir: %w", err)
	}
	targetManifest, err := VerifyDirManifest(targetDir, curManifest.SchemaDigest, "")
	if err != nil {
		return nil, fmt.Errorf("verify target dir: %w", err)
	}

	names := make([]string, 0, len(curManifest.Tables))
	for name, entry := range curManifest.Tables {
		if entry.IDsPermanent {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var losses []PermanentIDLoss
	for _, name := range names {
		curJSON, ok := findJSONArtifact(curManifest.Tables[name])
		if !ok {
			continue
		}
		curIDs, err := extractRowIDsFromJSON(filepath.Join(currentDir, curJSON))
		if err != nil {
			return nil, fmt.Errorf("read ids from current %s: %w", name, err)
		}
		targetIDs := map[uint64]struct{}{}
		if targetEntry, exists := targetManifest.Tables[name]; exists {
			if targetJSON, hasJSON := findJSONArtifact(targetEntry); hasJSON {
				targetIDs, err = extractRowIDsFromJSON(filepath.Join(targetDir, targetJSON))
				if err != nil {
					return nil, fmt.Errorf("read ids from target %s: %w", name, err)
				}
			}
		}
		var missing []uint64
		for id := range curIDs {
			if _, ok := targetIDs[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
			losses = append(losses, PermanentIDLoss{Table: name, MissingIDs: missing})
		}
	}
	return losses, nil
}

func findJSONArtifact(e TableEntry) (string, bool) {
	for _, a := range e.Artifacts {
		if strings.HasSuffix(strings.ToLower(a.File), ".json") {
			return a.File, true
		}
	}
	return "", false
}

func extractRowIDsFromJSON(path string) (map[uint64]struct{}, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	ids := make(map[uint64]struct{}, len(root.Data))
	for _, row := range root.Data {
		rawID, ok := row["id"]
		if !ok {
			continue
		}
		switch v := rawID.(type) {
		case float64:
			if v >= 0 {
				ids[uint64(v)] = struct{}{}
			}
		case string:
			if parsed, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				ids[parsed] = struct{}{}
			}
		}
	}
	return ids, nil
}

func renameReplace(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		_ = os.Remove(dst)
		return os.Rename(src, dst)
	}
	return nil
}
