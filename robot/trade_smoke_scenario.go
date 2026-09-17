package main

// trade-smoke 场景:三个机器人经 gate → client_rpc_router 对 go/trade(聚宝斋 P1)做端到端冒烟
// (docs/design/jubaozhai-market.md §9;实现规格工作包 D):
//
//	0. A(卖家)/ B(同区买家)登 zone_a,C(卖家兼买家)登 zone_b(cross_zone=false 时也登 zone_a);
//	   跨区时断言 C 的 gate ≠ A 的 gate;
//	1. 经 gRPC 直连 trade 的 TradeAdmin.SeedListing 造三条武器类商品(标题带本轮 nonce):
//	   A 立即寄售、C 立即寄售、A 公示中;断言服务端写入的 market_zone 是卖家归属区;
//	2. 反向断言:A 经 gate 发 TradeAdmin.SeedListing 消息号 → 必须超时(gate 丢弃非客户端消息号);
//	3. 市场范围(按 expect_scope):
//	   zone   —— B 看到 A 的、看不到 C 的;B 传 zone_filter=zone_b 仍看不到 C 的;C 只看到 C 的;
//	   global —— B 两条都看到;zone_filter=zone_b 只剩 C 的;
//	   每次浏览都断言 market_scope 与 expect_scope 一致、server_now_ms ≠ 0、页数自洽;
//	4. 公示:B 在公示列表看到 A 的公示商品(phase=PUBLIC_NOTICE),寄售列表里没有它;
//	5. 分页:page_size=50 被钳到 20;page=9999 被钳到末页;
//	6. 拍卖分区 → TradeFeatureDisabled;
//	7. 详情:B 取 A 的 → 受理且 is_mine=false;不存在 → TradeListingNotFound;
//	   zone scope 下 C 取 A 的 → TradeListingNotFound;A 取自己的 → is_mine=true;
//	8. 收藏:B 收藏 A 的 → true,favorites_only 浏览看到;取消 → false,再浏览看不到;
//	9. 货架:A 的货架含两条自己的种子(is_mine=true);B 的货架不含。
//
// 一次运行只能验一种 scope(Market.Scope 是 trade 进程级配置):两种都验 = 改 trade.yaml 重启 trade,各跑一次。
// P1 没有下架流程,种子商品会逐轮累积;所有断言按本轮 nonce / listing_id 找自己的商品,不断言总数。
//
// 结果约定(供外层脚本消费):
//	全过 → 日志一行 `TRADE_SMOKE_OK scope=… listing_a=… listing_c=… zone_a=… zone_b=…`,退出码 0;
//	任一步失败 → `TRADE_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// 依赖说明:本文件是 robot 第一次直接 import google.golang.org/grpc(之前只经 go/proto 间接依赖),
// 且需要 vendor 里有 proto/trade 与含 TradeError 的 shared/generated/pb/table ——
// 生成 proto / 导表之后须在 robot/ 下 `go mod tidy && go mod vendor` 再以 -mod=vendor 编译。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	tradepb "proto/trade"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。
// 94xx 与 battle-smoke(9001/9002)、跨 zone 匹配(9003/9004)、chat-smoke(9005/9006)、
// 属性(9101)、宠物(9102)、guild-smoke(9201-9203)、team-smoke(9301-9304,docs/design/team-system.md §I.5 预留)错开:
// 冒烟可能被脚本先后甚至并行跑,同账号会互相顶成 ReplaceLogin;而且归属区在首次建角时登记,
// team-smoke 要求 9303 在 zone_a 建角,与本场景 C 必须在 zone_b 建角直接矛盾,所以不能共用 93xx。
// (实现规格 §9 原写 9301-9303,因上述冲突改用 9401-9403,号段分配待主会话定稿。)
// 必须是**首次在各自 zone 建角**的账号:A/B 的归属区要是 zone_a、C 的要是 zone_b,
// 第 1 步 market_zone 断言与第 3 步范围断言才成立。
const (
	tradeSmokeAccountA = "robot_9401" // 卖家
	tradeSmokeAccountB = "robot_9402" // 同区买家
	tradeSmokeAccountC = "robot_9403" // 别区卖家兼买家(cross_zone=false 时也登 zone_a,跳过跨区断言)
)

// 单次聚宝斋 RPC 的等待预算:大于路由服 ForwardTimeoutMs(5s)与 trade zrpc Timeout(4000ms),
// 先看到路由服的信封拒绝而不是本地超时。第 2 步反向断言也按这个预算判"超时"。
const tradeSmokeRpcTimeout = 10 * time.Second

// 等进场的预算,与 prepareBehaviorClient 同值。
const tradeSmokeSceneReadyTimeout = 15 * time.Second

// 直连 TradeAdmin.SeedListing 的单次超时(规格:5s)。SeedListing 内部查 home_zone(1500ms)+ 发号 + 插入。
const tradeSmokeAdminTimeout = 5 * time.Second

// trade.yaml Market.MaxPageSize。第 5 步断言超上限的 page_size 被钳到它;改了 trade.yaml 这里要同步。
const tradeSmokeServerMaxPageSize uint32 = 20

// 第 5 步用的超限页长与超限页码。
const (
	tradeSmokeOversizePageSize uint32 = 50
	tradeSmokeBeyondLastPage   uint32 = 9999
)

// 种子商品的固定参数:类目武器(3)、子类 1("枪",客户端 JubaozhaiCatalog 武器子类表第 1 个)。
// 取值都在 trade 的校验范围内(规格 §6 SeedListing 校验:icon_key 只允许 ^[a-z0-9_]+$)。
const tradeSmokeCategory = tradepb.ListingCategory_LISTING_CATEGORY_WEAPON

