package ranked

import (
	"math"
	"path/filepath"
	"testing"

	matchpb "proto/match"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"

	"github.com/stretchr/testify/require"
)

func TestBuildFromRealTables(t *testing.T) {
	tableDir := filepath.Join("..", "..", "..", "..", "generated", "tables")
	ts, err := table.Prepare(tableDir, false)
	require.NoError(t, err)

	cat, err := BuildFromTableSet(ts)
	require.NoError(t, err)
	require.Equal(t, uint32(1), cat.RuleVersion())
	require.Equal(t, uint32(5), cat.BattleConfigId())
	require.Len(t, cat.PresetViews(), 6)
	require.Len(t, cat.Fingerprint(), 64)

	defC, defP := cat.DefaultPresets()
	require.Equal(t, uint32(101), defC)
	require.Equal(t, uint32(203), defP)

	for _, cid := range []uint32{101, 102, 103} {
		for _, pid := range []uint32{201, 202, 203} {
			require.True(t, cat.IsValidPair(cid, pid), "pair (%d,%d) should be valid", cid, pid)
		}
	}
	require.False(t, cat.IsValidPair(201, 101))
	require.False(t, cat.IsValidPair(999, 201))
	require.False(t, cat.IsValidPair(101, 999))

	// 五段边界与未亮段规则(1540 -> 1556 跨段)
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_UNSPECIFIED, cat.TierFor(185000, false))
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_CHUSHI, cat.TierFor(139999, true))
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_QINGFENG, cat.TierFor(140000, true))
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_QINGFENG, cat.TierFor(154000, true))
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_YUNZHANG, cat.TierFor(155600, true))
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_TIANSHU, cat.TierFor(170000, true))
	require.Equal(t, matchpb.RankedTier_RANKED_TIER_WUSHUANG, cat.TierFor(185000, true))

	// 硬上限分差窗口: 永不退化为 +Inf
	curve := cat.RankedCurve()
	require.Equal(t, uint32(100), curve.SpreadPoints(0))
	require.Equal(t, uint32(100), curve.SpreadPoints(29))
	require.Equal(t, uint32(200), curve.SpreadPoints(30))
	require.Equal(t, uint32(200), curve.SpreadPoints(89))
	require.Equal(t, uint32(300), curve.SpreadPoints(90))
	require.Equal(t, uint32(300), curve.SpreadPoints(36000))
	require.False(t, math.IsInf(curve.AnchorCenti(999_999_000), 1))
	require.Equal(t, 30000.0, curve.AnchorCenti(999_999_000))
}

func TestBuildRejectsBrokenPresets(t *testing.T) {
	base := DefaultFallback().Rule()
	badWeight := []*tablepb.RankedPresetTable{
		{Id: 101, RoleKind: RoleKindCharacter, WeightCon: 2, WeightSpi: 2, WeightStr: 2, WeightAgi: 0, SkillIds: []uint32{10002}},
		{Id: 203, RoleKind: RoleKindPet, PetTableId: 2, WeightCon: 2, WeightSpi: 2, WeightStr: 0, WeightAgi: 1, SkillIds: []uint32{10010}, PetEntryIds: []uint32{503}, PetEntryPoints: []uint32{20}},
	}
	_, err := NewFromRows(base, badWeight)
	require.Error(t, err)

	badBudget := []*tablepb.RankedPresetTable{
		{Id: 101, RoleKind: RoleKindCharacter, WeightCon: 1, WeightSpi: 0, WeightStr: 3, WeightAgi: 1, SkillIds: []uint32{10002}},
		{Id: 203, RoleKind: RoleKindPet, PetTableId: 2, WeightCon: 2, WeightSpi: 2, WeightStr: 0, WeightAgi: 1, SkillIds: []uint32{10010}, PetEntryIds: []uint32{503}, PetEntryPoints: []uint32{15}},
	}
	_, err = NewFromRows(base, badBudget)
	require.Error(t, err)
}
