# 背包、任务、活动客户端读取接口

## 目标与现有能力

2026-09-10，为游戏内三页原生 UI 提供真实服务端数据。复用已有 Bag / BagService、MissionTable 和 MissionsContainerComp，不创建并行物品或任务状态，不因开窗发道具、接任务、改存档。

背包支持已持久化的四类固定包读取，以及玩家显式整理人物背包、仓库。任务由 MissionsContainerComp 保存，随玩家数据库记录加载与持久化；显式接取、领奖经 PlayerMissionSystem 校验并驱动任务链，列表读取仍不产生接取或发奖副作用。活动目录来自 `MissionTable.mission_type = 2`，开放状态由服务端排期决定；没有排期时保持未排期且不可参与，不凭节日美术捏造开放活动或奖励。

## 协议

权威源为 `proto/scene/player_bag.proto`、`player_mission.proto`、`player_activity.proto`。正式生成器仅追加：

| 编号 | RPC | 返回 |
|---|---|---|
| 190 | SceneActivityClientPlayer.GetActivityList | 已配置活动目录、服务端时间 |
| 191 | SceneBagClientPlayer.GetBag | 单包实例、布局、货币快照 |
| 192 | SceneBagClientPlayer.SortBag | 整理后完整单包快照、changed |
| 193 | SceneMissionClientPlayer.GetMissionList | 配置目录和已有任务运行态 |
| 194 | SceneMissionClientPlayer.AcceptMission | 接取结果及最新任务列表 |
| 195 | SceneMissionClientPlayer.ClaimMissionReward | 领奖结果及最新任务列表 |

原有 0–189 编号逐行保持不变。不要沿用合并前“180–188 为宠物”的旧记录：180 为 DataServiceAllocateIdSegment，宠物为 181–189。

所有请求经现有玩家会话路由至 scene，不接受客户端传入 player_id。handler 只委托系统，错误码使用已有 tip 枚举，拒绝码直接写本次应答的 error_message（规则见 cpp/libs/services/player/rpc/player_service_interface.h）。

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

已加载玩家的任务列表恒返回 state_persistent=true；缺 MissionsContainerComp 时任务与活动列表返回 kServiceUnavailable，不补建存档或伪装成空目录。can_accept/can_claim 由当前任务状态与 CheckAccept/CheckClaim 的只读校验共同决定，冻结或条件不满足时为 false；读取本身不接取或发奖。配置目前没有展示名称、描述、NPC 和地图位置，响应留空，客户端不编造导航目标。

活动只展示现有 type=2 的任务配置；当前为 15、16、17。PlayerActivityScheduleSystem 填写 starts_at_ms/ends_at_ms 与开放状态，只有 status=OPEN 且任务接取校验通过才令 can_participate=true；无排期时为 UNSCHEDULED 且不可参与。活动皮肤可使用春节、元宵、中秋装饰，但美术不决定线上排期。排期遵循 `activity_maintenance_auto_shift.md` 的基线/生效时间契约，不能用客户端时钟自行开放。

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
### 已授权本地部署结果（2026-09-11）

此前的未部署状态已解除。用户明确允许更新 gate、scene 并启动本地环境后，安装 9 月 10 日 10:28 的已验证构建，保留旧程序备份。运行 gate SHA256=EE0EF183CEF0FF6A2A2869A7F19DBBB7494E84C2B559DC95D4429B278D0CF34F；scene SHA256=8160A9ABB94D4B8DC99C83DC5890E23DFB876BCCA508E0AF7B52BB52A98BEB65。两进程与新启动记录一致，四 RPC 的源编号、生成注册和客户端常量核对通过。

标准启动器 03:47:21 完成全部阶段。独立验收：七个命令服务 PID/启动时间/路径匹配 Kafka g2/256 清单；db/data_service 正常监听；scene 依赖就绪；网关 UP、一区 OPEN。客户端已打开到登录流程，未登录账号，未检验真实 UI RPC 回包。未清数据或初始化永久 ID 水位，任务奖励与活动排期的未实现边界不变。

证据：../tmp/features-deploy-20260911/install-result.json、service-verification.json、startup-result.json；旧程序：previous-nodes。启动器日志：run/logs/game-launcher/20260911-034142-036/launcher.log。外层输出管道等待已清理，未捕获命令退出码；成功结论来自六步启动记录与独立运行时健康检查。

## 继续完成交互与存档（2026-09-11，验证中）

用户要求继续完成此前缺失的任务接取、领奖、持久化及活动排期能力。本节描述本轮最终设计；前文为历史交付记录，不能用于判断本轮已经测试或部署。

