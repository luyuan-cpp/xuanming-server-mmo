package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"proto/common/component"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
)

// 纯离线预检仅运行本地MSET查询器，不读认证配置，也不执行登录或移动消息。
func TestAuthoredFestivalNavMeshReferencePreflight(t *testing.T) {
	if os.Getenv("AUTHORED_NAV_OFFLINE") != "1" {
		t.Skip("Set AUTHORED_NAV_OFFLINE=1 explicitly to inspect local reference geometry")
	}
	reportPath := os.Getenv("AUTHORED_NAV_OFFLINE_REPORT")
	if reportPath == "" {
		t.Fatal("AUTHORED_NAV_OFFLINE_REPORT is required")
	}
	report := map[string]any{"result": "RUNNING", "scope": "Offline original authored-map MSET references only; no login or movement network requests", "maps": []any{}}
	defer func() {
		if t.Failed() {
			report["result"] = "FAIL"
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.MkdirAll(filepath.Dir(reportPath), 0700)
		}
		if err == nil {
			err = os.WriteFile(reportPath, append(data, '\n'), 0600)
		}
		if err != nil {
			t.Error("Unable to write the offline reference report")
		}
	}()
	deliveryPath := os.Getenv("AUTHORED_NAV_DELIVERY")
	var delivery authoredDelivery
	if err := authoredReadJSON(deliveryPath, &delivery); err != nil || delivery.Result != "PASS" || len(delivery.Maps) != 2 {
		t.Fatal("A successful two-map delivery is required")
	}
	pairedPath := os.Getenv("AUTHORED_NAV_PAIRED_REPORT")
	if pairedPath == "" {
		pairedPath = filepath.Join(filepath.Dir(filepath.Dir(deliveryPath)), "paired-server", "paired-verification.json")
	}
	var paired authoredPaired
	if err := authoredReadJSON(pairedPath, &paired); err != nil || paired.Result != "PASS" || paired.UnityQueriesSHA != delivery.UnityQueriesSHA || delivery.UnityQueriesSHA == "" {
		t.Fatal("The paired report does not match the source delivery")
	}
	query := os.Getenv("AUTHORED_NAV_QUERY_EXE")
	if query == "" {
		query = filepath.Join("..", "tools", "navmesh_baker", "build", "Release", "navmesh_query.exe")
	}
	query, _ = filepath.Abs(query)
	report["deliverySha256"], _ = authoredSHA(deliveryPath)
	report["pairedReportSha256"], _ = authoredSHA(pairedPath)
	report["queryExeSha256"], _ = authoredSHA(query)
	applicable := 0
	for _, item := range delivery.Maps {
		mset := filepath.Join(item.ExportDir, "scene-nav.bin")
		expected := ""
		for _, ref := range paired.Cases {
			if ref.Name == item.MapKey {
				expected = ref.MSetSHA
			}
		}
		if sha, err := authoredSHA(mset); err != nil || sha != expected || expected == "" {
			t.Fatal("A local MSET differs from the locked paired receipt")
		}
		entry := map[string]any{"sceneConfigId": item.SceneConfigID, "mapKey": item.MapKey, "referenceMsetSha256": expected, "spawnUnity": item.SpawnUnity}
		before, beyond, hit, err := authoredWall(query, mset, item)
		if err != nil {
			entry["result"] = "NOT_APPLICABLE"
			entry["reason"] = err.Error()
		} else {
			dir := authoredDirection(item.SpawnUnity, hit)
			normal := authoredAdd(item.SpawnUnity, dir, 1)
			q, err := authoredQuery(query, mset, []authoredVec{before, beyond, normal}, [][2]authoredVec{{item.SpawnUnity, before}, {item.SpawnUnity, normal}})
			if err != nil || len(q.Points) != 3 || !q.Points[0].OnMesh || q.Points[1].OnMesh || !q.Points[2].OnMesh || len(q.Routes) != 2 || !q.Routes[0].Complete || !q.Routes[1].Complete || q.Routes[0].Ray == nil || q.Routes[0].Ray.Hit || q.Routes[1].Ray == nil || q.Routes[1].Ray.Hit {
				t.Fatal("The selected barrier or safe one-metre reference leg failed offline validation")
			}
			entry["result"] = "PASS"
			entry["approachUnity"] = before
			entry["blockedTargetUnity"] = beyond
			entry["rayHitUnity"] = hit
			entry["safeOneMetreEndUnity"] = normal
			entry["safeApproachDistance"] = authoredDistance(item.SpawnUnity, before)
			applicable++
		}
		report["maps"] = append(report["maps"].([]any), entry)
	}
	report["result"] = "PASS"
	if applicable != len(delivery.Maps) {
		report["result"] = "PARTIAL"
	}
}

