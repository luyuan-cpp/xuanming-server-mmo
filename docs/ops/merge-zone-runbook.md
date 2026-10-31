# 不停服合服操作手册

> **负责人要求(2026-10-30)**:合服目标改为**不停服合服**。具体方案由 Claude Code 自行调研现有实现与成熟方案,选择最适合本项目的方式。
> **状态**:工具、guild、trade、match 已按本手册落码,**未编译、未测试、未演练**。部署改动还没做(§9)。§11 的验证跑通之前,不能在真实环境执行。
> **方案**:[总体方案 §7.1](../design/player-data-tidb-scaling-plan.md)。**旧手册**:[停服版](../archive/merge-zone-runbook-stop-the-world.md),已作废。别处写的「merge-zone-runbook §N」指的都是旧手册的章节。

## 1. 方案与选型

合服只写一条映射:全局库 `zone_merged_into` 里的「源区 → 目标区」。玩家、公会、商品、榜单一行都不动,各服务读的时候把记录里的原始区解析成最终区。

| 做法 | 结论 | 理由 |
|---|---|---|
| 停服后改写(旧手册) | 不用 | 要踢人、解散队伍、排空,目标就是去掉它 |
| 在线搬数据(双写或增量同步后切换) | 不用 | 要解决搬动期间的一致性,每张表都要写一遍同步和校验。玩家存档按 `player_id` 存、靠 `player_route.storage_id` 找库,本来就不需要搬 |
| **只写映射,读时解析** | **采用** | 没有数据搬动,就没有搬动期间的一致性问题。提交点只有一个事务,撤销就是删一行 |

代价:所有按区比较和查询的地方都要走解析,漏一处就会分错组,靠逐处改和契约测试钉住;旧榜要折叠;撤销后合服期间跨原始区的关系留着(§7)。

依据是本仓代码和总体方案的推导,比较的是这三类做法本身,没有另外引用外部资料。

### 合并对象

| 对象 | 记录里存的 | 合服时 | 读时 |
|---|---|---|---|
| 玩家归属 `player_route.home_zone` | 建号时的区 | 不动 | `GetPlayerHomeZone`、`BatchGetPlayerHomeZone` 返回最终区 |
| `guild.zone_id` | 建帮时归属读到的区 | 不动 | 比较两边都 `Final()`;回档检查按 `zone_id IN (Members(z))` |
| 公会榜 `guild_rank:zone:{z}` | 可重建缓存 | 旧键周期折叠进最终区(§4) | 读写都按最终区 |
| `trade_listing.market_zone` | 上架时归属读到的区 | 不动 | 可见性 `Final()`,浏览 `market_zone IN (Members(z))`,给客户端的是最终区 |
| 人民币寄售 `market_zone` | 合同条款 | 不动 | 不解析。Go 的 `RmbList` 和 Java 商城都不按区过滤,这个字段只回显 |
| 玩家存档、资产、在途结算、邮件、收件箱、好友 | 按 `player_id` 存,没有区 | 不动 | 不涉及 |
| 全服榜 `go/rank`、聊天 | 没有区的概念 | 不动 | 不涉及 |
| 匹配票、队伍、申请、邀请、开战原件的区 | 路由行上的原始区(`player_route.home_zone`) | 不动 | 比较两边都 `Final()`;按区候选 `IN (Members(z))`;范围树(同区 → 大区 → 全服)按最终区;给客户端的是最终区 |
| 场景节点、gate、login、scene_manager | 物理资源 | 不碰 | 比较的是物理 zone,不解析 |
| match 里的物理 zone:玩家位置、`BattleRouting.zone_id`、节点注册路径、`gather_zone_mix_total` | 物理节点 | 不碰 | 不解析 |

**身份冲突规则**:没有冲突。`player_id`、`guild_id`、`listing_id` 全局发号;角色名、帮名本来就全局唯一(`player_name`、`uk_guild`),合服不需要改名。

### 三个完成条件

- **归属合并**:映射提交即完成,各服务 2 个刷新周期内生效(data_service、match 重读全局库 5 秒,guild、trade 重读 data_service 5 秒,共约 10 秒)。
- **数据归并**:不需要。玩家存档留在原来的库,靠 `player_route.storage_id` 找到。
- **源资源退役**:场景节点、gate 按现有排空流程随时下线,和合服解耦(§8)。源库不退役。

### 旧门禁去哪了

旧工具的门禁不是「跳过」,是它们防的那件事不存在了。

