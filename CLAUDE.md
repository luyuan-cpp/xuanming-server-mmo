# mmorpg 项目规范

> 本文档是本项目的"宪法",AI 协作和人类开发都须遵守。
> 架构/构建/编码细则见 [`.github/copilot-instructions.md`](./.github/copilot-instructions.md),本文件只保留稳定规范、不变量与索引,避免每次会话重复消耗 token。

## 1. 项目基本信息

- **类型**:MMORPG 服务器,多 zone、AOI、ECS 场景
- **多语言后端,按职责拆分**:
  - **C++ 节点**(`cpp/nodes/*`):scene / gate / centre 运行时进程 + RPC handler
  - **C++ 共享逻辑**(`cpp/libs/services/scene/*`):ECS 域逻辑(`system` / `comp`)
  - **Go 微服务**(`go/login` 等):go-zero,login / db / scene_manager / friend / guild / chat
  - **Java 网关**(`java/gateway_node`):Spring Boot,zone 目录 / gate 分配 / admin API
- **协议**:gRPC(同步) + Kafka(异步事件)
- **基础设施**:MySQL + Redis + Kafka + etcd

## 2. 中文回复

所有 AI 协作产出**用中文**。注释、commit message、文档全中文。

## 3. 生成代码与可编辑边界

- ❌ 不要手改 `generated/` 或各语言 `proto` 输出树下的生成产物
- 生成式 C++ handler 文件中含 `///<<< BEGIN WRITING YOUR CODE` 守护段的,自定义逻辑只写在守护段内
- 不要手写与 proto 重复的并行 struct

## 4. proto 同步流程

1. proto 源契约在 `proto/`,改动后重生成:`cd go && build.bat`(Go 侧 rpc/proto 产物)
2. 重新编译受影响的 C++ / Go / Java 服务
3. 字段编号:上线后**不复用**,只能 deprecate(`reserved N;` + 注释原因);开发期已删字段可复用但须重生并完整编译所有启用 module

### proto 字段类型约束(强制)

- **坐标/Transform 用 double**:`Location` / `Rotation` / `Scale` / `Vector3` / `Velocity` / `Acceleration` 的 x/y/z 必须 `double`,匹配 UE 客户端精度
- **BaseAttributesComp 用 uint64**:`strength` / `stamina` / `health` / `mana` / `critchance` / `armor` / `resistance` 必须 `uint64`,避免战斗公式 cast 时窄化
- **SnowFlake GUID 用 uint64**:`player_id` / `entity` / `scene_id` / `item_id` / `buff_id` / `skill_id`(运行时实例) / `tx_id` / `snapshot_id` 必须 `uint64`
- **Unix 时间戳用 uint64/int64**:`created_at` / `expires_at` / `castTime` / `last_time` / `start` 等必须 64-bit
- **货币用 uint64**:`CurrencyComp.values` / `owed` / `paid` / `balance_before/after`
- **可用 uint32**:`session_id` / `session_version` / `attack_power` / `defense_power` / `max_health` / `skill_table_id`(表查 key) / `current_frame`
- **可用 float**:`ViewRadius.radius`(AOI range)
- proto3 enum 第一个值必须是 0

## 5. 决策记录入口

`CLAUDE.md` 只保留稳定规范和索引,不维护长决策表。

- 架构级决策 → `docs/design/`(架构总图)
- 服务级决策 → 对应 `docs/design/<service>.md` 或服务 README
- 压测结论 → `docs/design/stress-<round>-*.md`
- 周期进度与流水账 → `PROGRESS.md`(只追加,不删旧条目)

**没写文档 = 没说过**(下个 AI 不会记得)。

## 6. 压测纪律(最重要)

### 6.1 复盘只读脚本输出

**压测复盘只读 `stress_summarize.ps1` 输出,不要手 grep raw prom dump**。

- 命令:`pwsh tools/scripts/stress_summarize.ps1 -RunDir robot/logs/stress-<name>-<ts>`
- 输出五段二维表:robot 每分钟 stats + entergame_total + dataloader stage avg + SceneManager EnterScene 子阶段 + DB task 子阶段 + Kafka lag,~3KB 上下文。
- **Round 16+ 推荐用 `tools/scripts/stress_snap.ps1` 后台批量拉 snapshot**(并行 scrape 三端口):
  `pwsh tools/scripts/stress_snap.ps1 -RunDir robot/logs/stress-<name>-<ts> -StartTime '<yyyy-MM-dd HH:mm:ss>' -Stages 2,5,10,15,18`
  端口分工:`:9101` login / `:9150` scene_manager / `:9160` db。
  文件命名 `t<N>m_login.txt` / `t<N>m_sm.txt` / `t<N>m_db.txt` —— summarize 脚本自动按后缀分流。
- 旧式单端口手拉(legacy 兼容):`curl -s http://127.0.0.1:9101/metrics > $RunDir/prom-snapshots/t<n>m.txt`。
- 历史复盘文档(`stress-1zone-*.md`)直接复用脚本输出的表格,不要再贴 raw count/sum 数字。

### 6.2 压测前后强制流程(任何一步漏了都重来)

