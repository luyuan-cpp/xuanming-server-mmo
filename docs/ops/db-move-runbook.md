# 不停服搬逻辑库操作手册

> **负责人要求(2026-10-31)**:「我确实会有不停服迁移物理库的」。一个逻辑库(比如 `player_store_3_db`、`mmorpg_guild`)要能从物理集群 A 搬到集群 B,玩家不掉线、不丢数据,只有秒级暂停,业务收到可重试错误。实现口径:窗口里这个库的写者池**读写都暂停**,都回可重试错误(设计 §2)。
> **状态**:工具、连接层、手册已按[设计 §8](../design/horizontal-scale-logical-db.md)落码,**未编译、未测试、未演练**。§10 的验证跑通之前,不能在真实环境执行。部署渲染已由 K1 接上,同样未编译:`k8s_deploy.ps1` 按 `-SqlClusters`、`-SqlDefaultCluster` 渲染服务 yaml 的 `DbPlacement` 段,写者的 registrar init 自带 `MMORPG_DB_PLACEMENT`。
> **2026-11-12 起的三处变化**(同样未编译):`cleanup` 删源库之前核复制流已经撤掉,要带 `-sync`(第 9 步);`cutover` 的冻结窗口里两边静止之后再比一次全量,窗口因此变长(第 5 步);校验和同时比表的定义,没有主键的表不能搬(§2 第 6 条)。

## 1. 方案

逻辑库到物理集群的映射放在 etcd(`/mmorpg/db_placement/<逻辑库>`,值 `{cluster, version, state}`),进程按映射拨号。切换时:

```
核复制流、账号、进程 → 冻结源库闸门 → 写哨兵行 → 等复制追上,目标读到哨兵 → 两边静止,全量再比一次 → 记下目标位置 → 查目标连接 → 解锁写者账号 → 打开目标闸门 → etcd CAS 翻转映射(提交点) → 写翻转标记
```

- **不丢数据**:闸门的冻结要拿到全部写者的 slot 行锁才提交,所以冻结提交时在途事务都结束了,之后的写事务在闸门上被拒。复制追上冻结点之后,目标和源库内容应当相同;工具不靠推断,在打开目标之前把两边全量再比一次,不一致就不翻转。
- **不掉线**:`*sql.DB` 句柄不变,翻转后旧连接被连接池丢弃,新连接拨新集群。冻结窗口里写者池上的读和写都回 `ErrWriterFrozen`,RPC 回 `Unavailable`,存盘按原序号退避重试,都不丢。只读连接池不过闸门,照常读,读到的是冻结时的数据。
- **不分叉**:翻转之后源库保持冻结,Watch 落后的进程写不进去,最坏是多重试几次。翻转之前目标上的写者账号是锁着的(`prepare` 锁,`cutover` 解):闸门表是复制过去的,目标上 `writable=1`,不锁的话提前连过来的写者能直接过闸。所以**任何进程都不提前写新地址**。
- **复制不留缺口**:两道。`verify` 记下复制流的身份和它的配置摘要,`cutover` 比对,流重建过、过滤改过就拒绝,要重新校验(§3)。身份和配置都没变、复制却被跳过一段的情况(§3 列了几种),位置和哨兵都看不出来,靠冻结窗口里那次全量比对拦住。
- **源库不被误删到目标上**:`DROP DATABASE` 也是一条会被复制的语句。`cleanup` 删源库之前核喂过目标的那条复制流已经撤掉,只是暂停、停住都不行,查不到也不删(第 9 步)。

### 什么时候能用

- **批 F1、F2、F3、D1 落地并验证过**。在那之前跨库 SQL 还在(设计 §11),库不能真的分开放。
- 一次只搬一个逻辑库。工具看到别的库在搬会拒绝。
- 不和发布同时做。`state=MOVING` 期间 `-migrate` 拒绝。窗口内写者注册回可重试错误,不用掉名册 entry,init 重启后接着注册;已经渲染好的名册不受 cutover、回滚影响(REGISTER 不绑 epoch,设计 §2)。

## 2. 前置条件

每次都查,任何一条不满足都不要往下走。

1. **所有用这个库的进程都配了 `DbPlacement`**,新旧两个集群都在 `SqlClusters` 里。K8s 上部署时给 `-SqlClusters "a=tidb-a:4000,b=tidb-b:4000" -SqlDefaultCluster a`,默认集群的地址必须等于 `-GlobalAuthorityTiDBHost`;这两个参数不粘滞,以后每次部署都要照传。渲染出来的形状:

   ```yaml
   # guild、trade、friend、player_inbox 写在 MySQL: 下面;match 在 GlobalMatch: 下面;data_service 在 SnapshotMySQL: 下面;chat、mail、rank、go/db 在顶层
   DbPlacement:
     DefaultCluster: a            # 没有映射键的库在这个集群
     EtcdHosts: ["etcd-0:2379"]
     SqlClusters:
       a: {Hosts: "tidb-a:4000"}
       b: {Hosts: "tidb-b:4000"}
   ```

   核对:`curl <进程>:<metrics 端口>/metrics | grep db_placement_`,要有两个集群的 `db_placement_known_cluster`,以及这个库的 `db_placement_version`(配了映射就上报 etcd 里有键的库;这个库还没有键时,进程连过才报 0)。配置没带 `DbPlacement` 的进程不会跟着翻转。Pod 实际用的是 `runtimeConfigSecret`,只改 ConfigMap 不算数;`cutover` 按 `-metrics-targets` 再核一次,进程不认识目标集群就拒绝。

   `DefaultCluster` 第一次部署时写进 etcd 的 `/mmorpg/db_placement_default`,之后配置不一致拒启:没有映射键的库都在这个集群上,不能靠改配置换。

2. **控制工具也知道映射**:`writer_authority`、`maintenance_phase`、`debug_fetch`、`admission_recover`、`create_residual` 的环境变量 `MMORPG_DB_PLACEMENT` 设成同形状的 JSON。`admission_recover`、`create_residual` 没设时拒绝运行。K8s 上写者的 registrar init 已经带这个变量(login、gateway 除外,它们写区域 MySQL 上的 `mmorpg`,见 §9),其余工具手工设。没设时它们连连接串里的地址,库搬走之后就连错了。`merge_zone` 不读映射,库搬走之后 `MERGE_ZONE_ROUTE_DB_DSN` 要改成新地址;连错时写操作被冻结的闸门拒绝,但 `merge-zone-audit` 读到的是冻结时的数据。

   **搬库期间(`prepare` 到 `cutover` 或 `rollback` 结束)禁用 `admission_recover` 和 `create_residual -mode apply`。** 它们是人工修账的工具,不是注册过的写者,不走写者池的闸门,只靠自己在同事务里锁 `route_meta` 判 `writable`(`create_residual` 修名字时持着两个库的闸门行锁再写):冻结时拒绝。这条判定只保证不写冻结的库,不保证修账和切换的先后符合预期,所以这段时间不要用。

   **搬的是目录库时,结束之后跑一次名字收尾**(2026-11-12)。冻结期间建角回滚的释放可能失败:释放之前要在玩家片证明角色永远不会提交,玩家片冻结时这一步写不了墓碑,名字留成孤儿(login 记 `[player-name] orphan reservation` 和 `login_create_player_name_orphan_total`)。`cutover` 或 `rollback` 结束、闸门打开之后跑 `create_residual -target names`(不带 `-shard`:名字行按 `name_norm` 分片,孤儿名字的名字行不在冻结的那一片上;plan,再 `-mode apply -repair name`,参数见 [PITR 手册](mysql-backup-pitr-runbook.md) §4.1a 第 2、3 步),它报 `orphan_name` 并按同带 token 的 `ReleasePlayerName` 收掉,有提交证据的只报告。不用再按日志逐条带 token 释放。

3. **账号在目标集群提前建好,口令相同**:写者账号(`writer_registry` 里 ACTIVE 的 `sql_principal`,加上名册里的全部主体:滚动重启时新 Pod 用名册里下一个)、注册账号、schema 账号和维护账号。`plan` 逐个核账号存在,`cutover` 冻结之前再核一遍,再比一遍两侧的 `SHOW GRANTS`。连接串不换,换的只有地址。BR 和 `mysqldump` 都不带 `mysql.*`,账号和授权要自己建;授权是按表的,表要先存在,所以放在全量恢复之后(§4 第 3 步)。

4. **工具用的账号**:

   | 环境变量 | 身份 | 需要的权限 |
   |---|---|---|
   | `DBMOVE_SOURCE_DSN`、`DBMOVE_TARGET_DSN` | 维护身份(`deploy/sql/authority_operator_role.sql`) | 模板已给:闸门表、`db_move_sentinel` |
   | `DBMOVE_SOURCE_ADMIN_DSN` | 管理员。`plan`、`prepare` 起就要(查没有主键的表) | 这个库全部表的 SELECT、DROP(表都看得到,`information_schema` 才列得全);`mysql` 库的 SELECT(`SHOW GRANTS FOR` 别的账号要它);TiDB 上 `ADMIN CHECKSUM TABLE` 要 SUPER(演练时确认) |
   | `DBMOVE_TARGET_ADMIN_DSN` | 管理员。mysql 档的 `cleanup` 也要(查复制通道撤掉了没有) | 这个库的 CREATE 和全部表的 SELECT;`mysql` 库的 SELECT(查账号、`SHOW GRANTS FOR`);`CREATE USER`(`ALTER USER ... ACCOUNT LOCK` 要它);TiDB 再要 SUPER、`PROCESS`(`cluster_processlist` 要看到全部账号的连接)和 `tidb_cdc` 的 SELECT;MySQL 再要 `performance_schema` 的 SELECT |
   | `DBMOVE_VERIFY_SOURCE_DSN` | 只读,**mysql 档必填** | 源库副本上这个库的 SELECT(§4 第 4 步) |
   | `DBMOVE_AUDIT_TARGET_DSN` | 只读,**mysql 档必填** | 目标库副本上这个库的 SELECT(§8) |

