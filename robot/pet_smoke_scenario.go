package main

// pet-smoke 场景:单机器人对本地服务端做「宝宝(宠物)系统」端到端冒烟。
//
// 兑现 docs/design/player-pet.md 的服务端契约,逐条断言:
//   0. 预备(让脚本可重复运行):等级归 1、GM 发金币、拿一只可用的宝宝
//      (槽位没满就 GmGrantPet 发一只新的,满了就复用列表里的第一只);
//   1. GetPetList 拿到全量列表(宝宝 / 四个维度 / 二级属性 / 资质 / 成长率都非空);
//   2. 池隔离:角色面板里**不能**出现宝宝池,宝宝列表里的维度**必须**全属于宝宝池
//      (owner_type 分流,设计文档 §2);
//   3. GmSetPlayerLevel 1 → 30:宝宝等级跟着涨到 30(等级派生自主人,§3.2),
//      宝宝属性点总量恰好 +145(每级 5 点,总量不落库、按等级换算);
//   4. 洗点回到干净起点:remaining == total、四维已分配全 0;
//   5. AutoAllocatePetPoints 只算不落 —— 列表不变,建议里的增量总和 = 剩余点;
//   6. AllocatePetPoints 按建议提交 → 剩余点归零、二级属性真实变大;
//   7. 幂等:重发同一份"目标值"被判"无变化"(kPetNothingToChange),不重复扣点;
//   8. 只增不减:提交比已分配更小的值被拒(kPetPointsCannotDecrease);
//   9. 不存在的 pet_id 被拒(kPetNotFound);
//  10. SummonPet → active_pet_id 指向它;重复召唤同一只被拒(kPetAlreadyActive);
//  11. 下线重登:宝宝数量 / 出战状态 / 等级 / 已分配 / 资质 / 二级属性与下线前一致
//      (pet_component 落库往返 + db 列迁移);
//  12. RecallPet → active_pet_id 归零;没有出战宝宝时再收回被拒(kPetNotActive)。
//
// 结果约定(供外层脚本消费):
//   全过 → 日志一行 `PET_SMOKE_OK …`,退出码 0;
//   任一步失败 → `PET_SMOKE_FAIL step=… reason=…`,退出码 1。

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧的 DevPasswordAuth(开发用密码认证)。
// 9102 避开压测区段、battle-smoke 的 9001/9002 与 attribute-smoke 的 9101。
const petSmokeAccount = "robot_9102"

// 单次宝宝 RPC 的等待预算。宝宝操作是纯内存 + 表查询,正常 <50ms;
// 给 10s 是覆盖 gate→scene 首次路由建立。
const petSmokeRpcTimeout = 10 * time.Second

// 预备阶段发的金币:覆盖 30 级洗点(300)+ 改名(200)再留余量。
const petSmokeGoldGrant int64 = 100000

// 与 data/Pet.xlsx 对齐:1 = 灵狐(1 级即可携带)。
const petSmokeTableId uint32 = 1

// 宝宝属性点池 id,与 data/AttributePool.xlsx 的 owner_type=1 行对齐。
const petPoolId uint32 = 4

// AttributePool 行 4 的 points_per_level(表值;冒烟用它算 1→30 级应得的精确点数)。
const petPointsPerLevel uint32 = 5

// tip id 一律读导表器生成的枚举,**不写字面量**(AGENTS.md §7 不变量 5)。
// 手抄数字的下场已经有过一次:tip 码轴 2026 年改成按段发号后,attribute-smoke 里那组
// 130-144 的常量全部失效,负向断言永远对不上且零报错。
var (
	tipPetNotFound             = uint32(tiptable.PetError_kPetNotFound)
	tipPetSlotFull             = uint32(tiptable.PetError_kPetSlotFull)
	tipPetAlreadyActive        = uint32(tiptable.PetError_kPetAlreadyActive)
	tipPetNotActive            = uint32(tiptable.PetError_kPetNotActive)
	tipPetPointsCannotDecrease = uint32(tiptable.PetError_kPetPointsCannotDecrease)
	tipPetNothingToChange      = uint32(tiptable.PetError_kPetNothingToChange)
)

