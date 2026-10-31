package placement

// 本文件是 D1 第 1b-2 批起「读方怎么信任副本」与「对账 leader 怎么挂标记」在 Redis 一侧的契约
// (docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §2 的 M1–M4,定稿后并入提案 §13)。
// route.go 是第 1a / 1b-1 批的契约(maprev、发布脚本、标记的值格式与 CAS / CAD),本文件只新增,不改它。
//
// 为什么读方不能只看「标记存在」(规格 §1):Redis 从旧备份恢复、哨兵把落后的副本提成主、标记被拷到别的实例时,
// 标记会和回退了的数据一起留下来,要等对账 leader 下一拍才撤得掉;这期间读方会把缺键当成「没有归属」。
// 所以读方在**同一段 Lua**里取回标记、本实例身份(INFO 的 run_id / role / master_replid)与映射键,
// 只有标记里记下的身份等于本次读所在实例的身份,才信任这份副本(M1)。
//
// 与 placement.go、route.go 同一约定:只依赖标准库;Redis 的执行由各服务用自己的客户端完成。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ── 控制键 ────────────────────────────────────────────────────────────────────

// RouteWarmKey 是挂标记用的预热令牌(M2):string,32 个小写十六进制字符,无 TTL。
// 对账 leader 每开始一个可能以挂标记收尾的任务(全量预热、身份变化后的修补)时 SET 新令牌,
// 挂标记时 RouteReadyArmScript 要求令牌仍等于本任务的令牌。任务途中被 FLUSHALL / FLUSHDB 清掉、被新 leader 覆写、
// 被 rewarm 删掉,都会让这次挂标记失败 —— 不完整的数据挂不上标记,旧 leader 迟到的挂标记也挂不上。
const RouteWarmKey = "mapping:route:warm"

// RouteWatermarkKey 是库回档检测用的水位键(M3):string 类型的 JSON(RouteWatermark),不表示「副本完整」,
// 读方不认它。db_first 下对账 leader 不论 ArmReadyMarker 取值,每拍在库水位提交之后单调推进它;
// 它比库里的 route_reconcile.watermark_ms 还大(或世代不同),说明库丢了已提交的写(回档),leader 冻结映射写者。
const RouteWatermarkKey = "mapping:route:watermark"

// RouteWarmTokenLen 是预热令牌的长度(32 个小写十六进制字符,即 16 字节随机数)。
const RouteWarmTokenLen = 32

// ValidRouteWarmToken 报告 s 是否是合法的预热令牌。
func ValidRouteWarmToken(s string) bool {
	return lowerHexOfLen(s, RouteWarmTokenLen, RouteWarmTokenLen)
}

// ── 水位键 ────────────────────────────────────────────────────────────────────

// RouteWatermarkFormatVersion 是 RouteWatermark.V 当前的取值(字段只增不改名,不兼容的改动才 +1)。
const RouteWatermarkFormatVersion = 1

// RouteWatermark 是 mapping:route:watermark 的值。
//   - Generation:写入时 route_meta.generation;
//   - WatermarkMs:写入时已提交的 route_reconcile.watermark_ms(库时钟)。
type RouteWatermark struct {
	V           int    `json:"v"`
	Generation  string `json:"generation"`
	WatermarkMs uint64 `json:"watermark_ms"`
}

// ErrRouteWatermarkNewerVersion 与 ErrRouteReadyMarkerNewerVersion 同义:更新版本的写者写的,只记日志,不删也不信任。
var ErrRouteWatermarkNewerVersion = errors.New("placement: route watermark has a newer format version")

// EncodeRouteWatermark 校验后编码(字段顺序固定)。V 为 0 时按 RouteWatermarkFormatVersion 编码。
func EncodeRouteWatermark(w RouteWatermark) (string, error) {
	if w.V == 0 {
		w.V = RouteWatermarkFormatVersion
	}
	if w.V != RouteWatermarkFormatVersion {
		return "", fmt.Errorf("%w: route watermark v=%d, this binary writes v=%d", ErrMalformed, w.V, RouteWatermarkFormatVersion)
	}
	if err := w.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("%w: encode route watermark: %v", ErrMalformed, err)
	}
	return string(b), nil
}

