package logic

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"time"

	componentpb "proto/common/component"
	scenepb "proto/scene"

	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/currencytype"
	"shared/generated/table"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 神秘商人的调度:每隔一段时间(默认每个整点)在候选地图的全部可进分线里随机挑一条,让商人落在
// 那条线的某个站位上,停留一段时间(默认 30 分钟),并掷出这一轮的货架(卖哪几件、各打几折)。
// 设计文档:docs/design/mystery-merchant.md §4。
//
// 为什么放在 scene_manager:
//   - 「全 zone 只有一个商人」需要一个全 zone 唯一的决策者,而领导者选举这里已经有;
//   - 线号与「哪些线此刻可进」只有 scene_manager 知道(分线目录就是它发布的),scene 节点只知道
//     自己身上的线。
//
// 本文件只做决策与发布,不碰任何玩家资产:结果整份写进 Redis(一把 HASH,见 MysteryMerchantKeyFmt),
// scene 节点只读它来回答查询、校验购买、发出现公告。所以这里出任何错,最坏结果是「这一轮没有商人」,
// 不会多发也不会少扣。
//
// 对既有状态全程只读:只读分线目录(world_channel_directory:*)与配置表;写的只有自己的那一把键。
const (
	// MysteryMerchantKeyFmt:HASH per zone。
	//   field "state" = scenepb.MysteryMerchantState 的序列化字节;
	//   field "v"     = round_id * mysteryMerchantVersionRoundFactor + revision 的十进制,只供写入脚本比较。
	//
	// **跨语言契约,字面量不许改**:C++ scene 节点按同一格式拼键、读同一个 field,对应常量是
	// cpp/libs/services/scene/player/system/player_mystery_merchant.cpp 的 kMysteryMerchantKeyFmt /
	// kMysteryMerchantStateField(那边的占位符是 C printf 的 %u,这边是 Go 的 %d;契约是**渲染出来的键**
	// 逐字节相同:mystery_merchant:zone:{zone 十进制})。改这里必须同批改那里;
	// TestMysteryMerchantKey_CrossLanguageContract 钉住本侧字面量与渲染结果。
	//
	// 键名刻意不用 world_channels:zone: 前缀:孤儿清理按那个前缀 SCAN(orphan_cleanup.go)。
	MysteryMerchantKeyFmt = "mystery_merchant:zone:%d"
	// MysteryMerchantStateField / MysteryMerchantVersionField 是上面那把 HASH 的两个 field 名。
	MysteryMerchantStateField   = "state"
	MysteryMerchantVersionField = "v"

	// 版本号 = round_id × 1000 + revision。revision 是「同一轮内第几次落位」,从 1 起,
	// 只有商人所在的线被回收 / 销毁时才 +1,所以 3 位足够(到 999 就不再换线,见 relocate)。
	// round_id 是这一轮出现时刻的 Unix 秒(见 merchantRoundAt),约 1.8×10^9,乘 1000 后约 1.8×10^12,
	// 远小于 2^53 —— 写入脚本在 Lua 里把它当 number 比较是精确的。
	mysteryMerchantVersionRoundFactor uint64 = 1000
	mysteryMerchantMaxRevision        uint32 = 999

	// 记录的 TTL = 间隔的这么多倍。记录本身带 expire_at_ms,读侧按它判定商人在不在;TTL 只负责
	// 「功能被关掉 / 调度长期停摆之后,旧记录自己消失」,所以取得宽松。
	mysteryMerchantStateTTLRounds int64 = 2

	// 商人所在的线连续这么多拍不在分线目录里,才判定「线没了」并换线。
	// 不是 1:autoscaler 开始排空一条线时,有一个毫秒级窗口让它在「在役」「回收中」两个集合里都读不到
	// (world_channel_directory.go luaAllocWorldChannelLineNumbers 的「已知残余」),那一轮目录里会短暂缺它。
	// 连续 3 拍(默认 15 秒)都缺才是真的销毁了。回收中(CLOSING)是明确信号,不等,立即换线。
	mysteryMerchantMissingTicksBeforeRelocate = 3
)

// 折扣档(折后价占原价的百分比),与 MysteryMerchantRule 表的三列权重一一对应。
// 顺序即 merchantRule.discountWeights 的下标含义,改这里必须同步 loadMerchantTables。
var mysteryMerchantDiscountPercents = [3]uint32{60, 50, 40}

// 规则表与商品表的取值范围(与 data/schema 里各列注释写的范围是同一份约定;表校验不过时本轮不调度)。
const (
	merchantMinIntervalSeconds int64  = 60
	merchantMaxIntervalSeconds int64  = 24 * 60 * 60
	merchantMinStaySeconds     int64  = 30
	merchantMaxShelfSize       uint32 = 32
	merchantMaxGoodsPrice      uint64 = 1_000_000_000
	merchantMaxLimitCount      uint32 = 99
)

func mysteryMerchantKey(zoneID uint32) string {
	return fmt.Sprintf(MysteryMerchantKeyFmt, zoneID)
}

