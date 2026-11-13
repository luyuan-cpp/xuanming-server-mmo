# 服务器代码缺口清单(2026-11-01)

负责人问「服务器代码做完了吗」时做的盘点。按当天工作区逐条对代码核对,只读,没有编译或运行。接手前先核对 `git log` 和本文状态:工作区有多个会话并行。

**负责人目标(2026-11-02)**:「我要达到全链路都能水平扩展」。下面第 4、5 条是已核实的阻塞,但本清单只列了这两条,不等于全链路只差这两条。额度重置后先按 [消除单节点总纲](../design/no-single-node-horizontal-scaling-20261001.md) 的口径逐环节重新盘点全链路:gate、login、client-rpc-router、data_service、scene_manager、scene、go/db、player_locator、各全局服务、Redis、Kafka、etcd、TiDB,看加机器能不能真正扩吞吐,把新发现补进本文,再按依赖顺序落码。现有的全服或多实例选主要逐个核:scene_manager 编排、player_locator 会话对账、guild / trade / player_inbox 资产通道。

**接手先看这段(2026-11-01 第二轮)**:第 6、1 条和第 2 条的终结工具已落码,都没有编译,验证见 [progress.md](player-save-direct-commit-20261030/progress.md) §16「清单第 1、2、6 条」。11-02 又落了第 3 条的大部分和第 11、12 条。11-05 落了第 2 条后半、第 3 条剩余,收尾了第 6 条,都没有编译。11-05 起多个会话分工:第 2、3、6 条在「按缺口清单补完服务器代码」会话;第 1 条残余和待确认里的 Agones 渲染在「按缺口清单做完服务器代码(11-05 额度重置后)」会话(11-05 已落码,未编译);第 9 条 11-05 移交「Scene manager zone selection and etcd threshold」会话;第 4、5 条 11-05 由「控制面按 zone 化与控制域 Redis 分片(缺口第 4、5 条)」会话认领(原归上一会话,对方未动手、同意移交),先写设计再分批落码;第 8、10、13 条和全链路盘点在「服务器代码缺口清单」会话;第 7 条的残余 11-05 由「服务器代码缺口补完」会话做完(未编译)。生成器不清的残留头文件 `player_rollback_service_metadata.h` 要在重新生成之后删。

**负责人 2026-11-06 定:第 15 条起全部做完。** 第 15 条及之后还没做完的条目,连同已落码条目的「仍剩」,等下次每周额度重置之后全部做完。重置时刻是本机时间 2026-11-12 15:00,重置前不再开新条目。截至 11-06 晚还要做的:

- 没人接:第 15、20、21 条;
- 已有认领、重置时先核落到哪一步:第 22 条(「服务器代码缺口补完」认领),第 25、26 条(原归「灰度发布、金丝雀发布和水平扩展」,该会话已结束);
- 只做了一部分:第 17、19、27、28 条的剩余部分;
- 设计已写、待落码:第 30 条(11-10 四批都已落码、未编译,待 Codex 验证;剩场景亲和与命令 topic 换代,等批 4 两项计数的压测数据再定,见第 30 行);
- 已落码条目的「仍剩」:第 16、24 条;第 1 条的 Agones 名额收敛(11-10 补登,原文在第 1 条「仍剩」;11-10 先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话认领并写了设计(`agones-scene-node-high-density.md` §13),还没落码;**同日负责人让「场景销毁认领:回滚原子化与重发幂等」会话现在做,已由该会话落码,未编译**,验证见 `docs/PROGRESS.md` 2026-11-10「Agones rooms 名额改为按身份的 List 条目」;剩下的「改派挑目标不看容量」11-11 由同一会话改为改派前先占名额,未编译,验证见 2026-11-11「世界频道改派前先占名额」);第 7 条的「跨 zone 重定向第一次判不出停写」(11-10 补登,原文在第 7 条「仍剩」;11-10 先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话认领,没动代码;**同日负责人让「玩家切换zone和gate链接」会话现在做,已由该会话落码,未编译**,验证见 `docs/PROGRESS.md` 2026-11-10「跨 zone 重定向读 login 发布的停写标记」);
- 第 23 条:负责人 11-10 看过「暂不改」后要求现在就改,11-11 由「帮会榜积分写入去掉全服维护锁(C3)」会话落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-11「帮会积分写入不再抢全服锁」。
- 11-10 负责人新增第 34 条(全部 Redis 上 Cluster):「Redis 分布式水平扩展」会话认领,设计已写;11-11 第 1 批已落码、未编译,第 2a 批起未开工。

做法照旧:动手前在本表对应行认领;先写设计再落码;每批不超过 30 个文件;编译、测试与冒烟交 Codex;Java 只登记待做。

**11-10 进度(第 15 条起;都未编译,验证命令见 `docs/PROGRESS.md` 各条)**:

- 已落码:第 15 条(「服务器代码缺口补完」);第 16 条,含按事件唤醒;第 25、27 条;第 20 条的测量指标与测量方案。这四条都是「Turn battle gap closure handoff」做的。
- 已结论不改:第 21 条(收尾归「服务器代码缺口补完」);第 26 条(补发单领导者是有意的)。
- 别的会话已落码(11-10 晚核对,都未编译):
  - 第 22 条:定案并按设计稿 §5 落码,归「缺口第 22 条:写者名册回收」,部署契约的失败也由它跟进;11-11 落码完毕(含 11-10 Codex 那轮两套部署契约失败的修复),未生成、未编译,验证见 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」;
  - 第 30 条:四批都已落码。批 1、4 是「服务器代码缺口补完」做的,批 2、3 是「缺口第 30 条:gate 连接与解码扇出」做的;
  - 第 25 条「仍剩」三项(mail / data_service 回收的重投分片、终态行清理、告警)归「G4a Redis 设计稿修订应用」。
- 第 23 条:11-11 已落码(未编译),归「帮会榜积分写入去掉全服维护锁(C3)」;全服帮会榜已删。
- 等压测证据再定(**11-11 起由「缺口清单压测项:统一压测方案与定案」会话负责;同日负责人改口径「不用等压测数据了,先写完」,改为现在定案、落码,分工见下面「11-11 归属核对」表**):
  - 第 17 条按片拆游标;
  - 第 20 条要不要改闸门;
  - 第 24 条热池;
  - 第 30 条的场景亲和与命令 topic 换代;
  - 第 15 条的 `snapshot_guid` 分桶与按天分区。
  - **11-11 定案结果**(「缺口清单压测项」会话,都未编译、未运行,验证见 `docs/PROGRESS.md` 2026-11-11「缺口清单压测项定案」):第 3 条定值并落码;第 14 条名次语义不改、按名次拉长轮询间隔已落码;第 15 条两项都不做(`snapshot_guid` 不单调,前提不成立);第 30 条两项都不做;第 33 条保留 200;第 24 条定了拆法方向,落码另开会话。第 17、18、20、25 条归「Turn battle gap closure handoff」。各行写了理由。

**11-11 核对(第 1~34 条;只对文档与代码核对,没有编译)**:

- 第 1~33 条的主体都已落码或已结论不改,绝大多数未编译;各行写明的残余照旧。11-10 晚之后又落了三项,都未编译:
  - 第 1 条最后一项「世界频道改派前先占名额」,见 `docs/PROGRESS.md` 2026-11-11 同名条目;
  - 第 24 条的 `ReadyAfter` 已接同一分片条件(`go/match/internal/authority/work_owner.go` 的 `workReadyTicket`);
  - 第 25 条「仍剩」三项,见 `docs/PROGRESS.md` 2026-11-11「资产指令重投分片第二批」。
- 没落码的只剩第 34 条(全部 Redis 上 Cluster,7 批约 170~200 个文件):11-11 起由「服务器代码缺口补完」[37a1b5] 从批 1 做,`docs/PROGRESS.md` 里还没有任何一批的落码条目。(11-11 补记:[37a1b5] 写完 Go 的 `clusterredis` 拷贝与族表之后不在了;「Redis 分布式水平扩展」会话没有结束,只是重启后换了标识,现为 [3204dc]。它补完第 1 批并落码,未编译,见 PROGRESS 2026-11-11「全部 Redis 上 Cluster 第 1 批」。)
- 第 32 条的回归用例 11-11 变红,同日已由另一个会话按下面的修法改好 `dead_node_reregister_test.go`(只改测试,未运行,待 Codex 重跑 scene_manager 全量)。Codex 11-11 跑 scene_manager 全量,`TestDeadNodeReregister_*` 里有 3 条失败(`docs/PROGRESS.md` 2026-11-11「跨 zone 停写标记验证结果」及其首错补充)。
  - 原因(只读代码判断,没有运行):11-10 断线重连第三轮的 R15 改了重新注册时的处理(`clearNodeDeathForInstance`):换了进程,或认不出是哪一代时,保留 `death_at`,让老实例的 20 秒屏障走完。
  - 这 3 条用例还按旧行为写,以为重新注册会当场清掉 `death_at`、收尾当拍就跑,所以期望出队 1 个,实际 0 个。
  - 修法只改 `dead_node_reregister_test.go`:重新注册之后,先断言屏障压着、实例原样;再用现成的 `elapseBarrier` 推过屏障;最后断言照常收尾。业务代码不用改。
- 除上面「等压测证据再定」的各项,下面这些残余也要压测数据才能定(11-11 同样改为现在定案,结果见上面「11-11 定案结果」与各行):
  - 第 3 条审计 topic 的 `retention.bytes`;
  - 第 14 条排队名次;
  - 第 18 条深名次计数;
  - 第 25 条到期索引加不加 `player_id`;
  - 第 33 条住户预算 200。
- 清单之外:负责人 11-11 要求所有节点都能随时无感重启、无感升级。核对结论是还做不到,记在 `node-lifecycle-elasticity-release-20261023.md` §3.7,没有编进本清单。

**11-12 核对(「Turn battle gap closure handoff」,只读核对 + 唤醒停着的会话)**:

- 第 16、17、18、20、25、27 条的代码都已写完,都没有单测与真库运行证据。11-12 Codex 的三语言全量 Docker 构建(C++、Go、Java 成功)只编了本机调试栈里的服务:`shared/authoritysql` 等随它们编过;rank、guild、trade、mail 与 robot 不在那次构建里。
- 三个会话在 11-11 07:16~07:48 UTC 前后被同一次中断打断、之后一直没恢复。11-12 已发消息让它们接着做完:
  - 第 2 条残余(trade 人工判定的工具出口):停在重写 `manual_test.go`,PROGRESS 里还没有落码条目;
  - 第 24 条热池拆分:只定了方向,仓库里还没有子池代码;
  - 第 34 条第 2a 批(scene_manager 键改形):未开工,2b~7 批都排在它后面。
- Codex 11-11 报的 `asset_op_system.cpp:431` 裸指针成员阻塞(`dev.bat build` 停在 scene),已由别的会话改成 `std::string`。
- 11-12 晚复核:第 2 条残余与第 24 条热池拆分都已落码(未生成、未编译,见 `docs/PROGRESS.md` 2026-11-12 两条同名条目);第 34 条第 2a 批负责人同意一批改约 60 个文件,会话正在做,2b~7 批排在它之后。按模式分榜的会话把第 18 条的「N+」接进了它的按榜快照,两条客户端用例仍在;A18 的连接复用在第 20 条合并读的基础上接着做,不冲突。

**11-11 归属核对(会话「服务器代码缺口清单」逐条核对;负责人要求每一条剩下的事都要有窗口)**:下表是 11-11 全部条目里还没收尾的部分。「Codex」表示剩下的只是编译、测试、冒烟、部署契约或压测执行;「Java」表示只剩 Java 版,已在 Java 仓 `PARITY.md` 登记待做。新开的窗口都在 main 上做、不开分支。

