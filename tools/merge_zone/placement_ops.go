package main

// 落点记录在 mapping Redis 上的读写(docs/design/player-storage-placement.md §4、§9、§10)。合服(pin 钉落点 /
// copy 分流 / 与搬库互斥)、批量钉落点(-mode pin-placement)、搬库(-mode relocate)、合服后验证、撤销与
// -mode storage-audit 共用这一层;编解码见 placement_codec.go。
//
// 两条贯穿的规矩:
//  1. 判定只用 Go 侧的编解码(与 go/db 同一口径),Lua 只负责「值没变 + 围栏不在」的原子确认再写
//     (placementCASScript)。不在 Lua 里再写一遍记录格式:两份解析迟早分叉(前导零、超出 double 精度的
//     版本号),而分叉的后果是工具与 go/db 对同一条记录得出不同的有效落点。
//  2. 畸形的记录一律 fail-closed:go/db 同样拒绝按它选库(该玩家的写进重试 / 死信),工具不能替人猜它指向哪。
//
// 所有键都在 mapping Redis(DB 0):落点记录与 player:zone 同库(data_service 建角时在同一段 Lua 里写两键),
// 合服围栏 merge:in_progress:{zone} 与能力标记 db:capability:zone:{zone} 也在这里 —— CAS 脚本才能在一次原子
// 执行里同时看见记录、home 与围栏。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ── 读 ───────────────────────────────────────────────────────────

// placementRead 是一名玩家的落点记录与 home_zone 的一次读取(同一次 MGET,属于同一瞬间)。
type placementRead struct {
	RecordRaw     string
	RecordPresent bool
	Record        placementRecord
	RecordErr     error // 记录畸形(RecordPresent 仍为 true):调用方必须 fail-closed
	HomeRaw       string
	HomePresent   bool
	Home          uint32
	HomeErr       error // home 畸形(HomePresent 为 false):调用方必须 fail-closed
}

// readPlacements 按 ids 顺序读 player:placement:{id} 与 player:zone:{id},一批一次 MGET(两键交错)。
// Redis 故障返回错误;单个值畸形不报错,记在对应的 *Err 上,由调用方按各自口径处理。
func readPlacements(ctx context.Context, rdb *redis.Client, ids []uint64) ([]placementRead, error) {
	if rdb == nil {
		return nil, errors.New("nil mapping redis handle")
	}
	out := make([]placementRead, 0, len(ids))
	for _, batch := range chunkUint64(ids, mappingScanCount) {
		keys := make([]string, 0, 2*len(batch))
		for _, id := range batch {
			keys = append(keys, placementKey(id), playerZoneKey(id))
		}
		vals, err := rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, fmt.Errorf("placement mget (%d keys): %w", len(keys), err)
		}
		if len(vals) != len(keys) {
			return nil, fmt.Errorf("placement mget returned %d values for %d keys", len(vals), len(keys))
		}
		for i, id := range batch {
			rd, derr := decodePlacementRead(vals[2*i], vals[2*i+1])
			if derr != nil {
				return nil, fmt.Errorf("player %d: %w", id, derr)
			}
			out = append(out, rd)
		}
	}
	return out, nil
}

// decodePlacementRead 把 MGET 的两个元素(nil = 键不存在)解成 placementRead。纯函数。
//
// home 的口径与 go/db 的 parsePlacementSnapshot 一致:空串按缺席;非法值、0 与超出 zone 库编号范围的值都是畸形
// (无记录时 home 就是落点,>= 1000000 派生出的是 player_store_* 而不是这个 zone 的库)。
func decodePlacementRead(rec, home any) (placementRead, error) {
	var r placementRead
	switch s := rec.(type) {
	case nil:
	case string:
		r.RecordRaw, r.RecordPresent = s, true
		r.Record, r.RecordErr = parsePlacement(s)
	default:
		return r, fmt.Errorf("unexpected MGET element type %T for the placement record", rec)
	}
	switch s := home.(type) {
	case nil:
	case string:
		r.HomeRaw = s
		zone, ok, err := parseHomeZone(s)
		switch {
		case err != nil:
			r.HomeErr = err
		case ok && !isZoneStorage(zone):
			r.HomeErr = fmt.Errorf("%w: home zone %d is outside the zone storage range 1..%d", errPlacementMalformed, zone, maxZoneStorageID)
		default:
			r.Home, r.HomePresent = zone, ok
		}
	default:
		return r, fmt.Errorf("unexpected MGET element type %T for player:zone", home)
	}
	return r, nil
}

// ── 合服:清单玩家的落点扫描(§10.1 第 1 条、§10.2)──────────────────