// DecodeRouteWatermark 解析水位键的值,分两步解的理由同 DecodeRouteReadyMarker。
func DecodeRouteWatermark(raw string) (RouteWatermark, error) {
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal([]byte(raw), &head); err != nil {
		return RouteWatermark{}, fmt.Errorf("%w: route watermark is not a JSON object with an integer v: %v", ErrMalformed, err)
	}
	if head.V > RouteWatermarkFormatVersion {
		return RouteWatermark{}, fmt.Errorf("%w: v=%d, this binary knows v<=%d", ErrRouteWatermarkNewerVersion, head.V, RouteWatermarkFormatVersion)
	}
	var w RouteWatermark
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return RouteWatermark{}, fmt.Errorf("%w: route watermark is not a JSON object of the expected shape: %v", ErrMalformed, err)
	}
	if w.V != RouteWatermarkFormatVersion {
		return RouteWatermark{}, fmt.Errorf("%w: route watermark v=%d, want %d", ErrMalformed, w.V, RouteWatermarkFormatVersion)
	}
	if err := w.validate(); err != nil {
		return RouteWatermark{}, err
	}
	return w, nil
}

func (w RouteWatermark) validate() error {
	switch {
	case !lowerHexOfLen(w.Generation, 1, routeReadyGenerationMaxLen):
		return fmt.Errorf("%w: route watermark generation %q must be 1..%d chars of [0-9a-f]", ErrMalformed, w.Generation, routeReadyGenerationMaxLen)
	case w.WatermarkMs == 0:
		return fmt.Errorf("%w: route watermark watermark_ms must be > 0", ErrMalformed)
	}
	return nil
}

// FloorFor 返回这把水位键对 generation 世代的库水位下限:世代相同时是 WatermarkMs,否则 0(别的世代的水位不约束本世代)。
// 解冻(route_tool unfreeze)要把库水位抬到不低于它,否则解冻后 leader 下一拍会按「水位键 > 库水位」重新冻结。
func (w RouteWatermark) FloorFor(generation string) uint64 {
	if w.Generation != generation {
		return 0
	}
	return w.WatermarkMs
}

// ── 实例身份 ──────────────────────────────────────────────────────────────────

// RedisInstanceIdentity 是一个 mapping Redis 实例此刻的身份:INFO server 的 run_id(每次进程启动都换)、
// INFO replication 的 role 与 master_replid(副本被提成主时换)。
type RedisInstanceIdentity struct {
	RunID        string
	Role         string
	MasterReplID string
}

// ParseRedisInstanceIdentity 从 INFO server / INFO replication 的原文里取身份字段(认 CRLF 与 LF)。
// run_id / master_replid 必须是 40 个小写十六进制字符,role 必须非空,否则报错(调用方按「身份读不到」处理)。
// 与第 1b-1 批 internal/routetool.ParseRedisIdentity 同口径,另取 role。
func ParseRedisInstanceIdentity(infoServer, infoReplication string) (RedisInstanceIdentity, error) {
	id := RedisInstanceIdentity{
		RunID:        infoValue(infoServer, "run_id"),
		Role:         infoValue(infoReplication, "role"),
		MasterReplID: infoValue(infoReplication, "master_replid"),
	}
	if err := id.validate(); err != nil {
		return RedisInstanceIdentity{}, err
	}
	return id, nil
}

func (id RedisInstanceIdentity) validate() error {
	switch {
	case !lowerHexOfLen(id.RunID, routeReadyRedisIDLen, routeReadyRedisIDLen):
		return fmt.Errorf("%w: redis run_id %q must be %d chars of [0-9a-f]", ErrMalformed, id.RunID, routeReadyRedisIDLen)
	case !lowerHexOfLen(id.MasterReplID, routeReadyRedisIDLen, routeReadyRedisIDLen):
		return fmt.Errorf("%w: redis master_replid %q must be %d chars of [0-9a-f]", ErrMalformed, id.MasterReplID, routeReadyRedisIDLen)
	case id.Role == "":
		return fmt.Errorf("%w: redis role is empty", ErrMalformed)
	}
	return nil
}