// luaWriteMysteryMerchantState 原子地写入一份新记录,**版本只进不退**。
//
// KEYS[1] = mystery_merchant:zone:{zone}
// ARGV[1] = 新版本号(十进制),ARGV[2] = MysteryMerchantState 字节,ARGV[3] = TTL 秒,
// ARGV[4] = '1' 表示强制覆盖(跳过版本比较),其它值照常比较
// Return  = 1 已写入 / 0 已有同版本或更新的记录(没写)
//
// 只有领导者调度,正常是单写者;放进 Lua 是防领导权交接窗口里新旧领导者各跑一拍、各掷各的:
// 没有这道比较,后写的会把先写的同一轮记录盖成另一条线、另一批货,而 scene 节点可能已经按前一份
// 发过公告、卖过东西。表里的 v 是坏值(非数字)时按 0 处理,等于允许覆盖。
//
// 强制覆盖只用在两种「现有记录已经不可信」的情形(见 scheduleMysteryMerchant):现有记录解析不了,
// 或它是按另一个出现间隔排出来的(调表 / 改覆盖项之后,旧时间轴上的版本号可能比新时间轴的大,
// 不强制的话新的一轮要等旧键 TTL 过期才写得进去)。
const luaWriteMysteryMerchantState = `
local current = tonumber(redis.call('HGET', KEYS[1], 'v') or '0') or 0
local incoming = tonumber(ARGV[1])
if not incoming then
    return 0
end
if ARGV[4] ~= '1' and current >= incoming then
    return 0
end
redis.call('HSET', KEYS[1], 'v', ARGV[1], 'state', ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 1
`

// merchantRand 是调度用到的全部随机性。生产实现走 math/rand/v2 的全局源(自动播种、并发安全);
// 单测注入确定序列(AGENTS.md §11.2:随机数显式可见)。
type merchantRand interface {
	// Uint64N 返回 [0, n) 的均匀整数;调用方保证 n >= 1。
	Uint64N(n uint64) uint64
}

type globalMerchantRand struct{}

func (globalMerchantRand) Uint64N(n uint64) uint64 { return rand.Uint64N(n) }

// merchantRule 是规则表那一行里调度用得到的部分(已校验、已应用 yaml 覆盖)。
type merchantRule struct {
	intervalSeconds int64
	staySeconds     int64
	shelfSize       int
	// 与 mysteryMerchantDiscountPercents 同下标:掷到 6 / 5 / 4 折的权重。
	discountWeights [3]uint64
}

// merchantGoods 是商品池里一件可上架的商品(weight > 0 且通过校验)。
type merchantGoods struct {
	id     uint32
	weight uint64
}

// merchantSpot 是一个候选站位。
type merchantSpot struct {
	id      uint32
	x, y, z float64
}

// merchantTables 是一拍调度所需的全部配表数据的快照。
// 每拍从全局表管理器现取、不跨拍缓存(data/AGENTS.md「只存 id,不存指针」)。
// scene_manager 目前只在启动时加载配置表,所以改表要重启它才生效;C++ scene 节点的表可以热更,
// 两边表版本不一致时由 scene 节点按自己的表再核一遍(player_mystery_merchant.cpp ResolveGoods)。
type merchantTables struct {
	rule  merchantRule
	goods []merchantGoods
	// scene_config_id -> 该地图上的站位。键集合就是「商人会去哪些地图」。
	spotsByConf map[uint32][]merchantSpot
}

