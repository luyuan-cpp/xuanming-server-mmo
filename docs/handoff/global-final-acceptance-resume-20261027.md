# 全球同服剩余验收：全新主窗口交接（2026-10-27）

负责人明确要求“上下文太长了，你交接开新窗口去做”。本文件用于全新上下文接续，原主窗口 01a15c60-cdde-7f31-9dd4-26389fdf824d 停止新源码与原生作业，由新主窗口接管唯一重型执行时段。整体任务没有完成，不能把交接、源码合入或临时编译算成全量验收完成。

## 1. 目标、仓库与最新版本

完成两版全球同服迁移的剩余实现、正规协议生成、C++／Go／Java 构建及测试、真库权限与故障回归、完整双区业务验收。目标是写资格围栏、严格排空、SQL 原子裁决和资产幂等收据；不能承诺理论上绝不回档或无限扩展，必须报告实际覆盖与剩余限制。

- 主仓 R：D:/luyuan/xuanming-server-mmo-cpp。
- 独立 Java 仓 J：D:/luyuan/xuanming-server-mmo。
- 临时证据 T：C:/Users/luyua/.codex/tmp。
- 不使用完全不同方向的 D:/luyuan/xuanming-server，不读写独立客户端。只在当前最新代码上增量修改；CurrentMaps、新地图、他人 SDK 和业务改动必须保留。禁止 reset／checkout 回退、复制旧源码覆盖、手改生成物、跳检查或删除断言。
- 交接时重新读取到 R HEAD 为 886dd2ceeeadfe250724ad4660eddcc05173bc1b，J HEAD 为 7dc8c6b2bb436ad690c402b00ff3b0bb4947d092。R 仅显示 librdkafka／redis／sol2／ue5navmesh 四个子模块内部修改，J 跟踪工作区干净；这不是本窗口创建的提交。本窗口没有 commit／push／tag。
- 前面运行证据的 HEAD 多为 R bd2b9715c8a123867f1c60788eb3122ef0f3a425、J 03a0708d35b1e938d59f51c4e868b45f88ed9da5。新提交保存了大量既有修改；接手者须读最新 log 和增量、按实际源字节重新绑定输入，不能把旧 HEAD 清单直接换标签，也不能回退到旧 HEAD。
- 交接机器快照：T/global-final-acceptance-new-window-handoff-20261027-r1/CURRENT-REPOSITORIES.actual.v2.json，SHA256 9489B7EBA85240B20EA8706C98B933A41AC28774FE354931DCD9572FD5F3F49B。含两仓 HEAD／状态／与前次验证 HEAD 的路径增量及关键当前源 SHA。v1 一项 utils 路径遗漏 internal，v2 已按实际原件纠正，未改源码。

新窗口按 AGENTS.md 启动门禁读 PROGRESS、copilot-instructions、相关设计、log20、可访问的打开 PR／Issue。重点设计为 db-write-fencing-mysql.md、global-data-layer-tidb-decision.md、cross-zone-matchmaking.md、节点排空与写资格相关设计；项目尚未上线。旧审查长文 docs/handoff/global-economy-claude-review-20261022.md 保留全部历史，当前状态以本文件及更新后的实际证据为准。

## 2. 已有授权，不重复询问

负责人最新直接指令：“授权按已备方案扩至545个文件，PD调整为1 GiB内存、2 GiB内存与交换区总额并恢复一次。继续做完剩下的编译、真库和双区验收，只能基于最新代码改，别覆盖新地图和别人的改动。”

授权原件 T/global-scope545-PD1G-direct-authorization-20261027-r1/human-authorization.actual.json，SHA 137E0EFDCBDDD4ACF1F1CF6564CF78ACF736FC502AFEB9E3ACAEC8F5715BEC4A，绑定范围方案 0B7492D16663BDFCFB39908608BBCEC770615A1BCDE3D3EF24230984C5B51AE9 与 PD 方案 BED01183E174C78FDCEB7A9201B383B9D809F01A64E4CB868CCDFD330B7B481B。

