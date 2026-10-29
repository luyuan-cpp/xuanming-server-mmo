package main

// 本测试驱动只发送正式客户端协议；SQL、归属判定和进程暂停由独立控制器负责。
// 每个角色独占进程，避免两个设备共享 pkg.Clients / 重定向全局回调。
import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"proto/login"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
)

type ownershipRobotPhase struct {
	Run    string `json:"run"`
	Player uint64 `json:"player_id"`
	Gold   uint64 `json:"gold"`
	Base   uint64 `json:"base_gold"`
	Gate   string `json:"gate"`
}

func TestFullstackOwnershipFaultRobot(t *testing.T) {
	if os.Getenv("OWNERSHIP_FAULT_REQUIRE") != "1" {
		t.Skip("未开启专用真实节点故障演练")
	}
	role := os.Getenv("OWNERSHIP_FAULT_ROLE")
	if role != "delay" && role != "zombie-source" && role != "zombie-target" {
		t.Fatal("未知专用角色")
	}
	dir, run := os.Getenv("OWNERSHIP_FAULT_DIR"), os.Getenv("OWNERSHIP_FAULT_RUN")
	if !filepath.IsAbs(dir) || len(run) != 32 || os.Getenv("OWNERSHIP_FAULT_MODE") != "dev" {
		t.Fatal("必须提供本轮独立目录、运行号和 dev 声明")
	}
	cfg, err := config.Load(os.Getenv("OWNERSHIP_FAULT_CONFIG"))
	if err != nil {
		t.Fatal("专用机器人配置无效")
	}
	u := strings.TrimPrefix(cfg.GatewayAddr, "http://")
	host, _, err := net.SplitHostPort(u)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || !strings.HasPrefix(cfg.GatewayAddr, "http://") {
		t.Fatal("网关必须是显式本机 HTTP 地址")
	}
	if cfg.TravelSmoke.HomeZone != 990017 || cfg.TravelSmoke.VisitZone != 990018 || cfg.AuthType != "password" {
		t.Fatal("仅允许两个专用 zone 和开发密码认证")
	}
	account := os.Getenv("OWNERSHIP_FAULT_ACCOUNT")
	cfg.Password = os.Getenv("OWNERSHIP_FAULT_PASSWORD")
	if !strings.HasPrefix(account, "robot_assetarrival_fault_") || cfg.Password == "" {
		t.Fatal("专用账号或环境凭据缺失")
	}
	cfg.LoadTables()
	cfg.ZoneID = 990017
	if role == "zombie-target" {
		cfg.ZoneID = 990018
	}
	loginTestCfg = cfg
	stats := metrics.NewStats()
	robotStatsRef = stats
	ctx, cancel := context.WithTimeout(context.Background(), 390*time.Second)
	defer cancel()
	phase := func(name string, p ownershipRobotPhase) {
		p.Run = run
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal("阶段序列化失败")
		}
		path := filepath.Join(dir, name+".json")
		f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("阶段文件已有或无法创建")
		}
		_, writeErr := f.Write(b)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil || os.Rename(path+".tmp", path) != nil {
			t.Fatal("阶段提交失败")
		}
	}
	wait := func(name string, bot *travelSmokeBot) {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		keepalive := time.NewTicker(8 * time.Second)
		defer keepalive.Stop()
		for {
			b, err := os.ReadFile(filepath.Join(dir, name+".json"))
			if err == nil {
				var p ownershipRobotPhase
				if json.Unmarshal(b, &p) != nil || p.Run != run {
					t.Fatal("控制阶段不属于本轮")
				}
				return
			}
			if !os.IsNotExist(err) {
				t.Fatal("无法读取控制阶段")
			}
			select {
			case <-ctx.Done():
				t.Fatal("机器人阶段截止，保留现场")
			case <-ticker.C:
			case <-keepalive.C:
				// 仅 login 刷新协议维持 gate 会话；等待期间不向暂停 scene 发请求。
				// 被新设备替换后的旧连接可以失效，不以发送成功充当仍在线的证据。
				if bot != nil {
					_, token, _, _ := bot.gc.SnapshotTokens()
					if token != "" {
						_ = bot.gc.SendRequest(game.ClientPlayerLoginRefreshTokenMessageId, &login.RefreshTokenRequest{RefreshToken: token})
					}
				}
			}
		}
	}
	var bot *travelSmokeBot
	defer func() {
		if bot != nil {
			ownershipCloseRobot(bot)
		}
	}()
	var base uint64
	if role == "zombie-target" {
		b, err := os.ReadFile(filepath.Join(dir, "zombie-source-ready.json"))
		var source ownershipRobotPhase
		if err != nil || json.Unmarshal(b, &source) != nil || source.Run != run || source.Player == 0 {
			t.Fatal("源设备身份缺失")
		}
		bot, err = ownershipStrictRobot(cfg, account, source.Player, stats)
		if err != nil {
			t.Fatal("B设备正式替换登录失败")
		}
		base, err = bot.readGold()
		if err != nil || base != source.Base {
			t.Fatal("新设备没有从已提交旧基线加载，或读到了旧实体未提交的11金币")
		}
		gold, err := bot.addGold(7)
		if err != nil || gold != base+7 {
			t.Fatal("新设备金币变更失败")
		}
		phase("zombie-target-written", ownershipRobotPhase{Player: bot.homePlayer, Base: base, Gold: gold, Gate: bot.gateAddr()})
		wait("zombie-target-flush", bot)
		id := bot.homePlayer
		ownershipCloseRobot(bot)
		bot = nil
		phase("zombie-target-done", ownershipRobotPhase{Player: id, Base: base, Gold: gold})
		return
	}
	// 初次建角后先正式保存、重登，确保控制器拿到的是可强读的完整 SQL 基线。
	bot, err = travelSmokeLogin(cfg, account, stats)
	if err != nil {
		t.Fatal("源设备正式登录失败")
	}
	if _, err := bot.addGold(1); err != nil {
		t.Fatal("基线初始化失败")
	}
	id := bot.homePlayer
	ownershipCloseRobot(bot)
	bot = nil
	select {
	case <-ctx.Done():
		t.Fatal("初始化截止")
	case <-time.After(2 * time.Second):
	}
	bot, err = ownershipStrictRobot(cfg, account, id, stats)
	if err != nil {
		t.Fatal("基线严格重登失败")
	}
	base, err = bot.readGold()
	if err != nil {
		t.Fatal("基线金币读取失败")
	}
	if role == "delay" {
		phase("delay-ready", ownershipRobotPhase{Player: id, Base: base, Gold: base, Gate: bot.gateAddr()})
		wait("delay-go", bot)
		if gold, err := bot.addGold(11); err != nil || gold != base+11 {
			t.Fatal("去程金币变更失败")
		}
		from := bot.gateAddr()
		hop, err := bot.travelTo(990018, cfg.TravelSmoke.SceneConfigId)
		if err != nil {
			t.Fatal("等待SQL ACK后的真实去程失败")
		}
		if _, err := bot.verifyArrival(hop, 990018, cfg.TravelSmoke.SceneConfigId, from); err != nil {
			t.Fatal("真实去程票据或落点错误")
		}
		if gold, err := bot.readGold(); err != nil || gold != base+11 {
			t.Fatal("去程资产丢失或重复")
		}
		if gold, err := bot.addGold(7); err != nil || gold != base+18 {
			t.Fatal("访客区金币变更失败")
		}
		from = bot.gateAddr()
		hop, err = bot.travelTo(990017, cfg.TravelSmoke.SceneConfigId)
		if err != nil {
			t.Fatal("真实回程失败")
		}
		if _, err := bot.verifyArrival(hop, 990017, cfg.TravelSmoke.SceneConfigId, from); err != nil {
			t.Fatal("真实回程票据或落点错误")
		}
		if gold, err := bot.readGold(); err != nil || gold != base+18 {
			t.Fatal("回程资产丢失或重复")
		}
		phase("delay-done", ownershipRobotPhase{Player: id, Base: base, Gold: base + 18, Gate: bot.gateAddr()})
		wait("delay-finish", bot)
		return
	}
	// 独立的准备变更让控制器能确认“当前实体/epoch 的一次真实周期存盘”，
	// 不能把前一个实体退出时的 ACK 冒充本周期的起点。下一次周期才会尝试后面的旧写。
	prepared, err := bot.addGold(2)
	if err != nil || prepared != base+2 {
		t.Fatal("周期基线准备失败")
	}
	base = prepared
	phase("zombie-source-baseline", ownershipRobotPhase{Player: id, Base: base, Gold: base, Gate: bot.gateAddr()})
	wait("zombie-source-dirty", bot)
	if gold, err := bot.addGold(11); err != nil || gold != base+11 {
		t.Fatal("旧实体脏内存准备失败")
	}
	phase("zombie-source-ready", ownershipRobotPhase{Player: id, Base: base, Gold: base + 11, Gate: bot.gateAddr()})
	wait("zombie-source-finish", bot)
}

