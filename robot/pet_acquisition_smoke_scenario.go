package main

// 独立开发账号验证普通获取入口。GM 只准备等级和银两，宠物必须由商店和捕捉取得。
import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"proto/battle"
	"proto/common/base"
	"proto/match"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
)

func validatePetAcquisitionEndpoint(address string) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil {
		return fmt.Errorf("pet acquisition smoke requires a local http://127.0.0.1 development gateway")
	}
	return nil
}

func RunPetAcquisitionSmoke(cfg *config.Config) {
	if err := runPetAcquisitionSmoke(cfg); err != nil {
		zap.L().Error("PET_ACQUISITION_SMOKE_FAIL", zap.Error(err))
		_ = zap.L().Sync()
		os.Exit(1)
	}
}

type petAcquisitionResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

func petAcquisitionCall(caller *smokeCaller, id uint32, request proto.Message, response petAcquisitionResponse) error {
	tip, err := caller.call(id, request, response)
	if err != nil {
		return err
	}
	if tip != 0 || response.GetErrorMessage().GetId() != 0 {
		return fmt.Errorf("RPC rejected message=%d envelope_tip=%d body_tip=%d", id, tip, response.GetErrorMessage().GetId())
	}
	return nil
}

func runPetAcquisitionSmoke(cfg *config.Config) error {
	if err := validatePetAcquisitionEndpoint(cfg.GatewayAddr); err != nil {
		return err
	}
	// 本地Docker共享口令由运行环境注入，不写进冒烟配置或日志。
	if password := os.Getenv("LOGIN_DEV_PASSWORD_SHARED_SECRET"); password != "" {
		local := *cfg
		local.Password = password
		cfg = &local
	}
	loginTestCfg = cfg
	stats := metrics.NewStats()
	// 每次都是新账号，既不碰用户角色，也不复用已经装满宠物的冒烟账号。
	account := fmt.Sprintf("robot_93%d", time.Now().UnixMilli())
	bot, err := battleSmokeLogin(cfg, account, stats)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	cleanup := func() {
		if bot == nil {
			return
		}
		_ = leaveGame(bot.gc, stats)
		gameobject.PlayerList.Delete(bot.gc.PlayerId)
		bot.gc.Close()
		bot = nil
	}
	defer cleanup()
	caller := newSmokeCaller(bot.gc, stats, account, petSmokeRpcTimeout, nil)
	if err := petAcquisitionCall(caller, game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: 30}, &scene.GmSetPlayerLevelResponse{}); err != nil {
		return fmt.Errorf("prepare-level: %w", err)
	}
	if _, err := gmAddGold(bot.gc, bot.player, stats, 100000, petSmokeRpcTimeout); err != nil {
		return fmt.Errorf("prepare-gold: %w", err)
	}

	shop := &scene.GetPetShopResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerGetPetShopMessageId, &scene.GetPetShopRequest{}, shop); err != nil {
		return fmt.Errorf("get-shop: %w", err)
	}
	if len(shop.GetOffers()) != 22 || shop.GetNextPurchaseSequence() != 1 {
		return fmt.Errorf("shop: offers=%d sequence=%d, expected 22 offers on a fresh account", len(shop.GetOffers()), shop.GetNextPurchaseSequence())
	}
	var offer *scene.PetShopOffer
	seen := make(map[uint32]bool)
	for _, row := range shop.GetOffers() {
		if row.GetPetTableId() == 0 || seen[row.GetPetTableId()] || row.GetPriceGold() == 0 {
			return fmt.Errorf("shop: invalid or duplicated offer=%d", row.GetPetTableId())
		}
		seen[row.GetPetTableId()] = true
		if row.GetPetTableId() == 1 {
			offer = row
		}
	}
	if offer == nil || offer.GetPriceGold() > shop.GetGold() {
		return fmt.Errorf("shop: ordinary pet 1 is missing or unaffordable")
	}
	request := &scene.BuyPetRequest{PetTableId: offer.GetPetTableId(), PurchaseSequence: shop.GetNextPurchaseSequence(), PriceGold: offer.GetPriceGold()}
	bought := &scene.BuyPetResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerBuyPetMessageId, request, bought); err != nil {
		return fmt.Errorf("buy: %w", err)
	}
	if bought.GetPetId() == 0 || len(bought.GetPets().GetPets()) != 1 || bought.GetGold() != shop.GetGold()-offer.GetPriceGold() || bought.GetNextPurchaseSequence() != 2 {
		return fmt.Errorf("buy: incorrect pet count, receipt, or debit: pet=%d pets=%d gold=%d before=%d price=%d sequence=%d", bought.GetPetId(), len(bought.GetPets().GetPets()), bought.GetGold(), shop.GetGold(), offer.GetPriceGold(), bought.GetNextPurchaseSequence())
	}
	retry := &scene.BuyPetResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerBuyPetMessageId, request, retry); err != nil {
		return fmt.Errorf("buy-retry: %w", err)
	}
	if retry.GetPetId() != bought.GetPetId() || retry.GetGold() != bought.GetGold() || retry.GetNextPurchaseSequence() != 2 || !proto.Equal(retry.GetPets(), bought.GetPets()) {
		return fmt.Errorf("buy-retry: receipt, balance or pet list changed")
	}
	zap.L().Info("PET_ACQUISITION_SHOP_OK", zap.String("account", account), zap.Uint64("player", bot.gc.PlayerId), zap.Int("offers", len(shop.Offers)), zap.Uint64("pet_id", bought.PetId), zap.Uint64("price", offer.PriceGold), zap.Uint64("gold", bought.Gold))

	battleID, capturedTable, attempts, err := petAcquisitionCapture(cfg, bot, stats)
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	list := &scene.GetPetListResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{}, list); err != nil {
		return fmt.Errorf("settled-list: %w", err)
	}
	var capturedID uint64
	for _, pet := range list.GetPets().GetPets() {
		if pet.GetPetId() != bought.GetPetId() && pet.GetPetTableId() == capturedTable {
			capturedID = pet.GetPetId()
		}
	}
	if len(list.GetPets().GetPets()) != 2 || findPet(list.GetPets(), bought.GetPetId()) == nil || capturedID == 0 {
		return fmt.Errorf("settled-list: expected purchased pet and exactly one captured pet, got pets=%d captured_id=%d", len(list.GetPets().GetPets()), capturedID)
	}
	beforeRelog := proto.Clone(list.GetPets()).(*scene.PetListInfo)
	playerID := bot.gc.PlayerId
	cleanup()
	time.Sleep(2 * time.Second)
	bot, err = battleSmokeLogin(cfg, account, stats)
	if err != nil {
		return fmt.Errorf("relogin: %w", err)
	}
	if bot.gc.PlayerId != playerID {
		return fmt.Errorf("relogin: role identity changed")
	}
	caller = newSmokeCaller(bot.gc, stats, account, petSmokeRpcTimeout, nil)
	restored := &scene.GetPetListResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerGetPetListMessageId, &scene.GetPetListRequest{}, restored); err != nil {
		return fmt.Errorf("relogin-list: %w", err)
	}
	if !proto.Equal(beforeRelog, restored.GetPets()) {
		return fmt.Errorf("relogin: pet list changed after persisted settlement")
	}
	afterRelogShop := &scene.GetPetShopResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerGetPetShopMessageId, &scene.GetPetShopRequest{}, afterRelogShop); err != nil {
		return fmt.Errorf("relogin-shop: %w", err)
	}
	afterRelogRetry := &scene.BuyPetResponse{}
	if err := petAcquisitionCall(caller, game.ScenePetClientPlayerBuyPetMessageId, request, afterRelogRetry); err != nil {
		return fmt.Errorf("relogin-retry: %w", err)
	}
	if afterRelogRetry.GetPetId() != bought.GetPetId() || afterRelogRetry.GetGold() != afterRelogShop.GetGold() || !proto.Equal(restored.GetPets(), afterRelogRetry.GetPets()) {
		return fmt.Errorf("relogin-retry: persisted purchase receipt did not prevent repeat grant/debit")
	}
	zap.L().Info("PET_ACQUISITION_SMOKE_OK", zap.String("account", account), zap.Uint64("player", playerID), zap.Uint64("battle", battleID), zap.Uint64("purchased_pet", bought.GetPetId()), zap.Uint64("captured_pet", capturedID), zap.Uint32("captured_kind", capturedTable), zap.Int("capture_attempts", attempts), zap.Bool("relogin_verified", true))
	return nil
}