5. **环境变量**。DSN 含口令,工具只从环境读,任何输出都不回显:

   ```powershell
   $env:MMORPG_DB_PLACEMENT = '{"DefaultCluster":"a","EtcdHosts":["etcd-0:2379"],"SqlClusters":{"a":{"Hosts":"tidb-a:4000"},"b":{"Hosts":"tidb-b:4000"}}}'
   $env:MMORPG_SQL_BACKEND = 'tidb'                                 # mysql 档写 mysql
   $env:MMORPG_SQL_OPERATOR_PRINCIPAL = '<维护账号@主机>'             # 这个账号 SELECT CURRENT_USER() 的结果
   $env:MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256 = '<64 位十六进制>'      # 按部署一份,搬库不换
   $env:DBMOVE_SOURCE_DSN = '<账号:口令@tcp(tidb-a:4000)/>'
   $env:DBMOVE_TARGET_DSN = '<账号:口令@tcp(tidb-b:4000)/>'
   $env:DBMOVE_SOURCE_ADMIN_DSN = '<账号:口令@tcp(tidb-a:4000)/>'
   $env:DBMOVE_TARGET_ADMIN_DSN = '<账号:口令@tcp(tidb-b:4000)/>'
   $env:DBMOVE_REGISTRAR_PRINCIPAL = '<注册账号@主机>'                # cutover 必填
   $env:MMORPG_SQL_SCHEMA_PRINCIPAL = '<schema 账号@主机>'            # cutover 必填
   ```

   连接串的地址要和 `SqlClusters` 一致,admin 连接串也一样,`plan`、`prepare`、`verify`、`cutover`、`rollback`、`audit`、`cleanup` 都核:工具核对过的库,必须就是翻转之后各进程连的那个。经隧道连同一个集群时加 `-allow-addr-mismatch`。

6. **每张表都有主键,两个集群的表定义相同。**
   - 没有主键的表不能搬,`plan`、`prepare` 点名拒绝,`verify` 再拦一次(`has no primary key`)。原因:TiDB 档的逐行摘要按 `BIT_XOR` 合并,完全相同的两行互相抵消,这种表数据不同也比不出来;TiCDC 本来也不同步既没有主键、又没有非空唯一键的表。只有非空唯一键、没有主键同样拒绝,先补主键(DDL 在 `prepare` 之前做)。
   - 校验和除了数据还比表的定义:每列的类型、排序规则、可空性、缺省值、生成方式,主键和每个索引的列序、前缀长度、唯一性。数据一样、定义不一样(目标少一个唯一约束、列的排序规则不同)同样判不一致。两边是同一种后端,所以 `information_schema` 的内容逐字比,不做归一化:**两个集群用同一个版本**,否则同一份定义展示不同(比如整数的显示宽度)也会判不一致。表选项(`AUTO_INCREMENT`、TTL 属性等)、注释、`CHECK`、外键、分区不比。

入口,在仓库根的 pwsh 会话里(环境变量也设在这个会话):

```powershell
& tools/scripts/dev_tools.ps1 -Command db-move -DbMoveArgs @('plan','-db','mmorpg_guild','-target-cluster','b'); "exit=$LASTEXITCODE"
```

从别的 shell 用 `pwsh -Command` 调时,命令末尾要加 `; exit $LASTEXITCODE`:不加的话 pwsh 把 2、3 都压成 1,而这几个退出码的处理完全不同(§4)。

下面的命令都省略这层包装,只写 `dbmove <子命令> ...`。本地日志在 `run/dbmove/dbmove-<逻辑库>-<run-id>.log`,每步一行 JSON,不含连接串。

## 3. 复制怎么起

工具不起复制、不停复制,只读进度。**`prepare` 之后、`verify` 之前**由运维起。

| | TiDB 档 | MySQL 档 |
|---|---|---|
| 全量 | BR | `mysqldump`(或 `mysqlsh` dump) |
| 增量 | TiCDC,**开 sync point** | GTID 复制,带过滤的专用通道 |
| `-sync` | `ticdc:<OpenAPI 地址>:<changefeed>` | `mysql-gtid`(通道名缺省 `dbmove`) |
| 校验位置 | TiCDC syncpoint 的一对 TSO | 两个副本停在同一 GTID |

### TiDB 档

```bash
# 两个集群都调大 GC:BR 到 changefeed 之间的增量、verify 读的快照、audit 读的目标位置都要留着。§4 第 8 步改回。
mysql -h tidb-a -e "SET GLOBAL tidb_gc_life_time='72h';"
mysql -h tidb-b -e "SET GLOBAL tidb_gc_life_time='72h';"

tiup br backup db --db mmorpg_guild --pd pd-a:2379 --storage "s3://<桶>/dbmove/mmorpg_guild"
BACKUP_TS=$(tiup br validate decode --field="end-version" --storage "s3://<桶>/dbmove/mmorpg_guild" | tail -n1)
tiup br restore db --db mmorpg_guild --pd pd-b:2379 --storage "s3://<桶>/dbmove/mmorpg_guild"

cat > changefeed.toml <<'EOF'
enable-sync-point = true
sync-point-interval = "30s"
sync-point-retention = "24h"
[filter]
rules = ['mmorpg_guild.*']
EOF
tiup cdc cli changefeed create --server=http://cdc-a:8300 --changefeed-id="dbmove-mmorpg-guild" \
  --sink-uri="mysql://<账号>:<口令>@tidb-b:4000/" --config changefeed.toml --start-ts="$BACKUP_TS"
```

changefeed 的账号要有 `SYSTEM_VARIABLES_ADMIN` 或 `SUPER`(sync point 要写 `tidb_external_ts`)。changefeed id 只能是字母数字和连字符。TiCDC 只同步有主键或非空唯一键的表;工具要求更严,没有主键的表在 `plan` 就拒绝(§2 第 6 条)。

changefeed 建好之后**不要再改它的配置**。`verify` 把 TiCDC 报告的整份 `config` 取摘要记下,`cutover` 比:过滤规则、`ignore-txn-start-ts`、事件过滤改过之后有一段写不再复制,而 changefeed 的 id、`start_ts`、`create_time` 都不变。不挑字段,任何一项变了(包括和内容无关的)都要重跑 `verify`;TiCDC 升级后缺省项变了也算。

### MySQL 档

源、目标都要 `gtid_mode=ON`、`enforce_gtid_consistency=ON`、`binlog_format=ROW`、`binlog_row_image=FULL`。目标主库 `read_only=0`:切换时要写闸门表,翻转后要收写入。目标主库 `log_replica_updates=ON`(8.0 默认),它的副本才收得到这个库。目标主库上可以有别的库,所以用带过滤的专用通道。

```bash
# 全量,从源库的副本取。8.0 的 mysqldump 写出来的是追加形式(SET GTID_PURGED='+...'),目标上已有的 GTID 不受影响。
mysqldump -h <源库副本> --single-transaction --set-gtid-purged=ON --databases mmorpg_guild > mmorpg_guild.sql
mysql -h mysql-b < mmorpg_guild.sql
```

dump 里有 `SET @@SESSION.SQL_LOG_BIN=0`,导入不进 binlog。目标主库有副本时(§8 的审计副本就是),同一份 dump 要在起通道之前也导进每个副本。

**目标已经有这批 GTID 时**(同一对集群搬第二个库、从全量重来、反向搬),`SET GTID_PURGED='+...'` 和目标的 `gtid_executed` 重叠,MySQL 报 ERROR 3546,导入在数据之前就中止。这时按下面做,不要删掉这条语句了事:

```bash
mysqldump -h <源库副本> --single-transaction --set-gtid-purged=COMMENTED --databases mmorpg_guild > mmorpg_guild.sql
```

```sql
-- 目标主库。<dump 的集合> 取 dump 文件头部注释里的 GTID_PURGED。
-- 第一条必须不含源库的 UUID:否则目标比这份 dump 新,那段写既不在 dump 里,自动定位也不会再传。重新取一份更新的 dump。
SELECT GTID_SUBTRACT(@@GLOBAL.gtid_executed, '<dump 的集合>');
-- 第二条是要补记的部分。导入 dump 之后执行,结果为空就跳过。
SELECT GTID_SUBTRACT('<dump 的集合>', @@GLOBAL.gtid_executed);
SET @@GLOBAL.GTID_PURGED='+<第二条的结果>';
```

这条流程没有在真实 MySQL 上走过(§10)。走错了也不会带缺口切换:`verify` 的校验和会报差异。

