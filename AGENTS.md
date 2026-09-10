# mmorpg 项目规范与 AI 协作守则

> 本文件是本项目对 AI Agent（Claude Code / Codex / Cursor / Copilot 等）和人类开发者的唯一规范入口。
> `CLAUDE.md` 只通过 `@AGENTS.md` 导入本文件，不在两处重复维护规则；规则变更只编辑本文件，`CLAUDE.md` 永远保持单行导入。仓库既有“`CLAUDE.md §N`”引用均解释为本文件同号章节，新引用统一写“`AGENTS.md §N`”。架构、构建与编码细则见 [`.github/copilot-instructions.md`](./.github/copilot-instructions.md)；若细则与本文件冲突，以本文件为准。

## 会话启动门禁（每次必做）

**AI 没有可靠的跨会话记忆。** 每次新会话动手前必须按序读完：

1. `AGENTS.md`（Claude Code 已通过 `CLAUDE.md` 自动导入，无需重复读取）
2. `PROGRESS.md` —— 当前进度
3. `.github/copilot-instructions.md` —— 架构、构建与编码细则
4. `docs/design/<相关服务>.md` —— 任务相关设计
5. `git log -20 --oneline` —— 最近改动
6. 当前打开的 PR / Issue

先确认任务范围、既有改动、约束和冲突；没读懂就不动手。

## 1. 项目基本信息

- **类型**：MMORPG 服务器，多 zone、AOI、ECS 场景
- **多语言后端，按职责拆分**：
  - **C++ 节点**（`cpp/nodes/*`）：scene / gate / centre 运行时进程 + RPC handler
  - **C++ 共享逻辑**（`cpp/libs/services/scene/*`）：ECS 域逻辑（`system` / `comp`）
  - **Go 微服务**（`go/login` 等）：go-zero，login / db / scene_manager / friend / guild / chat
  - **Java 网关**（`java/gateway_node`）：Spring Boot，zone 目录 / gate 分配 / admin API
- **协议**：gRPC（同步）+ Kafka（异步事件）
- **基础设施**：MySQL（→ 迁移中：TiDB 全区全服数据层，见 `docs/design/global-data-layer-tidb-decision.md`）+ Redis + Kafka + etcd

## 2. 中文回复

所有 AI 协作产出**用中文**。注释、commit message、文档全中文。

## 3. 生成代码与可编辑边界

- ❌ 不要手改 `generated/` 或各语言 `proto` 输出树下的生成产物
- 生成式 C++ handler 文件中含 `///<<< BEGIN WRITING YOUR CODE` 守护段的，自定义逻辑只写在守护段内
- 不要手写与 proto 重复的并行 struct

## 4. proto 同步流程

