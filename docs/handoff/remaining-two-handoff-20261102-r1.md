# 剩余两项验收交接（2026-11-02 晚，America/New_York）

本聊天按用户要求停止在本窗口继续执行，转新聊天接续。以下不是全部完成声明。交接时**没有构建、测试或 SQL 正在运行**；仅协调聊天仍在做公开源码准备。

## 用户最新范围与授权

- **新 1：Java 修复与真库验收（旧第 2 项）。**
- **新 2：双区业务、故障恢复联验及服务器启动（旧第 4 项）。**
- 原第 1、3、5 项结果保留，不再周期重复。不要把新编号与历史文件中的旧编号混淆。
- **客户端现在暂停。** 用户刚明确“客户端先别管，你先解决完 1 和 2”。不要继续 Unity 构建、许可、Hub 或 UI 操作。
- 用户已明确授权 Root 接手剩余修复、必要本地验证、并行协调，并说“所有东西不用问我”。例行必要工作不要重复请求许可。仍不提交、不推送、不打 tag，不操作生产，不登录远端账号。
- 玩家持久化始终是 **TiDB/SQL `player_store`**；Redis 仅协调，不改回 Redis 持久化。
- 每 20 分钟仅汇报新 1、新 2，自动化 ID `20`；创建新聊天后旧窗口会转移该自动化。不要另建重复提醒。

## 路径、工作区与运行规则

以下为文档内路径别名，所有工具命令展开成绝对路径：

| 别名 | 绝对路径 |
|---|---|
| R | `D:/luyuan/xuanming-server-mmo-cpp` |
| J | `D:/luyuan/xuanming-server-mmo` |
| S | `D:/luyuan/xuanming-server-mmo-cpp/build/verification/stage4-continuation-current-20261102-r1` |
| A5 | `D:/luyuan/xuanming-server-mmo-cpp/build/verification/all-five-resume-20261102-r1` |
| N | `D:/luyuan/xuanming-server-mmo-cpp/build/verification/r-java-normal-fixture-entry-prep-20261102-r1` |
| 旧 Q | `C:/Users/luyua/.codex/tmp/tidb4g-qualification-20261102-r1` |
| 新 Q | `C:/Users/luyua/.codex/tmp/wsl24g-qualification-20261103-r1` |
| RunRoot | `D:/luyuan/xuanming-server-mmo-cpp/build/verification/r-main36-owned-01d9656dd7c54c64bb659a7a4fb15b81` |
| 容量恢复记录 | `D:/luyuan/xuanming-server-mmo-cpp/build/verification/wsl-runtime-capacity-20261103-r1` |

启动先读 R/AGENTS.md、docs/PROGRESS.md、.github/copilot-instructions.md、相关设计、最近提交及可获得的当前 PR/Issue；J 另按 J/AGENTS.md 门禁。已用 diagnosing-bugs 技能，继续按实际失败定位，不盲重试。

交接时发现其他窗口保存了提交：R HEAD `78d54128c28fa378997b4c8e46f00dcaee12f5f7`，J HEAD `ff31532574956e6408696c33385e2c518e0d2e3e`。此前实际命令的 build.commit 标签仍有旧 `ccbb4af...` / `99e1b6ed...`。**本聊天没有执行 commit/push。** 已核关键源码 hash 未变，但新聊天须核当前源与实际输入清单，不能仅凭 HEAD 推断通过，也不能回退别人的提交。R 有 PROGRESS 文档及既有子模块脏状态，J 当时干净。

所有 build/test/SQL 延续原 72D、CPU2、单重任务、独立 IO/family 完整门禁。C++ `/m:1 /p:CL_MPCount=1`。启动提交余量 12 GiB、运行 8 GiB，口径是 `GlobalMemoryStatusEx.ullAvailPageFile`，物理内存只观察；不降低门禁。原 native 总时限 5400 秒，Java 内层 4200/外层 4500、每类 fork 1800 秒，不扩大时限凑绿。

pwsh：`C:/Users/luyua/.cache/codex-runtimes/codex-primary-runtime/dependencies/native/powershell/pwsh.exe`，SHA `362A356CE7F0940EC74F73A8FC2C990A2CC24A38A11C90BBD8ECA947110AD139`。

