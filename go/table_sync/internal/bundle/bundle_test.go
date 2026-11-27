package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSchema = "abcdef0123456789"

func writeTestTableDir(t *testing.T, dir string, version uint64, itemJSON string, skillJSON string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	files := map[string]struct {
		json      string
		pb        string
		permanent bool
		hot       bool
	}{
		"Item":  {json: itemJSON, pb: "pb-item", permanent: true, hot: false},
		"Skill": {json: skillJSON, pb: "pb-skill", permanent: false, hot: true},
	}
	tables := map[string]TableEntry{}
	digestSum := sha256.New()
	for _, name := range []string{"Item", "Skill"} {
		f := files[name]
		jFile := name + ".json"
		pFile := name + ".pb"
		jb := []byte(f.json)
		pb := []byte(f.pb)
		require.NoError(t, os.WriteFile(filepath.Join(dir, jFile), jb, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, pFile), pb, 0o644))
		jSum := sha256.Sum256(jb)
		pSum := sha256.Sum256(pb)
		jHex := hex.EncodeToString(jSum[:])
		pHex := hex.EncodeToString(pSum[:])
		_, _ = digestSum.Write([]byte(name + jHex + pHex))
		tables[name] = TableEntry{
			HotReload:    f.hot,
			IDsPermanent: f.permanent,
			Rows:         1,
			Artifacts: []Artifact{
				{Kind: "json", File: jFile, Size: int64(len(jb)), SHA256: jHex},
				{Kind: "pb", File: pFile, Size: int64(len(pb)), SHA256: pHex},
			},
		}
	}
	contentDigest := hex.EncodeToString(digestSum.Sum(nil))
	manifest := Manifest{
		SchemaVersion: 2,
		SchemaDigest:  testSchema,
		Version:       version,
		ContentDigest: contentDigest,
		Tables:        tables,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFilename), raw, 0o644))
	return contentDigest
}

// 导表器写出的清单里 tables 是数组,每项带 name。早先这里把它当对象解,真实的表目录一份都读不进来;
// 而本包的夹具是用同一个结构体写出去的,所以单测一直是绿的。这条把形状钉死,不依赖仓里的文件。
func TestManifestJSONUsesExporterArrayShape(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	raw := []byte(`{
	  "schema_version": 2, "schema_digest": "abcdef0123456789", "version": 7,
	  "content_digest": "` + sha + `",
	  "generated_at": "2026-11-26T00:00:00Z", "source_rev": {"commit": "unknown", "data_dirty": true}, "table_count": 2,
	  "tables": [
	    {"name": "Item", "hot_reload": false, "ids_permanent": true, "rows": 3,
	     "source": {"file": "Item.xlsx", "size": 1, "sha256": "` + sha + `"},
	     "artifacts": [{"kind": "json", "file": "item.json", "size": 10, "sha256": "` + sha + `"},
	                   {"kind": "binary", "file": "item.pb", "size": 4, "sha256": "` + sha + `"}]},
	    {"name": "Skill", "hot_reload": true, "ids_permanent": false, "rows": 1,
	     "artifacts": [{"kind": "json", "file": "skill.json", "size": 2, "sha256": "` + sha + `"}]}
	  ]}`)

	var m Manifest
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, 2, m.SchemaVersion)
	require.Equal(t, uint64(7), m.Version)
	require.Len(t, m.Tables, 2)
	item := m.Tables["Item"]
	require.Equal(t, "Item", item.Name)
	require.True(t, item.IDsPermanent)
	require.Len(t, item.Artifacts, 2)
	require.True(t, m.Tables["Skill"].HotReload)

	// 写回去仍是按表名排序的数组,别的读者(Go 服务、C++ 节点)才读得了
	out, err := json.Marshal(m)
	require.NoError(t, err)
	require.Contains(t, string(out), `"tables":[{"name":"Item"`)
	var again Manifest
	require.NoError(t, json.Unmarshal(out, &again))
	require.Equal(t, m, again)

	require.Error(t, json.Unmarshal([]byte(`{"tables":[{"name":"A"},{"name":"A"}]}`), &m), "duplicate table name")
	require.Error(t, json.Unmarshal([]byte(`{"tables":[{"name":" "}]}`), &m), "missing table name")
	require.Error(t, json.Unmarshal([]byte(`{"tables":{"A":{}}}`), &m), "tables must be an array")
}

// 再拿仓里真实导出的那一套核一遍:清单读得进来,登记的每个文件大小与 sha256 都对得上。
func TestVerifyDirManifestAcceptsRealExporterOutput(t *testing.T) {
	repoTables := filepath.Join("..", "..", "..", "..", "generated", "tables")
	if _, err := os.Stat(filepath.Join(repoTables, ManifestFilename)); err != nil {
		t.Skipf("real generated/tables/manifest.json unavailable: %v", err)
	}
	m, err := VerifyDirManifest(repoTables, "", "")
	require.NoError(t, err)
	require.NotEmpty(t, m.Tables)
	for name, entry := range m.Tables {
		require.Equal(t, name, entry.Name)
		require.NotEmpty(t, entry.Artifacts, name)
	}
}