| 旧门禁 | 它防什么 | 现在 |
|---|---|---|
| P2 源区节点全下线、P6 无会话 | 改写期间还有写者改同一批行 | 不改写。写者继续写,写的都是按 `player_id` 存的数据 |
| P3、P4 Kafka 排空 | 存盘按 `home_zone` 选 topic 和库,源区一停就有存盘滞留 | 存盘直接提交,不再按区选 topic 和库(总体方案 §13) |
| P7 不在活队伍 | 队伍记录写着 zone,合服不迁移 | 读时解析,记录不改写,所以不用离队,在途的队伍和票照常走完(§2) |
| 清单(manifest)、公会重名断言 | 撤销靠清单写回;改写后 zone 内唯一 | 不改写,撤销只删映射;帮名全局唯一 |
| 维护证据、冻结证明 | 工具写业务表时业务不能并发写 | 工具只写映射表。冻结时拒绝写映射(`route_meta.writable=0`),这一条保留 |
| 映射冲突检查 | 合进已合走的区、源区合进两个区 | **保留,移进写映射的事务** |
| 等缓存、Redis 镜像 | 后续步骤要等映射生效;读者读 Redis | 没有后续步骤;映射直读 TiDB |

## 2. 在线读写和在途操作

合服不踢人、不解散队伍、不需要任何玩家重连。

| 操作 | 合服时发生什么 | 局部等待或重试 |
|---|---|---|
| 在线玩家 | 留在原节点继续玩 | 无 |
| 进行中的战斗 | battle 是全局池,不分 zone,结果按 `player_id` 走收件箱 | 无 |
| 存盘 | 按 `player_id` 直接提交,落点来自 `player_route` | 无 |
| 充值、购买、商城订单、寄售 | 按账号和 `player_id`,不看区 | 无 |
| 邮件、收件箱 | 按 `player_id` | 无 |
| 登录 | 角色列表给最终区,客户端连目标区 gate;还连着源区 gate 也能进,进源区节点 | 无 |
| 源区建角 | `home_zone` 记源区,读出来是最终区 | 无 |
| 建帮 | 记归属读到的区 | 无 |
| 入帮申请、审批、看帮会 | 两边解析到最终区再比 | 映射生效前可能回一次「不同区」,玩家重试即可,不留错误状态 |
| 公会榜 | 新分数写最终区,旧条目等折叠 | 合服后最多一个折叠周期(默认 10 秒),源区公会暂时不在最终区榜上 |
| 聚宝斋浏览、详情 | 按解析后的一组原始区查 | 映射生效前同上,刷新即可 |
| 组队 | 两个原始区的人算同区,能互相申请、邀请;已有队伍不动,队伍里的 zone 给客户端的是最终区 | match 映射生效前可能回一次「跨区」拒绝,玩家重试即可,不留错误状态 |
| 排队、匹配 | 已在队列里的票和整队不用重排,原始入队时刻不变,下一轮按最终区凑。正在备战、已开局的不受影响 | match 映射生效前两个区还按不同区凑,多等一拍。这段时间成不了跨区的局,不是错误 |

**为什么缓存滞后不影响一致性**:映射只放宽「同区」判定,不决定任何资产。滞后窗口里各服务看到的最终区可能短暂不同,最坏结果是一次可重试的拒绝;记录里存的是原始区,窗口过后都解析到同一个最终区。

### 提交点、并发、幂等

- **提交点**:`tools/merge_zone` 写映射的那个事务提交。之前什么都没变,之后所有读者最迟一个滞后窗口看到。
- **并发**:所有映射写者串行在 `route_meta(id=1)` 的行锁上,再锁源区、目标区两条映射行(zone 升序)。判定在锁内,事务外读到的东西不算数。锁等待封顶 1 秒,等不到报失败(退出码 2),重跑。
- **校验**:两区不同;目标区没有被合走;源区没有合进别的区;写入后整张表过一遍解析,不成环、链长不超过 16;`route_meta.writable` 不是 0。
- **幂等**:同一对 (源, 目标) 已存在就是成功,什么都不写,`run_id` 不变,目标区后来又被合走也一样。撤销时没有这一行也是成功。
- **崩溃恢复**:工具没有中间状态。提交前死了什么都没写,提交时没回音算结局未知(退出码 2),重跑同一条命令,判定在锁内重做,写过了就走幂等分支。

## 3. 前置条件

