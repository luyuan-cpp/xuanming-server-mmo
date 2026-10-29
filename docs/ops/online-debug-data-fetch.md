# 线上玩家数据拉取与本地复现

2026-10-19 重写(此前版本的 `-mode player` 示例读的是没有生产写者的键族,照做拿不到玩家数据)。
入口仍是 `go/data_service/cmd/debug_fetch`(只读拉取)与 `go/data_service/cmd/debug_import`(导入本地);没有 ps1 包装器。

## 0. 结论

- **查某服某玩家的问题:`debug_fetch -mode player-bundle`**。一次拿全:核心 Redis 里该玩家的全部相关键(父快照、存盘屏障、归属代次、在线 / 会话、映射与落点、账号角色目录)、有效落点库里的两张玩家表、全局库的快照 / 货币物品流水 / 回档审计 / 名字,外加工具判读(在不在线、最近一次存盘落库没有、Redis 与 SQL 差在哪些字段)。
- **要在本地复现:`debug_import -file <包>`**。导入后本地 login 走真实冷加载链路就能把这个玩家加载起来。
- 「某个服」不是独立的存储单元:全服共用一台 Redis(DB 0,玩家键**不带 zone**),MySQL 是每 zone 一个库但同一实例。所以**按玩家拉,不要整库拷贝**——RDB / BGSAVE 会带走所有 zone 的玩家和以明文 token 为键名的会话键。合服后以 `player:zone:<id>` 的 home_zone 为准,不以玩家看到的服号为准;工具会自动解析。
- 旧的 `-mode player` / `snapshot` 读的是 data_service 自己的 `player:{id}:<field>` 键族,不是玩家实时数据;`-mode zone` 会 SCAN 全部映射键,线上不要用。

## 1. 玩家数据在哪(新鲜度由高到低)

| 位置 | 内容 | 新鲜度 |
|---|---|---|
| scene 进程内存(ECS) | 在线玩家的真实最新态 | 只能等玩家下线后落盘再拉 |
| 核心 Redis `PlayerAllData:<id>` | 父快照,scene 存盘写入,无 TTL | 周期存盘最多落后内存约 300 秒;资产操作即时存盘 |
| 落点库 `player_database` / `player_database_1` | Kafka DBTask 经 go/db 异步落库,行内带围栏 `fence_owner_epoch` / `fence_save_seq` | 落后父快照一段 Kafka 积压 + 重试时间;进死信时一直落后 |
| `player:{<id>}:save_barrier` | `pending` 与两张表的 ACK | **三者相等 = 最近一次存盘两张表都已落库** |
| 全局库 `player_snapshot`(source=1) | 登录 / 登出时 C++ 拍的快照 | 历史对比用 |
| 全局库 `transaction_log` | 货币 / 物品流水(按 `from_player` / `to_player`) | 「东西丢了」的主要凭证;投递失败不阻塞玩法,可能有缺口 |

导出包的 `analysis` 段直接给出这几项的判读。

## 2. 权限与数据保护

- 连线上(port-forward、取口令)由运维本人操作;AI 不登录 k8s,只处理已经拉到本地的文件(AGENTS §9)。
- 导出包含该玩家的游戏数据和账号名(角色目录);账号目录里的 `password` 字段在导出时清空且不带原字节。包文件以 0600 创建,**放仓库外的受控目录**,按工单留存,结案删除,不要进 git、不要上传。
- 工具只发读命令:Redis 一次 MULTI 里只有 TYPE / PTTL / GET / HGETALL / ZSCORE,不 SCAN;MySQL 只在可重复读事务里发固定 SELECT,结束回滚。仓库还没有 SELECT-only 账号,线上用的是业务账号——这是遗留风险,建议运维先建只读账号再用。
- 口令不写进 yaml 副本:用 `-redis-password-env` / `-mysql-password-env` 给环境变量名,工具只读变量,不打印。

## 3. 拉取

前置:**检出线上镜像对应的 commit**(镜像 tag 是 git 短 sha)。解码用的是本地检出的 proto,字段不一致时 protobuf 会「成功」解出错误的 JSON。把这个 sha 填进 `-server-revision` 留档。

