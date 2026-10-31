# 任务 JAVA：Java 版同批对齐（可见性闸 + 本地模式原操作 + 观众席节点配额 + 观众帧编码一次）

先读 `00-common-rules.md`（同目录）与 Java 仓 `D:\luyuan\xuanming-server-mmo\AGENTS.md`，遵守其规范。勘查参考：
`..\wf\scout_java.md`（行号已过时）。Java 仓里也有其他会话的未提交改动（GlobalMatchService / GlobalMatchStore / game-proto 等），
只做外科式 Edit，不跑 mvn / mvnw / ContractSync。Java 的客户端可见契约与内部 proto 由 Codex 运行
`java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp` 从本仓同步：同步后 Java 也会有
`eSpectateVisibility`、`CreateBattleRequest.spectate_visibility`、`SpectateBattleRecord.visibility`，代码里直接引用生成名。

## 要做的事（按顺序，全部在 Java 仓）
1. **可见性闸**（与 C++/Go 同一策略、同一执法点）：
   - 策略（单一权威，放在 game-match 的 spectate 包，例如 `SpectateRules` 或新类 `SpectateVisibilityPolicy`）：活动局（如帮会
     历练）→ PRIVATE；PVE_SOLO、PVE_TEAM → PUBLIC；1V1、5V5（计分局，见 mmorpg
     `docs/design/global-match-hierarchical-pools.md` D7）→ PRIVATE；PVP_CHALLENGE → PRIVATE；其它 / 未知 → PRIVATE。
     平铺显式决策表 + 「每个 MatchMode 枚举都必须在表里」的测试。
   - 建房时决定一次并冻结：所有开局最终经 `GatherPipeline` 组 CreateBattleRequest，设 `spectate_visibility`；落点记录
     （`game-match/src/main/proto/game/match/battle_placement.proto` 的 Java 内部 message，可加一个
     `eSpectateVisibility spectate_visibility` 新字段号，或从持久的 CreateBattleRequest 读取，取更简单且单一来源的一种）带上它。
   - 执法：公开钩子（`SpectateGatherHooks.publish` 一类）对非公开只写记录不进可观战索引；本地与全局 WatchBattle 按 id 观战时
     在任何副作用之前拒绝非公开（回与「不存在」相同的 16018 文案，不泄露存在性），随机观战跳过；可观战列表过滤；
     battle 侧 `BattleRoomServiceImpl.addObserver` 第二道闸：房间可见性非 PUBLIC → 回参数错误码（不能用 1004 房间不存在）。
   - 测试：策略表测试；私密局按 id 观战被拒且无副作用；列表与随机跳过私密；battle 第二道闸。现有观战测试的建房夹具补 PUBLIC。
2. **本地模式原操作（mmorpg 已修 Go 版，Java 在 `PARITY.md` 第 374 行登记为待做）**：读该行备注与 mmorpg
   `go/match/internal/logic/spectate.go` 的 `newLocalObserverAdd` 及观战标记改为原件的做法，Java 同口径实现：本地 163 发出的
   AddObserver 带规范 UUID op、`admit_before_ms` ≤ now + 25s（battle 上限 30s，留墙钟偏差余量）、`target_instance_id` 取 battle
   节点实例（落点或目录）；RemoveObserver（`DefaultObserverDialer` 本地 remove、换场 / 重看同场 `sameBattleRewatch`、开局清退）
   都用同一份原件的 op / 截止 / 实例；节点无实例时 fail-closed。补一条直连真实 `BattleRoomServiceImpl` 的回环用例。
3. **观众席节点配额**：全节点已登记观众席达到 `max(MAX_OBSERVERS_PER_ROOM, 连接上限 / 4)` 时新观众回满员码
   `MatchErrorTip.match_error.kMatchSpectateFull_VALUE`（与房间满员同码），配额在装配时显式注入，测试可注入小值。
4. **观众帧编码一次**：每回合观众帧（158）与收尾 166 现在逐连接写对象、由编码器各编一遍。改为先编码成一份 `ByteBuf`，
   对每个活直连写 `retainedDuplicate()`，最后释放原件（Netty 标准做法，注意引用计数不泄漏、写失败也释放）。
5. **PARITY.md**：只追加一行「观战 B 批与 P0 剩余：可见性闸 / 观众席节点配额 / 观众帧编码一次」（表头
   `| 功能 | mmorpg 位置 | mmorpg commit | Java 模块 | Java 版本 | 状态 | 备注 |`，mmorpg commit 写「未提交（基于当前工作区）」，
   Java 版本 0.1.0-SNAPSHOT，状态「已落码，未编译，待 Codex 验证；依赖 ContractSync」）；第 374 行只在备注末尾追加一句
   「本地模式原操作已在 Java 落码（见新行）」，其它已有行不动。gate 推送栅栏 Java 已有等价，不加行。

交付说明里给 Codex 的命令（Java 仓根目录）：先 `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check`
（不一致时先协调再跑写入模式），再 `./mvnw -B clean install -DskipTests`、`./mvnw -B -pl game-battle -am test`、
`./mvnw -B -pl game-match -am test`，列出重点测试类。