```sql
-- 目标主库
CHANGE REPLICATION SOURCE TO SOURCE_HOST='mysql-a', SOURCE_PORT=3306, SOURCE_USER='<复制账号>', SOURCE_PASSWORD='<口令>', SOURCE_AUTO_POSITION=1 FOR CHANNEL 'dbmove';
CHANGE REPLICATION FILTER REPLICATE_DO_DB=(mmorpg_guild) FOR CHANNEL 'dbmove';
START REPLICA FOR CHANNEL 'dbmove';
```

被过滤掉的事务也记 GTID,所以通道的 GTID 集合和源库一致,`WAIT_FOR_EXECUTED_GTID_SET` 能用。

通道必须是 `SOURCE_AUTO_POSITION=1`,工具会核:按文件位置复制能跳过事务,GTID 集合对不上也看不出来。

目标主库必须是 `replica_skip_errors=OFF`、`replica_exec_mode=STRICT`(都是缺省值),`verify` 和 `cutover` 每次读复制流都核,不是就拒绝:这两个开关让应用出错的事件被直接跳过,目标少了写入,GTID 集合和延迟却都对得上。通道的复制过滤(`performance_schema.replication_applier_filters`)也取摘要记下,`verify` 之后改过滤要重跑 `verify`。

### 复制断过之后

**复制重建之后全量数据必须重新校验。** changefeed 断过之后用较新的 `--start-ts` 重建,中间一段写没复制过去,但延迟回到 0,光看延迟全绿,目标缺数据就开放写入了。

- 工具靠复制流的身份发现这件事。`verify` 记下身份:TiDB 档是 changefeed id、`start_ts`、`create_time`,MySQL 档是通道、`source_uuid`、来源地址,两档都加上目标库 `route_meta` 的建表时间(删库重建之后它会变)和配置摘要(TiDB 档是 changefeed 的整份 `config`,MySQL 档是通道的复制过滤)。`cutover` 在预检和冻结之后各比一次,预检时变了就拒绝,要求重跑 `verify`;冻结之后变了,或者窗口里看到复制断开,工具回滚时清掉 `verify` 记录。
- 重建 changefeed 时 `--start-ts` 要不晚于断开时的 checkpoint(`tiup cdc cli changefeed query` 看),否则 `verify` 报差异。已经过了 GC 安全点就只能从全量重来。
- **身份和配置都不变的跳过,禁止做**:TiCDC 的 `resume --overwrite-checkpoint-ts`;MySQL 上手工 `SET GTID_PURGED`、往目标注入空事务跳过某个 GTID。这些操作之后延迟、位置、哨兵全都对得上,目标却少了一段写。`cutover` 冻结窗口里的全量比对会把它拦下来(回滚,清掉 `verify` 记录,见 §6);用 `-skip-final-compare` 跳过了那次比对的话,只剩翻转之后的 `audit` 能发现,那时数据已经分叉。复制报错要跳过时,从全量重来。
- 从全量重来:停复制,目标上 `DROP DATABASE`,重跑 `prepare`(同一个 run id,幂等,账号保持锁定),按本节重新起复制,再 `verify`。

## 4. 操作步骤

退出码:0 完成(含幂等);1 拒绝或核对不一致,这一步什么都没改;2 没做成,状态是干净的,重跑同一条命令是安全的;3 结局未知,看 §6。

**第 1 步:plan。** 只读,什么都不写。

```
dbmove plan -db mmorpg_guild -target-cluster b -roster <名册文件,逗号分隔>
```

`-roster`:写这个库的每个服务的 boot 名册文件。`plan` 可以不给,`prepare`、`cutover` 必填:ACTIVE 主体只是现在在跑的,滚动重启之后新 Pod 用名册里下一个,它在目标上没有的话,翻转之后连接全失败,还回不去。

判据:退出码 0,输出 `[plan] ok: mmorpg_guild can move from cluster a to cluster b`,括号里写明 `prepare` 会锁几个账号、几个因为还在写别的库不锁;最后一行给一个 run id。后面每一步都带这个 `-run-id`。

报 `have no primary key`:输出里点名的表没有主键,不能搬(§2 第 6 条)。先给它们加主键,再重跑 `plan`。`plan`、`prepare` 用 `DBMOVE_SOURCE_ADMIN_DSN` 查这件事,所以这两步也要设它。

报 `also have grants on other databases`:`ACCOUNT LOCK` 按账号生效,不按库。账号在目标集群还有别的业务库的授权时(go/db、data_service 一个 boot 写全部存档库),锁了就把那些库一起断了。工具默认拒绝。确认之后 `plan`、`prepare` 都加 `-allow-shared-writer-accounts`,这些账号不锁,记进映射的 `shared_accounts`。**残余风险**:这些账号翻转之前能连到目标、能过闸,只靠两点挡:Go 进程按映射拨号,翻转之前不会连过来;窗口里打开闸门之前查连接,只看连到这个库的。Java 进程不按映射拨号,用这个开关时 §9 更要守住。

**第 2 步:prepare。**

```
dbmove prepare -db mmorpg_guild -target-cluster b -run-id <run-id> -roster <名册文件,逗号分隔>
```

判据:退出码 0,`etcdctl get /mmorpg/db_placement/mmorpg_guild` 里 `state` 是 `MOVING`,`locked_accounts` 列出锁定的账号,目标上有空库,`SELECT User,Host,account_locked FROM mysql.user` 这些账号是 `Y`。此后 `-migrate` 拒绝这个库。

账号的锁不随复制走,TiDB 和 MySQL 都一样;由 `cutover` 解。不要手工解锁。之后名册换了或者多了写者,同一个 run id 重跑 `prepare`,幂等。

**第 3 步:起复制**,见 §3。等延迟稳定在几秒之内。

全量恢复之后在目标上补授权:和源库一样执行一遍写者计划的授权和 `deploy/sql/authority_*_role.sql`,账号保持锁定。`cutover` 冻结之前逐账号比两侧的 `SHOW GRANTS`(这个库和全局的授权),缺一个就拒绝:写者账号的授权缺了,翻转之后这个库写不进去,回不去。

**第 4 步:verify。** 复制运行中比校验和。大表要很久,放在低峰。

- TiDB 档:工具读 `tidb_cdc.syncpoint_v1` 最近一行,源库站在 `primary_ts`、目标站在 `secondary_ts`。changefeed 要开着 sync point。`primary_ts` 早于 changefeed 的 `start_ts` 就拒绝(那一行是同名的旧 changefeed 留下的,等一个间隔重来);物理时间比源库当前 TSO 旧超过 4 个 sync point 间隔也拒绝(复制落后或者 sync point 停了)。

  ```
  dbmove verify -db mmorpg_guild -run-id <run-id> -sync ticdc:cdc-a:8300:dbmove-mmorpg-guild
  ```

  比的是表的定义(§2 第 6 条),加上 `ADMIN CHECKSUM TABLE` 的 `Total_kvs`、行数、逐行摘要。**不比它的 `Checksum_crc64_xor`**:它把 `table_id` 算进去,两个集群上同样的数据校验和不同。逐行摘要是每列各取一次 `MD5`(`NULL` 用一个不是十六进制的字符顶替)、拼起来再取 `MD5`,所有行按 `BIT_XOR` 合并:每列占的长度固定,值里有逗号、是空串还是 `NULL` 都分得清。

- MySQL 档:`CHECKSUM TABLE` 要读锁,不能在主库上跑。在**源库的一个副本**上跑,目标主库上这个库没有业务写入,直接跑。两边要停在同一个位置:

  ```sql
  -- 源库副本
  STOP REPLICA;
  SELECT @@global.gtid_executed;   -- 记作 <G>
  -- 目标主库:追到 <G> 停住
  STOP REPLICA FOR CHANNEL 'dbmove';
  START REPLICA UNTIL SQL_AFTER_GTIDS='<G>' FOR CHANNEL 'dbmove';
  SELECT WAIT_FOR_EXECUTED_GTID_SET('<G>', 600);
  ```

  ```
  dbmove verify -db mmorpg_guild -run-id <run-id> -sync mysql-gtid    # DBMOVE_VERIFY_SOURCE_DSN 指向那个副本
  ```

  工具前后各核一次两边在同一位置。报 `the target is ahead` 就是目标已经过了 `<G>`:两边 `START REPLICA`,过一会儿重来。做完恢复:源库副本 `START REPLICA`,目标主库 `START REPLICA FOR CHANNEL 'dbmove'`。

判据:退出码 0,输出 `[verify] ok: every table matches`,后面带着这次比对花了多久和复制流的身份;映射的 `verified_feed` 记下了身份和配置摘要(`config_digest`),`verify_took_ms` 记下耗时。退出码 1 且报表不一致:**不要切**,先查复制(§8 的处理同样适用);报的是 `the table definition differs` 时是两边表定义不同,方括号里列出只在一边出现的列或索引。比较期间复制流或它的配置变了也拒绝,重跑。

记下输出里的比对耗时:`cutover` 的冻结窗口里还要再比一次,耗时和它相当(第 5 步)。

**第 5 步:cutover。** 选低峰,不和发布同时。`verify` 超过 `-verify-max-age`(默认 6 小时)会被拒绝,重跑一次。时间从比较的位置算,不从工具开始的时刻算:TiDB 档是 `primary_ts` 的物理时间,MySQL 档是 verify 开始时。

