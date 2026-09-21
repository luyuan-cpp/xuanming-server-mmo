package logic

// 帮会经济(B5)的配表校验与运行期查表(docs/design/guild-phase2/05-economy.md §5.11,
// 90-consistency.md X-04 / X-16)。
//
// 两条纪律沿用 guild_manage_logic.go 的 §10.2 / §3.3:
//
//  1. **配表只存 id、用时现查**:table 包是 atomic 快照,热更整批换指针;启动时
//     ValidateEconomyTables 已保证行存在且自洽,所以运行期查不到 = 配表被错误替换,一律 fail-closed。
//  2. **启动期拒启而不是运行期兜底**:捐献扣的是玩家的货币,表错了在运行期表现为
//     "扣了钱没入账""一次兑换溢出背包"这类不可逆的静默错误;启动期拒绝的代价只是一次部署回滚。

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"guild/internal/constants"
	"shared/gameday"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

// 可捐献的货币类型。数值的权威定义在 C++ 的 cpp/libs/modules/currency/constants/currency.h
// (kCurrencyGold = 0 / kCurrencyDiamond = 1),该枚举没有 proto 镜像,只能写数值 + 出处,
// 改那边要连这里一起改。绑定灵石(kCurrencyBindDiamond = 2)按 05 §5.1 R1 不可捐,所以不在这里。
const (
	donateCurrencyGold    uint32 = 0
	donateCurrencyDiamond uint32 = 1
)

// 商店分类:1 修行补给 / 2 帮会珍藏 / 3 节庆好礼。客户端按这三个分类各开一个页签,
// 所以每个分类都至少要有一行,否则页签是空的。
var shopCategories = []uint32{1, 2, 3}

// 配表取值的合法区间。写成常量而不是字面量:错误文案与判据用同一组数,改区间时不会出现
// "判据改了、文案还说旧范围"(与 guild_manage_logic.go 的区间常量同一口径)。
const (
	// asset_op_deadline_seconds:捐献指令多久没结算就改发中止。太短会把战斗中的正常捐献中止掉,
	// 太长则离线玩家的捐献会挂很久(占着今日次数)。
	minAssetOpDeadlineSeconds uint32 = 60
	maxAssetOpDeadlineSeconds uint32 = 86400
	// asset_op_retry_base_ms:重投循环的退避基数(assetop.LoopConfig.BaseBackoff)。
	// 下限防止把 scene 打爆;上限 60000 只与 AssetOp.MaxBackoffMs 的**默认值**一致,并不保证不超过
	// 运行时配置的 MaxBackoffMs(它最小可配到 1000)。"基数 ≤ 封顶"是配表与配置的交叉约束,
	// 由 ValidateAssetOpTiming 在启动期判(assetop.NewLoop 也会拒,只是报错不指名列)。
	minAssetOpRetryBaseMs uint32 = 100
	maxAssetOpRetryBaseMs uint32 = 60000
	// upgrade_cost_funds 的上限:防误填超大值(多写几个 0 会让帮会永远升不上去,且没有任何报错)。
	maxUpgradeCostFunds uint64 = 1_000_000_000_000
	// cost_contribution 的上限:保证 cost × MaxShopBuyCount 远离 uint64 溢出。
	maxShopCostContribution uint64 = 1_000_000_000
	// 每种货币至多几行捐献选项:客户端一个货币面板只放两个按钮。
	maxDonateOptionsPerCurrency int = 2
)

// economyTables 是 validateEconomyTables 的全部输入。纯数据,便于单测逐条造坏样例,
// 不必去动 table 包的全局快照。
type economyTables struct {
	rule    *tablepb.GuildRuleTable
	levels  []*tablepb.GuildLevelTable
	donates []*tablepb.GuildDonateTable
	shops   []*tablepb.GuildShopTable
	// itemMaxStack 取 Item.max_stack_size;ok=false = 物品不存在。
	itemMaxStack func(itemID uint32) (uint32, bool)
}

