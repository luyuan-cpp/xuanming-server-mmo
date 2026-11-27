# 数据覆写审计第二轮（r2）验证交接（2026-11-26，交 Gemini）

审计文档：[data-overwrite-rollback-audit-r2-20261019.md](../design/data-overwrite-rollback-audit-r2-20261019.md)。逐条最终状态以 §5.0 为准，各批收口记在 §6。

**2026-11-26 晚更正：代码还没修完。** 以下三处由 Claude 在修：
- 第 4 轮查找报的 3 条 high、4 条 medium；
- 11-19 评审记下的约 30 条建议；
- 早期各批的 4 条遗留（审计 §6「2026-11-26 清点」）。

下表的条目可以先验，但这三处修完会新增 PROGRESS 条目，受影响的模块要重跑。最省事的是等审计 §1「仍开着」清空之后，再整体跑一遍。

本单只排顺序、定口径。具体命令和每批新增的用例名，在各条 PROGRESS 条目的「交 Codex」段里，一律按交 Gemini 读。只在 main 上做，不建分支。项目未上线，TiDB / MySQL / Redis / Kafka / etcd 数据都可以全清。

## 1. 命令在哪

2026-11-19 起的条目在当前 `docs/PROGRESS.md`；更早的只在 `git show ccf2840b2:docs/PROGRESS.md`。按下表的标题开头 grep。

| 日期 | 标题开头 | 覆盖 |
| --- | --- | --- |
| 10-19 | `location 释放绑定第二轮审计修复`、`EnterScene 陈旧位置判定补注册表正面证据` | r2-gate#1、r2-sm#1/#2/#3 |
| 10-24 | `第二轮审计 medium 修复：r2-lc#1/#2` | r2-lc#1/#2（同条的 r2-rr#5 已随迁移删除） |
| 10-24 | `Claude：第二轮审计 r2-ds#1 / r2-ds#2` | r2-ds#1（r2-ds#2 已删） |
| 10-24 | `Claude：第二轮审计 r2-battle#2 / r2-guild#1` | 停机或崩溃后的结果交付 |
| 11-05 ~ 11-12 | 标题含 `数据审计遗留·` 的 24 条（含一条「补」开头的） | §5.0「已修、仍成立」的其余各条。`第二轮过时文档与注释`、`跨区传送文档其余段` 两条只改文档和注释，随编译即可 |
| 11-06、11-10 | 标题以 `Claude：评审修正·` 开头的 8 条 | 当时各批独立评审的修正 |
| 11-06 | `Match 个人结算首投积压告警` | 告警规则与合同用例 |
| 11-10 ~ 11-11 | `r2-battle#2 / r2-guild#1 独立评审与修正`、`go/match 全球局终局协议收尾批 B5 落码`，以及同日 B5 后续 4 条（`B5 第二轮评审修正`、`旧地址上线时丢掉`、`B5 三条残余`、`B5 再修三条`） | r2-battle#2、r2-guild#1 收尾；match 不可用时 ABORT 的硬上限 |
| 11-12 | `客户端·强化期望等级与幂等键` | 客户端仓 main |
| 11-12 | `数据覆写审计 r2 修复批次 S、T、U`、`数据覆写审计 r2 批 T、U 补做` | r2-ps#11、r2-trade#1、r2-doc#1/#2 |
| 11-19 | 标题以 `Claude：评审修正·` 开头的 6 条、`修复身份落码`、`排空框架首次接纳先提交起点` | 11-12 残余收口的评审修正；负责人定案的两项 |
| 11-20 | `数据覆写审计 r2 修复批次 S、T、U 的独立评审与修正`、`批 U 文档残留`、`批 S、T、U 评审可选项全部落码` | S / T / U 的评审修正与可选项 |

## 2. 顺序与通过标准