// 仅显式启用时才发网络；普通go test及只编译不会连接服务器。
// 输入：AUTHORED_NAV_E2E=1、AUTHORED_NAV_ROBOT_CONFIG、AUTHORED_NAV_DELIVERY、
// AUTHORED_NAV_REPORT；可选AUTHORED_NAV_PAIRED_REPORT和AUTHORED_NAV_QUERY_EXE。
// 仅创建本次随机专用测试角色，不读取或修改既有用户角色；报告不写账号和凭据。
func TestAuthoredFestivalNavMeshE2E(t *testing.T) {
	if os.Getenv("AUTHORED_NAV_E2E") != "1" {
		t.Skip("Set AUTHORED_NAV_E2E=1 explicitly to run native backend probes")
	}
	restoreLogger := zap.ReplaceGlobals(zap.NewNop())
	defer restoreLogger()
	report := map[string]any{"result": "RUNNING", "scope": "Native backend login, scene entry and movement only; no Unity Player or visual acceptance", "maps": []any{}}
	reportPath := os.Getenv("AUTHORED_NAV_REPORT")
	if reportPath == "" {
		t.Fatal("AUTHORED_NAV_REPORT is required")
	}
	defer func() {
		if t.Failed() || report["result"] != "PASS" {
			report["result"] = "FAIL"
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.MkdirAll(filepath.Dir(reportPath), 0700)
		}
		if err == nil {
			err = os.WriteFile(reportPath, append(data, '\n'), 0600)
		}
		if err != nil {
			t.Error("Unable to write the backend probe report")
		}
	}()
	cfg, err := config.Load(os.Getenv("AUTHORED_NAV_ROBOT_CONFIG"))
	if err != nil {
		t.Fatal("Unable to load the controlled robot configuration")
	}
	u, err := url.Parse(cfg.GatewayAddr)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Port() != "18085" || !authoredLoopback(u.Hostname()) || cfg.ZoneID == 0 || !cfg.UseHttpLogin || cfg.AuthType != "password" || strings.TrimSpace(cfg.Password) == "" || !strings.Contains(cfg.AccountFmt, "%") {
		t.Fatal("Probe requires canonical loopback gateway 18085, explicit zone, controlled HTTP password authentication and synthetic account format")
	}
	deliveryPath, err := filepath.Abs(os.Getenv("AUTHORED_NAV_DELIVERY"))
	if err != nil || os.Getenv("AUTHORED_NAV_DELIVERY") == "" {
		t.Fatal("AUTHORED_NAV_DELIVERY is required")
	}
	var delivery authoredDelivery
	if err = authoredReadJSON(deliveryPath, &delivery); err != nil || delivery.SchemaVersion != 1 || delivery.Result != "PASS" || len(delivery.Maps) != 2 {
		t.Fatal("The locked two-map delivery receipt is missing or invalid")
	}
	pairedPath := os.Getenv("AUTHORED_NAV_PAIRED_REPORT")
	if pairedPath == "" {
		pairedPath = filepath.Join(filepath.Dir(filepath.Dir(deliveryPath)), "paired-server", "paired-verification.json")
	}
	var paired authoredPaired
	if err = authoredReadJSON(pairedPath, &paired); err != nil || paired.Result != "PASS" {
		t.Fatal("The corresponding successful paired-server receipt is required")
	}
	if delivery.UnityQueriesSHA == "" || paired.UnityQueriesSHA != delivery.UnityQueriesSHA {
		t.Fatal("The paired-server receipt does not bind the common delivery queries")
	}
	queryExe := os.Getenv("AUTHORED_NAV_QUERY_EXE")
	if queryExe == "" {
		queryExe = filepath.Join("..", "tools", "navmesh_baker", "build", "Release", "navmesh_query.exe")
	}
	queryExe, _ = filepath.Abs(queryExe)
	if _, err = os.Stat(queryExe); err != nil {
		t.Fatal("The matching native navmesh_query executable is required")
	}
	report["deliverySha256"], _ = authoredSHA(deliveryPath)
	report["pairedReportSha256"], _ = authoredSHA(pairedPath)
	report["queryExeSha256"], _ = authoredSHA(queryExe)
	report["gateway"] = cfg.GatewayAddr
	report["zone"] = cfg.ZoneID
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal("Unable to allocate isolated test identities")
	}
	oldCfg := loginTestCfg
	loginTestCfg = cfg
	defer func() { loginTestCfg = oldCfg }()
	seenScenes := make(map[uint32]bool)
	for _, item := range delivery.Maps {
		if (item.SceneConfigID == 26000 && item.MapKey != "26000-mid-autumn") || (item.SceneConfigID == 26008 && item.MapKey != "26008-spring") ||
			(item.SceneConfigID != 26000 && item.SceneConfigID != 26008) || seenScenes[item.SceneConfigID] || item.SpawnUnity[1] != 0 || item.SourceAssetSHA == "" || item.GeometryHash == "" {
			t.Fatal("Unexpected authored-map identity or spawn contract")
		}
		seenScenes[item.SceneConfigID] = true
		var reference *authoredPairedCase
		for i := range paired.Cases {
			if paired.Cases[i].Name == item.MapKey {
				reference = &paired.Cases[i]
			}
		}
		if reference == nil {
			t.Fatal("A map is absent from the paired-server receipt")
		}
		// 地图之间使用不同的新角色，避免旧地图存盘落点影响出生点验收。
		// 前缀来自受控配置，随机后缀确保不复用既有角色；不将账号写入日志。
		prefix := fmt.Sprintf("%s_%x_%d_", fmt.Sprintf(cfg.AccountFmt, time.Now().UnixNano()), nonce, item.SceneConfigID)
		accounts := []string{prefix + "a", prefix + "b"}
		mapReport, runErr := authoredProbeMap(cfg, item, *reference, queryExe, accounts)
		report["maps"] = append(report["maps"].([]any), mapReport)
		if runErr != nil {
			message := authoredRedact(runErr.Error(), append(accounts, cfg.Password)...)
			report["error"] = message
			t.Fatal(message)
		}
	}
	if sha, _ := authoredSHA(deliveryPath); sha != report["deliverySha256"] {
		t.Fatal("The common delivery changed during the network probe")
	}
	report["result"] = "PASS"
}