// loadMerchantTables 从配置表读出一拍调度所需的数据并校验。
// 任何一条不满足都返回 error:调用方本拍不调度(已经排好的那一轮不受影响)。
// 宁可这一轮没有商人,也不能按坏表排出「停留比间隔还长」「货架上全是不存在的物品」这样的轮次。
func loadMerchantTables(cfg config.MysteryMerchantConfig) (merchantTables, error) {
	rows := table.MysteryMerchantRuleTableManagerInstance.FindAll()
	if len(rows) == 0 {
		return merchantTables{}, errors.New("MysteryMerchantRule 表为空")
	}
	row := rows[0]
	rule := merchantRule{
		intervalSeconds: int64(row.GetIntervalSeconds()),
		staySeconds:     int64(row.GetStaySeconds()),
		shelfSize:       int(row.GetShelfSize()),
		discountWeights: [3]uint64{
			uint64(row.GetWeightDiscount60()), uint64(row.GetWeightDiscount50()), uint64(row.GetWeightDiscount40()),
		},
	}
	// yaml 覆盖只用于联调 / 冒烟(把一小时缩成两分钟);生产不配,数值以表为准。
	if cfg.IntervalSecondsOverride > 0 {
		rule.intervalSeconds = cfg.IntervalSecondsOverride
	}
	if cfg.StaySecondsOverride > 0 {
		rule.staySeconds = cfg.StaySecondsOverride
	}
	if err := validateMerchantRule(rule); err != nil {
		return merchantTables{}, err
	}

	goods := make([]merchantGoods, 0, len(table.MysteryMerchantGoodsTableManagerInstance.FindAll()))
	for _, g := range table.MysteryMerchantGoodsTableManagerInstance.FindAll() {
		if g.GetWeight() == 0 {
			continue // 0 = 暂不上架,是合法配置
		}
		// 上架的行必须整行合法:scene 节点结算时用同一张表,这里放过一行坏数据,货架上就多一件买不了的东西。
		item, ok := table.ItemTableManagerInstance.FindById(g.GetItemId())
		if !ok {
			return merchantTables{}, fmt.Errorf("MysteryMerchantGoods id=%d 的 item_id=%d 不在 Item 表里", g.GetId(), g.GetItemId())
		}
		// 每份数量不得超过物品的堆叠上限(堆叠上限为 1 的物品必须填 1):一份东西应当正好是一组。
		if g.GetItemCount() > item.GetMaxStackSize() {
			return merchantTables{}, fmt.Errorf("MysteryMerchantGoods id=%d 的 item_count=%d 超过物品 %d 的堆叠上限 %d",
				g.GetId(), g.GetItemCount(), g.GetItemId(), item.GetMaxStackSize())
		}
		if g.GetItemCount() == 0 || g.GetPrice() == 0 || g.GetPrice() > merchantMaxGoodsPrice ||
			g.GetLimitCount() == 0 || g.GetLimitCount() > merchantMaxLimitCount ||
			!currencytype.IsKnown(g.GetCurrencyType()) {
			return merchantTables{}, fmt.Errorf("MysteryMerchantGoods id=%d 取值越界(item_count=%d price=%d limit_count=%d currency_type=%d)",
				g.GetId(), g.GetItemCount(), g.GetPrice(), g.GetLimitCount(), g.GetCurrencyType())
		}
		goods = append(goods, merchantGoods{id: g.GetId(), weight: uint64(g.GetWeight())})
	}
	if len(goods) == 0 {
		return merchantTables{}, errors.New("MysteryMerchantGoods 表里没有可上架的商品(weight 全为 0 或表为空)")
	}

	// 站位只能落在大世界地图上:只有大世界地图才有分线目录,填了别的地图商人永远没有线可落。
	worldConfs := make(map[uint64]struct{})
	for _, confID := range worldConfIds() {
		worldConfs[confID] = struct{}{}
	}
	spotsByConf := make(map[uint32][]merchantSpot)
	for _, s := range table.MysteryMerchantSpotTableManagerInstance.FindAll() {
		if _, isWorld := worldConfs[uint64(s.GetSceneConfigId())]; !isWorld {
			return merchantTables{}, fmt.Errorf("MysteryMerchantSpot id=%d 的 scene_config_id=%d 不是大世界地图(World 表里没有)",
				s.GetId(), s.GetSceneConfigId())
		}
		spotsByConf[s.GetSceneConfigId()] = append(spotsByConf[s.GetSceneConfigId()],
			merchantSpot{id: s.GetId(), x: s.GetX(), y: s.GetY(), z: s.GetZ()})
	}
	if len(spotsByConf) == 0 {
		return merchantTables{}, errors.New("MysteryMerchantSpot 表为空:商人没有可去的地图")
	}
	if cfg.SpotIdOverride != 0 {
		pinned, err := pinMerchantSpot(spotsByConf, cfg.SpotIdOverride)
		if err != nil {
			return merchantTables{}, err
		}
		spotsByConf = pinned
	}
	return merchantTables{rule: rule, goods: goods, spotsByConf: spotsByConf}, nil
}

// pinMerchantSpot 实现 MysteryMerchant.SpotIdOverride:只留下指定的那一个站位(连同它所在的地图)。
// 表里没有这个 id 时返回 error —— 钉一个不存在的站位只可能是配置笔误,不能悄悄退回随机。纯函数。
func pinMerchantSpot(spotsByConf map[uint32][]merchantSpot, spotID uint32) (map[uint32][]merchantSpot, error) {
	for confID, spots := range spotsByConf {
		for _, spot := range spots {
			if spot.id == spotID {
				return map[uint32][]merchantSpot{confID: {spot}}, nil
			}
		}
	}
	return nil, fmt.Errorf("MysteryMerchant.SpotIdOverride=%d 不在 MysteryMerchantSpot 表里", spotID)
}

func validateMerchantRule(rule merchantRule) error {
	if rule.intervalSeconds < merchantMinIntervalSeconds || rule.intervalSeconds > merchantMaxIntervalSeconds {
		return fmt.Errorf("神秘商人间隔 %d 秒不在 [%d, %d]", rule.intervalSeconds, merchantMinIntervalSeconds, merchantMaxIntervalSeconds)
	}
	if rule.staySeconds < merchantMinStaySeconds || rule.staySeconds > rule.intervalSeconds {
		return fmt.Errorf("神秘商人停留 %d 秒不在 [%d, 间隔 %d]", rule.staySeconds, merchantMinStaySeconds, rule.intervalSeconds)
	}
	if rule.shelfSize < 1 || rule.shelfSize > int(merchantMaxShelfSize) {
		return fmt.Errorf("神秘商人货架件数 %d 不在 [1, %d]", rule.shelfSize, merchantMaxShelfSize)
	}
	if rule.discountWeights[0]+rule.discountWeights[1]+rule.discountWeights[2] == 0 {
		return errors.New("神秘商人三档折扣权重之和为 0")
	}
	return nil
}

// merchantRound 是「now 落在哪一轮」的计算结果。
type merchantRound struct {
	id       uint64 // 轮次号 = 这一轮出现时刻的 Unix 秒(与 appearAt 同值)
	appearAt int64  // 这一轮的出现时刻(Unix 秒,含)
	expireAt int64  // 这一轮的离开时刻(Unix 秒,不含)
}

