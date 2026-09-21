package logic

// 帮会经济配表校验的单测(05-economy.md §5.11、90-consistency.md X-04 / X-16)。
//
// 全部用例只喂 validateEconomyTables 纯数据,不碰 table 包的全局快照:
// 本包其它用例依赖"未加载配表"的状态(例如 levelDisplay 查不到时留 0),改全局快照会让它们互相污染。
// 唯一例外是 build tag integration 的 economy_flow_integration_test.go:它必须加载真实配表,
// 做法是换上新建的管理器实例、用例结束换回原实例(见其 loadEconomyFlowTables),不改动这里依赖的状态。
// legalGuildRule / legalGuildLevels 在 guild_manage_logic_test.go(与 B2 共用同一份默认行)。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	tablepb "shared/generated/pb/table"
)

// legalDonates 是 GuildDonate.xlsx 的默认 3 行(05 §5.10)。
func legalDonates() []*tablepb.GuildDonateTable {
	return []*tablepb.GuildDonateTable{
		{Id: 1, Name: "银两小捐", CurrencyType: 0, CostAmount: 10000, ContributionGain: 10, FundsGain: 1000, DailyLimit: 5, MinGuildLevel: 1},
		{Id: 2, Name: "银两大捐", CurrencyType: 0, CostAmount: 100000, ContributionGain: 120, FundsGain: 12000, DailyLimit: 2, MinGuildLevel: 1},
		{Id: 3, Name: "灵石捐献", CurrencyType: 1, CostAmount: 100, ContributionGain: 200, FundsGain: 20000, DailyLimit: 1, MinGuildLevel: 1},
	}
}

// legalShops 是 GuildShop.xlsx 的默认 11 行(05 §5.10,204 的等级按 X-04 为 6)。
func legalShops() []*tablepb.GuildShopTable {
	return []*tablepb.GuildShopTable{
		{Id: 101, Name: "培元丹", Category: 1, ItemId: 15, ItemCount: 5, CostContribution: 30, RequiredGuildLevel: 1, LimitPeriod: 1, LimitCount: 10},
		{Id: 102, Name: "回灵散", Category: 1, ItemId: 16, ItemCount: 5, CostContribution: 30, RequiredGuildLevel: 1, LimitPeriod: 1, LimitCount: 10},
		{Id: 103, Name: "精炼石", Category: 1, ItemId: 17, ItemCount: 1, CostContribution: 80, RequiredGuildLevel: 2, LimitPeriod: 1, LimitCount: 5},
		{Id: 104, Name: "修行秘录残页", Category: 1, ItemId: 18, ItemCount: 1, CostContribution: 150, RequiredGuildLevel: 3, LimitPeriod: 2, LimitCount: 5},
		{Id: 201, Name: "帮会令牌", Category: 2, ItemId: 19, ItemCount: 1, CostContribution: 300, RequiredGuildLevel: 3, LimitPeriod: 2, LimitCount: 3},
		{Id: 202, Name: "玄铁护符", Category: 2, ItemId: 12, ItemCount: 1, CostContribution: 800, RequiredGuildLevel: 4, LimitPeriod: 2, LimitCount: 1},
		{Id: 203, Name: "灵兽口粮", Category: 2, ItemId: 20, ItemCount: 10, CostContribution: 120, RequiredGuildLevel: 2, LimitPeriod: 1, LimitCount: 3},
		{Id: 204, Name: "藏经阁手札", Category: 2, ItemId: 13, ItemCount: 1, CostContribution: 1500, RequiredGuildLevel: 6, LimitPeriod: 2, LimitCount: 1},
		{Id: 301, Name: "花灯", Category: 3, ItemId: 21, ItemCount: 1, CostContribution: 50, RequiredGuildLevel: 1, LimitPeriod: 1, LimitCount: 5},
		{Id: 302, Name: "月饼礼盒", Category: 3, ItemId: 22, ItemCount: 1, CostContribution: 100, RequiredGuildLevel: 1, LimitPeriod: 2, LimitCount: 7},
		{Id: 303, Name: "同心结", Category: 3, ItemId: 23, ItemCount: 1, CostContribution: 200, RequiredGuildLevel: 5, LimitPeriod: 0, LimitCount: 0},
	}
}

// legalItemStacks:商品引用的物品及其 max_stack_size(05 §5.10 已核:15–23 为 999,12、13 为 1)。
func legalItemStacks() map[uint32]uint32 {
	return map[uint32]uint32{12: 1, 13: 1, 15: 999, 16: 999, 17: 999, 18: 999, 19: 999, 20: 999, 21: 999, 22: 999, 23: 999}
}

func stackLookup(stacks map[uint32]uint32) func(uint32) (uint32, bool) {
	return func(itemID uint32) (uint32, bool) {
		v, ok := stacks[itemID]
		return v, ok
	}
}

// legalEconomyTables 每次都重新构造,用例之间不共享行指针。
func legalEconomyTables() economyTables {
	return economyTables{
		rule:         legalGuildRule(),
		levels:       legalGuildLevels(),
		donates:      legalDonates(),
		shops:        legalShops(),
		itemMaxStack: stackLookup(legalItemStacks()),
	}
}