```
dbmove cutover -db mmorpg_guild -run-id <run-id> -sync ticdc:cdc-a:8300:dbmove-mmorpg-guild \
  -roster <名册文件,逗号分隔> -metrics-targets guild-0:<metrics 端口>,guild-1:<metrics 端口>
```

可调:`-max-lag`(默认 3 秒,冻结之前的延迟上限)、`-budget`(默认 30 秒,冻结窗口里比对以外的时间上限:冻结、等复制追平、解锁、开闸门、翻转)、`-compare-budget`(默认 30 秒,窗口里那次全量比对的上限,另算,不占 `-budget`)。

**冻结窗口里要再比一次全量,窗口因此变长。** 源库冻结、复制追平之后,目标的闸门还没开,两边都不动:工具在这时把源库和目标再比一遍(和 `verify` 同一套比法,读当前数据,不用 syncpoint;MySQL 档这一次直接比两侧主库,源库冻结着,`CHECKSUM TABLE` 的读锁挡不到写入)。一致才记目标位置、解锁账号、打开目标;不一致就按原来的顺序回滚(先作废翻转、再解冻),清掉 `verify` 记录。它拦的是 `verify` 之后复制被跳过一段的情况(§3)。

- 这段时间这个库的读写一直暂停,时长约等于 `verify` 那次比对的耗时。小库是秒级;大库按 `verify` 输出的耗时估,**把 `-compare-budget` 设到比它大**:愿意让这个库停多久,由运维用这个参数明说。`cutover` 在冻结之前先比这两个数,装不下就退出码 1(`cannot hold`),不去白冻结一次。它和 `-budget` 分开算:库再大,等复制追平也只等 `-budget`,不会因为比对要很久就把源库冻着干等的时间也放宽。
- 库大到这样的暂停不能接受时,加 `-skip-final-compare` 跳过这次比对,窗口回到秒级。代价是 `verify` 之后复制有没有漏写,翻转之前没有人查过。跳过时工具在标准错误输出打一行 `WARNING: [cutover] -skip-final-compare is set`,写进本地日志,并把 `final_compare_skipped: true` 和翻转一起记进映射。**跳过之后翻转一完成就跑 `audit`**(第 7 步),它是唯一的核对;`audit` 通过之前不要 `cleanup`,冻结的源库是唯一的退路。
- 带 TTL 的表(第 8 步后面的 `mmorpg_chat`):源集群的 TTL 任务不过闸门,冻结之后它照样删行。正好在窗口里删,这次比对会判不一致并回滚;没有数据损失,但 `verify` 记录被清掉,要重跑。

冻结之前核完这些,一条不过就退出码 1,什么都没变:

- 复制流还是 `verify` 时那一条,配置摘要没变,延迟低于 `-max-lag`;MySQL 档目标上没有开跳过错误的开关(§3);
- `-compare-budget` 装得下窗口里那次全量比对(没加 `-skip-final-compare` 时);
- 维护账号能控制两侧闸门;
- 全部账号(ACTIVE、名册、`prepare` 记下的、注册、schema、维护)在目标上存在,写者账号都经过了 `prepare`,两侧 `SHOW GRANTS` 没有缺的;
- `-metrics-targets` 列出的每个进程都上报目标集群的 `db_placement_known_cluster`。**把用这个库的进程都列上**,Java 进程没有这个指标,不要列(§9)。

预期输出:

```
[cutover] source frozen: writer_epoch=...
[cutover] replication caught up, the target has the sentinel
[cutover] <表名>: N rows match              (每张表一行)
[cutover] the frozen source and the target match, compared in ...
[cutover] target point for audit: ...
[cutover] ok: mmorpg_guild flipped to cluster b, version N. The window took ...
```

判据:退出码 0;`etcdctl get` 看到 `cluster` 是目标、`version` 加 1、`state` 是 `ACTIVE`,`move.feed_endpoint` 是这次 `-sync` 的 TiCDC 地址(MySQL 档没有这一项);目标上写者账号的 `account_locked` 是 `N`;各进程日志打 `[dbplacement] mmorpg_guild: cluster=b version=N`;业务错误率回到切换之前。其它结局看 §6。

**第 6 步:撤掉复制。** 源库已冻结,复制里没有新东西了。**必须撤掉,不能只是暂停或停住**:

- TiDB:`tiup cdc cli changefeed remove --server=http://cdc-a:8300 --changefeed-id=dbmove-mmorpg-guild`
- MySQL:目标主库 `STOP REPLICA FOR CHANNEL 'dbmove'; RESET REPLICA ALL FOR CHANNEL 'dbmove';`

留着它的后果在第 9 步:源库的 `DROP DATABASE` 会顺着这条复制到达目标,把正在用的库删掉。changefeed 只 `pause`、通道只 `STOP REPLICA` 时,它一恢复(TiCDC 的 `resume`,或者目标 mysqld 重启后通道自动起来)就把这条 DROP 送过去。`cleanup` 会核这一步做了没有,没做就拒绝。

**第 7 步:audit。** 见 §8。

```
dbmove audit -db mmorpg_guild -run-id <run-id> -metrics-targets guild-0:<metrics 端口>,guild-1:<metrics 端口>
```

判据:退出码 0,输出 `[audit] ok: source and target match, every listed process is at version N`。

**第 8 步:改回 GC**(TiDB 档),audit 过了再改,两个集群都要改回 §3 之前的值(默认 `10m0s`):`SET GLOBAL tidb_gc_life_time='10m0s';`。

**搬的是 `mmorpg_chat` 时,同一步把保留期重新打开**(TiDB 档,2026-11-12 起)。`chat_send_authority` 带 TTL 属性,原件保留 30 天后由库删除(`docs/design/chat-world-channel-fanout.md` §5.2)。

- BR 恢复出来的表 `TTL_ENABLE` 是 `OFF`(BR 的既定行为,免得一恢复就删),TiCDC 的下游也应保持 `OFF`:源集群的 TTL 删除是普通事务,会随复制到目标;目标自己再删一遍的话,两边删的行不同步,`verify` 与 `audit` 在固定位置上的校验和会对不上。所以**从恢复到 audit 通过,目标上不要开 TTL**。先查一眼:`SHOW CREATE TABLE mmorpg_chat.chat_send_authority` 里应当是 `TTL_ENABLE='OFF'`,不是就先关掉。
- audit 通过之后在目标集群执行 `ALTER TABLE mmorpg_chat.chat_send_authority TTL_ENABLE='ON';`。忘了这一步,表从此只增不减;chat 每次启动会打一行 ERROR(`chat_send_authority has no enabled TTL attribute`),看到它就回来补这条。
- 别的逻辑库以后有带 TTL 的表,同样处理。

**第 9 步:cleanup。** 保留期默认 7 天。冻结的源库是目标出问题时唯一的退路,不要急着删。

```
dbmove cleanup -db mmorpg_guild -run-id <run-id> -sync ticdc:cdc-a:8300:dbmove-mmorpg-guild -confirm-drop
```

判据:退出码 0,输出 `[cleanup] ok: database mmorpg_guild dropped from cluster a`。

要求 audit 成功过。audit 已经跑不了时(比如快照被 GC)加 `-acknowledge-no-audit`。源库闸门不是这个 run 冻结的会拒绝:那说明连接串指错了。

**删库之前工具核喂过目标的那条复制流已经不存在**(第 6 步做了没有),所以 `-sync` 必填,写 `cutover` 用的那一个;不给直接拒绝,不会跳过这道检查。

- 核的是映射里 `move.verified_feed` 记下的那一条。TiDB 档问 TiCDC:changefeed 还查得到就拒绝(`still exists`),不管它是 `normal`、`stopped`(暂停)还是 `failed`;只有 TiCDC 明确回答「没有这个 changefeed」才算撤掉。MySQL 档连目标(要设 `DBMOVE_TARGET_ADMIN_DSN`,地址同样要和 `SqlClusters` 对上),目标上还有这个通道的配置就拒绝(`is still configured on the target`),`RESET REPLICA ALL` 之后才没有。
- 查不成一律拒绝(`cannot tell whether the replication stream`):TiCDC 连不上、回 5xx、回一个说不清的 404,或者读不了目标的 `performance_schema`。不知道不等于没有。
- TiDB 档只在 `cutover` 读过这条流的那个 TiCDC 地址上问(映射的 `move.feed_endpoint`)。换一个 TiCDC,它当然答「没有这个 changefeed」,这不说明什么;地址对不上就拒绝(`proves nothing`)。**反向搬的时候最容易写错**:复制方向反过来了,changefeed 在另一个集群的 TiCDC 上,id 却常常同名。确实是同一个 TiCDC 集群的另一个地址(换了 capture、走隧道)时加 `-allow-addr-mismatch`。
- `-sync` 里的 changefeed 或通道名和映射记下的不一样也拒绝(`-sync names ...`)。
- 工具只认 `verify` 记下的那一条。翻转之后另外起的、同方向的复制(别的 changefeed id、别的通道名)要自己撤,工具不知道它们。

核完紧接着才 `DROP DATABASE`。重跑幂等:库已经删了就只补记映射。

## 5. 玩家感知

