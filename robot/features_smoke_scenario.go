package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"proto/common/base"
	"proto/login"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
)

const featureSmokeTimeout = 15 * time.Second

type featureSmokeSession struct {
	gc     *pkg.GameClient
	player *gameobject.Player
	stats  *metrics.Stats
	done   chan struct{}
}

// RunFeaturesSmoke only selects an existing role. Read-only is the default;
// inventory sorting and each mission action require their own explicit option.
// Reports contain IDs/counts/status only, never credentials or packet dumps.
func RunFeaturesSmoke(cfg *config.Config, stats *metrics.Stats) error {
	if cfg == nil || strings.TrimSpace(cfg.FeaturesSmoke.Account) == "" {
		return fmt.Errorf("features_smoke.account must explicitly select an existing account")
	}
	options := cfg.FeaturesSmoke
	if err := validateFeatureBattleOptions(options); err != nil {
		return err
	}
	if options.BagType > 3 || (options.SortBag && options.BagType > 1) {
		return fmt.Errorf("invalid bag_type or unsupported sort bag")
	}
	if stats == nil {
		stats = metrics.NewStats()
	}
	session, err := openFeatureSmokeSession(cfg, stats)
	if err != nil {
		return err
	}
	defer func() {
		if session != nil {
			session.close()
		}
	}()
	bag, err := session.readBag(options.BagType)
	if err != nil {
		return err
	}
	missionBody, err := session.call(game.SceneMissionClientPlayerGetMissionListMessageId, &scene.GetMissionListRequest{})
	if err != nil {
		return err
	}
	missions := missionBody.(*scene.GetMissionListResponse)
	activityBody, err := session.call(game.SceneActivityClientPlayerGetActivityListMessageId, &scene.GetActivityListRequest{})
	if err != nil {
		return err
	}
	activities := activityBody.(*scene.GetActivityListResponse)
	for _, activity := range activities.Activities {
		if activity == nil {
			return fmt.Errorf("activity list contains missing entry")
		}
		if activity.Status == scene.PlayerActivityStatus_PLAYER_ACTIVITY_UNSCHEDULED &&
			(activity.StartsAtMs != 0 || activity.EndsAtMs != 0 || activity.CanParticipate) {
			return fmt.Errorf("unscheduled activity %d advertises dates or participation", activity.ActivityId)
		}
		if activity.CanParticipate && activity.Status != scene.PlayerActivityStatus_PLAYER_ACTIVITY_OPEN {
			return fmt.Errorf("activity %d advertises participation outside server OPEN state", activity.ActivityId)
		}
	}
	fmt.Printf("FEATURES_READ_OK player=%d bag_items=%d missions=%d activities=%d persistent=%t\n",
		session.gc.PlayerId, len(bag.Items), len(missions.Missions), len(activities.Activities), missions.StatePersistent)

	if options.SortBag {
		if !bag.Layout.CanSort {
			return fmt.Errorf("server does not allow sorting selected bag")
		}
		before, err := featureBagInventory(bag)
		if err != nil {
			return err
		}
		firstBody, err := session.call(game.SceneBagClientPlayerSortBagMessageId, &scene.SortBagRequest{BagType: options.BagType})
		if err != nil {
			return err
		}
		first := firstBody.(*scene.SortBagResponse).Bag
		if first.GetLayout().GetBagType() != options.BagType {
			return fmt.Errorf("sort response has wrong bag type")
		}
		firstInventory, err := featureBagInventory(first)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, firstInventory) || !proto.Equal(bag.Currency, first.Currency) {
			return fmt.Errorf("sort changed item totals or currency")
		}
		secondBody, err := session.call(game.SceneBagClientPlayerSortBagMessageId, &scene.SortBagRequest{BagType: options.BagType})
		if err != nil {
			return err
		}
		second := secondBody.(*scene.SortBagResponse)
		if _, err := featureBagInventory(second.Bag); err != nil {
			return err
		}
		if second.Changed || !featureBagsEqual(first, second.Bag) {
			return fmt.Errorf("second explicit sort is not idempotent")
		}
		bag = second.Bag
		fmt.Println("FEATURES_SORT_OK inventory_and_currency_preserved=true second_sort_changed=false")
	} else {
		fmt.Println("FEATURES_SORT_SKIPPED explicit sort_bag option is false")
	}

	if options.AcceptMissionID != 0 {
		mission := featureMission(missions, options.Scope, options.AcceptMissionID)
		shouldAccept, err := featureMissionAcceptDecision(mission, options.BattleConfigID != 0)
		if err != nil {
			return err
		}
		if shouldAccept {
			body, err := session.call(game.SceneMissionClientPlayerAcceptMissionMessageId,
				&scene.MissionActionRequest{Scope: options.Scope, MissionId: options.AcceptMissionID})
			if err != nil {
				return err
			}
			missions = body.(*scene.GetMissionListResponse)
			accepted := featureMission(missions, options.Scope, options.AcceptMissionID)
			if accepted == nil || accepted.Status == scene.PlayerMissionStatus_PLAYER_MISSION_NOT_ACCEPTED {
				return fmt.Errorf("accept response did not contain accepted mission state")
			}
			bag, err = session.readBag(options.BagType)
			if err != nil {
				return err
			}
			fmt.Printf("FEATURES_ACCEPT_OK scope=%d mission=%d\n", options.Scope, options.AcceptMissionID)
		} else {
			fmt.Printf("FEATURES_ACCEPT_ACTIVE_CONTINUE scope=%d mission=%d\n", options.Scope, options.AcceptMissionID)
		}
	} else {
		fmt.Println("FEATURES_ACCEPT_SKIPPED no mission selected")
	}

	if options.BattleConfigID != 0 {
		if err := session.runFeatureBattle(cfg); err != nil {
			return err
		}
		progressContext, cancelProgress := context.WithTimeout(context.Background(), featureSmokeTimeout)
		missions, err = waitFeatureMissionClaimable(progressContext, options.Scope, options.ClaimMissionID,
			func() (*scene.GetMissionListResponse, error) {
				body, err := session.call(game.SceneMissionClientPlayerGetMissionListMessageId, &scene.GetMissionListRequest{})
				if err != nil {
					return nil, err
				}
				return body.(*scene.GetMissionListResponse), nil
			})
		cancelProgress()
		if err != nil {
			return err
		}
		bag, err = session.readBag(options.BagType)
		if err != nil {
			return err
		}
		fmt.Printf("FEATURES_BATTLE_MISSION_OK scope=%d mission=%d can_claim=true\n", options.Scope, options.ClaimMissionID)
	} else {
		fmt.Println("FEATURES_BATTLE_SKIPPED battle_config_id is zero")
	}

	if options.ClaimMissionID != 0 {
		mission := featureMission(missions, options.Scope, options.ClaimMissionID)
		if mission == nil || !mission.CanClaim {
			return fmt.Errorf("selected mission does not advertise CanClaim")
		}
		request := &scene.MissionActionRequest{Scope: options.Scope, MissionId: options.ClaimMissionID}
		body, err := session.call(game.SceneMissionClientPlayerClaimMissionRewardMessageId, request)
		if err != nil {
			return err
		}
		missions = body.(*scene.GetMissionListResponse)
		claimed := featureMission(missions, options.Scope, options.ClaimMissionID)
		if claimed == nil || claimed.CanClaim || claimed.Status != scene.PlayerMissionStatus_PLAYER_MISSION_COMPLETED {
			return fmt.Errorf("claim response did not mark mission completed")
		}
		bag, err = session.readBag(options.BagType)
		if err != nil {
			return err
		}
		// 只认任务逻辑在响应体里的业务拒绝(featureServerError)。信封拒绝(gate 限流 / 1003 等)不能证明任务逻辑
		// 判过这次重复领取:可能根本没到任务逻辑,1003 也可能是结局未知(RPC0 超时 / 断开);超时同样不知道结局。
		// 这些都不能证明「服务器拒绝了重复领取」。
		_, repeatErr := session.call(game.SceneMissionClientPlayerClaimMissionRewardMessageId, request)
		if _, rejected := repeatErr.(*featureServerError); !rejected {
			if repeatErr != nil {
				return fmt.Errorf("duplicate claim was not rejected by mission logic: %w", repeatErr)
			}
			return fmt.Errorf("duplicate claim was not explicitly rejected by server")
		}
		afterDuplicate, err := session.readBag(options.BagType)
		if err != nil {
			return err
		}
		if !featureBagsEqual(bag, afterDuplicate) {
			return fmt.Errorf("duplicate claim changed bag or currency")
		}
		fmt.Printf("FEATURES_CLAIM_OK scope=%d mission=%d duplicate_rejected=true duplicate_inventory_unchanged=true\n",
			options.Scope, options.ClaimMissionID)
	} else {
		fmt.Println("FEATURES_CLAIM_SKIPPED no mission selected")
	}

	if options.VerifyRelogin {
		if !missions.StatePersistent {
			return fmt.Errorf("server has not enabled mission persistence")
		}
		playerID := session.gc.PlayerId
		session.close()
		session = nil
		time.Sleep(2 * time.Second)
		reloginConfig := *cfg
		reloginConfig.FeaturesSmoke.PlayerID = playerID
		session, err = openFeatureSmokeSession(&reloginConfig, stats)
		if err != nil {
			return err
		}
		body, err := session.call(game.SceneMissionClientPlayerGetMissionListMessageId, &scene.GetMissionListRequest{})
		if err != nil {
			return err
		}
		reloaded := body.(*scene.GetMissionListResponse)
		if !reloaded.StatePersistent || !featureMissionProgressEqual(missions, reloaded) {
			return fmt.Errorf("mission status/progress did not survive relogin")
		}
		reloadedBag, err := session.readBag(options.BagType)
		if err != nil {
			return err
		}
		if !featureBagsEqual(bag, reloadedBag) {
			return fmt.Errorf("bag did not survive relogin")
		}
		fmt.Println("FEATURES_RELOGIN_OK mission_progress_and_bag_preserved=true")
	} else {
		fmt.Println("FEATURES_RELOGIN_SKIPPED explicit verify_relogin option is false")
	}
	fmt.Println("FEATURES_SMOKE_OK configured_checks_passed=true")
	return nil
}