```powershell
# 运维在另外两个终端里保持端口转发(示例端口):
#   kubectl -n mmorpg-infra port-forward svc/redis 16379:6379
#   kubectl -n mmorpg-infra port-forward svc/mysql 13306:3306
# 配置:把 go/db/etc/db.yaml 与 go/data_service/etc/data_service.yaml 复制到仓库外(例如 D:\secure\),口令留空。
Set-Location go/data_service
go run ./cmd/debug_fetch -mode player-bundle -player <player_id> -reason "<工单或反馈编号>" -server-revision <线上镜像 sha> `
  -db-config D:\secure\db.yaml -config D:\secure\data_service.yaml `
  -redis-addr 127.0.0.1:16379 -mysql-addr 127.0.0.1:13306 -global-mysql-addr 127.0.0.1:13306 `
  -redis-password-env DBG_REDIS_PASS -mysql-password-env DBG_MYSQL_PASS `
  -out D:\secure\player-<player_id>.json
```

参数要点:

- `-db-config`:落点库所在 MySQL(`ServerConfig.Database`)+ 核心 Redis(`ServerConfig.RedisClient`)+ 映射 Redis(`Placement.Redis`,不写时同核心,与 go/db 一致)。
- `-config`:全局库(`SnapshotMySQL`)。读不到只在包里记 `global.errors`,不影响其余部分;`-skip-global` 跳过。
- 落点库按 go/db 同一规则解析:`player:placement:<id>` → `player:zone:<id>` → `-zone`(缺省 db.yaml 的 ZoneId)。结果和依据写在 `routing`;`-database` 可显式指定,会留警告。映射或落点值畸形(空串、二进制、home ≥ 1000000 等)时,线上 go/db 对这名玩家读写全部 fail-closed——这本身就是根因:包里 `routing.malformed` 写明原因,Redis 照常导出,落点库不读,需要时用 `-database` 指定要看的库。db.yaml 开了 `Placement.Required` / `RequireRouteReady` 时也会给出对应警告。
- `-limit` 控制快照 / 流水 / 回档审计的条数(默认 20);`-timeout-seconds` 默认 60。`rollback_audit_log` 没有 player_id 索引,按玩家查是全表扫描;全局库压力敏感时用 `-skip-global`。
- `-skip-account` 不读账号角色目录。
- 包里记录每台源实例的身份(Redis `run_id`、MySQL `@@server_uuid`),导入端据此拒绝写回导出来源。**TiDB 不记身份**(2026-10-22 起一律留空,理由见 §7):从 TiDB 导出的包,导入端无从比对,要靠本地库的标记表证明写目标是本地(§4 第 4 条)。Redis 的 `run_id` 只用来拒绝「相同」:线上切主、重启之后它就变,「不同」证明不了写目标是本地,所以 Redis 写目标一律要本地标记键(§4 第 4 条)。
- 导出时还会比对线上表的列与本地 proto:线上多出的列写在 `mysql.tables[].live_extra_columns`,说明本地检出比线上旧、这些列没导出,换到线上镜像的 commit 重导。

读包的顺序:`analysis`(online / barrier.conclusion / parent_vs_sql / warnings)→ `routing` → `redis`(每把键带用途说明、类型、TTL、解码结果)→ `mysql.tables` → `global`。

- `parent_vs_sql` 列出父快照与 SQL 行取值不同的顶层字段(忽略围栏两列;空组件与未设置视为相同):屏障已对齐还有差异,说明父快照在最后一次落库之后又存过盘。
- `barrier.anomalies` 列出 login 冷加载会拒绝、但正常写路径产生不了的状态:ACK 齐了而 SQL 行围栏落后(库被回档 / 恢复)、owner_epoch 低于 pending、父快照水位与 pending 不符。补投已被 go/db 终止时 `barrier.conclusion` 会写明原因。
- 玩家在线时导出的不是最终态(`warnings` 会写明):要确切数据,等玩家下线、`barrier.conclusion` 显示两表都已落库后重导。

## 4. 导入本地复现

前置:本地全套服务可用;本地检出与包里的 `server_revision` 一致并已跑本地迁移;本地同号玩家不在线。