| 操作 | 窗口里发生什么 | 窗口之后 |
|---|---|---|
| 存盘 | go/db 回 `Unavailable`,scene 按原序号退避重试,不记未知结局 | 重试落到新集群 |
| 服务写 RPC | `Unavailable`,客户端或调用方重试 | 同上 |
| 写者池上的读 | 也过闸门,也回 `Unavailable`。读写都暂停 | 同上 |
| 后台循环 | 撞到冻结不算结局未知,不打排空 HOLD,下一轮再试 | 同上 |
| 在途事务 | 冻结要等它结束;它提交了就算数,随复制过去 | 不受影响 |
| 只读连接池 | 照常读旧集群,读到的是冻结时的数据 | Watch 到后换连接 |
| 窗口里新起的 Pod | 注册回可重试错误,不用掉名册 entry。启动期的核对(`Open`、表形状)不过闸门,不会因为撞上窗口退出 | 正常起来 |
| 在线玩家 | 不掉线。这个库上的读写操作要等到窗口结束,小库几秒,大库见下 | |

窗口的长度 = 等复制追平的几秒 + 窗口里那次全量比对(§4 第 5 步)。小库是秒级;大库和 `verify` 的比对耗时相当,这段时间这个库读写一直暂停,事先按 `verify` 的耗时告知业务,或者用 `-skip-final-compare` 换回秒级并接受它的代价。实际时长压测确认。观察:`writer_frozen_seconds{logical_db}`(这个进程撞到闸门连续冻结了多久,解冻后归零)、`db_placement_version`、`db_placement_sync_fail_total`、`db_placement_rejected_total`(工具不会写出被拒绝的映射,这个数涨了要查)。`writer_frozen_seconds` 超过 2 分钟告警 `WriterGateFrozenTooLong`(`deploy/k8s/logical-db-alerts.yaml`);窗口里那次比对预计超过 2 分钟时,这条告警在正常的切换里也会响,事先知会值班。

## 6. 故障处置

每种故障的结局。退出码 3 的两种都不要手改闸门表和映射。