1. **协议生成与导表**：r2 的协议改动分在十月的 C、D、F、J、K1、K2、K3、N、R2b 批，以及 11-12、11-20 两批（如 `transaction_log` 索引、交易的 `escrow_owner_id`）；F、R6 批加过 tip 码。客户端仓也改了（上表 11-12 的客户端条目，和 11-19 对应的评审修正）。
   - 按 AGENTS.md §4 用缺省配置生成（会写客户端仓）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。生成器源码变了，先跑 `-Command proto-gen-build`。
   - 导表按 `data/AGENTS.md`。
   - 再到同级 `mmorpg-client` 跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`。
2. **构建**：
   - 先跑只读预检 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。
   - `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build`（全部 Go 二进制）。
   - `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`，必须串行，0 个 error C / LNK。
3. **Go 单测**：下列模块各跑 `go vet ./... && go test ./... -count=1`，要求 0 FAIL。
   - `go/shared`（重点 `drainproof`、`kafkautil`、`assetop`、`authoritysql`、`snowflakealloc`、`handofffence`）；
   - `go/data_service`、`go/db`、`go/scene_manager`、`go/login`、`go/guild`、`go/match`、`go/rank`、`go/mail`、`go/trade`、`go/player_inbox`；
   - `tools/data_consistency_check`（独立 go.mod）。
   - 各条目点名的新用例，要在输出里出现并通过。
   - 标了真库的用例，自起一次性 MySQL / TiDB 跑，别借 `codex-*` 容器。
4. **C++ 单测**：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build` 全量。它不重编库，所以第 2 步的 `game.sln` 必须先编完。重点看 `cross_zone_test`、`bag_test`、`rpc_controller_test`，以及 gate / battle 的测试工程。
5. **Java 网关与商城**：
   - 在 `java/` 下跑 `mvn -B -pl gateway_node -am test` 与 `mvn -B -pl mall_game -am test`，都跑全量；先跑条目里点名的测试类。
   - 网关的 `mmorpg` 库重建，或者执行 `ALTER TABLE zone_config ADD COLUMN version BIGINT NOT NULL DEFAULT 1 AFTER role_capacity;`。
6. **部署与告警契约**：条目里列出的 `tools/scripts/tests/*.tests.ps1` 全部通过（至少有 `k8s_deploy_contract`、`maintenance_drain`）。
7. **演练与冒烟**：
   - 排空框架几批的 Kafka 演练：`r2-kc#1`、`排空框架与消费组残余收口`，以及 11-19 的两条；
   - 客户端条目的 robot / Unity 冒烟。

## 3. 要先知道的

- **要改代码就停**：编译报错或用例变红、需要改源码或断言时，保留首错原文，停下交回 Claude。负责人 11-26 定的分工是：逻辑代码由 Claude 改，Gemini 只负责生成、编译、测试和冒烟。重新生成产物、重建库、清数据可以直接做。
- 工作区里有很多别的会话的未提交改动。红了先看是不是本线的文件，本线范围以各条目的文件清单为准。
- **不在本单**：
  - 随迁移失效的项（r2-db#1/#2/#3、r2-rr#1~#5、r2-mz#1/#2、r2-battle#1/#4、r2-ps#1/#3/#4、r2-login#1、r2-ds#2，以及 r2-low#1/#2 的代码部分）机制已删，不用验；
  - r2-ps#2/#5/#8/#9/#10 归「混版本持久字段保护」那条线；
  - r2-v1#1 主路径与 r2-v1#2 归「全球经济系统代码审核」那条线；
  - 评级停写不卡消费是全服匹配第四批，纯单测已过。
- **已定案的行为，不要当缺陷报**：
  - 排空框架遇到位点越过保留范围就 HOLD，不跳过；
  - 混版设计 §8.2 的世代预检不做；
  - FIGHTING 冻结续冻上限是到期 + 15 分钟；
  - match 不可用时，ABORT 最晚约在截止 + 14.2 分钟。
- Java 版只登记待做（Java 仓 `PARITY.md` 已登记），本单不涉及。
- 跑完在 `docs/PROGRESS.md` 末尾追加一条：每个条目跑了什么，点名用例有没有出现、是否通过，哪些没跑、为什么没跑。