// infoValue 取 INFO 原文里 "field:value" 一行的 value(整行匹配字段名,master_replid 不会误中 master_replid2)。
func infoValue(info, field string) string {
	prefix := field + ":"
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, prefix) {
			return line[len(prefix):]
		}
	}
	return ""
}

// ── 读方脚本(M1)────────────────────────────────────────────────────────────

// RouteReadyReadScript 在同一段只读 Lua 里取回:就绪标记原值、本实例的 run_id / role / master_replid、
// 以及调用方要读的映射键。
//
//	KEYS[1] = mapping:route:ready;KEYS[2..] = 映射键(player:placement / player:zone 等,单实例同库)
//	返回 {marker 或 nil, run_id, role, master_replid, v1, v2, …}
//
//   - 标记用 redis.pcall('GET'):键不是 string 类型时按「不存在」回 nil(与读方 MGET 口径一致),不让整段脚本报错;
//   - 映射键用 MGET:非 string 类型回 nil,与今天各读方的 MGET 一致;
//   - INFO 两段在 Lua 里用 string.match 取字段,只回传字段值,不回传整段文本;取不到回空串(Go 侧判 IdentityUnavailable);
//   - 身份与数据在同一段脚本里取:同一进程、同一时刻,检查与使用之间没有窗口;
//   - unpack 受 Lua C 栈上限约束(约 8000 个元素),一次传的映射键要由调用方分块(批量读方按既有批上限,远小于它)。
//
// 未核实(规格 §2 M1):Redis 7.2 允许脚本调 INFO(INFO 不带 noscript 标志,仓库 third_party/redis 8.4 源码与 miniredis 命令表
// 一致);真 Redis 用例取证。miniredis 不支持 INFO 的 server / replication 段,脚本的执行语义只能在真 Redis 上测。
const RouteReadyReadScript = `-- route ready read,spec_D1_batch1b2_2_3.md §2 M1
local m = redis.pcall('GET', KEYS[1])
if type(m) ~= 'string' then m = false end
local srv = redis.call('INFO', 'server')
local repl = redis.call('INFO', 'replication')
local out = {m,
  string.match(srv, 'run_id:(%x+)') or '',
  string.match(repl, 'role:(%a+)') or '',
  string.match(repl, 'master_replid:(%x+)') or ''}
if #KEYS > 1 then
  local vals = redis.call('MGET', unpack(KEYS, 2))
  for i = 1, #vals do out[4 + i] = vals[i] end
end
return out
`

// routeReadyReadFixed 是回复里固定的前缀长度(marker、run_id、role、master_replid)。
const routeReadyReadFixed = 4

// RouteReadyReadKeys 返回读脚本的 KEYS:就绪标记在前,映射键按调用方给的顺序在后。
func RouteReadyReadKeys(mappingKeys ...string) []string {
	keys := make([]string, 0, 1+len(mappingKeys))
	keys = append(keys, RouteReadyKey)
	return append(keys, mappingKeys...)
}

// RouteReadyRead 是读脚本一次回复的解析结果。
type RouteReadyRead struct {
	MarkerPresent bool
	MarkerRaw     string
	// Identity 是脚本执行时本实例的身份;字段取不到时为空串(不在这里校验,由 VerifyRouteReady 判)。
	Identity RedisInstanceIdentity
	// Values 与调用方给的映射键一一对应:nil = 键不存在(或不是 string 类型),否则是 string。
	// 类型取 []any 是为了能直接交给各读方现有的 MGET 结果解析函数。
	Values []any
}

// ErrUnexpectedRouteReadyReadReply 表示读脚本的回复形状不对(长度、类型):确定性错误,调用方按查询失败 fail-closed。
var ErrUnexpectedRouteReadyReadReply = errors.New("placement: unexpected route ready read reply")