func TestValidateEconomyTablesAcceptsDefaultRows(t *testing.T) {
	require.NoError(t, validateEconomyTables(legalEconomyTables()))
}

// TestValidateEconomyTablesRejects:每条规则各造一个坏样例。错误文案必须带表名 / 字段名,
// 策划看到启动日志要能直接定位到格子。
func TestValidateEconomyTablesRejects(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(*economyTables)
		wantContains string
	}{
		// 先走 validateGuildTables(X-16:GuildRule / GuildLevel 的结构规则只写一份)。
		{"缺少规则行", func(e *economyTables) { e.rule = nil }, "GuildRule"},
		{"等级表为空", func(e *economyTables) { e.levels = nil }, "GuildLevel"},

		// GuildRule 的两列资产参数。
		{"截止时长 59 秒", func(e *economyTables) { e.rule.AssetOpDeadlineSeconds = 59 }, "asset_op_deadline_seconds"},
		{"截止时长超过一天", func(e *economyTables) { e.rule.AssetOpDeadlineSeconds = 86401 }, "asset_op_deadline_seconds"},
		{"重投基准 99 毫秒", func(e *economyTables) { e.rule.AssetOpRetryBaseMs = 99 }, "asset_op_retry_base_ms"},
		{"重投基准超过 60 秒", func(e *economyTables) { e.rule.AssetOpRetryBaseMs = 60001 }, "asset_op_retry_base_ms"},
		{"升级花费超过 1e12", func(e *economyTables) { e.levels[0].UpgradeCostFunds = maxUpgradeCostFunds + 1 }, "upgrade_cost_funds"},

		// GuildDonate。
		{"捐献有空行", func(e *economyTables) { e.donates = append(e.donates, nil) }, "GuildDonate"},
		{"捐献 id 为 0", func(e *economyTables) { e.donates[0].Id = 0 }, "GuildDonate"},
		{"捐献 id 重复", func(e *economyTables) { e.donates[1].Id = e.donates[0].Id }, "GuildDonate.id"},
		{"绑定灵石不可捐", func(e *economyTables) { e.donates[2].CurrencyType = 2 }, "currency_type"},
		{"扣款为 0", func(e *economyTables) { e.donates[0].CostAmount = 0 }, "cost_amount"},
		{"扣款超过 MaxInt64", func(e *economyTables) { e.donates[0].CostAmount = 1 << 63 }, "cost_amount"},
		{"两项收益都为 0", func(e *economyTables) {
			e.donates[0].ContributionGain, e.donates[0].FundsGain = 0, 0
		}, "contribution_gain"},
		{"每日次数为 0", func(e *economyTables) { e.donates[0].DailyLimit = 0 }, "daily_limit"},
		{"捐献等级为 0", func(e *economyTables) { e.donates[0].MinGuildLevel = 0 }, "min_guild_level"},
		{"捐献等级超过最高级", func(e *economyTables) { e.donates[0].MinGuildLevel = 11 }, "min_guild_level"},
		{"同一货币三行", func(e *economyTables) {
			e.donates = append(e.donates, &tablepb.GuildDonateTable{
				Id: 4, CurrencyType: 0, CostAmount: 1, ContributionGain: 1, DailyLimit: 1, MinGuildLevel: 1,
			})
		}, "currency_type=0"},

		// GuildShop。
		{"商品有空行", func(e *economyTables) { e.shops = append(e.shops, nil) }, "GuildShop"},
		{"商品 id 为 0", func(e *economyTables) { e.shops[0].Id = 0 }, "GuildShop"},
		{"商品 id 重复", func(e *economyTables) { e.shops[1].Id = e.shops[0].Id }, "GuildShop.id"},
		{"分类非法", func(e *economyTables) { e.shops[0].Category = 4 }, "category"},
		{"物品不存在", func(e *economyTables) { e.shops[0].ItemId = 999999 }, "item_id"},
		{"每份数量为 0", func(e *economyTables) { e.shops[0].ItemCount = 0 }, "item_count"},
		{"每份数量超过堆叠", func(e *economyTables) { e.shops[0].ItemCount = 1000 }, "item_count"},
		{"堆叠 1 的物品每份 2 个", func(e *economyTables) { e.shops[5].ItemCount = 2 }, "item_count"},
		{"帮贡花费为 0", func(e *economyTables) { e.shops[0].CostContribution = 0 }, "cost_contribution"},
		{"帮贡花费超过 1e9", func(e *economyTables) { e.shops[0].CostContribution = maxShopCostContribution + 1 }, "cost_contribution"},
		{"商品等级为 0", func(e *economyTables) { e.shops[0].RequiredGuildLevel = 0 }, "required_guild_level"},
		// X-04:GuildLevel 有 10 级,越界例从旧稿的 6 改为 11。
		{"商品等级 11", func(e *economyTables) { e.shops[7].RequiredGuildLevel = 11 }, "required_guild_level"},
		{"限购周期非法", func(e *economyTables) { e.shops[0].LimitPeriod = 3 }, "limit_period"},
		{"不限购却填了份数", func(e *economyTables) { e.shops[10].LimitCount = 5 }, "limit_count"},
		{"限购却填 0 份", func(e *economyTables) { e.shops[0].LimitCount = 0 }, "limit_count"},
		{"缺节庆分类", func(e *economyTables) { e.shops = e.shops[:8] }, "category=3"},
		{"缺物品表查询", func(e *economyTables) { e.itemMaxStack = nil }, "Item"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables := legalEconomyTables()
			tc.mutate(&tables)

			err := validateEconomyTables(tables)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantContains)
		})
	}
}