| 现象 | 判断 | 结局和处置 |
|---|---|---|
| 退出码 1,`replication is not ready` | 延迟高或复制断了 | 冻结之前就拒绝了,什么都没变。查复制,重跑 |
| 退出码 1,`run verify again`、`no successful verify` | verify 过期或没做;窗口里复制断过或有写者连过目标,记录已清 | 跑 verify |
| 退出码 1,`the replication stream changed since the last verify` | 复制流重建过:changefeed 的 `start_ts`、`create_time` 变了,或通道的来源变了;或者目标库删了重建过(`target_created` 变了) | 什么都没变。**中间的写可能没复制过去**,重跑 verify,报差异就从全量重来(§3) |
| 退出码 1,`the config of the replication stream changed since the last verify` | 复制流没换,配置被改过:TiDB 档是 changefeed 的 `config`(过滤规则、`ignore-txn-start-ts` 等,TiCDC 升级带来的缺省项变化也算),MySQL 档是通道的复制过滤 | 什么都没变。改过之后可能有一段写没复制过去,重跑 verify,报差异就从全量重来(§3) |
| 退出码 1,`replica_skip_errors=...`、`replica_exec_mode=...` | MySQL 档目标主库开着跳过错误的开关:应用出错的事件被直接跳过,目标可能已经少了写入 | 什么都没变。改回 `replica_skip_errors=OFF`、`replica_exec_mode=STRICT`;之前有没有跳过已经说不清,从全量重来(§3) |
| 退出码 1,`which -compare-budget=... cannot hold` | `verify` 那次比对的耗时不小于 `-compare-budget`(默认 30 秒),窗口里的全量比对装不下 | 什么都没变,没有冻结过。把 `-compare-budget` 调到比 `verify` 的耗时大(这个库要停这么久),或者按 §4 第 5 步权衡 `-skip-final-compare` |
| 退出码 2,`cutover rolled back`,原因是 `replication is broken: the replication stream changed since the last verify`(或 `the config of the replication stream changed ...`) | 预检之后、窗口里复制流被重建了,或者它的配置被改了 | 已回滚,verify 记录已清。查是谁动的,重跑 verify |
| 退出码 2,`cutover rolled back`,原因是 `the target differs from the frozen source after replication caught up` | 窗口里的全量比对不一致:源库冻结、复制追平之后两边内容不同。`verify` 之后复制被跳过了一段(§3 禁止的那几种操作),或者有人写过目标;带 TTL 的表也可能是源集群的 TTL 任务正好在窗口里删行 | 已回滚,业务恢复,verify 记录已清,**没有翻转**。输出里有不一致的表和两边的行数、摘要。查 changefeed 或通道的操作记录;重跑 verify,报差异就从全量重来(§3)。只是 TTL 撞上的话重跑 verify 会通过 |
| 退出码 2,`cutover rolled back`,原因是 `comparing the frozen source with the target was cut short` 或 `compare the frozen source with the target: ...` | 窗口里的全量比对没做完:超了 `-compare-budget`,或者读表出错(输出里有原因) | 已回滚,verify 记录留着(没有证据说数据不一致)。前者加大 `-compare-budget`,后者按原因修,重跑 cutover |
| 退出码 1,`points at the target itself`、`is not a replica of the source` | MySQL 档 `DBMOVE_VERIFY_SOURCE_DSN` 配错了:指到了目标,或者集合里没有复制来源的事务 | 什么都没变。改成源库的副本 |
| 退出码 1,`the target admin account is not enough` | TiDB 档的 `DBMOVE_TARGET_ADMIN_DSN` 账号没有全局 `PROCESS`,查连接只看得到自己 | 什么都没变。补权限(§2),重跑 |
| 退出码 1,`shared writer accounts are locked` | 记成共享的账号在目标上其实锁着:上一次搬迁回滚留下的,或者先被锁、后来才补了别的库的授权。窗口不解锁它,翻转之后这个库连不上 | 什么都没变。确认没有进程提前连目标,手工 `ALTER USER ... ACCOUNT UNLOCK`,重跑 |
| 退出码 1,`is older than its start_ts`、`more than 4 sync point intervals` | verify 站的 syncpoint 不能用:同名旧 changefeed 留下的,或者复制落后、sync point 停了 | 前者等一个间隔重跑。后者查复制 |
| 退出码 1,`does not use SOURCE_AUTO_POSITION=1` | 通道按文件位置复制 | 改成 GTID 自动定位。之前有没有跳过事务看不出来,从全量重来 |
| 退出码 1,`these accounts do not exist on the target`、`missing grants` | 账号没建或授权没补,常见于名册里下一个主体 | 什么都没变。在目标上补上,新建的写者账号重跑 `prepare` 锁住,再重跑 cutover |
| 退出码 1,`appeared after prepare` | `prepare` 之后名册换了或多了写者 | 什么都没变。同一个 run id 重跑 `prepare`,再重跑 cutover |
| 退出码 1,`also have grants on other databases` | 写者账号还在写目标集群上别的库 | 见 §4 第 1 步 |
| 退出码 1,`have no primary key`(plan、prepare)或 `has no primary key`(verify、audit) | 点名的表没有主键:逐行摘要比不出这种表的差异,不能搬 | 什么都没变。给它加主键(§2 第 6 条)。`verify` 才报出来说明表是 `prepare` 之后建的:这期间不该有 DDL,查是谁建的 |
| 退出码 1,报表不一致,内容是 `the table definition differs` | 两边表定义不同(方括号里是只在一边出现的列、索引):目标少了唯一约束、列类型或排序规则不同、索引列序不同;也可能只是两个集群版本不同,同一份定义展示不一样 | 什么都没变。**不要切**。按差异把目标的定义改成和源库一样;改不了、或者说不清为什么不同,从全量重来(§3)。两边版本不同就先对齐版本 |
| 退出码 1,`etcd has no /mmorpg/db_placement_default`、`DefaultCluster is ... in etcd` | etcd 被清过,服务还没写回默认集群;或者 `MMORPG_DB_PLACEMENT` 和部署不一致 | 前者起一次 `-migrate` 或服务写回。后者改环境变量,默认集群以 etcd 为准 |
| 退出码 1,`these processes do not know cluster` | 进程的 `SqlClusters` 没有目标集群,常见于改了 ConfigMap 没改 `runtimeConfigSecret`;或读不到它的 metrics | 什么都没变。改 Secret 滚动重启,重跑。`-metrics-targets` 列错了就改 |
| 退出码 1,`cannot control the source gate` / `target gate` | 维护账号权限不够;或探测撞上长事务的锁 | 查 §2 第 4 条。锁等待的话重跑 |
| 退出码 1,`frozen by another run` | 排空 run 或别的搬迁开着 | 等它结束。不要绕过 |
| 退出码 2,`cutover rolled back`,原因是 `did not catch up within the budget` | 超预算 | 已经先作废翻转、再解冻源库,业务恢复,`state` 还是 `MOVING`。查复制,等延迟降下来重跑 cutover |
| 退出码 2,`cutover rolled back`,原因是 `replication is broken` | changefeed 停了或通道断了,不用等满预算 | 同上。verify 记录已清:修好复制,重跑 verify 再 cutover |
| 退出码 2,`cutover rolled back`,原因是 `writer accounts are connected to the target before the flip` | 有进程提前配了新地址,或者有人用写者账号手工连了目标。输出里有账号和库名 | 已回滚,账号保持锁定,verify 记录已清:它可能已经写过目标。查到进程改回去,断开连接,重跑 verify。报差异就从全量重来(§3) |
| 退出码 2,`cutover rolled back`,原因是 freeze 失败 | 闸门冻结拿不到锁(长事务占 slot)或源库连不上 | 源库已确认解冻。查长事务,重跑 |
| 退出码 2,`cutover rolled back`,原因是 `flip the mapping: ... changed concurrently` | 翻转时 CAS 冲突:映射被改了,还属于这个 run(比如并发跑了 verify) | 同上,重跑。不要并发跑两个步骤 |
| 退出码 2,`etcd did not answer, but this run never sent the flip` | 冻结后 etcd 挂了,还没走到翻转 | 没有人翻转得了,已解冻,业务恢复。etcd 恢复后重跑 |
| 退出码 3,`the flip may or may not have landed, so the source stays frozen` | 翻转时 etcd 不通或没有回音 | **源库保持冻结,这个库读写暂停**,绝不能手动解冻:翻转如果其实成了,旧进程写源库、新进程写目标,两边各丢一半。先恢复 etcd,然后重跑 cutover(读到已翻转就结束,没翻转就接着做完),或者 rollback |
| 退出码 3,`changed underneath this run` | 映射被别的东西改了,认不出来 | 源库保持冻结。`etcdctl get` 看现状,查是谁改的。还属于这个 run 就 rollback |
| 退出码 3,`the source is still frozen, could not release it` | 回滚时解冻失败 | 映射已作废。查源库连接,重跑 rollback(幂等) |
| 退出码 3,`the writer accounts on the target may still be unlocked` | 回滚时源库解冻成功,目标上的账号没锁回去 | 源库的闸门表正在复制过去,目标又是 `writable=1`,**这时连到目标的写者能写**。尽快修目标的连接,重跑 rollback(幂等) |
| 退出码 2,`is flipped, but the flip marker on the target could not be written` | 翻转已经成了,只是标记没写上 | 不影响业务。重跑 cutover 补上,输出 `already done`。没补上的话 etcd 回档之后 rollback 发现不了 |
| 退出码 1,`the target has the flip marker` | rollback、重跑 cutover 或 prepare 读到翻转标记,映射却说没翻转:etcd 回档过 | **不能解冻源库**,目标已经在收写入。按下面「etcd 从备份恢复」写回映射 |
| 退出码 1,`cannot read the flip marker` | 连不上目标 | 修连接重跑。rollback 时目标真的没了,而且确定没收过写入(etcd 没回档过),才加 `-target-lost` |
| 退出码 1,`cannot rule out that the flip took effect`;退出码 3,`the flip may have taken effect on the target` | 目标闸门被这个 run 打开过(`writer_epoch` 比冻结的源库大),而且写者账号连着目标,或者读不到目标。可能是翻转落地之后 etcd 回档、标记还没写;也可能是有进程提前配了新地址 | **源库保持冻结,不要手动解冻**。看输出里的账号和库名,查进程的 `db_placement_version` 和日志:进程确实收到过翻转,按下面「etcd 从备份恢复」写回映射;只是提前配错的进程,改回去、断开连接,重跑 rollback。目标真的没了才加 `-target-lost` |
| 翻转之后写者连目标报账号已锁,或 rollback 报 `Re-run cutover` | 并发跑了 rollback 或 prepare,翻转赢了但账号被锁上了 | 重跑 cutover,输出 `already done` 并解锁。不要并发跑两个步骤 |
| cleanup 退出码 1,`still exists`(TiDB 档)或 `is still configured on the target`(MySQL 档) | 喂过目标的复制流没撤掉,暂停、停住都算。这时删源库,`DROP DATABASE` 会顺着它到达目标,删掉正在用的库 | 什么都没删。按 §4 第 6 步撤掉(`changefeed remove`、`RESET REPLICA ALL`),重跑 cleanup |
| cleanup 退出码 1,`cannot tell whether the replication stream` | 查不到复制流还在不在:TiCDC 连不上或回了说不清的答案;MySQL 档读不了目标的 `performance_schema` | 什么都没删。修连接或权限(§2 第 4 条),重跑。不要因为查不到就手工 `DROP DATABASE` |
| cleanup 退出码 1,`proves nothing` | `-sync` 的 TiCDC 地址不是 `cutover` 读这条流用的那个(映射的 `move.feed_endpoint`)。常见于反向搬之后照抄了上一次的 `-sync` | 什么都没删。改成 `cutover` 用的 `-sync`。确实是同一个 TiCDC 集群的另一个地址才加 `-allow-addr-mismatch` |
| cleanup 退出码 1,`-sync names ...` | `-sync` 里的 changefeed 或通道名不是这次搬迁 `verify` 记下的那一个,或者档位不对 | 什么都没删。按映射的 `move.verified_feed` 改 `-sync` |
| cleanup 退出码 1,`does not record the replication stream` | 映射里没有 `move.verified_feed`:映射是手工写回的,漏了这一段 | 什么都没删。按本地日志里 `verify` 那一行(`replication stream {...}`)把 `verified_feed` 写回映射,TiDB 档连 `feed_endpoint` 一起,再重跑 |
| 工具进程被杀 | 窗口里被 `kill -9`,可能留下冻结的源库,也可能已经解锁账号、打开目标闸门。没人重跑的话这个库读写一直暂停,`WriterGateFrozenTooLong` 会响 | `etcdctl get` 看映射,run id 在 `move.run_id`。`state=MOVING`:用这个 run id 重跑 cutover 接着做完(复用记下的目标位置),或者 rollback(账号锁回去、解冻)。`cluster` 已是目标:重跑 cutover,补翻转标记,输出 `already done`。都幂等。**不要手改闸门表和账号** |
| `WriterGateFrozenTooLong` 告警 | 有写者被冻结的闸门挡了 2 分钟以上 | 先看 `dbmove cutover` 是不是还在跑:大库的窗口里要做一次全量比对,本身就可能超过 2 分钟,在跑就等它(输出里逐表打 `rows match`),不要抢着 rollback。工具已经不在了,再查这个库 `SELECT writable,HEX(freeze_run_id) FROM route_meta`。等于映射的 `move.run_id`:是搬迁留下的,按上一行。不等:是排空 run,按排空的流程。`writable=1` 而某个进程还在报:它的 Watch 没跟上,按下一行 |
| 工具进程收到 Ctrl-C | 等于超预算 | 走回滚,退出码 2 |
| 翻转之后个别进程一直报 `ErrWriterFrozen` | 它的 Watch 没跟上 | 写不进去,不丢数据。看它的 `db_placement_sync_fail_total` 和到 etcd 的网络,必要时重启 |
| 进程报「权威SQL逻辑库落点未知」 | 它的 `SqlClusters` 里没有目标集群,而且没列进 `-metrics-targets` | 改配置滚动重启。`db_placement_version` 停在旧版本,audit 也会报 |
| 进程起不来,报 `db placement: initial read` | 启动时读不到 etcd | 有意的:分不清「没有映射」和「读不到」的进程可能连到已搬走的库 |
| 进程起不来,报 `DefaultCluster differs from the one recorded in etcd` | 配置的默认集群和 `/mmorpg/db_placement_default` 不一致 | 有意的:没有映射键的库都在记下的集群上。改回配置。要把这些库搬走用 `tools/dbmove` |
| `-migrate` 报 `state=MOVING` | 搬迁期间不做 DDL | 搬完再发。发布不能等就 rollback |
| `-migrate` 报 `no DbPlacement section` | staging、prod 的 `schemaConfigSecret` 没有 `DbPlacement` 段,看不到 `MOVING` | 把段抄进 Secret(开服手册 §1)。go/db 的 `db-migrate` Job 同样注入;手工跑 `cmd/migrate` 时自己设 `MMORPG_DB_PLACEMENT_REQUIRED=1`,配置文件带这一段 |
| etcd 从备份恢复,映射回退了 | 必须用 `etcdutl snapshot restore <备份> --bump-revision 1000000000 --mark-compacted ...` 恢复。带了的话回退的值进程不接受(`db_placement_rejected_total{reason="version_regress"}`)。没带的话 revision 也回退,之后写的每个键都被运行中的进程当旧事件跳过,跟不上翻转,只有全量读时计 `reason="revision_regress"`。新起的进程读到旧映射,连到冻结的源库写不进去 | 不会分叉,`rollback` 读到翻转标记也会拒绝。按本地日志或进程日志里的集群和版本,用 `etcdctl put` 写回映射值。源库还没 `cleanup` 的话,写回的值要带上 `move`(`run_id`、`source_cluster`、`target_cluster`、`flipped_ms`、`target_point`,以及 `verified_feed`:本地日志里 `verify` 成功那一行的 `replication stream {...}` 原样抄回;TiDB 档再加 `feed_endpoint`,就是 `cutover` 的 `-sync` 里的 TiCDC 地址,带 `http://`):`audit`、`cleanup` 靠它认这次搬迁,没有 `verified_feed` 时 `cleanup` 不删源库。没带 `--bump-revision` 已经恢复了的,写回之后滚动重启全部用映射的进程 |

## 7. 回滚

```
dbmove rollback -db mmorpg_guild -run-id <run-id>
```