type authoredVec [3]float64
type authoredDelivery struct {
	SchemaVersion   int           `json:"schemaVersion"`
	Result          string        `json:"result"`
	UnityQueriesSHA string        `json:"pairedUnityQueriesSha256"`
	Maps            []authoredMap `json:"maps"`
}
type authoredMap struct {
	SceneConfigID  uint32                               `json:"sceneConfigId"`
	MapKey         string                               `json:"mapKey"`
	ExportDir      string                               `json:"exportDirectory"`
	SourceAssetSHA string                               `json:"sourceAssetSha256"`
	GeometryHash   string                               `json:"authoringGeometryHash"`
	SourceSHA      string                               `json:"sourceSha256"`
	SpawnUnity     authoredVec                          `json:"spawnUnity"`
	World          struct{ X, Z, Width, Depth float64 } `json:"worldRect"`
}
type authoredPaired struct {
	Result          string               `json:"result"`
	UnityQueriesSHA string               `json:"unityQueriesSha256"`
	Cases           []authoredPairedCase `json:"cases"`
}
type authoredPairedCase struct {
	Name       string `json:"name"`
	MSetSHA    string `json:"msetSha256"`
	ServerData struct {
		Points []struct {
			Input  authoredVec `json:"input"`
			OnMesh bool        `json:"onMesh"`
		} `json:"points"`
	} `json:"serverQueries"`
}
type authoredNative struct {
	Points []struct {
		OnMesh bool `json:"onMesh"`
	} `json:"points"`
	Routes []struct {
		Complete bool `json:"complete"`
		Ray      *struct {
			Hit      bool        `json:"hit"`
			Position authoredVec `json:"position"`
		} `json:"raycast"`
	} `json:"routes"`
}