```powershell
Set-Location go/data_service
# 先预览(默认 dry-run,不写任何东西)
go run ./cmd/debug_import -file D:\secure\player-<player_id>.json -local-account dev_tester
# 确认后执行;本地已有同号存档时加 -replace
go run ./cmd/debug_import -file D:\secure\player-<player_id>.json -local-account dev_tester --dry-run=false
```

导入做的事:

1. 选数据来源 `-source`:`auto`(默认,父快照可用就用父快照——它包含线上还没落库的最后状态;不可用时改用 SQL 并在预览里说明原因)/ `redis-parent` / `sql`(只复现 SQL 里最后一次两表都确认的版本)。父快照缺子表或身份不符时不补骨架,`redis-parent` 直接报错——线上 login 也会拒绝这样的父快照。
2. 在本地**重铸一个存盘版本** V=(E, S):E 取线上与本地观察到的最大归属代次 + 1,S 取观察到的最大存盘序号。两张 SQL 行的围栏、`save_barrier` 的 pending 与两个 ACK、`owner_epoch` 都写成 V——这是 login 冷加载接受有历史数据玩家的条件。不照搬线上的屏障与代次;本地残留的旧 DBTask 代次更低,会被挡掉。「本地观察到的」包括本地 `owner_epoch`、两张行的围栏与本地屏障令牌;本地行围栏或屏障令牌**高于**本地 `owner_epoch` 时拒绝导入(2026-10-22,见 §7)。
3. 分三段写,任一段之后中断 login 都会拒绝该玩家(屏障 pending≠ACK),带 `-replace` 重跑即可:
   ① 一个 Lua 原子复核「同号玩家此刻不在线、owner_epoch 与预检一致」,然后删本地父快照 / 子表缓存 / 资产提交证据 / 交接标记 / 旧屏障,屏障只写 pending、`owner_epoch` 前进到 E、移出本地待落库索引;
   ② 先在全局 SQL 账号目录以 `ImportRole` 把这个角色登记到 `-local-account` 名下(不给 `-local-account` 时沿用目标目录里该角色的现有归属,没有归属就拒绝;账号目录行不存在时一并建出),再一个事务替换两张 SQL 行;
   ③ 复核 pending 未变后补齐两表 ACK(只写屏障,不写 `player_to_account`);最后登记 `player:zone:<id>` = 本地 zone。
4. 写前闸门:
   - MySQL、核心 Redis、data_service 映射 Redis(db_first 时还有全局库)必须是 `localhost` / 回环地址;本地 docker 服务名用 `-allow-target-host=<名>` 显式放行。
   - 目标实例的身份(Redis `run_id`、MySQL `@@server_uuid`)与导出包记录的来源相同则拒绝——防止把线上 port-forward 当成本地。预览会列出全部写目标,执行前核对。
   - 2026-10-22 起,写目标证明不了「不是导出来源」就拒绝,除非该目标带本地标记,或加 `-allow-unverified-target-identity`(逐个目标打 `WARNING`)。身份相同时标记与放行都不管用。
     - MySQL 目标:目标身份取得到、导出时读过**同角色**的来源(落点库对落点库,全局库对全局库)、包里每个 MySQL 来源的身份都取得到且都不同,才算证明;目标或来源是 TiDB、旧版导出包、导出时连接失败、`-skip-global` 或落点畸形没读到对应的库,都要标记。
     - Redis 目标(核心 Redis、go/db 映射 Redis、data_service MappingRedis):**一律要标记**。`run_id` 只标识进程,线上 Redis 是 HA 形态(`svc/redis` 经 haproxy 指向当前主,Pod 滚动时主动切主),导出之后切一次主、重启一次,或导出时直连了从库 Pod,线上的 `run_id` 就和包里不同。
     - 本地 TiDB / MySQL 在**每个要写的库**(落点库 `zone_<N>_db` 与 data_service 全局库)执行一次建标记,只在确认是本地隔离环境的库上执行:
       ```sql
       CREATE TABLE IF NOT EXISTS debug_import_local_target (marker VARCHAR(64) NOT NULL PRIMARY KEY);
       INSERT IGNORE INTO debug_import_local_target (marker) VALUES ('local-debug-import-target');
       ```
     - 本地 Redis 在**每个要写的实例**上、按配置里的 DB 号(db.yaml `ServerConfig.RedisClient.DB` / `Placement.Redis.DB`;data_service.yaml `DevRedis.DB`;data_service MappingRedis 恒为 DB 0)执行,不设 TTL;`FLUSHALL` / `FLUSHDB`(AGENTS §6.2 压测与复现前清库)会把它一起清掉,清完要重建:
       ```
       redis-cli -n <DB> SET debug_import:local_target local-debug-import-target
       ```
     - 两种标记都不要放进任何部署脚本或初始化清单,否则集群实例也会带上,证明不了「这是本地」。
   - 写入库固定为 `zone_<本地 zone>_db`(go/db 的派生规则);db.yaml 写了别的 `DBName` 时拒绝。
   - 本地同号玩家在线一律拒绝;已有同号存档要 `-replace`;本地映射指向别的 zone、本地落点记录把该号钉到别的库或处于冻结态时拒绝。