// classifyMergePlacements 是合服清单阶段的落点判定(纯函数):
//   - 冻结中的记录一律拒绝:搬库正在把这名玩家从一个库挪到另一个库,合服又要改他的 home_zone(连带存盘
//     topic);两件事叠在一起,搬库 R2 等的是旧 topic 的在途写,切换之后新 topic 上的写没人等(§9 末条)。
//   - 畸形记录一律拒绝:go/db 不会按它选库,工具也不能决定它该不该拷、该不该钉。
//   - 其余已有记录按模式记下:copy 模式这些人不拷行(行在其落点库);pin 模式这些人不钉(已有落点)。
//     pin 模式里值恰好等于要钉的 "{src}:1" 的不算已有 —— 它与钉出来的一模一样(续跑时看到的正是上次钉的)。
func classifyMergePlacements(ids []uint64, reads []placementRead, mode string, src uint32) ([]manifestPlacement, error) {
	if len(reads) != len(ids) {
		return nil, fmt.Errorf("placement scan returned %d answers for %d players", len(reads), len(ids))
	}
	pinValue := stableValue(src, 1)
	var existing []manifestPlacement
	var frozen, malformed []string
	nFrozen, nMalformed := 0, 0
	for i, id := range ids {
		rd := reads[i]
		if !rd.RecordPresent {
			continue
		}
		switch {
		case rd.RecordErr != nil:
			nMalformed++
			if len(malformed) < unmappedSampleSize {
				malformed = append(malformed, fmt.Sprintf("%d=%q", id, rd.RecordRaw))
			}
		case rd.Record.Frozen:
			nFrozen++
			if len(frozen) < unmappedSampleSize {
				frozen = append(frozen, fmt.Sprintf("%d(run_id=%s)", id, rd.Record.RunID))
			}
		case mode == playerRowsModePin && rd.RecordRaw == pinValue:
		default:
			existing = append(existing, manifestPlacement{PlayerID: id, Value: rd.RecordRaw})
		}
	}
	if nFrozen > 0 {
		return nil, fmt.Errorf("%d manifest players have a FROZEN player:placement record — a relocation is moving them "+
			"(first: %v). A merge changes their home_zone and db_task topic and must not overlap a relocation "+
			"(player-storage-placement.md §9): finish that run (-mode relocate -manifest-path <its manifest>) or abort it "+
			"(-mode relocate-abort -manifest-path <its manifest>), then re-run this merge", nFrozen, frozen)
	}
	if nMalformed > 0 {
		return nil, fmt.Errorf("%d manifest players have a malformed player:placement record (first: %v): go/db refuses "+
			"to route them, and this merge cannot tell where their rows live. Repair those records first", nMalformed, malformed)
	}
	return existing, nil
}

// diffPlacementExisting 比较清单记下的已有落点(首跑时)与本次扫到的(续跑时),返回不一致的样本。
// 围栏之下没有别的写者能改清单玩家的落点(建号被围栏挡、搬库 R1 与 pin-placement 都检查围栏),
// 所以不一致只能是人工改过 —— 续跑不替人决定按哪份来。
func diffPlacementExisting(recorded, scanned []manifestPlacement) (int, []string) {
	want := make(map[uint64]string, len(recorded))
	for _, p := range recorded {
		want[p.PlayerID] = p.Value
	}
	got := make(map[uint64]string, len(scanned))
	for _, p := range scanned {
		got[p.PlayerID] = p.Value
	}
	var ids []uint64
	for id, v := range want {
		if g, ok := got[id]; !ok || g != v {
			ids = append(ids, id)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			ids = append(ids, id)
		}
	}
	ids = sortedUint64(ids)
	var sample []string
	for _, id := range ids {
		if len(sample) >= unmappedSampleSize {
			break
		}
		sample = append(sample, fmt.Sprintf("%d(manifest=%q now=%q)", id, want[id], got[id]))
	}
	return len(ids), sample
}

// frozenPlacementIDs 返回 reads 里记录处于冻结态的玩家样本与人数(撤销前的互斥检查用)。
func frozenPlacementIDs(ids []uint64, reads []placementRead) (int, []string) {
	n := 0
	var sample []string
	for i, id := range ids {
		rd := reads[i]
		if rd.RecordPresent && rd.RecordErr == nil && rd.Record.Frozen {
			n++
			if len(sample) < unmappedSampleSize {
				sample = append(sample, fmt.Sprintf("%d(run_id=%s)", id, rd.Record.RunID))
			}
		}
	}
	return n, sample
}

// idsWithPlacementRecord 返回有落点记录(含畸形)的玩家。畸形的也算「有记录」:调用方据此**不碰**他们的行,
// 这个方向是安全的。
func idsWithPlacementRecord(ids []uint64, reads []placementRead) []uint64 {
	var out []uint64
	for i, id := range ids {
		if reads[i].RecordPresent {
			out = append(out, id)
		}
	}
	return out
}

