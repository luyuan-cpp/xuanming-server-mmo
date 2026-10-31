package placement

// 本文件是「映射持久化(D1)」在 Redis 一侧的契约:新增的两把键、rev 的格式与比较、
// 发布脚本(set-if-newer)全文及其回复分类(docs/design/player-route-mapping-durability-proposal.md §3.3、§4.2,
// 第 1a 批定稿)。
//
// 真源在全局库 player_route(data_service 的 store 包);mapping Redis 里的 player:zone / player:placement
// 只是它的副本,player:maprev 记录副本已经到了库里哪一版(rev),发布脚本按它做 set-if-newer。
//
// 与 placement.go 同一约定:只依赖标准库。Redis 的读写代码不放这里 —— data_service 与 tools/merge_zone
// 用的 Redis 客户端不同,各自用自己的客户端执行 PublishRouteScript,这里只给脚本、键和回复的判定口径。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// MapRevKeyPrefix 是 player:maprev:{id} 的前缀:值是十进制 rev(无前导零),无 TTL。
// 只用于 set-if-newer 排序和审计;旧读方不认识它,也不受它影响。
const MapRevKeyPrefix = "player:maprev:"

// MapRevKey 返回 player:maprev:{id}。
func MapRevKey(playerID uint64) string {
	return MapRevKeyPrefix + strconv.FormatUint(playerID, 10)
}

// RouteReadyKey 是「副本完整可信」标记(提案 §3.3)。第 1a 批只定义,不写也不读:
// 写它的是第 1b 批的对账 leader,提前写会让读方误以为缓存缺键就是权威缺席。
//
// 2026-10-18(第 1b-1 批)补:值的格式见 RouteReadyMarker(string 类型的 JSON)。写者只有 db_first 的对账 leader
// (第 1b-2 批),用 SET NX 写;任何一方都可以按原值 compare-and-delete(RouteReadyCompareAndDeleteScript,撤掉信任是
// fail-closed 的方向);route_tool unfreeze 只做 compare-and-set 改 generation、或 compare-and-delete 撤掉畸形 / 失效值。
// 读方(第 3 批)只看「存在且非空」,不解析值。第 1b-1 批仍没有任何写者 SET 它。
// 2026-10-19 更正(spec_D1_batch1b2_2_3.md §2 M1):上一句已作废。读方用 route_ready.go 的 RouteReadyReadScript
// 在同一段 Lua 里取标记、本实例身份与映射键,经 VerifyRouteReady 判定:标记可解码且身份等于本实例才信任,
// 只看「存在」会把重启 / 切主 / 旧备份恢复后留下的旧标记当真。写者是对账 leader 的 RouteReadyArmScript(带预热令牌)。
const RouteReadyKey = "mapping:route:ready"

// RouteReadyFormatVersion 是 RouteReadyMarker.V 当前的取值。字段只增不改名、解析时忽略未知字段,
// 只有不兼容的改动才 +1。
const RouteReadyFormatVersion = 1