| 条目 | 还剩什么 | 归谁 |
|---|---|---|
| 全部已落码条目 | 编译、测试、冒烟(大部分没有运行证据) | Codex |
| 1 | Agones 真集群的三条假设(设计 §13.8) | Codex 上集群前核对;落码会话「场景销毁认领:回滚原子化与重发幂等」 |
| 2 | 四类情形没有工具出口、没有管理 RPC | **11-11 新开「缺口第 2 条残余:交易人工判定的工具出口」;11-12 代码全部落完,未生成协议、未编译、未测试,剩 Codex 验证**(命令见 `docs/PROGRESS.md` 2026-11-12「交易转人工合同四类残余的处置出口与管理 RPC」);Java 只登记待做 |
| 3、14、15、24(热池)、30(场景亲和、命令 topic)、33(住户预算) | 原为等压测证据定案;**负责人 11-11 改口径:「不用等压测数据了,先写完」**,改为现在按标准做法定案、落码,压测留作之后的验证 | **11-11 新开「缺口清单压测项:统一压测方案与定案」**(原定先写统一压测方案交 Codex;新口径已于 11-11 转告该会话)。第 24 条热池的落码 11-12 由「缺口第 24 条:热池按机房与子池拆写者」会话做完(未编译,见第 24 行) |
| 17、18(rank 按片拆游标、深名次与深页) | 同上,现在定案、落码。**11-11 第 17、18 条都已落码**(未生成、未编译);第 18 条负责人定「前 N 名精确、之后显示 N+」(N = 10000),服务端、客户端、robot 一起改,Java 待做 | 「Turn battle gap closure handoff」11-11 接手(原在上一行,两会话 11-11 商定改归它) |
| 20、25(到期索引) | 同上,现在定案、落码。**11-11 都已落码**(未生成、未编译):第 20 条合并事务内的读、预切分定为不做;第 25 条四张表 `idx_0` 加 `op_id`、`player_id` | 「Turn battle gap closure handoff」11-11 认领(第 20 条测量方案与第 25 条重投分片都是它写的) |
| 4、5、11 | 部署契约、kind 冒烟、告警入集群 | Codex;第 5 条的 Cluster 化并入第 34 条 |
| 7、16、18、19、21、27 | 只剩 Java 版 | Java |
| 9 | watch 的 DELETE 风暴 | 已由第 32、33 条覆盖 |
| 22 | REVOKED 登记行与 slot 不回收(设计稿 §5.8) | **11-11 新开「缺口第 22 条残余:写者名册 REVOKED 行回收」**;11-11 已定案落码(未编译):一代结束即删它的 64 个 slot 行,登记行与收据永久保留;剩 Codex 验证与 Java |
| 22 | 缩容后末代自动退休(设计稿 §5.9,原 §5.8「其余残余」第 1 条) | **11-12 代码全部落完,未编译、未测试,待 Codex 验证**(命令、顺序与通过标准见 `docs/PROGRESS.md` 2026-11-12「写者缩容收尾:被缩掉槽位的末代自动结束」):每个写者 StatefulSet(含 `<svc>-canary` 与 gateway)一份 CronJob `sql-writer-slot-retirer-<StatefulSet>`,每 5 分钟跑 `writer_authority -command retire-removed-slots`,先读目录快照、再看副本数与 Pod,只结束「序号 ≥ 副本数、Pod 对象已没了」的那一代(非 run 的 RETIRE、注册账号);按现状对账,不靠部署脚本发现副本数变小;冻结与排空 run 期间跳过;告警 `SqlWriterSlotRetirerFailing`。**11-12 第二轮把两条残余也修了**(未编译,验证见 `docs/PROGRESS.md` 2026-11-12「写者缩容收尾残余:部署当场收尾,金丝雀收尾做完自己删 CronJob」):缩容的那次部署当场跑一轮、等到汇总行 `pending=0` 才往下走,部署结束就能排空;金丝雀收尾做完自己删它的收尾 CronJob 与 RBAC;手工 Job 加 TTL。Go 命令、清单与部署脚本由「缺口第 22 条残余:写者名册 REVOKED 行回收」会话 11-11 写(随 `4edb25640` 入库),真库用例、告警、部署契约与文档由「缺口第 22 条残余:缩容后末代自动退休」会话 11-12 补齐(该会话已关闭,之后由前一个会话接着管)。**11-12 第三轮补归属**(改了协议,未生成、未编译,验证见 `docs/PROGRESS.md` 2026-11-12「写者缩容收尾补归属」):复查发现收尾只看「本集群里 Pod 不在」,同一份名册和注册账号被带到另一个集群(备份恢复、备用集群缩到 0)时会把正式集群在役写者全部结束;现在 REGISTER 记下登记它的 ServiceAccount 与 UID(`registered_by`,注册器运行期读,写者不用加 RBAC),收尾只结束本 namespace 登记的一代,别处登记的记 `registered-elsewhere`、不动、Job 失败并告警。Java 只登记待做 |
| 24 | 已建房票巡检 `ReadyAfter` 分片 | 「数据覆盖审计交接文档」会话(约定它改完那个文件后接) |
| 25 | 回收派发循环与 mail 过期清理的分工、循环卡死的活性指标 | **11-11 新开「缺口第 25 条残余与第 35 条:回收分工、活性指标、截断告警」**;11-11 已落码(未编译),剩 Codex(见第 25 行) |
| 29 | 网关全服前缀 watch 的扇出随 zone 数平方增长 | **11-11 新开「缺口第 29 条残余:网关健康探测的全服扇出」;同日定案落码(未编译、未测试)**:每 zone 的网关副本把本 zone 的健康汇总写进 etcd,网关只读汇总,设计 [gateway-zone-health-publish-20261111.md](../design/gateway-zone-health-publish-20261111.md);11-12 复查又改三处(定时任务各用各的线程、镜像重新同步串行化、删死配置);代码已写完,只剩 Codex 验证,命令见 `docs/PROGRESS.md` 2026-11-11「网关区服健康改为每 zone 发布汇总记录」与 2026-11-12「网关区服健康汇总收尾复查」;Java 不适用 |
| 33 | 设计稿 §9 的残余 | **11-11 已收尾(未编译)**,原落码会话「Returning scene node: drain scenes no longer mapped to it」做的:持有者记录一直读不到、丢水位那一轮两条定为接受,前者加了指标 `scene_ownership_own_holder_known` 与告警,两条的人工退路写进 etcd 运维手册 §7a;告警的 promtool 与入集群写进 Codex 命令。验证见 `docs/PROGRESS.md` 2026-11-11「缺口第 33 条残余收尾」;住户预算归压测会话 |
| 34 | 全部 Redis 上 Cluster,7 批 | 「服务器代码缺口补完」local_6cb05561(11-11 从批 1 开始) |
| 35 | mail 截断告警永远不触发 | **11-11 新开**,同第 25 条那个会话;11-11 定为删除并已落码(未编译),剩 Codex(见第 35 条) |

清单之外、11-06 之后 PROGRESS 里的「新发现」也核过:断线重连 R13 已有「审计流水与玩家快照改走事务性发件箱」会话,R6、R7 归断线重连会话,战斗直连心跳已落码,全服帮会榜负责人定为删除且已删,数据覆写审计的新发现由「数据覆写审计换视角查找第 2 轮(11-12 额度重置后)」接着跑,chat 缺 kafka-go 依赖已补,排空迁移的纵深防御确认直读 Redis。

结论:**没有全部做完。** 下面「未做」的项还在。负责人要求按最标准的方式做完;2026-11-01 暂停在这里是因为每周额度已用 94%(5 个会话共用,4 天后重置),不是判断它们不用做。

## 这一轮已补(未编译,待 Codex 验证,命令见 [progress.md](player-save-direct-commit-20261030/progress.md) §16)

| 项 | 改了什么 |
|---|---|
| 帮会幂等第 3 道查重失败 | 查询失败按故障回,不回会让客户端作废原键的业务拒绝。原来停在 worktree `peaceful-haibt-6a19a3` |
| gate 直连模式会话头 | `player_id` 在唯一构造点归一,不再带出 UINT64_MAX。原来停在 worktree `laughing-wright-5dc98a` |
| Refresh token | 宽限窗口、重用检测与家族吊销。负责人 10-22 批准,Java 版已做,Go 版停在 worktree `relaxed-cerf-d5da4d`。按主线账号单槽布局重写 |
| `TeleportRequest` | 明确回 1006,和 Java 版一致。原来回空应答,客户端以为传送成功 |
| Java 网关 `/api/hotfix-check` | 按配置比较版本。原来恒回「不用更新、1.0.0」 |
| scene 12 条 GM 回档桩 | 任何模式都回 1006。开发模式原来回空应答 |
| etcd 存活集闸门(B1-1) | player_locator:全空一直压住,骤降超过一半压住 30 拍。scene_manager:全量同步回空时不把节点全判消失 |
| 重连漏发入场基线 | 新会话复用还在目标场景的实体时,先离场再走完整入场 |
| 单 zone 容量默认值(U-2) | 5000 改 45000,对齐压测基线。Java 版记待做 |
| TiDB 开发镜像(T-2) | v8.5.2 改 v8.5.5 |
| 角色列表回落计数(X-3) | `homezone.go` 失败放行时计 `login_role_list_home_zone_fallback_total`。验证命令见 `docs/PROGRESS.md` 2026-11-01「映射直读收尾补回落计数」 |

## 未做,按影响排序

规模是估计的文件数。证据行号是 11-01 的,动手前重新核对。

### 会造成错误行为

1. **(11-01 已落码,未编译,验证见 progress.md §16「清单第 1、6 条」)scene_manager 写路径的非原子残余(R3、R5、排空)。**
   - 回滚收进脚本 `luaRollbackInstanceAllocation`:映射仍指向这次分配才认领,认领到才还名额。
   - 空闲回收脚本回显式状态,有人时在脚本里 `ZADD XX` 重置时钟;映射缺失时摘掉条目、交回 agones_gs,不再加回。
   - 三段认领脚本带 token 和 `scene:{id}:claim` 记录(TTL 600s),重发返回原快照;Eval 回错时 token 记在 `SceneClaimTokens`,同进程下一次认领接手。
   - force 认领以映射存在为前提(`absent` 模式除外)。
   - 排空的移动、期望数、标记合成一段脚本 `luaBeginDrainWorldChannel`。
   - 回归:`scene_claim_atomic_test.go`。
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「新场景登记改成一段脚本」)新场景的登记**:`allocateScene` 和建实例、铺世界频道的其余写入合成一段脚本(`scene_registration.go` 的 `luaRegisterInstance` / `luaRegisterWorldChannel`),带一次性 token,记在自己的 `scene:{id}:registration`(不与销毁认领共用记录键);EVAL 回错时用同一 token 跑 `luaAbortSceneRegistration` 问清结局,只有确认没落地、以后也不会落地才还 Agones 名额;同一 token 重放「已登记」前先核对映射仍指向原节点,被销毁或改派就回 conflict。新铸的号已有映射或别的 token 的记录时一个键都不写。指标 `scene_manager_scene_registration_total`,告警 `SceneManagerSceneRegistrationConflict` / `SceneManagerSceneRegistrationUnknown`,回归 `scene_registration_test.go`。
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「世界频道改派提交后与排空收尾」「改派 CAS 与 Agones 名额转移加重发回执」「回执改为按 token 分键」三条)世界频道改派与排空收尾(10-23 残余 R2、R4)**:init 的死节点改派改为先建后改(`moveWorldChannelOffDeadNode`),排空死属主分支过再入屏障、不扣死节点计数;改派 CAS 与名额转移每次调用一把 token 回执,go-redis 重发原样回结论,Eval 回错时用同一 token 问清结局并作废(`luaResolveCasReceipt`)。回归 `world_channel_commit_order_test.go`、`world_channel_cas_replay_test.go`。11-06 补:改派落空后回收预建实体先上了 60 秒回收栅栏,当天下午换成放置代次(`docs/design/world-channel-placement-generation.md`,PROGRESS 2026-11-06「世界频道放置代次」):proto 的 CreateScene / DestroyScene 加 `placement_generation`,scene_manager 每次换节点分配代次、回收先抬高水位,C++ 节点按代次忽略晚到的旧拆除、拒收被回收放置的晚到建场景;超过 60s 才送到的 DestroyScene 也拆不到之后合法改派过来的实体。回归 `world_channel_placement_generation_test.go`、`cpp/tests/cross_zone_test/scene_placement_fence_test.cpp`。同日再补:显式 DestroyScene 拒绝拆世界频道(以前按副本拆,删映射却不摘在役集合,频道被懒改派复活,不带代次的那次拆除晚到会拆掉复活的实体)。剩下的是孤儿实体(建场景超时晚到、改派者预建后崩溃),由第 33 条节点归属对账收。
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「场景拆除任务」)认领之后的副作用不再丢**:认领脚本在提交的同一步写拆除任务 `scene:{id}:teardown` 并进本 zone 的待办 `scene_teardown:zone:{z}:pending`,认领方当场做副作用、做完才删;没做完的(进程退出、回包丢失、某一步失败)由领导者扫描在 90s 租期后重做。scene_count 与属主反向索引挪进认领脚本同一步改;Agones 归还按任务至多一次。11-01 的进程内接手 `SceneClaimTokens` 随之删除。设计见 `scene-creation-architecture.md`「拆除任务」,告警 `SceneManagerSceneTeardownStuck`。
   - **(11-10 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-10「Agones rooms 名额改为按身份的 List 条目」)Agones 名额收敛**:原来 rooms 是计数,三个来源会多记、收不回来(预占在登记之前崩溃;拆除任务的归还「至多一次」,置位之后退出或调用本身失败),改派时新 GameServer 满了还会让新节点少记;对账只告警。
     - 做法(设计见 `agones-scene-node-high-density.md` §13):rooms 改为 Agones List,每次占用一个按身份的条目(scene_id 加随机串),记录 `scene:{id}:agones_gs` 写「GameServer/条目」;要名额之前先写在途占用 `agones_rooms:zone:{z}:holds`,登记与改派转移的脚本在同一步消费它,对账只取消过期的在途占用(取消之后晚到的提交被挡住,回 `fenced`);增删按条目幂等,拆除任务的归还改为失败重做到成功;对账逐条判,两个方向都收敛(删孤儿、取消过期在途占用、补缺失条目、把改派时没转过来的名额转过来)。
     - 11-10 先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话写了设计初稿;同日负责人让「场景销毁认领:回滚原子化与重发幂等」会话现在做,落码时改了两处(条目每次占用一个、不用裸 scene_id;在途占用代替「两轮加宽限期」),原因写在 §13.7。
     - **仍剩**:真集群上的三条假设没核对(GSA 列表字段形状、直接 Update 改 `status.lists`、FleetAutoscaler 的 List 策略,§13.8);GameServer 满了时补不上、转不过来的只能告警(`SceneManagerAgonesRoomsNotConverging`)。根因是世界频道改派(rebalance、懒改派、死节点改派与自愈)挑目标不看容量、事后才转名额,目标满了那台就被超卖一份。**(11-11 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-11「世界频道改派前先占名额」)** 负责人让「场景销毁认领:回滚原子化与重发幂等」会话在这里做:改派与新建一样「先占名额再搬」,换更好的家满了不搬,属主已死时满了交给 GSA 另挑一台,哪台都满就不搬(与新建频道 fail-closed 同一取舍),设计写在 §13.10。之后对账「转过来」只收进程在改派 CAS 与名额提交之间退出留下的那一种。
   - **验证状态(11-10 核对)**:Codex 11-06 全量跑过 scene_manager(build、vet、`./internal/logic/...` 756 PASS,见 PROGRESS 同日「修正 scene_manager 五项旧夹具」),本条各批 Go 用例都在内;11-08 命名统一后相关检查也通过。各批 PROGRESS 条目写的变异取红都还没跑。
   - **(11-06 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-06「销毁路径只改本 zone 的键」)销毁只改本 zone 的键**:认领脚本在同一步比对 `scene:{id}:zone`,不是认领方的 zone(或记录损坏)就一个键都不动;镜像级联与源迁移级联跳过登记在别的 zone 的子镜像;镜像只从本 zone(或本实例里没有 zone 记录)的源的子集合摘自己;显式 DestroyScene 只接本进程 zone 的请求,`zone_id` 没填按本进程的 zone。设计见 `scene-creation-architecture.md`「销毁只改本 zone 的键」,回归 `teardown_zone_scope_test.go`。
   - **仍剩(11-06 补记)**:建实例回滚补发的 DestroyScene 若早于节点处理完在途的 CreateScene,实体仍会留在节点上、没有映射。scene_manager 一侧收不了,由第 33 条「节点侧按 `scene:{id}:node` 对账」收:节点连续两次读到映射不是自己、过 60 秒宽限就排空销毁(11-06 已落码;11-08 Codex 命名统一那轮 `game.sln` 增量构建通过,`cross_zone_test` 里它的 `SceneOwnership*` 20 条与放置代次 `ScenePlacementFence.*` 8 条全 PASS,日志在 `build/verification/naming-audit-20261108/cpp-tests/cross_zone_test/`;真机冒烟没做)。

2. **(11-05 已落码,未编译,验证见 progress.md §16「清单第 2 条后半」)交易卡死资产指令的终结工具和 Manual 合同的恢复入口。**
   - 终结工具:`go/trade/cmd/assetopfix`(11-01)。
   - 恢复入口:`go/trade/cmd/commercefix`(list、show、resolve),动作 resume、reissue、refund,只改合同行,新腿由引擎发号。转人工时把原阶段和原因记进 workflow。设计见 `jubaozhai-market.md` §15.7,手册 §4.2。
   - 残余:原主已换、部分发放、竞价与拍品互相引用、已部分成交的市场单没有工具出口,要人工判定;没有 RPC 和管理后台。**11-11 起由「交易 Manual 合同残余处置(缺口第 2 条)」会话接残余。11-12 代码全部落完,未生成协议、未编译、未测试**,验证见 `docs/PROGRESS.md` 2026-11-12「交易转人工合同四类残余的处置出口与管理 RPC」,设计 `jubaozhai-market.md` §15.7.1:
     - 原主已换:引擎当场转人工(原因 `owner_changed`),人工 `redirect` 改绑到原账号名下的另一名角色;
     - 部分发放:`partial` 按人认定的缺口补发,托管扣款部分扣出按扣到的退回;
     - 竞价与拍品:拍品不引用的出价可以 `refund`,`void-auction` 同一事务退两张合同;
     - 已部分成交的市场单:`refund` 只退剩余部分,成交子单转人工不再连带母单;
     - 管理 RPC `TradeAdmin.ListManualContracts` / `ResolveManualContract`(serviceauth 调用方 `ops-commerce`、核对环境名、缺省关闭),`commercefix` 改为只调它们;
     - 顺带修:`refund` 跑两次会丢托管;成交子单结案只唤醒买方母单。
   - 仍剩:审批流(要管理后台的登录身份);官方商城与人民币寄售的人工处理要和商城订单一起做;部分扣出的实例、宝宝只能按 `player_snapshot` 人工补发。Java 只登记待做。