// ValidateEconomyTables 由 guild.go 紧跟 ValidateGuildTables 之后调用,失败即 logx.Must。
//
// 它开头**再调一次** validateGuildTables:GuildRule / GuildLevel 的结构规则只在那里写一份
// (X-16),这里要用"GuildLevel 从 1 连续"这条保证来推出最高级,不能假设调用方一定先调过那边。
func ValidateEconomyTables() error {
	rule, _ := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	return validateEconomyTables(economyTables{
		rule:         rule,
		levels:       table.GuildLevelTableManagerInstance.FindAll(),
		donates:      table.GuildDonateTableManagerInstance.FindAll(),
		shops:        table.GuildShopTableManagerInstance.FindAll(),
		itemMaxStack: itemMaxStack,
	})
}

// validateEconomyTables 是纯函数。错误文案一律带表名、行 id、字段名与实际值,
// 策划看到启动日志要能直接定位到格子。
func validateEconomyTables(t economyTables) error {
	if err := validateGuildTables(t.rule, t.levels); err != nil {
		return err
	}
	// 走到这里 rule 非 nil、levels 非空、无空行且 id 从 1 连续(validateGuildTables 的保证),
	// 所以最高级就是行数。
	maxLevel := uint32(len(t.levels))

	if v := t.rule.GetAssetOpDeadlineSeconds(); v < minAssetOpDeadlineSeconds || v > maxAssetOpDeadlineSeconds {
		return fmt.Errorf("GuildRule[%d].asset_op_deadline_seconds=%d 越界,应在 [%d,%d]",
			guildRuleRowID, v, minAssetOpDeadlineSeconds, maxAssetOpDeadlineSeconds)
	}
	if v := t.rule.GetAssetOpRetryBaseMs(); v < minAssetOpRetryBaseMs || v > maxAssetOpRetryBaseMs {
		return fmt.Errorf("GuildRule[%d].asset_op_retry_base_ms=%d 越界,应在 [%d,%d]",
			guildRuleRowID, v, minAssetOpRetryBaseMs, maxAssetOpRetryBaseMs)
	}
	for _, row := range t.levels {
		if cost := row.GetUpgradeCostFunds(); cost > maxUpgradeCostFunds {
			return fmt.Errorf("GuildLevel[%d].upgrade_cost_funds=%d 超过上限 %d(防误填超大值)",
				row.GetId(), cost, maxUpgradeCostFunds)
		}
	}
	if err := validateDonateRows(t.donates, maxLevel); err != nil {
		return err
	}
	return validateShopRows(t.shops, maxLevel, t.itemMaxStack)
}

// validateDonateRows:05 §5.11.2 规则 3。
func validateDonateRows(rows []*tablepb.GuildDonateTable, maxLevel uint32) error {
	seen := make(map[uint32]struct{}, len(rows))
	perCurrency := make(map[uint32]int, 2)
	for i, row := range rows {
		if row == nil {
			return fmt.Errorf("GuildDonate 表第 %d 行为空", i+1)
		}
		id := row.GetId()
		// 0 是请求里 donate_id 的 proto 缺省值:客户端漏填字段时不能恰好命中一个真实选项。
		if id == 0 {
			return fmt.Errorf("GuildDonate 第 %d 行 id=0:0 是请求缺省值,不能对应真实选项", i+1)
		}
		// 重复 id 会让选项列表里出现两个按钮、点下去却扣同一行的数值。
		if _, dup := seen[id]; dup {
			return fmt.Errorf("GuildDonate.id=%d 重复", id)
		}
		seen[id] = struct{}{}

		currency := row.GetCurrencyType()
		if currency != donateCurrencyGold && currency != donateCurrencyDiamond {
			return fmt.Errorf("GuildDonate[%d].currency_type=%d 非法,只允许 %d(银两)/ %d(灵石)",
				id, currency, donateCurrencyGold, donateCurrencyDiamond)
		}
		// scene 的 Debit 按有符号 64 位校验数额,超过 MaxInt64 会被判成非法包(永久拒绝)。
		if v := row.GetCostAmount(); v < 1 || v > math.MaxInt64 {
			return fmt.Errorf("GuildDonate[%d].cost_amount=%d 越界,应在 [1,%d]", id, v, uint64(math.MaxInt64))
		}
		// 两项收益都为 0 = 玩家白扣钱,只能是填错。
		if row.GetContributionGain() == 0 && row.GetFundsGain() == 0 {
			return fmt.Errorf("GuildDonate[%d] 的 contribution_gain 与 funds_gain 不能同时为 0", id)
		}
		if row.GetDailyLimit() < 1 {
			return fmt.Errorf("GuildDonate[%d].daily_limit=0:每日次数至少为 1", id)
		}
		if v := row.GetMinGuildLevel(); v < 1 || v > maxLevel {
			return fmt.Errorf("GuildDonate[%d].min_guild_level=%d 越界,应在 [1,%d](GuildLevel 最高级)", id, v, maxLevel)
		}
		perCurrency[currency]++
		if perCurrency[currency] > maxDonateOptionsPerCurrency {
			return fmt.Errorf("GuildDonate:currency_type=%d 的选项超过 %d 行(客户端一个货币面板只放两个按钮)",
				currency, maxDonateOptionsPerCurrency)
		}
	}
	return nil
}

