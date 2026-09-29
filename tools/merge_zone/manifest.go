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
//
// dry-run 不碰 -manifest-path(2026-09-28,player-storage-placement.md §12 A3):它把预览写到
// <path>.dryrun.json,并在内容里标 dry_run=true。此前 dry-run 直接写 -manifest-path,T-1 彩排用了
// 与 T-0 相同的路径时,T-0 的 -apply 会把彩排那一刻的玩家名单当成「续跑」读进来、不再重扫 ——
// 彩排之后新进源区的人不在名单里,改映射时被整批漏掉。预览只给人看,续跑 / 撤销 / 合服后验证一律拒读。
//
// 玩家行模式(2026-09-28,player-storage-placement.md §10):清单记下 pin / copy 与清单玩家合服前已有的
// 落点记录。pin 模式不拷行、不记 Tables,「玩家行这一步」换成 pin_placement(钉落点)。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// manifestVersion 变更规则:只要字段语义变了就 +1,loadManifest 拒绝不认识的版本。
// 宁可让运维手工确认,也不能让新工具误读旧清单去「撤销」。
const manifestVersion = 1

// manifest 里的步骤名。清单是幂等重跑与撤销的唯一依据,所以步骤名一旦发布
// 就不能改字面量(旧清单里存的是旧名字)。
const (
	stepPlayerRows    = "player_rows"     // zone_src_db → zone_dst_db 玩家行拷贝(copy 模式)
	stepPinPlacement  = "pin_placement"   // 清单玩家无记录者钉 player:placement = "{src}:1"(pin 模式,取代拷行)
	stepPlayerBlobs   = "player_blobs"    // 跨 data Redis 的 player:{id}:* 拷贝
	stepGuildMySQL    = "guild_mysql"     // guild.zone_id 改写 + 缓存失效
	stepTradeMySQL    = "trade_mysql"     // trade_listing.market_zone 改写(聚宝斋,trade_step.go)
	stepGuildRank     = "guild_rank"      // guild_rank:zone ZSET 合并
	stepPlayerMapping = "player_mapping"  // player:zone:{id} 改写
	stepHotState      = "hot_state"       // scene_manager 热状态清理
	stepPostMerge     = "post_merge_flag" // player_merge_notice 打标
)

// 玩家行模式(-player-rows-mode,player-storage-placement.md §10)。字面量写进清单,发布后不能改。
//
//	pin  :合服只改归属(home_zone),不搬玩家行 —— 改映射之前给无落点记录的清单玩家钉 "{src}:1",
//	       有效落点保持源区库;要求相关 zone 的 go/db 已按落点选库(能力标记)。默认。
//	copy :旧语义,把玩家行从 zone_src_db 拷到 zone_dst_db;只处理没有落点记录的玩家(有记录者的行在
//	       其落点库,不拷)。兼容还没升级的 go/db。
const (
	playerRowsModePin  = "pin"
	playerRowsModeCopy = "copy"
)

// parsePlayerRowsMode 校验 -player-rows-mode 的取值。
func parsePlayerRowsMode(s string) (string, error) {
	switch s {
	case playerRowsModePin, playerRowsModeCopy:
		return s, nil
	default:
		return "", fmt.Errorf("-player-rows-mode %q: expected %q or %q", s, playerRowsModePin, playerRowsModeCopy)
	}
}

// manifestPlacement 是一名清单玩家在合服动手之前就已有的落点记录(原值)。
type manifestPlacement struct {
	PlayerID uint64 `json:"player_id"`
	Value    string `json:"value"`
}