原 72D：`C:/Users/luyua/.codex/tmp/wire-trade-inbox-proposal-20261026-r1/native-stage-entry-20261026-r1/run-native-stage.with-pwsh.HOLD.ps1`，SHA `72D187F1E2C474CB19E9734795C6132DD7AC5FCC62B677C7E9A1CF87ACD3E6BC`。

执行：`pwsh -NoLogo -NoProfile -NonInteractive -File <72D> -PlanPath <公开计划> -ApprovedPlanSha256 <实际SHA> -Execute`。`S/Start-owned-infrastructure.ROOT.READY.plan.json` 可作原门禁模板，但其旧 launcher immutable pin `4CD...` 已有明确 `5B830F2B...` 后继，不能盲目接受其他漂移。源码只读完整闭包动作可明确重建 source_files_before/immutable_pins，不能把私密原件加入普通 hash 列表。

**私密 DSN、owner、receipt、seed、inventory 正文不普通读取/stat/hash。** 只通过已审 CF3 API。所有私密内存对象、完整 Context/Profile/PSI/SQL journal 都不可输出或整体序列化。旧 UNKNOWN 不清理、不接管、不改写历史失败。

CF3：`C:/Users/luyua/.codex/tmp/windows-private-control-fixture-fix-20261027-r1/built-after-CAS/private_control_file.exe`，SHA `CF3A6787005EA0F801D8157C968215D4E8A1E8873682E1F83C73183A33EDBC40`。

正式 API `R/tools/scripts/lib/sql_boot_catalog_local.ps1` SHA `CD802C088F49F051833E5A0B4F601810C128A2D6A6CA0572D76D1F94A75081D1`；R14 `C:/Users/luyua/.codex/tmp/r-authority-sql-file-surface-20261027-r1/sql_file_surface.exe` SHA `2F95B0EB0C36CCA484BB33BBA4387036075E615FAB5F8A51DE026AFAF151A54A`。SQL 每动作 common clock110/每句60，先用正式 splitter 拆多语句。CF3 所有 read 失败统一 exit1，不能推断文件不存在。

## 新 1：Java 实际结果与下一步

原 428 精确身份锚：`R/build/verification/java-original428-expected-case-anchor-20261028-r1/review-r1/EXPECTED.original428.canonical-case-identities.NOT-PASS.json`，SHA `BF6CD997A6916E66E9D73227D142CB25D7EA89262A5AC95A99DD1C159CC48C42`。

分组：Player145、pbmysql10、Guild167、Team18、Trade35、Friend44、Data9，总428。集合拆为155/167/106；不要用原始 XML 总数或选择器数代替精确身份，不把 stale XML、BeforeAll 合成错误、未运行或跳过当通过。

已修 `J/game-common/src/main/java/com/game/common/sql/testing/AuthoritySqlFixture.java` 的 prepareInternal 两个 DDL 循环：接既有 owner DDL 作用域，普通/NORMAL/业务预算和原断言不变。当前 SHA `833D91A32461279836A3D34CB232CA85FB3C6662E9719C7954DF185F9C405F20`。

- 新 owner DDL 回归4 + 旧握手1 = **5PASS**，原 outer `native-stage-20261103T004114536Z-11aa9597`。
- 真实 PlayerRoute7 + prepareWithPlayerSchema2 = **9PASS、0F/E/S**，outer `004713180Z-fca02275`，2803输入稳定。
- 两者证据在 `A5/j2-owner-ddl-fix-20261103-r5-HOLD`，不替代428。
- r6 Guild167 首轮实际 **124PASS/9FAIL/34SKIP**；9是 TiDB 可重试死锁采集前置关闭，34是 MAIL_RANK JDBC30 + game.it.redis4 条件缺失。原件保留。
- 已同旧 TiDB 实例仅开启 `deadlock_history_collect_retryable=true`，容量10不变，HTTP前后仅此键变化 + 正式 R14 SQL 回读，outer `013217845Z-5fc9f91a` 完整PASS。结果 `S/TiDB.retryable-deadlock-capture.result.actual.public.json` SHA `5ECDBFF5BBC2F55438F893BE66BCAA3F14632AF560CA5A4765E4A6B428516490`。**重启后该运行时设置须重新实际观察，不能假定仍有效；不要重跑旧 CreateNew 动作。** 旧 helper wget 分支未单列代理证据，未来只用 curl `--noproxy '*'`，写前立即复核 fresh。
- r7 首批自然结束于本地20:53：**原167精确48PASS/119未通过；原始 XML 48P/0F/21ERROR/0SKIP**。其中 GuildActivity29、AssetStore锁顺序2 已实际通过；一个资产证明夹具缺 player 表，后续因 WSL 内存耗尽/数据库连接超时，大量用例未运行。outer `native-stage-20261103T013429413Z-b1109550`，源码2803稳定，完整失败保留。
- r7 目录：`A5/j2-current428-required-environment-20261103-r7-HOLD`；结果 `guild-167/actual-r1/result.json`，XML已复制到对应 surefire-evidence。**155和106均尚未运行**，r6/r7旧计划均绑定旧物理/I3，不能直接开跑。

