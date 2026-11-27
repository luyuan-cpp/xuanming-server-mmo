# 缺口清单窗口的剩余验证（2026-11-27，做完删除本单）

> 来源：旧电脑上的「服务器代码缺口清单」窗口（Claude Code）。旧电脑已回收。
> 新电脑上负责人说「继续做完 handoff」时，先按 `new-computer-start-here-handoff-20261127.md` §1 做开工核对，再做本单。
> 这个窗口开出的两个会话各有自己的交接单，本单不重复：「交接单里的代码改动清零与方案入档」见 `claude-session-leftovers-handoff-20261127.md`；「后台管理员身份与高风险操作双人复核」见 `admin-identity-dual-control-batches4-7-handoff-20261126.md` §5。

## 1. 现状（11-27 核对过）

- **没有要写的代码。** 本窗口负责的缺口清单第 8、10、13、17、18、27、28、29、31 条都已落码，在 main，没有编译、也没有运行。各条做了什么、后来又被哪个会话接着改，见 `server-gap-audit-20261101.md` 对应条目。
- **方案都已写进设计稿**：
  - Java 网关令牌桶 Lua 与单键脚本：`open-server-rate-limit-design.md`
  - etcd 节点镜像 `NodeInfoMirror`、login 排空标记缓存 `DrainMarkCache`：`gate-load-balancing-design.md`
  - 排行榜 `order_key` 与 `entries`：`leaderboard-system.md`
  - mail 活动名单导入：`mail-system.md`
  - client_rpc_router 选点索引：`client-rpc-router.md`
  - 连接池指标 `sqlpoolstats`
- **普通单测不用单独跑**，总入口单 §4 第 2 步的全量编译与单测已经盖住：
  - Java 网关的单测在 `mvn -B -f java/pom.xml -pl gateway_node -am test` 里；
  - login 的 `TestDrainMarkCache_*`、client_rpc_router 的 `TestPickIndexFollowsEveryMirrorChange` 在全量 `go test` 里。
- **剩下的是要真 Redis、真 TiDB 的用例，和一轮压测基线。** 这些用例不设环境变量会被跳过，全量单测显示通过也不算数。见 §2。

## 2. 只交 Gemini 跑的

跑红了保留首错原文交回 Claude Code，不要自己改代码。生成已在总入口单 §4 第 1 步统一做过。

### 2.1 Java 网关限流：真 Redis 单机两版加集群

命令和新用例名在 PROGRESS 2026-11-19「网关前置限流的排队提示加下限与随机量…」一条，按其「验证」第 1~4 步跑。要点：

1. 起两个一次性容器，`redis:7.2` 与本地 compose 用的 8.x，端口错开。不要指向共享的开发 Redis。
2. 在 `java/` 下，对两个容器分别设 `GATEWAY_RATE_LIMIT_REDIS_URL=redis://127.0.0.1:<端口>`，各跑一次：
   `mvn -B -pl gateway_node -am test "-Dtest=RedisRateLimitStoreIntegrationTest" -Dsurefire.failIfNoSpecifiedTests=false`
3. 集群：先 `docker compose -f deploy/compose.redis-cluster.yml up -d`，再设 `GATEWAY_RATE_LIMIT_REDIS_CLUSTER_NODES=127.0.0.1:7100,127.0.0.1:7101,127.0.0.1:7102`，跑 `-Dtest=RedisRateLimitStoreClusterIntegrationTest`。
4. 带着第 2、3 步的环境变量，再跑一次全量 `mvn -B -pl gateway_node -am test`。

**通过标准**：Failures 与 Errors 都是 0；这两个类在输出里的用例数大于 0，被跳过说明环境变量没设上。两版 Redis 的实际版本号写进 PROGRESS。

### 2.2 mail 活动名单导入、rank 走索引读写：真 TiDB

1. 起一个一次性 TiDB，版本按 `docs/design/tidb-phase1-and-launch-readiness.md` 钉的那个。端点必须写成数字回环地址，如 `127.0.0.1:<端口>`；用例自己建随机库。
2. 设 `MAIL_RANK_TEST_TIDB=127.0.0.1:<端口>` 和 `MAIL_RANK_REQUIRE_TESTS=1`。设了后者，没配端点时用例直接失败，不会悄悄跳过。
3. 跑：
   - `cd go/mail && go test -count=1 ./internal/data/ -v`，其中有 `campaign_import_integration_test.go`；
   - `cd go/rank && go test -count=1 ./internal/data/ -v`，其中有 `read_order_integration_test.go`、`rank_limit_integration_test.go`、`shard_integration_test.go`。

**通过标准**：全部 PASS，输出里没有 SKIP。

### 2.3 压测工具的现状基线（缺口清单第 10 条，不阻塞别的事）

`stress_snap.ps1` 新加的 :9170 与 :9190 抓取、`stress_summarize.ps1` 新加的 G1/G2/G3/PL1 段、robot 的 `match-stress` 档，至今一次都没跑过。有压测窗口时：

1. 按 `docs/rules/stress-testing.md` 的前后流程跑一轮，robot 配置用 `robot/etc/robot.match-stress-100.yaml`。
2. 复盘只读 `stress_summarize.ps1` 的输出，确认 G1/G2/G3/PL1 段和 L1 末列 `bgz_fb` 都有数。
3. 结论写进 `docs/stress/stress-<轮次>-*.md`。

这一轮只建基线，不拿来声称性能提升。

## 3. 怎么收尾

1. §2.1、§2.2 全绿并写进 PROGRESS，§2.3 跑完或负责人说不跑，然后删掉本单。
2. 删掉 `docs/README.md` 里本单那一行：先跑 `python tools/scripts/gen_docs_index.py --check`，再生成，diff 只许删本单那一行。
3. 把总入口单 §4 里指向本单的那一条划掉。
4. PROGRESS 追加一条，写明删了本单。原文用 `git show <删除前的提交>:docs/handoff/server-gap-window-leftovers-handoff-20261127.md` 取。