const (
	tradeSmokeSubcategory      uint32 = 1
	tradeSmokeLevel            uint32 = 60
	tradeSmokePriceFen         uint64 = 123456
	tradeSmokeIconKey          string = "smoke_weapon"
	tradeSmokeSaleDurationMs   uint64 = uint64(time.Hour / time.Millisecond)
	tradeSmokeNoticeDurationMs uint64 = uint64(time.Hour / time.Millisecond)
)

// 取详情用的"不存在"商品编号:号段发号从小往上走,碰不到 int64 上限;
// 刻意不用 uint64 上限,避免 SQL 驱动对最高位为 1 的 uint64 参数的兼容差异把"不存在"测成"存储故障"。
const tradeSmokeMissingListingID uint64 = math.MaxInt64

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipTradeListingNotFound    = uint32(tiptable.TradeError_kTradeListingNotFound)
	tipTradeHomeZoneUnknown    = uint32(tiptable.TradeError_kTradeHomeZoneUnknown)
	tipTradeFeatureDisabled    = uint32(tiptable.TradeError_kTradeFeatureDisabled)
	tipTradeInvalidParameter   = uint32(tiptable.CommonError_kInvalidParameter)
	tipTradeServiceUnavailable = uint32(tiptable.CommonError_kServiceUnavailable)
)

var errTradeSmokeTimeout = errors.New("等回包超时")

// tradeResponse 是所有聚宝斋客户端响应的共同形状:业务拒绝码在响应体 error_message 里。
type tradeResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// tradeSmokeBot 是一个已登录进场的机器人会话。
type tradeSmokeBot struct {
	account string
	zone    uint32
	gate    string
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// 相邻请求的最小间隔(trade_smoke.request_interval_ms)。聚宝斋消息号不在 MessageLimiter 表里时
	// 吃 gate 默认档 3 次 / 窗口,按秒粒度淘汰,1.1s 间隔下缓冲区至多 2 条(推导见 chat_smoke_scenario.go)。
	requestSpacing time.Duration

	// RecvLoop goroutine 写、场景主流程读,mu 保护。
	mu       sync.Mutex
	replies  map[uint32]*base.MessageContent // trade 消息号 → 本次请求发出后收到的第一个回包
	gateTips []uint32                        // 本次请求期间收到的 SendTipToClient

	lastRequestAt time.Time // 仅主流程读写
}