- 545 为唯一手写路径上限。旧535已具名531，旧测试预留余4；新6固定路径已接，Discovery POM 使用1条既有POM预留，现已具名538，POM预留余3，总上限545。不要把整仓提交／dirty 都归为本批作者修改。
- 普通验证与服务按已获批提交余量门禁：启动至少12 GiB、运行至少8 GiB；物理内存只记录。单重型作业、实际2 CPU、采样目标1000ms／硬预算2000ms、未知重型进程0。不改业务超时、JVM／容器上限或系统页文件，不关闭用户应用。原授权 97F3E3F6AB0148077C6CCB162BD7C94032BD94BC99D7995A3A8A6124B44B5159。
- 已授权本机隔离 TiDB 随机库、短期账号／角色及逐表列权限验收；精确核归属后仅清理本次对象。已有账号授权只能撤销本次新增量，不可清掉原授权。密码只在受控环境或忽略私有文件，不入跟踪源码和日志。
- 允许持续向相关本机构建聊天协调时段，保留最新代码／地图，完成当前阶段后交还执行时段；不授权杀新进程或改他人源码。
- 支付先做本地 mock 全链路，真实渠道默认关闭，以后再接。公示48h、寄售7d、手续费5%向上取整到分、收入冷静3d、角色出售先离帮／保好友、货币默认仅金币，均可配置。
- PD 本次更新／恢复已成功执行，授权已经消耗，不能再自动 start／更新／扩容。

## 3. 本轮已实际落地的源改动

这些是实际 CAS／三方集成，并不等于完整测试通过。

| 内容 | 实际证据 | 当前边界 |
| --- | --- | --- |
| Java common 单一生成 route_authority，player-store 排重复，Discovery test-scope 现有驱动 | T/global-scope545-PD1G-direct-authorization-20261027-r1/applied-J-three-POM-20261027T062228774Z/actual.json，2B8DCF6521C132320C2A90495B5E84A803A0A2F2B88975BD306020C913BEB35C | 保留 descriptor／DDL；不向 Match 引入 PlayerStore 自动配置 |
| 生成器 private 单槽 protoc 执行及两个确定性测试 | T/generator545-two-source-integration-20261027-r1/apply-20261027T062238038Z-304ce95e/actual.json，D6EC00B80FF8777259557DD53D288AFE8C8E248D2C6C4724AD91A52741F81493 | canonical 路径在 tools/proto_generator/protogen/internal/utils/；utils.go 当前 F9ECCE40F27D7DEABC245A181F1AC9857119B090985D10A92743A4171791AD51；格式后 proto_test.go 为172784403C13FFC1BAC3DE47A83FA3235802CB20DE0A4132DB38952344759546 |
| 在线合服 NORMAL 工具6实际增量路径，3既有＋3新增 | T/merge-normal-authorized545-20261027-r1/applied-20261027T062738239Z-2355c270/actual.json，239DAAFF6597A42B6A857A0C352C513BC4346A27273C8A268FC5B9E271823A3C | main／current_tidb／route_store／pin_placement／relocate_run／route_store_test；真正三方合并保留最新格式；此前“8路径”记忆不作为范围依据 |
| 以上6＋生成器2正式 gofmt | N/native-stage-20261027T063448576Z-50e36a6d/proof.json，2CBB97176DE262FEDEB68BD3F1B510924A8303363EF11986FC66653BED56EC37 | 8路径标准格式，不改生产规则 |
| R AuthorityConnectionTest Mockito 构造顺序两行修正 | T/r-authority-jdbc-mockito-fix-20261027-r1/applied-20261027T063946219Z-71cc023e/actual.json，E977DE6623A4FC195639FB41CDF86418FB3997834B88D9F78230BCB38C977E51 | 当前源05B4333C74DBF50261292472B654E11E773BFD33AE41501A779DADE4C8F09DAE；保留13单测＋1真TiDB，共14默认测试尚未跑 |

下文 N = T/wire-trade-inbox-proposal-20261026-r1/native-stage-entry-20261026-r1。所有短 SHA 仅供定位，执行前读原件完整 SHA，不拼造 actual。

## 4. 已完成的运行证据及覆盖边界

