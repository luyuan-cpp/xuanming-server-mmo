// Package routetool 是 route_tool 两个子命令 import-from-redis / audit 的逻辑
// (docs/design/player-route-mapping-durability-proposal.md §7 P1、§11.2,映射持久化第 1a 批)。
//
// 分层:cmd/route_tool 只解析参数、装配、把结果映射成退出码;本包只依赖两个小接口 ——
// mappingReader(mapping Redis 的 SCAN / MGET,go-zero *redis.Redis 天然满足)与 routeDB
// (store.RouteStore 的导入与只读接口)。单测用 miniredis + 假 routeDB 驱动,不连库。
//
// 对 mapping Redis **只读**:导入不写 maprev、不发布(第 1b 批的全量预热负责;发布脚本的遗留值守卫
// 保证不写也安全)。对库只经 store.RouteStore:导入走 ImportRoutes / ImportMergedInto,审计只读。
//
// 本文件是 import 与 audit 共用的读 Redis 部分:SCAN 翻页、键名后缀与值的规范形式校验、merged_into 的 JSON 解析。
// 规范形式(无前导零、十进制回写逐字节相同)是两边比对成立的前提:允许 "0012" 与 "12" 并存,
// 同一个值就有两种写法,库里的 12 与缓存里的 "0012" 会被误判成不同,反过来也可能把畸形值当成合法值导进库。
package routetool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"data_service/internal/store"

	"shared/placement"
)

// 键前缀。player:placement / player:maprev 的前缀在 shared/placement 里导出;另外两个这里各镜像一份
// (routetool_test.go 钉住与 placement.HomeZoneKey / MergedIntoKey 逐字节相同)。
const (
	// homeZoneKeyPrefix = placement.HomeZoneKey 的前缀(写者 data_service 的 routing 包)。
	homeZoneKeyPrefix = "player:zone:"
	// mergedIntoKeyPrefix 是合走标记 merge:merged_into:{src}(写者 tools/merge_zone/merged_into.go,值 JSON {dst, run_id})。
	mergedIntoKeyPrefix = "merge:merged_into:"
)

// mappingReader 是本包对 mapping Redis 的全部依赖:只读。go-zero 的 *redis.Redis 天然满足
// (routing/router.go 已在用这两个方法)。
//
// MgetCtx 的约定(go-zero):返回值与 keys 逐位对齐,缺键返回空串。所以本包把「空串」一律当「缺席」:
// 一把存在但值为空串、或不是 string 类型的键(MGET 对它回 nil)也会被当成缺席 —— 本仓库没有写者会写出这两种键。
type mappingReader interface {
	ScanCtx(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error)
	MgetCtx(ctx context.Context, keys ...string) ([]string, error)
}