func TestPackVerifyExtractAndPermanentIDRollback(t *testing.T) {
	root := t.TempDir()
	srcV1 := filepath.Join(root, "v1")
	srcV2 := filepath.Join(root, "v2")
	outDir := filepath.Join(root, "out")
	bundlesDir := filepath.Join(root, "bundles")

	d1 := writeTestTableDir(t, srcV1, 1, `{"data":[{"id":1001}]}`, `{"data":[{"id":1}]}`)
	d2 := writeTestTableDir(t, srcV2, 2, `{"data":[{"id":1001},{"id":1002}]}`, `{"data":[{"id":1}]}`)

	res, err := Pack(srcV1, outDir)
	require.NoError(t, err)
	require.Equal(t, d1, res.ContentDigest)
	require.Equal(t, testSchema, res.SchemaDigest)

	// 再次打包同目录应产出完全相同的压缩包 SHA-256(确定性归档)。
	res2, err := Pack(srcV1, outDir)
	require.NoError(t, err)
	require.Equal(t, res.BundleSHA256, res2.BundleSHA256)

	extracted, manifest, err := VerifyAndExtract(res.ArchivePath, res.BundleSHA256, testSchema, d1, bundlesDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(bundlesDir, d1), extracted)
	require.Equal(t, uint64(1), manifest.Version)
	if runtime.GOOS != "windows" { // Windows 上目录没有 POSIX 权限位
		info, err := os.Stat(extracted)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "business containers read bundles under another UID only if the dir is world-readable")
	}

	// 摘要不匹配必须拒绝解包。
	_, _, err = VerifyAndExtract(res.ArchivePath, "0000000000000000000000000000000000000000000000000000000000000000", testSchema, d1, bundlesDir)
	require.Error(t, err)

	// 从 v2 回滚到 v1 时 Item 1002 消失:CheckPermanentIDRollback 必须检出。
	require.NotEqual(t, d1, d2)
	losses, err := CheckPermanentIDRollback(srcV2, srcV1, testSchema, d2, d1)
	require.NoError(t, err)
	require.Len(t, losses, 1)
	require.Equal(t, "Item", losses[0].Table)
	require.Equal(t, []uint64{1002}, losses[0].MissingIDs)
}

// 目录必须就是这次回滚的两包(数据覆写审计 r2 第 4 轮 r2-ops#1):current-dir 不是线上生效包、target-dir 不是回滚目标、
// 同一目录传两次、schema 线不对,比出来的「没有丢行」都不作数,一律报错。
func TestCheckPermanentIDRollbackRejectsDirsThatAreNotTheTwoBundles(t *testing.T) {
	root := t.TempDir()
	srcV1 := filepath.Join(root, "v1")
	srcV2 := filepath.Join(root, "v2")
	d1 := writeTestTableDir(t, srcV1, 1, `{"data":[{"id":1001}]}`, `{"data":[{"id":1}]}`)
	d2 := writeTestTableDir(t, srcV2, 2, `{"data":[{"id":1001},{"id":1002}]}`, `{"data":[{"id":1}]}`)

	cases := map[string]struct {
		currentDir, targetDir, schema, live, target string
	}{
		"current dir is not the live bundle":             {srcV1, srcV1, testSchema, d2, d1},
		"same dir twice: target dir is not the target":   {srcV2, srcV2, testSchema, d2, d1},
		"wrong schema line":                              {srcV2, srcV1, "0123456789abcdef", d2, d1},
		"missing live digest (nothing verified as live)": {srcV2, srcV1, testSchema, "", d1},
	}
	for name, tc := range cases {
		_, err := CheckPermanentIDRollback(tc.currentDir, tc.targetDir, tc.schema, tc.live, tc.target)
		require.Error(t, err, name)
	}
}

func TestVerifyAndExtractRejectsTraversalAndSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		hdr  *tar.Header
	}{
		{
			name: "path-traversal",
			hdr:  &tar.Header{Name: "../evil.json", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg},
		},
		{
			name: "subdir",
			hdr:  &tar.Header{Name: "sub/item.json", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg},
		},
		{
			name: "symlink",
			hdr:  &tar.Header{Name: "link.json", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tarPath := filepath.Join(dir, "bad.tar.gz")
			f, err := os.Create(tarPath)
			require.NoError(t, err)
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			require.NoError(t, tw.WriteHeader(tc.hdr))
			if tc.hdr.Size > 0 {
				_, err = tw.Write([]byte("{}"))
				require.NoError(t, err)
			}
			require.NoError(t, tw.Close())
			require.NoError(t, gz.Close())
			require.NoError(t, f.Close())

			_, sum, err := HashFile(tarPath)
			require.NoError(t, err)
			_, _, err = VerifyAndExtract(tarPath, sum, "", "", filepath.Join(dir, "bundles"))
			require.Error(t, err)
		})
	}
}