// merchantRoundAt 返回 nowUnix 所在的轮次。纯函数。
// 出现时刻对齐到间隔的整数倍:间隔 3600 = 每个整点(UTC 与东八区的整点重合)。
// nowUnix 为负(时钟坏了)按 0 处理,不让轮次号下溢。
//
// 轮次号直接用出现时刻的 Unix 秒,而不是「第几个间隔」(now / 间隔):后者依赖间隔本身,间隔一调大
// 轮次号就变小,版本号跟着倒退 —— 新的一轮写不进 Redis,scene 节点还会把它当旧记录丢弃。
// 出现时刻只跟墙钟走:间隔不变时逐轮递增;改间隔的那一刻对齐点会变,新一轮的出现时刻可能比旧一轮早
// (回退不到一个新间隔,只发生这一次)—— 那一次由 scheduleMysteryMerchant 强制覆盖旧记录来接上。
// 玩家存档里的限购记录按它记账:轮次号不同就当作新的一轮,不会因为改间隔而把两轮的已购数串在一起。
func merchantRoundAt(nowUnix, intervalSeconds, staySeconds int64) merchantRound {
	if nowUnix < 0 {
		nowUnix = 0
	}
	appearAt := nowUnix / intervalSeconds * intervalSeconds
	return merchantRound{id: uint64(appearAt), appearAt: appearAt, expireAt: appearAt + staySeconds}
}

// merchantChannel 是一条候选分线。
type merchantChannel struct {
	confID    uint32
	sceneID   uint64
	channelNo uint32
}

// eligibleMerchantChannels 从一张图的分线目录里挑出商人可以落脚的线。纯函数。
//
// 只要玩家进得去的线:流畅 / 繁忙。一条都没有时退而求其次用爆满的线(线上的人照样能买,
// 只是别的线的人暂时进不来)—— 比「这一轮没有商人」好。回收中 / 不可用的线永远不选:
// 前者马上要销毁,后者所在节点刚判死。
func eligibleMerchantChannels(directory *scenepb.SceneChannelDirectory) []merchantChannel {
	var open, full []merchantChannel
	for _, ch := range directory.GetChannels() {
		if ch.GetSceneId() == 0 {
			continue
		}
		candidate := merchantChannel{confID: directory.GetSceneConfigId(), sceneID: ch.GetSceneId(), channelNo: ch.GetChannelNo()}
		switch ch.GetState() {
		case scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY:
			open = append(open, candidate)
		case scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL:
			full = append(full, candidate)
		}
	}
	if len(open) > 0 {
		return open
	}
	return full
}

// pickMerchantChannel 在候选线里均匀随机挑一条;有别的选择时避开 avoidSceneID(上一轮 / 刚失效的那条线),
// 让「每轮换一条线」名副其实。candidates 为空时返回 false。纯函数(随机性来自 rnd)。
func pickMerchantChannel(candidates []merchantChannel, avoidSceneID uint64, rnd merchantRand) (merchantChannel, bool) {
	pool := candidates
	if avoidSceneID != 0 && len(candidates) > 1 {
		others := make([]merchantChannel, 0, len(candidates)-1)
		for _, c := range candidates {
			if c.sceneID != avoidSceneID {
				others = append(others, c)
			}
		}
		if len(others) > 0 {
			pool = others
		}
	}
	if len(pool) == 0 {
		return merchantChannel{}, false
	}
	return pool[rnd.Uint64N(uint64(len(pool)))], true
}

// pickWeightedIndex 按权重挑一个下标。调用方保证 weights 之和 >= 1。纯函数。
func pickWeightedIndex(weights []uint64, rnd merchantRand) int {
	var total uint64
	for _, w := range weights {
		total += w
	}
	roll := rnd.Uint64N(total)
	for i, w := range weights {
		if roll < w {
			return i
		}
		roll -= w
	}
	return len(weights) - 1 // 走不到:roll < total
}

// rollMerchantShelf 掷出一轮的货架:按权重**不放回**地抽 shelfSize 件(池子不够时有几件上几件),
// 每件再独立掷一个折扣档。返回值按 goods_id 升序,这就是客户端货架的展示顺序。纯函数。
func rollMerchantShelf(pool []merchantGoods, shelfSize int, discountWeights [3]uint64, rnd merchantRand) []*scenepb.MysteryMerchantShelfEntry {
	remaining := append([]merchantGoods(nil), pool...)
	shelf := make([]*scenepb.MysteryMerchantShelfEntry, 0, shelfSize)
	for len(shelf) < shelfSize && len(remaining) > 0 {
		weights := make([]uint64, len(remaining))
		for i, g := range remaining {
			weights[i] = g.weight
		}
		picked := pickWeightedIndex(weights, rnd)
		discount := mysteryMerchantDiscountPercents[pickWeightedIndex(discountWeights[:], rnd)]
		shelf = append(shelf, &scenepb.MysteryMerchantShelfEntry{GoodsId: remaining[picked].id, DiscountPercent: discount})
		remaining = append(remaining[:picked], remaining[picked+1:]...)
	}
	sort.Slice(shelf, func(i, j int) bool { return shelf[i].GoodsId < shelf[j].GoodsId })
	return shelf
}

