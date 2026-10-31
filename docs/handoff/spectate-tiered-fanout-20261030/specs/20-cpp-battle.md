# 任务 CPP-BATTLE：battle 节点可见性第二道闸 + 观众席节点配额 + 观众帧编码一次 + 大厅推送填目标玩家

先读 `00-common-rules.md`（同目录）。A 批已在本文件里加了 `RetireObserver` / `StartObserverSeatGrace` /
`ArmObserverSeatTimer` / `ReclaimExpiredObserverSeats` / `LiveObserverCount` / 公开常量 `kMaxObserversPerRoom`，先读懂再改。
「Turn battle gap closure handoff」会话确认不碰这几个文件；outcome / SQL outcome / FinishBattle / AbortAllRooms 段落归别人，不要动。

## 允许改的文件
- `cpp/nodes/battle/logic/battle_room_manager.h`、`cpp/nodes/battle/logic/battle_room_manager.cpp`
- `cpp/nodes/battle/client/battle_client_edge.h/.cpp`：仅当编码一次需要一个「发送已编码字节」的接口时才改，改动最小
- `cpp/nodes/battle/tests/battle_observer_contract_test.cpp`

## 要做的事
1. **可见性第二道闸（D94 前置，§10 第 6 条）**：`BattleRoom` 加字段保存 `CreateBattleRequest.spectate_visibility()`
   （建房时赋值一次）。`HandleAddObserver` 在「同 op 幂等重放」判定之后、席位 / 配额判定之前：房间可见性不是
   `SPECTATE_VISIBILITY_PUBLIC` → 回 `kInvalidParameter`（不能用 `kEntityIsNull`，那是 match 懒剔除索引的信号），
   打一行英文 `LOG_WARN`（只带 battle_id / observer，便于排查，不进指标标签）。拒绝分支沿用现有墓碑语义。
   头文件 `HandleAddObserver` 的契约注释同步。
2. **观众席节点配额**：观众直连与参战者共用 `battle_max_connections`（本地 4096；0 = 硬上限 65535，见
   `battle_client_edge.h`），热门节点的观众会挤掉参战者。规则：全节点已登记的观众席（各房间 `routingByObserver`
   之和，含待连接与宽限中的）达到 `max(kMaxObserversPerRoom, 有效连接上限 / 4)` 时，新观众回 `kMatchSpectateFull`
   （与房间满员同码：语义都是「直连席已满」，P1 起 match 据此转 gate 中继档）。配额值在节点装配时由连接上限算出并
   显式注入 `BattleRoomManager`（提供 setter，测试可注入小值），不要在热路径里读配置；计数用遍历求和即可（KISS，
   AddObserver 不是热路径），或者维护计数器但必须保证只在 admit / `RetireObserver` 两处增减。头文件写清契约。
3. **观众帧编码一次（§10 第 3 条）**：现在 `BroadcastTurnResult` 的观众循环与 `NotifySpectateEndAndClose` 对每个观众
   各做一遍 SerializeAsString + 包 MessageContent + fillEmptyBuffer。改成：同一帧先编码成一份客户端帧字节，再对每条
   活直连 `conn->send(data, len)`（或 `send(StringPiece)`）。**不能对共享 Buffer 调 `conn->send(Buffer*)`**——muduo 在
   loop 内会 `retrieveAll()` 清空传入的缓冲，第一个连接发完共享缓冲就空了。连接只按现有方式 `weak_ptr::lock()` 取用（§11.7）。
   参战者帧不在本任务范围，保持原样。
4. **大厅推送填目标玩家（C 批生产者一侧）**：`PushMessageViaGate`（约 :123-134）给 `PushToPlayerEvent` 填
   `set_player_id(playerId)`，`SendGateCommand`（约 :86-117）给 GateCommand 信封填 `player_id`；入口 playerId 为 0 时
   `LOG_ERROR` 并返回（生产者一侧拒绝发 0）。
5. **测试**（`battle_observer_contract_test.cpp`，沿用夹具 `BattleObserverContract`）：
   - **先改夹具**：`Create()` 等构造 `CreateBattleRequest` 的助手要设 `spectate_visibility = SPECTATE_VISIBILITY_PUBLIC`，
     否则现有观战用例全部会被第二道闸拒掉。
   - 新增：私密局（PRIVATE 与 UNSPECIFIED 各一）AddObserver 回 `kInvalidParameter`、`QueryObserver` 为 ABSENT、同 op 迟到
     不复活；节点配额注入为小值（如 3）后跨两个房间登记，第 4 名回 `kMatchSpectateFull`，回收一席后可再进；
     编码一次的回归（如果能在现有回环夹具上收两名观众的同一帧并比对字节就写，写不了在交付说明里说明替代验证）。
6. 交付说明里给 Codex 的命令：proto-gen（统一跑）后，`msbuild cpp/nodes/battle/battle.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`，
   再 `msbuild cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj ... /m:1`，运行
   `rpc_controller_test.exe --gtest_filter=BattleObserverContract.*:BattlePushPolicy*:BattleTicket*:BattleRoomTable*`。
