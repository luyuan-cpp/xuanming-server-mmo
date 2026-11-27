package pointer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKeysAndPruneUnusedBundles(t *testing.T) {
	require.Equal(t, "/mmorpg/tables/pointer/stable/", PointerPrefix("stable"))
	require.Equal(t, "/mmorpg/tables/pointer/canary/0123456789abcdef", PointerKey("canary", "0123456789abcdef"))
	require.Equal(t, "/mmorpg/tables/status/stable/scene-0", StatusKey("stable", "scene-0"))
	require.True(t, ValidTrack("stable"))
	require.True(t, ValidTrack("canary"))
	require.False(t, ValidTrack("zone-1"))

	dir := t.TempDir()
	keepPtr := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	keepFresh := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	staleOld := filepath.Join(dir, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	for _, p := range []string{keepPtr, keepFresh, staleOld} {
		require.NoError(t, os.MkdirAll(p, 0o755))
	}

	now := time.Now()
	oldTime := now.Add(-15 * time.Minute)
	require.NoError(t, os.Chtimes(keepPtr, oldTime, oldTime))
	require.NoError(t, os.Chtimes(staleOld, oldTime, oldTime))

	refs := ReferencedDigests(&Document{
		Track: "stable",
		Lines: map[string]Line{
			"0123456789abcdef": {Current: &Target{ContentDigest: filepath.Base(keepPtr)}},
		},
	}, nil)

	removed, err := PruneUnusedBundles(dir, refs, 10*time.Minute, now)
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	_, err = os.Stat(keepPtr)
	require.NoError(t, err)
	_, err = os.Stat(keepFresh)
	require.NoError(t, err)
	_, err = os.Stat(staleOld)
	require.True(t, os.IsNotExist(err))
}

// 卷的大小有上限(sizeLimit 1Gi,超了整个 Pod 被驱逐):保留期内没人用的包只留最新两个,
// sidecar 中途被杀留下的临时文件过了保留期也要删;被引用的包、正在用的新临时文件不能动。
func TestPruneCapsRecentUnreferencedBundlesAndRemovesOrphanedTempFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	makeAt := func(name string, age time.Duration, isDir bool) string {
		p := filepath.Join(dir, name)
		if isDir {
			require.NoError(t, os.MkdirAll(p, 0o755))
		} else {
			require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
		}
		at := now.Add(-age)
		require.NoError(t, os.Chtimes(p, at, at))
		return p
	}
	digest := func(c byte) string { return strings.Repeat(string(c), 64) }

	referencedFresh := makeAt(digest('a'), 1*time.Minute, true)
	newest := makeAt(digest('b'), 2*time.Minute, true)
	second := makeAt(digest('c'), 3*time.Minute, true)
	third := makeAt(digest('d'), 4*time.Minute, true)
	fourth := makeAt(digest('e'), 5*time.Minute, true)
	orphanExtract := makeAt(".extract-111", 15*time.Minute, true)
	orphanDownload := makeAt(".download-222.tar.gz", 15*time.Minute, false)
	activeExtract := makeAt(".extract-333", 10*time.Second, true)

	removed, err := PruneUnusedBundles(dir, map[string]bool{digest('a'): true}, 10*time.Minute, now)
	require.NoError(t, err)
	require.Equal(t, 4, removed)

	for _, kept := range []string{referencedFresh, newest, second, activeExtract} {
		_, err := os.Stat(kept)
		require.NoError(t, err, "must keep %s", filepath.Base(kept))
	}
	for _, gone := range []string{third, fourth, orphanExtract, orphanDownload} {
		_, err := os.Stat(gone)
		require.True(t, os.IsNotExist(err), "must remove %s", filepath.Base(gone))
	}
}

// POINTER.json 的往返:sync_errors 没有内容时不写出,有内容时业务进程按 "sync_errors" 这个键读;
// sidecar 重启时用 ReadDocument 把上一次落盘的线读回来。
func TestDocumentRoundTripCarriesSyncErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), PointerFilename)

	missing, err := ReadDocument(path)
	require.NoError(t, err)
	require.Nil(t, missing, "a pod's first start has no previous document")

	clean := Document{Track: "stable", Lines: map[string]Line{
		"0123456789abcdef": {Current: &Target{ContentDigest: "aa", Version: 3}},
	}}
	raw, err := json.Marshal(clean)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "sync_errors")

	failing := clean
	failing.SyncErrors = map[string]string{"fedcba9876543210": "download tables/x.tar.gz: status 503"}
	raw, err = json.Marshal(failing)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"sync_errors":{"fedcba9876543210":"download tables/x.tar.gz: status 503"}`)
	require.NoError(t, WriteAtomicFile(path, raw))

	got, err := ReadDocument(path)
	require.NoError(t, err)
	require.Equal(t, failing, *got)

	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err = ReadDocument(path)
	require.Error(t, err)

	require.Len(t, TrimSyncError(strings.Repeat("x", MaxSyncErrorChars+50)), MaxSyncErrorChars)
	require.Equal(t, "short", TrimSyncError("short"))
}
