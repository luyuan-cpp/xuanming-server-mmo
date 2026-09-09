package snowflakealloc

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// selectionVectors 是 testdata/selection_vectors.json 的结构(语言中立,C++ 侧同一份)。
type selectionVectors struct {
	Cases []struct {
		Name             string            `json:"name"`
		Used             []uint64          `json:"used"`
		Watermarks       map[string]uint64 `json:"watermarks"`
		LegacyWatermarks map[string]uint64 `json:"legacy_watermarks"`
		Now              uint64            `json:"now"`
		QuarantineSec    uint64            `json:"quarantine_sec"`
		MaxSlot          uint64            `json:"max_slot"`
		ExpectSlot       *uint64           `json:"expect_slot"`
	} `json:"cases"`
}

func slotMap(t *testing.T, m map[string]uint64) map[uint64]uint64 {
	t.Helper()
	out := make(map[uint64]uint64, len(m))
	for k, v := range m {
		s, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			t.Fatalf("bad slot key %q: %v", k, err)
		}
		out[s] = v
	}
	return out
}

// 表驱动:每条向量 = (used, wm, now, Q, maxSlot) → 期望槽(null = fail-closed)。
// C++ 的 SnowflakeSlotClient 必须用同一个文件得到同样的结果(设计稿 §3.7 第 6 条)。
func TestSelectSlot_Vectors(t *testing.T) {
	data, err := os.ReadFile("testdata/selection_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v selectionVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 8 {
		t.Fatalf("向量太少: %d", len(v.Cases))
	}
	required := map[string]bool{
		"all_quarantined_fail_closed":            false,
		"never_used_wins_over_oldest_used":       false,
		"tie_break_smallest_slot":                false,
		"legacy_only_watermark_quarantines_slot": false,
	}
	for _, c := range v.Cases {
		if _, ok := required[c.Name]; ok {
			required[c.Name] = true
		}
		t.Run(c.Name, func(t *testing.T) {
			used := make(map[uint64]bool, len(c.Used))
			for _, s := range c.Used {
				used[s] = true
			}
			wm := mergeWatermarks(slotMap(t, c.Watermarks), slotMap(t, c.LegacyWatermarks))
			got, ok := selectSlot(used, wm, c.Now, c.QuarantineSec, c.MaxSlot)
			switch {
			case c.ExpectSlot == nil && ok:
				t.Fatalf("期望无候选(fail-closed),却选出了 slot %d", got)
			case c.ExpectSlot != nil && !ok:
				t.Fatalf("期望 slot %d,却报无候选", *c.ExpectSlot)
			case c.ExpectSlot != nil && got != *c.ExpectSlot:
				t.Fatalf("选出 slot %d,期望 %d", got, *c.ExpectSlot)
			}
		})
	}
	for name, seen := range required {
		if !seen {
			t.Fatalf("向量文件缺少必备用例 %q", name)
		}
	}
}

// mergeWatermarks 按槽取 max,nil dst 可用。
func TestMergeWatermarks_TakesMax(t *testing.T) {
	got := mergeWatermarks(nil, map[uint64]uint64{1: 10, 2: 20})
	got = mergeWatermarks(got, map[uint64]uint64{1: 5, 2: 30, 3: 1})
	if got[1] != 10 || got[2] != 30 || got[3] != 1 {
		t.Fatalf("merge=%v", got)
	}
}