type petCaptureFrame struct {
	state    *battle.BattleStateS2C
	captured uint32
	err      error
}

func petAcquisitionCapture(cfg *config.Config, bot *battleSmokeBot, stats *metrics.Stats) (uint64, uint32, int, error) {
	planID, err := ensureNetworkProbe(bot.gc, stats, bot.account)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("network-probe: %w", err)
	}
	if err := battleSmokeJoinQueue(bot, stats, &match.JoinQueueRequest{PlayerId: bot.gc.PlayerId, Mode: match.MatchMode_MATCH_MODE_PVE_SOLO, BattleConfigId: 4, ZoneId: cfg.ZoneID, RequestId: chatSmokeRequestId(), NetworkPlanId: planID}, battleSmokeStartTimeout); err != nil {
		return 0, 0, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), battleSmokeStartTimeout)
	battleID, err := bot.player.WaitBattleStart(ctx)
	cancel()
	if err != nil {
		return 0, 0, 0, err
	}
	ctx, cancel = context.WithTimeout(context.Background(), battleDirectConnTimeout)
	assigned, err := bot.player.WaitBattleAssigned(ctx)
	cancel()
	if err != nil {
		return battleID, 0, 0, err
	}
	if assigned.GetBattleId() != battleID || assigned.GetRole() != battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT {
		return battleID, 0, 0, fmt.Errorf("incorrect battle assignment")
	}
	frames := make(chan petCaptureFrame, 32)
	direct, err := dialBattleDirect(bot.account, bot.gc.PlayerId, assigned, stats, func(client *pkg.GameClient, message *base.MessageContent) {
		frame := petCaptureFrame{}
		switch message.GetMessageId() {
		case game.BattleClientPlayerGetBattleStateMessageId:
			frame.state = &battle.BattleStateS2C{}
			frame.err = proto.Unmarshal(message.GetSerializedMessage(), frame.state)
		case game.BattleClientPlayerNotifyTurnResultMessageId:
			turn := &battle.TurnResultS2C{}
			frame.err = proto.Unmarshal(message.GetSerializedMessage(), turn)
			frame.state = turn.GetState()
			for _, event := range turn.GetEvents() {
				if event.GetEventType() == battle.EBattleEventType_BATTLE_EVENT_CAPTURE && event.GetSuccess() && event.GetSourceId() == bot.gc.PlayerId {
					frame.captured = uint32(event.GetValue())
				}
			}
		}
		if frame.state != nil || frame.err != nil {
			select {
			case frames <- frame:
			default:
			}
		}
		dispatchBattleDirectDefault(client, message)
	})
	if err != nil {
		return battleID, 0, 0, err
	}
	defer direct.Close()
	caller := newSmokeCaller(direct.gc, stats, bot.account, petSmokeRpcTimeout, nil)
	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	attempts := 0
	for {
		var frame petCaptureFrame
		select {
		case frame = <-frames:
		case <-deadline.Done():
			return battleID, 0, attempts, fmt.Errorf("capture timed out after %d attempts", attempts)
		}
		if frame.err != nil {
			return battleID, 0, attempts, frame.err
		}
		if frame.captured != 0 {
			// 已成功捕到一只后自动打完，避免把第二次随机成功当成验收前提。
			if frame.state.GetOutcome() == battle.EBattleOutcome_BATTLE_OUTCOME_ONGOING {
				if err := petAcquisitionCall(caller, game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{BattleId: battleID, Enabled: true}, &battle.SetAutoBattleResponse{}); err != nil {
					return battleID, 0, attempts, err
				}
			}
			endCtx, endCancel := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
			outcome, err := bot.player.WaitBattleEnd(endCtx)
			endCancel()
			if err != nil {
				return battleID, 0, attempts, fmt.Errorf("durable scene end: %w", err)
			}
			if battle.EBattleOutcome(outcome) == battle.EBattleOutcome_BATTLE_OUTCOME_VOID {
				return battleID, 0, attempts, fmt.Errorf("battle was voided")
			}
			zap.L().Info("PET_ACQUISITION_CAPTURE_OK", zap.Uint64("battle", battleID), zap.Uint32("kind", frame.captured), zap.Int("attempts", attempts), zap.Int32("outcome", outcome))
			return battleID, frame.captured, attempts, nil
		}
		if frame.state.GetOutcome() != battle.EBattleOutcome_BATTLE_OUTCOME_ONGOING || attempts >= 24 {
			return battleID, 0, attempts, fmt.Errorf("no successful capture within bounded battle, outcome=%v", frame.state.GetOutcome())
		}
		var target uint64
		for _, actor := range frame.state.GetActors() {
			if actor.GetCapturePetTableId() != 0 && actor.GetCapturedByPlayerId() == 0 && !actor.GetIsDead() && !actor.GetFled() {
				target = actor.GetActorId()
				break
			}
		}
		if target == 0 {
			return battleID, 0, attempts, fmt.Errorf("wild config4 has no eligible capture target")
		}
		if err := petAcquisitionCall(caller, game.BattleClientPlayerSubmitBattleActionMessageId, &battle.SubmitBattleActionRequest{BattleId: battleID, Action: &battle.BattleAction{ActionType: battle.EBattleActionType_BATTLE_ACTION_CAPTURE, TargetId: target}}, &battle.SubmitBattleActionResponse{}); err != nil {
			return battleID, 0, attempts, fmt.Errorf("submit capture: %w", err)
		}
		attempts++
	}
}