1. **跑测前** — 把上一次压测的 `stress_summarize.ps1` 输出存为 `prev-summary.txt`(放在 `docs/design/stress-<round>-<date>.md` 同目录或 commit 到对比 PR 描述里),作为 Round N 的对比基线。`prev-summary.txt` 不存就不许开下一轮。
2. **跑测前** — 清空所有可能污染数据的日志/缓存:
   - `robot/logs/stress-*` 旧目录(留最近 1 个备查,其余删掉)
   - `bin/log/*` cpp gate/scene 日志
   - 各 go service stderr/stdout(`go-svc-stop` + 删 `tools/scripts/.run/` 的 pid/log)
   - `redis-cli FLUSHALL` 清掉残留 lock / session / task:result key
   - kafka topic offset reset(`pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`)+ broker 数据目录可选清,生产/历史不要清
   - prom snapshot 目录 = 新建 `robot/logs/stress-<name>-<ts>/prom-snapshots/`
3. **压测中** — 至少在 ramp 完成 / 稳态中段 / 稳态末 三个时刻拉 snapshot 进 prom-snapshots/。
4. **跑测后** — 跑 `stress_summarize.ps1` 出 Round N 表,与 `prev-summary.txt` 二维对比写进新复盘文档,贴架构决策行 + 更新本文件 §5。
5. **压期间不能上传任何日志**。
6. **每次登录压测前,把所有 redis / mysql / etcd 数据全部删除再开新压测**。

**不要在没有对比表的情况下声明"性能提升"**。

## 7. 不变量(数据一致性 / 安全)

跨服务必须保持的不变量,违反 → PR review 直接拒。

1. **SnowFlake 节点隔离**:每种 SnowFlake ID 只能由一种 node 类型生产(不同 node 类型共享 node_id 范围会撞)。
   注意**全系统有两套互不兼容的位布局**,不要混用解析函数:
   - `shared/snowflake` 与 C++ `SnowFlake`:**17-bit** worker / 15-bit step / **秒**级 epoch(1773446400)。
     scene_id、guild_id、instance_id、GuidId 等走这套,可以用 C++ `ParseGuid` 解。
   - login 的 **PlayerId**:`bwmarrin/snowflake`,**13-bit** node / 9-bit step / **毫秒**级 epoch
     (见 `login/etc/login.yaml` 的 `Snowflake` 段)。**拿 `ParseGuid` 解 player_id 得到的是垃圾。**
     改布局会作废存量 player_id,所以这是既成事实,不是待修项。
2. **Kafka 防僵尸**:发往 `{type}-{id}` topic 的消息必须填 `target_instance_id`(目标节点 UUID),空值则关闭过滤
3. **kafka topic key = 业务实体 ID**(同一玩家 / 同一对局事件有序)
4. **proto 字段编号上线后不复用**(见 §4)
5. **ECS 组件访问**:per-tick 路径禁用 `get_or_emplace`;跨实体查询用 `try_get`,不用 `get`(见 copilot-instructions)
6. **保证修改代码的正确性、数据一致性**

## 8. 命名规范

- RPC 请求 handler 是 `cpp/nodes/scene/handler/rpc/...` 下的 `*Handler` 类
- 异步回复 handler 用 `On<Domain><Method>Reply`(`cpp/nodes/scene/rpc_replies`)
- ECS/域逻辑放 `*System` 类与 `*Comp` 结构(`cpp/libs/services/scene/...`)
- 用更清晰的动词式 RPC handler 名 + 薄包装委托模式(如 `ProcessClientPlayerMessage`)

## 9. 不要做的事

- ❌ 不要读 `client/` 目录
- ❌ 不要手改生成产物(见 §3)
- ❌ 不要在 docs/design/ 之外随便建 README,不要为记录改动新建 markdown(除非用户要求)
- ❌ 不要把 player_id 当 prometheus label(高基数会爆)
- ❌ 不要用 `--no-verify` 等绕过安全检查

## 10. AI 协作约定

AI 协作规则以 [`AGENTS.md`](./AGENTS.md) 为准,本文件不重复维护细则,避免双文档漂移。

### 10.1 编译 / 压测协作

**Claude 只改代码,不执行编译。编译由 Codex 执行。**

- Claude ❌ 不跑 `msbuild` / `dotnet build` / `go build` / `go test` / `go vet` / `mvn` / `cmake` / `build.bat` / `build_linux.sh` 等任何构建或测试命令,也不为"验证一下"而跑。
- Claude ✅ 改完代码后,给出 Codex 可直接执行的细节:**目标工程/服务、具体命令、工作目录、环境变量、前置清理、期望产物、通过标准、失败时要保留的日志或摘要**。多个工程要说明先后顺序(C++ MSBuild 必须串行 `/m:1`,并发会报假的 C1041/LNK1104)。
- Codex 按这些细节执行本地编译 / 测试 / 压测 / 汇总,不自行脑补压测口径或宣称性能结论。
- Claude 在没有 Codex 编译结果之前,**不得声称"编译通过""测试绿""已验证"**;交付说明里要如实写"未编译,待 Codex 验证"。