登录:角色归属已由第 3 条 ② 写进全局 SQL 账号目录(`-local-account` 名下),不依赖 `player_to_account`,也没有按它自愈的路径;用该 dev_ / robot_ 账号 Login 后 EnterGame 这个 player_id。也可以用能直接指定 player_id 的 robot 发 EnterGame。

限制:

- 只导入登录加载链路读的两张玩家表;好友 / 公会 / 邮件 / 交易 / 收件箱等其他服务的数据、名字注册表(名字可能显示为空)、配置表都不导入,本地配表版本须与线上一致。
- 不支持 `-player` 换号(blob 内的身份不会跟着改);撞号用 `-replace`。导入写了 `owner_epoch`,本地建角的撞号探测会避开这个号。
- 中途失败(例如 SQL 写完、Redis 没写)时 login 会 fail-closed 拒绝该玩家,带 `-replace` 重跑即可。

## 5. 其他模式

```powershell
# 只读 SQL:-database 可指向同一实例上的其他库(全局库、其他服务的独占库)
go run ./cmd/debug_fetch -db-config D:\secure\db.yaml -mode sql -database mmorpg_trade -sql "SELECT * FROM trade_asset_op WHERE player_id = <id> LIMIT 20"

# 扫描某库里以 player_id / role_id / uid 等列命中的所有表
go run ./cmd/debug_fetch -db-config D:\secure\db.yaml -mode player-db -zone <home_zone> -player <id>
```

- SQL 模式只接受单条 SELECT / SHOW / DESCRIBE / EXPLAIN,并拒绝:任何注释(`/*`、`--`、`#`,注释能拆开关键字绕过检查;字符串里恰好含这些字符也会被拒)、`ANALYZE`(含 `EXPLAIN ANALYZE`,也会误杀 `SHOW ANALYZE STATUS`)、任何 `INTO`、锁定读、`GET_LOCK` / `SLEEP` / `BENCHMARK` / `LOAD_FILE` 等。这是黑名单,根治是给排障建只读账号。
- 其他服务按玩家查时注意列名不是 `player_id`:`mail_message.owner_player_id`、`trade_listing.seller_player_id`、`trade_commerce.seller_player_id/buyer_player_id`、`friend_request.from_player_id/to_player_id`、`transaction_log.from_player/to_player`。资产指令终态行(guild / trade)与收件箱终态条目约 30 天后清理,接到「东西丢了」的反馈要尽快导出。
- `player-db` 模式:2026-10-19 修复了单连接池下游标未关就发下一条查询、必然卡到超时的问题;单表查询失败记在 `errors` 里,不再静默跳过;超过 `-limit` 的表标 `truncated`。
- 旧 playerdb 导出的导入:有 `errors`、被截断、或缺 `truncated` 标记(旧版导出)时一律拒绝;`player_database` / `player_database_1` 带行内围栏,只写行不对齐屏障,本地 login 必拒,所以默认拒绝(`-allow-fenced-tables` 才放行)——本地复现用 player-bundle。数值列按十进制字面量还原,不再经 float64 改号;`-player` 换号时连行内匹配列一起改。旧 player(Redis)导入也加了同样的本机写目标闸门。
- 旧 player / playerdb 导出文件**不记来源实例身份**,导入端无从比对。2026-10-22 起两种模式的每个写目标都要带本地标记(§4 第 4 条,同一张表、同一把键),否则加 `-allow-unverified-target-identity` 逐个打 `WARNING` 放行:playerdb 是落点库;player 是 data_service 的数据 Redis(DevRedis 或 Regions 集群),登记映射时还有 MappingRedis,`RouteStore.Mode=db_first` 时还有全局库(`player_route`)。在此之前这两种模式只有地址闸:线上 TiDB / MySQL 经 port-forward 到 127.0.0.1 时,playerdb 会把线上该玩家的其它表替换成导出时的旧状态(带 `-allow-fenced-tables` 连两张围栏表也写)。