纠正旧文对背包持久化的过早结论：已有 bag_marshal 桥及快照字段，不等于生产 MySQL 冷启动恢复链已接通。本轮在 player_database 新增 bag_component=13 与 mission_component=14；背包及任务领取状态随同一玩家数据库行保存。QuestAllData 新增 scopes=3 与 scoped_state_present=4，以稳定任务编号保存各 scope 的进度、已完成与待领取状态。未知历史任务编号保留，不把运行时位图下标写入存档。生产 loader 及正式生成模板接入桥；只有新数据库字段不存在时才回退旧顶层 bag_data/quest_data，新字段存在但为空时仍是权威数据。正常保存/重登语义沿用现有异步持久化，不声称进程崩溃时所有内存动作立即落盘。

正式协议生成器在旧 0–193 不变的基础上追加 194 AcceptMission、195 ClaimMissionReward，均回完整任务快照。任务写入口当前仅支持玩家 scope=0，不接受请求指定玩家。接取检查真实可完成的条件类别、合法表与奖励、跨服冻结、同类型占用与活动排期；没有现有 Dungeon/Monster 出生来源的怪物目标不可接，ANY 条件至少需要一种实际可达怪物。接取只向新任务补当前等级及它需要的历史完成事实，不重复推进其它任务。NPC 交互、使用物品及其它尚无正式事件来源的条件不开放。

领奖通过现有 BagService 一次预检并发放真实 RewardTable 道具，流水类型为 TX_QUEST_REWARD；容量不足、冻结、禁获、号段未就绪等失败保留待领取状态。自动奖励也使用同一入口，失败可转手工领取。一个击杀事实只推进当前顺序目标；战斗胜利结算携带真实有序击杀事实，scene 消费后推进任务。结算重复触发的保护与实际测试结果另行补记。

新增正式 ActivitySchedule 表（data/schema/activityschedule_table.proto 与 data/ActivitySchedule.xlsx），以任务 id 为外键，enabled 与 UTC 毫秒基线起止时间控制 [start,end) 开放区间。服务端是开放状态事实源，客户端统一标注北京时间 UTC+8。春节、元宵、中秋对应的现有任务 15/16/17 暂为 disabled、0/0，显示“未排期”；用户未给正式日期，不自动开放。维护顺延是后续运营策略，本轮绝对时间排期不假装实现自动顺延。

原生 uGUI 已增加接取、领奖、参与及查看任务操作，按服务端能力和忙碌状态控制按钮。写请求使旧列表回包失效，断线/换角按连接世代隔离；成功领奖刷新背包，失败保留有效快照并显示错误。视觉沿用现有 Q 版道家玉绿、米白金边与节庆资源。新增视觉状态仍是编辑器夹具，不能替代真实账号验收。

只读联机验收工具 features-smoke 要求显式指定已有测试账号；无已有角色立即失败，不自动创建，不用 GM 补任务或改进度。默认仅查询三个页面。可显式设置 bag_type（0–3）、sort_bag（仅0/1，整理两次检查守恒与幂等）、scope、accept_mission_id、claim_mission_id（检查重复领取拒绝）、verify_relogin（比较背包与任务存档）；未执行动作明确输出 SKIPPED。features_smoke.account 必填，player_id=0 选择已有首角色或显式指定已有角色。使用 robot.exe -c 指向用户选定的本地配置，凭据由正常配置加载，不放命令参数或报告。该工具仅完成内存测试及构建，截至本节写入未登录账号。

本轮证据目录：E:/work/tmp/features-completion-20260911；Unity 证据目录：../mmorpg-client/.codex-artifacts/gameplay-ui-20260911。生成与导表成功；C++ 首轮构建发现正式 loader 模板 include 路径错误，已修模板并重生成，待增量构建。后续真实验证、迁移、安装结果以追加记录为准。
## 本轮最终验证与安装（2026-09-11）

正式协议旧 0–193 共 194 条逐行保持不变，追加 194 接取、195 领奖。服务端 game.sln 最终串行构建退出 0；背包/功能完整 214/214、任务完整 16/16、战斗完整 93/93 通过，包含真实领奖、生产存档桥、战斗结算重复/失败恢复及有序击杀测试。旧测试等级事实的 amount 夹具已纠正，失败前记录保留，没有跳过用例。第三方缺失 PDB 的 LNK4099 和可选静态工具未安装仍是既有验证限制。