func openFeatureSmokeSession(cfg *config.Config, stats *metrics.Stats) (*featureSmokeSession, error) {
	host, portText, payload, signature, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("feature gate assignment failed")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("feature gate address is invalid")
	}
	gc, err := connectAndVerify(host, port, cfg.FeaturesSmoke.Account, payload, signature)
	if err != nil {
		return nil, fmt.Errorf("feature gate connection or verification failed")
	}
	if err := loginExistingFeaturePlayer(gc, cfg, stats); err != nil {
		gc.Close()
		return nil, err
	}
	player := gameobject.NewPlayer(gc.PlayerId)
	gameobject.PlayerList.Set(gc.PlayerId, player)
	pkg.Clients.Register(gc.PlayerId, gc)
	session := &featureSmokeSession{gc: gc, player: player, stats: stats, done: make(chan struct{})}
	go func() {
		defer close(session.done)
		// 这里只收推送与不关联请求的消息(id = 0)。本场景经 callVia 发出的请求,回包带请求号,由 GameClient
		// 交给发起的调用、在那里写 feature 快照,不会到这里。
		gc.RecvLoop(func(client *pkg.GameClient, message *base.MessageContent) {
			stats.MsgRecv()
			if featureLobbyCallMessage(message.MessageId) {
				// 同号却 id = 0 说明服务端没回显请求号,那次调用将以超时收场。丢弃而不交 feature 分发:
				// 写进快照会盖掉调用刚认领、还没来得及读的那一份。
				smokeDropUncorrelatedReply(client.Account, message)
				return
			}
			// 战斗公告(大厅上是开战,以及 scene 结算后的终局)进 feature waiter,见 waitFeatureBattleResponse。
			if cfg.FeaturesSmoke.BattleConfigID != 0 && handler.HandleFeatureBattleMessage(player, message) {
				return
			}
			if message.MessageId == game.SceneClientPlayerCommonRedirectToGateMessageId {
				// Common redirect relogin may auto-create roles. This mode fails
				// instead; the caller must select the existing role's correct zone.
				gc.Close()
				return
			}
			handler.MessageBodyHandler(client, message)
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), featureSmokeTimeout)
	defer cancel()
	if err := player.WaitSceneReady(ctx); err != nil {
		session.close()
		return nil, fmt.Errorf("existing feature role did not enter scene")
	}
	return session, nil
}

