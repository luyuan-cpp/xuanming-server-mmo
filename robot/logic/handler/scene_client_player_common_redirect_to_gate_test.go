package handler

import (
	"testing"

	"proto/scene"
	"robot/logic/gameobject"
	"robot/pkg"
)

// 没有真服务端时能钉住的部分:handler 的守卫与"翻译 notify → RedirectTarget"这一步。
// 真正的搬迁动作(换连接 / 验票 / 重登录)在 robot/pkg/redirect_test.go 里用假 gate 跑。

// 三条不该炸也不该动任何东西的输入:nil player、nil notify、注册表里已经没有连接。
func TestRedirectHandler_GuardsAgainstMissingInputs(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handler panicked: %v", r)
		}
	}()
	notify := &scene.RedirectToGateNotify{TargetIp: "10.0.0.9", TargetPort: 8100}

	SceneClientPlayerCommonRedirectToGateHandler(nil, notify)
	SceneClientPlayerCommonRedirectToGateHandler(gameobject.NewPlayer(1), nil)
	// player 有,但 pkg.Clients 里没有它的连接(会话正在拆)。
	SceneClientPlayerCommonRedirectToGateHandler(gameobject.NewPlayer(424242), notify)
}

// notify 带着一个非法目标时,handler 必须原样把失败吞在自己这里(记 ERROR 后返回),
// 且连接一个字节都不动 —— 老 gate 还连着,外层才有机会重连。
func TestRedirectHandler_InvalidTargetLeavesConnectionAlone(t *testing.T) {
	pkg.SetRedirectRelogin(func(*pkg.GameClient) error {
		t.Error("relogin must not run for an invalid redirect target")
		return nil
	})
	t.Cleanup(func() { pkg.SetRedirectRelogin(nil) })

	const playerID = uint64(31337)
	gc := &pkg.GameClient{PlayerId: playerID, Account: "robot_1"}
	pkg.Clients.Register(playerID, gc)
	t.Cleanup(func() { pkg.Clients.Unregister(playerID, gc) })

	player := gameobject.NewPlayer(playerID)
	// target_port = 0:gate 事件里 port 缺失就是这个样子。
	SceneClientPlayerCommonRedirectToGateHandler(player, &scene.RedirectToGateNotify{TargetIp: "10.0.0.9"})

	if ip, port := gc.GateAddr(); ip != "" || port != 0 {
		t.Fatalf("connection moved to %s:%d on an invalid target", ip, port)
	}
	if gc.RedirectHops() != 0 {
		t.Fatalf("hops = %d, want 0", gc.RedirectHops())
	}
}