// idsPlacedOffStorage 返回落点记录不指向 storage 的玩家:记录指向别的库,或记录畸形(旧版 go/db 会无视畸形记录、
// 按本进程 zone 选库,同样会写错库,所以按「不指向」计)。无记录的人不计入 —— 他们的有效落点就是 home。
// 撤销据此判断改回 home 之后 src 的 go/db 是否必须按记录选库。
func idsPlacedOffStorage(ids []uint64, reads []placementRead, storage uint32) []uint64 {
	var out []uint64
	for i, id := range ids {
		rd := reads[i]
		if rd.RecordPresent && (rd.RecordErr != nil || rd.Record.StorageID != storage) {
			out = append(out, id)
		}
	}
	return out
}

// ── 能力标记(§4.3):-db-capability-zones(没有缺省值)────────────────

// capabilityZonesNone 是 -db-capability-zones 的字面量 none:运维声明「此刻没有别的 zone 的 go/db 在跑」,不检查任何标记。
// 只有合服 / 撤销接受它(典型场景:只有 src、dst 两个 zone,两者都在 T-0 zone-down 了)。
const capabilityZonesNone = "none"

// capabilityZoneSpec 是 -db-capability-zones 的解析结果。
//
// 为什么没有缺省值:能力标记是 90s 心跳(go/db 每 30s 续写),只证明「最近 90s 这个 zone 有新版 go/db 在跑」。
// 以前的缺省 "src,dst" 在 T-0 必然被拒 —— runbook 在合服之前已对 src、dst 都 zone-down,两把标记随之过期;
// 而改成「缺省不查」又会让漏填的人无声放行。所以每次都要运维显式写出「此刻哪些 zone 在跑」,或写 none。
type capabilityZoneSpec struct {
	given bool     // 给了非空值
	none  bool     // 字面量 none
	zones []uint32 // 升序去重;未给或 none 时为空
}

func (s capabilityZoneSpec) String() string {
	switch {
	case !s.given:
		return "(not given)"
	case s.none:
		return capabilityZonesNone
	}
	return fmt.Sprint(s.zones)
}

// parseCapabilityZoneSpec 只做语法解析:空串 / 全空白 = 没给(由 requireCapabilityZones 按用途拒绝);
// none 单独出现;其余是逗号分隔的 zone 号(1..maxZoneStorageID),升序去重,不能为空。
// 旧的 src / dst 记号已删除:给了就报错并说明原因,不静默当成别的东西。
func parseCapabilityZoneSpec(raw string) (capabilityZoneSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return capabilityZoneSpec{}, nil
	}
	if strings.EqualFold(raw, capabilityZonesNone) {
		return capabilityZoneSpec{given: true, none: true}, nil
	}
	var zones []uint64
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		switch {
		case tok == "":
			continue
		case strings.EqualFold(tok, capabilityZonesNone):
			return capabilityZoneSpec{}, fmt.Errorf("-db-capability-zones %q: 'none' cannot be combined with zone ids", raw)
		case tok == "src" || tok == "dst":
			return capabilityZoneSpec{}, fmt.Errorf("-db-capability-zones %q: the 'src' / 'dst' shorthands were removed — src and dst "+
				"are zone-down at T-0, so their capability markers are gone. List the ids of the zones whose go/db is running "+
				"right now, or 'none'", raw)
		}
		v, err := strconv.ParseUint(tok, 10, 32)
		if err != nil || v == 0 || !isZoneStorage(uint32(v)) {
			return capabilityZoneSpec{}, fmt.Errorf("-db-capability-zones: %q is not a zone id (1..%d)", tok, maxZoneStorageID)
		}
		zones = append(zones, v)
	}
	zones = sortedUint64(zones)
	if len(zones) == 0 {
		return capabilityZoneSpec{}, fmt.Errorf("-db-capability-zones %q lists no zone", raw)
	}
	out := make([]uint32, len(zones))
	for i, z := range zones {
		out[i] = uint32(z)
	}
	return capabilityZoneSpec{given: true, zones: out}, nil
}

// capabilityUse 是 -db-capability-zones 的一种用途:决定缺省时怎么提示、能不能写 none。
type capabilityUse int

const (
	// capabilityForMerge:pin 合服(首跑 C / 续跑 R2),以及 copy 合服中清单玩家已有落点记录的情形(S)。
	capabilityForMerge capabilityUse = iota
	// capabilityForUnmerge:pin 模式的撤销(copy 撤销给了才查)。
	capabilityForUnmerge
	// capabilityForRelocate:-mode relocate。搬库在线进行,所有在跑的 zone 都必须列出并有标记,不接受 none。
	capabilityForRelocate
	// capabilityForCheck:-mode capability-check。只读核对,none 没有意义。
	capabilityForCheck
)