r7补了 MAIL_RANK_TEST_JDBC/REQUIRE_TESTS/REDIS、TEAM必需环境与 `-Dgame.it.redis=redis://127.0.0.1:36379`；同CID/image/project/run/loopback36379 + PING实际确认，Guild DB10初始空，不清理或接管；GuildActivity DB0随机前缀沿原 cleanup。协调 Redis 不承担玩家持久化。

资产证明测试：`J/game-guild/src/test/java/com/game/guild/asset/AssetOpSqlProofTiDbTest.java` 当前 SHA `D190E1EA5A639C4F98140E37101F3184B12222B5F9D3CFD1CFB3EEB9974AD0E8`。本聊天将旧 root/空密码/手建库业务池接到既有 AuthoritySqlFixture + NORMAL + protected Druid；真实 r7 红为 RouteAuthorityInitializer 缺 player 表。**已准备但未应用/未运行**修正：

`A5/j2-assetproof-canonical-player-schema-20261103-r1-HOLD/AssetOpSqlProofTiDbTest.candidate.java`，SHA `744E0BFBA9C887FF845F010C036EE3F25EB8BFD8317A1D8298AC4E29137AB3D3`。

仅改用现成 `prepareWithPlayerSchema` 正式固定 player DDL，删除自写简化 player_state DDL，并在原事务 INSERT 补正式 saved_epoch/updated_at 列；原业务断言逐行相同。candidate.NOT-PASS.public.json 和 before 副本均在同目录。新聊天核源后可应用，再定向真实复验该原案例。剩余验收至少要完成155、106及 Guild未通过原身份，不盲重复已通过且不受影响的检查；任何跨批源差异须明确审定，不伪称同一全量快照通过。

## 当前最关键事实：WSL 容量已经恢复，但物理/I3换代未重核

故障实证：Windows总物理34164105216字节；旧WSL MemTotal16279428KiB、Available308284KiB、4GiB swapFree0，full pressure avg60=55.27。Docker inspect15秒无响应，SQL握手/读取超时；不是已证明业务锁序失败。

用户明确允许最大内存及全部必要本地工作。已 CreateNew `C:/Users/luyua/.wslconfig`：memory24GB、swap16GB、swapFile指向容量恢复记录目录中的 `swap.vhdx`（D盘）。C盘仅约12GB空闲，D盘约1.4TB，因此交换文件放D盘；**不要删除这个仍被WSL配置引用的ignored文件/目录**。配置SHA `D73030A3C7ABC960E0005B60FAAF9162C193F8BBBF1A90E76772E1A1B50BA4C2`。

Docker官方 restart120秒失败并持续stopping；仅 `wsl --terminate docker-desktop` 成功，再按官方失败列表持原句柄核8个PID/Oct30出生/映像，Kill(false)并观察全部退出，不杀树、不碰其他应用/WSL服务、不删卷。随后官方 desktop start成功，当前状态running。完整恢复记录均在容量恢复记录目录。

**已实际核新WSL：** MemTotal24608628KiB，MemAvailable23132028KiB，swap16GiB全空，pressure0。公开 `wsl-after-restart-memory.actual.public.json` SHA `36CD6A6A35CF52E3046FE476BC16BB90F4F87976690161D69FCBB0AEFE3F15CE`。

同7个原CID/映像/挂载/限制/端口比较后启动原PD、TiKV、TiDB、Kafka；Redis/etcd已自动恢复，不重启；topic-init原exit0保持、不重跑。原72D `native-stage-20261103T021331915Z-83482849` **完整PASS**。`known-seven-resumed.actual.public.json` SHA `00F6554FF38DB63E7F33A1C094982CB7B9D3D314EF127802B00C91F262134050`，SQL0、应用0、卷删除0。运行不等于数据库或业务就绪。