// placementIDs 返回 ps 里的玩家 id(保持顺序)。
func placementIDs(ps []manifestPlacement) []uint64 {
	out := make([]uint64, len(ps))
	for i, p := range ps {
		out[i] = p.PlayerID
	}
	return out
}

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
	// Kind 区分清单种类:合服清单留空(历史清单都没有这个字段);搬库清单(relocate_manifest.go)写 "relocate"。
	// 只用于读的一侧拒绝拿错种类的文件,合服清单不写它。
	Kind            string `json:"kind,omitempty"`
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
	// RankMembersUnwritten 为 true = RankMembers 只是清单阶段读的源榜快照,还没交给过任何一次步骤 4 的写
	// (步骤 4 在 MULTI/EXEC 之前把撤销依据落盘时清成 false)。步骤 4 锁内重读发现源榜已不在时据此决定撤销依据:
	// 为 true 时快照里的成员从没进过目标榜(源榜是被 guild 服合法清空的,可能已解散),撤销依据记为空;
	// 为 false 时快照就是上一次并进目标榜的那一份,原样保留(见 guild_step.go chooseRankWrite)。
	// 新增字段不升 manifestVersion:旧清单没有它 = false = 「可能写过」—— 旧版步骤 4 写的正是清单快照,
	// 保留它作撤销依据与旧行为一致。
	RankMembersUnwritten bool `json:"rank_members_unwritten,omitempty"`
	// Tables:实际拷贝的玩家表(zone 库内的表名,不带库前缀)。撤销时按这个
	// 列表删目标库的行 —— 绝不按「当前发现的表」删,否则新加的表会被误删。
	Tables []string `json:"tables"`
	// TradeListingIDs:改写前 market_zone=src 的聚宝斋商品 id(trade_step.go)。撤销只把其中
	// 当前 market_zone=dst 的改回 src。它是本版本新增字段,**不升 manifestVersion**:既有
	// 字段语义没变;旧清单没有它 = 当时的工具没碰过 trade,读出来为空,撤销自然跳过,
	// 续跑时 trade 步骤也会按「未完成」重新收集。
	TradeListingIDs []uint64 `json:"trade_listing_ids"`

	// DryRun 为 true = 这是 dry-run 写出的预览(<path>.dryrun.json):记的是「dry-run 那一刻会动谁」,
	// 不是任何一次写入的凭据,loadRunManifest 一律拒读。新增字段不升 manifestVersion:旧清单没有它,
	// 读出来是 false,语义与此前相同。代价:旧版工具 dry-run 直接写在 -manifest-path 上的那份认不出来,
	// 升级后第一次 -apply 之前,要把旧版彩排留在同一路径上的清单删掉。
	DryRun bool `json:"dry_run,omitempty"`

	// PlayerRowsMode 是这次合服的玩家行模式(pin / copy,player-storage-placement.md §10.1 第 7 条)。
	// 续跑必须同模式;-verify-merged 与 -mode unmerge 按它判断玩家行在哪、撤销要不要删行。
	// 本字段之前写的清单没有它 = copy(那时只有拷行这一种做法)。
	// 新增字段不升 manifestVersion:既有字段的语义没变,旧清单按 copy 读与当时的行为一致。旧版工具读 pin 清单
	// 会忽略它 —— pin 清单不记 Tables、不标 player_rows,旧版续跑最多把行多拷一份成冷副本(有效落点仍是
	// 钉住的源区库),旧版撤销只删与源库逐字节相同的目标行(pin 模式没往目标库拷过),都不会碰活数据。
	PlayerRowsMode string `json:"player_rows_mode,omitempty"`
	// PlacementScanned:清单阶段已经扫过清单玩家的落点记录,PlacementExisting 是那一刻的结果。
	// false(本字段之前的清单)= 没扫过,续跑时补扫并记下,不做前后比对。
	PlacementScanned bool `json:"placement_scanned,omitempty"`
	// PlacementExisting:清单玩家里合服动手之前就已有落点记录的人与原值(pin 模式不含值等于 "{src}:1" 的,
	// 那与本次要钉的值相同)。copy 模式据此跳过拷行;两种模式都是 Redis 整体丢失时按清单重放落点的依据
	// (§4.4):清单玩家的落点 = 这里的值,不在这里的 = "{src}:1"(pin)/ 无记录(copy)。
	PlacementExisting []manifestPlacement `json:"placement_existing,omitempty"`

	Steps map[string]manifestStep `json:"steps"`
}

// rowsMode 返回清单记录的玩家行模式;没有记录(本字段之前写的清单)按 copy。
func (m *mergeManifest) rowsMode() string {
	if m == nil || m.PlayerRowsMode == "" {
		return playerRowsModeCopy
	}
	return m.PlayerRowsMode
}

// rowsStep 返回这个模式下「玩家行这一步」的步骤名:copy 是拷行,pin 是钉落点。
func rowsStepFor(mode string) string {
	if mode == playerRowsModePin {
		return stepPinPlacement
	}
	return stepPlayerRows
}

// validateManifestRowsMode 检查续跑时本次的 -player-rows-mode 与清单记录的一致(§10.1 第 7 条)。
// 换模式续跑没有意义:拷了一半的行不会因为换成 pin 就变成冷副本,钉了一半的落点也不会因为换成 copy 就撤掉。
func validateManifestRowsMode(m *mergeManifest, want string) error {
	if m == nil {
		return nil
	}
	got := m.rowsMode()
	if _, err := parsePlayerRowsMode(got); err != nil {
		return fmt.Errorf("manifest records an unknown player_rows_mode %q — refuse to guess", m.PlayerRowsMode)
	}
	if got != want {
		recorded := got
		if m.PlayerRowsMode == "" {
			recorded += " (no player_rows_mode field: written before pin mode existed)"
		}
		return fmt.Errorf("the manifest was written in -player-rows-mode %s but this run uses %s; "+
			"a resumed run must keep the manifest's mode — pass -player-rows-mode %s", recorded, want, got)
	}
	return nil
}