- schemamigrate 完整 build／test／vet：52 PASS（37顶层＋15子项），0FAIL／0SKIP，N/native-stage-20261027T022542180Z-3c8ffe6e/proof.json，1B65F1D713B8431721E130058AB12E15C9618F2BA4B600B0D4C0613D00D65F01。不能替代其余13 Go模块。
- 部署／排空三个脚本合同25／25，N/native-stage-20261027T060309444Z-a53b1942/proof.json，B00C9B5D1CFF371B60B3C8330EAD794FB9D91E22F14395C8C77ED100C1999C08。临时／DryRun合同，不是真实服务排空。
- J parent／proto／common 完整 clean test：175 PASS＝proto10＋common165，0FAIL／ERROR／SKIP，T/java-match-generated-receipt-classpath-fix-20261026-r1/native-stage-20261027T061102663Z-781f3c76/proof.json，88AA94FCBAF41B9158751197F3A454A3A4D675D417421A6581C5E71B030F938F。12591输入后核无漂移。此轮早于最新3POM及契约同步，不冒充当前全31项目。
- 官方生成器正规 proto-gen-build 通过：N/native-stage-20261027T063642209Z-76c819f4/proof.json，D4FAD18CD24462C1756A15C7B95D14EE08DBF158D3D4E0FA76D30D3031D5BFCC；实际两exe pbgen.exe／proto-gen.exe 均35D52C39CD3C8A941281F9F187092F8BEEAB56125E78DF0160B82CFCEA066E74。正式构建来源，不是拷贝tmp候选；完整生成仍失败，见下一节。
- J 官方 ContractSync 同步＋check 已通过：N/native-stage-20261027T065356837Z-36f620f0/proof.json，D30B742D523C93A4DD846F91B668C5AD22CBB7E4B9F3FFBEF63A1403B629C7C3，4native全0，8家族全退。后置实际交付 T/java-current545-contract-sync-preparation-20261027-r1/post-sync-current545-r1/DELIVERY.post-sync.actual.json，B70EFD3CB65B58A24E3A03DC84279058CC34C0B640657577B9FF2291DAE6E065，passed=true。
- 该同步后 protected-and-maps 为AED906A3582E593605914FDEC9725A7F56CB5DAF51E5F6181D6EFB5901A8FF5F，R933／J2500保护及完整namespace／8maps／metadata均核；managed252→252，仅SOURCE＋四proto共5变，未增删，247不变。当前 J/contract/SOURCE.properties SHA F8138A639E3D5947FEA3EA195D7D9748209DA60158DA308DE7F3CD49BCCE41FE，仍绑定当次R bd2b；最新提交推进后须重新核内容／绑定，禁止手改properties装作同步。

## 5. 完整协议生成：当前首个编译前置阻塞

正规 no-unity run 已实际失败，不可直接使用半生成物构建来声明全绿。

- 计划 T/generator545-official-run-boundary-preparation-20261027-r1/official-proto-gen-run.after-official-build.final.r2.plan.HOLD.json，D190F7381881FE7DD2933D173C44F303E908E3AEBF477B4900254DA81C5486A0。
- 失败 N/native-stage-20261027T064432375Z-c8aca7bb/proof.json，9E3CD5BD78CF72DB2D1DCFAE704B4DB120879B5B25BAF95A91D610246732D576。81.884s后“资源复核耗尽2秒单调预算”；primary受控停、IO退出1／ledger未闭，29家族最终全退，未见业务 protoc 报错。旧BC02及本次红件保留。
- 本次真实 before：同准备目录/final-before-after-official-build-20261027-r1/snapshot.actual.json，53833462CD26A97BFA28045B3510EAE65EEC19EF2665976370049CD26623F1BA。350源观察、35输出域1721文件，26文件／182guard与原基线一致。
- guard基线 T/wire-trade-inbox-proposal-20261026-r1/guard-baseline-before-generation-20261026-r1/GUARD-BASELINE.manifest.v1.json，5978735209BCB2C1BBFF1052892910AF1932219CCE6117164D443B1F9569A96C。
- After采集器 Capture-CurrentOfficialGenerationBoundary.r2.HOLD.ps1，7F504535CFC0414B53EB28BDADBB9A61E815455B8A9C2AA877D2AB75E2E44826；参数 OutputDirectory／Phase After／BeforeManifestPath／BeforeManifestSha256。尚未生成完整成功后的 After。
- 已排除“每个样本重复hash351输入”之说：输入核验只在命令前及准入等待结束。GetFast内2秒覆盖 Toolhelp、NT、Add-FastFamilySnapshot、heavy分类；成功样本64总1730ms但Toolhelp13ms／NT37ms，新增family1，具体余耗时未分段记录；失败样本70为2075ms。不能把CIM／新增子进程image hash猜测写成确诊。
- 下一步仅准备诊断投影：保持原2000ms同一Stopwatch、资源阈值与失败语义，给上述分段记录内存耗时／原异常栈，结束时写证据；不引入热路径磁盘IO、不重置时钟、不静默放宽。原冻结脚本不改，新具名入口最小投影且逆构原文核验。该helper代理额度中断，没有完成此投影，需接手者实现并审查后再执行，不能盲目重跑。

