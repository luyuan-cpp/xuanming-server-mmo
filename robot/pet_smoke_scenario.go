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
	"slices"
	"strconv"
	"time"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

	"proto/common/base"
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

// petSmokeSession 是一个已登录进场的机器人会话。宝宝与角色属性请求都经 caller 发出、只等本次请求自己的回包
// (按 MessageContent.id 认领),所以不再需要列表序号游标,也不再读 gameobject.Player 上的宝宝状态。
type petSmokeSession struct {
	gc     *pkg.GameClient
	stats  *metrics.Stats
	caller *smokeCaller
}

// newPetSmokeSession 包装一个已进场的连接。gc 的 RecvLoop 必须已启动(prepareBehaviorClient 返回前已起),
// 否则 caller 等不到回包。
func newPetSmokeSession(gc *pkg.GameClient, stats *metrics.Stats) *petSmokeSession {
	return &petSmokeSession{
		gc:     gc,
		stats:  stats,
		caller: newSmokeCaller(gc, stats, petSmokeAccount, petSmokeRpcTimeout, nil),
	}
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
	session = newPetSmokeSession(gc, stats)
	zap.L().Info("[pet-smoke] step 1: in scene", zap.Uint64("player", gc.PlayerId))

	// ---- 步骤 0:预备,把账号拉回已知起点(同账号可反复跑) ----
	// 等级归 1:后面第 3 步要断言 1→30 的点数增量,必须从 1 级起跳
	if _, err := session.callAttributePanel(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: 1}, &scene.GmSetPlayerLevelResponse{}); err != nil {
		fail("prep-level", "%v", err)
	}
	if _, err := gmAddGold(gc, player, stats, petSmokeGoldGrant, petSmokeRpcTimeout); err != nil {
		fail("prep-gold", "%v", err)
	}

	// 上一轮若在「出战」之后中途失败,账号会带着出战状态进来,第 10 步的 SummonPet 会被
	// kPetAlreadyActive 直接拒掉、整条冒烟从此在这个账号上永久红。无条件先收回一次
	// (本来就没出战时服务器回 kPetNotActive,容忍掉)。
	if _, _, err := session.callTolerant(game.ScenePetClientPlayerRecallPetMessageId,
		&scene.RecallPetRequest{}, &scene.RecallPetResponse{}, tipPetNotActive); err != nil {
		fail("prep-recall", "%v", err)
	}

	list, err := session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{},
		&scene.GetPetListResponse{})
	if err != nil {
		fail("prep-list", "%v", err)
	}
	// max_pets 来自 PetRule 表。表没导出时服务端会退回 1,槽位判断随之失真:
	// 已有 1 只宝宝就永远走「复用」分支,GmGrantPet 那条断言被静默跳过、冒烟照样打 OK。
	// 这里显式拦一道,让"表没跑导出"变成红,而不是变成一条更弱的绿。
	if list.MaxPets == 0 {
		fail("prep-max-pets", "Server returned max_pets=0; the PetRule table was probably not exported (run dev.bat gen first)")
	}
	// 核心线没有"放生",反复跑会一直加宝宝;槽位满了就复用既有的那只,
	// 让脚本在同一账号上永远可重复(设计文档 §8 缺口 4)。
	target := uint64(0)
	if uint32(len(list.Pets)) < list.MaxPets {
		grantResponse := &scene.GmGrantPetResponse{}
		granted, tip, err := session.callTolerant(game.ScenePetClientPlayerGmGrantPetMessageId,
			&scene.GmGrantPetRequest{PetTableId: petSmokeTableId}, grantResponse, tipPetSlotFull)
		if err != nil {
			fail("prep-grant", "%v", err)
		}
		if tip == 0 {
			list = granted
			target = grantResponse.GetPetId()
			if target == 0 {
				fail("prep-grant-id", "GmGrantPet succeeded but returned no pet_id")
			}
			if findPet(list, target) == nil {
				fail("prep-grant-list", "Newly granted pet_id=%d is missing from the returned list", target)
			}
		}
	}
	if target == 0 {
		if len(list.Pets) == 0 {
			fail("prep-no-pet", "Pet slots are full but the list is empty (max_pets=%d); server state is inconsistent", list.MaxPets)
		}
		target = list.Pets[0].PetId
		zap.L().Warn("[pet-smoke] Pet slots are full; reusing an existing pet (this account has been used in multiple runs)",
			zap.Uint64("pet_id", target), zap.Int("pets", len(list.Pets)))
	}
	zap.L().Info("[pet-smoke] step 0: prepared", zap.Uint64("pet_id", target),
		zap.Int("pets", len(list.Pets)), zap.Uint32("max_pets", list.MaxPets))

	// ---- 步骤 2:列表形状 + 池隔离 ----
	list, err = session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{},
		&scene.GetPetListResponse{})
	if err != nil {
		fail("get-list", "%v", err)
	}
	pet := findPet(list, target)
	if pet == nil {
		fail("list-shape", "List contains no pet_id=%d", target)
	}
	if len(pet.Dimensions) == 0 {
		fail("list-dimensions", "Pet has no dimensions (is AttributeDimension missing rows with owner_type=1?)")
	}
	if pet.Derived == nil || pet.Derived.MaxHealth == 0 {
		fail("list-derived", "Derived attributes are missing or max health is 0: %v", pet.Derived)
	}
	if pet.Growth == 0 {
		fail("list-growth", "Growth rate is 0 (expected the mean aptitude of the four dimensions, in basis points)")
	}
	for _, dimension := range pet.Dimensions {
		if dimension.PoolId != petPoolId {
			fail("pool-isolation-pet", "Pet dimension %d belongs to pool %d; all should belong to pet pool %d",
				dimension.DimensionId, dimension.PoolId, petPoolId)
		}
		if dimension.Aptitude == 0 {
			fail("list-aptitude", "Dimension %d aptitude is 0 (default should be 10000)", dimension.DimensionId)
		}
	}
	// 反向:角色面板里绝不能出现宝宝池 —— 这是 owner_type 分流的核心断言
	charPanel, err := session.callAttributePanel(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{}, &scene.GetAttributePanelResponse{})
	if err != nil {
		fail("char-panel", "%v", err)
	}
	for _, pool := range charPanel.Pools {
		if pool.PoolId == petPoolId {
			fail("pool-isolation-char", "Character panel contains pet pool %d (owner_type separation failed)", petPoolId)
		}
	}
	for _, dimension := range charPanel.Dimensions {
		if dimension.PoolId == petPoolId {
			fail("pool-isolation-char-dim", "Character panel contains pet dimension %d", dimension.DimensionId)
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
		&scene.GmSetPlayerLevelRequest{Level: targetLevel}, &scene.GmSetPlayerLevelResponse{}); err != nil {
		fail("gm-set-level", "%v", err)
	}
	list, err = session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{},
		&scene.GetPetListResponse{})
	if err != nil {
		fail("list-after-level", "%v", err)
	}
	pet = findPet(list, target)
	if pet == nil {
		fail("list-after-level", "After leveling up, list contains no pet_id=%d", target)
	}
	if pet.Level != targetLevel {
		fail("pet-level", "When owner level is %d, pet level should also be %d (derived level), got %d",
			targetLevel, targetLevel, pet.Level)
	}
	if want := petPointsPerLevel * (targetLevel - beforeLevel); pet.TotalPoints-beforeTotal != want {
		fail("pet-points", "Pet points from level %d to %d should increase by %d, got %d -> %d",
			beforeLevel, targetLevel, want, beforeTotal, pet.TotalPoints)
	}
	zap.L().Info("[pet-smoke] step 3: level follows owner",
		zap.Uint32("pet_level", pet.Level), zap.Uint32("total_points", pet.TotalPoints))

	// ---- 步骤 4:洗点回到干净起点 ----
	if reset, tip, err := session.callTolerant(game.ScenePetClientPlayerResetPetPointsMessageId,
		&scene.ResetPetPointsRequest{PetId: target}, &scene.ResetPetPointsResponse{}, tipPetNothingToChange); err != nil {
		fail("reset", "%v", err)
	} else if tip == 0 {
		list = reset
	}
	pet = mustPet(list, target, fail, "reset")
	if pet.RemainingPoints != pet.TotalPoints {
		fail("reset-clean", "All points should be available after reset: total=%d remaining=%d", pet.TotalPoints, pet.RemainingPoints)
	}
	for _, dimension := range pet.Dimensions {
		if dimension.Allocated != 0 {
			fail("reset-allocated", "After reset dimension %d still has %d points", dimension.DimensionId, dimension.Allocated)
		}
	}
	healthBefore := pet.Derived.MaxHealth
	attackBefore := pet.Derived.PhysicalAttack

	// ---- 步骤 5:自动加点只算不落 ----
	suggested, err := session.autoAllocate(target)
	if err != nil {
		fail("auto-wait", "%v", err)
	}
	var suggestedSum uint32
	for _, value := range suggested {
		suggestedSum += value
	}
	if suggestedSum != pet.TotalPoints {
		fail("auto-sum", "Suggested sum should equal total points (allocated points are 0 after reset): suggested=%d total=%d",
			suggestedSum, pet.TotalPoints)
	}
	after, err := session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{},
		&scene.GetPetListResponse{})
	if err != nil {
		fail("auto-recheck", "%v", err)
	}
	if p := mustPet(after, target, fail, "auto-recheck"); p.RemainingPoints != pet.TotalPoints {
		fail("auto-not-applied", "Automatic allocation only calculates without committing; remaining points should not change: %d -> %d",
			pet.TotalPoints, p.RemainingPoints)
	}
	zap.L().Info("[pet-smoke] step 5: auto suggestion computed only",
		zap.Uint32("sum", suggestedSum), zap.Int("dimensions", len(suggested)))

	// ---- 步骤 6:按建议提交 → 剩余归零、二级属性变大 ----
	list, err = session.call(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target, Allocated: suggested}, &scene.AllocatePetPointsResponse{})
	if err != nil {
		fail("allocate", "%v", err)
	}
	pet = mustPet(list, target, fail, "allocate")
	if pet.RemainingPoints != 0 {
		fail("allocate-remaining", "After submitting the full suggested allocation, remaining points should be 0, got %d", pet.RemainingPoints)
	}
	if pet.Derived.MaxHealth <= healthBefore && pet.Derived.PhysicalAttack <= attackBefore {
		fail("allocate-derived", "Derived attributes did not increase after allocation: health %d -> %d attack %d -> %d",
			healthBefore, pet.Derived.MaxHealth, attackBefore, pet.Derived.PhysicalAttack)
	}
	zap.L().Info("[pet-smoke] step 6: allocated",
		zap.Uint64("max_health", pet.Derived.MaxHealth),
		zap.Uint64("physical_attack", pet.Derived.PhysicalAttack))

	// ---- 步骤 7:幂等 —— 重发同一份目标值被判"无变化" ----
	if err := session.callExpectTip(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target, Allocated: suggested}, &scene.AllocatePetPointsResponse{},
		tipPetNothingToChange); err != nil {
		fail("allocate-idempotent", "Resending the same target values should return kPetNothingToChange(%d): %v",
			tipPetNothingToChange, err)
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
		fail("decrease-setup", "No suggested dimension is greater than 0; cannot construct a smaller target value")
	}
	if err := session.callExpectTip(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target, Allocated: lower}, &scene.AllocatePetPointsResponse{},
		tipPetPointsCannotDecrease); err != nil {
		fail("allocate-decrease", "Submitting smaller values should return kPetPointsCannotDecrease(%d): %v",
			tipPetPointsCannotDecrease, err)
	}

	// ---- 步骤 9:不存在的 pet_id ----
	if err := session.callExpectTip(game.ScenePetClientPlayerAllocatePetPointsMessageId,
		&scene.AllocatePetPointsRequest{PetId: target + 999999, Allocated: suggested}, &scene.AllocatePetPointsResponse{},
		tipPetNotFound); err != nil {
		fail("unknown-pet", "A nonexistent pet_id should return kPetNotFound(%d): %v", tipPetNotFound, err)
	}
	zap.L().Info("[pet-smoke] steps 7-9: idempotent / no-decrease / unknown-pet rejections ok")

	// ---- 步骤 10:出战 ----
	list, err = session.call(game.ScenePetClientPlayerSummonPetMessageId,
		&scene.SummonPetRequest{PetId: target}, &scene.SummonPetResponse{})
	if err != nil {
		fail("summon", "%v", err)
	}
	if list.ActivePetId != target {
		fail("summon-active", "After deploying the pet, active_pet_id should be %d, got %d", target, list.ActivePetId)
	}
	if p := mustPet(list, target, fail, "summon"); !p.IsActive {
		fail("summon-flag", "After deploying the pet, is_active should be true")
	}
	if err := session.callExpectTip(game.ScenePetClientPlayerSummonPetMessageId,
		&scene.SummonPetRequest{PetId: target}, &scene.SummonPetResponse{}, tipPetAlreadyActive); err != nil {
		fail("summon-twice", "Repeated summoning should return kPetAlreadyActive(%d): %v", tipPetAlreadyActive, err)
	}

	// ---- 步骤 11:下线重登,数据往返 ----
	snapshotBefore := mustPet(list, target, fail, "before-relogin")
	newSession, err := session.relogin(cfg)
	if err != nil {
		fail("relogin", "%v", err)
	}
	session = newSession
	list, err = session.call(game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{},
		&scene.GetPetListResponse{})
	if err != nil {
		fail("list-after-relogin", "%v", err)
	}
	if list.ActivePetId != target {
		fail("relogin-active", "Active pet state was lost after relogin: active_pet_id=%d", list.ActivePetId)
	}
	restored := mustPet(list, target, fail, "after-relogin")
	if restored.Level != snapshotBefore.Level ||
		restored.RemainingPoints != snapshotBefore.RemainingPoints ||
		restored.Growth != snapshotBefore.Growth ||
		restored.Derived.MaxHealth != snapshotBefore.Derived.MaxHealth ||
		restored.Derived.PhysicalAttack != snapshotBefore.Derived.PhysicalAttack {
		fail("relogin-mismatch",
			"State differs before and after relogin: level %d/%d remaining %d/%d growth %d/%d health %d/%d attack %d/%d",
			snapshotBefore.Level, restored.Level,
			snapshotBefore.RemainingPoints, restored.RemainingPoints,
			snapshotBefore.Growth, restored.Growth,
			snapshotBefore.Derived.MaxHealth, restored.Derived.MaxHealth,
			snapshotBefore.Derived.PhysicalAttack, restored.Derived.PhysicalAttack)
	}
	for _, dimension := range restored.Dimensions {
		before := allocatedOfPet(snapshotBefore, dimension.DimensionId)
		if dimension.Allocated != before {
			fail("relogin-allocated", "Dimension %d allocated points after relogin %d != %d",
				dimension.DimensionId, dimension.Allocated, before)
		}
		if dimension.Aptitude != aptitudeOfPet(snapshotBefore, dimension.DimensionId) {
			fail("relogin-aptitude", "Dimension %d aptitude changed after relogin (aptitude is fixed at birth)", dimension.DimensionId)
		}
	}
	zap.L().Info("[pet-smoke] step 11: persisted across relogin",
		zap.Uint32("level", restored.Level), zap.Uint32("growth", restored.Growth))

	// ---- 步骤 12:收回 ----
	list, err = session.call(game.ScenePetClientPlayerRecallPetMessageId, &scene.RecallPetRequest{},
		&scene.RecallPetResponse{})
	if err != nil {
		fail("recall", "%v", err)
	}
	if list.ActivePetId != 0 {
		fail("recall-active", "After recalling the pet, active_pet_id should be 0, got %d", list.ActivePetId)
	}
	if err := session.callExpectTip(game.ScenePetClientPlayerRecallPetMessageId,
		&scene.RecallPetRequest{}, &scene.RecallPetResponse{}, tipPetNotActive); err != nil {
		fail("recall-twice", "Recalling with no active pet should return kPetNotActive(%d): %v", tipPetNotActive, err)
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

// petListResponse 是回全量宝宝列表的响应的公共形状(error_message + pets):除 AutoAllocatePetPoints 外,
// 本冒烟发的宝宝 RPC 的响应都满足。
type petListResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
	GetPets() *scene.PetListInfo
}