// validateShopRows:05 §5.11.2 规则 4。
func validateShopRows(rows []*tablepb.GuildShopTable, maxLevel uint32, stackOf func(uint32) (uint32, bool)) error {
	if stackOf == nil {
		// 只可能是接线错误;没有物品表就判不了"一次兑换会不会溢出背包",不能放行。
		return errors.New("GuildShop 校验缺少 Item 表查询")
	}
	seen := make(map[uint32]struct{}, len(rows))
	categories := make(map[uint32]bool, len(shopCategories))
	for i, row := range rows {
		if row == nil {
			return fmt.Errorf("GuildShop 表第 %d 行为空", i+1)
		}
		id := row.GetId()
		if id == 0 {
			return fmt.Errorf("GuildShop 第 %d 行 id=0:0 是请求缺省值,不能对应真实商品", i+1)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("GuildShop.id=%d 重复", id)
		}
		seen[id] = struct{}{}

		category := row.GetCategory()
		if !isShopCategory(category) {
			return fmt.Errorf("GuildShop[%d].category=%d 非法,只允许 %v", id, category, shopCategories)
		}
		maxStack, ok := stackOf(row.GetItemId())
		if !ok || maxStack < 1 {
			return fmt.Errorf("GuildShop[%d].item_id=%d 在 Item 表里不存在或 max_stack_size=0", id, row.GetItemId())
		}
		// item_count ≤ max_stack_size 同时覆盖了"堆叠 1 的物品每份只能 1 个"(每份占 1 格)。
		if c := row.GetItemCount(); c < 1 || c > maxStack {
			return fmt.Errorf("GuildShop[%d].item_count=%d 越界,应在 [1,%d](物品 %d 的 max_stack_size)",
				id, c, maxStack, row.GetItemId())
		}
		// 由上一条可推出 MaxBuyCount ≥ 1;仍显式判一次,防 MaxBuyCount 的公式日后改动后悄悄变成 0
		// (那会让这件商品永远买不了,且没有任何报错)。
		if MaxBuyCount(row, maxStack) < 1 {
			return fmt.Errorf("GuildShop[%d] 单次可兑换份数为 0(item_count=%d,max_stack_size=%d)",
				id, row.GetItemCount(), maxStack)
		}
		if v := row.GetCostContribution(); v < 1 || v > maxShopCostContribution {
			return fmt.Errorf("GuildShop[%d].cost_contribution=%d 越界,应在 [1,%d]", id, v, maxShopCostContribution)
		}
		if v := row.GetRequiredGuildLevel(); v < 1 || v > maxLevel {
			return fmt.Errorf("GuildShop[%d].required_guild_level=%d 越界,应在 [1,%d](GuildLevel 最高级)", id, v, maxLevel)
		}
		period := row.GetLimitPeriod()
		if period != gameday.PeriodNone && period != gameday.PeriodDaily && period != gameday.PeriodWeekly {
			return fmt.Errorf("GuildShop[%d].limit_period=%d 非法,只允许 %d(不限)/ %d(每日)/ %d(每周)",
				id, period, gameday.PeriodNone, gameday.PeriodDaily, gameday.PeriodWeekly)
		}
		// 不限购就不占计数行(period_key=0);限购却填 0 份等于永远买不了。两者必须同时成立或同时不成立。
		if (period == gameday.PeriodNone) != (row.GetLimitCount() == 0) {
			return fmt.Errorf("GuildShop[%d]:limit_period=%d 与 limit_count=%d 不一致(不限购时份数必须为 0,限购时至少为 1)",
				id, period, row.GetLimitCount())
		}
		categories[category] = true
	}
	for _, c := range shopCategories {
		if !categories[c] {
			return fmt.Errorf("GuildShop 缺少 category=%d 的商品(客户端每个分类一个页签)", c)
		}
	}
	return nil
}