Unity 官方 MCP 完成 96/96 EditMode 回归，0 失败、0 跳过；两种尺寸原生 uGUI 共 26 张夹具截图，新增接取、领奖、忙碌、失败、活动参与状态。截图检查修复任务底部文字与边框/角花重叠。最终 Windows 包 Succeeded、errors=0、warnings=0，安装到 E:/work/tmp/showcase_player，289 个文件逐一 SHA256 一致，旧包备份于 E:/work/tmp/features-completion-20260911/previous-player。截图属于编辑器视觉夹具，真实物品/任务展示元数据缺失时继续使用配置编号回退，不把样例名称当运营配置。

本地 zone_1_db 已经用户明确授权，用正式迁移器新增 player_database 的 bag_component、mission_component、pet_component 三列并建立迁移台账，现 version 1 baseline 与 version 2 auto_proto_sync_3a252026dba3 均 ok。修复迁移器独立连接 ParseTime 后只读 status=0，plan=4 且没有待执行 DDL；仅保留 user_oauth.provider_id、user_phone.phone 两项原有索引字符串类型漂移警告，未使用 allow-modify。没有清除数据库、Redis、Kafka 数据或初始化永久 ID 水位。proto2mysql 原本地 replace 目录丢失，现从官方 v0.1.0 / 2aca007 基线恢复并按原契约加入主键 string/bytes 的 VARCHAR(191)/VARBINARY(191) 最小补丁，纯 DDL 回归和 Go DB 测试/构建通过；不声称找回未发布补丁原文。

用户另明确授权结算修复与相关本地服务更新后，gate、scene、battle、db 四项候选经构建/测试证据与哈希校验，已完成旧程序备份和安装。安装脚本首次使用空备份参数在 File.Replace 调用前失败，尚未替换任何程序；修为明确备份路径后安装成功。安装凭据位于 service-binary-backup-20260911T1334565070031Z-8669fae61af64b2cbf183a8aa0308397/installation-receipt.json。标准启动器后续运行结果单独追加，不把安装通过等同于启动或真实账号验证通过。

features-smoke 现支持显式 battle_config_id=1 的完整单人验收链，要求 scope=0、accept_mission_id=12、claim_mission_id=12、verify_relogin=true；仅选已有角色，不自动建角、不调用 GM、不固定账号。真实 PVE 胜利后轮询服务端任务可领取状态，再验证领奖、重复拒绝及重登；默认 battle_config_id=0 明确跳过战斗。新增专用回包 waiter 和纯内存测试已通过，机器人已重新构建，但截至本节未使用账号登录。节庆任务 15/16/17 仍没有正式日期，保持未排期/不可参与；正常保存重登有测试覆盖，不承诺领奖回包前已同步落盘或崩溃零丢失。

完整证据：E:/work/tmp/features-completion-20260911；客户端证据：E:/work/mmorpg-client/.codex-artifacts/gameplay-ui-20260911。三页预览为该客户端目录的 three-pages-preview.jpg。
### 本地启动配置收尾

新 db 首次启动发现本地 AutoCreateDatabase/AutoMigrateSchema 旧配置为 true，触发了未批准的索引字符串类型自动修改尝试，数据库以1170拒绝。已将两个开关设 false，遵循正式迁移后运行时不做DDL的契约；随后只读plan再次确认没有待执行ADD且两项类型警告不变。另本机Kafka管理JVM查询偶发超过20/30秒，标准启动器的4个只读查询预算调整为60秒，分区/副本/保留期/清理策略断言全部保留，未跳过校验或改主题。
### 本地环境最终就绪（2026-09-11 09:49:48 起）

标准启动器完成全部六阶段，网关 UP、一区 OPEN，最终游戏窗口已打开。独立校验 gate/scene/battle/db 四个运行进程的绝对路径、候选 SHA256 和监听端口均通过。另发现启动器遗漏 gate 的 gRPC 路由依赖 client_rpc_router：现已从既有源码通过全模块测试/构建，补入标准启动清单并由正式服务管理器启动，50600 监听和运行文件哈希核对通过。该无状态路由不使用账号、Redis 或 Kafka。外层 PowerShell 受后台子进程继承输出管道影响仍可能等待，未将未捕获的命令退出码写成0；成功依据六步启动日志及独立运行时复核。

最终证据为 E:/work/tmp/features-completion-20260911/running-service-verification.json、final-validation-summary.json、router-install.json 与 router-tests.log。任务实施和本地运行已完成；尚未执行真实账号联机验证，等待用户指定已有本地测试账号，未自行使用存储凭据。春节/元宵/中秋活动正式日期仍未提供，保持未排期。

## 2026-09-12 本地续作复核

本次标准启动器于 02:00:51 完成六阶段并打开游戏；02:04:49 独立复核 gate、scene、battle、db、client_rpc_router 五个运行程序的绝对路径、候选 SHA256 和监听端口全部匹配，网关 UP、一区 OPEN，客户端仅一个运行进程。证据在 E:/work/tmp/features-recheck-20260912/running-service-verification.json；保留已有数据库及服务数据。

