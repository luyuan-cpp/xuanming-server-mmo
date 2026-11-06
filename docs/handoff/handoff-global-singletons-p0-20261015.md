# 交接:全局单点 P0 五项的落码现状与剩余工作(2026-10-15)

> **交出方**:Claude(会话 2026-10-13 ~ 10-15)。**接手方**:另一个账号的 Claude。
> **一句话**:P0 五项能落的代码全部落了,并且已随负责人与其他会话的快照提交进入本地 main(HEAD `f0dadf5b0` 时工作区里已没有本任务的未提交改动)。**全部未编译、未测试**;部署侧四批(P0-A / P0-B 第二批、P0-C、P0-E)**还没有经过审查**。接手的第一件事是审查这四批并修复,然后交验证。
> **2026-10-16 更新(接手方)**:§3 第 1、3、6 条已做完 —— 部署侧四批审查并修复、PARITY 登记、P0-D 核对。结果、待负责人拍板的事与交验证命令见 **§6**;§1 表与 §3 以 §6 为准。
> **2026-11-05 更新**:负责人要求「按最标准的方法做」,§2 六条取值与 §6.2 五项已定案,C++ Redis 口令改走 Secret、重启闸改为服务端预演比对、etcd 换官方镜像钉摘要(P0-C 阶段 2 收尾),见 **§8**;§2 与 §6.2 以 §8 为准。
> **编译与测试**:负责人说编译他自己做;按 AGENTS.md §10.1,Claude 不运行构建、测试、部署脚本,只给命令。评审只看语义 / 运行期,不要再为「能不能编过」单独派代理。
> **材料**:
> - 来源交接:[handoff-global-singletons-10m-ccu-20261013.md](handoff-global-singletons-10m-ccu-20261013.md) §3(P0 清单)、§7(顺序);
> - 调查记录与修复设计:[global-singletons-p0-findings-20261014.md](global-singletons-p0-findings-20261014.md)(各项「原报告:修复设计」,以及 P0-A / P0-B / P0-D 开头的「落码说明 / 去向」);
> - Codex 任务单:[global-singletons-p0-codex-tasksheet-20261014.md](global-singletons-p0-codex-tasksheet-20261014.md)(§0~§6 实测、§7 C++ 修复的验证);
> - 部署侧四批的实现报告原文:[附录](handoff-global-singletons-p0-20261015-appendix.md);
> - 进度:`docs/PROGRESS.md` 2026-10-14、2026-10-15 两条「全局单点」条目。

---

## 1. 现状总表