func authoredProbeMap(cfg *config.Config, item authoredMap, reference authoredPairedCase, queryExe string, accounts []string) (map[string]any, error) {
	out := map[string]any{"sceneConfigId": item.SceneConfigID, "mapKey": item.MapKey, "result": "FAIL", "sourceAssetSha256": item.SourceAssetSHA, "authoringGeometryHash": item.GeometryHash}
	var source struct{ GeometrySHA, AuthoringSHA, ArgumentsSHA string }
	var raw map[string]json.RawMessage
	if err := authoredReadJSON(filepath.Join(item.ExportDir, "source.json"), &raw); err != nil {
		return out, fmt.Errorf("read bound source: %w", err)
	}
	_ = json.Unmarshal(raw["geometrySha256"], &source.GeometrySHA)
	_ = json.Unmarshal(raw["authoringSha256"], &source.AuthoringSHA)
	_ = json.Unmarshal(raw["serverBakeArgumentsSha256"], &source.ArgumentsSHA)
	for file, expected := range map[string]string{"source.json": item.SourceSHA, "world-y-up.obj": source.GeometrySHA, "navigation.json": source.AuthoringSHA, "server-bake-args.json": source.ArgumentsSHA, "scene-nav.bin": reference.MSetSHA} {
		got, err := authoredSHA(filepath.Join(item.ExportDir, file))
		if err != nil || expected == "" || got != expected {
			return out, fmt.Errorf("locked reference file mismatch: %s", file)
		}
	}
	mset := filepath.Join(item.ExportDir, "scene-nav.bin")
	wallFrom, wallTo, wallHit, err := authoredWall(queryExe, mset, item)
	if err != nil {
		return out, err
	}
	out["referenceMsetSha256"] = reference.MSetSHA
	out["barrierReference"] = map[string]any{"approach": wallFrom, "blockedTarget": wallTo, "rayHit": wallHit}
	stats := metrics.NewStats()
	var bots []*teamFollowSmokeBot
	defer func() {
		for _, bot := range bots {
			bot.gc.Close()
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
		}
		time.Sleep(100 * time.Millisecond)
	}()
	for i, account := range accounts {
		bot, err := authoredLogin(cfg, string(rune('A'+i)), account, stats)
		if err != nil {
			return out, fmt.Errorf("synthetic actor %c login: %w", 'A'+i, err)
		}
		bots = append(bots, bot)
		host, port, err := net.SplitHostPort(bot.gate)
		if err != nil || port != "28000" || !authoredLoopback(host) {
			return out, fmt.Errorf("canonical gate assignment mismatch")
		}
	}
	a, b := bots[0], bots[1]
	sceneID, entity, position, err := authoredEnter(a, item.SceneConfigID, 0)
	if err != nil {
		return out, fmt.Errorf("actor A scene entry: %w", err)
	}
	if _, _, peerSpawn, err := authoredEnter(b, item.SceneConfigID, sceneID); err != nil {
		return out, fmt.Errorf("actor B scene entry: %w", err)
	} else if !authoredNear(authoredFromServer(peerSpawn), item.SpawnUnity, .02) {
		return out, fmt.Errorf("actor B authoritative spawn differs from delivery")
	}
	if !authoredNear(authoredFromServer(position), item.SpawnUnity, .02) {
		return out, fmt.Errorf("actor A authoritative spawn differs from delivery")
	}
	out["sceneInstanceId"] = strconv.FormatUint(sceneID, 10)
	out["authoritativeSpawnUnity"] = authoredFromServer(position)
	// 第一段仅1米，观察者仍在同一AOI内，核对服务端广播而不是只靠没有Ack推断接受。
	direction := authoredDirection(item.SpawnUnity, wallHit)
	normalTarget := authoredAdd(item.SpawnUnity, direction, 1)
	observerMark := b.recordMark()
	actual, acks, err := authoredWalk(a, position, authoredToServer(normalTarget), false)
	if err != nil || acks != 0 {
		return out, fmt.Errorf("safe one-metre movement was corrected or failed: acknowledgements=%d error=%v", acks, err)
	}
	stopped, err := teamFollowSmokeWait(b, observerMark, game.SceneMovementClientPlayerNotifyActorMoveMessageId, 5*time.Second,
		func() *scene.ActorMoveS2C { return &scene.ActorMoveS2C{} }, func(m *scene.ActorMoveS2C) bool { return m.GetEntity() == entity && m.GetMoveState() == 0 })
	if err != nil || !authoredNear(authoredFromVector(stopped.GetTransform().GetLocation()), normalTarget, .02) {
		return out, fmt.Errorf("observer did not confirm the accepted one-metre endpoint")
	}
	out["safeMove"] = map[string]any{"distance": 1, "moveAcks": acks, "observerConfirmed": true, "endUnity": normalTarget}
	actual, acks, err = authoredWalk(a, actual, authoredToServer(wallFrom), false)
	if err != nil || acks != 0 {
		return out, fmt.Errorf("reference-clear barrier approach failed: acknowledgements=%d error=%v", acks, err)
	}
	actual, acks, err = authoredWalk(a, actual, authoredToServer(wallTo), true)
	if err != nil || acks == 0 {
		return out, fmt.Errorf("cross-obstacle movement was not rejected: acknowledgements=%d error=%v", acks, err)
	}
	accepted := authoredFromServer(actual)
	if authoredNear(accepted, wallTo, .5) || authoredDot(authoredSub(accepted, wallHit), direction) > .1 {
		return out, fmt.Errorf("authoritative position crossed the reference barrier")
	}
	query, err := authoredQuery(queryExe, mset, []authoredVec{accepted}, nil)
	if err != nil || len(query.Points) != 1 || !query.Points[0].OnMesh {
		return out, fmt.Errorf("corrected position is not on the bound reference navigation")
	}
	out["blockedMove"] = map[string]any{"moveAcks": acks, "acceptedUnity": accepted, "requestedUnity": wallTo, "result": "PASS", "reportedSpeedLimit": 3, "reportIntervalMs": 250}
	// 当前真实地图若没有已证实的断岛，明确N/A，不把图外点冒充独立可走岛。
	out["disconnectedIsland"] = map[string]any{"result": "NOT_APPLICABLE", "reason": "No disconnected on-mesh endpoint in the locked sampled receipt"}
	for _, point := range reference.ServerData.Points {
		if !point.OnMesh || authoredNear(point.Input, item.SpawnUnity, .02) {
			continue
		}
		q, err := authoredQuery(queryExe, mset, nil, [][2]authoredVec{{item.SpawnUnity, point.Input}})
		if err != nil || len(q.Routes) != 1 {
			return out, fmt.Errorf("unable to classify sampled island connectivity")
		}
		if q.Routes[0].Complete {
			continue
		}
		// 从原射线返回出生点后，按真实节拍尝试向断岛移动；遇到首个Ack即停止。
		actual, _, err = authoredWalk(a, actual, position, false)
		if err != nil {
			return out, fmt.Errorf("unable to return along the clear approach before island probe: %w", err)
		}
		actual, acks, err = authoredWalk(a, actual, authoredToServer(point.Input), true)
		if err != nil || acks == 0 || authoredNear(authoredFromServer(actual), point.Input, .5) {
			return out, fmt.Errorf("disconnected island movement was not rejected")
		}
		out["disconnectedIsland"] = map[string]any{"result": "PASS", "targetUnity": point.Input, "acceptedUnity": authoredFromServer(actual), "moveAcks": acks}
		break
	}
	out["result"] = "PASS"
	return out, nil
}