客户端包已在 9 月 11 日 11:04、11:37 被后续登录界面构建更新，因此不能再用 09:16 的旧安装清单判断当前包。最终 prelogin-hero-center 构建直接输出 showcase_player，Succeeded、0 errors、503 条既有图形/推理着色器警告。本次只读核对其构建源工程 citymove_verify_project 与正式客户端中三页 UI、数据客户端及资源共 60 文件，哈希全部一致；未恢复旧包。构建来源与逐文件比对见同目录 client-build-provenance.json。旧 96 项 Unity 与 323 项 C++ 结果仍属于既有验证，本次没有重新运行这些测试。

功能实施与本地启动完成，真实账号的接取、战斗、领奖、拒绝重复领取及重登验收仍等待用户指定已有测试账号名，未自行选择账号或读取登录凭据。活动 15/16/17 仍未收到正式日期，保持未排期。

## 2026-10-22 活动中心：权威目录、活跃与恢复合同

本节是本轮实现合同；前文测试和部署仅证明当时版本。本轮不部署、不清数据，不把配表中的三项任务变成美术图的六项日常。现有任务仍为 scope=0，不可重复接取，因此次数明确为“每角色一次”的完成位 0/1，不用任务目标 progress 伪造每日完成次数。

`ActivitySchedule` 追加不可复用的 `definition_key`、配置修订、分类、展示字段和活跃贡献。现有任务 15/16/17 仅配置稳定身份 `activity.mission.<id>.initial` 和修订 1；排期仍 disabled/0/0，分类仍未配置，贡献为 0。复用数值活动 ID 时必须更换内容身份；已完成任务 ID 的重用不受任务域支持。收藏以角色与内容身份隔离，由客户端偏好保存，不影响权威资格。

目录奖励由现有 Reward 表的真实物品和 Item 元数据展开，重复物品聚合，装备由正式 `equip_kind>0` 判定，其余为道具。Reward 表不承载经验、道行或货币语义，这些筛选不会凭名称或图标猜出结果；绑定说明缺失保持空。入口只下发实际已支持的任务入口与 scope，客户端仍调用原任务接取/查看入口，不生成地图坐标或假传送。排期错误保留真实目录 ID 和不可参与原因，不能发布 id=0 的伪卡。

### 正式活跃配置与来源

新增 `ActivityVitalityRule` 与 `ActivityVitalityReward` 源 schema/空 xlsx，由正式导表器生成各语言绑定。当前两表没有正式数据，响应 `available=false`，明确“活跃规则与阈值奖励尚未配置”，不能领取。

启用时只允许一条有效规则，必须给出稳定规则 ID、修订、首个周期 UTC 绝对起点、周期毫秒长度及积分上限；阈值行明确关联规则、门槛和现有 Reward ID。没有代码内置的 05:00、10/100、五档或节庆开放日期。单周期最多 64 档是输入预算，不是运营档数；规则锚点/周期长度属于规则身份，不得在同 ID 下改成另一套周期。修订可热更新展示/奖励，但同 `rule_id+period_id+threshold_id` 仍最多领取一次，旧来源的积分不会因修订变化清零。

积分只从 `PlayerMissionSystem::ApplyFollowUps.completed` 的正式完成通知进入，先验证当前任务已完成及其活动配置。正活跃贡献必须同时配置稳定 `definition_key` 与非零配置修订，缺失时目录明确报配置异常，参与/接取入口拒绝，完成来源也不入账。已接任务如果在后续内容身份/活动修订缺失，或活跃规则未启用/非法期间完成，当前没有历史扫描/自动追补合同，`ApplyFollowUps.completed` 不会因配表修复自动重发旧完成事实；未记录来源的事实若由有效服务端通知重新投递，仍通过现有完成校验入账，并由已记录来源去重。阈值奖励表异常仍按下段保留积分事实。来源永久去重键是 `scope+mission_id`，仅适用于当前一次性任务；重复投递、重启恢复、跨周期再次通知都不会再次入账。周期按事实处理时传入的服务端时间固定记录，之后不按查询时间重新归日。尚未实现每日可重复活动的完成事件编号/次数重置。

完成来源与任务完成位同帧写入 `MissionListComp.activity_ledger`，沿既有 mission_marshal 整份携带，不新增独立任务状态或数据库列。任务完成事实不能额外套资产的退出/无围栏闸而漏记唯一通知；强归属、退出、交接、战斗校验在领取资产提交点执行。阈值奖励配表临时异常只禁领，不改变已启用积分规则的事实入账。规则本身缺失、多条启用或非法周期时禁止计分及领取，运营启用前需先验证配置。