// readMysteryMerchantState 读出当前记录。键或 field 不存在返回 (nil, false, nil)。
// 字节解析不了、或记录写的不是这个 zone,返回 (nil, true, nil):坏记录按「没有记录」处理,并告诉调用方
// 下一次写入要强制覆盖(坏记录的 v 可能恰好是合法的本轮版本,不强制就要等到下一轮才盖得掉),
// 不能让一份坏数据把调度卡住。坏在哪由调用方经限流日志打出(这里每拍都会走到,不直接打)。
func readMysteryMerchantState(svcCtx *svc.ServiceContext, zoneID uint32) (state *scenepb.MysteryMerchantState, corrupt bool, err error) {
	values, err := svcCtx.Redis.Hmget(mysteryMerchantKey(zoneID), MysteryMerchantStateField)
	if err != nil {
		return nil, false, fmt.Errorf("读神秘商人记录失败: %w", err)
	}
	if len(values) == 0 || values[0] == "" {
		return nil, false, nil
	}
	parsed := &scenepb.MysteryMerchantState{}
	if err := proto.Unmarshal([]byte(values[0]), parsed); err != nil || parsed.GetZoneId() != zoneID {
		return nil, true, nil
	}
	return parsed, false, nil
}

// writeMysteryMerchantState 把一份记录写进 Redis。返回 false = 已有同版本或更新的记录(别的副本先写了)。
// force = true 时跳过版本比较,只用于现有记录已经不可信的情形(见 luaWriteMysteryMerchantState)。
func writeMysteryMerchantState(svcCtx *svc.ServiceContext, state *scenepb.MysteryMerchantState, force bool) (bool, error) {
	payload, err := proto.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("序列化神秘商人记录失败: %w", err)
	}
	version := state.GetRoundId()*mysteryMerchantVersionRoundFactor + uint64(state.GetRevision())
	ttlSeconds := int64(state.GetIntervalSeconds()) * mysteryMerchantStateTTLRounds
	// 值是 protobuf 字节,直接当 Go string 传(Redis 字符串二进制安全;分线目录同款)。
	forceArg := "0"
	if force {
		forceArg = "1"
	}
	raw, err := svcCtx.Redis.Eval(luaWriteMysteryMerchantState, []string{mysteryMerchantKey(state.GetZoneId())},
		strconv.FormatUint(version, 10), string(payload), strconv.FormatInt(ttlSeconds, 10), forceArg)
	if err != nil {
		return false, fmt.Errorf("写神秘商人记录失败: %w", err)
	}
	written, ok := raw.(int64)
	if !ok {
		return false, fmt.Errorf("神秘商人写入脚本返回值类型非法: %T", raw)
	}
	return written == 1, nil
}

// readMerchantChannelDirectory 读一张图的分线目录。目录键不存在(刚开服 / 领导者刚切换)返回 (nil, nil)。
func readMerchantChannelDirectory(svcCtx *svc.ServiceContext, zoneID, confID uint32) (*scenepb.SceneChannelDirectory, error) {
	raw, err := svcCtx.Redis.Get(worldChannelDirectoryKey(zoneID, uint64(confID)))
	if err != nil {
		return nil, fmt.Errorf("读地图 %d 的分线目录失败: %w", confID, err)
	}
	if raw == "" {
		return nil, nil
	}
	directory := &scenepb.SceneChannelDirectory{}
	if err := proto.Unmarshal([]byte(raw), directory); err != nil {
		return nil, fmt.Errorf("地图 %d 的分线目录解析失败: %w", confID, err)
	}
	return directory, nil
}

// collectMerchantChannels 汇总全部候选地图上此刻可落脚的线。按 (地图, 线号) 排序,
// 让「随机数 -> 选中哪条线」只取决于目录内容,不取决于 map 遍历顺序(单测要能断言)。
func collectMerchantChannels(svcCtx *svc.ServiceContext, zoneID uint32, spotsByConf map[uint32][]merchantSpot) ([]merchantChannel, error) {
	confIDs := make([]uint32, 0, len(spotsByConf))
	for confID := range spotsByConf {
		confIDs = append(confIDs, confID)
	}
	sort.Slice(confIDs, func(i, j int) bool { return confIDs[i] < confIDs[j] })

	var channels []merchantChannel
	for _, confID := range confIDs {
		directory, err := readMerchantChannelDirectory(svcCtx, zoneID, confID)
		if err != nil {
			return nil, err
		}
		if directory == nil {
			continue
		}
		// 目录写的不是这张图(键与内容对不上,只可能是数据被写坏):整份不用。
		// 候选线带的地图 id 取自目录内容,随后要用它去 spotsByConf 里找站位,对不上会取到空表。
		if directory.GetSceneConfigId() != confID {
			return nil, fmt.Errorf("地图 %d 的分线目录里写的是地图 %d", confID, directory.GetSceneConfigId())
		}
		channels = append(channels, eligibleMerchantChannels(directory)...)
	}
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].confID != channels[j].confID {
			return channels[i].confID < channels[j].confID
		}
		return channels[i].channelNo < channels[j].channelNo
	})
	return channels, nil
}