- **翻转之前**:先读目标上的翻转标记(读到就拒绝:映射只在 etcd 里,etcd 回档过的话它说没翻转不算数),再核目标闸门没有被这个 run 打开过、或者打开过但没有写者连着(§6),把写者账号再锁一遍(崩在解锁之后的话还开着),把映射改回 `ACTIVE`(让并发的翻转失效),最后解冻源库。解冻之后源库的闸门表复制到目标,目标又是 `writable=1`,所以锁账号排在解冻前面。重跑幂等。输出 `[rollback] ok: mmorpg_guild is ACTIVE on cluster a and writable`。然后停复制(§4 第 6 步),删掉目标上的半成品:`DROP DATABASE mmorpg_guild;`。不删的话下一次 `plan` 会拒绝。
- **账号回滚之后保持锁定**。下一次 `prepare` 幂等,不用动。不打算再搬到这个集群时手工 `ALTER USER ... ACCOUNT UNLOCK` 或者删账号。
- **目标连不上**:读不到翻转标记,分不清翻转过没有,默认拒绝。确定目标没收过写入之后加 `-target-lost`,不读标记、不碰账号,只改映射、解冻源库。目标恢复之后手工核账号还锁着,再删半成品。
- **翻转之后**:拒绝。目标已经在收写入,回到源库会丢这部分数据。要回去只能反向再搬一次:先撤掉这一次的复制(§4 第 6 步),再 `cleanup` 源库(保留期没到就加 `-retention 0s -confirm-drop`,`-sync` 照带),再用新的 run id 从 `plan` 开始。`-retention 0s` 是翻转之后立刻删源库,这时复制多半还在跑:不撤掉它,`DROP DATABASE` 会被复制到正在收写入的目标上,所以 `cleanup` 看到复制流还在就拒绝。反向搬走完全相同的账号流程,`-retention 0s` 不改变这一点:原来的源集群这时是目标,`prepare` 在上面锁写者账号,`cutover` 解锁。账号在那个集群上还写别的库的话,要加 `-allow-shared-writer-accounts`(§4 第 1 步)。

## 8. 审计

翻转之后跑,目标已经在收写入,所以目标要站在翻转之前的位置上比。它比的内容和 `cutover` 窗口里那次全量比对相同(表的定义加数据),多核一遍各进程的版本;`cutover` 加了 `-skip-final-compare` 时(映射里 `move.final_compare_skipped` 是 `true`),它是这次搬迁在 `verify` 之后唯一的一次数据核对,翻转一完成就跑。工具做两件事:

1. 读 `-metrics-targets` 里每个进程的 `db_placement_version{logical_db}`,必须等于映射版本。进程配了映射就上报 etcd 里有键的库,不等它连过。**把用这个库的进程都列上**:没列的不核,列了却没上报的算失败。Java 进程没有这个指标,不要列(§9)。
2. 全量校验和:源库读当前数据(已冻结)。目标的位置:
   - TiDB 档:`tidb_snapshot` 设成 cutover 记下的 TSO(映射值 `move.target_point`)。这个 TSO 要还没被 GC,所以第 8 步在 audit 之后才改回 GC。
   - MySQL 档:目标主库的一个**副本**,切换之前先 `STOP REPLICA` 停住,切换之后追到 cutover 打印的 `target point`:

     ```sql
     -- 单线程应用才能停得准:并行应用可能越过。
     SET GLOBAL replica_parallel_workers=0;
     START REPLICA UNTIL SQL_AFTER_GTIDS='<target point>';
     ```

     `DBMOVE_AUDIT_TARGET_DSN` 指向它。它的 `gtid_executed` 必须正好等于 target point,否则工具拒绝:多一个事务就可能含翻转之后的写入。

   目标位置在打开目标闸门之前写进映射,窗口崩了重跑时复用,所以目标站的位置在那次打开之前。`route_meta`、`writer_slot` 单独比:目标允许比源库正好多一次 release(epoch 加 1、`writable=1`、`freeze_run_id` 清空,`writer_slot` 同步),多了少了都算不一致。`db_move_sentinel` 不比:重跑一次窗口源库就多一行新哨兵,翻转标记也在这张表,都和业务数据无关。

判据:退出码 0,输出 `[audit] ok`。

退出码 1 的处理:

- **进程版本落后或没上报**:按 §6 处理那个进程,重跑 audit。不影响数据。
- **表内容不一致**:先不要 cleanup,冻结的源库是证据也是退路。报告里有表名和两边的行数、摘要;报的是 `the table definition differs` 时是表定义不同,方括号里列出只在一边出现的列或索引。
  1. 看是不是操作问题:审计副本的位置对不对(MySQL 档工具会核),有没有人手动写过源库(`db_move_sentinel` 之外不该有变化)。
  2. 是真不一致:写入已经落在目标上,不能简单切回。按表用 `sync-diff-inspector`(TiDB)或逐行比对找出差异行,判断是复制丢了还是后来的业务写入。这是数据事故,按事故流程处理,保留本地日志、changefeed 或通道的状态、差异行。

## 9. Java 进程的库

Java 进程(网关的 `mmorpg` 库,`mall_game` 的 `COMMERCE_JDBC_URL`)不做动态拨号,JDBC URL 是启动时定的。搬这些库时:

1. **翻转之前不要动 Java 配置,不要提前写新地址。** 目标上的闸门表是复制过去的,`writable=1`:配了新地址的进程一重启就连到目标。账号锁着时它连不上、起不来;账号没锁时它能写,两边分叉,`cutover` 查到连接只能回滚。
2. 窗口里:Java 写者在闸门上收到 `WriterFrozenException`(`SQLTransientException`,SQLSTATE 40001),按可重试处理。读也一样。
3. **cutover 翻转之后才改 Java 配置,立刻滚动重启这些进程**,用新地址。重启完成之前这个库对它们是读写暂停(源库冻结),不会写错地方。窗口比 Go 服务长,选低峰。
   2026-11-06 起网关 Pod 的 registrar init 和 Go 写者一样按库映射拨号(`MMORPG_DB_PLACEMENT`),主进程的 JDBC URL 仍是静态的。翻转之后、改配置之前有网关 Pod 重启的话,registrar 登记在目标集群,主进程仍连源库,启动核对资格对不上就拒启;不会写错地方,改完配置重启即恢复。
4. `go/login` 的密码校验是只读直连 `mmorpg`,`PasswordAuth` 的 DSN 同样改完滚动重启;重启之前读到的是冻结时的数据。`password_admin` 的连接串也改成新地址。

`cutover`、`audit` 的 `-metrics-targets` 不列这些进程。它们的账号写进 `-roster` 或者已经 ACTIVE,照样由 `prepare` 锁、`cutover` 解。

## 10. 给 Codex 的验证要求

**以下都没有跑过。** 按顺序执行,前一项不过就停下来报告,保留证据:工具的完整输出和退出码、本地日志、`etcdctl get` 结果、`git rev-parse HEAD`。

```bash
# go/proto 落后于 proto 源时(SqlWriterBootRoster_Entry 还是 Request 单数)先重生,否则下面编不过
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
cd go/shared && go vet ./dbplacement/ ./authoritysql/ ./domainhost/ ./assetop/ && go test -count=1 ./dbplacement/ ./authoritysql/ ./assetop/
cd go/schemamigrate && go test -count=1 ./...
cd tools/dbmove && go mod tidy && go vet ./... && go test -count=1 ./...
cd tools/merge_zone && go mod tidy && go build ./... && go test -count=1 ./...    # authoritysql 新依赖 prometheus,这个 module 之前没有
cd go && for m in guild trade friend player_inbox mail rank chat match data_service db; do (cd $m && go build ./... && go vet ./...); done
cd go/data_service && go test -count=1 ./internal/store/ ./internal/routetoolcli/ ./cmd/writer_authority/ ./cmd/admission_recover/
cd go/db && go test -count=1 ./cmd/... ./internal/logic/pkg/proto_sql/...
cd go/match && go test -count=1 ./internal/kafka/ ./internal/authority/
cd go/chat && go test -count=1 ./internal/authority/
cd java/authority_jdbc && mvn -q test -Dtest=AuthorityConnectionTest
pwsh -File tools/scripts/tests/dev_tools_db_move_contract.tests.ps1
pwsh -File tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1
pwsh -File tools/scripts/tests/sql_boot_catalog_local.tests.ps1
pwsh -File tools/scripts/tests/docker_debug_sql_boot.tests.ps1
pwsh -File tools/scripts/tests/maintenance_drain.tests.ps1
pwsh -File tools/scripts/tests/global_authority_deploy.tests.ps1
pwsh -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1
```

真库用例,TiDB 和 MySQL 两档各跑一遍(`DBMOVE_IT_REQUIRE=1` 下 SKIP 算失败)。要两个实例,数字 loopback,账号能建库删库:

```powershell
cd tools/dbmove
$env:DBMOVE_IT_REQUIRE='1'
$env:DBMOVE_IT_SOURCE_DSN='<账号:口令@tcp(127.0.0.1:4000)/>'
$env:DBMOVE_IT_TARGET_DSN='<账号:口令@tcp(127.0.0.1:4001)/>'
$env:MMORPG_SQL_BACKEND='tidb'; go test -count=1 -run '^TestIT' -v ./...      # 换 mysql 实例和 'mysql' 再跑一遍
cd go/shared
$env:DBPLACEMENT_IT_ETCD='127.0.0.1:2379'; go test -count=1 -run '^TestStoreAgainstRealEtcd$' -v ./dbplacement/
```