// RouteReadyMarker 是 mapping:route:ready 的值(提案 §11.2「第 1b 批」)。必须存成 **string** 类型的 JSON:
// 读方(go/db、data_service、guild)经 RouteReadyReadScript 的 redis.pcall('GET') 读它,键不是 string 类型时按缺席处理
// (verdict=absent),整段脚本不报错;读方不再用 MGET 读标记(2026-10-20 更正,原句写的是第 3 批之前的 MGET 设计)。
//
//   - Generation:写标记时 route_meta.generation(库世代),与库不等即说明库被重置过;
//   - RedisRunID / MasterReplID:写标记时 mapping Redis 的 INFO server run_id 与 INFO replication master_replid,
//     与当前实例不等 = Redis 重启 / 切主 / 换实例,标记不再可信;
//   - SetAtMs:写标记时的库时钟(诊断;第 1b-2 批 rewarm 用它判「新标记」);
//   - WatermarkMs:写 / 每拍 CAS 更新标记时的 route_reconcile.cache_verified_ms(cvm,「缓存已验证到」的库时钟;
//     spec_D1_batch1b2_2_3.md §13.1 第 2 条),不是库水位 watermark_ms。只用于两处:身份变化后的回看修补起点
//     max(0, min(WatermarkMs, 库 cvm) − RepairLookbackMs),以及 route_tool unfreeze 取库水位下限。**库回档检测不用它**,
//     只看水位键 RouteWatermarkKey(M3)、世代与 cache_ahead —— 不要拿它当库水位或回档判据;
//   - Leader:写标记的 leader 标识(诊断),字符集与 player_route.updated_by 相同。
type RouteReadyMarker struct {
	V            int    `json:"v"`
	Generation   string `json:"generation"`
	RedisRunID   string `json:"redis_run_id"`
	MasterReplID string `json:"master_replid"`
	SetAtMs      uint64 `json:"set_at_ms"`
	WatermarkMs  uint64 `json:"watermark_ms"`
	Leader       string `json:"leader"`
}

// 标记各字段的长度约束。generation 与 route_meta.generation(varchar(32) ascii,迁移种 32 个十六进制字符)同口径;
// run_id 与 master_replid 是 Redis 生成的 40 个十六进制字符;leader 与 route_reconcile.leader 的列宽 varchar(96) 同口径。
const (
	routeReadyGenerationMaxLen = 32
	routeReadyRedisIDLen       = 40
	routeReadyLeaderMaxLen     = 96
)

// ErrRouteReadyMarkerNewerVersion 表示标记的 v 大于本二进制认识的 RouteReadyFormatVersion:它是更新版本的写者写的。
// 调用方只记日志,**既不删也不信任**(删了会与新版本的写者来回打架;信任了就是按看不懂的格式下结论)。
// 与 ErrMalformed 分开:畸形值可以按原值 compare-and-delete 撤掉,新版本的值不行。
var ErrRouteReadyMarkerNewerVersion = errors.New("placement: route ready marker has a newer format version")

// EncodeRouteReadyMarker 校验后编码成标记值(字段顺序固定,同一份 m 每次编码逐字节相同)。
// V 为 0 时按 RouteReadyFormatVersion 编码;其它取值必须等于它。
func EncodeRouteReadyMarker(m RouteReadyMarker) (string, error) {
	if m.V == 0 {
		m.V = RouteReadyFormatVersion
	}
	if m.V != RouteReadyFormatVersion {
		return "", fmt.Errorf("%w: route ready marker v=%d, this binary writes v=%d", ErrMalformed, m.V, RouteReadyFormatVersion)
	}
	if err := m.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("%w: encode route ready marker: %v", ErrMalformed, err)
	}
	return string(b), nil
}

// DecodeRouteReadyMarker 解析标记值。未知字段忽略(字段只增不改名)。
//   - v 大于 RouteReadyFormatVersion → ErrRouteReadyMarkerNewerVersion(不再校验其余字段:新格式的规则本二进制不知道);
//   - 其余任何不合规(不是 JSON 对象、v 不是 1、generation 不是 1..32 个 [0-9a-f]、run_id / replid 不是 40 个 [0-9a-f]、
//     set_at_ms / watermark_ms 为 0、leader 不是 1..96 字节的 [A-Za-z0-9._@:-])→ 用 %w 包 ErrMalformed。
//
// 分两步解(2026-10-18 修复评审):先只解 v,再按 v=1 的结构整体解。一步整体解时,新版本若改了某个已知字段的类型
// (例如 set_at_ms 改成字符串),json.Unmarshal 先报类型错,还没看 v 就判成 ErrMalformed,调用方随即按原值 compare-and-delete,
// 删掉新版本写者的标记 —— 正是 ErrRouteReadyMarkerNewerVersion 要防的来回打架。v 本身解不出来(不是对象、v 不是整数)才算畸形。
func DecodeRouteReadyMarker(raw string) (RouteReadyMarker, error) {
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal([]byte(raw), &head); err != nil {
		return RouteReadyMarker{}, fmt.Errorf("%w: route ready marker is not a JSON object with an integer v: %v", ErrMalformed, err)
	}
	if head.V > RouteReadyFormatVersion {
		return RouteReadyMarker{}, fmt.Errorf("%w: v=%d, this binary knows v<=%d", ErrRouteReadyMarkerNewerVersion, head.V, RouteReadyFormatVersion)
	}
	var m RouteReadyMarker
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return RouteReadyMarker{}, fmt.Errorf("%w: route ready marker is not a JSON object of the expected shape: %v", ErrMalformed, err)
	}
	if m.V != RouteReadyFormatVersion {
		return RouteReadyMarker{}, fmt.Errorf("%w: route ready marker v=%d, want %d", ErrMalformed, m.V, RouteReadyFormatVersion)
	}
	if err := m.validate(); err != nil {
		return RouteReadyMarker{}, err
	}
	return m, nil
}