每次都查,任何一条不满足都不要往下走。

1. **版本**:data_service 有 `GetMergedZones`,guild、trade、match 是读映射的版本。
2. **告警**:`DataServiceMergedZoneRefreshFailing`、guild、trade 和 match 的刷新失败告警都没有在响。
3. **容量**:目标区的 gate、scene 按合并后的在线算。合服之后新登录都进目标区。
4. **目标区健康**:`pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-status -ZoneName <目标区名>`,gate、scene 全部就绪。
5. **环境变量**。DSN 含口令,不写进 shell 历史:

   ```powershell
   $env:MERGE_ZONE_ROUTE_DB_DSN = '<账号:口令@tcp(地址:端口)/全局库>'   # dev: testdb, K8s: mmorpg_global
   $env:MMORPG_SQL_OPERATOR_PRINCIPAL = '<账号@主机>'                   # 这个账号 SELECT CURRENT_USER() 的结果,真写才要
   $env:MMORPG_SQL_BACKEND = 'mysql'                                    # 只有 mysql 档要设,缺省 tidb
   ```

6. **账号权限**:操作员账号要有 `route_meta` 的 SELECT、UPDATE(`FOR UPDATE` 要它),`zone_merged_into` 的 SELECT、INSERT、DELETE,都在 `deploy/sql/authority_operator_role.sql` 的 maintenance 身份里。dry-run 和审计只要 SELECT。match 的运行账号要有全局库 `zone_merged_into` 的 SELECT,和 `player_route` 同样列进写者计划的 `tables`;没有的话 match 起不来。

## 4. 操作步骤

退出码:0 完成(含幂等);1 拒绝,什么都没写;2 没做成或结局未知,重跑同一条命令是安全的。

**第 1 步:看现状。**

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN
```

判据:退出码 0,`route_meta.writable: true`,目标区不在 `src_zone` 列。

**第 2 步:预演。**

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeSourceZone <源> -MergeTargetZone <目标> -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN -DryRun
```

判据:退出码 0,输出 `[DRY-RUN] would record zone <源> -> zone <目标>`。预演不加锁,真写时在锁内重判。

**第 3 步:合服。**

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeSourceZone <源> -MergeTargetZone <目标> -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN
```

判据:退出码 0,输出 `merged: zone <源> -> zone <目标>`。这一步之后归属合并就完成了。

**第 4 步:确认生效。** 等 2 个刷新周期(默认约 10 秒)。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN
```

判据,全部满足:

- 审计里源区的 `final_zone` 是预期的最终区,`final zone <目标> includes zones [...]` 含源区;
- 每个 data_service 副本打过 `[MergedZoneCache] mapping changed`,每个 guild 副本打过 `[guild] merged zone mapping changed`,每个 trade 副本打过 `[trade] merged zone mapping changed`,每个 match 副本打过 `[match] merged zone mapping changed`;
- `data_service_merged_zone_refresh_total`、`guild_merged_zone_refresh_total`、`trade_merged_zone_refresh_total`、`match_merged_zone_refresh_total` 的 `result="error"` 不再增长;
- 公会榜旧键已折叠:guild 打过 `[guild] folded ... rank entries`(源区本来没榜就不打),`redis-cli -n <guild 库> EXISTS guild_rank:zone:<源>` 返回 0。

**第 5 步:业务核对。**

- 源区玩家登录,角色列表的 zone 是最终区;
- 目标区玩家能看到、能申请源区公会;公会榜上有源区公会;
- 聚宝斋按区浏览,目标区玩家能看到源区商品,`market_zone` 显示最终区;
- 两区玩家能互相邀请、同队开战,队伍视图的 zone 是最终区;两区玩家单排同一个模板,不用等范围放宽就能进同一局;
- 源区在线玩家全程没有被踢,进行中的战斗正常结束。

完成。源区节点什么时候下线是另一件事,见 §8,可以隔天做。

### 连续合服

5 → 6 之后再合 6 → 7:工具允许,5 区沿链解析到 7。不能合进 6(已合走),报错会点名最终区。公会榜旧键一次折到最终区。

## 5. 故障处置

