# 全局单点 P0 五项:静态调查记录与修复设计(2026-10-14)

> 给落码的会话读,Codex 不必读。配套 [Codex 任务单](global-singletons-p0-codex-tasksheet-20261014.md) 与[步骤原文](global-singletons-p0-measure-steps-20261014.md)。
>
> - **来源**:两轮多代理工作流的原始产出,主会话没有逐字改写。第一轮(基线 `0aa1027bc` 加当时的工作区)每项一个调查代理、两个对抗复核代理、一个定稿代理;第二轮把结论锚定到 `2f897bd07`,并做跨项审查。
> - **行号**:每项「原报告」各节按 `0aa1027bc`;「HEAD 前进后的更正」一节给出到 `2f897bd07` 的对照与失效说法。落码前一律按函数名重新定位。
> - **证据级别**:全部是静态读码加对抗复核,没有任何运行证据。标「外部常识」的外部组件行为都未核实。
> - **未定事项**:各项「待拍板」里的方案选择(例如 P0-A 口令走 ConfigMap 还是 Secret、P0-C 的保留量与配额、P0-E 的 maxclients 取值)都是代理的建议,不是负责人的决定。
> - 原报告里「工作区不干净」「编译阻塞是 110 个错误 / 装备表缺失」「子模块全未初始化」等环境描述以「HEAD 前进后的更正」为准。

## 跨项审查(基线 2f897bd07)


### 跨项依赖

核对基线:HEAD 2f897bd07(2f897bd0780c1cc78284548284f20d01e61d7ea8)。以下 file:line 都用 git show 2f897bd07: 重读过;主工作区有在途改动(见 conflicts 第 1 条),没有拿它当证据。

D1【成立,硬约束】任何 etcd 滚动或重启,都要等 P0-D(C++ 流重建)修复并验证通过之后
- 依据:handoff-global-singletons-10m-ccu-20261013.md:70(原文「这是任何 etcd 滚动操作的前置条件」)、:138。
- 机理:
  - grpc_init_client.cpp:407-411 收到 ok=false 时直接 return,丢掉这次完成事件;
  - etcd_service.cpp:361-365 只处理服务端发来的 canceled。
- 受这条约束的部署步骤:
  - P0-C 阶段 2:etcd.yaml 加 env、钉镜像、加抓取注解,都会触发 StatefulSet 滚动;
  - P0-C 1a 里 compose 的 etcd env:已有的开发机要重建 etcd 容器才生效,重建就是重启(etcd-compaction.md risks 第 26 条)。
- 受这条约束的实测步骤:P0-D M4/B4/B6、P0-C M6 都会重启开发 etcd。在跑的 C++ 节点因此进入约 181s 一轮的重注册循环,只能重启进程。
- 新核出的放大效应:Apply-Infra 每次都 apply 全部五份 infra 清单(k8s_deploy.ps1:6448,$infraManifests = etcd/redis/redis-match-cluster/kafka/mysql)。
  - 后果:P0-C 阶段 2 的 etcd.yaml 改动一旦合入 main,之后任何一次 infra-up 都会顺带滚动 etcd,包括只为 P0-A 的 redis-auth Secret、P0-B 的 battle-node-config、P0-E 的 redis.yaml 而跑的那几次。
  - 结论:阶段 2 不能在 P0-D 部署到所有环境之前合入 main。
- 例外:P0-C 1a 里新增的独立 CronJob 只做 compact、不重启 etcd,不受这条约束(etcd-stream-rebuild.md:600)。

D2【成立,软约束,有条件】开启压缩后,watch 的续看点可能已被压缩
- C++ 现状:
  - 收到取消的分支直接 return,不推进 revision(etcd_service.cpp:361-365);
  - 2s 后用 revision[prefix] 重建 watch(:393-400、:278-285);
  - 这个值每 2s 被周期 Range 刷成最新(etcd_service.cpp:51、:67-69;service_discovery_manager.cpp:16-18、:27-33)。
  - 所以:只要保留量远大于 2s 的写入量,重建就不会反复撞上 compacted。但取消到重建之间的 DELETE 事件会丢,因为 Range 只当 PUT 处理(:58-59),会留下幽灵节点;直到 P0-D 的「list → 对账删除 → 从 R+1 开始 watch」落地才解决(etcd-stream-rebuild.md fix【二】3-4)。
- Go 现状:各 watcher 都是先 Get 再 WithRev(rev+1)(scenenode/watcher.go:177、match/internal/discovery/node_watcher.go:135、client_rpc_router node_watcher.go:137、load_reporter.go:808、killswitch/watch.go:87、allocator.go:876/:1089/:1169/:1244),出错就回到全量重同步。压缩是否一定以 Err 的形式到达,要 P0-C M6 实测。
- 由此得出三条约束:
  - (a) KEEP_REVISIONS 必须远大于 2s × revision/s。建议值 1e6 满足。
  - (b) 交接 §4.4「启动时 Range 一次,之后只消费 watch」会去掉这道 2s 兜底,必须排在 P0-D 之后。
  - (c) P0-C 对 P0-D 提的第 4 条验收「DELETE 处理不依赖前值」,P0-D 的设计没有覆盖(见 gaps 第 13 条)。

D3【成立,硬约束】C++ Redis AUTH 是接入 redis-ha 的前置
- 依据:handoff-global-singletons:222(原文「redis-ha 前置:C++ AUTH + WAITAOF」)。
- 机理:
  - redis-ha.yaml:257-259:口令非空就带 --requirepass 和 --masterauth;
  - haproxy 只在自己的健康检查里发 AUTH(:503-514),不替客户端认证;
  - 切主时会掐断已有连接(:19-21);
  - C++ 的重连路径不发 AUTH:DoReconnect 在 redis_manager.cpp:141-157,连上回调在 :97-104。
  - 所以 P0-A 修复必须做到「每条新连接、每次重连都先 AUTH,再补发积压」。
- 现状:redis-ha 还没接入脚本(tools/scripts 下 RedisReplicas、redis-ha.yaml 都是 0 命中),所以这条不阻塞本轮实测。
- WAITAOF 那一半归 go/db 交接(§2.3 表)。
- 注意:单实例 redis.yaml:93-97 本来就带 requirepass,所以 release 档今天已经受影响,不是只有接 redis-ha 时才有问题。

D4【成立】maxclients 与 nofile 联动
- Redis 启动时把 nofile 抬到 maxclients+32;硬限不够时下调 maxclients 并打日志。
  - 出处:third_party/redis/src/server.c:2503-2572;常数 CONFIG_MIN_RESERVED_FDS 在 server.h:150。
  - 这是 8.4.0 的源码;redis:7.2 镜像的实际表现以 P0-E S2.2/S2.3 实测为准。
- redis.yaml:93-97 两条 exec redis-server 都没有 --maxclients,也没有 ulimit 或 securityContext。P0-E 要改的正是这两行。
- 探针也占名额:startup/readiness 探针的 redis-cli ping 走普通连接(:113-127)。到线时探针失败,Service 摘掉端点,全服失去 Redis(P0-E S2.4 实测)。
- redis-ha 同样没设 maxclients(:250-259);它的 haproxy 另有一道上限 maxconn 20000(:530)。接入 redis-ha 时总账要重算。
- 这条与 gate/scene/battle 容器的 nofile(launch-readiness §4 第 7 项)不是一件事,不要合并处理。

D5【成立】battle Kafka 与 Redis AUTH 改的是同一个 ConfigMap 生成函数
- 函数:New-NodeConfigMapYaml,k8s_deploy.ps1:2109-2319。
  - P0-B 改 $baseDeployConfig 的 Kafka.GroupID(:2262,Kafka 段 :2257-2266);
  - P0-A 方案 A 改 $gameConfig 的 zoneredis.password(:2301,zoneredis 段 :2298-2305)。
  - 同一个函数同时生成 zone 的 node-config(:6043)和 battle-node-config(:6436-6437)。
- 契约测试的落点:两项都要在 $battleInfraRun(k8s_deploy_contract.tests.ps1:1149-1151)之后加用例;P0-A 还要在 :999-1001 之后加。
- P0-E 第二批也碰同一区域:总账要读 zoneredis.max_connections(:2304);重启闸改 Apply-Infra 的 redis 分支(:6491-6523),P0-A 的 redis-auth Secret 段(:6497-6514)就在这个分支里。
- 指纹注解耦合:P0-A 要给 5 个 C++ Pod 模板加指纹注解(:2354、:2936、lib/k8s_client_entry.ps1:740)。
  - 如果哈希的是整份 node-config,P0-B 批 2 改 GroupID 时会自动滚动 gate(踢人)和 battle(作废对局)。
  - 建议:只哈希口令。

D6【新核出】GroupID 改名与 DisableLegacyPerNodeTopic 耦合
- 旧的「一节点一 topic」消费组:GroupID 非空时就用它;为空时回落成 <prefix>-<nodeId>(node_kafka_command_handler.h:124-129)。
- 这条路径用默认 assignPolicy,即 ownOffsets=false,对应 earliest、自动提交(kafka_consumer.cpp:112-113)。
- 后果:P0-B 批 2 把 GroupID 改空后,gate/scene 进入新组,会从最早位点重放旧 topic 里的残留命令。node_id 会被复用,所以重放的可能是前任节点的命令。
- 生产侧静态证据:
  - C++ 的 LegacyPerNodeTopic( 在 HEAD 上只有定义,没有调用方(node_command_route.h:57);
  - login 已收口到新 topic(go/login/internal/svc/gate_command.go:15-23)。
  - 所以按 handoff-global-singletons:68 的原方案,同批置 DisableLegacyPerNodeTopic: true 有依据。
- 仍需上线前实测:线上旧版本 login 是否还在写旧 topic、旧 topic 里有无残留。
- 同一行在 godb 附录 :170 被当作「顺带发现」报告过(当时行号 :1830)。

D7 C++ 的阻塞与落码互相耦合
- 共同的运行期阻塞。以下步骤全部被同一组环境问题卡住:P0-A B 段、P0-B 步骤 2/3、P0-C M5/M6 的 C++ 部分、P0-D A1/B、P0-E S3b/S4。
  - Boost 模块布局导致 C1083(PROGRESS:7637);
  - install_vs2026_dbg 缺失(PROGRESS:7462 同一原因:缺 protobuf 头);
  - 漏项:grpc 嵌套子模块(googletest、boringssl、cares、re2、zlib 等)未检出。测试工程的 gtest include 路径就指向 third_party/grpc/third_party/googletest(rpc_controller_test.vcxproj:114、kafka_command_test.vcxproj:158),所以 GTest 用例连编译都过不了。
- 落码冲突点:
  - node.cpp:P0-A 改 :541-557,P0-B 改 :634-637,P0-D 改 :1320-1416、:1537-1572;主工作区还有在途改动在 :1179-1192。
  - rpc_controller_test.vcxproj 的 :169-171:P0-A 和 P0-D 各加一行 ClCompile。
  - kafka_command_test.vcxproj 的 :138、:161-162:P0-B 和 godb 附录 :262 都要改。
  - kafka_producer.{h,cpp}:P0-B 和 godb 附录都要改。
  - redis_manager.h(P0-A)和生成器模板(P0-D,要重生 17 个客户端)都会触发大面积重编。
- 建议:三项的 C++ 第一批排好先后,只做一次全量 /m:1 构建,一次跑红绿。

D8 部署窗口受 L-04 约束
- godb 交接 §2.4:L-04 部署之前,有真实存档的环境不要 apply 当前的 mysql.yaml,它会触发 Recreate,唯一的 MySQL 要停一次。
- 而 infra-up 每次都会 apply mysql.yaml(:6448)。
- P0-A、P0-B、P0-C 1b、P0-E 的部署都要走 infra-up。所以在 L-04 部署之前,这些项只能在维护窗口里做,或者改用定向 kubectl apply。
- L-04 现状:只在主工作区落了第 1 批,未提交(在途 PROGRESS 条目原文「L-04 仍未关闭」)。
- 5 项里只有 P0-C 写了这条约束。

D9 C++ 滚动合并成一个窗口
- 要换 node 镜像的:P0-A、P0-B 的第一批,以及 P0-D。
- 要重启 Pod 的:P0-A 和 P0-B 的第二批。改 ConfigMap 不会自动滚 Pod,因为节点 Pod 模板只有 sidecar 的哈希注解(:2354、:2936)。
- 建议合成一个窗口:gate 重启会踢人,battle 重启会作废对局。
- 顺序约束:P0-D 要在任何 etcd 滚动之前;P0-A 要在 redis-ha 之前。

### 待拍板(汇总,按阻塞程度排序)

按阻塞程度从高到低排列;已去掉重复项。

1. 【最阻塞】C++ 构建环境恢复要不要授权
   - 这一项卡住所有 C++ 运行证据和全部 C++ 修复验证,5 项都受影响。
   - 内容:
     - Boost 扁平头:可以在 third_party/boost 里生成,也可以改各个 vcxproj 的 include。归档恢复不可行,分卷已经删了。
     - 用 build_grpc.ps1 构建 Debug 版 install_vs2026_dbg。
     - 另外构建 hiredis 和 yaml-cpp 的 Debug 库。
     - 初始化 grpc 嵌套子模块(googletest、boringssl 等)。
     - 那 5 个顶层未初始化的子模块,是否真需要,先请 Codex 只读确认。
   - 注意:build_grpc.ps1 会通过 winget 安装或升级 cmake/ninja,并带上 --accept-source-agreements / --accept-package-agreements(build_grpc.ps1:167-174),等于自动同意条款。如果本机已有达标版本,可以用 -SkipToolCheck 跳过。
   - 建议:作为一项跨 5 个 P0 的独立环境任务授权一次,在专用构建树里做,不放在主工作区,结果写进 PROGRESS。在此之前,先用「渲染 + 探针 + 静态证据」作为第一批落码的门槛。

2. 测量树和预写代码放在哪
   - 建议:
     - 所有渲染只用一棵新工作树 %TEMP%\p0-measure-2f897bd07。
     - 预写代码一律放在草稿目录,不进主工作区,由 Codex 拷进测量树。涉及:P0-A 和 P0-B 的契约红用例、P0-B 的 p0b-red.patch、P0-D 的 P1/P2、P0-E 的 R0。
     - 主工作区、f452、793e 都不动。
   - 要不要现在就让 Claude 写这些代码,请一并决定。

3. 下载要不要同意,只在 S0 盘点确认缺失时才需要
   - 涉及:redis:7.2(P0-A、P0-E)、bitnamilegacy/etcd:latest(P0-C)、Go 模块(默认 GOPROXY=off)、网关 jar 的 Maven 依赖(P0-E S3b)。
   - 建议:两个镜像同意拉取;Go 模块和 Maven 依赖等第 1 项授权时一并决定。

4. 第二批落码怎么排队
   - 涉及文件:k8s_deploy.ps1、两份契约测试、两份 README。
   - 建议:
     - 先等主工作区里评审修复会话和路由服预算会话的改动提交。
     - 之后按 P0-B 批 2(GroupID,改动最小)→ P0-A 批 2 → P0-E 批 2 → P0-C 1b 串行落码。
     - 每批落码前先重新 git status,只 add 本批文件。
   - 两条 HEAD 上已知的红用例(prod 档缺排队令牌、:1649 中文断言)交给正在修它们的在途会话。主工作区的在途改动显示,这两处它都已经在改。

5. 开发环境上的破坏性操作,要不要做、合并成哪个窗口
   - 涉及:
     - 重启开发 etcd:P0-D M4、B4、B6;
     - 不可逆压缩并重启:P0-C M6;
     - 把共享 Redis 名额占满:P0-E S5;
     - 往开发 etcd 和 Kafka 里留下注册信息和 topic:P0-A B4、P0-B 步骤 3、P0-D B2。
   - 建议:等第 1 项完成后,在一个时段内按第 5 段的顺序一次做完,做完整栈重启。M4 并进 P0-D 的 B4。

6. L-04 部署之前的部署窗口
   - 问题:infra-up 每次都会 apply 当前的 mysql.yaml(会 Recreate)。
   - 建议:有真实存档的环境,所有 P0 的 infra 侧部署要么等 L-04 部署后再做,要么在维护窗口里用定向 kubectl apply。在 P0-A、P0-B、P0-E 的上线步骤里写明这一点。

7. P0-B 批 2:GroupID 置空,DisableLegacyPerNodeTopic 是否同批
   - 建议:同批置 true,与源交接 :68 一致。依据是 C++ 已经没有写旧 topic 的调用方,login 也已收口。
   - 上线前先在目标环境查两件事:旧 topic 的末位点,线上 login 的版本。
   - 如果不同批,就必须接受新组会从 earliest 开始重放。

8. P0-A 用方案 A 还是方案 B,指纹覆盖什么
   - 方案 A:明文写进 ConfigMap 加指纹,有 G4b 先例。
   - 方案 B:走 Secret,与源交接 :67 一致。
   - 建议:方案 A 可以接受;但指纹只覆盖口令,不哈希整份 node-config,避免 P0-B 和其它 ConfigMap 改动连带滚动 gate 和 battle。

9. P0-D 何时开工、怎么验收
   - 开工门槛:建议采用「HEAD 静态链 + gRPC 头文件里的约定 + M2 模板红」作为门槛。
   - 验收:A1 和 B 路线等第 1 项完成后补跑。
   - P0-C 要求的「DELETE 不依赖前值」是否纳入 P0-D 的范围,建议纳入验收。
   - 「重建不设次数上限」是有意偏离 §11.3,建议认可,并写进设计文档。

10. 运行期新增的 fail-closed 行为是否接受
    - P0-B:Kafka.Brokers 为空时拒绝启动。
    - P0-A:zoneredis.db 不为 0 时 LOG_FATAL;方向上选 AUTH,不选 NetworkPolicy。
    - 建议全部接受。

11. P0-C 的参数
    - 建议:KEEP_REVISIONS 取 1e6;配额取 3GiB;镜像按 tag@摘要 钉住。
    - 阶段 2 等 P0-D 部署完之后才合入 main。1b 与 1a 同批。

12. P0-E 的参数
    - 建议:N 取 40000;路径选甲还是乙,看有没有需要保护的在线环境;已有环境在线 CONFIG SET 必须在到线之前做;compose 是否同批改;redis-ha 文件头加注释;重启闸与 G4a 的 -ConfirmRedisCoreRestart 合并;要不要上 redis_exporter;总账超限只告警。

13. 代码交接方式和 PARITY 用词
    - 建议用补丁交接,不做本地 commit。PARITY 落码后先登记「mmorpg 待做」,验证通过后再改「已对齐」。

### 漏项

1. 【P0-A A2 在 HEAD 上必然 exit≠0;已知红用例清单不全】
   - 原因链:
     - prod 档 LoginQueue 取 auto,结果是开(k8s_deploy.ps1:242-247、:349-363)。
     - zone-up 部署 login 时(Test-ZoneLoginDeployed :1637-1639),写路径入口(:6642)会调用 Resolve-InjectedSecret 读 MMORPG_LOGIN_QUEUE_TOKEN_SECRET(:532-541)。
     - prod 档变量为空就 throw(release_common.ps1:696-698)。P0-A 的 $envs 没有这个变量。
   - 后果:
     - A2 会走进「改 dev 重跑」的回退分支,prod 路径的证据就丢了。
     - 契约测试 $ProdEnv(:975-990)同样缺它,所以「prod 档位 + 注入环境变量时生成器可以正常出产物」(:999)以及依赖 $prodRun 的 :1006、:1016、:1037、:1053、:1844、:1852、:1863、:1893 在 HEAD 上都会红,不止 :1649 那一条。
     - P0-A 计划在 :999-1001 之后复用 $prodRun 写红用例,在 HEAD 上会因为这个无关原因失败。
   - 佐证:主工作区的在途改动正好在 :990 补了这个变量,值是 'contract-test-queue-token-0123456789abcdef'。
   - 修法:A2 的 $envs 加上同一个值(≥32 位,且与 GATE、INTERNAL_AUTH 都不同),或在参数里加 -LoginQueue off。
2. 【主工作区状态写错】P0-C、P0-D、P0-E 以及本任务单都写「只剩两个子模块脏标记」。本次核对是 98 处在途改动,其中有 node.cpp、k8s_deploy.ps1(+126)、契约测试(+497)、scene 的 redis.cpp 与 vcxproj、PROGRESS。
   - P0-D M0 写的是「不一致就停下」,一跑就会停,要改成只记录。
   - P0-B 步骤 0、P0-D A1/B 默认在主工作区编译,结果无法归因,见 conflicts 第 1 条。
3. 【P0-A C 段 docker 命令不完整】没有 --context desktop-linux,也没有 --pull never:docker run 遇到缺失的镜像会静默拉取,「下载先征得同意」守不住。镜像出处也写错了:compose 的 image 在 docker-compose.yml:218,不是 :217,P0-A 订正清单第 14 条本身又错了一行。
4. 【P0-A B4 没把 install_vs2026_dbg\bin 加进 PATH】cpp_nodes.ps1 自己不加;P0-D B2 和 start_game.ps1:551 都加了。不加的话 scene 缺 zlibd.dll(PROGRESS:4206),会被误判为「没跑起来」。P0-A B 段也没有「依赖就绪」检查(etcd health、kafka 在监听)。注意 RegisterKafkaHandlers 失败会 LOG_FATAL(node.cpp:963)。
5. 【Kafka topic 预建只有 P0-D 写了】P0-A B4、P0-B 3-2、3-5 都会让 C++ 节点生产消息,把未预建的 topic 以 1 个分区自动建出来,污染开发环境(docker-compose.yml:66-81)。
6. 【会话环境变量泄漏】
   - P0-A 的 GAME_CONFIG_PATH(battle 也读,battle_room_manager.cpp:359)、P0-B 的 RPC_PORT,都没写清理时机。
   - P0-B 3-1 直接改被跟踪的 bin/etc 文件,而 base_deploy_config 没有环境变量覆盖(node.cpp:1245)。各项都没说明它与其它 B 段的串行关系。
7. 【渲染前没有清 MMORPG_*】只有 P0-A 清了 MMORPG_REDIS_PASSWORD;P0-B、P0-D 都没清。子进程会继承调用方的环境(deploy_capture.ps1:52-62)。先例做法见 PROGRESS:7403 第 0 步。
8. 【Go 步骤的下载控制不一致】P0-D M2 只设了 GOTOOLCHAIN=local 和 -mod=readonly,没设 GOPROXY=off,缺模块时可能静默下载。P0-E 设了 GOPROXY=off,godb 任务单也是这个约定。
9. 【pwsh 门槛不一致】P0-A、P0-B 要求 7.x,P0-C、P0-E 要求 ≥7.3。M1 的 etcdctl get "" 需要 7.3 的原生空串传参。建议统一为 ≥7.3。
10. 【工作树重复且路径不一致】四个工作树取了四个名字;P0-A 还建在 .codex/worktrees 下。全量契约测试被跑三次(A1、1-3、M3);P0-A A3 不带口令那次与 P0-B 1-1 参数相同,也是重复渲染。
11. 【GTest 构建前置少一条】测试工程的 gtest 头取自 third_party/grpc/third_party/googletest(rpc_controller_test.vcxproj:114、kafka_command_test.vcxproj:158),这个嵌套子模块未检出(gtest.h 不存在)。三项的 B0、步骤 0 都只查了 install 目录。另有 PROGRESS:7462 记录的「缺 protobuf 头」失败,与 7637 同属 install 目录缺失,各项只引用了 7637。
12. 【工具可用性口径矛盾】
    - P0-D 说 protoc 35.1 只在 install_vs2026_dbg\bin,所以 regen 受阻;但 PROGRESS:7538 记录 10-13 Codex 用 protoc 35.1 做过语法预检和一次全量 proto-gen。
    - Python:PROGRESS:7397 说只有 Store 占位入口,PROGRESS:7538 却跑过导表。
    - 需要 Codex 回报真实位置,否则「受阻」的判断不可靠。
13. 【P0-C 与 P0-D 验收口径不一致】P0-C 要求 P0-D 满足四条验收,其中一条是「DELETE 处理不依赖前值」(etcd-compaction.md risks 一.1)。P0-D 的设计用「list → 对账 → 从 R+1 开始 watch」处理了压缩导致的取消,但在线 watch 的 DELETE 仍然用 prev_kv(etcd_service.cpp:388)。两边没有对齐。
14. 【漏掉的同类入口】
    - P0-B 批 2 改 GroupID 会让旧 topic 的消费组从 earliest 重放(D6),P0-B 只说了「位点副作用」,没有给判据或检查命令。
    - P0-E 的总账没算 redis-ha 的 haproxy maxconn 20000(redis-ha.yaml:530)对主节点的影响;P0-E 只把它放进了 owner (e)。
15. 【预写代码的落点不一致】P0-A、P0-D(P1)、P0-E(R0)计划直接写进主工作区,P0-B 计划放草稿目录。主工作区的 tools/scripts/tests 一级目录会被 CI 和其它会话整体运行,新红用例会干扰别人。建议统一放草稿目录,由 Codex 拷进 $W。
16. 【既有判据仍然区分不了两种情形】
    - P0-D 判据 D(RPC failed 行):原单已注明。
    - P0-C M1 在 revision 较小时得不出结论:原单已注明。
    - P0-E S3a 中 lib-name 为空的一组同时包含 C++、login、db,只能靠进程计数区分:原单已注明。
    - 新增 P0-A C 段:一会用 redis:latest、一会用 redis:7.2,得到的 NOAUTH 与 WRONGPASS 文本不能直接当作 K8s(7.2)的结论。应统一用 7.2。
17. 【DryRun 隔离只靠代码分支】DryRun 不连集群依赖 k8s_deploy.ps1 的 DryRun 分支(:1503、:2035-2037、:2849)和 client_entry 的约定。本机默认 context 指向别的项目的集群(kind-config.yaml:12-15),建议再设一个不存在的 KUBECONFIG 作纵深防御,各项都没写。
18. 【G2b 的历史通过标准已不可用】PROGRESS:7403 给 Codex 的标准是「98 通过 0 失败」和总账那一行。在 HEAD 上已有 gaps 第 1 条的红,G2b 的验证结果至今也没回报。所以 HEAD 契约测试的基线还要靠本次跑出来,不能沿用那条标准。

### 冲突与重叠

1. 主工作区能不能当构建树,各项说法冲突
   - P0-B 步骤 0 与 P0-D A1/B 打算在主工作区编译;P0-A、P0-E 说不能。
   - 实际情况:主工作区有其他会话未提交的 C++ 半成品:
     - entt 审计涉及 node.cpp:1179-1192、return_define.h、ecs_context 等;
     - 入场会话栅栏:新增未跟踪的 enter_fence.h,并改了 scene.vcxproj、scene 的 redis.cpp;
     - 另有 gate、scene 的 handler 改动。
   - 在主工作区编译,结果无法归因。统一用专用构建树(shared S7)。

2. 落码文件互相争用,第二批要排队
   - k8s_deploy.ps1:
     - P0-A 改 :2298-2305、5 个 C++ 模板、:6497-6514;
     - P0-B 改 :2257-2266;
     - P0-C 1b 改 :6448、:6610;
     - P0-E 改 :6491-6523,并在 :1008-1268 一带加总账;
     - 主工作区还有在途 +126 行(路由服转发预算,插在 :1003、:1771、:6763、:6770,之后的行号整体后移约 120)。
   - k8s_deploy_contract.tests.ps1:P0-A 插在 :1001 与 :1151 之后,P0-B 插在 :1148 与 :1151 之后,P0-E 照 :1455 的形状加,另有在途 +497 行。
   - 其它共用文件:deploy/k8s/README.md(P0-B、P0-C、P0-E)、docs/README.md 与 gen_docs_index.py(P0-C,另有在途改动)、docs/PROGRESS.md(各项)。
   - C++ 的冲突见 cross_item_dependencies 的 D7。

3. 与 launch-readiness 交接冲突
   - 顺序:§7 的总顺序把全局单点阶段 0(也就是这 5 个 P0)排在 §4 第 1~4 项之后:G2b、G4b、三视角评审修复、行片 16→64。G2b、G4b 已入库;评审修复在主工作区在途;在途 PROGRESS 记录「行片 16→64 做了一半、queue_test.go 在 HEAD 编译不过」。所以 P0 的第二批对 k8s_deploy.ps1 和契约测试的改动,要排在评审修复会话提交之后。
   - 文档索引:§5 第 7 条要求跑 python gen_docs_index.py --check,与 PROGRESS:7397「无 Python」冲突。P0-C 修复第 5 步也依赖这个检查。
   - 归属:§4 第 7 项把「钉 etcd 镜像版本」「关掉一节点一 topic」归给全局单点交接。前者与 P0-C 阶段 2 一致;后者与 P0-B owner 第 6 条「另案处理」冲突。

4. 与源交接 handoff-global-singletons 冲突
   - :68 写 battle Kafka 修法是「模板 GroupID 改空,同时加 DisableLegacyPerNodeTopic: true」;P0-B 建议另案处理。按 D6 的分析,两者是耦合的。
   - :67 写 Redis 口令「走 Secret」;P0-A 现在改为建议方案 A(明文写进 ConfigMap 加指纹),理由是 G4b 的先例(k8s_deploy.ps1:5556-5568)。方向与源交接相反,需要负责人确认。

5. 与 godb 交接冲突或重叠
   - §2.4 的 L-04 约束与 infra-up 连带 apply mysql.yaml(D8)。只有 P0-C 提到,P0-A、P0-B、P0-E 的部署计划都没写这个窗口。
   - 附录 :170 已报告 K8s GroupID 共享组的问题(与 P0-B 批 2 是同一行),应在两边互相登记,避免重复修改。
   - 附录 :262 也要在 kafka_command_test.vcxproj 里加一行 ClCompile,并在 kafka_producer.cpp:224 加 partitioner;与 P0-B 的红补丁改同一组文件。
   - 附录第 7 步要删的是 login ConfigMap 里的 GroupID,与 C++ node-config 的 GroupID 同名不同物,文档里要写明,避免混淆。
   - §3.3 的 go.mod 升级与 launch-readiness §5 第 8 条「不改 go.mod」冲突。本次实测统一用 -mod=readonly,不改 go.mod 与 go.sum。

6. 与 0aa1027bc 之后的 PROGRESS 条目冲突
   - PROGRESS:7403(G2b)给的通过标准「98 通过、0 失败」:在 HEAD 上已不可能,原因见 gaps 第 1、18 条。
   - PROGRESS:7637 说「独立 GTest 可行」(BattleAdmissionGate):对 P0-B、P0-D 的用例不适用,它们必须链接 gRPC、librdkafka 或 muduo。P0-B、P0-D 自己已说明;本任务单里「独立 GTest 走得通」的推论不能推广到这两项。
   - PROGRESS:7538 与 :7397 关于 Python、protoc 的说法互相矛盾(gaps 第 12 条)。

7. 各项之间说法不一致
   - 主工作区在途改动数:P0-A 写 71,P0-B 写 45,P0-C、P0-D、P0-E 写「干净」;本次核对是 98。
   - redis 镜像行号:P0-A 写 :217,P0-E 写 :218,实际是 :218。
   - pwsh 门槛:7.x 与 7.3 两种说法。
   - 工作树路径:四处各自取名。
   - P0-D 说压缩 CronJob 不受「不滚 etcd」的限制;P0-C 的 1a 同时改 compose 的 etcd env,既有开发机要重建容器才生效。这两句话合在一起,意味着 1a 里 compose 那一半实际上也是一次 etcd 重启,要么排在 P0-D 之后,要么注明需要整栈重启。

8. 实测步骤之间直接冲突
   - P0-E S3a 要求开发栈在跑;P0-D B0 要求没有任何节点在跑。
   - P0-E S5 占满共享 Redis 的名额,期间其它依赖 Redis 的步骤都会失败。
   - P0-D M4/B4、P0-C M6 重启开发 etcd,会让在跑的 C++ 节点永久进入重注册循环。
   - 处理方式:按 recommended_execution_order 第 3、5 段排序。

### 公共环境(已并入任务单 §2)

S0 只读盘点:一个 pwsh 会话,约 5 分钟。合并了 P0-A 0.1/B0、P0-B P/0、P0-C M0 的环境部分、P0-D M0、P0-E S0。
```
pwsh --version; $PSVersionTable.PSVersion   # 统一要求 ≥7.3
Get-Command pwsh,go,docker,kubectl,kind,protoc,python,py,cmake,ninja,winget -All -ErrorAction SilentlyContinue | Format-Table Name,Version,Source
go version; go env GOMODCACHE GOPROXY GOTOOLCHAIN GOFLAGS
docker context ls
docker --context desktop-linux info --format '{{.OSType}}|{{.ServerVersion}}'
docker --context desktop-linux image ls --format '{{.Repository}}:{{.Tag}} {{.ID}} {{.CreatedAt}}' | Select-String 'redis|etcd|mmorpg-node'
docker --context desktop-linux ps -a --format '{{.Names}}|{{.Image}}|{{.Status}}|{{.Ports}}'
$R='D:\luyuan\xuanming-server-mmo-cpp'
git -C $R rev-parse 2f897bd07
(git -C $R status --short | Measure-Object).Count   # 只记录,不作停止条件
git -C $R worktree list
git -C $R submodule status
git -C $R\third_party\grpc submodule status
'third_party\boost\boost\any.hpp','third_party\grpc\install_vs2026_dbg','third_party\grpc\third_party\googletest\googletest\include\gtest\gtest.h','bin\gate.exe','bin\scene.exe','bin\battle.exe' | % { "$_=" + (Test-Path (Join-Path $R $_)) }
Get-ChildItem $R\lib -Filter *.lib -ErrorAction SilentlyContinue | Measure-Object
& "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe'
pwsh -NoProfile -File $R\tools\scripts\cpp_nodes.ps1 -Command status
Get-Process gate,scene,battle -ErrorAction SilentlyContinue
Test-Path "$env:TEMP\kind-mmorpg.kubeconfig"
```
- 期望(10-14 本次只读核对):
  - rev-parse 输出 2f897bd0780c1cc78284548284f20d01e61d7ea8。
  - 主工作区约 98 处在途改动,只记录。注意 P0-D M0 写的「只剩两行子模块脏标记,否则停下」必须改成只记录(见 gaps 第 2 条)。
  - 顶层未初始化的子模块恰好是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash。
  - grpc 的嵌套子模块除 abseil-cpp、protobuf 外都未初始化。
  - 上面 6 个 Test-Path 都是 False,lib 下 0 个 .lib。
- 需要额外回报的事项:
  - protoc 35.1 在哪。PROGRESS:7538 记录 Codex 10-13 用过它。
  - Python 是否可用。PROGRESS:7397 说只有 Store 占位入口,而 PROGRESS:7538 显示 10-13 跑过导表。
  - 10-14 构建生成器和 BattleAdmissionGate 用的 gtest 装在哪里。
  - Codex 沙箱是否放行 127.0.0.1 回环。godb 任务单 :23 有同样的前置;P0-C 的 /metrics、P0-A 的 6390 端口、P0-D A1 都要用回环。

S1 测量工作树:所有纯 HEAD 的渲染只用这一棵,替换四处各自取名的工作树。
```
$W = Join-Path $env:TEMP 'p0-measure-2f897bd07'
git -C $R worktree add --detach $W 2f897bd07
git -C $W rev-parse HEAD; git -C $W status --short   # 期望完整哈希,status 为空
```
- 它替代的路径:
  - P0-A 的 C:/Users/luyua/.codex/worktrees/p0a-2f897bd07。不要建在 Codex 自己的 worktrees 目录里。
  - P0-B 的 %TEMP%\p0b-render-2f897bd07。
  - P0-D 的 %TEMP%\p0d-etcd。
  - P0-E 的 %TEMP%\p0e-maxclients-2f897bd07。
- 顺序要求:先跑完全部纯 HEAD 的 DryRun 和契约测试,再往里拷任何测试文件。拷入后 git status 非空,k8s_deploy.ps1:6632-6633 会打 dirty 警告。
- 主工作区、f452、793e 一律不碰。

S2 DryRun 会话卫生。新开一个 pwsh 7 会话,只用来跑 DryRun。沿用 G2b 的先例(PROGRESS:7403 第 0 步):
```
Get-ChildItem Env:MMORPG_* | Remove-Item
Remove-Item Env:GAME_CONFIG_PATH,Env:RPC_PORT -ErrorAction SilentlyContinue
$env:KUBECONFIG = Join-Path $env:TEMP 'p0-no-cluster.kubeconfig'   # 不存在的文件
Set-Location $W
. ./tools/scripts/tests/lib/test_harness.ps1
. ./tools/scripts/tests/lib/deploy_capture.ps1
$out = Join-Path $env:TEMP ('p0-out-' + (Get-Date -Format yyyyMMdd-HHmmss))
New-Item -ItemType Directory $out | Out-Null
```
- 为什么清 MMORPG_*:Invoke-CapturedPwsh 只覆盖传入的变量,其余全部继承给子进程(deploy_capture.ps1:52-62、:77-78)。dev 档的口令变量只要非空就会被采用(release_common.ps1:690-692),既会让判据串味,也会把真值写进保存的输出。
- 为什么设一个不存在的 KUBECONFIG:纵深防御。本机默认 context 指向别的项目的集群(kind-config.yaml:12-15);DryRun 本身不调 kubectl(k8s_deploy.ps1:1503、:2035-2037、:2849)。

S3 一次渲染、多项共读
- zone-up prod,一次:P0-A 的 A2,补上 MMORPG_LOGIN_QUEUE_TOKEN_SECRET(见 gaps 第 1 条)。同一份 node-config 里也读出 Kafka.GroupID,作为 P0-B 判据 A 的 zone 部分;它和 dev 档是同一个模板常量(:2262)。
- zone-up dev,一次:P0-B 的 1-2。参数就是契约测试 :33-40 的 $BaseArgs。
- infra-up,带口令和不带口令各一次:
  - P0-A 的 A3 不带口令那次,与 P0-B 的 1-1 参数逐字相同(都等于契约测试 :1149-1150),所以只跑一次。从它的 battle-node-config 里同时读 P0-A 要的 zoneredis 段,和 P0-B 要的 GroupID、LogLevel、DisableLegacyPerNodeTopic。
  - 带口令那次只为判 Secret 是否创建。
- 全量契约测试只跑一次,替代 P0-A A1、P0-B 1-3、P0-D M3 三次运行:
  ```
  pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1 *> $out\contract-baseline.txt
  ```
  按用例名分别判读。
- P0-C 的 S1 和 P0-E 的 S1 都是对 2f897bd07 做 git show / git grep,不需要工作树。

S4 Go 步骤共用环境。用于 P0-D M2、P0-E S2b、P0-C M5a:
```
$env:GOPROXY='off'; $env:GOTOOLCHAIN='local'; $env:GOFLAGS='-mod=readonly'
```
- 依据:godb 任务单(主工作区版本)第 43 行用 GOPROXY=off;PROGRESS:7636 用 GOTOOLCHAIN=local 和 -mod=readonly。
- 缺模块就记「没跑起来」,不下载。

S5 docker 一次性探针的共用规则。用于 P0-A C 段、P0-C M0~M4、P0-E S2。
- 每条命令都带 --context desktop-linux(start_game.ps1:110-112);每条 docker run 都带 --pull never。P0-A 的 C 段两样都没带,要补上。
- 镜像只在 S0 盘点一次。缺哪个镜像,就停哪一项、去征得同意,不在步骤里隐式拉取。
- 判语义统一用 redis:7.2(K8s 版本,redis.yaml:83)。compose 里的 redis:latest(docker-compose.yml:218)只在 S3a 里记录版本。
- 名字和端口互不冲突,不碰 6379、2379,也不碰 godb 任务单用的 13307、14000:

| 项 | 容器名前缀 | 宿主端口 |
|---|---|---|
| P0-A | p0a-* | 6390、6391 |
| P0-C | etcd-p0c-* | 42379~42382 |
| P0-E | p0e-* | 不映射 |

- P0-C M3 有 420 秒等待,可以在这段时间穿插 P0-A C 段和 P0-E S2;为了输出好读,也可以顺序跑。
- 收尾时各项删除自己的容器,P0-C 还要删 etcd-p0c-vol 卷。

S6 必须分开跑、会互相干扰的步骤
- 开发 etcd:只读的 P0-C M1、P0-E S3a 必须先于任何重启。会重启开发 etcd 的有 P0-D M4/B4/B6 和 P0-C M6;执行时在跑的 C++ 节点都会被打进重注册循环。
- C++ 进程登记:P0-A B4(scene,-Zone 99)、P0-B 3-2(battle,经 dev_tools.ps1 转到 cpp_nodes.ps1,dev_tools.ps1:1449)、P0-D B2(gate)共用同一棵树的 run/pids/cpp_nodes.pid.json。
  - stop 按基名停掉所有 zone 的同名节点(cpp_nodes.ps1:449-458);
  - dev-stop 会停掉全部 Go 和 C++ 进程(dev_tools.ps1:1481-1486)。
  - 所以三段必须串行,每段开始前 status 为空。
- 会话环境变量:
  - P0-A B3 设的 GAME_CONFIG_PATH,battle 也会读(battle_room_manager.cpp:359);
  - P0-B 3-2 设的 RPC_PORT 也会泄漏给后续进程(start_game.ps1:552 的做法是起完置空)。
  - 每段结束都要 Remove-Item 这两个变量。
- 被跟踪的配置文件:P0-B 3-1 直接改 bin/etc/base_deploy_config.yaml。这个文件没有环境变量覆盖,路径写死在 node.cpp:1245。改动期间同一棵树起的任何节点都会带上 GroupID 和 LogLevel 1。必须等 3-4 恢复、restore_exit=0 之后,才能做 P0-D B。
- Kafka:C++ 节点首次生产时,未预建的 topic 会以 1 个分区自动创建,之后 data_service 会永久拒绝它(docker-compose.yml:66-81)。
  - P0-A B4、P0-B 步骤 3、P0-D B2 起节点前都要先预建。
  - 用 start_game.ps1:182-205 的 Initialize-LocalKafkaTopics,还是 compose 的 kafka-topic-init(:82-84),需 Codex 先确认。
  - 五项里只有 P0-D 写了这一条。
- 共享 Redis:P0-E S5 期间名额被占满,任何依赖共享 Redis 的步骤都会失败,要单独安排时段。
- kind:kind-mmorpg 存在期间,不要在宿主机起 C++ 节点,会和 20000/30000/30001 端口冲突(kind-config.yaml:46-50)。P0-E S6 和 P0-C S2 的 kubectl 一律带 --context kind-mmorpg 和 --kubeconfig。
- 整栈采样合并:P0-E S3b(空闲态)和 P0-C M5(两次快照,间隔至少 30 分钟)共用同一次起栈。M5 必须在 S4 机器人登录之前采完,因为登录会改变 revision/s。

S7 构建树(受阻段共用)
- 不在主工作区编译:那里有其他会话未完成的 C++ 改动,包括 node.cpp:1179、scene 的 redis.cpp、scene.vcxproj 新加的 enter_fence.h、entt 审计涉及的 10 余个文件。
- 也不在 f452、793e 里编译。
- 建议新建专用的构建工作树 $B,检出 2f897bd07。能否用主仓 .git/modules 作源离线初始化子模块,需 Codex 先确认;不能的话就属于下载。
- 三项的 C++ 红用例在同一次 /m:1 构建里编:P0-B 的 p0b-red.patch 加 kafka_manager_init_test.cpp,和 P0-D 的 etcd_stream_break_test.cpp。

### 执行顺序(已并入任务单 §4)

通用规则:每步保留原始输出。某项出现与期望不符的红就停下这一项,不连续重试、不换写法;其它彼此独立的项照常继续。只有环境类失败才整体停,例如 pwsh 低于 7.3、docker 不可用、回环被拦。全程不压测、不清库、不 push。

第 0 段 只读盘点(现在就能跑,约 5 分钟)
1. 按 shared_environment_setup 的 S0 执行。
2. 停止条件:
   - pwsh 低于 7.3:整体停下,等负责人决定是否授权安装。
   - 回环被拦:先申请放行,第 2、3 段暂停。

第 1 段 部署渲染与静态核对(现在就能跑;只在 $W 里;不连集群、不用 docker、不编译;约 30~60 分钟)
1. S1 建工作树;S2 准备会话。
2. P0-A A2:prod 档 zone-up,$envs 里加 MMORPG_LOGIN_QUEUE_TOKEN_SECRET='contract-test-queue-token-0123456789abcdef'。同一份输出顺带读 Kafka.GroupID,供 P0-B 用。
3. P0-B 1-2:dev 档 zone-up,外加 Get-EtcValue 读 bin/etc 的 GroupID。
4. infra-up 跑两次(带口令、不带口令):
   - P0-A A3 的第 7~11 行;
   - 从不带口令的那份输出读 P0-B 1-1 要的三个值。
5. P0-C S1、P0-E S1:git show / git grep 2f897bd07。
6. 全量契约测试跑一次,输出存 $out\contract-baseline.txt。
   - P0-D M3 只看三条用例(按名):NodeTTLSeconds、gate 就绪探针、scene 就绪探针。
   - 已知红只记录,见 gaps 第 1 条。
7. P0-E S2b:go list 读模块缓存源码,GOPROXY=off。
8. 负责人同意预写代码之后,才往 $W 拷入测试文件并跑红:
   - P0-D M2:P1 的 Go 模板用例;
   - P0-E R0:新建的 redis_maxclients_contract.tests.ps1;
   - P0-A 与 P0-B 的契约红用例:落在同一份 k8s_deploy_contract.tests.ps1 里,一起拷入,跑一次,按用例名判。
   - 每加一类文件就单独跑一次。判据是「以断言文本失败」;解析失败或参数错误记为「没跑起来」。
9. $W 保留到负责人确认结果,再 git worktree remove。

第 2 段 docker 一次性探针(现在就能跑;镜像缺失时该项停下、等同意;约 45 分钟)
1. 先跑 P0-C:M0 → M2 → M3。M3 内部先探针 B、B2,等 420 秒,再探针 A;B 或 B2 不通过就不做 A。然后 M4,最后清理。
2. 再跑 P0-A C0~C7,补上 --context 与 --pull never。
3. 再跑 P0-E S2.1~S2.4。
4. 第 2、3 步可以放进 P0-C M3 的 420 秒等待里做。三者只用各自的临时容器,不碰 compose 里的 redis 与 etcd。

第 3 段 只读采样(有条件:对应环境已经在跑,否则记「未取得」)
1. P0-C M1:开发 etcd 在跑时做。
2. P0-E S3a:开发栈在跑时做。
3. P0-D M1:只在已有的节点日志里 grep。
4. P0-C S2 与 P0-E S6:kind-mmorpg 已存在时做,命令必须显式带 context 和 kubeconfig。
5. 这一段必须排在任何破坏性步骤之前。

第 4 段 需要负责人同意、但不需要编译
1. P0-D M4(重启开发 etcd、观察租约):须同意,且确认没有 C++ 节点在跑。建议推迟,并入 P0-D 的 B4(B4 的「原租约仍在」就是同一判据)。
2. P0-C M5a(Go 开发栈采样):须「同意 2a」;且要 Codex 先确认缺 C++ 时 login 与 scene_manager 能起来。建议推迟到第 5 段整栈时一起做。

第 5 段 受阻:构建环境恢复之后再做
- 解除条件(全部满足):
  - 负责人授权(owner_decisions 第 1 条);
  - 在专用构建工作树 $B 里,Boost 扁平头、install_vs2026_dbg(含 hiredisd.lib、yaml-cppd.lib)、grpc 嵌套的 googletest 都就绪;
  - 步骤 0 / B0 的检查全为 True。
- 执行顺序:
  1. 一次 /m:1 构建,内容是 HEAD 加上两份红补丁(P0-B 的 kafka_manager_init_test,P0-D 的 etcd_stream_break_test)。
  2. 跑红:
     - P0-B 步骤 2:run_cpp_tests -Filter kafka_command;
     - P0-D A1:run_cpp_tests -Build -Filter rpc_controller_test。
     - 「没跑起来」(error C、LNK、缺 DLL)与「红」分开记。
  3. 先预建 Kafka topic(S6),再起整栈:
     - P0-E S3b 空闲采样与 P0-C M5 同时进行,两次快照间隔至少 30 分钟;
     - 然后 P0-E S4 机器人轻负载;
     - 负责人同意的话,接着做 P0-E S5,300 秒后自动恢复。
  4. 停整栈,确认 cpp_nodes status 为空,然后串行:
     - P0-A B2~B6:scene 对带口令的探针,加对照组;结束时 Remove-Item Env:GAME_CONFIG_PATH。
     - P0-B 3-1~3-4:battle;结束时恢复 bin/etc,restore_exit 应为 0;Remove-Item Env:RPC_PORT。
     - P0-D B0~B7:gate 加开发 etcd 重启;B6 可选。
     - P0-C M6:压缩并重启,与 P0-D 共用同一个 etcd 窗口。须「同意 3」。
  5. 最后整栈重启,让开发环境回到正常状态。
- P0-B 的 3-5(端到端冒烟)建议不做:它要求整栈运行在被改过的 base_deploy_config 上,与上面的串行要求冲突。

第 6 段 修复合入之后
- P0-D 的 K1(三成员 kind 验收),以及各项第二批的契约测试转绿,都按各自的单子执行。

受阻步骤汇总
| 步骤 | 解除条件 | 由谁解除 |
|---|---|---|
| P0-A B0~B6 | 第 5 段条件 | 负责人授权,Codex 修环境 |
| P0-B 步骤 2、3 | 同上;另需 Claude 先写好红补丁 | 同上 |
| P0-C M5、M6 的 C++ 部分 | 同上;另需同意 2b、同意 3 | 负责人 |
| P0-D A1、B0~B7 | 同上;B4、B6 另需同意重启开发 etcd | 负责人 |
| P0-E S3b、S4 | 同上;另需网关 jar(mvn 打包,可能下载) | 负责人 |
| P0-E S5 | 本机有在跑的栈,且负责人同意 | 负责人 |
| P0-D K1 | 修复合入后,单独授权 | 负责人 |

## P0-A C++ 节点连 Redis 不发 AUTH


### 结论

- 第一轮判定:`confirmed_static`。
- 在 `2f897bd07` 上是否仍成立:是。

**把握**:

high(仅指静态结论)。

依据:
- 证据链每一环都在 HEAD 上重读过。k8s_deploy.ps1 用 `git show HEAD:` 核对,避开了工作区里他人的改动。
- `zone_redis().password()` 和 `.db()` 的读取点经全 cpp/ grep 为 0。
- 建连入口只有 node.cpp:557 一处,建连和重连路径都不发任何认证命令。
- 部署模板里 C++ 的口令是字面量空串,五个 C++ 工作负载模板没有任何口令环境变量。
- 服务端 requirepass 与 Go 侧口令注入都有代码行对应。
- 两份对抗复核各自独立重读,都没有找到让缺陷不成立的保护。

不在 high 范围内的部分:
- 运行期的具体表现(错误文本、errorstats 计数、hiredis 回调时序)依赖外部组件行为,没有任何实测;本机当前也编不出二进制。
- 修复设计未编译。「握手连接隔离后调用点零改动」的依据是逐个读过调用点的判空写法。回调时序部分依赖 hiredis 行为,要靠 6 条回归用例和 6 组黑盒验收确认。
- Java 版的结论只到「口令已接线」,带口令的真 Redis 未实测。

对交接原文的三处偏差同样是 high:gate 不受影响;缺陷不限于 release 档;冷启动下 scene 是进场全败,不是存盘堆积。

### HEAD 前进后的更正(0aa1027bc → 2f897bd07)

【结论复核】以下每一跳都用 `git show 2f897bd07:<文件>` 或 `git grep … 2f897bd07` 重读过。核对途中主工作区出现了他人在途改动,所以没有用工作区文件作证据。整条证据链在 2f897bd07 上仍然成立:
- 口令解析:k8s_deploy.ps1:502-503。dev 档非空即采用:release_common.ps1:690-692。
- 服务端开 requirepass:k8s_deploy.ps1:6497-6514 建 Secret redis-auth;redis.yaml:94-95 带 --requirepass,:98-104 读 Secret。
- C++ 配置口令是字面量空串:k8s_deploy.ps1:2298-2301,位于 New-NodeConfigMapYaml(:2109)。
- C++ 工作负载没有任何口令 env:k8s_deploy.ps1:2484-2493;lib/k8s_client_entry.ps1:669-683。
- Go 服务拿到口令:k8s_deploy.ps1:3859、:4001、:4638。
- C++ 读入但不用:config.cpp:297 写入;cpp/ 下 `.password()` 读取点为 0。
- 唯一建连点:node.cpp:557。建连与重连都不认证:redis_manager.cpp:44-46、:97-104、:141-157。
- 生产路径 AUTH / SELECT 为 0,只在 mrediscli 示例和死测试里出现。
- gate 源码无 Redis 命令;battle RedisReady 在 battle_room_manager.cpp:205-209;scene 进场失败在 player_lifecycle.cpp:1935、:1944。

【0aa1027bc..2f897bd07 中与本项有关的改动】
1. tools/scripts/k8s_deploy.ps1(e4ff1d165,+794 行:G2b 连接总账、G4b 削峰与 gateway Redis 口令、文案英文化)
   - 影响结论吗:不影响。node-config 仍写 `password: ""`(:2301);C++ 模板仍无口令 env。New-NodeConfigMapYaml 新增的 -IncludeBattleSettings 只加 battle 票据密钥(:2148-2154)。
   - 影响实测步骤吗:影响。全部行号漂移(对照表见 stale_claims)。A2 / A3 的参数、函数名、判据文本仍然存在。另多出一条已知红:脚本 :1238 现打 `[MySQLConnectionLedger]OverLimit:`,契约测试仍断言旧中文。
   - 影响修复设计吗:影响。G4b 已把 gateway 的 `spring.data.redis.password` 写进 ConfigMap(:5562-5568、:5626),并在限流开启时把 Redis 口令纳入 `mmorpg.io/peak-shaving-hash` 指纹(:5697-5713)。注释写明「改走 Secret 不缩小暴露面」(:5556-5561)。这给方案 A/B 增加了一个仓内先例。Add-PodTemplateAnnotation 新增 -AdditionalAnnotations(:5225),但 5 个 C++ 模板仍自己拼 sidecar 注解(:2354、:2936、lib :740),第二批仍要自己改。「等 G2b/G4b 提交」这个前提已满足。
2. tools/scripts/lib/k8s_client_entry.ps1(e4ff1d165,+97):只改 New-GatewayRateLimitYaml 和预检警告文案。C++ 相关函数行号不变(:646、:669-683、:701、:740、:764、:988、:1108)。三问都不影响。
3. tools/scripts/tests/k8s_deploy_contract.tests.ps1(e4ff1d165,+309)
   - 结论:不影响。仍没有任何 C++ Redis 口令断言。
   - 实测:影响。prod 段移到 :975-1001,battle infra 段移到 :1149-1151;新增的 G2b 用例 :1649 在 :1656 断言旧中文 `MySQL连接总账超限`,HEAD 上必红。
   - 修复设计:影响契约用例的插入位置,改为 :1001 之后、:1151 之后。
4. tools/scripts/tests/k8s_client_entry_contract.tests.ps1(+46):带口令的用例仍在 :1447。不影响。
5. deploy/k8s/manifests/infra/redis-ha.yaml(e4ff1d165):只有 echo 文案英文化。健康检查 AUTH 仍在 :503-514,代理不替客户端认证;脚本仍无 -RedisReplicas。不影响。
6. battle_room_manager.cpp、player_scene.cpp、player_team.cpp、player_lifecycle.cpp:4203(e4ff1d165,日志英文化):全部等行替换,行号不变。不影响结论与修复。只有顺带发现里的中文日志「battle 结算已销账」现为 `battle settlement acknowledged`(:1787)。
7. cpp/nodes/battle 的 main.cpp、handler/grpc/battle_node.*、handler/rpc/battle_handler.cpp,以及 protogen 模板(2f897bd07 等):无 Redis 改动。不影响。
8. cpp/nodes/scene/main.cpp:只改一行文案,:350 依赖闸不变。tools/scripts/release_preflight.ps1:只改末尾文案,:297-299 不变。不影响。
9. tools/scripts/run_cpp_tests.ps1:提示改英文(:72、:196、:199)。表内 `N/M 全过`(:188)未变。只影响修复设计第 10 条的汇总文字。
10. 装备生成物(0d34dcf8d / 898c803ea / 0b40715d0 / 6e212616b):equipaffixpool_table.h 等已入库,item_base_comp.pb.h 已有 has_equip,player_equip.cpp 的 include 移到 :26。不影响结论。影响实测前置:B0 不再查它,原「110 errors」改为未核实。
11. 用 git diff 核对过、在区间内没有改动的文件:redis_manager.{h,cpp}、node.cpp、config.cpp、redis_client.h、scene redis.{h,cpp}、两份 Hiredis、scene_handler.cpp、player_battle.cpp、bin/etc/*、redis.yaml、docker-compose.yml、release_common.ps1、cpp_nodes.ps1、dev_tools.ps1、deploy_capture.ps1、test_harness.ps1、rpc_controller_test、configuration_table_test、config.proto、shared-redis-split-by-purpose.md、cpp/AGENTS.md、deploy/k8s/AGENTS.md、Dockerfile.cpp。

【非提交的环境变化,影响实测分组与第二批时机】
- third_party/redis 已检出(Redis 8.4.0,可读 deps/hiredis/async.c 与 src/*.c);third_party/yaml-cpp 已检出。
- third_party/boost 是模块布局:有 libs/any/include/boost/any.hpp,没有 third_party/boost/boost/。
- lib\ 只有一个被 .gitignore 忽略的 battle.pdb。
- Java 仓 HEAD 从 d942f74 变为 52cccf2。引用的三个 Java 源文件未改,只有 PARITY.md +5 行。
- Codex 793e worktree 里四个部署文件与 HEAD 逐字节相同。
- 2026-10-14 本次核对时,主工作区出现他人在途改动 71 处(不是「只剩两个子模块脏标记」)。其中 k8s_deploy.ps1 +7 行、k8s_deploy_contract.tests.ps1 +424 行(G4b 评审修复:常量表自检、削峰契约用例),核对过程中还在增长。另有 PROGRESS.md、shared-redis-split-by-purpose.md、go/db/etc/db.yaml、player_lifecycle.cpp。这些不影响结论,但实测不能在主工作区跑 A 段,第二批也要等它们提交。

**失效的说法**:

【confidence / claim_evidence】
1. 「k8s_deploy.ps1 用 git show HEAD: 核对,避开他人改动」「HEAD 0aa1027bc」:HEAD 改为 2f897bd07。主工作区又有他人在途改动(见 head_delta 末段),所以仍须用 `git show 2f897bd07:` 或新工作树核对。
2. 「本机编不出二进制」仍成立,但原因换了:Boost 模块布局导致 boost/any.hpp 报 C1083(PROGRESS:7637、:7645),加上 install_vs2026_dbg 缺失。
3. k8s_deploy.ps1 行号对照(旧→新;:24 与 :37-38 未变):
   - 密钥解析:Initialize-InjectedSecrets :431→:481;MMORPG_REDIS_PASSWORD :452-453→:502-503。
   - 服务端 Secret 段 :5809-5826→:6497-6514。其中删除 :5810→:6498,模板 :5816-5824→:6504-6512,name :5820→:6508,password :5823→:6511。「Secret 先于 redis.yaml」:5807-5808→:6495-6496;「DryRun 会打口令」:5815→:6501-6503。
   - New-NodeConfigMapYaml :1677→:2109(整函数 :1677-1887→:2109-2319)。zoneredis 段 :1863-1874→:2295-2306,`zoneredis:` :1866→:2298,`password: ""` :1869→:2301,game_config.yaml 键 :1884→:2316,遮蔽注释 :1685-1687→:2117-2119,GateTokenSecret 保护 :1708-1714→:2140-2146,LogLevel :1790→:2222。
   - zone node-config 生成 :5355→:6043(名字在 :6013)。battle-node-config :5748-5749→:6436-6437(Apply-Infra,BattleReplicas>0 才生成);battle 写入 infra namespace :5740→:6402。
   - New-NodeDeploymentYaml :1889→:2321,env :2052-2061→:2484-2493,挂载 :2064→:2496。New-SceneFleetYaml :2473→:2905,挂载 :2604→:3036。sidecar 注解 :1922 / :2504→:2354 / :2936。
   - Add-GoSvcSecretEnv :4596→:5161,其 secretKeyRef 行 :4638→:5203,optional 理由 :4594→:5159。Add-PodTemplateAnnotation :4656→:5225。Get-InjectedSecretFingerprint :4719→:5316。Apply-InjectedSecret :4747→:5344,DryRun 隐值 :4759-4762→:5357-5360。
   - Go 口令 :3343→:3859、:3467→:4001(db 段 :3464-3467→:3998-4001)、:4073→:4638。「DB 号全仓一致」:4065→:4630;「客户端带口令 / 服务端无口令」注释 :4068-4069→:4633-4634。
   - DryRun 打印:Invoke-Kubectl :1603-1606→:2035-2037;Invoke-KubectlWithInputFile 全文 :1627-1636→:2059-2068。
   - lib/k8s_client_entry.ps1、release_common.ps1、redis.yaml、release_preflight.ps1:297-299 和全部 C++ 行号:未变。
4. E8「redis-ha.yaml:505-507 本次未重读」:已重读,AUTH 只在 haproxy 健康检查 :503-514 发出。

【handoff_corrections】
5. 第 1、4、9 条的行号:按第 3 条改。「:452-453 未漂移」作废。
6. 第 10 条「G4b 把 Redis 口令纳入 Pod 指纹」:已落到 HEAD(:5697-5713),但只在限流开启时带(:5705)。
7. 第 11 条「java/gateway_node 的 ConfigMap 缺 spring.data.redis.password(:4949-4952)」:已由 G4b(e4ff1d165)修复,见 :5556-5568、:5626,口令非空才写。
8. 第 13 条整条过期:
   - 子模块已初始化 14/19,未初始化的是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash。
   - 有 lib\,但只有一个被忽略的 battle.pdb。
   - 装备生成物已入库;player_equip.cpp 的 include 在 :26。「110 errors / 装备表缺失」改为未核实。
   - 现知阻塞是 boost/any.hpp 的 C1083 加 install_vs2026_dbg 缺失。PROGRESS:7005、:7051 只是历史记录。
9. 第 14 条:
   - 「15 改 5 新,k8s_deploy.ps1 +431、契约测试 +301 在途」:那批已以 +794 / +309 随 e4ff1d165 入库。但当前又有新的在途改动(+7 / +424)。
   - Codex 有 Go 1.27.2,有 VS 2026 18.10.3 / MSBuild 18.10.1(C:\Program Files\Microsoft Visual Studio\18\Enterprise)。pwsh 在 Codex 的 MSBuild 进程里被调起过,版本未记录。
   - f452 仍有 8 个未提交文档;另有 793e(0aa1027bc,303 处未提交)。
10. 本任务单前提「主工作区只剩两个子模块脏标记」:在本次核对时已不成立(71 处在途)。

【entry_point_inventory】
11. 测试行号:prod DryRun :922-944→:975-1001;battle infra :1096-1098→:1149-1151;:33-47 与 client_entry :1447 未变。
12. 「third_party/redis 未检出,读不到 async.c」:已检出(Redis 8.4.0),静态核到:
    - 建连回调前已置 REDIS_CONNECTED(async.c:694-695);
    - 回调栈内 free 延后(:415-423);
    - 释放时挂起回调收空应答(:368-372);
    - REDIS_REPLY_ERROR=6(read.h:56)。
    链接用的 hiredisd.lib 是否同一版本:未核实。
13. run_cpp_tests 的汇总:表内 `N/M 全过`(:188)未变,末行改为 `All passed (N projects)`(:199)/`N test projects failed`(:196)。
14. 原报告的小错(与 HEAD 无关):compose 的 image 在 docker-compose.yml:217,:218 是 container_name。
15. 顺带发现里的「battle 结算已销账」现为 `battle settlement acknowledged`(:1787)。

【codex_measurement】
16. 0.2 检出 0aa1027bc / 期望 0aa1027bc901…:改为 2f897bd07 / `2f897bd0780c1cc78284548284f20d01e61d7ea8`。「主仓工作区有另一会话改动」仍成立,但内容已换。
17. A1「HEAD 是否全绿没有证据」:已知至少一条红(:1649 / :1656 对脚本 :1238)。
18. A2 / A3 出处:按第 3、11 条改。命令与判据文本仍成立。
19. B0 整段过期:「期望未初始化数 0 / 现状 19」「grpc 是空目录」「库里只有 equipslot_table.*」以及 player_equip.cpp:19 都已不适用。新 B0 见 measurement_final。
20. B3「yaml-cpp 未检出,LoadFile 能否读绝对路径需确认」:源码已核,yaml-cpp/src/parse.cpp:32-38 用 std::ifstream。运行期以 node.cpp:1255 的日志行为准。
21. B2 与「需 Codex 实测确认」清单中的几条外部常识现在都有源码出处:NOAUTH 文本(server.c:2077)、errorstats 格式(:6555)、向无口令 Redis 发 AUTH 报错(acl.c:3255-3257)、WRONGPASS 不回显(acl.c:1479)、类型号 6、三条 hiredis 行为。运行镜像版本仍以 C 段实测为准。

【fix_design】
22. 「第二批等 G2b/G4b 提交」「k8s_deploy.ps1 和契约测试有另一会话未提交改动」:G2b/G4b 已提交,但现在又有新在途改动(G4b 评审修复)。
23. 第 6 条的行号:
    - Apply-InjectedSecret :4747→:5344;
    - 仿 :5809-5811→:6497-6499;
    - :4594→:5159;
    - 指纹 :4719→:5316;
    - :1922、:2504→:2354、:2936;
    - :1708-1714→:2140-2146;
    - :4068-4069→:4633-4634;
    - :4065→:4630;
    - :5807-5808→:6495-6496;
    - :5740→:6402。
24. 第 8(c) 条契约用例插入位置:改为 :1001 之后、:1151 之后。
25. 第 10 条第 1 步:「报错只在 bag_marshal.cpp / equipaffixpool_table.h 是既有阻塞」改为「C1083 boost/any.hpp(PROGRESS:7637)与缺 install_vs2026_dbg 的链接错误」。
26. 第 10 条第 5 步:「Codex 若没有 Go」改为 Codex 有 Go 1.27.2,但该步仍依赖 C++ 节点。
27. 风险节「G4b 要给 gateway 写口令;本项不替它做」:已完成。它的指纹写法用的是 Add-PodTemplateAnnotation 的 -AdditionalAnnotations(:5710-5713),C++ 模板不能直接复用。

【java_parity】
28. Java HEAD d942f74 改为 52cccf2,引用的源文件不变。PARITY.md 状态取值行 :23 改为 :25(复核写的 :22 同样过期)。PARITY 里仍无本项条目。

【risks_open_questions / changes_from_review】
29. 第 1 条两个阻塞描述过期(见第 8 条)。第 2 条「等 G2b/G4b」已满足,但又有新在途改动。「重叠」节里「G2b 在途改写」与「G4b 要写 gateway 口令」都过期。「PROGRESS 有他人未提交追加」仍成立,但内容已换。
30. 「HEAD 契约测试是否全绿」:已知至少一条红。
31. 复核一的环境事实(19 行全 `-`;grpc、redis 是空目录;没有 lib\)与复核二的「装备生成物阻塞」:均过期。

### 受阻步骤与先写的测试

受阻:B0~B6(运行期 Q-B),以及修复后的全部 C++ 验证(fix_design 第 10 条第 1、2、4、5 步)。

阻塞原因:
1. Boost 是模块布局,而 vcxproj 只把 third_party/boost/ 根加进 include。muduo 的 EventLoop.h:18(boost/any.hpp)和 Timestamp.h:12(boost/operators.hpp)因此报 C1083(PROGRESS:7637)。归档恢复脚本要的分卷已随历史重写删除,本机也没有 Python。
2. third_party/grpc/install_vs2026_dbg 不存在。三个节点和 rpc_controller_test / configuration_table_test 链接的 hiredisd.lib、yaml-cppd.lib、gRPC / absl / protobuf 库全部缺失,运行期需要的 zlibd.dll 也在这里。
3. bin\ 下没有 scene.exe。
4. 5 个未初始化子模块(cppcodec、fmt、muduo-linux、recastnavigation、xxhash)是否被 Windows 构建需要,待确认。
5. 上述解除后主干其余部分能否编过,未核实。
6. 编译树未定:新工作树没有子模块(初始化即下载);主工作区当前有 71 处他人在途改动,编出来的不是纯 HEAD 或纯修复版。

解除条件:B0 前三项都为 True,且 B1 全量构建 `/m:1` 退出码为 0、产出 bin\scene.exe。

由谁解除:
- 负责人拍板授权。装 / 升 cmake、ninja,在 third_party/boost 生成扁平头或改 vcxproj include,build_grpc.ps1 加 yaml-cpp / hiredis 的全量构建,初始化子模块,都属 AGENTS.md §10.2 的停止条件。
- 授权后由 Codex 修环境并构建,具体命令需 Codex 先确认。Claude 不改构建环境。

建议:B 段推迟到第一批落码之后,环境修好后做一次构建,同时取红态(黑盒第 0 组)和绿态。

**实测前要先写的代码**:

实测本身(步骤 0、A、C、B)不需要 Claude 先写任何代码:A、C 段用现成脚本与 docker,B 段用现成二进制与 cpp_nodes.ps1。

【可选一项(建议):修复前必红的部署侧回归用例】
- 文件:D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/k8s_deploy_contract.tests.ps1。改,不新建。
- 插入位置(按 HEAD 行号):
  1. prod 段用例 :999-1001 之后加一条,复用 $prodRun,不新起子进程;
  2. battle 段 :1151 之后新起一次 `Invoke-DeployDryRun -Arguments $BattleInfraArgs -Env ($BattleTestEnv + @{ MMORPG_REDIS_PASSWORD = 'contract-test-redis-pw-01' })`,再加一条;
  3. dev 基线($devOut,不注入口令)加一条对照。
- 内容要点(对方案 A / B 中立):
  1. prod zone-up 注入口令时,下面两种至少满足一种,否则失败:
     - node-config 的 `data.game_config.yaml.zoneredis.password` 等于注入值;
     - 每个挂 /app/bin/etc 的块都有一条 valueFrom.secretKeyRef 的口令环境变量。
     筛出的块数为 0 时直接判失败(夹具前提)。
  2. 带口令的 infra-up:battle-node-config 按同样规则判定,或者 battle 工作负载块带 secretKeyRef。
  3. 对照:dev 基线不注入口令时,node-config 口令为空串,C++ 块里没有 secretKeyRef。这条修复前后都绿,用来钉住空口令行为不变。
  - 断言用 Assert-True / Assert-Equal 并写 -Because;不写反斜杠路径(CI 在 ubuntu 上跑)。
- 期望:修复前 1、2 红,3 绿;第二批落码后全绿。该文件在 HEAD 已有一条无关红(:1649),所以按用例名判,不按退出码判。
- 要不要同步 CMakeLists / vcxproj / sln:不需要,这是 PowerShell。
- 为什么属于实测而不是修复:它把 A2 / A3 的一次性渲染固化成能稳定复现缺陷的用例。它不改任何产品行为,修复前由 Codex 用 pwsh 7 就能跑出红,符合 AGENTS.md §11.4「先写复现问题的回归测试」。真正的修复(模板改动)在第二批。
- 前提:主工作区里这个文件当前有他人在途改动(+424 行),要等其提交后再写,避免冲突。这一点需负责人同意(见 owner_decisions 第 6 条)。

【C++ 侧:无】理由:
1. 修复前 RedisManager 的接口不接收口令,fix_design 8(a) 的 6 条认证用例修复前无法编译。
2. configuration_table_test 的 ZoneRedisPasswordFromEnvOverridesYaml 修复前可编译、运行期红,但它测的是新增的环境变量覆盖,属于修复第一批本身。
3. 二者以及任何「独立 GTest」都绕不开 B0 的阻塞:
   - RedisManager 经 muduo EventLoop.h:18 需要 boost/any.hpp;
   - config.cpp 经 muduo/base/Logging.h 到 Timestamp.h:12,需要 boost/operators.hpp;
   - 两个测试工程还链接 install_vs2026_dbg。
   Codex 的 BattleAdmissionGate 独立 GTest(gtest 取自 third_party/yaml-cpp/test/googletest-1.16.0)能跑通,推断是因为它不经过 muduo,需 Codex 先确认。所以本项不建议另建独立 GTest 工程。

### 待拍板

1. 落码前实测做到哪一步。
   - 建议:现在只跑步骤 0、A2、A3 和 C 段(A1 可选)。把「A 段渲染 + C 段外部前提 + 静态证据」作为第一批落码前的核实。
   - 运行期红 / 绿等环境就绪后,在修复后的同一次构建里一起取,黑盒第 0 组就是修复前的表现。

2. 构建环境修复授权。这影响所有 C++ 验证,不只本项。
   (a) Boost 扁平头,三个选项:
       - i:在 third_party/boost 生成扁平的 boost/ 头目录。只改子模块工作区,产物不入库,具体命令需 Codex 先确认(可能要先构建 b2)。
       - ii:把各 vcxproj 的 include 改成模块路径。仓库改动面大,应另立项。
       - iii:恢复归档源码。不可行,分卷已随历史重写删除。
       建议选 i,作为跨项环境任务另起一项,由 Codex 在授权后执行并写入 PROGRESS。
   (b) install_vs2026_dbg:用 build_grpc.ps1 构建 Debug(可能经 winget 装 / 升 cmake、ninja,耗时长),再构建 yaml-cpp 与 hiredis 的 Debug。建议授权,但排在第一批落码之后做一次。
   (c) 5 个未初始化子模块:建议先让 Codex 只读确认 Windows 构建是否需要,再决定是否初始化(下载)。
   (d) B 段用哪棵树编译:
       - 新工作树:需初始化子模块,即下载;
       - 主工作区:子模块齐,但当前有他人在途改动,编出来的不是纯 HEAD 或纯修复版。
       建议等主工作区在途改动提交后用主工作区,或者经授权后在新工作树初始化子模块。

3. 下载 Redis 镜像:仅当本机没有任何 redis 镜像时需要。建议同意,这是 C 段代价最小的一步。

4. 第二批(k8s_deploy.ps1、k8s_client_entry.ps1、两份契约测试)何时放行。
   - G2b / G4b 已随 e4ff1d165 入库,原条件满足。
   - 但 10-14 本次核对时,主工作区这两个文件又有他人在途改动(G4b 评审修复,+7 / +424 行,仍在增长)。
   - 建议等这批改动提交后再放行;落码时按函数名定位,不得 `git add -A`。

5. 第二批口令来源选方案 A 还是方案 B。
   - HEAD 有新先例:G4b 给 gateway 走「明文进 ConfigMap + 指纹注解」,并写明「改走 Secret 不缩小暴露面」(k8s_deploy.ps1:5556-5561、:5697-5713)。同 namespace(包括 infra)的 Go ConfigMap 也是明文。
   - 建议改选方案 A:明文进 node-config,加非 dev 档空口令 / 非法字符的生成期 throw,并给 5 个 C++ 模板加口令指纹注解。与仓内现行做法一致,改动面小。
   - 原报告倾向方案 B,这里因为新先例改了建议;负责人若坚持「密码走 Secret」的方向,就取方案 B。
   - 两种方案都要做指纹注解。

6. 是否先写那条方案无关、修复前必红的部署契约用例(见 pre_measure_code)。建议同意,时机同第 4 条。

7. 方向确认(shared-redis-split-by-purpose.md:1025):给 RedisManager 加 AUTH,还是不开 requirepass、改用 NetworkPolicy。建议加 AUTH:release 档已强制口令,Go 与 Java 也已接线。

8. `zoneredis.db != 0` 时是否 LOG_FATAL。这是行为变更。建议同意:所有模板都是 0,跨运行时契约把库号钉在 0,启动期配置错误应当场失败。

9. 是否另起一项给 scene / battle 加「zone Redis 已认证」的就绪条件(现成接缝 cpp/nodes/scene/main.cpp:350)。建议另起,不进本项最小修复。

10. 已知的契约测试红(:1649 / :1656 断言旧中文,脚本 :1238 已改英文)不在本项内。建议由正在改该文件的在途会话顺手修,或另起一项。

11. PARITY 状态用词。建议落码后登记 `mmorpg 待做`,验证通过后改 `已对齐`。

### 原报告:证据链

【结论】在 HEAD 0aa1027bc 上静态成立,两份复核都没能推翻:C++ 节点建连和重连都不发 AUTH;注入了口令的部署会让共享 Redis 带 requirepass 启动;K8s 给 C++ 的 zoneredis.password 是字面量空串,C++ 工作负载也没有任何口令环境变量。
需要收窄两处:
- 受影响的是 scene 和 battle;gate 建连但从不发 Redis 命令。
- 触发条件是「部署注入了口令」,不限于 staging / prod。
运行期表现没有任何实测。本机当前编不出 C++ 二进制(见 codex_measurement 的 B0)。k8s_deploy.ps1 的行号一律取自 `git show HEAD:`,因为工作区里有他人未提交的改动。

E1 档位与强制口令(代码事实)
- tools/scripts/k8s_deploy.ps1:37-38 `[ValidateSet("dev", "staging", "prod")]` / `[string]$ReleaseProfile = "dev",`。「release 档」即 `-ReleaseProfile staging|prod`。
- 同文件 :452-453(函数 Initialize-InjectedSecrets,:431)`$script:RedisPassword = Resolve-InjectedSecret -EnvName "MMORPG_REDIS_PASSWORD" ` / `-DevFallback "" -ReleaseProfile $ReleaseProfile -Purpose "Redis 密码" -MinLength 12`。
- tools/scripts/lib/release_common.ps1:695-703:非 dev 档,变量为空、是占位串或长度不足都 throw。
- 同文件 :690-692 `if ($ReleaseProfile -eq 'dev') {` / `if ([string]::IsNullOrWhiteSpace($value)) { return $DevFallback }` / `return $value`。dev 档变量非空就原样采用,不查长度。

E2 服务端开 requirepass(代码事实)
- k8s_deploy.ps1:5809-5826:口令为空则 `delete secret redis-auth`(:5810),否则在 $InfraNamespace 建 Secret(:5820 `name: redis-auth`,:5823 `password: "$($script:RedisPassword)"`)。
- deploy/k8s/manifests/infra/redis.yaml:94-95 `if [ -n "$REDIS_PASSWORD" ]; then` / `exec redis-server --appendonly yes --requirepass "$REDIS_PASSWORD"`。
- 同文件 :98-104:env 来自 secretKeyRef(`name: redis-auth`,`optional: true`)。

E3 C++ 的配置和工作负载都没有口令来源(代码事实)
- k8s_deploy.ps1:1866-1869(函数 New-NodeConfigMapYaml,:1677)`zoneredis:` / `  host: "redis.${InfraNamespace}"` / `  password: ""`(中间的 port 行略)。这是字面量,没有引用 $script:RedisPassword。
- 同一函数产出 zone 的 node-config(:5355)和 battle-node-config(:5748-5749)。ConfigMap 以 readOnly 整目录挂到 /app/bin/etc(:2062-2065),遮蔽镜像内的配置(:1685-1687 注释)。
- C++ 工作负载的 env:
  - New-NodeDeploymentYaml(:2052-2061)只有 POD_IP / RPC_PORT / NODE_PORT 和 gRPC 段;
  - lib/k8s_client_entry.ps1:669-683 的 Get-ClientEntryNodeBaseEnv 只有 POD_IP / RPC_PORT / GRPC_SERVER_MAX_POLLERS / SNOWFLAKE_CACHE_DIR;
  - 生成器里 secretKeyRef 只出现在 Add-GoSvcSecretEnv(:4638)。
- 对照:Go 服务拿到明文口令。:3343 `$redisPassword = $script:RedisPassword`;:3467 `Password: "${redisPassword}"`;:4073 `Pass: "${redisPassword}"`。

E4 C++ 读进配置但从不使用(代码事实)
- cpp/libs/engine/config/config.cpp:297 `gameConfig.mutable_zone_redis()->set_password(zoneredisNode["password"].as<std::string>());`,:301 同形写 db。
- 在 cpp/ 下 grep `zone_redis()`:只有 config.cpp:289 / 293 / 297 / 301 的写入,和 node.cpp:541 / 542 / 1029 对 host / port 的读取。`.password()` 和 `.db()` 的读取点为 0。

E5 建连与重连都不认证(代码事实)
- cpp/libs/engine/core/node/system/node/node.cpp:541-557,末行 `tlsRedis.Connect(eventLoop, zoneRedisAddress);`。所有节点类型都执行。
- cpp/libs/engine/thread_context/redis_manager.cpp:44-46 `zoneRedis_ = std::make_unique<HiredisPtr::element_type>(loop, redisAddr_);` / `InstallCallbacks();` / `zoneRedis_->connect();`。
- 连接回调 :97-104 在 status 为 OK 时直接调 `reconnectCb_()`。重连 DoReconnect(:154-156)同形。
- cpp/libs/engine/muduo_windows/src/contrib/hiredis/Hiredis.cc:53 `context_ = ::redisAsyncConnect(serverAddr_.toIp().c_str(), serverAddr_.port());`。Hiredis.h:34-50 的公共接口只有 connect / disconnect / command / ping。
- 在 cpp/ 下 grep `"auth ` 和 `"select `:只命中示例与死测试(mrediscli.cc:133、:136;cpp/tests/mrediscli_test/mredisclitest.cpp:223、:226)。生产路径 0 处。

E6 没有任何启动期或探针保护(代码事实,末条是推断)
- 启动 banner 只打 host:port(node.cpp:1028-1029)。
- scene 的启动依赖闸只有号段一项:cpp/nodes/scene/main.cpp:350 `context->dependencyGate.AddCondition("id segments ready", []`。
- 发布预检 tools/scripts/release_preflight.ps1:297-299 只查 Go 三个服务的 Redis 口令。
- 推断:被拒时 Pod 仍 Ready,故障只在日志和客户端提示里可见。这是「探针不可见」,不是静默。

E7 后果(代码事实;被拒时的错误文本属外部常识,未实测)
- scene 冷启动(在带口令的 Redis 上从零起服):
  - 首次连上即经重连回调发 SCRIPT LOAD(cpp/libs/services/scene/core/system/redis.cpp:28-49 → cpp/libs/engine/infra/storage/redis_client/redis_client.h:297-302、:561-562),被拒只打日志(:570-571)。
  - 每次进场先清继承标记再载入(cpp/nodes/scene/handler/rpc/scene_handler.cpp:275、:277)。GET 被拒走 redis_client.h:810-820,再到 player_lifecycle.cpp:1935(LOG_ERROR)和 :1944(推 kEnterSceneFailed)。
  - 实体在载入成功后才建,所以冷启动下进场 100% 失败,没有存盘可发。不会被当成新号,只有 NIL 重试耗尽才是 DataNotFound(redis_client.h:823-841)。
- scene 过渡场景(节点上已有在线玩家,Redis 带新口令重启):断线重连后不认证,存盘走 redis_client.h:716-739 的 ERROR 分支。6 次后打 :679 和 player_lifecycle.cpp:1960 的告警,保留最新值按封顶退避无限重试。
- battle:RedisReady 只看 connected()(cpp/nodes/battle/logic/battle_room_manager.cpp:205-209)。结算 EVAL 被拒后降级为「未落库直接投递」(:1698-1705)。
- gate:cpp/nodes/gate 源码里只有一行 include(rpc_replies/route_message_response_handler.cpp:7),没有任何 Redis 命令。功能不受影响。
- 定级:带口令的这套配置从未真跑过(docs/PROGRESS.md:5401「说明这套 staging/prod 配置从未被真正跑过」)。这是开服阻塞项,不是线上事故。

E8 专门找过、没有找到的保护
- 部署脚本没有「带口令时拒绝部署 C++」的闸。
- C++ 的五个工作负载模板只有日志 sidecar 的哈希注解(k8s_deploy.ps1:1922、:2504;lib/k8s_client_entry.ps1:740),没有 node-config 或口令指纹。
- redis-ha 的代理不替客户端认证(复核一核到 redis-ha.yaml:505-507 只在健康检查里发 AUTH;该行号本次未重读)。

### 原报告:对交接的更正

1. 行号漂移(以 HEAD 为准):
   - `k8s_deploy.ps1:1862-1872` 应为 :1863-1874(`zoneredis:` 在 :1866,`password: ""` 在 :1869);
   - `node.cpp:540-555` 应为 :541-557;
   - `redis.yaml:93-96` 应为 command :90-97(requirepass 在 :94-95)、env :98-104;
   - `:452-453` 未漂移。
2. 触发条件说窄了。「release 档强制开密码」只对 staging / prod 成立;dev 档只要设了 MMORPG_REDIS_PASSWORD(任意长度)同样建 Secret、开 requirepass(release_common.ps1:690-692)。应改为「凡注入了口令的部署都出现」。
3. 影响面说宽了。「gate / scene / battle 全部 NOAUTH」不准确:
   - gate 建连但从不发 Redis 命令,功能不受影响;
   - battle 只在结算等路径受影响,表现为降级;
   - scene 冷启动下进场 100% 失败(有 ERROR 日志和客户端提示,Pod 仍 Ready);存盘堆积只出现在「已有在线玩家、Redis 带新口令重启」的过渡场景。
4. zoneredis 不在 base_deploy_config.yaml 里。它在 ConfigMap 的 `game_config.yaml` 键(k8s_deploy.ps1:1863-1874、:1884),由 readGameConfig 读(config.cpp:283-306)。任务单里「base_deploy_config 里的 zoneredis.password」的说法不准。
5. 漏项(修复时必须同批处理):`bin/etc/game_config.yaml:25` 和 `bin/etc/game_config_instance.yaml:17` 写着占位值 `password: "yourpassword"`。现在没人读所以无害;C++ 一旦真发 AUTH,本机无口令 Redis 会拒绝它,本机开发栈全挂。
   - 该行为是外部常识,未实测。
   - 仓库内有两处同样的表述:redis.yaml:87、k8s_deploy.ps1:4069。
6. 漏项:`zoneredis.db` 同样读了不用(config.cpp:299-302),C++ 也不发 SELECT。所有模板都是 db: 0,目前无表象。
7. 漏项:`RedisManager::SetupReconnect`(redis_manager.h:42、.cpp:63-86)是零调用方的第二条建连入口,补 AUTH 时要一并删掉。
8. 「建议:重连后先 AUTH 再补发」不够。握手完成前这条连接不能对业务可见:scene 有一个每秒冲刷积压的定时器,只看 connected()(redis.cpp:55-63 → redis_client.h:328-338),不经重连回调。只推迟「补发」挡不住它。
9. 「密码走 Secret」与现状不符。HEAD 上 Go 服务的 Redis 口令都是明文进 ConfigMap(k8s_deploy.ps1:3467、:4073)。唯一的 Secret 是 infra namespace 的 redis-auth,只给 Redis 服务端用。battle 也部署在 infra namespace(:5740、:5748-5749),可以直接引用它;gate / scene 在 zone namespace,要另建。
10. 漏项:「开口令」这一步不会滚动 C++ Pod。五个 C++ 模板没有配置或口令指纹(E8),而 C++ 只在启动时读配置。只改 ConfigMap 或 Secret 后,在跑的节点仍以旧口令(或无口令)连接。先例:G4b 为 gateway 把 Redis 口令纳入了 Pod 指纹(docs/handoff/launch-readiness-20261013/spec_G4b.md:449-450)。
11. 同类缺陷未在这一行登记:本仓 `java/gateway_node` 的 K8s ConfigMap 没写 `spring.data.redis.password`(k8s_deploy.ps1:4949-4952)。它已由 G4b 认领(spec_G4b.md:232 起),不要重复做。
12. 交接 §9 说 `docs/design/shared-redis-split-by-purpose.md` 未跟踪,现已随 d5cbb8a8e 入库。该文 :1025 把「给 RedisManager 加 AUTH,还是核心不开 requirepass、改用 NetworkPolicy」列为待拍板。本项默认走前者,需要负责人确认。
13. 交接 §7 阶段 0 写「每项先 Codex 实测;C++ 编译 MSBuild /m:1 串行」,按现状做不到:
    - 主仓和 Codex worktree 的 19 个子模块全部未初始化,没有 lib\ 和任何节点 exe;
    - HEAD 的 C++ 主干本身编不过(docs/PROGRESS.md:7005 记 exit 1、110 errors;:7051「主干的 C++ 目前本来就编不过」;cpp/libs/services/scene/player/system/player_equip.cpp:19 包含的 `table/code/equipaffixpool_table.h` 不在库里)。
14. 任务单的环境前提有三处与实际不符:
    - 工作区不干净。`git status` 有 15 个已修改、5 个未跟踪文件,其中 `tools/scripts/k8s_deploy.ps1`(+431 行)和 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`(+301 行)仍在被另一会话改写。
    - 本调查会话里 `pwsh` 不在 PATH,只有 Windows PowerShell 5.1.26100.9444;`go` 也不在 PATH。Codex 的环境是否不同,未核实。
    - Codex 的 worktree 有 8 个未提交改动,且这 8 个文件在 73b852f32 与 0aa1027bc 之间都有差异,不能直接 checkout。

### 原报告:同类入口盘点

【代码:连接的创建】
- 唯一生产入口:`Node::Initialize` → `InitRpcServer` → `tlsRedis.Connect`(node.cpp:495、:557)。对 gate / scene / battle 都执行。`tlsRedis` 是 thread_local(redis_manager.cpp:23),只有主 EventLoop 线程调 Connect,所以每个 C++ 进程恰好 1 条 zone Redis 连接。
- 死入口:`RedisManager::SetupReconnect`(redis_manager.cpp:63-86),零调用方。
- 同步客户端 `MessageSyncRedisClient::Connect`(cpp/libs/engine/infra/storage/redis_client/redis_client.cpp:15,`redisConnectWithTimeout`,无 AUTH):只被死工程 cpp/tests/redis_test 使用(run_cpp_tests.ps1:41-45 的说明)。
- 测试自建连接:cpp/tests/currency_test/currency_test.cpp:818(异步)、:854(`redisConnect`);cpp/tests/rpc_controller_test/hiredis_command_lifecycle_test.cpp:76(连假 Redis)。
- 封装层 Hiredis:
  - Windows 用 cpp/libs/engine/muduo_windows/src/contrib/hiredis/Hiredis.{h,cc}。
  - Linux 镜像用 tools/archived/muduo_linux_overlay/contrib/hiredis/Hiredis.cc(与 Windows 那份逐字节一致,已用 cmp 核对;overlay 里没有 .h)。
  - 命令接口 `int command(const CommandCallback& cb, muduo::StringArg cmd, ...)`(Hiredis.h:48)是 printf 风格,`%b` 取 (ptr, len)。仓库内先例见 redis_client.h:592-596。可直接发 `"AUTH %b"`,不需要改封装。
  - 未连接时直接返回 REDIS_ERR(Hiredis.cc:230)。

【代码:读取与使用】(所有直发点都在同一调用栈里先判 `redis && redis->connected()`)
- scene 存读:`MessageAsyncClient` 持有 `tlsRedis.GetZoneRedis()` 这个 unique_ptr 的引用(redis.cpp:19;redis_client.h:180-183)。就绪判断在 redis_client.h:220、:274、:330、:531、:556、:634。
- scene 直发:
  - player_lifecycle.cpp:635、:991、:1314、:1718、:3083、:4540、:4805;
  - player_battle.cpp:116-120(RedisReady),共 19 处 `->command(`;
  - player_team.cpp:50-54,4 处;
  - player_scene.cpp:48-52,1 处(:339)。
- battle:battle_room_manager.cpp:205-209(RedisReady),命令在 :1694、:1775、:1800、:1892、:1984。
- gate:无。
- 结论:把 `GetZoneRedis()` 在握手期间保持为空,对上面所有调用点都安全,不需要改任何一处。

【代码:断线、恢复、销毁】
- 断线:redis_manager.cpp:113-117,打 `Redis disconnected (status=..), scheduling reconnect to ..`,然后 ScheduleReconnect(:120-130;间隔写死 3.0s,:122)。
- 重连:DoReconnect(:141-157)销毁旧连接并新建;挂起命令以空应答回调(:145-146 注释)。仓库内有用例断言这一点(hiredis_command_lifecycle_test.cpp:254-280),是否跑绿未核实。
- 断线期间的命令:
  - 存盘进 pending_save_queue_(redis_client.h:531-538、:634-638);
  - 载入进 pending_retry_queue_(:220-231);
  - 直发命令的调用方各自跳过或降级(例:player_team.cpp:168;battle_room_manager.cpp:1682-1691)。
- 刚连上:连接回调里同步调 `reconnectCb_()`(redis_manager.cpp:101-104)。只有 scene 注册了它(redis.cpp:28-49):补发撤回、继承标记清理、改派核实,再 `OnReconnected()` 把在途与排队的存读全部重发(redis_client.h:272-322)。battle 没有重连钩子。
- 每秒定时器:redis.cpp:55-70 调 `RetryInheritedMarkClears` / `RetryDuePending` / `RetryPendingHandoffWithdrawals` / `SweepRelocateConfirms`,各自只看 connected()。
- 销毁:`RedisManager::Shutdown`(redis_manager.cpp:49-61);调用点 node.cpp:1181、node_entry.h:164、cpp/nodes/scene/main.cpp:366。

【代码:错误应答的处理与日志】
- 存盘:`Redis Save error for key: <key> err=<服务端文本>`(redis_client.h:736),之后 `Queued Save retry N/6 for key: ...`(:694-695)。6 次后打 `Save exhausted 6 retries for key: ...`(:679)和 `HandlePlayerAsyncSaveFailed: DATA-DURABILITY RISK ...`(player_lifecycle.cpp:1960)。
- 载入:`Redis GET error for key: <key> err=<文本>`(redis_client.h:817-818),之后 `HandlePlayerAsyncLoadFailed: Redis load failed for player <id>`(player_lifecycle.cpp:1935)。
- 脚本:`SCRIPT LOAD failed for PlayerAllData (reply <类型号>); will retry on next save`(redis_client.h:570-571),不带服务端文本。成功时打 `SCRIPT LOAD ok for PlayerAllData sha1=`(:575)。full_name 来自 redis.h:11 与 proto/common/database/player_cache.proto:30。
- 把错误应答当正常结果、且不打服务端文本的路径(不在 P0-A 最小修复内,接 redis-ha 之前要单独过一遍):
  - battle_room_manager.cpp:1783-1791:应答不是预期字符串(含 ERROR 和空应答)就打「battle 结算已销账」并删发件箱条目;
  - battle_room_manager.cpp:216:location 读到 ERROR 当作玩家不在任何场景;
  - player_battle.cpp:2030-2036:挂起结算读到 ERROR 当作无挂起结算,并按锁重建冻结;
  - player_battle.cpp:2053-2054:DEL 的应答完全不看;
  - player_battle.cpp:393-397:PING 回调不看 reply,直接调 AckSettlementPending。
- 日志级别:本机 `LogLevel: 2`(bin/etc/base_deploy_config.yaml:25)按 muduo 枚举是 INFO(Logging.h:20-28;node.cpp:1232-1234)。该 yaml 行注释写「2=WARN」是错的。K8s 的 node-config 是 `LogLevel: 1`(k8s_deploy.ps1:1790)。所以判据用的 INFO 行都会输出。

【配置模板】
- 本机:bin/etc/game_config.yaml:22-29、bin/etc/game_config_instance.yaml:14-21(占位口令 "yourpassword",db: 0)。
- 归档:tools/archived/debug_gate.sh:30,无口令键。
- 测试辅助头 cpp/tests/test_config_helper.h:61-67 也读 `etc/game_config.yaml`。
- proto:proto/common/base/config.proto 的 ZoneRedisConfig 已有 password 和 db 字段(生成物 config.pb.h:1821),不需要改 proto。
- 环境变量覆盖的既有模式:config.cpp:254(SCENE_NODE_TYPE)、:268(ZONE_ID);node.cpp:1250-1257(GAME_CONFIG_PATH)。

【部署脚本(HEAD 行号)】
- 口令解析:k8s_deploy.ps1:431-453。
- 服务端 Secret:k8s_deploy.ps1:5803-5826,配合 redis.yaml:90-104。
- C++ 配置:New-NodeConfigMapYaml(:1677-1887)。同函数对 GateTokenSecret 有「为空即 throw」的保护(:1708-1714),Redis 口令没有。
- C++ 工作负载模板共 5 个,都把 node-config 挂到 /app/bin/etc(:2064、:2604;lib :701):
  - New-NodeDeploymentYaml(:1889);
  - New-SceneFleetYaml(:2473);
  - lib/k8s_client_entry.ps1 的 New-GateStatefulSetYaml(:764)、New-BattleFleetYaml(:988)、New-BattleHostPortDeploymentYaml(:1108)。后三个的 env 共用 Get-ClientEntryNodeBaseEnv(:669-683)和 Get-ClientEntryEnvYaml(:646,目前只支持 value 与 fieldRef)。
- 现成的 Secret 原语:
  - Add-GoSvcSecretEnv(:4596);
  - Add-PodTemplateAnnotation(:4656 一带);
  - Get-InjectedSecretFingerprint(:4719,空值即 throw);
  - Apply-InjectedSecret(:4747,DryRun 下只打一行隐去值的意图,:4759-4762)。
- Redis 自身:脚本里没有任何 rollout restart;Secret 新建后已在跑的 Redis 不会自己重启。
- redis-ha:deploy/k8s/manifests/infra/redis-ha.yaml:3 提到的 `-RedisReplicas` 参数在脚本里不存在(shared-redis-split-by-purpose.md:201)。客户端经代理连主(:7),C++ 靠重连恢复。
- 发布预检:release_preflight.ps1:297-299 不查 C++。

【docker-compose 与本机】
- deploy/docker-compose.yml:217-221:redis 用 `image: redis:latest`、`container_name: redis`、端口 6379,无 requirepass(:234-239)。
- 本机 Go yaml 口令都是空串:go/db/etc/db.yaml:90、go/login/etc/login.yaml:43、go/player_locator/etc/player_locator.yaml:13。
- 本机节点启动:tools/scripts/cpp_nodes.ps1(参数 :58-104;exe 在 bin\,:126-130;stdout 日志在 run\logs\cpp_nodes\,:116、:351;滚动日志在 bin\logs\cpp_nodes\,:318)。子进程继承父 shell 环境。dev_tools.ps1 的 cpp-node-start 是它的包装(:1308)。

【测试】
- 部署契约测试:tools/scripts/tests/k8s_deploy_contract.tests.ps1。
  - 基线 dev DryRun 在 :33-47,prod DryRun 在 :922-944,battle infra DryRun 在 :1096-1098。
  - 另有 tools/scripts/tests/k8s_client_entry_contract.tests.ps1:1447 一组带 MMORPG_REDIS_PASSWORD 的用例。
- 采集库 tools/scripts/tests/lib/deploy_capture.ps1:Invoke-DeployDryRun(:95)、Get-ManifestBlocks(:122)、Select-ManifestByName(:147)、ConvertTo-FlatManifest(:166)、Get-FlatValue(:180)。要求 PowerShell 7(:1),子进程也用 pwsh(:78)。
- 断言库 tools/scripts/tests/lib/test_harness.ps1:Test-Case(:20)、Assert-True(:38)、Assert-Equal(:46)、Assert-Match(:57)。
- 现状:没有任何用例断言 C++ 的 Redis 口令。
- C++:
  - cpp/tests/rpc_controller_test/hiredis_command_lifecycle_test.cpp:59-145 有假 Redis 夹具。工程直接链接 lib\ 下的库(含 thread_context.lib、scene.lib、modules.lib),源文件清单在 rpc_controller_test.vcxproj:169-171。该工程不在 game.sln 里(run_cpp_tests.ps1:6)。
  - cpp/tests/configuration_table_test/configuration_table_test.cpp:489-538 有 GameConfigEnvOverride 用例组。
  - 包含 redis_manager.h 的测试:aoi_test、cross_zone_test、currency_test。
  - 真 Redis 用例的先例:currency_test.cpp:788-794,读环境变量 MMORPG_TEST_REDIS_ADDR。
  - tests 目录没有 CMakeLists,只有 vcxproj。
  - 统一入口 tools/scripts/run_cpp_tests.ps1:只打「N/M 全过」并删掉输出(:188、:191)。

【文档】
- 已登记本问题:docs/design/shared-redis-split-by-purpose.md:41、:195-199、:1004、:1025;docs/handoff/launch-readiness-20261013/spec_G4a.md:50、:204-208;docs/PROGRESS.md:7339。
- release 档实测清单漏了 C++ 和 Java:docs/PROGRESS.md:5440。
- 登记 readGameConfig 环境变量覆盖的两处:cpp/AGENTS.md:28、deploy/k8s/AGENTS.md:105。

### 原报告:修复设计

> **2026-10-15 落码说明(以此为准,下面的设计原文保留作推导记录)**:实际实现是「私有握手位 + 占位对象」,不是下文的「握手期间 `GetZoneRedis()` 为空」。
> - `Connect` 先把 `zoneRedis_` 设成一个只构造、从不 `connect()` 的占位 Hiredis(`connected()` 恒假、`command()` 直接返回 `REDIS_ERR` 且不调回调),所以 `GetZoneRedis()` 在 Connect 之后恒非空,十几处直接 `GetZoneRedis()->` 的调用点不需要改。
> - 每条新连接先放在私有的 `handshakeRedis_`;空口令连上即移交,有口令则第一条命令是 AUTH,回 `+OK` 才移交并同步调用重连回调。被拒、发不出去、一个重连间隔内无应答(超时)都不移交,各打一条带 `metric=zone_redis_auth_failed` 的 ERROR,到点在回调栈外重建握手再试。
> - fail-closed 的直接理由:`BattleRoomManager::ProbeAndRetryOne` 把非预期应答当成「已销账」并删外发箱条目,放行未认证连接会让 NOAUTH 被误读成资产结论。
> - 回归用例实际是 `RedisManagerAuth` 组 10 条(EmptyPasswordSendsNoAuth、AuthIsFirstCommandAndCallbackFollows、RejectedAuthKeepsZoneRedisUnavailableAndRetries、PasswordEchoedByServerIsRedacted、ReauthenticatesAfterServerDisconnect、ShutdownDuringHandshakeIsQuiet、AuthTimeoutRebuildsConnection、NonPositiveReconnectIntervalFallsBackToDefault、ServerClosingBeforeAuthReplyIsRetried、ServerClosingAfterRejectedAuthIsRetried)与配置测试 `ZoneRedisPasswordFromEnvOverridesYaml` / `ZoneRedisPasswordWithoutEnvKeepsYaml`。验证命令见 [Codex 任务单](global-singletons-p0-codex-tasksheet-20261014.md) §7。

【总体】本设计未编译、未测试,待 Codex 验证;本机当前也无法编译(见 codex_measurement 的 B0)。
- 不改两份 Hiredis.cc,不改 proto,不动 generated/。
- AUTH 用现有 `command()` 发(`"AUTH %b"`,二进制安全)。
- 握手逻辑全部收在 RedisManager 一处,业务调用点零改动。

【分两批交付】
- 第一批(可立即落码,不碰任何他人在途文件):
  - redis_manager.{h,cpp}、config.cpp、node.cpp;
  - bin/etc 两份 yaml;
  - C++ 测试(两个源文件加一个 vcxproj);
  - cpp/AGENTS.md;
  - docs/PROGRESS.md 末尾追加;
  - Java 仓库 PARITY.md。
  这些文件都不在 `git status` 里(docs/PROGRESS.md 除外,它只追加)。新二进制配空口令时行为与今天相同,可以单独上线。
- 第二批(部署侧;等 G2b / G4b 提交,或负责人明确放行):
  - tools/scripts/k8s_deploy.ps1、tools/scripts/lib/k8s_client_entry.ps1;
  - 两份契约测试;
  - deploy/k8s/AGENTS.md、docs/design/shared-redis-split-by-purpose.md。
  理由:
  - k8s_deploy.ps1 和 k8s_deploy_contract.tests.ps1 此刻有另一会话未提交的改动,仍在变;
  - 用户要求不碰(交接 :249-250「完整清单以 `git status` 为准」);
  - 另一份交接规定 G4b 排在 G2b 之后,两者都改这个脚本(handoff-launch-readiness-and-d1-20261013.md §4 第 2 项)。

【1. cpp/libs/engine/thread_context/redis_manager.{h,cpp}:握手连接与可用连接分开】

要点:正在建连或认证的连接放在私有成员 handshakeRedis_ 里,AUTH 回 +OK 后才移交给 zoneRedis_ 并调重连回调。握手期间、AUTH 被拒期间 `GetZoneRedis()` 为空,现有的 `redis && redis->connected()` 判断自然走「未连接」分支。

接口:
```cpp
struct RedisConnectOptions
{
	std::string password;               // 空 = 不认证(dev / 本机);非空 = 每条新连接先 AUTH
	double reconnectIntervalSec = 3.0;  // 断线重连间隔,同时是一次握手(建连 + AUTH)的截止时间
};
// 只返回已完成握手的连接;握手中、AUTH 被拒、断线待重连时为空或 connected()==false。
HiredisPtr &GetZoneRedis() { return zoneRedis_; }
void Connect(muduo::net::EventLoop *loop, const muduo::net::InetAddress &addr, RedisConnectOptions options);
```
- options 不给默认值,强迫唯一调用点(node.cpp:557)显式传。
- 重连间隔做成可注入,是为了让回归用例不依赖写死的 3 秒墙钟(redis_manager.cpp:122)。
- 删除零调用方的 `SetupReconnect`,否则留一条不认证的入口。
- 不新增 IsReady():隔离之后「GetZoneRedis() 非空且 connected()」就等于已认证。

草图:
```cpp
void RedisManager::StartHandshake()            // Connect() 与 DoReconnect() 共用
{
	handshakeRedis_ = std::make_unique<hiredis::Hiredis>(&loop_->get(), redisAddr_);
	handshakeRedis_->setConnectCallback([this](hiredis::Hiredis *, int status) { OnHandshakeConnected(status); });
	handshakeRedis_->setDisconnectCallback([this](hiredis::Hiredis *, int status) {
		LOG_ERROR << "Redis disconnected (status=" << status << "), scheduling reconnect to " << redisAddr_.toIpPort();
		ScheduleReconnect(); });
	handshakeRedis_->connect();
}
void RedisManager::OnHandshakeConnected(int status)
{
	if (status != kRedisOk) { /* 原 LOG_WARN 不变 */ ScheduleReconnect(); return; }
	LOG_INFO << "Redis connected to " << redisAddr_.toIpPort();
	if (options_.password.empty()) { PromoteHandshake(); return; }     // 与改动前的行为相同
	ScheduleReconnect();   // 握手截止:一个重连周期内没有 +OK,就由 DoReconnect 在回调栈外重建
	const int ret = handshakeRedis_->command(
		[this](hiredis::Hiredis *me, redisReply *reply) { OnAuthReply(me, reply); },
		"AUTH %b", options_.password.data(), options_.password.size());
	if (ret != kRedisOk)
		LOG_ERROR << "Redis AUTH could not be sent to " << redisAddr_.toIpPort() << " (ret=" << ret << "), will retry";
}
void RedisManager::OnAuthReply(hiredis::Hiredis *me, const redisReply *reply)
{
	// 空应答 = 这条连接正在被释放或已断开;me 不是当前握手连接 = 迟到回调。都不处理,下一步由重连定时器负责。
	if (reply == nullptr || me != handshakeRedis_.get()) return;
	if (reply->type == REDIS_REPLY_STATUS) { LOG_INFO << "Redis AUTH ok to " << redisAddr_.toIpPort(); PromoteHandshake(); return; }
	LOG_ERROR << "Redis AUTH rejected by " << redisAddr_.toIpPort() << ": " << DescribeAuthFailure(reply, options_.password)
			  << " -- zone Redis stays unavailable, retrying every " << options_.reconnectIntervalSec << "s";
	// 不在这里销毁连接(原因见下)。重连定时器已在跑。
}
void RedisManager::PromoteHandshake()
{
	CancelReconnectTimer();
	zoneRedis_ = std::move(handshakeRedis_);   // 此刻 zoneRedis_ 必为空:Connect / DoReconnect 都先 ResetConnections()
	if (reconnectCb_) reconnectCb_();
}
void RedisManager::ResetConnections()         // 原 ResetConnection 改为对两个成员各做一遍:先清回调,再 reset
```

说明:
- 顺序保证:握手连接上只有 AUTH 一条命令,业务命令只会落在已移交的连接上。scene 的每秒定时器(redis.cpp:55-70)和所有直发点在握手期都看到「未连接」。
- 放行积压:`reconnectCb_`(scene 的三张重发表和 OnReconnected)只在移交之后调。此时这条连接上还没有任何业务命令,OnReconnected「新连接上的第一批命令」这个前提不变(redis_client.h:269-271 的注释)。
- 重连路径:DoReconnect 先销毁两个成员再 StartHandshake,天然重新认证。这覆盖 redis-ha 代理掐会话的场景。
- 断线回调装在连接对象上,移交后继续有效;旧连接留在 zoneRedis_ 里(connected() 为假)直到 DoReconnect 销毁它,与今天相同。
- AUTH 被拒时不在回调里销毁连接。
  - 回调栈是 `Hiredis::commandCallback → (*cb)(this, reply)`(Hiredis.cc:259-263)。
  - 在栈内销毁 Hiredis 会让 hiredis 延后释放 context,随后 cleanup 钩子拿着已析构的对象调 removeChannel(Hiredis.cc:221-226)。
  - 「回调栈内 free 会延后」是 hiredis 内部行为,外部常识,未核实。
  - 所以失败时只留着定时器,由 DoReconnect 在回调栈外重建。
- 生命周期(AGENTS.md §11.7):
  - 连接、断线、AUTH 回调都捕获裸 this,属于「与被绑对象同寿命」的例外。两个 Hiredis 由本对象独占持有,ResetConnections 先清回调再释放。
  - AUTH 回调在连接释放时会收到空应答(仓库内用例:hiredis_command_lifecycle_test.cpp:254-280),开头就返回。
  - unique_ptr::reset 先置空再析构(同文件 :123 的注释),所以 `me != handshakeRedis_.get()` 也成立。
  - 在头文件注释里写明,写法同 redis_client.h:173-179。
- 日志不出现口令:只打地址和服务端错误文本。`DescribeAuthFailure` 是文件内的小函数,把错误文本截到 200 字节;文本里若包含口令原文,整段换成 `<redacted>`。
- 不用宏。

【2. 失败语义(fail-closed)】
- AUTH 被拒、发不出去或超时:zoneRedis_ 保持为空,对业务等同于「Redis 不可用」。
  - 存盘和载入留在排队里(redis_client.h:220-231、:531-538);
  - 直发命令各走既有的未连接分支;
  - battle 走「未落库直接投递」并打 `metric=battle_settlement_not_durable`(battle_room_manager.cpp:1682-1690)。
  - 每个重连周期重建并重试一次,打一条 ERROR。
- 与今天的差别:口令配错时,进场请求会排队等待,而不是立刻收到失败提示。这与「Redis 连不上」的既有表现一致。
- 进程不崩:scene 崩溃会丢内存态。
- 「客户端配了口令但服务端没开」同样按失败处理,与 Go 服务一致(k8s_deploy.ps1:4068-4069 的注释)。
- 空口令:不发 AUTH,行为与改动前相同。
- db:不实现 SELECT。
  - 跨运行时契约把库号钉在 0(k8s_deploy.ps1:4065「DB 号必须全仓一致 = 默认 0」)。
  - node.cpp 在建连前检查 `zone_redis().db() != 0` 则 LOG_FATAL 并说明原因(启动期配置错误当场失败,同 gate 的 ValidateGateTokenSecretOrDie)。
  - 这是行为变更,需负责人点头;不同意就只打 ERROR 或本项不动。

【3. cpp/libs/engine/core/node/system/node/node.cpp(:541-557)】
- 用 `zone_redis().password()` 填 RedisConnectOptions 传给 Connect。
- 原有的 `Zone Redis address: <ip:port>` 一行不动,另加一行 `Zone Redis auth: on|off`。
- 加上面的 db 检查。

【4. cpp/libs/engine/config/config.cpp(zoneredis 块内,yaml 解析之后)】
```cpp
if (const char *envPassword = GetNonEmptyEnv("ZONE_REDIS_PASSWORD"))
{
	gameConfig.mutable_zone_redis()->set_password(envPassword);
	LOG_INFO << "ZONE_REDIS_PASSWORD env override applied (value hidden)";
}
```
- 与 SCENE_NODE_TYPE / ZONE_ID 的覆盖同一模式(:254、:268;GetNonEmptyEnv 在 :11)。
- 用途:
  - 本机与 Codex 验收时不必把口令写进被跟踪的 yaml;
  - 第二批不论选哪种口令来源,C++ 都不用再改。
- 同步文档 cpp/AGENTS.md:28(列出 readGameConfig 认的环境变量)。

【5. bin/etc/game_config.yaml:25、bin/etc/game_config_instance.yaml:17】
- 占位值 "yourpassword" 改为 "",注释写明「空 = 不认证;带口令的环境经部署注入或 ZONE_REDIS_PASSWORD」。
- 必须与第 1 条同批:否则修复后本机节点会拿占位口令去认证,本机无口令 Redis 会拒绝(外部常识,未实测;黑盒第 4 组验证)。

【6. 第二批:口令怎样到达 K8s 上的 C++(需负责人二选一;第一批对两种都兼容)】

> **2026-10-15 第二批落码说明(以此为准,下面的设计原文保留;未经 Codex 运行验证)**:负责人定方案 A。
> - `tools/scripts/k8s_deploy.ps1`:`New-NodeConfigMapYaml` 的 `zoneredis.password` 取 `Get-CppZoneRedisPassword`(读 `$script:RedisPassword`;未解析 / 非 dev 档为空 / 含 `"`、`\`、控制字符一律 throw,只点 `MMORPG_REDIS_PASSWORD`)。字符集另在 `Initialize-InjectedSecrets` 里、任何写操作之前查一次(`Assert-RedisPasswordYamlSafe`,同一个值也进 Go / gateway ConfigMap 与 Secret `redis-auth`)。
> - 指纹注解 `mmorpg.io/zone-redis-auth-hash`(`Get-CppZoneRedisAuthAnnotations`,只哈希口令,用途串 `zone-redis/cpp-node`,口令为空不写):本脚本的 Deployment / scene Fleet 模板经 `New-CppPodAnnotationBlock`,lib 的三个模板经 `New-ClientEntryPodCommon -PodAnnotations`;两边共用 lib 新增的 `ConvertTo-ClientEntryPodAnnotationBlock`,与 sidecar 指纹写进同一个 `annotations:` 段。没有复用 `Add-PodTemplateAnnotation`:它要求恰好一处 `template:`→`metadata:` 且缩进是空格,Fleet 有两层 template、Deployment 模板用制表符缩进。
> - battle 工作负载的渲染从 `Assert-ClientEntryDeployPreflight` 挪到 `Apply-Infra` 开头(仍早于任何基础设施写操作),结果显式传给 `Apply-BattlePool -Workload`,删掉了 `$script:BattleWorkload`:入口预检排在密钥解析之前,在那里渲染拿不到口令。
> - 契约测试:`k8s_deploy_contract.tests.ps1` 3a 节 7 条 + battle 段 1 条,`k8s_client_entry_contract.tests.ps1` 1 条;文档 `deploy/k8s/README.md`「C++ 节点的 zone Redis 口令」、`deploy/k8s/AGENTS.md`、`docs/design/shared-redis-split-by-purpose.md` §2.5 / §12 / §13。
- 方案 A(明文进 node ConfigMap):
  - New-NodeConfigMapYaml 里把 `password: ""` 改成取 $script:RedisPassword。
  - 加两道生成期保护,仿 GateTokenSecret(:1708-1714):
    - 非 dev 档口令为空就 throw;
    - 口令含 `"`、`\` 或控制字符就 throw(只点变量名,不回显值)。
  - 优点:与 Go 服务同一做法。
  - 缺点:明文在 ConfigMap 和 DryRun 输出里;与交接「密码走 Secret」的方向相反。
- 方案 B(Secret 注入环境变量 ZONE_REDIS_PASSWORD):
  - battle 在 infra namespace,直接引用服务端在用的 Secret `redis-auth`(key `password`),与服务端同源同开关。
  - gate / scene 在 zone namespace,用 Apply-InjectedSecret(:4747)建同名 Secret;口令为空时删掉它,仿 :5809-5811。
  - 5 个模板在口令非空时加一条 secretKeyRef 环境变量,不写 optional(Secret 缺失时 Pod 起不来,而不是带空口令起来,同 :4594 的理由)。
  - lib 的三个模板要让 Get-ClientEntryEnvYaml(:646)支持 secretKeyRef。
  - node-config 里的 password 保持空串。
  - 同步 deploy/k8s/AGENTS.md:105。
  - 优点:明文不进 ConfigMap,符合交接方向。
  - 缺点:改动面比 A 大;Go 服务仍是明文,单做 C++ 不改变整体暴露面。
- 两种方案都要做:给 5 个 C++ 模板加口令指纹注解(Get-InjectedSecretFingerprint,:4719;口令为空时不写)。
  - 原因:ConfigMap 和 Secret 的变化都不触发 Pod 滚动,而 C++ 只在启动时读配置。
  - 现有模板只会拼一条日志 sidecar 注解(:1922、:2504;lib :740),要改成能拼多条。
  - 这一步使 A 的改动面不再是「一处」,与 B 的差距缩小。
- 倾向 B(符合交接方向;battle 与服务端同源)。最终由负责人定。

【7. 上线顺序与新旧并存】
- 新二进制加空口令:与今天相同,可以先滚。
- 旧二进制加带口令的配置:旧版忽略口令,照旧被拒。所以带 AUTH 的镜像必须先行。
- 给已在运行的环境开口令,是一次有停机窗口的操作,步骤:
  1. 先发带 AUTH 的 C++ 镜像;
  2. 维护窗口内带 MMORPG_REDIS_PASSWORD 重跑 infra-up 和各 zone 的 zone-up;
  3. 手动重启 Redis(脚本不会做;Secret 新建不改 Pod 模板,这是 K8s 行为,外部常识,未核实);
  4. C++ 工作负载在第二批做了指纹注解之后自动滚动,没做就要手动滚动(scene 滚动即玩家被迁走)。
  - 通过标准:每个 C++ 节点日志出现 `Redis AUTH ok`,Redis 的 errorstats 里 NOAUTH 不再增长。
- 首次开服是冷启动:Secret 先于 redis.yaml 落地(k8s_deploy.ps1:5807-5808),不存在滚动问题。
- redis-ha 的前置:本项、WAITAOF(另一条 P0)、脚本补 `-RedisReplicas`。Service redis 不得重建(C++ 只在启动时解析一次地址)。

【8. 回归测试】
(a) 新文件 cpp/tests/rpc_controller_test/redis_manager_auth_test.cpp(第一批),并在 rpc_controller_test.vcxproj:169-171 之后加一行 ClCompile。tests 没有 CMake,不用同步。
- 夹具沿用 hiredis_command_lifecycle_test.cpp 的做法:EventLoopThread 加 muduo TcpServer 当假 Redis。
- 假 Redis 做最小 RESP 解析,记录每条连接收到的字节,可配置对 AUTH 回 `+OK`、回 `-WRONGPASS ...`、压住不回、认证后主动断开。
- 被测对象是测试里自建的 RedisManager(不用 tlsRedis),重连间隔注入为 0.05 秒,等待一律用限时 future。
- 链接桩已由 rpc_client_lifecycle_test.cpp 定义,不要重复定义。
- 用例组 RedisManagerAuth,共 6 条:
  1. EmptyPasswordSendsNoAuth:空口令时假 Redis 收不到任何 AUTH,连上即触发回调,GetZoneRedis() 非空。
  2. SendsAuthFirstAndPromotesOnlyAfterOk:
     - 压住 AUTH 应答期间,收到的字节恰好是一条 `*2\r\n$4\r\nAUTH\r\n...`,GetZoneRedis() 为空,回调 0 次;
     - 放出 +OK 后回调恰好 1 次,GetZoneRedis() 非空且 connected();
     - 回调里发的 PING 排在 AUTH 之后。
  3. RejectedKeepsZoneRedisNullAndRetries:
     - 回 -WRONGPASS 时回调 0 次,GetZoneRedis() 始终为空;
     - 限时内观察到第二条连接,且它的第一条命令仍是 AUTH;
     - 假 Redis 全程没有收到 AUTH 以外的任何命令;
     - 用 Logger::setOutput(Logging.h:76)抓日志,断言含 `Redis AUTH rejected`、不含口令原文。
  4. ReauthenticatesAfterDisconnect:认证后假 Redis 主动断开,新连接再次先发 AUTH,回调第 2 次触发。
  5. AuthTimeoutRebuildsConnection:AUTH 一直不回,限时内观察到第二条连接,回调 0 次。
  6. ShutdownDuringHandshakeIsQuiet:握手未完成时 Shutdown,不触发回调、不崩。
- 这组用例在修复前因接口不存在而无法编译(AGENTS.md §11.4 的「无法先写」情形)。红态证据用 (b) 和黑盒第 0 组替代。

(b) cpp/tests/configuration_table_test/configuration_table_test.cpp 的 GameConfigEnvOverride 组新增 2 条(第一批):
- ZoneRedisPasswordFromEnvOverridesYaml:设 ZONE_REDIS_PASSWORD 后 `cfg.zone_redis().password()` 等于它。修复前可编译,运行期红。
- ZoneRedisPasswordEmptyEnvKeepsYaml:不设时等于随仓库发布的 yaml 值,即空串。这条同时钉住占位口令不再回来。

(c) 契约用例(第二批),加在 tools/scripts/tests/k8s_deploy_contract.tests.ps1 的 prod 段(:944 之后)和 battle 段(:1098 之后)。不要写反斜杠路径(CI 在 ubuntu 上跑)。
- 方案 A 的断言:
  - prod 档 node-config 的 `data.game_config.yaml.zoneredis.password` 等于注入值;
  - dev 档为空串;
  - battle-node-config 在带口令的 infra-up DryRun 里等于注入值;
  - 负向:非 dev 档口令为空、口令含引号,各断言错误文本。
- 方案 B 的断言:
  - node-config 的口令仍为空串;
  - 筛出挂载 /app/bin/etc 的每个块,都有 ZONE_REDIS_PASSWORD 的 secretKeyRef;
  - zone namespace 有建 Secret 的 DryRun 行;
  - dev 档不设口令时三者都没有。
- 两种方案都加:换一个口令重渲染,gate / scene / battle 模板的指纹注解变化;口令为空时没有该注解。
- 修复前「prod 档 C++ 有口令来源」这条运行期红。

【9. 文档与登记(落码会话做)】
- docs/PROGRESS.md 末尾追加条目(写之前重新读一次,不改任何已有行),写明:
  - 第一批改了什么;
  - 「未编译、未测试,且本机当前无法编译,待 Codex 验证」;
  - 第二批未做及其原因;
  - Java 版状态「已做,实现不同」。
- Java 仓库 PARITY.md 追加一行(见 java_parity)。
- 第二批落码时,在 docs/design/shared-redis-split-by-purpose.md 的 :195-197 和 :1025 注明结论。

【10. 交 Codex 的命令(按顺序,前一步不过就停;全部以 codex_measurement 的 B0 通过为前提)】

1. 全量构建(pwsh,工作树根;必须 /m:1;redis_manager.h 被十几个翻译单元包含):
```powershell
$msbuild = & "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe' | Select-Object -First 1
& $msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
```
- 通过:退出码 0;产出 bin\scene.exe、bin\gate.exe、bin\battle.exe、lib\thread_context.lib。
- 失败:保留前 20 行 `error`。
- 报错若只在装备相关文件(bag_marshal.cpp、equipaffixpool_table.h,docs/PROGRESS.md:7005),那是既有阻塞,不是本改动,照实报告后停下。

2. 单测(测试工程不在 game.sln 里,直接链接 lib\ 下的库,必须排在第 1 步之后):
```powershell
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter configuration_table
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter aoi
build\cpp\tests\rpc_controller_test.exe --gtest_filter=RedisManagerAuth.* *> (Join-Path $env:TEMP 'p0a_redis_manager_auth.txt')
build\cpp\tests\configuration_table_test.exe --gtest_filter=GameConfigEnvOverride.* *> (Join-Path $env:TEMP 'p0a_game_config_env.txt')
```
- 通过:
  - 五张汇总表都是「全过」;
  - 第一个输出文件含 `[  PASSED  ] 6 tests`;
  - 第二个含 `[  PASSED  ] 7 tests`(原有 5 条加新增 2 条)。
- 失败:保留两个输出文件全文。汇总脚本只打「N/M 全过」并删掉输出(run_cpp_tests.ps1:188、:191),所以新用例要直接跑 exe。
- 可选的红态:Codex 的树里若留有修复前的 lib\config.lib,先只跑 `-Build -Filter configuration_table`(只建测试工程),ZoneRedisPasswordFromEnvOverridesYaml 应为红。没有就跳过。

3. 部署契约(仅第二批):
```powershell
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
```
- 通过:全部 PASS,退出码 0。
- 失败:保留完整输出。

4. 黑盒验收(沿用 codex_measurement 的 B2~B6;口令经 `$env:ZONE_REDIS_PASSWORD` 传给 scene,不写进任何文件),共 6 组:
- 第 0 组(等价于修复前的红态):不设口令,连带口令探针。期望 `Zone Redis auth: off`、两条 `SCRIPT LOAD failed for PlayerAllData (reply`,errorstats 出现 NOAUTH。
- 第 1 组:`$env:ZONE_REDIS_PASSWORD='p0a-probe-pass-01'`,连带口令探针。期望 `Zone Redis auth: on`、`Redis AUTH ok to 127.0.0.1:6390`、`Redis reconnected, retrying pending player loads`、两条 `SCRIPT LOAD ok`;NOAUTH 计数不再增长。
- 第 2 组:口令设错。期望约每 3 秒一条 `Redis AUTH rejected by 127.0.0.1:6390`,且不出现 `Redis reconnected, retrying pending player loads` 和任何 `SCRIPT LOAD`。
- 第 3 组:不设口令,连无口令探针。日志与修复前一致,只多 `Zone Redis auth: off`。
- 第 4 组:设口令,连无口令探针。期望 `Redis AUTH rejected`(同时核实「向无口令 Redis 发 AUTH 会报错」这条外部常识)。
- 第 5 组(重连):第 1 组跑着时执行 `docker restart p0a-redis-auth`。期望依次出现 `Redis disconnected`、`Redis connected to`、`Redis AUTH ok`、两条 `SCRIPT LOAD ok`。
- 每组之间先 `stop -Nodes scene` 再启动。
- 全部日志里搜两个口令字符串(正确的和设错的),必须 0 命中。
- 保留:每组命中的日志行和 errorstats 输出。

5. 本机回归:起本机开发栈跑一轮登录冒烟,确认 bin/etc 改成空口令后行为不变。
- 入口:`pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start`(dev_tools.ps1:1314、:1453)。
- 冒烟配置在 robot/etc/robot_smoke.yaml。
- 具体的 robot 启动命令需 Codex 先确认。
- 本调查会话里没有 Go 工具链;Codex 环境里若也没有,本步不可执行,照实报告。

6. 未验证项(不做就在交付说明里写明):
- Linux 镜像构建。入口 `docker build -f deploy/k8s/Dockerfile.cpp -t mmorpg-node:p0a .`(Dockerfile.cpp:6-7;首次约 30~60 分钟,要下载基础镜像,需负责人同意;同样受子模块和主干编译状态限制)。
- 缺陷实际出现在 K8s 的 Linux 镜像里,Windows 上的验证不能替代它。

### 原报告:Java 版

【结论】Java 版已做,实现不同:用 Redisson,口令已接线,建连与重连时的认证由客户端库负责,没有手写的连接握手。没有发现同类缺陷的静态证据。

【证据(Java 仓库 D:/luyuan/xuanming-server-mmo,只读;工作区干净,HEAD d942f74)】
- game-discovery/src/main/java/com/game/discovery/RedisProperties.java:23-24:`@ConfigurationProperties("game.redis")`,record 含 `String password`。:34-36 把空白口令归一为 null。
- game-discovery/src/main/java/com/game/discovery/RedisAutoConfiguration.java:44-46 `.setAddress(props.address())` / `.setDatabase(props.database())` / `.setPassword(props.password())`。
- game-login/src/main/java/com/game/login/LoginConfiguration.java:191-193:Sa-Token Redis 的口令读环境变量,非空才 `server.setPassword(redisPassword);`。

【未核实】
- Redisson 重连后自动重新认证,是外部常识。
- Java 版连带口令的真 Redis 没有实测记录。
- Java 仓库的 deploy/ 下没有 K8s 部署脚本(两份复核的结论,本次未重查),带口令环境里口令怎么注入没有对应物可查。

【注意区分】本仓 `java/gateway_node` 属于 C++/Go 版的组成部分,不是 Java 版。它的 K8s ConfigMap 缺 `spring.data.redis.password`(k8s_deploy.ps1:4949-4952),由 G4b 修,不进 PARITY。

【PARITY.md 怎么登记】
- 状态取值以 PARITY.md:23 为准:`已对齐` / `Java 待做` / `mmorpg 待做` / `Java 进行中` / `不适用(写原因)`。
- 落码但 Codex 未验证期间登记 `mmorpg 待做`;验证通过后改 `已对齐`。
- 不用「不适用」:两版都需要、也都有这项能力,落后的一方是 mmorpg。「不适用」会把这一点盖掉。
- 仓库里把「Java 有别的机制」登记成「不适用」的先例(第 151、152 行),说的是 Java 没有对应机制的部署门禁和 Kafka 落库进程,情形不同。
- 本仓 docs/PROGRESS.md 条目按 AGENTS.md §12 写「Java 版:已做,实现不同」。

建议追加的一行(列:功能 | mmorpg 位置 | mmorpg commit | Java 模块 | Java 版本 | 状态 | 备注):
| C++ 节点连共享 Redis 的口令认证(每条新连接先 AUTH,回 +OK 后才对业务可见并补发积压;口令来自 `zoneredis.password`,环境变量 `ZONE_REDIS_PASSWORD` 覆盖;空口令不发 AUTH) | `cpp/libs/engine/thread_context/redis_manager.{h,cpp}`、`cpp/libs/engine/config/config.cpp`、`cpp/libs/engine/core/node/system/node/node.cpp`、`bin/etc/game_config*.yaml` | <修复提交;提交前写「未提交」> | game-discovery(`RedisProperties`、`RedisAutoConfiguration`)、game-login(`LoginConfiguration`) | 0.1.0-SNAPSHOT | mmorpg 待做 | <日期> 登记。服务端内部实现,不涉及客户端契约。mmorpg:第一批(仅 C++)已落码,未编译、未测试,待 Codex 验证;K8s 口令注入(第二批)未做,等 `k8s_deploy.ps1` 的在途改动提交。Java:已做,实现不同——`game.redis.password` 经 `SingleServerConfig.setPassword` 交给 Redisson,Sa-Token Redis 的口令读环境变量;带口令的真 Redis 未实测,Java 仓库没有 K8s 口令注入脚本。验证通过后本行状态改「已对齐」。 |

### 原报告:风险与未决

【需要负责人拍板】
1. 落码前的「实测」做到哪一步。B 段(运行期)和修复后的全部 C++ 验证被两件事卡住:
   - 子模块与第三方库没有(两个工作树都是 19 个未初始化);
   - 主干 C++ 本身编不过(装备属性生成物缺失)。
   二选一:
   - (1) 授权搭环境(拉子模块、第三方全量构建),并先完成装备线的导表与 proto-gen,之后再跑 B 段;
   - (2) 接受「步骤 0 加 A 段渲染结果加静态证据」作为第一批的落码前核实,运行期红 / 绿等环境就绪后在一次构建里补(黑盒第 0 组就是修复前的表现)。
   建议 (2)。
2. 是否接受拆两批:先交 C++ 与 bin/etc;部署模板和契约测试等 G2b / G4b 提交后,或负责人明确放行后再做。
3. 方向确认。shared-redis-split-by-purpose.md:1025 把「给 RedisManager 加 AUTH,还是核心不开 requirepass、改用 NetworkPolicy」列为待拍板。本项按前者设计。
4. 第二批的口令来源选方案 A(明文进 node ConfigMap)还是方案 B(Secret 注入环境变量),以及是否同批给 5 个 C++ 模板加口令指纹注解。倾向 B 加指纹注解。不做指纹注解的话,开口令和轮换口令都要手动滚动全部 C++ 工作负载。
5. `zoneredis.db != 0` 时是否 LOG_FATAL。这是行为变更;仓库内所有模板都是 0。
6. 是否另起一项给 scene / battle 加「zone Redis 已认证」的就绪条件。
   - 现成接缝:cpp/nodes/scene/main.cpp:350-351 的 dependencyGate.AddCondition。
   - 修复后口令配错的表现是「进场排队等待、Pod 仍 Ready」,这一项的价值比以前高。
   - 不在本项最小修复内。
7. PARITY 状态用词(`mmorpg 待做` → `已对齐`,还是 `不适用`)。两份复核意见相反,本报告取前者。影响小。
8. 下载类事项:本机没有可用的 Redis 镜像时拉镜像;Linux 镜像构建要拉基础镜像。都需要同意。

【需 Codex 实测确认(外部组件行为,本次只凭记忆或仓库内注释)】
- 未认证时的错误文本以 NOAUTH 开头(仓库内表述:redis.yaml:108-109、:117)。
- Redis 6.2 以上有 `INFO errorstats`。
- 向无口令服务端发 AUTH 会报错(仓库内表述:redis.yaml:87、k8s_deploy.ps1:4069;黑盒第 4 组验证)。
- AUTH 的错误文本不回显真实口令(设计里另有脱敏兜底)。
- REDIS_REPLY_ERROR 的类型号是 6(由 mrediscli.cc:20-23 的字符串表推出)。
- hiredis 内部行为:
  - 连接回调触发时已置 REDIS_CONNECTED、可以发命令(仓库内已有同位置发命令的先例:redis.cpp:28-49 → redis_client.h:272-302);
  - 回调栈内 free 会延后;
  - 断线时挂起命令以空应答回调。
  third_party/redis 子模块未检出,读不到 async.c。环境就绪后由 Codex 对照源码核一遍。
- YAML::LoadFile 是否接受绝对路径(实测 B3 依赖它)。
- scene 在 etcd / kafka 不可达时会不会在 loop 开始前退出。
- `docker restart` 之后探针 Redis 的口令仍在(启动参数不变,应当成立)。
- Secret 新建不会让已在跑的 Redis 重启(K8s 行为)。
- 全量 C++ 构建耗时。
- Codex 环境里有没有 pwsh 7、Go 工具链、可用的 Redis 镜像、可编译的树。
- HEAD 上 k8s_deploy_contract.tests.ps1 是否全绿。
- Redisson 重连后是否自动重新认证;Java 版在带口令 Redis 上的实际表现。

【与另外两份交接的重叠与冲突】
- tools/scripts/k8s_deploy.ps1 和 tools/scripts/tests/k8s_deploy_contract.tests.ps1 正被另一会话改写(G2b,未提交,行数仍在涨)。第二批要改同一脚本的 New-NodeConfigMapYaml、5 个工作负载模板和同一份契约测试。落码时:
  - 按函数名定位;
  - 不得 `git add -A`;
  - 提交顺序排在 G2b、G4b 之后(handoff-launch-readiness-and-d1-20261013.md §4 第 2 项、§7 第 5 条)。
- G4b 要在 New-JavaSvcConfigMapYaml 里给 gateway 写 Redis 口令,并把口令纳入 Pod 指纹(spec_G4b.md:232 起、:449-450)。函数不同,但同一文件。本项不替它做;指纹注解的写法应与它一致。
- godb 交接的 WAITAOF 和本项同为 redis-ha 的前置,互不依赖。
- docs/PROGRESS.md 在工作区里有他人未提交的追加。本项只在文件末尾追加,不改已有行。

【技术风险】
- `GetZoneRedis()` 的语义变了:以前 Connect 之后立刻非空,现在握手完成前为空。已逐一核对所有 `->command(` 调用点都先判空;以后新增调用点必须沿用 `redis && redis->connected()`。
- 口令配错时每个重连周期重试并打一条 ERROR,约每节点每分钟 20 行。
- 握手截止复用重连定时器,首次建连时截止时间是 3 秒;Redis 响应慢于此会反复重建。
- 只支持默认用户的单参数 AUTH,不支持 ACL 用户名。
- 方案 A 下口令含 `"` 或 `\` 时 YAML 双引号写法会坏(设计里用生成期 throw 挡住);Go 服务的 ConfigMap 有同样的既有限制。
- 带口令的 infra-up DryRun 会把口令打进输出(k8s_deploy.ps1:5815 注释,既有行为)。
- 改 redis_manager.h 会触发大范围重编。包含它的测试工程(aoi_test、cross_zone_test、currency_test、rpc_controller_test)都要重编,否则类布局不一致。
- 第一批单独上线后,K8s 上注入口令的环境仍然不通,要等第二批。交付说明里必须写明,避免被理解为 P0-A 已闭环。

【顺带发现(不在本项内,建议另开)】
- battle_room_manager.cpp:1783-1791:结算重投的探测读,应答为空(断线)或 ERROR 时一律判「已销账」并删发件箱条目。Redis 抖动时会丢重投。
- player_battle.cpp:2030-2036、battle_room_manager.cpp:216 把 ERROR 当「没有记录」。接 redis-ha 之后的 READONLY / LOADING 会触发。
- bin/etc/base_deploy_config.yaml:25 的注释把 LogLevel 2 写成 WARN,按 muduo 枚举是 INFO。
- docs/PROGRESS.md:6870「C++ 在主仓(子模块齐全)编」已不成立。

### 原报告:预计改动文件

- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/thread_context/redis_manager.h
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/thread_context/redis_manager.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/config/config.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/core/node/system/node/node.cpp
- D:/luyuan/xuanming-server-mmo-cpp/bin/etc/game_config.yaml
- D:/luyuan/xuanming-server-mmo-cpp/bin/etc/game_config_instance.yaml
- D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/rpc_controller_test/redis_manager_auth_test.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj
- D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/configuration_table_test/configuration_table_test.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/AGENTS.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/PROGRESS.md
- D:/luyuan/xuanming-server-mmo/PARITY.md
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/k8s_deploy.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/lib/k8s_client_entry.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/k8s_deploy_contract.tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/k8s_client_entry_contract.tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/AGENTS.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/design/shared-redis-split-by-purpose.md

## P0-B K8s 上 battle 的 Kafka 生产者拿不到 broker


### 结论

- 第一轮判定:`partially_confirmed`。
- 在 `2f897bd07` 上是否仍成立:是。

**把握**:

high(对「缺陷存在、只影响 battle、由 K8s 模板 GroupID 非空触发」这一核心判断)。

原因:
- 证据链的每一跳本轮都在 HEAD 上逐行重读过,没有分叉:Init 两个分支、Subscribe 的无回调早退、返回值被丢、battle 不注册消费回调、setBrokers 全仓只有两个调用点、send 在无 broker 时的早退、模板常数与 battle-node-config 的生成点。
- 两份对抗复核各自独立重读后都同意缺陷成立,并且专门找过保护逻辑(环境变量覆盖、子类覆写、第二条订阅路径、镜像自带配置),都不存在;其中环境变量与覆写两项本轮重新 grep 过。
- C++ 与 bin/etc 在 73b852f32..HEAD 之间无差异,交接给的 C++ 行号全部仍然有效。

保留:
- 没有任何运行证据(AGENTS §10.1);最终以 Codex 的步骤 1 与步骤 2 为准。步骤 2 目前受编译环境限制,可能跑不了。
- 对「后果」的把握是 medium:票据下发断掉、结算有 Redis 兜底这两点读过代码;丢票补签实际能挽回多少、活动局路径没有重读。
- 对修复设计的把握是 medium-high:改动小且有先例可循,但测试能否链接通过、infra 与 core 在当前主干上能否单独编过,都没有运行证据。
- 判 partially_confirmed 而不是 confirmed_static,只因为交接对后果的描述有三处不准(不是静默、topic 名、漏了票据),根因部分没有疑点。

### HEAD 前进后的更正(0aa1027bc → 2f897bd07)

【证据链在 HEAD 2f897bd07 上逐跳重读,全部成立】
1. kafka_manager.cpp:45-55:GroupID 为空时 :52 调 setBrokers,然后 :54 return true。
2. kafka_manager.cpp:57-63:GroupID 非空时调一次 Subscribe(..., {zoneId}),callback 用缺省值(kafka_manager.h:36 `KafkaMessageCallback callback = {},`)。
3. kafka_manager.cpp:76-79:没有回调时 LOG_DEBUG 后 return false。登记 broker 在后面的 :90,所以非空分支走不到 :90。
4. node.cpp:634-637:InitKafka 丢掉 Init 的返回值。:477 断言在 loop 线程,:497 InitKafka 早于 :498 InitEtcdService。
5. battle 从不订阅:
   - main.cpp:236 刻意不声明 KafkaCommandType;
   - node_entry.h:85-86 只在声明了它时才 SetKafkaHandlers;
   - node.h:224 没装 handler 时放行;
   - cpp/nodes/battle 下 git grep RegisterKafkaMessageHandler / SetKafkaHandlers / Subscribe( 都无命中。
6. 生产者:
   - kafka_producer.cpp:92 `if (pendingBrokers_.empty()) return false;`;:133-137 Produce rejected,返回 ERR__STATE。
   - 全仓 setBrokers 仍只有 kafka_manager.cpp:52 和 :90 两处。
7. K8s 模板:
   - k8s_deploy.ps1:2262 `GroupID: "game-consumer-group"`。
   - Apply-Infra 在 :6436-6437 用同一个 New-NodeConfigMapYaml(:2109)生成 battle-node-config。
   - ConfigMap 以只读整目录挂到 /app/bin/etc(:2494-2497),注释在 :2117-2119。
   - 本机 bin/etc/base_deploy_config.yaml:75 是 `GroupID: ""`。
8. 没有环境变量能覆盖:config.cpp:204-207 只读 yaml;cpp/libs 和 cpp/nodes 下 grep `KAFKA_` 只命中 LIBRDKAFKA_STATICLIB 宏。
9. 发送线程:battle_node.cpp:137(另有 :177/:204/:231/:258)经 loop_.runInLoop 投递,loop 是 node.GetLoop()(battle main.cpp:265)。和 Init 在同一线程,用的是同一个 thread_local 生产者。
10. 区间内没有任何修复:git grep game-consumer-group 仍然只命中 k8s_deploy.ps1:2262 和 tools/archived/debug_gate.sh:22。

【区间改动逐项(0aa1027bc..2f897bd07,共 13 个提交)】

A. e4ff1d165:tools/scripts/k8s_deploy.ps1,6019 → 6715 行
- 改了什么:
  - G2b MySQL 连接池镜像与总账:Assert-MysqlPoolMirror / Write-MysqlConnectionLedger,接在 zone-up/all-up/infra-up 写路径入口 :6639-6654。
  - G4b 削峰参数 -GatewayRateLimit / -LoginQueue(dev 档 auto = 关)和 Show-PeakShavingPlan。
  - 提示文案改英文。
  - New-NodeConfigMapYaml 的 Kafka 段内容一字未改,只是整体下移:函数 1677→2109,Kafka 段 1825-1834→2257-2266,GroupID 1830→2262,LogLevel 1790→2222,Apply-Infra 5744→6432。
  - 新增两处直接调 kubectl(:1509、:2856),但 DryRun 下会先返回(:1503、:2849)。
- 影响结论:否。
- 影响实测:只影响出处行号。HEAD 的契约测试用一模一样的参数跑 battle infra-up(:1149-1151),所以 1-1 / 1-2 的参数不用改。
- 影响修复设计:批 2 落点不变,仍在读 Kafka.CommandTopicPartitions 那两行旁边(现在是 :2131-2132),只是行号全变。昨天「文件被他人未提交改动占用」这个延后理由已消失,但见 E 条。

B. e4ff1d165:tools/scripts/tests/k8s_deploy_contract.tests.ps1,1596 → 1897 行
- 改了什么:
  - 位置变化:Kafka 命令 topic 用例 1090-1095→1143-1148;$BattleInfraArgs / $BattleTestEnv / $battleInfraRun 1096-1098→1149-1151;键路径写法 1229→1282。
  - 仍然没有任何用例钉 Kafka.GroupID。
  - 新增的「负向:zones.10zones.yaml 时打 MySQL连接总账超限 警告且不阻断」(:1649)断言中文「MySQL连接总账超限」(:1656),但脚本现在打的是 [MySQLConnectionLedger]OverLimit:(k8s_deploy.ps1:1238)。按静态阅读这一条会红。
- 影响结论:否。
- 影响实测:1-3 的期望要改成「不要求退出码 0,至少这条红」。
- 影响修复设计:批 2 第 11 条的插入点改为 :1148 之后,battle 那一半要放在 :1151 之后。

C. e4ff1d165:只改日志文案(英文化),各文件增删行数相等,行号不漂移
- 涉及文件:battle_room_manager.cpp、battle main.cpp、battle_client_edge.cpp、battle_handler.cpp、gate/scene main.cpp、kafka_consumer.cpp(:218-240)、node_kafka_command_handler.h(:133-134)。
- 影响结论:否。三处 send 仍在 :93 / :170 / :292,失败只记日志仍在 :95-100 / :172-178 / :294-298。
- 影响实测:步骤 3-5 的日志判据文本变了:
  - 「battle 出站 GateCommand 发送失败」→「battle outbound GateCommand send failed」(:97);
  - 场景事件 → 「battle outbound scene event send failed」;
  - 对局结果 → 「battle result send failed」。
  - kafka_manager.cpp:46 / :77、kafka_producer.cpp:34 / :135 的判据文本没变。
- 影响修复设计:否。kafka_consumer.cpp:85-90 那段失实注释仍在原行。

D. e4ff1d165:其它脚本与文档
- run_cpp_tests.ps1 只改了 :72、:196-199 的文案;$projects(:43-68)仍然没有 kafka_command_test。
- start_game.ps1 只改文案,:551 / :587-588 / :260 / :282-283 都在原行。
- deploy/k8s/README.md 加了 103 行:game-events 行 1098→1195,迁移窗口 722→819。
- infra 清单、workflows、release_preflight.ps1(:445-446→:444-446)、build_linux.sh 与本项无关。
- 影响:只影响行号。

E. 2f897bd07:battle_node.{h,cpp} 改由 protogen 模板生成,带守护段
- 线程模型不变。报告里的「battle_node.cpp:102」改为 :137。
- 影响结论:否。影响修复设计:否。修复不碰这个文件;它现在是生成产物,按 AGENTS §3 只能改守护段内。

F. 其余提交(宠物、日志英文化、装备生成物与逻辑、两次合并、friend、文档)
- 不碰证据链。
- 898c803ea / 0b40715d0 补了装备生成物:equipaffixpool_table.h 已存在,player_bag.pb.h 有 has_equip。所以昨天「装备生成物缺失导致 110 个错误」可能已不成立,改为「未核实」。

G. 环境(不是提交)
- 唯一有实编证据的阻塞是 Boost:Codex 10-14 串行编 battle.vcxproj,在 EventLoop.h:18 报 C1083 boost/any.hpp(PROGRESS:7637)。
  - third_party/boost 在记录版本 c89e626,是模块化布局,根目录下没有 boost/ 头文件树。
  - infra.vcxproj:159 只挂了 third_party/boost/ 根,而 kafka_manager.cpp:4-5 直接包含 muduo/net/EventLoop.h 和 boost/algorithm/string/join.hpp。所以步骤 2 要编的 infra 一定会撞上同一个错误。
  - 原来的本地 Boost 分叉 87db1d0d 和分卷归档(tools/patches/local-worktree-20260929.json:5-16)已随 10-13 的历史重写移除,tools/patches/boost/ 只剩 restore_local_source.py,本机也没有 Python。
- 本次调查期间主工作区又冒出 45 个他人未提交改动,而且还在变。我读取时契约测试从 1897 行变成 1901 行;未提交的 +4 行在 :987-990,给 $ProdEnv 补 MMORPG_LOGIN_QUEUE_TOKEN_SECRET。docs/PROGRESS.md 也在被追加。
  - 批 1 要动的文件、k8s_deploy.ps1、README 当前都与 HEAD 一致(逐个 git diff --quiet 确认过)。
  - 本报告所有行号都按 HEAD blob 取。


**失效的说法**:

1. 主线程交给我的环境说明「主工作区只剩两个子模块脏标记」本身也已过时。
- 现状:本次调查期间又出现 45 个他人未提交改动(C++、Go、proto、docs/PROGRESS.md、tools/scripts/tests/k8s_deploy_contract.tests.ps1 等),而且还在被写。
- 更正:实测一律在新建的 2f897bd07 worktree 里跑;落码前照旧先 `git status --short`,每个文件先 Read 再 Edit。
- 批 1 涉及的文件目前与 HEAD 一致:kafka_producer.h、kafka_manager.{h,cpp}、kafka_consumer.cpp、node.cpp、kafka_command_test.vcxproj、run_cpp_tests.ps1。

2. 昨天的「工作区不干净 / 并发会话正在改 k8s_deploy.ps1、契约测试、README」已失效。
- 出现在:handoff_corrections 第 12 条、risks【风险】第 2 条、fix_design 第 0 条、files_to_touch 的三个[批2]标注、changes_from_review 第 1、10 条。
- 那批改动(G2b 等)已随 e4ff1d165 入库。
- 现状:k8s_deploy.ps1 和 README 与 HEAD 一致。契约测试又有一处他人未提交改动(+4 行,:987-990,和 GroupID 无关)。
- 更正:批 2 的阻塞变成「契约测试文件被另一会话改动 + HEAD 上已有已知红用例」,不再是 G2b 未提交。

3. 行号口径
- 昨天的说法「k8s_deploy.ps1 / 契约测试按 HEAD blob,磁盘后移 341 行」作废;HEAD 本身已经前进。
- k8s_deploy.ps1 对照(旧 → 新):
  - :267 → :280
  - :1603-1606 / :1622-1632 / :1654-1657 → :2035-2038 / :2059-2069 / :2087-2089
  - :1677 → :2109;:1685-1687 → :2117-2119;:1699-1700 → :2131-2132;:1719-1727 → :2149-2160
  - :1790 → :2222;:1825-1834 → :2257-2266;:1826-1827 → :2258-2259;:1830 → :2262
  - :2049 → :2481;:2063-2067 → :2494-2499;:2124 / :2179 → :2556
  - :5674 → :6362;:5696 → :6384;:5744 → :6432;:5748-5749 → :6436-6437
  - 新增引用:Get-AuthoritativeScalar 在 :572-584。
- 契约测试::1090-1095 → :1143-1148;:1096-1098 → :1149-1151;:1093 → :1146;:1229 → :1282。:20-21 和 :33-40 不变。
- 其它文件:
  - deploy/k8s/README.md:722-724 → :819;:1098(磁盘 1189)→ :1195;
  - release_preflight.ps1:445-446 → :444-446;
  - battle_node.cpp:102 → :137。
- 「补丁在 73b852f32 与 0aa1027bc 上都能套」作废。run_cpp_tests.ps1 在区间内改过文案,kafka_consumer.cpp 在 :218-240 改过文案,两份补丁一律按 2f897bd07 重新生成。
- 其余 C++ 行号仍然有效:有的区间内只改了日志、行数不变,有的根本没改。
- bin/etc、start_game.ps1、dev_tools.ps1、deploy_capture.ps1、release_common.ps1、kafka_command_test.vcxproj、game.sln 的引用也仍然有效。

4. 编译阻塞原因
- 「子模块全未初始化,librdkafka / grpc 是空目录」→ 主工作区 19 个已初始化 14 个,未初始化的是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash。librdkafka 已检出,rdkafkacpp.h 存在。grpc 已检出,但两处都没有 install_vs2026_dbg。f452 本轮未复核。
- 「没有 lib/、build/」→ 两个目录现在都在,但 lib\ 只有 battle.pdb,build\ 只有 intermediate 和 protoc-gen-go.exe。没有任何 .lib、节点 exe 或测试 exe。bin\ 仍只有 README.md、etc、logs。
- 「主干自 10-10 起整体编不过,110 个错误,装备生成物未产出(PROGRESS:7005;cpp/generated 里 grep 不到 has_equip)」→ 改为「未核实」。装备生成物已补,但没有新的编译证据。
- 当前唯一有实证的阻塞是 Boost 模块化布局造成的 C1083 boost/any.hpp(PROGRESS:7637)。kafka_manager.cpp:4-5 / infra.vcxproj:159 说明步骤 2 也会撞上它。
- fix_design 第 9 条「不用 game.sln 作通过标准,因为装备生成物未产出」→ 理由改为「Boost 布局阻塞 + 主干整体编译状态未知」。

5. worktree
- 「必须在干净的 0aa1027bc 检出里跑」→ 改为检出 2f897bd07。rev-parse 期望 2f897bd0780c1cc78284548284f20d01e61d7ea8,worktree 目录名改为 p0b-render-2f897bd07。
- 补充:不许动的 Codex worktree 除了 f452(73b852f32),还有 793e(0aa1027bc,数百个未提交改动)。

6. 步骤 1-3 的期望
- 「contract exit=0」→ HEAD 上至少「负向:zones.10zones.yaml 时打 MySQL连接总账超限 警告且不阻断」(:1649 / :1656 对 k8s_deploy.ps1:1238)会红。
- prod 档用例可能也会红(推断:有人正在 $ProdEnv 里补 MMORPG_LOGIN_QUEUE_TOKEN_SECRET)。
- 「既有问题」的基线改为 2f897bd07。

7. 日志判据文本
- 出现在:claim_evidence 五、handoff_corrections 第 6 条、步骤 3-5。
- 「battle 出站 GateCommand 发送失败」→「battle outbound GateCommand send failed」(battle_room_manager.cpp:97)。

8. confidence 里「C++ 与 bin/etc 在 73b852f32..HEAD 无差异」
- 作废:kafka_consumer.cpp、node_kafka_command_handler.h 和 battle 若干文件有改动。
- 但都只是日志文案、行数不变;kafka_manager.*、kafka_producer.*、node.cpp、node.h、node_entry.h、config.cpp、bin/etc 都没改。

9. entry_point_inventory【测试】「契约测试 :1090-1095 只钉了分区数和世代」
- 改为 :1143-1148,仍然没钉 GroupID。
- 「此刻正被并发会话改写」作废,但现在另有一处他人未提交改动(见第 1、2 条)。

10. risks【需要 Codex 实测确认】「可编译检出在哪」部分有了答案:Codex 10-14 的 battle 构建是在主工作区做的(lib\battle.pdb;PROGRESS:7637)。但主工作区此刻有他人的 C++ 半成品,不适合直接当构建检出。

11. java_parity 里 Java 仓库的 HEAD
- d942f74 → 现在是 52cccf2。
- BattleResultProperties.java:28 和 KafkaBattleResultSink.java:183 仍在原行;两个提交之间 game-battle 的相关文件没有变化;结论不变。

12. 仍然成立,无需修改的说法:
- C++ 测试没有 CI(cpp-build-ci.yml 不跑测试);
- kafka_command_test 已在 game.sln:231;
- 节点 Pod 模板不带 node-config 哈希,改 ConfigMap 不会自动滚 Pod(只有 cpp-log-sidecar-config-hash,:2354 / :2936,它只哈希 sidecar 配置,:2674-2682);
- deploy/k8s/AGENTS.md:38、godb 附录 :170 / :262、交接 :68、PROGRESS:7340 在区间内没变。

### 受阻步骤与先写的测试

步骤 2(2-1~2-3,回答判据 B)受阻
- 阻塞原因:
  1. Boost 是模块化布局。third_party/boost 在 c89e626,根目录没有 boost/ 头文件树。infra.vcxproj:159 和测试工程 vcxproj:116-133 只挂了 third_party/boost/ 根,而 kafka_manager.cpp:4-5 要 muduo EventLoop.h(:18 需要 boost/any.hpp)和 boost/algorithm。结果必然 C1083,Codex 10-14 在 battle.vcxproj 上已实证(PROGRESS:7637)。原来的本地分叉 87db1d0d 和恢复分卷已随 10-13 的历史重写移除。
  2. third_party/grpc/install_vs2026_dbg 不存在。protobuf / gtest / abseil 等库和 zlibd.dll 都在这里;rdkafka.lib / rdkafka.dll 的来源仓库内找不到,需 Codex 先确认。
  3. lib\ 下没有任何 .lib。
  4. 红态代码还没写。
- 解除条件:步骤 0 全部 True,并且按 2f897bd07 生成的红补丁已就位。
- 由谁解除:
  - 1~3 需负责人先拍板授权,再由 Codex 修环境。这属于修改构建环境、长时间构建,可能下载,见 AGENTS §10.2。可用的入口有 dev_tools.ps1 -Command third-party-grpc-build(:1417 → tools/scripts/third_party/build_grpc.ps1);Boost 头文件树怎么补,仓库里没有现成脚本,需 Codex 先确认做法。
  - 第 4 项由 Claude 写,何时写由负责人定。
- 独立 GTest 路线绕不开:本项用例必须链接 KafkaManager / KafkaProducer 及其依赖,不是零依赖逻辑。

步骤 3(3-1~3-5,回答判据 C,可选旁证)受阻
- 阻塞原因:没有 battle.exe(bin\ 只有 README.md、etc、logs);编 battle 节点卡在同一个 Boost C1083;运行期 DLL 应在 install_vs2026_dbg\bin(start_game.ps1:551),但这个目录不存在。3-5 另外需要 gate / scene exe、robot.exe、Go 服务和 Java 网关 jar。
- 解除条件:battle 节点能编出,且步骤 0 里 battle.exe 和两个 DLL 都为 True。
- 由谁解除:负责人授权后由 Codex 修环境。建议负责人同意无限期延后,不要单独为它修环境。

**实测前要先写的代码**:

第 1 项【步骤 2 必需,红态测量仪器】
交付方式:Claude 按 2f897bd07 生成补丁 p0b-red.patch,再加一个新文件,放在草稿目录。不 commit,也不落到主工作区(主工作区此刻有他人改动,又是 Codex 的构建检出)。

a) 改 cpp/libs/engine/infra/messaging/kafka/kafka_producer.h
- 在 `initialized()`(:98)旁边加:`bool brokersConfigured() const { return producer_ != nullptr || !pendingBrokers_.empty(); }`。
- 注释写「已登记 broker(句柄可能尚未惰性创建)。单测与诊断用」。
- 内联 const 函数,不加数据成员,类布局不变。文件带 BOM,保持不变。

b) 新建 cpp/tests/kafka_command_test/kafka_manager_init_test.cpp
- 4 个用例:
  - KafkaManagerInit.ProducerBrokersConfiguredWhenGroupIdIsEmpty
  - KafkaManagerInit.ProducerBrokersConfiguredWhenGroupIdIsSet
  - KafkaManagerInit.EmptyBrokersRejectedWhenGroupIdIsEmpty
  - KafkaManagerInit.EmptyBrokersRejectedWhenGroupIdIsSet
- 每个用例在新的 std::thread 里:构造 KafkaManager,调 Init,再读 KafkaProducer::Instance().brokersConfigured()。用新线程是因为生产者是 thread_local(kafka_producer.h:100-101)。
- broker 用 127.0.0.1:1,组名用 game-consumer-group。不读墙钟,不连外部服务。析构时的 flush 在没有句柄时直接返回 true(kafka_producer.cpp:341-344)。
- 存成带 BOM 的 UTF-8,字符串字面量保持 ASCII(该工程 Debug|x64 没有 /utf-8,vcxproj:116-133)。main() 沿用 kafka_command_test.cpp 里现有的。
- 完整代码见昨天报告 fix_design 第 5a 条,对 2f897bd07 不需要改。
- 在 HEAD 上的预期:IsSet 那条红(kafka_manager.cpp:57-63 → :76-79);EmptyBrokers…IsEmpty 那条红(:50-54);另两条绿。

c) 改 cpp/tests/kafka_command_test/kafka_command_test.vcxproj
- ItemGroup(:160-163)加一行 `<ClCompile Include="kafka_manager_init_test.cpp" />`。
- Debug|x64 的 AdditionalDependencies(:138)去掉 `rdkafka++.lib;`。理由:battle.vcxproj:142 只链 rdkafka.lib + infra.lib;预编译的 rdkafka++.dll 有 CRT 边界问题,见 kafka_consumer.h:3-13。

d) 改 tools/scripts/run_cpp_tests.ps1
- 在 $projects 的 'currency_test'(:53)和 'message_limiter_test'(:54)之间加:`'kafka_command_test'       = 'cpp\tests\kafka_command_test\kafka_command_test.vcxproj'`。
- 理由:不登记的话这个工程不在统一入口里,统一入口还负责同步运行期 DLL。

需要同步的工程文件:
- 不需要 .filters:该目录只跟踪 3 个文件。
- 不需要 CMakeLists:测试工程没有 CMake,build_linux.sh 只有 LIB_PROJECTS(:90)和 EXE_PROJECTS(:110);infra/CMakeLists.txt 也不涉及,因为不新增 infra 源文件。
- game.sln 已经登记过这个工程(:231)。

为什么算「实测」而不是「修复」:它们是判据 B 的测量仪器。按 AGENTS §11.4,修复前必须先在 HEAD 上跑红,证明缺陷在运行期能复现。这里不含任何修复语义:Init、node.cpp、kafka_consumer.cpp 一行不改,新加的访问器只读、不改行为。修复(fix_design 第 2、3、4 条)放在另一份 p0b-fix.patch 里,两份补丁的文件不重叠,可以叠加。

已知冲突:docs/handoff/godb-pipeline-handoff-20261013-appendix.md:262 也计划在同一个 ItemGroup 加一行。

现状:步骤 2 受阻,写好也只能算「未验证」。

第 2 项【可选,批 2 的回归测试,写好就能跑】
- 改 tools/scripts/tests/k8s_deploy_contract.tests.ps1。
- 新用例放在 :1151($battleInfraRun 的定义)之后、和现有 battle-node-config 用例相邻。对 node-config($devOut)和 battle-node-config($battleInfraRun.Output)各断言:
  - `data.base_deploy_config.yaml.Kafka.GroupID` 等于 `Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.GroupID'`;
  - 再各加一条 Assert-NotMatch … 'game-consumer-group'。
- 预期:在 2f897bd07 上红,批 2 之后绿。
- 运行:pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1。
- 不需要同步任何工程文件:deploy-config-tests.yml 会自动收集一级目录下的 .ps1。
- 为什么算实测:它把判据 A 变成可重复运行的先红后绿回归用例,本身不改生成器。
- 限制:这个文件此刻在主工作区有他人未提交改动(:987-990),HEAD 上也已有已知红用例。只在负责人批准批 2、并与那位作者排好先后之后再写。

### 待拍板

1. C++ 这一半按什么口径算「核实」
- 选项:
  - (i) 授权 Codex 修构建环境:补 Boost 头文件树(拿回分叉 87db1d0d,或从 c89e626 生成)、third-party-grpc-build 产出 install_vs2026_dbg、确认 rdkafka.lib / rdkafka.dll 的来源、编出依赖库,然后跑步骤 2 的红绿。会涉及下载和长时间构建。
  - (ii) 接受「步骤 1 的渲染结果 + 逐行静态证据链」作为本项口径。批 1 按「未编译,待 Codex 验证」交付,步骤 2 挂起,等环境修好后补跑。
  - (iii) 如果本机 docker 里已有 mmorpg-node 的 Linux 镜像,用它做真实进程旁证。镜像是否存在、tag 是什么,需 Codex 先确认;没有的话要构建镜像(耗时长)。
- 建议:本项先走 (ii)。同时把 (i) 作为独立事项由负责人排期:Boost 阻塞是所有 C++ 改动(本项、Redis AUTH、etcd 流重建、battle handler 生成)的共同前置,不该在本项里顺带修。

2. 红态代码(pre_measure_code 第 1 项)何时写、放哪里
- 建议:现在由 Claude 写成补丁,放草稿目录,不落主工作区、不 commit。环境就绪后由 Codex 先套红补丁跑红,再叠修复补丁跑绿。
- 如果希望直接作为未提交改动落到主工作区,需要负责人明确发话:主工作区此刻有他人改动,而且它就是 Codex 的构建检出。

3. C++ 构建检出放哪里
- 选项:(a) 主工作区,但要先等他人的 C++ 改动提交或搁置;(b) 新 worktree 加子模块初始化,可能要从远端拉取,属于下载。
- 建议:等 1 选了 (i) 之后再定;在那之前不要在主工作区套任何补丁。

4. 批 2 的时机(K8s 模板 GroupID 改为从 bin/etc 镜像,加契约测试和 README)
- 昨天「G2b 未提交」这个阻塞已解除,k8s_deploy.ps1 和 README 现在是干净的。但契约测试文件又被另一会话改动,HEAD 上也已有已知红用例。
- 选项:(a) 现在授权,与契约测试那位作者排定提交先后;(b) 等那批改动提交后再做。
- 建议:选 (a),理由有两点:
  - 模板里 GroupID 这一行改空,单独就能让旧二进制上的 battle 恢复;
  - 批 2 的契约测试红用例只要 pwsh 就能跑,是本项唯一能马上拿到完整红绿运行证据的部分。
- 上线时注意:
  - 旧 topic 的消费组换名带来的位点副作用;
  - 改 ConfigMap 不会自动滚 Pod:节点 Pod 模板只有 cpp-log-sidecar-config-hash 注解(k8s_deploy.ps1:2354 / :2936)。重启 Deployment 形态的 battle 会作废正在打的局。

5. Kafka.Brokers 为空时改为拒启(行为收紧)是否接受
- 建议接受(fail-closed,AGENTS §11.3)。本机配置和 K8s 模板都填了 Brokers(bin/etc/base_deploy_config.yaml:53-54;k8s_deploy.ps1:2258-2259)。

6. DisableLegacyPerNodeTopic 是否翻 true、怎么承载
- 和本缺陷没有因果关系。
- 建议另案处理,和开服交接 §4 第 7 项合并,不进本项的两批。

7. 代码交接方式
- 建议用补丁,不 commit。如果要改用一次本地提交(不 push),需要用户明确发话(AGENTS §9)。

8. 红用例能否碰生产头文件(加一个只读访问器)
- 建议允许。
- 退路:红用例只断言 Init 的返回值。代价是测的是代理量,不是缺陷本身(那个返回值本来就被 node.cpp:636 丢掉了)。

9. 步骤 3(判据 C)延后
- 建议同意延后到 battle 节点能编出来之后,不单独为它修环境。

10. 步骤 1-3 发现既有红用例时怎么处理
- 建议只记录,不在本项修。比如中文「MySQL连接总账超限」断言、prod 档缺 MMORPG_LOGIN_QUEUE_TOKEN_SECRET,这些属于 e4ff1d165 的后续修补。

### 原报告:证据链

结论:缺陷本身(根因 + 触发条件)在 HEAD 0aa1027bc 上静态证据链完整、成立,两份复核都没有推翻。交接对「后果」的描述有三处不准(不是静默、topic 名写错、漏了票据下发),所以仍判 partially_confirmed。全部结论来自只读阅读,没有任何运行证据。未特别标注的都是本轮逐行重读过的代码事实。

【行号口径】
- C++ 与 bin/etc 的行号在 HEAD 与磁盘一致:这些文件没有未提交改动,且 `git diff --stat 73b852f32 0aa1027bc -- cpp bin/etc` 为空。
- tools/scripts/k8s_deploy.ps1 与 tools/scripts/tests/k8s_deploy_contract.tests.ps1 的行号一律是「HEAD blob 行号」。磁盘上这两个文件带他人未提交改动:前者首个 hunk 是 `@@ -932,0 +933,341 @@`,此后整体后移 341 行以上(`GroupID: "game-consumer-group"` HEAD 1830 → 磁盘 2171;`function New-NodeConfigMapYaml` HEAD 1677 → 磁盘 2018);后者仍在被并发会话改写(本轮 04:26 看到 +33 行,04:31 看到 +300 行)。落码和实测一律按函数名 / 字符串定位。

【本轮重读范围】kafka_manager.{h,cpp}、kafka_producer.{h,cpp}、node_kafka_command_handler.h 全文;node.cpp 340-361 / 470-504 / 628-692 / 905-1034 / 1226-1262;kafka_consumer.h 1-60、kafka_consumer.cpp 60-139;bin/etc/base_deploy_config.yaml 全文;kafka_command_test.vcxproj、run_cpp_tests.ps1 全文;HEAD 版 k8s_deploy.ps1 的 502-515 / 1591-1657 / 1677-1730 / 1786-1836 / 2036-2082;契约测试 1-45 / 1086-1100 及其 lib。其余沿用原报告并经两份复核核对,未逐行重读的在文中标注。

一、根因链(battle,K8s 模板)
1. Init 只在 GroupID 为空时登记生产端 broker。cpp/libs/engine/infra/messaging/kafka/kafka_manager.cpp:45-55
   `if (config.group_id().empty()) {` … `KafkaProducer::Instance().setBrokers(brokers);` … `return true;`
2. GroupID 非空时走一次不带回调的 Subscribe。kafka_manager.cpp:57-63
   `return Subscribe(config, CollectTopics(config), config.group_id(), { static_cast<int32_t>(zoneId) });`
   回调缺省为空:kafka_manager.h:36 `KafkaMessageCallback callback = {},`
3. 无回调的 Subscribe 在登记 broker 之前返回。kafka_manager.cpp:76-79
   `if (!callback) { LOG_DEBUG << "KafkaManager: No message callback provided, skipping subscribe."; return false; }`
   登记在其后:kafka_manager.cpp:90 `KafkaProducer::Instance().setBrokers(brokers);`
   所以 Init 的非空分支恒返回 false、到不了 :90。blame:Init 调 Subscribe 来自 4b8d61d80,`if (!callback)` 来自 b136dfbfe,都是 2026-03-10;空组分支是 2026-06-01 的 9119e9568 加的。
4. 返回值被丢弃。cpp/libs/engine/core/node/system/node/node.cpp:634-637
   `kafkaManager.Init(tlsNodeConfigManager.GetBaseDeployConfig().kafka());`
   调用点唯一,来自 Node::Initialize(node.cpp:497),在 loop 线程(:477 `eventLoop->assertInLoopThread();`),早于 InitEtcdService(:498)。
5. battle 之后没有任何 Subscribe。cpp/nodes/battle/main.cpp:236 `// 刻意不声明 KafkaCommandType:battle 不消费 battle-{id} topic。`;只有声明了才装回调:node_entry.h:85-88 `if constexpr (… has_kafka_command_type<THooks>::value) { node.SetKafkaHandlers(…`;没装时放行:node.h:224 `return kafkaHandlersFn_ ? kafkaHandlersFn_(*this) : true;`。全仓没有子类覆写 RegisterKafkaHandlers(git grep 只有 node.h:224 与 node.cpp:961)。
6. 生产者没有 broker 时每次发送立即被拒。kafka_producer.cpp:90-94 `if (pendingBrokers_.empty()) return false;`;kafka_producer.cpp:133-138 `logFailure("[Kafka] Produce rejected (message NOT queued): producer unavailable" …); return RdKafka::ERR__STATE;`。全仓 setBrokers 只有 kafka_manager.cpp:52 与 :90 两个调用点(git grep)。
7. K8s 模板 GroupID 非空,battle 用同一份模板。k8s_deploy.ps1(HEAD):1830 `GroupID: "game-consumer-group"`(自 2026-03-15 的 861d37ccb 起就有);:5748-5749 `-ConfigName 'battle-node-config' -IncludeBattleSettings`;Pod 把 ConfigMap 整目录挂到 /app/bin/etc(HEAD:2063-2064),遮蔽镜像里的配置(HEAD:1685-1687 注释)。本机配置为空:bin/etc/base_deploy_config.yaml:75 `GroupID: ""`(2026-05-27 的 f430ab651 改空,当时没改 K8s 模板)。
8. 没有环境变量可以覆盖:config.cpp:204-207 只读 yaml;cpp/libs 与 cpp/nodes 下 grep 不到 KAFKA_ 环境变量。

二、gate / scene 不受影响(逐类)
- gate:cpp/nodes/gate/main.cpp:168 `using KafkaCommandType = contracts::kafka::GateCommand;`,:245 `node.SetKafkaHandlers(…` → node_kafka_command_handler.h:113-115 `node.RegisterKafkaMessageHandler({ commandTopic }, …, makeCallback(), { partition }, assignPolicy)` → node.cpp:673 带回调的 Subscribe → kafka_manager.cpp:90 登记 broker。gate 下没有生产调用点(git grep `KafkaProducer::Instance` 无命中)。
- scene:cpp/nodes/scene/main.cpp:68 `using KafkaCommandType = contracts::kafka::SceneCommand;`,同一路径在 StartRpcServer(node.cpp:914 断言 loop 线程,:961 注册)里登记 broker。scene 的生产点:player_lifecycle.cpp:3731、transaction_log_system.cpp:83、anomaly_detector.cpp:130、snapshot_system.cpp:101。
- 【推断】scene 四个生产点都在 loop 线程(只看了 kafka_producer.h:105 的注释,没有逐个调用栈核实)。KafkaProducer::Instance() 是 thread_local(kafka_producer.h:100-103),登记只对调用线程那一个实例有效。
- 同源副作用(交接未提;go/db 交接附录 :170 已记过):旧路径的组名取 Kafka.GroupID(node_kafka_command_handler.h:126 `std::string legacyGroupId = kafkaConfig.group_id();`),K8s 上全服 gate + scene 进同一个消费组 game-consumer-group,各自订阅自己的旧 topic(:124、:136),消费者开着 `allow.auto.create.topics`(kafka_consumer.cpp:126)。旧 topic 已无生产者(bin/etc/base_deploy_config.yaml:97-99 的说明;Go 侧 LegacyPerNodeTopic 只剩注释与测试引用),无功能影响,只有规模隐患。

三、legacy per-node topic 与 DisableLegacyPerNodeTopic
- 旧路径 = 每节点一个 topic `<类型>-<node_id>` + group 订阅;新路径 = `<类型>-cmd_g<N>` 的 node_id % P 分区,assign 不进组(node_kafka_command_handler.h:47-65)。
- 开关语义:node_command_route.h:44-47 `return …kafka().disable_legacy_per_node_topic();`;为 true 时注册完新路径就返回(node_kafka_command_handler.h:119-121)。
- 默认 false:proto3 bool(proto/common/base/config.proto 的 `disable_legacy_per_node_topic = 8`),config.cpp:226-229 只在键存在时赋值;K8s 模板 Kafka 段(HEAD:1825-1834)没有这个键;本机显式 false(bin/etc/base_deploy_config.yaml:104)。
- 这个开关只影响 gate / scene 的消费,和 battle 生产端有没有 broker 没有因果关系。

四、battle 的生产调用点(3 处,都走 KafkaProducer::Instance().send)
- battle_room_manager.cpp:93 → `gate-cmd_g<N>`(ResolveCommandRoute,:92),key=player_id;载荷是大厅公告回落:NotifyBattleAssigned(票据 + 地址,:615)与 NotifyBattleStart(:621)。
- battle_room_manager.cpp:170 → `scene-cmd_g<N>`,key=player_id;BattleConfirmedEvent(:625 首发)与 BattleSettlementEvent。
- battle_room_manager.cpp:292 → `match-results`(:286),key=battle_id。
- 失败处理:三处都只记 ERROR 后继续(:95-100、:172-178、:294-298),不向上返回。
- 发送线程:Handle* 经 runInLoop 投递(battle_node.cpp:102 等;battle_room_manager.cpp:1456 注释),与 Init 是同一个 thread_local 实例。
- 不变量遵守:gate 命令空实例号拒发(:74-79);确认事件带 target_instance_id;match-results 无目标实例语义(:283-285)。结算重投留空实例号等细节沿用原报告,本轮未重读。

五、后果(对交接的订正)
- 运行期不是静默:每次被拒都有 ERROR(kafka_producer.cpp:135,限流每 10s 前 20 条,kafka_producer.h:147)+ battle 侧 ERROR(`battle 出站 GateCommand 发送失败` 等)。K8s 上这些 ERROR 会经默认开启的日志 sidecar 进 Loki(HEAD:267 `$script:CppLogSidecarEnabled = -not $NoCppLogSidecar`;selector 在 HEAD:2179 附近)。
- 但没有任何主动暴露:Init 失败只有一行 LOG_DEBUG(kafka_manager.cpp:77),返回值被丢,启动横幅照常打印 `kafka: <brokers>`(node.cpp:988-1027);C++ 没有指标导出;deploy 与 tools/scripts 下 grep 不到针对 Produce rejected 的告警规则。准确说法:无指标、无探针、无告警;日志可查,但没人会主动查。
- 最重的后果是票据下发:CreateBattleResponse 只有 battle_id 与 error_message(proto/battle/battle_node.proto:37-40),首次 NotifyBattleAssigned 只走 Kafka→gate(battle_room_manager.cpp:613-615 注释「此刻还没有直连,经 Kafka→gate 回落下发」)。剩下的只有 match 的丢票补签(go/match/internal/logic/requestbattleticketlogic.go:54、:70);robot 的冒烟不走补签,只等 WaitBattleAssigned(robot/battle_direct_conn.go:76)。
- 结算有 Redis 兜底、活动局有 guild 巡检器;普通局评分与确认事件没有兜底(沿用原报告,本轮未重读)。
- 这是开服前的阻断项,不是已发生的线上事故:deploy/k8s/AGENTS.md:38 写明「K8s 路由模式下的 battle-smoke 是事后补验项」。

六、未核实
- 没有任何运行证据。
- 外部常识,未核实(third_party/librdkafka 是未初始化的子模块):librdkafka 建生产者不需连通、produce 只入本地队列;ERR__STATE 的数值;MSVC 的库搜索顺序。
- Linux 版 muduo 的 LogLevel 枚举顺序(third_party/muduo-linux 为空);Windows 分支是 TRACE=0 / DEBUG=1 / INFO=2(cpp/libs/engine/muduo_windows/src/muduo/base/Logging.h:19-29)。
- kafka_consumer.cpp 139 行之后未读。

### 原报告:对交接的更正

1. 行号。kafka_manager.cpp:45-52 / 56-63 / 76-79 / 90 与 node.cpp:634-637 在 HEAD 上都对(C++ 在 73b852f32..HEAD 无改动)。k8s_deploy.ps1:1830 只对 HEAD blob 成立,磁盘上因他人未提交改动已在 2171 行。交接 :7 自己写了「落码前按函数名重新定位」,照办。

2. 「GroupID 非空就跳过 setBrokers」不够准确。实际是:非空分支调了一次不带回调的 Subscribe,必然在 kafka_manager.cpp:76-79 返回 false,到不了 :90。按 blame,至少自 2026-03-10 起这个分支恒返回 false(更早的历史没追)。

3. 「发往 match-rating」写错。topic 是 `match-results`(battle_room_manager.cpp:286);match-rating 是 match 的消费组名。

4. 「scene / gate 的结算和确认命令」不准。scene 收的是 BattleConfirmedEvent + BattleSettlementEvent;gate 收的是大厅公告回落(NotifyBattleAssigned 票据、NotifyBattleStart),不是结算。

5. 漏了最重的后果:首次票据 NotifyBattleAssigned 只走 Kafka→gate,生产者没 broker 时客户端收不到票据和 battle 地址,正常开局路径断掉。

6. 「而且静默」不成立。运行期每次被拒都有 ERROR,K8s 上还会经默认开启的日志 sidecar 进 Loki。准确说法:启动期只有一行 DEBUG、返回值被丢、横幅照常;运行期只有日志,没有指标、探针、告警规则。

7. 「全部被拒」对投递成立,但结算不等于丢:有 Redis 待结算记录与 scene 进场景钩子兜底,活动局结果有 guild 巡检器。无兜底的是普通局评分和确认事件。

8. 「先在 kind 上实测」对模板这一半没必要:渲染有现成的 DryRun 契约测试入口(tools/scripts/tests/lib/deploy_capture.ps1 的 Invoke-DeployDryRun),不部署、不连集群。C++ 行为这一半的运行证据目前卡在编译环境上(见 risks),不是卡在有没有 kind。

9. 「模板 GroupID 改空」单独就能止血,旧二进制也生效(走 Init 的空组分支),交接没点明。但有两点要补:这个文件此刻带他人未提交改动,不能现在动;改空等于给 gate / scene 的旧路径换消费组名,有位点副作用(见 fix_design 批 2)。

10. 「同时加 DisableLegacyPerNodeTopic: true」和本缺陷没有因果关系。它是控制面 topic 迁移的第 4 步,bin/etc/base_deploy_config.yaml:100-101 明写「由运维显式翻」,需要单独拍板、单独提交。

11. 漏了同源副作用:K8s 上全服 gate + scene 因同一个 GroupID 进同一个消费组订阅旧 topic(node_kafka_command_handler.h:126)。go/db 交接附录 :170 已把它记为「建议另开一项」。

12. §9 的状态要这样读:§9 点名的那一批(k8s_deploy.ps1、docker-compose.yml、loginqueue、kafka_offset_reset.ps1、shared-redis-split-by-purpose.md)已随 d5cbb8a8e 提交(`git show --stat d5cbb8a8e` 证实)。但之后工作区又出现新的他人未提交改动,而且有并发会话正在写:本轮 04:31 的 `git status --short` 有 15 个已改文件、5 个未跟踪文件,其中包括 tools/scripts/k8s_deploy.ps1(+431 行,G2b 的 MySQL 连接总账)、tools/scripts/tests/k8s_deploy_contract.tests.ps1(几分钟内从 +33 行涨到 +300 行)、deploy/k8s/README.md、docs/PROGRESS.md。§9 的「完整清单以 git status 为准」仍然有效。原报告「工作区干净,§9 已过时」的说法作废。

13. 顺带发现(另案,不在本项修):日志级别口径三处不一致。K8s 模板 `LogLevel: 1`(HEAD:1790);bin/etc/base_deploy_config.yaml:25 注释写「0=DEBUG,1=INFO」;tools/scripts/release_preflight.ps1:445-446 按「0=DEBUG」把下限设成 1。而 node.cpp:1232-1234 直接 static_cast 成 muduo 枚举,Windows 分支里 1 是 DEBUG。Linux 枚举未核实。
    - (2026-10-22 已处理,未执行,待 Codex 验证)三处已统一到 muduo 枚举:K8s 模板改为从 bin/etc 镜像(默认 2 = INFO,不再写死 1),预检下限改为 2,bin/etc 注释改正。Linux 构建用的 third_party/muduo-linux 是上游 chenshuo/muduo 子模块(tools/archived/muduo_linux_overlay 不覆盖 Logging.h),上游枚举顺序同为 TRACE / DEBUG / INFO / WARN / ERROR / FATAL(本机子模块未检出,按上游源码,未在本机核对)。见 PROGRESS 同日条目。

### 原报告:同类入口盘点

k8s_deploy.ps1 与契约测试的行号都是 HEAD blob 行号,磁盘上已后移,按函数名定位。

【代码】
- 登记 broker(创建):kafka_manager.cpp:49-53(Init 空组分支)、:90(Subscribe)。全仓只有这两处调 setBrokers。
- 读取 / 惰性建生产者:kafka_producer.cpp:90-94 ensureInitialized ← send() :133。
- 更新 / 恢复:rebuildAfterFatal 沿用 brokers_(kafka_producer.cpp:221、:253),重建失败回填 pendingBrokers_(:260)。
- 销毁:~KafkaProducer(kafka_producer.cpp:96-104);KafkaManager::Shutdown(kafka_manager.cpp:179-197)。
- Init 调用点:node.cpp:636(唯一)。Subscribe 调用点:kafka_manager.cpp:58(恒失败的分支)、node.cpp:673 ← node_kafka_command_handler.h:113(新路径)与 :136(旧路径)。
- 生产调用点 8 处:battle_room_manager.cpp:93 / :170 / :292;player_lifecycle.cpp:3731;transaction_log_system.cpp:83;anomaly_detector.cpp:130;snapshot_system.cpp:101;kafka_manager.cpp:124(Publish,无调用者)。
- 节点类型:cpp/nodes 下三类可执行节点。gate 只消费;scene 消费 + 生产;battle 只生产(唯一受影响)。另有 _template 示例(main.simple.cpp.example:14、main.with_context.cpp.example:19 都声明 KafkaCommandType,照模板建的节点不受影响;只生产不消费的新节点会重蹈 battle 的覆辙)。
- Kafka.GroupID 的读者:kafka_manager.cpp:45 / :61 / :87;node_kafka_command_handler.h:126;config.cpp:204-207。
- 修复后失去读者的配置键:Kafka.Topics(只被 CollectTopics 读,kafka_manager.cpp:22-29)、EnableAutoCommit、AutoOffsetReset(cpp/libs 与 cpp/nodes 下 grep 不到读者;消费端按 assignPolicy 写死,kafka_consumer.cpp:112-113)。
- 修复后失实的注释:kafka_consumer.cpp:85-90(「eager Node::InitKafka() pass」「node_kafka_command_handler.h:150」;该头文件现在 178 行,相关逻辑在 :124-129);bin/etc/base_deploy_config.yaml:60-61 同样引用 :150。
- 潜伏的同类入口:KafkaProducer::Instance() 是 thread_local(kafka_producer.h:100-103),非 loop 线程上的 send 拿不到 broker。现有调用点【推断】都在 loop 线程。

【配置模板】
- 本机:bin/etc/base_deploy_config.yaml:52-104(GroupID 空、DisableLegacyPerNodeTopic false、LogLevel 2)。配置路径写死 `etc/base_deploy_config.yaml`(node.cpp:1245),没有环境变量可换。
- K8s:k8s_deploy.ps1 的 New-NodeConfigMapYaml(HEAD:1677;Kafka 段 HEAD:1825-1834)。同一函数生成 zone 的 node-config 和 battle-node-config(HEAD:5748-5749)。
- 逐字段对比(本机 → K8s):Brokers `localhost:9092` → `kafka.${InfraNamespace}:9092`;Topics 相同;**GroupID `""` → `"game-consumer-group"`(触发缺陷的唯一字段)**;CommandTopicPartitions / Generation 从 bin/etc 镜像(HEAD:1699-1700);EnableAutoCommit / AutoOffsetReset 相同;DisableLegacyPerNodeTopic `false` → 缺键(等效 false);LogLevel 2 → 1。
- 归档:tools/archived/debug_gate.sh 也写着 game-consumer-group(不在用;原报告结论,本轮未重读)。

【部署脚本】
- k8s_deploy.ps1:New-BattleWorkloadManifest(HEAD:5674)、Apply-BattlePool(HEAD:5696)、Apply-Infra(HEAD:5744)。
- Pod 模板:workingDir /app/bin,ConfigMap 挂 /app/bin/etc,日志卷挂 /app/bin/logs(HEAD:2049、:2063-2067)。
- 日志 sidecar 默认开启(HEAD:267),读 /app/bin/logs/cpp_nodes/*.log 推 Loki(HEAD:2124)。
- C++ 节点 Pod 模板没有 node-config 的哈希注解:改 ConfigMap 不会自动滚 Pod(原报告结论,本轮未重读)。
- DryRun 不调用 kubectl:Invoke-Kubectl 与 Invoke-KubectlWithInputFile 在 DryRun 下只打印(HEAD:1603-1606、:1622-1632),Ensure-KubectlAvailable 直接返回(HEAD:1654-1657)。
- 本机:tools/scripts/start_game.ps1:587-588(battle,RPC_PORT=20010);tools/scripts/cpp_nodes.ps1:395(工作目录 bin);tools/scripts/dev_tools.ps1:1449-1450(cpp-node-start / cpp-node-stop)、:1484(dev-stop)。
- Linux 构建:tools/scripts/build_linux.sh 只有 LIB_PROJECTS(:90)与 EXE_PROJECTS(:110),不含测试工程。K8s 上实际运行的 Linux 二进制没有单测路径。

【docker-compose】
- deploy/docker-compose.yml 不含 C++ 节点配置。:180-183 的注释写「Kafka.GroupID is empty, so KafkaManager::Init skips the top-level subscribe」,修复后措辞过时(本项不改)。
- 容器名 kafka / redis / etcd(:7、:219、:451)。

【测试】
- cpp/tests/kafka_command_test:现有两个源文件(kafka_command_test.vcxproj:161-162),都不引用 KafkaManager / KafkaProducer(git grep cpp/tests 只命中一行注释)。
- 该工程已在 game.sln 的 Debug|x64 构建配置里(game.sln:231、:516-517),但不在 tools/scripts/run_cpp_tests.ps1 的清单里(:43-68),仓库也没有 C++ 测试的 CI。
- 链接行(vcxproj:138)同时带 rdkafka.lib、infra.lib 和预编译的 rdkafka++.lib,还带 scene.lib、modules.lib、table.lib、battle.lib 等。Debug|x64 编译选项没有 /utf-8(vcxproj:116-133)。
- 运行期 DLL:run_cpp_tests.ps1:120-129 每次运行前把 zlibd.dll / rdkafka.dll / rdkafka++.dll 拷进 build/cpp/tests;缺了会以 0xC0000135 静默退出(:17-19)。
- tools/scripts/tests/k8s_deploy_contract.tests.ps1:HEAD:1090-1095 只钉了 CommandTopicPartitions / Generation,没钉 GroupID;HEAD:1096-1098 有现成的 battle-node-config 渲染。CI 入口 .github/workflows/deploy-config-tests.yml(ubuntu-latest,pwsh)。这个测试文件此刻正被并发会话改写。
- 端到端:robot/etc/battle_smoke.yaml(:3-6 用法,:35-37 收到 NotifyBattleAssigned 才建直连);结果标记 BATTLE_SMOKE_OK / BATTLE_SMOKE_FAIL(robot/battle_smoke_scenario.go:277、:124)。

【文档】
- 交接 docs/handoff/handoff-global-singletons-10m-ccu-20261013.md:68;docs/PROGRESS.md:7340。
- bin/etc/base_deploy_config.yaml:57-75(GroupID 说明)、:94-104(迁移开关)。
- deploy/k8s/README.md:HEAD:722-724(迁移窗口)、HEAD:1098(磁盘 1189,game-events 行写「Kafka.GroupID 为空」,对 K8s 模板不成立)。
- deploy/k8s/AGENTS.md:38(K8s battle-smoke 是事后补验项,补验前不要写「已验证可用」)、:48-50(kind 的固定用法)。
- docs/design/control-plane-topic-partitioning-20260908.md(迁移顺序,原报告引用 :242-258,本轮未重读)。
- docs/handoff/godb-pipeline-handoff-20261013-appendix.md:170(已把 K8s 模板的 GroupID 常数记为同类问题)、:262(计划在同一个 vcxproj 加一行)。
- .github/copilot-instructions.md 仍按旧的 `{type}-{node_id}` topic 描述(另案,本轮未重读)。

### 原报告:修复设计

> **2026-10-15 批 2 落码说明(以此为准,下面的设计原文保留作推导记录)**:负责人「全部做完给我」,批 2 与原列「另案」的第 13 条一起落码,未运行任何测试。
> - `tools/scripts/k8s_deploy.ps1` 的 `New-NodeConfigMapYaml`:`GroupID` 改为 `Get-AuthoritativeScalar ... 'Kafka.GroupID'` 镜像 bin/etc(空串照常读出;另加字符集校验,值含引号 / 反斜杠 / 空白等直接 throw);Kafka 段新增 `DisableLegacyPerNodeTopic: true`(**模板常数,不镜像 bin/etc**,本机那份仍是 `false`、由运维显式翻)。两者同批,理由即第 10 条的副作用二:只改组名不关旧路径,旧路径换到没有位点的新组会从 earliest 重放旧 topic。
> - 第 13 条的前提(无生产者写旧 topic)2026-10-15 全仓静态 grep 复核成立:C++ 只有 battle 发命令且走 `ResolveCommandRoute`,`LegacyPerNodeTopic()` 只剩单测引用;Go 的 login / scene_manager / player_locator / match / friend / guild 全部经 `shared/kafkacmd`、`shared/kafkautil`,chat / trade / team / client_rpc_router 不连 Kafka;`java/gateway_node` 不连 Kafka。核实依据与上线影响见 `deploy/k8s/README.md`「控制面命令 topic」一节的「K8s 已关老路径」条。取值待实测 / 压测复核。
> - 契约测试:`k8s_deploy_contract.tests.ps1` 紧跟 `$battleInfraRun` 定义新增一例,对 node-config 与 battle-node-config 断言 GroupID == bin/etc、不含 `game-consumer-group`、`DisableLegacyPerNodeTopic` 为 `true`。修复前红、修复后绿,待 Codex 跑。
> - 文档:README 迁移窗口条与保留期现状表的 `gate-<id>` / `game-events` 两行、bin/etc 的 GroupID / 迁移开关注释(只改注释)、`deploy/docker-compose.yml` 的 game-events 注释、`kafka_manager.cpp` 与 `kafka_manager_init_test.cpp` 里「至今写死」的注释。

目标:生产端 broker 的登记不再依赖任何消费侧配置;Brokers 配置为空在启动时拒绝;K8s 模板不再自带与本机不同的 GroupID 常数。不改协议、不改持久化数据、不动 generated/、不新增宏;只多存一个字符串,不涉及连接或回调的生命周期(AGENTS §11.7 不适用)。

【0. 分批(硬约束)】
- 批 1(现在可落):只动 C++ 与测试。这些文件此刻没有他人未提交改动;落码前再跑一次 `git status --short -- cpp tools/scripts/run_cpp_tests.ps1` 确认为空。批 1 单独就能修好 battle:新二进制在旧模板下也会登记 broker(见第 7 条的矩阵)。
- 批 2(延后):tools/scripts/k8s_deploy.ps1、tools/scripts/tests/k8s_deploy_contract.tests.ps1、deploy/k8s/README.md。这三个文件此刻带他人未提交改动,其中契约测试正被并发会话改写。等 G2b 提交、或负责人明确授权后再做;届时按函数名和字符串重新定位,不用本文的 HEAD 行号。
- 另案(单独拍板、单独提交):DisableLegacyPerNodeTopic 是否翻 true、用镜像还是部署参数承载。
- 顺序:步骤 1(渲染)→ 红补丁交 Codex 跑红 → 修复补丁叠上去跑绿 → 落 docs/PROGRESS.md。

批 1 ────────────────────────

【1. kafka_producer.h:加只读访问器】cpp/libs/engine/infra/messaging/kafka/kafka_producer.h,放在 `initialized()`(:98)旁边
```cpp
// 已登记 broker(句柄可能尚未惰性创建)。单测与诊断用。
bool brokersConfigured() const { return producer_ != nullptr || !pendingBrokers_.empty(); }
```
内联 const 函数,不加数据成员,类布局不变;没重编的库(modules、scene)与新头文件二进制兼容。该文件是带 BOM 的 UTF-8,保持不变。

【2. kafka_manager.cpp / .h:Init 无条件登记】
```cpp
bool KafkaManager::Init(const KafkaConfig& config) {
	// 生产端 broker 与本节点有没有消费者无关,无条件登记。
	// setBrokers 只存字符串,librdkafka 句柄仍在第一次 send() 时惰性创建。
	// 2026-10 之前这里只在 Kafka.GroupID 为空时登记;非空时走一次不带回调的 Subscribe,
	// 必然在登记之前返回 false。只生产不消费的节点(battle)在 GroupID 非空的部署里
	// 因此拿不到 broker,每次 send() 都被拒(K8s node ConfigMap 曾写死 game-consumer-group)。
	const std::string brokers = JoinBrokers(config);
	if (brokers.empty()) {
		LOG_ERROR << "KafkaManager::Init failed: Kafka.Brokers is empty.";
		return false;
	}
	KafkaProducer::Instance().setBrokers(brokers);
	// 没有顶层默认订阅:消费一律经 Node::RegisterKafkaMessageHandler -> Subscribe。
	return true;
}
```
- 删掉恒失败的分支(原 :57-63)和只被它用的 CollectTopics(:22-29)。include 不动(无法编译验证,避免连带)。
- Subscribe 里的 :90 保留:幂等,零行为风险。代价是 gate / scene 启动时 `brokers stored for lazy init` 会打 2~3 次。
- kafka_manager.h 给 Init 补契约注释:必须在将来调用 send() 的线程上调用(生产者是 thread_local);返回 false 仅当 Kafka.Brokers 为空。
- 日志只含 broker 地址;KafkaConfig 没有任何凭据字段(proto/common/base/config.proto 的 KafkaConfig 共 8 个字段),不会泄密。
- 编码:kafka_manager.cpp 现在是纯 ASCII、无 BOM;infra 工程带 /utf-8(infra.vcxproj:164),中文注释可以写。

【3. node.cpp:检查返回值,fail-closed】cpp/libs/engine/core/node/system/node/node.cpp
在匿名命名空间里 FatalClientEndpoint(:355-361)旁边加同样写法的函数,再改 InitKafka(:634-637):
```cpp
// K8s 下 LOG_FATAL 来不及刷异步日志,先同步写一行 stderr(同 FatalClientEndpoint)。
[[noreturn]] void FatalKafkaConfig(const std::string &reason)
{
	std::fprintf(stderr, "FATAL kafka config: %s\n", reason.c_str());
	std::fflush(stderr);
	LOG_FATAL << "Kafka config: " << reason;
	std::abort(); // LOG_FATAL 已 abort;这一行只为让 [[noreturn]] 成立
}

void Node::InitKafka()
{
	// 三类节点都离不开 Kafka:gate / scene 收控制面命令,scene 发存盘 DBTask,
	// battle 发票据公告 / 确认 / 结算 / 对局结果。带着发不出消息的生产者启动 = 存盘与结算丢失。
	if (!kafkaManager.Init(tlsNodeConfigManager.GetBaseDeployConfig().kafka()))
	{
		FatalKafkaConfig("Kafka.Brokers is empty in etc/base_deploy_config.yaml; refusing to start");
	}
}
```
失败语义:
- Brokers 为空 → 拒启。玩家资产与存盘路径,AGENTS §11.3 默认 fail-closed;与 node.cpp:961-964 的 RegisterKafkaHandlers 失败是同一惯用法。
- broker 填了但连不上 → 不拒启,维持现状:librdkafka 后台重连,失败有 ERROR 与计数。
- 不在本项做:运行期生产者不可用时让 battle 拒绝建房。那是新的准入策略,另立项。

【4. kafka_consumer.cpp:订正失实注释(只改注释)】cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp:85-90
现文写的是「Callers were hitting this during the eager Node::InitKafka() pass … (see node_kafka_command_handler.h:150)」。修复后 Init 不再调 Subscribe,且该头文件只有 178 行。改成:空 group.id 只可能来自直接调用 Subscribe 时两处都没给组名;旧路径的组名回落见 node_kafka_command_handler.h 的 RegisterKafkaCommandHandler。该文件是带 BOM 的 UTF-8,保持不变。

【5. 回归测试】
5a. 新增 cpp/tests/kafka_command_test/kafka_manager_init_test.cpp。存成带 BOM 的 UTF-8(该工程 Debug|x64 没有 /utf-8,vcxproj:116-133),字符串字面量保持 ASCII;main() 沿用 kafka_command_test.cpp:288 的。
```cpp
#include <gtest/gtest.h>
#include <string>
#include <thread>

#include "messaging/kafka/kafka_manager.h"
#include "messaging/kafka/kafka_producer.h"
#include "proto/common/base/config.pb.h"

namespace {

struct InitOutcome { bool initOk = false; bool producerHasBrokers = false; };

KafkaConfig MakeConfig(const std::string& broker, const std::string& groupId) {
    KafkaConfig config;
    if (!broker.empty()) { config.add_brokers(broker); }
    config.set_group_id(groupId);
    return config;
}

// KafkaProducer::Instance() 是 thread_local:每个场景放进独立线程,互不沾染。
// 只走 setBrokers,不创建 librdkafka 句柄,不连 broker。
InitOutcome RunInitOnFreshThread(const KafkaConfig& config) {
    InitOutcome outcome;
    std::thread worker([&outcome, &config] {
        KafkaManager manager;
        outcome.initOk = manager.Init(config);
        outcome.producerHasBrokers = KafkaProducer::Instance().brokersConfigured();
    });
    worker.join();
    return outcome;
}

} // namespace

// 本机形态(GroupID 空):修复前后都绿,作对照。
TEST(KafkaManagerInit, ProducerBrokersConfiguredWhenGroupIdIsEmpty) {
    const auto o = RunInitOnFreshThread(MakeConfig("127.0.0.1:1", ""));
    EXPECT_TRUE(o.initOk);
    EXPECT_TRUE(o.producerHasBrokers);
}
// K8s + battle 形态(GroupID 非空、无消费回调):修复前红。
TEST(KafkaManagerInit, ProducerBrokersConfiguredWhenGroupIdIsSet) {
    const auto o = RunInitOnFreshThread(MakeConfig("127.0.0.1:1", "game-consumer-group"));
    EXPECT_TRUE(o.initOk);
    EXPECT_TRUE(o.producerHasBrokers);
}
// 空 Brokers + 空 GroupID:修复前红(HEAD 返回 true,kafka_manager.cpp:51-54)。
TEST(KafkaManagerInit, EmptyBrokersRejectedWhenGroupIdIsEmpty) {
    const auto o = RunInitOnFreshThread(MakeConfig("", ""));
    EXPECT_FALSE(o.initOk);
    EXPECT_FALSE(o.producerHasBrokers);
}
// 空 Brokers + 非空 GroupID:修复前后都绿(修复前是因为别的原因返回 false),守住修复后的语义。
TEST(KafkaManagerInit, EmptyBrokersRejectedWhenGroupIdIsSet) {
    const auto o = RunInitOnFreshThread(MakeConfig("", "game-consumer-group"));
    EXPECT_FALSE(o.initOk);
    EXPECT_FALSE(o.producerHasBrokers);
}
```
确定可重复:不读墙钟、不连外部服务。HEAD 的非空分支会读 `tlsNodeConfigManager.GetGameConfig().zone_id()`(kafka_manager.cpp:57),它是 thread_local 且可默认构造(node_config_manager.cpp:3),新线程里安全。

5b. cpp/tests/kafka_command_test/kafka_command_test.vcxproj:
- ItemGroup(:160-163)加一行 `<ClCompile Include="kafka_manager_init_test.cpp" />`。没有 .filters 文件,没有 CMake 要同步。
- Debug|x64 的 AdditionalDependencies(:138)去掉 `rdkafka++.lib;` 这一项。理由:这个 exe 第一次引用 RdKafka C++ 包装层;包装层随 infra 从源码编译(infra.vcxproj:39-51),预编译的 rdkafka++.dll 有 CRT 边界问题(kafka_consumer.h:3-13)。battle 节点就是只链 rdkafka.lib + infra.lib(cpp/nodes/battle/battle.vcxproj:142),2026-09-05 对 bag_test 等三个测试也是这么改的(docs/PROGRESS.md:4181-4182)。

5c. tools/scripts/run_cpp_tests.ps1 的 $projects(:43-68)按字母序加一行:
`'kafka_command_test'       = 'cpp\tests\kafka_command_test\kafka_command_test.vcxproj'`
理由:这个工程不在统一入口里,仓库又没有 C++ 测试的 CI,不登记的话新回归用例以后没人跑;统一入口还自带运行期 DLL 同步。

【6. 交接给 Codex 的方式(不 commit)】
- 红态 = 第 1 条 + 第 5 条;修复 = 第 2、3、4 条。两组文件互不重叠,可以叠加。
- 落码会话在草稿目录生成两份补丁和一个新文件:
```powershell
git diff --binary -- cpp/libs/engine/infra/messaging/kafka/kafka_producer.h cpp/tests/kafka_command_test/kafka_command_test.vcxproj tools/scripts/run_cpp_tests.ps1 > <草稿目录>\p0b-red.patch
git diff --binary -- cpp/libs/engine/infra/messaging/kafka/kafka_manager.cpp cpp/libs/engine/infra/messaging/kafka/kafka_manager.h cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp cpp/libs/engine/core/node/system/node/node.cpp > <草稿目录>\p0b-fix.patch
Copy-Item cpp\tests\kafka_command_test\kafka_manager_init_test.cpp <草稿目录>\
```
- `git commit` 仅在用户明确要求时执行(AGENTS §9)。若用户更愿意用一次本地提交(不 push)来交接,由用户发话。
- 「先实测再落码」的落法:红补丁先只在 Codex 的检出里套用并跑红;主工作区在红结果出来之前不落修复(第 2、3、4 条)。

【7. 新旧并存与回滚】
- 旧二进制 + 旧模板:缺陷在。新二进制 + 旧模板:battle 恢复(批 1 的效果)。旧二进制 + 新模板:battle 恢复(批 2 的效果)。新二进制 + 新模板:恢复。两批互不依赖,先后都安全。
- 没有协议或数据变更,回滚就是各自回退。
- 唯一的行为收紧:Kafka.Brokers 为空的环境以前能起来、现在拒启。本机配置(bin/etc/base_deploy_config.yaml:53-54)与 K8s 模板(HEAD:1826-1827)都填了。

【8. 文档与登记(批 1)】
- docs/PROGRESS.md:落之前重新 Read(别的会话在写,且工作副本有 CRLF 提示),只在文件末尾另起一条。写明:改了什么;「未编译,待 Codex 验证」或贴 Codex 的红绿结果;批 2 因文件被占用而延后、K8s 模板此刻仍是 game-consumer-group;Linux 镜像构建与真实 K8s 部署未验证;「对应 Java 版本号:无(不适用)」。
- 措辞约束:没有 Codex 结果前不得写「编译通过」「已验证」(AGENTS §10.1);任何时候都不得写「K8s 上 battle 已验证可用」(deploy/k8s/AGENTS.md:38)。
- Java 仓库 PARITY.md 的登记见 java_parity。

【9. 修复后交 Codex 的命令】PowerShell 7,工作目录 = 步骤 0 确认的 `$b`,接在 codex_measurement 步骤 2 的红态之后。顺序执行,前一步红了就停。MSBuild 必须串行 /m:1。
```powershell
git -C $b apply --check "$s\p0b-fix.patch"
git -C $b apply "$s\p0b-fix.patch"
Set-Location $b
msbuild cpp\libs\engine\infra\infra.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\libs\engine\core\core.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\tests\kafka_command_test\kafka_command_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
pwsh tools/scripts/run_cpp_tests.ps1 -Filter kafka_command
.\build\cpp\tests\kafka_command_test.exe --gtest_output=xml:"$s\p0b-green.xml"
dumpbin /dependents build\cpp\tests\kafka_command_test.exe | Select-String rdkafka
```
通过标准:
- 三次 msbuild 都 0 error(core 这一步验证 node.cpp 能编过)。
- 汇总行 `kafka_command_test  OK  N/N 全过`,脚本退出码 0;XML 里 4 条 KafkaManagerInit 与原有的寻址、策略用例全部通过。
- dumpbin 只出现 `rdkafka.dll`。
不用 `msbuild game.sln` 作通过标准:docs/PROGRESS.md:7005、:7051 记录主干整体编不过(装备生成物未产出),与本项无关。

有可运行的新 battle.exe 时追加(需要先能编出 battle 节点;编不出就在交付说明里写「拒启分支与端到端未验证」):
a) 按 codex_measurement 步骤 3-1 把 GroupID 改成 game-consumer-group 后起 battle:本次启动的日志里应出现 `KafkaProducer: brokers stored for lazy init:`(修复后验收只看这一行;`Init skipped: empty default group_id` 这行已随修复删除)。
b) 把本机配置的 Brokers 清空后起 battle:`run\logs\cpp_nodes\z1_battle.stderr.log` 应有 `FATAL kafka config:`,进程非 0 退出。
c) 保持 a) 的配置跑一次 `.\robot.exe -c etc/battle_smoke.yaml`,应得到 BATTLE_SMOKE_OK。
d) 每一项跑完都用备份恢复配置,`git diff --quiet -- bin/etc/base_deploy_config.yaml` 退出码为 0。
失败时保留:msbuild 完整输出、run_cpp_tests 汇总表、gtest XML 与控制台输出、本次启动的节点日志与 stderr 日志、冒烟输出。

批 2 ────────────────────────(等 G2b 提交或负责人授权)

【10. K8s 模板:GroupID 从 bin/etc 镜像】tools/scripts/k8s_deploy.ps1 的 New-NodeConfigMapYaml
- 在读取 `Kafka.CommandTopicPartitions` 的那两行旁边加:`$kafkaGroupId = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.GroupID'`。
- 模板里把 `GroupID: "game-consumer-group"` 改成 `GroupID: "${kafkaGroupId}"`。只动这一个键。
- 理由:这份 ConfigMap 整目录遮蔽镜像里的 bin/etc,生成器自带常数正是这次漂移的来源(2026-05-27 只改了 bin/etc,没改模板)。空串能被 Get-AuthoritativeScalar 读到而不抛错(release_common.ps1:126-133、:143-158;由 codex_measurement 步骤 1-2 的最后一行实测确认)。
- 副作用一:gate / scene 的旧路径从「全服 1 个组、N 个成员」变成 N 个组 `<类型>-group-<node_id>`(node_kafka_command_handler.h:126-129),与本机行为相同。约 100 个 zone 的规模下组数很多,真正的收口是关掉旧路径(另案)。
- 副作用二:换了组名的新组没有已提交位点,旧路径的消费者用 earliest(默认策略 ownOffsets=false,kafka_consumer.cpp:112-113;node_kafka_command_handler.h:136 不传策略)。旧 topic 里若还有保留期内的消息会被重读;非空且不匹配的 target_instance_id 会被过滤掉(node_kafka_command_filter.h:134-135),空实例号的不会。旧 topic 自 2026-09-08 起没有生产者,【推断】已无残留。对长期运行的环境,上线批 2 前先确认旧 topic 为空或不存在。
- 注意:改 ConfigMap 不会滚 Pod,要手动重启;重启 Deployment 形态的 battle 会作废在打的局。

【11. 契约测试】tools/scripts/tests/k8s_deploy_contract.tests.ps1,紧跟「node ConfigMap 的 Kafka 命令 topic 世代和分区数必须与预建 Job 同源」那条用例
- 对 node-config 与 battle-node-config 分别断言 `data.base_deploy_config.yaml.Kafka.GroupID` 等于 `Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.GroupID'`。Assert-Equal 允许空串(test_harness.ps1:46-55)。
- 再对两段输出各加一条 `Assert-NotMatch … -Pattern 'game-consumer-group'`,防止常数被写回去。
- 修复前红、修复后绿。命令:`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`,通过标准是退出码 0。

【12. 文档(批 2)】
- deploy/k8s/README.md 的 game-events 行(HEAD:1098,磁盘 1189)改成与修复后一致的说法;迁移窗口一节补一句「K8s 模板的 Kafka.GroupID 现从 bin/etc 镜像」。
- bin/etc/base_deploy_config.yaml:60-61 的过时行号引用可在这一批顺手订正(只改注释)。
- deploy/docker-compose.yml:180-183 的注释不碰。
- docs/PROGRESS.md 另起一条;PARITY.md 那一行的备注追加批 2。

另案 ────────────────────────

【13. DisableLegacyPerNodeTopic】
- 与本缺陷无因果,本项两批都不动它。
- 要定两件事:是否翻 true(迁移第 4 步,前提是确认没有任何环境还在跑 2026-09-08 之前的生产者、旧 topic 写入与积压为 0);K8s 上用什么承载(从 bin/etc 镜像会让一次改动同时翻本机和所有 K8s 环境,与 bin/etc/base_deploy_config.yaml:100-101「由运维显式翻」的口径不符;部署参数可以逐环境翻)。
- 开服交接 §4 第 7 项(:129)把「关掉旧的一节点一 topic 路径」划给了全局单点这条线,是同一件事。

### 原报告:Java 版

结论:不适用。Java 版没有「生产端 broker 靠消费订阅顺带登记」这种结构,也没有 C++ 节点的 node ConfigMap。

证据(D:/luyuan/xuanming-server-mmo,HEAD d942f74,工作区干净,只读):
- battle 的 Kafka 生产者直接用配置里的地址构造。game-battle/src/main/java/com/game/battle/BattleConfiguration.java:242 `KafkaBattleResultSink.Clients.kafka(resultProps.bootstrapServers(), clientId)`;game-battle/src/main/java/com/game/battle/port/kafka/KafkaBattleResultSink.java:183 `props.put(ProducerConfig.BOOTSTRAP_SERVERS_CONFIG, bootstrapServers);`
- 地址为空直接拒启。game-battle/src/main/java/com/game/battle/port/kafka/BattleResultProperties.java:28-30 `if (bootstrapServers == null || bootstrapServers.isBlank()) { throw new IllegalArgumentException(…`;缺省值来自 game-battle/src/main/resources/application.yaml:97 `bootstrap-servers: ${GAME_KAFKA_BOOTSTRAP_SERVERS:127.0.0.1:9092}`。
- battle → scene 的确认与结算不经 Kafka。BattleConfiguration.java:200 注释「确认事件 → scene:Dubbo SceneBattleService.confirmBattle」;:207「结算 → scene:先落 Redis、后投递」。
- Java 仓库 deploy/ 下只有 compose 与 docker 两个目录,`git ls-files` 里没有 k8s / helm / configmap,不存在模板与本机配置漂移这一面。

未核实:Java 版在 Kafka 不可达时的降级取舍(不拒启、结果事件写兜底日志)与本缺陷不同类,没有展开核对。scene 审计生产者(AuditPipeline.java)沿用原报告与复核的结论,本轮未重读。

PARITY.md 登记(「功能对齐」表末尾追加一行;列顺序见 PARITY.md:25:功能 | mmorpg 位置 | mmorpg commit | Java 模块 | Java 版本 | 状态 | 备注;状态取值见 :23):
- 登记时机:等批 1 真正提交、有了 commit 号再登记(提交要等用户发话)。若用户同意先登记,commit 列写「未提交」,提交后回填。Java 仓库不在本轮只读范围之外做任何改动。
- 批 1 的行(不要写模板镜像,那是批 2):
| C++ 节点 Kafka 生产端 broker 无条件登记(修只生产不消费的 battle 在 Kafka.GroupID 非空时拿不到 broker)、Kafka.Brokers 为空拒启 | `cpp/libs/engine/infra/messaging/kafka/kafka_manager.cpp`(`KafkaManager::Init`)、`kafka_producer.h`、`cpp/libs/engine/core/node/system/node/node.cpp`(`InitKafka`)、`cpp/tests/kafka_command_test/kafka_manager_init_test.cpp` | `<提交后回填>` | — | — | 不适用(Java battle 的生产者由 `game.battle.result.bootstrap-servers` 显式构造、空值拒启;确认与结算不经 Kafka) | `<登记日期>`;证据 `BattleResultProperties.java:28-30`、`KafkaBattleResultSink.java:183`、`BattleConfiguration.java:200-242` |
- 批 2 落地后在同一行备注追加:「node ConfigMap 的 Kafka.GroupID 改为从 bin/etc 镜像(`tools/scripts/k8s_deploy.ps1` 的 `New-NodeConfigMapYaml`);Java 无 K8s 模板,不适用」。

docs/PROGRESS.md 的新条目里写:对应 Java 版本号:无(不适用)。

### 原报告:风险与未决

【需要负责人拍板】
1. C++ 这一半按什么口径算「实测核实」。现状:本机两个检出都没有 C++ 编译环境,也没有任何 battle 二进制;主干 C++ 自 2026-10-10 起整体编不过(docs/PROGRESS.md:7005、:7051,装备生成物未产出;已跟踪的 cpp/generated 里 grep 不到 has_equip,而 scene / modules 源码在用)。目前真正能跑的只有步骤 1(模板渲染)。三选一:
   (i) 授权补环境(初始化子模块、构建 gRPC、完成 equipment-attributes.md §9 的导表与全量 proto-gen;属于下载与长时间构建),再跑步骤 2 的红绿;
   (ii) 接受「步骤 1 的渲染结果 + 逐行静态证据链」作为本项的核实口径,批 1 按「未编译,待 Codex 验证」交付;
   (iii) 若本机 docker 里已有 mmorpg-node 的 Linux 镜像,用它做真实进程旁证(需 Codex 先确认镜像是否存在;上 kind 要照 deploy/k8s/AGENTS.md:48-50,可能涉及下载)。
2. 批 2 的时机。k8s_deploy.ps1、它的契约测试、deploy/k8s/README.md 此刻被他人在改。等 G2b 提交后再做;或授权在同一文件的不同区域并行改;或请 G2b 的作者顺带把模板 GroupID 这一行改掉(单这一行就能让旧二进制上的 battle 恢复)。
3. Kafka.Brokers 为空改为拒启(行为收紧)是否接受。
4. DisableLegacyPerNodeTopic 是否翻 true、何时翻、K8s 上用镜像还是部署参数承载。与本缺陷无因果,建议另案。
5. 代码交接方式:默认用补丁(不 commit)。若更愿意用一次本地提交(不 push)交接,需要用户明确发话(AGENTS §9)。
6. 红补丁里带一个只读访问器(kafka_producer.h 加一行内联函数)。如果要求红用例阶段严格不碰生产头文件,退路是红用例只断言 Init 的返回值、访问器随修复一起加;代价是红用例看的是代理量(返回值本来就被唯一的调用者丢弃,node.cpp:636),不是缺陷本身。

【需要 Codex 实测确认】
- 步骤 0 的环境清单:可编译检出在哪;lib/ 下 scene.lib、modules.lib、table.lib 等是否有可用的旧产物;rdkafka.dll、zlibd.dll 在哪;有没有 battle.exe。
- 契约测试在干净的 0aa1027bc 上是否本来就全绿(步骤 1-3)。
- 新用例能否链接通过;exe 是否只依赖 rdkafka.dll(dumpbin)。
- infra.vcxproj、core.vcxproj 在当前主干上能否单独编过(未核实;它们是否也依赖未产出的生成物没有查)。
- 缺陷在真实进程上的日志表现;battle_smoke 在缺陷下失败的具体 step 名;start_game.ps1 的起栈参数。
- 本机 docker 里有没有 mmorpg-node 镜像。
- 批 2 上线前:长期运行的环境里旧 topic `gate-<id>` / `scene-<id>` 是否为空。

【风险】
- 没有任何运行证据;修复代码同样无法由本会话编译。
- 有并发会话在写主工作区:本轮几分钟内看到契约测试从 +33 行涨到 +300 行,docs/README.md 与 godb-pipeline-handoff-20261013.md 新变成已改状态。每次落码前重跑 `git status --short`,对每个要改的文件先 Read 再 Edit。docs/PROGRESS.md 也在被别人追加,只在末尾另起一条。
- 批 1 落地后、批 2 落地前:旧二进制 + 旧模板的环境缺陷仍在。K8s 上必须发新镜像才生效。
- Linux 侧没有验证路径:build_linux.sh 不含测试工程,K8s 上实际运行的二进制只靠读代码保证。交付说明要写明。
- 修复后 battle 在 K8s 上第一次真正往 gate-cmd / scene-cmd / match-results 生产。K8s 上的战斗链路此前没有跑通记录(deploy/k8s/AGENTS.md:38 把它列为事后补验项),修好这一处后可能露出下一个问题。
- 批 2 改组名的位点副作用(见 fix_design 第 10 条)。
- 改 ConfigMap 不会自动滚 Pod;重启 Deployment 形态的 battle 会作废在打的局。
- 运行期 broker 不可达时,battle 仍会接新房间,确认和票据发不出去。本项不处理,建议另立项。
- KafkaProducer 是 thread_local,登记只对调用线程生效;将来若有非 loop 线程发送会重现同样的症状。本项不改这个结构。
- 修复后 Kafka.Topics / EnableAutoCommit / AutoOffsetReset 三个键没有读者,本项不清理。

【与另外两份交接的重叠 / 冲突】
- 开服交接(handoff-launch-readiness-and-d1-20261013.md):§4 第 1、2 项的 G2b、G4b 都要改 k8s_deploy.ps1 及其契约测试(:80-88,「必须排在 G2b 之后,两者都改 k8s_deploy.ps1」)。批 2 只动 New-NodeConfigMapYaml 的 Kafka 段,区域不重叠,但要排提交先后。§4 第 7 项(:129)的「关掉旧的一节点一 topic 路径」与上面第 4 个拍板项是同一件事。
- go/db 交接附录(godb-pipeline-handoff-20261013-appendix.md):170 已把 K8s 模板的 GroupID 常数记为「同类问题,建议另开一项」,本项的批 2 就是它,不要重复立项。它还提了一个待核实点:node_id 是否跨 zone 唯一(不唯一时旧 topic 名跨 zone 撞名)。本项未核实。
- 同一附录 :262 计划在 kafka_command_test.vcxproj 的同一个 ItemGroup 再加一行,合并时会撞同一处。
- go/db 交接 §2.3 提醒 k8s_deploy.ps1 里还有同名的 GroupID(db 与 login 的 Go ConfigMap,值 db_rpc_consumer_group)。那是另一个字段,批 2 不碰,改的时候别混。
- go/db 交接 §2.4:G2b 的 mysql.yaml 一旦 apply 会让唯一的 MySQL 停一次。与本项无直接关系,但说明批 2 不要和 G2b 的部署动作绑在一起。

【未核实项】
- librdkafka 的行为(建生产者不需连通、produce 入本地队列、ERR__STATE 数值)与 MSVC 的库搜索顺序只凭常识;子模块未初始化,无法对照源码。步骤 2 的用例不依赖前者。
- Linux 版 muduo 的日志级别枚举顺序。
- scene 四个生产点是否全部在 loop 线程(只看了注释)。
- f452 直接切到 0aa1027bc 会不会被 git 拒绝(复核的推断;本方案不依赖它,改用新 worktree)。
- LoadAllConfigData 是否需要外部服务(影响步骤 3 能否在不起 docker 依赖时看到判据行)。
- kafka_consumer.cpp 139 行之后未读;battle_room_manager.cpp 的结算重投、活动局路径本轮未重读。
- Java 版在 Kafka 不可达时的降级取舍未展开核对。

【顺带发现,不在本项修】
- 日志级别口径三处不一致:K8s 模板 LogLevel: 1、bin/etc/base_deploy_config.yaml:25 的注释、release_preflight.ps1:445-446 的下限。按 Windows 枚举,发布门禁会放行 DEBUG 级。
- .github/copilot-instructions.md 仍按旧的 `{type}-{node_id}` topic 描述。
- 主干 C++ 整体编不过是所有 C++ 修复(本项、Redis AUTH、etcd 流重建)共同的前置,建议负责人单独排期。

### 原报告:预计改动文件

- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/infra/messaging/kafka/kafka_manager.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/infra/messaging/kafka/kafka_manager.h
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/infra/messaging/kafka/kafka_producer.h
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/libs/engine/core/node/system/node/node.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/kafka_command_test/kafka_manager_init_test.cpp
- D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/kafka_command_test/kafka_command_test.vcxproj
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/run_cpp_tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/docs/PROGRESS.md
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/k8s_deploy.ps1 [批2:此刻带他人未提交改动,G2b 提交或负责人授权前不要动]
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/k8s_deploy_contract.tests.ps1 [批2:并发会话正在改写,同上]
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/README.md [批2:此刻带他人未提交改动,同上]
- D:/luyuan/xuanming-server-mmo-cpp/bin/etc/base_deploy_config.yaml [批2 可选:只订正 :60-61 注释里的过时行号]
- D:/luyuan/xuanming-server-mmo/PARITY.md [Java 仓库:批 1 提交、有 commit 号之后登记一行]

## P0-C etcd 没有 MVCC 历史压缩


### 结论

- 第一轮判定:`partially_confirmed`。
- 在 `2f897bd07` 上是否仍成立:是。

**把握**:

仓库侧事实:high。本轮在 HEAD 0aa1027bc 上重新逐行读过的有:etcd.yaml 全文;docker-compose 的 etcd 段;C++ 的 etcd_helper 全文、etcd_manager 的键与注册段、etcd_service 的 Range / watch / 重建段、service_discovery_manager、node.cpp 的摘除与失租段、gate 上报;Go 的 snowflakealloc 水位循环、putWatermarkLocked、rewatch、login 毫秒水位、killswitch / scenenode / load_reporter 的 watch 循环;Java GateWatcher;k8s_deploy.ps1 的 Apply-Infra 与 Show-InfraStatus(HEAD 与工作区两套行号);契约测试夹具、测试骨架、CI workflow;kind-config.yaml 与 start_game.ps1 的 context 约定。「无压缩配置、无 Compact 调用」另有本轮重跑的全仓 grep 佐证。两份复核也各自独立核对过引用,没有发现不符。

整体结论(会 NOSPACE、多久写满):medium。它依赖未核实的外部事实(etcd 与 bitnamilegacy 镜像的默认行为、默认配额、每 revision 字节数),时间是推算。所以判 partially_confirmed,等 Codex 的 M2 / M3 定论。

低把握的点:
- 初稿里 Go 各服务节点注册的行号(一次性写入,不影响结论),本轮没有逐个重读,依赖复核 2 的核对;
- go-zero 内置 discov 与 etcd Go 客户端对压缩的处理(本机没有模块缓存);
- 服务端取前值的时机、NOSPACE 下各类请求的确切表现;
- 修复草图里的 etcdctl 输出格式、报错原文、指标名、bitnami 配置目录(凭记忆,已逐处标出并安排了确认步骤);
- 带摘要的镜像引用在 kind 里的行为。

本轮全程只读:只用了读文件、检索和只读 git 命令,没有运行任何 docker、kubectl、构建或测试命令。

### HEAD 前进后的更正(0aa1027bc → 2f897bd07)

核对方法:git diff 0aa1027bc 2f897bd07 -- <文件>,git log --full-history 定位提交。0aa1027bc 是 2f897bd07 的祖先,区间 13 个提交、943 个文件。

【先纠正主线程环境说明里的一处不准确】cpp/generated/grpc_client/grpc_init_client.cpp 在区间内改过(+58 行,提交 898c803ea,经 db1b8c5bb / ab00ad8b8 合入):新增 GuildInternal / MatchInternal 的分派。`RPC failed` 分支从 :366-367 移到 :408-409,逻辑没变,仍是 `if (!ok) { LOG_ERROR << "RPC failed"; return; }`。其余几项说法核对属实:kafka_manager / redis_manager / node.cpp / etcd 相关 C++、etcd.yaml、redis.yaml、docker-compose.yml 在区间内都没有改动。

【对本项有影响的改动】每条依次回答:影响结论吗 / 影响实测步骤吗 / 影响修复设计吗。
1. tools/scripts/k8s_deploy.ps1(e4ff1d165,G2b 连接总账等)。Apply-Infra 函数体没有任何 hunk,只是整体后移 688 行:
   - 函数 :5744→:6432-6594;
   - `$infraManifests` :5760→:6448,仍是 5 个清单加条件部署的 loki;
   - 占位替换 :5849→:6537;fail-closed :5870→:6558-6561;etcd 分支 :6460-6470;
   - Show-InfraStatus 的 get 行 :5922→:6610,仍不列 cronjob;
   - 「五个 infra manifest」两处注释 :5592/:5879→:6280/:6567;
   - login 模板的 IdSegment 与 `FallbackToSnowflake: false` :3661-3666→:4226-4231;
   - Global 标记 :939/:941/:944→:1351/:1353/:1356;GateReplicas 仍在 :76;
   - DryRun 会连 Secret 明文一起打印,注释在 :6503。
   结论:不影响。实测:只换出处行号,实测本来就不运行它。修复:1b 要按新行号定位;「等 G2b 会话提交」这个前置已经满足。
2. tools/scripts/tests/k8s_deploy_contract.tests.ps1(e4ff1d165,+309 行):
   - infra-up DryRun 夹具 :1096-1098→:1149-1151;
   - dev 档 kafka 断言 :1354-1361→:1406-1413;
   - 「image 不得 latest」用例 :896-907→:949-957,仍只扫 zone-up 的 $devOut(:33-47);
   - 对 etcd 清单仍然零断言;
   - 新增 mysql-connection-alerts.yaml 的形状断言(:1663-1690),可作告警文件测试的先例。
   结论:不影响。实测:不影响。修复:1b 换行号;「契约测试是移动靶」的说法失效,可以在 2f897bd07 上取基线。
3. .github/workflows/deploy-config-tests.yml(e4ff1d165):
   - 触发路径新增 `deploy/k8s/*.yaml`、`deploy/k8s/*.json` 和 java application.yaml;
   - 扫描一级目录的行 :111→:119;
   - 仍不含 deploy/docker-compose.yml。
   结论:不影响。实测:不影响。修复:新建的 deploy/k8s/etcd-alerts.yaml 会自动触发 CI;compose 仍要等 1b 补路径。
4. deploy/k8s/mysql-connection-alerts.yaml(e4ff1d165):由未跟踪变为已提交,已提交的 PrometheusRule 从 3 份变为 4 份。只影响修复:告警模板多了一个已提交的可选项。
5. deploy/k8s/manifests/infra/mysql.yaml(e4ff1d165,只改注释)与 godb 交接(+52 行,§9):
   - §2.4 的 L-04 约束仍在 docs/handoff/godb-pipeline-handoff-20261013.md:91;
   - L-04 仍只是设计评审稿(docs/design/db-task-store-unavailable-deferral.md);
   - infra-up 仍会连带 apply mysql.yaml(k8s_deploy.ps1:6448)。
   结论:不影响。实测:不影响。修复:不变,有存档的环境仍须单独 `kubectl apply` etcd-maintenance.yaml。
6. 日志英文化(946ffd216,同样内容也随 e4ff1d165 进入 main):
   - go/shared/killswitch/watch.go 的重同步日志现为 :60 `[killswitch] Watch interrupted; repeating full synchronization`,旧中文文案已不存在;原报告写的 :59 本来就差一行;
   - scenenode / match / client_rpc_router 的 NodeWatcher 文案也改成英文(scenenode/watcher.go:125、match/internal/discovery/node_watcher.go:90);
   - allocator.go 的日志本来就是英文,没改;node.cpp 没改。
   结论:不影响。实测:M6 的匹配文本要换。修复:不影响。
7. grpc_init_client.cpp,见开头。实测:M6 出处换成 :409。修复:fix_design 方案 (a) 里引用的 :366-367 改为 :408-409。结论:不影响。
8. cpp/nodes/gate/main.cpp(e4ff1d165 / 946ffd216):只改了 :232-240 的日志文案,每 10s 上报仍在 :408-411,UpdateNodeInfo 全仓仍只有这一个调用者。三项都不影响。
9. go/login/login.go、go/guild/guild.go、go/match/internal/svc/servicecontext.go:只有日志文案和一行 PrimeEnterGameMetrics,位置都在 snowflakealloc 持有点之后。以下行号不变:login.go:192/:198/:263-272、guild.go:190/:193、match :117/:120。三项都不影响。
10. go/login/etc/login.yaml(e4ff1d165,只改注释):IdSegment 块 :187-192→:188-196,`FallbackToSnowflake: false` 在 :196。「号段模式下 login 的 PlayerId 不受 NOSPACE 影响」仍成立,只换行号。
11. tools/scripts/start_game.ps1、go_services.ps1:日志英文化,逐行替换。以下行号都不变:start_game.ps1 的 :110-112、:260、:270、:282、:347、:364、:371、:379、:383、:445-446、:450;go_services.ps1 的 :87、:662-663。三项都不影响。
12. docs/README.md 与 gen_docs_index.py:
   - 运维手册段里 cross-zone-failure-test-runbook 与 gate-kernel-tuning-runbook 两行从 :246-247 移到 :250-251;
   - gen_docs_index.py 的 OPS_RULES :96-101→:97-102,first_heading :106-115→:108-117。
   只影响修复第 5 步的插行位置。
13. deploy/k8s/README.md(+103 行):etcd 一节仍在 :444-466,:460-462 的 etcdctl 进入方式不变。只影响「他人未提交改动」这个说法。
14. 环境变化:
   - 主工作区干净(只有 librdkafka、ue5navmesh 两个子模块脏标记);
   - Codex worktree 有两处:f452@73b852f32(8 个未提交文档)、793e@0aa1027bc(303 个未提交改动);
   - third_party/grpc 已检出源码,但 install_vs2026_dbg 与 install_vs2026 都不存在;
   - bin/ 下仍只有 README.md、etc、logs,run/ 不存在;
   - battle 编译报 C1083 找不到 boost/any.hpp(docs/PROGRESS.md:7637)。
   影响实测 M5 / M6 的受阻原因描述,以及修复 1b 的前置。
15. Java 仓 HEAD d942f74→52cccf2(工作区干净),仍不用 etcd(deploy/compose/infra.yaml:11)。PARITY.md 表头 :25→:27,先例 :36→:39。只影响修复登记。
16. 区间内新增的 etcd 访问只有读:login 的 loginqueue 每次算容量都 Range 一次(新注释),不产生 revision。对新增代码行做 diff grep(clientv3. / .Txn( / OpPut / SendKVPut / PutServiceNodeInfo / UpdateNodeInfo / RunEvery),没有新的写入点。

【区间内未改动,原出处全部仍有效】
- deploy/k8s/manifests/infra/etcd.yaml、deploy/docker-compose.yml;
- go/shared/snowflakealloc/、go/scene_manager/internal/svc/servicecontext.go;
- cpp/libs/engine/core/node/system/etcd/*、discovery/*、node.cpp、cpp/generated/grpc_client/etcd/*;
- proto/etcd/etcd.proto、Java GateWatcher.java;
- docs/ops/release-checklist.md、deploy/k8s/AGENTS.md、deploy/k8s/kind-config.yaml、incident 文档;
- stress_round19.ps1、deploy-staging.sh、tools/scripts/tests/lib/*、bin/etc/base_deploy_config.yaml、player_id_minter.go;
- go-svc 清单、owner-epoch-alerts.yaml、mysql-backup-cronjob.yaml;
- handoff-backlog、handoff-global-singletons 两份交接;
- 生成器模板 grpc_async_client.cpp.tmpl、grpc_init_total.cpp.tmpl。

**失效的说法**:

括号里是报告文件的行号。
1. confidence (:9)、claim_evidence (:24):「本轮在 HEAD 0aa1027bc 上逐行重读」以及「HEAD 与工作区两套行号」→ 已在 2f897bd07 上重读关键跳,结论不变。工作区行号这一套作废,只保留 HEAD 行号。
2. claim_evidence A.5 (:37-39):k8s_deploy.ps1 「HEAD :5760 / :5849,工作区 :6178 / :6267」→ HEAD 2f897bd07 上分别是 :6448($infraManifests)和 :6537(占位替换);Apply-Infra 在 :6432-6594。
3. claim_evidence A.3 (:34):「grep 只命中 go/proto/etcd/etcd_grpc.pb.go:259、:266」→ 这条不是 HEAD 前进造成的,是本轮复核发现的。那两行是 `Compact(` 服务端桩,不含该模式里的任何词。在 2f897bd07 上用同一模式、同一组文件类型重跑,代码与配置零命中。「没有任何脚本、清单或手写代码执行压缩」的结论不变,生成层的 SendKVCompact 加空 handler(etcd_service.cpp:85-87)也不变。
4. claim_evidence C (:62):k8s_deploy.ps1 的 Global 标记 「HEAD :939、:941、:944」→ :1351(login)、:1353(scene-manager)、:1356(match);:76 仍对。
5. claim_evidence D (:68):k8s_deploy.ps1 「HEAD :3661-3666」→ :4226-4231;go/login/etc/login.yaml 「:187-192」→ :188-196(`FallbackToSnowflake: false` 在 :196)。
6. handoff_corrections 第 13 条 (:97-102)「工作区不干净:15 个已修改、5 个未跟踪,另一会话在写」→ 失效。现在只剩 third_party/librdkafka、third_party/ue5navmesh 两个子模块脏标记。点名的这些文件都已随 e4ff1d165 等提交进 HEAD:k8s_deploy.ps1、契约测试、deploy/k8s/README.md、docs/README.md、PROGRESS、workflow、mysql.yaml、mysql-connection-alerts.yaml。godb 交接 :300「主工作区从 04:00 起另一会话在写」同样已过时。
7. handoff_corrections 第 15 条 (:104-109):
   - grpc_init_client.cpp 「:366-367」→ :408-409(该文件在区间内改过,主线程说它没改不准确);
   - 「k8s_deploy.ps1 要注明 HEAD 还是工作区,工作区后移约 418 行」→ 只有 HEAD,Apply-Infra 一带相对 0aa1027bc 后移 688 行;
   - 契约夹具「HEAD :1096-1098 / 工作区 :1148-1150」→ :1149-1151;
   - etcd.yaml:75、:114 仍对。
8. handoff_corrections 第 16 条 (:110)、M5 现状 (:475)、changes_from_review 复核 2 的 blocker (:904):「third_party/grpc 是空目录」→ 源码已检出(子模块 c876f4d),但 third_party/grpc/install_vs2026_dbg 和 install_vs2026 都不存在。「bin/ 只有 README.md、etc、logs」「没有 run/ 目录」仍成立,两个 Codex worktree 也一样。编译阻塞的现行原因改为:battle.vcxproj Debug|x64 串行 /m:1 时报 C1083,找不到 boost/any.hpp(third_party/muduo/muduo/net/EventLoop.h:18;Boost 是模块化 include 布局,工程只包含根目录;PROGRESS.md:7637)。本报告里没有「110 个编译错误 / 装备表缺失 / 子模块全未初始化」这三种说法,无需撤回。子模块现状是 19 个里初始化了 14 个,未初始化的是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash;它们对 Windows 节点构建是否必需,未核实。
9. entry_point_inventory【六】(:159-160):
   - Apply-Infra 「HEAD :5744-5906(工作区 :6162-6324)」→ :6432-6594;
   - etcd 分支「工作区 :6190-6200」→ :6460-6470;
   - fail-closed 「HEAD :5870(工作区 :6288)」→ :6558-6561;
   - Show-InfraStatus 「HEAD :5913-5923,get 行 :5922(工作区 :6340)」→ :6601-6611,get 行 :6610,仍不列 cronjob。
10. entry_point_inventory【八】(:168-172):
   - 夹具 → :1149-1151;kafka 断言「:1354-1361」→ :1406-1413;「不得 latest」用例「:896-907」→ :949-957;
   - 「该文件有他人未提交改动」失效;
   - CI 扫描行「HEAD :111」→ :119;
   - 「他人未提交的改动新增了 deploy/k8s/*.yaml」→ 已提交,另外还加了 deploy/k8s/*.json 与 java application.yaml;仍不含 deploy/docker-compose.yml。
11. entry_point_inventory【九】(:177):「已提交的 PrometheusRule 只有 3 份」→ 4 份,多了 mysql-connection-alerts.yaml,它在契约测试 :1663-1690 有形状断言。
12. entry_point_inventory【十】(:181、:186):deploy/k8s/README.md「有他人未提交改动」失效,etcd 一节仍在 :444-466;docs/README.md「有他人未提交的索引行」失效。
13. codex_measurement【0. 规则】(:216):「主工作区有别的会话在写」→ 现在是干净的,但仍不往主工作区和两个 Codex worktree 写任何文件。
14. codex_measurement【M2 出处】(:317):键形「allocator.go:186-188、:207-215、:226、:240-242」→ 精确位置是 rootKey :186、watermarkPrefix :206、watermarkKey :213、watermarkMsKey :219、legacyGuardPrefix :224、legacyGuardMsPrefix :225、legacyGuardKey :240、legacyGuardMsKey :244。allocator.go 在区间内没改,这是原稿行号不精确,顺带订正。
15. codex_measurement【M6】(:500-501):
   - killswitch 文案「`watch 中断,重新全量同步`(watch.go:59)」→ `[killswitch] Watch interrupted; repeating full synchronization`(watch.go:60);
   - grpc_init_client.cpp 「:367」→ :409;
   - 其余文本与行号仍对:allocator.go:1016/:1253,node.cpp:1320/:1342/:1356/:1497/:1541。
16. codex_measurement【K8s 清单核对】(:454):「它正被别的会话修改」失效;「DryRun 打印 Secret 明文,工作区 :6233」→ HEAD :6503(注释)。仍然不运行 DryRun,改为 `git show 2f897bd07:` 读静态文件。
17. codex_measurement【M5】(:475):阻塞原因按第 8 条更新。
18. fix_design 1b (:690-698):
   - 「等 G2b 会话提交」→ 已提交(e4ff1d165),只剩负责人同意;
   - 行号「HEAD :5760 / 工作区 :6178」→ :6448;「HEAD :5922 / 工作区 :6340」→ :6610;「HEAD :5592、:5879」→ :6280、:6567。
19. fix_design 第 3 步 (:632)、第 4 步 (:659)、第 5 步 (:683):
   - CI 「HEAD :111」→ :119;
   - 「不用未跟踪的 mysql-connection-alerts.yaml」→ 它已提交,可以作模板,owner-epoch 也仍可用;
   - docs/README.md「格式照 :247-249」→ 插在 :250 与 :251 之间;
   - gen_docs_index.py「:96-101、:106-115」→ :97-102、:108-117。
20. fix_design V0 / V1 / V2 (:718、:725、:730):
   - 「Codex 自己的 worktree(停在 73b852f32)」→ 现在有两处:f452@73b852f32、793e@0aa1027bc;
   - 「k8s_deploy_contract.tests.ps1 被别的会话改,是移动靶」→ 失效;
   - V2 的 `worktree add --detach $wt 0aa1027bc` → 改成 2f897bd07,并用 `git -C $wt rev-parse HEAD` 核对,期望 2f897bd0780c1cc78284548284f20d01e61d7ea8;
   - grpc_init_client.cpp:366-367(方案 (a),:519)→ :408-409。
21. java_parity (:798、:806-812):
   - Java HEAD 「d942f74」→ 52cccf2,仍干净,仍不用 etcd;
   - PARITY 表头「:25」→ :27,先例「:36」→ :39;
   - commit 栏「未提交(工作区,基于 0aa1027bc)」→ 「基于 2f897bd07」;
   - Java AGENTS.md:86 仍对。
22. risks 第 12 条 (:859)、第 28 条 (:883)、changes_from_review (:889、:905、:911、:918):「文件被另一会话修改、+431 行未提交、无法只提交本项」「修复前红在 0aa1027bc 上取」「主工作区有人在写」→ 按第 6、18、20 条更正。
23. 仍然有效、不需要改的:
   - infra-up 连带 apply mysql.yaml 的 L-04 约束(L-04 仍是设计稿);
   - compose 已随 d5cbb8a8e 提交;
   - capacity-model-hok-scale.md:212 仍引 allocator.go:1427,应为 :1383,文档未改;
   - etcd.yaml 与 compose 的全部行号。

### 受阻步骤与先写的测试

1. M5a(Go 开发栈采样):不需要 C++,卡在授权与前置确认。
   - 阻塞原因:要在新 worktree 里 go build login / scene_manager,可能下载 Go 模块;compose 会复用或新建开发依赖容器;开发 etcd 会被写入。
   - 解除条件:负责人给「同意 2a」;Codex 先确认这两个服务在没有 C++、Java、player_locator、data_service 时能起来,并完成槽位申领。
   - 由谁解除:负责人拍板,Codex 确认。
2. M5(整栈采样):受阻。
   - 阻塞原因:bin/ 下没有任何 exe,start_game.ps1 会在 :371、:379、:383 抛错;battle.vcxproj 编译报 C1083 找不到 boost/any.hpp(Boost 是模块化 include 布局,工程只包含根目录,PROGRESS.md:7637);third_party/grpc/install_vs2026_dbg 不存在(依赖库、DLL、protoc 35.1 都在这里);5 个子模块未初始化(是否必需未核实);缺 Java 网关 jar。
   - 解除条件:修好构建环境,并另开构建任务单,串行 /m:1 编 C++,再构建 Go 与 Java。
   - 由谁解除:负责人给「同意 2b」(修 Boost include 路径、重建 install_vs2026_dbg、初始化子模块,都涉及下载或改环境,AGENTS §10.2),由 Codex 修环境。本项结论不依赖 M5。
3. M6(开发 etcd 手工压缩并重启):C++ 部分受阻,原因同 M5;并且它不可逆。
   - 解除条件:负责人给「同意 3」,并与 P0-D 的实测合并成一次。Go 部分可以在 M5a 的栈上做,但同样需要同意 3。
   - 由谁解除:负责人拍板,Codex 执行。

**实测前要先写的代码**:

无。实测阶段不需要 Claude 先写任何代码,理由如下。

1. 实测本身不需要仓库代码:
   - A 组(M0–M4、S1、S2)用到的容器内脚本都内联在任务单里,由 Codex 写进仓库外的 $out,再 docker cp 进临时容器,不进仓库;
   - M5a / M5 / M6 只用现成脚本:start_game.ps1、go_services.ps1、compose;
   - 本项的结论不需要独立 GTest,也没有要先建的测试夹具。
2. 回归测试属于修复,不属于实测:
   - 测试文件是 tools/scripts/tests/etcd_maintenance_contract.tests.ps1,新建;不涉及 CMakeLists / vcxproj / sln;CI 自动扫描一级目录,所以也不用改 workflow 的扫描规则(.github/workflows/deploy-config-tests.yml:119)。
   - 它断言的是修复的形态:CronJob 名、KEEP_REVISIONS 取值、镜像 tag@摘要、compose 的三项 env。
   - 这些取值依赖 M0(摘要)、M2(b)、M3(env 是否生效)的结果,以及负责人对保留量、配额、钉版本的拍板。实测之前就写,会变成移动靶。
3. 这样仍满足 AGENTS §11.4(先有稳定复现问题的回归测试):测试在 1a 落码时随修复一起写;「修复前必红」由 V2 在新建的 2f897bd07 worktree 上取(只拷入测试文件、不拷修复,期望退出码非 0),「修复后绿」由 V1 取。红绿都由 Codex 跑。
4. 不归本项:C++ 侧「DELETE 处理依赖 prev_kv」(etcd_service.cpp:388 → node.cpp:1339-1345)的独立 GTest 归 P0-D(C++ etcd 流重建)。

### 待拍板

1. 同意 1:M0 发现本机没有 bitnamilegacy/etcd:latest 时,是否从 Docker Hub 拉取(大小未核实)。
   - 建议:同意,但只在 M0 确认缺镜像后才拉。
   - 拉下来后要记录摘要,并注明它不一定等于各环境正在跑的版本。
2. 同意 2a(M5a Go 开发栈采样:新 worktree 里 go build,可能下载模块;compose 复用或新建开发依赖容器;开发 etcd 会被写入)。
   - 建议:延后。M2 的合成写入已能给出 b,速率用代码事实代入外推公式即可。
   - 只有需要真实 revision/s、或想排查盘点之外的写入方时再做。
3. 同意 2b(修 C++ 构建环境,是 M5 和 M6 C++ 部分的前提):Boost 模块化 include 布局导致 C1083;重建 third_party/grpc/install_vs2026_dbg;初始化 5 个子模块;构建 Java jar。都涉及下载或改构建环境(AGENTS §10.2)。
   - 建议:本项不为此授权。本项结论不依赖整栈。
   - 把它并入 P0-D 或整栈构建的单独任务单统一授权。
4. 同意 3(M6:在开发 etcd 上做不可逆压缩并重启)。
   - 建议:延后,与 P0-D 的实测合并成一次做。
   - 只做 Go 部分没有单独价值。
5. pwsh 7.3+ 若不在 Codex 的 PATH 上,是否授权安装(安装工具需授权)。
   - 建议:先让 Codex 跑 `pwsh --version` 确认。
   - 如果没有:授权安装,或改由有 pwsh 7 的执行者跑。不要退回 Windows PowerShell 5.1:它会丢掉空字符串参数,M1 的 `get "" --prefix` 会变形。
6. 修复 1a 的落码时机,以及其中三件事:改 deploy/docker-compose.yml;在 docs/README.md、docs/PROGRESS.md 各追加一行;新建 docs/ops/etcd-maintenance-runbook.md(属 AGENTS §5 的 docs/ops 类)。
   - 建议:等 M0 与 M3 的结果回来后再一次性落。镜像摘要、env 是否生效,决定 compose 与 CronJob 的写法。
7. 修复 1b(把 etcd-maintenance.yaml 接入 infra-up,即 k8s_deploy.ps1:6448 与 :6610;补契约测试、deploy/k8s/README.md、deploy/k8s/AGENTS.md;CI 加 deploy/docker-compose.yml 路径)。原前置「等 G2b 会话提交」已满足(e4ff1d165),主工作区干净。
   - 选项:(a) 与 1a 同批做;(b) 仍然分开做。
   - 建议 (a):可以一次堵住「新环境漏 apply」的风险(risks 第 25 条)。
   - 动手前重新 git status,分清哪些改动是自己的。
   - 1b 之前,先在 2f897bd07 上取 k8s_deploy_contract.tests.ps1 的基线。
   - 有存档的环境仍须单独 `kubectl apply` etcd-maintenance.yaml,因为 infra-up 会连带 apply mysql.yaml,而 L-04 仍未落地(godb 交接 :91)。
8. 保留量 KEEP_REVISIONS。
   - 选项:1000000(按 6.2Z+2 推算,100 个 zone 约 27 分钟,常态约 200~400 MB),或 200000(约 5 分钟)。
   - 建议:1000000,等 M2 给出 b 后最终确认;扩 zone 时同步调大。
9. 配额。
   - 选项:3GiB;显式写出默认的 2GiB;4GiB。
   - 建议:3GiB(3221225472)。理由:要满足「配额 × 2 + 1Gi ≤ PVC 8Gi」(etcd.yaml:200),4GiB 不满足。
10. defrag。
   - 建议:只在 runbook 里写手工、逐成员的流程,不做 CronJob(YAGNI)。
11. 钉哪个版本。
   - 建议:按 M0 的「tag@摘要」钉,必须等于各环境正在跑的版本(用 S2 逐成员核对 imageID),禁止降级。
   - 是否镜像到自有仓库另议;AI 不能 docker push(AGENTS §9)。
12. 阶段 2(etcd.yaml 加 env、钉镜像、加抓取注解,会滚动 etcd)。
   - 建议:等 P0-D 修复并经 Codex 验证之后再做。
   - 稳定后,etcd-compact 保留一个版本兜底,再删除。

### 原报告:证据链

结论:partially_confirmed,与初稿一致,两份复核都没能推翻。仓库侧三条(无压缩配置、无 Compact 调用、镜像 latest)和「有持续写入点」是代码事实,本轮在 HEAD 0aa1027bc 上逐条重读过。「会写满 2GB 并 NOSPACE」依赖三个外部事实(etcd 默认不自动压缩、默认配额 2GiB、bitnamilegacy 镜像不预置默认值),仓库内无法核实,所以不是 confirmed_static。

【A. 仓库内没有任何压缩 / 配额 / defrag 配置或调用(代码事实)】
1. deploy/k8s/manifests/infra/etcd.yaml
   - :114 `image: bitnamilegacy/etcd:latest`;:115 `imagePullPolicy: IfNotPresent`
   - env 只有 :117-152 的 POD_NAME / ETCD_NAME / ALLOW_NONE_AUTHENTICATION / ETCD_DATA_DIR / ETCD_LISTEN_* / ETCD_ADVERTISE_CLIENT_URLS / ETCD_INITIAL_*;容器没有 command / args
   - :189-190 注释:「真正要盯的是 etcd 自身 2GB 默认 quota 与 compaction,不是 PVC 大小。」(知道但没做)
2. deploy/docker-compose.yml:447-476
   - :450 `image: bitnamilegacy/etcd:latest`;:451 `container_name: etcd`;:453 `"2379:2379"`
   - :456-473 共 7 项 env,无压缩 / 配额项;:475 `- etcd-data:/bitnami/etcd/data`
3. 全仓 grep(本轮重跑,排除 vendor / generated / third_party / .git)。模式 `auto[-_]?compaction|quota[-_]backend|defrag|NOSPACE|ETCD_QUOTA|AlarmDisarm|etcdctl compact`,范围 go / cpp / h / java / yaml / yml / ps1 / sh / bat / cmd / json / toml。只命中生成的服务端桩 go/proto/etcd/etcd_grpc.pb.go:259、:266,没有任何脚本、清单或手写代码执行压缩。
4. C++ 生成层有 `SendKVCompact`(cpp/generated/grpc_client/etcd/etcd_grpc_client.cpp,5 处),手写代码无调用者,只装了空 handler:etcd_service.cpp:85-87 `if (!etcdserverpb::AsyncKVCompactHandler) { etcdserverpb::AsyncKVCompactHandler = emptyHandler; }`
5. tools/scripts/k8s_deploy.ps1 的 Apply-Infra 对 etcd.yaml 只做一次占位替换,不注入 env:
   - HEAD :5760 `$infraManifests = @("etcd.yaml", "redis.yaml", "redis-match-cluster.yaml", "kafka.yaml", "mysql.yaml")`
   - HEAD :5849 `.Replace("__INFRA_NAMESPACE__", $InfraNamespace)`
   - 工作区因他人未提交改动,这两行现在在 :6178、:6267
6. etcd 服务端镜像引用全仓只有两处:etcd.yaml:114、docker-compose.yml:450。另三份 compose 文件没有 etcd 服务,只有注释和客户端地址(docker-compose.login-stack.yml:126)。
7. 本仓 Java 网关只读不写:java/gateway_node/src/main/java/com/game/gateway/etcd/GateWatcher.java:85-87 `resp = etcdClient.getKVClient().get(prefixKey, option)`
8. 契约层旁证(不等于核实了服务端默认值):仓库自带的 etcd 官方 proto,proto/etcd/etcd.proto:30-32 的注释说明 KV 存储需要定期压缩,否则事件历史会无限增长。

【B. 持续产生新 revision 的写入点(代码事实)】
1. Go 秒级水位,每个 snowflakealloc Handle 每秒 1 笔 Txn:
   - go/shared/snowflakealloc/allocator.go:926 `guardWriteInterval = time.Second`
   - :972 `ticker := time.NewTicker(guardWriteInterval)`;:1005 `h.advanceGuard(kaCtx)`
   - :1383-1390 `h.cli.Txn(putCtx).If(clientv3.Compare(clientv3.Value(h.sKey()), "=", h.UUID)).Then(clientv3.OpTxn(...` 内含 OpPut
   - 每拍 target = 当前秒 + 2(:1308-1314),值每秒都变,所以每拍必写;值是十进制字符串(:1375)
   - 持有者只有四处:go/login/login.go:192、go/scene_manager/internal/svc/servicecontext.go:90、go/match/internal/svc/servicecontext.go:117、go/guild/guild.go:190
2. login 另有毫秒水位,每进程每秒再 1 笔:login.go:263-272 `ticker := time.NewTicker(time.Second)` … `sfHandle.PutMsWatermark(context.Background(), target)`,落到 allocator.go:1549-1563 同一个 putWatermarkLocked。
3. 过渡期镜像双写,每笔 Txn 写 2 个 key:
   - allocator.go:182 `func (r resolvedOptions) legacyMirror() bool { return r.cluster == 0 && r.legacy != "" }`
   - :1379-1382 `ops = append(ops, clientv3.OpPut(legacyKey, value))`
   - 四个调用方都传了 LegacyPrefix(login.go:198、scene_manager servicecontext.go:93、match servicecontext.go:120、guild.go:193)
4. gate 每 10s 一次 Put:cpp/nodes/gate/main.cpp:408-411 `context.playerCountReportTimer.RunEvery(10.0, [&n] {` … `n.GetEtcdManager().UpdateNodeInfo();` → etcd_manager.cpp:146-150 → etcd_helper.cpp:8-31 `SendKVPut(...)`。全仓 UpdateNodeInfo 只有这一个调用者(grep 排除 generated)。
5. C++ 已不写水位:etcd_service.cpp:642-645 注释「2026-09-08 起这里不再激活任何发号器」。
6. 续租不是 KV 写:C++ etcd_manager.cpp:193-197 每 KeepaliveInterval(bin/etc/base_deploy_config.yaml:5 为 1)发 LeaseKeepAlive;Go 走 clientv3.KeepAlive。「续租不产生 revision」是外部常识,未核实。

【C. 推算(不是事实)】
- K8s 默认档每 zone:login 2 副本 × 2 笔/s + scene_manager 2 副本 × 1 笔/s + gate 2 副本 × 0.1 笔/s ≈ 6.2 revision/s;key 版本约 12.2/s(镜像双写)。全局池 match 2 副本另加 2 revision/s。guild 没有 K8s 清单(deploy/k8s/manifests/go-svc 下无 guild.yaml),只在本机栈里出现。
- 出处:login.yaml:24、scene-manager.yaml:24、match.yaml:36 的 `replicas: 2`;k8s_deploy.ps1 HEAD :76 `[int]$GateReplicas = 2`;login 与 scene-manager 不带 `Global = $true`(HEAD :939、:941),match 带(:944)。
- 按每个 key 版本 100~200 字节估:单 zone 约 100~210 MB/天,10 zone 约 1~2 天、100 zone 约 2.5~5 小时到 2GiB。与交接量级一致;每版本字节数和 2GiB 配额都未实测。

【D. 后果要分项(K8s 默认档;「NOSPACE 拒什么」是外部常识,待探针 A 实测)】
- 立即失败:新节点注册、失租重注册(C++ 走 Txn,etcd_helper.cpp:94、:108、:170)、gate 人数上报、全部水位 Txn。扩容、滚动发布、崩溃恢复都受阻。
- 不立即受影响:已注册且租约仍在续的节点;读与 watch。
- login 的 PlayerId 不受影响:K8s 模板与默认配置都走号段,不回退 snowflake(k8s_deploy.ps1 HEAD :3661-3666 `FallbackToSnowflake: false`;go/login/etc/login.yaml:187-192;go/login/internal/svc/player_id_minter.go:27-31)。
- 约 2 小时后:仍在用 snowflakealloc 发号的 scene_manager、match、guild 因水位年龄超过 F 而拒发(allocator.go:120 `defaultFenceAfter = 2 * time.Hour`)。
- 新起或重启的 login / scene_manager 申领不到槽;只有本地缓存仍在 F 内才能降级启动(allocator.go:753-756 注释)。这一条是推断。

【E. 外部环节(未核实,全部列入 Codex 实测)】
- etcd 默认不自动压缩、默认配额 2GiB;
- bitnamilegacy/etcd 是否预置压缩默认值,是否把 ETCD_* 环境变量转写进配置文件,已有数据目录重启时是否仍生效;
- NOSPACE 下各类请求的确切表现与报错原文。

### 原报告:对交接的更正

1. 「发号水位每秒写」是 Go 侧,不是 C++。代码在 go/shared/snowflakealloc/allocator.go:926 / 972 / 1005 / 1383 和 go/login/login.go:263-272;C++ 自 2026-09-08 起不写水位(etcd_service.cpp:642-645)。
2. 漏了 login 每进程 2 笔/s(秒级 + 毫秒级)。capacity-model-hok-scale.md:212 的「N_g/10 + 4Z」少算,应为约 6Z + N_g/10 + 全局池;交接 §5 的「写约 670/s」反而与 6Z 吻合。
3. 漏了过渡期镜像双写(allocator.go:182、:1379-1382):ClusterId=0 时每笔水位 Txn 写新旧两个 key,key 版本数是 revision 数的 2 倍。注释写「发布一版之后删」(:1369),删掉可让字节增速约减半,属另一项改动。
4. 「仓库内无 Compact 调用」要加限定:生成层有 SendKVCompact 和空 handler(etcd_service.cpp:85-87),只是没人调。
5. tools/scripts/deploy-staging.sh:23 注释写 etcdctl 用于「snowflake compaction」,脚本体内没有任何 etcdctl 调用,是误导性残留。
6. docs/ops/release-checklist.md:329 已有「etcd `Compact + Defrag`」检查项,但没有命令,也没有 runbook。
7. 「全服注册和发号失败」口径偏重,应分项(见 claim_evidence 【D】):
   - 新注册、重注册、gate 上报、水位 Txn 立即失败;
   - 已注册节点靠续租仍在;
   - login 的 PlayerId 在默认的号段模式下不受影响;
   - scene_manager / match / guild 的 snowflake 约 2 小时后才拒发。
8. 交接没写「钉镜像版本」本身会滚动 etcd:改 etcd.yaml:114 的 image 字符串就是改 StatefulSet 的 Pod 模板。真正零滚动的只有新增独立 CronJob。钉版本、加 env、加抓取注解都必须并入 etcd 滚动窗口,前置是 P0-D。
9. 任务单说镜像有「四处口径」,HEAD 上镜像引用只有两处(etcd.yaml:114、docker-compose.yml:450)。另两个相关口径:
   - etcd.yaml:115 的 `IfNotPresent`:各节点缓存的 latest 可能不是同一版;
   - tools/scripts/start_game.ps1:446 的 `'up','-d','--no-recreate','--pull','never'`:改 compose 后本机既有容器不会自动重建,新引用须已在本地。
10. 交接没提同一份 compose 的 etcd 也是 Linux 预发压测机的 etcd(deploy-staging.sh:94-96 检查 `redis etcd mysql kafka` 容器)。那里没有 CronJob,只能靠 env 自动压缩或手工 `docker exec etcd etcdctl compact`。
11. 交接没提与既有未结清单重叠:docs/handoff/handoff-backlog-2026-09-05.md:162 的 P1-02(「kafka/etcd latest」)和 :877(「infra 产物 image: 无 latest」用例,注明 kafka / etcd 钉版本前会红)。本项只承接 etcd 部分,kafka 仍是 latest(kafka.yaml:156、kafka-topic-init.yaml:61、docker-compose.yml:6、:83)。
12. 交接没提下发路径冲突:`infra-up` 会把 mysql.yaml 一起 apply(k8s_deploy.ps1 HEAD :5760),而 go/db 交接 §2.4(godb-pipeline-handoff-20261013.md:91)规定 L-04 部署前不得对有真实存档的环境 apply 那份 mysql.yaml。所以 etcd 修复不能靠 infra-up 下发到有存档的环境。
13. 任务单说「工作区干净」,实际不是。本轮 `git status`:15 个已修改、5 个未跟踪,且 04:00 之后仍在被另一个会话改写(godb 交接 :300 有记录)。与本项直接相关的:
    - tools/scripts/k8s_deploy.ps1:+431 行,04:11 改;
    - tools/scripts/tests/k8s_deploy_contract.tests.ps1:+300 行,04:34 改。复核 1 写它「目前干净」,已过时;
    - deploy/k8s/README.md(+97)、docs/README.md、docs/PROGRESS.md、.github/workflows/deploy-config-tests.yml、deploy/k8s/manifests/infra/mysql.yaml;
    - 未跟踪的 deploy/k8s/mysql-connection-alerts.yaml。
    本项要动的文件里目前干净的:etcd.yaml、deploy/docker-compose.yml、docs/ops/release-checklist.md、deploy/k8s/AGENTS.md、tools/scripts/tests/lib/*。
14. 交接 §9(:249)把 `deploy/docker-compose.yml`(noeviction)列为他人在途改动。它已随 d5cbb8a8e 提交,工作区干净;按同一节「完整清单以 git status 为准」现在可以改,但请负责人知情。
15. 行号漂移:
    - capacity-model 引的 allocator.go:1427 现在是注释行,真正的 Txn 在 :1383(:926 仍对);
    - 交接 §4.4 的 service_discovery_manager.cpp:16-19,28-34 现为 :16-18、:27-33(文件在 cpp/libs/engine/core/node/system/discovery/ 下);
    - gate 上报是 main.cpp:408-411;grpc_init_client.cpp 的失败分支是 :366-367;
    - k8s_deploy.ps1 的行号一律要注明 HEAD 还是工作区(工作区整体后移约 418 行);契约测试的 infra-up 夹具 HEAD :1096-1098,工作区 :1148-1150;
    - etcd.yaml:75、:114 仍对。
16. 交接说「没有可运行的开发栈」这件事没提:主仓库与 Codex 的 worktree 的 bin/ 下都只有 README.md、etc、logs,没有任何 exe、bin/go_services、gateway jar,third_party/grpc 是空目录。「先测 revision/s」不能假设栈已经在跑。

### 原报告:同类入口盘点

【一、代码:会产生新 revision 的写入点】

| 写入方 | 位置 | 时机 | revision |
|---|---|---|---|
| Go snowflakealloc 秒级水位 | allocator.go:972、1005、1383 | 每 Handle 每 1s | 1 笔/s,过渡期每笔写 2 个 key |
| login 毫秒水位 | login.go:263-272 → allocator.go:1549-1563 | 每 login 进程每 1s | 1 笔/s,过渡期 2 个 key |
| gate 人数上报 | cpp/nodes/gate/main.cpp:408-411 → etcd_manager.cpp:146-150 → etcd_helper.cpp:8-31 | 每 gate 每 10s | 0.1 笔/s |
| C++ 节点首次注册 | etcd_manager.cpp:152-179(端口键)、:102-118(分配键)、:127-142(服务键) | 启动一次 | 3 笔 |
| C++ 失租重注册 | node.cpp:1533-1543 → etcd_service.cpp:467-479 | 仅失租时(P0-D 成立时约每 180s) | 3 笔/次 |
| C++ 关服 | node.cpp:1281-1284 `RevokeLeaseAndCleanup` | 关服一次 | 1 笔(推断) |
| Go 各服务节点注册 | shared/noderegistry、各服务 internal/noderegistry 或 internal/node、login 的 pkg/node 与 pkg/etcd(初稿所列行号本轮未逐个重读,复核 2 已核对) | 启动、失租、关服 | 一次性 |
| snowflakealloc 申领 / reclaim / released | allocator.go:341-352、407-418、1105-1113、1606-1608 | 启动、失租、Close | 一次性 |
| killswitch 规则 | 运维手工 `etcdctl put /mmorpg/killswitch/...`(go/chat/etc/chat.yaml:80-82) | 人工 | 极低频 |
| go-zero 自带 zrpc 注册 | 框架内部 | 启动 | 一次性(外部常识,未核实) |
| Java gateway | GateWatcher.java:85-87 | — | 只读,不写 |

不产生 KV revision 的(外部常识,未核实):LeaseGrant、LeaseKeepAlive、失败的 CAS Txn、Range。

【二、代码:读取与 watch】
- C++:
  - 每 2s 全前缀 Range:service_discovery_manager.cpp:16-18、27-33;
  - Range 应答只按 PUT 处理并刷新续 watch 的 revision:etcd_service.cpp:51、58-59、67-69;
  - 建 watch 带 prev_kv 与 start_revision:etcd_helper.cpp:50、57-60;
  - watch 应答处理:etcd_service.cpp:349-391;取消后 2s 重建全部前缀:393-400、278-285;
  - DELETE 完全靠 prev_kv 的值:etcd_service.cpp:388 → node.cpp:1317-1358。
- Go(clientv3.Watch 全仓只有这些):
  - snowflakealloc allocator.go:876、1089、1169、1244;
  - shared/killswitch/watch.go:87;
  - shared/scenenode/watcher.go:175;
  - match/internal/discovery/node_watcher.go:133;
  - client_rpc_router/internal/discovery/node_watcher.go:135;
  - scene_manager/internal/logic/load_reporter.go:806(唯一带 WithPrevKV 的,:809;全仓没有读 ev.PrevKv 的手写代码)。
- go-zero 内置的 discov 订阅属框架内部,未核实。

【三、代码:销毁 / 回收历史】无。Compact、Defragment、Alarm 全仓零调用。

【四、代码:恢复】没有 NOSPACE 处理,没有针对「已压缩」的专门处理。Go 的 watch 循环是「通道关闭或 Err 非空就全量重同步」;C++ 只认 canceled,不看 compact_revision。见 risks 第 1、2 条。

【五、配置模板】
- deploy/k8s/manifests/infra/etcd.yaml(唯一的 K8s etcd 定义);
- deploy/docker-compose.yml:447-476(本机开发与 Linux 预发共用);
- bin/etc/base_deploy_config.yaml:2-6(C++ 客户端参数:KeepaliveInterval 1、NodeTTLSeconds 180);
- k8s_deploy.ps1 生成的 node ConfigMap 只含客户端地址、TTL、前缀,没有服务端参数。

【六、部署脚本】
- k8s_deploy.ps1 的 Apply-Infra:HEAD :5744-5906(工作区 :6162-6324);etcd 分支只删旧 Deployment(工作区 :6190-6200);未替换占位 fail-closed 检查 HEAD :5870(工作区 :6288);
- Show-InfraStatus:HEAD :5913-5923,`get deploy,sts,po,pvc,pdb,job,svc,cm` 在 HEAD :5922(工作区 :6340),不列 cronjob;
- start_game.ps1:445-446 起 compose,:450 探活 `etcdctl endpoint health`,:110-112 固定 `--context desktop-linux`;
- stress_round19.ps1:218 `docker exec etcd etcdctl del "" --prefix`(清键,不是压缩);
- deploy-staging.sh:23 只有注释,:94-96 要求 etcd 容器在跑。

【七、docker-compose】只有 deploy/docker-compose.yml 定义 etcd 服务,卷 etcd-data(:475、:491)。docker-compose.login-stack.yml / tidb.yml / observability.yml 只在注释或客户端地址里提到 etcd。

【八、测试】
- tools/scripts/tests/k8s_deploy_contract.tests.ps1 有 infra-up 的 DryRun 夹具(HEAD :1096-1098),对 kafka 清单有断言(HEAD :1354-1361),对 etcd 清单零断言。该文件有他人未提交改动。
- 既有「不得 latest」用例只扫 zone-up 产物(HEAD :896-907)。
- 可照抄的独立测试文件先例:tools/scripts/tests/k8s_migrate_gate.tests.ps1(:13 引 test_harness,:15 取仓库根,:84 直接读清单目录)。
- 测试骨架:tools/scripts/tests/lib/test_harness.ps1(Test-Case :20、Assert-Match :57、Assert-NotMatch :69、Complete-TestRun :84)。
- CI:.github/workflows/deploy-config-tests.yml 扫 tools/scripts/tests 一级目录全部 .ps1(HEAD :111)。HEAD 的触发路径含 `tools/scripts/**`、`deploy/k8s/manifests/**`,不含 deploy/docker-compose.yml;他人未提交的改动新增了 `deploy/k8s/*.yaml`。

【九、监控 / 告警】
- etcd Pod 没有 prometheus.io 注解(go-svc 清单的写法见 chat.yaml:52-54);
- 仓库里没有任何 ServiceMonitor / PodMonitor 资源,只在告警文件头注释里作为前提出现(scene-manager-alerts.yaml:11);
- 已提交的 PrometheusRule 只有 3 份(data-service-audit / owner-epoch / scene-manager),都是手工 `kubectl apply -n observability -f`(owner-epoch-alerts.yaml:6-7),没有 etcd 的;
- 可参考的 CronJob 先例只有 deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml,它不在 $infraManifests 里,手工 apply。

【十、文档】
- deploy/k8s/README.md:444-466(etcd 一节,无维护内容;文件有他人未提交改动,但这一节未被改);
- deploy/k8s/AGENTS.md:24、:82;
- docs/ops/release-checklist.md:329;
- docs/design/capacity-model-hok-scale.md:212;
- docs/handoff/handoff-backlog-2026-09-05.md:162、:877(P1-02、P2-02);
- docs/README.md 的索引是生成段(:26-34,生成器 tools/scripts/gen_docs_index.py;本机没有可用的 Python,PROGRESS 末条有记录);
- docs/ops 下没有 etcd runbook。

【十一、迁移】没有版本迁移路径。latest + IfNotPresent 之下,各环境实际跑的 etcd 版本未知。

### 原报告:修复设计

【总述】
推荐方案 (c),拆成三步:
- 1a:现在就能做,不滚动 etcd,不碰任何他人在改的文件;
- 1b:接入 infra-up,等 G2b 会话提交 k8s_deploy.ps1 或负责人明确同意后再做;
- 2:并入 etcd 滚动窗口,前置是 P0-D(C++ etcd 流重建)修复并经 Codex 验证。

本项只改部署清单、compose、测试脚本和文档。不改 C++ / Go / Java 代码,不碰 generated/,不需要 MSBuild 或 go build;AGENTS §11.7、§11.2 的回调与宏规则不涉及;etcd 无鉴权,不存在密码进日志的问题。

【三种方案对比】
- (a) 只加 env 开自动压缩:不依赖 CronJob 调度,本机 compose 与预发单机也生效。但改的是 StatefulSet 的 Pod 模板,必然逐个重启 3 个成员;C++ 的流断开后不重建(grpc_init_client.cpp:366-367 `if (!ok) { LOG_ERROR << "RPC failed";`),所以不能先做。
- (b) 只上 CronJob 跑 etcdctl compact:零滚动,立即止血。依赖 K8s 调度,对 compose 无效,任务挂了要靠告警才知道。
- (c) 两者结合:先 (b) 止血,滚动窗口再上 (a),之后 CronJob 留一个版本做兜底再删。

【落码前置(来自 Codex 实测)】
- M0:镜像的 tag 与摘要、镜像内 PATH 与工具。没有它们,镜像引用写不出来。
- M3 探针 B 与 B2 通过:compose 的三项 env 才落;不通过则 compose 与阶段 2 的注入方式另议,1a 只落 CronJob。
- M3 探针 A:runbook 的 NOSPACE 一节与脚本里「已压缩」的匹配文本用实测原文。
- M2 的 b:用来核定保留量。

【取值(待负责人拍板,见 risks)】
- 保留量 KEEP_REVISIONS = 1000000。按 6.2Z+2 推算的时间窗:1 个 zone 约 34 小时,10 个约 4.3 小时,100 个约 27 分钟;常态占用约 KEEP × b(b 取 200~400 字节时是 200~400 MB)。初稿的 200000 在 100 个 zone 时只剩约 5 分钟。
- 配额 3GiB(3221225472)。判据:配额 × 2 + 1Gi ≤ PVC(8Gi,etcd.yaml:200),给 defrag 的临时文件留余量(defrag 的空间需求是外部常识)。初稿的 4GiB 不满足这条。
- 三处用同一个保留量与同一个配额:etcd-maintenance.yaml、docker-compose.yml、阶段 2 的 etcd.yaml。

【阶段 1a:零共享文件】

1. 新增 deploy/k8s/manifests/infra/etcd-maintenance.yaml。
   - 独立文件:有存档的环境可以只 apply 这一个,不必走 infra-up 连带 apply mysql.yaml;也不会误改 StatefulSet 模板。
   - 不含 `__XXX__` 占位,端点用同 namespace 的短名,可直接 `kubectl apply -n <infra> -f`。
   - 标签一律 `app: etcd-maintenance`,绝不能是 `app: etcd`,否则会被 Service、PDB、StatefulSet 的 selector 选中(etcd.yaml:26-27、:46-47、:63-65、:81-83)。
   - 脚本放进同文件的 ConfigMap,而不是内联在 command 里:这样验证时能把同一份脚本原文拿去临时容器里跑。代价是多一个对象。
   - 只做压缩,不做 defrag,不自动解除告警。
   - 草图:
   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: etcd-maintenance-scripts
     labels: { app: etcd-maintenance }
   data:
     compact.sh: |
       set -u
       : "${KEEP_REVISIONS:?KEEP_REVISIONS is required}"
       OUT=$(etcdctl get etcd-maintenance/probe --consistency=l -w json) || { echo "read revision failed"; exit 1; }
       REV=$(printf '%s' "$OUT" | sed -n 's/.*"revision":\([0-9][0-9]*\).*/\1/p')
       [ -n "$REV" ] || { echo "cannot parse revision"; exit 1; }
       TARGET=$((REV - KEEP_REVISIONS))
       if [ "$TARGET" -gt 0 ]; then
         if CMP=$(etcdctl compact "$TARGET" 2>&1); then
           echo "compacted to revision $TARGET (current $REV, keep $KEEP_REVISIONS)"
         else
           case "$CMP" in
             *"required revision has been compacted"*) echo "revision $TARGET already compacted" ;;
             *) echo "compact failed: $CMP"; exit 1 ;;
           esac
         fi
       else
         echo "nothing to compact (current $REV, keep $KEEP_REVISIONS)"
       fi
       etcdctl endpoint status -w table || echo "WARN: endpoint status failed (not fatal)"
       ALARMS=$(etcdctl alarm list) || { echo "alarm list failed"; exit 1; }
       if [ -n "$ALARMS" ]; then echo "ALARM ACTIVE: $ALARMS"; exit 2; fi
   ---
   apiVersion: batch/v1
   kind: CronJob
   metadata:
     name: etcd-compact
     labels: { app: etcd-maintenance }
   spec:
     schedule: "*/5 * * * *"
     concurrencyPolicy: Forbid
     startingDeadlineSeconds: 120
     successfulJobsHistoryLimit: 1
     failedJobsHistoryLimit: 5
     jobTemplate:
       spec:
         backoffLimit: 0
         activeDeadlineSeconds: 120
         template:
           metadata:
             labels: { app: etcd-maintenance }
           spec:
             restartPolicy: Never
             automountServiceAccountToken: false
             securityContext: { runAsUser: 1001, runAsNonRoot: true }
             containers:
               - name: compact
                 image: bitnamilegacy/etcd:<M0 的 tag>@sha256:<M0 的摘要>
                 imagePullPolicy: IfNotPresent
                 command: ["sh", "/scripts/compact.sh"]
                 env:
                   - name: ETCDCTL_ENDPOINTS
                     value: "http://etcd-0.etcd-headless:2379,http://etcd-1.etcd-headless:2379,http://etcd-2.etcd-headless:2379"
                   - name: KEEP_REVISIONS
                     value: "1000000"
                 volumeMounts:
                   - { name: scripts, mountPath: /scripts, readOnly: true }
             volumes:
               - name: scripts
                 configMap: { name: etcd-maintenance-scripts }
   ```
   - 失败语义:
     - revision 读不到或解析不出就退出 1,不盲压;压缩目标只来自线性一致读;
     - 压缩失败退出 1;对已压缩的 revision 再压视为成功;
     - 某个成员不可达只打 WARN,不让任务失败(滚动窗口里不把「成员在重启」报成「压缩失败」);
     - 查告警失败退出 1(初稿的管道写法在查询失败时等于「没有告警」,是 fail-open);
     - 有告警时照常压缩(压缩是恢复的第一步),但以 2 退出让它可见。
   - 需 Codex 确认:镜像在覆盖 command 后 etcdctl 仍在 PATH(M0);etcdctl 认 `ETCDCTL_ENDPOINTS`(验证步骤 V4-d);「已压缩」的报错原文(探针 A);JSON 里 `"revision":` 的写法。
   - uid 1001 的出处:etcd.yaml:99-105。CronJob 的写法先例:mysql-backup-cronjob.yaml:32-52。

2. deploy/docker-compose.yml 的 etcd 段(前提:探针 B、B2 通过)。本机开发与 Linux 预发共用这份文件。
   ```yaml
   image: bitnamilegacy/etcd:<M0 的 tag>@sha256:<M0 的摘要>   # 与 etcd-maintenance.yaml 逐字相同
   environment:
     # 既有 7 项不动
     - ETCD_AUTO_COMPACTION_MODE=revision
     - ETCD_AUTO_COMPACTION_RETENTION=1000000
     - ETCD_QUOTA_BACKEND_BYTES=3221225472
   ```
   - 已有环境不会自动生效:start_game.ps1:446 带 `--no-recreate --pull never`。手工重建命令写进 runbook;重建等于重启 etcd,C++ 开发节点会撞上 P0-D,需要整栈重启。
   - 按「tag@摘要」钉、且摘要等于本机 latest 的摘要时不需要下载(外部常识,M0 已安排确认)。换了机器或本地没有这个摘要时仍要拉取,属下载,须负责人同意。写法先例是 Java 仓库的 deploy/compose/infra.yaml:29。

3. 新增 tools/scripts/tests/etcd_maintenance_contract.tests.ps1。独立文件,只读静态文件,不调用 k8s_deploy.ps1,不动他人在改的契约测试。CI 会自动扫到(.github/workflows/deploy-config-tests.yml HEAD :111)。骨架照 k8s_migrate_gate.tests.ps1:1-15:`#requires -Version 7`、引 lib/test_harness.ps1、结尾 `exit (Complete-TestRun)`,路径一律 Join-Path。用例:
   - C1:清单存在,含名为 etcd-compact 的 CronJob;`concurrencyPolicy: Forbid`、`restartPolicy: Never`、`backoffLimit: 0`。
   - C2:清单里任何 `app:` 标签的值都不是 `etcd`。
   - C3:清单里没有 `__[A-Z][A-Z0-9_]*__` 形态的占位(可单独 apply;将来进 infra-up 时不会被 fail-closed 检查拦下)。
   - C4:compact.sh 去掉注释行后,含 `--consistency=l` 与 `compact`;不含 `defrag`、`alarm disarm`;告警查询后面不跟管道。
   - C5:KEEP_REVISIONS 是不小于 100000 的整数;compose 的 ETCD_AUTO_COMPACTION_RETENTION 与它相等;compose 的模式是 revision;compose 的配额是正整数。
   - C6:清单与 compose 的 etcd 镜像引用逐字相同,不是 latest,带 `@sha256:`。只断言 etcd 镜像,不加通用的「infra 无 latest」用例(kafka 仍是 latest,那条归 backlog P1-02 / P2-02)。
   - 修复前红:HEAD 上清单不存在,C1–C4 取不到文件;compose 是 latest 且无 env,C5、C6 红。
   - 取脚本块的辅助函数(测试与验证命令共用同一段写法):
   ```powershell
   function Get-BlockScalar([string]$Path, [string]$Key) {
       $lines = Get-Content -LiteralPath $Path
       $hit = $lines | Select-String -Pattern ('^(\s*)' + [regex]::Escape($Key) + ':\s*\|\s*$') | Select-Object -First 1
       if (-not $hit) { throw "找不到 $Key" }
       $indent = $hit.Matches[0].Groups[1].Value.Length
       $body = [System.Collections.Generic.List[string]]::new()
       for ($i = $hit.LineNumber; $i -lt $lines.Count; $i++) {
           $l = $lines[$i]
           if ($l.Trim().Length -eq 0) { $body.Add(''); continue }
           if (($l.Length - $l.TrimStart().Length) -le $indent) { break }
           $body.Add($l)
       }
       $min = ($body | Where-Object { $_.Length -gt 0 } | ForEach-Object { $_.Length - $_.TrimStart().Length } | Measure-Object -Minimum).Minimum
       (($body | ForEach-Object { if ($_.Length -ge $min) { $_.Substring($min) } else { '' } }) -join "`n").TrimEnd() + "`n"
   }
   ```

4. 新增 deploy/k8s/etcd-alerts.yaml(PrometheusRule)。形态与已提交的三份相同:独立文件,手工 `kubectl apply -n observability -f`,不进 infra-up。格式模板用 deploy/k8s/owner-epoch-alerts.yaml(已提交),不用未跟踪的 mysql-connection-alerts.yaml。
   - 规则:
     - 物理库大小 ÷ 配额 > 0.5 warning、> 0.8 critical(配额按物理大小判断是外部常识,所以以物理大小为准);
     - 在用大小 ÷ 配额 > 0.3 warning(压缩跟不上);
     - 当前 revision − 压缩 revision > 2 × KEEP_REVISIONS 持续 15 分钟(压缩停滞);
     - etcd-compact 超过 15 分钟没有成功,或 Job 失败。
   - 文件头写明前提:
     - 抓取:仓库里没有任何对 etcd 的抓取配置,接上之前规则是哑的;按 Service 名 `etcd` 的 client 端口抓,不要按 `app=etcd` 选(etcd 与 etcd-headless 同标签,会重复抓);
     - CronJob 类规则依赖 kube-state-metrics;
     - etcd 指标名待用 M1 / M2 保存的 metrics 文件逐个核对,核对前标「凭记忆」。
   - 不新增 ServiceMonitor 清单:仓库里没有先例,没装 CRD 的集群(例如 kind)apply 会失败。

5. 新增 docs/ops/etcd-maintenance-runbook.md。内容:
   - 日常巡检:endpoint status、alarm list、CronJob 最近一次结果;
   - K8s 上手工触发压缩:`kubectl --context <ctx> -n <infra> create job --from=cronjob/etcd-compact <名字>`;
   - 首次启用压缩后:对每个成员各做一次 defrag,做完核对 dbSize(只压缩不 defrag,库文件不会变小);
   - defrag 规则:手工、逐成员(`kubectl exec -n <infra> etcd-N -- etcdctl defrag`,进入方式见 deploy/k8s/README.md:460-462);先 compact;做之前 3/3 健康;做完等该成员恢复健康再做下一个;开服窗口不做;不用 `--cluster` 一次做完;做之前确认磁盘余量;
   - NOSPACE 处置:alarm list → 取 revision → compact → 逐成员 defrag → alarm disarm → put 验证。报错原文与每一步之后 put 的表现用探针 A 的实测输出填;
   - compose 与预发单机一节:没有 CronJob;env 生效前用 `docker exec etcd etcdctl compact <revision>` 手工压;改完 compose 后的重建命令,以及重建等于重启 etcd;
   - 保留量与规模的关系:上面的时间窗表,扩 zone 时要同步调大;
   - 改配额、升镜像、加 env 必须走滚动窗口及其前置(P0-D);
   - Git Bash 下以 `/` 开头的键要加 `MSYS_NO_PATHCONV=1`,或改用 PowerShell;kubectl 一律显式带 context。
   同步两处:
   - docs/ops/release-checklist.md:329 改为指向这份 runbook;
   - docs/README.md「运维手册」一段按文件名顺序插一行(cross-zone-failure-test-runbook 之后、gate-kernel-tuning-runbook 之前),格式照 :247-249,标题取 runbook 的第一个 `#` 标题(生成器规则 tools/scripts/gen_docs_index.py:96-101、:106-115)。该文件有他人未提交的索引行,只插一行,不动别的。交付说明写明「索引为手工对齐,生成器未运行(本机无 Python)」。

6. docs/PROGRESS.md 末尾追加一条(追加前重新读文件末尾,另一个会话也在追加)。要点:
   - 核实结论与实测结果;1a 落了什么;「未验证,待 Codex 执行」;
   - 「backlog P1-02 的 etcd 部分由本项承接,kafka 部分仍 open」;
   - 「Java 版:不适用(Java 版不使用 etcd);对应 Java 版本号 0.1.0-SNAPSHOT,PARITY.md 已登记」。

【阶段 1b:接入 infra-up(等 G2b 提交或负责人同意)】
- tools/scripts/k8s_deploy.ps1,按届时的实际行号定位:
  - `$infraManifests` 在 "etcd.yaml" 后加 "etcd-maintenance.yaml"(HEAD :5760,工作区 :6178);
  - Show-InfraStatus 的资源列表加 `cronjob`(HEAD :5922,工作区 :6340);
  - 「五个 infra manifest」的两处注释同步(HEAD :5592、:5879)。
- 契约测试补一条「infra-up 的 DryRun 产物里有 etcd-compact」。
- deploy/k8s/README.md 的 etcd 一节(:444-466)追加「维护」一段;deploy/k8s/AGENTS.md:24、:82 同步。
- .github/workflows/deploy-config-tests.yml 的 push 与 pull_request 两段 paths 各加一行 `deploy/docker-compose.yml`。在此之前只改 compose 的提交不会触发测试,交付说明写明「compose 侧暂不受 CI 保护」。
- 在 1b 之前,新环境要手工 apply etcd-maintenance.yaml,这一点写进 runbook。若阶段 2 能紧跟着落,1b 的 `$infraManifests` 一行可与阶段 2 合并。

【阶段 2:etcd 滚动窗口】前置是 P0-D 修复并经 Codex 验证。
1. etcd.yaml 的 StatefulSet:
   - :114 钉到与另两处逐字相同的引用。必须等于各环境当前实际运行的版本,只许同版本或升级,不许降级;各环境先逐成员核对 imageID;
   - env 加三项,取值与 compose 相同;
   - Pod 模板加 prometheus.io 注解(写法见 chat.yaml:52-54);
   - 更新 :188-190 注释。
2. 测试文件加 C7:etcd.yaml 的 StatefulSet 块含三项 env 且与 compose 相等;三处镜像引用逐字相同;配额 × 2 + 1Gi ≤ PVC。
3. 新旧并存(外部常识,未核实):压缩经 raft 复制,任一成员发起即全员生效;配额各成员本地判断,库远小于 2GiB 时混跑无影响。滚动由 RollingUpdate 逐个进行(etcd.yaml:79-80),PDB minAvailable 2(:62)加 readiness 把关。
4. 回滚:去掉 env 再滚一次;已压缩的历史不可恢复,但业务不读历史。
5. 稳定后 etcd-compact 保留一个版本做兜底(对已压缩的 revision 再压无害),之后删除。

【交 Codex 的验证命令(1a 落码后)】PowerShell 7;本项无需任何编译。没有 Codex 结果之前,交付说明写「未验证,待 Codex 执行」。

V0 记录现场:
```powershell
git -C D:\luyuan\xuanming-server-mmo-cpp rev-parse HEAD
git -C D:\luyuan\xuanming-server-mmo-cpp status --short
```
修复不会被提交,改动只在主工作区,Codex 自己的 worktree 看不到。所以 V1、V3、V4 都读主工作区的文件(只读,不在里面写任何东西)。

V1 新契约测试:
```powershell
pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\etcd_maintenance_contract.tests.ps1; "exit=$LASTEXITCODE"
```
- 通过标准:退出码 0,C1–C6 全过。失败时保留完整输出。
- 既有的 k8s_deploy_contract.tests.ps1 不属于 1a 的通过标准(别的会话正在改它,基线是移动靶)。

V2 修复前红(干净的临时 worktree,只拷测试文件,做法同 godb 交接 :124):
```powershell
$wt = Join-Path $env:TEMP 'etcd-p0c-red'
git -C D:\luyuan\xuanming-server-mmo-cpp worktree add --detach $wt 0aa1027bc
Copy-Item D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\etcd_maintenance_contract.tests.ps1 (Join-Path $wt 'tools\scripts\tests')
pwsh -NoProfile -File (Join-Path $wt 'tools\scripts\tests\etcd_maintenance_contract.tests.ps1'); "exit=$LASTEXITCODE"
git -C D:\luyuan\xuanming-server-mmo-cpp worktree remove --force $wt
```
- 期望:退出码非 0,失败信息点名「找不到 etcd-maintenance.yaml」与 compose 的镜像 / env。

V3 compose 只解析、不起容器:
```powershell
docker --context desktop-linux compose -f D:\luyuan\xuanming-server-mmo-cpp\deploy\docker-compose.yml --profile redis-cluster config --quiet; "exit=$LASTEXITCODE"
```
- 期望退出码 0(`--profile` 的出处 start_game.ps1:445)。

V4 脚本功能(临时容器,不碰开发栈)。用上面的 Get-BlockScalar 把脚本原文取出来:
```powershell
$out = Join-Path $env:TEMP ("etcd-p0c-v-" + (Get-Date -Format 'yyyyMMdd-HHmmss')); New-Item -ItemType Directory $out | Out-Null
# 先在本会话里定义 Get-BlockScalar(见上)
[IO.File]::WriteAllText("$out\compact.sh", (Get-BlockScalar 'D:\luyuan\xuanming-server-mmo-cpp\deploy\k8s\manifests\infra\etcd-maintenance.yaml' 'compact.sh'))
docker --context desktop-linux run -d --pull never --name etcd-p0c-v -e ALLOW_NONE_AUTHENTICATION=yes bitnamilegacy/etcd:latest
Start-Sleep 10
docker --context desktop-linux cp "$out\compact.sh" etcd-p0c-v:/tmp/compact.sh
docker --context desktop-linux exec etcd-p0c-v sh -c 'i=0; while [ $i -lt 300 ]; do etcdctl put p0c/k$i v >/dev/null; i=$((i+1)); done'
docker --context desktop-linux exec -e KEEP_REVISIONS=100 etcd-p0c-v sh /tmp/compact.sh; "a exit=$LASTEXITCODE"
docker --context desktop-linux exec -e KEEP_REVISIONS=100 etcd-p0c-v sh /tmp/compact.sh; "b exit=$LASTEXITCODE"
docker --context desktop-linux exec -e KEEP_REVISIONS=100000000 etcd-p0c-v sh /tmp/compact.sh; "c exit=$LASTEXITCODE"
docker --context desktop-linux exec -e KEEP_REVISIONS=100 -e ETCDCTL_ENDPOINTS=http://127.0.0.1:1 etcd-p0c-v sh /tmp/compact.sh; "d exit=$LASTEXITCODE"
docker --context desktop-linux exec etcd-p0c-v sh /tmp/compact.sh; "f exit=$LASTEXITCODE"
```
- 期望:
  - a:退出 0,打印 `compacted to revision`;
  - b:退出 0,打印 `already compacted`;
  - c:退出 0,打印 `nothing to compact`;
  - d:退出 1(读不到 revision 即失败)。若仍为 0,说明 etcdctl 不认 `ETCDCTL_ENDPOINTS`,CronJob 必须改成显式 `--endpoints`,停下报告;
  - f:缺 KEEP_REVISIONS,退出非 0。
- e(NOSPACE):按测量单 M3 探针 A 的做法起一个 16MiB 配额的容器并灌满(fill.sh),拷入同一份 compact.sh,以 `KEEP_REVISIONS=100` 运行。期望退出 2,打印 `ALARM ACTIVE`。
- 结束后 `docker --context desktop-linux rm -f -v` 删掉本步创建的临时容器。
- 失败时保留每条命令的完整输出。

V5 kind 上的功能验证(可选;只在 kind-mmorpg 集群已存在且已部署 infra 时做;每条都带 context 与 kubeconfig;namespace 以实际为准):
```powershell
$kc = "$env:TEMP\kind-mmorpg.kubeconfig"; $ns = 'mmorpg-infra'
kubectl --context kind-mmorpg --kubeconfig $kc get node
kubectl --context kind-mmorpg --kubeconfig $kc -n $ns get sts etcd -o jsonpath='{.status.currentRevision}'
kubectl --context kind-mmorpg --kubeconfig $kc apply -n $ns -f D:\luyuan\xuanming-server-mmo-cpp\deploy\k8s\manifests\infra\etcd-maintenance.yaml
kubectl --context kind-mmorpg --kubeconfig $kc -n $ns create job --from=cronjob/etcd-compact etcd-compact-manual
kubectl --context kind-mmorpg --kubeconfig $kc -n $ns wait --for=condition=complete job/etcd-compact-manual --timeout=120s
kubectl --context kind-mmorpg --kubeconfig $kc -n $ns logs job/etcd-compact-manual
kubectl --context kind-mmorpg --kubeconfig $kc -n $ns get sts etcd -o jsonpath='{.status.currentRevision}'
```
- 通过标准:Job Complete;日志含 `compacted to revision` 或 `nothing to compact`;StatefulSet 的 currentRevision 前后不变(证明没有滚动)。
- Pod 卡在拉镜像:kind 节点里没有这个摘要的镜像(见 risks 第 9 条),保留 `describe pod` 输出后停下。
- 失败时保留 Job 日志全文与 `describe job` 输出。

V6 本机 compose 生效验证(可选,要重建开发 etcd 容器,属重启基础设施,须负责人同意,并先停本机开发栈):重建后配额指标为 3221225472,10 分钟内压缩 revision 大于 0。

【回归测试小结】
- 位置:新文件 tools/scripts/tests/etcd_maintenance_contract.tests.ps1;不涉及 CMakeLists / vcxproj。
- 修复前红、修复后绿由 V2 与 V1 给出;脚本行为由 V4 给出(a–f 六种情形,含 fail-closed 的 d)。

【收尾登记】
- docs/PROGRESS.md:见 1a 第 6 条。
- 交接文档 handoff-global-singletons-10m-ccu-20261013.md:在文末追加订正小节(做法同 godb 交接 §9),不回头改 §3 的表格行。
- Java 仓库 PARITY.md:见 java_parity。

### 原报告:Java 版

结论:不适用(Java 版不使用 etcd)。

证据(Java 仓库 D:/luyuan/xuanming-server-mmo,只读;HEAD d942f74,工作区干净):
- deploy/compose/infra.yaml:11:「只放依赖。基线的 etcd / Nacos / Redis Cluster / kafka-ui / topic-init 都不移植(spec §2.3)。」该文件的镜像只有 mysql / redis / kafka(:29、:60、:74)。
- game-discovery/src/main/java/com/game/discovery/killswitch/RedisKillSwitchSync.java:28-29:「基线是 etcd list-watch;Java 版没有 etcd,规则少、全量读一个小哈希每秒一次足够便宜」。
- PARITY.md:36 已有同类先例:「节点号租约有效期判定 … 无对应(mmorpg 用 etcd 租约 / 各自 snowflake) … 不适用(Java 内部)」。
- 本轮 grep:pom.xml、yaml、properties 里除 infra.yaml:11 那条注释外没有 etcd / jetcd;含 etcd 字样的 Java 文件是注释、同步来的 proto 生成物和键名常量,没有 etcd 客户端代码。

可借鉴:Java 版的 compose 镜像已按「tag@摘要」钉死(infra.yaml:9、:29),本仓钉 etcd 版本时用同一写法。

PARITY.md 登记(只追加一行,7 栏,格式照 :25 的表头):
| etcd MVCC 历史压缩 / 配额 / 镜像钉版本(运维) | `deploy/k8s/manifests/infra/etcd-maintenance.yaml`、`deploy/docker-compose.yml`(阶段 2 再加 `etcd.yaml`) | 未提交(工作区,基于 `0aa1027bc`) | — | 0.1.0-SNAPSHOT | 不适用(Java 版不用 etcd) | Java 版依赖只有 MySQL / Redis / Kafka(`deploy/compose/infra.yaml:11`);节点目录与热关停走 Redis。不涉及客户端契约 |

登记时注意:
- 「mmorpg commit」栏先写「未提交(工作区,基于 0aa1027bc)」,用户要求提交后再更新。
- 这是跨仓库写入。动之前按 Java 仓库自己的 AGENTS.md 检查工作区;它要求提交前构建与测试通过、PARITY 已登记(AGENTS.md:86),本项只改文档,不提交。
- 本仓 PROGRESS 条目按 AGENTS §12 写明:「Java 版:不适用;对应 Java 版本号 0.1.0-SNAPSHOT,PARITY.md 已登记」。

未核实:Java 版用 Redis 承担注册与热关停后,有没有对应的「无界增长」风险(Redis 键 TTL、Kafka 保留期)。不属于本项,没有查。

### 原报告:风险与未决

【一、与 P0-D(C++ etcd 流重建)的交叉:压缩开启后 watch 的行为】

1. C++ 现状(代码事实):
   - 建 watch 带 start_revision 与 prev_kv:etcd_helper.cpp:50 `createReq.set_prev_kv(true);`、:57-60。
   - 续 watch 的 revision 有两个来源:每 2s 的 Range 应答(etcd_service.cpp:51、:67-69 `revision[prefix] = nextRevision;`)和任意 watch 应答(:369-378)。
   - 取消时::361-366 打 `Watch canceled` 后 2s 重建全部前缀(:393-400、:278-285)。
   - 四个缺口:
     - (i) 不看 compact_revision,重建前也不重新对账。契约见 proto/etcd/etcd.proto:444-452:带压缩 revision 的取消之后不应再用同一个起点建 watch。
     - (ii) Range 应答只当 PUT 处理(:58-59),节点下线只来自 watch 的 DELETE;空档里丢掉的 DELETE 永远补不回来,会留下幽灵节点。
     - (iii) 一个 watch 被取消就重建全部前缀,健康的那些会重复(etcd_helper.cpp:65-68 的 StopAllWatching 还是 TODO)。这条是推断。
     - (iv) 摘除节点完全依赖 DELETE 事件里的前值:etcd_service.cpp:388 `HandleDeleteEvent(event.kv().key(), event.prev_kv().value());`;前值为空时 node.cpp:1339-1345 解析失败后直接返回,节点不摘除。契约 proto/etcd/etcd.proto:400-401 写明前值已被压缩时不返回。今天没有压缩,这条路径不存在。
   - 今天的暴露面(推断):每 2s 的 Range 把起点刷到最新,保留量在分钟级以上时,C++ 几乎不会从已压缩的位置建 watch;流断开后根本不重建,所以压缩既不加重也不缓解 P0-D。(iv) 何时真的丢前值取决于服务端取前值的时机,是外部常识,由探针 D 与 M6 确认。
   - 交接 §4.4 的修法是「启动 1 次 Range,之后只消费 watch」,那样 2s 刷新就没了。P0-D 的验收因此要写进四条:
     - 重建流时先 Range 再 watch;
     - 遇到带压缩 revision 的取消时重新 list 并对账删除;
     - 不重复建 watch;
     - DELETE 处理不得依赖前值(键里已有 zone / node_type / node_id,etcd_manager.cpp:47-58,可按键在本地注册表反查,或 Range 后对账)。
   - 改动位置:生成器模板 tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl、grpc_init_total.cpp.tmpl,或非生成的 EtcdService。归 P0-D,不在本项。

2. Go 现状:
   - 代码事实:所有 watch 循环都是「通道关闭或 Err 非空就返回并全量重同步」:allocator.go:1014-1019(之后 :1233-1245 先 Get 校验归属再从当前 revision 重建);killswitch/watch.go:103-112 返回后回到 :37-61;scenenode/watcher.go:183-191;load_reporter.go:823-830。
   - 推断,待 M6 实测:「对压缩安全」。压缩是否一定以 Err 非空或通道关闭的形式到达,取决于 etcd Go 客户端,本机没有它的源码。
   - go-zero 内置 discov 订阅对压缩的处理未核实。
   - 全仓没有手写代码读事件里的前值,Go 侧不受 (iv) 影响。

3. Java gateway 只做当前 revision 的 Range(GateWatcher.java:85-87),不受压缩影响。

【二、仍未核实的外部事实(由 Codex 实测确认)】
4. bitnamilegacy/etcd 是否预置压缩默认值;是否用配置文件启动;三项 env 是否生效;已有数据目录重启后是否仍生效 —— M2、M3 探针 B / B2。
5. etcd 默认配额 2GiB;NOSPACE 下 put / txn / lease grant 被拒、读与删除放行、续租不受影响;告警不会自动解除;配额按物理库大小判断 —— M3 探针 A。
6. 每个 revision 的字节数 —— M2。「10 区 1~3 天、100 区几小时」在此之前只是推算。
7. 服务端取前值的时机,已同步的 watch 会不会丢前值 —— M4 探针 D、M6。
8. 续租、失败的 CAS、LeaseGrant 不产生 KV revision;压缩经 raft 全员生效;defrag 是成员本地操作并阻塞该成员、临时需要额外磁盘;`podManagementPolicy: Parallel` 不影响 RollingUpdate 逐个更新 —— 本单没有安排专门的测法,阶段 2 之前由 Codex 在 kind 上确认。
9. 按「tag@摘要」钉的镜像:本机 docker 在摘要已缓存时是否不需要下载(M0 已安排);kind 里用 `kind load` 载入的镜像能否被带摘要的引用命中(V5 才能确认)。命中不了时 K8s 侧退回只钉 tag,由负责人定。与 kind-config.yaml:26-27 记的 backlog P2-03 是同一类问题。
10. 静态盘点之外有没有别的写入方、真实的 revision/s —— 只有 M5(真栈)能确认。当前两个工作树都没有可运行的二进制,需要先构建。

【三、需负责人拍板】
11. 是否同意 1a 现在落码,以及其中三件事:
    - 改 deploy/docker-compose.yml(交接 §9 点过名,现已提交、工作区干净);
    - 在 docs/README.md、docs/PROGRESS.md 各追加一行(两个文件都有他人未提交的改动,只追加不动别的);
    - 新建 docs/ops/etcd-maintenance-runbook.md(属 AGENTS §5 的 docs/ops 类,但 §9 写了「不为记录改动新建 markdown」)。
12. 1b 要改 tools/scripts/k8s_deploy.ps1、契约测试、deploy/k8s/README.md、CI workflow,它们正被另一个会话修改(交接 §9、godb 交接 :300)。是等对方提交,还是现在就允许在同一文件里各改两行。同一文件混入两方未提交改动后,无法只提交本项。
13. 保留量:1000000(100 个 zone 约 27 分钟,常态约 200~400 MB)还是 200000(约 5 分钟,约 40~80 MB)。正确性不依赖这个窗口(C++ 的起点每 2s 刷新,Go 出错即重同步),它只决定余量和日志噪声;M2 出了 b 之后再定。扩 zone 时要同步调大。
14. 配额:2GiB(显式写出默认值)、3GiB(本稿建议)还是 4GiB(初稿;不满足「配额 × 2 + 1Gi ≤ PVC」)。PVC 是 8Gi 且 volumeClaimTemplates 不能在线改。
15. defrag 是否自动化。本稿按 YAGNI 不提供 CronJob,只在 runbook 里写手工逐成员流程。
16. 钉哪个版本:必须等于各环境当前实际运行的版本,先逐环境逐成员核对 imageID;禁止降级。bitnamilegacy 是冻结归档,是否镜像到自有仓库也要定(AI 不能 docker push,AGENTS §9)。
17. 阶段 2 的窗口与 P0-D 的先后;阶段 2 之后何时删除 etcd-compact。
18. 是否授权为 M5 构建开发栈(可能要下载 Go 模块、third_party、Maven 发行版),以及 M6 的不可逆压缩与重启。
19. 另起的后续项,不在本项夹带:
    - 删掉过渡期镜像双写(allocator.go:1369「发布一版之后删」),字节增速约减半,涉及滚动升级兼容;
    - 号段模式下 login 是否还需要每秒两笔水位(每 zone 6.2 笔里它占 4 笔,护的是默认不发号的发号器);
    - 1s 的水位节拍本身是正确性约束(allocator.go:926-933),不动;
    - capacity-model-hok-scale.md:212 的「4Z」订正为约 6Z 并注明双写。

【四、与另外两份交接的重叠和冲突】
20. go/db 交接 §2.4(:91):L-04 部署前不得对有真实存档的环境 apply 新的 mysql.yaml,而 infra-up 会一并 apply 它。所以 1a 在这类环境只能单独 `kubectl apply -f etcd-maintenance.yaml`,由负责人执行;Claude 和 Codex 都不碰生产集群(:92)。
21. launch-readiness 交接 §4 第 7 项要求的「钉镜像版本、配额、defrag」已并入本设计(钉版本在 1a 的 CronJob 与 compose、阶段 2 的 etcd.yaml;配额在 compose 与阶段 2;defrag 在 runbook)。
22. backlog P1-02 / P2-02(handoff-backlog-2026-09-05.md:162、:877)与本项重叠:本项只承接 etcd,不加通用的「infra 无 latest」用例。

【五、其它风险】
23. 首次在已经涨大的环境上压缩:一次要清掉大量历史,期间 etcd 延迟可能升高(外部常识);之后库文件不会变小,必须紧跟逐成员 defrag。runbook 要求在低峰做,做完核对 dbSize。
24. CronJob 的 Pod 可能被调度到没有缓存该镜像的节点,需要从 Docker Hub 拉取;拉不到任务就失败,要靠告警发现。而仓库里现在没有任何对 etcd 的抓取配置,告警规则在接上抓取之前是哑的。
25. 在 1b 之前,新建的环境不会自动带上 etcd-compact,要靠 runbook 提醒手工 apply;漏了就回到今天的状态。
26. compose 改完后,既有的开发机与预发机要手工重建 etcd 容器才生效;重建会重启 etcd,C++ 节点撞上 P0-D,需要整栈重启。新机器或本地没有该摘要时要先拉镜像。
27. 在 1b 之前,只改 compose 的提交不会触发 CI 里的契约测试。
28. 修复不提交,Codex 自己的 worktree(停在 73b852f32)看不到改动;验证命令已写明读主工作区的文件。主工作区同时有别人在写,V0 要记录当时的 HEAD 与 git status。

### 原报告:预计改动文件

- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/manifests/infra/etcd-maintenance.yaml
- D:/luyuan/xuanming-server-mmo-cpp/deploy/docker-compose.yml
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/etcd_maintenance_contract.tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/etcd-alerts.yaml
- D:/luyuan/xuanming-server-mmo-cpp/docs/ops/etcd-maintenance-runbook.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/ops/release-checklist.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/README.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/PROGRESS.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/handoff/handoff-global-singletons-10m-ccu-20261013.md
- D:/luyuan/xuanming-server-mmo/PARITY.md
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/k8s_deploy.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/k8s_deploy_contract.tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/README.md
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/AGENTS.md
- D:/luyuan/xuanming-server-mmo-cpp/.github/workflows/deploy-config-tests.yml
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/manifests/infra/etcd.yaml

## P0-D C++ etcd 双向流 ok=false 后不重建


### 结论

- 第一轮判定:`confirmed_static`。
- 在 `2f897bd07` 上是否仍成立:是。

**把握**:

claim_verdict 的把握:high(仓库内的因果链);medium(依赖外部语义的后果细节);实测方案的可执行性:low(受环境阻塞)。

high 的部分:「ok=false 被顶层分发丢弃 → 不再续读、写标志卡死 → 没有任何重建 → 续租无 ACK → 约 181s 本地判失租并重注册、循环;2s 的 Range 只增不删」。链上每一步我都回到 HEAD 逐行重读(E1~E15),流的创建点和完成队列轮询点都是全仓唯一,模板和生成物一致,相关路径在 73b852f32..HEAD 之间没有差异。两位复核人各自重读后同意,并专门找过能让缺陷不成立的保护逻辑(第二个轮询点、通道或存根重建、失租自杀后被拉起、探针杀容器),都不存在。仓库自己的设计文档([历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md):17、:261)在 2026-09-28 独立写过同一结论。

medium 的部分:
1. gRPC 对断开的流返回 ok=false、流不会自愈。这是该异步接口的标准语义,但两棵工作树的 gRPC 子模块都是空的,没能对着头文件核实。
2. 「从第 2 轮起键会被短暂删除、gate 把会话绑定置无效、scene_manager 当成节点离开、node_id 被抢、启动顺序会挂死」都依赖 etcd 租约时序和运行行为,没有运行证据。

low 的部分:本机今天没有任何能运行的 C++ 产物,恢复环境要下载和安装工具,scene 库还有 110 个与本项无关的编译错误。这不影响静态结论,但决定了「先实测再落码」需要负责人先做选择。

### HEAD 前进后的更正(0aa1027bc → 2f897bd07)

区间 0aa1027bc..2f897bd07 共 13 个提交。缺陷链上的文件这段区间全部零差异:etcd_grpc_client.{h,cpp}、etcd_service / etcd_manager / etcd_helper、node.cpp、node_allocator.cpp、service_discovery_manager.cpp、node_connector.cpp、grpc_channel_cache.h、三个 gRPC 客户端模板、grpc_process.go、model.go、proto/etcd、cpp/generated/proto/etcd、bin/etc/base_deploy_config.yaml、cpp_nodes.ps1、dev_tools.ps1、build_grpc.ps1、docker-compose.yml、etcd.yaml、rpc_controller_test,以及各份引用文档。core 下只有 node_kafka_command_handler.h 改了一行日志文案。与本项有关的改动分四组:

1. 898c803ea(10-10):cpp/generated/grpc_client/grpc_init_client.cpp 增加 58 行;另新增 guild/guild_internal_grpc_client.{h,cpp}、match/match_internal_grpc_client.{h,cpp},data_service_grpc_client.{h,cpp} 扩充。新增的 guild_internal(消息号 244)、match_internal(245)都是 unary;data_service 的分发条件加了 248。
   - 更正主线程的环境说明:「grpc_init_client.cpp 在这段区间没有改动」不成立。
   - 影响结论:否。!ok 分支逐字未变,只是从 :365-369 下移到 :408-411。全仓除 etcd 外仍没有任何流式 RPC,E12「第二个使用方只有 unary」仍成立。
   - 影响实测:是。第 0 步 `git log -1 -- grpc_init_client.cpp …` 的期望值从 `a8e9ffb1f 2026-10-07` 变为 `898c803ea 2026-10-10`。生成文件 34 个变 38 个(18 对客户端加 grpc_init_client.{h,cpp}),顶层分发覆盖 15 个变 17 个(db 仍被跳过)。
   - 影响修复设计:只改护栏数字,不改方案。护栏 a「其余 31 个生成文件零差异」改为 35 个;护栏 c 的 etcd 消息号行从 :393 移到 :434-435,11 个号不变;护栏 b 的 etcd 前置声明仍在 :81;「把 ok 推广到全部客户端文件」的规模从 15 个变 17 个。

2. e4ff1d165:把昨天工作区里他人未提交的改动入库。
   - tools/scripts/k8s_deploy.ps1:本项引用处统一下移 432 行,语义不变。diff 里与 etcd、租约、探针相关的只有英文化文案。新位置:
     - New-NodeConfigMapYaml :2109;TTL 与保活取值 :2126-2127;Etcd 段 :2207-2211(Hosts 仍只有 :2209 一项);LogLevel 1 在 :2222;前缀列表 :2224-2242。
     - New-NodeDeploymentYaml :2321;探针共同约束 :2385-2397;gate 不加 startup / liveness :2399-2412;scene 同 :2413-2436;battle startupProbe :2449-2453(2s×150)。
     - NodeRemovalGraceSeconds 仍不在模板里。
     - 影响结论:否;实测:只改行号;修复:否(本修复不碰该脚本)。
   - tools/scripts/tests/k8s_deploy_contract.tests.ps1(+309 行):DryRun 基线仍在 :32-45。相关用例的位置:NodeTTLSeconds 用例 :866,battle :1164,gate 探针 :1176,scene 与 Agones :1274。该文件含孤立 CR,Git Bash 的 grep 会少算 4 行,所以按用例名定位。影响:可以加一步「部署渲染核对」(M3),不影响结论和修复。
   - tools/proto_generator/protogen/go.mod:go 1.26.5 改为 1.27.2。昨天报告写「要求 1.27.2」,其实读的是未提交工作区;现在 HEAD 就是 1.27.2,Codex 有 Go 1.27.2(PROGRESS:7636)。「可能要下载工具链」这一顾虑消除。
   - run_cpp_tests.ps1:末行汇总改成英文(:196 `<n> test projects failed`、:199 `All passed (...)`),表格结果列仍是中文「N/M 过,挂:<用例>」(:184)。影响判据文本。
   - start_game.ps1、gate/main.cpp、client_message_processor.cpp:只改日志文案,行数不变。本项引用的这些行都没有漂移:start_game :112、:266-267、:270、:446、:450、:551;gate :290、:325、:329、:405、:408-411;client_message_processor :53-81、:775-779、:1104-1113。
   - 判据用到的 C++ 日志全在 core,没被英文化改动碰到。Codex 793e 树里在途的英文化改动也不涉及 etcd 与 node.cpp。
   - deploy/k8s/manifests/infra 下改的 redis-ha / kafka-topic-init / mysql 与 etcd 无关。

3. 0d34dcf8d(10-13):新增 cpp/generated/table/code/equipaffixpool_table.h 等装备表产物,0aa1027bc 上没有这些文件。影响结论:否。影响实测:「scene 110 个错误 / 装备表缺失」这条阻塞原因失效,改为「scene 在 HEAD 能否编过未核实」。影响修复:否。

4. 2f897bd07 及前一个 battle handler 提交:
   - 改动:protogen cmd/root.go 加 `--grpc-handler` 定向入口;proto_gen.yaml 加一行,enable_unity_client 从 :243 移到 :244,team 段的副本说明从 :503-511 移到 :504-512;新模板 grpc_service_handler.*.tmpl 用 go:embed 装入;cpp/nodes/battle/* 也有改动。
   - 没动的:三个 gRPC 客户端模板和 grpc_process.go;客户端模板仍在运行时从磁盘读(internal/utils/file.go:72-73)。
   - 影响结论:否。
   - 影响实测:
     - Codex 10-14 用 Go 1.27.2(GOTOOLCHAIN=local、-mod=readonly)跑通了 internal/generator/cpp 的测试,并构建、两次运行了生成器二进制(PROGRESS:7636、:7642)。所以 Go 模板红测现在就能跑;对定向模式而言,「生成器被应用控制拦截」已不成立。
     - 同一条记录给出新的 C++ 编译阻塞:Boost 是模块化布局,导致 C1083 找不到 boost/any.hpp(PROGRESS:7637)。
   - 影响修复设计:否。regen 仍要全量 proto-gen-run,定向入口只生成 handler。

另:Java 仓 HEAD 从 d942f74 到 52cccf2(8 个提交),只有 PARITY.md 多了 5 行;game-discovery、gate presence、scene ownership 都没动。java_parity 结论不变。

【在 HEAD 2f897bd07 上逐跳重读,全部成立】
- E1:etcd.proto:42、:56 是双向流。
- E2:流只建一次。etcd_service.cpp:31 → grpc_init_client.cpp:486、:488(emplace 完成队列)、:504-505 → etcd_grpc_client.cpp:974-1010,全用 registry.emplace。
- E3:全仓只有 etcd_service.cpp:35 一处轮询。
- E4:grpc_init_client.cpp:407-411 遇 `if (!ok) { LOG_ERROR << "RPC failed"; return; }` 整条丢弃;模板 grpc_init_total.cpp.tmpl:84-88 相同。
- E5 / E6:写标志只在成功的 WRITE 里复位(KeepAlive :615 置位、:629 复位;Watch :389、:403);读只在成功分支续发(:415-423、:641-649);环形缓冲容量 200(.h:246)。
- E7:FINISH 分支是 `cq.Shutdown()`(:413、:639);cpp 非生成代码里 WritesDone / TryCancel / wait_for_ready 零调用;StopAllWatching 是空 TODO(etcd_helper.cpp:65-68)。
- E8 / E9:续租定时器 etcd_manager.cpp:191-199;ACK 只在 etcd_service.cpp:611 记时;判定 :622-624;触发 node.cpp:1537-1546;复位条件 node.cpp:1555-1561。
- E10:2s 的 Range 只做新增和更新(etcd_service.cpp:58-59),删除只有 watch DELETE 一条路(:386-389)。
- E11~E15 引用的行全部未漂移。

外部语义 X1 现在可以对着头文件核实:third_party/grpc(c876f4d,gRPC C++ 1.83.0)的 include/grpcpp/completion_queue.h 有明文约定:
- StartCall ok=false 表示 RPC 不会上线(:145-150);
- Write ok=false 表示调用已死(:152-158);
- Read ok=false 表示此流再也读不到消息(:160-167);
- 客户端 Finish 的 ok 恒为 true(:169)。
这些与 E4~E7 拼成完整的因果链。仍然没有运行证据。

**失效的说法**:

1. 【行号:grpc_init_client.cpp】
   - E4 的 :365-369、handoff_corrections 1 的 :358-368 / :366-369,改为:HandleCompletedQueueMessage 在 :400,while 在 :407,!ok 加 return 在 :408-411。
   - E2 的 :438(emplace)改为 :488;:454-455 改为 :504-505。
   - entry_point_inventory 的「grpc_init_client.cpp:358-434」改为 :400-484。
   - fix_design【六】5.c 的「HEAD :393」改为 :434-435;5.b 的 :81 不变。
   - 主线程说「该文件区间内没改」,不对,见 head_delta 第 1 组。
2. 【claim_evidence 首段】「git diff --stat 73b852f32 HEAD -- cpp/generated/grpc_client cpp/libs/engine/core/node tools/…/template 为空」。对 2f897bd07 已不为空:多了 grpc_init_client.cpp、guild_internal、match_internal、data_service 的生成物、node_kafka_command_handler.h 和 grpc_service_handler 模板。改为:只看 etcd 相关路径(etcd_grpc_client.*、etcd/*、node.cpp、grpc_init_total / grpc_async_client 模板)零差异。
3. 【生成文件计数】「16 个文件对加 grpc_init_client 共 34 个生成文件,顶层分发覆盖 15 个」改为:18 对共 38 个,覆盖 17 个,db 仍被跳过。regen 护栏「其余 31 个」改为 35 个;「推广到全部 15 个客户端文件」改为 17 个。
4. 【第 0 步期望】「最后一条应恰为 a8e9ffb1f 2026-10-07」改为拆成两条:
   - `git log -1 … -- cpp/generated/grpc_client/etcd/ cpp/libs/engine/core/node/system/etcd/` 期望 `a8e9ffb1f 2026-10-07`;
   - `… -- cpp/generated/grpc_client/grpc_init_client.cpp` 期望 `898c803ea 2026-10-10`。
5. 【k8s_deploy.ps1 函数与模板位置】这些出处都失效:
   - E9 的「HEAD :1694-1695、:1778-1779;工作区 :2035-2036、:2119-2120」;
   - handoff_corrections 8 的「HEAD :1777,工作区 :2118」;
   - 「找过但不存在的保护逻辑」里的 :1959、:1971、:1993、:2017;
   - 入口清单的 :1694-1695、:1775-1779、:1789-1810。
   在 2f897bd07 上:New-NodeConfigMapYaml :2109,TTL / 保活 :2126-2127,Etcd 段 :2207-2211(Hosts :2209,仍只有一项),LogLevel :2222,前缀 :2224-2242,New-NodeDeploymentYaml :2321,探针约束 :2385-2397,gate :2399-2412,scene :2413-2436,battle startupProbe :2449-2453。旧行号整体加 432 即得。
6. 【契约测试位置】「HEAD :32-45」仍然有效。补充用例位置(按 Grep 工具行号):NodeTTLSeconds :866(0aa1027bc 上在 :813)、battle :1164、gate :1176、scene / Agones :1274。该文件有孤立 CR,不同工具算出的行号会差 4 行,一律按用例名引用。
7. 【工作区不干净】这些说法失效:handoff_corrections 11、risks 23,fix_design【六】的「这棵树有他人未提交的文档 / Go / 部署脚本改动」,【七】的「PROGRESS.md、README.md 当前都有他人未提交修改」,入口清单的「工作区该文件有他人未提交的 431 行」。现状:那批改动已在 e4ff1d165 等提交入库,主工作区只剩 third_party/librdkafka、third_party/ue5navmesh 两个子模块的脏标记。提交时逐文件暂存的规矩照旧。
8. 【编译阻塞原因】
   - 「两棵工作树 19 个子模块全未检出(首列全是 -)」,改为:主工作区已初始化 14 个,未初始化的是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash。
   - 「third_party/grpc 是空目录」,改为:源码已检出(c876f4d,1.83.0)。但嵌套子模块只检出了 abseil-cpp、protobuf,boringssl-with-bazel、cares、re2、zlib、googletest 等都没有;install_vs2026_dbg 仍不存在;build_grpc.ps1:89 遇到 BoringSSL 缺失会直接报错。
   - 「scene 库 110 个编译错误(装备生成物缺失,PROGRESS:7005)」,改为:装备表产物已由 0d34dcf8d 补上,scene 在 HEAD 能否编过未核实,不再引用 110 个错误。
   - 当前已知的第一个阻塞是 Boost 模块化布局(PROGRESS:7637)。third_party/boost 只有 libs/*/include,没有 boost/ 头目录;core、grpc_client、rpc_controller_test 的 include 路径也只有 third_party/boost/ 根。它同样卡住本项:muduo 的 EventLoop.h:18 引用 boost/any.hpp,etcd_grpc_client.h:5 引用 boost/circular_buffer.hpp,etcd_grpc_client.cpp:8 引用 boost/pool/object_pool.hpp。
   - 「没有 lib/、build/」,改为:两个目录都在,但 lib/ 只有 battle.pdb,build/ 只有 rpc-lambda-tools/protoc-gen-go.exe,没有任何节点 .lib 或 .exe。
   - codex_measurement 甲选项的「gate.exe 还要等 scene 库装备缺口修掉」、乙选项的「绕开 scene 的 110 个错误」,理由均失效。乙仍能绕开 scene / gate 的链接依赖,但绕不开 gRPC 安装和 Boost 这两个阻塞。
9. 【X1 外部常识未核实】confidence 的 medium 第 1 条、claim_evidence「外部常识」段、risks 24,当时的理由是 gRPC 子模块是空目录。现在 completion_queue.h:145-169 与 async_stream.h:43-45、:56-83 有明文约定,X1 可以升级为「API 文档确认、未运行」。I6(起节点时 etcd 不可达就永不注册)因此多了一条依据:fail-fast 下 StartCall 的 ok=false(:145-150),而全仓没有 wait_for_ready。risks 16 部分消解:头文件写明 Finish 不得与其它操作并发(:58),设计里 TryFinish 要求 startPending、readPending、写在途都为假,正好满足;只剩「INIT 失败后直接 Finish」一点,仍靠 T5 验证。
10. 【生成器环境】
    - risks 7 的「本机记录的是 Go 1.26.x,可能要下载工具链」改为:Codex 已有 1.27.2,并在 10-14 用 GOTOOLCHAIN=local 跑通。
    - 「生成器在本机被应用控制策略拦过(PROGRESS:6817)」改为:10-14 Codex 已构建并两次运行生成器二进制(定向模式);全量 proto-gen-run 是否被拦未核实。主工作区仍没有 proto-gen.exe,Codex 构建的那份在哪需回报。
    - regen 要的 protoc 35.1 与 grpc_cpp_plugin 原定取自 install_vs2026_dbg\bin,该目录缺失,所以 regen 同样受阻。
11. 【proto_gen.yaml】:243 的 enable_unity_client 改为 :244;:503-511 的副本说明改为 :504-512;:71-72 不变。
12. 【run_cpp_tests 期望文本】「期望汇总行是『挂:EtcdStreamBreakTest…』」改为:汇总表结果列为「N/M 过,挂:EtcdStreamBreakTest.KeepAliveWriteStateIsNotStuckAfterStreamBreak」(:184),末行英文 `1 test projects failed`(:196)。
13. 【Java】
    - java_parity 的「HEAD d942f74」改为 52cccf2;
    - 「PARITY.md:25(列序)」改为 :27;相邻行 :36 改为 :39。
    - 结论「不适用」不变。
14. 【独立 GTest 的说法】主线程说「独立 GTest 这条路走得通」,对本项不适用。BattleAdmissionGate 能独立编,是因为它刻意不依赖 muduo、protobuf 和引擎(battle_admission_gate_test.cpp:5-7);etcd 流用例必须链真实 gRPC 和生成的 etcd 客户端。另外 grpc 内置的 googletest 子模块也没检出,Codex 用的「现成 GTest」装在哪需回报。
15. 【B1 起依赖】新发现的风险:原步骤没有预建 Kafka 正式 topic。docker-compose.yml:66-81 的注释写明,broker 开着自动建 topic,建出来是 1 个分区,之后契约核对或 kafka-topic-init 会失败。所以 B1 前是否要先预建(start_game.ps1:182-205 的做法),需 Codex 先确认。

### 受阻步骤与先写的测试

1. A1(路线 A,进程内复现 T1)
   - 阻塞原因:
     ① gRPC Debug 安装缺失(third_party/grpc/install_vs2026_dbg 不存在;嵌套子模块 boringssl、cares、re2、zlib、googletest 等未检出,build_grpc.ps1:89 会直接报错)。
     ② Boost 只有模块化布局。core、grpc_client、rpc_controller_test 的 include 路径只有 third_party/boost/ 根,所以 muduo 的 EventLoop.h:18(boost/any.hpp)、etcd_grpc_client.h:5(circular_buffer)、etcd_grpc_client.cpp:8(pool)都会 C1083,PROGRESS:7637 已实测。
     ③ rpc_controller_test 还链 scene.lib、gate.lib 等,scene 在 HEAD 能否编过未核实。
     ④ 五个未初始化子模块的影响未核实。
   - 解除条件:递归初始化子模块(下载)→ 跑 build_grpc.ps1(可能经 winget 装或升级 cmake、ninja)→ 修 Boost 头文件布局(做法由 Codex 先确认)→ 在主工作区串行 /m:1 编译,再跑 run_cpp_tests.ps1 -Filter rpc_controller_test。
   - 由谁解除:负责人拍板授权;Codex 修环境并执行。这属于 §10.2 的下载、安装工具、改构建环境。
2. B0~B7(路线 B,docker 重启实验)
   - 阻塞原因:A1 的全部阻塞,加上没有 gate.exe(bin\ 下没有任何 exe),而 gate 链 scene.lib。
   - 解除条件:A1 解除后编出 gate.exe。或者 M0 发现 2026-10-07 之后构建的节点镜像,且负责人同意由 Codex 先提「容器起 gate」的方案,仓库里没有现成命令。
   - 由谁解除:同 A1,由负责人拍板、Codex 执行。
   - 另外 B4、B6 会重启或停掉共享 etcd,需要负责人同意;B1 前要不要预建 Kafka 正式 topic,需 Codex 先确认。
3. K1(三成员 kind 验收)
   - 阻塞原因:需要 kind、镜像和 Linux 构建,属于下载加构建;按设计也只在修复之后做。
   - 解除条件:修复合入后,负责人单独授权。

**实测前要先写的代码**:

P1 Go 模板红测(供 M2 使用,现在就能跑)
- 文件:新建 tools/proto_generator/protogen/internal/generator/cpp/grpc_stream_lifecycle_test.go,同包 `package cpp`,复用 grpc_process_test.go 里的 makeGrpcInitService、boolPtr、strPtr。
  - 原 fix_design 写的是追加到 grpc_process_test.go。改成新文件,是为了红测时只把一个新文件拷进 2f897bd07 的干净 worktree,不覆盖 HEAD 上的文件。修复阶段两种放法都行;files_to_touch 相应替换一项,总数仍是 26。
- 用例 1:TestGrpcInitTemplateDoesNotDropFailedStreamCompletions。
  - 准备:造一个流式文件(方法描述符的 ClientStreaming / ServerStreaming 置 true,节点类型 EtcdNodeService)和一个纯 unary 文件。照既有用例的做法,先保存 _config.Global 与 internal.NodeEnumValueSet,在 t.Cleanup 里还原。
  - 渲染 grpc_init_total.cpp.tmpl,切出 HandleCompletedQueueMessage 段,做三条断言:
    - `if (!ok)` 块内不得有 `return;`;
    - 流式文件的 `Handle<File>CompletedQueueMessage(` 调用实参里有 `ok`;
    - unary 文件的调用实参里没有 `ok`。
- 用例 2:TestGrpcAsyncClientTemplateStreamLifecycle。
  - 用 GrpcServiceTemplateData(字段见 grpc_process.go:97-104)渲染 grpc_async_client.h.tmpl 与 .cpp.tmpl,方法设为双向流。
  - 要补齐模板用到的配置字段(如 Naming.CompleteQueue),以及 model.go:308-319 访问器需要的流式描述符字段。
  - 断言:出现 `Restart<S><M>Stream`、`Cancel<S><M>Stream`、`<S><M>StreamClosedHandler`;FINISH 分支调用关闭回调;不得出现 `cq.Shutdown()`。
- 红的原因必须是断言:模板解析或渲染失败时,用以 parse / render 开头的独立 t.Fatalf 文案,M2 据此区分「没跑起来」。
- 同步:Go 测试,不涉及 CMakeLists、vcxproj、sln。
- 为什么属于实测而不是修复:它不改任何模板或产物,只把 E4、E7 的模板事实变成可机械复跑的红信号(Q0)。它同时就是 AGENTS.md §11.4 要求的「修复前先红」的回归测试;修复改了模板之后,同一用例转绿。

P2 C++ 进程内红测 T1(供 A1 使用;能写,但编译和运行受阻)
- 文件:
  - 新建 cpp/tests/rpc_controller_test/etcd_stream_break_test.cpp;
  - 改 cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj,在 :169-171 的 ClCompile 列表里加一行 `<ClCompile Include="etcd_stream_break_test.cpp" />`。
  - 该工程没有 .vcxproj.filters、没有 CMakeLists,也不在 game.sln 里(由 run_cpp_tests.ps1:60 的清单配合 -Build 编译),所以别处都不用改。
- 内容要点:
  - 假服务:进程内在本地回环端口起一个 grpc::Server,注册一个实现 etcdserverpb::Lease::Service(etcd.grpc.pb.h:1276)的假服务。LeaseKeepAlive 读到第一条请求就回一个 ACK,之后按测试指令以 UNAVAILABLE 结束流。
  - 客户端走真实路径:`InitGrpcNode(channel, tlsNodeContextManager.GetRegistry(EtcdNodeService), registry.create())`(先例 rpc_client_lifecycle_test.cpp:213-215),然后调 SendLeaseLeaseKeepAlive。
  - 泵消息:在有界截止时间内循环调用 HandleCompletedQueueMessage,不靠「sleep 次数」(§11.3)。注册表是 thread_local,创建和泵必须在同一线程。
  - 断言:收到一次 ACK → 让假服务结束流 → 再发一拍并泵若干轮 → 断言 `registry.get<LeaseKeepAliveRequestWriteInProgress>(e).isInProgress == false`。HEAD 上会红(卡在 true,即 E6),修复后转绿。
  - 收尾:完成队列先 Shutdown 再排空,之后才销毁实体;复位全局处理器,不污染同工程的其它用例。
  - 只用 HEAD 已有的接口,不依赖修复后的新 API。
- 依赖:真实 gRPC 运行库、生成的 etcd 客户端、core、thread_context,所以受 A1 阻塞。现在写只能标「未编译,待 Codex 验证」。
- 为什么属于实测而不是修复:它是 Q1 的进程内运行证据(断流后写标志卡死、流不重建),修复前必须先红,本身不含任何修复逻辑。阶段 B 的 T2~T10 依赖修复后的新接口,属于修复,不在这里。

路线 B、M0、M1、M3、M4 都不需要 Claude 先写代码。

### 待拍板

1. 构建环境修复授权(A1、路线 B 的前提)。内容:递归初始化子模块(联网下载);跑 build_grpc.ps1(可能经 winget 装或升级 cmake、ninja);修 Boost 头文件布局(改构建环境,做法由 Codex 先确认)。
   - 建议:批准,当作与 battle 节点编译共用的同一个阻塞一次解决(PROGRESS:7637)。范围限定在主工作区;修好后先只编 grpc_client、core 和 rpc_controller_test,不必先编全量 game.sln。
2. 「先实测再落码」的门槛,二选一:
   - 甲:严格等 A1 跑出红再动修复。
   - 丙′(建议):以「HEAD 上的静态链 + gRPC 头文件 API 约定(completion_queue.h:145-169)+ M2 Go 模板红」为开工门槛,修复照常测试先行,与环境恢复并行。A1 与路线 B 作为「修复前红 / 修复后绿」的运行验收,环境恢复后补跑;交付说明如实写运行证据缺口。
3. 是否现在就让 Claude 写实测代码。建议是:
   - P1(Go 红测)写在新文件 grpc_stream_lifecycle_test.go,不追加到 grpc_process_test.go,方便只拷一个文件进干净 worktree 跑红;
   - P2(C++ T1)同时写好,标「未编译」,等 A1 解除。
4. 实测在哪棵树跑。建议:
   - M2、M3 在新建的 worktree 里跑(`git worktree add --detach $env:TEMP\p0d-etcd 2f897bd07`),只拷入测试文件,跑完删除;
   - C++ 编译和路线 B 只能在主工作区跑(新 worktree 没有子模块,初始化要下载),需负责人确认主工作区可以用来产出构建物;
   - 两棵 Codex 树一律不动。
5. 回归用例放哪个工程,二选一:
   - 放现有的 rpc_controller_test(建议):不新建工程,但它链 scene / gate;
   - 新建只链 grpc_client、proto、core、thread_context 的小工程:属范围扩大,文件数逼近 30 个的门槛,而且仍绕不开 gRPC 安装和 Boost 两个阻塞。
   - 建议先用 rpc_controller_test;环境修好后若 scene / gate 因与本项无关的原因编不过,再申请小工程。
6. 破坏性步骤要不要做:路线 B 的 B4(重启共享 etcd)、B6(停 etcd 约 20s),以及可选的 M4。
   - 建议:路线 B 等 gate.exe 就绪后整体做;M4 可选,只在本机没有其它依赖 etcd 的服务在跑时做。
7. 节点镜像替代路线。若 M0 发现 2026-10-07 之后构建的节点镜像,是否让 Codex 提「容器起 gate」的方案。
   - 建议:只有第 1 条的环境授权被否时才启用,不临时拼命令。
8. 三成员 kind 验收(下载加构建)。建议延后到修复合入之后,单独授权。
9. 沿用昨天的待决项:
   - 修复范围:建议「流重建 + 重建后按前缀对账删除(含 Range 串行化)」一批做完。
   - 「不设重试次数上限」是对 §11.3 的有意偏离:建议认可,并写进设计文档。
   - 运维口径:修复验证通过之前不滚动 etcd(钉镜像版本、改 etcd.yaml 都会触发滚动);现在就按 M1 排查存量受害节点。
   - 验收口径:node-id-overhaul-plan:191 / :276 的「无任何节点 fence」应加上「且零重注册」。

### 原报告:证据链

结论:交接的主张在 HEAD 0aa1027bc 上静态成立。两份复核都同意,我逐行重读后维持。仓库内因果链完整;三处外部语义(gRPC 的 ok 标志、channel 重连、etcd 租约时序)只凭常识,运行证据为零。
提交差异:`git diff --stat 73b852f32 HEAD -- cpp/generated/grpc_client cpp/libs/engine/core/node tools/proto_generator/protogen/internal/template` 为空;同区间只有 deploy/docker-compose.yml 的 redis 段不同(12 行)。

【代码事实(逐行读过)】
E1 两条都是双向流,不是每拍 unary。proto/etcd/etcd.proto:42 `rpc Watch (stream WatchRequest) returns (stream WatchResponse);`,:56 `rpc LeaseKeepAlive (stream LeaseKeepAliveRequest) returns (stream LeaseKeepAliveResponse);`。生成客户端成员是 `grpc::ClientAsyncReaderWriter`(cpp/generated/grpc_client/etcd/etcd_grpc_client.h:161、:242)。除 proto 自带的 etcd.grpc.pb.* 外,全仓只有这两处用它。
E2 流在进程内只建一次。etcd_service.cpp:31 `InitGrpcNode(channel, ...GetRegistry(EtcdNodeService), ...GetOrCreateGlobalEntity(EtcdNodeService));` → grpc_init_client.cpp:438 `registry.emplace<grpc::CompletionQueue>(nodeEntity);`、:454-455 → etcd_grpc_client.cpp:988-992 `->AsyncWatch(&client.context, &registry.get<grpc::CompletionQueue>(nodeEntity), (void*)(got_tag));`,:1003-1007 AsyncLeaseKeepAlive 同形。组件全用 registry.emplace(:982-986、:997-1001),InitEtcdGrpcNode 不可重入。
E3 完成队列只有一个轮询点。etcd_service.cpp:33-37 `grpcHandlerTimer.RunEvery(0.005, [] { for (auto& registry : ...GetAllRegistries()) { HandleCompletedQueueMessage(registry); } });`。全仓只有 :35 调它。node.h:251 另有同名定时器成员,只在 node.cpp:1170 被取消,从未启动。
E4 ok=false 分支。grpc_init_client.cpp:365-369:
  `while (completeQueueComp.AsyncNext(&got_tag, &ok, tm) == grpc::CompletionQueue::GOT_EVENT) {`
  `    if (!ok) { LOG_ERROR << "RPC failed"; return; }`
  事件被整条丢弃:不分发、不释放 GrpcTag,return 还把同一注册表本轮剩余事件推迟 5ms,且不区分失败原因。模板原文 tools/proto_generator/protogen/internal/template/grpc_init_total.cpp.tmpl:84-88。
E5 读只在成功分支续发。etcd_grpc_client.cpp:415-423(Watch)、:641-649(KeepAlive)`client.stream->Read(&response, (void*)got_tag);`。READ 以 ok=false 回来即被 E4 丢掉,此后不再有 Read。
E6 写状态机会卡死。etcd_grpc_client.cpp:608-617 `if (writeInProgress.isInProgress || pendingWritesBuffer.empty()) { return; }` … `writeInProgress.isInProgress = true;` … `client.stream->Write(request, (void*)(got_tag));`。复位只在 WRITE 完成分支(:624-631)。WRITE 以 ok=false 回来被 E4 丢掉,isInProgress 永久为 true,之后每拍续租只进 200 容量的环形缓冲(etcd_grpc_client.h:246)。Watch 同形(:378-392、:398-406)。
E7 没有重建路径。
  - cpp 非第三方代码里 `WritesDone|TryCancel|wait_for_ready` 零调用,只有一处注释命中(cpp/libs/engine/core/network/node_utils.cpp:50)。
  - switch 里 WRITES_DONE / FINISH 分支无人投递;FINISH 分支是 `cq.Shutdown();`(etcd_grpc_client.cpp:412-414、:638-640)。
  - 唯一的"重连"是 etcd_service.cpp:361-365 `if (response.canceled()) { ... ScheduleWatchReconnect(); return; }`:要先收到应答,而且只是往同一条流上再写 create(:393-399 → etcd_helper.cpp:45-63),流死后无效。
  - EtcdHelper::StopAllWatching 是空 TODO(etcd_helper.cpp:65-68)。
E8 续租。etcd_manager.cpp:193-198 每 keep_alive_interval 调一次 `SendLeaseLeaseKeepAlive(...)`;ACK 入口 etcd_service.cpp:611 `lastKeepAliveAckTime_ = std::chrono::steady_clock::now();`。流死后不再有 ACK。
E9 「每 180s 判失租并重注册」的代码。
  - 判定:etcd_service.cpp:622-624 `auto elapsedSeconds = ...duration_cast<std::chrono::seconds>(elapsed).count(); return elapsedSeconds > leaseTtlSeconds_;`(整秒截断,实际 ≥181s)。
  - 触发:node.cpp:1537-1546 `LOG_ERROR << "Lease deadline exceeded: no keepalive ACK from etcd within TTL. "` … `RequestReRegistration("lease deadline exceeded locally");`。
  - 取值:bin/etc/base_deploy_config.yaml:5 `KeepaliveInterval: 1`、:6 `NodeTTLSeconds: 180`、:28 `HealthCheckInterval: 1`。K8s 从同一文件取(`git show HEAD:tools/scripts/k8s_deploy.ps1` 的 :1694-1695、:1778-1779;工作区该文件另有他人未提交的 431 行,同一处在 :2035-2036、:2119-2120)。
  - 重注册链:etcd_service.cpp:467-480 → :402-413(LeaseGrant,unary)→ :494-503 `lastKeepAliveAckTime_ = ...now(); ... StartLeaseKeepAlive(); ... NodeAllocator::ReRegisterExistingNode();` → node_allocator.cpp:98-108 → etcd_manager.cpp:152-179(端口键无条件 Put)→ :104-112(分配键「不存在或已是我的」CAS)→ :130-137(服务键无条件 Put)。node_id 不变。
  - 为什么循环:StartLeaseKeepAlive 只是重装定时器(etcd_manager.cpp:191-199),写仍卡在 E6,新租约同样没人续。健康监控的复位条件是「本地快照里还有自己」(node.cpp:1555-1561);watch 已死收不到自己的 DELETE,2s 的 Range 还会把自己加回来,条件恒成立。
E10 watch 失效只被部分掩盖。service_discovery_manager.cpp:16-18 `serviceNodeSyncTimer_.RunEvery(2.0, [this] { FetchServiceNodes(); });`,:29-32 对每个前缀 RangeQuery(本机 10 个前缀,base_deploy_config.yaml:40-49)。应答处理 etcd_service.cpp:58-59 `for (const auto& kv : reply.kvs()) { HandlePutEvent(kv.key(), kv.value());`,只做新增和更新,没有删除对账。删除只有 watch DELETE 一条路(etcd_service.cpp:386-389 → node.cpp:1317)。
E11 失租不 fence。node.cpp:1534-1536 注释「失租**不再**直接走冲突关停:拿新 lease 重注册,分配键的 CAS 才是身份的裁判」。SnowFlake 守卫已删(etcd_service.cpp:642-645;永久 guid 走号段)。走到冲突关停的路有两条:
  - 重注册 CAS 失败:etcd_service.cpp:242-251。
  - 劫持检测:收到自己服务键的值是别的 uuid(:313-337 `remoteInfo.node_uuid() != myInfo.node_uuid()` → `OnNodeIdConflictShutdown`)。这条路由 2s 的 Range 持续喂数据(:58-59),watch 死后仍然有效。
E12 顶层分发还有第二个使用方。node_connector.cpp:74 `InitGrpcNode(grpcChannel, targetRegistry, createdId);` 给每个 gRPC 对端建完成队列,和 etcd 共用 HandleCompletedQueueMessage。改顶层模板会影响它们;今天它们只有 unary 方法。
E13 gate 每 10s 无条件写一次服务键。cpp/nodes/gate/main.cpp:408-411 `playerCountReportTimer.RunEvery(10.0, ...) n.GetEtcdManager().UpdateNodeInfo();` → etcd_manager.cpp:146-150,带当前租约号;租约号在拿到新租约时就更新(etcd_service.cpp:486 `leaseId = reply.id();`),早于分配键 CAS 裁定。这个定时器只在依赖门就绪后才起(main.cpp:405)。
E14 节点被摘在 gate 上的消费方。
  - node.cpp:1415-1416 `// --- Immediate removal path (production: grace_seconds == 0) ---` `ExecuteNodeRemoval(stoppedNode);`。NodeRemovalGraceSeconds 在 base_deploy_config.yaml:12 是注释掉的,K8s 模板(HEAD :1775-1810)也没有这个键。
  - client_message_processor.cpp:1109-1111 `if (session.second.GetEntityId(pb.node_type()) != pb.entity()) continue; session.second.SetEntityId(pb.node_type(), SessionInfo::kInvalidEntityId);`。
  - 之后发往 scene 的消息::775-779 `LOG_ERROR << "Node not found for session id: "` … `SendTipToClient(conn, kServiceUnavailable);`。
  - :53-81 PickRandomNode 只按 zone 过滤后均匀随机,不看通道状态;只有 SceneManager 有分级挑选(node_utils.cpp:239-280)。
E15 节点号分配是从 1 起线性探测。node_allocator.cpp:76-84 取本地快照加已输号集合之外的最小号;etcd_service.cpp:259-266 CAS 输了 `RecordLostId(lostId); ... acquireNodeTimer.RunAfter(1, ...)`。本地快照不含分配键(service_discovery_manager.cpp:158-162),zone 内类型也不含外区节点(:75-83)。

【找过但不存在的保护逻辑】
没有第二个完成队列轮询点;没有任何地方重调 InitEtcdGrpcNode、重建通道或存根;失租不退出进程(node.cpp:1534-1536),没有「重启顺带恢复」;gate / scene 没有会杀容器的探针(k8s_deploy.ps1 HEAD :1959、:1971、:1993),battle 有 300s 的 startupProbe(HEAD :2017)。

【外部常识,未核实(两棵工作树的 third_party/grpc 都是空目录,D:/luyuan 下找不到 grpcpp 头文件)】
X1 gRPC 异步接口:流断开后 Read / Write 的完成事件 ok=false,流不会自行恢复;unary Finish 的 ok 恒为 true;流起始失败时 INIT 标签 ok=false。
X2 channel 会自动重连,所以 unary(Range / LeaseGrant / Txn)能恢复。
X3 etcd 在租约到期后约 0.5s 内撤销并删除其上的键;单成员带持久化重启后租约仍在,TTL 被刷新。
X4 etcd 服务端在续租转发失败(例如换主)时会主动结束 LeaseKeepAlive 流。成立的话,只有续租流死、watch 还活着,连在其它成员上的节点也会中招。

【推断(X1~X3 成立时;均无运行证据)】
I1 首轮在最后一次 ACK 后约 181s 重注册,此后每约 181s 一轮,直到进程重启。
I2 从第 2 轮起,etcd 侧的 180s 到期早于本地的 181s 判定,本节点三把键先被删 1~2s 再写回。
I3 I2 的对端后果:
  - watch 正常的 gate 立刻摘掉这个 scene,并把指向它的全部会话的 scene 绑定置无效(E14);scene 重新出现后是新实体,旧绑定不会自己恢复。scene 侧是否重推绑定未核实。
  - scene_manager leader 按节点离开处理:`removeNodeFromRedis` 并 `RebalanceWorldChannelsForZone`(go/scene_manager/internal/logic/load_reporter.go:957-994)。
  - 窗口内别的进程探测到这个号就会抢走(E15),本节点重注册 CAS 失败,走冲突关停。受害节点多时这不是小概率。
I4 watch 死掉的节点永远收不到 DELETE,已下线的对端永久留在本地快照和节点实体里。gate 随机挑实例不看通道状态(E14),每滚动一次 Go 服务就多一批幽灵实例。Go 实例重启后 uuid 是否变化未核实。
I5 每轮只有前 TTL/2 身份探针为真(etcd_service.cpp:637-639 `return elapsedMs * 2 <= leaseTtlSeconds_ * 1000;`),scene 的断线释放标记 A1′ 另一半时间被 fail-closed 挡住(cpp/nodes/scene/main.cpp:98-99;player_lifecycle.cpp:855-862)。
I6 启动期流就死(起节点时 etcd 不可达)时,节点永远不注册:申请租约只由第一条 watch 应答触发(etcd_service.cpp:355-359 `if (!hasSentWatch) { RequestNodeLease(); hasSentWatch = true; }`)。
I7 触发面不限于 etcd 成员重启。E4 不区分原因,任何一次到 etcd 的连接断开都算:网络抖动、连接跟踪表被清、HTTP/2 保活超时(grpc_channel_cache.h:47、:56 的 30000 / 10000 ms)、开发机休眠恢复。node.cpp:1531-1532 的注释 `this catches network partitions where the watch stream is dead` 说明作者预期过分区,但只兜了租约。

### 原报告:对交接的更正

1. 行号基本没漂,但证据不全。grpc_init_client.cpp:358-368 仍是 HandleCompletedQueueMessage 开头到 `return;`(!ok 分支在 :366-369)。etcd_grpc_client.cpp:397-405 只是 Watch 的 WRITE 分支,应补:
   - Watch 状态机 :378-435,KeepAlive 状态机 :604-661;
   - 流的唯一创建点 InitEtcdGrpcNode :974-1010;
   - 模板 grpc_init_total.cpp.tmpl:84-88、grpc_async_client.cpp.tmpl:43-100、:250-281。
2. 「每 180s」实际是 ≥181s(etcd_service.cpp:622-624 整秒截断后用 `>` 比较,健康检查 1s 一拍)。180 来自配置 NodeTTLSeconds,不是常数。
3. 触发条件写窄了。不只是 etcd 成员重启:顶层分发不区分 ok=false 的原因(grpc_init_client.cpp:366-369),任何一次连接断开都会让两条流永久失效。所以「不滚动 etcd」挡不住,现有环境里可能已经有节点处在 181s 循环里。
4. 后果写轻了(以下都是推断,待实测):
   - 从第 2 轮起键会先被 etcd 删 1~2s 再写回。watch 正常的 gate 立刻摘节点(生产宽限期为 0,node.cpp:1415-1416),并把指向它的全部会话的 scene 绑定置无效(client_message_processor.cpp:1104-1113),这些玩家之后发往 scene 的消息回「服务不可用」(:775-779)。
   - scene_manager leader 按节点离开处理(load_reporter.go:957-994)。
   - 缺席窗口里可能被线性探测的新节点抢走 node_id(node_allocator.cpp:76-84),被抢的节点走冲突关停。
   - 每轮后半段 IsIdentityConfirmedFresh 为假,scene 的 A1′ 释放标记被挡。
   - 启动时 etcd 不可达会让节点永远不注册;gate / scene 在 K8s 上没有会杀容器的探针。
5. 「watch 失效」要分开说:每 2s 的全前缀 Range 掩盖了新增和更新(2s 内补上),**没有掩盖删除**(etcd_service.cpp:58-59 只调 HandlePutEvent)。已下线的对端永久留在本地;gate 的 PickRandomNode 不看通道状态(client_message_processor.cpp:53-81),每滚动一次 Go 服务就多一批幽灵实例。
6. 「在非生成的 EtcdService 里重建流」单独做不到,必须动生成器模板,EtcdService 只负责策略:
   - ok 在生成的顶层分发里就被吞了;
   - InitEtcdGrpcNode 用 registry.emplace,不可重入;
   - 手写代码看不到流上还有没有在途操作,无法安全销毁 ClientContext。
7. 这不是新发现。[历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md):17 和 :261(§10 第 1 条,2026-09-28)已写明「流的 !ok 分支:泄漏 tag、提前 return 跳过本轮其余事件、流不重建(KeepAlive 死后约每个 TTL 重注册一轮)。另立项」;docs/handoff/handoff-backlog-2026-09-05.md:636-654(P1-13)也点过 `!ok` return 和 tag 泄漏。落码时回写这两处。
8. 「连在它上面的 C++ 节点」:K8s 上 C++ 连的是 ClusterIP Service `etcd.${InfraNamespace}:2379`(k8s_deploy.ps1 HEAD :1777,工作区 :2118;deploy/k8s/manifests/infra/etcd.yaml:18-31),只取 Hosts 第一项(etcd_service.cpp:17),不是成员列表。连到哪个成员由 kube-proxy 在建连时决定,节点日志里看不出来。
9. 漏了两处连带缺陷:
   - 生成物里 WRITES_DONE / FINISH 是死分支,FINISH 分支写的是 `cq.Shutdown()`(etcd_grpc_client.cpp:412-414、:638-640),真走到会把整条 etcd 完成队列关掉。
   - ScheduleWatchReconnect 的日志写「after stream break」(etcd_service.cpp:398),实际只处理服务端 canceled;重建用的 revision 每 2s 被 Range 改写成当前值(:67-69),中间漏掉的事件不会回放。仓库自带的 proto 注释(proto/etcd/etcd.proto:444-452)要求压缩取消后不得再用同一 start_revision 建 watch,今天的代码没有处理 compact_revision。
10. docs/design/node-id-overhaul-plan-20260908.md:191、:276 与 etcd.yaml:10 的验收口径是「删一个 etcd Pod 后无任何节点 fence」。按本项分析确实不 fence,但会进入永久重注册循环,这条口径不够用,应加「且零重注册」。
11. 交接 §9 的「他人未提交改动」已经变了:deploy/docker-compose.yml(noeviction)已随 HEAD 入库;tools/scripts/k8s_deploy.ps1 仍未提交(比 HEAD 多 431 行),docs/PROGRESS.md、docs/README.md 也有他人未提交的修改。
12. 旁支:bin/etc/base_deploy_config.yaml:25 注释写「2=WARN」,grpc-client-deadline 文档 :254 也这么说;muduo 的枚举是 TRACE=0 / DEBUG=1 / INFO=2(cpp/libs/engine/muduo_windows/src/muduo/base/Logging.h:20-29),node.cpp:1232-1234 直接强转,所以本机默认是 INFO。

### 原报告:同类入口盘点

【代码:创建】
- 流:etcd_grpc_client.cpp:974-1010(InitEtcdGrpcNode),调用方 grpc_init_client.cpp:454-455。
- InitGrpcNode 有两个调用方:etcd_service.cpp:31(etcd);node_connector.cpp:74(每个 gRPC 对端一条完成队列)。
- channel:core/node/system/grpc_channel_cache.h:110-139。缓存里存的是 weak_ptr(:142),通道由实体上的三个 stub 持有。HTTP/2 保活默认 30s / 10s,`GRPC_KEEPALIVE_TIME_MS=0` 可关掉(:42-51)。
- 完成队列:grpc_init_client.cpp:438。

【代码:读取】
- 轮询:etcd_service.cpp:33-37 → grpc_init_client.cpp:358-434 → etcd_grpc_client.cpp:825-874。
- Watch 应答:etcd_service.cpp:91-93 → OnWatchResponse :349-391。
- KeepAlive 应答::101-103 → OnKeepAliveResponse :588-613。
- Range 应答::50-75(成功处理器拿不到请求)。Txn 应答::107-128。
- Range 失败:生成的 `AsyncKVRangeFailedHandler(const GrpcCallFailure&, const RangeRequest&)`(etcd_grpc_client.h 的 KVRange 区;清空入口 etcd_grpc_client.cpp:928-938)。今天 EtcdService 没装,gate 经 SetIfEmptyFailedHandler 装了全局兜底(cpp/nodes/gate/main.cpp:325-331)。

【代码:更新 / 写】
- watch create:etcd_service.cpp:278-285 → etcd_helper.cpp:45-63(带 prev_kv,:50)→ SendWatchWatch(etcd_grpc_client.cpp:437-444)。
- 续租:etcd_manager.cpp:191-199 → SendLeaseLeaseKeepAlive(:663-670)。
- revision 记录:etcd_service.cpp:51、:67-69(每次 Range 应答把全部前缀设为 header.revision+1)和 :369-378(watch 应答)。
- 周期 Range:service_discovery_manager.cpp:12-33。
- gate 每 10s 无条件 Put 服务键:cpp/nodes/gate/main.cpp:408-411 → etcd_manager.cpp:146-150 → etcd_helper.cpp:8-31。

【代码:恢复】
- 只有 canceled 一条:etcd_service.cpp:361-365、:393-400。
- 失租重注册:node.cpp:1521-1574;etcd_service.cpp:467-480、:482-508、:598-608;node_allocator.cpp:98-108;etcd_manager.cpp:87-179。
- unary 丢应答兜底:ArmTxnTimeout / OnTxnTimeout(etcd_service.cpp:519-565)、OnLeaseGrantTimeout(:420-430)。
- 劫持检测(第二条冲突关停路径,由 2s Range 喂数据):etcd_service.cpp:313-337。
- 流断开后的恢复:没有。

【代码:销毁】
- EtcdService::Shutdown(etcd_service.cpp:442-465):先停轮询定时器,再用 SetEtcdHandler 清 11 个应答处理器;不清失败处理器(今天没有捕获 this 的)。
- StopAllWatching 是空 TODO(etcd_helper.cpp:65-68);Node::StopWatchingServiceNodes(node.cpp:1276-1279)调的也是它。
- Node::FinalizeShutdownInLoop(node.cpp:1166-1183):ReleaseNodeId 发 LeaseRevoke(:1281-1284),随后 serviceDiscoveryManager.Shutdown。
- 流和完成队列没有排空;这是 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md:499 登记的独立事项。

【代码:迁移】无。纯客户端行为,不涉及协议和存储。

【代码:删除事件的消费】
- node.cpp:1317-1417(HandleServiceNodeStop,含宽限期)、:1432-1498(ExecuteNodeRemoval)。
- gate:client_message_processor.cpp:1104-1113(OnNodeRemoveEventHandler)、:187-214(ResolveSessionTargetNode)、:53-81(PickRandomNode)。
- 只有 SceneManager 的挑选按通道状态分级:core/network/node_utils.cpp:239-280。
- 节点号线性探测:node_allocator.cpp:33-96。

【代码:依赖「续租新鲜」的调用方】cpp/nodes/scene/main.cpp:98-99 → services/scene/player/system/player_lifecycle.cpp:814-862。

【生成器】
- 模板:tools/proto_generator/protogen/internal/template/grpc_async_client.{h,cpp}.tmpl、grpc_init_total.{h,cpp}.tmpl。
- 入口:internal/generator/cpp/grpc_process.go:119-220(模板在 :159、:168、:205、:212 引用,运行时按相对路径从磁盘读)。grpc_async.go 只剩指向模板的注释。
- 流式判定:internal/model.go:308-313。
- 操作枚举源头:proto/common/constants/etcd_grpc.proto:5-11(READ / INIT / WRITE / WRITES_DONE / FINISH);修复用到的 FINISH 已存在,不用改 proto。
- 手工维护的配套头:cpp/generated/grpc_client/grpc_call_tag.h(:11-12 写明不是生成物)。
- 运行入口:tools/scripts/dev_tools.ps1:373-394(proto-gen-build,`go build`)、:396-465(proto-gen-run,-ConfigPath / -UseBinary)。
- 外部工具按 PATH 找:protoc(internal/prototools/descriptor.go:155)、grpc_cpp_plugin(internal/generator/cpp/gen.go:204-209)。
- 配置:protogen/etc/proto_gen.yaml:71-72;:243 `enable_unity_client: true`,:503-511 要求用关掉该项的副本跑。
- 生成器模块要求 `go 1.27.2`(tools/proto_generator/protogen/go.mod:3)。

【同机制的其它使用者】
- 生成的客户端共 16 个文件对加 grpc_init_client.{h,cpp},合计 34 个生成文件。顶层分发覆盖其中 15 个(db 被 grpc_process.go:52 跳过),只有 etcd 有流式方法。
- gate 在 cpp/nodes/gate/main.cpp:290、:325 装了全局的 IfEmpty 应答 / 失败处理器,gate 自己的 etcd unary 失败也走它(:323 注释)。

【Go / 本仓库 Java 网关(不是同一缺陷)】
- Go 各服务用 clientv3 的 KeepAlive / Watch,库内部管流重建(外部常识,未逐个核实)。例:go/match/internal/noderegistry/registry.go:214-218、:318。
- go/match/internal/discovery/node_watcher.go:207-208 跳过 `/allocated/` 键。
- java/gateway_node 的 GateWatcher 是 KV Get,不持流(未重读,沿用原报告)。

【配置】
- bin/etc/base_deploy_config.yaml:2-6(Etcd.Hosts / KeepaliveInterval / NodeTTLSeconds)、:12(NodeRemovalGraceSeconds,注释掉)、:25(LogLevel)、:28、:39-49(10 个前缀)、:234(`EtcdNodeService: 5000` unary deadline)。
- 读取:cpp/libs/engine/config/config.cpp:27-54。

【K8s 模板 / 部署脚本】(行号取自 `git show HEAD:`;工作区该文件有他人未提交的 431 行,同一内容下移约 340~420 行)
- tools/scripts/k8s_deploy.ps1 HEAD :1694-1695、:1775-1779(Hosts 只有一项)、:1789-1810(LogLevel 1、前缀列表)、:1959 / :1971 / :1993(gate、scene 无 startup 和 liveness)、:2017(battle startupProbe)。
- deploy/k8s/manifests/infra/etcd.yaml:10(验收口径)、:18-31(ClusterIP)、:37-54(headless)、:57-65(PDB minAvailable 2)、:68-78(3 副本,Parallel)、:114(镜像 latest)、:158-179(探针)、:193-200(PVC)。

【docker-compose】
- deploy/docker-compose.yml(HEAD):447-476:容器名 etcd,镜像 bitnamilegacy/etcd:latest,卷 `etcd-data:/bitnami/etcd/data`(:474-475、:491),`restart: unless-stopped`。Codex 的 73b852f32 工作树里这段内容相同,行号减 6;redis 段的淘汰策略不同。
- 本机启动脚本对 etcd 的用法:tools/scripts/start_game.ps1:110-113(固定 `--context desktop-linux`)、:270(`etcdctl get … --prefix --write-out=json`)、:445-450。

【本机起节点的脚本】
- tools/scripts/cpp_nodes.ps1:-Command / -Nodes / -NodeIp(:58-104);exe 缺失只打 Warning 跳过(:325-328);不设置 PATH;30s 没看到 banner 只打 `[timeout]`(:277),不查进程;stdout / stderr 落 run\logs\cpp_nodes\<key>.{stdout,stderr}.log(:351-352)。
- Debug 运行库目录由 start_game.ps1:551 加进 PATH(`third_party/grpc/install_vs2026_dbg/bin`)。
- 节点文件日志:基名 `logs/cpp_nodes/<短名>`(node.cpp:405),1 秒一刷(node.cpp:374;muduo AsyncLogging.cc:79、:132),文件名是「基名.时间.主机名.进程号.log」(LogFile.cc:110-129)。stdout 那份没有任何刷新(core/log/system/console_log.cpp:49-53)。

【测试】
- 流的生命周期没有任何测试。
- 生成器测试只有 internal/generator/cpp/grpc_process_test.go:40(只断言 init 模板的分支结构)。
- 脚本契约测试:tools/scripts/tests/k8s_deploy_contract.tests.ps1(`pwsh -File` 加 -DryRun,不起 kubectl;HEAD :32-45)。
- 可承载新 C++ 用例的工程:cpp/tests/rpc_controller_test(已在 tools/scripts/run_cpp_tests.ps1:60 的清单里;源文件清单在 vcxproj:169-171;不在 game.sln 里;没有 CMakeLists)。它的依赖行(vcxproj:126)除 grpc++ / grpc_client / proto / core / thread_context 外还链 gate.lib、scene.lib、session.lib、table.lib,所以受同一个编译阻塞。cpp/tests 下链 grpc_client.lib 的 25 个工程里只有 turn_battle_engine_test 不链 scene.lib。
- 生成的假服务基类可用:cpp/generated/proto/etcd/etcd.grpc.pb.h:982、:991(Watch::Service)、:1276、:1288(Lease::Service)。

【文档】
- [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md):17、:29、:233-238、:242、:261。
- docs/handoff/handoff-backlog-2026-09-05.md:636-654。
- docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md:499。
- docs/design/node-id-overhaul-plan-20260908.md:191、:276。
- docs/handoff/handoff-global-singletons-10m-ccu-20261013.md:70、:136-138、:221、:243-250。
- docs/handoff/handoff-launch-readiness-and-d1-20261013.md:122-126、:171。
- docs/PROGRESS.md(HEAD):7342、:6081。

【Java 版(D:/luyuan/xuanming-server-mmo,HEAD d942f74)】
- 不用 etcd:deploy/compose/infra.yaml:11。
- 节点号租约:game-discovery/.../NodeIdLease.java:41-42、:72-73、:93、:142-168。
- 节点目录:game-discovery/.../NodeDirectory.java:22-27、:48-49。
- 常驻 Redis 订阅(同类风险点):game-gate/.../presence/GatePushSubscriber.java:53;game-scene/.../ownership/OwnerTakeoverSubscriber.java:35;game-scene/.../SceneNode.java:850-851。

### 原报告:修复设计

> **2026-10-15 去向(以此为准)**:本项不在旧的完成队列(CQ)模板上修。负责人 2026-10-14 拍板的 gRPC→muduo 迁移([grpc-muduo-transport.md](../design/grpc-muduo-transport.md))第 2 批就是「etcd 迁到新传输:流断重建、按 revision 续 watch、compact 处理」,第 3 批重写客户端生成模板;在旧模板上修会与迁移会话撞车,修好也会被替换(同 entt 审计「旧模板不在那边修」的先例)。迁移会话已把下面的结论与验收口径并入该文档 §10.2,作为第 2 批的验收条件,并同意「任何 etcd 滚动(含 P0-C 钉镜像、加压缩 env)排在这项验证通过之后」。第 2 批保留开关回退到旧 CQ 版,回退时这些缺陷仍在。下面的设计原文保留作推导与测试设计参考。
> **2026-10-16 补**:上一句已过时 —— f0dadf5b0 删除了旧 CQ 实现,`NODE_ETCD_TRANSPORT=grpc` 只打 WARN、仍走 muduo_h2,已无回退。新传输上 §10.2 第 1~5 条代码已落、第 6 条没有运行证据,见 [交接 §6.1](handoff-global-singletons-p0-20261015.md)。

前提:实测成立(或负责人按上面的「丙」破例)后再落码。以下全部未编译、未测试(AGENTS.md §10.1)。

【方案对比与推荐】
- 方案 A(推荐):生成器模板提供机制,EtcdService 负责策略。根因在生成的状态机丢了 ok,只能在那里修;手写代码只用生成的公开函数。
- 方案 B:只改 EtcdService。做不到正确:ok 在 grpc_init_client.cpp:366-369 已被吞;InitEtcdGrpcNode 不可重入;手写代码不知道流上还有没有在途操作,而 Watch 连心跳都没有。
- 方案 C:EtcdService 自己手写两条流。模板不改的话,生成的 InitEtcdGrpcNode 仍会建两条空流、仍打 `RPC failed`。不取。

【一、模板与配套头(不手改生成物)】
1. grpc_init_total.cpp.tmpl 的完成循环(:84-104)。
   - 去掉 `return`;先取 tag 再看 ok。
   - 含流式方法的文件:模板里用变量遍历 `$file.Methods` 判 `.ClientStreaming`(`{{ $hasStream = true }}`,不改 Go 源码),调用 `Handle<File>CompletedQueueMessage(registry, e, cq, grpcTag, ok)`;文件前部的前置声明(:39)同步加 `bool ok`。
   - 只有 unary 的文件:签名不变。分支里写 `if (!ok) { LOG_ERROR << "gRPC unary completion ok=false, messageId=" << messageId; } else { Handle…(…); }`。不分发的原因:此时 status 没被填写,默认是 OK,分发会被当成成功。按 gRPC 约定这条路不可达(外部常识 X1),真走到会漏一个 tag 和一个调用对象,换来不误判。
   - 这样 regen 只动 3 个生成文件:grpc_init_client.cpp、etcd/etcd_grpc_client.{h,cpp}。
2. cpp/generated/grpc_client/grpc_call_tag.h(手工维护,:11-12)新增普通枚举和名字表:
   ```cpp
   enum class GrpcStreamPhase : uint8_t { kStarting, kOpen, kDraining, kFinishing, kClosed, kCount };
   inline constexpr std::array<const char*, static_cast<size_t>(GrpcStreamPhase::kCount)> kGrpcStreamPhaseNames{
       "Starting", "Open", "Draining", "Finishing", "Closed"};   // 长度与 kCount 由数组类型锁死
   ```
3. grpc_async_client.h.tmpl 流式分支。
   ```cpp
   struct Async<S><M>GrpcClient {
       uint32_t messageId{ <S><M>MessageId };
       struct Call { ClientContext context; Status status;
                     std::unique_ptr<grpc::ClientAsyncReaderWriter<Req, Resp>> stream; };
       std::unique_ptr<Call> call;   // 每次(重)建流换一个新的;旧的只在收到 FINISH 之后销毁
   };
   struct <Req>StreamState { GrpcStreamPhase phase{GrpcStreamPhase::kStarting}; bool startPending{true}; bool readPending{false}; };
   using Async<S><M>StreamClosedHandlerFunctionType = std::function<void(const grpc::Status&)>;
   extern Async<S><M>StreamClosedHandlerFunctionType Async<S><M>StreamClosedHandler;
   void Cancel<S><M>Stream(entt::registry&, entt::entity);
   bool Restart<S><M>Stream(entt::registry&, entt::entity);
   GrpcStreamPhase Get<S><M>StreamPhase(entt::registry&, entt::entity);
   ```
   - 现有的 `<Req>Buffer`、`<Req>WriteInProgress` 两个组件原样保留(阶段 A 的用例依赖后者)。
   - ClientContext 不可复用、不可移动。每次流的状态收进堆上的 Call,重建就是换一个新对象,不依赖 EnTT 对不可移动组件做 erase 再 emplace 的行为(entt 子模块未检出,无法核实)。
   - 这两个结构只被生成物引用(全仓 grep 只命中 etcd_grpc_client.{h,cpp}),改形状不影响手写代码。
4. grpc_async_client.cpp.tmpl 流式分支改成显式状态机。
   ```cpp
   TryFinish:  if (phase != kDraining || startPending || readPending || write.isInProgress) return;
               phase = kFinishing; call->stream->Finish(&call->status, tag(FINISH));
   BeginDrain: if (phase == kStarting || phase == kOpen) { phase = kDraining; call->context.TryCancel(); }
               TryFinish();
   case INIT:  startPending = false;
               if (ok && phase == kStarting) { phase = kOpen; StartRead(); TryWriteNext(); } else BeginDrain();
   case READ:  readPending = false;
               if (ok && phase == kOpen) {
                   if (Handler) Handler(call->context, response);
                   // 处理器里可能调了 Cancel…Stream:回来必须重新判阶段,Finish 之后不许再发起读写
                   if (phase == kOpen) { StartRead(); TryWriteNext(); }
               } else BeginDrain();
   case WRITE: write.isInProgress = false;
               if (ok && phase == kOpen) { pending.pop_front(); TryWriteNext(); } else BeginDrain();
   case FINISH: phase = kClosed;
               const grpc::Status closedStatus = call->status;        // 按值拷出
               if (ClosedHandler) ClosedHandler(closedStatus);         // 回调里允许同步 Restart;之后不再碰任何组件引用
   ```
   - 不变量:Finish 只在起始标签已返回、且读写都不在途时发出,所以 FINISH 是这次流的最后一个标签;销毁旧 Call 之前流上没有在途操作。这是与方案 B 的本质区别。
   - TryWriteNext 只在 kOpen 时写。
   - `Cancel…Stream` 就是 BeginDrain。
   - `Restart…Stream` 只在 kClosed 时可调,否则打 ERROR 并返回 false。做法:清空待发缓冲、复位写标志和 StreamState、`call = std::make_unique<Call>()`、重新 `Async<M>(&call->context, &cq, tag(INIT))`。`Init<File>GrpcNode` 首次建流走同一个函数,流的创建只留一个入口。
   - 删掉 WRITES_DONE 分支和 FINISH 里的 `cq.Shutdown()`。
   - 文件级分发 `Handle<File>CompletedQueueMessage(..., GrpcTag*, bool ok)`:流式方法把 ok 传进状态机;同文件的 unary 方法 ok=false 时只记 ERROR 不分发;每个出口都 `tagPool.destroy`。
   - 草图里「INIT 失败后直接 Finish」「TryCancel 后不等 Read 返回假就 Finish」的时序属外部常识,由【五】的 T5、T6 在真实 gRPC 库上验证。

【二、EtcdService(策略)】
文件:etcd_service.{h,cpp}、etcd_helper.{h,cpp}、service_discovery_manager.cpp、node.{h,cpp}。
1. 关闭回调与退避重建。
   - 装 `AsyncWatchWatchStreamClosedHandler`、`AsyncLeaseLeaseKeepAliveStreamClosedHandler`,转到 `OnStreamClosed(kind, status)`。kind 用 `enum class EtcdStream : uint8_t { kWatch, kLeaseKeepAlive, kCount }`,每条流一份 `{ TimerTaskComp timer; uint32_t consecutiveFailures; steady_clock::time_point startedAt; }`。
   - OnStreamClosed:`shuttingDown_` 为真直接返回。上一次流存活满 10s 才把连续失败数清零,否则加一。延迟取 `EtcdHelper::StreamRestartDelaySeconds(failures, jitterUnit)`,即 min(5s, 0.2s × 2^(n-1)) × [0.8, 1.2];随机数由调用方传入。
   - 日志:`etcd <Watch|LeaseKeepAlive> stream closed: code=<n> msg=<…>; restart in <s>s, attempt=<n>`(WARN,每 10 次连续失败升为 ERROR);首个应答恢复时 `etcd <kind> stream recovered`(INFO)。
   - 不设放弃上限。这是对 AGENTS.md §11.3「次数上限」的有意偏离,写进设计文档:gate / scene 没有会杀容器的探针,放弃等于永久失联;失租的有界处理仍是既有的 181s 重注册,身份探针仍是 TTL/2,两条 fail-closed 语义都不改。
   - 退避数值注明「待压测复核」。
2. KeepAlive 流重建后,leaseId>0 就立刻入队一拍,不等 1s 定时器。
   - 窗口核算:TTL 180s。断链感知:RST / GOAWAY 即时;静默断链最多约 40s(HTTP/2 保活 30s + 10s,外部常识)。退避最多 6s。合计远小于 TTL/2(90s),探针一般不翻。
   - etcd 整体停机超过 90s 时探针为假(fail-closed),恢复后首个 ACK 即恢复。租约真过期时应答 TTL=0,走既有的 etcd_service.cpp:598-608。
   - 约束写进设计文档:不得用 `GRPC_KEEPALIVE_TIME_MS=0` 关掉保活(grpc_channel_cache.h:42-51),否则静默断链时两条流都不会收到 ok=false。
   - 流在 kStarting 期间,每秒的续租请求会在缓冲里堆积(上限 200),打开后一次发出;协议上无害,不为此加分支。
3. Watch 流打开或重建后,逐前缀做「list → 对账删除 → 从 R+1 watch」,不按旧 revision 续。
   - 状态:`prefixesNeedingWatch_`(流每次(重)建后置为全部前缀)、`rangeQueue_`(待发前缀,按前缀去重,上界等于前缀数)、`rangeInFlight_`(单个在途前缀)。删掉 `hasSentRange`、`revision`、`watchReconnectTimer`、`ScheduleWatchReconnect`、`StartWatchingPrefixes`。
   - Range 串行化:生成的成功处理器拿不到请求,所以同一时刻只发一条 Range,应答归属于在途前缀。周期 Range 也走这个队列(service_discovery_manager.cpp:27-33 改调 `etcdService.RequestRange(prefix)`)。
   - 同时装 `AsyncKVRangeFailedHandler`(回调里有请求副本):释放在途前缀、沿用 `gRPC KV.Range failed: code=` 的日志格式、继续发下一条。在途前缀靠应答或 5s deadline(base_deploy_config.yaml:234)必然释放;该前缀仍在 `prefixesNeedingWatch_` 里,下一个 2s 周期自然重试。
   - Range 成功时:先照旧对每个 kv 调 HandlePutEvent。若该前缀在 `prefixesNeedingWatch_` 里(此时它没有活的 watch,不会被事件超车),再做对账并建 watch。
   - 对账删除的安全条件(全部满足才删,否则跳过删除并打 ERROR,仍从 R+1 建 watch):`reply.more()` 为假;每个键都以在途前缀开头;每个非 `/allocated/` 的值都能解析成 NodeInfo 且 uuid 非空;前缀能映射到合法节点类型(NodeUtils::GetServiceTypeFromPrefix,node_util.cpp:30-42)。
   - 要删的集合:本地该类型节点里 uuid 不在快照中的。走与 HandleServiceNodeStop 相同的摘除路径:把 node.cpp:1360-1416 的后半段抽成 `Node::RemoveServiceNode(const NodeInfo&, NodeRemovalTrigger)`(枚举 kWatchDelete / kResyncReconcile / kCount);对账触发时,已在宽限期待摘的不重置定时器。
   - 自己的键若不在快照里,本地快照被摘,触发既有的「missing local node snapshot」重注册(node.cpp:1564-1572),这是期望行为。
   - 「快照 → uuid 集合与是否完整」「本地列表 + 快照 → 要摘的 uuid」「退避计算」三个纯函数放进 EtcdHelper(它已有 TxnClaimSucceeded 这类纯函数,不用新增源文件,也不用动 vcxproj / CMakeLists)。
   - 周期 2s Range 的增量语义本次不动;它等交接 §4.4 那项去掉,而那项必须排在本修复之后。
   - 本次不处理的既有竞态,写进设计文档:周期 Range 发出后到达的 watch DELETE,会被随后的 Range 应答加回来(etcd_service.cpp:58-59 不比较 mod_revision)。
4. 服务端 canceled(etcd_service.cpp:361-365)改成 `CancelWatchWatchStream`,走「拆流 → 重建 → 对账」同一条路;压缩取消(proto/etcd/etcd.proto:444-452)因此也被覆盖。这一步在应答处理器内取消流,靠【一】第 4 条的重新判阶段保证安全。
5. 启动路径并进同一机制:首条 watch 应答触发申请租约的逻辑不变(:355-359)。启动时 etcd 不可达也能自愈。
6. 备选(供负责人取舍,不推荐):单独维护只由 watch 应答推进的续看版本,重建时从它续,由 etcd 回放漏掉的删除;只有收到带 compact_revision 的取消才走全量对账。
   - 好处:对账只在压缩时触发。
   - 代价:恢复路径变成两条,压缩那条平时走不到、最难测;对账代码一行也省不掉;文件数相同。etcd 压缩那张 P0 落地后,旧 revision 被压缩会成为常态。

【三、生命周期(AGENTS.md §11.7)】
- 不新增任何对 channel 的强引用,重建复用实体上已有的 stub。
- 新定时器是 EtcdService 的成员 TimerTaskComp:析构即 Cancel,并带存活令牌(time/comp/timer_task_comp.cpp:15-17、:58-75),属于「与被绑对象同寿命的成员」。
- EtcdService 装进生成层、捕获 this 的全局回调,以及各自在哪清(落码时逐项对照):
  | 回调 | 装 | 清 |
  | 11 个应答处理器(现有) | InitHandlers | Shutdown 的 `SetEtcdHandler(emptyHandler)`(etcd_service.cpp:460-461) |
  | AsyncKVRangeFailedHandler(新) | InitKVHandlers | Shutdown 置空 |
  | AsyncWatchWatchStreamClosedHandler(新) | InitWatchHandlers | Shutdown 置空 |
  | AsyncLeaseLeaseKeepAliveStreamClosedHandler(新) | InitLeaseHandlers | Shutdown 置空 |
- Shutdown 还要:置 `shuttingDown_`;取消两个重建定时器;清空 `rangeQueue_`、`rangeInFlight_`、`prefixesNeedingWatch_`。
- 只把 Range 的失败处理器置空,不调 `SetEtcdFailedHandler({})`:后者会把 gate 装的全局兜底(gate/main.cpp:325,无捕获)一并清掉,没有必要。
- 关机时流和完成队列的排空维持现状(incident 文档 :499 的独立事项),本次不扩。

【四、其它要求】
- 不用宏:模板只生成普通函数和结构;枚举写明底层类型,末项 kCount。
- 日志只有状态码、消息、次数、时长,不含密钥(etcd 未开鉴权)。
- 可观测性:关闭、重建、首个应答恢复、对账(前缀、revision、摘除数、是否完整)各一条日志。C++ 侧有没有指标导出未核实,先不加指标。
- 新旧并存:不改协议、键布局、配置键,新旧节点可混跑,回滚就是回滚二进制。
- 失败语义:流不通期间服务发现是陈旧的,这一点无法避免;恢复后以快照对账。身份和资产路径仍然 fail-closed。对账删除在快照不完整时一律不删。

【五、回归测试】
1. Go 模板测试,放 internal/generator/cpp/grpc_process_test.go。HEAD 上红,而且红的原因必须是断言,不是夹具渲染报错。
   - `TestGrpcInitTemplateDoesNotDropFailedStreamCompletions`:渲染一个含流式方法的文件加一个纯 unary 文件;完成循环的 `if (!ok)` 内没有 `return;`;流式文件的分发收到 `ok`,unary 文件不收。
   - `TestGrpcAsyncClientTemplateStreamLifecycle`:渲染一个流式方法;必须出现 Restart / Cancel / StreamClosedHandler 和 FINISH 分支调用关闭回调;不得出现 `cq.Shutdown()`。夹具要按 model.go 的访问器补齐流式方法的描述符字段。
2. C++ 行为测试,放 cpp/tests/rpc_controller_test,登记到 rpc_controller_test.vcxproj:169-171 旁。不依赖真 etcd:进程内起 grpc::Server,用生成的 `etcdserverpb::Lease::Service` / `Watch::Service`(etcd.grpc.pb.h:982、:1276)做假服务,可按指令让流以 UNAVAILABLE 结束。客户端用真实的 InitGrpcNode 和 HandleCompletedQueueMessage,注册表取 `tlsNodeContextManager.GetRegistry(EtcdNodeService)`,每个用例自己 `registry.create()` 一个实体(先例 rpc_client_lifecycle_test.cpp:213-215)。
   - 阶段 A 文件 etcd_stream_break_test.cpp,只用 HEAD 已有接口:
     - T1 `EtcdStreamBreakTest.KeepAliveWriteStateIsNotStuckAfterStreamBreak`:收到一次 ACK → 假服务结束流 → 再发一拍并泵若干轮 → 断言 `LeaseKeepAliveRequestWriteInProgress.isInProgress == false`。HEAD 上红(卡在 true),修复后绿。
   - 阶段 B 文件 etcd_stream_lifecycle_test.cpp:
     - T2 关闭回调在排空后恰好触发一次,状态非 OK。
     - T3 重建后续租 ACK 恢复。
     - T4 重建后 watch create 收到 created。
     - T5 起始连不上(未监听的端口)→ 关闭 → 起假服务 → 重建成功。
     - T6 在应答处理器内调用 Cancel:关闭回调恰好一次,到 kClosed 时 readPending 为假,过程中没有 gRPC 断言。
     - T7 在关闭回调里同步调用 Restart 可用。
     - T8 退避函数:单调、封顶、抖动上下界(注入随机数)。
     - T9 快照解析:空快照算完整;more 为真、任一值解析失败、键不属于该前缀都算不完整;`/allocated/` 被跳过。
     - T10 要摘集合:本地有而快照无的才摘;快照多出的不影响。
   - 收尾:完成队列先 Shutdown 再排空,之后才销毁实体;把全局处理器复位,不污染同工程的其它用例。
3. EtcdService 的策略耦合 gNode,不单测;由 Codex 重跑路线 B 验收,期望 A / B / C 全部翻成「不成立」,并出现新的 `stream closed`、`stream recovered` 日志。这个缺口如实写进交付说明。
4. 这个工程链 scene.lib、gate.lib(vcxproj:126),受 scene 库的编译阻塞。要不要另建只链 grpc_client / proto / core / thread_context 的小工程,见 risks。

【六、交 Codex 的顺序与通过标准】
工作目录一律是主仓 `D:\luyuan\xuanming-server-mmo-cpp`:改动不提交,只在这棵树可见;这棵树里有他人未提交的文档、Go、部署脚本改动,不要动。负责人要求在 Codex 的树里跑时,改用补丁文件传递。Shell:PowerShell。
0. 工具自检并回报:`go version`(生成器要求 go 1.27.2,tools/proto_generator/protogen/go.mod:3);`protoc --version` 必须是 `libprotoc 35.1`,否则停;`Get-Command grpc_cpp_plugin`;MSBuild 路径:
   ```powershell
   $msbuild = & "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe' | Select-Object -First 1
   ```
   protoc 与 grpc_cpp_plugin 用 `third_party\grpc\install_vs2026_dbg\bin` 那份,放到 PATH 最前(docs/PROGRESS.md:5997、:6345)。
阶段 A(只落测试,留红):
1. `Set-Location D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen; go test ./internal/generator/cpp/ -run "TestGrpcInitTemplateDoesNotDropFailedStreamCompletions|TestGrpcAsyncClientTemplateStreamLifecycle" -count=1 -v`。期望两条 FAIL,保留输出。
2. `Set-Location D:\luyuan\xuanming-server-mmo-cpp; & $msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`,然后 `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test`。期望汇总行是「挂:EtcdStreamBreakTest.KeepAliveWriteStateIsNotStuckAfterStreamBreak」,其余用例与基线一致。T1 在 HEAD 上若是绿的,停下回报。
阶段 B(落修复和其余测试):
3. 重跑第 1 条,期望 PASS。
4. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(本机现在没有 proto-gen.exe)。
5. regen,在隔离的临时 worktree 里做:
   - 先把改过的 3 个 .tmpl 拷进去(模板运行时从磁盘读);把 HEAD 上尚未 regen 的 proto 退回上次 regen 的内容(先例见 grpc-client-deadline 文档 §9.1);
   - 用 `enable_unity_client: false` 的配置副本跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath <副本>`;
   - 只拷回 3 个文件:cpp/generated/grpc_client/grpc_init_client.cpp、etcd/etcd_grpc_client.{h,cpp}。
   - 验收三条,任一不满足就停下报告:
     a. cpp/generated/grpc_client 其余 31 个生成文件零差异;
     b. grpc_init_client.cpp 的差异只出现在完成循环体和 etcd 那一行前置声明(HEAD :81),任何 messageId 列表变化都算不满足;
     c. etcd 的 11 个消息号(0、4、5、6、13、20、22、25、59、73、80,HEAD :393)不变。
   - 生成器被本机应用控制策略拦截(docs/PROGRESS.md:6817 有先例)时停下报告,不要绕。
6. 全量编译:`& $msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。改动面:grpc_client → core → 三个节点 → 测试。通过标准:exit 0;bin 下三个 exe 的修改时间晚于 lib 下所有 .lib。
7. `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test`:T1~T10 和原有用例全过。
8. 重跑路线 B 的 B0~B6。
失败时保留:第一条 `error C` 或 `LNK` 及前后 20 行;gtest 输出;节点文件日志。失败不连续重试。
Linux 构建(deploy/k8s/Dockerfile.cpp)未验证;本修复不新增 core 源文件,不需要改 CMakeLists。

【七、文档与登记】
- 新增 docs/design/etcd-stream-rebuild.md,并在 docs/README.md 登记一行。
- 回写 [历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §10 第 1 条、handoff-backlog-2026-09-05.md 的 P1-13、node-id-overhaul-plan-20260908.md:191 / :276 的验收口径(加「且零重注册」)、handoff-global-singletons-10m-ccu-20261013.md §3 的状态列。
- docs/PROGRESS.md 只在文件最末尾另起一条(该文件和 docs/README.md 当前都有他人未提交的修改),写明 Java 状态;没有 Codex 结果前写「未编译,待 Codex 验证」。
- Java 仓库 PARITY.md 追加一行(见 java_parity)。

### 原报告:Java 版

结论:不适用。Java 版不用 etcd,本项修复没有对应物。

证据(D:/luyuan/xuanming-server-mmo,HEAD d942f74,只读):
- deploy/compose/infra.yaml:11 `# - 只放依赖。基线的 etcd / Nacos / Redis Cluster / kafka-ui / topic-init 都不移植(spec §2.3)。`
- 节点号租约:game-discovery/src/main/java/com/game/discovery/NodeIdLease.java。
  - Redis `setIfAbsent` 占号(:93)。
  - 每 TTL/3 发一条独立的续期 Lua(:41-42、:72-73、:147-158)。
  - 续期失败等下一轮,超过 TTL 判丢失(:160-167)。
  - isValid 取 2/3 TTL(:142-144)。
  - 每次续期是独立命令,没有流状态可丢。
- 节点目录:game-discovery/.../NodeDirectory.java:22-27、:48-49。带 TTL 的 Redis 条目,读方按需读,没有 watch。
- 热关停同步:killswitch/RedisKillSwitchSync.java:60-61,定时轮询。
- PARITY.md:36 已有相邻行「节点号租约有效期判定 … 无对应(mmorpg 用 etcd 租约 …)… 不适用(Java 内部)」。

同类风险点(不是本项的同一缺陷,未实测):Java 版有常驻的 Redis 发布订阅监听,Redis 重启后能不能恢复靠 Redisson 自动重订阅(外部常识,未核实)。
- game-gate/.../presence/GatePushSubscriber.java:53 `listenerId = topic.addListener(byte[].class, (channel, message) -> accept(message));`
- game-scene/.../ownership/OwnerTakeoverSubscriber.java:35(同形)
- game-scene/.../SceneNode.java:850-851(增益封禁变更通知)
- game-login/.../ownership/RedisOwnerTakeovers.java 是发布方(:30 publishAsync),不是监听方。

PARITY.md「功能对齐」表末尾追加一行(七列,列序见 PARITY.md:25;状态列按现有写法用全角括号):
| C++ etcd 长连接(Watch / LeaseKeepAlive 双向流)断线重建,重建后按前缀全量对账 | `cpp/libs/engine/core/node/system/etcd/etcd_service.{h,cpp}`、`tools/proto_generator/protogen/internal/template/grpc_async_client.{h,cpp}.tmpl`、`grpc_init_total.cpp.tmpl` | <修复提交> | — | — | 不适用（Java 不用 etcd） | <日期> 登记。Java 的节点号租约是 Redis 占号加每 TTL/3 一条独立续期命令(`NodeIdLease`),节点目录是带 TTL 的 Redis 条目(`NodeDirectory`),热关停同步是定时轮询,都没有需要重建的流。Java 的同类风险点是 Redis 发布订阅监听(`GatePushSubscriber`、`OwnerTakeoverSubscriber`、scene 的增益封禁通知)在 Redis 重启后的重订阅,依赖 Redisson,未实测,另行登记待核实 |

本仓库 docs/PROGRESS.md 的条目同时写:「Java 版(AGENTS §12):对应 Java 版本号:无;状态:不适用(Java 不用 etcd);已在 PARITY.md 登记」。

### 原报告:风险与未决

【需要负责人拍板】
1. 「先实测再落码」的门槛怎么过。本机今天跑不了任何 C++ 实测(子模块未检出、没有 gate.exe、scene 库编不过、没有历史日志)。三个选项见 codex_measurement 的「负责人先拍板」:甲 恢复整套构建环境;乙 只恢复 gRPC 加一个最小测试工程;丙 破例先落码。甲、乙都要联网下载子模块,build_grpc.ps1 还会经 winget 装或升级 cmake、ninja,属于 AGENTS.md §10.2 的「安装工具」。
2. 阶段 A 只加测试文件算不算「落码」。我的理解是它本身就是实测手段,请确认。
3. 修复范围。推荐一批做完「流重建 + 重建后按前缀对账删除(含 Range 串行化)」。只重建流、不做对账,断流期间下线的对端会永久留在本地。备选(从 watch 自己的 revision 续,压缩时才对账)见 fix_design【二】第 6 条。
4. 文件数。预计 26 个文件(含 3 个 regen 产物、8 个文档),贴近 §10.2 的 30 个文件门槛。若再新建测试工程,或把 ok 参数推广到全部 15 个客户端文件,就会越线,要先授权。
5. 回归用例放哪。放 rpc_controller_test 不用新建工程,但它链 scene.lib、gate.lib,被 scene 的 110 个错误卡住。另建只链 grpc_client / proto / core / thread_context 的小工程可以解耦,但属范围扩大,而且这几个库在 HEAD 上能否单独编过没有证据。
6. 构建树与改动传递。改动不提交,只在主仓工作区可见;建议 Codex 直接在主仓跑,或由负责人指定用补丁文件传给 Codex 的树。
7. regen 由谁跑、怎么隔离。本机没有 proto-gen.exe;生成器模块要求 go 1.27.2,而本机记录过的是 1.26.x,可能要下载工具链;全量 proto-gen-run 会重写别的会话尚未 regen 的产物(装备等),必须在隔离 worktree 里跑并只拷回 3 个文件;生成器在本机被应用控制策略拦过(docs/PROGRESS.md:6817)。
8. 修复上线前的运维口径,两条都要:
   - 不滚动 etcd。钉镜像版本(开服交接 §4 第 7 项的附加要求)、改 etcd.yaml 都会触发 StatefulSet 滚动,必须排在本修复验证通过之后。压缩那一项按交接首选的「不重启 etcd 的定时 compact」做,不受此限。
   - 现在就排查存量:任何一次连接断开都会触发本缺陷,现有环境里可能已有节点处在 181s 循环里(命令见 codex_measurement【存量排查】),受害节点只能重启。
9. 退避数值(0.2s 起,封顶 5s,±20% 抖动,稳定 10s 清零)是按 TTL 180s 推出来的,待压测复核。约 100 个 zone 时全集群同时重建流会不会给 etcd 造成惊群,没有数据。「不设次数上限」是对 §11.3 的有意偏离,需认可。
10. 成员失去法定人数的场景不在本次修复内。连接正常但所连成员掉出法定人数时,通道不会换成员(只配了一个 ClusterIP 地址),流重建会一直打到同一个成员上。是否加「连续 N 次失败后重建通道」,等三成员实测后再定。
11. 验收口径。node-id-overhaul-plan:191 / :276 与 etcd.yaml:10 的「无任何节点 fence」应加「且零重注册」。etcd.yaml 属于 etcd 压缩那张卡片的文件,本项不碰,请那边顺带改注释。

【风险】
12. 推断 I2 / I3(键每轮被删 1~2s、gate 把会话绑定置无效、scene_manager 当成节点离开、node_id 被抢)没有运行证据。成立的话,线上影响比交接写的重得多。路线 B 的 5.5 和三成员验收第 3 项是它的证据来源。
13. 对账删除是新增的「主动摘节点」路径,风险是误摘。设计里限定只在该前缀没有活 watch 时才对账,快照不完整一律不删,并从同一快照的 R+1 续 watch;即使误摘,2s 内的周期 Range 也会加回来,但 gate 上会有一次会话绑定失效。
14. EtcdService 的策略没有单测,只有 docker 实测兜底。
15. 修复依赖 HTTP/2 保活才能感知静默断链;`GRPC_KEEPALIVE_TIME_MS=0` 会让它失效(今天全仓没有地方设置这个变量)。
16. 状态机里「INIT 失败后直接 Finish」「TryCancel 后不等 Read 返回假就 Finish」的合法性属外部常识,靠 T5、T6 在真实库上验证;不通过就改成「先读到 ok=false 再 Finish」。
17. 关机时流和完成队列不排空的旧问题(incident 文档 :499)仍在;本修复不让它变坏,也不解决。
18. 既有竞态仍在:周期 Range 应答可能把刚被 watch 删掉的节点加回来(不比较 mod_revision)。本次只登记。
19. gate 每 10s 的无条件 Put 在新租约拿到后、分配键 CAS 裁定前就会用新租约写服务键(E13)。既有问题,本次不动。

【与另外两份交接的重叠 / 冲突】
20. 开服交接(handoff-launch-readiness-and-d1)§4 第 7 项、§7:etcd 相关止血归本交接,附加「钉住 etcd 镜像版本」。和本项只有先后依赖(见第 8 条),文件不重叠;本修复不碰 k8s_deploy.ps1。
21. go/db 交接(godb-pipeline-handoff)§2.3、§2.4:和本项没有文件或语义重叠。§2.4「新账号不碰任何集群」同样适用。
22. 交接 §4.4 的「去掉 2s 全量 Range、只消费 watch」必须排在本修复之后。今天这条轮询是 watch 死后新增节点还能被发现的唯一途径。
23. 工作区不干净,和任务书写的「工作区干净」不一致。我核对时有 15 个已修改、6 个未跟踪文件,且数量在调查期间还在变。和本项直接相关的三处:
   - docs/PROGRESS.md 末尾有 Codex 未提交的 16 行;docs/README.md 也有他人未提交的修改。本项只在末尾追加,提交时逐文件、逐段暂存,不要 `git add -A`。
   - tools/scripts/k8s_deploy.ps1 比 HEAD 多 431 行,本报告引用的是 HEAD 行号。
   - 另一个会话正在同一工作树里写文件。

【未核实项(实测或查证后再定)】
24. gRPC 语义 X1(Codex 用路线 A 的 T1 实测):断流后 Read / Write 的 ok=false;unary Finish 的 ok 恒真;INIT 失败的表现。
25. etcd 语义 X3、X4(Codex 用路线 B 和三成员验收实测):租约到期到删键的延迟;单成员带持久化重启后租约是否保留并刷新 TTL;换主时是否主动结束 LeaseKeepAlive 流;bitnamilegacy 镜像的默认数据目录是否就是 /bitnami/etcd/data(compose 没显式设 ETCD_DATA_DIR)。
26. 环境(Codex 第 0 步回报):pwsh 是否可用(Directory.Build.targets:52 的生成前检查也调用它);Go 的位置与版本;protoc 版本;本机有没有可用的节点镜像。
27. Docker Desktop 的端口转发在容器重启时会不会马上断开宿主侧连接;不会的话 `RPC failed` 最多晚约 40s 出现。MSVC 运行库对重定向 stdout 的缓冲行为(Codex 在 B2 回报两份日志的时间差)。
28. 单起 gate(只有 etcd / redis / kafka)能不能到启动 banner:依据只有 start_game.ps1:266-267 的注释,未实跑。
29. Go 实例重启后 node_uuid 是否变化;scene 被 gate 摘掉再加回后,会话绑定有没有别的恢复路径(需读 scene / scene_manager 侧代码,本次未做)。
30. C++ 节点有没有可用的指标导出,用来加「流重建次数」计数。
31. Go 各服务的 etcd 续租和 watch 重建只看了调用点,没有逐个核实 clientv3 的行为。
32. Java 版 Redisson 断线后的自动重订阅(Java 仓库另行登记)。

### 原报告:预计改动文件

- tools/proto_generator/protogen/internal/template/grpc_init_total.cpp.tmpl
- tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl
- tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl
- tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test.go
- cpp/generated/grpc_client/grpc_call_tag.h
- cpp/generated/grpc_client/grpc_init_client.cpp
- cpp/generated/grpc_client/etcd/etcd_grpc_client.h
- cpp/generated/grpc_client/etcd/etcd_grpc_client.cpp
- cpp/libs/engine/core/node/system/etcd/etcd_service.h
- cpp/libs/engine/core/node/system/etcd/etcd_service.cpp
- cpp/libs/engine/core/node/system/etcd/etcd_helper.h
- cpp/libs/engine/core/node/system/etcd/etcd_helper.cpp
- cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp
- cpp/libs/engine/core/node/system/node/node.h
- cpp/libs/engine/core/node/system/node/node.cpp
- cpp/tests/rpc_controller_test/etcd_stream_break_test.cpp
- cpp/tests/rpc_controller_test/etcd_stream_lifecycle_test.cpp
- cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj
- docs/design/etcd-stream-rebuild.md
- [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md)
- docs/design/node-id-overhaul-plan-20260908.md
- docs/handoff/handoff-backlog-2026-09-05.md
- docs/handoff/handoff-global-singletons-10m-ccu-20261013.md
- docs/README.md
- docs/PROGRESS.md
- D:/luyuan/xuanming-server-mmo/PARITY.md

## P0-E 共享 Redis 没设 maxclients


### 结论

- 第一轮判定:`partially_confirmed`。
- 在 `2f897bd07` 上是否仍成立:是。

**把握**:

对 partially_confirmed 这个判定:高。两份对抗复核都没能推翻它,查到的问题只让缺陷更硬。

- 高(本次逐行重读过):
  - 四处 Redis 服务端都没有 maxclients,全仓部署与代码目录 0 命中;
  - 各客户端创建点、池参数与连接名缺失、三个 go-redis 版本的分布;
  - C++ 每进程 1 条;login 的 Pub/Sub 连接;K8s 副本数、CPU limit、两个池指向;
  - redis.yaml 的渲染即文件、每次 infra-up 都 apply;redis-ha 没有 apply 路径;
  - v9.18.0 的缺省池大小、封顶、无后台回收、握手吞错误、重试分支(仓库内 vendored 源码);
  - 他人在途文件的现状(git status 与 diff --stat)。
- 中:
  - 每 zone 130~350 条的口径。算术已核对,但依赖两条未实测的输入:K8s 上的 GOMAXPROCS;go-zero 不设 PoolSize。
  - v9.16.0、v9.17.3 与 v9.18.0 行为相同(推断,S2b 读源码可确认)。
  - 到线后 Go 客户端看到的错误文本(由握手代码推断)。
- 低(全部列入实测或交负责人):Redis 缺省 10000 与 nofile 下调行为;生产运行时的 nofile 硬限;到线后探针、haproxy、C++ 的连锁反应。

会影响落码的一点:工作区在调查与复核期间持续被另一会话改动(k8s_deploy.ps1 从 +340 到 +431,契约测试文件从干净变成 +301),本报告的分批以定稿时的 git status 为准,落码前要再看一次。

### HEAD 前进后的更正(0aa1027bc → 2f897bd07)

逐提交核对 0aa1027bc..2f897bd07(13 个提交)里与本项相关的改动。每条回答三个问题:影响结论吗 / 影响实测步骤吗 / 影响修复设计吗。

【没有改动的关键文件(已用 git diff --quiet 确认)】
deploy/k8s/manifests/infra/redis.yaml、redis-match-cluster.yaml、kafka.yaml、etcd.yaml;deploy/docker-compose.yml;deploy/k8s/manifests/go-svc/*、java-svc/*;deploy/k8s/AGENTS.md;tools/scripts/dev_tools.ps1;tools/scripts/tests/lib/test_harness.ps1;robot/etc/robot_smoke.yaml;cpp 的 node.cpp、redis_manager.cpp、config.cpp;robot/vendor 里 go-redis v9.18.0 的源码(锚点 options.go:341-342、:379,pool.go:343、:707、:746、:991、:1091,redis.go:504-525,error.go:85-87、:109 全部原位);docs/design/shared-redis-split-by-purpose.md、gate-connection-admission-control.md;交接 docs/handoff/handoff-global-singletons-10m-ccu-20261013.md。

【A】e4ff1d165 tools/scripts/k8s_deploy.ps1(+745/-49)
1. G2b 的 MySQL 连接总账入库:Get-AuthoritativeScalar :572、Get-MysqlConnectionPlan :1024、Get-MysqlMaxConnectionsFromManifest :1053、Get-ManifestDeploymentReplicas :1074、Get-MysqlConnectionLedgerInputs :1099、Get-MysqlConnectionLedger :1143、Write-MysqlConnectionLedger :1201、Get-MysqlPoolMirror :1268;写路径调用在 :6651-6653(只告警)。
   - 结论:不影响。
   - 实测:不影响。
   - 修复设计:影响。第二批的「阻塞于 G2b 提交」已解除。Get-ManifestDeploymentReplicas 的报错前缀仍写死「MySQL 连接总账」(:1078、:1082),原计划「加前缀参数,不复制第二份」照旧。另外,段头 :1008-1012 有一条约束:总账函数只能依赖 $RepoRoot 与 Get-AuthoritativeScalar,契约测试才能按 AST 抽出来。Redis 总账函数必须守同一条。
2. G4b 削峰:
   - 新增 -GatewayRateLimit,缺省 auto(:242);由 Resolve-PeakShavingSwitch 解析(:349-363),staging / prod 缺省开,dev 缺省关。
   - gateway ConfigMap 现在写 Redis 口令(:5562-5568、:5626),Pod 模板带削峰指纹注解(:5694-5715)。
   - 结论:不影响。只是 staging / prod 每个 gateway 副本由 1 条变 2 条(RateLimitConfig.java:50、:61 的专用连接),每 zone 多 2 条,不改档位。
   - 实测:不影响。本机 dev 档与 jar 内配置仍是限流关(application.yaml:129)。
   - 修复设计:影响。总账常数表里 Java gateway 在 staging / prod 按 2 条/副本计。原「邻接发现:gateway ConfigMap 没有 Redis 口令」已修复,应删除。
3. 行号整体漂移(Apply-Infra 段 +688,其上各段 +432 到 +563 不等)。Redis 部署语义未变:
   - Apply-Infra :6432;$infraManifests :6448,仍不含 redis-ha.yaml(RedisReplicas、ConfirmRedisCoreRestart、RedisSplit 在 tools/scripts 下 0 命中);
   - redis 分支 :6491-6523;只替换 __INFRA_NAMESPACE__ 在 :6537;通用占位检查 :6558-6561;apply :6563;等待 :6581;
   - 触发条件 $deploysInfra :1682。
   - 结论:不影响。实测和修复设计:只需改出处行号。

【B】e4ff1d165 tools/scripts/tests/k8s_deploy_contract.tests.ps1(+305/-4)
- MySQL 总账用例入库:函数抽取在 :1455-1466,用例在 :1509-1690。AST 抽取点 :390、:1341、:1459。GateRouterMode 透传用例在 :1251,helper 在 :1223。
- 文件里仍然 0 条 redis 断言(`redis\.yaml|requirepass|appendonly|maxmemory|maxclients` 0 命中)。
- 结论:不影响。实测:不影响。
- 修复设计:影响。「该文件有他人未提交改动,不能加用例」已不成立。第二批的总账、重启闸、透传用例可以照 :1455 起的形状加进这个文件。第一批的静态断言仍可单开新文件。

【C】e4ff1d165 deploy/k8s/README.md(+100/-3)
- 新增「MySQL 连接总账」小节,从 :656 起。
- 原报告引用的 nofile 说明从 :1021 移到 :1118-1119。
- 修复设计:Redis 小节照这个小节的形状写。其余只改出处。

【D】e4ff1d165 告警文件与 CI 触发路径
- deploy/k8s/mysql-connection-alerts.yaml 入库(新文件 199 行)。
- .github/workflows/deploy-config-tests.yml 的触发路径新增 deploy/k8s/*.yaml、deploy/k8s/*.json 和 gateway 的 application.yaml;仍不含 deploy/docker-compose.yml。
- 结论、实测:不影响。
- 修复设计:第三批告警可以照 mysql-connection-alerts.yaml 写,它比 scene-manager-alerts.yaml 更贴近「总账同源阈值」;新增的 deploy/k8s/redis-connection-alerts.yaml 会触发 CI。

【E】e4ff1d165 Go 工具链升级
- 所有 go/*/go.mod 的 go 行统一为 1.27.2(原来是 1.24.5 和 1.26.5 两组)。
- deploy/k8s/Dockerfile.go-svc:64 的构建镜像改为 golang:1.27.2-alpine3.24,并断言 go1.27.2。
- go-redis / go-zero 版本未变:
  - v9.16.0:login、player_locator、guild、shared;
  - v9.17.3:scene_manager、match、chat、friend、client_rpc_router(indirect)、trade(直接依赖);
  - v9.18.0:db、data_service、robot。
- automaxprocs v1.6.0 仍全是 // indirect;deploy、tools/scripts、go 下 GODEBUG / GOMAXPROCS 0 命中。
- 结论:成立与否不变,但总账系数 G 的预期变了:
  - Go 1.25 起的容器感知 GOMAXPROCS(外部常识:按 CPU limit 向上取整,下限 2)现在对全部模块生效。500m 和 "1" 的 limit 预期 G=2;若 go-zero 链进的 automaxprocs 生效,可能是 1。
  - B 档(每 zone 约 250 条,约 39 个 zone 越过 10000)成为基线。
  - 「G 没被收住 = 节点核数」那一档,现在只在两种机制都被关掉时才出现。
- 实测:S2b、S6 仍要做,期望改为 G=2(或 1)。本机 Windows 没有 cgroup,本机起栈仍要显式设 GOMAXPROCS=1。
- 修复设计:总账里「G 未实测前按 2 取」的理由更硬了。

【F】11493a157、946ffd216 go/friend/internal/svc/servicecontext.go
- 两个 MustNewRedis 从 :152、:164 移到 :165、:177。没有新增 Redis 客户端;go 下非测试代码的创建点重新 grep,与原清单一致。
- login 的排队(loginqueue.New / NewDispatcher,servicecontext.go:442、:454)复用 s.RedisClient,不增加池。
- 三个问题都只影响出处行号。

【G】e4ff1d165 deploy/k8s/manifests/infra/redis-ha.yaml(+14/-14)
- 只把 echo 文案换成英文,行数不变。:3、:250-255、:530、:546-547、:625-629、:808、:812 全部原位。
- 三个问题都不影响。

【H】e4ff1d165 与 946ffd216 的日志英文化
- 涉及 start_game.ps1、k8s_gate_drain.ps1、k8s_zone_rollback.ps1、k8s_image.ps1、go_services.ps1、cpp/nodes/gate/main.cpp、battle 的 Redis 日志。都是等行数替换,本项引用的行号全部原位(start_game.ps1:22、:110-113、:341、:366-371、:445-446、:451、:551、:564、:580、:595-606;k8s_gate_drain.ps1:630-640;k8s_image.ps1:421;go_services.ps1:85-87、:662-663、:1007-1008、:1039)。
- S5 要匹配的 Go 侧文本没变,例如 player_locator 的 `failed to connect Redis`(servicecontext.go:35)。
- 三个问题都不影响。

【I】e4ff1d165 docs/handoff/godb-pipeline-handoff-20261013.md
- 文件头插入 1 行,存盘交接引用整体 +1:
  - :90 → :91
  - :225 → :226
  - :243 → :244
- §3.5 在 :150。只影响出处。

【J】其余提交(0d34dcf8d、a9bfffc2b、898c803ea、6e212616b、302ecdd21、0b40715d0、db1b8c5bb、ab00ad8b8、0fbfbe6c8、2f897bd07)
- 装备、宠物、帮会契约、battle handler 生成器与模板、文档。
- 不碰任何 Redis 建连点或服务端配置。battle 仍经 node.cpp:557 每进程 1 条。
- 三个问题都不影响。

【环境变化(不是提交)】
- 在本项里的影响:C++ 三个节点 exe 编不出来(C1083 boost/any.hpp),install_vs2026_dbg 和网关 jar 也不存在。所以原 S3 的整栈自起,以及依赖它的 S4,全部受阻。
- 结论、修复设计:不影响。本项判定只依赖 S1、S2(以及有环境时的 S6)。

【Java 仓库】
- HEAD 从 d942f74 前进到 52cccf2,相关文件里只有 PARITY.md 变了(+5 行)。
- deploy/compose/infra.yaml:62、:64 原位;game-discovery 与 pom.xml 未变。java_parity 的结论不变。

**失效的说法**:

按原报告各节列出失效的说法与更正。

【confidence】
- 原文:「工作区在调查与复核期间持续被另一会话改动(k8s_deploy.ps1 +431、契约测试 +301),本报告的分批以定稿时的 git status 为准」。
- 已失效:那批改动已随 e4ff1d165 入库。现在主工作区只剩 third_party/librdkafka、third_party/ue5navmesh 两个子模块的脏标记。
- 落码前仍要看一次 git status:可能有多会话并行,这一条保留。

【claim_evidence】
1. 「行号以 HEAD 0aa1027bc 为准;工作区那份多出 431 行,按函数名定位」:改为以 2f897bd07 为准,工作区与 HEAD 一致(k8s_deploy.ps1 没有未提交改动)。
2. 一.4 `HEAD:5760 $infraManifests` → :6448。
3. 一.5:
   - `HEAD:5843-5849` → :6531-6537(Replace 在 :6537);
   - `HEAD:5875` → :6563;
   - `HEAD:1250` → :1682。
4. 一.6:
   - friend 的创建点 `:152`、`:164` → servicecontext.go:165、:177;
   - `Mode: redis HEAD:3537-3538` → :4100-4101。
5. 一.12 `zoneredis.max_connections: 100 HEAD:1872` → :2304(zoneredis 段 :2298-2305)。
6. 一.13 补一条新事实:K8s staging / prod 缺省开限流。依据是 -GatewayRateLimit auto(:242)和 :349-363。所以 gateway 每副本 2 条(dev 1 条)。
7. 一.14:
   - data-service 两个池 `HEAD:3488-3503` → :4037-4052;
   - Agones 上界 `HEAD:2666` → :3098。
   - 参数行 :24、:76、:80、:84-90、:88、:135-136 不变。
8. 一.15 失效:原文是「login…shared 是 go 1.24.5;db…trade 是 go 1.26.5;golang:1.26.5-alpine」。
   - 更正:全部 go.mod 为 go 1.27.2;Dockerfile.go-svc:64 是 golang:1.27.2-alpine3.24。
9. 三:
   - 「Go 1.25 起的容器感知缺省值只对 go.mod 为 1.25 以上的模块生效」:前提已对全部模块满足。
   - 「两者都不生效时 G 等于节点核数」:现在只在 GODEBUG 关掉它、且 automaxprocs 也不生效时才出现,可能性下降,仍待 S6 实测。
   - `deploy/k8s/README.md HEAD:1021` → :1118-1119。
10. 二(总账):
   - 数字不需要改档。
   - 口径更正:G=2 的 B 档成为预期基线。staging / prod 每 zone 再加 2 条(gateway 限流连接),B 档变为约 252 条/zone,越线 zone 数仍约 39。

【handoff_corrections】
1. 第 1 条:
   - `§2 的 k8s_deploy.ps1:5746 现为 HEAD:5760` → 现为 :6448;
   - `:1862-1872 现为 HEAD:1866-1873` → 现为 :2298-2305;
   - `:452-453 不变(HEAD:452)` → 现为 :502-503;
   - 「工作区正被另一会话改动(+431)」删除。
2. 第 5 条 `HEAD:3488-3503` → :4037-4052。
3. 第 6 条 GOMAXPROCS 一段:补一句「Go 1.27.2 下预期 G=2,未实测」。
4. 第 13 条 `HEAD:1250、:5875` → :1682、:6563。
5. 第 14 条整条失效:k8s_deploy_contract.tests.ps1、deploy/k8s/README.md、docs/PROGRESS.md 的他人改动已入库(e4ff1d165 等)。

【entry_point_inventory】
1. Go 创建点:friend `:152、:164` → :165、:177。
2. 客户端地址:
   - C++ :1866-1873 → :2298-2305;
   - data-service :3488-3503 → :4037-4052;
   - gateway :4949-4952 → :5623-5626。gateway 段现在带 ${gatewayRedisPasswordLine}。
3. 部署脚本各出处:

   | 内容 | 原行号 | 现行号 |
   |---|---|---|
   | Apply-Infra | :5744 | :6432 |
   | $infraManifests | :5760 | :6448 |
   | redis 分支 | :5803-5835 | :6491-6523 |
   | 读文件与替换 | :5843-5849 | :6531-6537 |
   | 占位检查与 apply | :5870-5876 | :6558-6564 |
   | 等待 | :5893 | :6581 |
   | Wait-ForStatefulSetReady | :2935-2956 | :3367-3388(rollout status 在 :3378) |
   | 触发条件 | :1250 | :1682 |
   | 调用点 | :5962、:5989 | :6658、:6685 |
   | Get-LiveStatefulSetReplicas | :1085 | :1497 |
   | Get-KafkaTopologyChangeVerdict | :1116 | :1528 |
   | Assert-KafkaTopologyChangeIsSafe | :1130 | :1542 |

4. 「在途、未提交、他人的:工作区 :934 起的 MySQL 总账…HEAD 里没有这些函数」整条失效,函数已在 HEAD,行号见 head_delta A.1。
5. 测试:「该文件工作区现有他人未提交 +301 行(:1448 起),本项不能往里加用例」失效。用例已入库(:1455-1690),文件干净。「对 redis 清单没有任何断言」仍成立。
6. CI:触发路径新增 deploy/k8s/*.yaml、deploy/k8s/*.json、gateway application.yaml;仍不含 docker-compose.yml。
7. 告警:「mysql-connection-alerts.yaml 是他人未跟踪文件」失效,已入库。
8. 文档:「README 没有 Redis 小节(现有他人未提交改动)」→ README 已入库,有 MySQL 总账小节(:656),仍没有 Redis 小节。deploy/k8s/AGENTS.md:24 仍写「redis/kafka/mysql are still single-replica Deployments」,未改,仍过时。
9. 存盘交接 :243 → :244。

【codex_measurement】
1. 工作目录:「W = Codex 的 f452 worktree」失效,f452 不许动。改为新建 worktree 检出 2f897bd07。
2. S0:
   - 「git checkout --detach 0aa1027bc」与「73b852f32 到 0aa1027bc 之间 cpp 无变化,go 改了…」失效,删除。
   - 「-CheckOnly 报缺 exe 时停下」改为已知事实:bin/ 只有 README.md、etc、logs;install_vs2026_dbg 不存在;java/gateway_node/target 没有 jar。
3. S1:
   - `git grep … 0aa1027bc`、`git show 0aa1027bc:` → 2f897bd07;
   - `HEAD:5843-5849、:5870-5876` → :6531-6537、:6558-6564;
   - `HEAD:5814-5816` → :6502-6504(DryRun 打印口令的注释在 :6503)。
4. S2b:
   - 「本机 exe 记版本 bin/go_services/login.exe」:主工作区和新 W 里都没有这个 exe,要先构建,否则记「未取得」;
   - 补 GOTOOLCHAIN=local;
   - 期望中补「Go 1.27.2 容器感知」。
5. S3:原「没有栈就用 start_game.ps1 一键启动」现在受阻:没有 C++ exe(C1083 boost/any.hpp),没有 install_vs2026_dbg(start_game.ps1:551),没有网关 jar(:370-371)。只剩「已有在跑的栈」可以采样。
6. S4、S5 的前提随 S3 变化。S5 的 go-svc-stop / start-exe 要在该栈自己的目录执行(pid 文件按目录存放,go_services.ps1:85-86)。
7. S6:期望从「G 为 1 或 2」改为「预期 2;automaxprocs 生效时可能 1」。

【fix_design】
1. 一·分批:
   - 「此刻带着他人未提交改动的文件…」失效;
   - 第二批「阻塞于 G2b 提交」解除;
   - 「Get-ManifestDeploymentReplicas、Get-MysqlConnectionPlan 只存在于未提交工作区(:954、:1004)」→ 已在 HEAD :1024、:1074。
2. 路径甲/乙:
   - `HEAD:1250、:5875` → :1682、:6563;
   - 路径乙不再被 G2b 卡住,可以与重启闸、总账同批。
3. 三.3 `HEAD:5760` → :6448。
4. 三.5:
   - 「新开文件的理由是契约测试有他人未提交改动」失效。新文件仍可用,理由改为「第一批独立、可单独跑红绿」;
   - `HEAD:5843-5849` → :6531-6537。
5. 三.6 「PROGRESS 有他人未提交的追加」失效。只追加的规矩照旧。
6. 五.1:报错前缀写死的位置「工作区 :1009、:1013」→ HEAD :1078、:1082。
7. 五.2:
   - `HEAD:1085` → :1497;
   - `:1116` → :1528;
   - `:5870-5873` → :6558-6561;
   - `HEAD:3000` → :3432;
   - `:2946` → :3378;
   - `:5893` → :6581。
8. 五.4:
   - AST 抽取 `HEAD:337、:1288` → :390、:1341,MySQL 总账另在 :1459;
   - 透传用例 `HEAD:1170` → :1251(helper :1223)。
9. 六.1:告警模板改为优先照 deploy/k8s/mysql-connection-alerts.yaml。
10. 八.1:「区分本项与他人在途改动」的前提已弱化,基线步骤保留。
11. 八.3:临时 worktree `p0e-red-0aa1027bc` / `0aa1027bc` → 检出 2f897bd07。

【risks_open_questions】
1. 风险 1:「在途改动仍在变化,第二批排在 G2b 提交之后」失效。G2b 已入库;只保留「落码前看 git status,不 git add -A」。
2. 风险 2:
   - `HEAD:1250、:5875` → :1682、:6563;
   - 存盘交接 `:90` → :91。
3. 风险 3:「GOMAXPROCS 确为 1 或 2」→ 预期 2(Go 1.27.2),未实测。
4. 与另两份交接的关系:
   - 「G2b…复用它的 Get-ManifestDeploymentReplicas」现在可直接复用;
   - 存盘交接 `:90、:225` → :91、:226。
5. 「Go 1.25 容器感知…是否只对 go.mod 1.25 以上生效」的后半句已无意义,全部 1.27.2。
6. 邻接发现:
   - 「Java gateway 的 K8s ConfigMap 没有 Redis 密码(HEAD:4949-4952)」已修复,删除;
   - `zoneredis.max_connections: 100 (HEAD:1872)` → :2304;
   - `HEAD:1002 gateway 指向 java-svc` → :1414。

【java_parity】
- Java 仓库 HEAD d942f74 → 52cccf2,只有 PARITY.md 变了。
- 「状态词取值说明 PARITY.md:23」→ :25。
- 其余证据原位,结论不变。

【changes_from_review】
- 这一节是历史复核记录,其中的 HEAD:xxxx 行号全部按 0aa1027bc。不逐条回改,以本复核的行号为准。

【编译阻塞原因】
- 原报告任何一节都没有写「110 个错误 / 装备表缺失 / 子模块全未初始化」。只在 S0 写了「-CheckOnly 报缺 exe 就停下,本项不为采样编译 C++」。所以没有要撤回的错误原因。
- 需要补上的阻塞事实:
  - battle.vcxproj 在 third_party/muduo/muduo/net/EventLoop.h:18 报 C1083,找不到 boost/any.hpp(Boost 是模块化 include 布局,见 docs/PROGRESS.md:7637);gate、scene 也经 muduo 引入,推断同样受阻;
  - third_party/grpc/install_vs2026_dbg 不存在;
  - cppcodec、fmt、muduo-linux、recastnavigation、xxhash 五个子模块未初始化,是否被节点编译用到,需 Codex 先确认;
  - cpp/generated/table/code/equipaffixpool_table.h 已存在,「装备表缺失」不成立,也不要再引用。

### 受阻步骤与先写的测试

【S3b 自起整栈空闲采样】
四个阻塞原因:
1. 没有 gate / scene / battle 的 exe。
   - Codex 2026-10-14 串行 /m:1 编译 cpp/nodes/battle/battle.vcxproj(Debug|x64)时,third_party/muduo/muduo/net/EventLoop.h:18 报 C1083,找不到 boost/any.hpp。原因是 Boost 采用模块化 include 布局,而工程只包含根目录(docs/PROGRESS.md:7637)。
   - gate、scene 同样经 muduo 引入这个头文件,推断同样受阻。
2. third_party/grpc/install_vs2026_dbg 不存在。
   - start_game.ps1:551 把它的 bin 放进 PATH。
   - 它由 `dev_tools.ps1 -Command third-party-grpc-build` 产出(dev_tools.ps1:1417 → tools/scripts/third_party/build_grpc.ps1:486)。
3. 没有网关 jar。
   - start_game.ps1:370-371 要求 java/gateway_node/target/gateway-node-*.jar,:366-369 要求 Java ≥ 23。
4. cppcodec、fmt、muduo-linux、recastnavigation、xxhash 五个子模块未初始化,是否被节点编译用到,需 Codex 先确认。

解除条件:三个 exe、install_vs2026_dbg、网关 jar 都齐备,并且 `start_game.ps1 -CheckOnly` 通过。

由谁解除:
- 1、2、4 属于修构建环境(改 Boost include 布局或工程属性;编译 gRPC 很耗时;初始化子模块可能要下载)。须负责人授权,Codex 执行。
- 3 是 Codex 常规的 mvn 打包,不改环境,但耗时。是否为本项去做,由负责人定。

【S4 轻负载与不回落】
- 阻塞原因:依赖 S3b。机器人要走网关 8081 → gate → scene(robot_smoke.yaml:3)。另外,机器人的认证方式是否匹配当前栈,需 Codex 先确认。
- 解除条件:S3b 解除;或者本机已有带网关和 gate 的现成栈,这时转为 S3a 条件下执行。
- 由谁解除:同 S3b。

【S5 到线时 Go 客户端的表现】
- 阻塞原因:需要一个在跑的本机开发栈(S3a 或 S3b);并且有破坏性,期间本机登录与存盘失败。
- 解除条件:有栈,并且负责人同意。
- 由谁解除:负责人拍板。

【S6 K8s 实机只读核对】(只在没有 K8s 环境时算受阻)
- 解除条件:已有环境。新建 kind 或拉镜像要负责人同意;而且 kind 回答不了生产运行时的情况。
- 生产或预发集群上 redis-0 的 nofile 硬限和各 Go 服务的 GOMAXPROCS,只能由负责人或运维在目标集群上只读取一次。

【对判定的影响】
S3b、S4、S5 只用来校准总账系数,看到线时的表现,不参与「成立与否」。建议不为本项单独解除构建阻塞:这几步延后,等其他项修好 C++ 构建环境后顺带补测。

**实测前要先写的代码**:

S0~S6 的实测步骤都不需要先写代码。

唯一建议先写的是回归测试(AGENTS §11.4:缺陷修复原则上先写能稳定复现问题的回归测试,红绿由 Codex 跑)。

【文件与位置】
- 路径:D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/redis_maxclients_contract.tests.ps1,新建。

【内容要点】
骨架与同目录其他用例相同:
- 开头 dot-source tools/scripts/tests/lib/test_harness.ps1(文件未变,只有 Test-Case、Assert-*、Complete-TestRun,没有按名字筛选的入口);
- 结尾 `exit (Complete-TestRun -SuiteName …)`;
- 路径一律用 Join-Path,保证 Linux pwsh 可跑;
- 每个用例自己读文件、自己查找,查不到只让该条变红。

用例:
- T1:deploy/k8s/manifests/infra/redis.yaml 里 `MAXCLIENTS=<正整数>` 恰好出现 1 次。
- T2:每一行 `exec redis-server` 都带 `--maxclients "$MAXCLIENTS"`,且这样的行至少 2 行。
- T3:启动薄壳里有 `redis fd limit:` 这一行打印,部署后核对靠它。
- T4:只在负责人决定 docker-compose 同改时才加。断言 deploy/docker-compose.yml 的 redis command 带 `--maxclients`,数值与 T1 读到的相同。

可以直接读清单的理由:Apply-Infra 对 redis.yaml 只替换 __INFRA_NAMESPACE__(k8s_deploy.ps1:6537),渲染结果就是文件本身。

修复前(2f897bd07)的期望:T1~T3 以「查不到 MAXCLIENTS」一类文本失败,退出码非 0。这就是 R0。

【工程文件】
- 不需要同步 CMakeLists、vcxproj、sln。
- CI 的 .github/workflows/deploy-config-tests.yml 会自动跑 tools/scripts/tests 一级目录下所有不以下划线开头的 .ps1。
- 注意:CI 触发路径不含 deploy/docker-compose.yml。T4 只改 compose 时不会重跑 CI。

【为什么归入「实测」而不是「修复」】
- 它不改任何行为,只把「redis.yaml 没有显式 maxclients、也没有 fd 打印」这条静态缺陷变成机器可复现的红。效果与 S1 相同,但可以在 CI 里长期钉住。
- 它的内容不依赖实测结果:只断言存在一个正整数,不钉 N 的取值。所以可以在 S2、S6 之前写好、先跑红。
- 落 redis.yaml 修复时,它再转绿。

【放哪个文件】
- k8s_deploy_contract.tests.ps1 现在已经干净入库,原来「不能往里加」的理由失效了。
- 仍建议第一批的静态断言放新文件:第一批可以独立跑红绿、独立回滚,也避开多会话并行时改同一个 1897 行的文件。
- 第二批的总账、重启闸、透传用例,照 :1455 起的 MySQL 总账形状加进 k8s_deploy_contract.tests.ps1。

【交付说明】没有 Codex 结果之前,如实写「未运行,待 Codex 验证」。

### 待拍板

1. 本机没有 redis:7.2 时,是否同意拉取(S2 用)。建议同意:它是 S2 的唯一前置,而 S2.1 决定成立与否。拉取前让 Codex 报告镜像大小(取法需 Codex 先确认)。

2. C++ 构建环境要不要为本项解除。涉及三件事:Boost 模块化 include 布局导致的 C1083;用 third-party-grpc-build 产出 install_vs2026_dbg;初始化五个未初始化的子模块(可能要下载)。
   - 建议不为本项单独授权。S3b、S4 只校准系数,不影响判定。
   - 等其他项要修构建环境时,一并授权 Codex,再顺带补测 S3b、S4。

3. 是否同意 S3b、S4 延后。如果有现成栈,S5 是否也延后。建议同意。本项判定以 S1、S2.1(以及有环境时的 S6)为准。

4. S5(有破坏性,期间本机登录与存盘失败)做不做,在谁的栈上做,什么时段做。建议:只在负责人自己的本机栈、明确的时段做;没有现成栈就延后。

5. S6:
   - 现在有没有在跑的 K8s 环境。
   - 建议不为本项新建 kind:要拉镜像,而且回答不了生产运行时的情况。
   - 生产或预发集群上 redis-0 的 nofile 软硬限和各 Go 服务的 GOMAXPROCS,由谁只读取一次。建议由负责人或运维取。

6. 是否先让 Claude 写 redis_maxclients_contract.tests.ps1,由 Codex 在 2f897bd07 上跑红(R0)。建议是(§11.4)。文件放新文件(建议),第二批的用例进 k8s_deploy_contract.tests.ps1。

7. 修复设计里原有的拍板项,仍待定:
   - (a) N 的取值。建议 40000。G=2 成为基线后,B 档在 80% 水位能承载约 127 个 zone,仍盖得住约 100 个 zone 的目标。
   - (b) 路径甲还是路径乙。G2b 已入库,路径乙不再被卡住。建议:有需要保护的在线环境时选乙,redis.yaml 与重启闸、总账同批落;没有就选甲,apply 窗口由负责人定。
   - (c) 已有环境是否先在线 CONFIG SET,由谁执行。必须在到线之前做。
   - (d) docker-compose 是否同批改。改了要手动重建本机 redis 才生效。
   - (e) redis-ha.yaml 文件头是否加一行接入前置注释(不改行为)。
   - (f) 重启闸的语义与 G4a 的 -ConfirmRedisCoreRestart 合成一套,由谁做。
   - (g) 是否上 redis_exporter(与 G4a §13 第 6 条一起定)。第三批的告警照已入库的 deploy/k8s/mysql-connection-alerts.yaml 写。
   - (h) Redis 总账超限是只告警还是升级为门禁。MySQL 那份现在是只告警(k8s_deploy.ps1:6651)。建议两份一起定。
   - (i) tcp-keepalive。建议本批不动。

### 原报告:证据链

【结论】partially_confirmed。
- 成立(静态证据链完整):四处 Redis 服务端都没有显式 maxclients;Go / C++ / Java 客户端都没有显式连接池上限,也不设连接名。按池上限口径重算,每 zone 130~350 条,约 28~76 个 zone 越过 10000,与交接的「每 zone 100~250、约 40~100 个 zone」同量级。
- 不成立或要加条件(四处):「早于命令量上限」;「C++ 每线程一条」;「与规模无关」;隐含前提「今天生效的上限就是 10000」(取决于容器 nofile 硬限,未实测,可能更低)。
- 两份复核都同意结论成立。复核查到的三件事让缺陷更硬:连接数不回落;到线后客户端多半不暴露原因;到线后管理命令也进不去。
- 行号以 HEAD 0aa1027bc 为准。k8s_deploy.ps1 的行号是 HEAD 版本;工作区那份因他人在途改动多出 431 行,落码时按函数名重新定位。

一、代码事实(逐行读过)

1. 共享 Redis 服务端只带 appendonly 与可选 requirepass。deploy/k8s/manifests/infra/redis.yaml:94-97:
   `if [ -n "$REDIS_PASSWORD" ]; then`
   `exec redis-server --appendonly yes --requirepass "$REDIS_PASSWORD"`
   `exec redis-server --appendonly yes`
   同文件没有 maxmemory、timeout、tcp-keepalive、tcp-backlog,没有任何 fd 语句,没有 updateStrategy;resources 只有 requests(:140-143);镜像 redis:7.2(:83)。
2. `git grep -i maxclients HEAD` 在 deploy、tools、go、cpp、java、bin、proto 下 0 命中;命中只在 docs 与 robot/vendor 的 go-redis 源码。
3. 另外三处服务端同样没设:
   - redis-ha.yaml:250-255 `set -- redis-server \` / `--appendonly yes \` …;
   - redis-match-cluster.yaml:18-28 的 redis.conf;
   - deploy/docker-compose.yml:233-238(`redis-server --appendonly yes --maxmemory 8gb --maxmemory-policy noeviction --notify-keyspace-events Eg`),镜像是 `redis:latest`(:218),不是 K8s 的 7.2。
   - java/springboot_satoken_auth_starter/docker-compose.yml:19 另有一个 `redis:7`(auth starter 自带,不是共享库)。
4. redis-ha.yaml 没有 apply 路径:k8s_deploy.ps1 HEAD:5760 `$infraManifests = @("etcd.yaml", "redis.yaml", "redis-match-cluster.yaml", "kafka.yaml", "mysql.yaml")`;`RedisReplicas` 只出现在 redis-ha.yaml:3 的注释;`ConfirmRedisCoreRestart`、`RedisSplit` 在 tools/scripts 下 0 命中(G4a 只有设计稿)。
5. redis.yaml 的渲染结果就是文件本身,而且每次都 apply:HEAD:5843-5849 只做 `$manifestContent.Replace("__INFRA_NAMESPACE__", $InfraNamespace)`;HEAD:5875 `Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $manifestContent`。触发条件 HEAD:1250 `$deploysInfra = ($Command -eq "infra-up") -or ($Command -eq "all-up" -and -not $SkipInfra)`。所以改 redis.yaml 的 Pod 模板后,下一次 infra-up 就把新模板送进集群。
6. Go 客户端没有任何池参数,也不设连接名。`git grep -E "PoolSize|MinIdleConns|MaxActiveConns|ClientName"` 在 go/ 的非测试代码里与 Redis 相关的 0 命中。创建点(非测试、非 cmd):
   - go-redis 直连:login `internal/svc/servicecontext.go:111`;db `:24`、`:37`;player_locator `:28`;trade `internal/svc/assetchannel.go:102`;guild `:57`、`:72`、`:180`;data_service `internal/routing/router.go:60`(DevRedis)、`:71`(集群客户端,配了 DevRedis 就短路)。
   - go-zero:scene_manager `:103`;match `:202`、`:206`;chat `:74`、`:84`;friend `:152`、`:164`;data_service `router.go:48`、`route_publish.go:398`。
   - Pub/Sub 专用连接只有一处:`go/login/internal/dispatcher/task_result_dispatcher.go:227` `psub := d.rc.Subscribe(ctx, TaskResultNotifyChannel)`。
   - 阻塞命令只有 WAITAOF(`route_publish.go:301`、`:404`),仅 db_first 模式启用;K8s 模板是 `Mode: redis`(HEAD:3537-3538)。BLPOP 一类 0 命中。
   - client_rpc_router、go/team、go/battle 不建 Redis 客户端(grep 0 命中)。
7. go-redis 在仓库里有三个版本,源码只有 v9.18.0 一份(robot/vendor):
   - v9.16.0:login、player_locator、guild、shared(各自 go.mod);
   - v9.17.3:scene_manager、match、chat、friend、client_rpc_router(indirect)、trade(直接依赖);
   - v9.18.0:db、data_service、robot。
8. v9.18.0 的连接池(robot/vendor/github.com/redis/go-redis/v9):
   - 缺省池 = 10 × GOMAXPROCS:options.go:341-342 `if opt.PoolSize == 0 {` / `opt.PoolSize = 10 * runtime.GOMAXPROCS(0)`;
   - 池内连接数由信号量封顶:internal/pool/pool.go:343 `semaphore: internal.NewFastSemaphore(opt.PoolSize),`、:707 `if err = p.waitTurn(ctx); err != nil {`;
   - **没有后台回收**:空闲超时只在取连接时检查,pool.go:746-747 `if !p.isHealthyConn(cn, nowNs) {` / `_ = p.CloseConn(cn)`,判定在 :1342-1343;池里的 goroutine 只有 :428(补 MinIdle)、:621、:857(拨号),`NewTicker|Reap` 0 命中;
   - 缺省后进先出:pool.go:991-993 `idx := len(p.idleConns) - 1` / `cn = p.idleConns[idx]`;
   - 归还时不裁剪:pool.go:1091 `if p.cfg.MaxIdleConns == 0 || p.idleConnsLen.Load() < p.cfg.MaxIdleConns {`。
   结论:连接数停在历史并发峰值,上限是 PoolSize;低并发下栈底的旧连接永远不会被取出检查。ConnMaxIdleTime 缺省 30 分钟(options.go:378-379)不会让连接数自己回落。
9. v9.18.0 的握手会吞掉到线错误文本。redis.go:504-525:
   `if initErr = conn.Hello(ctx, c.opt.Protocol, username, password, c.opt.ClientName).Err(); initErr == nil {`
   `} else if !isRedisError(initErr) {` … `} else if password != "" {`
   `return fmt.Errorf("failed to authenticate: %w", initErr)`
   服务端对 HELLO 回的任何 Redis 错误都被当成「不支持 HELLO」继续往下走:有密码时接着发 AUTH,失败文本是 failed to authenticate;无密码时落到后续命令的网络错误。
10. 到线后会重试并重新拨号。error.go:85-87 `if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {` / `return true`;:109-111 `if proto.IsMaxClientsError(err) {` / `return true`。
11. login 与 db 关掉了 CLIENT SETINFO:login servicecontext.go:115、db :28、:41 `DisableIndentity: true, // suppress CLIENT SETINFO on Redis < 7.2`。它们的连接 lib-name 为空。
12. C++ 每进程只有 1 条:
    - 唯一建连点 cpp/libs/engine/core/node/system/node/node.cpp:557 `tlsRedis.Connect(eventLoop, zoneRedisAddress);`;
    - 重连是先销毁再新建:cpp/libs/engine/thread_context/redis_manager.cpp:147、:154;每 3 秒一次(:122 `static constexpr double kReconnectIntervalSec = 3.0;`);
    - 断连日志不带原因:redis_manager.cpp:115 `LOG_ERROR << "Redis disconnected (status=" << status << "), scheduling reconnect to "`;
    - `zoneredis.max_connections: 100`(HEAD:1872)是死配置:cpp/libs/engine/config/config.cpp:283-301 只读 host、port、password、db。
13. Java gateway 用 Lettuce:`StringRedisTemplate`(ZoneHealthProbeService.java:31);限流打开时再加一条专用连接(RateLimitConfig.java:50、:61 `@ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")`)。application.yaml 没有 lettuce 池配置。
14. K8s 部署形态:
    - 每 zone:login、db、data-service、player-locator、scene-manager 各 `replicas: 2`(deploy/k8s/manifests/go-svc/*.yaml);gate 2(HEAD:76)、scene 4(HEAD:88);Java gateway 2(java-svc/gateway.yaml:24)。
    - 全局池:match、chat、friend、trade 各 2;battle 1(HEAD:80)。guild 没有 K8s 清单。
    - CPU limit:Go 服务都是 500m,login 是 `"1"`(login.yaml:89)。
    - data-service 在 K8s 上两个池都指向共享 Redis(HEAD:3488-3503 的 MappingRedis 与 DevRedis)。
    - scene 进程数有三条路径:`-SceneReplicas`;拆分池 `-SceneWorldReplicas`、`-SceneInstanceReplicas`(HEAD:89-90,进入拆分模式后 legacy scene 被忽略,HEAD:84-87 注释);Agones 自动伸缩上界 `-AgonesMaxReplicas`(HEAD:135-136、:2666)。
15. Go 版本与工具链:login、player_locator、scene_manager、match、chat、client_rpc_router、shared 的 go.mod 是 `go 1.24.5`;db、data_service、friend、guild、trade 是 `go 1.26.5`;镜像用 `golang:1.26.5-alpine` 编译(deploy/k8s/Dockerfile.go-svc:64)。各 go.mod 的 `go.uber.org/automaxprocs v1.6.0` 都是 `// indirect`,仓库内没有任何 .go 文件直接引用它。
16. 启动即探活:login(servicecontext.go:118-119)、player_locator(:34-35)、guild(:63-64、:77-78)Ping 失败直接 panic;db(:31)、trade(assetchannel.go:109)也在启动时 Ping,失败处理未逐行核实。

二、按代码事实重算的总账(G = 该 Pod 的 GOMAXPROCS)

每副本上限:login 10G+1;db 10G;data-service 20G;player-locator 10G;scene-manager 10G;match、chat、friend、trade 各 10G;gate、scene、battle 各 1;Java gateway 1(限流开时 2)。
公式:每 zone 上限 = 2×(10G+1) + 2×10G + 2×20G + 2×10G + 2×10G + gate 台数 + scene 进程数 + 2;全局池 = 4×2×10G + 1。

| 口径 | 每 zone | 全局池 | Z=100 合计 | Z=220 合计 | 越过 10000 的 zone 数 |
|---|---|---|---|---|---|
| A 默认副本,G=1 | 130 | 81 | 13,081 | 28,681 | 约 76 |
| B 默认副本,G=2 | 250 | 161 | 25,161 | 55,161 | 约 39 |
| C 目标形态(login 4 核,gate 5~22,scene 9~44),其余 G=1 | 198~250 | 81 | 19,881~25,081 | 43,641~55,081 | 约 40~50 |
| D 目标形态,其余 G=2 | 298~350 | 161 | 29,961~35,161 | 65,721~77,161 | 约 28~33 |
| 最坏:G 没被收住,按 16 核节点 | 1,930 | 1,281 | — | — | 约 5 |

- 目标形态的出处:交接 :181「现有 200 副本 × 4 核」、:183「约 600 台」、:157「Z≈220」;capacity-model-hok-scale.md:47-48 的 c_gate「4.5k~20k」、c_scene「2.25k~11.25k」。
- 与容量模型对数:capacity-model-hok-scale.md:270 的连接数一行是 S1 2.5k~6.2k、S2 1.2 万~3 万、S3 2.5 万~6 万,折合每 zone 约 250~280,落在 B、C 两档。
- 规划档(在途 MySQL 总账的 4 个 zone):A 601,B 1,161。
- 稳态口径用池上限,不用冷启动地板。地板(每 zone 约 48、全局约 51)只是进程刚起来时的瞬时值:只要出现过一次并发峰值,连接数就停在峰值(一.8)。go/db 每 zone 80 路子分片工人(存盘交接 :243),负载下池必然打满。
- 滚动发布:每个 Deployment 临时多出 1~2 个 Pod(db.yaml:33 的注释按多 2 个计)。

三、推断与外部常识(未核实,全部列入实测)

- Redis 缺省 maxclients=10000;启动时需要 maxclients+32 个 fd,软限不够会抬到硬限,硬限也不够就下调并打日志。仓库内 gate-connection-admission-control.md:105 有同一说法,仍属外部常识。
- 不少 containerd 配置的 nofile 只给 1024(deploy/k8s/README.md HEAD:1021、kafka.yaml:157-158 的仓库内说法)。若硬限也是 1024,今天生效的上限约 992,规划档 4 个 zone 就到线。
- StatefulSet 缺省 updateStrategy 是 RollingUpdate,模板变了就重建 redis-0。
- go-zero 的 redis 封装固定 MinIdleConns=8、不设 PoolSize、Must 版本启动失败即退出。佐证只有仓库内注释:go/friend/internal/data/session_reader_test.go:65、go/data_service/cmd/route_tool/main.go:314-315。
- K8s 上的 GOMAXPROCS:automaxprocs 是否由 go-zero 引入并生效;Go 1.25 起的容器感知缺省值只对 go.mod 为 1.25 以上的模块生效,且下限可能是 2。两者都不生效时 G 等于节点核数。
- v9.16.0 与 v9.17.3 的池封顶、无后台回收、握手行为与 v9.18.0 相同(推断)。
- 到线时 Redis 先回一行 `-ERR max number of clients reached` 再关连接,不给管理连接留名额。所以在线调大必须在到线之前做。
- 到线时 Go 调用方多半看到 EOF、连接重置或 failed to authenticate,而不是 max number of clients(由一.9 推断)。
- 到线时 C++ 每 3 秒一轮重连且日志看不出原因;若重连后有待发命令,错误文本可能经 `Redis Save error for key`(redis_client.h:736)打出来(hiredis 行为属外部常识)。
- 到线时就绪探针失败:redis.yaml:117、:126 只认 `PONG|NOAUTH*`,连续 3 次失败后 Service 摘掉唯一端点。
- redis-ha 的倒挂:haproxy `maxconn 20000`(redis-ha.yaml:530)× 2 个代理(:808,`maxSurge: 1` 在 :812)大于节点缺省上限;到线时健康检查(:625-629,期望 `+PONG`)失败,`on-marked-down shutdown-sessions` 踢掉全部会话。haproxy frontend 的缺省 maxconn 是否等于 global 未核实。
- Lettuce 缺省是共享连接、不是池。

四、交接结论里不成立或要加条件的部分

1. 「早于命令量上限」与交接 §5 :171 矛盾(「命令约 27 万在线(约 3 个满载 zone)」)。连接数主要随 zone 数与副本数增长;只有「zone 多而轻载」的形态才先于命令量撞线。
2. 「C++ 每线程一条」不对,是每进程 1 条(一.12)。
3. 「与规模无关」不准确:主要正比于 zone 数;scene、gate 台数与 CPU limit 还随区内人数放大。
4. 「默认 10000」是有条件的:目标运行时的 nofile 硬限不低于 10032 才成立,本机无法核实。

### 原报告:对交接的更正

1. 行号:
   - `redis.yaml:95-97` 仍准确(两条 exec 在 :95、:97,if 在 :94)。
   - §2 的 `k8s_deploy.ps1:5746` 现为 HEAD:5760(`$infraManifests`)。
   - §3 第一行引用的 `:1862-1872`(C++ zoneredis)现为 HEAD:1866-1873;`:452-453` 不变(HEAD:452)。
   - 工作区的 k8s_deploy.ps1 正被另一会话改动(现为 +431 行),行号一律按函数名重新定位。
2. 「C++ 每线程一条」应为每进程 1 条。`tlsRedis` 是 thread_local,但只有 node.cpp:557 一处 Connect。capacity-model-hok-scale.md:270 写的「C++ 每进程 1 条」是对的;shared-redis-split-by-purpose.md:23、:41 写的「每个线程一条连接」同样不准。
3. 「go-zero 每客户端至少常驻 8 条」只适用于 go-zero 客户端(scene_manager、match、chat、friend、data_service 的 MappingRedis),而且「8」是外部常识。login、db、player_locator、trade、guild 用 go-redis 直连,启动 Ping 后至少留 1 条。
4. 说漏:**连接数不回落**。go-redis v9 没有后台回收(pool.go:746、:991、:1091),连接数停在历史并发峰值。总账必须按池上限口径,不能按常驻下限。
5. 说漏的连接来源:
   - login 每副本 1 条 Pub/Sub 专用连接(task_result_dispatcher.go:227);
   - data_service 在 K8s 上有两个池同时指向共享 Redis(router.go:48、:60;HEAD:3488-3503);
   - chat v1 不读共享库,但仍建了 go-zero 池(chat servicecontext.go:74);
   - 探针与运维工具各占瞬时连接。
6. 说漏的放大因素:
   - 池上限 = 10 × GOMAXPROCS,与 CPU limit 隐式耦合。按交接自己的目标形态(login 4 核、gate 5~22 台、scene 9~44 个),每 zone 198~350 条,约 28~50 个 zone 到线。
   - K8s 上的 GOMAXPROCS 没有实测;两套收敛机制都不生效时,每 zone 上千条。
   - 到线后 go-redis 会重试并重新拨号(error.go:85-87、:109-111)。
7. 说漏:**到线后客户端多半不报「max number of clients」**。握手的 HELLO 把它当成「服务端不支持 HELLO」吞掉(redis.go:504-525);release 档可能表现为 failed to authenticate,容易与「C++ 不发 AUTH」那一项混淆。C++ 断连日志本来就不带原因(redis_manager.cpp:115)。唯一可靠的信号在服务端(rejected_connections)。
8. 「早于命令量上限」与 §5 :171 冲突,应改为「在 zone 多而轻载的形态下先于命令量」。
9. 归类应从「与规模无关的缺陷」改为「随 zone 数线性增长的配置缺口」。按规划档 4 个 zone 只有 601~1,161 条,前提是生效上限确为 10000。
10. 「默认 10000」「nofile 不够自动下调」是外部常识,交接没有标注;「默认 10000」还依赖容器 nofile 硬限。
11. 「由 Codex 用 CLIENT LIST 实测」不够用:
    - 全仓没有任何客户端设置连接名,name 全空;login 与 db 连 lib-name 也是空的(DisableIndentity);
    - 本机 Docker Desktop 做了地址转换,addr 反查不到宿主进程。要在客户端一侧按进程数连接。
    - 少量机器人测到的只是冷启动地板,不能用来判断成立与否。
12. redis-ha.yaml 的倒挂没有提(haproxy maxconn 20000 × 代理副本数,大于节点缺省上限)。接入 redis-ha 之前必须一起修。
13. 修复不是「改一行配置」:redis.yaml 的 Pod 模板一变,下一次 infra-up 就会重建唯一的 Redis(HEAD:1250、:5875)。交接的「立即修」没有提这个上线约束。
14. §9 的他人在途清单已变化:`deploy/docker-compose.yml`(noeviction)与 G4a 设计稿已随 d5cbb8a8e 入库,当前工作区干净;`tools/scripts/tests/k8s_deploy_contract.tests.ps1`(+301 行)、`deploy/k8s/README.md`、`docs/PROGRESS.md` 现在带着他人未提交改动。以 git status 为准。

### 原报告:同类入口盘点

【代码:创建连接】
- C++:node.cpp:541-557(解析地址并 Connect,所有 C++ 节点类型共用);redis_manager.cpp:30-47(Connect)、:141-157(DoReconnect);cpp/libs/engine/muduo_windows/src/contrib/hiredis/Hiredis.cc:53(redisAsyncConnect)。测试里另有 cpp/tests/currency_test/currency_test.cpp:854 的同步连接。
- Go,go-redis 直连:login servicecontext.go:111、auth_init.go:68(SaToken,K8s 不配);db :24、:37(落点句柄缺省复用);player_locator :28;trade assetchannel.go:102;guild :57、:72、:180;data_service router.go:60、:71。
- Go,go-zero:scene_manager :103;match :202、:206(MatchRedis 是独立集群);chat :74、:84;friend :152、:164;data_service router.go:48、route_publish.go:398(WAITAOF 探针,仅 db_first)。
- Pub/Sub:login task_result_dispatcher.go:227。
- 阻塞命令:WAITAOF(route_publish.go:301、:404),仅 db_first;K8s 现为 `Mode: redis`(HEAD:3537-3538)。
- Java:gateway ZoneHealthProbeService.java:31、RateLimitConfig.java:50-62;springboot_satoken_auth_starter(没有 K8s 清单,java-svc/ 下只有 gateway.yaml)。
- 工具与一次性进程:go/data_service/cmd/route_tool/main.go:317;go/db/cmd/data_stress/main.go:85、cmd/verifier/main.go:132;go/guild/cmd/assetopfix/main.go:198;tools/merge_zone、tools/data_consistency_check(go-redis v9.7.3);robot(v9.18.0)。
- 版本:v9.16.0(login、player_locator、guild、shared)、v9.17.3(scene_manager、match、chat、friend、trade)、v9.18.0(db、data_service)。

【代码:读取 / 更新 / 销毁 / 恢复】
- 读取配置:cpp/libs/engine/config/config.cpp:283-301(不读 max_connections、timeout、retry_interval);Go 各服务 internal/config/config.go 的 Redis 段都没有池字段。
- 销毁:redis_manager.cpp:49-61(Shutdown)、:159-168(ResetConnection);Go 侧各 ServiceContext.Stop。
- 恢复:C++ 每 3 秒重连(redis_manager.cpp:120-130);go-redis 按需重拨,且把 EOF 与到线错误都当可重试(error.go:85-87、:109-111);go-zero 重建常驻空闲(外部常识);login 订阅出错后重订阅(task_result_dispatcher.go:210-219)。
- 启动即探活:login :118-119、player_locator :34-35、guild :63-64 失败即 panic;db :31、trade :109 也 Ping。到线时这些服务在 K8s 上表现为反复重启并持续抢名额。
- Pod 被强杀或节点失联不会发 FIN,服务端的死连接要等 tcp-keepalive(缺省 300 秒,外部常识)才释放,期间与新连接同时占名额。

【配置模板】
- 服务端参数:redis.yaml:90-97(命令)、:112-135(探针)、:136-143(资源);redis-ha.yaml:250-255(redis-start.sh)、:530(haproxy maxconn)、:546-547(timeout client/server 1h)、:625-629(健康检查)、:808、:812(代理副本与 maxSurge);redis-match-cluster.yaml:18-28;docker-compose.yml:217-238(共享,redis:latest)与 :258 起(集群节点,redis:7.2,profile redis-cluster);java/springboot_satoken_auth_starter/docker-compose.yml:19。
- 客户端地址(k8s_deploy.ps1 HEAD):C++ :1866-1873;data-service :3488-3503;其余各 Go 服务 ConfigMap 段与 gateway :4949-4952(gateway 没有密码,属 C++ AUTH 那一项的邻接问题)。
- 本地配置:bin/etc/game_config.yaml 的 zoneredis 段;go/*/etc/*.yaml 的 Redis 段。

【部署脚本】
- k8s_deploy.ps1 HEAD:Apply-Infra :5744;`$infraManifests` :5760;redis 分支 :5803-5835(密钥、删旧 Deployment);读文件与占位替换 :5843-5849;未替换占位检查与 apply :5870-5876;等待就绪 :5893(`Wait-ForStatefulSetReady`,:2935-2956,用 `kubectl rollout status`)。
- 触发条件:HEAD:1250(infra-up,或不带 -SkipInfra 的 all-up);调用点 :5962、:5989。
- 可复用的形状:Get-LiveStatefulSetReplicas :1085、Get-KafkaTopologyChangeVerdict :1116、Assert-KafkaTopologyChangeIsSafe :1130(读线上现状,加纯判定函数)。
- 包装入口逐个开关手写透传:tools/scripts/dev_tools.ps1:672、:870 `if ($AllowDisruptiveSwitch) { $args.AllowDisruptiveSwitch = $true }`;tools/scripts/k8s_image.ps1:421 同一写法。k8s_deploy.ps1 新增开关必须同步这两处。
- 在途、未提交、他人的:工作区 k8s_deploy.ps1:934 起的 MySQL 连接总账(Get-MysqlConnectionPlan :954、Get-ManifestDeploymentReplicas :1004、Get-MysqlConnectionLedgerInputs :1029、Get-MysqlConnectionLedger :1073、Write-MysqlConnectionLedger :1131)。HEAD 里没有这些函数。
- 其他会碰 Redis 的脚本:start_game.ps1:445-446(`--profile redis-cluster`,`up -d --no-recreate --pull never`)、:451(redis-cli ping);k8s_gate_drain.ps1:630-640(kubectl exec redis-cli,密码经 REDISCLI_AUTH);k8s_zone_rollback.ps1:711、:934;stress_round19.ps1;currency_crash_window.ps1。到线后这些 redis-cli 调用同样进不去。

【测试】
- HEAD 的 tools/scripts/tests/k8s_deploy_contract.tests.ps1 对 redis 清单没有任何断言(`redis\.yaml|requirepass|appendonly|maxmemory` 0 命中)。
- 该文件工作区现有他人未提交的 +301 行(:1448 起的 MySQL 连接总账用例),本项不能往里加用例。
- 测试骨架 tools/scripts/tests/lib/test_harness.ps1 只有 Test-Case、Assert-*、Complete-TestRun,没有按名字筛选的入口。
- CI:.github/workflows/deploy-config-tests.yml 把 tools/scripts/tests 一级目录下所有不以下划线开头的 .ps1 各跑一遍,要求 Linux pwsh 可跑;触发路径含 `tools/scripts/**`、`deploy/k8s/manifests/**`,不含 deploy/docker-compose.yml。新增测试文件会被自动纳入。
- C++ 与 Go 没有相关测试。

【告警与观测】
- 已跟踪的规则文件三份:deploy/k8s/data-service-audit-alerts.yaml、owner-epoch-alerts.yaml、scene-manager-alerts.yaml,都没有 Redis 连接数规则。deploy/k8s/mysql-connection-alerts.yaml 是他人未跟踪文件。
- 仓库里没有 redis_exporter(deploy、tools、go、cpp、java 下 0 命中;shared-redis-split-by-purpose.md:810 同一结论)。
- 各 Go 服务的 metrics 端口:login 9101、player_locator 9190、scene_manager 9150、db 9160、match 9170、friend 9180、chat 9210、guild 9220、trade 9230、data_service 9260(各自 etc/*.yaml)。go-zero 是否自带连接池指标属外部常识。

【文档】
- 交接 §3 :73、§5 :171 与 :173、§7 :221、§10 M1 :292;
- docs/design/capacity-model-hok-scale.md:270、:416;
- docs/design/shared-redis-split-by-purpose.md:23、:41(连接盘点)、:447-449(`-RedisCoreMaxMemory`、`-ConfirmRedisCoreRestart`)、:492-495(`__REDIS_CORE_ARGS__`、redis-ha.yaml 不动)、:810-814(监控);
- docs/design/gate-connection-admission-control.md:105;
- redis.yaml 文件头与 :136-139 注释;redis-ha.yaml 文件头;
- deploy/k8s/README.md 没有 Redis 连接上限小节(该文件现有他人未提交改动);
- deploy/k8s/AGENTS.md:24 仍写「redis/kafka/mysql are still single-replica Deployments」,已过时(文档漂移,不在本项内改,交付说明里点名)。

### 原报告:修复设计

【范围】只改清单、部署脚本、契约测试、文档。不编译 C++ / Go / Java,不动 proto 和 generated/,不涉及连接回调生命周期(§11.7)与宏(§11.2),密码不进日志。实测结论回来、负责人拍板后再落码。

一、分批(按「不碰他人未提交改动」重排)

此刻带着他人未提交改动的文件:tools/scripts/k8s_deploy.ps1(+431)、tools/scripts/tests/k8s_deploy_contract.tests.ps1(+301)、deploy/k8s/README.md、docs/PROGRESS.md。Redis 连接总账要复用的 Get-ManifestDeploymentReplicas、Get-MysqlConnectionPlan 只存在于那份未提交的工作区(工作区 k8s_deploy.ps1:954、:1004),HEAD 里没有。

| 批 | 内容 | 前置 |
|---|---|---|
| 0 | Codex 实测 S0~S2b、S6(有环境时);已有环境在线调大上限的指引 | 无,现在可做 |
| 1 | redis.yaml 显式上限与 fd 打印;新增一份静态契约测试;docker-compose 同值(可选);redis-ha.yaml 文件头一行前置注释(可选);PROGRESS 追加;PARITY 登记 | 批 0 结果;负责人就取值与「下一次 infra-up 会重启 redis-0」拍板 |
| 2 | k8s_deploy.ps1 的 Redis 连接总账、重启闸、部署后核对;dev_tools.ps1 与 k8s_image.ps1 透传;契约测试;README 小节;两份设计文档各一行 | **阻塞于 G2b 提交** k8s_deploy.ps1、契约测试、README |
| 3 | 告警(依赖 exporter 的拍板);go-redis 显式 PoolSize;C++ 断连日志带原因 | 各自另起,后两项要编译 |

批 1 只碰当前干净的文件。但它改了 Pod 模板:合入后任何一次 infra-up 或不带 -SkipInfra 的 all-up 都会滚动 redis-0(HEAD:1250、:5875)。两条路由负责人选:
- 路径甲:确认当前没有需要保护的在线环境,批 1 现在落;apply 的窗口由负责人定,建议与 G2b 的 mysql.yaml 同窗(存盘交接 :90、:225 是同一类问题)。在重启闸落地之前,对有在线玩家的环境只能带 -SkipInfra,或按维护窗口执行。
- 路径乙:有需要保护的环境,批 1 的 redis.yaml 推迟,并入批 2 与重启闸同一提交。
两条路下,已有环境都先在线调大(第四节)。

二、取值

1. 公式:maxclients ≥(Z × 每 zone 上限 + 全局池上限 + 滚动余量 + 运维余量)÷ 0.8。各项取「证据」里的总账;G 取 S6 实测值,未取得之前按 G=2 取保守值。
2. 各候选值在 80% 水位下能承载的 zone 数:

| N | A 默认 G=1 | B 默认 G=2 | C 目标形态 | D 目标形态且 G=2 |
|---|---|---|---|---|
| 10000 | 60 | 31 | 31~39 | 22~26 |
| 20000 | 122 | 63 | 63~80 | 45~53 |
| 40000 | 245 | 127 | 127~161 | 90~106 |

3. 建议 N = 40000。理由:
   - 用户目标是约 100 个 zone;40000 在 A、B、C 三档都留有 20% 以上余量,D 档 100 个 zone 时占 88%;
   - 20000 只够约 63~80 个 zone(B、C 档),盖不住 100;
   - 需要 40032 个 fd,低于常见的 65536 硬限。
   备选:20000(明确只承诺到约 60 个 zone);10000(只把缺省值写明,行为不变)。Z≈220 时任何一档都要重算。
4. 内存:每条连接的占用属外部常识(空闲几 KB,最坏约 20KB),4 万条最坏约 0.8GB;redis.yaml 只有 512Mi 的 requests、没有 limits(:136-143)。S3 的 tot-mem 均值用来校这个数。
5. 取值写在清单里,不走部署参数:清单是唯一真相;部署参数不粘滞,某次带了、下次漏带就会回落并再触发一次重启。
6. 单实例按连接数撑到 100 个 zone 只是算术:命令量约 3 个满载 zone 就到顶(交接 :171)。这个数真正的用处在分片之后(第六节)。

三、批 1 的文件与改法

1. deploy/k8s/manifests/infra/redis.yaml
   - 启动薄壳里定义一次 MAXCLIENTS,两条 exec 都带上;启动前打印 fd 限值。草图:
   ```
   MAXCLIENTS=40000
   need=$((MAXCLIENTS + 32))
   soft=$(ulimit -Sn 2>/dev/null || echo unknown)
   hard=$(ulimit -Hn 2>/dev/null || echo unknown)
   echo "redis fd limit: soft=$soft hard=$hard need=$need maxclients=$MAXCLIENTS"
   case "$hard" in
     unlimited|unknown) ;;
     *) if [ "$hard" -lt "$need" ]; then echo "ERROR: nofile hard limit $hard < $need, redis-server will lower maxclients below $MAXCLIENTS" >&2; fi ;;
   esac
   if [ -n "$REDIS_PASSWORD" ]; then
     exec redis-server --appendonly yes --maxclients "$MAXCLIENTS" --requirepass "$REDIS_PASSWORD"
   fi
   exec redis-server --appendonly yes --maxclients "$MAXCLIENTS"
   ```
   - 打印 fd 限值是必做项,不是可选:生产运行时的 nofile 本机无法核实,这一行是事后唯一能看到的证据。
   - 硬限不够时打 ERROR 但不拒启:唯一的 Redis 起不来等于全服停。这是有意的 fail-open,靠日志、部署后核对与告警兜住。补救办法是调节点上容器运行时的 LimitNOFILE;K8s 清单层无法声明 ulimit(kafka.yaml:157 的注释同一结论)。
   - 不自己执行 `ulimit -n`:Redis 会把软限抬到硬限(外部常识,S2.3 核实)。若 S2.3 证明它不抬,照 kafka.yaml:173-175 加一行。
   - 就绪探针不改。原报告建议把 max number of clients 视为就绪,撤回:理由「客户端能拿到明确错误文本」不成立(握手会吞掉它);而且到线时不论探针判定如何,新连接都进不来。S2.4 的结果出来后若有净收益再议。
   - 不设 timeout(保持缺省 0:设了会不停踢掉常驻空闲连接和 C++ 的单连接)。
   - tcp-keepalive、tcp-backlog 本批不动。tcp-backlog 缺省 511 且受容器 somaxconn 限制(外部常识),S2.1 与 S6 读到实际值后再定;重启后上万条连接同时重连只会慢,不影响正确性。
   - 注释写清:取值公式与出处;「改这个数或改本段任何一行都会改 Pod 模板,apply 即重启 redis-0」;「redis-ha.yaml 接入时,节点上限 ≥(代理副本数 + maxSurge)× haproxy maxconn + 余量」。
2. deploy/docker-compose.yml(可选,负责人定):redis 的 command 加一行 `--maxclients 40000`,与 K8s 同值。
   - 一键启动带 `--no-recreate`(start_game.ps1:446),改了 command 也不会生效。要生效须手动重建:`docker --context desktop-linux compose -f deploy/docker-compose.yml up -d --force-recreate redis`(会重启本机 Redis,数据在卷里)。
   - 核对命令:`docker --context desktop-linux exec redis redis-cli config get maxclients`。
3. deploy/k8s/manifests/infra/redis-ha.yaml(可选):只在文件头加一行接入前置条件的注释,不改行为。它没有 apply 路径(HEAD:5760),改了也无法验证;G4a 设计稿 :495 也写明不动它。`--maxclients` 与不变量测试留给「接入 redis-ha」那一项。
4. redis-match-cluster.yaml 不改:连它的只有 match 两个副本的集群客户端,不随 zone 数增长,远低于缺省上限。
5. 新增 tools/scripts/tests/redis_maxclients_contract.tests.ps1(回归测试;修复前红,修复后绿)
   - 为什么新开文件:k8s_deploy_contract.tests.ps1 现有他人未提交改动。新文件与同目录其他用例同一骨架(dot-source lib/test_harness.ps1,结尾 `exit (Complete-TestRun -SuiteName …)`),CI 会自动纳入。
   - 为什么可以直接读清单:redis.yaml 的渲染结果就是文件本身(HEAD:5843-5849)。
   - 路径一律 Join-Path,Linux pwsh 可跑。每个用例自己读文件、自己查找,查不到只让该条变红。
   - T1:redis.yaml 里 `MAXCLIENTS=<正整数>` 恰好出现 1 次。
   - T2:每一行 `exec redis-server` 都带 `--maxclients "$MAXCLIENTS"`,且这样的行至少 2 行。
   - T3:启动脚本里有 `redis fd limit:` 这一行打印(部署后核对靠它)。
   - T4(采纳第 2 条时):docker-compose 的 redis 带 `--maxclients`,数值与 T1 读到的相同。
   - 修复前:T1~T3(以及 T4)以「查不到 MAXCLIENTS」一类文本失败。
   - 不涉及 CMakeLists 或 vcxproj。
6. docs/PROGRESS.md:在末尾另起一条(该文件有他人未提交的追加,只追加、不改已有行;`merge=union`)。写明:改了什么;「未运行,待 Codex 验证」与验证命令的位置;「redis.yaml 改了 Pod 模板,apply 即重启 redis-0」;Java 版状态与 PARITY 登记。
7. Java 仓库 PARITY.md 登记一行(见 java_parity)。

四、上线顺序、新旧并存、回滚

1. 已有环境先在线调大:由负责人或运维在 redis-0 里执行 `CONFIG SET maxclients <N>`(运行期可调属外部常识,S5 顺带核实),立即生效,不重启。AI 不碰集群。
   - **必须在到线之前做**:到线后 redis-cli 自己也连不进去(S2.4 核实)。
   - CONFIG SET 不持久:redis-0 任何一次重启都会回到缺省值,直到清单生效。
2. 清单生效 = 重启一次 redis-0。计划内重启走 SIGTERM,退出前会刷 AOF(redis.yaml:79-80,给了 30 秒);丢写风险只在非正常退出(超时被强杀、OOM、节点故障)。重启期间全服登录中断并重放 AOF。在存盘关键写补上 WAITAOF 之前,按维护窗口处理:停写,确认重试队列与死信为 0(存盘交接 §3.5)。
3. 重启后核对:`CONFIG GET maxclients` 等于清单值;日志有 `redis fd limit:` 一行;没有 ERROR 行,没有 Redis 自己的下调日志(原文以 S2.2 实测为准)。
4. 回滚:在线 CONFIG SET 改回;清单回退需要再重启一次。
5. 客户端不需要任何改动,不存在新旧客户端并存的问题。以后要调大:先在线改,再改清单,等下一次窗口。

五、批 2 的设计(G2b 提交之后)

1. tools/scripts/k8s_deploy.ps1,新增一节「Redis 连接总账」,紧挨 MySQL 连接总账,形状和纪律照它:
   - Get-RedisMaxClientsFromManifest:读 redis.yaml 里的 `MAXCLIENTS=<数字>`,不是恰好 1 次就 throw。
   - Get-RedisConnectionLedgerInputs:
     - 副本数复用 Get-ManifestDeploymentReplicas。它的报错前缀写死「MySQL 连接总账」(工作区 :1009、:1013),入库后给它加一个前缀参数;不要复制第二份。
     - gate 取实际部署数;scene 按实际编排取:拆分模式是 world 加 instance(HEAD:84-90),Agones 自动伸缩取 -AgonesMaxReplicas(HEAD:135-136),否则 -SceneReplicas;battle 取 -BattleReplicas。
     - 每副本上限是一张常数表(客户端对象数、订阅连接数、G),每行带出处注释。G 以 S6 实测为准并注明来源;未实测的服务按 2。
   - Get-RedisConnectionLedger:纯函数,输出每 zone、全局、稳态、滚动余量、峰值、余量。规划 zone 数与滚动余量取自 Get-MysqlConnectionPlan,同一来源,不再写第二个 4。
   - Write-RedisConnectionLedger:写路径入口打印一行;超过 80% 只告警不阻断,与 MySQL 那份同一纪律。是否升级成门禁,两份一起拍板。
   - 部署后核对:非 DryRun 且带 -WaitReady 时,读线上 `CONFIG GET maxclients` 与清单值比较,不等就告警(到线前唯一能自动发现「被 nofile 下调」的地方)。读法照 k8s_gate_drain.ps1:630-640(密码经 REDISCLI_AUTH,不进命令行与日志)。
2. 重启闸(与 G4a 合成一套,不做两套):
   - 语义:本次 apply 会改变线上 `sts/redis` 的 Pod 模板(命令、env、探针、镜像、resources 任何一处)而没给 `-ConfirmRedisCoreRestart` 时,在 Apply-Infra 的任何写操作之前 throw;报错写明「apply 会重启 redis-0,全服登录中断并重放 AOF」。DryRun 不查集群。
   - 这比 G4a 设计稿 :447 的「给了 -RedisCoreMaxMemory 就必须给 Confirm」更宽:后者是它的特例,而且能拦住「上次带了参数、这次漏带」的回落重启。同名开关只能有一个语义,先与 G4a 负责人对齐。
   - 实现建议:部署脚本对渲染后的 StatefulSet `template:` 段算摘要,写进 StatefulSet 自己的注解(不在 Pod 模板里,写它不触发滚动);下次 apply 前读线上注解比较。读线上现状照 Get-LiveStatefulSetReplicas(HEAD:1085)的写法;判定拆成纯函数(线上不存在 / 相同 / 不同或没有注解)供契约测试直接喂值,照 Get-KafkaTopologyChangeVerdict(HEAD:1116)。注解占位用 `__XXX__` 形态,未替换时有现成的 fail-closed 检查(HEAD:5870-5873)。
   - 备选实现:比较 `kubectl apply --dry-run=server` 与线上的 `.spec.template`。依赖 kubectl 的输出形态,需 Codex 在 kind 上先确认。
   - 考虑过、不采用:把 redis 的 updateStrategy 改成 OnDelete。理由一,改动会潜伏到下一次意外重启才生效,写错的模板在最坏的时刻才暴露;理由二,`kubectl rollout status` 对 OnDelete 的 StatefulSet 直接报错(HEAD:3000 的注释),而 infra-up 的等待正是用它(HEAD:2946、:5893),同样要改脚本。
   - 运维后果要写进 README:闸合入后,到维护窗口之前,模板有变化的 infra-up 会被拒,这是预期行为。
3. tools/scripts/dev_tools.ps1、tools/scripts/k8s_image.ps1:透传 `-ConfirmRedisCoreRestart`,写法照 `-AllowDisruptiveSwitch`(dev_tools.ps1:672、:870;k8s_image.ps1:421)。不补的话从文档入口无法确认重启(dev_tools.ps1:54-55 的注释记过同类事故)。
4. tools/scripts/tests/k8s_deploy_contract.tests.ps1(对方提交之后再加):
   - 规划档峰值 ≤ 0.8 × 清单值(按 AST 抽函数,照 HEAD:337、:1288 的做法);
   - 输入钉子:总账用的 G 与各 Go 清单的 CPU limit、副本数一致,改了就变红,逼着重算;
   - 重启闸纯判定函数的三种结果,负向用例断言错误文本;
   - 两个包装入口的透传,照 HEAD:1170 起的 GateRouterMode 透传用例。
5. deploy/k8s/README.md 新增「Redis 连接上限与总账」小节:公式与取值;上线顺序;排查命令(CONFIG GET maxclients、INFO clients、INFO stats 的 rejected_connections);「客户端日志里不一定出现 max number of clients,以服务端 rejected_connections 为准」;「到线之后 redis-cli 也进不去,在线调大必须在到线之前做」;接入 redis-ha 的不变量。同文件与 deploy/k8s/AGENTS.md:24 里「redis 是单副本 Deployment」的旧说法在交付说明里点名。
6. 文档各留一行:shared-redis-split-by-purpose.md §4.3 与连接盘点处(「每个拆出的实例都显式 --maxclients,按用途重算总账」「C++ 是每进程一条」);capacity-model-hok-scale.md:270 的现状一栏改为指向清单。交接文档不回头改。

六、批 3 与关系说明

1. 告警是必需项,不是可选:两类客户端到线后都不暴露原因,服务端信号是唯一可靠的信号。但仓库里没有 redis_exporter,规则文件在它落地前没有数据。
   - 规则(照已跟踪的 deploy/k8s/scene-manager-alerts.yaml 的写法):connected_clients ÷ maxclients 超过 0.8,warning;rejected_connections 有增量,critical;生效的 maxclients 小于清单值,critical。指标名(redis_connected_clients、redis_config_maxclients、redis_rejected_connections_total)凭记忆,未核实。
   - 是否上 exporter 与 G4a §13 第 6 条一起拍板。过渡做法:批 2 的部署后核对,加上 G4a §6.4 计划的 INFO 快照里带 clients 段。
2. 后续项(另起,要编译):go-redis 直连的客户端显式设 PoolSize(login、db、player_locator、trade、guild、data_service 的 DevRedis),让总账常数不再跟着 CPU limit 漂;C++ 断连回调打印原因(redis_manager.cpp:113-117)。在此之前靠批 2 的输入钉子防漂。
3. 与 redis-ha:接入前置条件增加一条,节点上限 ≥(代理副本数 + maxSurge)× haproxy maxconn + 余量(redis-ha.yaml:530、:808、:812)。不变量测试届时从清单读取这三个数,不写死 2。
4. 与 G4a 按用途拆分:每个服务会对每个实例各开一个池,单实例连接数下降、进程总连接数上升;总账函数加一个「用途」维度;redis-purpose.yaml 模板带 `--maxclients`。G4a §4.3 的「缺省渲染与今天逐字节相同」的基准文件要以本项落地后的清单为基线;`__REDIS_CORE_ARGS__` 的行内占位要接在 `--maxclients "$MAXCLIENTS"` 之后。
5. 与按 player_id 分片:每个进程对每个分片各开一个池,每片的连接数约等于今天单实例的总数,不随片数下降。「分片后每片重算」的结果是每片约等于现值,这一句要写进分片设计。

七、失败语义

- 运行期到线:服务端拒绝新连接,天然 fail-closed。Go 侧登录、存盘失败并重试(推断,错误文本见 S5);C++ 每 3 秒重连且原因不进日志;gateway 的限流是 fail-open(java/gateway_node/src/main/resources/application.yaml:129 的注释)。
- 部署期:清单里的 MAXCLIENTS 不是恰好一次,throw;重启闸 fail-closed;总账超限只告警。
- 启动期 fd 不够:不拒启,打 ERROR(有意的 fail-open,已配日志与测试,指标待 exporter)。

八、批 1 落码后交 Codex 的命令(PowerShell 7;工作目录 D:/luyuan/xuanming-server-mmo-cpp,因为修复是主工作区里的未提交改动)

1. 落码前先存基线(区分本项与他人在途改动):
```
Set-Location D:/luyuan/xuanming-server-mmo-cpp
git status --short
Get-ChildItem tools/scripts/tests -Filter *.ps1 -File | Where-Object { $_.Name -notlike '_*' } | Sort-Object Name | ForEach-Object { pwsh -NoProfile -File $_.FullName *> $null; '{0} exit={1}' -f $_.Name, $LASTEXITCODE }
```
2. 绿:落码后跑新文件,再把上一条重跑一遍。
```
pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1
```
   通过标准:新文件退出码 0;其余文件的退出码与基线逐个相同。失败时保留该文件的完整输出。
3. 红:在未修复的提交上只放进新测试文件。
```
git worktree add --detach "$env:TEMP/p0e-red-0aa1027bc" 0aa1027bc
Copy-Item tools/scripts/tests/redis_maxclients_contract.tests.ps1 "$env:TEMP/p0e-red-0aa1027bc/tools/scripts/tests/"
pwsh -NoProfile -File "$env:TEMP/p0e-red-0aa1027bc/tools/scripts/tests/redis_maxclients_contract.tests.ps1"
git worktree remove --force "$env:TEMP/p0e-red-0aa1027bc"
```
   期望:退出码非 0,T1~T3 以查不到 MAXCLIENTS 一类文本失败。
4. 薄壳真实执行(6 种组合:三种 nofile × 有无密码)。从清单里把脚本原样取出,不手抄:
```
$lines = Get-Content deploy/k8s/manifests/infra/redis.yaml
$start = ($lines | Select-String -Pattern '^\s+- \|\s*$' | Select-Object -First 1).LineNumber
$end = ($lines | Select-String -Pattern '^\s+env:\s*$' | Select-Object -First 1).LineNumber
$body = $lines[$start..($end - 2)] | ForEach-Object { $_ -replace '^ {14}', '' }
$dir = Join-Path $env:TEMP 'p0e-shim'; New-Item -ItemType Directory -Force $dir | Out-Null
[IO.File]::WriteAllText((Join-Path $dir 'start.sh'), (($body -join "`n") + "`n"))
$pw = [guid]::NewGuid().ToString('N')
docker --context desktop-linux run -d --rm --name p0e-shim -v "${dir}:/shim:ro" redis:7.2 sh /shim/start.sh
Start-Sleep 2
docker --context desktop-linux exec p0e-shim redis-cli config get maxclients
docker --context desktop-linux logs p0e-shim
docker --context desktop-linux stop p0e-shim
```
   - 变体:run 时加 `--ulimit nofile=1024:1024` 或 `--ulimit nofile=1024:65536`;有密码的组合 run 时加 `-e REDIS_PASSWORD=$pw`,exec 时加 `-e REDISCLI_AUTH=$pw`。
   - 期望:缺省与 1024:65536 两种下,日志有 `redis fd limit:` 一行、没有 ERROR,`config get` 等于清单值;1024:1024 下日志有 ERROR 一行,Redis 仍能起来,`config get` 小于清单值。有密码的三种里,`docker logs` 的输出不包含 `$pw`。
5. 采纳 docker-compose 那一条时:负责人同意后手动重建本机 redis,再执行第三节第 2 条的核对命令。
6. 可选,需负责人同意:在 kind 上 infra-up 后执行第四节第 3 条的核对;命令要带全 `-KubeContext kind-mmorpg -KubeConfig "$env:TEMP\kind-mmorpg.kubeconfig"`(deploy/k8s/AGENTS.md:48-49)。
7. 本批没有 MSBuild、go build、mvn。没有 Codex 结果之前,交付说明写「未运行,待 Codex 验证」。

### 原报告:Java 版

【结论】待做(同类问题存在;部署门禁部分不适用)。

【证据】(Java 仓库 D:/luyuan/xuanming-server-mmo,HEAD d942f74,工作区干净)
- 服务端没有设 maxclients。deploy/compose/infra.yaml:60-62:
  `image: redis:8.10.2@sha256:…`
  `command: ["redis-server", "--appendonly", "yes"]`
  宿主端口缺省也是 6379(:64 `"127.0.0.1:${GAME_REDIS_HOST_PORT:-6379}:6379"`)。deploy/ 下只有 compose 与 docker,没有 K8s 清单。
- 客户端是 Redisson 单机模式,没有设池参数。game-discovery/src/main/java/com/game/discovery/RedisAutoConfiguration.java:34 `configure(config.useSingleServer(), props);`,:44-50 只设 address、database、password、超时与重试;RedisProperties.java:24 的 record 没有任何池字段。客户端 Bean 带 `@Lazy`(:30),不用 Redis 的进程不会建连。
- Redisson 版本 3.50.0(pom.xml:54)。缺省池参数是外部常识,未核实:连接池 64、最小空闲 24、订阅连接池 50。按这组缺省值估算,每个用到 Redis 的进程常驻约 25 条、上限约 114 条——这是估算,不是事实。
- 依赖 game-discovery 的模块有 13 个;其中 gate、scene、login、data 用了 Pub/Sub。Java 版的部署规模本次未核实。

【PARITY.md 登记】在「功能对齐」表末尾追加一行(列顺序:功能 | mmorpg 位置 | mmorpg commit | Java 模块 | Java 版本 | 状态 | 备注):

| 共享 Redis 连接上限显式化(maxclients 写进清单、启动时打印 fd 限值、各客户端池上限记总账) | `deploy/k8s/manifests/infra/redis.yaml`、`deploy/docker-compose.yml`、`tools/scripts/tests/redis_maxclients_contract.tests.ps1`;连接总账与重启闸在 `tools/scripts/k8s_deploy.ps1`(第二批) | 未提交(待负责人发话,提交后补哈希) | game-discovery(`RedisProperties`、`RedisAutoConfiguration`)、deploy/compose | — | Java 待做 | 登记日期。服务端内部事项,不涉及客户端契约。Java 版 `deploy/compose/infra.yaml` 的 redis 同样没设 maxclients;Redisson 没设池参数(按缺省值估算每进程常驻约 25 条、上限约 114 条,缺省值未核实)。待做:`game.redis.*` 增加连接池上限与最小空闲两项并给出总账;compose 显式 `--maxclients`。Java 没有 K8s 清单,部署门禁部分不适用。 |

- 「Java 待做」是该表已有的状态词(PARITY.md:23 的取值说明)。
- mmorpg 这边的 docs/PROGRESS.md 条目同时写明:「Java 版:待做(已在 PARITY.md 登记);对应 Java 版本号:—」。Java 仓库只动 PARITY.md。

### 原报告:风险与未决

【风险】

1. 工作区里他人的在途改动仍在变化。此刻带着未提交改动且与本项相关的文件:tools/scripts/k8s_deploy.ps1(+431)、tools/scripts/tests/k8s_deploy_contract.tests.ps1(+301)、deploy/k8s/README.md、docs/PROGRESS.md。本项第二批必须排在 G2b 提交之后;不得覆盖,不得 git add -A。落码前重新看一次 git status。
2. 改 redis.yaml 的 Pod 模板会让下一次 infra-up 重建唯一的 Redis(HEAD:1250、:5875),全服登录中断。计划内重启会先刷 AOF(redis.yaml:79-80),丢写只在非正常退出;但在存盘关键写补上 WAITAOF 之前,任何一次 Redis 重启都应按维护窗口处理(存盘交接 §3.5、:90)。重启闸落地之前没有任何机制拦住它。
3. 优先级是有条件的。按规划档 4 个 zone 只有 601~1,161 条,前提有两个,都未实测:生效上限确为 10000(目标运行时的 nofile 硬限不低于 10032);K8s 上的 GOMAXPROCS 确为 1 或 2。任何一个不成立,今天的规模就可能到线。
4. 池上限与 CPU limit 隐式耦合(10 × GOMAXPROCS)。为撑单 zone 10 万人调大 login 或 db 的 CPU limit,会悄悄放大 Redis 连接数。显式 PoolSize 落地之前,只有第二批的输入钉子能发现。
5. 连接数不回落(go-redis 无后台回收)。一次并发尖峰之后,连接数长期停在峰值;滚动发布叠加时更高。
6. 到线后的连锁:go-redis 重试并重新拨号;login、player_locator、guild 启动 Ping 失败直接 panic,go-zero 的 Must 版本也会退出(外部常识),K8s 上表现为反复重启并持续抢名额;就绪探针失败后 Service 摘掉唯一端点;运维的 redis-cli 也进不去。
7. 到线后日志容易误导:release 档可能出现 failed to authenticate,与「C++ 不发 AUTH」那一项的症状混淆。
8. 在线 CONFIG SET 不持久。清单生效之前,redis-0 任何一次意外重启都会静默回到缺省值;在告警落地前没有东西会提醒。
9. 接入 redis-ha 之前不修倒挂:到线时 haproxy 健康检查失败会踢掉全部会话(推断,未实测)。

【需要负责人拍板】

a. N 取 40000(建议)、20000 还是 10000。
b. 第一批走路径甲(现在落 redis.yaml,接受下一次 infra-up 重启 redis-0,窗口由负责人定)还是路径乙(等重启闸同批)。前提问题:现在有没有需要保护的在线环境。
c. 已有环境是否先在线 CONFIG SET,由谁执行。
d. docker-compose 是否同批加 `--maxclients`(要手动重建本机 redis 才生效)。
e. redis-ha.yaml 文件头是否加一行接入前置注释(不改行为)。
f. 重启闸的语义(「模板有变化就要确认」)是否取代 G4a 设计稿里按参数触发的写法;两边合成一套,由谁做。
g. 告警:是否上 redis_exporter(与 G4a §13 第 6 条一起);在此之前接受只有部署后核对。
h. Redis 连接总账超限是只告警还是升级为部署门禁,与 MySQL 那份一起定。
i. 是否顺带显式写 tcp-keepalive 60(建议本批不动)。
j. 实测授权:S2 若本机没有 redis:7.2 要拉镜像;S5 会让本机开发栈短时登录失败;S6 若要新建 kind。
k. 生产或预发集群上 redis-0 的 nofile 硬限与各 Go 服务的 GOMAXPROCS,由谁在目标集群上取一次。

【与另两份交接的重叠或冲突】

- 开服准备交接:G2b 的 MySQL 连接总账与本项同构,第二批复用它的 Get-ManifestDeploymentReplicas 与规划 zone 数,不另起一套;它的 §4 第 7 项把 capacity-model:416「显式设置 PoolSize 和 maxclients」归到本交接,其中 PoolSize 在本项里是第三批。它自己也写了「G4b 必须排在 G2b 之后,两者都改 k8s_deploy.ps1」(:88),本项同理。
- G4a 设计稿(shared-redis-split-by-purpose.md,已入库):§4.3 要求核心清单缺省渲染与今天逐字节相同,并已起名 `-ConfirmRedisCoreRestart` 与 `__REDIS_CORE_ARGS__`。本项会改变缺省渲染,它的基准文件要以本项之后为准;同名开关的语义要先对齐;它的「C++ 每线程一条连接」(:23、:41)需要订正。
- 存盘管线交接:G2b 的 mysql.yaml 同样是「改模板即重启唯一实例」(:90、:225),两次重启宜并进同一个窗口;它的第 4 项计划导出 go-redis 的 PoolStats,那是客户端侧连接数的现成观测口,不要重复注册。Redis 到线时 go/db 的重试是否消耗预算、是否进死信,本次未核实。

【未核实项与由谁确认】

由 Codex 实测确认:
- Redis 缺省 maxclients、timeout、tcp-keepalive、tcp-backlog;fd 不足时的自动下调与日志原文;Redis 是否自己抬软限(S2)。
- 到线时服务端回的文本、是否给管理连接留名额、探针句子的判定(S2.4)。
- go-zero 的 MinIdleConns 与 PoolSize、是否引入 automaxprocs、是否关掉 CLIENT SETINFO、是否自带连接池指标与熔断;v9.16.0 / v9.17.3 的池与握手是否与 v9.18.0 相同(S2b;本机没有找到模块缓存,读不到就记未取得)。
- 本机每进程连接数、每连接内存、负载后是否回落(S3、S4)。
- CONFIG SET maxclients 是否运行期可调;到线时 Go 客户端的错误文本(S5,可选)。
- kind 上 redis-0 的 nofile、somaxconn、各 Go 服务的 GOMAXPROCS(S6)。
- 薄壳在三种 nofile 与有无密码下的真实行为(修复设计第八节第 4 步)。
- 第二批落码时:kubectl 读注解与 server 端 dry-run 的输出形态;dev_tools.ps1 与 k8s_image.ps1 透传后从文档入口可达。

由负责人确认(本机无法取得):
- 生产或预发运行时的 nofile 软硬限与 GOMAXPROCS。
- 是否存在需要保护的在线环境。

仍未核实、暂无实测安排:
- haproxy 3.2 的 frontend 缺省 maxconn 是否等于 global;到线时 `on-marked-down shutdown-sessions` 的连锁(留给接入 redis-ha 那一项)。
- C++ 到线时的日志表现(hiredis 把服务端错误交给哪个回调属外部常识;留给「断连日志带原因」那一项实测)。
- Redisson 3.50.0 的缺省池参数;Lettuce 到线时的重连行为。
- redis_exporter 的指标名。
- Go 1.25 起容器感知 GOMAXPROCS 的下限是否为 2,以及它是否只对 go.mod 为 1.25 以上的模块生效。
- StatefulSet 缺省 updateStrategy 为 RollingUpdate(据此判断「改模板即重启」)。
- db、trade 启动 Ping 失败后的处理;复核意见提到的「裸重启 gate 的实例键与一键启动不一致」(S5 已不再重启 gate,未去核对)。

【邻接发现,不在本项内修】

- `zoneredis.max_connections: 100`(HEAD:1872)是死配置,容易让人以为 C++ 有连接池。
- Java gateway 的 K8s ConfigMap 没有 Redis 密码(HEAD:4949-4952),release 档会 NOAUTH,属于 C++ AUTH 那一项的同类入口。
- deploy/k8s/AGENTS.md:24 仍写 redis 是单副本 Deployment,已过时。
- deploy/k8s/manifests/go-svc/gateway.yaml 是否还有消费者未核实(部署脚本里 gateway 指向 java-svc 的配置,HEAD:1002)。

【过程说明】
本次定稿只读仓库,没有运行构建、测试或任何外部服务命令。为了按 HEAD 行号定位,曾把 `git show HEAD:` 的两个文件输出重定向到会话临时目录(仓库之外),仓库内没有新建、修改或删除任何文件。

### 原报告:预计改动文件

- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/manifests/infra/redis.yaml
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/redis_maxclients_contract.tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/deploy/docker-compose.yml
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/manifests/infra/redis-ha.yaml
- D:/luyuan/xuanming-server-mmo-cpp/docs/PROGRESS.md
- D:/luyuan/xuanming-server-mmo/PARITY.md
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/k8s_deploy.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/dev_tools.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/k8s_image.ps1
- D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/k8s_deploy_contract.tests.ps1
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/README.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/design/shared-redis-split-by-purpose.md
- D:/luyuan/xuanming-server-mmo-cpp/docs/design/capacity-model-hok-scale.md
- D:/luyuan/xuanming-server-mmo-cpp/deploy/k8s/redis-connection-alerts.yaml
