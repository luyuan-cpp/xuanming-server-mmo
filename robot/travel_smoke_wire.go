package main

// travel-smoke 的「线上符号」隔离层:本文件是 robot 里**唯一**引用 TravelToZone 生成物的地方。
//
// 为什么单独一个文件:下面三个符号要等阶段 2 的 proto regen 才存在 ——
//
//	game.SceneSceneClientPlayerTravelToZoneMessageId   robot/generated/pb/game/message_id.go(proto-gen 从 proto/message_id.txt 整文件生成)
//	scene.TravelToZoneRequest / scene.TravelToZoneResponse   go/proto/scene/player_scene.pb.go
//
// 而且 robot 走 vendor 编译:regen 只更新 go/proto,还要把 go/proto/scene/player_scene.pb.go 逐字节同步到
// robot/vendor/proto/scene/(本机跑不了 `go mod vendor`,见 commit 0a25c828e 的说明)。
// 在那之前**只有本文件**编译不过,属预期;travel_smoke_scenario.go 只经由下面几个小函数碰这三个符号,
// 所以 proto 改名(服务名 / 字段名)时也只需要改这里。
//
// 服务名为什么是 SceneSceneClientPlayer 而不是设计文档 CZ-7 字面写的 ScenePlayer:后者是 gate→scene 的内部服务,
// 没有 OptionIsClientProtocolService,客户端包会在 gate 被当成非法包(见 proto/scene/player_scene.proto 的注释)。

import (
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/scene"
	"robot/generated/pb/game"
)

// travelToZoneMessageId 是 TravelToZone 的消息号。号由 proto-gen 发(会先随机填 message_id.txt 的空洞,
// 无洞才取最大值 +1),所以只按常量名引用,任何地方都不许写死数字。
const travelToZoneMessageId uint32 = game.SceneSceneClientPlayerTravelToZoneMessageId

// newTravelToZoneRequest 构造传送请求。sceneConfigId=0 = 由目标 zone 的 scene_manager 挑默认大世界。
func newTravelToZoneRequest(targetZoneId, sceneConfigId uint32) proto.Message {
	return &scene.TravelToZoneRequest{TargetZoneId: targetZoneId, SceneConfigId: sceneConfigId}
}

// travelToZoneRejectTip 解 TravelToZone 回包体,返回同步拒绝码(0 = 已受理)。
//
// 只能用 id != 0 判拒绝,不能判 error_message 是否为 nil:C++ 侧 CallMethod 里的 TRANSFER_ERROR_MESSAGE
// 宏总会 mutable 出一个 error_message,受理时它是个 id=0 的空消息。
// "已受理"只代表 scene 已冻结玩家并开始存盘,不代表已到达:到达 = 之后收到 msg 124;
// 受理后失败 = 之后收到 SendTipToClient(scene 已解冻,玩家留在原地)。
func travelToZoneRejectTip(body []byte) (uint32, error) {
	var resp scene.TravelToZoneResponse
	if err := proto.Unmarshal(body, &resp); err != nil {
		return 0, err
	}
	return resp.GetErrorMessage().GetId(), nil
}

// travelSmokeTicketBinding 取重定向票据里 CZ-8 的两个绑定字段:持票者 player_id 与 target_zone_id。
// available=false 表示本次构建取不到(见下方 TODO),调用方据此跳过这两条断言并在结果行里写明 skipped。
//
// TODO(regen): GateTokenPayload 的字段 5(player_id)/ 6(target_zone_id)是阶段 1 加的,它们的 getter
// base.GateTokenPayload.GetPlayerId() / GetTargetZoneId() 要等 regen 并把 go/proto/common/base/message.pb.go
// 同步进 robot/vendor/proto/common/base/ 之后才存在(摸底时两边都是 0 处命中)。届时把函数体换成:
//
//	return payload.GetPlayerId(), payload.GetTargetZoneId(), true
//
// travel_smoke_scenario.go 的 verifyArrival 已经按 available=true 写好两条更强的断言
// (票据 player_id == 出发时的 player_id、票据 target_zone_id == 本跳目标 zone),不用再改。
// 放在本文件而不是场景文件里,是为了让"依赖 regen 的符号"始终只有这一处。
func travelSmokeTicketBinding(payload *base.GateTokenPayload) (playerId uint64, targetZoneId uint32, available bool) {
	_ = payload
	return 0, 0, false
}