func isShopCategory(c uint32) bool {
	for _, known := range shopCategories {
		if c == known {
			return true
		}
	}
	return false
}

// MaxBuyCount 是一件商品单次最多兑换的份数(05 §5.11.2):
// max_stack_size == 1 → 1(每份占 1 格);否则 min(MaxShopBuyCount, max_stack_size / item_count),
// 即一次兑换的物品总数不超过一组堆叠 —— 背包只需一格空位就能收下,"背包满"不会因为买得多而变成常态。
//
// 它不看限购:限购份数小于单次上限时,实际以限购为准(事务内判)。
// 行为 nil、堆叠为 0 或 item_count 为 0(都只可能来自坏配表)时回 0 = 不可兑换,fail-closed。
func MaxBuyCount(row *tablepb.GuildShopTable, maxStack uint32) uint32 {
	if row == nil || maxStack == 0 {
		return 0
	}
	if maxStack == 1 {
		return 1
	}
	itemCount := row.GetItemCount()
	if itemCount == 0 {
		return 0
	}
	return min(maxStack/itemCount, constants.MaxShopBuyCount)
}

// AssetOpRetryBase 是重投循环的退避基数(GuildRule[1].asset_op_retry_base_ms),由 guild.go
// 在 ValidateEconomyTables 之后传给 svc.LoopConfigFrom。
//
// 配表缺行时回 0:assetop.NewLoop 的校验要求 BaseBackoff > 0,于是启动失败而不是带着一个
// 猜出来的默认值跑 —— 同一个数只有配表一个来源(DRY)。
func AssetOpRetryBase() time.Duration {
	rule, ok := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	if !ok {
		return 0
	}
	return time.Duration(rule.GetAssetOpRetryBaseMs()) * time.Millisecond
}

// ValidateAssetOpTiming 校验运行配置(AssetOp.*)与配表(GuildRule 的两列资产参数)之间的**交叉**约束:
// 两边各自都在合法区间里、组合起来却会静默出错的那一类。只在资产通道开启时有意义(关着时不插行、不起循环);
// guild.go 在 ValidateEconomyTables 之后、建重投循环之前调用,失败即拒启。
//
//  1. lease + reconcileInterval < asset_op_deadline_seconds。插行时 next_attempt_ms = lease_until_ms = start + lease,
//     同步投递被跳过(剩余预算不足)或提交后进程崩溃的捐献,最早要在 start + lease 之后的下一轮对账才被循环领到;
//     那时若已过截止,循环直接改发中止 —— 玩家的捐献一次扣款都没尝试就被"超时撤销",且没有任何报错。
//     余量只取一个对账间隔(保证循环第一次领到它时仍按扣款发),**不**再加 MaxBackoff:截止取配表下限 60s、
//     MaxBackoff 取默认 60s 时,退避从基数起指数增长,截止前仍有多次重试,加上它会把这类合法组合拒掉。
//  2. asset_op_retry_base_ms ≤ maxBackoff。assetop.NewLoop 同样会拒,但报错只有"退避区间非法";
//     这里把配表列名与配置项名一起报出来,运维不必去翻两处。
func ValidateAssetOpTiming(lease, reconcileInterval, maxBackoff time.Duration) error {
	rule, ok := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	if !ok {
		return fmt.Errorf("GuildRule 表缺少 id=%d 的规则行", guildRuleRowID)
	}
	return validateAssetOpTiming(rule, lease, reconcileInterval, maxBackoff)
}