1. proto 源契约在 `proto/`，改动后重生成：`cd go && build.bat`（Go 侧 rpc/proto 产物）
2. 重新编译受影响的 C++ / Go / Java 服务
3. 字段编号上线后**不复用**，只能 deprecate（`reserved N;` + 注释原因）；开发期已删字段可复用，但须重生并完整编译所有启用 module
4. **tip 错误码不在 proto 里手写**：`generated/code/proto/tip/*.proto` 全部由导表器从 `data/tip/Tip.xlsx` 生成。
   加一个码 = 表里加一行（A 列码名，**全局唯一**，建议带域前缀；B 列中文文案；服务端内部故障在
   `fault` 列填 `1`，业务拒绝留空）→ 跑导表器 → 在服务的 `constants.go` 里写 `ErrX = uint32(table.XxxError_kXxxX)`。
   「哪个码算故障」也由表生成（`go/shared/generated/tip/faults.go`），**不要在服务里再包一层 fault map**。
   新开一个域 = 表里加一行组头 `//xxx_error base=<起始号> width=1000`，并把新生成的
   `.pb.cc/.pb.h` 加进 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`。
   详见 [docs/design/tip-code-axis.md](docs/design/tip-code-axis.md)。
5. **配置表的 schema 也不在 xlsx 表头里写**：事实源是 `data/schema/<sheet>_table.proto`
   （2026-09-03 起），类型 / owner / 键 / 索引 / 外键 / 位序 / 注释全用
   `data/schema/cfg_options.proto` 的自定义 option 表达；xlsx 只出列名与数据。
   加列、加表、改字段号的完整流程与全表索引见 [data/AGENTS.md](data/AGENTS.md)。
   配表字段号与 §4.3 同规矩：**上线后不复用，删字段写 `reserved N;`**。

### proto 字段类型约束（强制）

- **坐标/Transform 用 double**：`Location` / `Rotation` / `Scale` / `Vector3` / `Velocity` / `Acceleration` 的 x/y/z 必须 `double`，匹配 UE 客户端精度
- **BaseAttributesComp 用 uint64**：`strength` / `stamina` / `health` / `mana` / `critchance` / `armor` / `resistance` / `speed` 必须 `uint64`，避免战斗公式 cast 时窄化
- **SnowFlake GUID 用 uint64**：`player_id` / `entity` / `scene_id` / `item_id` / `buff_id` / `skill_id`（运行时实例）/ `tx_id` / `snapshot_id` 必须 `uint64`
- **Unix 时间戳用 uint64/int64**：`created_at` / `expires_at` / `castTime` / `last_time` / `start` 等必须 64-bit
- **货币用 uint64**：`CurrencyComp.values` / `owed` / `paid` / `balance_before/after`
- **可用 uint32**：`session_id` / `session_version` / `attack_power` / `defense_power` / `max_health` / `skill_table_id`（表查 key）/ `current_frame`
- **可用 float**：`ViewRadius.radius`（AOI range）
- proto3 enum 第一个值必须是 0

## 5. 决策记录入口

`AGENTS.md` 只保留稳定规范和索引，不维护长决策表。

- 架构级决策 → `docs/design/`（架构总图）
- 服务级决策 → 对应 `docs/design/<service>.md` 或服务 README
- 压测结论 → `docs/design/stress-<round>-*.md`
- 周期进度与流水账 → `PROGRESS.md`（只追加，不删旧条目）

**没写文档 = 没说过**（下个 AI 不会记得）。

## 6. 压测纪律（最重要）

### 6.1 复盘只读脚本输出

**压测复盘只读 `stress_summarize.ps1` 输出，不要手 grep raw prom dump**。

- 命令：`pwsh tools/scripts/stress_summarize.ps1 -RunDir robot/logs/stress-<name>-<ts>`
- 输出五段二维表：robot 每分钟 stats + entergame_total + dataloader stage avg + SceneManager EnterScene 子阶段 + DB task 子阶段 + Kafka lag，约 3KB 上下文。
- **Round 16+ 推荐用 `tools/scripts/stress_snap.ps1` 后台批量拉 snapshot**（并行 scrape 三端口）：
  `pwsh tools/scripts/stress_snap.ps1 -RunDir robot/logs/stress-<name>-<ts> -StartTime '<yyyy-MM-dd HH:mm:ss>' -Stages 2,5,10,15,18`
  端口分工：`:9101` login / `:9150` scene_manager / `:9160` db。
  文件命名 `t<N>m_login.txt` / `t<N>m_sm.txt` / `t<N>m_db.txt`，summarize 脚本自动按后缀分流。
- 旧式单端口手拉（legacy 兼容）：`curl -s http://127.0.0.1:9101/metrics > $RunDir/prom-snapshots/t<n>m.txt`。
- 历史复盘文档（`stress-1zone-*.md`）直接复用脚本输出的表格，不要再贴 raw count/sum 数字。

### 6.2 压测前后强制流程（任何一步漏了都重来）