## 6. 要「问题发生那一刻」的状态

Redis 只有 AOF,没有历史归档;MySQL 有每日全量 + binlog。先用包里 `global.snapshots` 的登录 / 登出快照与 `global.transaction_log` 对比;不够再按 [mysql-backup-pitr-runbook.md](mysql-backup-pitr-runbook.md) §4.1 恢复到影子库,§4.2 抽单玩家。

## 7. 2026-10-22 修复(第二轮审计 r2-ds#1 / r2-ds#2 与账号目录疑点)

未编译、未测试,待 Codex 验证。

- **r2-ds#1 身份闸对 TiDB 失效**:旧实现取不到身份就跳过比较,而 TiDB 取不到 `@@server_uuid`,线上 TiDB 经 port-forward 到 127.0.0.1 并混用配置时,`--dry-run=false` 会把线上两张玩家行替换成导出时的旧状态并抬高行内围栏,之后线上存盘全被判 stale_owner。现在每个写目标都要证明自己不是导出来源,三种证明按顺序:两侧身份都取得到且不同 → 目标库有本地标记表 → `-allow-unverified-target-identity`(打 `WARNING`)。都没有就拒绝。(同日评审后收紧:身份比对只对 MySQL 且按角色配对,Redis 只认标记,见本节末「评审补充」。)代码:`go/data_service/cmd/debug_import/target_identity.go`;身份与标记查询:`go/data_service/cmd/debugutil/connect.go`。
  - **TiDB 为什么不取身份而用标记**:TiDB 是多个 tidb-server 挂在负载均衡后面,能取到的值(若有)只标识单个 tidb-server 进程,同一集群换个 Pod 就变,会把同一集群误判成两个实例而放行;能唯一标识整个集群的变量 / 系统表本仓库没有核实,不猜。判断是否 TiDB 用 `tidb_version()`(与 `shared/authoritysql` 同一判据,不受 `server-version` 改写 `VERSION()` 影响),导出与导入两侧都按「取不到」处理。Java 版用 `CLUSTER_INFO` 的 PD 地址作集群身份,两版取舍不同。
  - **只有确认不是 TiDB 才取 `@@server_uuid`**:`tidb_version()` 明确报「函数不存在」(MySQL 1305)才按 MySQL 取 `@@server_uuid`;探测超时、断连、无权限等其它失败分不清是不是 TiDB,一律按身份未知处理(走标记或显式放行)。
  - **标记为什么不进 `deploy/mysql-init`**:K8s 的 `mysql-init-sql` ConfigMap 原样带入那份脚本,集群 MySQL 也会有标记,就证明不了「这是本地」。建标记必须是对本地库的一次有意操作(§4 第 4 条的两句 SQL)。
- **r2-ds#2 重铸代次没看本地痕迹**:重铸代次 E 取 max(线上, 本地 `owner_epoch`, 本地两行围栏, 本地屏障令牌 `pending` / 两表 ACK / `outbox_version`) + 1(行围栏一项 2026-10-21 已有,本次补屏障令牌)。并且本地行围栏或屏障令牌高于本地 `owner_epoch` 时拒绝导入:那说明本地 Redis 丢过状态(FLUSHALL、单删键)而 MySQL / Kafka 没清,Kafka 里可能压着代次更高、在本地 Redis 与 MySQL 都不留痕迹的残留 DBTask,go/db 只按行内围栏裁决,导入的行会被冲掉。处置:清本地环境(Redis、该玩家的两张行一起清,并 `pwsh -File tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`)再导入。用旧 playerdb 带 `-allow-fenced-tables` 写过这两张行的本地库同样会被拒,删掉这两张行即可。
  - 残余:本地从没有过这名玩家的行、而 Kafka 里压着他首次存盘的 DBTask、又 FLUSHALL 过 Redis 时,本地没有任何痕迹可看,仍可能被冲掉;按 AGENTS §6.2 压测 / 复现前把 Redis、MySQL、Kafka 位点一起清。