func (u capabilityUse) allowsNone() bool {
	return u == capabilityForMerge || u == capabilityForUnmerge
}

func (u capabilityUse) label() string {
	switch u {
	case capabilityForMerge:
		return "a pin merge (or a copy merge whose manifest players already have a placement record)"
	case capabilityForUnmerge:
		return "unmerging a pin-mode merge"
	case capabilityForRelocate:
		return "-mode relocate"
	default:
		return "-mode capability-check"
	}
}

// requireCapabilityZones 按用途拒绝「没给」与「不该给 none」。文案与 dev_tools.ps1 的 ps1 侧提示同一口径。
func requireCapabilityZones(spec capabilityZoneSpec, use capabilityUse) error {
	switch {
	case !spec.given && use.allowsNone():
		return fmt.Errorf("%s requires -db-capability-zones (there is no default): list the ids of the zones whose go/db is "+
			"running right now (comma-separated), or 'none' if no other zone is running. src/dst went down at T-0, so list "+
			"only the zones still running; check dst's capability after its zone-up with -mode capability-check "+
			"-db-capability-zones <dst> before opening it (runbook §5.1 / §8 Step 6)", use.label())
	case !spec.given && use == capabilityForRelocate:
		return fmt.Errorf("%s requires -db-capability-zones: list every zone whose go/db is running (comma-separated zone ids, "+
			"e.g. 1,2,3); 'none' is not accepted — a relocation runs online, every running zone must carry the marker", use.label())
	case !spec.given:
		return fmt.Errorf("%s requires -db-capability-zones <zone ids to check>, e.g. the merge target right after its zone-up", use.label())
	case spec.none && !use.allowsNone():
		return fmt.Errorf("-db-capability-zones none is not accepted by %s: list the zone ids explicitly", use.label())
	}
	return nil
}

// capabilityDownZonesHint 是能力检查失败、而列表里有本次 src / dst 时追加的提示:T-0 它们已 zone-down,标记 90s 内过期,
// 列进去必然被拒。纯函数。
func capabilityDownZonesHint(zones []uint32, src, dst uint32) string {
	var listed []uint32
	for _, z := range zones {
		if z != 0 && (z == src || z == dst) {
			listed = append(listed, z)
		}
	}
	if len(listed) == 0 {
		return ""
	}
	return fmt.Sprintf(" Zones %v are this merge's src/dst: src/dst went down at T-0 (runbook §8 Step 2), so their markers "+
		"expire within 90s — list only the zones still running (or 'none' if no other zone is running); check dst's "+
		"capability after its zone-up with -mode capability-check -db-capability-zones %d before opening it (runbook §8 Step 6; "+
		"after an unmerge, check both zones that come back up)", listed, dst)
}

// checkCapabilityZoneSpec 按 spec 核对能力标记:none 不查(调用方记日志);列出的 zone 照常查,失败时带上 src / dst 提示。
// 调用方必须先用 requireCapabilityZones 确认给了值。
func checkCapabilityZoneSpec(ctx context.Context, rdb *redis.Client, spec capabilityZoneSpec, src, dst uint32) error {
	if spec.none {
		return nil
	}
	if err := checkPlacementCapabilities(ctx, rdb, spec.zones); err != nil {
		return withCapabilityDownZonesHint(err, spec.zones, src, dst)
	}
	return nil
}

// withCapabilityDownZonesHint 只在列表里有本次 src / dst 时把 capabilityDownZonesHint 接到 err 后面,否则原样返回 err。
// 不能无条件拼「%v.%s」:T-0 的常见写法恰好不列 src / dst,提示为空时句末会多出一个「.」,调用方再接「. Upgrade ...」
// 就成了「wrong database.. Upgrade」。用 %w 保留错误链。纯函数。
func withCapabilityDownZonesHint(err error, zones []uint32, src, dst uint32) error {
	hint := capabilityDownZonesHint(zones, src, dst)
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w.%s", err, hint)
}

// capabilityShortfall 是纯判定:MGET 回来的能力标记里,哪些 zone 缺标记、哪些值不认识。
// 只认 placement-routing-v1:shared 侧将来加新常量时,这里要把新值加进可接受的集合(能力只增不减)。
func capabilityShortfall(zones []uint32, vals []any) (missing []uint32, unexpected []string) {
	for i, z := range zones {
		var v any
		if i < len(vals) {
			v = vals[i]
		}
		s, ok := v.(string)
		switch {
		case !ok:
			missing = append(missing, z)
		case s != capabilityRoutingV1:
			unexpected = append(unexpected, fmt.Sprintf("zone %d=%q", z, s))
		}
	}
	return missing, unexpected
}