## 6. PD已经恢复，SQL资格尚未闭合

本次恢复成功且只消费一次：

- PD固定CID a0c1c5384ce1696b606a7efca6bca4803dc413873a4c7fffd67c2a6e01a680d7。
- TiKV固定CID dda06e80828c32db2af3020ea0fc2ffe56c569947b3c3731a2200e9725d30f64。
- TiDB固定CID d7904a278d938dd1c9fd6b5fcc466834f95d6afdf572f244bb204e303d2d1d51。
- T/owned-PD1G2G-recovery-exact-five-20261027-r1/actual-five-20261027T063807140Z-e99dc355/proof.json，403B15C4ACECD7D9AB6F297EDC338C8080187E6E50B7B5116CE389644BF15262：5条Docker命令全0，仅两个cap变化，update／start各一次，原CID／镜像／卷／配置不变。
- PD RAM1073741824／MemorySwap2147483648，实际新StartedAt 2026-10-27T06:38:08.542994065Z，PID56254。原恢复授权已消耗，不再自动恢复。
- 恢复后三台 full inspect实际 N/native-stage-20261027T064119950Z-5d0c5cb7/proof.json，B1BBA7FC5796D3F34114D2B7881937BD3EA3B7BED5355B1F3313C6AD07361B13。原stdout位于同run/owned-three-current-full-inspect-after-PD1G2G/owned-three-current-full-inspect-after-PD1G2G.stdout.log，19FF6A160A6B48A54C11311B4804D1D6E6F88833903063C24474A65BD553369F。三台healthy、OOM=false；TiDB固定8.5.8维护构建849a85c3795c3472b46f59a8f2917b880d7ee923，127.0.0.1:24000。
- raw所用32字段真实绑定位于PD目录/raw-four-after-current-three-20261027-r1/fresh-three-32.binding.Root-approved.json，C4565C0F46159B615AAD87496E66F31411344D344FE8DD0D6BE2723CAA5FC82D；PID／纳秒时间／null类型均保留。时间已推进，下一执行仍须当前前后inspect核对，不能拿旧32字段伪装新运行世代。

### raw四物理强会话失败与V2候选

旧raw已实际失败1235：N/native-stage-20261027T064820599Z-854954c8/proof.json，460DA0155FD72D15F28A833372DE6EF6FF3E39AB637FACAA053AC35F739CB213。4物理连接头身份及8强变量均通过；第一个 BeginTx(ReadOnly:true) 被TiDB默认拒绝 mysql_1235_42000，尚未进入真实KV读取；四连接及pool关闭、前后3CID一致。不得计为S1／I3通过。

原因是 TiDB 默认不支持 READ ONLY 事务；不能开启 tidb_enable_noop_functions 冒充数据库只读保护。已备V2改为显式 READ COMMITTED 普通事务，固定SELECT mysql.tidb后Rollback，只读由工具固定SQL限定，并明确 backend_read_only_protection_claimed=false；无DDL／写SQL／自动fallback，原70s／4连接／8变量／KV／Rollback／关闭／前后identity均保留。官方依据见 https://docs.pingcap.com/tidb/stable/system-variables/#tidb_enable_noop_functions 。