- **账号目录疑点(`debugutil/bundle.go` 的 `AccountRedisKeySpec`)不成立**:`account:{account}` 只由 `debug_fetch` 只读导出(password 脱敏、无原字节),`debug_import` 的 bundle / player / playerdb 三种模式都不读它、不写它,也不写 `player_to_account`。角色归属写的是全局 SQL 目录 `AccountDirectoryStore.ImportRole`,与 login 建角(`Create`)走同一个 `createRole`:同一事务里先 `SELECT … FROM account_directory … FOR UPDATE` 锁住账号行,再按 `player_id` 插入 / 更新**本角色这一行**、在锁内复核角色上限,不整块覆盖账号目录,并发新建的角色不会被盖掉。§4 第 3 条 ③ 原写「写 `player_to_account`」、「登录」一段原写「按 `player_to_account` 自愈」,都早于 2026-10-21 的 SQL 账号目录改动,已就地改正。导出包里这把键的用途说明原写「login 权威记录」,已改为「旧账号目录缓存,只读查看」。
- **评审补充(同日,未编译、未测试,待 Codex 验证)**:
  - **Redis 写目标改为必须有本地标记**:原实现把「`run_id` 与来源不同」当成证明,和不信 TiDB 单进程身份是同一个理由,而且线上更容易触发——生产 Redis 是 HA 形态(`deploy/k8s/manifests/infra/redis-ha.yaml`,`svc/redis` 经 haproxy 指向当前主,Pod 滚动时主动切主),单实例重启也会换 `run_id`。导出之后切一次主,核心 Redis 配置仍指向线上 port-forward、MySQL 目标又确是本地并带 `-replace` 时,第 1 段会在线上删掉该玩家的父快照(含未落库的 write-behind 状态)、子表缓存、资产提交证据与交接标记,把屏障改成 pending、owner_epoch 写成 E,之后线上该玩家冷加载 fail-closed、进不来。现在 `run_id` 只用来拒绝「相同」(带标记、加放行都越不过),「不同」不算证明,Redis 目标要 `debug_import:local_target` 键(值 `local-debug-import-target`,§4 第 4 条)或 `-allow-unverified-target-identity`。代码:`debugutil/connect.go` 的 `HasLocalRedisMarker`、`debug_import/target_identity.go` 的 `unverifiedReason`。
  - **MySQL 身份比对按角色配对**:落点库目标只认导出时读过的落点库来源,全局库目标只认全局库来源;对应角色的来源导出时没读(`-skip-global`、落点畸形没读落点库)就判无从比对,要标记或放行。原实现按口径比较,导出包一个 MySQL 来源都没有时落点库目标凭自己的 `server_uuid` 就放行。
  - **旧 player / playerdb 模式补上同一道闸**(见 §5 末条):旧导出文件不记来源身份,每个写目标都只认标记或显式放行。
  - **`commitLocalRedis` 去掉没用的参数**:原先把 `player_to_account` 键与 `-local-account` 作为 KEYS[2] / ARGV[2] 传进 Lua,脚本并不使用,容易让人以为这里写了反向索引;现在只传屏障键。
  - **接线测试**:`debug_import/target_wiring_test.go` 用只认身份探测与标记查询的假库加 miniredis,覆盖 `checkBundleWriteTargets`(TiDB 无标记拒绝、Redis 无标记拒绝、MySQL 身份等于来源拒绝)与旧 player 模式的目标收集;`inspectLocalPlayer` 的 Redis 段抽成 `readLocalRedisState`,`TestReadLocalRedisStateFeedsBarrierEpochIntoAdmit` 用 miniredis 断言屏障 `outbox_version` 高于 owner 时准入拒绝。
  - 残余:`runBundleImport` 本身(配置装载、连接、三段写入的串联)没有单测,把 `reforgedEpoch` 的调用改回内联公式不会让单测变红,只能靠端到端冒烟与评审;Redis 标记被 `FLUSHALL` 清掉后导入会被拒,按拒绝信息重建即可(fail-closed,不会误写)。