func loginExistingFeaturePlayer(gc *pkg.GameClient, cfg *config.Config, stats *metrics.Stats) error {
	request := &login.LoginRequest{Account: gc.Account, Password: cfg.Password}
	if cfg.AuthType == "satoken" {
		token, err := fetchSaToken(cfg.SaTokenAddr, gc.Account)
		if err != nil {
			return fmt.Errorf("feature SA-Token authentication failed")
		}
		request.Password = ""
		request.AuthType = "satoken"
		request.AuthToken = token
	} else if cfg.AuthType != "" && cfg.AuthType != "password" {
		return fmt.Errorf("unsupported feature auth_type")
	}
	if cfg.UseHttpLogin {
		result, err := httpLogin(cfg.GatewayAddr, &httpLoginRequest{ZoneID: cfg.ZoneID, Account: gc.Account,
			Password: request.Password, AuthType: request.AuthType, AuthToken: request.AuthToken}, featureSmokeTimeout)
		if err != nil {
			return fmt.Errorf("feature HTTP authentication failed")
		}
		if result.Code != 0 || result.AccessToken == "" {
			return fmt.Errorf("feature HTTP authentication rejected or queued (code=%d)", result.Code)
		}
		gc.SetTokens(result.AccessToken, result.RefreshToken, result.AccessTokenExpire, result.RefreshTokenExpire)
		request.Password = ""
		request.AuthType = "access_token"
		request.AuthToken = result.AccessToken
	}
	var response login.LoginResponse
	if err := featureAuthCall(gc, stats, game.ClientPlayerLoginLoginMessageId, request, &response); err != nil {
		return err
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		return &featureServerError{game.ClientPlayerLoginLoginMessageId, tip}
	}
	playerID, err := featureExistingPlayerID(&response, cfg.FeaturesSmoke.PlayerID)
	if err != nil {
		return err
	} // Intentionally no CreatePlayer request or fallback.
	if response.AccessToken != "" {
		gc.SetTokens(response.AccessToken, response.RefreshToken, response.AccessTokenExpire, response.RefreshTokenExpire)
	}
	var entered login.EnterGameResponse
	if err := featureAuthCall(gc, stats, game.ClientPlayerLoginEnterGameMessageId, &login.EnterGameRequest{PlayerId: playerID}, &entered); err != nil {
		return err
	}
	if tip := entered.GetErrorMessage().GetId(); tip != 0 {
		return &featureServerError{game.ClientPlayerLoginEnterGameMessageId, tip}
	}
	if entered.PlayerId != playerID {
		return fmt.Errorf("enter response did not match selected existing role")
	}
	gc.PlayerId = playerID
	return nil
}

