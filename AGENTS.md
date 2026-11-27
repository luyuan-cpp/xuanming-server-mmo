# mmorpg 项目规范与 AI 协作守则

> 本文件是本项目对 AI Agent（Claude Code / Codex / Gemini / Cursor / Copilot 等）和人类开发者的**唯一规范总入口**。
> `CLAUDE.md` 与 `.github/copilot-instructions.md` 只通过 `@AGENTS.md` 导入本文件，不在多处重复维护规则；规则变更只编辑本文件或 `docs/rules/` 下对应的分章文档，导入文件永远保持单行导入。仓库既有“`CLAUDE.md §N`”或“`.github/copilot-instructions.md`”引用均解释为本文件对应章节及其引用的分章文档，新引用统一写“`AGENTS.md §N`”。

## 会话启动门禁（每次必做）

**AI 没有可靠的跨会话记忆。** 每次新会话动手前必须按序读完：

0. **新电脑 / 新 AI Agent 环境必先安装必装 Skills**（`mattpocock/skills`、`DietrichGebert/ponytail` 及精选 ≥2 万 Star 代码工程 Skills），清单与安装命令见 → [docs/rules/required-skills.md](docs/rules/required-skills.md)
1. `AGENTS.md`（已通过 `CLAUDE.md` 或 `.github/copilot-instructions.md` 自动导入的无需重复读取）以及任务涉及的 `docs/rules/*.md` 分章细则
2. `docs/PROGRESS.md` —— 当前进度（最新条目在文末；按任务关键词 grep 标题再读对应条目，不要从头整份读）
3. `docs/planning/<相关玩法>.md`（玩法策划）与 `docs/design/<相关服务>.md`（技术架构）—— 任务相关设计（2026-11-25 目录分拆旧→新路径映射见 [docs/planning/MIGRATION-MAP.md](docs/planning/MIGRATION-MAP.md)）
4. `git log -20 --oneline` —— 最近改动
5. 当前打开的 PR / Issue

先确认任务范围、既有改动、约束和冲突；没读懂就不动手。

**上线状态是兼容判断的前提**：负责人 2026-10-18 确认项目尚未上线。涉及混跑、降级、迁移、灰度或回档时，先读 [存盘围栏设计](docs/design/db-write-fencing-mysql.md) 文末「上线状态」，按其中限定的上线前范围判断阻塞；上线后以当时线上状态为准。

## 规范分章索引（`docs/rules/`）

| 章节 | 主题 | 完整规范文档 |
|---|---|---|
| 门禁 / §10 | **必装 AI Agent Skills 清单**（新电脑 / 新 Agent 必先安装） | [docs/rules/required-skills.md](docs/rules/required-skills.md) |
| §3 / §4 | 生成物边界、proto 同步、字段类型、tip 与配置表 schema | [docs/rules/proto-and-tables.md](docs/rules/proto-and-tables.md) |
| §6 | 压测纪律、基线清场与复盘脚本流程 | [docs/rules/stress-testing.md](docs/rules/stress-testing.md) |
| §7 | 系统不变量（永久 ID / SnowFlake、Kafka 防僵尸、tip 单轴、ECS、逻辑库自治） | [docs/rules/invariants.md](docs/rules/invariants.md) |
| §8 / §9 | 命名规范第一原则、关联 Git 仓库与权限/线程硬边界 | [docs/rules/naming-and-boundaries.md](docs/rules/naming-and-boundaries.md) |
| §10 | AI 协作分工（Claude / Gemini / Codex 仅作图）、写回流程、模型档位与设计落盘 | [docs/rules/ai-workflow.md](docs/rules/ai-workflow.md) |
| §11 | 工程设计与编码原则（SOLID/KISS/不用宏/裸指针约束/实例布局分层/muduo 生命周期） | [docs/rules/engineering-principles.md](docs/rules/engineering-principles.md) |
| §13（位图） | 位图（bit）优先存储规范、领取状态归属与策划删表 ID 红线 | [docs/rules/bit-storage-and-table-ids.md](docs/rules/bit-storage-and-table-ids.md) |
| §13（运行时） | C++ 编码/门禁/生命周期、通信广播、gRPC 关机与构建测试命令 | [docs/rules/cpp-runtime-and-build.md](docs/rules/cpp-runtime-and-build.md) |