// merchantMissingTicks 记「商人所在的线连续几拍不在分线目录里」,键是 zone。
// 只有领导者的调度循环读写;加锁是因为单测会并发驱动,且循环与测试辅助函数不在同一个 goroutine。
// 进程内状态:领导者切换后从 0 重数,最多让换线晚几拍,不影响正确性。
var (
	merchantMissingMu    sync.Mutex
	merchantMissingTicks = map[uint32]int{}
)

func bumpMerchantMissing(zoneID uint32) int {
	merchantMissingMu.Lock()
	defer merchantMissingMu.Unlock()
	merchantMissingTicks[zoneID]++
	return merchantMissingTicks[zoneID]
}

func resetMerchantMissing(zoneID uint32) {
	merchantMissingMu.Lock()
	delete(merchantMissingTicks, zoneID)
	merchantMissingMu.Unlock()
}

// scheduleMysteryMerchant 对一个 zone 跑一拍调度,返回本拍的 outcome(metrics.MerchantSchedule*)。
// outcome 为 error / invalid_table 时第二个返回值非 nil。
//
// 一拍里只可能发生三件事之一:
//  1. 本轮已经排好、线还在 → 什么都不写(kept);
//  2. 本轮已经排好、但线被回收或销毁了 → 同一轮内换一条线,货架与限购不变(relocated);
//  3. 进入了新的一轮且还在停留期内 → 挑线、挑站位、掷货架,写入新记录(created)。
//
// now 与 rnd 由调用方传入:这是本函数仅有的两处不确定性,显式传入才能写确定的单测。
func scheduleMysteryMerchant(svcCtx *svc.ServiceContext, zoneID uint32, now time.Time, rnd merchantRand, tables merchantTables) (string, error) {
	round := merchantRoundAt(now.Unix(), tables.rule.intervalSeconds, tables.rule.staySeconds)

	current, corrupt, err := readMysteryMerchantState(svcCtx, zoneID)
	if err != nil {
		return metrics.MerchantScheduleError, err
	}
	// 现有记录不可信的两种情形:解析不了,或它是按另一个出现间隔排的(调表 / 改覆盖项之后)。
	// 都按「没有记录」重新排,并在写入时强制覆盖 —— 旧时间轴上的版本号可能比新的大,不强制就写不进去。
	// 两个副本在领导权交接窗口里同时走到这里会各写一份(强制覆盖不比较版本),后写的赢;
	// 这只可能发生在「改间隔」与「换领导者」撞在同一拍,下一拍就收敛(记录已是新间隔,回到正常路径)。
	force := corrupt
	if corrupt {
		logMerchantProblem(zoneID, now, "existing record failed to parse or belongs to another zone; treating it as absent and overwriting")
	}
	// 这个分支在「改了间隔」到「新间隔下的第一轮开始」之间每一拍都会走到(那段时间没有东西可写),
	// 所以这里只记下旧间隔,等真正覆盖写入时再打一条日志。
	var replacedIntervalSeconds uint32
	if current != nil && int64(current.GetIntervalSeconds()) != tables.rule.intervalSeconds {
		replacedIntervalSeconds = current.GetIntervalSeconds()
		current = nil
		force = true
	}
	if current != nil && current.GetRoundId() == round.id {
		return keepOrRelocateMerchant(svcCtx, zoneID, now, rnd, tables, current)
	}
	resetMerchantMissing(zoneID)

	// 本轮的停留期已经过了(调度在整点之后很久才恢复):不补排,等下一个整点。
	if now.Unix() >= round.expireAt {
		return metrics.MerchantScheduleIdle, nil
	}

	channels, err := collectMerchantChannels(svcCtx, zoneID, tables.spotsByConf)
	if err != nil {
		return metrics.MerchantScheduleError, err
	}
	// 上一轮的线尽量避开。记录是更早的轮次或压根没有时 GetSceneId 为 0 = 不避让。
	channel, ok := pickMerchantChannel(channels, current.GetSceneId(), rnd)
	if !ok {
		// 没有任何可进的线(目录还没发布 / 线全在回收或不可用):本拍不写,下一拍再试。
		return metrics.MerchantScheduleNoChannel, nil
	}
	shelf := rollMerchantShelf(tables.goods, tables.rule.shelfSize, tables.rule.discountWeights, rnd)

	state := &scenepb.MysteryMerchantState{
		ZoneId:          zoneID,
		RoundId:         round.id,
		Revision:        1,
		AppearAtMs:      uint64(round.appearAt) * 1000,
		ExpireAtMs:      uint64(round.expireAt) * 1000,
		IntervalSeconds: uint32(tables.rule.intervalSeconds),
		Shelf:           shelf,
		UpdatedAtMs:     uint64(now.UnixMilli()),
	}
	placeMerchant(state, channel, tables.spotsByConf[channel.confID], rnd)

	written, err := writeMysteryMerchantState(svcCtx, state, force)
	if err != nil {
		return metrics.MerchantScheduleError, err
	}
	if !written {
		return metrics.MerchantScheduleLostRace, nil
	}
	if replacedIntervalSeconds != 0 {
		logx.Infof("[MysteryMerchant] zone=%d: appearance interval changed from %ds to %ds; the record scheduled on the old interval was overwritten",
			zoneID, replacedIntervalSeconds, tables.rule.intervalSeconds)
	}
	logx.Infof("[MysteryMerchant] zone=%d round=%d: appeared on map %d channel %d (scene=%d, spot=%d), shelf=%d goods, stays until %s",
		zoneID, state.GetRoundId(), state.GetSceneConfigId(), state.GetChannelNo(), state.GetSceneId(), state.GetSpotId(),
		len(shelf), time.Unix(round.expireAt, 0).Format(time.RFC3339))
	return metrics.MerchantScheduleCreated, nil
}