| 项 | 内容 | 落码 | 审查 | 验证 |
|---|---|---|---|---|
| P0-A 第一批(C++) | `RedisManager` 私有握手位 + 占位对象,AUTH 回 +OK 才移交,失败 / 超时 fail-closed 并带 `metric=zone_redis_auth_failed`;`ZONE_REDIS_PASSWORD` 环境变量覆盖;`zoneredis.db != 0` 拒启;bin/etc 占位口令清空 | 已落 | 两轮对抗审查 + 修复 | 未编译未测试 |
| P0-B 第一批(C++) | `KafkaManager::Init` 无条件登记 broker;Brokers 为空拒启;空 group.id 日志改 ERROR | 已落 | 双视角审查 + 修复 | 未编译未测试 |
| P0-A 第二批(部署) | `New-NodeConfigMapYaml` 把 `$script:RedisPassword` 写进 C++ node-config(方案 A:明文 + 只哈希口令的指纹注解 `mmorpg.io/zone-redis-auth-hash`);生成期拒绝非 dev 档空口令与含 `"` `\` 控制字符的口令;battle 渲染挪到 `Apply-Infra` 开头、`Apply-BattlePool` 改为必须传 `-Workload` | 已落 | **未审查** | 未运行 |
| P0-B 第二批(部署) | 模板 `Kafka.GroupID` 改为从 bin/etc 镜像(空串);同批写 `DisableLegacyPerNodeTopic: true`(已全仓核实没有生产者再写旧的一节点一 topic) | 已落 | **未审查** | 未运行 |
| P0-C(部署) | 新增 `deploy/k8s/manifests/infra/etcd-maintenance.yaml`(CronJob 每 5 分钟压缩到 current − 1,000,000);接入 infra-up;compose 的 etcd 加自动压缩与 3GiB 配额 env(只对新建容器生效);runbook;静态契约测试。**阶段 2(改 etcd.yaml 的 env / 钉镜像)没做** | 1a + 1b 已落 | **未审查** | 未运行 |
| P0-D | C++ etcd 双向流不重建 | 不在旧 CQ 模板上修,已并入 [grpc-muduo-transport.md](../design/grpc-muduo-transport.md) §10.2 第 2 批的验收条件 | — | 见 §3 第 6 条 |
| P0-E(部署) | `redis.yaml` 固定 `MAXCLIENTS=40000` 并启动时打印 fd 限值;compose 同值;Redis 连接总账(超 80% 只告警)、部署后核对;**重启闸** `-ConfirmRedisCoreRestart`(Pod 模板摘要写在 sts 自己的注解上);dev_tools / k8s_image 透传。第 3 批(告警、go-redis 显式 PoolSize、C++ 断连日志带原因)没做 | 1、2 批已落 | **未审查** | 未运行 |
| L-04、WAITAOF 的 go/db 部分、db 消费组按 zone | 不在本线 | 归 go/db 存盘管线会话(已在对话里确认分工) | — | — |

文件清单:C++ 部分见 `docs/PROGRESS.md` 2026-10-15「全局单点 P0-A / P0-B 落码」条目的「文件」一行;部署侧四批见[附录](handoff-global-singletons-p0-20261015-appendix.md)各段的「改动文件」。这些文件都已在 HEAD 里;要单独看本任务的改动,用 `git log -p -S <关键字>` 按函数名找(例如 `Get-CppZoneRedisPassword`、`etcd-compact`、`ConfirmRedisCoreRestart`、`DisableLegacyPerNodeTopic`),不要以为某个提交只含本任务的东西(它们是多会话快照)。

---

## 2. 负责人还没确认、由交出方按推荐取值落码的决定

负责人说的是「全部做完给我」,以下取值是按调查结论的推荐落的,**交付时要请负责人逐条过目**,不同意就改:

1. P0-A:Redis 认证失败或超时时 fail-closed(zone Redis 对业务不可用,每 3 秒重试并打 ERROR),不放行未认证连接。理由:`BattleRoomManager::ProbeAndRetryOne` 会把非预期应答当成「已销账」删外发箱条目。
2. P0-A:`zoneredis.db != 0` 启动即失败;P0-B:`Kafka.Brokers` 为空启动即失败。
3. P0-A 第二批选方案 A(口令明文进 node ConfigMap + 指纹注解,与 Go 服务、gateway 现行做法一致),没有选交接原文的 Secret。改口令会滚动全部 C++ Pod(gate 踢人、battle 作废在打的局)。
4. P0-B 第二批同批打开 `DisableLegacyPerNodeTopic: true`。上线前要在目标环境确认线上 login 等 Go 服务已写新 topic、旧 topic 无积压;否则把模板那一行改回 `false`(改回会让旧路径换新消费组、从 earliest 重读旧 topic)。
5. P0-C:`KEEP_REVISIONS = 1000000`,配额 3GiB;CronJob 镜像暂用 `bitnamilegacy/etcd:latest`,钉 tag@摘要要等实测。
6. P0-E:`maxclients = 40000`;路径甲(没有需要保护的在线环境,第 1 批直接落);重启闸语义比 G4a 设计稿更宽(任何 Pod 模板变化都要确认),G4a 文档已注明。

---

## 3. 剩余工作(按顺序)

1. **审查部署侧四批**(本任务的空档,必须先做)。
   - 范围:[附录](handoff-global-singletons-p0-20261015-appendix.md)四段列出的改动;按函数名在 HEAD 里定位。
   - 视角只要语义 / 运行期:here-string 插值与转义渲染出的实际内容、`-f` 与 `+` 优先级、`$null` / 空数组分支、`$script:` 变量在 zone-up / infra-up / all-up / DryRun 各入口下是否已赋值、DryRun 不调 kubectl、报错与日志不回显 secret、函数签名改动后所有调用方(尤其 P0-A 改了 `Apply-BattlePool` 的签名,P0-E 改了 `Get-ManifestDeploymentReplicas` 加可选参数)、YAML 缩进与多行 shell 薄壳、契约测试修复前红修复后绿、dev 档渲染是否与改动前逐字相同(P0-A 要求口令为空时不写注解)。
   - 硬约束复核:etcd.yaml 的 StatefulSet 字段一个都不能动(只允许注释);redis.yaml 的就绪探针与 timeout 不改。
   - 发现问题直接修;一次只派少量代理(本账号上一轮的 4 个审查代理因额度与 403 全部失败)。
2. **交验证**(负责人自己跑或转 Codex):
   - C++:任务单 §7(库与节点 MSBuild `/m:1` 串行重编,`GameConfigEnvOverride`、`RedisManagerAuth` 10 条重复 20 轮、`KafkaManagerInit` 4 条,可选的修复前红态与节点黑盒 6 组)。
   - 部署:`pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`,按用例名判(新用例名见附录各段);先在改动前的提交上跑一次存基线,区分其他会话的红。
   - P0-E 的 dev_tools DryRun 透传检查与 P0-C 的 kind 手动触发 Job,命令见附录。
   - 结果写回:`docs/PROGRESS.md` 追加「全局单点 P0 验证结果」;任务单 §7 末尾补「验证结论」。
3. **登记**(交出方没来得及做):
   - `docs/PROGRESS.md` 追加一条「全局单点 P0 部署侧四批落码」(内容可直接取本文 §1 / §2 / §4,写明 Java 状态);
   - Java 仓库 `D:\luyuan\xuanming-server-mmo\PARITY.md`(只追加,该文件常有他人在途改动):P0-B 那一行的备注补「node ConfigMap 的 GroupID 改为镜像 bin/etc、K8s 关闭旧路径;Java 没有 K8s 模板,不适用」;新增 P0-C 一行「不适用(Java 不用 etcd)」;新增 P0-E 一行「Java 待做(Java compose 的 redis 同样没设 maxclients,Redisson 未设池参数)」。P0-A、P0-B 两行在 10-15 已登记。
4. **上线前必须告诉负责人的运维后果**(合入后第一次部署时出现):
   - 第一次 `infra-up` / 不带 `-SkipInfra` 的 `all-up` / `k8s-release-all` 会被重启闸拒绝,必须在维护窗口带 `-ConfirmRedisCoreRestart`;窗口前可先 `CONFIG SET maxclients 40000` 在线调大(不持久)。infra-up 还会连带 apply `mysql.yaml`,与 L-04 的部署约束(godb-pipeline-handoff §2.4)合并到同一窗口。
   - 注入了 Redis 口令的环境,C++ Pod 会因指纹注解变化自动滚动;先发带 AUTH 的 C++ 镜像,再改配置。
   - compose 的 etcd / redis 新参数只对新建容器生效,要 `--force-recreate`,应与整栈重启一起做。
5. **之后的批次**:
   - P0-C 阶段 2(etcd.yaml 加 env、配额、钉镜像、抓取注解):前置是 etcd 流重建验证通过(见第 6 条)。
   - P0-E 第 3 批:告警(先定是否上 redis_exporter,与 G4a §13 第 6 条一起)、go-redis 客户端显式 `PoolSize`(login、db、player_locator、trade、guild、data_service)、C++ 断连日志带原因。
6. **核对 P0-D**:HEAD `f0dadf5b0`「移除旧gRPCetcd传输并同步工程配置」说明迁移会话已经把 C++ etcd 换到新传输。对照 grpc-muduo-transport.md §10.2 的验收条件(流断退避重建、重建后 Range 对账删除、DELETE 不依赖 prev_kv、compacted 处理、「无 fence 且零重注册」)确认是否都落了;落了之后,P0-C 阶段 2 的前置才可能满足(还要有运行证据)。
7. **另开的两张任务卡片**(交出方会话里点出,未必有人接):`BattleRoomManager::ProbeAndRetryOne` 把空应答 / ERROR 当已销账;Hiredis 适配层同批事件释放后使用与 Windows POLLHUP 缺 close 回调。

---

## 4. 已知问题与提醒

- 负责人中途的提交 `676c64789`、`9b8b902da` 里 `k8s_deploy.ps1` 是半截状态(`Apply-BattlePool` 已要求 `-Workload`,调用处未传),不要单独检出这两个提交部署;之后的提交已完整。
- P0-E 实现者看到但没碰:`k8s_deploy.ps1` 里 player-inbox 的 ConfigMap 生成器查 `SharedRedis.Host`,而 `go/player_inbox/etc/player_inbox.yaml` 那段叫 `CoreRedis`,可能导致生成期 throw,需要收件箱那条线确认。
- 文档里还没改的过时说法:`deploy/k8s/AGENTS.md` 里「redis 是单副本 Deployment」;`bin/etc/base_deploy_config.yaml` 注释里的 `go/shared/kafkautil/command_topic.go`(实际在 `go/shared/kafkacmd/`)。
- 同一工作区常有多个会话并行写文件,并且负责人会做快照提交:动手前 `git status`,改共享文件(`k8s_deploy.ps1`、契约测试、README、PROGRESS)只用 Edit、先重读;不要 `git add -A`;go/db 会话在改 `k8s_deploy.ps1` 的 param 块、Go ConfigMap 的 db / login 分支、写路径门禁块、Kafka PVC / retention 段与契约测试文件末尾。
- 子代理按提交读文件时会 `git show … > 文件` 在仓库外留副本:提示里写明「直接输出或 git grep,不要重定向」,结束后核对删除。

---

## 5. 给接手账号的开场提示词(可直接复制)

```text
请接手 docs/handoff/handoff-global-singletons-p0-20261015.md 的工作。
1. 先按 AGENTS.md「会话启动门禁」读规定材料,再读这份交接全文和它的附录。
2. 本轮只做交接 §3 的第 1、3 项:审查部署侧四批(P0-A / P0-B 第二批、P0-C、P0-E)并直接修复问题;补 docs/PROGRESS.md 条目与 Java 仓 PARITY.md 登记。
3. 编译、测试我自己跑:你不运行任何构建、测试、部署脚本,只给命令;评审只看语义 / 运行期,不要为「能不能编过」派代理;一次只派少量代理。
4. 同一工作区有其他会话在改:动手前 git status,共享文件只用 Edit、先重读,不 git add -A、不提交,我自己提交。
5. 交接 §2 里按推荐取值落的决定,交付时逐条列给我确认。
```

---

## 6. 2026-10-16 接手方:审查、修复、P0-D 核对与交验证

> 做了 §3 第 1、3、6 条。流程:每批一个只读审查代理 + 一个反驳式复核代理;修复后又派代理复核修复两轮(第二轮发现的
> 「应急探针落库会让其他环境滚动 etcd」与 AST 用例盲点已改)。**全部未运行**:没有跑任何构建、测试、部署脚本(AGENTS.md §10.1);下面的「绿 / 红」都是静态推断,以 §6.3 的实际运行为准。

### 6.1 结果

| 项 | 审查结论 | 处理 |
|---|---|---|
| P0-B 第二批 | 无问题。逐个核实 gate / scene / battle 全部 Kafka 消费者:K8s 上 `GroupID=""` + `DisableLegacyPerNodeTopic: true` 时没有消费者以空 group.id 订阅(新路径一律 assign);本机 bin/etc 路径不受影响;README 的回退说法与代码一致 | 只修了 README「上线影响 1」一句过时描述 |
| P0-A 第二批 | 入口顺序、口令进 YAML 的转义、指纹只哈希口令、dev 档逐字相同、不回显口令、`Apply-BattlePool` 唯一调用方均核过。1 条 low:README 上线步骤漏了 Agones 已 Allocated 的旧实例 —— 它们一直用旧口令 / 空口令,Redis 开口令后 NOAUTH 到退出,不会自愈;battle 空口令时还会把 NOAUTH 当「已销账」删结算外发箱条目 | README「C++ 节点的 zone Redis 口令」上线步骤第 3 步加前置(先等 Fleet 滚完、查旧一代 GameServerSet、主动删旧 Allocated 实例),第 4 步收窄;`Get-CppZoneRedisAuthAnnotations` 注释同步 |
| P0-C | 清单与脚本语义核过(线性一致读、退出码、超时、标签、占位替换、etcd.yaml 字段未动)。runbook:K8s 上 etcd 的 `/health` 探针先查告警,NOSPACE 会让三成员 NotReady 并被 liveness 反复重启(原文写「读与 watch 暂不受影响」);告警在时压缩 Job 必然 Failed,`wait --for=condition=complete` 必然超时;告警在时 §5.2 的「3/3 健康 / READY」门槛过不去,处置会卡死;defrag 超过约 50 秒会被 liveness 杀成员。契约测试:没守 `activeDeadlineSeconds`(Forbid 下防卡死 Job 的唯一兜底),「阶段 2 不得提前」没守钉镜像 / 抓取注解 / 探针 | runbook §2、§4.3、§5.2、§5.3、§7、§8、§9 修订(v1.1);`etcd_maintenance_contract.tests.ps1` 补断言;`etcd.yaml` 只在 liveness 上方加 3 行注释 |
| P0-E | 3 条 low + 第二轮 2 条:① 重启闸只比对脚本写的注解,带外改过的模板字段会被 apply 改回并重启,而放行日志写「does not restart」;② 线上还是 2026-09-19 前的 `deployment/redis` 时被判「全新集群」放行,随后无确认删除(emptyDir 数据清空);③ redis.yaml 过闸、apply、部署后核对 MAXCLIENTS 各读一次盘;④ `--cascade=orphan` 删 sts 后的孤儿 redis-0 同样被判「全新集群」;⑤ 新修复缺回归测试 | ① 改文案与注释,完整修法待拍板(§6.2);② 新增 `legacy-deployment` 判定,要求 `-ConfirmRedisCoreRestart`,读线上两次查询失败都 fail-closed;③ 只读一次,三处共用 `$redisManifestRendered`;④ 记为盲区(函数注释与 README),未实现;⑤ 补 4 条用例(见 §6.3) |
| P0-D | §10.2 第 1~5 条代码已落在 muduo_h2 新传输上,逐条静态核对未发现高置信缺陷;第 6 条(删 / 重启一个成员后无 fence 且零重注册)机制成立(断流检测 ≤40s、重建 ≤6s,远小于 TTL 180s)但**没有任何运行证据**;第 7 条的回退已被 f0dadf5b0 取消:`NODE_ETCD_TRANSPORT=grpc` / 未知值只打 WARN、仍走 muduo_h2,WARN 在 Node 构造前打、只进 stdout。最终版 etcd 代码从未编译;HEAD 因 gRPC 迁移第 3 批做到一半整体编不过 | 不改 C++ 代码(归 gRPC 迁移那条线)。PROGRESS 2026-10-15 第 2 批条目「交验证」第 3、4、6 步的期望日志与现代码不符,已在本日 PROGRESS 条目里给出更正。**P0-C 阶段 2 的前置不满足** |

顺带修的过时说法:`deploy/k8s/AGENTS.md`(redis / kafka 形态、kafka 默认 broker 数、etcd 禁止清单补「改探针」)、`deploy/k8s/manifests/infra/redis-ha.yaml`(文件头写「-RedisReplicas 3 时 apply 本文件」,实际没有任何部署路径接入它;另三处「脚本会处理」改为「尚未实现」)、`bin/etc/base_deploy_config.yaml` / `node_command_topic.h` / `go/shared/kafkacmd/testdata/command_partition_vectors.json` 里 `command_topic.go` 的旧路径(只改注释与 schema 说明串,已确认没有测试断言该串)。

### 6.2 要负责人拍板的事

1. **交接 §2 的 6 条取值仍待确认**(审查没有发现要推翻的理由):fail-closed、`zoneredis.db != 0` / Brokers 为空拒启、方案 A 明文口令 + 指纹、K8s 同批关旧路径、KEEP_REVISIONS 1000000 / 配额 3GiB / 镜像暂用 latest、maxclients 40000 与重启闸的宽语义。
2. **重启闸要不要补成「比对线上真实模板」**(P0-E ①):用 `kubectl apply --dry-run=server -o json` 取出 StatefulSet 的 `spec.template` 与线上比较,不等即要求确认。能消掉「带外改动被静默改回并重启」这个盲区;代价是多一次 API 往返、契约测试要给 dry-run 做替身,且 dry-run 返回形态需实测。不做的话,现状是文档化的盲区。
3. **孤儿 Pod 盲区要不要实现**(P0-E ④):没有 sts 时再查 `pods -l app=redis`,有就要求确认。目前只有 redis-ha 接入时会走 `--cascade=orphan`,而 redis-ha 尚未接入,所以只记了盲区。
4. **P0-D 已无回退**:下一个能编过的构建会让所有环境的 C++ 节点直接用上未验证的 etcd 传输。建议把 PROGRESS 更正后的故障注入验证排在**任何 C++ 节点部署之前**,而不只是 etcd 滚动之前;要不要恢复一个回退开关,归 gRPC 迁移那条线决定。
5. **runbook §7 的应急改探针**要在事发时由负责人批准:只 patch 线上 StatefulSet、不落库(落库会让用同一份 `etcd.yaml` 的其他环境下次 infra-up 时滚动 etcd);被 patch 的集群在阶段 2 之前只走 zone-up / `all-up -SkipInfra`,确需 infra-up 就进全栈停机窗口。

### 6.3 交验证(负责人或 Codex 执行)

工作目录仓库根,PowerShell 7。先清 `MMORPG_*` 环境变量(dev 基线用例假定都没设):

```powershell
Get-ChildItem Env:MMORPG_* | ForEach-Object { Remove-Item "Env:$($_.Name)" }
pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1; "exit=$LASTEXITCODE"
pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1; "exit=$LASTEXITCODE"
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1; "exit=$LASTEXITCODE"
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1; "exit=$LASTEXITCODE"
```

通过标准(按用例名判,`k8s_deploy_contract` 与 `k8s_client_entry_contract` 里有其他会话的在途用例,与本项无关的红按文件归属单独汇报):

- `etcd_maintenance_contract`:total=9 pass=9,退出码 0。
- `redis_maxclients_contract`:total=5 pass=5。
- `k8s_deploy_contract` 里本轮新增或改过的重启闸用例都是 `[PASS]`:
  - `重启闸纯判定:线上不存在 → absent(放行),只剩旧 Deployment → legacy-deployment(要确认),…`
  - `负向:线上只剩旧 Deployment redis(2026-09-19 之前的形态)时重启闸拒绝,…`
  - `重启闸读线上(Get-LiveRedisCoreTemplateDigest):kubectl 失败一律 throw(fail-closed);…`
  - `Apply-Infra 只读一次 redis.yaml:过闸算摘要、循环里 apply、-WaitReady 部署后核对的 MAXCLIENTS 都用同一份内容(AST 契约)`
  - 以及原有的 `负向:重启闸要确认时 throw,…`、`infra-up DryRun:sts/redis 自己的 metadata 带 16 位模板摘要注解…`。
- 附录各段列的原有新用例(P0-A 9 条、P0-B 1 条、P0-C 1 条、P0-E 9 条)仍为 `[PASS]`。

修复前必红(可选,跑完删 worktree;**附录 P0-C V2 的命令写的是 `HEAD`,改动已入库,照原样跑会 9/9 全绿,要改用下面的基线**):

```powershell
# P0-C:基线 526780b4e(= de4960bee^)只拷入新测试文件。期望:前 8 条 FAIL,只有「阶段 2 未经许可不得提前做…」PASS。
$wt = Join-Path $env:TEMP 'p0c-red'; git worktree add --detach $wt 526780b4e
Copy-Item tools/scripts/tests/etcd_maintenance_contract.tests.ps1 (Join-Path $wt 'tools/scripts/tests')
pwsh -NoProfile -File (Join-Path $wt 'tools/scripts/tests/etcd_maintenance_contract.tests.ps1'); "exit=$LASTEXITCODE"
git worktree remove --force $wt
# P0-E 本轮修复:基线 HEAD 的 k8s_deploy.ps1 + 当前测试文件。期望上面 4 条新用例 FAIL(纯判定报参数 -LegacyDeployment 不存在;
# 旧 Deployment 用例不 throw;读线上用例在「无 sts、查 Deployment 失败」一步不 throw;AST 契约找不到 $manifestContent = $redisManifestRendered)。
# 当前测试文件里有其他会话的在途用例,它们在 HEAD 上也可能红,只看这 4 条。
$wt = Join-Path $env:TEMP 'p0e-red'; git worktree add --detach $wt HEAD
Copy-Item tools/scripts/tests/k8s_deploy_contract.tests.ps1 (Join-Path $wt 'tools/scripts/tests')
pwsh -NoProfile -File (Join-Path $wt 'tools/scripts/tests/k8s_deploy_contract.tests.ps1') *> (Join-Path $env:TEMP 'p0e-red.txt'); "exit=$LASTEXITCODE"
git worktree remove --force $wt
```

其余验证照旧:C++ 两项按任务单 §7;P0-E 的 dev_tools DryRun 透传与 P0-C 的 kind 手动触发 Job 按附录;P0-D 的故障注入按 PROGRESS 2026-10-15 第 2 批条目的步骤,**期望日志以 2026-10-16 本条的更正为准**。结果写回 `docs/PROGRESS.md`「全局单点 P0 验证结果」。

### 6.4 仍未做 / 不在本线

- P0-C 阶段 2:前置是 P0-D 有运行证据(§6.1),不满足。
- P0-E 第 3 批:告警(redis_exporter 待拍板)、go-redis 显式 `PoolSize`、C++ 断连日志带原因。
- `docs/design/grpc-muduo-transport.md` §10.2 第 322 行、§13.1、§13.4、§13.8、§13.9 仍写「默认 grpc、可回退、5ms CQ 轮询留在 EtcdService」,与 f0dadf5b0 之后的代码矛盾;该文件由 gRPC 迁移那条线在改(工作区有在途改动),本线没碰,§14.1 与代码一致。本线自己的 `global-singletons-p0-findings-20261014.md` P0-D「去向」那句已加注更正。
- 审查顺带看到、与本批无关的既有问题:`k8s_image.ps1` 不声明也不透传 `-InfraNamespace`(release-all 的重启闸固定查 mmorpg-infra);`Apply-Infra` 对旧的 `deployment/etcd`、`deployment/kafka` 同样无确认删除(2026-09-08 之前的集群必然也有旧 redis Deployment,会先被重启闸拦下);`docs/design/tidb-*` 的 L-06 也写着 redis-ha 由 `-RedisReplicas 3` 接入。
- §3 第 7 条的两张卡片(`ProbeAndRetryOne` 把空应答 / ERROR 当已销账;Hiredis 适配层)仍未见有人接;前者在 Agones 开口令场景下会被放大(§6.1 P0-A 一行)。

## 7. 2026-10-20 Codex 首轮验证状态补记

四批审查与 P0-E 第 3 批已经落地，旧文中的「全部未编译、未测试」不再代表最新事实。Codex 本轮已执行 C++ 串行构建、七个 Go 服务及指定测试、四套部署契约，结果见[任务单 §7.5](global-singletons-p0-codex-tasksheet-20261014.md#75-2026-10-20-codex-验证回执首轮结束未收口)及 PROGRESS 同日「全局单点 P0 首轮验证」条目。

当前**未收口**：C++/Go 有首错，k8s_deploy 契约 12 条失败，且运行期间共享输入发生变化，不能将混合树结果归为当前 HEAD 的验收结论。etcd 静态 9/9、Redis maxclients 7/7、服务端客户端入口契约 111/111 在各自无漂移窗口通过。P0-D 零重注册证据仍缺，P0-C 阶段 2 继续保留门禁。没有部署、重启共享基础设施、改业务代码、自动修复或重试；下一步需稳定输入窗口及首错后的续验决策。

## 8. 2026-11-05 按「最标准的方法」定案并收尾(§2 六条取值、§6.2 待拍板项、P0-C 阶段 2)

负责人 11-05 原话:「这些都按最标准的方法帮我做」。下面逐条写业界标准做法、现状与处理。**全部未运行**:Claude 没有跑任何构建、测试、部署脚本(AGENTS.md §10.1),验证命令在本节末尾。本批没有改 C++ 代码,不需要重编 C++。

### 8.1 §2 六条取值的定案

| # | §2 原取值 | 标准做法 | 处理 |
|---|---|---|---|
| 1 | AUTH 失败或超时 fail-closed,不放行未认证连接 | 认证失败 = 连接不可用,各家官方客户端都这样;放行未认证连接会被 battle 当成「已销账」 | **保持** |
| 2 | `zoneredis.db != 0`、`Kafka.Brokers` 为空启动即失败 | 启动期配置校验 fail-fast;Redis Cluster 只支持 db 0,多库本身不推荐 | **保持** |
| 3 | 方案 A:口令明文进 node ConfigMap + 指纹注解 | K8s 凭据走 Secret + `secretKeyRef`,口令不进 ConfigMap、不进 DryRun 输出;Secret 变了靠 checksum 注解滚动(Helm 做法) | **改**:C++ 改走 Secret(§8.3);Go 服务与 gateway 11-06 同样改走 Secret(§9) |
| 4 | K8s 同批 `DisableLegacyPerNodeTopic: true` | 迁移完成后删掉死代码与开关 | **已由别的会话做完**:A7b(10-30 / 11-01)把旧的一节点一 topic 路径与这个开关整条删了,不再有可调的值 |
| 5 | `KEEP_REVISIONS = 1000000`,配额 3GiB,镜像暂用 `bitnamilegacy/etcd:latest` | 用 etcd 内置的自动压缩;镜像用上游官方的,按版本 + 摘要钉死 | **改**:官方 `quay.io/coreos/etcd:v3.6.16@sha256:a9c7…a965`,CronJob 退役,补告警(§8.4)。保留量与配额不变 |
| 6 | `maxclients = 40000`;重启闸「任何 Pod 模板变化都要确认」 | 上限按测算出的连接需求设;变更前用 `kubectl diff` 同一种服务端预演判断会不会重启 | `maxclients` **保持**;重启闸**改**为服务端预演比对(§8.2) |

### 8.2 §6.2 五项

1. 6 条取值:见 §8.1。
2. 重启闸比对线上真实模板:**做了**。线上 `sts/redis` 在时,把这次要 apply 的那份 StatefulSet 交给 `kubectl apply --dry-run=server -o json`,
   预演结果的 `spec.template` 与线上的比较(键排序后的规范 JSON)。带外 `kubectl set image` / `edit` 过的字段在预演结果里已被三方合并改回,判「要重启」;
   线上没有摘要注解的老集群不再一律要求确认,模板真没变就放行。预演失败一律拒绝(fail-closed)。1 → 3 要先删再建 StatefulSet,不做预演,直接按重启处理。
   文本摘要注解保留,只作标记。
3. 孤儿 Pod 盲区:10-19 已实现(`Get-LiveRedisOrphanPodOrdinals`,判 `orphan-pods` 要确认),本批只核对没改。
4. P0-D 没有回退:不变,归 gRPC 迁移那条线。本批把它从「P0-C 阶段 2 的前置」改成「已有集群滚 etcd 的前置」:全新集群的首次部署不滚 etcd,不受影响;
   有 C++ 节点在跑的集群,换 etcd 镜像那次滚动放进全栈维护窗口,或等 P0-D 的故障注入验证通过 —— 滚动本身就是那项验证的现成场景。
5. runbook §7 的应急改探针:主 etcd 的两个探针 10-30 起已排除 NOSPACE,应急 patch 只剩发号器两套用得上(须负责人批准,改不改仓库由发号器那条线定)。

### 8.3 上一轮提给负责人的两条首次部署影响

- **重启闸与 `-ConfirmRedisCoreRestart`**:项目未上线,首次部署是全新集群,判 absent 放行,不需要开关、不会重启 Redis。已有的 dev 集群只在 Pod 模板真的变了时被拒。
- **C++ Pod 滚动(gate 踢人、battle 作废在打的局)**:首次部署口令从第一天就在 Secret 里,Pod 第一次启动就拿到,没有滚动。以后换口令按 README「C++ 节点的 zone Redis 口令」
  的**双口令轮换**做(Redis ACL 让新旧口令同时有效 → 换 Secret → 旧 Pod 退场 → 删旧口令),旧 Pod 退场前不会 NOAUTH;
  踢人与作废的局只在缺省的 Deployment 形态滚动时发生,external gate(OnDelete + `k8s_gate_drain.ps1` 排空)与 Agones(不打断 Allocated 实例)不打断。
  高可用形态还要先滚代理、处理 redis-exporter 的口令,README 写了。双口令步骤按 Redis 文档写,**没有在本仓库实测**。

口令改走 Secret 的做法:`node-config` / `battle-node-config` 的 `zoneredis.password`、`scene_control_redis.password` 一律写空串;C++ Pod 用 `secretKeyRef`
(不写 optional)从本 namespace 的 Secret `redis-auth`(键 `password`)拿 `ZONE_REDIS_PASSWORD`,scene 另拿 `SCENE_CONTROL_REDIS_PASSWORD`(控制域会话
「控制面按 zone 化与控制域 Redis 分片」11-05 请本线一并处理);`config.cpp` 早已认这两个环境变量。Secret 由新函数 `Sync-RedisAuthSecret` 建:
infra namespace 在 `Apply-Infra` 的 Redis 分支(取代原来会在 DryRun 打印口令的 here-string),每个 zone namespace 在 `Apply-Zone` 开头。
lib 的三个模板经新参数 `New-ClientEntryPodCommon -SecretEnv` 注入。dev 档无口令时生成物与引入之前逐字相同。

### 8.4 P0-C 阶段 2 剩余的钉镜像:做了

- 依据(2026-11-05 查 Docker Hub 与 Quay 的 API):`bitnamilegacy/etcd:latest` 实际是 3.6.1,2025-07-02 之后不再更新;官方 3.6 线最新补丁 v3.6.16(2026-10-22),
  Quay 清单列表摘要 `sha256:a9c7cc12156f4b26ca41f0b57ba5b76c45037ff6c1e47ea3d68cb7908d48a965`;gcr.io 同一 tag 的摘要不同,只用 quay.io。
  3.6.1 → 3.6.16 是补丁升级;不选 3.5(3.6 的数据不能靠换镜像退回);3.7 太新暂不跟。
- 改法:`etcd.yaml`、`etcd-id-secondary.yaml`、`etcd-id-registry.yaml`(发号器那条线的会话已结束,「全球 ID 预发布交接文档」会话确认可以改;两套仍是各自独立的部署与数据目录)、
  两份 compose 同一个引用;显式 `command: ["/usr/local/bin/etcd"]`;去掉 bitnami 专用的 `ALLOW_NONE_AUTHENTICATION`;数据目录挪到 `/var/lib/etcd`(PVC 下 `data/` 子目录不变,
  已有数据照用);发号器两套补抓取注解。
- CronJob `etcd-compact` 退役:官方镜像没有 shell,它的脚本跑不了,内置压缩做同一件事、同一个窗口。删了清单,`infra-up` 不再下发它并删集群里的残留。
  「所有服务中 redis mysql tidb etcd」会话在核对各服务 watch 遇到 compaction 时是否全量重 list,查出缺口会发给本线。
- 告警:`deploy/k8s/etcd-alerts.yaml`,照 etcd 官方 mixin(无主、配额 80% / 95%、4 小时内写满、碎片过半、频繁换主、提案失败、fsync / 提交延迟)。
- runbook `docs/ops/etcd-maintenance-runbook.md` 重写为 v3;官方镜像是 distroless,所有容器内命令改成直接调 `etcdctl`,新增契约测试守住「exec 进 etcd 不开 shell」。

### 8.5 文件

- 部署脚本:`tools/scripts/k8s_deploy.ps1`(`Sync-RedisAuthSecret`、`Get-CppZoneRedisSecretEnvEntries`、`Get-CppZoneRedisEnvLines`、`New-NodeConfigMapYaml` 的两处口令、
  `New-NodeDeploymentYaml` / `New-SceneFleetYaml` / `New-CppNodePodCommon` 的 env;`Get-RedisStatefulSetDocument`、`ConvertTo-CanonicalJsonText`、
  `Get-RedisStatefulSetTemplateElement`、`Compare-RedisPodTemplateJson`、`Invoke-RedisStatefulSetServerDryRun`,`Assert-RedisCoreRestartConfirmed` 与它在 `Apply-Infra` 的调用;
  `$infraManifests` 与 etcd 残留清理、`Show-InfraStatus` 注释)、`tools/scripts/lib/k8s_client_entry.ps1`(`-SecretEnv`)。
- 清单:`deploy/k8s/manifests/infra/{etcd,etcd-id-secondary,etcd-id-registry}.yaml`、删除 `etcd-maintenance.yaml`、`redis.yaml` / `redis-ha.yaml`(只改注释)、
  `deploy/docker-compose.yml`、`deploy/debug/compose.game.yml`、新增 `deploy/k8s/etcd-alerts.yaml`。
- 测试:`tools/scripts/tests/etcd_maintenance_contract.tests.ps1`(重写)、`k8s_deploy_contract.tests.ps1`(3a 节、battle 段、重启闸各用例、etcd-compact 用例)、
  `k8s_client_entry_contract.tests.ps1`(新增 `-SecretEnv` 用例)。
- 文档:`deploy/k8s/README.md`(etcd「维护」、重启闸、C++ 口令三节与清单列表)、`deploy/k8s/AGENTS.md`、`cpp/AGENTS.md`、`docs/ops/etcd-maintenance-runbook.md`、
  `docs/ops/release-checklist.md`、`docs/design/player-data-tidb-scaling-plan.md`(两行现状)、本节、`docs/PROGRESS.md`。

### 8.6 交验证(Codex 执行,仓库根,PowerShell 7)

前置:清掉 `MMORPG_*` 环境变量(dev 基线用例假定都没设):`Get-ChildItem Env:MMORPG_* | ForEach-Object { Remove-Item "Env:$($_.Name)" }`。
逐个跑,每条记下 total / pass / fail 与退出码;与本节无关的红按文件归属单独汇报(同一工作区有其他会话的在途用例)。

1. `pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1` —— 期望 8/8 通过、退出码 0。
2. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1` —— 本节相关用例全部 `[PASS]`:3a 节(`zone Redis 口令…` 各条、`zone Redis 口令契约…`、
   `Sync-RedisAuthSecret…`)、`battle:带口令的 infra-up…`、`重启闸比较(Compare-RedisPodTemplateJson…`、`重启闸预演(Invoke-RedisStatefulSetServerDryRun…`、
   `负向:重启闸按服务端预演判定…`、`重启闸 -StatefulSetRecreated…`、旧 Deployment / 孤儿 Pod / `-RollingHandover` / AST 顺序几条、`infra-up 不再下发 etcd-compact…`。
