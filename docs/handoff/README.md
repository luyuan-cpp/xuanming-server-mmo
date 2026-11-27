# 换电脑接续：全部交接单的总入口（2026-11-27）

> 负责人在新电脑上说「继续做完 handoff」时，Claude Code 从本单开始：先过 §1，再按 §3 → §4 的顺序做。
> 本表是 11-27 旧电脑回收前的快照。各单开头的状态行比本表新，两边不一致时以各单为准，并顺手改本表；`docs/handoff/` 里有本表没列的新单，按它自己的开头做，并补进本表。

## 1. 开工核对（新电脑，Claude Code）

旧电脑回收后，没推送到 GitHub 的东西都没了。下面缺一条就停下问负责人，不要凭印象补写。

1. 拉取 main：本仓、客户端仓 `mmorpg-client`、Java 仓 `xuanming-server-mmo`（按 AGENTS §9 的 GitHub 地址找，不写死盘符）。再跑 `git submodule update --init --recursive`：要不报错，`git submodule status` 每行开头是空格。
   - 11-27 核对时，子模块 `third_party/ue5navmesh` 在旧电脑上检出的是 `4c6b821`（11-11「保存导航网格构建兼容性改动」）。它不在任何分支上，也没推到子模块仓；本仓 HEAD 记的还是 `aacf2f82`。这一步报错，多半是本仓提交了新指针，子模块那个提交却没推上去。
2. `git log --oneline -1 -- docs/handoff/README.md` 有输出（本单已入库）。
3. 各单自己的开工核对也要过：第 4 轮单 §0a 第 0 步、合服副本单 §2 第 1 步。
4. Claude Code 的本机记忆不在仓库里，不会跟过来。各线要记住的定案多数已抄进交接单与设计稿，以它们为准。
5. 不随仓库走、也不用找的：旧电脑的 `build/verification` 运行证据、客户端 `Logs/`、Docker 卷里的节点日志、会话草稿目录。PROGRESS 写了「已验证」的以 PROGRESS 为准，要原件只能重跑。

## 2. 规矩（AGENTS.md 照常全文读，这里只列最容易做错的）

- 只在 main 上改，不开分支、不开 worktree；提交与推送由负责人做。
- 代码与文档由 Claude Code 写。Gemini 只跑生成、编译、测试、冒烟；跑出要改代码的地方，保留首错原文交回 Claude Code。Codex 只作图。
- 写代码和做分析按 AGENTS §10.3 用最高档模型。不是最高档时先问负责人一句，负责人明说用当前模型就照做（旧电脑上 11-26、11-27 负责人放行过 Opus 5.5）。
- Java 独立仓只在 `PARITY.md` 登记待做，不写 Java 代码。
- 每修一处，在所属设计稿写五项：问题、方案、为什么这样选、被否决的做法、代价与边界。负责人没想到的好方案一定写进去。PROGRESS 只追加；交接单的状态行同步改。
- 全部 Claude 模型的周额度过 80% 时，只做风险最高的，然后停下报告。

## 3. 要写代码的（Claude Code，按这个顺序）

| 顺序 | 交接单 | 从哪开始 | 要做的 |
|---|---|---|---|
| 1 | `data-overwrite-audit-r2-round4-handoff-20261126.md` | §0a | 复核批 V/W/X、§7.4 五条、§6.1 九条；r2-craft#1 批 C、批 B（批 C 前提②先问负责人）；guild 人工终结的条件写 SQL；各批顺带发现；文档。第 5 轮审计放最后 |
| 2 | `review-suggestions-remaining-handoff-20261126.md` | §8 | 选角页回选上限的用例（客户端仓，只加用例）。不依赖第 1 行，在第 5 轮审计之前做完即可 |
| 3 | `admin-identity-dual-control-batches4-7-handoff-20261126.md` | §5「批 7 剩余」 | 第 1 项（后台上 K8s）与第 3 项 11-27 已写完、未运行，交 Gemini，命令在 PROGRESS 标题带「双人复核·批 7 剩余」的条目。第 2 项（本机后台到 trade 的转发）原定前提不成立，等负责人定做法（见第 5 节），定了再写 |
| 4 | `merged-zone-etcd-copy-handoff-20261127.md` | §2 | 代码已写完。先交 Gemini 跑 §3；全绿后做 §4（第二批的独立复核等） |
| 5 | `login-queue-verification-handoff-20261126.md` | §4 | 开闸：Claude Code 改常量与契约用例。前提是第 2、5、7 步全绿并写进 PROGRESS，且负责人确认 |

