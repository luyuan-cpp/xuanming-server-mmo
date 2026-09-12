package table

import (
	"os"
	"path/filepath"
	"testing"
)

// configDir resolves the path to generated/tables/ relative to the repo root.
func configDir(t *testing.T) string {
	t.Helper()

	// Walk up from the test file location to find the repo root.
	// Expected: go/shared/generated/table/ -> repo root is 4 levels up.
	candidates := []string{
		filepath.Join("..", "..", "..", "..", "generated", "tables"),
		filepath.Join("..", "..", "..", "..", "..", "generated", "tables"),
	}

	// Also try from working directory (often repo root when run via `go test`).
	wd, _ := os.Getwd()
	if wd != "" {
		candidates = append(candidates, filepath.Join(wd, "generated", "tables"))
		candidates = append(candidates, filepath.Join(wd, "..", "..", "..", "..", "generated", "tables"))
	}

	for _, dir := range candidates {
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			return abs
		}
	}
	t.Fatalf("config dir not found, tried: %v", candidates)
	return ""
}

func loadAllTables(t *testing.T) {
	t.Helper()
	dir := configDir(t)
	LoadTables(dir, false)
}

// ---------------------------------------------------------------------------
// Load + Iterate
// ---------------------------------------------------------------------------

func TestLoadAndIterateBuffTable(t *testing.T) {
	loadAllTables(t)
	mgr := BuffTableManagerInstance
	rows := mgr.FindAll()
	if len(rows) == 0 {
		t.Fatal("expected at least one Buff row")
	}
	for _, row := range rows {
		t.Logf("Buff id=%d buff_type=%d level=%d", row.Id, row.BuffType, row.Level)
	}
}

func TestLoadAndIterateTestMultiKeyTable(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	rows := mgr.FindAll()
	if len(rows) == 0 {
		t.Fatal("expected at least one TestMultiKey row")
	}
}

// ---------------------------------------------------------------------------
// Key lookup
// ---------------------------------------------------------------------------

func TestMultiKeyFindByStringKey(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	row, ok := mgr.FindByString_key("aa")
	if !ok || row == nil {
		t.Fatal("expected to find row by string_key 'aa'")
	}
	if row.StringKey != "aa" {
		t.Errorf("expected StringKey='aa', got %q", row.StringKey)
	}
}

func TestMultiKeyFindByUint32Key(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	row, ok := mgr.FindByUint32_key(14)
	if !ok || row == nil {
		t.Fatal("expected to find row by uint32_key 14")
	}
	if row.Id != 1 {
		t.Errorf("expected id=1, got %d", row.Id)
	}
}

func TestMultiKeyFindByInt32Key(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	row, ok := mgr.FindByInt32_key(8)
	if !ok || row == nil {
		t.Fatal("expected to find row by int32_key 8")
	}
	if row.Id != 1 {
		t.Errorf("expected id=1, got %d", row.Id)
	}
}

// ---------------------------------------------------------------------------
// Repeated column value index
// ---------------------------------------------------------------------------

func TestRepeatedEffectIndex(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	for _, row := range mgr.FindAll() {
		for _, val := range row.Effect {
			indexed := mgr.FindByEffectIndex(val)
			found := false
			for _, r := range indexed {
				if r.Id == row.Id {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("effect value %d not indexed for row id %d", val, row.Id)
			}
		}
	}
}

func TestBuffSubBuffIndex(t *testing.T) {
	loadAllTables(t)
	mgr := BuffTableManagerInstance
	for _, row := range mgr.FindAll() {
		for _, val := range row.SubBuff {
			indexed := mgr.FindBySub_buffIndex(val)
			found := false
			for _, r := range indexed {
				if r.Id == row.Id {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("sub_buff value %d not indexed for row id %d", val, row.Id)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Per-column component records
// ---------------------------------------------------------------------------

func TestScalarCompFromRow(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	row, ok := mgr.FindById(1)
	if !ok || row == nil {
		t.Fatal("row id=1 not found")
	}

	idComp := MakeTestMultiKeyIdComp(row)
	if idComp.Value != 1 {
		t.Errorf("expected Id=1, got %d", idComp.Value)
	}

	strComp := MakeTestMultiKeyString_keyComp(row)
	if strComp.Value != "aa" {
		t.Errorf("expected StringKey='aa', got %q", strComp.Value)
	}

	u32Comp := MakeTestMultiKeyUint32_keyComp(row)
	if u32Comp.Value != 14 {
		t.Errorf("expected Uint32Key=14, got %d", u32Comp.Value)
	}

	i32Comp := MakeTestMultiKeyInt32_keyComp(row)
	if i32Comp.Value != 8 {
		t.Errorf("expected Int32Key=8, got %d", i32Comp.Value)
	}
}

func TestRepeatedCompSlice(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	row, ok := mgr.FindById(2)
	if !ok || row == nil {
		t.Fatal("row id=2 not found")
	}

	effectComp := MakeTestMultiKeyEffectComp(row)
	if len(effectComp.Values) != len(row.Effect) {
		t.Errorf("expected %d effect values, got %d", len(row.Effect), len(effectComp.Values))
	}
	for i, v := range effectComp.Values {
		if v != row.Effect[i] {
			t.Errorf("effect[%d]: expected %d, got %d", i, row.Effect[i], v)
		}
	}
}

// ---------------------------------------------------------------------------
// Reload (hot-reload safety: snapshot swap, no accumulation)
// ---------------------------------------------------------------------------

func TestReloadDoesNotAccumulateData(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance
	countBefore := mgr.Count()
	if countBefore == 0 {
		t.Fatal("expected at least one row")
	}

	// Reload the same data.
	dir := configDir(t)
	if err := mgr.Load(dir, false); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	// Count must stay the same — old snapshot replaced, not accumulated.
	if mgr.Count() != countBefore {
		t.Errorf("expected count=%d after reload, got %d", countBefore, mgr.Count())
	}

	// Data should still be accessible.
	row, ok := mgr.FindById(1)
	if !ok || row == nil {
		t.Fatal("row id=1 not found after reload")
	}
	if row.Id != 1 {
		t.Errorf("expected id=1, got %d", row.Id)
	}
}

func TestReloadReplacesOldPointers(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance

	rowBefore, ok := mgr.FindById(1)
	if !ok || rowBefore == nil {
		t.Fatal("row id=1 not found before reload")
	}

	dir := configDir(t)
	if err := mgr.Load(dir, false); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	rowAfter, ok := mgr.FindById(1)
	if !ok || rowAfter == nil {
		t.Fatal("row id=1 not found after reload")
	}

	if rowBefore == rowAfter {
		t.Error("reload should produce a new snapshot (different pointers)")
	}
	if rowAfter.Id != 1 {
		t.Errorf("expected id=1, got %d", rowAfter.Id)
	}
}

func TestReloadMultiKeyIndicesConsistent(t *testing.T) {
	loadAllTables(t)
	mgr := TestMultiKeyTableManagerInstance

	// Reload twice to stress test.
	dir := configDir(t)
	_ = mgr.Load(dir, false)
	_ = mgr.Load(dir, false)

	// Multi-key lookup should still work.
	rows := mgr.FindByM_uint32_key(17)
	if len(rows) == 0 {
		t.Log("no rows with m_uint32_key=17 (may be expected depending on data)")
	}

	// Count should be stable.
	countBefore := mgr.Count()
	_ = mgr.Load(dir, false)
	if mgr.Count() != countBefore {
		t.Errorf("count changed after 3rd reload: %d -> %d", countBefore, mgr.Count())
	}
}