// 只在本探针复用正式HTTP登录与EnterGame流程，不改变旧team冒烟的认证路径。
func authoredLogin(cfg *config.Config, name, account string, stats *metrics.Stats) (*teamFollowSmokeBot, error) {
	host, portText, payload, signature, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("canonical gate assignment: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port != 28000 || !authoredLoopback(host) {
		return nil, fmt.Errorf("canonical gate assignment mismatch")
	}
	gc, err := connectAndVerify(host, port, account, payload, signature)
	if err != nil {
		return nil, fmt.Errorf("canonical gate connection: %w", err)
	}
	if err := loginAndEnterViaHttpGateway(gc, cfg, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("canonical HTTP login and entry: %w", err)
	}
	bot := &teamFollowSmokeBot{teamSmokeBot: &teamSmokeBot{name: name, account: account, zone: cfg.ZoneID, gate: net.JoinHostPort(host, portText), gc: gc,
		player: gameobject.NewPlayer(gc.PlayerId), stats: stats, caller: newSmokeCaller(gc, stats, account, teamSmokeRpcTimeout, errTeamSmokeTimeout)}}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)
	ctx, cancel := context.WithTimeout(context.Background(), teamSmokeSceneReadyTimeout)
	defer cancel()
	if err := bot.player.WaitSceneReady(ctx); err != nil {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait authoritative scene entry: %w", err)
	}
	return bot, nil
}

