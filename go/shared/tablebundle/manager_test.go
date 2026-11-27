package tablebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"shared/generated/table"
)

const testSchemaDigest = "0123456789abcdef"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeFakeBundle(t *testing.T, dir string, schemaDigest string, version uint64, files map[string]string, hotReload map[string]bool) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	entries := make(map[string]ManifestTableEntry, len(files))
	names := make([]string, 0, len(files))
	for tbl := range files {
		names = append(names, tbl)
	}
	sort.Strings(names)
	digestInput := ""
	for _, tbl := range names {
		body := files[tbl]
		fileName := tbl + ".json"
		raw := []byte(body)
		require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), raw, 0o644))
		h := sha256Hex(raw)
		digestInput += tbl + ":" + h + "\n"
		entries[tbl] = ManifestTableEntry{
			Name:      tbl,
			HotReload: hotReload[tbl],
			Rows:      1,
			Artifacts: []ManifestArtifact{{
				Kind:   "json",
				File:   fileName,
				Size:   int64(len(raw)),
				SHA256: h,
			}},
		}
	}
	contentDigest := sha256Hex([]byte(digestInput))
	manifest := BundleManifest{
		SchemaVersion: ManifestSchemaVersion,
		SchemaDigest:  schemaDigest,
		Version:       version,
		ContentDigest: contentDigest,
		Tables:        entries,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFilename), raw, 0o644))
	return contentDigest
}

