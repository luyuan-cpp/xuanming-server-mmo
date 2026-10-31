package main

// 合服清单(manifest):在**写第一个字节之前**落盘的那份「这次合服动了哪些业务对象」。
//
// 别名合服不改玩家,清单里没有玩家,但公会 zone_id、聚宝斋 market_zone、公会榜是按区存的业务数据:改完之后就分不清
// 哪些是搬过来的、哪些是目标区原住民。所以先把公会 id / 商品 id / ZSET 成员与分数写进清单,再开始写;撤销
// (-mode unmerge)只认清单里列出的对象,绝不碰目标区原住民。每完成一步就 markStep + 落盘一次,重跑按 stepDone 续跑。
//
// 写入方式:temp 文件 + rename(同目录),避免进程被 kill 时留下半个 JSON。
//
// dry-run 不碰 -manifest-path:预览写到 <path>.dryrun.json,内容里标 dry_run=true。T-1 彩排用了与 T-0 相同的路径时,
// 彩排那一刻的名单不能被 T-0 的 -apply 当成续跑读进来。预览只给人看,续跑 / 撤销 / 合服后验证一律拒读。
//
// run_id / source_zone / target_zone / dry_run 这几个字段名同时是 go/db barrierrestore 校验 P7 审计时读的契约,不能改名。

import (
	"crypto/rand"
	"encoding/hex"
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
// 2 = 别名合服(2026-10-30):不再有玩家名单、玩家表、落点记录与映射来源,步骤名也换了。
const manifestVersion = 2

// manifest 里的步骤名。清单是幂等重跑与撤销的唯一依据,所以步骤名一旦发布就不能改字面量。
const (
	stepMergedInto       = "merged_into"        // zone_merged_into + merge:merged_into 标记
	stepMergedIntoSettle = "merged_into_settle" // 等各服务的映射缓存刷新
	stepGuildMySQL       = "guild_mysql"        // guild.zone_id 改写 + 缓存失效
	stepTradeMySQL       = "trade_mysql"        // trade_listing.market_zone 改写(聚宝斋,trade_step.go)
	stepGuildRank        = "guild_rank"         // guild_rank:zone ZSET 合并
	stepMergeDone        = "merge_done"         // 全部步骤完成
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
	RunID           string `json:"run_id"` // 写进 zone_merged_into.run_id,撤销按它认领
	SourceZone      uint32 `json:"source_zone"`
	TargetZone      uint32 `json:"target_zone"`
	StartedAt       string `json:"started_at"`
	StartedAtUnixMs int64  `json:"started_at_unix_ms"`
	Operator        string `json:"operator"` // host/pid,排障时对得上是谁跑的

	// ── 对象清单(第一次写之前定稿;公会与商品在各自步骤开始时再并进一次滞后窗口内落到源区的对象)──
	GuildIDs    []uint64             `json:"guild_ids"`
	RankMembers []manifestRankMember `json:"rank_members"`
	// RankMembersUnwritten 为 true = RankMembers 只是清单阶段读的源榜快照,还没交给过任何一次榜单步骤的写
	// (步骤在 MULTI/EXEC 之前把撤销依据落盘时清成 false)。步骤锁内重读发现源榜已不在时据此决定撤销依据:
	// 为 true 时快照里的成员从没进过目标榜(源榜是被 guild 服合法清空的,可能已解散),撤销依据记为空;
	// 为 false 时快照就是上一次并进目标榜的那一份,原样保留(见 guild_step.go chooseRankWrite)。
	RankMembersUnwritten bool `json:"rank_members_unwritten,omitempty"`
	// TradeListingIDs:改写前 market_zone=src 的聚宝斋商品 id(trade_step.go)。撤销只把其中
	// 当前 market_zone=dst 的改回 src。
	TradeListingIDs []uint64 `json:"trade_listing_ids"`

	// DryRun 为 true = 这是 dry-run 写出的预览(<path>.dryrun.json):记的是「dry-run 那一刻会动谁」,
	// 不是任何一次写入的凭据,loadRunManifest 一律拒读。
	DryRun bool `json:"dry_run,omitempty"`

	// UnmergeStarted 为 true = 这份清单开始过撤销。撤销不记步骤进度,合服的步骤标记还在;拿它续跑合服会跳过每一步、
	// 打出 Done,而榜单和商品可能已经回到源区。见 refuseManifestBeingUnmerged。
	UnmergeStarted bool `json:"unmerge_started,omitempty"`

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

// refuseManifestBeingUnmerged 拒绝拿开始过撤销的清单续跑合服。
func refuseManifestBeingUnmerged(m *mergeManifest) error {
	if m != nil && m.UnmergeStarted {
		return fmt.Errorf("manifest run_id=%s has been through -mode unmerge: finish the unmerge with the same command, "+
			"then merge again with a new -manifest-path", m.RunID)
	}
	return nil
}

// dryRunManifestSuffix 是 dry-run 预览清单的后缀,保留给预览专用(见文件头)。
const dryRunManifestSuffix = ".dryrun.json"

// dryRunManifestPath 返回 dry-run 预览的落盘路径 <path>.dryrun.json。
func dryRunManifestPath(path string) string { return path + dryRunManifestSuffix }

// loadRunManifest 读一份「能当凭据用」的清单:合服续跑、-mode unmerge、-verify-merged 都走它。
// 与 loadManifest 的区别只有一条 —— dry-run 预览一律拒绝:按路径后缀拒一次(文件还不存在时
// 也挡住),按内容里的 dry_run 再拒一次(被改名的预览同样挡住)。预览记的是彩排那一刻的名单,
// 拿它续跑会漏掉彩排之后进源区的对象,拿它撤销 / 验证则核对的根本不是那次写入。
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

// loadVerifyManifest 读 -verify-merged 用的清单:必须给、必须存在、不是 dry-run 预览、src/dst 与本次一致。
// 错误由调用方报成 INFRA —— 没有清单就无从核对,结论不可信。
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

// randomToken 返回 32 位随机 hex。
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实践中等于系统坏了;退化成时间戳仍然唯一到毫秒 + pid。
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

func operatorTag() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/pid=%d", host, os.Getpid())
}

// newRunID 生成本次运行的身份,写进清单与 zone_merged_into.run_id。
func newRunID(src, dst uint32, now time.Time) string {
	return fmt.Sprintf("%d-%d-%s-%s", src, dst, now.UTC().Format("20060102T150405Z"), randomToken()[:8])
}