func authoredEnter(bot *teamFollowSmokeBot, configID uint32, sceneID uint64) (uint64, uint64, *component.Location, error) {
	pushMark, recordMark := bot.mark(), bot.recordMark()
	var response scene.EnterSceneC2SResponse
	tip, err := bot.call(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{SceneInfo: &scene.SceneInfoComp{SceneConfigId: configID, SceneId: sceneID}}, &response)
	if err != nil || tip != 0 || response.GetErrorMessage().GetId() != 0 {
		return 0, 0, nil, fmt.Errorf("EnterScene was not accepted: envelopeTip=%d businessTip=%d error=%v", tip, response.GetErrorMessage().GetId(), err)
	}
	entered, err := bot.waitEnterScene(pushMark, 25*time.Second, func(s *scene.EnterSceneS2C) bool {
		return s.GetSceneInfo().GetSceneConfigId() == configID && (sceneID == 0 || s.GetSceneInfo().GetSceneId() == sceneID)
	})
	if err != nil {
		return 0, 0, nil, err
	}
	self, err := bot.waitActorCreate(recordMark, bot.gc.PlayerId, 20*time.Second)
	if err != nil {
		return 0, 0, nil, err
	}
	time.Sleep(300 * time.Millisecond)
	return entered.GetSceneInfo().GetSceneId(), self.GetEntity(), teamFollowSmokeFromVector(self.GetTransform().GetLocation()), nil
}

func authoredWalk(bot *teamFollowSmokeBot, from, target *component.Location, stopOnAck bool) (*component.Location, int, error) {
	dx, dy := target.GetX()-from.GetX(), target.GetY()-from.GetY()
	distance := math.Hypot(dx, dy)
	if distance < .001 {
		return from, 0, nil
	}
	if distance > 500 {
		return from, 0, fmt.Errorf("probe leg exceeds the bounded movement distance")
	}
	steps := int(math.Ceil(distance / .75))
	velocity := &component.Velocity{X: dx / (float64(steps) * .25), Y: dy / (float64(steps) * .25)}
	mark := bot.recordMark()
	bot.inputSeq++
	if err := bot.sendMove(game.SceneMovementClientPlayerMoveStartMessageId, &scene.MoveStartC2S{StartLocation: from, Velocity: velocity, ClientTimeMs: uint64(time.Now().UnixMilli()), InputSeq: bot.inputSeq}); err != nil {
		return from, 0, err
	}
	position := from
	for i := 1; i <= steps; i++ {
		time.Sleep(250 * time.Millisecond)
		if ack := bot.lastMoveAck(mark); ack != nil {
			position = ack
			break
		}
		fraction := float64(i) / float64(steps)
		position = &component.Location{X: from.GetX() + dx*fraction, Y: from.GetY() + dy*fraction, Z: from.GetZ()}
		bot.inputSeq++
		if err := bot.sendMove(game.SceneMovementClientPlayerMoveSyncMessageId, &scene.MoveSyncC2S{Location: position, Velocity: velocity, ClientTimeMs: uint64(time.Now().UnixMilli()), InputSeq: bot.inputSeq}); err != nil {
			return position, 0, err
		}
	}
	bot.inputSeq++
	if err := bot.sendMove(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{EndLocation: position, ClientTimeMs: uint64(time.Now().UnixMilli()), InputSeq: bot.inputSeq}); err != nil {
		return position, 0, err
	}
	time.Sleep(700 * time.Millisecond)
	count := 0
	for _, record := range bot.recordsSince(mark) {
		if record.messageId == game.SceneMovementClientPlayerNotifyMoveAckMessageId {
			var ack scene.MoveAckS2C
			if err := proto.Unmarshal(record.body, &ack); err != nil {
				return position, count, fmt.Errorf("invalid movement acknowledgement")
			}
			count++
			position = ack.GetServerLocation()
		}
	}
	if count > 0 && !stopOnAck {
		return position, count, fmt.Errorf("reference-clear movement received a correction")
	}
	return position, count, nil
}