3. **(11-05 剩余部分也已落码,未编译,验证见 progress.md §16「清单第 3 条剩余与第 6 条」)审计快照管道(X-4、L-07)。**
   - 11-02:C++ 生产者 `message.max.bytes` 16MiB、lz4;快照 topic 的 `max.message.bytes` 16MiB;发送失败带 `metric=audit_snapshot_send_failed`;`store.RunAuditRetention` 分批删过期行(快照 30 天、流水 180 天),索引、指标、告警。
   - 11-05:`game_config.yaml` 的 `audit_snapshot` 段(进场、离场各自开关,按 player_id 稳定抽样,缺省全开全量),判定在 `modules/snapshot/audit_snapshot_policy.h`,K8s node-config 整段复制;`k8s_deploy.ps1` 的 `-AuditKafkaRetentionBytes` 渲染进 data-service 并计入 Kafka 盘预算,合同测试 `audit_pipeline_deploy_contract.tests.ps1`。
   - 残余:`-AuditKafkaRetentionBytes` 三档缺省都沿用 broker 缺省,值要按压测的审计写入速率和要扛住的消费中断时长定(`deploy/k8s/README.md`「审计 topic 的 retention.bytes」),定之前积压超过 broker 缺省时未消费的审计消息仍会被删。
   - **11-11 定案并落码(未运行,「缺口清单压测项:统一压测方案与定案」)**:目标定为消费端中断 2 小时不丢;按容量模型一个 45k zone 峰时单分区进水约 0.5MB/s,prod / prod-like 缺省 4GiB(dev 沿用 broker 缺省),prod 盘预算 40Gi → 80Gi;多 zone 按 zone 数等比加审计分区;账本行加 `audit_retention_bytes`;`DataServiceAuditLagHigh` 阈值 520 → 4000。改了 `k8s_deploy.ps1`、两份契约测试、`data-service-audit-alerts.yaml`、`deploy/k8s/README.md`、审计 topic 运维手册。验证见 `docs/PROGRESS.md` 2026-11-11「缺口清单压测项定案」。

### 挡水平扩展

4. **(11-05 已落码;Go 11-06、C++ 11-08 已由 Codex 跑通;11-12 修掉设计稿 §6 第 1 条(进场时大世界没频道当场补铺、跨 zone 预检不算排空节点),这一轮未运行;部署契约与 kind 冒烟未跑,见 [control-plane-zone-scoping-20261105.md](../design/control-plane-zone-scoping-20261105.md) §6、§8)控制面没有按 zone 化(B0-5 A1),30~45 个文件。**
   - 落码:C++ zone-scoped 类型(含 SceneManager)只 Range / watch 本 zone 前缀,兜底全量 Range 2s → 60s±20%,watch 取消按前缀退避重拉;gate 10s 写 etcd 保留(它是第 7 条停写判定的心跳),扇出随监听方收窄到本 zone;scene_manager 按 zone 选主(`scene_manager:leader:lock:zone:<z>`)、只 watch 本 zone、领导者循环只处理 `ZoneId`;别的 zone 的持有者判定改为 etcd 直读加本副本首次观察屏障;K8s 上 go-zero 发现键按 zone 加 `.z<zone>`。残余见设计稿 §6。
   - 以下是 11-01 的原文。
   - C++ 每 2 秒全前缀 Range(`service_discovery_manager.cpp:16`、`:29-32`),gate 每 10 秒把人数写 etcd(`cpp/nodes/gate/main.cpp:559-563`)。
   - scene_manager 全服一把锁、全服 watch(`scene_manager_service.go:94-97`、`load_reporter.go:31`、`:886`),领导者循环跑全部 zone。
   - scene 和 gate 连接全部 scene_manager 实例(`cpp/nodes/scene/main.cpp:107`、`cpp/nodes/gate/main.cpp:371`)。
   - 后果:etcd 读量约按进程数平方增长,容量模型算 S2 档是参照值的 6.9~134 倍(`docs/design/capacity-model-hok-scale.md:449-457`)。收窄 watch 前要先改 `load_reporter.go:197-198` 的判定。

5. **(11-05 已落码;11-10 修两处缺陷并补齐连接总账与 redis-split-status,这一轮未运行;部署契约与 kind 冒烟未跑,见 [control-plane-zone-scoping-20261105.md](../design/control-plane-zone-scoping-20261105.md) §4、§7 末尾、§8)控制域 Redis 全服单主,15~25 个文件,依赖第 4 条。** **11-12 口径**:负责人 11-10 要求全部 Redis 上 Cluster,「按 zone 组分实例」只是过渡形态,本条要等控制域上了 Cluster 才算做完。剩下的在第 34 条:2b 批(scene_manager 换 UniversalClient)、第 3 批(C++ 控制集群句柄,已有会话认领)、第 6 批(部署 `redis-control`、退役 `redis-ctl-g<N>`)。「控制面按 zone 化」会话已向第 34 条负责会话提出接 2b 与第 6 批,等它回复分工。
   - **11-10 负责人改口径**:「我要分布式水平扩展」。按 zone 组分实例不算完成,控制域也要上 Redis Cluster,见第 34 条。本条的不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」照用,它是按 zone 打 tag 的前提。
   - 11-10 修的两处缺陷:K8s 没建玩家域集群时,scene_manager 的玩家域会回落到控制域实例(改为回落到 `CoreRedis` 指向的核心);首次部署会被控制域入口闸拒绝(`$null` 被转成空串)。
   - 落码:选「按 zone 组分实例」,不上 Cluster(理由见设计稿 §4.1)。控制域实例 `redis-ctl-g<N>`,组号沿用 zones 配置的 `redisQueueGroup`,模板与形态规则复用排队实例,不受 `-RedisSplit` 控制;键名不改,选主锁与死节点收尾任务表改按 zone,zone 登记集合删除;新不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」(跨 zone 预检改用 etcd、别的 zone 的成员集合不写);C++ scene 新句柄 `scene_control_redis` 读分线目录与神秘商人,落点唤醒 PUBLISH 留在核心;gate 排空标记经 `CoreRedis` 读核心。没有跨 zone 的原子操作(设计稿 §4.4 逐脚本核对)。`player-data-tidb-scaling-plan.md` §8.3 与 progress.md「已知残余」的说法已改正。残余见设计稿 §6。
   - 以下是 11-01 的原文。
   - `k8s_deploy.ps1:3819` 只拆了排队;`:10175-10176` scene-manager 所有 zone 都连 `redis.<infra>`;`scene_atomic.go:123-129` 的键没有 hash tag。
   - 所有 zone 的场景路由、世界频道、成员集合和选主锁在一个主节点上,加机器不能扩写入,这个主节点故障时全服进不了场景。
   - progress.md「已知残余」和 `player-data-tidb-scaling-plan.md:447` 写的「按 zone 组分实例」与代码不符。

### 删除和小项

6. **(11-05 收尾:重新生成已做,残留的 `player_rollback_service_metadata.h` 和两条工程条目已删,未编译)scene 12 条 GM 回档桩的 proto 删除。** 同时删了只被它们引用的 `SnapshotDiff*`。生成器不清退 `cpp/generated/rpc/service_metadata/player_rollback_service_metadata.h`(`rpc.vcxproj` 的条目也不管),没有编译单元包含它,无害;重新生成之后由 Claude 删掉它和两条工程条目。当时列的文件:`proto/scene/player_rollback.proto`、`proto/message_id.txt`(102、103、104、106、107、109、110、112、113、115、116、117 改 `reserved:`)、`cpp/nodes/scene/handler/rpc/player/player_rollback_handler.{h,cpp}`、`scene.vcxproj`、`.filters`、`CMakeLists.txt`、`player_service_interface.cpp`、`scene_handler.cpp`、`player_gm_guard.h`、`gate_gm_client_messages.h`、`player_inbox_system.cpp`、`cpp/tests/currency_test/client_gm_gate_test.cpp`、`tools/proto_generator/protogen/internal/client_rpc_kind{,_test}.go`。然后重新生成。
7. **(11-05 已落码,未编译,需先重新生成协议)gate 上限和新鲜度(L-2)。** login 挑 gate 前剔除停写的 gate(三个上报周期没重写 etcd 记录,按 ModRevision 判,与镜像新鲜度分开)和人数到自报 `max_connections` 90% 的 gate,全部不合格时回落全集并计 `login_gate_pick_fallback_total`;`FreeSlots` 不变。NodeInfo 加 `max_connections = 13`,由 C++ gate 启动时写入。设计见 `gate-load-balancing-design.md`「满载与停写的 gate」,验证命令见 `docs/PROGRESS.md` 2026-11-05「gate 停写与满载过滤」。
   - (11-05 残余也已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「gate 停写与满载过滤的残余」)scene_manager 跨 zone 重定向同样剔除这两类 gate:满载读 NodeInfo 的 `max_connections`,停写在本进程按每次直读到的 ModRevision 记历史判(`gatepick.RevisionTracker`);排队取走的两个签票闭包合成 `loginqueue.NewAdmitSigner`,签之前复核放行时选定的 gate,停写、满载或已不在候选里就重挑,读不到候选照签。剔除与回落的规则挪进 `go/shared/gatepick`,四处共用。
   - (11-05 又补,规则未过 promtool、未部署,验证见 `docs/PROGRESS.md` 2026-11-05「挑 gate 回落告警」)两条 fallback 指标加了告警:`deploy/k8s/login-alerts.yaml` 的 `LoginGatePickFallbackStale`(critical)、`LoginGatePickFallbackDegraded`(warning;11-06 由 `LoginGatePickFallbackFull` 改名,见下一条),`scene-manager-alerts.yaml` 的 `SceneManagerGateRedirectFallback`(warning);静态契约 `tools/scripts/tests/gate_pick_alerts_contract.tests.ps1`。
   - (11-06 复核后改,未编译,验证见 `docs/PROGRESS.md` 2026-11-06「挑 gate 排空并入分档」)排空并入 `gatepick` 的一次分档:完全合格 > 连得上的(排空或满载)> 全部(全部停写时)。此前排空在调用方先剔、停写与满载再剔,两层各自回落,「没排空的全部停写、只剩排空中的健康」会把票签给卡住的 gate;login 的排空还在 `candidatesFromNodes` 就剔掉,`FreeSlots` 少算排空 gate 上的在线人数、排空期间多放人。现在候选只打 `Draining` 标记、容量照数。另加 `go/shared/gatepick/report_period_crosslang_test.go` 钉住 C++ gate 每 10s 无条件重写 etcd 记录(停写判定的前提)。
   - 仍剩:scene_manager 第一次重定向到某个 zone 时判不出停写(只有本进程的历史);Java 版三项都记待做(11-06 补:Java 的 `GatePicker` 也按同一分档做)。
     - (11-10 已落码,未编译、未测试,验证见 `docs/PROGRESS.md` 2026-11-10「跨 zone 重定向读 login 发布的停写标记」)目标 zone 的 login 把 `NodeWatcher` 判出的停写写成带 30s 过期的 `gate:<node_id>:stale`(核心 Redis,不主动删),scene_manager 读排空标记的那次 MGET 一并读出,与本进程历史任一成立即算停写;两个键名收进 `go/shared/gatepick/marks.go` 只定义一份。设计见 `gate-load-balancing-design.md`「满载与停写的 gate」的「停写标记」一条。剩 Java 版三项待做。
   - (11-11 已落码,未编译、未测试,验证见 `docs/PROGRESS.md` 2026-11-11「挑 gate 记本进程在途票,补两条 gate 健康告警」)设计稿「还没做」里的两条也做了:快路径跨请求、dispatcher 跨拍都按「上报人数 + 本进程在途票」比负载(`loginqueue/gate_inflight.go`,窗口取交票宽限 20s);停写 gate 持续被剔除、排队取走复核持续读不到候选两条只进看板的指标补了告警(`login-alerts.yaml` 的 `login.gate-health` 组,契约测试同步)。本条只剩告警的 promtool 与部署演练、Java 版待做。
8. **(11-05 已落码,未编译)连接池指标(L-08)。** 新增 `go/shared/sqlpoolstats/register.go`(`Register` / `Single` / `PrimaryPool`),chat、friend、guild、mail、match、rank、trade 在入口各注册一个 `primary` 池,login 只在打开 PasswordAuth 时注册 `password_auth` 池;指标同为 `mmorpg_sql_pool_*{sql_client,pool}`,`mysql-connection-alerts.yaml` 的 sql-pool.client 规则按 `(namespace, sql_client, pool)` 分组,新服务不用改规则。Java 网关加 `micrometer-registry-prometheus`,池名固定为 `gateway`,只在 K8s 管理端口 8082 的 `/actuator/prometheus` 上暴露(ConfigMap 打开 exposure,抓取注解由 `k8s_deploy.ps1` 与口令指纹一起生成),告警是新组 `sql-pool.gateway`。player_inbox 按它自己的设计继续导出 `go_sql_*`。验证命令见 `docs/PROGRESS.md` 2026-11-05「连接池指标与压测工具前置」。
9. **(11-05 已落码;scene_manager 用例在 11-06 Codex 全量里全过,player_locator 与 liveview 单测还没有跑,见 `docs/PROGRESS.md` 2026-11-10「核对存活集闸门(B1-1)与缺口第 9、32、33 条的验证证据」;原验证命令见 2026-11-05「etcd 存活集闸门定阈值并补齐 scene_manager」)scene_manager 存活集残缺。** 这一轮只拦了全空,部分缺失还会按消失处理。
   - 阈值已定(决策清单 B1-1):空集一直压住;基线不少于 4 个且缺席过半就压住,缺席数连续不变满 T 才放行,变了从头计时。T:scene_manager 60 秒,player_locator 30 分钟。恢复口径:scene_manager 缺席清零才复位,player_locator 不再过半就复位(它每个 gate 还有两拍去抖)。状态机 `go/shared/liveview`,两处共用。
   - scene_manager:fullSync 按 zone 比「旧镜像 ∪ 领导者读到的负载集」(`node_view_gate.go`),补上进程刚启动、镜像为空时比不出骤降的洞;陈旧清扫用闸门同一份负载集读数,zone 范围只经 `nodeViewZones`(按 zone 选主落地时改成只本 zone)。
   - player_locator:11-01 版的压住计数会被让出分片的空拍清零,上限永远到不了,改为按时间计、压住不算失败、不让出段。
   - 两边各加 Held / Released 两条 critical 告警。watch 的 DELETE 风暴不过闸门,因为那是 etcd 里真实发生的删除。由此引出的拆实例问题已经分别收掉:
     - 同一进程 1~2 秒内按原 node_uuid 重新注册:第 32 条撤收尾任务,R15 由领导者同一步清 `death_at`;
     - 号被新进程接走:R15 保留老实例的屏障;
     - 失联超过屏障才回来:由第 33 条的节点侧归属对账收。
     - 都未编译。