V2目录 T/raw-four-explicit-fixed-select-V2-20261027-r1/，仅已备两源，尚未编译／执行：

- raw_four_session_bootstrap_v2.go，D2BFD041170632B53D6E631A0D2EC076F932DA027928040E3FA4C6679AFCF918。
- Invoke-RawFourSessionBootstrap.v2.pending.ps1，F9A3428D4A25E801F2AC39FC7F80B709305C9E5940CC7BFBDD35E2728BA4344A。
- minimal-two-source-delta.actual-static.json 保存17处受限替换／逆构／Parser检查，仅静态证据。helper额度中断，没有完成build／执行模板，接手者审源码后沿原AE600s离线build，再填实际exeSHA到新72D240s执行计划。
- DSN仅通过既有私有文件传入，不读取／打印内容。路径 T/sql-lock-primitive-qualification-20261026-r1/private-context-039c1e356c3544c18c10f3adbbb0a6ba/owned-tidb-admin.dsn。Docker必须本机pipe＋本轮独立空config，不复用失效身份。

### I3与真实外部fixture

I3目录 T/raw-four-to-I3-successor-20261027-r2/，冻结交付BD004AAAA7456535E8481A684C8A97DAF55C8EA2801A5828285B04479ADC8021；reader9EF268E6／launcherD99187AD。旧Go源码9173511F38E21A59C39AC6E72D6E4B7E6F73202B432C8BB73FF11CFE78569585已真实build B9C4B8A4164789923F6B6998D54061F99E5B6D156E5D4C7756F9175C4EB80EC1，对应EXE EC09DB10CA3159B5979360E6AB06464D5CA87685C10BABEDDA46B72E027007E0，尚未执行。r2只修精确核原句柄退出的stale ABI，正向8E2＋两负例04262336通过，不是SQL资格。

rawV2成功后，需新具名I3 reader／Go读取显式事务V2格式，保留四连接与所有强语义校验。Root从独立fullinspect＋成功raw bracket建立真实local physical binding，再完成 SELECT_ONLY／LOCK_TOUCH、64slot、真实DATA_LOCK_WAITS、权限拒绝、旧行不变、精确清理；不得以SQL变量自证物理来源、不得伪造actual／生产clusterID。权威DDL R/go/shared/authoritysql/catalog.go，CFEA64F6BDE3072C22EFCDCAB8C25A2F4A262805E727C552D8CD38247D4413DF。

外部R14正式fixture由架构继任窗继续准备，目录 T/r-authority-external-fixture-concrete-plan-20261027-r1/。GO-FIXTURE-EXECUTION.PLAN.HOLD.v2.json 为46F0DE0D4BA3757B23EA79FFFAF81C7CC987BC6F4270392F6790E1231C29E77B，含正式data_service migrate、render-roles、writer_authority register/status argv、私有模板和逐条SQL；不是可执行完成证明。

发现两个执行前必须封口的问题已交继任窗：完整operator叶对既有Schema/admin新增具名库授权，清理须先记原授权，只撤本次增量并核恢复；另核 schema_migrations 是否确为当前migrate产物，不凭计划造表。所有真实GoEXE／physical／raw／I3／helper引用仍待actual。USAGE新账号探针必须无默认库连接后SELECT CURRENT_USER，不能在未授权库上先选库。

## 7. Java接线与全量验证

三项新真实TiDB测试必须隔离自己的SQL身份，不可用全局 game.it.mysql=24000 伪装所有旧MySQL专项。旧CatalogAdmin固定4000、FriendConnectionPolicy的MySQL13316判据、Commerce URL形状等原断言要保留。全31普通组的外部条件SKIP必须单列，之后按410真库专项补齐，不把普通组SKIP改判通过。