| 组件 | 原CID | 新startedAt（UTC）/LinuxPID |
|---|---|---|
| PD | `a0c1c5384ce1696b606a7efca6bca4803dc413873a4c7fffd67c2a6e01a680d7` | `2026-11-03T02:13:35.501677303Z` /1608 |
| TiKV | `95b8c34f6e862972a0db65b9c98d8025108afbba0e2909ed69ffac37730bf32d` | `2026-11-03T02:13:37.25484758Z` /2064 |
| TiDB | `d7904a278d938dd1c9fd6b5fcc466834f95d6afdf572f244bb204e303d2d1d51` | `2026-11-03T02:13:38.04824625Z` /2178 |
| Kafka | `03adeede97d7a789ecd384c101fb293795c8e1b97547b3383a785dcbd697ecce` | `2026-11-03T02:13:36.280161506Z` |
| Redis | `c8007bdc107b05f6158573240aa9b98ff3090eb10ef1922f99450297be04e6e8` | `2026-11-03T02:07:45.225622126Z` |
| etcd | `93ab3bb79c203407963c825b79c8cfdac502429740f0d647a51567a798f3cfc4` | `2026-11-03T02:07:45.217435128Z` |

TiDB仍4GiB/swap4GiB、PD4GiB/swap8GiB、TiKV8GiB/swap16GiB。SQL24000、Redis36379、Kafka29492、etcd42379均旧回环配置。容器镜像/数据卷未删除。

**下一步必须真正完成新epoch独立full/normalized32采样→Fresh→S1四held→physical→I3两原语及CF3投影→新context/owner续接/最小当前结构。** 旧 C75物理、D192 I3、旧120秒sealed和r10 context都只能历史，不改时间冒充。

协调聊天正在做上述新source候选：`S/wsl24g-requalification-20261103-r1`，目前只建目录/源梳理，未冻结、未执行。新Q见上。I3 Go旧421073源写死旧Q Raw launcher/E601，需新最小Go源后继及**实际构建新PE**，不能旧PE换label。

## 新 2：保留的实证、剩余运行工作

同 runId `01d9656dd7c54c64bb659a7a4fb15b81` / bootId `01d9656d`，不换UUID、不重建14库、不重放strict CREATE。

- 14库严格CREATE真实62/62：`S/Main36-current-run-SQL-receipts.readonly.r2.actual.public.json` SHA `05D0FA4F14ABDA76B3D0AD66BCE651AFFC461F19255387670C00BC9B2A6BE84E`；outer `195902646Z-58329649`。
- 7个服务迁移已成功，gateway/mall已初始化；剩 **match、trade、player_inbox** 三迁移。
- 两派生索引已实际补齐且9/9覆盖：`S/owned-index-supplement-20261102-r1/apply-r1.actual.public.json` SHA `3AB76E47F251B08DE3822AB5E6B3D014DEBD609B76E6144CAECFF93E8A04256E`，outer `232643545Z-47cba73d`。不重复Apply。
- 最近旧epoch199表当前结构：`N/normal-fixture-fresh-guard-bfa4532203094cdbb735dabfa951dff8/main36-schema-readiness.actual.public.json` SHA `7E140A95ECA748357334BFC671E7109576EAC77E417AD8035708FDCF7E00FE6E`；outer `004016565Z-aeba8f89`。199表两索引后连续性实际 `S/Main36.schema-continuity.after-indexes.actual.public.json` SHA `4DDEF2D9E1B70ACB3277240E725E0A8E8C96F12614EED1E5AA957381D3DEAAFF`。恢复后须读当前事实，不重复签旧结构当前。
- **Prelude已真实完成，不重跑**：`S/Main36.runtime-match-prelude.actual.public.json` SHA `AC38B8CA7771C4AAC33A2C713B5248062E4F379ED7741C89EAD6F06F6AB9F603`，outer `010600055Z-277f9b62` proof SHA `3297D662B93BCA78DEC99045C8E2D0DEC988AC8906A572D74D326D3F28BAC576`。迁移缺凭据精确拒绝、CF3共享密钥逐键回读、独立match Schema身份4exec4query确认。**NORMAL登记0、应用0、业务未验。**
- 私密原件仅后续CF3读：RunRoot/private/runtime-shared-domains.current.json，SHA `b511f58dd9792c3113baf55b323095d7b2c9e2a1f11327cbbcf0a280f8d45b87`；match-scoped-migration-identity.current.json SHA `49fa7d27d9db04584937b161025b7572575965647d2a6528ba5a191f88adbd50`。**不重新生成密钥或账号。** 密钥envelope源码7E21会显式核旧physical/I3，须在新实证后用明确同存储续接方案保留同值/原件，不把它与新context硬拼，也不旋转凑过。