3. `pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1` —— 新用例 `库:-SecretEnv 渲染成 secretKeyRef…` 通过,其余与改动前相同。
4. 回归:`id_issuer_deploy_contract.tests.ps1`、`redis_maxclients_contract.tests.ps1`、`k8s_redis_ctl_contract.tests.ps1`、`k8s_redis_split_contract.tests.ps1`、
   `redis_player_cluster_contract.tests.ps1`、`redis_exporter_contract.tests.ps1`,不应新增失败。
5. 摘要复核(只读远端清单、不拉镜像;本机没有网络或负责人不同意就跳过并如实报告):`docker buildx imagetools inspect quay.io/coreos/etcd:v3.6.16`,顶层 Digest 应为
   `sha256:a9c7cc12156f4b26ca41f0b57ba5b76c45037ff6c1e47ea3d68cb7908d48a965`。对不上就停下报告,不要自己改。
6. 可选,要拉镜像,先征得负责人同意:kind 上带 `MMORPG_REDIS_PASSWORD` 跑一次全新 `infra-up` 与一个 `zone-up`,核对 runbook §9 的各项(启动日志里的压缩参数、
   `/metrics` 的指标名、`etcdctl` 的 exec 形式、C++ 日志 `Redis AUTH ok`、`kubectl get secret redis-auth -n <zone-ns>` 存在),再改一处 `redis.yaml` 的 Pod 模板
   重跑 `infra-up`,确认重启闸按预演拒绝、加 `-ConfirmRedisCoreRestart` 后放行。