// scanKeys 逐页 SCAN match,每页非空时回调一次。SCAN 允许同一个键在不同页重复出现(rehash 期间),
// 调用方自己去重;SCAN 期间新增的键可能出现也可能不出现,这正是 audit 要复核的原因。
func scanKeys(ctx context.Context, rd mappingReader, match string, count int64, page func(keys []string) error) error {
	var cursor uint64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("scan %s: %w", match, err)
		}
		keys, next, err := rd.ScanCtx(ctx, cursor, match, count)
		if err != nil {
			return fmt.Errorf("scan %s: %w", match, err)
		}
		if len(keys) > 0 {
			if err := page(keys); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// snapshotMergedInto 原样读一次全部 merge:merged_into:*(键 → 原值,不解析、不校验;值为空串 = SCAN 与 MGET 之间
// 被删,视为不在)。import 在开头与结尾各读一次并比对:导入窗口内跑过的合服 / 撤销合服一定会让这两份快照不同。
// 原值比对(而不是解析后的行)连畸形值的变化也算进去 —— 这里只关心「窗口内有没有人写过」,不关心写的对不对。
func snapshotMergedInto(ctx context.Context, rd mappingReader, count int64) (map[string]string, error) {
	seen := make(map[string]struct{})
	var keys []string
	err := scanKeys(ctx, rd, mergedIntoKeyPrefix+"*", count, func(page []string) error {
		for _, k := range page {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(keys))
	chunk := int(count)
	if chunk < 1 {
		chunk = 1
	}
	for start := 0; start < len(keys); start += chunk {
		part := keys[start:min(start+chunk, len(keys))]
		vals, err := rd.MgetCtx(ctx, part...)
		if err != nil {
			return nil, fmt.Errorf("mget %d merged_into keys: %w", len(part), err)
		}
		if len(vals) != len(part) {
			return nil, fmt.Errorf("mget merged_into keys: got %d values for %d keys", len(vals), len(part))
		}
		for i, key := range part {
			if vals[i] != "" {
				out[key] = vals[i]
			}
		}
	}
	return out, nil
}

// diffMergedIntoSnapshots 列出两份快照的差异(升序):+key 新增、-key 消失、~key 值变了。无差异返回 nil。
func diffMergedIntoSnapshots(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		old, ok := before[k]
		switch {
		case !ok:
			out = append(out, "+"+k)
		case old != v:
			out = append(out, "~"+k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, "-"+k)
		}
	}
	// 按键名排,前缀符号不参与排序:同一把键只会出现一次。
	sort.Slice(out, func(i, j int) bool { return out[i][1:] < out[j][1:] })
	return out
}

// ── 规范形式 ──────────────────────────────────────────────────────────────────

// parseKeyPlayerID 取 {prefix}{id} 里的 id:非 0,且十进制回写与后缀逐字节相同(player:zone:007 判畸形)。
func parseKeyPlayerID(key, prefix string) (uint64, bool) {
	suffix, ok := strings.CutPrefix(key, prefix)
	if !ok || suffix == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(suffix, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != suffix {
		return 0, false
	}
	return id, true
}

// parseKeyZone 同 parseKeyPlayerID,后缀是 zone id(uint32)。
func parseKeyZone(key, prefix string) (uint32, bool) {
	suffix, ok := strings.CutPrefix(key, prefix)
	if !ok || suffix == "" {
		return 0, false
	}
	zone, err := strconv.ParseUint(suffix, 10, 32)
	if err != nil || zone == 0 || strconv.FormatUint(zone, 10) != suffix {
		return 0, false
	}
	return uint32(zone), true
}

// parseCanonicalZone 校验 player:zone 的值:placement.ParseHomeZone 能解析、不超过 store.RouteHomeZoneMax
// (= player_route 的 chk_route_home)、且十进制回写与原值逐字节相同("0012" 判畸形)。
func parseCanonicalZone(raw string) (uint32, error) {
	zone, ok, err := placement.ParseHomeZone(raw)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: empty home zone", placement.ErrMalformed)
	}
	if zone > store.RouteHomeZoneMax {
		return 0, fmt.Errorf("%w: home zone %d exceeds %d", placement.ErrMalformed, zone, store.RouteHomeZoneMax)
	}
	if strconv.FormatUint(uint64(zone), 10) != raw {
		return 0, fmt.Errorf("%w: home zone %q is not canonical decimal", placement.ErrMalformed, raw)
	}
	return zone, nil
}

// parseCanonicalPlacement 校验 player:placement 的值:placement.Parse 能解析,且 Encode() 回写与原值逐字节相同
// ("01:1" 判畸形)。冻结态与稳定态是两个不同的值,这里不做任何归一。
func parseCanonicalPlacement(raw string) (placement.Record, error) {
	rec, err := placement.Parse(raw)
	if err != nil {
		return placement.Record{}, err
	}
	if enc := rec.Encode(); enc != raw {
		return placement.Record{}, fmt.Errorf("%w: placement %q is not canonical (re-encodes as %q)", placement.ErrMalformed, raw, enc)
	}
	return rec, nil
}

// parseCanonicalMapRev 校验 player:maprev 的值(placement.ValidRev:无前导零、1..20 位、不超过 MaxUint64)。
func parseCanonicalMapRev(raw string) (uint64, error) {
	if !placement.ValidRev(raw) {
		return 0, fmt.Errorf("%w: maprev %q", placement.ErrMalformed, raw)
	}
	rev, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: maprev %q: %v", placement.ErrMalformed, raw, err)
	}
	return rev, nil
}

// ── 一名玩家的三族键 ──────────────────────────────────────────────────────────

// routeTriple 是一名玩家在 mapping Redis 里三族键的原值("" = 缺席,见 mappingReader 的 MgetCtx 约定)。
type routeTriple struct {
	Zone      string // player:zone:{id}
	Placement string // player:placement:{id}
	MapRev    string // player:maprev:{id}
}

// absent 报告三把键是否都不在。
func (t routeTriple) absent() bool {
	return t == routeTriple{}
}

// readRouteTriples 用一次 MGET 读一批玩家的三族键,结果与 ids 逐位对齐。ids 为空时不发命令。
func readRouteTriples(ctx context.Context, rd mappingReader, ids []uint64) ([]routeTriple, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, 3*len(ids))
	for _, id := range ids {
		keys = append(keys, placement.HomeZoneKey(id), placement.Key(id), placement.MapRevKey(id))
	}
	vals, err := rd.MgetCtx(ctx, keys...)
	if err != nil {
		return nil, fmt.Errorf("mget route keys of %d players: %w", len(ids), err)
	}
	if len(vals) != len(keys) {
		// 对不齐就无法知道哪个值属于哪个玩家,只能整体失败(fail-closed)。
		return nil, fmt.Errorf("mget route keys: got %d values for %d keys", len(vals), len(keys))
	}
	out := make([]routeTriple, len(ids))
	for i := range ids {
		out[i] = routeTriple{Zone: vals[3*i], Placement: vals[3*i+1], MapRev: vals[3*i+2]}
	}
	return out, nil
}