1. **跑测前** —— 把上一次压测的 `stress_summarize.ps1` 输出存为 `prev-summary.txt`（放在 `docs/design/stress-<round>-<date>.md` 同目录或 commit 到对比 PR 描述里），作为 Round N 的对比基线。`prev-summary.txt` 不存就不许开下一轮。
2. **跑测前** —— 清空所有可能污染数据的日志/缓存：
   - `robot/logs/stress-*` 旧目录（留最近 1 个备查，其余删掉）
   - `bin/log/*` cpp gate/scene 日志
   - 各 go service stderr/stdout（`go-svc-stop` + 删 `tools/scripts/.run/` 的 pid/log）
   - `redis-cli FLUSHALL` 清掉残留 lock / session / task:result key
   - kafka topic offset reset（`pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`）+ broker 数据目录可选清，生产/历史不要清
   - prom snapshot 目录 = 新建 `robot/logs/stress-<name>-<ts>/prom-snapshots/`
3. **压测中** —— 至少在 ramp 完成 / 稳态中段 / 稳态末三个时刻拉 snapshot 进 `prom-snapshots/`。
4. **跑测后** —— 跑 `stress_summarize.ps1` 出 Round N 表，与 `prev-summary.txt` 二维对比写进新复盘文档，贴架构决策行 + 更新本文件 §5。
5. **压期间不能上传任何日志**。
6. **每次登录压测前，把所有 redis / mysql / etcd 数据全部删除再开新压测**。

**不要在没有对比表的情况下声明“性能提升”**。

## 7. 不变量（数据一致性 / 安全）

跨服务必须保持的不变量，违反 → PR review 直接拒。

1. **SnowFlake 节点隔离**：每种 SnowFlake ID 只能由一种 node 类型生产（不同 node 类型共享 node_id 范围会撞）。
   注意**全系统有两套互不兼容的位布局**，不要混用解析函数：
   - `shared/snowflake` 与 C++ `SnowFlake`：**17-bit** worker / 15-bit step / **秒**级 epoch（1773446400）。
     scene_id、guild_id、instance_id、GuidId 等走这套，可以用 C++ `ParseGuid` 解。
   - login 的 **PlayerId**：`bwmarrin/snowflake`，**13-bit** node / 9-bit step / **毫秒**级 epoch
     （见 `login/etc/login.yaml` 的 `Snowflake` 段）。**拿 `ParseGuid` 解 player_id 得到的是垃圾。**
     改布局会作废存量 player_id，所以这是既成事实，不是待修项。
2. **Kafka 防僵尸**：发往 `{type}-{id}` topic 的消息必须填 `target_instance_id`（目标节点 UUID），空值则关闭过滤
3. **kafka topic key = 业务实体 ID**（同一玩家 / 同一对局事件有序）
4. **proto 字段编号上线后不复用**（见 §4）
5. **tip 错误码只能由导表器发号，不许手写数字**。`TipInfoMessage.id` 是客户端可见契约（客户端按 id 查文案），
   全仓一条数轴。段由 `data/tip/Tip.xlsx` 的组头行声明（`//guild_error base=14000 width=1000`），
   发号器按段分配并在生成期自检（段不重叠 / 码不越段 / 枚举名全局唯一）。
   历史教训：手写「私有段」只是**约定**，拦不住不知情的新服务 —— `go/match` 曾从 1 开始重数，
   20 个码全部压在 common / login 段上，客户端弹出的是完全无关的文案，且全程零报错。
   guild / friend / match 的 `constants_test.go` 里有 `TestNoHandWrittenTipCodes` 机械守住这条。
   「这个码算不算服务端故障」同样是表的一列（`fault`），生成到 `tip.Faults` 供 `serverbase.TipVerdict` 消费；
   服务里不许再手写故障码集合——那会把「段和发号分家」的病在分类上重新制造一遍。
   详见 [docs/design/tip-code-axis.md](docs/design/tip-code-axis.md)。
5. **ECS 组件访问**：per-tick 路径禁用 `get_or_emplace`；跨实体查询用 `try_get`，不用 `get`（见 copilot-instructions）
6. **保证修改代码的正确性、数据一致性**

## 8. 命名规范