10. **(11-05 已落码,未运行)压测工具前置。** `stress_snap.ps1` 缺省加抓 :9170(`t<N>m_match.txt`)与 :9190(`t<N>m_pl.txt`),这两个后缀进 Redis target 保留名;`stress_summarize.ps1` 新增 G1 / G2 / G3(按池撮合总览、按范围层级的成局与等待、按模式)和 PL1(player_locator 对账与服务端 RPC),L1 末列加 `bgz_fb` = `login_role_list_home_zone_fallback_total`,新参数 `-MatchRoundIntervalMs`(缺省 500);robot 新增 `join_queue` 动作与 `match-stress` 档(先地域探测再入队,排满 `match_queue.dwell_seconds` 撤单,成局后等这局结束再排;stress 档权重不变),配置样例 `robot/etc/robot.match-stress-100.yaml`。取值数组由 match、scope、player_locator 三处 Go 钉名测试守住。现状基线还没跑(要 Codex 按 AGENTS.md §6.2 跑一轮)。
11. **(11-02 已加 `SceneManagerDeadNodeReconcileAbandoned`,未部署验证)`dead_node_reconcile_total{outcome="abandoned"}` 没有告警规则。**
12. **(11-02 已删,未编译)login 死配置。** `TaskWaitTimeout`、`LoginTotalTimeout` 从 `config.go` 和 3 份配置里删掉。
13. **(11-05 已落码,未编译,验证见 [mixed-version-persist-handoff-20261019.md](mixed-version-persist-handoff-20261019.md) §17)只在混版本时触发,上线前不阻塞。** 字段台账的机器门禁改为发布前封存:台账加 `sealed=` 行,生成器每次核对已封存世代不变,发布预检 `release.persist_schema.sealed` 要求封存到最大世代(设计 §3.1「封存」);运行期字段集合指纹没做,理由见设计 §13 第 11 条。战斗结算遇到不认识的币种改回 `kVersionSkew`,坏载荷照旧 `kDefer`。

### 全链路水平扩展盘点(2026-11-05,只读)

**11-06 进度(会话「服务器代码缺口清单」)**:第 17、18、27(活动导入)、28(选点)、29、31 条已落码,都没有编译,验证命令见 `docs/PROGRESS.md` 2026-11-05「排行榜读写按名次索引走」「缺口清单第 27、28、29 条落码」「缺口清单第 31 条落码」三条。还没人做的是 16、19、20、21、22、30 和 27、28 的另一半,这一轮没动的原因写在各行:16 所在的 trade/commerce 正被两个数据覆写审计会话和「服务器数据一致性问题」会话审;21 的 data_service 有多个会话的未提交改动,读池还要新的只读账号;20 要先在压测里看热点;19、22、30 和 28 的「连全服全部目标」要先定设计(19 的推送要动客户端协议,22 的名册复用动防僵尸写者的保证,28、30 要做确定性子集或按 zone 部署);30 里 `node_command_topic.h` 另有会话在改。

**11-06 续(同一会话,写设计稿)**:负责人要求把剩下的设计级条目写成设计稿。已写第 30 条与第 28 条后半([connection-fanout-subsetting-20261106.md](../design/connection-fanout-subsetting-20261106.md))。每周额度到 80% 停在这一批的边界,当时还没写的第 15、16、20、21、22 条,之后都已由别的会话接手(**11-10 核对**:15、16 已落码,20 测量指标已落码,21 收尾归「服务器代码缺口补完」,22 由「缺口第 22 条:写者名册回收」按 writer-roster-exhaustion-20261106.md 落码,状态以各行为准),本段列出的已无遗留。第 15 条那句「本会话接」查不到是哪个会话写的,实际没人在做;第 14 条 11-06 起归另一个「服务器代码缺口补完」窗口,第 19 条归同名的另一个窗口。

按「给这个组件加机器,吞吐能不能跟着涨」逐环节核对当前代码,三路并行读,行号是 11-05 的。「抽查」= 本会话又按行号读过一遍;其余是盘点代理读到的,动手前重新核对。第 4、5、7、9 条已有专人,下面不重复;与它们相关的新方面单独标出,已转给对应会话。

**确认能扩、不用改**:go/db 存盘(每 zone 2 副本,只锁单个玩家的写权行,每个 Pod 有自己的 writer_key);发号 `AllocateIdSegment`(内存切段,用完才对 etcd CAS 一次);主键全是打散过的号,目录生产 64 片;C++ battle 房间状态只在进程内、选点随机二选一;friend 推送单收件人;C++ 节点必需密钥在 Deployment 与 Fleet 两种形态都注入了(「服务器代码缺口补完」11-05 核)。

**阻塞:加机器不涨吞吐**

| # | 环节 | 问题 | 证据 | 修法方向 | 归属 |
|---|---|---|---|---|---|
| 14 | login 排队(开排队时) | 全部 zone 的排队在一台 Redis(`*redis.Client`,不支持 Cluster,脚本注释 (g));每 zone 两把热键 `queue:zone:<z>`、`admitted:zset:zone:<z>`;每次轮询 GET + GETDEL + EVALSHA;每 zone 一个放行 leader,`PopAdmit` 对本拍成员逐条串行跑脚本,放行速率约等于 1 / Redis 往返 | `loginqueue/queue.go:200`、`:271-272`、`:413`、`:912`、`:751-777`(抽查)、`dispatcher.go:63` | 放行按批流水线或一段脚本放一批;名次改用入队序号差加进程内缓存,少一次往返;排队实例按 zone 组分开 | **11-06 由「服务器代码缺口补完」会话接手并落码(未编译,验证见 `docs/PROGRESS.md` 2026-11-06「login 排队放行流水线与轮询一次往返」)**:放行第一条单发探路、之后每块 128 条一个 pipeline;轮询的 token 核对、取放行槽、读状态与续期合成一段 `lookupScript`,三次往返降到一次。排队实例按 zone 组拆分本来就有(`-RedisSplit queue`,首次上线显式开),没改。名次改入队序号差没做:它改客户端可见的名次语义,等压测证明 redis-queue 扛不住再做(`login-queue-2026-05.md` §14 第 10 条)。只改了 `loginqueue/{queue,gatetoken}.go` 与测试,`dispatcher.go`、`loginpregate` 没动。**11-11 定案(「缺口清单压测项:统一压测方案与定案」)**:名次改序号差不做(只省一条 ZRANK,却把名次变成上界);排队压力来自轮询次数,改为服务端按名次拉长 `RetryAfterMs`(前 2000 名 2s,之后与名次成正比,上限 min(30s, AdmitTTL/2)),已落码未编译(`loginqueue/retry_after.go`,三处回包),10 万人时轮询约降到八分之一;这一项原排在第 34 条第 4 批,已提前做完。Java 记待做 |
| 15 | 审计流水链路 | 全服一个流水库;流水 topic 默认 6 分区、快照 3 分区,不许原地扩,消费者最多 6 / 3 个;流水每进程一个循环同步落库,快照默认同时只落一批;`timestamp_sec`、`created_at` 等单调索引让全服追加写打在同一个尾部 Region;清理时各副本删同一批最老的行 | `data_service/internal/config/config.go:489`、`:495`、`:555`(抽查)、`kafka/txlog_consumer.go:94-149`、`store/audit_retention.go:55`、`proto/common/database/rollback_database_table.proto:20,68` | 放开扩分区(落库前已按 tx_id 重排并 IGNORE,扩分区不破坏正确性,`store/transaction_log_store.go:118`);按分区并行落库;索引加分桶列或按天分区,清理改 DROP PARTITION | 本会话接(与第 3 条的保留与采样分开,不碰 player_lifecycle.cpp 与 Kafka 渲染段);**11-06 核对:写这句的会话查不到、多半已结束,实际待认领**,设计稿也还没写(见本节开头 11-06 续);**11-06 由「服务器代码缺口补完」会话认领(负责人要求第 15 条以后都做完),先写设计再落码**;**11-06 已落码(未编译,需先重新生成协议;验证见 `docs/PROGRESS.md` 2026-11-06「审计流水链路水平扩展」)**,设计见 [audit-pipeline-horizontal-scale-20261106.md](../design/audit-pipeline-horizontal-scale-20261106.md):两个审计 topic 只许加分区、少了原地加(`kafkautil.TopicSpec.AllowPartitionGrowth`,四处预建同改);流水每分区一个 worker 并行落库(与快照共用 `kafka/partition_workers.go`,`Kafka.TransactionLogMaxInflightFlushes`);两表加 `shard` 列,按时间的索引都在时间列前插 `shard`;清理每个 (表, 桶) 一把 `GET_LOCK`,各副本各删各的桶。**没做**:`snapshot_guid` 及其唯一键的分桶(要连去重路径一起改,先看压测热点)、按天分区 + `DROP PARTITION`(proto2mysql 表达不了,唯一键也要带分区列)。**11-11 定案,两项都不做(「缺口清单压测项:统一压测方案与定案」)**:`snapshot_guid` 早已是发号器 snapshot 号段经 `MixGuid` 打散的号、不单调,没有尾部热点,「按 SnowFlake 单调」的前提不成立,只改正了 proto、`snapshot_store.go` 与设计稿的过时注释;按天分区不做,清理已按桶随副本线性扩,分区要改建表、唯一键、加分区维护任务、按玩家查询读放大几十倍,理由写在设计稿 §2.D。Java 记待做。**11-10 核对验证证据**:单测已由 Codex 11-08 跑过且全过(`build/verification/naming-audit-20261108/`);真库集成用例(MySQL / TiDB)、部署脚本契约与 compose 冒烟还没跑,清单见 `docs/PROGRESS.md` 2026-11-10「核对第 15 条(审计流水链路)的 Codex 运行证据」 |
| 16 | trade 合同推进 | 每个副本一个协程逐行跑全局 `Due` 的前 100 行,各副本读同一个队头靠行锁互撞;`advance` 每次都 `Save(updated_ms=now)`,挂单被反复轮询,新成交排在全部活跃合同之后 | `trade/internal/commerce/engine.go:918-953`、`data/commerce_repo.go:50`、`:165`、`svc/assetchannel.go:474-505` | 按 commerce_id 分片归属;没变化不重存;按事件唤醒 | **11-06 已落码(未编译,「Turn battle gap closure handoff」会话)**,设计见 [jubaozhai-market.md](../design/jubaozhai-market.md) §15.8,验证见 `docs/PROGRESS.md` 2026-11-06「trade 合同推进按片分工」:合同按 `commerce_id % 64`、收据 ACK 按 `op_id % 64` 分给本 zone 的 stable 副本(新包 `go/shared/workshard`,条件在 `LIMIT` 之前);推进一步没有变化就不重存、不改版本,只等时间或玩家操作的排到截止(最远 1 小时),其余仍每轮复查;撮合失败唤醒下一轮重撮。**11-10 唤醒也已落码(未编译,同一会话)**:资产指令终结(`Finalize` / `Abandon` / 人工终结)提交后唤醒所属合同,成交子单收尾后唤醒母单;在途腿与等子单的母单改为 30 秒兜底复查;设计见 §15.8 第 3 点,验证见 `docs/PROGRESS.md` 2026-11-10「第 16 条按事件唤醒」。**仍剩**:Java 待做。**11-10 核对已收拢**:打分只剩 `shared/workshard.Owner` 一份(11-06 随 1194c4d46 改为 SHA-256,match 的 `poolowner` 与 player_inbox 的 `shardowner` 都调它);成员表与「没就绪时做全部还是跳过」按设计由各服务自己定,不再加开关 |
| 17 | rank 写 | 同一张榜的每次 Submit 都先 `SELECT … FROM rank_board … FOR UPDATE` 再给 `committed_cursor` +1,同榜写入全部串行 | `rank/internal/data/repo.go:146`、`:222`(抽查) | 游标挪到 (榜, 片) 行,封榜时求和 | **11-05 部分落码(未编译,需重新生成协议)**:整批一次读、一次多行写,持锁时间从两百多次往返降到常数次;同榜写入仍串行,按片拆游标行的方案写在 `leaderboard-system.md`「2026-11-05 名次读写走索引」残余第 1 条,等压测证明不够再做。**11-11 按片拆游标已落码**(负责人「不用等压测数据了,先写完」;未生成协议、未编译,「Turn battle gap closure handoff」):新表 `rank_board_shard`,`Submit` 只锁本片片行,封榜、移除先锁榜行再按片号升序锁全部片行,同一实体跨片靠分数行锁串行、新实体撞唯一键整笔重试;设计见 `leaderboard-system.md` 文末「2026-11-11 同榜写入按来源片并行」,验证见 `docs/PROGRESS.md` 2026-11-11「缺口清单第 17、20、25 条不等压测直接定案落码」;Java 待做 |
| 18 | rank 读 | `Page` / `Around` 每次都对全榜做 `ROW_NUMBER() OVER` + `COUNT`,开销随榜长线性增长,全压在库上 | `rank/internal/data/read.go:54-63`、`:111-113`(抽查) | 名次用索引范围计数,首页与热门页缓存;大榜考虑定期快照 | **11-05 已落码(未编译,需重新生成协议)**:`order_key` 把升降序统一成索引方向,总数取榜行 `entries`,分页顺着索引取,本人名次用索引范围计数,封榜取前 N 也走索引;名次语义与客户端契约不变。残余:深名次的计数与深页 OFFSET 仍随名次增长(同一文档残余第 2、3 条);Java 记待做。**11-11 定案并落码**(负责人在两条路里选「前 N 名精确、之后显示 N+」;未生成协议、未编译,「Turn battle gap closure handoff」):N = 10000,本人与附近名次最多数 N(附近 N + radius)行,翻页只到第 N 名,排在其后的条目 `rank = 0`、`beyond_rank_limit = true`,回包带 `rank_limit`;客户端显示「10000+」;robot 冒烟按新口径判在不在榜上。设计见 `leaderboard-system.md` 文末「2026-11-11 前 N 名精确、之后显示 N+」,验证见 `docs/PROGRESS.md` 2026-11-11 同名条目。11-11 起总数改取各片行之和(第 17 条)。Java 待做 |
| 19 | chat 世界频道 | 没有推送扇出,全员靠拉 SQL;全服只有一个 `stream_key = "world-v2"`,读写挤在 `idx_chat_stream` 同一段尾部 | `chat/internal/authority/store.go:174`(抽查)、`:337`、`schema.go:29`(抽查) | 按 gate 批量推送 + 近窗内存缓存;流键按时间分桶 | **11-06 第一批已落码(未编译,验证见 `docs/PROGRESS.md` 2026-11-06「世界频道每副本近窗快照」)**:每个 chat 副本持有世界频道最新一窗的快照,过期才重读、重读单飞,拉取语义与直读共用一份代码,库的读量与在线人数无关,加副本即扩吞吐;本副本提交立刻可见,跨副本最多滞后 `WorldHistoryCacheMs`(缺省 500ms)。设计见 [chat-world-channel-fanout.md](../design/chat-world-channel-fanout.md)。**11-06 第二批推送扇出也已落码(未编译,需先重新生成协议;验证见 `docs/PROGRESS.md` 2026-11-06「世界频道推送扇出」)**:新下行方法 `NotifyWorldChat`;chat 首次提交后写一条 `BroadcastToAllEvent` 进 `chat-world-bcast_g<N>`(4 分区,四处预建);每台 gate 读全部分区、只认这一个消息号,按每台每秒 10 条、突发 20 条的预算推给本机会话,推送开销与全服聊天量无关;推送至多一次,真相仍是拉历史;客户端改动交 Codex。按时间分桶不做:只读最新一窗,分桶不减写热点(理由见设计稿 §5)。Java 记待做。**11-11~12 第三批与客户端已落码(未编译、未在 Unity 里运行,「服务器代码缺口补完」;验证见 `docs/PROGRESS.md` 2026-11-12「世界频道第三批」)**:设计稿原先留作「以后再说」的三条都做了——世界频道按人的发言间隔(`WorldChannelCooldownMs` 缺省 5 秒)、原件表用 TiDB 的 TTL 保留 30 天(新列 `accepted_at`,运行账号仍无 DELETE)、世界流键按发送者分 8 段(`world-v3:<k>`,读时合并);gate 转发预算挪成独立头并补用例;Unity 客户端订阅 `NotifyWorldChat`、历史与推送合并去重。**要清 chat 库**(表结构、流键、策略行版本都变了)。Java 三批都待做 |