// call 发一个宝宝 RPC 并取它带回的新列表(成功路径);任何拒绝都视为失败。
func (s *petSmokeSession) call(messageId uint32, request proto.Message, response petListResponse) (*scene.PetListInfo, error) {
	list, _, err := s.callTolerant(messageId, request, response)
	return list, err
}

// callTolerant 发一个宝宝 RPC,只等本次请求自己的回包并直接解析(按 MessageContent.id 认领,
// docs/design/client-rpc-callback.md §6)。response 传该请求对应响应类型的空值,返回后调用方还能从中读列表以外的
// 字段(GmGrantPet 的 pet_id)。
// 结局:受理回 (列表, 0, nil);scene 在响应体里拒绝、且码在 allowedTips 里回 (nil, tip, nil);其余一律返回 error:
// scene 的其它拒绝码(tip 一并返回)、gate 信封拒绝、超时 / 断线 / 没发出、受理却不带列表。
// allowedTips 只对 scene 的业务码生效:信封拒绝说明请求没到 scene 的宝宝逻辑,现有调用方容忍 / 期待的都是业务码。
//
// 迁移前等的是生成 handler 写进 gameobject.Player 的「列表序号 + 来源消息号」(信封错误则落成超时):服务器会在升级等
// 路径主动推 NotifyPetListChanged,不看来源就会把推送当成本次响应。现在回包按请求号认领,拿到的就是本次的响应;
// 推送照旧经 handler 写 Player。Player 上的宝宝状态(列表 / tip / 建议 / 新发 pet_id)全包只有本场景读过,
// 所以直接解析(交接 §8.1 做法 1),不再经 handler。
//
// 调用约束:连接的 RecvLoop 必须已启动;不得在 RecvLoop goroutine 上调用(同步等回包会自锁)。
func (s *petSmokeSession) callTolerant(messageId uint32, request proto.Message, response petListResponse,
	allowedTips ...uint32) (*scene.PetListInfo, uint32, error) {
	envelopeTip, err := s.caller.call(messageId, request, response)
	if err != nil {
		return nil, 0, err
	}
	if envelopeTip != 0 {
		return nil, 0, sceneRequestEnvelopeRejection(messageId, envelopeTip)
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		if slices.Contains(allowedTips, tip) {
			return nil, tip, nil
		}
		return nil, tip, fmt.Errorf("rejected by scene: message_id=%d tip=%d", messageId, tip)
	}
	// 迁移前受理却不带列表的回包不推进序号,表现为等满超时;现在回包就是本次的,不会再有下一份,直接判失败
	list := response.GetPets()
	if list == nil {
		return nil, 0, fmt.Errorf("message_id=%d was accepted but the response carries no pet list", messageId)
	}
	return list, 0, nil
}