- RPC 请求 handler 是 `cpp/nodes/scene/handler/rpc/...` 下的 `*Handler` 类
- 异步回复 handler 用 `On<Domain><Method>Reply`（`cpp/nodes/scene/rpc_replies`）
- ECS/域逻辑放 `*System` 类与 `*Comp` 结构（`cpp/libs/services/scene/...`）
- 用更清晰的动词式 RPC handler 名 + 薄包装委托模式（如 `ProcessClientPlayerMessage`）

## 9. 禁止事项与权限边界

- ❌ 客户端统一使用同级独立仓库 `../mmorpg-client/`；服务端不再维护 `client/` 目录，未获客户端任务授权时不要读取或修改独立客户端
- ❌ 不要手改生成产物（见 §3）
- ❌ 不要在 `docs/design/` 之外随便建 README，不要为记录改动新建 markdown（除非用户要求）
- ❌ 不要把 `player_id` 当 Prometheus label（高基数会爆）
- ❌ 不要用 `--no-verify`、注释断言、跳 test 等方式绕过安全检查
- ❌ 不要执行 `git push` / `git tag`；`git commit` 仅在用户明确要求时执行
- ❌ 不要登录远端账号（GitHub / k8s / 云厂商 / 注册表），不要改 CI 凭证 / secrets
- ❌ 不要把 secret / token / 密码写入 git 跟踪文件或日志
- ❌ 不要 `kubectl apply` 到生产或 `docker push` 到 registry；只可操作本地或用户明确指定的 dev 环境

## 10. AI 协作约定

在 §9 权限内，AI 可写代码（C++ / Go / Java / proto / yaml / shell / ps1）、文档和测试；可跑本地 docker-compose / `dev_tools.ps1`，建议 commit message / PR 描述，执行代码与设计评审，分析 `stress_summarize` 输出。不同 Agent 按 §10.1 分工。

### 10.1 编译 / 测试 / 压测协作

**Claude 可修改代码、测试和文档，但不执行编译、测试或压测命令；这些命令由 Codex 执行。**

- Claude ❌ 不跑 `msbuild` / `dotnet build` / `go build` / `go test` / `go vet` / `mvn` / `cmake` / `build.bat` / `build_linux.sh` 等任何构建、测试或压测命令，也不为“验证一下”而跑。
- Claude ✅ 改完代码后，给出 Codex 可直接执行的细节：**目标工程/服务、具体命令、工作目录、环境变量、前置清理、期望产物、通过标准、失败时要保留的日志或摘要**。多个工程要说明先后顺序；C++ MSBuild 必须串行 `/m:1`，并发会报假的 C1041 / LNK1104。
- Codex 按上述细节执行本地编译、测试、压测与汇总，不自行脑补压测口径或宣称性能结论。
- Claude 在没有 Codex 运行结果前，**不得声称“编译通过”“测试绿”“已验证”**；交付说明必须如实写明“未编译，待 Codex 验证”。

### 10.2 执行与交付

- 默认直接执行：完成会话启动门禁 → 明确范围 → 改代码/proto/yaml/脚本/文档 → 由有权限的执行者跑受影响的 build/test/lint → 汇报改动范围、验证证据、未验证项和剩余风险。需要 commit 时等用户明确发话。
- 动手前先检查工作区和相关文件的既有改动；不覆盖、回退或顺手整理他人的工作，不夹带任务外改动。
- 失败时不假装成功；命令失败后不连续自动重试，不注释断言或跳过测试，不用 `git reset` / `git checkout --` 销毁进度。保留关键错误与日志摘要并报告，等待决策。
- 请求触犯 §9 禁令，或任务范围明显扩大、遗漏关键文件、规范文档冲突、预计改动 30+ 文件、需要安装/升级工具、修改系统环境、关闭防火墙、写 secrets、触碰生产、build 损坏其他服务或即将 push 时，立即停止并报告，等待授权或决策。

## 11. 工程设计与编码原则（强制）

适用于新增、修改、重构和评审。“成熟工程团队标准”是模块接口清楚、接缝明确、改动可审、结果可验证，不是堆设计模式、层数、文件数或框架。