---

## 1. 项目基本信息

- **类型**：MMORPG 服务器，多 zone、AOI、ECS 场景
- **多语言后端，按职责拆分**：
  - **C++ 节点**（`cpp/nodes/*`）：scene / gate / battle 运行时进程 + RPC handler
  - **C++ 共享逻辑**（`cpp/libs/services/scene/*`）：ECS 域逻辑（`system` / `comp`）
  - **Go 微服务**（`go/login` 等）：go-zero，login / db / scene_manager / friend / guild / chat
  - **Java 网关**（`java/gateway_node`）：Spring Boot，zone 目录 / gate 分配 / admin API
- **协议**：C++ 节点间 muduo TCP RPC、Go/C++ 控制面 gRPC，以及 Kafka 异步命令与事件
- **基础设施**：MySQL（→ 迁移中：TiDB 全区全服数据层，见 `docs/design/global-data-layer-tidb-decision.md`）+ Redis + Kafka + etcd

## 2. 中文回复与日志语言

- 所有 AI 协作产出**用中文**。注释、commit message、文档全中文。
- 服务和脚本的日志文案（含标准输出和标准错误中的诊断信息）必须使用**英文**。

## 3. 生成代码与可编辑边界

- ❌ 禁止手改 `generated/` 或各语言 `proto` 输出树下的生成产物；C++ 守护段只写在 `///<<< BEGIN WRITING YOUR CODE` 内；不手写与 proto 重复的并行 struct。
- 完整细则见 → [docs/rules/proto-and-tables.md](docs/rules/proto-and-tables.md#1-生成代码与可编辑边界)。

## 4. proto 同步、tip 与配表 Schema

- 改协议先改 `proto/` 再跑 `dev_tools.ps1 -Command proto-gen-run` 并重编受影响服务；上线后字段号不复用（`reserved N;`）。
- 改动涉及客户端可见 `.proto` 或 `Tip.xlsx` 时，服务端生成完毕须同步在同级 `mmorpg-client` 执行 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1` 与 `tools/client_compile_check.ps1`（完整步骤见 `docs/rules/proto-and-tables.md` §2），防止客户端 `*Rpc.cs` 与 `Proto/Generated/*.cs` 错位触发 Unity Safe Mode。
- tip 错误码只由 `data/tip/Tip.xlsx` 导表生成，不手写数字；配置表 schema 事实源在 `data/schema/*_table.proto`（见 [data/AGENTS.md](data/AGENTS.md)）。
- 强制字段类型（坐标 `double`、`BaseAttributesComp`/64位 ID/时间戳/货币 `uint64`）与完整流程见 → [docs/rules/proto-and-tables.md](docs/rules/proto-and-tables.md)。

## 5. 决策记录入口

`AGENTS.md` 只保留稳定总纲和索引，不维护长决策表。**没写文档 = 没说过**（下个 AI 不会记得）。

- 规范分章细则 → `docs/rules/`
- 玩法策划与数值/经济设计 → `docs/planning/`（上位红线 [docs/planning/project-vision.md](docs/planning/project-vision.md)；旧 `docs/design/` 迁出对照见 [docs/planning/MIGRATION-MAP.md](docs/planning/MIGRATION-MAP.md)）
- 异机 Codex UI / 美术交接单 → `docs/planning/ui-handoff/`（落盘后须在回复直接提供可复制的**纯功能作图清单**，输出放 `D:\work\image\designs\<新专题目录>\`，严禁写画风/色号及任何 Unity/程序内容）
- 技术架构 / 服务级工程设计决策 → `docs/design/` 或对应 `docs/design/<service>.md`
- 压测结论 → `docs/stress/stress-<round>-*.md`
- 运维手册与事故复盘 → `docs/ops/`
- 交接说明与会话小结 → `docs/handoff/`（2026-11-20 起仅保留当前有效交接；旧交接用 `git show ccf2840b2:docs/handoff/<文件>` 查）
- 已过期、仅作历史保留的文档 → `docs/archive/`
- 周期进度与流水账 → `docs/PROGRESS.md`（只追加，不删旧条目；2026-11-20 前历史用 `git show ccf2840b2:docs/PROGRESS.md` 查；`.gitattributes` 配了 `merge=union`，补充旧条目另起一条）
- 文档总索引 → `docs/README.md`（新增文档后跑 `python tools/scripts/gen_docs_index.py` 更新）

## 6. 压测纪律（最重要）

- **复盘只读 `stress_summarize.ps1` 输出**，不手 grep raw prom dump；**没有对比表不许声称“性能提升”**。
- 跑测前必须固化 `prev-summary.txt`、清空日志/Redis/Kafka/MySQL/etcd，压测期间禁止上传日志。
- 完整压测前后强制流程与脚本命令见 → [docs/rules/stress-testing.md](docs/rules/stress-testing.md)。

## 7. 不变量（数据一致性 / 安全）

跨服务必须保持的硬不变量，违反 → PR review 直接拒（完整细则见 → [docs/rules/invariants.md](docs/rules/invariants.md)）：

1. **永久 ID 与 SnowFlake 隔离**：发号器永久 ID（63 位打散计数器，无位布局，禁 `ParseSnowflakeGuid`，生产强制双本地 etcd + 全局登记处）与 SnowFlake（只剩 `scene_manager` 的 `scene_id`）严禁混用。
2. **Kafka 防僵尸与保序**：定向节点命令必带 `target_instance_id`；`topic key = 业务实体 ID`。
3. **proto / 配表字段号上线后不复用**。
4. **tip 错误码全仓单轴**：只由 `Tip.xlsx` 导表发号与判定 `fault`，严禁手写数字或私有 fault map。
5. **ECS 组件访问**：按 [ecs-component-access-rules.md](docs/design/ecs-component-access-rules.md) 分类，读路径只读，跨实体用 `try_get`。
6. **逻辑库自治**：一库一服务，禁止跨库 SQL，只走 RPC；事务内不调 RPC。
7. **保证代码正确性与数据一致性**。

## 8. 命名规范

- **第一原则（强制）**：所有命名（类、函数、变量、字段、proto、配置表、错误码、指标等）**必须清晰易懂且语义完全准确**，拒绝模糊缩写、歧义布尔值、泛化名与误导性历史名。
- 分层命名（`*Handler` / `*System` / `*Comp` / `On<Domain><Method>Reply` / RPC0 调用点 lambda）详见 → [docs/rules/naming-and-boundaries.md](docs/rules/naming-and-boundaries.md#1-命名规范8)。

## 9. 禁止事项与权限边界

- **关联主仓库**（按 GitHub 地址定位，不写死盘符）：`luyuan-cpp/xuanming-server-mmo`（本仓）、`luyuan-java/xuanming-server-mmo`、`luhailong-python/xuanming-server-mmo-python`、`luhailong-cpp/mmorpg-client`、`luyuan-cpp/proto2mysql`。
- ❌ **不要开启新的线程**：默认异步挂主 EventLoop，除非压测证实瓶颈确需新线程（如评估新增 Kafka consumer 线程）且**先与程序/负责人确认说明原因**并登记 `test_cpp_thread_sources.py`。
- ❌ **严禁引入任何可能导致游戏不过审或被迫下架的功能/文案/开关**（一票否决，2026-11-26 强制）：禁止在客户端内置二维码/收款码/外链捐赠/切支付及无版号内购；激励视频默认首审 `enabled = 0` 且仅允许发放不可交易绑定银两（`CURRENCY_TYPE_BIND_GOLD = 3`），严禁诱导点击/看广告支持作者文案；禁止绕过实名防沉迷、账号真注销与 UGC 内容安全（细则见 `docs/rules/naming-and-boundaries.md`、`docs/design/rewarded-ad-system.md` 与 `docs/planning/project-vision.md`）。
- ❌ 禁止手改生成物、禁止把 `player_id` 当 Prometheus label、禁止 `--no-verify`、禁止未授权 `git commit`/`push`/`tag`、禁止写 secret 进仓库、禁止碰生产集群。
- 完整清单见 → [docs/rules/naming-and-boundaries.md](docs/rules/naming-and-boundaries.md#2-禁止事项与权限边界9)。

## 10. AI 协作约定

- **§10.1 分工**：Claude 改代码/测试/文档（交付 C++ 前先跑只读预检 `test_cpp_no_raw_pointer_member.py` 与 `test_cpp_async_callback_capture.py`），不跑编译/测试/压测命令，交由 Gemini 执行；**Codex 仅负责作图（UI / 美术素材 / 视觉精修）**，不参与编码、分析、编译、协议生成、测试、压测与运维收尾（原由 Codex 承担的除作图外的职责统归 Gemini）；无运行证据不得声称已验证。
- **§10.2 执行与交付**：先查既有改动，不覆盖他人工作；失败保留首错日志，遇红线立即停下汇报。
- **§10.3 模型档位（强制）**：写/改代码以及**需求分析、设计分析、思想分析、漏洞分析、步骤分析、执行步骤**必须用各软件最高档模型（Claude Code `Fable 5.2 Max` / Gemini `Gemini 4 Neon Extra`）；仅编译、生成、跑测与冒烟用低档模型（Gemini `Carbon Medium` / Claude Code `Opus 5 High`）。**Codex 仅负责作图（UI / 美术素材 / 视觉精修）**，不参与编码、分析、编译、协议生成、测试、压测与运维收尾。
- **§10.4 设计方案落盘 `.md`、反「上班」策划红线与异机作图交接（强制）**：以后编写策划/技术设计方案时，Claude Code（及其他 AI Agent）写好设计后必须将**需求分析、设计分析、思想分析、漏洞分析、步骤分析、执行步骤**及完整方案按目录分类保存——**玩法策划写入 `docs/planning/<主题>.md`，UI/美术交接写入 `docs/planning/ui-handoff/<主题>.md`，技术架构与工程实现写入 `docs/design/<主题>.md`**（或更新既有对应文档），不得只留在对话中；**策划设计严禁做成像「上班」**（禁止每日签到/点雕像收菜/捐献打卡、日清活跃条与高额日限购倒逼、固定钟点同时在线、强组队日清与帮会催打卡指标，禁止零操作收益倒挂正常游玩，详规见 [docs/planning/project-vision.md](docs/planning/project-vision.md) 红线第 2 条）；玩法接入完毕须确认逻辑、服务端/客户端代码与 UI 接入全部闭环，确实还能扩展得更好玩才继续扩展（**严禁硬扩展，没可玩性不要扩展**）；**需要让 Codex 作新 UI 时必须告知负责人，写完 `.md` 后必须在回复中直接给出可直接复制给 Codex 的纯功能作图信息**（另一台电脑 Codex 拿不到任何本地文件，严禁只给 `.md` 路径），且**只告诉 Codex 做什么 UI，不要告诉 GPT / Codex 要定什么风格**（严禁写任何画风词、风格词、材质词或色号/配色），**输出目录一律新建在图仓 `D:\work\image\designs\<新专题目录>\` 且严禁与 Unity / 游戏代码有任何关系**（禁写 `Unity`、`uGUI`、`Prefab`、`Assets/Resources/`、`.meta`、C# 脚本名等，只写要做什么 UI、用途、尺寸、文件名、透明底 PNG-32、文字留白规则、拉伸留白与图仓输出目录；详见 [docs/planning/project-vision.md](docs/planning/project-vision.md) 第三节）。
- 完整分工与写回流程见 → [docs/rules/ai-workflow.md](docs/rules/ai-workflow.md)。

### §10.4 异机 UI / 切图交接复核（2026-11-26）

本节适用于每一次新做、修改、返工、重新切图，以及换电脑后的接续；不能因为同一专题先前展示过参照就跳过本次开工展示。

- **先展示真实参照（2026-11-26 扩充至 7 张长期标准参照）**：交接方先在图仓核实并实际查看下列七张 PNG，再在本次聊天中逐张嵌入图片，标明名称和各自准确的图仓相对路径；只列文件名或链接不算展示。帮会图仍是当前 UI 风格、控件和字体的首要标准。同功能另有用户已确认的 `designs` 成图时，也核实、查看并展示；目录存在、版本号较新或仅有预览不等于已确认。图仓在新电脑上缺图时如实报告缺失，取得真实文件后再展示，不用文字描述或其他图代替。
  - 邮件界面：`designs/mail-ui-v1/01-mail-event.png`
  - 组队界面：`designs/team-ui-v2/team-ui-v2.png`
  - 帮会界面（首要）：`designs/ranked-play-v2-ui-20261125/assets/approved-guild-reference.png`
  - 天墉生死擂 · 下达战书：`designs/ui-standard-references/jianghu-duel/01-challenge-approved.png`
  - 江湖恩怨录：`designs/ui-standard-references/jianghu-duel/02-grudges-approved.png`
  - 帮派恩怨墙：`designs/ui-standard-references/jianghu-duel/03-guild-wall-approved.png`
  - 江湖战书 · 三种弹窗：`designs/ui-standard-references/jianghu-duel/04-popups-approved.png`
- **排除误用的赛事预览**：《群仙问道赛 · V2 增量组件》`designs/season-team-tournament-v2/previews/07-components.png` 只是该批组件拼装预览，不用于后续 UI 的开工展示、同功能参照或生图／AI 编辑附图，也不推荐为固定标准或画风依据。保留其历史交付与来源记录，不因本条改写旧图。
- **只交本次美术需求**：新专题的成图、预览、正式切片和逐图来源放在图仓 `D:\work\image\designs\<新专题目录>\`；修改既有专题时沿用其已核实的任务目录和版本合同。可复制的作图正文只列本次要做的 UI、用途、内容与布局、状态展示、像素尺寸、文件名、透明底 PNG-32、文字留白和必要的拉伸端头留白。不得自行扩展未要求的玩法、页面、状态或切片；没有实质需求不硬扩展。
- **正文与研发彻底分开**：上文 §5 和 §10.4 关于不写程序内容的禁令，适用于交给异机 Codex 的 `.md` 作图正文及聊天可复制正文。正文不得出现引擎术语（包括 `Unity`、`uGUI`、`Prefab`、`Canvas`、`Sprite`、`spriteBorder`、`.meta`）、客户端工程路径、C# 类名或脚本名、协议文件名或 RPC 名；也不得给 GPT / Codex 画风、配色、色号、材质、字体或装饰画法指令。若确需记录本机资源导入映射、脚本或协议依赖，只能在交接 `.md` **文末**另设「本机研发导入备忘」，明确不属于作图正文，回复时不复制给 Codex。
- **职责与回复**：这份交接只要求 Codex 作图、修图、切图及图片验收；协议、导表、客户端与服务端编码、编译和游戏测试归 Gemini / 本机研发。交接 `.md` 落盘后，回复必须直接给出可整段复制给异机 Codex 的完整作图正文，不能只回 `.md` 路径，也不能把文末研发备忘混入可复制内容。

## 11. 工程设计与编码原则（强制）

优先级：`正确性 / 安全 / 数据一致性 / 向后兼容 > 简单 / 可读 / 与仓库一致 > 可维护 / 可测试 > 有数据支撑的性能 > 假想扩展`。

- **§11.1–§11.5**：SOLID、KISS/YAGNI/DRY、**系统/模块间采用 Actor 消息通信（状态私有、只传不可变消息、串行消费、禁借全局单例绕过；模块内部保持正常直接调用，不把内部函数过度拆成消息）**、**适合用 23 种设计模式时主动采用并向程序说明原因**、**优先使用当前开发语言的标准库，标准库没有时再去 GitHub 找同语言高 Star 成熟库（门槛随该语言生态体量浮动），交由程序决定并说明原因**、显式依赖、**C++ 非必要不写宏定义（难懂且破坏可读性，禁止用宏生成声明）**、**C++ 裸指针约束（禁止类/结构体/联合体非静态成员、容器元素如 `std::vector<T*>`、业务返回值及裸 `new`/`delete`；函数参数可传指针，局部变量仅限接 ECS 组件、配置表行及无法改签名的外部/框架/生成/C API 接缝指针并就地判空使用、严禁持有或向外传播；类成员受 pre-commit/CI 及按需 MSBuild AST 门禁强制拦截）**、fail-closed、面向接口测试与交付门禁。
- **§11.6 实例与布局分层**：实例层只描述「是什么」，布局层只描述「摆在哪」，布局层源码不得出现领域概念。
- **§11.7 连接与回调生命周期（muduo 与异步回调，强制）**：应用层不拥有连接也不裸绑自己——**存连接只存 `weak_ptr`，关机两跳 `queueInLoop` 再 `quit()`**；新异步代码按**冷热分离四选一**（ECS 业务只按值捕 `entt::entity` + 业务 ID 保住 CPU Cache Line、多步流用协程/`LoopTaskScope`、基础设施对象用 `enable_shared_from_this` + 私有构造/`Create()` + `weak_from_this()`、冷路径句柄才用内部 `shared_ptr<State>` 且严禁塞业务热数据）；异步回调**禁 `[=]`/`[&]`、禁裸 `this`/引用、禁 `std::bind(..., this/shared_from_this())`、构造期不绑回调**（受 pre-commit 与 CI `test_cpp_async_callback_capture.py` 自动拦截），并配**先析构再触发回调**单测。
- 完整细则见 → [docs/rules/engineering-principles.md](docs/rules/engineering-principles.md)。

## 12. 双版本服务器（C++/Go 版 + Java 版，强制）

- 2026-09-29 起本仓库（C++ 节点 + Go 微服务）与 **Java 版**（`https://github.com/luyuan-java/xuanming-server-mmo`）并行演进；所有功能两版都要做，交付写明另一版状态。
- 对账本为 Java 仓库根目录 `PARITY.md`；两版仅共享客户端契约（帧格式、客户端可见 proto、tip 码、配置表数据），服务端内部按各自语言惯用方式实现。
- Java 版选型优先同类头部高 Star 成熟库（参考 ≥ 2 万 Star 并按领域生态浮动；JDK 与 Spring 视为事实标准），构建与测试以 Java 仓 `AGENTS.md` 为准。

## 13. 架构、构建与编码细则

- **位图（bit）优先存储与策划删表 ID 红线**（含空位永久占位、严禁复用旧 ID、查表判空、`bitset.count()` 陷阱、灰度高位未知 bit 透传、给策划与程序的完整清单）→ 见 [docs/rules/bit-storage-and-table-ids.md](docs/rules/bit-storage-and-table-ids.md)。
- **C++ 运行时与构建细则**（避免 `static` 存储、K8s Gate 暴露、MSVC `/utf-8`、Lambda `C3495`、无裸指针成员门禁、`TimerTaskComp` 生命周期、流量统计、三级广播 bitmap、`muduo_grpc` 服务端与关机、C++/Go/Java 构建与本地开发命令）→ 见 [docs/rules/cpp-runtime-and-build.md](docs/rules/cpp-runtime-and-build.md)。