// ParseRouteReadyRead 解析读脚本的回复。nMappingKeys 是调用方传的映射键个数。
// go-redis 与 go-zero 都把 Lua 的 false / nil 元素解成 nil,字符串解成 string。
func ParseRouteReadyRead(raw any, nMappingKeys int) (RouteReadyRead, error) {
	arr, ok := raw.([]any)
	if !ok {
		return RouteReadyRead{}, fmt.Errorf("%w: %T", ErrUnexpectedRouteReadyReadReply, raw)
	}
	if nMappingKeys < 0 || len(arr) != routeReadyReadFixed+nMappingKeys {
		return RouteReadyRead{}, fmt.Errorf("%w: %d elements, want %d", ErrUnexpectedRouteReadyReadReply, len(arr), routeReadyReadFixed+nMappingKeys)
	}
	var r RouteReadyRead
	switch m := arr[0].(type) {
	case nil:
	case string:
		r.MarkerPresent, r.MarkerRaw = true, m
	default:
		return RouteReadyRead{}, fmt.Errorf("%w: marker element is %T", ErrUnexpectedRouteReadyReadReply, arr[0])
	}
	fields := make([]string, 3)
	for i := range fields {
		s, ok := arr[1+i].(string)
		if !ok {
			return RouteReadyRead{}, fmt.Errorf("%w: identity element %d is %T", ErrUnexpectedRouteReadyReadReply, 1+i, arr[1+i])
		}
		fields[i] = s
	}
	r.Identity = RedisInstanceIdentity{RunID: fields[0], Role: fields[1], MasterReplID: fields[2]}
	r.Values = make([]any, nMappingKeys)
	for i := 0; i < nMappingKeys; i++ {
		switch v := arr[routeReadyReadFixed+i].(type) {
		case nil, string:
			r.Values[i] = v
		default:
			return RouteReadyRead{}, fmt.Errorf("%w: value %d is %T", ErrUnexpectedRouteReadyReadReply, i, v)
		}
	}
	return r, nil
}

// RouteReadyVerdict 是读方对一次读到的副本下的结论。只有 RouteReadyTrusted 允许信任值与缺键;
// 其余都是「副本不可信」,值与缺键都不能用来选库、判归属或判封锁。
type RouteReadyVerdict int

// 各取值的含义:
//   - Trusted:标记可解码,且标记记下的身份就是本次读所在的主实例;
//   - Absent:标记不存在(或不是 string);
//   - Malformed:标记解不开或字段不合规;
//   - NewerVersion:标记是更新格式版本写的,不信任,也不删;
//   - IdentityUnavailable:脚本里取不到本实例的 run_id / master_replid / role;
//   - NotMaster:本实例不是主(读到的是副本,或降级了的旧主);
//   - IdentityMismatch:标记记下的身份不是本实例(重启、切主、旧备份、被拷到别的实例)。
const (
	RouteReadyTrusted RouteReadyVerdict = iota
	RouteReadyAbsent
	RouteReadyMalformed
	RouteReadyNewerVersion
	RouteReadyIdentityUnavailable
	RouteReadyNotMaster
	RouteReadyIdentityMismatch
	routeReadyVerdictCount
)

var routeReadyVerdictNames = [routeReadyVerdictCount]string{
	"trusted", "absent", "malformed", "newer_version", "identity_unavailable", "not_master", "identity_mismatch",
}

// String 返回低基数的名字,可直接作指标 label。
func (v RouteReadyVerdict) String() string {
	if v < 0 || v >= routeReadyVerdictCount {
		return "unknown"
	}
	return routeReadyVerdictNames[v]
}

// RouteReadyVerdictNames 返回全部 verdict 名字(给指标启动预置 0 用),顺序与常量一致。
func RouteReadyVerdictNames() []string {
	out := make([]string, len(routeReadyVerdictNames))
	copy(out, routeReadyVerdictNames[:])
	return out
}