仍需：新epoch资格→3迁移→26个NORMAL REGISTER、grant/catalog正规校验→Go28/Java4/Cpp4真正启动和68监听原句柄观察→管理API建990017/990018目录→真实HTTP登录/AssignGate/TCP票据/EnterGame/跨区→SQL存盘故障控制器及恢复→保留健康服务。当前**服务器业务进程未启动**。

故障控制器：正式 `R/tools/scripts/tests/run_ownership_fault_fullstack.ps1` 和 data_service/internal/faultdrill、robot；只持本run scene-0原句柄，实际UUID/zone990017/save30/TTL30/barrier20，原预算360+2*interval，不跳。还缺专用SQL观察身份（仅zone_990017_db player_store读取/锁及必要PROCESS探针），不能将root/schema DSN注入应用。原RMain36.OwnedFaultAdapter的旧Cpp建目录分支拒绝playerRedisEndpoints空，不用它；用当前Cpp参数/CF3配置库。

gateway原DDL仅seed zone1，990017/18经真实 `POST /admin/zones` 管理接口建，不跨服务SQL补。Go dev口令只dev/test、账号前缀robot_/dev_，密钥来源为已存CF3环境，不进CLI/日志。

## 已审源码接线清单（不是可直接运行资格）

下表均位于S，除另注明；**多数写死旧C75/D192或旧actual，必须新epoch最小后继后再审**。