原则冲突时按以下顺序取舍：

`正确性 / 安全 / 数据一致性 / 向后兼容 > 简单 / 可读 / 与仓库一致 > 可维护 / 可测试 > 有数据支撑的性能 > 假想扩展`

已经文档化的 SLO、容量上限和 per-tick 热路径预算属于正确性约束，不降级为普通性能偏好。

偏离本节原则必须有当前需求或证据支撑，并在代码注释、设计文档或交付说明中记录原因与代价。

### 11.1 模块与职责

- **单一职责（SRP）**：一个模块只对一种变化原因或一类业务角色负责；这不等于“一个类只能有一个方法”。总是一起变化的规则放在一起，独立变化的策略、存储、传输和副作用分开。
- **高内聚、低耦合**：相关状态与行为就近收口；模块只了解完成职责所需的信息，依赖单向且不得形成循环。
- **信息隐藏与深模块**：用小而完整的接口隐藏验证、顺序、错误模式、配置与性能细节。承担协议转换、隔离外部依赖或稳定接缝的薄适配器有明确价值；避免没有隔离、转换或契约价值的连续透传层、万能管理器和需要调用者拼装内部步骤的接口。
- **接口是完整契约**：除类型外，还必须明确输入约束、不变量、顺序、所有权、错误、超时、幂等、线程模型和性能特征。调用者不依赖未承诺的实现细节。
- **开闭原则（OCP）**：面对已经确认且反复出现的变化轴，在接缝后增加适配器，避免每个新变体都修改全部调用者；扩展点必须受真实需求约束。
- **里氏替换（LSP）**：任何实现或子类型替换后都保持接口契约，不得加强前置条件、削弱后置条件或改变错误语义。
- **接口隔离（ISP）**：调用者只依赖自己需要的能力；但不得机械拆成大量单方法接口和透传层。
- **依赖倒置（DIP）**：业务策略不依赖数据库、网络、框架或全局单例的具体细节；依赖在清晰接缝处注入。
- **组合优于继承**：优先组合可替换行为构建能力；继承只用于真正稳定的 is-a 契约，避免复杂、脆弱且难以局部替换的继承树。

### 11.2 简单、标准、可读

- **KISS**：选择满足当前需求的最简单完整方案。优先标准库、仓库现有模式和团队已采用的成熟依赖；新增框架或依赖必须说明现有能力为何不足。
- **YAGNI**：不实现没有调用者、验收标准或近期计划的能力；扩展性以真实变化为依据。
- **DRY**：一条业务知识只有一个权威来源。只消除会共同变化的重复，不因几行形似代码就过早抽象。
- 控制流保持直线化：优先早返回和小范围变量，减少无必要的深层嵌套、隐藏跳转和跨层回调；函数保持同一抽象层级。
- 命名表达业务含义和单位，拒绝模糊缩写、布尔陷阱与魔法值。注释解释“为什么、约束和风险”，不复述代码“做了什么”。
- **显式依赖**：依赖通过参数、构造函数或装配层传入；时间、随机数、I/O 和副作用必须显式可见。限制全局状态、静态可变状态和隐式初始化顺序。
- **不用宏生成代码**：禁止 X-macro（`#define FOO_LIST(X) X(a) X(b)` 再展开成枚举 / 数组 / switch）以及任何"用宏拼出声明"的写法。它让代码无法直接阅读、跳转定义失效、编译错误指向宏展开处而非源头、调试器看不到真实符号。
  - **正确做法**：写普通枚举 + 一张平铺的常量表，用 `static_assert(数组长度 == 枚举 kCount)` 保证两者不脱节——这正是 X-macro 唯一真正提供的保障，而它不牺牲可读性。枚举的最后一项固定为 `kCount`，同时用作数组长度。
  - 示例见 `cpp/libs/modules/id_segment/guid_segment_registry.{h,cpp}`（`GuidKind` + `kGuidKindNames`）。
  - 允许的宏只有四类：头文件卫士；平台 / 编译期条件编译；必须拿到 `__FILE__`/`__LINE__` 的日志与断言；以及**必须往调用方注入控制流**（`return` / `continue` / `break`）因而函数写不出来的守卫宏——例如 `cpp/libs/engine/core/macros/return_define.h` 的 `ECS_GET_OR_RETURN` 系列。
  - 这四类之外一律用函数、模板或 `constexpr`。判据很简单：**这个宏是在生成声明，还是在做函数做不到的事？** 生成声明的一律不要。
  - 枚举的底层类型按**取值个数**选，不要跟着被编号对象的位宽走（`GuidKind : uint8_t` 表示"种类数 ≤ 255"，与 guid 本身是 uint64 无关）。
