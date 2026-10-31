# 观战 B / C / P0 落码：所有代理共用的硬约束

背景：仓库 `D:\luyuan\xuanming-server-mmo-cpp`（C++ 节点 + Go 微服务，主工作区 main 分支），Java 版 `D:\luyuan\xuanming-server-mmo`。
设计依据 `docs/design/battle-spectate-tiered-fanout.md`（§10、§10.1、§12 D94–D98、§13 已拍板），项目未上线，玩家数据可清空，
不需要兼容旧数据或新旧版本混跑。负责人要求：数据强一致、代码可读易懂、可水平扩展，按最标准的做法实现。

## 硬约束（违反任何一条都算失败）

1. 只改你的任务说明里列出的文件（必要时可新增测试文件或说明里允许的新文件），不碰其它文件。
   不改生成产物：`generated/`、`go/proto/`、`cpp/generated/`、`*.pb.go`、`*.pb.h/.cc`、Java 生成目录、
   `gate_kafka_command_router.cpp`（它由生成器产出）。
2. proto 契约已由主会话改好（不要再改 proto，除非任务说明明确授权）：
   - `proto/battle/battle_data.proto` 末尾新增 `enum eSpectateVisibility { UNSPECIFIED=0; PRIVATE=1; PUBLIC=2; }`
     （C++ 名 `SPECTATE_VISIBILITY_PUBLIC` 等，全局非限定；Go 名 `battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC` 之类，以生成器命名规则为准）。
   - `proto/battle/battle_node.proto` `CreateBattleRequest.spectate_visibility = 15`。
   - `proto/match/match_service.proto` `SpectateBattleRecord.visibility = 3`（已加 import battle_data.proto）。
   - `proto/contracts/kafka/gate_event.proto` `PushToPlayerEvent.player_id = 3`；`BroadcastToPlayersEvent.player_list = 5`
     （与 `proto/gate/gate_service.proto` `BroadcastToPlayersRequest.player_list = 5` 同号同义同顺序契约）。
   生成物要等 Codex 跑 proto-gen 后才存在，代码里直接引用生成后的名字，这是预期的。
3. 工作区里有大量其他会话未提交的改动，而且他们此刻可能还在改。每次 Edit 前先 Read 目标区域最新内容；
   只做外科式 Edit（old_string→new_string），绝不用 Write 覆盖已有文件，绝不回退、整理或格式化别人的代码。
4. 禁止任何 git 写操作（commit / stash / checkout / reset / restore / add），禁止运行任何构建、测试、生成、导表命令
   （go build / go test / go vet / gofmt -w / msbuild / cmake / mvn / mvnw / protoc / proto-gen / 导表器 / ContractSync 都不许跑）。
   可以用 Read / Grep / Glob / 只读的 git diff / git log 看代码。不要把文件导出到 %TEMP% 或任何临时目录。
5. 遵守 AGENTS.md：注释与文档中文、新写的日志文案用英文（跟随文件既有写法）；§11.7（muduo 回调 / 定时器 / queueInLoop
   只存只绑 weak_ptr）；§11.3（fail-closed、幂等、默认拒绝）；§11.2（不用宏生成代码、显式依赖、时间可注入、DRY、KISS）；
   §11.4（缺陷修复配回归测试，面向接口测试，测试确定可重复）；§7.5 tip 码不手写数字；不要把 player_id / battle_id
   当 Prometheus label；`metric=` 日志行的标签只能是低基数枚举。注释解释「为什么 / 约束 / 风险」，不复述代码。
6. 续做说明：如果你开工时发现目标文件里已有本任务的半截改动（例如函数已加一半），那是本任务上一轮被打断留下的，
   在其基础上补完，不要重复添加。
7. 结束时输出（中文，务必简洁，3000 字以内）：
   - 改动文件清单（每个文件：改动的函数 / 行号区间 + 一句话说明）；
   - 新增 / 修改的测试用例名；
   - 给 Codex 的执行命令（按顺序；C++ MSBuild 必须 `/m:1`）与通过标准；
   - 未完成项、风险与需要主会话拍板的点。
   不要声称编译或测试通过。

## 2026-10-30 更新（P1 波）

- Codex 已跑过导表与 proto-gen（10-30 03:18）：`AttachRelayGate` / `PromoteSpectateBroadcast` 的 C++ / Go 绑定、`SpectateFrameEvent`、凭证、`GateClientSpectate`（消息号 351 / 352）、`kMatchSpectateFull`、`kMatchSpectateTicketInvalid`（16024）都已生成，可以直接引用；代码里仍只用符号名，不写数字。
- 共用规则第 7 条的交付说明写到 `docs/handoff/spectate-tiered-fanout-20261030/reports/<任务代号>.md`（可以覆盖自己的旧报告），最终回复只要三五行摘要。这个文件在你的允许修改范围内。
- 工作区仍有其他会话的未提交改动（全服匹配第二批、玩家存储等），只做外科式编辑。
