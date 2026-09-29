package main

// 搬库清单(-mode relocate / relocate-abort,docs/design/player-storage-placement.md §9)。
//
// 与合服清单(manifest.go)同一条规矩:**在任何写之前落盘**,之后逐人记录状态;续跑读清单、不重新枚举;
// 撤销(relocate-abort)只认清单里的人与本次的 run_id。它是 Redis 整体丢失时重放落点的依据之一(§4.4):
// 每名切换成功的玩家记下最终的落点值。
//
// 种类与版本:清单带 "kind":"relocate",版本号放在 relocate_version 而**不是** version —— 合服清单的读取方
// (loadManifest)先认 kind 再认 version,旧版工具不认 kind、只认 version,读到这里的 version=0 同样拒绝。
// 两边都不会把一份搬库清单当成合服清单去续跑或撤销。
//
// 落盘时机:每批两次(R1 冻结之后、R5 切换之后),整份原子重写(writeJSONAtomic)。崩溃留下的「清单落后于
// Redis」由续跑的 R1 认领(见 relocate.go 的 decideFreeze):本次 run_id 的冻结值认回冻结态,v+1 的目标值认回
// 已切换。所以清单只需要在冻结之前记下版本号 v。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	relocateManifestKind    = "relocate"
	relocateManifestVersion = 1
)

// 清单里每名玩家的状态。字面量写进清单,发布后不能改。
const (
	relocPending   = "pending"    // 还没冻结
	relocFrozen    = "frozen"     // R1 已冻结("{S}:{v}:frozen:{run}"),R2~R5 未完成
	relocSwitched  = "switched"   // 终态:R5 切换成功,落点 = "{T}:{v+1}"
	relocSkipped   = "skipped"    // 终态:R1 条件不满足(或撤销时还没冻结),本次没有碰过他的落点
	relocUnfrozen  = "unfrozen"   // 终态:R2~R4 失败或被撤销,已 CAS 回 "{S}:{v}"
	relocCASFailed = "cas_failed" // 终态:切换 / 解冻时记录已不是本次的冻结值,保持原状,需要人看
)

// relocFinal 报告状态是否是终态(续跑不再处理)。
func relocFinal(state string) bool {
	switch state {
	case relocSwitched, relocSkipped, relocUnfrozen, relocCASFailed:
		return true
	default:
		return false
	}
}

func validRelocState(state string) bool {
	return state == relocPending || state == relocFrozen || relocFinal(state)
}

// relocatePlayer 是清单里一名玩家的进度。
type relocatePlayer struct {
	PlayerID uint64 `json:"player_id"`
	State    string `json:"state"`
	// HomeZone:冻结时读到的 home_zone(R2 按它拼 db_task topic)。冻结期间合服被互斥挡住,它不会变。
	HomeZone uint32 `json:"home_zone,omitempty"`
	// Version:冻结时的落点版本 v(无记录按 1)。解冻回 "{S}:{v}",切换成 "{T}:{v+1}";续跑据此认领已切换的人。
	Version uint64 `json:"version,omitempty"`
	// ColdCopyRows:R3 在 T 里删掉的该玩家旧行数(冷副本,P-1)。
	ColdCopyRows int `json:"cold_copy_rows,omitempty"`
	// Final:本次留给他的稳定态落点值(switched / unfrozen),Redis 丢失时照它重放。
	Final  string `json:"final,omitempty"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at,omitempty"` // RFC3339,最后一次状态变化
}

// relocateManifest 是一次搬库的对象清单 + 进度。
type relocateManifest struct {
	Kind            string `json:"kind"`
	RelocateVersion int    `json:"relocate_version"`
	// RunID 写进冻结值("{S}:{v}:frozen:{run_id}"),必须满足 validRunID;续跑 / 撤销按它认领冻结记录。
	RunID           string `json:"run_id"`
	SourceStorage   uint32 `json:"source_storage"`
	TargetStorage   uint32 `json:"target_storage"`
	StartedAt       string `json:"started_at"`
	StartedAtUnixMs int64  `json:"started_at_unix_ms"`
	Operator        string `json:"operator"`
	// TopicGeneration:R2 拼 db_task topic 用的世代号(-kafka-topic-generation)。续跑必须一致,
	// 否则等的是另一个 topic 上的排序锁。
	TopicGeneration uint32 `json:"topic_generation"`
	// Tables:R3 拷贝 / R4 比对的玩家表(S 库里发现的,T 库必须列名对齐)。续跑时表集合变了即拒绝。
	Tables []string `json:"tables"`
	// CapabilityZones:动手前确认过能力标记的 zone(-db-capability-zones ∪ 清单玩家的 home_zone)。
	CapabilityZones []uint32 `json:"capability_zones"`
	// DryRun:dry-run 写出的预览(<path>.dryrun.json),只给人看,任何模式都拒读。
	DryRun bool `json:"dry_run,omitempty"`
	// Aborted:-mode relocate-abort 跑过。之后不能再续跑(要搬就用新清单另起一次)。
	Aborted bool             `json:"aborted,omitempty"`
	Players []relocatePlayer `json:"players"`
}