| 源 | SHA/说明 |
|---|---|
| RMain36.PublicDefinitions.ROOT.ps1 | `ED10462483F9424B9507850D39A38EE164E61CB2D0E15AE7ACA8EC66786D2396`，完整函数声明导入，不丢外置参数 |
| RMain36.Core.current-runtime.r4.ROOT.ps1 | `7A4EFF096B2656F9A98DBACCF635C5392804D7E73EC662C50A82337FED87A37B` |
| go_services.runtime-owned.current-prometheus-memory.r5.ROOT.ps1 | `BCEE6873C83232C602502116874E086833B5F73EE3EF8D7ACB61F192BCB64A07`；明确start-exe消费链已审不打开私密I3原件 |
| RMain36.bootstrap-current-runtime.sources.public.json | `2F966CE4F185EDC125519F9AD9AAC7514DD8EB0FC79F58E63CE7059A9C736271`，冻结746基线，后继另显式pin |
| Invoke-RMain36Bootstrap.CatalogResume.current-r6.ROOT.ps1 | `04F686F6F03B295BCE37831F49B772F433DE469B0DF25EFBAE95A155C210CF7D`，Catalog-only续接，不重复CREATE |
| catalog-resume-currentdata-materialized-r1/guards文件、functions文件 | SHA `B0860068...FEEF` / `181B6326...5E2E`，9 Stage、3余迁移 |
| RMain36.CatalogActualJournal.MEMORY.r2.ROOT.ps1 | `AFB811A80A4B549DD7B2C5FE63BD29C5787A1E2D3265B354511B6914DC7F6118`，真实成功返回才记SQL/Register/Assert/migrate，不输出journal |
| tidb4g-requalification-20261102-r1/RMain36.CatalogResume.STAGES.PROVIDER.r5.DRAFT.ps1 | `BAF10114AB089C573BA45D428D9BE27C51744D47DDFEA91AF9DA03DB7055E015` |
| RMain36.CatalogRootAdapter.ASSEMBLY.r3.DRAFT.ps1 | `19C7BD26D7A508500FBBA8B19E547FD392AFE1671EC5D8324623274AB39A6390`，新Root须读接口和Inputs矩阵 |
| RMain36.PrivateCustody.READONLY.r2.DRAFT.ps1 | `DA04BC893A483A172E8C231E482CF01D052F0E0C589EFA712F035FFC2776BB7A` |
| RMain36.HistoricalCapture.READONLY.DRAFT.ps1 | `B55107EB27D67DA71336902A4CFA41575B0EEB41DCC624B96FA4A54D52BF4A12` |
| RMain36.ActualIdentityArguments.CF3.r2.DRAFT.ps1 | `4DE446E3247CA3CAC443C391CE2EA1D7D625152BFA9992C1E018893D6D0373C9`，14defs，回调返6BF十键；公开文件同批bytes核SHA；旧8087不执行 |
| RMain36.RuntimeSupervisor.JOB.DRAFT.ps1 | `4D38130BC14966B70C2FFB9C63BE3434B7E704867A3ACE7A7A8F3EBEA774D585`，独立长期Root非有限72D family |
| RMain36.RuntimeOwner.IDENTITY.DRAFT.ps1 | `6BF355CFDAC535CA472F40BC2971961EBB5E1B6FB80006CDCDEB606517B1FF07`，28Go原8字段+Java4实际birth，原句柄防PID复用 |
| RMain36.RuntimeSupervisor.PHASEB.resource-prime.r5.ROOT.ps1 | `3999A974217E45183767D4E33724F14ECCE18BBDE1E4C19FCBEC199DC53CBAF4`，真实Job之后/NewState之前prime；未实际AddType/启动 |
| RuntimeResourceProducer.TYPED.JOB.DRAFT.ps1 | `B1505CA7D7F9D3C5E7819BB35F15825FB243B3D23805170830A82CB5D41FC367`，真实typed Toolhelp/NT/Job分类，原2秒上限，未实际prime |
| RMain36.RuntimeSecrets.CF3.r2.ROOT.ps1 | `7E21FC693DC24E216211468145610B9E34D81AB8B0A53ABC0042733BC0EE668A`，旧物理envelope，不能重生 |
| RMain36.ServiceProfiles.MEMORY.ROOT.ps1 | `5A2C33635F9D384EB99B6779879495EFE06314D3D3CCF23C896CF3A83371122D`，36role环境只内存；先清ambient受管全域 |
| RMain36.CppConfigurations.CF3.ROOT.ps1 | `C8D82119A63546E64C098A7BAAA647B0E6257D6FE5C3B4D7C20260D2CFF86B4B`，未建Cpp目录，需真实完整B967计划/preflight |
| RMain36.NewBirthPrivacy.ROOT.ps1 | `547E2ED74F48767FA9EB78D6B09F1783032BEE06B1AD47B606882292D83C0883`，仅新Go pids目录/Java新boot leaf设正式ACL+CF3，原existing拒绝保留 |
| Invoke-RMain36Bootstrap.birth-privacy.r6.ROOT.ps1 | `CED26101C8E32FC4C607A35F92C8CE16702BE3250F52AD771A2694822CF4528F`，仅Launch变体；FunctionExtent `C844A76A4EA6D1952A0E910CDE54BA2FBD8B4A53DF709637DE99AF1F9D70A1E8` |

Java creator原body `F7327A4BA4FBE60CB7F072457ACCDFAC5FB69E2648DBA4CB7A769AE0795C988E` → private新body `F7419ECDBD585AFC459AF3D6D6F24F832851C59E659451C4265F75B9DFD3B204`，只替换新目录创建行，原拒已存在、全部birth CreateNew/失败清理/120秒保持。**不能预建Java同boot目录，不能改旧parent ACL。** 读actual32时用Go精确pids父根、Java精确boot父根和ctx.privateRoot分别CF3，不扩大根。

**两个未完成、不可运行的Root草稿：**

- `S/RMain36.RuntimeControl.PUBLIC.ROOT.ps1` SHA `D23041950093430E58610115C457DA1E57CCF6C698FEC639164031E57E0BCA44` 有已确认缺口：整体序列化Snapshot；seq未严格integer；文件读/Flush在2秒主循环；控制目录未独占防重放/父junction；Stop后Register需等原句柄实际退出。**r2尚未写，须先修。** 建议单独worker做文件IO、主循环只非阻塞queue，19个Snapshot/10个instance字段严格白名单primitive重建，seq整数正范围，本Host新独占目录+birth绑定；Register在主线程按实际原句柄退出后出队。
- `S/Invoke-Main36PersistentRuntime.ROOT.ps1` SHA `E4410BF99E4D4C6BDFE221F00447523A8AC4A5B14392F577A866B01D99A5A830` 只是未运行入口草稿：外部Root持句柄授权Gate、prime3999、32provider、Cpp4、loop；已发现/修JSON pid需Int32问题，但尚未接4DE provider真实initializer ABI、未适配新epoch、引用坏D230、父starter和真实Cpp有限装配未完成。不要直接调用。Root授权不能由子进程自签；源callback会在32reader后清Schema控制口令，需审实际同scope安装与bodySHA。

