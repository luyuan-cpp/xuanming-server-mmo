# 交接：玩家态 Redis 集群化第 2 批（2a–2d）落码简报（2026-10-23）

> 方案权威：[玩家态 Redis 集群化设计 v2](../design/redis-player-store-cluster.md)。本文只是第 2 批的执行简报，冲突时以设计稿为准。
>
> 状态：第 1 批已落码（未编译，验证命令见 `docs/PROGRESS.md` 2026-10-23 条目）；第 2 批未开工——本会话在 5 小时额度 81%、周额度 76% 时停下，避免实现代理中途被掐断留下半截改动。
>
> **2026-10-24 方向变更（先读）**：用户 10-24 明确「玩家数据不要 redis 存储，其他改用的地方还是要用」，见 [TiDB 决策 §9](../design/global-data-layer-tidb-decision.md)。2a（owner_epoch 改名，协调键）已落码（未编译），不受影响；2b（父快照改名）在看到本变更之前已落码（未编译），保留还是回退待用户定；**2c（补投索引按 slot）、2d（补投器分片）服务于要移出 Redis 的父快照与补投意图，已暂停，开工前先向用户确认**。上面「第 2 批未开工」是 10-23 的状态。

## 0. 每一批都要遵守

- **先核现状**：工作区常有别的会话在改。动手前 `git status` / `git diff` 看目标文件；`9d2784fbf` 快照已把 `redis_client.h`、`player_ownership_comp.h`、`settlement_outbox.h`、`redis.cpp` 等文件的他人改动带进来，行号与盘点时不同，按函数名定位。
- **续做说明**：被中断后续跑时，先看 `git diff` 判断做到哪一步，在半截基础上补完，不重复添加。
- **每批 ≤ 30 个文件（含测试）**；超了先停下报告，不要硬塞。
- **同一键族的 C++ / Go 镜像、以及读写该键族的工具同批改完**；来不及改的工具（merge_zone、barrierrestore、debug_* 等）在该批改为拒绝运行（fail-closed，报明确错误），不能带着旧键名静默空过。
- 注释与文档中文；新写的日志 / 控制台输出英文；错误返回文案跟随所在文件既有写法。
- Claude 不跑任何构建 / 测试；每批交付给出 Codex 命令、工作目录、通过标准与失败时要保留的输出（AGENTS.md §10.1）。
- 键形规则：把键里的玩家 id 用花括号包起来（`X:<pid>:Y` → `X:{<pid>}:Y`）；构造统一走共享函数（Go `shared/ownerepoch` 等既有包 + `shared/redisslot`，C++ `player_ownership_comp.h` + `redis_slot.h`），不新增手拼。

## 1. 第 2a 批：owner_epoch 改名为 `player:{<pid>}:owner_epoch`

- C++：`cpp/libs/services/scene/player/comp/player_ownership_comp.h` 的 owner 键构造；`role_admission_system.cpp` 三处内联拼键改为调用构造函数；`redis_client.h` 严格存盘 Lua 里 owner 键的逐字校验（`KEYS[2] ~= 'player:' .. ARGV[7] .. ':owner_epoch'` 一行）改为新形状；测试里的字面键（`cpp/tests/currency_test/currency_test.cpp`、`cross_zone_test.cpp`、`player_inbox_system_test.cpp`）。
- Go：`go/shared/ownerepoch/ownerepoch.go` 的 `Key` 与测试；Go 侧逐字复制 C++ 脚本的跨语言测试（grep `owner_epoch_crosslang`）；`tools/merge_zone/save_barrier_verify.go` 的 `playerOwnerEpochKey` 镜像（merge_zone 是独立 module，不引用 shared）；`go/db/internal/barrierrestore`、`go/data_service/cmd/{debugutil,debug_fetch,debug_import}` 中的 owner 键；player_inbox 若有 owner 键模板配置（`go/player_inbox/internal/config`、`etc/player_inbox.yaml`）一并改。
- data_service：`go/data_service/internal/logic/data_logic.go` 的 `isReservedPlayerField` 改为 `redisslot.IsReservedPlayerKeySuffix`（保留 `__` 前缀规则）。
- 不动：handoff、location 等同文件里的其他键（第 3 批）。