### 8.7 还没做 / 另立

- Go 服务与 Java gateway 的 Redis 口令改走 Secret:11-06 已落码(未编译、未测试),见 §9。
- 双口令轮换、Secret 下发、服务端预演都没有在集群上跑过(§8.6 第 6 步)。
- Java 版(AGENTS.md §12):三项都是本仓库 K8s 部署侧的改动,Java 版没有 K8s 部署模板、不用 etcd,**不适用**,不往 PARITY 加行。

## 9. 2026-11-06 Go 服务与 Java gateway 的共享 Redis 口令改走 Secret(§8.7 第一条)

背景见 §8.1 第 3 条:C++ 11-05 已改走 Secret `redis-auth`,同一个口令在各 Go 服务与 gateway 的 ConfigMap 里仍是明文。本节把它们改成同样的 Secret 注入,
口令离开所有 ConfigMap 与 `-DryRun` 输出。**全部未运行**:Claude 没有跑任何构建、测试、DryRun(AGENTS.md §10.1),只做了 `gofmt -l` 与 PowerShell AST 解析
两项语法检查(均无输出 / 0 错误)。验证命令在 §9.5。

### 9.1 Go 侧做法与理由

- **形状**:ConfigMap 里共享 Redis 段的 `Pass` / `Password` 一律写空串;口令非空时配置末尾多一行顶层 `SharedRedisPasswordEnv: SHARED_REDIS_PASSWORD`。
  新包 `go/shared/redisauth` 只有一个函数 `Apply(envName, lookup, targets...)`;各服务的 `config.Config` 加 `SharedRedisPasswordEnv` 字段和
  `ResolveSharedRedisPassword(lookup)` 方法(方法里列出本服务管哪些段),main 在**迁移分支之后、建任何 Redis 客户端之前**调用一次,传 `os.LookupEnv`。
  填完之后,建客户端与玩家域回落(`playerredis.Config.OrDefault`)读到的都是填好的值,下游代码不用动。