// placeMerchant 把「落在哪条线、站在哪」写进记录。spots 是该线所在地图的站位,调用方保证非空
// (候选线只来自 spotsByConf 的键)。
func placeMerchant(state *scenepb.MysteryMerchantState, channel merchantChannel, spots []merchantSpot, rnd merchantRand) {
	spot := spots[rnd.Uint64N(uint64(len(spots)))]
	state.SceneConfigId = channel.confID
	state.SceneId = channel.sceneID
	state.ChannelNo = channel.channelNo
	state.SpotId = spot.id
	state.Position = &componentpb.Vector3{X: spot.x, Y: spot.y, Z: spot.z}
}

// keepOrRelocateMerchant 处理「本轮已经排好」的情形:核对商人所在的线还在不在,不在了就同一轮内换线。
//
// 换线只动落点(地图 / 线 / 站位)与 revision,**轮次号与货架原样保留**:玩家的限购记录按轮次号记,
// 货架一变,已经买过的人就对不上账了。
func keepOrRelocateMerchant(svcCtx *svc.ServiceContext, zoneID uint32, now time.Time, rnd merchantRand, tables merchantTables, current *scenepb.MysteryMerchantState) (string, error) {
	// 已经过了离开时刻:商人反正不在了,线没了也不必换。
	if uint64(now.UnixMilli()) >= current.GetExpireAtMs() {
		resetMerchantMissing(zoneID)
		return metrics.MerchantScheduleIdle, nil
	}

	directory, err := readMerchantChannelDirectory(svcCtx, zoneID, current.GetSceneConfigId())
	if err != nil {
		return metrics.MerchantScheduleError, err
	}
	if directory == nil {
		// 目录暂缺(领导者刚切换,最多缺几十秒):无从判断线在不在,不动。
		return metrics.MerchantScheduleKept, nil
	}

	channelState, listed := merchantChannelState(directory, current.GetSceneId())
	switch {
	case listed && channelState != scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING:
		// 流畅 / 繁忙 / 爆满都照常营业。不可用(所在节点刚判死)也先不动:
		// 线会在别的节点按**同一个 scene_id** 重建,记录不用改。
		resetMerchantMissing(zoneID)
		return metrics.MerchantScheduleKept, nil
	case !listed:
		// 目录里压根没有这条线:可能是真的销毁了,也可能只是排空起步的那个瞬间。连续几拍都没有才算数。
		if bumpMerchantMissing(zoneID) < mysteryMerchantMissingTicksBeforeRelocate {
			return metrics.MerchantScheduleKept, nil
		}
	}
	// 走到这里只有两种情形:这条线明确在回收(CLOSING),或者已经连续多拍不在目录里。

	if current.GetRevision() >= mysteryMerchantMaxRevision {
		return metrics.MerchantScheduleError, fmt.Errorf("round=%d 已换线 %d 次,达到上限,本轮不再换线", current.GetRoundId(), current.GetRevision())
	}
	channels, err := collectMerchantChannels(svcCtx, zoneID, tables.spotsByConf)
	if err != nil {
		return metrics.MerchantScheduleError, err
	}
	// 新落点不能还是那条失效的线:候选里先把它剔掉(它若是 CLOSING 本来也不在候选里)。
	alive := make([]merchantChannel, 0, len(channels))
	for _, c := range channels {
		if c.sceneID != current.GetSceneId() {
			alive = append(alive, c)
		}
	}
	channel, ok := pickMerchantChannel(alive, 0, rnd)
	if !ok {
		return metrics.MerchantScheduleNoChannel, nil
	}

	relocated := proto.Clone(current).(*scenepb.MysteryMerchantState)
	relocated.Revision = current.GetRevision() + 1
	relocated.UpdatedAtMs = uint64(now.UnixMilli())
	placeMerchant(relocated, channel, tables.spotsByConf[channel.confID], rnd)

	written, err := writeMysteryMerchantState(svcCtx, relocated, false)
	if err != nil {
		return metrics.MerchantScheduleError, err
	}
	if !written {
		return metrics.MerchantScheduleLostRace, nil
	}
	resetMerchantMissing(zoneID)
	logx.Infof("[MysteryMerchant] zone=%d round=%d: previous channel (scene=%d) was reclaimed or destroyed; relocated to map %d channel %d (scene=%d, spot=%d), placement #%d",
		zoneID, relocated.GetRoundId(), current.GetSceneId(), relocated.GetSceneConfigId(), relocated.GetChannelNo(),
		relocated.GetSceneId(), relocated.GetSpotId(), relocated.GetRevision())
	return metrics.MerchantScheduleRelocated, nil
}