// RunTradeSmoke 是 main.go `mode: trade-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunTradeSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.TradeSmoke

	var adminConn *grpc.ClientConn
	var bots []*tradeSmokeBot
	cleanup := func() {
		if adminConn != nil {
			_ = adminConn.Close()
		}
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			sendDisconnectBestEffort(bot.gc)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("TRADE_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	wantScope, ok := tradeSmokeScopeOf(sc.ExpectScope)
	if !ok {
		// config.validate 已拦;这里兜底,避免拿 UNSPECIFIED 去断言
		fail("config", "trade_smoke.expect_scope=%q,期望 zone 或 global", sc.ExpectScope)
	}
	zoneScope := wantScope == tradepb.MarketScope_MARKET_SCOPE_ZONE
	spacing := time.Duration(sc.RequestIntervalMs) * time.Millisecond
	zoneC := sc.ZoneA
	if sc.CrossZone {
		zoneC = sc.ZoneB
	}

	// ---- 步骤 0:登录(并发) ----
	type loginSpec struct {
		account string
		zone    uint32
	}
	specs := []loginSpec{{tradeSmokeAccountA, sc.ZoneA}, {tradeSmokeAccountB, sc.ZoneA}, {tradeSmokeAccountC, zoneC}}
	loggedIn := make([]*tradeSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, spec loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = spec.zone
			loggedIn[idx], loginErrs[idx] = tradeSmokeLogin(&zoneCfg, spec.account, spacing, stats)
		}(i, spec)
	}
	wg.Wait()
	// 先收齐已登录的会话再判错:否则某人失败时其他人的会话不会被 cleanup 收尾。
	for _, bot := range loggedIn {
		if bot != nil {
			bots = append(bots, bot)
		}
	}
	for i, err := range loginErrs {
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", specs[i].account, specs[i].zone, err)
		}
	}
	a, b, c := loggedIn[0], loggedIn[1], loggedIn[2]
	if sc.CrossZone && c.gate == a.gate {
		// 本地各 zone 的 gate 端口不同;地址相同说明 C 其实进了 zone_a,后面"别区不可见"的结论就不成立。
		fail("zone-placement", "C 与 A 分到同一个 gate %s(zone_a=%d zone_b=%d),不是跨 zone 运行", a.gate, sc.ZoneA, sc.ZoneB)
	}
	zap.L().Info("[trade-smoke] robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId), zap.Uint64("c", c.gc.PlayerId),
		zap.Bool("cross_zone", sc.CrossZone), zap.String("expect_scope", sc.ExpectScope))

	// ---- 步骤 1:经 gRPC 直连 TradeAdmin 造种子商品 ----
	// 不带任何会话 metadata:trade 的会话拦截器把"无会话"视为内部调用放行,SeedListing 自己再查 Mode ∈ {dev,test}。
	var err error
	adminConn, err = grpc.NewClient(sc.AdminAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("admin-dial", "admin_addr=%s: %v", sc.AdminAddr, err)
	}
	admin := tradepb.NewTradeAdminClient(adminConn)

	nonce := tradeSmokeNonce()
	seedA := tradeSmokeSeedRequest(a.gc.PlayerId, nonce+"-A", 0)
	seedC := tradeSmokeSeedRequest(c.gc.PlayerId, nonce+"-C", 0)
	seedNotice := tradeSmokeSeedRequest(a.gc.PlayerId, nonce+"-AN", tradeSmokeNoticeDurationMs)
	gateTitle := nonce + "-GATE" // 第 2 步经 gate 发的种子标题;任何浏览结果里都不许出现

	seededA, err := tradeSmokeSeed(admin, seedA)
	if err != nil {
		fail("seed-a", "%v", err)
	}
	if seededA.GetMarketZone() != sc.ZoneA {
		fail("seed-a-zone", "A 的商品 market_zone=%d,期望 A 的归属区 %d(market_zone 必须来自卖家 home_zone)",
			seededA.GetMarketZone(), sc.ZoneA)
	}
	seededC, err := tradeSmokeSeed(admin, seedC)
	if err != nil {
		fail("seed-c", "%v", err)
	}
	// cross_zone=false 时 C 的归属区取决于账号历史(可能曾在 zone_b 建角),只要求非 0,不断言具体值。
	if sc.CrossZone && seededC.GetMarketZone() != sc.ZoneB {
		fail("seed-c-zone", "C 的商品 market_zone=%d,期望 C 的归属区 zone_b=%d(market_zone 必须来自卖家 home_zone)",
			seededC.GetMarketZone(), sc.ZoneB)
	}
	if !sc.CrossZone && seededC.GetMarketZone() == 0 {
		fail("seed-c-zone", "C 的商品 market_zone=0(cross_zone=false 时不断言具体值,但受理的商品必须带非 0 分区)")
	}
	seededNotice, err := tradeSmokeSeed(admin, seedNotice)
	if err != nil {
		fail("seed-notice", "%v", err)
	}
	if seededNotice.GetMarketZone() != sc.ZoneA {
		fail("seed-notice-zone", "A 的公示商品 market_zone=%d,期望 %d", seededNotice.GetMarketZone(), sc.ZoneA)
	}
	listingA, listingC, listingNotice := seededA.GetListingId(), seededC.GetListingId(), seededNotice.GetListingId()
	zap.L().Info("[trade-smoke] step 1: seeded listings via TradeAdmin",
		zap.String("nonce", nonce), zap.Uint64("listing_a", listingA), zap.Uint64("listing_c", listingC),
		zap.Uint64("listing_notice", listingNotice), zap.Uint32("market_zone_c", seededC.GetMarketZone()))

	// ---- 步骤 2:TradeAdmin 对客户端不可达 ----
	// trade_admin.proto 不标 OptionIsClientProtocolService → gate 的 IsClientMessageId 不收该消息号,
	// 直接丢弃且不回包(cpp/nodes/gate/handler/rpc/client_message_processor.cpp),并计一次非法包
	// (默认阈值 50 才踢线),所以只发一次。信封拒绝说明白名单收了这个号、请求进了路由服,同样不合格。
	envelopeTip, err := a.call(game.TradeAdminSeedListingMessageId,
		tradeSmokeSeedRequest(a.gc.PlayerId, gateTitle, 0), &tradepb.SeedListingResponse{})
	switch {
	case errors.Is(err, errTradeSmokeTimeout):
		// 期望:gate 丢弃
	case err != nil:
		fail("admin-via-gate", "%v", err)
	case envelopeTip != 0:
		fail("admin-via-gate", "期望 gate 丢弃(超时),实得信封 tip=%d:gate 客户端白名单收了 TradeAdmin 消息号", envelopeTip)
	default:
		fail("admin-via-gate", "客户端经 gate 调 TradeAdmin.SeedListing 拿到了业务回包")
	}
	zap.L().Info("[trade-smoke] step 2: TradeAdmin.SeedListing via gate timed out (dropped)",
		zap.String("method", tradepb.TradeAdmin_SeedListing_FullMethodName))

	onSale := func(zoneFilter uint32, favoritesOnly bool) *tradepb.BrowseListingsRequest {
		return &tradepb.BrowseListingsRequest{
			Tab:           tradepb.ListingTab_LISTING_TAB_ON_SALE,
			Section:       tradepb.ListingSection_LISTING_SECTION_CONSIGNMENT,
			Category:      tradeSmokeCategory,
			Subcategory:   tradeSmokeSubcategory,
			Search:        nonce,
			Sort:          tradepb.ListingSort_LISTING_SORT_DEFAULT,
			Page:          1,
			ZoneFilter:    zoneFilter,
			FavoritesOnly: favoritesOnly,
		}
	}

	// ---- 步骤 3:市场范围 ----
	bOnSale, err := b.browse(onSale(0, false), wantScope)
	if err != nil {
		fail("scope-b", "%v", err)
	}
	if s := tradeSmokeFind(bOnSale.GetListings(), listingA); s == nil {
		fail("scope-b-sees-a", "B 浏览(scope=%s)没看到同区 A 的商品 %d", sc.ExpectScope, listingA)
	} else if err := tradeSmokeCheckSummary(s, seedA, tradepb.ListingPhase_LISTING_PHASE_ON_SALE, false, sc.ZoneA); err != nil {
		fail("scope-b-sees-a", "%v", err)
	}
	if tradeSmokeHasTitle(bOnSale.GetListings(), gateTitle) {
		fail("admin-via-gate-verify", "浏览结果里出现了经 gate 发出的种子 %q:TradeAdmin 对客户端可达", gateTitle)
	}
	if zoneScope {
		if sc.CrossZone {
			if tradeSmokeFind(bOnSale.GetListings(), listingC) != nil {
				fail("scope-zone-b-hides-c", "zone scope 下 B(zone %d)看到了 zone %d 的商品 %d", sc.ZoneA, sc.ZoneB, listingC)
			}
			bFiltered, err := b.browse(onSale(sc.ZoneB, false), wantScope)
			if err != nil {
				fail("scope-zone-filter", "%v", err)
			}
			if tradeSmokeFind(bFiltered.GetListings(), listingC) != nil {
				fail("scope-zone-filter", "zone scope 下 B 传 zone_filter=%d 看到了别区商品 %d:客户端参数扩大了市场范围", sc.ZoneB, listingC)
			}
			if tradeSmokeFind(bFiltered.GetListings(), listingA) == nil {
				fail("scope-zone-filter", "zone scope 下 zone_filter 必须被忽略,B 传 zone_filter=%d 后却看不到本区商品 %d", sc.ZoneB, listingA)
			}
			cOnSale, err := c.browse(onSale(0, false), wantScope)
			if err != nil {
				fail("scope-zone-c", "%v", err)
			}
			if tradeSmokeFind(cOnSale.GetListings(), listingC) == nil {
				fail("scope-zone-c", "C 没看到自己区的商品 %d", listingC)
			}
			if tradeSmokeFind(cOnSale.GetListings(), listingA) != nil {
				fail("scope-zone-c", "zone scope 下 C(zone %d)看到了 zone %d 的商品 %d", sc.ZoneB, sc.ZoneA, listingA)
			}
		}
	} else {
		if tradeSmokeFind(bOnSale.GetListings(), listingC) == nil {
			fail("scope-global-b-sees-c", "global scope 下 B 没看到 C 的商品 %d", listingC)
		}
		if sc.CrossZone {
			bFiltered, err := b.browse(onSale(sc.ZoneB, false), wantScope)
			if err != nil {
				fail("scope-global-filter", "%v", err)
			}
			if tradeSmokeFind(bFiltered.GetListings(), listingC) == nil {
				fail("scope-global-filter", "global scope 下 zone_filter=%d 没返回该区商品 %d", sc.ZoneB, listingC)
			}
			if tradeSmokeFind(bFiltered.GetListings(), listingA) != nil {
				fail("scope-global-filter", "global scope 下 zone_filter=%d 仍返回了 zone %d 的商品 %d", sc.ZoneB, sc.ZoneA, listingA)
			}
		}
	}
	zap.L().Info("[trade-smoke] step 3: market scope filtering holds", zap.String("scope", sc.ExpectScope))

	// ---- 步骤 4:公示列表 ----
	if tradeSmokeFind(bOnSale.GetListings(), listingNotice) != nil {
		fail("notice-not-on-sale", "公示中的商品 %d 出现在寄售列表里", listingNotice)
	}
	noticeReq := onSale(0, false)
	noticeReq.Tab = tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE
	bNotice, err := b.browse(noticeReq, wantScope)
	if err != nil {
		fail("notice", "%v", err)
	}
	if s := tradeSmokeFind(bNotice.GetListings(), listingNotice); s == nil {
		fail("notice", "B 在公示列表里没看到 A 的公示商品 %d", listingNotice)
	} else if err := tradeSmokeCheckSummary(s, seedNotice, tradepb.ListingPhase_LISTING_PHASE_PUBLIC_NOTICE, false, sc.ZoneA); err != nil {
		fail("notice", "%v", err)
	}
	if tradeSmokeFind(bNotice.GetListings(), listingA) != nil {
		fail("notice", "已进入寄售的商品 %d 出现在公示列表里", listingA)
	}
	zap.L().Info("[trade-smoke] step 4: public-notice tab separated from on-sale tab")

	// ---- 步骤 5:分页钳制 ----
	oversizeReq := onSale(0, false)
	oversizeReq.PageSize = tradeSmokeOversizePageSize
	oversize, err := b.browse(oversizeReq, wantScope)
	if err != nil {
		fail("page-size-clamp", "%v", err)
	}
	if oversize.GetPageSize() != tradeSmokeServerMaxPageSize || uint32(len(oversize.GetListings())) > tradeSmokeServerMaxPageSize {
		fail("page-size-clamp", "page_size=%d 的响应 page_size=%d 条数=%d,期望被钳到 %d",
			tradeSmokeOversizePageSize, oversize.GetPageSize(), len(oversize.GetListings()), tradeSmokeServerMaxPageSize)
	}
	// 页长 1:本轮 nonce 下可见商品 ≥2 条时(global,或同区跑)末页不是第 1 页,钳制才有区分度。
	beyondReq := onSale(0, false)
	beyondReq.Page = tradeSmokeBeyondLastPage
	beyondReq.PageSize = 1
	beyond, err := b.browse(beyondReq, wantScope)
	if err != nil {
		fail("page-clamp", "%v", err)
	}
	if beyond.GetPage() != beyond.GetPageCount() || len(beyond.GetListings()) > 1 {
		fail("page-clamp", "page=%d page_size=1 的响应 page=%d page_count=%d 条数=%d,期望钳到末页且至多 1 条",
			tradeSmokeBeyondLastPage, beyond.GetPage(), beyond.GetPageCount(), len(beyond.GetListings()))
	}
	zap.L().Info("[trade-smoke] step 5: page and page_size clamped",
		zap.Uint32("page_size", oversize.GetPageSize()), zap.Uint32("page", beyond.GetPage()), zap.Uint32("page_count", beyond.GetPageCount()))

	// ---- 步骤 6:拍卖分区未开放 ----
	auctionReq := onSale(0, false)
	auctionReq.Section = tradepb.ListingSection_LISTING_SECTION_AUCTION
	if err := b.expect(game.ClientPlayerJubaozhaiBrowseListingsMessageId, auctionReq,
		&tradepb.BrowseListingsResponse{}, tipTradeFeatureDisabled); err != nil {
		fail("auction-disabled", "%v", err)
	}

	// ---- 步骤 7:详情 ----
	bDetail := &tradepb.GetListingDetailResponse{}
	if err := b.expect(game.ClientPlayerJubaozhaiGetListingDetailMessageId,
		&tradepb.GetListingDetailRequest{ListingId: listingA}, bDetail, 0); err != nil {
		fail("detail-b", "%v", err)
	}
	if err := tradeSmokeCheckDetail(bDetail, seedA, false, sc.ZoneA); err != nil {
		fail("detail-b", "%v", err)
	}
	if err := b.expect(game.ClientPlayerJubaozhaiGetListingDetailMessageId,
		&tradepb.GetListingDetailRequest{ListingId: tradeSmokeMissingListingID}, &tradepb.GetListingDetailResponse{}, tipTradeListingNotFound); err != nil {
		fail("detail-missing", "%v", err)
	}
	if sc.CrossZone {
		if zoneScope {
			if err := c.expect(game.ClientPlayerJubaozhaiGetListingDetailMessageId,
				&tradepb.GetListingDetailRequest{ListingId: listingA}, &tradepb.GetListingDetailResponse{}, tipTradeListingNotFound); err != nil {
				fail("detail-other-zone", "%v", err)
			}
		} else {
			cDetail := &tradepb.GetListingDetailResponse{}
			if err := c.expect(game.ClientPlayerJubaozhaiGetListingDetailMessageId,
				&tradepb.GetListingDetailRequest{ListingId: listingA}, cDetail, 0); err != nil {
				fail("detail-other-zone-global", "%v", err)
			}
			if err := tradeSmokeCheckDetail(cDetail, seedA, false, sc.ZoneA); err != nil {
				fail("detail-other-zone-global", "%v", err)
			}
		}
	}
	aDetail := &tradepb.GetListingDetailResponse{}
	if err := a.expect(game.ClientPlayerJubaozhaiGetListingDetailMessageId,
		&tradepb.GetListingDetailRequest{ListingId: listingA}, aDetail, 0); err != nil {
		fail("detail-seller", "%v", err)
	}
	if err := tradeSmokeCheckDetail(aDetail, seedA, true, sc.ZoneA); err != nil {
		fail("detail-seller", "%v", err)
	}
	zap.L().Info("[trade-smoke] step 7: listing detail visibility holds")

	// ---- 步骤 8:收藏 ----
	favOn := &tradepb.SetFavoriteResponse{}
	if err := b.expect(game.ClientPlayerJubaozhaiSetFavoriteMessageId,
		&tradepb.SetFavoriteRequest{ListingId: listingA, Favorite: true}, favOn, 0); err != nil {
		fail("favorite-on", "%v", err)
	}
	if favOn.GetListingId() != listingA || !favOn.GetFavorite() {
		fail("favorite-on", "收藏响应 listing_id=%d favorite=%v,期望 %d / true", favOn.GetListingId(), favOn.GetFavorite(), listingA)
	}
	favView, err := b.browse(onSale(0, true), wantScope)
	if err != nil {
		fail("favorite-browse", "%v", err)
	}
	if s := tradeSmokeFind(favView.GetListings(), listingA); s == nil || !s.GetIsFavorite() {
		fail("favorite-browse", "favorites_only 浏览没看到已收藏的商品 %d(或 is_favorite=false)", listingA)
	}
	favOff := &tradepb.SetFavoriteResponse{}
	if err := b.expect(game.ClientPlayerJubaozhaiSetFavoriteMessageId,
		&tradepb.SetFavoriteRequest{ListingId: listingA, Favorite: false}, favOff, 0); err != nil {
		fail("favorite-off", "%v", err)
	}
	if favOff.GetListingId() != listingA || favOff.GetFavorite() {
		fail("favorite-off", "取消收藏响应 listing_id=%d favorite=%v,期望 %d / false", favOff.GetListingId(), favOff.GetFavorite(), listingA)
	}
	unfavView, err := b.browse(onSale(0, true), wantScope)
	if err != nil {
		fail("favorite-off-browse", "%v", err)
	}
	if tradeSmokeFind(unfavView.GetListings(), listingA) != nil {
		fail("favorite-off-browse", "取消收藏后 favorites_only 浏览仍返回商品 %d", listingA)
	}
	zap.L().Info("[trade-smoke] step 8: favorite set / browse / unset round trip")

	// ---- 步骤 9:货架 ----
	// 货架按 listing_id DESC,本轮两条种子是 A 最新的商品,默认页长的第 1 页一定包含它们。
	aShelf := &tradepb.GetMyShelfResponse{}
	if err := a.expect(game.ClientPlayerJubaozhaiGetMyShelfMessageId, &tradepb.GetMyShelfRequest{Page: 1}, aShelf, 0); err != nil {
		fail("shelf-a", "%v", err)
	}
	if aShelf.GetServerNowMs() == 0 {
		fail("shelf-a", "货架响应 server_now_ms=0")
	}
	for _, id := range []uint64{listingA, listingNotice} {
		s := tradeSmokeFind(aShelf.GetListings(), id)
		if s == nil {
			fail("shelf-a", "A 的货架第 1 页(共 %d 条)没有本轮种子商品 %d", aShelf.GetTotalCount(), id)
		}
		if !s.GetIsMine() {
			fail("shelf-a", "A 货架里的商品 %d is_mine=false", id)
		}
	}
	bShelf := &tradepb.GetMyShelfResponse{}
	if err := b.expect(game.ClientPlayerJubaozhaiGetMyShelfMessageId, &tradepb.GetMyShelfRequest{Page: 1}, bShelf, 0); err != nil {
		fail("shelf-b", "%v", err)
	}
	for _, id := range []uint64{listingA, listingNotice} {
		if tradeSmokeFind(bShelf.GetListings(), id) != nil {
			fail("shelf-b", "B 的货架里出现了 A 的商品 %d:货架没按会话身份过滤", id)
		}
	}
	zap.L().Info("[trade-smoke] step 9: shelf scoped to caller")

	zoneB := uint32(0)
	if sc.CrossZone {
		zoneB = sc.ZoneB
	}
	zap.L().Info(fmt.Sprintf("TRADE_SMOKE_OK scope=%s listing_a=%d listing_c=%d zone_a=%d zone_b=%d",
		sc.ExpectScope, listingA, listingC, sc.ZoneA, zoneB),
		zap.Bool("cross_zone", sc.CrossZone),
		zap.Uint64("player_a", a.gc.PlayerId), zap.Uint64("player_b", b.gc.PlayerId), zap.Uint64("player_c", c.gc.PlayerId))
	cleanup()
	_ = zap.L().Sync()
}

// tradeSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。与 guildSmokeLogin 同形。
func tradeSmokeLogin(cfg *config.Config, account string, spacing time.Duration, stats *metrics.Stats) (*tradeSmokeBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate: %w", err)
	}
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &tradeSmokeBot{
		account:        account,
		zone:           cfg.ZoneID,
		gate:           host + ":" + portStr,
		gc:             gc,
		player:         gameobject.NewPlayer(gc.PlayerId),
		stats:          stats,
		requestSpacing: spacing,
		replies:        make(map[uint32]*base.MessageContent),
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), tradeSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

// onMessage:trade 消息号的回包(含信封拒绝)由本场景认领;gate 的 SendTipToClient 记下来供快速失败;其余交给通用分发。
//
// 必须在分发前认领**全部**五个 trade 消息号:ClientPlayerJubaozhai 服务名含 "ClientPlayer",
// 生成器会给它产出 robot handler stub 并登记进 message_body_handler.go;信封拒绝(body 为空)
// 一旦落进 MessageBodyHandler 会被解成全零响应,看上去像 tip=0 受理。
func (b *tradeSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	if tradeSmokeIsTradeMessage(msg.GetMessageId()) {
		b.mu.Lock()
		if _, seen := b.replies[msg.GetMessageId()]; !seen {
			b.replies[msg.GetMessageId()] = msg
		}
		b.mu.Unlock()
		return
	}
	if msg.GetMessageId() == game.SceneClientPlayerCommonSendTipToClientMessageId {
		var tipInfo base.TipInfoMessage
		if err := proto.Unmarshal(msg.GetSerializedMessage(), &tipInfo); err == nil && tipInfo.GetId() != 0 {
			b.mu.Lock()
			b.gateTips = append(b.gateTips, tipInfo.GetId())
			b.mu.Unlock()
		}
	}
	handler.MessageBodyHandler(client, msg)
}