// redisRoute 是一名玩家三族键校验之后的值。
type redisRoute struct {
	PlayerID  uint64
	Raw       routeTriple
	HomeZone  uint32            // 0 = 无 player:zone
	Placement *placement.Record // nil = 无 player:placement
	MapRev    uint64            // 0 = 无 player:maprev
}

// hasMapping 报告这名玩家在缓存里有没有归属或落点(只有 maprev 不算)。
func (r redisRoute) hasMapping() bool {
	return r.HomeZone != 0 || r.Placement != nil
}

// importRow 转成 store.ImportRoutes 的一行。
func (r redisRoute) importRow() store.RouteImportRow {
	return store.RouteImportRow{PlayerID: r.PlayerID, HomeZone: r.HomeZone, Placement: r.Placement, MapRev: r.MapRev}
}

// malformedValue 是一把不规范的键或值:Key 是出问题的键,Value 是它的原值,Reason 是判畸形的理由。
type malformedValue struct {
	Key    string
	Value  string
	Reason string
}

// parseRedisRoute 按规范形式校验一名玩家的三族键。任一值不规范就返回第一个畸形项,调用方**整名玩家跳过**:
// 只导一半(例如导了 zone、丢了畸形的落点)会让库里出现一行与缓存都不一致的「半截真源」。
func parseRedisRoute(playerID uint64, raw routeTriple) (redisRoute, *malformedValue) {
	r := redisRoute{PlayerID: playerID, Raw: raw}
	if raw.Zone != "" {
		zone, err := parseCanonicalZone(raw.Zone)
		if err != nil {
			return redisRoute{}, &malformedValue{Key: placement.HomeZoneKey(playerID), Value: raw.Zone, Reason: err.Error()}
		}
		r.HomeZone = zone
	}
	if raw.Placement != "" {
		rec, err := parseCanonicalPlacement(raw.Placement)
		if err != nil {
			return redisRoute{}, &malformedValue{Key: placement.Key(playerID), Value: raw.Placement, Reason: err.Error()}
		}
		r.Placement = &rec
	}
	if raw.MapRev != "" {
		rev, err := parseCanonicalMapRev(raw.MapRev)
		if err != nil {
			return redisRoute{}, &malformedValue{Key: placement.MapRevKey(playerID), Value: raw.MapRev, Reason: err.Error()}
		}
		r.MapRev = rev
	}
	return r, nil
}