// petSmokeSession 是一个已登录进场的机器人会话 + 列表序号游标。
type petSmokeSession struct {
	gc     *pkg.GameClient
	player *gameobject.Player
	stats  *metrics.Stats
	seq    uint64 // 已消费到的列表序号,保证每次等的是"这次请求的新列表"
}

// RunPetSmoke 是 main.go `mode: pet-smoke` 的入口。
func RunPetSmoke(cfg *config.Config) {
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var session *petSmokeSession
	cleanup := func() {
		if session != nil && session.gc != nil {
			_ = leaveGame(session.gc, stats)
			sendDisconnectBestEffort(session.gc)
			gameobject.PlayerList.Delete(session.gc.PlayerId)
			session.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("PET_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 步骤 1:登录进场 ----
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		fail("resolve-gate", "%v", err)
	}
	port, _ := strconv.Atoi(portStr)
	gc, player, err := prepareBehaviorClient(host, port, petSmokeAccount, cfg.Password, stats, tokenPayload, tokenSig)
	if err != nil {
		fail("login", "account=%s err=%v", petSmokeAccount, err)
	}
	session = &petSmokeSession{gc: gc, player: player, stats: stats}
	zap.L().Info("[pet-smoke] step 1: in scene", zap.Uint64("player", gc.PlayerId))

	// ---- 步骤 0:预备,把账号拉回已知起点(同账号可反复跑) ----
	// 等级归 1:后面第 3 步要断言 1→30 的点数增量,必须从 1 级起跳
	if _, err := session.callAttributePanel(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: 1}); err != nil {
		fail("prep-level", "%v", err)
	}
	if _, err := gmAddGold(gc, player, stats, petSmokeGoldGrant, petSmokeRpcTimeout); err != nil {
		fail("prep-gold", "%v", err)
	}

	// 上一轮若在「出战」之后中途失败,账号会带着出战状态进来,第 10 步的 SummonPet 会被
	// kPetAlreadyActive 直接拒掉、整条冒烟从此在这个账号上永久红。无条件先收回一次
	// (本来就没出战时服务器回 kPetNotActive,容忍掉)。
	if _, _, err := session.callTolerant(game.ScenePetClientPlayerRecallPetMessageId,
		&scene.RecallPetRequest{}, tipPetNotActive); err != nil {
		fail("prep-recall", "%v", err)
	}

	list, err := session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{})
	if err != nil {
		fail("prep-list", "%v", err)
	}
	// max_pets 来自 PetRule 表。表没导出时服务端会退回 1,槽位判断随之失真:
	// 已有 1 只宝宝就永远走「复用」分支,GmGrantPet 那条断言被静默跳过、冒烟照样打 OK。
	// 这里显式拦一道,让"表没跑导出"变成红,而不是变成一条更弱的绿。
	if list.MaxPets == 0 {
		fail("prep-max-pets", "服务端下发 max_pets=0,PetRule 表多半没导出(先跑 dev.bat gen)")
	}
	// 核心线没有"放生",反复跑会一直加宝宝;槽位满了就复用既有的那只,
	// 让脚本在同一账号上永远可重复(设计文档 §8 缺口 4)。
	target := uint64(0)
	if uint32(len(list.Pets)) < list.MaxPets {
		granted, tip, err := session.callTolerant(game.ScenePetClientPlayerGmGrantPetMessageId,
			&scene.GmGrantPetRequest{PetTableId: petSmokeTableId}, tipPetSlotFull)
		if err != nil {
			fail("prep-grant", "%v", err)
		}
		if tip == 0 {
			list = granted
			target = player.GetPetGranted()
			if target == 0 {
				fail("prep-grant-id", "GmGrantPet 成功但没回 pet_id")
			}
			if findPet(list, target) == nil {
				fail("prep-grant-list", "新发的 pet_id=%d 不在返回的列表里", target)
			}
		}
	}
	if target == 0 {
		if len(list.Pets) == 0 {
			fail("prep-no-pet", "槽位满但列表为空(max_pets=%d),服务端状态不自洽", list.MaxPets)
		}
		target = list.Pets[0].PetId
		zap.L().Warn("[pet-smoke] 槽位已满,复用既有宝宝(该账号已跑过多次)",
			zap.Uint64("pet_id", target), zap.Int("pets", len(list.Pets)))
	}
	zap.L().Info("[pet-smoke] step 0: prepared", zap.Uint64("pet_id", target),
		zap.Int("pets", len(list.Pets)), zap.Uint32("max_pets", list.MaxPets))

	// ---- 步骤 2:列表形状 + 池隔离 ----
	list, err = session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{})
	if err != nil {
		fail("get-list", "%v", err)
	}
	pet := findPet(list, target)
	if pet == nil {
		fail("list-shape", "列表里找不到 pet_id=%d", target)
	}
	if len(pet.Dimensions) == 0 {
		fail("list-dimensions", "宝宝没有任何维度(表 AttributeDimension 缺 owner_type=1 的行?)")
	}
	if pet.Derived == nil || pet.Derived.MaxHealth == 0 {
		fail("list-derived", "二级属性缺失或气血上限为 0:%v", pet.Derived)
	}
	if pet.Growth == 0 {
		fail("list-growth", "成长率为 0(应为四维资质均值,万分比)")
	}
	for _, dimension := range pet.Dimensions {
		if dimension.PoolId != petPoolId {
			fail("pool-isolation-pet", "宝宝维度 %d 属于池 %d,应全部属于宝宝池 %d",
				dimension.DimensionId, dimension.PoolId, petPoolId)
		}
		if dimension.Aptitude == 0 {
			fail("list-aptitude", "维度 %d 资质为 0(缺省应为 10000)", dimension.DimensionId)
		}
	}
	// 反向:角色面板里绝不能出现宝宝池 —— 这是 owner_type 分流的核心断言
	charPanel, err := session.callAttributePanel(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{})
	if err != nil {
		fail("char-panel", "%v", err)
	}
	for _, pool := range charPanel.Pools {
		if pool.PoolId == petPoolId {
			fail("pool-isolation-char", "角色面板里出现了宝宝池 %d(owner_type 分流失效)", petPoolId)
		}
	}
	for _, dimension := range charPanel.Dimensions {
		if dimension.PoolId == petPoolId {
			fail("pool-isolation-char-dim", "角色面板里出现了宝宝维度 %d", dimension.DimensionId)
		}
	}
	zap.L().Info("[pet-smoke] step 2: list + pool isolation ok",
		zap.Int("dimensions", len(pet.Dimensions)), zap.Uint32("growth", pet.Growth),
		zap.Uint64("max_health", pet.Derived.MaxHealth))

	// ---- 步骤 3:主人 1 → 30 级,宝宝等级跟随、点数精确 +145 ----
	const targetLevel = 30
	beforeTotal := pet.TotalPoints
	beforeLevel := pet.Level
	if _, err := session.callAttributePanel(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: targetLevel}); err != nil {
		fail("gm-set-level", "%v", err)
	}
	list, err = session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{})
	if err != nil {
		fail("list-after-level", "%v", err)
	}
	pet = findPet(list, target)
	if pet == nil {
		fail("list-after-level", "升级后列表里找不到 pet_id=%d", target)
	}
	if pet.Level != targetLevel {
		fail("pet-level", "主人 %d 级时宝宝应同为 %d 级(等级派生),实际 %d",
			targetLevel, targetLevel, pet.Level)
	}
	if want := petPointsPerLevel * (targetLevel - beforeLevel); pet.TotalPoints-beforeTotal != want {
		fail("pet-points", "宝宝 %d→%d 级点数应 +%d,实际 %d→%d",
			beforeLevel, targetLevel, want, beforeTotal, pet.TotalPoints)
	}
	zap.L().Info("[pet-smoke] step 3: level follows owner",
		zap.Uint32("pet_level", pet.Level), zap.Uint32("total_points", pet.TotalPoints))

	// ---- 步骤 4:洗点回到干净起点 ----
	if reset, tip, err := session.callTolerant(game.ScenePetClientPlayerResetPetPointsMessageId,
		&scene.ResetPetPointsRequest{PetId: target}, tipPetNothingToChange); err != nil {
		fail("reset", "%v", err)
	} else if tip == 0 {
		list = reset
	}
	pet = mustPet(list, target, fail, "reset")
	if pet.RemainingPoints != pet.TotalPoints {
		fail("reset-clean", "洗点后应全额可用:total=%d remaining=%d", pet.TotalPoints, pet.RemainingPoints)
	}
	for _, dimension := range pet.Dimensions {
		if dimension.Allocated != 0 {
			fail("reset-allocated", "洗点后维度 %d 仍有 %d 点", dimension.DimensionId, dimension.Allocated)
		}
	}
	healthBefore := pet.Derived.MaxHealth
	attackBefore := pet.Derived.PhysicalAttack

	// ---- 步骤 5:自动加点只算不落 ----
	if err := session.send(game.ScenePetClientPlayerAutoAllocatePetPointsMessageId,
		&scene.AutoAllocatePetPointsRequest{PetId: target}); err != nil {
		fail("auto-send", "%v", err)
	}
	suggested, err := session.waitSuggestion(target)
	if err != nil {
		fail("auto-wait", "%v", err)
	}
	var suggestedSum uint32
	for _, value := range suggested {
		suggestedSum += value
	}
	if suggestedSum != pet.TotalPoints {
		fail("auto-sum", "建议总和应等于总点数(洗点后已分配为 0):suggested=%d total=%d",
			suggestedSum, pet.TotalPoints)
	}
	after, err := session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{})
	if err != nil {
		fail("auto-recheck", "%v", err)
	}
	if p := mustPet(after, target, fail, "auto-recheck"); p.RemainingPoints != pet.TotalPoints {
		fail("auto-not-applied", "自动加点只算不落,剩余点不该变:%d → %d",
			pet.TotalPoints, p.RemainingPoints)
	}
	zap.L().Info("[pet-smoke] step 5: auto suggestion computed only",
		zap.Uint32("sum", suggestedSum), zap.Int("dimensions", len(suggested)))

	// ---- 步骤 6:按建议提交 → 剩余归零、二级属性变大 ----
	list, err = session.call(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target, Allocated: suggested})
	if err != nil {
		fail("allocate", "%v", err)
	}
	pet = mustPet(list, target, fail, "allocate")
	if pet.RemainingPoints != 0 {
		fail("allocate-remaining", "按建议全额提交后剩余点应为 0,实际 %d", pet.RemainingPoints)
	}
	if pet.Derived.MaxHealth <= healthBefore && pet.Derived.PhysicalAttack <= attackBefore {
		fail("allocate-derived", "加点后二级属性没有变大:health %d→%d attack %d→%d",
			healthBefore, pet.Derived.MaxHealth, attackBefore, pet.Derived.PhysicalAttack)
	}
	zap.L().Info("[pet-smoke] step 6: allocated",
		zap.Uint64("max_health", pet.Derived.MaxHealth),
		zap.Uint64("physical_attack", pet.Derived.PhysicalAttack))

	// ---- 步骤 7:幂等 —— 重发同一份目标值被判"无变化" ----
	if tip := session.callExpectTip(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target, Allocated: suggested}); tip != tipPetNothingToChange {
		fail("allocate-idempotent", "重发同一份目标值应回 kPetNothingToChange(%d),实际 tip=%d",
			tipPetNothingToChange, tip)
	}

	// ---- 步骤 8:只增不减 ----
	lower := map[uint32]uint32{}
	for dimensionId, value := range suggested {
		if value > 0 {
			lower[dimensionId] = value - 1
			break
		}
	}
	if len(lower) == 0 {
		fail("decrease-setup", "建议里没有任何一个大于 0 的维度,无法构造更小的目标值")
	}
	if tip := session.callExpectTip(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target, Allocated: lower}); tip != tipPetPointsCannotDecrease {
		fail("allocate-decrease", "提交更小的值应回 kPetPointsCannotDecrease(%d),实际 tip=%d",
			tipPetPointsCannotDecrease, tip)
	}

	// ---- 步骤 9:不存在的 pet_id ----
	if tip := session.callExpectTip(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target + 999999, Allocated: suggested}); tip != tipPetNotFound {
		fail("unknown-pet", "不存在的 pet_id 应回 kPetNotFound(%d),实际 tip=%d", tipPetNotFound, tip)
	}
	zap.L().Info("[pet-smoke] steps 7-9: idempotent / no-decrease / unknown-pet rejections ok")

	// ---- 步骤 10:出战 ----
	list, err = session.call(game.ScenePetClientPlayerSummonPetMessageId,
		&scene.SummonPetRequest{PetId: target})
	if err != nil {
		fail("summon", "%v", err)
	}
	if list.ActivePetId != target {
		fail("summon-active", "出战后 active_pet_id 应为 %d,实际 %d", target, list.ActivePetId)
	}
	if p := mustPet(list, target, fail, "summon"); !p.IsActive {
		fail("summon-flag", "出战后 is_active 应为 true")
	}
	if tip := session.callExpectTip(game.ScenePetClientPlayerSummonPetMessageId,
		&scene.SummonPetRequest{PetId: target}); tip != tipPetAlreadyActive {
		fail("summon-twice", "重复召唤应回 kPetAlreadyActive(%d),实际 tip=%d", tipPetAlreadyActive, tip)
	}

	// ---- 步骤 11:下线重登,数据往返 ----
	snapshotBefore := mustPet(list, target, fail, "before-relogin")
	newSession, err := session.relogin(cfg)
	if err != nil {
		fail("relogin", "%v", err)
	}
	session = newSession
	list, err = session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{})
	if err != nil {
		fail("list-after-relogin", "%v", err)
	}
	if list.ActivePetId != target {
		fail("relogin-active", "重登后出战状态丢失:active_pet_id=%d", list.ActivePetId)
	}
	restored := mustPet(list, target, fail, "after-relogin")
	if restored.Level != snapshotBefore.Level ||
		restored.RemainingPoints != snapshotBefore.RemainingPoints ||
		restored.Growth != snapshotBefore.Growth ||
		restored.Derived.MaxHealth != snapshotBefore.Derived.MaxHealth ||
		restored.Derived.PhysicalAttack != snapshotBefore.Derived.PhysicalAttack {
		fail("relogin-mismatch",
			"重登前后不一致: level %d/%d remaining %d/%d growth %d/%d health %d/%d attack %d/%d",
			snapshotBefore.Level, restored.Level,
			snapshotBefore.RemainingPoints, restored.RemainingPoints,
			snapshotBefore.Growth, restored.Growth,
			snapshotBefore.Derived.MaxHealth, restored.Derived.MaxHealth,
			snapshotBefore.Derived.PhysicalAttack, restored.Derived.PhysicalAttack)
	}
	for _, dimension := range restored.Dimensions {
		before := allocatedOfPet(snapshotBefore, dimension.DimensionId)
		if dimension.Allocated != before {
			fail("relogin-allocated", "维度 %d 已分配点重登后 %d != %d",
				dimension.DimensionId, dimension.Allocated, before)
		}
		if dimension.Aptitude != aptitudeOfPet(snapshotBefore, dimension.DimensionId) {
			fail("relogin-aptitude", "维度 %d 资质重登后变了(资质出生即定终身)", dimension.DimensionId)
		}
	}
	zap.L().Info("[pet-smoke] step 11: persisted across relogin",
		zap.Uint32("level", restored.Level), zap.Uint32("growth", restored.Growth))

	// ---- 步骤 12:收回 ----
	list, err = session.call(game.ScenePetClientPlayerRecallPetMessageId, &scene.RecallPetRequest{})
	if err != nil {
		fail("recall", "%v", err)
	}
	if list.ActivePetId != 0 {
		fail("recall-active", "收回后 active_pet_id 应为 0,实际 %d", list.ActivePetId)
	}
	if tip := session.callExpectTip(game.ScenePetClientPlayerRecallPetMessageId,
		&scene.RecallPetRequest{}); tip != tipPetNotActive {
		fail("recall-twice", "无出战宝宝时再收回应回 kPetNotActive(%d),实际 tip=%d", tipPetNotActive, tip)
	}

	zap.L().Info(fmt.Sprintf(
		"PET_SMOKE_OK player=%d pet_id=%d pets=%d level=%d growth=%d max_health=%d",
		session.gc.PlayerId, target, len(list.Pets), restored.Level, restored.Growth,
		restored.Derived.MaxHealth))
	cleanup()
	_ = zap.L().Sync()
}