// call 发一个 trade 请求并等同消息号的第一个回包。
// 返回的 envelopeTip 非 0 表示 gate / 路由服级拒绝(信封 error_message,body 为空),此时 response 未填充;
// 为 0 时 response 已解码。gate 推 kServiceUnavailable 返回 error;超时返回包裹 errTradeSmokeTimeout 的 error。
func (b *tradeSmokeBot) call(messageId uint32, request, response proto.Message) (envelopeTip uint32, err error) {
	if wait := b.requestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	b.mu.Lock()
	delete(b.replies, messageId)
	b.gateTips = nil
	b.mu.Unlock()

	if err := b.gc.SendRequest(messageId, request); err != nil {
		return 0, fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()

	deadline := time.Now().Add(tradeSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		reply := b.replies[messageId]
		gateUnavailable := false
		for _, tip := range b.gateTips {
			gateUnavailable = gateUnavailable || tip == tipTradeServiceUnavailable
		}
		b.mu.Unlock()

		if reply != nil {
			if tip := reply.GetErrorMessage().GetId(); tip != 0 {
				return tip, nil
			}
			if err := proto.Unmarshal(reply.GetSerializedMessage(), response); err != nil {
				return 0, fmt.Errorf("decode message_id=%d: %w", messageId, err)
			}
			return 0, nil
		}
		if gateUnavailable {
			return 0, fmt.Errorf("gate SendTipToClient tip=%d(gate 选不到路由服实例,或未以 GATE_CLIENT_RPC_ROUTER=1 启动)",
				tipTradeServiceUnavailable)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, fmt.Errorf("message_id=%d account=%s %w(%s)", messageId, b.account, errTradeSmokeTimeout, tradeSmokeRpcTimeout)
}

// rpc 发请求并返回业务 tip(响应体 error_message.id,0 = 受理)。
// 信封拒绝与超时一律 error:它们说明请求没到 trade 业务逻辑,不能当成 trade 给的码去断言。
func (b *tradeSmokeBot) rpc(messageId uint32, request proto.Message, response tradeResponse) (uint32, error) {
	envelopeTip, err := b.call(messageId, request, response)
	if err != nil {
		return 0, err
	}
	if envelopeTip != 0 {
		return 0, fmt.Errorf("gate/路由服信封拒绝 message_id=%d tip=%d(请求未到达 trade 业务逻辑:tip=%d 多为路由服没有 trade 实例、"+
			"路由表未更新,或 trade 返回了 gRPC 错误(会话拦截器拒绝等);其它常见为 gate 限流,可调大 request_interval_ms)",
			messageId, envelopeTip, tipTradeServiceUnavailable)
	}
	return response.GetErrorMessage().GetId(), nil
}

// expect 发请求并断言业务 tip 等于 want;常见的环境类拒绝码翻译成可读原因。
func (b *tradeSmokeBot) expect(messageId uint32, request proto.Message, response tradeResponse, want uint32) error {
	got, err := b.rpc(messageId, request, response)
	if err != nil {
		return err
	}
	if got == want {
		return nil
	}
	switch got {
	case tipTradeHomeZoneUnknown:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(data_service 里没有该角色的归属区映射;"+
			"老账号请跑 tools/merge_zone -backfill-home-zone,或换一个首次登录的 robot_ 账号)", messageId, b.account, got)
	case tipTradeServiceUnavailable:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(trade 侧存储 / data_service 故障,看 trade 错误日志)",
			messageId, b.account, got)
	case tipTradeInvalidParameter:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(请求校验不通过,或 trade 在逻辑层拿不到会话身份——"+
			"检查路由服是否注入会话 metadata),期望 tip=%d", messageId, b.account, got, want)
	}
	return fmt.Errorf("message_id=%d account=%s 期望 tip=%d,实得 tip=%d", messageId, b.account, want, got)
}

// browse 发 BrowseListings 并断言受理,同时核对每次浏览都必须成立的响应不变量:
// market_scope 等于 expect_scope、server_now_ms 非 0、page_count = max(1, ceil(total/page_size))、1 ≤ page ≤ page_count。
func (b *tradeSmokeBot) browse(req *tradepb.BrowseListingsRequest, wantScope tradepb.MarketScope) (*tradepb.BrowseListingsResponse, error) {
	resp := &tradepb.BrowseListingsResponse{}
	if err := b.expect(game.ClientPlayerJubaozhaiBrowseListingsMessageId, req, resp, 0); err != nil {
		return nil, err
	}
	if resp.GetMarketScope() != wantScope {
		return nil, fmt.Errorf("account=%s 浏览响应 market_scope=%s,期望 %s(trade.yaml Market.Scope 与 trade_smoke.expect_scope 不一致?)",
			b.account, resp.GetMarketScope(), wantScope)
	}
	if resp.GetServerNowMs() == 0 {
		return nil, fmt.Errorf("account=%s 浏览响应 server_now_ms=0", b.account)
	}
	if !tradeSmokePageCountConsistent(resp.GetTotalCount(), resp.GetPageSize(), resp.GetPageCount()) ||
		resp.GetPage() == 0 || resp.GetPage() > resp.GetPageCount() {
		return nil, fmt.Errorf("account=%s 浏览响应分页不自洽:total=%d page_size=%d page=%d page_count=%d",
			b.account, resp.GetTotalCount(), resp.GetPageSize(), resp.GetPage(), resp.GetPageCount())
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// TradeAdmin 直连
// ---------------------------------------------------------------------------

// tradeSmokeSeedRequest 构造一条武器类种子商品;noticeMs=0 表示立即进入寄售。
func tradeSmokeSeedRequest(seller uint64, title string, noticeMs uint64) *tradepb.SeedListingRequest {
	return &tradepb.SeedListingRequest{
		SellerPlayerId:   seller,
		Category:         tradeSmokeCategory,
		Subcategory:      tradeSmokeSubcategory,
		Title:            title,
		Level:            tradeSmokeLevel,
		PriceFen:         tradeSmokePriceFen,
		Summary:          "trade-smoke 种子",
		Description:      "trade-smoke 种子商品 " + title,
		IconKey:          tradeSmokeIconKey,
		NoticeDurationMs: noticeMs,
		SaleDurationMs:   tradeSmokeSaleDurationMs,
	}
}

// tradeSmokeSeed 调 TradeAdmin.SeedListing(无会话 metadata),受理且 listing_id 非 0 才返回响应。
func tradeSmokeSeed(client tradepb.TradeAdminClient, req *tradepb.SeedListingRequest) (*tradepb.SeedListingResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tradeSmokeAdminTimeout)
	defer cancel()
	resp, err := client.SeedListing(ctx, req)
	if err != nil {
		hint := ""
		switch status.Code(err) {
		case codes.PermissionDenied:
			hint = "(trade 的 Mode 不是 dev/test,SeedListing 被关闭;或调用带上了会话 metadata)"
		case codes.Unavailable, codes.DeadlineExceeded:
			hint = "(trade 未起或 trade_smoke.admin_addr 不可达)"
		case codes.Unimplemented:
			hint = "(trade 没有注册 TradeAdmin 服务,二进制是否过旧)"
		}
		return nil, fmt.Errorf("SeedListing seller=%d title=%q gRPC 错误 %v%s", req.GetSellerPlayerId(), req.GetTitle(), err, hint)
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		hint := ""
		switch tip {
		case tipTradeHomeZoneUnknown:
			hint = "(卖家没有归属区映射:换首次登录的 robot_ 账号,或跑 tools/merge_zone -backfill-home-zone)"
		case tipTradeServiceUnavailable:
			hint = "(trade 侧存储 / data_service / 号段故障:检查 mmorpg_trade 迁移、data_service BootstrapTags 含 trade_listing)"
		case tipTradeInvalidParameter:
			hint = "(种子参数没通过 trade 校验)"
		}
		return nil, fmt.Errorf("SeedListing seller=%d title=%q 业务拒绝 tip=%d%s", req.GetSellerPlayerId(), req.GetTitle(), tip, hint)
	}
	if resp.GetListingId() == 0 {
		return nil, fmt.Errorf("SeedListing seller=%d title=%q 受理但 listing_id=0", req.GetSellerPlayerId(), req.GetTitle())
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func tradeSmokeIsTradeMessage(messageId uint32) bool {
	switch messageId {
	case game.ClientPlayerJubaozhaiBrowseListingsMessageId,
		game.ClientPlayerJubaozhaiGetListingDetailMessageId,
		game.ClientPlayerJubaozhaiSetFavoriteMessageId,
		game.ClientPlayerJubaozhaiGetMyShelfMessageId,
		game.TradeAdminSeedListingMessageId:
		return true
	}
	return false
}

func tradeSmokeScopeOf(scope string) (tradepb.MarketScope, bool) {
	switch scope {
	case "zone":
		return tradepb.MarketScope_MARKET_SCOPE_ZONE, true
	case "global":
		return tradepb.MarketScope_MARKET_SCOPE_GLOBAL, true
	}
	return tradepb.MarketScope_MARKET_SCOPE_UNSPECIFIED, false
}

func tradeSmokeFind(listings []*tradepb.ListingSummary, listingID uint64) *tradepb.ListingSummary {
	for _, s := range listings {
		if s.GetListingId() == listingID {
			return s
		}
	}
	return nil
}

func tradeSmokeHasTitle(listings []*tradepb.ListingSummary, title string) bool {
	for _, s := range listings {
		if s.GetTitle() == title {
			return true
		}
	}
	return false
}

// tradeSmokePageCountConsistent:page_count 必须等于 max(1, ceil(total / page_size)),page_size 不能为 0。
func tradeSmokePageCountConsistent(total, pageSize, pageCount uint32) bool {
	if pageSize == 0 {
		return false
	}
	want := (uint64(total) + uint64(pageSize) - 1) / uint64(pageSize)
	if want == 0 {
		want = 1
	}
	return uint64(pageCount) == want
}

// tradeSmokeCheckSummary 核对列表 / 详情里的商品摘要与种子请求一致(防串商品、防阶段 / 归属推导错)。
func tradeSmokeCheckSummary(s *tradepb.ListingSummary, seed *tradepb.SeedListingRequest,
	wantPhase tradepb.ListingPhase, wantMine bool, wantZone uint32) error {
	switch {
	case s.GetTitle() != seed.GetTitle():
		return fmt.Errorf("商品 %d title=%q,期望 %q", s.GetListingId(), s.GetTitle(), seed.GetTitle())
	case s.GetCategory() != seed.GetCategory() || s.GetSubcategory() != seed.GetSubcategory():
		return fmt.Errorf("商品 %d category=%s subcategory=%d,期望 %s / %d",
			s.GetListingId(), s.GetCategory(), s.GetSubcategory(), seed.GetCategory(), seed.GetSubcategory())
	case s.GetLevel() != seed.GetLevel() || s.GetPriceFen() != seed.GetPriceFen():
		return fmt.Errorf("商品 %d level=%d price_fen=%d,期望 %d / %d",
			s.GetListingId(), s.GetLevel(), s.GetPriceFen(), seed.GetLevel(), seed.GetPriceFen())
	case s.GetSummary() != seed.GetSummary() || s.GetIconKey() != seed.GetIconKey():
		return fmt.Errorf("商品 %d summary=%q icon_key=%q,期望 %q / %q",
			s.GetListingId(), s.GetSummary(), s.GetIconKey(), seed.GetSummary(), seed.GetIconKey())
	case s.GetPhase() != wantPhase:
		return fmt.Errorf("商品 %d phase=%s,期望 %s", s.GetListingId(), s.GetPhase(), wantPhase)
	case s.GetIsMine() != wantMine:
		return fmt.Errorf("商品 %d is_mine=%v,期望 %v", s.GetListingId(), s.GetIsMine(), wantMine)
	case s.GetMarketZone() != wantZone:
		return fmt.Errorf("商品 %d market_zone=%d,期望 %d", s.GetListingId(), s.GetMarketZone(), wantZone)
	case s.GetSaleEndMs() <= s.GetNoticeEndMs():
		return fmt.Errorf("商品 %d sale_end_ms=%d 不晚于 notice_end_ms=%d", s.GetListingId(), s.GetSaleEndMs(), s.GetNoticeEndMs())
	}
	return nil
}

// tradeSmokeCheckDetail 核对详情响应:摘要与种子一致、描述一致、server_now_ms 非 0。
func tradeSmokeCheckDetail(resp *tradepb.GetListingDetailResponse, seed *tradepb.SeedListingRequest, wantMine bool, wantZone uint32) error {
	if resp.GetServerNowMs() == 0 {
		return errors.New("详情响应 server_now_ms=0")
	}
	summary := resp.GetDetail().GetSummary()
	if summary == nil {
		return errors.New("详情受理但响应里没有 summary")
	}
	if err := tradeSmokeCheckSummary(summary, seed, tradepb.ListingPhase_LISTING_PHASE_ON_SALE, wantMine, wantZone); err != nil {
		return err
	}
	if resp.GetDetail().GetDescription() != seed.GetDescription() {
		return fmt.Errorf("商品 %d description=%q,期望 %q", summary.GetListingId(), resp.GetDetail().GetDescription(), seed.GetDescription())
	}
	return nil
}

// tradeSmokeNonce 返回本轮标题前缀 `SMK-<纳秒>`:P1 没有下架,种子逐轮累积,
// 所有浏览都以它作 search 并按 listing_id 找自己的商品。不含 LIKE 通配符,也不是纯数字(不会走编号精确匹配)。
func tradeSmokeNonce() string {
	return "SMK-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}