// callExpectTip 发一个预期被 scene 以 want 拒绝的请求:响应体 tip == want 返回 nil;受理、别的拒绝码、gate 信封拒绝
// 与超时都返回描述实际结局的 error。迁移前这几种结局挤在一个 tip 返回值里(受理与超时都是 0)。
func (s *petSmokeSession) callExpectTip(messageId uint32, request proto.Message, response petListResponse, want uint32) error {
	_, tip, err := s.callTolerant(messageId, request, response, want)
	if err != nil {
		return err
	}
	if tip != want {
		return fmt.Errorf("message_id=%d was accepted", messageId)
	}
	return nil
}

// callAttributePanel 借用角色属性通道(改等级 / 查角色面板)。宝宝的等级来自主人,
// 所以这条冒烟必须同时驱动两套协议。任何拒绝都视为失败;认领方式与结局判据见
// requestAttributePanel(attribute_smoke_scenario.go)。
func (s *petSmokeSession) callAttributePanel(messageId uint32, request proto.Message, response attributePanelResponse) (*scene.AttributePanelInfo, error) {
	panel, _, err := requestAttributePanel(s.caller, messageId, request, response)
	return panel, err
}

// autoAllocate 发 AutoAllocatePetPoints 并直接解析本次回包,返回 petId 的建议目标已分配值(只算不落)。
// 响应不带列表,所以不走 callTolerant;scene 拒绝、gate 信封拒绝、超时,以及建议不属于该宝宝或为空,都返回 error。
func (s *petSmokeSession) autoAllocate(petId uint64) (map[uint32]uint32, error) {
	messageId := uint32(game.ScenePetClientPlayerAutoAllocatePetPointsMessageId)
	response := &scene.AutoAllocatePetPointsResponse{}
	envelopeTip, err := s.caller.call(messageId, &scene.AutoAllocatePetPointsRequest{PetId: petId}, response)
	if err != nil {
		return nil, err
	}
	if envelopeTip != 0 {
		return nil, sceneRequestEnvelopeRejection(messageId, envelopeTip)
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		return nil, fmt.Errorf("auto allocation rejected by scene: tip=%d", tip)
	}
	// 迁移前这两种回包不满足等待条件,表现为等满超时;现在回包就是本次的,不会再有下一份,直接判失败
	if response.GetPetId() != petId || len(response.GetSuggested()) == 0 {
		return nil, fmt.Errorf("auto allocation returned pet_id=%d with %d suggested dimensions, expected pet_id=%d with a nonempty suggestion",
			response.GetPetId(), len(response.GetSuggested()), petId)
	}
	return response.GetSuggested(), nil
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
	gc, _, err := prepareBehaviorClient(host, port, petSmokeAccount, cfg.Password, s.stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("relogin: %w", err)
	}
	return newPetSmokeSession(gc, s.stats), nil
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
		fail(step, "List contains no pet_id=%d", petId)
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