// merchantChannelState 在目录里找一条线,返回它的状态;第二个返回值 false = 目录里没有这条线。
func merchantChannelState(directory *scenepb.SceneChannelDirectory, sceneID uint64) (scenepb.SceneChannelState, bool) {
	for _, ch := range directory.GetChannels() {
		if ch.GetSceneId() == sceneID {
			return ch.GetState(), true
		}
	}
	return scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNKNOWN, false
}

// ScheduleMysteryMerchantForZone 对一个 zone 跑一拍调度并记指标。导出是为了让单测与循环共用同一个入口。
//
// **只有领导者执行**,闸门放在这里而不是只放在循环里:记录是整份覆盖,两个副本各掷各的会让同一轮
// 出现两条线。写入脚本的版本比较是第二道保险,不是可以不设闸门的理由。
func ScheduleMysteryMerchantForZone(svcCtx *svc.ServiceContext, zoneID uint32, now time.Time) {
	if !isLeader() {
		return
	}
	tables, err := loadMerchantTables(svcCtx.Config.MysteryMerchant)
	if err != nil {
		metrics.ObserveMysteryMerchantSchedule(zoneID, metrics.MerchantScheduleInvalidTable)
		logMerchantProblem(zoneID, now, fmt.Sprintf("table validation failed; skipping this tick: %v", err))
		return
	}
	outcome, err := scheduleMysteryMerchant(svcCtx, zoneID, now, globalMerchantRand{}, tables)
	metrics.ObserveMysteryMerchantSchedule(zoneID, outcome)
	// 这三种结果在外面看都只是「商人不来」,只有指标在涨;各留一条限流日志,说清是哪一种。
	switch {
	case err != nil:
		logMerchantProblem(zoneID, now, fmt.Sprintf("scheduling tick failed: %v", err))
	case outcome == metrics.MerchantScheduleNoChannel:
		logMerchantProblem(zoneID, now, "no channel to land on: the candidate maps' channel directory is not published yet (ChannelSwitch.DirectoryRefreshSeconds < 0?), "+
			"or every channel is closing / unavailable; retrying next tick")
	case outcome == metrics.MerchantScheduleLostRace:
		logMerchantProblem(zoneID, now, "Redis already holds a record of the same or newer version; yielding this tick (normal during a leadership handover; "+
			"if it persists, the record version is ahead of the current time: check the replicas' system clocks)")
	}
}

// 同一个问题每拍(默认 5 秒)都会重现一次;日志按「同一条内容至少隔这么久才再打」限流,
// 指标照常每拍计数(告警看指标,排障看日志)。
const merchantProblemLogInterval = time.Minute

var (
	merchantProblemMu   sync.Mutex
	merchantProblemLast = map[uint32]merchantProblemRecord{}
)

type merchantProblemRecord struct {
	message string
	at      time.Time
}

func logMerchantProblem(zoneID uint32, now time.Time, message string) {
	merchantProblemMu.Lock()
	last, seen := merchantProblemLast[zoneID]
	suppress := seen && last.message == message && now.Sub(last.at) < merchantProblemLogInterval
	if !suppress {
		merchantProblemLast[zoneID] = merchantProblemRecord{message: message, at: now}
	}
	merchantProblemMu.Unlock()
	if !suppress {
		logx.Errorf("[MysteryMerchant] zone=%d: %s", zoneID, message)
	}
}

// StartMysteryMerchantScheduler 起神秘商人的周期调度循环。
//
// 每拍(默认 5 秒)只做一次读 + 偶尔一次写,开销可以忽略;拍得密是为了整点一到商人就出现,
// 以及线被回收时尽快换线。MysteryMerchant.Disabled 时不起循环(已写下的记录到点自然失效)。
func StartMysteryMerchantScheduler(ctx context.Context, svcCtx *svc.ServiceContext) {
	cfg := svcCtx.Config.MysteryMerchant
	if cfg.Disabled {
		logx.Info("[MysteryMerchant] Not started: MysteryMerchant.Disabled = true")
		return
	}
	tickSeconds := cfg.EffectiveTickSeconds()
	logx.Infof("[MysteryMerchant] Started: tick=%ds interval_override=%ds stay_override=%ds spot_override=%d (0 = use the table)",
		tickSeconds, cfg.IntervalSecondsOverride, cfg.StaySecondsOverride, cfg.SpotIdOverride)

	// safego.Loop:单拍 panic 只丢那一拍。调度停摆在外面看只是「商人不来了」,
	// 没人会联想到 goroutine,所以必须带点位名进 safego_panic_total。
	safego.Loop(ctx, SafePointMysteryMerchant, time.Duration(tickSeconds)*time.Second,
		func(ctx context.Context) {
			// 先在这里挡一次只是为了让跟随者每拍不白拷一份 zone 列表;真正的闸门在 ScheduleMysteryMerchantForZone 里。
			if !isLeader() {
				return
			}
			now := time.Now()
			for _, zoneID := range GetActiveZones() {
				if ctx.Err() != nil {
					return
				}
				ScheduleMysteryMerchantForZone(svcCtx, zoneID, now)
			}
		})
}