// VerifyRouteReady 判一次读到的副本可不可信(纯函数)。判定顺序:标记在不在 → 能否解码 → 实例身份取不取得到 →
// 是不是主 → 标记身份是否等于本实例。返回的 marker 只在能解码时非零(诊断用)。
func VerifyRouteReady(r RouteReadyRead) (RouteReadyVerdict, RouteReadyMarker) {
	if !r.MarkerPresent {
		return RouteReadyAbsent, RouteReadyMarker{}
	}
	m, err := DecodeRouteReadyMarker(r.MarkerRaw)
	switch {
	case errors.Is(err, ErrRouteReadyMarkerNewerVersion):
		return RouteReadyNewerVersion, RouteReadyMarker{}
	case err != nil:
		return RouteReadyMalformed, RouteReadyMarker{}
	}
	id := r.Identity
	if !lowerHexOfLen(id.RunID, routeReadyRedisIDLen, routeReadyRedisIDLen) ||
		!lowerHexOfLen(id.MasterReplID, routeReadyRedisIDLen, routeReadyRedisIDLen) || id.Role == "" {
		return RouteReadyIdentityUnavailable, m
	}
	if id.Role != "master" {
		return RouteReadyNotMaster, m
	}
	if m.RedisRunID != id.RunID || m.MasterReplID != id.MasterReplID {
		return RouteReadyIdentityMismatch, m
	}
	return RouteReadyTrusted, m
}

// ── 挂标记脚本(M2)──────────────────────────────────────────────────────────

// RouteReadyArmScript 由对账 leader 挂就绪标记,三个前提都在脚本里原子复核:
//
//	KEYS[1] = mapping:route:warm  KEYS[2] = mapping:route:ready
//	ARGV[1] = 本任务的预热令牌   ARGV[2] = 标记值   ARGV[3] = 标记里的 redis_run_id   ARGV[4] = 标记里的 master_replid
//	回 1:已挂;0:令牌不等于 ARGV[1](任务途中被清空 / 被新任务或新 leader 覆写 / 被 rewarm 删掉);
//	   2:标记已存在(不覆写);3:本实例身份不是 ARGV[3] / ARGV[4] 或不是主。0 / 2 / 3 都零写入。
//
// 不带 WAITAOF:调用方在同一条连接(同一个 pipeline)里紧跟 WAITAOF,确认标记落盘。
// 令牌键不是 string 类型时 GET 报 WRONGTYPE,脚本以错误回复结束、零写入。
const RouteReadyArmScript = `-- route ready arm (warm token fenced),spec_D1_batch1b2_2_3.md §2 M2
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
if redis.call('EXISTS', KEYS[2]) == 1 then return 2 end
local srv = redis.call('INFO', 'server')
local repl = redis.call('INFO', 'replication')
if (string.match(srv, 'run_id:(%x+)') or '') ~= ARGV[3]
  or (string.match(repl, 'master_replid:(%x+)') or '') ~= ARGV[4]
  or (string.match(repl, 'role:(%a+)') or '') ~= 'master' then
  return 3
end
redis.call('SET', KEYS[2], ARGV[2])
return 1
`

// RouteReadyArmOutcome 与 RouteReadyArmScript 的返回码一一对应。
type RouteReadyArmOutcome int64

const (
	RouteReadyArmTokenMismatch   RouteReadyArmOutcome = 0
	RouteReadyArmArmed           RouteReadyArmOutcome = 1
	RouteReadyArmAlreadyPresent  RouteReadyArmOutcome = 2
	RouteReadyArmIdentityChanged RouteReadyArmOutcome = 3
)

// String 返回低基数的名字。
func (o RouteReadyArmOutcome) String() string {
	switch o {
	case RouteReadyArmTokenMismatch:
		return "token_mismatch"
	case RouteReadyArmArmed:
		return "armed"
	case RouteReadyArmAlreadyPresent:
		return "already_present"
	case RouteReadyArmIdentityChanged:
		return "identity_changed"
	}
	return "unknown"
}