func newRelocateManifest(runID string, s, t uint32, generation uint32, tables []string, players []uint64,
	operator string, now time.Time) *relocateManifest {
	m := &relocateManifest{
		Kind:            relocateManifestKind,
		RelocateVersion: relocateManifestVersion,
		RunID:           runID,
		SourceStorage:   s,
		TargetStorage:   t,
		StartedAt:       now.UTC().Format(time.RFC3339),
		StartedAtUnixMs: now.UnixMilli(),
		Operator:        operator,
		TopicGeneration: generation,
		Tables:          append([]string(nil), tables...),
		Players:         make([]relocatePlayer, 0, len(players)),
	}
	for _, id := range sortedUint64(players) {
		m.Players = append(m.Players, relocatePlayer{PlayerID: id, State: relocPending})
	}
	return m
}

// newRelocateRunID 生成搬库的 run_id。它会被写进冻结值,只能含 [A-Za-z0-9_-](validRunID)。
func newRelocateRunID(s, t uint32, now time.Time) string {
	return fmt.Sprintf("reloc-%d-%d-%s-%s", s, t, now.UTC().Format("20060102T150405Z"), randomToken()[:8])
}

// defaultRelocateManifestPath 生成 ./relocate_<S>_to_<T>_<ts>.json。
func defaultRelocateManifestPath(s, t uint32, now time.Time) string {
	return fmt.Sprintf("relocate_%d_to_%d_%s.json", s, t, now.UTC().Format("20060102T150405Z"))
}

func saveRelocateManifest(path string, m *relocateManifest) error {
	return writeJSONAtomic(path, m)
}

// loadRelocateManifest 读搬库清单。不存在返回 (nil, nil)(首跑的正常状态)。拒读:dry-run 预览(按后缀、按内容)、
// 不是搬库清单的文件、不认识的版本、不认识的逐人状态 —— 宁可停下来,也不拿读不懂的清单去解冻或切换。
func loadRelocateManifest(path string) (*relocateManifest, error) {
	if strings.HasSuffix(path, dryRunManifestSuffix) {
		return nil, fmt.Errorf("-manifest-path %s ends with %s, which is reserved for dry-run previews — "+
			"pass the manifest written by the -apply run instead", path, dryRunManifestSuffix)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read relocate manifest %s: %w", path, err)
	}
	var m relocateManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse relocate manifest %s: %w", path, err)
	}
	if m.Kind != relocateManifestKind {
		return nil, fmt.Errorf("%s is not a relocate manifest (kind=%q; a merge manifest?) — pass the file written by -mode relocate", path, m.Kind)
	}
	if m.RelocateVersion != relocateManifestVersion {
		return nil, fmt.Errorf("relocate manifest %s has relocate_version %d, this build understands %d — refuse to guess",
			path, m.RelocateVersion, relocateManifestVersion)
	}
	if m.DryRun {
		return nil, fmt.Errorf("relocate manifest %s is a dry-run preview (dry_run=true): it records who a rehearsal WOULD "+
			"have frozen, not what any run wrote", path)
	}
	if !validRunID(m.RunID) {
		return nil, fmt.Errorf("relocate manifest %s has run_id %q, which cannot appear in a frozen placement value", path, m.RunID)
	}
	for _, p := range m.Players {
		if !validRelocState(p.State) {
			return nil, fmt.Errorf("relocate manifest %s: player %d has unknown state %q — refuse to guess", path, p.PlayerID, p.State)
		}
	}
	return &m, nil
}

// relocStateCounts 按状态计数(日志与退出码用)。
func (m *relocateManifest) relocStateCounts() map[string]int {
	out := map[string]int{}
	for _, p := range m.Players {
		out[p.State]++
	}
	return out
}

// relocSummary 是一行人读的计数摘要,状态顺序固定。
func (m *relocateManifest) relocSummary() string {
	c := m.relocStateCounts()
	return fmt.Sprintf("players=%d switched=%d unfrozen=%d skipped=%d cas_failed=%d frozen=%d pending=%d",
		len(m.Players), c[relocSwitched], c[relocUnfrozen], c[relocSkipped], c[relocCASFailed], c[relocFrozen], c[relocPending])
}

// relocSample 返回处于 state 的前 unmappedSampleSize 名玩家「id: 原因」,进收尾文案。
func (m *relocateManifest) relocSample(state string) []string {
	var out []string
	for _, p := range m.Players {
		if p.State != state {
			continue
		}
		out = append(out, fmt.Sprintf("%d: %s", p.PlayerID, p.Reason))
		if len(out) >= unmappedSampleSize {
			break
		}
	}
	return out
}