**部分:能扩,但有放大或上限**

| # | 环节 | 问题 | 证据 | 修法方向 | 归属 |
|---|---|---|---|---|---|
| 20 | 写者闸门 | 每笔事务读 `route_meta(id=1)` 两次,并按实例轮转锁或改写 64 个 `writer_slot` 行之一;所有实例的事务都改写这张小表,TiDB 上可能是单 Region 写热点;玩家档每 zone 一个库(`NewPlayerStorageId` 静态) | `shared/authoritysql/writer.go:17`、`:196-262`(抽查) | 先在压测里看热点视图(KeyViz)再定;改用已锁 slot 上的 epoch、少读一次 route_meta | **11-10 测量方案已写、测量指标已落码(未编译,「Turn battle gap closure handoff」会话)**:见 [db-write-fencing-mysql.md](../design/db-write-fencing-mysql.md) 文末「2026-11-10 写事务闸门的开销与热点」,验证见 `docs/PROGRESS.md` 2026-11-10「第 27 条帮会邮件按帮计数、第 20 条写者闸门加测量指标」。新指标 `writer_gate_seconds{logical_db, phase}`,闸门判定不变。**11-11 定案并落码**(负责人「不用等压测数据了,先写完」;未编译):事务内闸门语句由四条 / 三条降为两条(条件写命中不再回读 slot 行;`route_meta` 与 `writer_registry` 合成一条读),判定不变;预切分 `writer_slot` 定为不做(每库写者少、切不开单个写者;`SPLIT TABLE` 要的 `INSERT` 只有注册账号有;每次启动换 `writer_key`,按写者切 Region 会无限增长),扩写吞吐靠加库;不去掉 BEGIN 之前那次 `route_meta` 读。见设计稿同一节「11-11 定案」,验证见 `docs/PROGRESS.md` 2026-11-11「缺口清单第 17、20、25 条不等压测直接定案落码」;Java 待做 |
| 21 | 目录读 | data_service 的连接池全是写者权限,纯读也开闸门事务;go/db 每次认领都 RPC 查一次落点,不缓存 | `shared/authoritysql/driver.go:556`、`data_service/internal/svc/servicecontext.go:305`、`db/internal/playerstore/server.go:142` | 读走 Reader 池;go/db 的落点缓存要先设计:玩家换库或合服 pin 之后旧缓存会把认领和存盘导到旧库,造成数据分叉,必须带落点版本并在存储侧复核,不能直接加 TTL 缓存(11-05 核对 `go/db/player_store_wiring.go` 的 `newStorageResolver`,SavePlayer 与 LoadAndClaim 每次都经 data_service 路由 RPC) | **11-10 收尾归「服务器代码缺口补完」**:前半「纯读不开闸门事务」已在主干(`authoritysql` 读闸门 `admitRead`,规格 `horizontal-scale-logical-db.md` §2);后半 go/db 落点缓存经核不做,理由见同一规格 §7 末条。「Turn battle gap closure handoff」11-10 曾认领,已撤回、未改文件。没有另开 Reader 池:无闸门读在搬库切换后会读到源库旧数据。未编译,验证见 `docs/PROGRESS.md` 2026-11-10「读闸门收尾」;Java 网关 `WriterAuthority` 未改,Java 记待做 |
| 22 | 写者名册 | 每启动一次 Pod 永久用掉一条,上限 1024;频繁扩缩容或重启会用完 | `tools/scripts/k8s_deploy.ps1`「Every Pod start uses one entry」一段(该文件在改,行号会变) | 名册回收或按 StatefulSet 序号复用 | **11-11 落码完毕,未生成、未编译、未测试,待 Codex 验证**(命令、顺序与通过标准见 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」):账号按槽位、每次启动在槽位上换代(`writer_registry.boot_seq`,注册事务里把上一代退休),名册不再随启动消耗;12 个写者改 StatefulSet,槽位 = 序号;`TestFrequentRestartsNeverExhaustRoster` 守住「同一槽位启动 1100 次」。**11-11 起由本会话接残余**(「缺口第 22 条残余:写者名册 REVOKED 行回收」;原「缺口第 22 条:写者名册回收」会话已不在会话列表):**残余回收 11-11 定案并落码,未编译、未测试,待 Codex 验证**(命令见 `docs/PROGRESS.md` 2026-11-11「写者名册残余:一代结束即删 slot 行」),设计稿 §5.8:slot 行只属于在役的一代,换代、人工与排空退休在结束那一代的同一事务里删掉它的 64 行,`writer_slot` 恒为 ACTIVE × 64;登记行(唯一约束墓碑、boot_seq 高水位)与收据永久保留,每次启动每库只多 1 行登记加 2 份收据。缩容末代自动退休 11-12 也已落完(未编译,设计稿 §5.9,见上面残余表同名一行);Java 只登记待做。**11-10 定案(设计稿 §4):账号按槽位、每次启动换代;槽位 = StatefulSet 序号;分 4 批落码。11-10 起由「缺口第 22 条:写者名册回收」会话(「核对其余 worktree 停着的修复有没有进 main」派出)接手,原认领会话「服务器代码缺口补完」已告知。** 以下是 11-06 的分析:**11-06 由「服务器代码缺口补完」会话认领;分析与方案已写,未落码,等负责人拍板**:[writer-roster-exhaustion-20261106.md](../design/writer-roster-exhaustion-20261106.md)。两种修法都要改写者闸门 2026-10-26 的不变量「主体和 instance 永久唯一」(理由是写后崩溃的 boot 排空证明接不上),预计 30 个文件以上。推荐「槽位身份 + 每次启动换代」:名册按槽位批准,每次启动在槽位上原子退休上一代、注册新一代(writer key 与 instance 仍每代唯一,闸门不用改),顺带解决死写者要人工逐个退休。待定:是否放宽「每次启动一个新 SQL 账号」;槽位按 StatefulSet 序号分还是按租约领 |
| 23 | guild 排行缓存 | 改分、重建、解散摘榜、合服折叠都要持全服一把 Redis 锁(SETNX,20ms 轮询,TTL 5 分钟),全服公会改分串行;锁是为了让缓存重建与改分不互相覆盖 | `guild/internal/data/guild_repo.go:608`、`:666`、`:759`、`:861`、`:914-944`(抽查) | 改分走带版本的 Lua 写入(版本落一张 hash,解散写墓碑),重建按版本合并而不是整键替换,热路径不再拿锁 | **11-11 已落码,未编译**(负责人 11-10 看过 11-05 的「暂不改」后要求现在就改;验证见 `docs/PROGRESS.md` 2026-11-11「帮会积分写入不再抢全服锁」):`guild` 表加 `score_version`(改分事务里 +1);改分、建帮上榜、重建合并、折叠合并都走同一段「版本不比已记的旧才写」的脚本;解散在帮会合服组的每个区立墓碑,墓碑按区存、带到期时刻,24 小时后过期,不要清理任务;重建按版本逐条合并,删条目之前回库复核;折叠按版本合并、按读到的版本摘源区。写分、解散摘榜不再拿锁,维护锁只留给重建与折叠互斥,没有删。与左栏原方案的两处不同:不给所有榜键加一个 `{guild_rank}` tag,那会把全部区挤进一个槽;改成每个脚本只碰一个区的键,上 Cluster 时按区打 tag。墓碑不写「最大版本」,改成按区的到期集合,这样不用清理任务。设计见 `docs/design/guild_ranking_architecture_zh.md` §3、§5、§7 |
| 24 | match | 一个池只归一个副本,热池上限就是单副本能力;Pending 全局先 LIMIT 32 再按归属过滤;5 个队伍维护循环每个副本都跑,每 500ms 给每支排队整队开一个锁全员票的事务;投榜没选主,每个副本都串行投全部 64 片 | `match/internal/logic/global_pool_matcher.go:40,80-86`、`authority/store.go:980`、`team/sql_recovery.go:85-145`、`authority/team_maintenance.go:30-66`、`authority/rank_publisher.go:174-218` | 热池拆归属键;Pending 按归属查;维护循环与投榜套用 poolowner 分工 | **11-06 由「全服匹配方案讨论」会话接手并落码(未编译,验证见 `global-match-hierarchical-pools.md` §12.7)**:撮合池、成局推进、整队维护(prepared / queued / forming / completed 四个循环)、闲置队伍到期、投榜共用一条归属规则 `svc.OwnsWork`(成员表为空时全部副本都做);成局推进按 battle_id、整队按发起者、闲置按 team_id 分 64 片,条件放在 SQL 的 LIMIT 之前;投榜按 (模式, 来源片) 分工。残余:已建房票巡检 `ReadyAfter` 同类,文件正由数据覆盖审计会话改,约定其落完后接同一分片条件;热池仍单写者,拆法按压测证据定(设计 §5.2 末条)。**11-11 定案方向(「缺口清单压测项:统一压测方案与定案」,只改设计稿 §5.2)**:本机房阶段按首选机房拆写者(本来就只撮合同机房的人,不损质量),跨机房阶段单写;单机房部署再按玩家打散成 K 个子池(热门模板配置,缺省不拆),等满合并时长回到机房池;票与整队原件加首选机房、子池号两列做池索引前缀;提交点不变。落码量大且 match 正有多个会话的未提交改动,另开会话先写细化设计再分批落码。**11-12 落码完毕(「缺口第 24 条:热池按机房与子池拆写者」会话),未生成协议、未编译、未测试,待 Codex 验证**(命令、顺序与通过标准见 `docs/PROGRESS.md` 2026-11-12「全服匹配热池拆分:按首选机房与子池拆写者」,细化设计 `global-match-hierarchical-pools.md` §5.2.1,验证 §12.8):每个池切成撮合段,每组 1 人的池一个整池段;其余每池每机房一个机房段,热门模板再按 player_id(整队取发起者)取模拆 K 个子池段(`GlobalMatch.SubPools`,缺省不拆;合并时长 `SubPoolMergeSeconds` 缺省 5 秒),外加一个只读等满跨机房阈值的跨机房段;归属键按段,照旧走 `svc.OwnsWork`。票与整队原件各加 `pool_region` 列和一条以它做前缀的索引。与方向稿不同的两处:子池号不落列(主键取模,在索引项上过滤);首选机房列在探测刷新时跟着新原件重算。提交点不变;票行复核失败改为哨兵 `ErrClaimStale`,撮合侧不再把段之间的撞车记成排空 HOLD。新指标 `match_pool_owned{role}`、`match_pool_segment_claim_total{role,outcome}`。**同日收尾**(验证见 `docs/PROGRESS.md` 2026-11-12「热池拆分收尾」,与上一条合成一轮):票表删掉两条没有读者的二级索引(4 条变 3 条);整队读页 `mode IN(5,0)` 改成 `mode=?`;`stress_summarize.ps1` 加 G4 表(按撮合段角色)与钉名测试;设计稿里「以后再定」的几项全部定案:跨机房段与机房段的撞车不另加机制(只在同一轮同时出现两种机会时才撞)、写者增量缓存与内存撮合都不做、`match_formation` 不改 NONCLUSTERED(battle_id 已打散)、K 缺省 1 与合并时长 5 秒由运维按 G1 / G4 表配。拆分效果要按 AGENTS §6 跑前后两轮才能下结论;Java 只登记待做 |
| 25 | 资产重投 | guild / trade 的 assetop 与 match 终局发件箱,所有副本读同一个到期队头,靠租约 CAS 互斥,空 CAS 随副本数增长 | `shared/assetop/reconcile.go:577-611`、`match/internal/authority/outcome_outbox.go:159,293` | 按玩家分片归属 | **11-10 已落码(未编译,「Turn battle gap closure handoff」会话)**,设计见 [assetop-redelivery-sharding-20261110.md](../design/assetop-redelivery-sharding-20261110.md),验证见 `docs/PROGRESS.md` 2026-11-10「资产指令重投与终局发件箱按玩家分片」:assetop 加可选能力 `OwnedDueLister` 与 `Loop.BindWorkOwner`,guild / trade 的重投到期扫描按 `player_id` 分 64 片(本 zone 的 stable 副本,成员表为空时做全部);match 终局发件箱整局结果按 `battle_id`、个人结算按 `player_id`,走 `OwnsWork`;条件都在 `LIMIT` 之前;新指标 `assetop_owned_shards`。**仍剩**:mail 与 data_service 回收的重投循环没分片(成员表怎么圈要另核);到期索引不含 `player_id`,积压大时多扫行,加不加列等压测;guild / trade 的终态行清理每个 stable 副本都跑(重复执行,另记);没加告警规则(设计里给了式子)。**11-10 「仍剩」三项由「G4a Redis 设计稿修订应用」会话认领**(原落码会话「Turn battle gap closure handoff」确认不做)。**11-11 这三项已落码(未编译、未运行,同一会话)**,设计见同一设计稿 §10~§13,验证见 `docs/PROGRESS.md` 2026-11-11「资产指令重投分片第二批」:mail(`mail-assetop`)与 GM 回收(`gm-recall-assetop`)的重投都按 `player_id` 分片、成员表都只圈本 zone(回收的流水库虽全集群一份,但 data_service 随 zone 部署、配置各 zone 自有,圈全服会漏片);guild / trade 终态清理的候选读都能把片条件放在 `LIMIT` 之前,改为复用重投成员表按 `player_id` 分片(类别键 `guild-assetop-cleanup` / `trade-assetop-cleanup`),不改单领导者;告警 `AssetOpOwnedShardsMissing`(`sum by (namespace, service) (assetop_owned_shards) < 64` 持续 5 分钟)与升级规则 `AssetOpOwnedShardsMissingBacklogAging`(再与最老未决年龄 > 30 分钟同时持续 30 分钟)。mail 原先没有任何 assetop 指标,已补。仍不做:到期索引加 `player_id`(等压测)、回收派发循环与 mail 过期清理的分工、循环卡死的活性指标。**11-11 起由「缺口第 25 条残余与第 35 条:回收分工、活性指标、截断告警」会话接后两项**(回收派发循环与 mail 过期清理的分工、循环卡死的活性指标);到期索引归「Turn battle gap closure handoff」(见文首「11-11 归属核对」),本会话不碰。**11-11 到期索引已落码**(负责人「不用等压测数据了,先写完」;改 proto 索引选项,未生成、未编译):四张表 `idx_0` 改为 `(status, next_attempt_ms, op_id, player_id)`,`ORDER BY next_attempt_ms, op_id` 走索引顺序(TiDB 上主键非聚簇,旧索引要整段读完再排序),分片条件在索引条目上判;要删库重建。见设计稿 §2.4、§8 第 10~14 步。**11-11~12 回收派发与 mail 过期清理的分工、循环活性已落码(未编译、未运行,第四、五批,本行开头接手的那个会话)**,设计见同一设计稿 §14,验证见 `docs/PROGRESS.md` 2026-11-12「回收派发与 mail 过期清理按片分工、循环活性指标、删 mail 截断计数」与「其余按片分工循环的活性、每轮各自 recover」:GM 回收派发按 `player_id` 分片(类别键 `gm-recall-dispatch`,复用回收重投的成员表),mail 过期清理按 `mail_id` 分片(`mail-expiry-sweep`,复用附件重投的成员表;按收件人分会把 `owner_player_id=0` 的共享邮件全挤进一片),片条件都在 `LIMIT` 之前;第 25 条里六个按片分工的循环(重投由四个服务共用 assetop、派发、mail 清理、guild / trade 终态清理、match 终局发件箱)各有一个「最近一次跑完一轮的时刻」gauge,口径收在共享包 `go/shared/looplive` 一处(循环起来写、每一轮正常返回才写、有意停止才删序列,不带可变 label);每一轮各自 recover:mail 清理与 match 发件箱原来一轮 panic 会打死进程(按片分工时会随着片换主把副本挨个打死),trade 终态清理原来会让循环退出、进程还活着,现在都是只丢这一轮、循环接着跑;五条按副本判的停摆告警(`asset-channel.loop-liveness` 组三条、`MailSweepStalled` 改按副本、`MatchOutcomeOutboxLoopStalled`)加一条对全部 Go 服务后台点位的「panic 过」(`GoBackgroundPanicRecovered`,新文件 `deploy/k8s/go-background-alerts.yaml`),契约测试 `loop_liveness_alerts_contract.tests.ps1`。顺带修:mail 清理活性原来记的是一轮的开始时刻(defer 在开头求值);派发读队头失败原来只有日志,加计数 `data_service_recall_dispatch_list_errors_total`;match 撮合的两个循环原来一轮 panic 后循环静默退出、投榜循环原来一轮 panic 打死进程(同一类缺陷,归第 24 条那条线),一并改成每轮各自 recover,见设计稿 §14.10。第六批(11-12)补上 match 其余后台循环的活性:经 `StartSourceLoop` / `StartRelayLoop` 起的 13 个循环与投榜共 14 个点位,一族 gauge `match_loop_last_run_unixtime{point}`(`looplive.NewFamily`),告警 `MatchBackgroundLoopStalled`(`match-alerts.yaml`,300 秒、按副本按点位判),契约测试钉住点位名单与每个循环的节拍;验证看 PROGRESS 2026-11-12「match 其余后台循环的活性」。全部未编译、未运行。Java 只登记待做 |
| 26 | player_inbox | 入箱、拉取能扩;补发与对账只在单个 leader 上串行(`SweepTerminal` 恒回 0 是有意的:终态行是永久幂等收据) | `player_inbox/internal/inbox/loops.go:22,339-357` | 64 片分给各副本 | **11-10 核对,不再改**:对账已按 64 片分给各 stable 副本(`internal/shardowner`,11-06 落码,验证见 `docs/PROGRESS.md` 当天条目);清理、补发、gauge 有意保持单领导者:它们的活只随异常增长,单领导者加快速接任就够(`loops.go` 包注释、[消除单节点总纲](../design/no-single-node-horizontal-scaling-20261001.md) §9) |
| 27 | mail | 帮会邮件计入全服活跃上限 100(还没有调用方);活动导入逐片锁活动行再全量 COUNT,名单越大越接近平方 | `mail/internal/data/repo.go:254-261`、`campaign.go:129,157` | 按帮计数;片表累加 | **11-05 活动导入已落码(未编译)**:已导入数改用片行 `recipient_count` 之和,整片一条多行 INSERT、撞主键按冲突整片拒绝(`campaign.go`,用例 `campaign_import_integration_test.go`);帮会邮件计入全服上限(还没有调用方)**11-10 已落码(未编译,「Turn battle gap closure handoff」会话)**:系统邮件仍按全服 `MaxActiveSystemMails`,帮会邮件按帮计 `MaxActiveGuildMailsPerGuild`(缺省 20),发件与建活动共用 `checkSharedMailCapacity`;设计见 `mail-system.md` 文末 2026-11-10 节,验证见 `docs/PROGRESS.md` 同日条目 |
| 28 | client_rpc_router | 每个请求扫描并拷贝全服镜像选目标,开销随全服实例数增长;连全服全部目标 | `client_rpc_router/internal/discovery/node_watcher.go:269-300`、`:313-344` | 按 zone 建索引 + 原子快照,选点 O(1) | **11-05 选点已落码(未编译)**:镜像每变一次在写锁里重建只读索引(按 zone、按发布轨道各一份)并原子替换,选点无锁 O(1);轨道退回仍由 `releasetrack.Select` 在轨道组之间判断,语义不变(用例 `TestPickIndexFollowsEveryMirrorChange`)。「连全服全部目标」**11-10 已落码(未编译,第 30 条批 1)**:全服池目标改用确定性 aperture(同轨道路由服按 node_uuid 排名、在目标环上均匀各占一段,每台约连 `GlobalTargetSubsetSize`=4 台,各目标份额相等、一台不漏;没用设计初稿的 HRW,理由见该文 §5),连接缓存空闲满 10 分钟回收,`ForwardTimeoutMs` 必须短于回收时长;zone-scoped 目标随 gate 侧的 zone 子集收敛(批 2)。见 [connection-fanout-subsetting-20261106.md](../design/connection-fanout-subsetting-20261106.md) §3.3,验证命令在 `docs/PROGRESS.md` 11-10「连接扇出第 30 条批 1、批 4」 |
| 29 | Java 网关 | assign-gate 限流每 zone 一把 Bucket4j 键、CAS 版,副本越多冲突越多,热键超 500ms 即放行;健康探测每副本每 5s 全前缀读全服 gate + scene | `java/gateway_node/.../AssignGateRateLimiter.java:97-104,167`、`RateLimitConfig.java:6,91-96`、`ZoneHealthProbeService.java:68-77` | 限流改单条 Lua 原子扣减;探测改 watch 或共享快照 | **11-05 已落码(未编译)**:限流改成一段 Lua 同时判 IP 桶与区桶(`ratelimit/RedisRateLimitStore.java`),先 IP 后区、区桶空不扣 IP 令牌(顺带了结开服限流设计 R13 / J1),拒绝路径只读、键在补满时过期,阈值随调用传入、滚动即生效;Redis 出错 fail-open 并熔断 5 秒,进程内冷却照判;新增 `gateway_rate_limit_decisions_total` / `gateway_rate_limit_fail_open_total`;去掉 Bucket4j 依赖。健康探测与 login 发现改为 list + watch 镜像(`etcd/NodeInfoMirror.java`),稳态不整段读,watch 出错或满 5 分钟才重新整段读。剩余:全服前缀的 watch 仍是每副本一份,事件扇出随 zone 数平方增长(只有变更,gate 每 10 秒一条);再要降,改由一个发布者汇总成每 zone 一条健康记录。`k8s_deploy.ps1` 与两份 ps1 契约测试的注释仍写着「Bucket4j」(键名与连法没变,只是叫法旧)。Java 版限流已是 Lua(等价),gateway 全服共用、探测读量随 zone 线性增长,不适用。**11-11 起由「缺口第 29 条残余:网关健康探测的全服扇出」会话接残余;同日定案并落码(未编译、未测试,待 Codex 验证)**,设计见 [gateway-zone-health-publish-20261111.md](../design/gateway-zone-health-publish-20261111.md),验证见 `docs/PROGRESS.md` 2026-11-11「网关区服健康改为每 zone 发布汇总记录」:每个 zone 的网关副本只 watch 本 zone 的 gate / scene,把本 zone 的健康汇总写进 etcd(挂进程租约,状态或分母变了才写、在线人数漂移满 1% 且跨档立即写否则 30 秒一次),网关只读汇总,不再 watch 全服 gate / scene,也不再每 5 秒 `findAll` 区服目录;没有汇总的 zone 看 login 登记(都没了判 DOWN,login 还在判 UNKNOWN 并告警);新属性 `gateway.home-zone-ids`(K8s 渲染成本 zone)。login 发现核对后保持全服(客户端只有一个网关入口,login 登记不周期重写)。静态发布不做:要客户端改读法与对象存储、CDN。读方剩下的交叉项 4Z²·w 由写入策略封顶(Z≈445 时上界约 2.6 万事件/s,改动前常驻约 11.9 万)。顺带:ConfigMap 里从没生效过的 `zone.probe.interval-ms` 改成 `zone-probe.interval-ms`;上面说的「Bucket4j」旧叫法,`k8s_deploy.ps1` 连接总账注释与 `deploy/login-stack.linux/README.md` 标题已改,契约测试里已是历史说法。**11-12 复查(未编译)**:网关 5 个定时任务原来共用 1 个调度线程,别的任务卡住会拖掉健康发布者的租约,改为线程数等于任务数(`spring.task.scheduling.pool.size`,`ScheduledTaskThreadsTest` 钉住);`EtcdPrefixMirror` 的重新同步改成串行,两个任务同时读 login 镜像不再有一方失败;删掉没人读的 `zone-probe.gate-stale-threshold-seconds`;`deploy/k8s/README.md` 加「区服健康汇总与告警」一节。第 29 条的代码到此写完,只剩 Codex 验证,见 `docs/PROGRESS.md` 2026-11-12「网关区服健康汇总收尾复查」 |
| 30 | gate | 连本 zone 全部 scene 和全服全部路由服(G×S + G×R);场景广播给每台 gate 各发一条整包,login 挑 gate 不看场景亲和;gate-cmd 固定 256 分区,每个节点解码全分区再过滤;每台 battle watch 全部同类节点 | `cpp/nodes/gate/main.cpp:376-378`、`cpp/libs/engine/core/.../client_message_sender.cpp:134-150`、`node_command_topic.h:39`、`cpp/nodes/battle/main.cpp:133` | 每台只连 k 个路由服;按场景亲和挑 gate;命令 topic 按 zone 拆或换代扩分区 | **11-06 设计稿已写,待落码**:[connection-fanout-subsetting-20261106.md](../design/connection-fanout-subsetting-20261106.md)。gate→路由服按 zone 取 HRW 子集(k=3);每类 C++ 节点只发现自己用到的类型,battle 只看自己的键;gate↔scene 的 zone 内全连接保持;命令 topic 每节点只 assign 一个分区,先加读放大计数再决定换代;场景亲和先测,要做时按 gatepick 的一维加(挑 gate 规则以「玩家切换zone和gate链接」会话为准)。分 4 批,见该文 §6。**11-10 按批分工**:批 1(路由服全服池子集与空闲回收,含第 28 条「连全服全部目标」)与批 4(读放大与扇出计数)归「服务器代码缺口补完」;批 2(gate 路由服子集与连接准入钩子)与批 3(每类节点的发现集合)原归「Turn battle gap closure handoff」,该会话未动手;**11-10 负责人改派给「缺口第 30 条:gate 连接与解码扇出」会话**,先补设计稿(批 3 补上 node_id 与端口分配对同类快照的依赖)再落码。**11-10 批 1、批 4 已落码(未编译,「服务器代码缺口补完」)**:批 1 见第 28 条;批 4 加了命令 topic 读放大计数 `node_command_consumed_total{node_type,topic,target}` 与告警 `deploy/k8s/node-command-topic-alerts.yaml`,以及广播扇出直方图 `scene_broadcast_gate_fanout{kind}`(Prometheus 写出器补了 histogram 类型)。Java 只登记批 1 的同类问题(gate 经 Dubbo 连全部提供者),验证命令在 `docs/PROGRESS.md` 11-10「连接扇出第 30 条批 1、批 4」。**11-10 批 2、批 3 已落码(未编译、未测试,「缺口第 30 条:gate 连接与解码扇出」;验证命令与通过标准见 `docs/PROGRESS.md` 11-10「连接扇出第 30 条批 2、批 3」与 11-11「……批 2、批 3 补」,后者多一条部署契约测试)**:gate 按 zone 号做 HRW,只连、只挑 k 台路由服(`GATE_ROUTER_SUBSET_SIZE`,缺省 3),掉出子集的过调用期限加 1 秒再拆,选点 READY 优先;每类 C++ 节点按 `main.cpp` 声明的发现集合 Range / watch(不再看 Friend、Guild、用不到的全服池类型),battle 只看自己那一条发现键;顺带修了 11-05 发现前缀按 zone 收窄后的逐个撞号:分配键 CAS 失败时同一个 txn 读回全服已分配的号,下次选号跳过。Java 已核对:批 2 的对应项就是上面 Java 登记的那条,批 3 已有等价或不适用(设计稿 §8)。**第 30 条仍剩**:场景亲和与命令 topic 换代,按设计先看批 4 两项计数的压测数据再定。**11-11 定案,两项都不做(「缺口清单压测项:统一压测方案与定案」,只改设计稿 §1 第 5、6 条与 §3.5、§3.6)**:命令 topic 每个节点的解码量 ≈ 总速率 ÷ P,与节点数无关,最大目标口径下每节点每秒一两百条,告警只抓异常;场景亲和每次广播至多发 G_z 份、上界由单 zone 规模限定,做亲和要拿 gate 的负载均衡与故障隔离去换,万人同屏时整体重做。**11-11 批 4 残余补完(未编译,「服务器代码缺口补完」)**:命令消费回调里「是不是发给本节点」收成纯函数 `detail::IsCommandForThisNode`,派发与 own / foreign 计数共用,补了 `kafka_command_test` 的 `IsCommandForThisNode.*` 两条用例,验证见 `docs/PROGRESS.md` 2026-11-12「世界频道第三批」 |
| 31 | 每次 AssignGate | 两次 MGET 读控制域 Redis 的排空标记 | `login/internal/logic/pkg/loginqueue/gatedrain.go:77`、`login/internal/svc/servicecontext.go:540` | 排空标记进 gate 目录镜像 | **11-05 已落码(未编译)**:没把标记搬进 etcd(要改 `k8s_gate_drain.ps1` 的原子标记脚本与 scene_manager 的读法),改成 login 进程内按 zone 的读穿透缓存 `loginqueue.DrainMarkCache`:选 gate 的各条路径(快速通道、容量判断、dispatcher、放行复核)共用一份,每个 login 实例每个 zone 每秒至多一次 MGET;并发过期只查一次,查询失败照旧按没人排空放行且同样缓存 1 秒。标记最多晚 1 秒生效,与排空流程本来就容忍的晚到玩家(标记前签出的票、gate 每 10 秒才报在线数)同一类。scene_manager 跨 zone 重定向的 `drainingGateIDs` 调用量小,没改。另一会话的设计稿 `gate-drain-session-migration.md`(未落码)第 2 步「读调用方 gate 的 draining 标记」是纵深防御,落码时要直读 Redis,不要走这份缓存 |

