package main

// 合服清单(manifest):在**写第一个字节之前**落盘的那份「这次合服到底动了谁」。
//
// 为什么必须有:
//   合服的四个写入面(zone 库玩家行 / guild MySQL / guild_rank ZSET / mapping)
//   分布在两个 MySQL 库和三个 Redis DB 上,没有任何跨面事务。跑到一半失败时,
//   「已经改了哪些」这件事只存在于日志里 —— 而日志既不结构化也不保证落盘。
//   更要命的是重跑:remap 之后 collectPlayerIDsWithHomeZone 再也找不到源区玩家
//   (它们的 home_zone 已经是目标区),第二次跑会得到空集合并「成功」地什么都不做,
//   把「合了一半」当成「合完了」。
//
//   所以:先把玩家 id / 公会 id / ZSET 成员与分数写进清单,再开始写。之后的重跑
//   读清单而不是重新扫描;撤销(-mode unmerge)也只认清单里列出的那些对象,
//   绝不会碰目标区原住民。
//
// 写入方式:temp 文件 + rename(同目录),避免进程被 kill 时留下半个 JSON。
// 每完成一步就 markStep + 落盘一次(几十 KB 级别,代价可以忽略)。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// manifestVersion 变更规则:只要字段语义变了就 +1,loadManifest 拒绝不认识的版本。
// 宁可让运维手工确认,也不能让新工具误读旧清单去「撤销」。
const manifestVersion = 1

// manifest 里的步骤名。清单是幂等重跑与撤销的唯一依据,所以步骤名一旦发布
// 就不能改字面量(旧清单里存的是旧名字)。
const (
	stepPlayerRows    = "player_rows"     // zone_src_db → zone_dst_db 玩家行拷贝
	stepPlayerBlobs   = "player_blobs"    // 跨 data Redis 的 player:{id}:* 拷贝
	stepGuildMySQL    = "guild_mysql"     // guild.zone_id 改写 + 缓存失效
	stepGuildRank     = "guild_rank"      // guild_rank:zone ZSET 合并
	stepPlayerMapping = "player_mapping"  // player:zone:{id} 改写
	stepHotState      = "hot_state"       // scene_manager 热状态清理
	stepPostMerge     = "post_merge_flag" // player_merge_notice 打标
)

// manifestRankMember 是 guild_rank:zone:{src} 的一个成员快照。撤销时按它
// ZADD 回源区并从目标区 ZREM —— 分数必须原样带回,不能按 MySQL score 现算,
// 因为 ZSET 是**可重建缓存**,合服窗口里两者可能已经不同步。
type manifestRankMember struct {
	Member string  `json:"member"`
	Score  float64 `json:"score"`
}

// manifestStep 记录一步的完成情况。Done=false 的步骤在重跑时会被重做;
// Done=true 的被跳过(每一步本身都是幂等的,跳过只是省时间与省风险)。
type manifestStep struct {
	Done   bool   `json:"done"`
	At     string `json:"at,omitempty"`     // RFC3339,完成时刻
	Detail string `json:"detail,omitempty"` // 人读的一行摘要(计数等)
}

// mergeManifest 是整次合服的对象清单 + 进度。
type mergeManifest struct {
	Version         int    `json:"version"`
	RunID           string `json:"run_id"` // 与 merge:in_progress 标记里的 run_id 同一个值
	SourceZone      uint32 `json:"source_zone"`
	TargetZone      uint32 `json:"target_zone"`
	StartedAt       string `json:"started_at"`
	StartedAtUnixMs int64  `json:"started_at_unix_ms"`
	Operator        string `json:"operator"` // host/pid,排障时对得上是谁跑的

	// ── 对象清单(在第一次写之前定稿,之后只读)────────────────────
	PlayerIDs   []uint64             `json:"player_ids"`
	GuildIDs    []uint64             `json:"guild_ids"`
	RankMembers []manifestRankMember `json:"rank_members"`
	// Tables:实际拷贝的玩家表(zone 库内的表名,不带库前缀)。撤销时按这个
	// 列表删目标库的行 —— 绝不按「当前发现的表」删,否则新加的表会被误删。
	Tables []string `json:"tables"`

	Steps map[string]manifestStep `json:"steps"`
}

func newMergeManifest(runID string, src, dst uint32, operator string, now time.Time) *mergeManifest {
	return &mergeManifest{
		Version:         manifestVersion,
		RunID:           runID,
		SourceZone:      src,
		TargetZone:      dst,
		StartedAt:       now.UTC().Format(time.RFC3339),
		StartedAtUnixMs: now.UnixMilli(),
		Operator:        operator,
		Steps:           map[string]manifestStep{},
	}
}

// stepDone 报告某一步是否已经完成过(nil 清单一律 false:没有清单就全做一遍)。
func (m *mergeManifest) stepDone(name string) bool {
	if m == nil {
		return false
	}
	return m.Steps[name].Done
}

// markStep 记一步完成。调用方随后必须 save 一次 —— 没落盘的进度等于没有。
func (m *mergeManifest) markStep(name, detail string) {
	if m == nil {
		return
	}
	if m.Steps == nil {
		m.Steps = map[string]manifestStep{}
	}
	m.Steps[name] = manifestStep{Done: true, At: time.Now().UTC().Format(time.RFC3339), Detail: detail}
}

// defaultManifestPath 生成 ./merge_<src>_to_<dst>_<ts>.json。
func defaultManifestPath(src, dst uint32, now time.Time) string {
	return fmt.Sprintf("merge_%d_to_%d_%s.json", src, dst, now.UTC().Format("20060102T150405Z"))
}

// saveManifest 原子落盘:同目录写 .tmp 再 rename。**不能**直接覆盖写 —— 进程
// 在 write 中途被 kill 会留下一个语法错误的 JSON,而那正是最需要它的时刻。
func saveManifest(path string, m *mergeManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// Windows 上 rename 到已存在的文件会失败,先删旧的。断电窗口极小,
	// 且 .tmp 仍在,人工可恢复。
	_ = os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// loadManifest 读清单。文件不存在返回 (nil, nil) —— 「没有清单」是首次运行的
// 正常状态,不是错误。版本不认识 / JSON 坏了都是错误:宁可停下来。
func loadManifest(path string) (*mergeManifest, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m mergeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("manifest %s has version %d, this build understands %d — refuse to guess",
			path, m.Version, manifestVersion)
	}
	if m.Steps == nil {
		m.Steps = map[string]manifestStep{}
	}
	return &m, nil
}

// validateManifestForRun 检查清单与本次调用的 src/dst 一致。不一致 = 运维复制
// 粘贴错了路径,继续跑会把 A→B 的清单当成 C→D 的进度,必须硬失败。
func validateManifestForRun(m *mergeManifest, src, dst uint32) error {
	if m == nil {
		return nil
	}
	if m.SourceZone != src || m.TargetZone != dst {
		return fmt.Errorf("manifest is for zone %d→%d but this run is %d→%d",
			m.SourceZone, m.TargetZone, src, dst)
	}
	return nil
}

// sortedUint64 返回升序去重副本。清单里的 id 一律有序去重,这样
//   - 两次运行的清单可以直接 diff;
//   - 分批 IN (...) 的批次边界稳定,失败重跑落在同一批。
func sortedUint64(in []uint64) []uint64 {
	if len(in) == 0 {
		return nil
	}
	cp := append([]uint64(nil), in...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	out := cp[:1]
	for _, v := range cp[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