// validateAssetOpTiming 是 ValidateAssetOpTiming 的纯函数部分(不碰 table 包的全局快照,便于单测)。
func validateAssetOpTiming(rule *tablepb.GuildRuleTable, lease, reconcileInterval, maxBackoff time.Duration) error {
	deadline := time.Duration(rule.GetAssetOpDeadlineSeconds()) * time.Second
	if lease+reconcileInterval >= deadline {
		return fmt.Errorf("AssetOp.LeaseMs(%v)+ AssetOp.ReconcileIntervalMs(%v)必须小于 GuildRule[%d].asset_op_deadline_seconds(%v):"+
			"否则同步投递被跳过的捐献在重投循环第一次领到它之前就已过截止,未尝试扣款即被中止",
			lease, reconcileInterval, guildRuleRowID, deadline)
	}
	if base := time.Duration(rule.GetAssetOpRetryBaseMs()) * time.Millisecond; base > maxBackoff {
		return fmt.Errorf("GuildRule[%d].asset_op_retry_base_ms(%v)不得大于 AssetOp.MaxBackoffMs(%v)",
			guildRuleRowID, base, maxBackoff)
	}
	return nil
}

// assetOpDeadlineMs 是捐献指令从创建到改发中止的时长(毫秒)。ok=false = 配表缺行,调用方回 Internal。
func assetOpDeadlineMs() (uint64, bool) {
	rule, ok := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	if !ok {
		return 0, false
	}
	return uint64(rule.GetAssetOpDeadlineSeconds()) * uint64(time.Second/time.Millisecond), true
}

// itemMaxStack 取物品的堆叠上限。ok=false = Item 表里没有这个物品。
func itemMaxStack(itemID uint32) (uint32, bool) {
	row, ok := table.ItemTableManagerInstance.FindById(itemID)
	if !ok {
		return 0, false
	}
	return row.GetMaxStackSize(), true
}

// upgradeLevelLookup 满足 data.LevelLookup:按等级取升级花费与成员上限。
// 升级扣的是**当前等级行**的花费,新成员上限取**下一级行**的 max_members(05 §5.18),两次查询都在 repo 事务里做。
func upgradeLevelLookup(level uint32) (uint64, uint32, bool) {
	row, ok := table.GuildLevelTableManagerInstance.FindById(level)
	if !ok {
		return 0, 0, false
	}
	return row.GetUpgradeCostFunds(), row.GetMaxMembers(), true
}

// sortedDonateRows:捐献选项按 donate_id 升序下发(协议注释)。
// 复制一份再排:FindAll 返回的是快照内部切片,原地排序等于改别人的数据。
func sortedDonateRows() []*tablepb.GuildDonateTable {
	all := table.GuildDonateTableManagerInstance.FindAll()
	rows := make([]*tablepb.GuildDonateTable, 0, len(all))
	for _, row := range all {
		if row != nil {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].GetId() < rows[j].GetId() })
	return rows
}

// sortedShopRows:商品按 (category, goods_id) 升序下发(协议注释)。复制理由同上。
func sortedShopRows() []*tablepb.GuildShopTable {
	all := table.GuildShopTableManagerInstance.FindAll()
	rows := make([]*tablepb.GuildShopTable, 0, len(all))
	for _, row := range all {
		if row != nil {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].GetCategory() != rows[j].GetCategory() {
			return rows[i].GetCategory() < rows[j].GetCategory()
		}
		return rows[i].GetId() < rows[j].GetId()
	})
	return rows
}
