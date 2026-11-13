package logic

import "proto/scene_manager"

// resumeSceneOf 决定一次进场要不要先回到 location 记的场景(docs/design/player-connection-lost-hold.md §6)。
// 返回 0 = 不回,按原逻辑解析(请求指定的场景 / 地图,或默认大世界最空的频道)。
//
// 为什么要有:GO-5 定的是「访客短暂断线后在重连窗口内重登回到原处」(cross-zone-scene-travel.md §12.3),
// login 对 ShortReconnect / ReplaceLogin 因此发 ZoneId = 0、SceneId = 0,去向交给 location。之前这里只跟随了
// location 的 zone,场景仍按默认大世界最空频道重新挑:在副本、非默认地图或别的频道的玩家重连就被换走,
// 断线保留在原节点上的实体也接不回来(换节点要先等它退出、写释放标记)。这里补到场景一级。
//
// 全部满足才回:
//   - 请求没有任何去向:ZoneId、SceneId、SceneConfId 全 0。只有 login 的 ShortReconnect / ReplaceLogin
//     (entergamelogic.go resolveEnterSceneRoute 第 2 条)与 player_locator 的托管归位(player-connection-lost-hold.md §4.5:
//     离线托管中的角色没有节点负责时放回他最后在的场景)这样发;scene 自发的换图、跟随、疏散、交接重发都带 zone,
//     客户端选线带 scene_id,首登带本 zone —— 它们一律不受影响。sceneConfID 传的是第 3 步算过等待落点之后的值。
//   - location 落在具体节点上(node_id 非空、记着场景),不是跨 zone 传送途中的等待落点。
//   - location 的 zone 就是本次落点的 zone:第 1b 步按它定了 zone,不同 zone 的已在第 2 步重定向走了。
//
// 解析失败(场景已回收、属主节点已死且场景重建不了、读失败)由调用方回落默认大世界,与加这一步之前的行为相同;
// 再入屏障未到照旧回可重试的错误。
func resumeSceneOf(in *scene_manager.EnterSceneRequest, sceneConfID uint64, currentLoc *scene_manager.PlayerLocation,
	awaitingPlacement bool, currentZoneID, targetZoneID uint32) uint64 {
	if in.GetZoneId() != 0 || in.GetSceneId() != 0 || sceneConfID != 0 {
		return 0
	}
	if currentLoc == nil || awaitingPlacement || currentLoc.GetNodeId() == "" || currentLoc.GetSceneId() == 0 {
		return 0
	}
	if currentZoneID == 0 || currentZoneID != targetZoneID {
		return 0
	}
	return currentLoc.GetSceneId()
}