Catalog source审边界：noPreviousPublish仅本run受控流程+最终CreateNew不覆盖，不能假造catalogCount0/“文件不存在”；CF3失败不等于absence。历史ADC stdout只按8行与固定控制流推导前7成功，不是7份独立native exit；新AC38 negative另有精确实际。最终Catalog source-body captured originals和active wrappers都须逐stage绑定，26 REGISTER需实证，不是渲染SQL/配置字典。

## 已有运行包与后续计划

`S/Main36.current-application-artifacts.ROOT.READY.public.json` SHA `D43DF0B39B3DCF008C873A4F92A5BFE11012E41B95584F83585A94245A5BD919`。

- 最新19Go build/vet/test过（3358输入稳定），16Go PE已实际构建备份安装，`S/latest-runtime-go-20261102-r1/Latest16Go.qualified-install.manifest.public.json` SHA `ECA541F71B3A68DD4866351320E112E4AE473DF8C0FA54853323C2768C60EAD3`。
- Cpp显式proto.lib后gate/scene串行构建过（2140输入），gate SHA `C7CCD81E28A53B07328CF3243F99F2ABFC9E988A4C96CE8FF8BCDD8C74649E29`，scene `38FFBC91A18F78C063B3D8D9F44139C5A6A38AAB3FC0E6E36AA9E39731EB332D`。
- Go metrics正式launcher修正后84脚本测试PASS，R/tools/scripts/go_services.ps1当前 `5B830F2BA332C6AA9771B4DCB6E3A19A0B86513683CD42F794E7F6B90506C8E8`。
- Go28实际静态计划 `RunRoot/go-plan-current-prometheus-r4/go28.plan.actual.public.json` SHA `B7ECBA283630B04E604818DC19B628D90DC80428DA592A67FCC4839F98D71687`，54启用RPC/metrics唯一，两router正式禁metrics，不要求它们有metrics。
- Java4计划 `S/phases-prepared-r2/RMain36.Java4.canonical-jdk23.r2.HOLD.json` SHA `F3ED0C0C417BB640F5C112EB3E2E7A86C8CA65387CCF570523665ADE55BB276D`；mall18081/82，gateway18083/84；JDK23，四Java都bind127.0.0.1。
- 完整静态36进程68监听+Cpp4参数 `S/Main36.full-current-ports-and-cpp-parameters.r2.ROOT.public.json` SHA `B96795ECBEBAA74FB670FD1218C23276974DF57E041362835FA64FC85A882570`。Cpp gate19017/18、scene29017/18、scene grpc59017/18、metrics19171/72/81/82。只是静态计划，没有监听通过或业务通过。

## 周期报告与协调入口

当前检查点：`A5/checkpoint.json`，V2，历史item_status键保留，`reporting.currentItems`把新1→旧2、新2→旧4。旧窗口曾有一次XML变量覆盖checkpoint，已保留坏副本并从实际重建；后续用独特变量名/AsHashtable，不能复用强类型XML变量。

最后一次全周期汇报是UTC02:04（本地21:04），下次02:24。最近用户被告知后端顺利估算还需3–5小时，不是承诺；客户端现在暂停，不再报其许可为当前待解决事项。

现有已授权协调聊天：`01a199fc-276c-7e00-9010-bb5e6ef12a55`，host `local`，最近cursor `acef9fce-1ba6-4720-8580-aa22ff623898:81`。原用户明确允许协调。该聊天只准备/独立审公开源码，Root执行全部native/build/test/SQL；不会自己操作环境。它正在分工新epoch：主Fresh+独立two inspect；checklist S1四held；memory I3 source/reader/controller/plan/projection。新聊天ID出来旧窗口会通知它。新Root先接这个成果，别重复造框架或启动重复任务。

Root旧会话曾达到subagent限额，不用切model绕限；已有协调可复用。新聊天按可用性合法并行源码准备和只读核对，重任务仍单槽。用户要求持续做完，不要最终停在“计划备好/源码审过”。当前最先解决新epoch资格，然后继续新1/2，最后保持服务器可用。