- **规则(fail-closed)**:`envName` 为空 = 照用字面量(本机 `etc/*.yaml` 与 dev 档 ConfigMap);指名了变量但变量缺席或为空 = 拒启;
  某段同时写了口令字面量 = 拒启(口令只能有一个来源,否则换了 Secret 进程还在用旧值)。出错时不改任何字段,错误只点键名与变量名,不含口令。
- **为什么在配置里写变量名,而不是像 C++ 那样认一个固定变量名**:Go 侧既有约定都是 `*Env` 字段 —— `login.go` 的注释写明「需要从环境变量取的密钥一律写成
  `*Env` 字段」,另有 `PlayerRedis.PasswordEnv`、mail 的 `SharedRedisPasswordEnv`、`MySQL.DataSourceEnv`、`PlayerInbox.AdminTokenEnv`。读 ConfigMap 就能看出口令从哪来;
  本机 yaml 不写这个键,机器上残留的同名环境变量不会悄悄改掉本机行为;而且能做 fail-closed(配置写明要从变量取,变量却没有值)。
- **为什么一个服务一个键,而不是每段一个 PasswordEnv**:go-zero 的 `redis.RedisConf`(含 `zrpc.RpcServerConf` 内嵌的 `Redis` 段)加不了字段;
  这些段连的实例(核心、`redis-queue-g<N>`、`redis-ctl-g<N>`)部署上用同一个口令,Secret `redis-auth` 也只有一个键,分段写只会多出写错的机会。
