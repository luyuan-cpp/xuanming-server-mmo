# 登录 Redis 清退后的连接总账：交 Claude Code 独立审核

日期：2026-11-06（America/New_York）。用户要求：先修复，再交给 Claude Code，按 Claude 自己的思路审核。

## 给审核者

请独立判断本次修改是否正确、完整，审核入口、方法和深度由你决定。下面的修复解释与测试结果是证据，不是预设结论；可以质疑计算口径，并沿相关代码寻找遗漏。请报告有证据的问题、影响和位置；没有发现问题也请说明实际审核边界。遵循根 `AGENTS.md`：需要补充编译或测试时，给出由 Codex 执行的命令。

工作目录：`D:\luyuan\xuanming-server-mmo-cpp`。起始核对 HEAD 为 `1194c4d46`，期间其他会话继续提交。工作区有多个会话并行修改，甚至同一个脚本也有其他修改；下面明确区分本次改动，不要把整份 `git diff` 都归到这次任务。本会话未执行提交或推送。

## 已有事实与本次修复

六个在线服务 chat、friend、guild、match、mail、trade 的登录 Redis 配置清退已经由身份会话完成；本次核对时 `Get-RedisPoolMirror`、`Get-RedisPlayerPoolMirror`、ConfigMap 与登录域总账已同步，没有再次删除这些配置。背景见 [身份清退执行清单](gate-online-identity-single-source-20261105/CODEX.md) 与 [身份设计](../design/gate-online-identity.md)。

发现的残留是 match 的控制域默认池：`go/match/internal/svc/servicecontext.go` 的 `NewRedisHandles` 只返回 MatchRedis；K8s 为它配置专用 `redis-match-cluster`，不会同时创建旧 `sharedRds`。但核心 Redis 总账仍记 `DefaultPools=1; GoMaxProcs=2`，每个 match 副本多计 20 条连接，滚动峰值也相应多计。

本次改动：

- `tools/scripts/k8s_deploy.ps1`：仅 `Get-RedisConnectionLedgerInputs` 中 match 行的 `DefaultPools`、`GoMaxProcs` 改为 0，并修正该行与零连接过滤的注释。未拆玩家域时仍计 PlayerRedis；拆到玩家域集群后，既有过滤会移除 match 的零连接控制域行。
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`：仅 match 的默认池期望改为 0。
- `tools/scripts/tests/redis_player_cluster_contract.tests.ps1`：原 mail 的迁移断言扩为 mail、match，核对未拆时仅有玩家域池、拆后无控制域行，并保持 chat 不连 Redis 的断言。
- `tools/scripts/tests/k8s_redis_split_contract.tests.ps1`：同步控制域实例接入后过时的两条断言。缺省 infra-up 只排除共享模板的整行说明注释，实际输出仍禁止 queue 实例；redis-wipe 预览必须同时列出 `redis-ctl-g<N>`，原有准入与不执行操作的断言保留。

同期出现的发号器预检、MySQL 告警断言等差异属于其他会话，不是本次修复。

## 验证证据

日志目录：`C:\Users\luyua\AppData\Local\Temp\redis-ledger-review-20261106`。

- `match-ledger-red.log`：先改回归断言，生产脚本未修时 2 项均失败，实际值分别为默认池 1 和 `1/2/0/0`。
- `match-ledger-green.log`：修复后同两项 2/2 PASS，退出 0。
- `match-ledger-regression.ps1`：通过 AST 提取两个仓库原始 Test-Case 与正式生产函数，保留可复现的定向入口；没有复制另一套断言。
- `redis_maxclients_contract.log`：完整 7/7 PASS，退出 0。
- `redis-player-cluster-full.log`：完整 34/34 PASS，退出 0，包含普通/集群形态下六服务不生成登录 Redis 配置的检查。
- `k8s_redis_split_contract.log`：首次完整执行 41 PASS、2 FAIL。失败仅为上述过时断言：缺省 infra-up 的两个 `redis-queue-g` 命中都是模板整行注释，非注释命中为 0；实际清库选择器与预览均已包含控制域实例。
- `k8s_redis_split_contract.after.log`：修改后完整 43/43 PASS，退出 0，包含高可用形态、连接/代理总账、形态切换闸、状态检查和清库替身。
- 四个修改过的 PowerShell 文件解析无错误（`powershell-parse.log`）；针对本次文件的 `git diff --check` 无空白错误。

完整 `k8s_deploy_contract.tests.ps1` 的基线运行在修生产代码前发现三项发布轨道失败，已停止，不能当作整套通过。日志 `k8s_deploy_contract.before.log`：

1. canary zone 的 Deployment 用例调用未定义的 `Select-CppWorkloadBlock`。
2. canary zone 的 Agones Fleet / external gate 用例同样调用未定义函数。
3. 全局 canary 用例以 `-canary\b` 搜索稳定版整个块，命中后失败。

本轮没有修改以上发布轨道用例，没有修改服务业务代码，没有运行 Go/C++ 构建、真实 Kubernetes 部署或故障切换。排队 HA 的契约测试结果与真实集群验收应分别表述。

Java：本次修复属于本仓 PowerShell 部署总账，Java 版没有同一实现，代码修复不适用；六服务身份清退与 Redis 拆分的 Java 状态仍以 `D:\luyuan\xuanming-server-mmo\PARITY.md` 为准（0.1.0-SNAPSHOT 的既有待办），本次未改 Java。

## 交接状态

已找到本机 Claude Code CLI，但 `auth status --text` 返回未登录。Claude 桌面 Code 页面已登录并选中本仓库，但显示 `Weekly limit reached`。本轮没有成功提交审核请求，尚无 Claude 审核结论；未变更账号、套餐或额度设置。

恢复可用后，交给 Claude Code 的请求可直接使用：

> 请在 `D:\luyuan\xuanming-server-mmo-cpp` 独立审核本次 Redis 连接总账修复，先读 `AGENTS.md` 和 `docs/handoff/redis-login-ledger-review-20261106.md`。审核方法和判断由你决定，不预设结论，可以质疑修复依据并检查相关遗漏。请给出发现、影响、证据与审核边界；需要额外运行验证时，按仓库分工给 Codex 具体命令。当前同文件存在其他会话改动，请核对归属。