- 改动保持小而完整、可独立审阅和回滚；不在功能改动中夹带无关格式化、重命名或顺手重构。

### 11.3 正确性与生产质量

- 在系统接缝校验不可信输入；内部不变量尽早断言或显式失败。错误不得吞掉、伪装成功或静默降级；安全、权限、玩家资产和数据完整性路径默认 fail-closed。可用性优先的路径若选择 fail-open，必须记录风险依据并配套日志、指标和测试。
- 能在副作用前安全完成的纯校验应先完成；提交点仍须用同一并发控制机制原子复核权限、版本和状态，不能让预检代替提交校验。跨存储、跨服务或异步流程必须明确原子范围、提交点、回滚/补偿、重试与幂等语义。
- 重试只能用于可重试且幂等的操作，必须有超时、次数上限、退避与必要的抖动；支持取消并释放文件、锁、连接、线程与定时器等资源。
- 并发代码必须写清状态所有者、同步方式和生命周期；共享可变状态不得依赖“通常不会并发”的假设。进程内耗时和超时预算用单调时钟与截止时间；跨进程绝对截止时间使用协议约定的时钟和格式，并明确时钟偏差与过期语义。不得用“重试次数 × sleep”代替时间预算。
- 安全遵循最小权限、默认拒绝、纵深防御和敏感信息最少暴露；真实 secret、token 和密钥值不得硬编码或进入日志、指标、错误响应。主体标识只在确有诊断需要时最小化、脱敏并受控记录，不得外发给无权调用者。
- 已发布协议、持久化数据、外部配置和滚动升级接缝优先向后兼容。破坏性变更必须有版本、迁移、灰度与回滚方案，并验证新旧版本并存窗口。
- 关键路径提供结构化日志、低基数指标和必要追踪；日志通过受控关联标识定位请求、阶段和结果，但不得使用 `player_id` 等高基数值作指标 label。
- 性能优化必须由 profiling、benchmark 或压测数据驱动；先修算法、I/O 和数据结构瓶颈，不以牺牲正确性和可读性换未经测量的微优化。

### 11.4 测试与评审

- **面向接口测试**：接口同时是调用面和测试面。优先验证可观察行为与契约，避免测试私有实现步骤；测试替身通过同一接口注入。
- 缺陷修复原则上先写能稳定复现问题的回归测试；红/绿运行证据按 §10.1 由 Codex 执行。确实无法先写时，必须说明原因并给出替代验证。
- 按改动风险覆盖正常、边界、失败、回滚、幂等、并发和兼容场景中适用的部分；测试必须确定、可重复。单元测试不得依赖执行顺序、真实墙钟或外部残留状态；集成测试必须隔离输入、限定等待并清理自身状态。
- 评审既检查代码，也检查遗漏的同类入口、调用点、生成源、配置、迁移、监控、文档和清理路径。新增状态或规则时必须搜索所有创建、读取、更新、迁移、销毁与恢复路径并逐项对齐。
- 合并前执行受影响范围的格式化、静态检查、构建和测试；按本文件现有门禁区分静态、编译、单测、集成、真实环境和压测证据，任何未执行项都明确报告。

### 11.5 执行与完成门禁