### 领取身份、提交与重启恢复

正式 RPC 为 `SceneActivityClientPlayer.ClaimActivityVitalityReward`。请求无 player_id，主体来自原玩家鉴权路由；意图含规则 ID、修订、周期起点、阈值 ID 和客户端先持久化的 request_id。重放同一 RPC 同时承担原意图查询，不能超时后更换 request_id。

处理在 scene EventLoop 单线程完成：先验证账本，按 request_id 查原记录并核对完整请求身份，然后才检查当前周期/配置/积分/权限和容量。原记录的重放优先于跨日、配置撤销和冻结；同 request_id 改换阈值或修订拒绝。同一周期阈值被另一 request_id 占用时拒绝，不另发奖励。所有返回业务拒绝 tip 的路径均发生在本次资产提交前；系统故障或传输错误不能推断原请求未提交。

新领取通过原 `BagService::AddItems` 整批预检与发放，道具发放和领取记录在同一帧内提交，随后用 `SavePlayerForAssetCommit` 请求整份玩家存盘。背包满、禁获、号段不足、未知配置等拒绝不消耗领取权；记录成功后没有重新解释奖励配置的重放。积分来源和领取记录分别最多保留 4096 条，达到预算后 fail-closed，不自动剪掉幂等身份；正式长期运营前需确定有证明的归档/保留策略。

`claimed=true,durable=false` 只表示已在当前权威内存受理，界面显示保存中，继续以原意图有界重查；不能显示领取成功。`durable=true` 必须同时有非零 owner_epoch、相同 player_id，且 `PlayerLastPersistedSnapshotComp` 的正式 mission_component 内包含逐字节相同的领取记录（含原 request_id、修订、周期和阈值）。保存函数返回 true 不能证明成功。

本轮 durable 的准确边界是：严格父快照、pending 屏障和两条可恢复 SQL 投递意图已通过既有原子写路径确认；SQL 异步任务继续由既有工作器恢复，冷加载仍须通过 SQL 双行 ACK 屏障。本轮不把此标志宣称为同步 SQL 已提交，也不提供绕过 owner_epoch=0 的旧兼容路径。任务/背包及账本随同一玩家记录一起恢复；重启后未有本进程持久快照证明时，原记录先返回保存中，再重新提交并查询证明。响应 `snapshot` 是当前周期，`claimed/durable/replayed` 只对应请求原记录，跨日恢复不能用新周期阈值替代旧结果。

### 本轮验证与两版状态

正式导表与协议生成结果、当前 C++/Go/Java 编译及回归证据在 `build/verification/activity-center-20261022/`。首轮导表缺本机 protoc-gen-go PATH、首轮 MSBuild 使用非嵌套目标的失败日志均保留，后续结果单独记录，不覆盖成成功。客户端 C# protobuf 必须另跑独立仓库正式 `Tools/gen_proto.ps1` 与 `Tools/gen_messageids.ps1`，不能只生成 RPC 调用桩。

另一整套 Java 仓库的规范位置 `D:/luyuan/xuanming-server-mmo` 当前不存在；`D:/work/xuanming-server-mmo` 仅有 `third_party/grpc/.build_vs_release` 构建残留，没有业务源、AGENTS、PARITY 或 Git 元数据。没有可对齐的源仓，不能登记 PARITY 或声称双版本完成。同仓 `java/config_node` 仅为本轮配表消费者，单独编译不等于另一版玩法实现。正式分类、展示文案、活动日期、日常次数、活跃规则/阈值/日界线和长期账本归档仍待运营配置；现有一次性任务尚无循环完成事实与次数重置合同，不能仅配表就宣称支持日常循环。在线、故障恢复真库及另一版 Java 行为仍未验收。

### 2026-10-23 续作：跨语言消费验证

`resume-20261023-go-java/final-receipt.json` 记录 Go/proto 28 个包编译退出 0（该模块无测试文件，169 项输入无漂移）；同仓 Java `config_node` 使用 release 21 编译 405 个 Java 源退出 0，406 项编译输入无漂移。真实 `AllTable` 的 JSON/PB 消费烟测退出 0，确认三项活动仍为 15/16/17、停用且零排期、稳定身份/修订 1/零贡献，两张活跃表均为 0 行，96 项配置输入无漂移。它们证明本仓跨语言绑定与配置消费，不代表另一版 Java 玩法已对齐。旧 Java JAR 路径失败原日志保留，本次未复现，不能推定旧失败根因已修复。