已备3文件专属参数 game.it.authority.jdbc，fallback旧game.it.mysql、缺失／空值fail-closed，候选未实际写canonical：T/java-authority-test-target-property-20261027-r1/DELIVERY.candidates.HOLD.json，9CC628F35F77A6844886E8FE5AFC80811F63309FE768884D736A1C77E893D0E6。最新交接快照仍核三源before：Discovery749DD663、MatchE4A4534B、SceneC3B11AA。Java协作新窗负责纯文件核对／新全源计划；主协调重新核最新before后实际CAS并回传actual，再由Java协作窗捕新全源，避免双方同时写。

全31执行准备目录 T/java-current545-full-clean-install-20261027-r1/：

- runner run-native-stage.java-full-install-raw-seven.r1.HOLD.ps1，DC55FCDF45E65050BB069CD49E2B5107509D5615D3A838FCD8BADC2C614DBE3C，在原r5上仅加6个raw/helper env＋显式GAME_MYSQL_PASSWORD内存注入，逆构／23形状用例8FE7A332A6F4F4CB80114F27274F10F1BB2C0F1A49095ECAC72FC1F1A63C8059。
- full31-clean-install.raw-seven.TEMPLATE.HOLD.json，ADCF5E42693AF8213A4A1CB02D58AEC3D31329F100BEC17532A673F81441400D，仅模板，不可当actual运行。需最新HEAD／POM／SOURCE／全源短投影／全新head缓存与只读tail、actual raw／I3／helper绑定后封具体计划。
- 计划完整offline clean install，31项目，无测试filter／skip；7200s命令／7500s总预算、2 CPU。不能拿临时test-compile或旧175个单测代替。

R独立 authority_jdbc 原完整14测试另有具体入口 T/r-authority-jdbc-full14-execution-preparation-20261027-r1/INDEX.v1.json，379C9C3D70D78613F400C5F59C5CFEA1ACDE99E3C5C883F2D54E80C948C1E181；计划1F431870326611489CBA35625BDDB797B9ED24E0A49345D1693D30DE8858EC5B，真实refs仍空。R须其正式10env，不能直接套J七env；独立未消费NORMAL boot由正式fixture注册，先有exact私有3键，后7键内存注入，不落最终10键明文。

R现POM固定mysql-connector-j9.2.0缓存缺失；依赖下载方案 T/r-authority-mysql-9.2.0-dependency-plan-20261027-r1/MYSQL-9.2.0.DEPENDENCY-PLAN.v1.json，81F2EC13BA04AF33DD1A7FB9749835867816E45C891BAB1C5F7779D5BEE29C96，沿dependency-plugin3.7.0:get、固定Central、严格checksum，隔离freshhead和只读tail，不升级POM或读取用户secrets。尚未下载／跑R14。

## 8. C++／Go／双区仍需完成

- C++当前545准备 T/cpp-current545-build-entry-preparation-20261027-r1/DELIVERY.readonly.json，53B7040EEA2ADF79BE30C6FAA8C43065FAA26DD70D75A44CA1FFBB7AFB29956E。46个solution配置、29测试，其中4个方案外aoi_delivery／ecs_context／muduo_grpc／rpc_controller必须另编；当前50工程已静查官方grpc／gpr生效链接为0，但静态不能替代运行。
- game.sln原成熟预算10800s，RPC独立3600s，其余3独立1800s；串行/m:1 /nr:false、Debug|x64、CL_MPCount1。通用AE总预算5400装不下game，必须沿成熟长预算入口做最小后继，不缩短原预算。
- 最新独立Protobuf／Abseil／GTest／BoringSSL SDK已由专项窗口完成迁移／移除旧官方grpc，保留最新改动；不要借旧grpc库／旧SDK。当前主线完整生成通过后重新冻结实际输入，完整构建＋29测试／互通／节点回归。
- Go剩余13模块默认build／test／vet准备 T/go-default-test-gate-20261027-r1/GO-DEFAULT-13.READONLY.v1.json，75F6381EE3D0364B9ED3EE7EBBD921A5D8E70874833DE243335D3F27E1FACD16。普通外部条件SKIP与真实专项分开；生成器canonical新两测试也要实际执行，不能借旧overlay14冒充。
- 真库约410项当前源集合、Redis／Kafka／etcd新运行世代、主版Main36／Java33完整业务双区与故障链仍未全部闭合。旧2026-10-25诸绿只证明当时源／当时运行世代，不直接继承到最新写资格架构。
- 部分Battle SQL commit／丢响应的JDI故障检查点及normal RPC故障装配曾被自动审批以“可能涉及网络安全风险”拒绝；不得换线程／工具绕过重试，不标PASS。普通双区仍可做。未解限制在最终交付单独如实说明，不把此限制扩大成所有测试不能做。