| 现象 | 判断 | 处置 |
|---|---|---|
| 工具退出码 1,`already merged into zone X` | 源区合进过别的区 | 确认要改目标就先撤销那一次;否则别动 |
| 工具退出码 1,`target zone ... merge into its final zone` | 目标区已被合走 | 按报错里的最终区重写命令 |
| 工具退出码 1,`does not resolve` | 映射表有环或超长,被人手改过 | 先审计看表,修好再合。这种表各服务也刷新不了(下一行) |
| 工具退出码 1,`route_meta.writable=0` | 目录在维护冻结中 | 等维护结束或解冻,不要绕过 |
| 工具退出码 1,`route_meta has no row` | DSN 指错库,或迁移没跑 | 核对 DSN |
| 工具退出码 2,锁等待、连接失败 | 没做成 | 查清占锁的会话或连接问题,重跑 |
| 工具退出码 2,`commit: the outcome is unknown` | 可能已提交 | 重跑同一条命令,或先审计。不要手写表 |
| 工具报 `MMORPG_SQL_OPERATOR_PRINCIPAL`,或连库报「物理会话或boot身份核验失败」 | 操作员身份不对,或 `MMORPG_SQL_BACKEND` 和实际的库不符 | 用这个账号 `SELECT CURRENT_USER()` 的结果;核对档位 |
| 映射提交了,服务迟迟不生效 | 看刷新指标和日志 | 下面两行 |
| `DataServiceMergedZoneRefreshFailing` | data_service 读不到全局库,继续用旧映射 | 查全局库连接和 `zone_merged_into` 读权限。旧映射下玩家归属还是原值,最坏是同区判定拒绝,没有错误数据。恢复后等 2 个周期。**正在合服时先别做业务核对** |
| `GuildMergedZoneRefreshFailing`、`TradeMergedZoneRefreshFailing` | 读不到 data_service,继续用旧映射 | 查 data_service。恢复后等 1 个周期 |
| `MatchMergedZoneRefreshFailing` | match 读不到全局库,继续用旧映射 | 查全局库连接和 `zone_merged_into` 读权限。旧映射下最坏是同区判定拒绝和多等一拍,没有错误数据。恢复后等 1 个周期 |
| match 起不来,报 `cannot read <全局库>.zone_merged_into` | 全局库连不上、账号没有这张表的 SELECT,或者映射表写坏 | 按错误原文处理,权限见 §3 第 6 条。这是有意的:分不清「没有合服」和「读不到」的实例不能对外服务 |
| 开战回 `kTeamCrossZoneDenied`,队里的人明明一直在一起 | 撤销合服留下的跨原始区队伍(§7) | 踢人或离队之后再开。要合就重新合服 |
| `does not resolve` 同时刷新一直失败 | 映射表写坏,缓存拒绝坏表、保留旧值 | 按审计结果用 `merge-zone-unmerge` 删掉坏的那一行(撤销不校验整张表)。修好之前不要重启服务:启动读到坏表会拒绝启动 |
| guild 或 trade 起不来,报 `GetMergedZones` | 启动时等了 60 秒还读不到映射 | 先起 data_service(zone 级,`zone-up` 才有)。这是有意的:分不清「没有合服」和「读不到」的实例不能对外服务 |
| 榜单缺源区公会超过 1 分钟 | 折叠失败,看 guild 日志 `fold ranks of merged zones failed` | 通常是抢不到榜维护锁或 Redis 故障。可滚动重启 guild,启动时按当前映射全量重建 |
| 榜上某公会分数比 `guild.score` 旧 | 滞后窗口里旧副本写了源区键,折叠时 NX 没盖 | 只影响显示。下一次推分或重启 guild 自愈,权威分在 `guild.score` |
| scene 日志 `EnterScene: home_zone changed` | 在线玩家重新路由时归属变成了最终区 | 合服后每个在线玩家最多一次,正常,不用处理 |

