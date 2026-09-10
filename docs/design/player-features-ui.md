# 背包、任务、活动客户端读取接口

## 目标与现有能力

2026-09-10，为游戏内三页原生 UI 提供真实服务端数据。复用已有 Bag / BagService、MissionTable 和 MissionsContainerComp，不创建并行物品或任务状态，不因开窗发道具、接任务、改存档。

背包支持已持久化的四类固定包读取，以及玩家显式整理人物背包、仓库。任务系统当前仅有运行态组件，未接存档；`GetMissionReward` 只清领取位，`OnMissionAwardEventHandler` 为空，因此本轮只读、不暴露接取或领奖 RPC。活动尚无正式运营排期，目录来自 `MissionTable.mission_type = 2`，全部保持未排期且不可参与，不凭节日美术捏造开放活动或奖励。

## 协议

权威源为 `proto/scene/player_bag.proto`、`player_mission.proto`、`player_activity.proto`。正式生成器仅追加：

| 编号 | RPC | 返回 |
|---|---|---|
| 190 | SceneActivityClientPlayer.GetActivityList | 已配置活动目录、服务端时间 |
| 191 | SceneBagClientPlayer.GetBag | 单包实例、布局、货币快照 |
| 192 | SceneBagClientPlayer.SortBag | 整理后完整单包快照、changed |
| 193 | SceneMissionClientPlayer.GetMissionList | 配置目录和已有任务运行态 |

原有 0–189 编号逐行保持不变。不要沿用合并前“180–188 为宠物”的旧记录：180 为 DataServiceAllocateIdSegment，宠物为 181–189。

所有请求经现有玩家会话路由至 scene，不接受客户端传入 player_id。handler 只委托系统，错误码使用已有 tip 枚举，写入 TLS TipInfoMessage 后由生成层 `TRANSFER_ERROR_MESSAGE` 转移；直接写响应错误会被该宏覆盖。

## 背包的不变量

- `BagItemInfo` 只描述实例，含 64 位 item_id、config_id、数量及显示元信息。
- `BagLayoutInfo` 包含 bag_type、capacity、slots；slot 记录显式的 0 起槽号与 item_id，不依赖 repeated 的顺序。UI 的七列分页只是显示方式，不改服务器槽号。
- 四类固定包当前为 FlatLayout / FixedSlotLayout，权威占位函数恒 1×1。通过 `Bag::GetItemFootprintByGuid` 只读查询，由桥层决定形状；未来多格包还需协议布局类型和列数，本接口不假装已支持。
- bag_type 为 0 人物背包、1 仓库、2 装备栏、3 临时格；整理仅允许 0/1，不能重排穿戴槽或改变临时格淘汰顺序。
- GetBag 不创建缺失组件、不合堆、不整理。玩家无组件时返回 kServiceUnavailable，不能伪装成容量为 0 的成功空包。
- SortBag 调用 `BagService::SortByPlayerRequest`，继承跨服冻结检查与被合并实例的销毁流水。重试已整理好的包返回 changed=false。读写均在 scene EventLoop 上，不跨线程留表指针或组件引用。
- CurrencyComp.Values 延用 0 金币、1 钻石、2 绑定钻石，全部为 uint64；与背包同帧复制。
- 物品表暂没有名称、描述、图标。相应字符串为空，客户端按配置编号显示合法回退，不给存量实例伪配美术或道具名。

## 任务与活动边界

任务列表合并正式配置和已有运行态，保留存量但缺表的任务并标记 configured=false；已接受、完成、待领取由 MissionsComp 的真实状态决定。目标带显式 objective_index，可区分重复 condition_id；目标数量采用任务覆盖值或条件表默认值，完成判定复用 condition_util。已完成任务没有历史计数时不伪造计数。

任务返回 state_persistent=false，can_accept/can_claim=false。配置目前没有展示名称、描述、NPC 和地图位置，响应留空，客户端不编造导航目标。完成任务的发奖与持久化需要独立玩法任务完成后才能开启操作能力。

活动只展示现有 type=2 的任务配置；当前为 15、16、17。starts_at_ms/ends_at_ms 均为 0，status=UNSCHEDULED，can_participate=false。活动皮肤可使用春节、元宵、中秋装饰，但美术不决定线上排期。未来排期按 `activity_maintenance_auto_shift.md` 的基线/生效时间契约接入，不能用客户端时钟自行开放。

## 生成、构建和验证

正式生成器先以本机 Go 1.26.5 当前源码构建，`-mod=readonly`，再执行既有生成管线。生成时临时配置关闭 Unity 输出，避免与客户端编辑并发。生成器原有 proto2mysql v0.1.0 的 go.sum 错误行已与 Go 官方 sumdb、已修复的 data_service 及本机缓存三方核验后修正；版本不变，校验保持开启。

权威 `.vcxproj` 已登记三份 proto、三份 handler 及快照源。Linux 标准构建会从 `.vcxproj` 正式重建 CMakeLists，不手改生成 CMakeLists。

验证证据目录：`../tmp/features-backend-20260910/`。新增测试位于 `cpp/tests/bag_test/player_feature_snapshot_test.cpp` 与 `player_feature_handler_test.cpp`，覆盖无副作用读取、稀疏槽号、64 位身份与货币、非法包、冻结整理、幂等整理、真实任务进度、不领取奖励、未排期目录以及生成层错误转移。实际构建/测试结果在交接时追加。

本任务不初始化永久 ID 水位、不改数据库权限、不清数据、不启停或部署服务、不登录账号。真实联机验证仍依赖本机 data_service/号段环境的独立修复；静态配置与单测通过不等于端到端已通过。

### 实测结果与最小部署（2026-09-10）

- 正式生成退出码 0；旧 190 个编号逐行不变，新增 190–193。已生成 C++ proto/注册/handler 与 Go proto/常量；Unity 生成由客户端代理单独执行。
- 最终 `MSBuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 /p:PostBuildEventUseInBuild=false` 退出码 0。首轮两个源码编译问题（ECS 头缺失、私有占位函数）和次轮测试 include 路径问题已分别修复，保留原始日志。生成的占位查询通过只读 Bag 桥接 API 暴露。
- 新功能测试 13/13 通过，完整背包测试 158/158 通过，含 145 条既有基线。Go proto 模块 `go test -mod=readonly ./...` 退出码 0，各包无测试，结论是生成协议编译通过。
- `git diff --check` 通过。第三方 PDB 缺失产生既有 LNK4099 链接警告；可选 no-raw-pointer-member 因工具缺失 SKIP，不计为已执行静态检查。
- 运行新接口只需受控更新 gate 与玩家绑定的 scene：四条消息均为 protocol=0 / SceneNodeService，通过原生场景通道转发，不经过 Go client_rpc_router。其它 Go 服务只追加未被现有流程调用的常量，因此本次无需重启；没有数据库迁移。新 handler 已由 `InitPlayerService` 注册到 scene。
- 待部署产物在 `build/cpp/nodes/gate.exe`、`scene.exe`，哈希见证据目录 `registry-and-binaries.json`；本任务尚未复制到 bin 或重启。部署应保留现有 Kafka g2/256 和运行身份检查，连接已有角色后才能验证真实界面请求。