// capabilityConfirmed 是纯判定:vals(与 zones 同序的 MGET 回复)里值正是 placement-routing-v1 的 zone。
func capabilityConfirmed(zones []uint32, vals []any) map[uint32]bool {
	out := make(map[uint32]bool, len(zones))
	for i, z := range zones {
		if i >= len(vals) {
			break
		}
		if s, ok := vals[i].(string); ok && s == capabilityRoutingV1 {
			out[z] = true
		}
	}
	return out
}

// readCapabilityMarkers 读 zones 的能力标记原值(与 zones 同序;nil = 没有标记)。纯读。
func readCapabilityMarkers(ctx context.Context, rdb *redis.Client, zones []uint32) ([]any, error) {
	if rdb == nil {
		return nil, errors.New("nil mapping redis handle")
	}
	if len(zones) == 0 {
		return nil, nil
	}
	keys := make([]string, len(zones))
	for i, z := range zones {
		keys[i] = capabilityKey(z)
	}
	vals, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read capability markers %v: %w", keys, err)
	}
	return vals, nil
}

// checkPlacementCapabilities 要求每个 zone 的 db:capability:zone:{z} == placement-routing-v1(§4.3)。
// 没有标记 = 那个 zone 的 go/db 还是旧版,按本进程 zone 选库:被钉到别处(pin 合服)或被搬走(relocate)的
// 玩家,经它的写会落进它自己的 zone 库,与真源分叉。错误文案不带下一步建议,由调用方按场景补。
func checkPlacementCapabilities(ctx context.Context, rdb *redis.Client, zones []uint32) error {
	if len(zones) == 0 {
		return errors.New("no zone to check")
	}
	vals, err := readCapabilityMarkers(ctx, rdb, zones)
	if err != nil {
		return err
	}
	missing, unexpected := capabilityShortfall(zones, vals)
	if len(missing) == 0 && len(unexpected) == 0 {
		return nil
	}
	return fmt.Errorf("go/db capability %s is not confirmed for every required zone (checked %v): missing in %v, "+
		"unexpected values %v — the go/db of those zones still picks the database by its own zone id, and would write "+
		"players whose placement points elsewhere into the wrong database", capabilityRoutingV1, zones, missing, unexpected)
}

// ── 写:带前提的原子 CAS ─────────────────────────────────────────

// placementCAS 是一次「前提都成立才写」的落点记录写入。
type placementCAS struct {
	PlayerID uint64
	// ExpectAbsent:要求记录此刻不存在;否则要求记录原值 == ExpectRecord(逐字节,读的时候拿到的原值)。
	ExpectAbsent bool
	ExpectRecord string
	// ExpectHome 非空时要求 player:zone 原值 == ExpectHome(无记录时 home 就是有效落点;搬库还靠它拼 topic)。
	ExpectHome string
	// FenceZone 非 0 时要求 merge:in_progress:{FenceZone} 不存在(与合服互斥)。持围栏的合服自己写时传 0。
	FenceZone uint32
	NewValue  string
}

// placementCASOutcome 与 placementCASScript 的返回码一一对应。
type placementCASOutcome int64

const (
	placementCASRecordChanged placementCASOutcome = 0 // 记录与期望不符(本该不存在却存在 / 原值变了)
	placementCASWritten       placementCASOutcome = 1
	placementCASHomeChanged   placementCASOutcome = 2 // player:zone 与期望不符
	placementCASFenced        placementCASOutcome = 3 // 合服围栏在
)

// placementCASResult 是一次 CAS 的结局,带上脚本执行那一刻的记录与 home 原值(空串 = 不存在),供文案与判定。
type placementCASResult struct {
	Outcome       placementCASOutcome
	CurrentRecord string
	CurrentHome   string
}

// placementCASScript:判断与写入在同一段 Lua 里原子完成 —— 记录没变、home 没变、围栏不在,才写新值。
//
//	KEYS[1]=player:placement:{id}  KEYS[2]=player:zone:{id}  KEYS[3](可选)=merge:in_progress:{zone}
//	ARGV[1]='1' 期望记录不存在 / '0' 期望原值 == ARGV[2]   ARGV[3]=期望的 home 原值('' 不核对)  ARGV[4]=新值
//	返回 {code, 执行时的记录原值, 执行时的 home 原值},code 见 placementCASOutcome。
//
// 所有键都在 mapping Redis 这一个实例上(与 data_service 的 getHomeZoneAndMergeFenceScript 同一前提)。
var placementCASScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
local home = redis.call('GET', KEYS[2])
local curOut = cur or ''
local homeOut = home or ''
if ARGV[1] == '1' then
  if cur then return {0, curOut, homeOut} end
