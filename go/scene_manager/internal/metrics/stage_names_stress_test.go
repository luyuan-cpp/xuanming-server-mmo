package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestEnterSceneStagesMatchStressSummarize 读 tools/scripts/stress_summarize.ps1 的 $smEnterSceneStages,
// 与 enter_scene_stage_seconds 的 stage 常量逐项同序比对:前 7 个是原 SM stages 表的列(列序不能动),
// home_zone / role_lookup 进 S1 表。两边不一致时对应列静默变成 n/a,只有这里能先红。
// 新增 stage 时同时改这里的清单、Help 文本与脚本数组。
func TestEnterSceneStagesMatchStressSummarize(t *testing.T) {
	want := []string{
		EnterSceneStageDedup, EnterSceneStageSceneResolve, EnterSceneStageReserve, EnterSceneStageUpdateLoc,
		EnterSceneStageRouteGate, EnterSceneStageReleaseDispatch, EnterSceneStageCrossZone,
		EnterSceneStageHomeZone, EnterSceneStageRoleLookup,
	}
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$smEnterSceneStages\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$smEnterSceneStages list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $smEnterSceneStages = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $smEnterSceneStages[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
