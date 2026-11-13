package main

// asset-arrival-prepare 只为本机全链验收准备专用角色：正式建角/GM/强化/卸装，
// 正式下线重登复核后输出标识，并以只读 GetBag 有界保活。结果不是 SQL 到账凭据；
// 两个 fullstack 测试仍须独立查询真实 TiDB、owner epoch 和 SQL 提交证据。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"proto/scene"
	"robot/config"
	"robot/metrics"
)

type assetArrivalPrepared struct {
	PlayerID uint64 `json:"player_id"`
	ItemGUID uint64 `json:"item_guid"`
	ZoneID   uint32 `json:"zone_id"`
}

// RunAssetArrivalPrepare 不重试资产写；中途失败保留角色现状，后续显式重跑可复用唯一原件。
func RunAssetArrivalPrepare(cfg *config.Config, stats *metrics.Stats) error {
	if err := cfg.ValidateAssetArrivalPrepare(); err != nil {
		return err
	}
	output, err := os.OpenFile(cfg.AssetArrivalPrepare.OutputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("结果文件必须独立且尚不存在: %w", err)
	}
	written := false
	defer func() {
		_ = output.Close()
		if !written {
			_ = os.Remove(cfg.AssetArrivalPrepare.OutputPath)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 总预算包含最多两分钟准备余量；每个既有 RPC 仍使用自己的有界等待，
	// 取消后不再开始资产操作。不使用公共模式的多轮排队重试。
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.AssetArrivalPrepare.HoldSeconds+120)*time.Second)
	defer cancel()
	loginTestCfg = cfg
	session, err := openAssetArrivalSession(cfg, stats)
	if err != nil {
		return err
	}
	defer func() { session.close() }()
	bags, err := equipForgeReadBags(session)
	if err != nil {
		return err
	}
	var itemGUID uint64
	if cfg.AssetArrivalPrepare.Equipment {
		itemGUID, err = prepareAssetArrivalEquipment(ctx, session, bags)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bags, err = equipForgeReadBags(session)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	goal := cfg.AssetArrivalPrepare.GoldTarget
	if have := equipForgeGold(bags.inventory); have < goal {
		if err := equipForgeAddGold(session, int64(goal-have)); err != nil {
			return err
		}
	}
	// 正式离场触发存盘，重登只核对，不在丢失时重新发放资产。
	playerID := session.gc.PlayerId
	session.close()
	if err := sleepCtx(ctx, 2*time.Second); err != nil {
		return err
	}
	session, err = openAssetArrivalSession(cfg, stats)
	if err != nil {
		return err
	}
	if session.gc.PlayerId == 0 || session.gc.PlayerId != playerID {
		return fmt.Errorf("重登没有恢复同一专用角色")
	}
	bags, err = equipForgeReadBags(session)
	if err != nil {
		return err
	}
	if equipForgeGold(bags.inventory) < goal || equipBagFreeSlots(bags.inventory) < 1 {
		return fmt.Errorf("重登后金币不足目标或主背包没有空位")
	}
	if itemGUID != 0 {
		item, worn, err := assetArrivalTarget(bags)
		if err != nil || item == nil || worn || item.GetItemId() != itemGUID || item.GetEnhanceLevel() == 0 {
			return fmt.Errorf("重登后成长原件未完整恢复到主背包")
		}
	}
	prepared := assetArrivalPrepared{PlayerID: playerID, ItemGUID: itemGUID, ZoneID: cfg.ZoneID}
	if err := json.NewEncoder(output).Encode(prepared); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	written = true
	// 文件只含选定标识；不输出账号、凭据、资产载荷或服务端 SQL 证据。
	if err := json.NewEncoder(os.Stdout).Encode(prepared); err != nil {
		return err
	}
	hold := time.NewTimer(time.Duration(cfg.AssetArrivalPrepare.HoldSeconds) * time.Second)
	defer hold.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hold.C:
			return nil
		case <-tick.C:
			// 成交会移走原件，保活只验证连接和只读包响应，不重发、不补资产。
			if _, err := session.getBag(equipSmokeBagInventory); err != nil {
				return fmt.Errorf("专用角色保活失败: %w", err)
			}
		}
	}
}

func validateAssetArrivalAssignment(reply *httpAssignGateResponse) error {
	if reply == nil || reply.Code != 0 || reply.GatePort <= 0 || reply.GatePort > 65535 {
		return fmt.Errorf("本机专用栈未直接分配可用 gate；不进入排队或自动换区")
	}
	if ip := net.ParseIP(reply.GateIP); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("专用 gate 只允许字面量回环 IP")
	}
	return nil
}