**未核实,要压测或读客户端才能定**:10 万人排队、每 2s 轮询约 15 万条命令 / s 是否打满单台 Redis;路由服到每个目标只有一条 h2 连接的吞吐上限;客户端拉世界频道的频率(没读客户端仓)。

### 11-05 新发现:会造成错误行为

32. **(11-05「Scene manager zone selection and etcd threshold」会话做第 9 条时发现;11-05 同一会话已落码;7 条回归用例在 11-06 Codex 的 scene_manager 全量里全过,见 `docs/PROGRESS.md` 2026-11-10「核对存活集闸门(B1-1)与缺口第 9、32、33 条的验证证据」;原验证命令见 2026-11-05「死节点收尾认出失租后回来的同一进程」)节点失租后同一进程重新注册,死节点收尾仍会销毁它名下的实例。**
   - **已改**:收尾任务记下判死那一代的 `node_uuid`(`node:zone:{z}:{n}:reconcile_instance`,watch DELETE 带着它,fullSync 清扫从旧镜像补);drain 在判屏障之前看镜像,同一代回来就撤任务,场景与计数不动(`outcome="instance_back"`);换代、或不知道是哪一代时照旧收尾。结束脚本加了守卫:任务已不在表里就不删计数。改的是 `reentry_barrier.go` 的任务脚本与 drain、`load_reporter.go` 的清扫,PUT 分支没动。
   - **残余**:只覆盖「在屏障放行、drain 动手之前回来」。租约 180s、屏障 20s,失联约 200 秒以上才回来的节点见第 33 条。注册键被误删、租约被撤销、租约集中到期这几种,节点 1~2 秒内就按同一个 node_uuid 重新注册(keepalive 每秒一次,健康检查发现本节点不在发现快照里也会重注册),都在本条覆盖范围内。
   - C++ 节点 keepalive 回 TTL=0 或本地租约超时后只重新拿租约、按原 node_id 重新注册(`etcd_service.cpp` `OnKeepAliveResponse`、`node.cpp` 约 2367 行),进程和实例都还在;只有分配键 CAS 失败才走 15 秒疏散后退出。
   - scene_manager 在 watch DELETE 时已经写 death_at、摘负载集,并把判死时刻的场景快照写进收尾任务。重新注册的 PUT 只清 death_at(`clearNodeDeath`)、取消推迟摘除,不撤收尾任务;清掉 death_at 之后 `CanReclaimDeadNode` 立即放行,下一拍 `drainPendingDeadNodeReconciles` 按快照强制销毁那些实例,节点此时已回到负载集,`destroyInstanceInternal` 还会向它发 DestroyScene。
   - 后果:单个节点失租超过 TTL(etcd 抖动、网络闪断)就能把它上面的副本全部拆掉,里面的玩家被踢出;etcd 回档造成的大面积失租会放大成大面积拆实例。存活集闸门(第 9 条)只拦 fullSync,拦不住这条。
   - 方向:收尾任务记下判死那一代的 `node_uuid`,重新注册的是同一个 uuid 就撤掉任务(场景不动),是新 uuid 才按快照收尾。涉及 `reentry_barrier.go` 的任务脚本与 `handleWatchEvent` 的 PUT 分支,与按 zone 选主的会话同文件,动手前先对段落。只读核对过代码,没有复现。

