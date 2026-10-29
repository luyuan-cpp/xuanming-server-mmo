package main

// equip-forge-topup 模式:用 GM 指令给一个开发账号补强化石 / 宝石 / 银两,然后退出(docs/design/equipment-enhance-inlay.md §11.6)。
//
// 为什么要有它:游戏里没有强化石、宝石的产出,客户端也没有 GM 发物入口;equip-forge-smoke 每次强化前只「缺多少补多少」,
// 跑完之后账号上的强化石是 0。想在真实客户端里点一次强化 / 镶嵌,得先用它把东西补进人物背包。
//
// 它不是冒烟:只发东西,不强化、不镶嵌、不卸装备、不改别的。GM 指令被拒(GM 闸关着、Item 表没导这一行……)当场判失败;
// 发完再重新读一次人物背包,核对每种物品与银两确实多了这么多,核对不上同样判失败,免得以为补上了、进游戏才发现按钮还是灰的。
//
// 结果约定(供外层脚本消费):
//   全部发到 → 日志一行 `EQUIP_FORGE_TOPUP_OK …`,退出码 0;
//   任一步失败 → `EQUIP_FORGE_TOPUP_FAIL step=… reason=…`,退出码 1。

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"proto/scene"
	"robot/config"
	"robot/metrics"
)

func RunEquipForgeTopup(cfg *config.Config) {
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	topup := cfg.EquipForgeTopup

	var session *equipSmokeSession
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("EQUIP_FORGE_TOPUP_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		session.close() // nil 安全
		_ = zap.L().Sync()
		os.Exit(1)
	}
	// LoadConfig 已经校验过;这里再拦一次,免得有人绕过 LoadConfig 直接调。
	if err := topup.Validate(); err != nil {
		fail("config", "%v", err)
	}

	opened, err := openEquipSessionAs(cfg, stats, topup.Account)
	if err != nil {
		fail("login", "%v", err)
	}
	session = opened
	zap.L().Info("[equip-forge-topup] in scene", zap.String("account", topup.Account), zap.Uint64("player", session.gc.PlayerId))

	before, err := session.getBag(equipSmokeBagInventory)
	if err != nil {
		fail("read-before", "%v", err)
	}
	for _, item := range topup.Items {
		if _, err := equipForgeGrant(session, item.ConfigId, uint64(item.Count)); err != nil {
			fail("grant", "%v", err)
		}
	}
	if topup.Gold > 0 {
		if err := equipForgeAddGold(session, topup.Gold); err != nil {
			fail("gold", "%v", err)
		}
	}
	after, err := session.getBag(equipSmokeBagInventory)
	if err != nil {
		fail("read-after", "%v", err)
	}
	// GM 闸拒绝(gate 信封 / scene 响应体)与 Item 表缺行(kEquipGrantInvalid)在 grant / gold 两步就已判红;
	// 走到这里是指令都回了成功,背包却没多出这么多
	if problems := equipForgeTopupShortfalls(before, after, topup); len(problems) != 0 {
		fail("verify", "GM 指令回了成功,人物背包却没有全部到账(scene 发物 / 加银两没落到背包?): %s",
			strings.Join(problems, "; "))
	}

	zap.L().Info("EQUIP_FORGE_TOPUP_OK " + equipForgeTopupSummary(before, after, topup))
	session.close()
	_ = zap.L().Sync()
}

// openEquipSessionAs 取 gate、用指定账号登录进场(账号没有角色时自动建角),返回新会话。
// openEquipForgeSession 是它的特例(账号固定为 equipForgeAccount)。
func openEquipSessionAs(cfg *config.Config, stats *metrics.Stats, account string) (*equipSmokeSession, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate: %w", err)
	}
	port, _ := strconv.Atoi(portStr)
	gc, player, err := prepareBehaviorClient(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("account=%s: %w", account, err)
	}
	return &equipSmokeSession{gc: gc, player: player, stats: stats, lastSent: make(map[uint32]time.Time)}, nil
}

// equipForgeTopupWanted 把配置里的物品按 config 汇总(同一个 config 可以写多项)。
func equipForgeTopupWanted(topup config.EquipForgeTopupConfig) map[uint32]uint64 {
	want := make(map[uint32]uint64, len(topup.Items))
	for _, item := range topup.Items {
		want[item.ConfigId] += uint64(item.Count)
	}
	return want
}

// equipForgeTopupShortfalls 核对发完之后的人物背包:每种物品至少多了配置的数量、银两至少多了配置的金额。
// 只要求「至少」:同一账号别处同时在加东西不算错;少了就是 GM 指令没生效。返回按 config 升序的问题列表,空 = 都发到了。
func equipForgeTopupShortfalls(before, after *scene.BagInfo, topup config.EquipForgeTopupConfig) []string {
	var problems []string
	want := equipForgeTopupWanted(topup)
	ids := make([]uint32, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		had, has := equipForgeConfigCount(before, id), equipForgeConfigCount(after, id)
		if has < had || has-had < want[id] {
			problems = append(problems, fmt.Sprintf("config_id=%d 应多 %d 个,实际 %d → %d", id, want[id], had, has))
		}
	}
	if topup.Gold > 0 {
		had, has := equipForgeGold(before), equipForgeGold(after)
		if has < had || has-had < uint64(topup.Gold) {
			problems = append(problems, fmt.Sprintf("银两应多 %d,实际 %d → %d", topup.Gold, had, has))
		}
	}
	return problems
}

// equipForgeTopupSummary 是 OK 那一行的内容:账号、银两前后、每种物品前后(config 升序)。
func equipForgeTopupSummary(before, after *scene.BagInfo, topup config.EquipForgeTopupConfig) string {
	parts := []string{
		"account=" + topup.Account,
		fmt.Sprintf("gold=%d->%d", equipForgeGold(before), equipForgeGold(after)),
	}
	want := equipForgeTopupWanted(topup)
	ids := make([]uint32, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("item_%d=%d->%d", id, equipForgeConfigCount(before, id), equipForgeConfigCount(after, id)))
	}
	return strings.Join(parts, " ")
}