## 6. 撤销

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge -MergeSourceZone <源> -MergeTargetZone <目标> -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge -MergeSourceZone <源> -MergeTargetZone <目标> -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN
```

撤销之后**滚动重启一次 guild**:启动时 `RebuildRanks` 按当前映射把榜拆回去。不重启的话,已折叠的条目留在目标区榜上,直到下一次重启。

判据:退出码 0,审计里没有这一行;2 个刷新周期后源区玩家的角色列表回到源区;guild 重启后 `guild_rank:zone:<源>` 只有源区公会。

不要求逆序:4→5→6 可以先撤 5→6,4 之后解析到 5。没有这一行是成功;源区映射到别的区时什么都不删,输出会说明。

## 7. 撤销的边界

撤销只删映射,不写回任何东西。所以:

- **合服期间新建的公会、商品留在目标区**。它们记的是当时归属读到的最终区,撤销后还在目标区展示,卖家、帮主在源区也一样。不影响资产。
- **合服期间跨原始区入帮的人,撤销后还是成员**。玩家自己的帮会永远看得见(`GetPlayerGuild` 不按区过滤),经济和管理操作不查区;但帮会活动页按区过滤,会答「未入帮」。要处理只能人工调整。
- **合服期间的跨区入帮申请**:审批时锁内复核,不同区就拒绝这次审批,不会造出新的跨区成员。申请行留着(映射缓存滞后时这个判定可能是错的,不能删),由审批人拒绝或者到期清掉。
- 合服期间新建的角色按自己的 `home_zone` 回到各自的区。
- **合服期间组成的跨原始区队伍,撤销后还是队伍**。不踢人、不解散,离队、踢人、转让、解散照常。队伍和成员记的是原始区,视图自动回到各自的区。
- **已经在途的队伍和票走完这一局**。已在队列里、已成局的不重判同区,维护和恢复循环不报错,对局失败时整队原样回队列。票和开战原件记的是原始区,撤销后和路由行照样对得上。范围树上,这支队伍自身的跨度不用等,和陌生人成局仍要双方等够(`scope.UnitAllowed`、`scope.Admit`)。
- **新开战按撤销后的映射判**。禁止跨区组队(`Team.AllowCrossZone=false`)时,跨原始区的名单回 `kTeamCrossZoneDenied`,不留任何东西,玩家踢人或离队之后再开。
- **合服期间留下的入队申请、邀请**:审批、接受时按记录里的原始区重判,不同区回 `kTeamCrossZoneDenied`,不会造出新的跨区成员;它们到期自己清掉。

所以撤销只适合发现问题后尽快用。时间越长,跨区关系越多。

## 8. 源区节点和 gate 的下线

和合服解耦,可以隔天做,不做也不影响合服结果。全程不需要动映射。

1. **摘入口**:网关把源区标成维护,不再分配 gate。

   ```bash
   curl -X POST https://<gateway>/admin/zones/<源>/maintenance -H "X-Admin-Key: <admin-key>" \
     -H "Content-Type: application/json" -d '{"maintenanceMsg":"本区已合并,请从角色列表进入"}'
   ```

2. **排空 gate**(external 形态的 StatefulSet),逐台:

   ```powershell
   pwsh -File tools/scripts/k8s_gate_drain.ps1 -ZoneName <源区名> -Ordinal <N>
   ```

   脚本拒绝排空最后一台(login 在候选全部排空时会忽略标记)。第 1 步已经摘了入口,最后一台直接进第 3 步。

3. **停场景节点和剩余 gate**:

   ```bash
   kubectl get deploy,statefulset -n mmorpg-zone-<源>        # 以实际列出的名字为准,Agones 形态缩对应的 Fleet
   kubectl scale deploy -n mmorpg-zone-<源> <各 scene deploy> --replicas=0
   ```

   scene 收到退出信号先走停机排空:所有在线玩家的最终存盘提交成功才退出。玩家被踢后重连,角色列表给的是最终区,连到目标区继续玩。在途战斗在全局 battle 池里继续,结果经收件箱按 `player_id` 送达,不丢。

4. **收 namespace**:确认 scene 全部退出后,

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <源区名>
   ```

   它删的是 `mmorpg-zone-<源>` 这个 namespace,MySQL、Redis、Kafka、etcd、battle 都在 infra,不受影响。**不要**用 `k8s-all-down`。

判据:源区 namespace 没了;源区玩家能从目标区登录,背包、等级、公会、好友都在;`player_route.home_zone` 还是源区,`zone_merged_into` 还在。

**下线之后不能再撤销**,除非先把源区的节点拉回来:撤销后这些玩家的最终区回到源区,没有节点接他们。

源区留在共享 Redis 里的热状态(`scene_manager:zones` 的登记、`scene_nodes:zone:<源>:*`)不清:陈旧位置由持有节点判死兜住,目标区没有 gate 时分配 gate 会拒绝,剩下的只是垃圾键(读代码得出,未实测)。

### 源库为什么不退役

源库 `zone_<源>_db` 不退役。玩家存档留在里面,`player_route.storage_id` 指着它,目标区的 go/db 按落点按需打开(`AllowStoreFamilies` 缺省放行)。单集群里它只是一个 schema,不占额外的进程。要回收属于阶段 C 的分区搬迁,不是合服的一部分。