func ownershipCloseRobot(bot *travelSmokeBot) {
	_ = leaveGame(bot.gc, bot.stats)
	sendDisconnectBestEffort(bot.gc)
	pkg.Clients.Unregister(bot.homePlayer, bot.gc)
	gameobject.PlayerList.Delete(bot.homePlayer)
	bot.gc.Close()
}

func ownershipStrictRobot(cfg *config.Config, account string, want uint64, stats *metrics.Stats) (*travelSmokeBot, error) {
	host, portText, payload, sig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, err
	}
	gc, err := connectAndVerify(host, port, account, payload, sig)
	if err != nil {
		return nil, err
	}
	if err := travelSmokeStrictRelogin(gc, cfg.Password, stats, want); err != nil {
		gc.Close()
		return nil, fmt.Errorf("严格重登失败")
	}
	bot := &travelSmokeBot{account: account, password: cfg.Password, gc: gc, player: gameobject.NewPlayer(want), stats: stats, homePlayer: want,
		hopTimeout: time.Duration(cfg.TravelSmoke.HopTimeoutSeconds) * time.Second}
	pkg.SetRedirectRelogin(bot.relogin)
	gameobject.PlayerList.Set(want, bot.player)
	pkg.Clients.Register(want, gc)
	go gc.RecvLoop(bot.onMessage)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := bot.player.WaitSceneReady(ctx); err != nil {
		ownershipCloseRobot(bot)
		return nil, err
	}
	return bot, nil
}