elseif cur ~= ARGV[2] then
  return {0, curOut, homeOut}
end
if ARGV[3] ~= '' and home ~= ARGV[3] then return {2, curOut, homeOut} end
if KEYS[3] and redis.call('EXISTS', KEYS[3]) == 1 then return {3, curOut, homeOut} end
redis.call('SET', KEYS[1], ARGV[4])
return {1, curOut, homeOut}
`)

// validate 在发出去之前挡住写坏值:新值必须是合法记录,「期望原值」不能为空(空串永远不等于一条存在的记录)。
func (op placementCAS) validate() error {
	if _, err := parsePlacement(op.NewValue); err != nil {
		return fmt.Errorf("player %d: refusing to write %q: %w", op.PlayerID, op.NewValue, err)
	}
	if !op.ExpectAbsent && op.ExpectRecord == "" {
		return fmt.Errorf("player %d: a CAS on an existing record needs its expected value", op.PlayerID)
	}
	return nil
}

func (op placementCAS) keysAndArgs() ([]string, []any) {
	keys := []string{placementKey(op.PlayerID), playerZoneKey(op.PlayerID)}
	if op.FenceZone != 0 {
		keys = append(keys, mergeFenceKey(op.FenceZone))
	}
	absent := "0"
	if op.ExpectAbsent {
		absent = "1"
	}
	return keys, []any{absent, op.ExpectRecord, op.ExpectHome, op.NewValue}
}

// decodePlacementCASReply 解读脚本回复(纯函数,单测直接驱动)。
func decodePlacementCASReply(v any) (placementCASResult, error) {
	reply, ok := v.([]any)
	if !ok || len(reply) != 3 {
		return placementCASResult{}, fmt.Errorf("unexpected placement CAS reply %T %v", v, v)
	}
	code, cok := reply[0].(int64)
	cur, rok := reply[1].(string)
	home, hok := reply[2].(string)
	if !cok || !rok || !hok || code < int64(placementCASRecordChanged) || code > int64(placementCASFenced) {
		return placementCASResult{}, fmt.Errorf("unexpected placement CAS reply %v", reply)
	}
	return placementCASResult{Outcome: placementCASOutcome(code), CurrentRecord: cur, CurrentHome: home}, nil
}

// runPlacementCAS 按 ops 顺序执行 CAS,分批走 pipeline;返回与 ops 等长、同序的结局。
// 一批里 Exec 报错时这一批哪些已生效不确定:调用方按「重跑幂等」处理(每个调用点的新值都由读到的状态决定,
// 已写成的下次读到就是新值,不会被写两次)。
func runPlacementCAS(ctx context.Context, rdb *redis.Client, ops []placementCAS) ([]placementCASResult, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	if rdb == nil {
		return nil, errors.New("nil mapping redis handle")
	}
	for _, op := range ops {
		if err := op.validate(); err != nil {
			return nil, err
		}
	}
	// pipeline 里的 EVALSHA 遇到 NOSCRIPT 无法回退(整批已经发出),先 SCRIPT LOAD 一次(同 remapPlayerMapping)。
	if err := placementCASScript.Load(ctx, rdb).Err(); err != nil {
		return nil, fmt.Errorf("load placement CAS script: %w", err)
	}
	out := make([]placementCASResult, 0, len(ops))
	for start := 0; start < len(ops); start += mappingScanCount {
		batch := ops[start:min(start+mappingScanCount, len(ops))]
		pipe := rdb.Pipeline()
		cmds := make([]*redis.Cmd, len(batch))
		for i, op := range batch {
			keys, args := op.keysAndArgs()
			cmds[i] = placementCASScript.Run(ctx, pipe, keys, args...)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return out, fmt.Errorf("placement CAS pipeline (%d players): %w", len(batch), err)
		}
		for i, cmd := range cmds {
			v, err := cmd.Result()
			if err != nil {
				return out, fmt.Errorf("placement CAS for player %d: %w", batch[i].PlayerID, err)
			}
			res, err := decodePlacementCASReply(v)
			if err != nil {
				return out, fmt.Errorf("player %d: %w", batch[i].PlayerID, err)
			}
			out = append(out, res)
		}
	}
	return out, nil
}

// ── 钉落点(合服 pin_placement 步骤 / -mode pin-placement)─────────────

// pinReport 是一轮钉落点的计数。
type pinReport struct {
	Candidates    int      // 本轮看过的玩家
	Pinned        int      // 本次写成 "{zone}:1"(dry-run:将会写)
	AlreadyPinned int      // 已经是 "{zone}:1"(续跑,或早先钉过)
	Kept          int      // 已有别的落点记录,原样保留
	HomeMoved     int      // home 已不是这个 zone(-mode pin-placement:跳过)
	Anomalies     []string // 合服 pin 步骤里不该出现的情形(样本,封顶 unmappedSampleSize)
	AnomalyCount  int
}

func (r pinReport) String() string {
	return fmt.Sprintf("candidates=%d pinned=%d already_pinned=%d kept_existing=%d home_moved=%d anomalies=%d",
		r.Candidates, r.Pinned, r.AlreadyPinned, r.Kept, r.HomeMoved, r.AnomalyCount)
}

func (r *pinReport) anomaly(format string, args ...any) {
	r.AnomalyCount++
	if len(r.Anomalies) < unmappedSampleSize {
		r.Anomalies = append(r.Anomalies, fmt.Sprintf(format, args...))
	}
}

// pinVerdict 是一名玩家在钉落点时的判定。
type pinVerdict int

const (
	pinVerdictPin           pinVerdict = iota // 无记录、home 就是这个 zone:钉(dry-run:将会钉)
	pinVerdictAlreadyPinned                   // 记录已是 "{zone}:1"
	pinVerdictKept                            // 已有别的记录
	pinVerdictHomeMoved                       // 无记录、home 不是这个 zone
)

// decidePin 是钉落点的纯判定:recordRaw / homeRaw 是同一瞬间读到的原值(空串 = 不存在)。
// 合服(pin 模式)与 -mode pin-placement 共用;两者对 pinVerdictKept / pinVerdictHomeMoved 的处置不同,由调用方决定。
func decidePin(recordRaw, homeRaw string, zone uint32) pinVerdict {
	switch {
	case recordRaw == stableValue(zone, 1):
		return pinVerdictAlreadyPinned
	case recordRaw != "":
		return pinVerdictKept
	case homeRaw == strconv.FormatUint(uint64(zone), 10):
		return pinVerdictPin
	default:
		return pinVerdictHomeMoved
	}
}

// pinManifestPlacements 是合服 pin 模式的「钉落点」步骤(§10.1 第 2 条):清单里每个玩家,若无记录则钉
// "{src}:1"(有效落点本来就是 src,钉住之后改 home 不再牵动落点);已有记录的不动。
//
// 写用 placementCASScript:期望记录不存在、期望 home 仍是 src(无记录时 home 就是有效落点;home 若已不是 src,
// 钉 src 会改变他的有效落点),不传围栏键 —— 围栏就是本次合服自己持有的。
// 围栏之下不该出现的情形一律记为异常,整步报错(调用方保留围栏中止):
//   - 记录存在、既不是 "{src}:1" 也不是清单阶段记下的原值:有人在围栏之下改了落点;
//   - 无记录、home 不是 src:映射被改过或丢了(步骤 5 之前 home 必须还是 src)。
//
// dry-run 只读,按同一判定分类。钉是幂等的:续跑时已钉的按 already_pinned 计。
func pinManifestPlacements(ctx context.Context, rdb *redis.Client, ids []uint64, existing map[uint64]string,
	src uint32, dryRun bool) (pinReport, error) {
	var rep pinReport
	pinValue := stableValue(src, 1)
	srcVal := strconv.FormatUint(uint64(src), 10)
	// classify 处理「没有写成」的那些人(dry-run 全部走这里)。apply 下判成「该钉」却没写成,说明脚本看到的
	// 状态与判定对不上(例如一个值为空串的畸形键),同样记异常,不能计成已钉。
	classify := func(id uint64, recordRaw, homeRaw string, applied bool) {
		rep.Candidates++
		switch decidePin(recordRaw, homeRaw, src) {
		case pinVerdictPin:
			if applied {
				rep.anomaly("%d: the pin was not written although the record looked absent (record %q, player:zone %q)", id, recordRaw, homeRaw)
				return
			}
			rep.Pinned++
		case pinVerdictAlreadyPinned:
			rep.AlreadyPinned++
		case pinVerdictKept:
			if want, ok := existing[id]; ok && want == recordRaw {
				rep.Kept++
				return
			}
			rep.anomaly("%d: record %q appeared or changed under the merge fence (manifest recorded %q)", id, recordRaw, existing[id])
		default:
			rep.anomaly("%d: no placement record and player:zone is %q, not %d", id, homeRaw, src)
		}
	}
	for _, batch := range chunkUint64(ids, mappingScanCount) {
		if dryRun {
			reads, err := readPlacements(ctx, rdb, batch)
			if err != nil {
				return rep, err
			}
			for i, id := range batch {
				classify(id, reads[i].RecordRaw, reads[i].HomeRaw, false)
			}
			continue
		}
		ops := make([]placementCAS, len(batch))
		for i, id := range batch {
			ops[i] = placementCAS{PlayerID: id, ExpectAbsent: true, ExpectHome: srcVal, NewValue: pinValue}
		}
		results, err := runPlacementCAS(ctx, rdb, ops)
		if err != nil {
			return rep, err
		}
		for i, id := range batch {
			switch res := results[i]; res.Outcome {
			case placementCASWritten:
				rep.Candidates++
				rep.Pinned++
			case placementCASFenced:
				// 没传围栏键,脚本不会回这个码;回了就是契约坏了,不能当成已钉。
				rep.Candidates++
				rep.anomaly("%d: unexpected fence verdict from the pin script", id)
			default:
				classify(id, res.CurrentRecord, res.CurrentHome, true)
			}
		}
	}
	if rep.AnomalyCount > 0 {
		return rep, fmt.Errorf("%d manifest players could not be pinned to storage %d (first: %v) — the merge fence should "+
			"have kept everyone else away from their placement / home_zone; find out who changed them before re-running",
			rep.AnomalyCount, src, rep.Anomalies)
	}
	return rep, nil
}

// ── 全量枚举:有效落点为某个库的玩家(搬库整库、-mode storage-audit)──────

// storagePlayers 是「有效落点 == storage」的玩家(§1:有记录看记录,无记录看 home)。
type storagePlayers struct {
	ByRecord  []uint64 // 落点记录指向 storage(含冻结中的)
	Frozen    int      // ByRecord 里记录处于冻结态的人数
	ByHome    []uint64 // 无记录、home == storage(storage 是 zone 库时才有)
	Malformed []uint64 // 落点记录畸形:指向哪里不知道(全库,不只 storage)
}

// all 返回全部有效落点为 storage 的玩家(升序)。
func (s storagePlayers) all() []uint64 {
	return sortedUint64(append(append([]uint64(nil), s.ByRecord...), s.ByHome...))
}

// collectStoragePlayers SCAN 全量 player:placement:* 与(zone 库时)player:zone:*,找出有效落点为 storage 的玩家。
// 每批 SCAN 用一次 MGET 取值(同 collectPlayerIDsWithHomeZone)。SCAN 期间新写的记录可能漏掉:本函数给的是
// 「扫描那一刻」的集合,调用方(搬库)对每个人在冻结时再原子复核一次。
func collectStoragePlayers(ctx context.Context, rdb *redis.Client, storage uint32) (storagePlayers, error) {
	var out storagePlayers
	if rdb == nil {
		return out, errors.New("nil mapping redis handle")
	}
	var cur uint64
	for {
		keys, next, err := rdb.Scan(ctx, cur, placementKeyPrefix+"*", mappingScanCount).Result()
		if err != nil {
			return out, fmt.Errorf("scan %s*: %w", placementKeyPrefix, err)
		}
		if len(keys) > 0 {
			vals, err := rdb.MGet(ctx, keys...).Result()
			if err != nil {
				return out, fmt.Errorf("placement mget (%d keys): %w", len(keys), err)
			}
			for i, key := range keys {
				raw, ok := vals[i].(string)
				if !ok {
					continue // 扫描与读取之间被删了
				}
				pid, perr := strconv.ParseUint(strings.TrimPrefix(key, placementKeyPrefix), 10, 64)
				if perr != nil || pid == 0 {
					continue // 不是本契约的键
				}
				rec, rerr := parsePlacement(raw)
				switch {
				case rerr != nil:
					out.Malformed = append(out.Malformed, pid)
				case rec.StorageID == storage:
					out.ByRecord = append(out.ByRecord, pid)
					if rec.Frozen {
						out.Frozen++
					}
				}
			}
		}
		cur = next
		if cur == 0 {
			break
		}
	}
	if isZoneStorage(storage) {
		homeIDs, err := collectPlayerIDsWithHomeZone(ctx, rdb, storage)
		if err != nil {
			return out, err
		}
		reads, err := readPlacements(ctx, rdb, homeIDs)
		if err != nil {
			return out, err
		}
		for i, id := range homeIDs {
			if !reads[i].RecordPresent {
				out.ByHome = append(out.ByHome, id)
			}
		}
	}
	out.ByRecord = sortedUint64(out.ByRecord)
	out.ByHome = sortedUint64(out.ByHome)
	out.Malformed = sortedUint64(out.Malformed)
	return out, nil
}

// sortedZones 返回 set 的升序切片(日志与清单里的 zone 列表要稳定)。
func sortedZones(set map[uint32]bool) []uint32 {
	out := make([]uint32, 0, len(set))
	for z := range set {
		out = append(out, z)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