// TestValidateAssetOpTiming:配置与配表各自合法、组合起来会出错的两条交叉约束。
// 反例来自评审:LeaseMs=120000、截止 60s 两边都合法,同步投递被跳过的捐献会在循环第一次领到它之前就过截止,
// 一次扣款都没尝试就被中止。
func TestValidateAssetOpTiming(t *testing.T) {
	const (
		defaultLease      = 10 * time.Second
		defaultInterval   = 2 * time.Second
		defaultMaxBackoff = 60 * time.Second
	)
	rule := func(deadlineSeconds, retryBaseMs uint32) *tablepb.GuildRuleTable {
		r := legalGuildRule()
		r.AssetOpDeadlineSeconds, r.AssetOpRetryBaseMs = deadlineSeconds, retryBaseMs
		return r
	}

	t.Run("默认配置与默认配表", func(t *testing.T) {
		assert.NoError(t, validateAssetOpTiming(legalGuildRule(), defaultLease, defaultInterval, defaultMaxBackoff))
	})
	t.Run("截止取配表下限 60s 仍接受默认配置", func(t *testing.T) {
		assert.NoError(t, validateAssetOpTiming(rule(60, 1000), defaultLease, defaultInterval, defaultMaxBackoff))
	})
	t.Run("租约 120s 对截止 60s", func(t *testing.T) {
		err := validateAssetOpTiming(rule(60, 1000), 120*time.Second, defaultInterval, defaultMaxBackoff)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "asset_op_deadline_seconds")
	})
	t.Run("租约加一个对账间隔恰好等于截止", func(t *testing.T) {
		err := validateAssetOpTiming(rule(60, 1000), 58*time.Second, defaultInterval, defaultMaxBackoff)
		require.Error(t, err, "循环第一次领到它时恰好到截止,rpcFor 按 nowMs >= deadline 改发中止")
	})
	t.Run("退避基数大于封顶", func(t *testing.T) {
		err := validateAssetOpTiming(rule(600, 5000), defaultLease, defaultInterval, 2*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "asset_op_retry_base_ms")
	})
	t.Run("退避基数等于封顶", func(t *testing.T) {
		assert.NoError(t, validateAssetOpTiming(rule(600, 2000), defaultLease, defaultInterval, 2*time.Second))
	})
}

// TestMaxBuyCount 钉住单次上限的公式:堆叠 1 → 1;否则 min(20, 堆叠 / 每份数量)。
// 协议把它随视图下发,客户端据此限制份数输入,公式一变客户端与服务端就对不上。
func TestMaxBuyCount(t *testing.T) {
	row := func(itemCount uint32) *tablepb.GuildShopTable {
		return &tablepb.GuildShopTable{Id: 1, ItemCount: itemCount}
	}
	cases := []struct {
		name      string
		row       *tablepb.GuildShopTable
		maxStack  uint32
		wantCount uint32
	}{
		{"培元丹:999 / 5 被 20 封顶", row(5), 999, constants.MaxShopBuyCount},
		{"灵兽口粮:999 / 10 被 20 封顶", row(10), 999, constants.MaxShopBuyCount},
		{"堆叠 1 恒为 1", row(1), 1, 1},
		{"堆叠 30 每份 10 → 3", row(10), 30, 3},
		{"恰好 20", row(5), 100, 20},
		{"每份数量超过堆叠 → 0", row(20), 10, 0},
		{"每份数量为 0 → 0", row(0), 999, 0},
		{"堆叠为 0 → 0", row(5), 0, 0},
		{"行为空 → 0", nil, 999, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantCount, MaxBuyCount(tc.row, tc.maxStack))
		})
	}
}

// TestDefaultShopRowsBuyCounts 对照 05 §5.10 表格最后一列"单次上限(算出)"。
func TestDefaultShopRowsBuyCounts(t *testing.T) {
	want := map[uint32]uint32{101: 20, 102: 20, 103: 20, 104: 20, 201: 20, 202: 1, 203: 20, 204: 1, 301: 20, 302: 20, 303: 20}
	stacks := legalItemStacks()
	for _, row := range legalShops() {
		assert.Equal(t, want[row.GetId()], MaxBuyCount(row, stacks[row.GetItemId()]), "GuildShop[%d]", row.GetId())
	}
}