// validate 校验 v 以外的字段(手写字符校验、不引 regexp,与 ValidRev 同口径)。
func (m RouteReadyMarker) validate() error {
	switch {
	case !lowerHexOfLen(m.Generation, 1, routeReadyGenerationMaxLen):
		return fmt.Errorf("%w: route ready marker generation %q must be 1..%d chars of [0-9a-f]", ErrMalformed, m.Generation, routeReadyGenerationMaxLen)
	case !lowerHexOfLen(m.RedisRunID, routeReadyRedisIDLen, routeReadyRedisIDLen):
		return fmt.Errorf("%w: route ready marker redis_run_id %q must be %d chars of [0-9a-f]", ErrMalformed, m.RedisRunID, routeReadyRedisIDLen)
	case !lowerHexOfLen(m.MasterReplID, routeReadyRedisIDLen, routeReadyRedisIDLen):
		return fmt.Errorf("%w: route ready marker master_replid %q must be %d chars of [0-9a-f]", ErrMalformed, m.MasterReplID, routeReadyRedisIDLen)
	case m.SetAtMs == 0 || m.WatermarkMs == 0:
		return fmt.Errorf("%w: route ready marker set_at_ms=%d watermark_ms=%d must both be > 0", ErrMalformed, m.SetAtMs, m.WatermarkMs)
	case !routeReadyLeaderOK(m.Leader):
		return fmt.Errorf("%w: route ready marker leader %q must be 1..%d bytes of [A-Za-z0-9._@:-]", ErrMalformed, m.Leader, routeReadyLeaderMaxLen)
	}
	return nil
}