## 9. 支持范围和未完成项

**支持**:同一个全局库、同一个集群内的合服;多个源区合进一个目标区;连续合服(链长上限 16);任意一跳撤销。

**不支持**:跨集群或跨地域合服(映射只在一个全局库里,跨集群搬迁要在 `player_store` 上重写,总体方案 §7);拆出多集群之后映射放控制面,到时候再做。

**未完成,做完才能进真实环境**:

- Java 版:同样的读时解析没有做。

## 10. 原「当前目标」逐条对照

| 原要求 | 回应 |
|---|---|
| source 与 target 持续提供服务,已有游戏与对局能够继续 | 合服不碰节点,不踢人(§2) |
| 玩家持久数据、资产及在途充值、购买、邮件和结算保持一致 | 这些都按 `player_id` 或账号存,合服不写它们(§1、§2) |
| 区服归属合并、玩家数据库数据归并与源存储退役分别明确完成条件 | §1「三个完成条件」 |
| 局部等待、重试或重连的必要性、影响范围与恢复方式 | §2 表格最后一列、§8 |
| 核对现状,盘点读写入口、工具门禁、在途任务和恢复路径 | §1「合并对象」「旧门禁去哪了」、§2;已实现与未完成见开头状态和 §9 |
| 自主选型,写明依据、代价和适用边界 | §1 选型表、§9 |
| 合并对象、身份冲突、在线读写、提交点、并发、幂等、失败恢复、撤销边界、核验、退役条件 | §1、§2、§4、§5、§7、§8 |
| 单集群与跨集群,标明支持范围 | §9 |
| 前置条件、实际命令、完成判据、故障处置 | §3、§4、§5 |
| 不得靠删门禁、跳检查或改成功口径声称支持不停服 | §1「旧门禁去哪了」逐条说明;成功口径见 §4 第 4、5 步和 §11 |
| 交付验证要求,完成前保留「未实现／未验证」状态 | §11;开头状态行 |

## 11. 给 Codex 的验证要求

**以下都没有跑过。** 按顺序执行,前一项不过就停下来报告,保留证据:工具的完整输出和退出码、审计输出、相关服务日志、SQL 和 Redis 查询结果、`git rev-parse HEAD`。

