# 压测纪律与复盘流程

> 对应根索引 [AGENTS.md §6](../../AGENTS.md#6-压测纪律最重要)。**不要在没有对比表的情况下声明“性能提升”。**

## 1. 复盘只读脚本输出

**压测复盘只读 `stress_summarize.ps1` 输出，不要手 grep raw prom dump。**

- 命令：`pwsh tools/scripts/stress_summarize.ps1 -RunDir robot/logs/stress-<name>-<ts>`
- 输出多段二维表：robot 每分钟 stats + EnterGame 端到端 + 建角阶段 + login 出站 RPC + SceneManager EnterScene 子阶段 + data_service 归属读 + 存盘接口（P1~P4）+ Redis 负载（R1 / R2）+ Kafka lag。
- **Round 16+ 推荐用 `tools/scripts/stress_snap.ps1` 后台批量拉 snapshot**（并行 scrape 端口）：
  ```powershell
  pwsh tools/scripts/stress_snap.ps1 -RunDir robot/logs/stress-<name>-<ts> -StartTime '<yyyy-MM-dd HH:mm:ss>' -Stages 2,5,10,15,18
  ```
  - 端口分工：`:9101` login / `:9150` scene_manager / `:9160` db / `:9260` data_service（2026-10-23 起）。
  - 文件命名：`t<N>m_login.txt` / `t<N>m_sm.txt` / `t<N>m_db.txt` / `t<N>m_ds.txt`，summarize 脚本自动按后缀分流。
  - 同一时刻对 `-RedisTargets`（缺省 `@{ core = 'docker:redis' }`）抓 `redis-cli INFO` 快照 `t<N>m_redis_<name>.txt`，汇总脚本相邻两份求差出 Redis 的 ops/s、CPU、热点命令；Redis 求差要基线，`-Stages` 建议带上 `0`。
  - Redis Cluster 的 target：K8s 上玩家域写 `k8s-cluster:<namespace>/redis-player`，控制集群写 `k8s-cluster:<namespace>/redis-control`；本机开发集群写 `docker-cluster:redis-player-cluster-0:7100`（本机控制域句柄缺省回落单实例 `redis`，指到开发集群时与玩家域共用这一套，指法见 `tools/scripts/README.md`「compose.redis-cluster.yml」）。集群 target 逐主节点抓，R1 / R2 逐主节点出行，再合成一行 `<name>-all`。
- 旧式单端口手拉（legacy 兼容）：`curl -s http://127.0.0.1:9101/metrics > $RunDir/prom-snapshots/t<n>m.txt`。
- 历史复盘文档（`stress-1zone-*.md`）直接复用脚本输出的表格，不要再贴 raw count/sum 数字。

## 2. 压测前后强制流程（任何一步漏了都重来）

1. **跑测前 —— 固化基线**：把上一次压测的 `stress_summarize.ps1` 输出存为 `prev-summary.txt`（放在 `docs/stress/stress-<round>-<date>.md` 同目录或 commit 到对比 PR 描述里），作为 Round N 的对比基线。`prev-summary.txt` 不存就不许开下一轮。
2. **跑测前 —— 彻底清场**：清空所有可能污染数据的日志与缓存：
   - `robot/logs/stress-*` 旧目录（留最近 1 个备查，其余删掉）；
   - `bin/log/*` cpp gate/scene 日志；
   - 各 go service stderr/stdout（`go-svc-stop` + 删 `tools/scripts/.run/` 的 pid/log）；
   - 清掉残留 lock / session key：
     - 本机：`pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`（单实例加本机开发集群 `redis-player-cluster-*` 的每个主节点；本机控制句柄指到开发集群时也在这一套里）；
     - K8s 压测环境：`-Command k8s-redis-wipe -ConfirmWipeAllRedis`（只许 `mmorpg-stress-*` 的 infra namespace，逐主节点清其中实际存在的 `redis`、`redis-match-cluster`、`redis-player`、`redis-control`）。Redis Cluster 的 `FLUSHALL` 只清本节点，不要只对一个节点执行；
   - kafka topic offset reset（`pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`）+ broker 数据目录可选清，生产/历史不要清；
   - 新建 prom snapshot 目录：`robot/logs/stress-<name>-<ts>/prom-snapshots/`。
3. **压测中**：至少在 ramp 完成 / 稳态中段 / 稳态末三个时刻拉 snapshot 进 `prom-snapshots/`。
4. **跑测后**：跑 `stress_summarize.ps1` 出 Round N 表，与 `prev-summary.txt` 二维对比写进新复盘文档，贴架构决策行并更新文档索引。
5. **压测期间禁止上传任何日志**。
6. **每次登录压测前，把所有 Redis / MySQL / etcd 数据全部删除再开新压测**。