- **为什么不用 `conf.UseEnv()`**:它对整份配置做 `os.ExpandEnv`,任何值里的 `$` 都会被改写。
- **变量名取 `SHARED_REDIS_PASSWORD`,没有用任务里举例的 `CORE_REDIS_PASSWORD`**:同一个口令还用于 `redis-ctl-g<N>`(scene-manager 的 `Redis` 段)
  与 `redis-queue-g<N>`(login 的 `QueueRedis`、gateway)。本仓库里 core / `CoreRedis` 专指核心实例,叫 CORE_ 会让人以为另外两类实例另有口令;
  README 与 mail 的键名都叫「共享 Redis」。
- **各服务管的段**:login `Node.RedisClient`、`Node.QueueRedis`(写了才有);data-service `NameCacheRedis`、`SharedRedis`;player-locator `RedisClient`;
  scene-manager `Redis`(控制域)、`CoreRedis`;match `Redis`(`MatchRedis` 是独占集群,不归它);trade `SharedRedis`;friend `Redis`(`FriendRedis` 是独立实例,不归它);
  player-inbox `CoreRedis`;guild `RedisClient`、`PlayerLocatorRedis`。chat、db、rank、client-rpc-router 不连共享 Redis,不改。
- **为什么放在迁移分支之后**:data-service、match、trade、friend、player-inbox、guild 的 `-migrate` Job 用同一份 ConfigMap,但不连 Redis,Job 也不注入这个变量;
  放在前面会让迁移 Job 拒启。data-service 的发号器运维入口同理。