1. 动手前能用一句话说明目标模块的职责、调用者、变化原因和不变量；复杂跨模块变更先落对应设计文档。
2. 先找仓库现有实现、标准库和成熟依赖，沿用已有分层、命名、错误语义和测试方式。新增接口或接缝必须对应真实变化点、外部系统隔离或测试隔离需求；只有一个实现且没有上述需求时保持具体。
3. 实现保持最小且完整，不夹带无关重构，不新造平行框架，不复制第二份业务真相。原则冲突或确需偏离时记录证据、原因和代价。
4. 完成时按风险检查 `SRP / OCP / LSP / ISP / DIP / 高内聚、低耦合 / 信息隐藏 / KISS / YAGNI / DRY / 组合优于继承 / 显式依赖 / 面向接口测试`，以及 `失败 / 数据 / 并发 / 兼容 / 安全 / 可观测性 / 改动范围` 中的适用项；不适用项无需逐项罗列。
5. 按 §10.1 完成相应构建、测试和 lint 协作；只报告实际证据、未验证项和剩余风险，不用静态阅读代替运行证据。

### 11.6 实例与布局分层（数据层 / 显示层）

一条记录只回答一个问题：**「它是什么」，还是「它摆在哪」**。实例是数据层的真相，摆放只是显示层的一种策略。两者写在一起时，实例记录会被迫携带在别的场景毫无意义的位置字段，而换一种摆法就要同时改协议、改存储、改客户端。

适用于任何「多个实例 + 有位置概念」的系统：背包 / 仓库 / 装备、宠物出战、队伍站位、技能栏与快捷栏、时装外观、坐骑、好友分组、邮件附件。参考实现与完整推导见 `docs/design/bag-instance-layout-split.md`。

- **实例层只描述实体**：身份（guid）、模板（config_id）、数量、状态、时间戳。不含 `pos` / `slot` / `index` / `bag_type` / 站位 / 排序等「摆在哪」的字段。判据：同一实例记录被邮件附件、掉落、交易、战斗快照复用时，如果需要写下「这个字段在这里被忽略」的注释，就说明分层错了。
- **布局层只描述排列**：容量、槽位、占位（footprint）、顺序、整理策略。扁平、固定槽、格子是同一模型下的三种布局策略，不是三套系统；新增一种摆法只应新增一个布局实现，不改实例层，也不改调用方。
- **接缝判据（可 grep，硬约束）**：布局层源码里不得出现任何领域概念——本仓库即 `config_id` / `ItemComp` / `ItemTable` / `entt`。布局层需要的领域信息（如格子占位 w×h）由桥接层显式传入，布局层不得自行反查配置表。这条缝一旦漏，两层就退化回一层。
- **「是什么」不与「在哪」共用一个字段**：`equip_slot`（部位即位置）这类设计在「一个部位要放两件」时立刻破产。正确形状是实例上写**种类**（`equip_kind`），槽位由**配置表**枚举（`EquipSlot` 表中多行同 kind ＝ 同部位多槽）。同理，不得用 `active_xxx_id` / `equipped_xxx` 这类标量字段表达「当前出战/装备的那一个」——它把「同时出战两个」写死成了协议变更；用槽位表承载，数量变化只改表。
- **集合顺序必须显式**：`repeated` / `vector` 的下标不得承载业务语义。位置要么写成字段，要么由槽位表给出；靠下标兜住的顺序会在序列化、合并、跨服迁移、并发写入中的任一环节静默丢失，且丢失时无任何报错。
- **显示态不进持久化记录**：在线状态、客户端展示缓存、以及任何可由数据层重算的派生排列，不得写进要落库的 message。持久化结构里凡是注释「仅内存 / 不入库」的字段，都应移到独立的运行时结构——注释拦不住序列化。
- **命名跟着分层走**：通用接口名不得泄漏某一种布局的实现细节（`GridsNeededFor` 这种名字把「要几个实例」和「占几格」混成一件事，应为 `StacksNeededFor`）。

新增或修改上述系统前，先按本节确认分层再动手；已上线结构受 §11.3 向后兼容约束，改动需配版本、迁移与回滚方案。