// ---------------------------------------------------------------------------
// 会话辅助
// ---------------------------------------------------------------------------

// call 发一个宝宝 RPC 并等它带回的新列表(成功路径);tip != 0 视为失败。
func (s *petSmokeSession) call(messageId uint32, request proto.Message) (*scene.PetListInfo, error) {
	list, tip, err := s.callTolerant(messageId, request)
	if err != nil {
		return nil, err
	}
	if tip != 0 {
		return nil, fmt.Errorf("服务器拒绝: tip=%d", tip)
	}
	return list, nil
}

// callTolerant 发一个宝宝 RPC:成功回 (列表, 0, nil);被拒且 tip 在 allowedTips 里回 (nil, tip, nil);
// 其它拒绝回 error。只认"来源 = 本次请求消息号"的列表 —— 服务器会在升级等路径主动推
// NotifyPetListChanged,不看来源就会把推送当成本次响应消费,之后每一步都拿到旧列表。
func (s *petSmokeSession) callTolerant(messageId uint32, request proto.Message, allowedTips ...uint32) (*scene.PetListInfo, uint32, error) {
	if err := s.send(messageId, request); err != nil {
		return nil, 0, err
	}
	deadline := time.Now().Add(petSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if tip := s.player.GetPetLastTip(); tip != 0 {
			for _, allowed := range allowedTips {
				if tip == allowed {
					return nil, tip, nil
				}
			}
			return nil, tip, fmt.Errorf("服务器拒绝: tip=%d", tip)
		}
		list, seq := s.player.GetPetList()
		if list != nil && seq > s.seq && s.player.GetPetListSource() == messageId {
			s.seq = seq
			return list, 0, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, 0, fmt.Errorf("等响应超时 message_id=%d", messageId)
}

// callExpectTip 发一个预期会被拒绝的请求,返回服务器给的 tip id(0 = 没被拒或超时,均视为断言失败)。
func (s *petSmokeSession) callExpectTip(messageId uint32, request proto.Message) uint32 {
	_, tip, _ := s.callTolerant(messageId, request, 0xFFFFFFFF)
	return tip
}

// callAttributePanel 借用角色属性通道(改等级 / 查角色面板)。宝宝的等级来自主人,
// 所以这条冒烟必须同时驱动两套协议。
func (s *petSmokeSession) callAttributePanel(messageId uint32, request proto.Message) (*scene.AttributePanelInfo, error) {
	s.player.SetAttributePanel(nil, 0, 0)
	_, cur := s.player.GetAttributePanel()
	if err := s.gc.SendRequest(messageId, request); err != nil {
		return nil, fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	s.stats.MsgSent()
	deadline := time.Now().Add(petSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if tip := s.player.GetAttributeLastTip(); tip != 0 {
			return nil, fmt.Errorf("服务器拒绝: tip=%d", tip)
		}
		panel, seq := s.player.GetAttributePanel()
		if panel != nil && seq > cur && s.player.GetAttributePanelSource() == messageId {
			return panel, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("等属性响应超时 message_id=%d", messageId)
}

func (s *petSmokeSession) send(messageId uint32, request proto.Message) error {
	// 每次发请求前清 tip / 建议,并把游标同步到当前列表序号:
	// 上一步没被消费的列表(推送 / 迟到响应)不能算作本次结果
	s.seq = s.player.ResetPetWaiters()
	if err := s.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	s.stats.MsgSent()
	return nil
}

func (s *petSmokeSession) waitSuggestion(petId uint64) (map[uint32]uint32, error) {
	deadline := time.Now().Add(petSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		gotPet, suggested := s.player.GetPetSuggestion()
		if gotPet == petId && len(suggested) > 0 {
			return suggested, nil
		}
		if tip := s.player.GetPetLastTip(); tip != 0 {
			return nil, fmt.Errorf("服务器拒绝自动加点: tip=%d", tip)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("等自动加点建议超时")
}

// relogin 下线(LeaveGame + Disconnect)后用同一账号重新登录进场,返回新会话。
// 中间等 2s 让 scene 的异步存盘落到 db,否则重登可能读到旧档
// (HandlePlayerAsyncSaved 会把过快的重登判成"重连取代登出")。
func (s *petSmokeSession) relogin(cfg *config.Config) (*petSmokeSession, error) {
	_ = leaveGame(s.gc, s.stats)
	sendDisconnectBestEffort(s.gc)
	gameobject.PlayerList.Delete(s.gc.PlayerId)
	s.gc.Close()
	time.Sleep(2 * time.Second)

	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate: %w", err)
	}
	port, _ := strconv.Atoi(portStr)
	gc, player, err := prepareBehaviorClient(host, port, petSmokeAccount, cfg.Password, s.stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("relogin: %w", err)
	}
	return &petSmokeSession{gc: gc, player: player, stats: s.stats}, nil
}

// ---------------------------------------------------------------------------
// 列表取值
// ---------------------------------------------------------------------------

func findPet(list *scene.PetListInfo, petId uint64) *scene.PetInfo {
	if list == nil {
		return nil
	}
	for _, pet := range list.Pets {
		if pet.PetId == petId {
			return pet
		}
	}
	return nil
}

// mustPet 取宝宝,取不到直接判失败(所有断言步骤都以"这只宝宝还在列表里"为前提)。
func mustPet(list *scene.PetListInfo, petId uint64, fail func(string, string, ...any), step string) *scene.PetInfo {
	pet := findPet(list, petId)
	if pet == nil {
		fail(step, "列表里找不到 pet_id=%d", petId)
	}
	return pet
}

func allocatedOfPet(pet *scene.PetInfo, dimensionId uint32) uint32 {
	if pet == nil {
		return 0
	}
	for _, dimension := range pet.Dimensions {
		if dimension.DimensionId == dimensionId {
			return dimension.Allocated
		}
	}
	return 0
}

func aptitudeOfPet(pet *scene.PetInfo, dimensionId uint32) uint32 {
	if pet == nil {
		return 0
	}
	for _, dimension := range pet.Dimensions {
		if dimension.DimensionId == dimensionId {
			return dimension.Aptitude
		}
	}
	return 0
}