## 4. 只交 Gemini 跑的（代码已写完）

先统一生成一遍，很多线生成之前编不过：按 `data/AGENTS.md` 导 `Tip.xlsx`；仓库根跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`；客户端仓依次跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`。之后按各单自己的顺序跑：

- `account-deletion-handoff-20261120.md`
- `battle-script-review-retention-handoff-20261126.md`
- `config-table-hot-reload-handoff-20261126.md`
- `data-consistency-review-handoff-20261126.md`
- `gate-online-identity-simplify-20261105/CODEX.md`
- `minigame-websocket-handoff-20261120.md`
- `passport-hardening-handoff-20261126.md`
- `real-name-anti-addiction-handoff-20261126.md`
- `redis-cluster-all-domains-handoff-20261126.md`
- `scene-gate-instance-session-handoff-20261126.md`
- `third-party-login-and-anti-addiction-handoff-20261126.md`
- `login-queue-verification-handoff-20261126.md` 第 2 节
- `review-suggestions-remaining-handoff-20261126.md` 第 6 节，以及第 1~5 节各条 PROGRESS 里的命令
- `payment-integration-handoff-20261120.md`「还剩」：Go `-race` 要在 Linux 或 Docker 里跑；整链路冒烟；kind 冒烟要先经负责人同意装工具
- `data-overwrite-audit-r2-handoff-20261126.md`：r2 全线的总验证。等第 3 节第 1、2 行做完、审计文档 §1「仍开着」清空，再整体跑一遍最省事
- PROGRESS 文末 11-26、11-27 标「未编译」「待 Gemini」的各条（data_service 启动期故障隔离三条、gate 准入 P2-4、官方通行证加固复查等），命令在各条里

## 5. 等负责人定的

- r2-craft#1 批 C 前提②：K8s 各档位要不要打开 trade 的 `AssetOp.Enabled`（第 4 轮单 §0a 第 2.1 步）。
- 登录排队什么时候开闸（第 3 节第 5 行）。
- 本机后台到 trade 的转发怎么做：本机 Docker 调试栈里没有 trade 容器，原定做法的前提不成立（admin-identity 单 §5、设计稿 `admin-identity-and-dual-control.md` §11.4 末节）。
- 支付：回调用的公网 HTTPS 域名与证书、各渠道商户号与私钥（`server-code-completion-20261120.md` §3.1）；Java 版什么时候移植（`payment-integration-handoff-20261120.md`）。

## 6. 不是代码、或只留档的

- `spectate-tiered-fanout-followup-20261121.md`：公网上线前在目标物理机或云主机混跑，复测 Gate 扇出 p99。
- `team-follow-afk-hosting-handoff-20261120.md`：实网多节点冒烟；Codex 补美术。
- `server-gap-audit-20261101.md`：缺口总账，不是待办单。11-26 晚核对时，写着交 Gemini 的代码都已写完；各条「仍剩」多是真集群上的核对（如第 1 条 Agones 的三条假设）与 Java。本表没有逐条重核。
- `server-code-completion-20261120.md`：11-20 的完成度核对，留档；§3.1 的支付域名已列进第 5 节。
- `battle-report-replay-client.md`：11-21 已闭环验证，留档。
- `data-overwrite-audit-r2-round4-20261126/`：第 4 轮查找的原始结果（三个 JSON），第 4 轮单引用。
