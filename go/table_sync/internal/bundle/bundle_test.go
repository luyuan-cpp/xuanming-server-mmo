package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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

	// 摘要不匹配必须拒绝解包。
	_, _, err = VerifyAndExtract(res.ArchivePath, "0000000000000000000000000000000000000000000000000000000000000000", testSchema, d1, bundlesDir)
	require.Error(t, err)

	// 从 v2 回滚到 v1 时 Item 1002 消失:CheckPermanentIDRollback 必须检出。
	require.NotEqual(t, d1, d2)
	losses, err := CheckPermanentIDRollback(srcV2, srcV1)
	require.NoError(t, err)
	require.Len(t, losses, 1)
	require.Equal(t, "Item", losses[0].Table)
	require.Equal(t, []uint64{1002}, losses[0].MissingIDs)
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