## 9. 执行入口、分工与接手顺序

冻结通用入口均在N：run-native-stage.HOLD.ps1＝AE1030C55068499CAC616A84A0F4E2EC8182468098D3D87F542FC7AC9CCDC85F；with-pwsh＝72D187F1E2C474CB19E9734795C6132DD7AC5FCC62B677C7E9A1CF87ACD3E6BC；with-owned-docker＝95C884193CE22396A058E7A6D9686EF7A1CAA96FB7DE58B6F213F2ADAF731410。固定pwsh C:/Users/luyua/.cache/codex-runtimes/codex-primary-runtime/dependencies/native/powershell/pwsh.exe，362A356CE7F0940EC74F73A8FC2C990A2CC24A38A11C90BBD8ECA947110AD139。执行前核原件及实际依赖；不改旧proof／计划。exec_command用login:false，不传sandbox_permissions。

1. 读本文件及机器快照，确认最新HEAD／增量／来源保护，收Java和架构新窗ID，明确新主协调唯一native／SQL／容器／构建执行；源和纯文件审查可分工并行，重型仍串行。
2. 主协调核最新before、实际CAS三专属测试参数，Java协作窗收最新全源投影；架构协作窗封外部fixture授权差量清理／migrate产物及统一INDEX。主协调并行纯文件诊断生成器资源耗时、完成rawV2 build／执行入口。
3. 正规生成完整成功后核完整After／守护段／CurrentMaps；rawV2真实通过后做新physical binding／I3／外部fixture。各独立路径在一个重型时段顺次执行，失败先诊断，不连续自动重试。
4. 当前来源C++／Go／Java全量构建、默认测试／lint，再实际真库与完整双区；精确记录通过／失败／SKIP／未执行，先有actual后填引用，不造伪资格。
5. 最终更新改动逐文件清单及证据，给负责人／Claude Code审核。用户多次催进度；只报实际完成和具体阻塞，不无依据重复承诺“还要4小时”。

协作旧窗：Java 01a17646-8537-7c12-814e-a55eb6180773；架构01a17646-8d2e-75f0-81d8-40c73ec6edda。架构新窗已创建01a17d3d-8e14-74f2-8896-fdb84a31fc68，题为“继续两版测试修复与最终验证”；其交接 T/r-j-authority-tests-new-window-handoff-20261027-r1/HANDOFF.v1.json，C77D19989B8C3C33DD9BF6CEDB6443AE8459D29D7543E94069DBD3DF65431BED。Java新窗01a17d3f-d684-7551-b598-10966a0ee5c5，题为“全球同服 Java 验证续接与收尾”，已明确仅纯文件核验并请求新主协调实际CAS。新主协调ID创建后在旁侧JSON登记并通知两方。

独立任务“实现Java版批次2.10”01a17704-f895-7801-aee9-ed62be8fcbf8工作树D:/luyuan/wt-port-next，不能读写或拿结果冒充J主仓；其第三轮重型已明确交还，后续未获新主协调交还前保持纯文件。“确认C++是否可删除gRPC”01a1784d-700c-7da1-8a8f-a0c55b230783仅准备独立ecs入口／核对，不能同时生成或构建主仓。原本窗自有native／IO最后均已结束；新主窗口开始前再次核当前未知重型0，不把聊天active／idle视为执行许可。

本次交接不安装工具、不改变容器／系统、不连接生产、不清业务库、不修改客户端／地图、不创建提交。旧主窗交接后不再开展新的业务写入或原生作业，避免双主协调。