33. **(11-06 登记;11-06 初版已落码,20 条 C++ 用例在 11-08 Codex 的 `cross_zone_test` 全集里全过,见 `docs/PROGRESS.md` 2026-11-10「核对存活集闸门(B1-1)与缺口第 9、32、33 条的验证证据」;11-10 补完三项残余,已落码,未编译、未测试,用例改为 28 条 C++(11-11 加 1 条)、新增 15 条 Go(含 etcd 修订号倒退后换纪元的持有者修正,11-10 按父会话评审重做),验证见 [scene-node-ownership-reconcile.md](../design/scene-node-ownership-reconcile.md) §10 与 `docs/PROGRESS.md` 2026-11-10「场景归属对账补完」)失联超过屏障又回来的节点,还托管着已被改派的场景。**
   - **11-10 补完**:去掉「etcd 身份确认有效才核对」,与 etcd 失联期间也按映射收敛(etcd 整体故障时 scene_manager 不判死、映射不变,不会误排空);scene_manager 把每个节点号最近一次登记的进程写进 `node:zone:{z}:{n}:holder`(按 etcd ModRevision 单调,`node_holder.go`),节点读到自己的号被更新的另一代接走就把本地场景全部排空;etcd 修订号倒退(换集群、不带 bump 恢复)时领导者按「先读本 zone 水位、后线性一致读 etcd 当前修订号」认出来,换一次 etcd 纪元,倒退之前的记录让位给新登记(告警 `SceneManagerEtcdRevisionRegressed`,处置见 etcd 运维手册 §7a);开始排空按每轮每节点 200 名住户的预算限速,控制域丢数据时分批改派;新增 `deploy/k8s/scene-ownership-alerts.yaml` 五条告警与契约测试。
   - **11-10 之后仍剩**(设计稿 §9),**11-11 起由「场景归属对账」窗口(本条的落码会话)逐条收尾**,代码与文档都写完,未编译、未测试,验证见 `docs/PROGRESS.md` 2026-11-11「缺口第 33 条残余收尾」:
     ① 本进程一直没读到自己的持有者记录时认不出被接走:**接受,加了兜底**。不拿本进程注册时 etcd 的应答修订号顶替,因为 etcd 倒退之后、换纪元之前会把旧纪元记录当成「别人更新的登记」误排空;节点新增指标 `scene_ownership_own_holder_known`,托管着场景却持续 10 分钟是 0 时告警 `SceneOwnershipOwnHolderUnknown`;退路是修写入或读,修不好且节点与 etcd 失联时核对登记后重启它,号真被接走时节点重连 etcd 也照常疏散(设计稿 §9 第 1 条)。
     ② 控制域刚丢了修订号水位的那一轮认不出 etcd 倒退:**接受,有手工退路**。水位只在控制域丢数据时才会没有:整实例丢数据时持有者一起没了;备份恢复时两者出自同一份快照;主从切换只丢最后一次抬升。剩下只有人手工删键,手册写明不要删。退路是在那个 zone 的控制域上手工 `INCR scene_manager:etcd_epoch:zone:<z>`,写进 etcd 运维手册 §7a 第 4 步(设计稿 §9 第 9 条)。
     ③ 告警的 promtool 与入集群:**交 Codex**,命令在设计稿 §10 第 7、7a 步(两份规则文件;入集群只在本机或指定的 dev 集群,没有 Prometheus Operator 就报告未入集群)。
     ④ 住户预算 200 的初值:**交「缺口清单压测项:统一压测方案与定案」会话**,本窗口不管。**11-11 该会话定案:保留 200**(每节点每秒 20 名,与 R18 实测每个 scene 节点约 17.5 次进场/秒同量级;按节点限不按 zone 均摊、不做配置项,理由见设计稿 §4.7、§9 第 3 条;只改了 `scene_ownership_rules.h` 的注释与设计稿,「清空控制域」故障演练列进压测方案作复核)。
     ⑤ 倒退后没重启的老进程认不出被接走:**按 etcd 运维手册 §7a 处理**,11-11 补写了原因与「逐个核对 scene Pod 启动时间晚于换集群时刻」一步。
   - **11-06 初版**(当时只在 etcd 身份确认有效时核对,11-10 去掉了这道门禁):scene 节点每 10 秒用 MGET 读本地全部场景的 `scene:{id}:node`(`tlsSceneControlRedis`,只读);连续两次不是本节点、且距第一次核对或最近一次 CreateScene 按号命中满 60 秒,就调 `SceneLifecycle::DestroyScene`(先 `BeginSceneDrain` 改派住户,没有绑定才销毁)。CreateScene 去重命中已有实体时 `SceneLifecycle::ReuseExistingScene` 把宽限期从头算(盖住 scene_manager「先建后改」迁回本节点的预建)。跨 zone 类型与镜像的口径与普通场景相同(设计稿 §5)。顺带收掉其他「有实体没映射」的孤儿,包括 11-05「场景拆除任务」残余里「回滚补发的 DestroyScene 早于在途 CreateScene」那一条(设计稿 §7)。
   - **11-06 初版的残余**(11-10 已补,见上):一直连不上 etcd 的节点不对账,分裂持续到 etcd 恢复;控制域丢数据时会大面积排空;还没有告警规则。
   - 以下是 11-06 登记时的原文。
   - 条件:节点与 etcd 单边失联(仍连着 gate 和 Redis,玩家还在上面玩)超过租约 180s 加屏障 20s。
   - 这时 scene_manager 已经把它判死:
     - 拆掉它名下实例在 Redis 里的映射。当时它不在负载集,不发 DestroyScene;
     - 把世界频道改派到别的节点。
   - 网络恢复后它按原 node_id、同一个 node_uuid 重新注册,第 32 条的撤销已经晚了。进程里留下两类没有映射的实体:
     - 实例:新玩家进不去,scene_manager 的空闲回收也看不到它;
     - 世界频道:还留在旧实体里的玩家,与新进来的玩家分在两个节点上,互相看不见。
   - 玩家数据不受影响:被接管过的玩家,旧节点的存盘由 owner epoch 拒绝;没被接管的照常存盘。
   - 不选「失租就自己停服」:etcd 整体故障时所有节点都会失租,自停等于 etcd 一挂全服掉线,而节点分不清「etcd 挂了」和「只有我连不上」。11-05 数据一致性那条线(`go/scene_manager/internal/constants/reentry_barrier.go` 文件头的更正)也定的是「老节点继续服务、靠写权行挡回档」。
   - 标准做法是节点回来之后按控制面的期望状态对账:Kubernetes 的 kubelet 回到集群后,把 API server 上已删的 Pod 在本地停掉;Java 版是 scene 拉计划、不在计划里的频道就排空(PARITY「场景实例与主世界频道」行 D3)。
   - 方向:
     - scene 节点在身份确认有效(`IsIdentityConfirmedFresh`)时,定期经 `scene_control_redis` 核对本地每个场景的 `scene:{id}:node`;
     - 连续两次不是自己、且场景建好已超过一个下限(避开 scene_manager「先建后改」的窗口),就 `BeginSceneDrain` 排空并销毁;
     - 跨 zone 场景与镜像的口径要单独核。
   - 只在节点侧做,不需要新协议。涉及 `scene_lifecycle.cpp`、`player_lifecycle.cpp` 的排空和控制域读,这几处同时有别的会话在改,先写设计再落码。

### 11-10 新发现:挡水平扩展