## 2. 第 2b 批：父快照改名为 `PlayerAllData:{<pid>}`

- C++：`redis_client.h` 里父键构造（`full_name() + ":" + id` 的几处）与严格 Lua 的父键尾缀校验（改为要求以 `':{' .. pid .. '}'` 结尾）。
- Go：login `dataloader/data_loader.go`、`clientplayerlogin/role_appearance_backfill.go`（同批改 `playerstore.MGetBySlot`，此时 login 仍是单实例句柄，MGetBySlot 对 `*redis.Client` 同样适用）、`player_class_backfill.go`；data_service `logic/asset_op_ledger_logic.go`、`persisted_player_database.go`、`cmd/debugutil/bundle.go`；friend `data/friend_profiles.go`、`data/online_directory.go`；match `team/presence.go`；`go/db/internal/barrierrestore/restore.go`；`tools/merge_zone/player_rows.go`；相关测试。
- 建议先在 Go 共享包加一个父键构造函数（例如 `shared/redisslot` 或新的小包里 `ParentKey(pid)`），各处改为调用，避免再出现手拼。

## 3. 第 2c 批：补投索引按 slot

- C++：`player_lifecycle.cpp` 里索引键构造（`player:save_pending:<home_zone>:<pid%64>`）改为 `"player:save_pending:" + redis_slot::PlayerIndexTag(pid)`；`redis_client.h` 严格 Lua 的索引键校验改为 `^player:save_pending:{[a-z]+}$`，删除桶号求余与「topic 的 zone 等于索引 zone」两项，保留 topic 形状校验；`RedisSaveCommitIntent::kIndexBuckets` 删除。
- Go saveoutbox：`contract.go` 的 `pendingIndexKey` 改为按 slot；`replay.go` 改为遍历 slot（本批**暂保留**「只认领 topic 属于本 zone 的意图」、沿用单 topic 发布器，别 zone 的意图跳过不计次）；`scripts.go` 的认领 / 推迟脚本键位不变；删除 `migrateScript` 与跨 zone 迁移分支（索引已与 zone 无关）。replay 的 `r.mapping.Get` 分支随之删除——「Go scene_manager 读 Redis 出错…」会话已撤回它在 worktree 里对该分支的改动，以本批为准。
- 工具：`tools/merge_zone/preflight.go` 的 `assertSaveOutboxDrained` 改为遍历 16384 个 slot 索引并按成员屏障 `outbox_topic` 判 zone；`go/db/internal/barrierrestore` 恢复脚本按设计 §5.3 改形（维护键移出脚本、64 桶改为「玩家不在其 slot 索引中」的同槽 ZSCORE、映射键随映射域移出）；`go/data_service/cmd/debug_import` 的 stage / commit 脚本按 §5.3 改形；`debugutil` / `debug_fetch` 的索引键。

## 4. 第 2d 批：补投器分片

按设计 §5.4：成员登记 `player:save_outbox_members:{afps}`、256 段租约 `player:save_outbox_lease:{SlotTag(64c)}`、公平份额 `ceil(256/N)` 与每次最多接管 `share − 已持有`、续约前提（最近一拍完成扫描）、多 topic 发布器（只许当前世代 DBTask topic）与按 topic 熔断、冷却 / legacy 闸按意图 zone 逐条判、去掉映射依赖（`saveoutbox.Start` 的 mapping 参数与 replay 的 mapping 客户端）。调用点 `go/db/db.go` 一带的 `saveoutbox.Start`。注意 go/db `internal/svc/servicecontext.go`、`internal/config/config.go` 也在「账号登录逻辑/物理服瓶颈」会话的改动范围内（删 `Placement.Redis` 等），改前先发消息确认对方有无在途改动，按「先落的通知后落的」走。

## 5. 之后的批次

见设计稿 §13：3（归属与位置族改名、离场成员集合计数）→ 4（go/db 玩家域句柄）→ 5a / 5b / 5c（C++ 路由层）→ 6（各 Go 服务玩家域句柄）→ 7（player_locator）→ 8（战斗结算）→ 9（工具收尾）→ 10（部署）→ 11（登录域）。