// RouteReadyArmKeysAndArgs 按标记组装挂标记脚本的 KEYS / ARGV:身份参数取自标记本身,保证「脚本复核的身份」
// 与「写进去的标记里的身份」是同一份。标记或令牌不合法时报错,不发脚本。
func RouteReadyArmKeysAndArgs(token string, m RouteReadyMarker) ([]string, []any, error) {
	if !ValidRouteWarmToken(token) {
		return nil, nil, fmt.Errorf("%w: warm token %q must be %d chars of [0-9a-f]", ErrMalformed, token, RouteWarmTokenLen)
	}
	raw, err := EncodeRouteReadyMarker(m)
	if err != nil {
		return nil, nil, err
	}
	return []string{RouteWarmKey, RouteReadyKey}, []any{token, raw, m.RedisRunID, m.MasterReplID}, nil
}

// ClassifyRouteReadyArmReply 解析挂标记脚本的回复;不是 0..3 的整数一律 ErrUnexpectedRouteReadyReadReply 同类错误。
func ClassifyRouteReadyArmReply(raw any) (RouteReadyArmOutcome, error) {
	n, ok := raw.(int64)
	if !ok || n < 0 || n > 3 {
		return 0, fmt.Errorf("%w: arm reply %T %v", ErrUnexpectedRouteReadyReadReply, raw, raw)
	}
	return RouteReadyArmOutcome(n), nil
}

// ── 合走标记 merge:merged_into:{src} ──────────────────────────────────────────

// MergedIntoKeyPrefix 是合走标记的键前缀,键的后缀是**被合走的源区** zone id(十进制)。
const MergedIntoKeyPrefix = "merge:merged_into:"

// MergedIntoKey 返回 merge:merged_into:{src}。
func MergedIntoKey(src uint32) string {
	return MergedIntoKeyPrefix + strconv.FormatUint(uint64(src), 10)
}

// mergedIntoValue 与 tools/merge_zone/merged_into.go 的 mergedIntoValue 逐字段相同(字段只增不改名)。
// 用 json.Marshal 编码,与 merge_zone 写出的值逐字节相同(TestMergedIntoValueMatchesMergeZoneEncoding 钉住)。
type mergedIntoValue struct {
	Dst   uint32 `json:"dst"`
	RunID string `json:"run_id"`
}

// EncodeMergedIntoValue 编码合走标记的值。dst 必须 ≥ 1,run_id 必须过 ValidRunID。
func EncodeMergedIntoValue(dst uint32, runID string) (string, error) {
	if dst == 0 {
		return "", fmt.Errorf("%w: merged_into dst must be >= 1", ErrMalformed)
	}
	if !ValidRunID(runID) {
		return "", fmt.Errorf("%w: merged_into run_id %q must be 1..64 bytes of [A-Za-z0-9_-]", ErrMalformed, runID)
	}
	b, err := json.Marshal(mergedIntoValue{Dst: dst, RunID: runID})
	if err != nil {
		return "", fmt.Errorf("%w: encode merged_into: %v", ErrMalformed, err)
	}
	return string(b), nil
}

// DecodeMergedIntoValue 解析合走标记的值(未知字段忽略)。src 用来拒绝 dst == src 的自指值。
func DecodeMergedIntoValue(src uint32, raw string) (dst uint32, runID string, err error) {
	var v mergedIntoValue
	if jerr := json.Unmarshal([]byte(raw), &v); jerr != nil {
		return 0, "", fmt.Errorf("%w: merged_into value %q is not {dst, run_id}: %v", ErrMalformed, raw, jerr)
	}
	if v.Dst == 0 || v.Dst == src {
		return 0, "", fmt.Errorf("%w: merged_into dst %d must be >= 1 and differ from src %d", ErrMalformed, v.Dst, src)
	}
	if !ValidRunID(v.RunID) {
		return 0, "", fmt.Errorf("%w: merged_into run_id %q must be 1..64 bytes of [A-Za-z0-9_-]", ErrMalformed, v.RunID)
	}
	return v.Dst, v.RunID, nil
}