34. **(11-10 负责人要求;「Redis 分布式水平扩展」会话认领;设计已写、未落码,见 [redis-cluster-all-domains-20261110.md](../design/redis-cluster-all-domains-20261110.md))服务器里的 Redis 没有全部分布式水平扩展。** **11-11 接手**:原认领会话已结束(PROGRESS 11-11 有记),由「服务器代码缺口补完」[37a1b5] 接手,从批 1 开始按设计稿 §10 逐批落码;同名会话 [aca21c] 及其他会话要动第 34 条,先找它对分工。
   - **11-11 第 1 批落码,未编译**:[37a1b5] 写了 `go/shared/clusterredis`(从 playerredis 拷来,加两种键策略)与 `go/shared/redisslot/control_keys.go` 之后不在了。「Redis 分布式水平扩展」会话(现为 [3204dc],负责人 11-11 让它开工)接着补完:`playerredis` 改成只剩别名、控制策略用例、C++ 族表镜像、`KeyDomain` 与 `SPUBLISH` 登记。第 2a 批起由 [3204dc] 继续,要动第 34 条的会话先找它。验证命令见 PROGRESS 2026-11-11「全部 Redis 上 Cluster 第 1 批」。
   - **11-12 第 2a 批落码,未编译**:「Redis 上 Cluster 第 2a 批:scene_manager 键改形」会话把 scene_manager 控制域键全部改成 `{ctl:<z>}`(场景键加 zone 参数、删 `scene:<id>:zone` 与 `foreign` 分支、只给场景号的入口按本进程的 zone),四个跨语言键同批改 C++,约 100 个文件(超过每批 30 个,理由见设计稿 §10 表第 2a 行;负责人 11-12 答「同意一批做完」,当时问的是约 60 个文件,实际约 100 个)。验证命令见 PROGRESS 2026-11-12「全部 Redis 上 Cluster 第 2a 批」;第 2b 批(换句柄)起未开工。做 2a 的会话只受派这一批,不接后面的批次;截至 11-12,第 2b、3、6、7 批清单里无人认领(第 4、5 批见下两条),由发起会话「Redis 分布式水平扩展」或负责人安排。第 2b 批还要等「分布式锁安全性与一致性」会话的 R7(任期号)与第 5c 批的 `shared/leader` UniversalClient 版 store。
   - **11-12 第 5 批由「按缺口清单做完服务器代码(11-05 额度重置后)」会话认领并开工**(它只依赖第 1 批,与第 2a 批不碰同一批文件):拆成 5a(guild 键改形、去 SCAN、每 zone 维护锁、assetopfix,连同 `tools/data_consistency_check`)、5b(friend 换句柄、player_locator 控制键加 `{pl:gix}`)、5c(data_service 名字缓存 `MGetBySlot` 与换句柄、trade 与 player_inbox 换句柄、`shared/leader` 的 UniversalClient 版 store、R8 收口)、5d(这六个服务 `ControlRedis` 段的最小渲染与契约测试;集群模板与参数仍归第 6 批),逐个落码、各记一条 PROGRESS。唤醒频道改分片(SPUBLISH / SSUBSCRIBE)仍归第 3 批,本批不动。第 2b、3、6、7 批还没人认领(第 4 批见下一条)。**11-12 晚子批状态(全部未编译,见 PROGRESS 同日「第 5a / 5b / 5c 批」三条)**:5a guild 已落码(`tools/data_consistency_check` 由「数据审计遗留」会话改,本会话没碰);5b friend、player_locator 已落码;5c data_service 唤醒句柄与 trade 已落码、`shared/leader` 新增 `store_universal.go`(`NewUniversalStore`),data_service 名字缓存(`MGetBySlot`)与 player_inbox 因别的会话正在改同一批文件未做;5d 渲染未做(`k8s_deploy.ps1` 与部署契约测试正被别的会话改),要改的点列在 5c 条目末尾。**11-12 深夜收尾(未编译,见 PROGRESS 同日「第 5 批收尾」)**:player_inbox 的 `CoreRedis` 已换成 `ControlRedis`(选主改 `NewUniversalStore`),唤醒频道的过时注释已改;5d 渲染并入第 6b 批,由「控制面按 zone 化与控制域 Redis 分片(缺口第 4、5 条)」会话做,它要求别的会话不要改 `k8s_deploy.ps1` 与那三份部署契约测试;在 6b 接上之前 guild、friend、player_locator、data_service、trade、player_inbox 的 K8s 渲染与部署契约测试会读不到旧段名而报错。**只剩 data_service 名字缓存**(`NameCacheRedis` 换 UniversalClient、`MGetBySlot`、`create_residual` 的 `-name-cache-*` 参数):`data_service.go`、`routing/player_name_cache.go`、`cmd/create_residual` 当时有别的会话的在途改动,没人认领;接手的会话先看这三个文件的 `git diff`,做完告诉第 6b 批的会话。
   - **11-12 第 4 批由「服务器代码缺口补完」会话认领并开工**(只依赖第 1 批,与第 2a、5 批不碰同一批文件):拆成 4a(gate 标记改 `gate:{gm:<z>}:<id>:…`:`shared/gatepick`、login 的 gatedrain / gatestale / 迁移租约、scene_manager `gate_redirect.go`、`k8s_gate_drain.ps1`)、4b(login 排队与放行锁改 `{q:<z>}`、句柄换 `clusterredis`、放行改节点客户端 pipeline、删 `QueueShardCount`)、4c(本仓 Java 网关限流拆两段单键脚本、Lettuce 换集群客户端),逐个落码、各记一条 PROGRESS。要动 `go/login/internal/logic/pkg/loginqueue`、`go/shared/gatepick/marks.go`、`gate_redirect.go` 读标记那一段、`k8s_gate_drain.ps1` 的键与 redis-cli 调用、`RedisRateLimitStore.java` 的会话先找它。集群模板与 `-ControlRedisMasters` 等参数仍归第 6 批。
   - **11-12 第 4 批落码,未编译**:4a gate 标记改 `gate:{gm:<zone>}:<id>:draining|drained|stale`(login、scene_manager、排空脚本三方同一个 zone 号);4b 排队五把键与放行锁带 `{q:<zone>}`,login 新增顶层 `ControlRedis` 段、句柄换 `clusterredis`(结果未知不重放),放行流水线改发给队列槽所在主节点,删 `QueueShardCount`,`DispatcherLockKey` 含花括号拒启;4c 本仓网关限流拆成单键扣令牌 + 区桶空时退还,Lettuce 按 `spring.data.redis.cluster.nodes` 选集群客户端。协调与偏差见设计稿 §10.3,验证命令见 PROGRESS 2026-11-12「全部 Redis 上 Cluster 第 4 批」。第 2b 批 11-12 由「Redis 上 Cluster 第 2a 批」会话接着做;第 3、6、7 批仍无人认领。
   - **11-12 第 6 批由「控制面按 zone 化与控制域 Redis 分片(缺口第 4、5 条)」会话认领**(「Redis 分布式水平扩展」会话分派;负责人 11-12 对剩余批次的原话「Start them」),拆成两半:
     - 6a 现在做:控制集群本身的部署。集群清单通用化成一份模板,渲染 `redis-player` 与 `redis-control`;`k8s_deploy.ps1` 的 `-ControlRedisMasters` 等参数、口令 Secret `redis-control-auth`、建群与就绪等待、`redis-wipe` / `redis-split-status` / 连接总账;告警与手册通用化;match 私有集群升 8.10.2;删 `redis-rank.yaml`;部署契约测试。
     - 6b 等 2b~5 批定下各服务的 `ControlRedis` 配置段之后再做:各服务 ConfigMap 与 C++ node-config 接控制集群,gate / battle 去掉 Redis 口令环境变量,Java 网关集群节点,退役核心 / queue / ctl 实例与 `-RedisSplit` 等参数。
     - **11-13 6a 代码写完,未运行**(28 个文件,验证命令见 PROGRESS 2026-11-13「全部 Redis 上 Cluster 第 6a 批」,落码与偏差见设计稿 §10.6;要等 6b 落地后一起验证,因为第 2b、3、5 批已改掉旧配置段,现在的 K8s 渲染起不来)。
     - **11-13 6b 开工**:第 2b(scene_manager)、3(C++ `control_redis`、gate / battle 不连 Redis)、5(guild、friend、player_locator、data_service、trade、player_inbox)批的会话都已告知配置段定稿,第 5 批的 5d(`k8s_deploy.ps1` 渲染)并进 6b;data_service 的 `NameCacheRedis` 仍保留,等名字缓存改完再合并。
     - **11-13 名字缓存改完,第 5 批代码全部写完**(G4a 会话 [5e2384],未编译):`NameCacheRedis` 段删除、并进 `ControlRedis`;批量读改单键 GET 管道(不用 `MGetBySlot`:不带 tag 的同槽两键会被控制集群守卫拒绝,理由在设计稿 §4.8);常驻启动无条件要求 `ControlRedis.Addrs`;`create_residual` 参数改 `-name-cache-addrs` / `-name-cache-cluster`。6b 渲染里 data_service 可以删 `NameCacheRedis` 段了(已发消息)。验证见 PROGRESS 2026-11-13「第 5 批收尾——data_service 名字缓存并进控制集群」。
   - **11-12 第 3 批由「Redis 上 Cluster 第 3 批:C++ 控制集群句柄与分片唤醒」会话认领并开工**(「Redis 分布式水平扩展」会话分派):C++ scene 两个句柄合成 `tlsControlRedis`、配置段合成 `control_redis`、三处读与归属对账、唤醒改 `SPUBLISH`、gate / battle 不再连 Redis、CreateScene 校验 zone(内部协议加字段)。Go `go/shared/assetopwake` 的 64 路 `SSUBSCRIBE` 由同题会话 [0cc556] 做(两边对好频道写法,只改那个包,订阅方调用点不动)。K8s 的 node-config 渲染与 gate / battle 口令环境变量仍归 6b。要动 `player_landing.h` 的唤醒常量、C++ 的 `control_redis` 配置段或 CreateScene 节点请求的会话先找它。
   - **11-12 第 3 批已落码,未生成协议、未编译**(PROGRESS 2026-11-12「全部 Redis 上 Cluster 第 3 批」与「第 3 批 Go 部分」两条,协调与偏差在设计稿 §10.5):3a 句柄、配置、唤醒发布、gate / battle 不连 Redis;3b CreateScene 校验 zone(`CreateSceneRequest.zone_id = 7`,节点 zone 不符或为 0 拒建);Go 订阅自己收包与重订,没用 `ChannelWithSubscriptions`(go-redis 在集群上自动重订会连错节点)。另加 Go 订阅端的真 Redis 用例(按环境变量开),Codex 在本机 7.2 与 8.x 两套集群各跑一轮。**K8s 上第 6b 批落地之前不要部署 scene**:旧渲染的 node-config 让它连到 127.0.0.1:6379(渲染要求已发给 6b,写在 §10.5)。仍剩:订阅只订本副本分片(第 25 条)、C++ 在真集群上的冒烟(等 2b、5、6b)。
   - **11-12 第 2b 批落码,未编译**:做 2a 的会话接着做(负责人 11-12 在该会话说「没做完就按最标准的方式去做」,并同意这一批一次做完)。scene_manager 的 go-zero Redis 句柄与 `CoreRedis` 段退役,控制域与 gate 标记都走新的控制集群句柄 `controlredis.Client`(`clusterredis.New(…, ControlClusterKeys)` 上的形状适配层,调用点不改写);配置段 `ControlRedis`,配置里还写着 `Redis.Host` 就拒启;选主换 `leader.NewUniversalStore`,删 `shared/leader/store_gozero.go`;负载分刷新带围栏的 ZADD 单发;测试里经控制句柄读写玩家域键的 190 处改走玩家域句柄。52 个文件(问的时候估约 45)。验证命令见 PROGRESS 2026-11-12「全部 Redis 上 Cluster 第 2b 批」。**第 6b 批落地之前不要往 K8s 部署 scene-manager**(旧渲染会拒启),渲染要求写在设计稿 §10.4 末尾。
   - 负责人原话:「我要分布式水平扩展」。按 zone 组分单主实例(`redis-queue-g<N>`、`redis-ctl-g<N>`)和核心单主都不算完成,所有 Redis 都要能加主节点扩写入。
   - 现状(11-10 只读盘点):
     - 只有玩家域集群(玩家 + 登录域,已落码未运行)和 match 私有集群是 Cluster;排队、控制域每个 zone 组一个主;核心 `redis` 全服一个主,上面有 gate 标记、唤醒频道、四个服务的选主锁、guild 与 friend 私有键、player_locator 控制键、名字缓存。
     - 只换地址会出错或静默失效:入队跨槽直接报错;gate 排空过滤与网关限流跨槽后 fail-open,不报错地失效。
   - 方向:
     - 新建控制集群 `redis-control`,多键操作的键按 zone / 帮 / 玩家共用 hash tag;唤醒改分片 Pub/Sub(`player_id % 64`)。
     - Go 句柄全部换成与玩家域同一个构造器,go-zero 的 Redis 句柄退役;C++ scene 合成一个集群句柄,gate、battle 不再连 Redis。
     - 退役核心、queue、ctl 实例。单个 zone 的控制域与排队仍各在一个主节点上(设计 §6.2)。
   - 分 7 批,每批不超过 30 个文件,合计约 170~200 个文件。断线重连清单的 R8 并入句柄替换批,R7 归断线重连会话(11-10 两边约定,设计 §13)。
   - 依赖:唤醒订阅只订本副本分片,要等第 25 条;排队按名次拉长轮询间隔,要先核对客户端遵守 `RetryAfterMs`。

### 11-11 新发现

35. **(11-11 登记;出处 `docs/PROGRESS.md` 2026-11-10「缺口清单第 27 条帮会邮件按帮计数、第 20 条写者闸门加测量指标」的「顺带发现」)mail 的截断告警永远不会触发。**`go/mail/internal/metrics/metrics.go` 的 `ObserveListTruncated` 没有任何调用方,告警 `MailSystemListTruncated` 依赖它,所以 ListMails 读系统有效集撞上限时没有任何信号。方向:按代码事实二选一——在撞上限的路径调用它,或确认没有这条路径后删掉指标与告警。**11-11 起由「缺口第 25 条残余与第 35 条:回收分工、活性指标、截断告警」会话接。** **11-11 结论与落码(未编译、未运行)**:没有这条路径——ListMails 从 10-30 起把个人、系统、帮会、活动邮件放在同一条查询里按 `(sent_ms, mail_id)` 翻页(每页 `LIMIT ≤ 100`),超出一页的翻页能看到,没有「系统有效集撞上限、少给玩家看」的地方;所以删掉 `mail_list_truncated_total`、`ObserveListTruncated` 与告警 `MailSystemListTruncated`,不补调用点。同类的死计数器 `mail_claim_short_circuit_total`(M1 领取短路,已被 M2 资产通道领取取代)一并删掉。见 `docs/design/mail-system.md` 文末 2026-11-11 节,验证见 `docs/PROGRESS.md` 2026-11-12「回收派发与 mail 过期清理按片分工、循环活性指标、删 mail 截断计数」;Java 只登记待做。

### 待确认

- battle 不上报负载(R6;11-05 已归回合制缺口并落码,未编译,待 Codex 验证:可分配状态 `NodeInfo.admission` 只在变化时写 etcd、按回读对账,房间数随 CreateBattle 应答捎回,match 跳过 CLOSED 并按房间数 + 在途选点;验证见 `global-match-hierarchical-pools.md` §12.4)、评分不分模式(R5;11-05 已归回合制缺口并落码,未编译,待 Codex 验证:评分按 `(player_id, mode)` 存,投榜来源与 outbox 按模式分片、每个计分模式一张榜(board_type 1V1=1、5V5=2、PVP_TEAM=3),封榜按模式停写;回合打满先看个人原件里 0 队是否全灭、旧版结果按模板 `time_limit` 换算的回合上限判平,删 `RatingDrawRoundCap*`;计分局禁观战核对为已做;验证见 `global-match-hierarchical-pools.md` §12.6)。
- (11-05 核对:不是有意的,已补,未运行,验证见 `docs/PROGRESS.md` 2026-11-05「新场景登记改成一段脚本」)`-SceneOrchestrator agones -AgonesHighDensity` 时 Fleet 的 rooms Counter 和 FleetAutoscaler 都按 rooms 计数,scene_manager 的 ConfigMap 却没有 Agones 段,Go 侧 `Agones.Enabled` 落回 false,rooms 没人记账;访问 Agones API 的 RBAC 也只能手工 apply。现在 `k8s_deploy.ps1` 的 `Test-SceneManagerAgonesEnabled` 为真时写 Agones 段、给 Pod 挂 `scene-manager` 身份并在 Deployment 之前 apply `scene-manager-agones-rbac.yaml`,不接管时删掉这三个对象。只开 agones 编排、不开高密度时仍不接管:一个 GameServer 一次只接一个房间的分配模式不适合世界频道与副本共用进程的 scene。
- 按玩家查询近期战斗终局的 RPC 没找到(`disconnect-reconnect-audit-20261020.md` §9.3),可能归回合制缺口。**结论(11-05):确认是缺口,已补,服务端落码未编译。** 结果在结算应用的同一帧写进存档里的有界列表、与奖励同一次提交,新协议 `SceneBattleResultClientPlayer.GetRecentBattleResults` / `AckBattleResults`;设计与给 Codex 的验证见 [战斗结果重登恢复](../design/battle-result-recovery.md) §10,客户端改动 §7 交 Codex,Java 版待做。
- match 投影表退役(R4):11-05 已落码(未编译),投影已无读者,不需要压测证据;见 `global-match-hierarchical-pools.md` §6。

## 明确不做

按玩家搬库、全局账号目录、跨地域实时对战、实时战斗的 buff 和技能 TODO(负责人:做成实时 MMORPG 之后才改)、NetEase 登录(等接入)、月卡 AI 托管。

## 查过,已经做了或已作废

MessageSyncRedisClient、redis_test、B0-3 全部、reassignSceneNode CAS、match 旧队列兼容、存盘 INFO、B0-4 部署接线、T-7~T-10、T-12、T-16、T-17、G-1~G-3、G-6、G-8、G-11~G-17、G-21、G-25~G-27、L-1、L-3、L-01、L-04、L-05、L-09~L-12、L-16、X-3。聚宝斋旧商品对侧账 TODO 只挂在没有生产调用方的 `EnqueueEscrowDebit` 上。