func featureExistingPlayerID(response *login.LoginResponse, selected uint64) (uint64, error) {
	for _, wrapper := range response.GetPlayers() {
		id := wrapper.GetPlayer().GetPlayerId()
		if id != 0 && (selected == 0 || id == selected) {
			return id, nil
		}
	}
	return 0, fmt.Errorf("selected account has no matching existing role; no role was created")
}

// Dedicated auth round-trip checks the envelope too. Existing modes retain
// sendAndRecvTimeout behavior; failures here never include body/token strings.
func featureAuthCall(gc *pkg.GameClient, stats *metrics.Stats, id uint32, request, response proto.Message) error {
	if err := gc.SendRequest(id, request); err != nil {
		return fmt.Errorf("feature auth send failed message_id=%d", id)
	}
	stats.MsgSent()
	result := make(chan error, 1)
	go func() {
		for {
			message, err := gc.RecvOne()
			if err != nil {
				result <- fmt.Errorf("feature auth receive failed message_id=%d", id)
				return
			}
			stats.MsgRecv()
			if message.MessageId != id {
				gc.DeferMessage(message)
				continue
			}
			if tip := message.GetErrorMessage().GetId(); tip != 0 {
				result <- &featureServerError{id, tip}
				return
			}
			if err := proto.Unmarshal(message.SerializedMessage, response); err != nil {
				result <- fmt.Errorf("invalid feature auth response message_id=%d", id)
				return
			}
			result <- nil
			return
		}
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(featureSmokeTimeout):
		gc.Close()
		return fmt.Errorf("feature auth timed out message_id=%d", id)
	}
}

// featureServerError 是带 tip 的服务端拒绝。callVia 只用它报业务拒绝(快照里响应体的 tip),信封拒绝报包裹
// *pkg.ServerTipError 的 error —— 重复领奖只认业务拒绝。登录握手(featureAuthCall)与 waitFeatureBattleResponse
// 不区分两者,一律用它。
type featureServerError struct{ MessageID, TipID uint32 }

func (e *featureServerError) Error() string {
	return fmt.Sprintf("server rejected message_id=%d tip=%d", e.MessageID, e.TipID)
}

// featureLobbyCallMessage 报告 messageID 是否是本场景经大厅连接用 callVia 发出的请求号:背包两条、任务三条、
// 活动列表与 JoinQueue。它们的回包带请求号,不会进大厅 RecvLoop 的回调。
func featureLobbyCallMessage(messageID uint32) bool {
	switch messageID {
	case game.SceneBagClientPlayerGetBagMessageId,
		game.SceneBagClientPlayerSortBagMessageId,
		game.SceneMissionClientPlayerGetMissionListMessageId,
		game.SceneMissionClientPlayerAcceptMissionMessageId,
		game.SceneMissionClientPlayerClaimMissionRewardMessageId,
		game.SceneActivityClientPlayerGetActivityListMessageId,
		game.MatchServiceJoinQueueMessageId:
		return true
	}
	return false
}