// ── merged_into ───────────────────────────────────────────────────────────────

// mergedIntoJSON 是 merge:merged_into:{src} 的值,镜像 tools/merge_zone/merged_into.go 的 mergedIntoValue:
// 字段只增不改名,未知字段放行(encoding/json 默认忽略未知字段)。
type mergedIntoJSON struct {
	Dst   uint32 `json:"dst"`
	RunID string `json:"run_id"`
}

// parseMergedIntoValue 校验一条合走标记:JSON {dst, run_id},dst >= 1 且 != src,run_id 过 placement.ValidRunID
// (merge_zone 的 run_id 形如 {src}-{dst}-{UTC}-{hex8},满足这一条)。MergedAtMs 不从 Redis 取:原始时刻已无从得知,
// 导入时取库时钟。
func parseMergedIntoValue(src uint32, raw string) (store.MergedIntoRow, error) {
	var v mergedIntoJSON
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return store.MergedIntoRow{}, fmt.Errorf("not a merged_into JSON value {dst, run_id}: %v", err)
	}
	if v.Dst == 0 || v.Dst == src {
		return store.MergedIntoRow{}, fmt.Errorf("dst %d must be >= 1 and differ from src %d", v.Dst, src)
	}
	if !placement.ValidRunID(v.RunID) {
		return store.MergedIntoRow{}, fmt.Errorf("run_id %q must be 1..64 bytes of [A-Za-z0-9_-]", v.RunID)
	}
	return store.MergedIntoRow{SrcZone: src, DstZone: v.Dst, RunID: v.RunID}, nil
}

// mergedIntoScan 是一次全量读合走标记的结果。
type mergedIntoScan struct {
	Rows      []store.MergedIntoRow // 合法的,按 src 升序、无重复
	Malformed []malformedValue      // 键名后缀或值不规范的
}

// scanMergedInto 全量读 merge:merged_into:*(条数 = 合过的区数,很小,一次收齐再分批 MGET)。
// 值为空串的键(SCAN 与 MGET 之间被 unmerge 删掉)跳过:此刻已经没有可比对的值。
func scanMergedInto(ctx context.Context, rd mappingReader, count int64) (mergedIntoScan, error) {
	seen := make(map[string]struct{})
	var keys []string
	err := scanKeys(ctx, rd, mergedIntoKeyPrefix+"*", count, func(page []string) error {
		for _, k := range page {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return mergedIntoScan{}, err
	}
	sort.Strings(keys)

	var out mergedIntoScan
	chunk := int(count)
	if chunk < 1 {
		chunk = 1
	}
	for start := 0; start < len(keys); start += chunk {
		part := keys[start:min(start+chunk, len(keys))]
		vals, err := rd.MgetCtx(ctx, part...)
		if err != nil {
			return mergedIntoScan{}, fmt.Errorf("mget %d merged_into keys: %w", len(part), err)
		}
		if len(vals) != len(part) {
			return mergedIntoScan{}, fmt.Errorf("mget merged_into keys: got %d values for %d keys", len(vals), len(part))
		}
		for i, key := range part {
			src, ok := parseKeyZone(key, mergedIntoKeyPrefix)
			if !ok {
				out.Malformed = append(out.Malformed, malformedValue{Key: key, Value: vals[i], Reason: "key suffix is not a canonical non-zero zone id"})
				continue
			}
			if vals[i] == "" {
				continue
			}
			row, err := parseMergedIntoValue(src, vals[i])
			if err != nil {
				out.Malformed = append(out.Malformed, malformedValue{Key: key, Value: vals[i], Reason: err.Error()})
				continue
			}
			out.Rows = append(out.Rows, row)
		}
	}
	sort.Slice(out.Rows, func(i, j int) bool { return out.Rows[i].SrcZone < out.Rows[j].SrcZone })
	return out, nil
}