- **运维工具**:guild 的 `cmd/assetopfix` 在 guild Pod 里读同一份 ConfigMap 并连缓存 Redis,在 `conf.Load` 之后同样调用。trade 的两个工具、data_service 与 match 的
  各个 cmd、login 的 cmd 都不读服务配置里的共享 Redis,不改。
- **mail 不改**:它早已写 `SharedRedisPasswordEnv: MMORPG_MAIL_REDIS_PASSWORD`,manifest 里对 `redis-auth` 的引用是 `optional: true`,ConfigMap 里没有明文。
  它对「变量为空」的语义是不认证(dev 档 Secret 不存在),与新包的 fail-closed 不同;改成新包要让 dev 档也不写这一行,dev 产物就不再逐字不变,所以保留。

### 9.2 Java gateway 与部署侧

- **gateway**:`spring.data.redis.password` 只写 `"${SHARED_REDIS_PASSWORD}"`。**不带缺省值**:`RateLimitConfig` 用 `@Value("${spring.data.redis.password:}")` 取口令,
  嵌套占位解析不了时 Spring 拒启 —— 与同一份 ConfigMap 里 `admin.api-key: "${ADMIN_APIKEY}"` 同一做法(fail-closed),不会按空口令起来、让限流静默 fail-open。
  Java 代码不用改。口令为空时不写这一行,与之前相同。
- **Deployment**(`tools/scripts/k8s_deploy.ps1`):
  - `Add-SharedRedisAuthToManifest` 经 `Add-GoSvcSecretEnv` 注入 `SHARED_REDIS_PASSWORD`(`secretKeyRef` 到本 namespace 的 `redis-auth` / `password`,不写 optional),
    再往 Pod 模板注解追加指纹 `mmorpg.io/shared-redis-auth-hash`(`Get-InjectedSecretFingerprint`,用途串 `shared-redis/client`,只哈希口令)。
    `-HashOnly` 给 mail,只补指纹。口令为空原样返回。
  - 新函数 `Add-PodTemplateAnnotationEntry`:Pod 模板已有块式 `annotations:` 段就追加到段末,没有就交给 `Add-PodTemplateAnnotation` 新建;段里已有同名键、
    流式写法一律 throw。多数 Go manifest 手写了 prometheus 注解段,`Add-PodTemplateAnnotation` 遇到已有段会 throw,所以要这个函数。
    login 与 gateway 的注解段由脚本一次写入,口令指纹追加在段末,不动第一行(契约测试按位置取削峰指纹与管理面口令指纹)。
  - 调用点:`Apply-OneGoSvc` 里资产密钥占位处理之后、gRPC 口准入之前(条件是 `$script:SharedRedisAuthGoServices` / `$script:SharedRedisAuthHashOnlyGoServices` 两份名单,
    `k8s_migrate_gate.tests.ps1` 不定义它们,条件恒假,不用补桩);`Apply-JavaSvcManifests` 里 gateway 的注解写入之后。
  - ConfigMap:`New-GoSvcConfigMapYaml` 里 11 处口令行与 `New-GuildServiceConfigYaml` 那一处都改写空串,`switch` 之后对名单内的服务按口令非空追加
    `SharedRedisPasswordEnv`;配置里已有同名顶层键即 throw。
  - Secret:沿用 `Sync-RedisAuthSecret`。全局服务在 infra namespace(`Apply-Infra` 的 Redis 分支先建),zone 级服务与 gateway 在 zone namespace(`Apply-Zone` 开头先建)。
  - `Get-SharedRedisClientPassword`:生成期保护同 `Get-CppZoneRedisPassword`(未解析 throw、非 dev 档为空 throw、字符集按 `Assert-RedisPasswordYamlSafe`)。
- **换口令自动滚动**:指纹进了 Pod 模板,只改 Secret 也会滚动。顺带修了 README 记着的既有缺口「只改 Redis 口令时 login 不滚动」;mail 也随之滚动。
  gateway 原有的削峰指纹仍把口令算在输入里,只为不改变既有产物,与新指纹并存。
- **dev 档逐字不变**:口令为空时各段本来就是空串,不追加键、不注入、不写指纹。
- `Assert-RedisPasswordYamlSafe` 的字符集不变,说明与报错里的理由改成「高可用哨兵原样写口令」(口令已不进任何 ConfigMap)。

### 9.3 上线与兼容

- 项目未上线:首次部署整批起步即可。
- 已有集群要先发 Go 镜像,再用新脚本部署:新脚本给某服务写空口令之后,旧镜像不认 `SharedRedisPasswordEnv`,会按空口令连 Redis,全线 NOAUTH;
  反过来新镜像配旧脚本没问题(配置里仍是口令字面量)。gateway 同理:旧脚本写明文、新脚本写引用,Java 代码两边都认。