// placementExistingMap 把 PlacementExisting 转成 id → 原值。
func (m *mergeManifest) placementExistingMap() map[uint64]string {
	out := make(map[uint64]string, len(m.PlacementExisting))
	for _, p := range m.PlacementExisting {
		out[p.PlayerID] = p.Value
	}
	return out
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
	return writeJSONAtomic(path, m)
}

// writeJSONAtomic 是合服清单与搬库清单共用的原子落盘(理由见 saveManifest)。
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
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
	// 先认种类再认版本:搬库清单没有 version 字段,只看版本会报一句「version 0」,看不出是拿错了文件。
	if m.Kind != "" {
		return nil, fmt.Errorf("manifest %s is a %q manifest, not a merge manifest — pass the file written by the merge run", path, m.Kind)
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

// dryRunManifestSuffix 是 dry-run 预览清单的后缀,保留给预览专用(见文件头 A3)。
const dryRunManifestSuffix = ".dryrun.json"

// dryRunManifestPath 返回 dry-run 预览的落盘路径 <path>.dryrun.json。
func dryRunManifestPath(path string) string { return path + dryRunManifestSuffix }

// loadRunManifest 读一份「能当凭据用」的清单:合服续跑、-mode unmerge、-verify-merged 都走它。
// 与 loadManifest 的区别只有一条 —— dry-run 预览一律拒绝(A3):按路径后缀拒一次(文件还不存在时
// 也挡住),按内容里的 dry_run 再拒一次(被改名的预览同样挡住)。预览记的是彩排那一刻的名单,
// 拿它续跑会漏掉彩排之后进源区的人,拿它撤销 / 验证则核对的根本不是那次写入。
func loadRunManifest(path string) (*mergeManifest, error) {
	if strings.HasSuffix(path, dryRunManifestSuffix) {
		return nil, fmt.Errorf("-manifest-path %s ends with %s, which is reserved for dry-run previews — "+
			"pass the manifest written by the -apply run instead", path, dryRunManifestSuffix)
	}
	m, err := loadManifest(path)
	if err != nil || m == nil {
		return m, err
	}
	if m.DryRun {
		return nil, fmt.Errorf("manifest %s is a dry-run preview (dry_run=true): it records who a rehearsal WOULD have "+
			"touched, not what any run wrote. Refusing to resume / unmerge / verify from it", path)
	}
	return m, nil
}

// loadVerifyManifest 读 -verify-merged 逐 id 核对用的清单(A6):必须给、必须存在、不是 dry-run 预览、
// src/dst 与本次一致。错误由调用方报成 INFRA —— 没有清单就无从逐 id 核对,结论不可信。
func loadVerifyManifest(path string, src, dst uint32) (*mergeManifest, error) {
	if path == "" {
		return nil, errors.New("-verify-merged requires -manifest-path <the manifest written by the merge being verified>")
	}
	m, err := loadRunManifest(path)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("manifest %s does not exist", path)
	}
	if err := validateManifestForRun(m, src, dst); err != nil {
		return nil, err
	}
	return m, nil
}

// checkResumeTables 是续跑时的表集合校验(A5):本次在源库发现的玩家表必须与清单记录的完全一致。
//
// 清单的 Tables 是步骤 1 拷贝与撤销删行(1')的唯一依据。清单落盘之后两库又跑了迁移、玩家表多了或少了时,
// 继续跑要么让新表里清单玩家的行没拷(步骤 1 已标完成就不会再做),要么让撤销漏删 —— 这不是工具能替人
// 决定的事,拒绝并点名差异。recorded 为空(没有表可比的旧清单)时沿用本次发现,不拒绝。
func checkResumeTables(recorded, discovered []string) error {
	if len(recorded) == 0 {
		return nil
	}
	// columnsNotIn 是通用的「a 里有、b 里没有」(保持 a 的顺序),不只用于列名。
	onlyRecorded := columnsNotIn(recorded, discovered)
	onlyDiscovered := columnsNotIn(discovered, recorded)
	if len(onlyRecorded) == 0 && len(onlyDiscovered) == 0 {
		return nil
	}
	return fmt.Errorf("the player table set changed since the manifest was written: only_in_manifest=%v only_discovered_now=%v. "+
		"Step 1 copies and -mode unmerge deletes by the manifest's table list, so resuming would leave the new tables' rows "+
		"uncopied or the dropped tables' rows undeleted. Bring both zone databases back to the schema revision the manifest "+
		"was written with, or finish the affected tables by hand, before re-running", onlyRecorded, onlyDiscovered)
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