先修依赖和生成(合服改动依赖新 RPC `GetMergedZones`):

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
```

```bash
cd tools/merge_zone && go mod tidy && go vet ./... && go test -count=1 ./...
cd go/shared && go test -count=1 ./mergedzone/ ./placement/
cd go/guild && go build ./... && go test -count=1 ./...
cd go/trade && go build ./... && go test -count=1 ./...
cd go/match && go build ./... && go vet ./... && go test -count=1 ./...
pwsh -File tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1
```

真库用例,TiDB 和 MySQL 两档各跑一遍(`MERGE_ZONE_IT_REQUIRE=1` 下 SKIP 算失败):

```powershell
cd tools/merge_zone
$env:MERGE_ZONE_IT_REQUIRE='1'; $env:MERGE_ZONE_IT_DSN='<账号:口令@tcp(127.0.0.1:端口)/>'   # 无库名无参数,数字 loopback
$env:MMORPG_SQL_BACKEND='tidb'; go test -tags merge_integration -count=1 -v ./...           # 换 mysql 实例和 'mysql' 再跑一遍
cd go/guild
$env:GUILD_TEST_MYSQL_DSN='<一次性测试库 DSN>'; go test -count=1 ./internal/data/...         # 用例会重建表,只指向测试库
$env:GUILD_IT_MYSQL_DSN='<能建库的账号 DSN>'; go test -tags integration -count=1 ./internal/data/...
cd go/trade
$env:TRADE_TEST_MYSQL_DSN='<一次性测试库 DSN>'; go test -tags integration -count=1 ./internal/data/...
cd go/match
$env:MATCH_IT_TIDB='<数字 loopback 地址:端口>'; $env:MATCH_IT_REQUIRE_TIDB='1'; go test -count=1 ./internal/team/... ./internal/authority/...   # 只有 TiDB 档
```

场景用本机两区:`pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start-zones -Zones 1,2`,源区 1、目标区 2。每个场景前核对 `zone_merged_into` 是空表。

| 场景 | 步骤 | 通过标准 |
|---|---|---|
| **正常在线合服** | 两区各起 robot(`-Command dev-robot-zones -Zones 1,2`,持续在线)。两区各建公会、推榜分、上架商品。按 §4 跑完第 1~5 步 | 工具退出码 0。合服前后 robot 没有断线、登录和存盘失败数不增长。10 秒内 §4 第 4 步的判据全满足。第 5 步人工项全过。`SELECT COUNT(*) ... GROUP BY zone_id` 在 `guild`、`trade_listing`、`player_route.home_zone` 上合服前后不变 |
| **并发写入** | 合服前开始持续写:两区同时建帮、入帮申请和审批、推榜分、上架、存盘(robot 加循环)。写入途中执行合服,窗口内和窗口后各再跑 2 分钟。另跑 `TestIT_ConcurrentMergesSerializeOnRouteMeta` | 没有新的错误类型;窗口内只允许可重试的「不同区」拒绝,窗口后归零。结束时逐公会比对:`guild.score` 等于 `ZSCORE guild_rank:zone:2 <id>`(不等的只能是窗口内旧副本写的,记录 id 和原因)。没有公会同时缺榜,`guild_rank:zone:1` 不存在。并发用例里只有一个 worker 成功、表里只有一行 |
| **在途结算** | robot 跑 `battle-smoke` 对局,对局中途执行合服;再开一局,对局中途按 §8 停源区 scene。商城、寄售各留一笔未完成的订单跨过合服 | 两局结果都到账(`player_store` 资产和收件箱都有,且只有一次);第二局玩家被踢后重连到目标区,结算不丢。订单状态正常推进。`player_inbox` 没有积压告警 |
| **重复执行** | 同一条合服连跑 3 次;合进去之后换目标区跑;撤销连跑 2 次;撤销后再合 | 3 次都是退出码 0,后两次 `nothing written`,`run_id` 不变。换目标区退出码 1,表不变。撤销第 2 次退出码 0,`nothing to delete`。再合成功,`run_id` 是新的 |
| **进程崩溃恢复** | ① 另开会话 `BEGIN; SELECT writable FROM route_meta WHERE id=1 FOR UPDATE;` 占锁,跑合服,确认退出码 2,释放锁后重跑。② 同样占锁,`kill -9` 正在等锁的工具,重跑。③ 合服后 `kill -9` 全部 data_service、guild、trade,再起。④ 造一个上万成员的 `guild_rank:zone:1`,折叠中途 `kill -9` guild。⑤ 停 data_service,起 guild 和 trade。⑥ 运行中停 data_service 1 分钟 | ①② 表里没有半截状态,重跑成功。③ 启动日志读到映射,解析正确,`RebuildRanks` 按最终区。④ 重启后榜完整,旧键不存在,分数等于 `guild.score`。⑤ 60 秒内 data_service 起来就正常启动;超过 60 秒退出码非 0,错误点名 `GetMergedZones`,没有注册进 etcd。⑥ guild、trade 继续服务,刷新失败指标增长,恢复后停止增长 |
| **队伍与匹配跨过合服** | 合服前:两区各一支整队进队列(目标人数大于队伍人数),两区各几张同模板单排票。执行合服。再让两区玩家组一支跨原始区的队伍开战进队列。等它们成局或继续排队时撤销合服,再等 2 个周期 | 合服前两区的票不互相成局(范围未放宽时);合服后不用重排就能成局,`SELECT zone_id FROM match_authority_ticket` 还是原始区。撤销前后 `match.team.sql.queued`、`prepared`、`forming` 循环没有新错误,`relay` 没有 HOLD,关停能排空。跨原始区的队伍撤销后没有被解散,这一局走完;它再次开战回 `kTeamCrossZoneDenied`。撤销后新的跨区申请、邀请回同一个码 |
| **切换后一致性** | 合服完成后,抽样核对:源区玩家的存档、背包、公会、好友、邮件;`go/rank` 榜;聚宝斋详情和收藏;回档检查 `ListAppliedAssetOpsSince` 带 `zone_id=1`、`zone_id=2` 各查一次。再做连续合服 2→3 和撤销,重复一遍 | 数据和合服前逐项一致。回档检查两个 zone 返回同一组行。连续合服后 1、2 都解析到 3。撤销 + 重启 guild 后榜和归属回到撤销后的映射,合服期间建的公会、商品留在目标区,§7 的边界如实出现,没有别的变化 |

**不要**在报告里写「性能提升」或「线上可用」,这些场景只证明逻辑。压测口径按 AGENTS.md §6。