// call 经大厅连接发一条 feature RPC,见 callVia。
func (s *featureSmokeSession) call(id uint32, request proto.Message) (proto.Message, error) {
	return s.callVia(s.gc, id, request)
}

// callVia 经 gc(大厅连接,或 callDirect 给的战斗直连)发一条 feature RPC,只认领本次请求自己的回包
// (按 MessageContent.id,docs/design/client-rpc-callback.md §6)。回包(应答或信封拒绝)先在 gc 的 RecvLoop 上
// 交给 recordFeatureReply 写进 Player 的 feature 快照,CallSyncThen 才返回,所以返回后按发送前的游标读快照,
// 读到的就是本次结果,不必轮询;上一次超时请求的迟到回包由 GameClient 丢弃,不会被当成本次结果。
//
// 结果仍以快照为准:响应完整性判据(缺 layout、排队无票据等)只在 logic/handler 的 HandleFeature* 里,这里不复制;
// 两条连接的应答记在同一个 Player 上,排队与自动战斗的快照另由 waitFeatureBattleResponse 读。
//
// gc 的 RecvLoop 必须已经起来;不得在任何 RecvLoop goroutine 上调用(CallSyncThen 的约束)。
func (s *featureSmokeSession) callVia(gc *pkg.GameClient, id uint32, request proto.Message) (proto.Message, error) {
	cursor := s.player.FeatureSequence()
	reply, err := gc.CallSyncThen(context.Background(), id, request, featureSmokeTimeout, s.recordFeatureReply)
	if errors.Is(err, pkg.ErrRPCNotSent) {
		return nil, fmt.Errorf("feature send failed message_id=%d: %w", id, err)
	}
	s.stats.MsgSent()
	if reply != nil {
		s.stats.MsgRecv()
	}
	return s.featureCallResult(id, cursor, err)
}

// recordFeatureReply 是 callVia 的 onReply,跑在回包所在连接的 RecvLoop 上,不得阻塞。它把本次回包交给 feature 快照的
// 权威分发函数:排队 / 自动战斗归 HandleFeatureBattleMessage,大厅场景 RPC 归 HandleFeatureMessage,两者认领的消息号
// 不相交。信封拒绝也交:两个函数都先看信封,记成带 tip 的失败快照,不会把空 body 解成全零响应。
func (s *featureSmokeSession) recordFeatureReply(reply *base.MessageContent, _ error) {
	if handler.HandleFeatureBattleMessage(s.player, reply) {
		return
	}
	handler.HandleFeatureMessage(s.player, reply)
}

// featureCallResult 把 callVia 的结局(err 的形状同 pkg.GameClient.CallSyncThen)翻译成返回值,cursor 是发送前的
// FeatureSequence。拒绝分三种报:信封拒绝(gate / 路由服 / 战斗直连面在信封里给了 tip;通常请求没执行,但 1003
// 也可能表示结局未知 —— RPC0 或 gRPC 调用超时 / 断开后回的,业务可能已执行,不能当作没发生而重试)返回包裹
// *pkg.ServerTipError 的 error;业务拒绝(响应体 tip)返回 *featureServerError;快照记下的其余失败(响应残缺、
// 解码失败)返回普通 error。没有回包的结局(超时 / 连接被关)分开报,error 包裹 pkg 的哨兵。
func (s *featureSmokeSession) featureCallResult(id uint32, cursor uint64, err error) (proto.Message, error) {
	var envelope *pkg.ServerTipError
	switch {
	case errors.As(err, &envelope):
		return nil, fmt.Errorf("feature request envelope rejected message_id=%d: %w", id, err)
	case errors.Is(err, pkg.ErrRPCTimeout):
		return nil, fmt.Errorf("feature response timed out message_id=%d: %w", id, err)
	case errors.Is(err, pkg.ErrRPCDisconnected):
		return nil, fmt.Errorf("feature connection closed before response message_id=%d: %w", id, err)
	case err != nil:
		return nil, fmt.Errorf("feature call failed message_id=%d: %w", id, err)
	}
	snapshot, ok := s.player.FeatureResponse(id, cursor)
	if !ok {
		// 回包到了却没有新快照:这个消息号不归 HandleFeature* 认领。不能拿游标之前的旧快照充当本次结果。
		return nil, fmt.Errorf("feature response was not recorded message_id=%d", id)
	}
	if snapshot.TipID != 0 {
		return nil, &featureServerError{id, snapshot.TipID}
	}
	if snapshot.Failure != "" {
		return nil, fmt.Errorf("feature response rejected message_id=%d: %s", id, snapshot.Failure)
	}
	if snapshot.Response == nil {
		return nil, fmt.Errorf("missing feature response message_id=%d", id)
	}
	return snapshot.Response, nil
}