func writePointerDoc(t *testing.T, bundleRoot string, line *PointerLine) {
	t.Helper()
	doc := PointerDocument{
		Track: "stable",
		Lines: map[string]PointerLine{},
	}
	if line != nil {
		doc.Lines[testSchemaDigest] = *line
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	require.NoError(t, WriteAtomicFile(filepath.Join(bundleRoot, PointerFilename), raw))
}

func TestVerifyTableDirManifest(t *testing.T) {
	dir := t.TempDir()
	digest := writeFakeBundle(t, dir, testSchemaDigest, 1, map[string]string{
		"Skill": `{"data":[]}`,
	}, map[string]bool{"Skill": true})

	m, err := VerifyTableDirManifest(dir, false, testSchemaDigest)
	require.NoError(t, err)
	require.Equal(t, digest, m.ContentDigest)

	// 错误的 schema_digest 被拒绝。
	_, err = VerifyTableDirManifest(dir, false, "fedcba9876543210")
	require.Error(t, err)

	// 篡改文件后 sha256 校验失败。
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Skill.json"), []byte(`{"data":[1]}`), 0o644))
	_, err = VerifyTableDirManifest(dir, false, testSchemaDigest)
	require.Error(t, err)
}

func TestVerifyTableDirManifestRejectsInvalidArrayEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"object_tables", `{"schema_version":2,"schema_digest":"0123456789abcdef","version":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tables":{"Skill":{}}}`},
		{"missing_name", `{"schema_version":2,"schema_digest":"0123456789abcdef","version":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tables":[{"hot_reload":true,"artifacts":[]}]}`},
		{"duplicate_name", `{"schema_version":2,"schema_digest":"0123456789abcdef","version":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tables":[{"name":"Skill","artifacts":[]},{"name":"Skill","artifacts":[]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m BundleManifest
			require.Error(t, json.Unmarshal([]byte(tc.raw), &m))
		})
	}
}

func TestVerifyTableDirManifestMatchesRealRepoTables(t *testing.T) {
	// 直接核对仓库真实导出的 generated/tables/manifest.json(数组格式)与当前 table.SchemaDigest。
	repoTables := filepath.Join("..", "..", "..", "generated", "tables")
	if _, err := os.Stat(filepath.Join(repoTables, ManifestFilename)); err != nil {
		t.Skipf("real generated/tables/manifest.json unavailable: %v", err)
	}
	for _, useBinary := range []bool{false, true} {
		m, err := VerifyTableDirManifest(repoTables, useBinary, table.SchemaDigest)
		require.NoError(t, err, "useBinary=%v", useBinary)
		require.NotEmpty(t, m.Tables)
	}
	m, err := ValidateTablesDir(repoTables, false, nil)
	require.NoError(t, err)
	require.NotEmpty(t, m.Tables)
}

func TestManagerStartupFallsBackToImageAndHotReloadsScheduled(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	bundleRoot := filepath.Join(root, "bundles_root")
	statusRoot := filepath.Join(root, "status_root")

	imgDigest := writeFakeBundle(t, imageDir, testSchemaDigest, 10, map[string]string{
		"Skill": `{"v":10}`,
		"World": `{"v":10}`,
	}, map[string]bool{"Skill": true, "World": false})
	writePointerDoc(t, bundleRoot, nil)

	now := time.Unix(1_800_000_000, 0)
	var committed []string
	var derivedVersion uint64

	mgr, err := LoadAtStartup(Options{
		ServiceName:   "test-svc",
		ImageTableDir: imageDir,
		BundleRoot:    bundleRoot,
		StatusRoot:    statusRoot,
		SchemaDigest:  testSchemaDigest,
		NowFn:         func() time.Time { return now },
		IsHotReloadableFn: func(name string) bool {
			return name == "Skill"
		},
		LoadTablesFn: func(dir string, _ bool) (*table.TableSet, error) {
			return &table.TableSet{}, nil
		},
		CommitTablesFn: func(ts *table.TableSet) {
			committed = append(committed, ts.Identity.ContentDigest)
		},
		PrepareHook: func(_ string, ts *table.TableSet) (func(), error) {
			v := ts.Identity.Version
			return func() { derivedVersion = v }, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{imgDigest}, committed)
	require.Equal(t, uint64(10), derivedVersion)
	st := mgr.SnapshotStatus()
	require.Equal(t, "image", st.Source)
	require.Equal(t, imgDigest, st.LoadedDigest)

	// 准备第 11 版(只改 Skill,未来 60 秒生效)。
	candTmp := filepath.Join(root, "tmp11")
	candDigest := writeFakeBundle(t, candTmp, testSchemaDigest, 11, map[string]string{
		"Skill": `{"v":11}`,
		"World": `{"v":10}`,
	}, map[string]bool{"Skill": true, "World": false})
	require.NoError(t, os.MkdirAll(filepath.Join(bundleRoot, "bundles"), 0o755))
	require.NoError(t, os.Rename(candTmp, filepath.Join(bundleRoot, "bundles", candDigest)))

	activateAt := uint64(now.Add(60 * time.Second).UnixMilli())
	writePointerDoc(t, bundleRoot, &PointerLine{
		Next: &PointerTarget{
			ContentDigest: candDigest,
			Version:       11,
			ActivateAtMs:  activateAt,
		},
	})

	// 未到点:应停在 prepared,不提交。
	mgr.PollOnce()
	st = mgr.SnapshotStatus()
	require.Equal(t, "prepared", st.PendingState)
	require.Equal(t, imgDigest, st.LoadedDigest)
	require.Equal(t, uint64(10), derivedVersion)

	// 撤销 next:应清理 prepared,继续用旧表。
	writePointerDoc(t, bundleRoot, &PointerLine{})
	mgr.PollOnce()
	st = mgr.SnapshotStatus()
	require.Equal(t, "", st.PendingState)
	require.Equal(t, imgDigest, st.LoadedDigest)

	// 重新设 next 并把时钟推进到 activate_at:应原子提交。
	writePointerDoc(t, bundleRoot, &PointerLine{
		Next: &PointerTarget{
			ContentDigest: candDigest,
			Version:       11,
			ActivateAtMs:  activateAt,
		},
	})
	mgr.PollOnce()
	require.Equal(t, "prepared", mgr.SnapshotStatus().PendingState)

	now = now.Add(60 * time.Second)
	mgr.PollOnce()
	st = mgr.SnapshotStatus()
	require.Equal(t, "bundle", st.Source)
	require.Equal(t, candDigest, st.LoadedDigest)
	require.Equal(t, "", st.PendingState)
	require.Equal(t, uint64(11), derivedVersion)
	require.Equal(t, []string{imgDigest, candDigest}, committed)
}

func TestManagerNeedsRestartAndRejectedDoNotRetry(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	bundleRoot := filepath.Join(root, "bundles_root")
	statusRoot := filepath.Join(root, "status_root")

	imgDigest := writeFakeBundle(t, imageDir, testSchemaDigest, 1, map[string]string{
		"Skill": `{"v":1}`,
		"World": `{"v":1}`,
	}, map[string]bool{"Skill": true, "World": false})
	writePointerDoc(t, bundleRoot, nil)

	prepareCalls := 0
	var hookErr error
	mgr, err := LoadAtStartup(Options{
		ServiceName:   "test-svc",
		ImageTableDir: imageDir,
		BundleRoot:    bundleRoot,
		StatusRoot:    statusRoot,
		SchemaDigest:  testSchemaDigest,
		IsHotReloadableFn: func(name string) bool {
			return name == "Skill"
		},
		LoadTablesFn: func(string, bool) (*table.TableSet, error) {
			prepareCalls++
			return &table.TableSet{}, nil
		},
		CommitTablesFn: func(*table.TableSet) {},
		PrepareHook: func(string, *table.TableSet) (func(), error) {
			return nil, hookErr
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, prepareCalls)

	// 1. 改动了不可热更的 World 表 -> 回报 needs_restart,不解析,不重试。
	tmpWorld := filepath.Join(root, "tmp_world")
	worldDigest := writeFakeBundle(t, tmpWorld, testSchemaDigest, 2, map[string]string{
		"Skill": `{"v":1}`,
		"World": `{"v":2}`,
	}, map[string]bool{"Skill": true, "World": false})
	require.NoError(t, os.MkdirAll(filepath.Join(bundleRoot, "bundles"), 0o755))
	require.NoError(t, os.Rename(tmpWorld, filepath.Join(bundleRoot, "bundles", worldDigest)))
	writePointerDoc(t, bundleRoot, &PointerLine{
		Next: &PointerTarget{ContentDigest: worldDigest, Version: 2, Immediate: true},
	})
	mgr.PollOnce()
	mgr.PollOnce()
	st := mgr.SnapshotStatus()
	require.Equal(t, "needs_restart", st.PendingState)
	require.Contains(t, st.PendingReason, "World")
	require.Equal(t, imgDigest, st.LoadedDigest)
	require.Equal(t, 1, prepareCalls)

	// 2. 改 Skill 但校验失败 -> 回报 rejected,继续用旧表,第二次轮询不再重试。
	hookErr = errors.New("bad skill balance")
	tmpBad := filepath.Join(root, "tmp_bad")
	badDigest := writeFakeBundle(t, tmpBad, testSchemaDigest, 3, map[string]string{
		"Skill": `{"v":3}`,
		"World": `{"v":1}`,
	}, map[string]bool{"Skill": true, "World": false})
	require.NoError(t, os.Rename(tmpBad, filepath.Join(bundleRoot, "bundles", badDigest)))
	writePointerDoc(t, bundleRoot, &PointerLine{
		Next: &PointerTarget{ContentDigest: badDigest, Version: 3, Immediate: true},
	})
	mgr.PollOnce()
	mgr.PollOnce()
	st = mgr.SnapshotStatus()
	require.Equal(t, "rejected", st.PendingState)
	require.Contains(t, st.PendingReason, "bad skill balance")
	require.Equal(t, imgDigest, st.LoadedDigest)
	require.Equal(t, 2, prepareCalls)
}

// 表包没同步下来时,sidecar 把这条线记在 sync_errors 里,不写进 lines。
// 这时启动必须失败:用镜像自带的表等于静默退回旧数据,而别的实例还在用热更后的表。
// sync_errors 里记的是别的 schema 线时,本进程照常用镜像自带的表。
func TestStartupRefusesImageFallbackWhenOwnLineFailedToSync(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	bundleRoot := filepath.Join(root, "bundles_root")
	writeFakeBundle(t, imageDir, testSchemaDigest, 1, map[string]string{
		"Skill": `{"v":1}`,
	}, map[string]bool{"Skill": true})

	writeDoc := func(syncErrors map[string]string) {
		raw, err := json.Marshal(PointerDocument{Track: "stable", Lines: map[string]PointerLine{}, SyncErrors: syncErrors})
		require.NoError(t, err)
		require.NoError(t, WriteAtomicFile(filepath.Join(bundleRoot, PointerFilename), raw))
	}
	opts := Options{
		ServiceName:    "test-svc",
		ImageTableDir:  imageDir,
		BundleRoot:     bundleRoot,
		SchemaDigest:   testSchemaDigest,
		LoadTablesFn:   func(string, bool) (*table.TableSet, error) { return &table.TableSet{}, nil },
		CommitTablesFn: func(*table.TableSet) {},
	}

	writeDoc(map[string]string{testSchemaDigest: "download tables/x.tar.gz: status 503"})
	_, err := LoadAtStartup(opts)
	require.ErrorContains(t, err, "not available locally")
	require.ErrorContains(t, err, "status 503")

	writeDoc(map[string]string{"fedcba9876543210": "download failed"})
	mgr, err := LoadAtStartup(opts)
	require.NoError(t, err)
	require.Equal(t, "image", mgr.SnapshotStatus().Source)
}