func authoredWall(exe, mset string, item authoredMap) (authoredVec, authoredVec, authoredVec, error) {
	length := math.Hypot(item.World.Width, item.World.Depth) * 2
	var rays [][2]authoredVec
	for i := 0; i < 16; i++ {
		a := float64(i) * math.Pi / 8
		rays = append(rays, [2]authoredVec{item.SpawnUnity, authoredAdd(item.SpawnUnity, authoredVec{math.Cos(a), 0, math.Sin(a)}, length)})
	}
	result, err := authoredQuery(exe, mset, nil, rays)
	if err != nil {
		return authoredVec{}, authoredVec{}, authoredVec{}, err
	}
	best := math.Inf(1)
	var before, beyond, hit authoredVec
	for i, route := range result.Routes {
		if route.Ray == nil || !route.Ray.Hit {
			continue
		}
		p := route.Ray.Position
		distance := authoredDistance(p, item.SpawnUnity)
		if distance < 3 || distance >= best {
			continue
		}
		direction := authoredDirection(rays[i][0], rays[i][1])
		candidateBefore, candidateBeyond := authoredAdd(p, direction, -1.5), authoredAdd(p, direction, 1.25)
		if candidateBeyond[0] <= item.World.X+.5 || candidateBeyond[0] >= item.World.X+item.World.Width-.5 || candidateBeyond[2] <= item.World.Z+.5 || candidateBeyond[2] >= item.World.Z+item.World.Depth-.5 {
			continue
		}
		q, err := authoredQuery(exe, mset, []authoredVec{candidateBefore, candidateBeyond}, [][2]authoredVec{{item.SpawnUnity, candidateBefore}})
		if err != nil || len(q.Points) != 2 || !q.Points[0].OnMesh || q.Points[1].OnMesh || len(q.Routes) != 1 || q.Routes[0].Ray == nil || q.Routes[0].Ray.Hit {
			continue
		}
		best, before, beyond, hit = distance, candidateBefore, candidateBeyond, p
	}
	if math.IsInf(best, 1) {
		return before, beyond, hit, fmt.Errorf("no reference-proven interior barrier probe with safe approach exists")
	}
	return before, beyond, hit, nil
}

func authoredQuery(exe, mset string, points []authoredVec, routes [][2]authoredVec) (authoredNative, error) {
	args := []string{"--nav", mset}
	for _, point := range points {
		args = append(args, "--point", authoredCoordinates(point[:]))
	}
	for _, route := range routes {
		values := append(append([]float64{}, route[0][:]...), route[1][:]...)
		args = append(args, "--route", authoredCoordinates(values))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, exe, args...).Output()
	var result authoredNative
	if err == nil {
		err = json.Unmarshal(data, &result)
	}
	return result, err
}

func authoredCoordinates(values []float64) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatFloat(value, 'g', 17, 64)
	}
	return strings.Join(parts, ",")
}
func authoredToServer(v authoredVec) *component.Location {
	return &component.Location{X: v[2], Y: v[0], Z: v[1]}
}
func authoredFromServer(v *component.Location) authoredVec {
	return authoredVec{v.GetY(), v.GetZ(), v.GetX()}
}
func authoredFromVector(v *component.Vector3) authoredVec {
	return authoredVec{v.GetY(), v.GetZ(), v.GetX()}
}
func authoredNear(a, b authoredVec, tolerance float64) bool {
	return math.Abs(a[0]-b[0]) <= tolerance && math.Abs(a[1]-b[1]) <= tolerance && math.Abs(a[2]-b[2]) <= tolerance
}
func authoredAdd(a, direction authoredVec, distance float64) authoredVec {
	return authoredVec{a[0] + direction[0]*distance, a[1] + direction[1]*distance, a[2] + direction[2]*distance}
}
func authoredSub(a, b authoredVec) authoredVec {
	return authoredVec{a[0] - b[0], a[1] - b[1], a[2] - b[2]}
}
func authoredDot(a, b authoredVec) float64      { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func authoredDistance(a, b authoredVec) float64 { return math.Hypot(a[0]-b[0], a[2]-b[2]) }
func authoredDirection(a, b authoredVec) authoredVec {
	d := authoredDistance(a, b)
	return authoredVec{(b[0] - a[0]) / d, 0, (b[2] - a[2]) / d}
}
func authoredLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
func authoredReadJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func authoredSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func authoredRedact(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return regexp.MustCompile(`(?i)(account|password|access_token|refresh_token|token|secret)\s*[:=]\s*[^\s,;]+`).ReplaceAllString(message, "$1=[REDACTED]")
}