重点核:`ADMIN CHECKSUM TABLE` 在 `tidb_snapshot` 下的 `Total_kvs` 能读到、管理员账号够不够(`TestITChecksumMatchesAndCatchesDifference`);TiDB 全表逐行摘要(每列一次 `MD5` 再合一次,比原来的一行一次 `MD5` 重)在大表上的耗时和内存(超了要改成分块):它现在也跑在冻结窗口里,直接决定窗口多长;账号锁定、`SHOW GRANTS` 解析、连接列表在两个后端上的形状(`TestITAccountLockGrantsAndConnections`)。TiCDC OpenAPI 的 `start_ts`、`create_time`、`config.enable_sync_point`、`config.sync_point_interval` 字段形状是按记忆写的,用真实 `curl <cdc>/api/v2/changefeeds/<id>` 核一次,对不上就改 `changefeedDetail`。

2026-11-12 新加的语句同样没在真库上跑过,对应的用例和要核的点:

- `TestITTableDefinitionsOnRealDatabases`:`information_schema.columns`、`information_schema.statistics` 读表定义(`statistics` 的 `expression` 列要 MySQL 8.0.13 以上;`non_unique` 两个后端类型不同,按文本读),两个后端各比出七种定义差异;没有主键、只有非空唯一键的表被 `TablesWithoutPrimaryKey` 点名、被比对拒绝。
- `TestITChecksumMatchesAndCatchesDifference` 新增的一段:`('a,b','c')` 和 `('a','b,c')` 这样逗号挪了位置的两行要判不同(旧的 `CONCAT_WS` 拼法在 TiDB 档判相同)。
- `TestITReplicaSettingsOnRealMySQL`(只在 mysql 档起作用):`@@global.replica_skip_errors`、`@@global.replica_exec_mode`、`performance_schema.replication_applier_filters`、`replication_connection_configuration` 读得出来。变量名是 8.0.26 起的写法。
- `TestITCutoverOnRealDatabases`:窗口里多了一次全量比对,读两侧主库的当前数据。
- TiCDC 对不存在的 changefeed 的回答(HTTP 400 加 `CDC:ErrChangeFeedNotExists`)只有替身用例,`cleanup` 认的就是它:用真实 TiCDC `curl` 一个不存在的 id 核一次,状态码或错误码不同就改 `changefeedNotExists`,否则 `cleanup` 永远拒绝。changefeed 的 `config` 整份取摘要,连读两次同一个 changefeed 摘要要相同(`verify` 自己会比,不同就一直报 `changed during verify`)。

**演练**,两个集群各一套(TiDB 两个、MySQL 两个),`robot` 持续在线,按 §4 全程跑一遍:

| 场景 | 步骤 | 通过标准 |
|---|---|---|
| **正常搬库** | 搬 `mmorpg_guild` 和一个 `player_store_<id>_db` | 切换前后 robot 不断线;读写暂停的实际时长记下来,和 `verify` 输出的比对耗时对照(窗口里多一次全量比对);`verify`、`audit` 退出码 0;`db_placement_version` 全部跟上;之后的写都落在目标集群(源库 `route_meta` 不再变) |
| **MOVING 拒绝 DDL** | `prepare` 之后跑服务的 `-migrate` 和 go/db 的 `cmd/migrate -command up` | 都拒绝,点名 `state=MOVING`;`-command plan` 照常 |
| **超预算回滚** | 暂停 changefeed 或停通道 IO,跑 cutover | 延迟高时拒绝;窗口内暂停时回滚,退出码 2,业务恢复;恢复复制后重跑成功 |
| **复制断开** | 窗口内 `changefeed pause` / `STOP REPLICA` | 立刻回滚,不等满预算 |
| **etcd 故障** | ① 冻结后、翻转前停 etcd;② 翻转那一步停 etcd(可在 `-budget` 内用防火墙挡) | ① 解冻,退出码 2;② 源库保持冻结,退出码 3,恢复 etcd 后重跑结束。全程没有两边都写的时刻 |
| **进程崩溃** | 窗口内 `kill -9` 工具 | 源库冻结;重跑 cutover 能做完;换一次重跑 rollback 能解冻 |
| **并发写入对账** | robot 带持续存盘、邮件、交易跑全程 | `audit` 一致;抽样玩家存档、资产和切换前的预期一致;没有写丢 |
| **Watch 落后** | 隔离一个进程到 etcd 的网络,翻转,恢复 | 隔离期间它写不进去、不写错地方;只读连接池读到冻结时的数据;恢复后自动跟上 |
| **反向再搬** | 撤掉复制、cleanup(带 `-sync`)之后搬回去,含 `-retention 0s`;反向那一次的 cleanup 故意照抄正向的 `-sync` 再试一遍 | 版本接着涨,账号在原集群上锁定、翻转时解锁,全程照常;照抄 `-sync` 的那次被拒绝(`proves nothing`) |
| **复制重建** | `verify` 之后删 changefeed,写入一段,用更新的 `--start-ts` 同名重建;MySQL 档换通道来源 | 延迟回到 0,`cutover` 仍退出码 1,点名 `replication stream changed`,什么都没变;重跑 `verify` 报差异 |
| **提前连目标** | 复制期用写者账号连目标;再试一次在 `prepare` 之前连好、不断开 | 新连接被账号锁拒绝;已有连接让 `cutover` 退出码 2 回滚,账号仍锁定,verify 记录已清 |
| **窗口崩溃的每个点** | 在记目标位置之后、解锁之后、打开目标闸门之后、翻转之后各 `kill -9` 一次 | 重跑 cutover 都能做完,`target_point` 复用,`audit` 退出码 0;换成 rollback 时账号锁回去、源库解冻,不分叉 |
| **etcd 回档** | 翻转之后从翻转之前的快照恢复 etcd,带 `--bump-revision`;再做一次不带的 | `rollback` 读到翻转标记拒绝;带的计 `version_regress`,不带的计 `revision_regress`;全程没有两边都写的时刻;`etcdctl put` 写回后恢复 |
| **旧名册注册** | 一次 cutover 和一次回滚之后,用部署时渲染的名册起新 Pod;多库 entry 注册到一半杀 init、再杀 Pod | 注册成功,收据记实际 epoch;杀 init 后接着注册剩下的库;杀 Pod 后换下一条 entry,已注册的按非 run RETIRE 退休 |
| **窗口里起 Pod** | 窗口内滚动重启 data_service、go/db、chat、match,窗口人为拉长 | 不用掉名册 entry;进程启动期的核对不报「实际库不匹配」;`writer_frozen_seconds` 上报,解冻后归零;后台循环不打 HOLD |
| **共享账号** | 搬一个 `player_store_<id>_db`,目标上已有别的存档库 | 不加开关拒绝;加了之后不锁,目标上别的库写入不受影响 |
| **工具被杀不重跑** | 窗口内 `kill -9`,放着不管 3 分钟 | `WriterGateFrozenTooLong` 响;重跑 cutover 或 rollback 后归零 |
| **进程不认识目标** | 只改 ConfigMap、不改 `runtimeConfigSecret`,跑 cutover | 退出码 1,点名进程和集群,什么都没变 |
| **复制没撤就 cleanup** | 翻转之后不做第 6 步,直接 `cleanup -retention 0s -confirm-drop`;TiDB 档再分别试 `changefeed pause` 之后、`-sync` 写成目标集群的 TiCDC;MySQL 档试只 `STOP REPLICA` | 每一种都退出码 1(`still exists`、`is still configured on the target`、`proves nothing`),源库还在,**目标库没有被删**;按第 6 步撤掉之后重跑成功 |
| **verify 之后跳过一段复制** | `verify` 通过后暂停复制、在源库写几行,TiDB 档 `changefeed resume --overwrite-checkpoint-ts` 跳过它们,MySQL 档在目标注入空事务占掉对应的 GTID;等延迟回到 0 跑 cutover | 冻结之后回滚,退出码 2,原因是 `the target differs from the frozen source`,点名表;没有翻转,业务恢复,verify 记录已清;再加 `-skip-final-compare` 重做一遍:翻转成功、标准错误输出有 `WARNING`、映射里 `final_compare_skipped` 是 `true`,`audit` 报不一致 |
| **改复制的过滤** | `verify` 之后 `changefeed pause`、`update` 加一条排除规则、`resume`;MySQL 档 `CHANGE REPLICATION FILTER ... FOR CHANNEL` 加一条忽略规则;再试目标带 `replica_skip_errors=1062` 启动 | `cutover` 退出码 1,点名 `the config of the replication stream changed`(后一种是 `replica_skip_errors=1062`),什么都没变 |
| **窗口装不下比对** | 用一个 `verify` 要比几十秒的库,`-compare-budget 5s` 跑 cutover;再用够大的 `-compare-budget`,`-budget` 保持默认 | 前者退出码 1(`cannot hold`),源库没有冻结过;后者成功,窗口时长约等于 `verify` 的比对耗时加几秒,比对超过 30 秒也不因为 `-budget` 回滚 |
| **表定义不同、没有主键** | 复制期在目标上删一个唯一索引再建成普通索引,跑 verify;另建一个没有主键的表,跑 plan | verify 退出码 1,报 `the table definition differs` 并点名那个索引;plan 退出码 1,点名那张表 |

**不要**在报告里写「性能提升」或「线上可用」,这些场景只证明逻辑。压测口径按 AGENTS.md §6。
