# 线上玩家数据拉取与本地复现

2026-10-30 按 `player_store` 重写。工具在 `go/data_service/cmd/debug_fetch`(只读导出)和 `go/data_service/cmd/debug_import`(写进本地角色),包格式和判定在 `go/data_service/internal/playerbundle`。没有 ps1 包装器。

未编译、未测试,待 Codex 验证。

## 1. 玩家数据在哪

| 位置 | 内容 | 新鲜度 |
|---|---|---|
| scene 进程内存(ECS) | 在线玩家的真实最新态 | 只能等玩家下线、最终存盘后再拉 |
| 落点库 `player_store` | 写权行(kind 0:代次、序号、世代、持有者)和 5 个数据行(kHot、kAsset、kGrowth、kProfile、kLegacy1) | 周期存盘最多落后内存约 300 秒;资产操作和关键事件立存 |
| 全局库 `player_route` | 归属区、落点、冻结状态 | 强一致,没有缓存副本 |
| 全局库 `account_role` | 角色归属的账号 | 强一致 |
| 全局库 `player_snapshot` | 登录、登出时拍的快照 | 历史对比用 |
| 全局库 `transaction_log` | 货币、物品流水(按 `from_player`、`to_player`) | 「东西丢了」的主要凭证;投递失败不阻塞玩法,可能有缺口 |

玩家存档不在 Redis,没有 Redis 键要导出。

## 2. 权限与数据保护

- 连线上(port-forward、取口令)由运维本人操作;AI 不登录 k8s,只处理已经拉到本地的文件(AGENTS §9)。
- 导出包含完整存档和账号名。文件权限 0600,不要放进仓库或工单附件,用完删除。
- `debug_fetch` 用 `authoritysql` 的 Reader 权限打开连接,只发固定的 SELECT。
- DSN 只从环境变量取(`-dsn-env` 给变量名),工具不打印,解析失败的错误也不带原文。

## 3. 导出

前置:**检出线上镜像对应的 commit**。给人看的解码用本地检出的 proto,字段对不上时会提示「本地检出比线上旧」;原字节不受影响。

```powershell
# 运维在另一个终端保持端口转发,DSN 不带库名
$env:DBG_TIDB_DSN = 'user:password@tcp(127.0.0.1:14000)/'
Set-Location go/data_service
go run ./cmd/debug_fetch -dsn-env DBG_TIDB_DSN -player <player_id> -reason "<工单或反馈编号>" -server-revision <线上镜像 sha> -out D:\secure\player-<player_id>.json
```

| 参数 | 说明 |
|---|---|
| `-global-db` | 全局库名,默认 `mmorpg_global` |
| `-limit` | 快照和流水最多导多少行,默认 20 |
| `-timeout` | 总超时,默认 1 分钟 |
| `-skip-global` | 只导路由和存档 |

后端档位由环境变量 `MMORPG_SQL_BACKEND` 决定,默认 `tidb`,MySQL 部署设 `mysql`。

读包的顺序:

1. `analysis`:`online` 为 true 说明写权行有持有者且没有释放,玩家在线或者上次没有正常释放,导出的不是最终态,等玩家下线后重导。`frozen` 为 true 说明路由被冻结。`warnings` 列出其余要注意的事。
2. `route`:归属区、落点、存档所在的库。
3. `rows`:每行的 `payload` 是原字节(base64),`decoded` 是解码结果。
4. `global`:`account`、`snapshots`、`transaction_log`。读不到只记在 `global.errors`,不影响存档部分。

## 4. 导入本地复现

导入**把存档盖到本地已有的一个角色上**,不改路由和账号目录,所以不用处理撞号和归属。

前置:

1. 本地检出与包里的 `server_revision` 一致并已生成代码。包的世代比本地新时会拒绝。
2. 用本地客户端或机器人建一个角色,下线,记下它的 player_id。角色在线时会拒绝。
3. 在**本地**落点库建一次标记。只在确认是本地隔离环境的库上执行;不要放进任何部署脚本或初始化清单,否则线上库也会带上,证明不了「这是本地」。port-forward 到线上时地址同样是 127.0.0.1,所以只看地址拦不住,标记才是判据。

```sql
CREATE TABLE IF NOT EXISTS debug_import_local_target (marker VARCHAR(64) NOT NULL PRIMARY KEY);
INSERT IGNORE INTO debug_import_local_target (marker) VALUES ('local-debug-import-target');
```

```powershell
$env:DBG_LOCAL_DSN = 'root:@tcp(127.0.0.1:4000)/'
Set-Location go/data_service
# 先预览,默认不写
go run ./cmd/debug_import -dsn-env DBG_LOCAL_DSN -file D:\secure\player-<player_id>.json -local-player <本地角色>
# 确认后执行
go run ./cmd/debug_import -dsn-env DBG_LOCAL_DSN -file D:\secure\player-<player_id>.json -local-player <本地角色> -dry-run=false
```

本地服务名(例如 docker 里的 `mysql`)要显式放行:`-allow-target-host=mysql`。

导入在一个事务里完成:锁写权行,删掉目标角色的全部数据行,插入包里的数据行,把写权行的序号和世代抬到两边的较大值。代次和持有者不动,它们属于本地。

限制:

- 只导存档。好友、公会、邮件、交易、收件箱等其他服务的数据不导;角色名以全局库 `player_name` 为准,显示可能和存档里的副本不同。
- 存档里的物品 ID 是线上发的,和本地发号器理论上可能重复,复现问题时可以忽略,不要拿导入后的本地库做资产验证。
- 不写 `player_snapshot` 和 `transaction_log`。

## 5. 要「问题发生那一刻」的状态

先用包里的 `global.snapshots` 和 `global.transaction_log` 对比。不够再由运维从 TiDB 的 BR 备份和 `tidb_snapshot` 历史读取,MySQL 档用每日全量加 binlog。

## 6. 其他服务按玩家查

其他服务的表直接用只读账号查,注意列名不是 `player_id`:`mail_message.owner_player_id`、`trade_listing.seller_player_id`、`trade_commerce.seller_player_id` 和 `buyer_player_id`。查完不要把结果写进仓库。