// lowerHexOfLen 报告 s 是否由 [minLen, maxLen] 个 [0-9a-f] 组成(大写不算:三种来源都只产小写)。
func lowerHexOfLen(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// routeReadyLeaderOK 与 data_service store 包的 validRouteWriter 同一字符集与上限(leader 存进 route_reconcile.leader,
// 列是 ascii varchar(96))。这里镜像一份而不是 import:本包不依赖 data_service。
func routeReadyLeaderOK(s string) bool {
	if s == "" || len(s) > routeReadyLeaderMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '@', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// RouteReadyCompareAndSetScript 按原值换标记:KEYS[1] 当前值等于 ARGV[1] 才 SET 成 ARGV[2](不带过期),回 1;
// 否则零写入,回 0。键不在时 GET 回 false,与任何 ARGV[1] 都不等,回 0。键不是 string 类型时 GET 报 WRONGTYPE,
// 脚本以错误回复结束、零写入。
const RouteReadyCompareAndSetScript = `-- route ready marker compare-and-set,docs/design/player-route-mapping-durability-proposal.md §11.2(第 1b 批)
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2])
  return 1
end
return 0
`

// RouteReadyCompareAndDeleteScript 按原值撤标记:KEYS[1] 当前值等于 ARGV[1] 才 DEL,回 1;否则零写入,回 0。
// 「只删自己读到的那一版」:读到之后被别人换成新值的标记不会被误删。
const RouteReadyCompareAndDeleteScript = `-- route ready marker compare-and-delete,docs/design/player-route-mapping-durability-proposal.md §11.2(第 1b 批)
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`

// RouteRowShards 是 player_route 的固定分片数:route_shard = player_id % RouteRowShards,
// 是聚簇主键 (route_shard, player_id) 的首列。目的是把单调递增的 player_id 在 TiDB 上拆成 64 条写入尾部
// (docs/design/player-route-mapping-durability-proposal.md §11.2「第 1a 批热点修复」):号段发号在每个 login 段内
// 严格递增,只按 player_id 排序的主键 / 索引在开服首日始终只写同一个尾部 Region。
//
// **模数建表后不可改**(任何环境建过 player_route 之后):改模数 = 每一行换主键 = 整表重写;
// data_service store 包 schema.go 的 chk_route_shard 里写死的模数与这里必须一致(route_store_test.go 钉住)。
// 上限 256:route_shard 列是 tinyint unsigned。
//
// 2026-10-13 按容量模型(docs/design/capacity-model-hok-scale.md §4.1)定为 64:单 Region 的写入上限没测过,
// 而模数建表后改不了,取 64 是保险。此前 HEAD(d5cbb8a8e)按 16 提交过:**按 16 建过表的库必须先 DROP 四张路由表
// (player_route、zone_merged_into、route_meta、route_reconcile)再迁移**(提案 §11.2
// 「行片 16 → 64」的前置,做法同 HR4)。不 DROP 的话旧表的 CHECK 仍是 % 16:建 route store 的入口(db_first 的
// data_service、route_tool、debugutil)建池时由探针 P5 拒绝(store.ErrRouteShardModulusMismatch;data_service 不装配
// route store、RegisterPlayerZone 一律 fail-closed,Store.Required=true 时拒启;route_tool 退出码 2);redis 模式的
// data_service 不建 route store、认不出旧模数,所以人工 SHOW CREATE TABLE player_route 核对仍要做(提案 §11.2
// 「正向探针 P5」)。
const RouteRowShards uint32 = 64

// RouteRowShard 返回 player_route.route_shard。写者与读者都只能经它计算,不得自己再写一份取模 —— 算错片的写者
// 在 CHECK 不生效的库上能给同一个 player_id 写出两行,而按正确片点查的读者看不到另一行。
// 2026-10-19 更正:tools/merge_zone **没有** import shared(独立 module,不改 go.mod),第 2 批在 route_publish.go 里
// 镜像了本常量与取模,由 merge_zone 的 TestMirroredRouteScriptsMatchShared 按源码逐字比对,并在开写前跑 P4 / P5 探针。
func RouteRowShard(playerID uint64) uint32 {
	return uint32(playerID % uint64(RouteRowShards))
}

// maxRevDigits 是 uint64 十进制的最大位数(18446744073709551615)。
const maxRevDigits = 20

// FormatRev 把库里的 rev 格式化成发布脚本要的十进制串。rev == 0 不是合法版本(chk_route_rev 要求 >= 1),
// 返回空串;调用方必须先拒绝 0,空串交给脚本会被当成坏参数拒掉,不会写进缓存。
func FormatRev(rev uint64) string {
	if rev == 0 {
		return ""
	}
	return strconv.FormatUint(rev, 10)
}

// ValidRev 判断 s 是否是规范的 rev:^[1-9][0-9]{0,19}$ 且不超过 MaxUint64。
//
// 规范形式(无前导零、无符号、无空白)是「先比长度再比字典序」成立的前提:允许 "010" 的话,
// "010" 与 "9" 的比较结果就错了。与 Lua 里的 valid_rev 同口径,只多一条 MaxUint64 上限
// (Lua 那边只靠写者纪律,脚本写进缓存的 rev 只可能来自本函数校验过的参数)。
func ValidRev(s string) bool {
	if len(s) < 1 || len(s) > maxRevDigits || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	// 字符集与长度都对时,ParseUint 只剩「超过 MaxUint64」一种失败。
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// CompareRev 比较两个 rev:a<b 回 -1,相等回 0,a>b 回 1。先比长度、再逐字节比,
// 与发布脚本的 rev_ge 同一判据(不经 double,也不依赖 locale)。任一方不规范时回 ErrMalformed。
func CompareRev(a, b string) (int, error) {
	if !ValidRev(a) || !ValidRev(b) {
		return 0, fmt.Errorf("%w: rev %q vs %q", ErrMalformed, a, b)
	}
	switch {
	case len(a) < len(b):
		return -1, nil
	case len(a) > len(b):
		return 1, nil
	case a < b:
		return -1, nil
	case a > b:
		return 1, nil
	default:
		return 0, nil
	}
}

// PublishRouteKeys 返回发布脚本的 KEYS:player:zone、player:placement、player:maprev。
//
// 多键脚本成立的前提是 mapping Redis 为单实例(非 Cluster),所以键里不带 hash tag。
func PublishRouteKeys(playerID uint64) []string {
	return []string{HomeZoneKey(playerID), Key(playerID), MapRevKey(playerID)}
}

// PublishRouteScript 是把库里一行发布到 mapping Redis 的脚本(set-if-newer)。
//
// 在提案 §4.2 的基础上加了四处(规格 b.1):rev 参数校验、
// 无 maprev 时的遗留值守卫(不静默覆盖 P1 导入之前或绕过本脚本的写者留下的值)、返回 0 时把 maprev
// 原值写回一次(touch,让同一连接随后的 WAITAOF 覆盖「此刻已在缓存里的状态」)、逐字节比较。
//
// 用 EVAL 而不是 EVALSHA:pipeline 里遇到 NOSCRIPT 无法透明回退,且与 data_service 现有脚本
// (router.go 全部 EvalCtx)一致。不加 shebang:miniredis 是否兼容未核实。
//
// 回复形状与含义见 ClassifyPublishReply;改脚本必须同步改它和 route_test.go 的向量。
const PublishRouteScript = `-- route publish (set-if-newer),docs/design/player-route-mapping-durability-proposal.md §4.2(第 1a 批定稿)
-- KEYS: 1=player:zone:{id} 2=player:placement:{id} 3=player:maprev:{id}
-- ARGV: 1=rev(十进制,无前导零,1..20 位) 2=home(''=DEL) 3=placement(''=DEL)
-- 返回: {1, rev}                          已写入三键
--       {0, cur, curHome, curPlacement}   缓存 rev >= 本次:映射未改(maprev 原值写回一次,见下)
--       {2, '', curHome, curPlacement}    缓存无 maprev 且遗留值与本次不同:零写入
local function valid_rev(s)
  return type(s) == 'string' and #s >= 1 and #s <= 20 and string.match(s, '^[1-9][0-9]*$') ~= nil
end
-- 两边都已过 valid_rev:先比长度再逐字节比。不经 Lua number(double),也不依赖 strcoll / locale。
local function rev_ge(a, b)
  if #a ~= #b then return #a > #b end
  for i = 1, #a do
    local x, y = string.byte(a, i), string.byte(b, i)
    if x ~= y then return x > y end
  end
  return true
end
local rev = ARGV[1]
if not valid_rev(rev) then
  return redis.error_reply('ERR route publish: bad rev argument')
end
local cur = redis.call('GET', KEYS[3])
local curHome = redis.call('GET', KEYS[1])
local curPlc = redis.call('GET', KEYS[2])
if cur then
  if not valid_rev(cur) then
    return redis.error_reply('ERR route publish: malformed maprev')
  end
  if rev_ge(cur, rev) then
    -- 同值写回:让本连接随后的 WAITAOF 覆盖「此刻已在缓存里的状态」,
    -- 否则幂等重发(上次的 WAITAOF 超时)会在没确认落盘的情况下回成功。
    redis.call('SET', KEYS[3], cur)
    return {0, cur, curHome or '', curPlc or ''}
  end
else
  -- 无 maprev:这两把键要么从没写过,要么是 P1 导入之前、或绕过本脚本的写者(第 1a 批的 merge_zone)
  -- 留下的遗留值。遗留值存在且与本次不同时一律不覆盖 —— 静默改回库里的旧值就是写错库。
  if (curHome and curHome ~= ARGV[2]) or (curPlc and curPlc ~= ARGV[3]) then
    return {2, '', curHome or '', curPlc or ''}
  end
end
if ARGV[2] == '' then redis.call('DEL', KEYS[1]) else redis.call('SET', KEYS[1], ARGV[2]) end
if ARGV[3] == '' then redis.call('DEL', KEYS[2]) else redis.call('SET', KEYS[2], ARGV[3]) end
-- maprev 最后写,表示「三键已到这一版」。Redis 7 对没有 shebang 的脚本只在第一条写命令前做一次 OOM 检查,
-- 之后不会中途拒写,所以不存在 OOM 半写;顺序只为可读。
redis.call('SET', KEYS[3], rev)
return {1, rev}
`

// PublishOutcome 是一次发布脚本调用的结局。
type PublishOutcome uint8

const (
	// PublishApplied 脚本回 {1, rev}:三键已写成本次的值。
	PublishApplied PublishOutcome = iota + 1
	// PublishSuperseded 脚本回 {0, cur, h, p} 且 cur > rev:缓存已是更新的版本,本次不写。
	// 是否算成功由写者再看 CurHome(第 1a 批 home 写定后不再改,cur 更新而 home 不同只可能是库被回档
	// 或有写者绕过了本脚本,写者必须 fail-closed)。
	PublishSuperseded
	// PublishConfirmed 脚本回 {0, cur, h, p} 且 cur == rev、值与本次相同:幂等重发,已确认。
	PublishConfirmed
	// PublishConflict 脚本回 {0, cur, h, p} 且 cur == rev、值与本次不同:有绕过本脚本的写者,
	// 或库回档后 rev 被复用。写者按失败返回。
	PublishConflict
	// PublishLegacyConflict 脚本回 {2, '', h, p}:缓存没有 maprev,而遗留的 zone / placement 与本次不同。
	// 零写入,写者按失败返回(覆盖它就是把缓存改成可能已过时的库值)。
	PublishLegacyConflict
)

// String 仅供日志与指标 label(低基数,五个固定值),与 data_service_route_publish_total{result} 的取值同名。
func (o PublishOutcome) String() string {
	switch o {
	case PublishApplied:
		return "applied"
	case PublishSuperseded:
		return "superseded"
	case PublishConfirmed:
		return "confirmed"
	case PublishConflict:
		return "conflict"
	case PublishLegacyConflict:
		return "legacy_conflict"
	default:
		return "unknown"
	}
}

// PublishReply 是分类后的脚本回复。CurRev / CurHome / CurPlacement 是脚本执行那一刻缓存里的值
// (Applied 时就是本次写入的值;LegacyConflict 时 CurRev 为空)。
type PublishReply struct {
	Outcome      PublishOutcome
	CurRev       string
	CurHome      string
	CurPlacement string
}

// ErrUnexpectedPublishReply 表示脚本回复不是 PublishRouteScript 能产生的三种形状之一。
// 写者必须 fail-closed(当作发布失败,不重试):形状不对说明脚本被改过、或连的根本不是同一套契约。
var ErrUnexpectedPublishReply = errors.New("placement: unexpected route publish reply")

// ClassifyPublishReply 把 PublishRouteScript 的回复分类。rev / home / plc 是本次发给脚本的 ARGV。
//
// 只接受三种形状(go-redis 把 Lua 表回成 []any,整数是 int64、串是 string):
//   - {int64(1), rev}:第二个元素必须等于 rev;
//   - {int64(0), cur, h, p}:cur 必须规范且 >= rev(> 为 Superseded;== 时按值是否相同分 Confirmed / Conflict);
//   - {int64(2), "", h, p}:且 h != home || p != plc。
//
// 其余一律回 ErrUnexpectedPublishReply,包括「0 回复里 cur < rev」「2 回复里值反而相同」
// 「cur 是 20 位数字但超过 MaxUint64」—— 脚本不可能产生这些状态,出现就是契约被破坏,按 fail-closed 处理。
func ClassifyPublishReply(raw any, rev, home, plc string) (PublishReply, error) {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return PublishReply{}, fmt.Errorf("%w: %T %v", ErrUnexpectedPublishReply, raw, raw)
	}
	code, ok := arr[0].(int64)
	if !ok {
		return PublishReply{}, fmt.Errorf("%w: first element %T %v is not an integer", ErrUnexpectedPublishReply, arr[0], arr[0])
	}
	switch code {
	case 1:
		if len(arr) != 2 {
			return PublishReply{}, fmt.Errorf("%w: applied reply has %d elements, want 2", ErrUnexpectedPublishReply, len(arr))
		}
		got, ok := arr[1].(string)
		if !ok || got != rev {
			return PublishReply{}, fmt.Errorf("%w: applied reply carries rev %v, sent %q", ErrUnexpectedPublishReply, arr[1], rev)
		}
		return PublishReply{Outcome: PublishApplied, CurRev: rev, CurHome: home, CurPlacement: plc}, nil
	case 0:
		cur, h, p, err := publishReplyTail(arr, "unchanged")
		if err != nil {
			return PublishReply{}, err
		}
		cmp, err := CompareRev(cur, rev)
		if err != nil {
			return PublishReply{}, fmt.Errorf("%w: unchanged reply: %w", ErrUnexpectedPublishReply, err)
		}
		reply := PublishReply{CurRev: cur, CurHome: h, CurPlacement: p}
		switch {
		case cmp < 0:
			// 脚本只在 cur >= rev 时回 0;cur < rev 却回 0 说明比较口径被改过。
			return PublishReply{}, fmt.Errorf("%w: unchanged reply with cached rev %s < sent rev %s", ErrUnexpectedPublishReply, cur, rev)
		case cmp > 0:
			reply.Outcome = PublishSuperseded
		case h == home && p == plc:
			reply.Outcome = PublishConfirmed
		default:
			reply.Outcome = PublishConflict
		}
		return reply, nil
	case 2:
		first, h, p, err := publishReplyTail(arr, "legacy")
		if err != nil {
			return PublishReply{}, err
		}
		if first != "" {
			return PublishReply{}, fmt.Errorf("%w: legacy reply carries rev %q, want empty", ErrUnexpectedPublishReply, first)
		}
		if h == home && p == plc {
			// 脚本只在遗留值与本次不同时回 2;值相同却回 2 说明守卫口径被改过。
			return PublishReply{}, fmt.Errorf("%w: legacy reply with values identical to the sent ones", ErrUnexpectedPublishReply)
		}
		return PublishReply{Outcome: PublishLegacyConflict, CurHome: h, CurPlacement: p}, nil
	default:
		return PublishReply{}, fmt.Errorf("%w: unknown reply code %d", ErrUnexpectedPublishReply, code)
	}
}

// publishReplyTail 取 0 / 2 回复的后三个元素,三者都必须是串。
func publishReplyTail(arr []any, kind string) (string, string, string, error) {
	if len(arr) != 4 {
		return "", "", "", fmt.Errorf("%w: %s reply has %d elements, want 4", ErrUnexpectedPublishReply, kind, len(arr))
	}
	var out [3]string
	for i := range out {
		s, ok := arr[i+1].(string)
		if !ok {
			return "", "", "", fmt.Errorf("%w: %s reply element %d is %T, want string", ErrUnexpectedPublishReply, kind, i+1, arr[i+1])
		}
		out[i] = s
	}
	return out[0], out[1], out[2], nil
}