- 发布轨道的金丝雀(批 2b)走同一个 `Apply-OneGoSvc` 渲染,金丝雀 Deployment 带同一个 env 与指纹(发布轨道会话已确认)。

### 9.4 文件

- 新增:`go/shared/redisauth/{redisauth.go,redisauth_test.go}`;login、data_service、player_locator、scene_manager、match、trade、friend、player_inbox、guild
  九个服务各一个 `internal/config/shared_redis_password_test.go`。
- Go:上述九个服务的 `internal/config/config.go` 与 main(`login.go`、`data_service.go`、`player_locator.go`、`scene_manager_service.go`、`match_service.go`、
  `trade.go`、`friend.go`、`player_inbox.go`、`guild.go`),以及 `go/guild/cmd/assetopfix/main.go`。
- 部署:`tools/scripts/k8s_deploy.ps1`。测试:`tools/scripts/tests/k8s_deploy_contract.tests.ps1`(新节「Go 服务与 gateway 的共享 Redis 口令」,
  改写 3b 节两条 gateway 用例,3a 两处说明文字)。
- 文档:`deploy/k8s/README.md`(新节「Go 服务与 gateway 的共享 Redis 口令」,C++ 口令一节与削峰一节的相关句子)、`deploy/k8s/AGENTS.md`(口令来源一条改成现状)、
  `docs/design/gateway-k8s-deployment.md`(限流那条)、本节与 §8.1 / §8.7、`docs/PROGRESS.md`。

### 9.5 交验证(Codex 执行;C++ 不用重编)

前置:清掉 `MMORPG_*` 环境变量(dev 基线用例假定都没设):`Get-ChildItem Env:MMORPG_* | ForEach-Object { Remove-Item "Env:$($_.Name)" }`。
每条记下通过数 / 失败数与退出码;与本节无关的红按文件归属单独汇报(同一工作区有其他会话的在途改动)。

1. 格式:仓库根 `gofmt -l go/shared/redisauth go/login go/data_service go/player_locator go/scene_manager go/match go/trade go/friend go/player_inbox go/guild` 应无输出。
2. Go(在各模块目录分别执行,任一失败就停下报告):
   - `go/shared`:`go vet ./redisauth/` 与 `go test ./redisauth/ -count=1 -v`,期望 7 个用例全 PASS。
   - 九个服务(`go/login`、`go/data_service`、`go/player_locator`、`go/scene_manager`、`go/match`、`go/trade`、`go/friend`、`go/player_inbox`、`go/guild`)依次:
     `go build ./...`,`go vet ./internal/config/`,`go test ./internal/config/ -count=1 -run 'TestResolveSharedRedisPassword' -v`(每个服务 3~4 条全 PASS),
     再跑整个包 `go test ./internal/config/ -count=1`,不应新增失败。`go/guild` 另跑 `go build ./cmd/assetopfix`。
3. 部署契约(仓库根,PowerShell 7):`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`。本节相关用例全部 `[PASS]`:
   - 新节 6 条,标题以「共享 Redis 口令:」「共享 Redis 口令(」「Add-PodTemplateAnnotationEntry:」「Add-SharedRedisAuthToManifest:」开头;
   - 改写的两条:`staging / prod 的 gateway 带 spring.data.redis.password,值只是对 SHARED_REDIS_PASSWORD 的引用…`、`gateway 削峰指纹随 Redis 口令变化…`;
   - 3a 节各条(`zone Redis 口令…`、`Sync-RedisAuthSecret…`)与 battle 段照旧通过。
   本节新增 1 个 DryRun 子进程(带口令的 infra-up)。
4. 回归,不应新增失败:`k8s_migrate_gate.tests.ps1`、`lobby_authority_render.tests.ps1`、`k8s_redis_split_contract.tests.ps1`、`redis_player_cluster_contract.tests.ps1`、
   `k8s_client_entry_contract.tests.ps1`、`logical_db_deploy_contract.tests.ps1`、`login_metrics_contract.tests.ps1`、`k8s_gate_drain.tests.ps1`、
   `k8s_login_auth_provider_guard.tests.ps1`。
5. dev 档逐字不变的直接证据(建议做):补丁 11-06 已直接打入主工作区,打入前没有留 DryRun 基线,改用比对脚本
   `docs/handoff/shared-redis-secret-20261106/shared-redis-secret-dev-compare.ps1`,在仓库根执行 `pwsh -NoProfile -File <该路径>`(11-06 会话 worktree 删除前已把补丁、比对脚本和它要用的 `k8s_deploy.ps1` 备份按原相对布局挪到这里)。
   - 脚本用打入前的 `k8s_deploy.ps1` 备份,以及「备份 + 本补丁」两份脚本,各跑一次 dev zone-up 与带全局 Go 服务的 dev infra-up(只用 `-DryRun`),逐字比对输出。
   - 两份只差本补丁,其他会话之后对主工作区的改动不会混进来。临时目录名与临时脚本名先归一。
   - 通过的标志是两组都打印 `IDENTICAL`、退出码 0;不同时打印第一处差异的行号,两份全文留在 `%TEMP%`。
   - 脚本、补丁及所需备份已保留在上述交接目录,删除原 worktree 不影响这一步;备份脚本已单独解除 Git 忽略,后续提交时应连同交接附件一起纳入。
6. 可选,要拉镜像,先征得负责人同意:kind 上带 `MMORPG_REDIS_PASSWORD` 跑全新 `infra-up` 与一个 `zone-up`,核对各 Go 服务与 gateway 都能连上 Redis(无 NOAUTH),
   并确认 `@(kubectl get cm -n <ns> -o yaml | Select-String -SimpleMatch -Pattern $env:MMORPG_REDIS_PASSWORD).Count` 为 0(口令从环境变量取,不敲在命令行上);
   再只换口令重跑,确认各 Deployment 滚动、新 Pod 用新口令。

### 9.6 还没做 / 风险

- Secret 注入、指纹滚动、Spring 嵌套占位的解析都没有在集群上跑过(§9.5 第 6 步)。Spring 那一条与 `admin.api-key` 的既有先例一样,只按文档核对过。
- Spring 会把占位解析出来的值再当占位解析一遍:口令里含 `${…}` 会被改写或解析失败。这是既有行为(以前写明文时同样如此),Go 与 C++ 不受影响。
  字符集检查没有拦 `$`,本批不改;生成口令时不要用 `$`。
- 同一批 ConfigMap 里还有别的明文:MySQL 口令(`MMORPG_MYSQL_PASSWORD`)、gateway 数据源口令、`GateTokenSecret`、`Secrets.InternalAuth`、排队令牌密钥。
  属同类问题,不在本批范围,建议另立任务按同一套做法迁移。
- 玩家域 Redis 口令(`PLAYER_REDIS_PASSWORD`)给 Go 服务注入了环境变量,但 Go 服务的 Pod 模板没有对应指纹,只换玩家域口令时 Go 服务不滚动(既有缺口,本批没动)。
- Java 版(AGENTS.md §12):核对了 `D:\luyuan\xuanming-server-mmo`,`deploy/` 下只有 compose、docker、sql、ci,没有 K8s 清单、ConfigMap 或 `secretKeyRef`。
  本批是本仓库 K8s 部署侧与 Go 配置加载的改动,**不适用**,不往 PARITY 加行。本仓库的 `java/gateway_node` 属于 C++/Go 版,代码不用改。