func openAssetArrivalSession(cfg *config.Config, stats *metrics.Stats) (*equipSmokeSession, error) {
	// 匿名分配:本机专用栈不开排队,账号在连上 gate 之后才认证(prepareBehaviorClient)。
	reply, err := assignAssetArrivalGate(cfg.GatewayAddr, &httpAssignGateRequest{ZoneID: cfg.ZoneID}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := validateAssetArrivalAssignment(reply); err != nil {
		return nil, err
	}
	gc, player, err := prepareBehaviorClient(reply.GateIP, reply.GatePort, cfg.AssetArrivalPrepare.Account,
		cfg.Password, stats, reply.TokenPayload, reply.TokenSignature)
	if err != nil {
		return nil, err
	}
	return &equipSmokeSession{gc: gc, player: player, stats: stats, lastSent: make(map[uint32]time.Time)}, nil
}

func assignAssetArrivalGate(endpoint string, request *httpAssignGateRequest, timeout time.Duration) (*httpAssignGateResponse, error) {
	client := *sharedHTTPClient
	// 即使本机网关误配跳转，也不得把专用账号或请求带去另一地址。
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return postJSONWithClient(&client, endpoint+"/api/assign-gate", request, timeout)
}

// muduo 的 incoming 在 Close 后仍不关闭。超时只留下一个只读等待协程，
// 调用方关闭本次客户端并让独占准备进程失败退出；不会重试握手或继续资产写。
func waitAssetArrivalVerification(ctx context.Context, verify func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- verify() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}

// 只接受唯一的 1101 原件；不会按 repeated 下标猜测交易身份或发第二件掩盖失败。
func assetArrivalTarget(bags equipForgeBags) (*scene.BagItemInfo, bool, error) {
	var target *scene.BagItemInfo
	worn := false
	for _, candidate := range []struct {
		bag  *scene.BagInfo
		worn bool
	}{{bags.inventory, false}, {bags.equipment, true}} {
		for _, item := range candidate.bag.GetItems() {
			if item.GetConfigId() != equipForgeWeapon {
				continue
			}
			if target != nil || item.GetItemId() == 0 || item.GetCount() != 1 || item.GetMaxStack() != 1 || item.GetEquipKind() == 0 {
				return nil, false, fmt.Errorf("专用角色必须只有一件有效的 1101 装备原件")
			}
			target, worn = item, candidate.worn
		}
	}
	return target, worn, nil
}

func prepareAssetArrivalEquipment(ctx context.Context, session *equipSmokeSession, bags equipForgeBags) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	target, worn, err := assetArrivalTarget(bags)
	if err != nil {
		return 0, err
	}
	if target == nil {
		if equipBagFreeSlots(bags.inventory) < 2 {
			return 0, fmt.Errorf("主背包至少需要两个空位准备原件与强化材料")
		}
		if _, err := equipForgeGrant(session, equipForgeWeapon, 1); err != nil {
			return 0, err
		}
	} else if worn {
		change, err := session.unequip(target.GetItemId())
		if err != nil || change.tip != 0 {
			return 0, fmt.Errorf("正式卸装失败: tip=%d err=%v", change.tip, err)
		}
	}
	bags, err = equipForgeReadBags(session)
	if err != nil {
		return 0, err
	}
	target, worn, err = assetArrivalTarget(bags)
	if err != nil || target == nil || worn {
		return 0, fmt.Errorf("正式发放/卸装后主背包缺少唯一原件")
	}
	if target.GetEnhanceLevel() == 0 {
		next := target.GetEnhanceNext()
		if next == nil || next.GetNextLevel() != 1 || next.GetSuccessRate() != 10000 || next.GetCostGold() > 100000 ||
			next.GetMaterialCount() > 99 || (next.GetMaterialConfigId() != 0 && next.GetMaterialConfigId() != equipForgeStone) {
			return 0, fmt.Errorf("1101 首次强化预览不满足确定成功与受限 GM 预算")
		}
		if _, err := equipForgeFund(session, bags, target.GetItemId()); err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		change, err := equipForgeEnhance(session, target.GetItemId(), target.GetEnhanceLevel())
		if err != nil || change.tip != 0 || !change.success || change.level == 0 {
			return 0, fmt.Errorf("正式首次强化未成功: tip=%d err=%v", change.tip, err)
		}
	}
	return target.GetItemId(), nil
}