func (s *featureSmokeSession) readBag(bagType uint32) (*scene.BagInfo, error) {
	body, err := s.call(game.SceneBagClientPlayerGetBagMessageId, &scene.GetBagRequest{BagType: bagType})
	if err != nil {
		return nil, err
	}
	bag := body.(*scene.GetBagResponse).Bag
	if bag.GetLayout() == nil || bag.Layout.BagType != bagType {
		return nil, fmt.Errorf("bag type does not match request")
	}
	if _, err := featureBagInventory(bag); err != nil {
		return nil, err
	}
	return bag, nil
}

func (s *featureSmokeSession) close() {
	if s == nil || s.gc == nil {
		return
	}
	_ = leaveGame(s.gc, s.stats)
	sendDisconnectBestEffort(s.gc)
	gameobject.PlayerList.Delete(s.gc.PlayerId)
	pkg.Clients.Unregister(s.gc.PlayerId, s.gc)
	s.gc.Close()
}

func featureMission(list *scene.GetMissionListResponse, scope, id uint32) *scene.PlayerMissionInfo {
	for _, mission := range list.GetMissions() {
		if mission.GetScope() == scope && mission.GetMissionId() == id {
			return mission
		}
	}
	return nil
}

// Counts are grouped by stack-compatible configuration. Instance IDs may merge
// during sorting; every layout reference must still resolve to one real item.
func featureBagInventory(bag *scene.BagInfo) (map[[3]uint32]uint64, error) {
	if bag.GetLayout() == nil {
		return nil, fmt.Errorf("bag has no layout")
	}
	totals := make(map[[3]uint32]uint64)
	items := make(map[uint64]bool)
	for _, item := range bag.Items {
		if item == nil || item.ItemId == 0 || item.Count == 0 || items[item.ItemId] {
			return nil, fmt.Errorf("invalid or duplicated bag item")
		}
		items[item.ItemId] = true
		totals[[3]uint32{item.ConfigId, item.EquipKind, item.MaxStack}] += uint64(item.Count)
	}
	positions := make(map[uint32]bool)
	references := make(map[uint64]bool)
	for _, slot := range bag.Layout.Slots {
		if slot == nil || slot.Slot >= bag.Layout.Capacity || positions[slot.Slot] || !items[slot.ItemId] || references[slot.ItemId] || slot.Width != 1 || slot.Height != 1 {
			return nil, fmt.Errorf("bag slot does not reference one valid unique item")
		}
		positions[slot.Slot] = true
		references[slot.ItemId] = true
	}
	if len(references) != len(items) {
		return nil, fmt.Errorf("bag contains unplaced items")
	}
	return totals, nil
}

func featureBagsEqual(a, b *scene.BagInfo) bool {
	if a == nil || b == nil || a.Layout == nil || b.Layout == nil {
		return false
	}
	x, y := proto.Clone(a).(*scene.BagInfo), proto.Clone(b).(*scene.BagInfo)
	for _, bag := range []*scene.BagInfo{x, y} {
		sort.Slice(bag.Items, func(i, j int) bool { return bag.Items[i].ItemId < bag.Items[j].ItemId })
		sort.Slice(bag.Layout.Slots, func(i, j int) bool { return bag.Layout.Slots[i].Slot < bag.Layout.Slots[j].Slot })
	}
	return proto.Equal(x, y)
}

func featureMissionProgressEqual(a, b *scene.GetMissionListResponse) bool {
	if len(a.GetMissions()) != len(b.GetMissions()) {
		return false
	}
	for _, mission := range a.GetMissions() {
		other := featureMission(b, mission.GetScope(), mission.GetMissionId())
		if other == nil || mission.GetStatus() != other.Status || len(mission.Objectives) != len(other.Objectives) {
			return false
		}
		for i, goal := range mission.Objectives {
			if !proto.Equal(goal, other.Objectives[i]) {
				return false
			}
		}
	}
	return true
}
