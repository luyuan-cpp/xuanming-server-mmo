package pointer

import (
	"os"
	"path/filepath"
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
