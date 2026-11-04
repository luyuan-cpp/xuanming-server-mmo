# Golang 后端面试详细问答

整理日期：2026-11-03。围绕 Go、MySQL、Redis、高并发、分布式消息与系统架构，按“面试口述 → 原理 → 落地方案 → 失败边界”组织。金额示例使用整数分；数据库示例默认采用 MySQL InnoDB，普通查询默认处于常见的 RC / RR 隔离级别。

项目经历应使用自己实际参与的设计、故障与验证数据。架构回答中的通用方案是讲述框架；本仓已有的历史压测案例见 [MMORPG 项目压测容量与瓶颈优化面试回答](mmorpg-stress-interview-qa.md)，各案例的数字与限制以该文档为准。

## 面试官重点考察什么

| 能力 | 回答时需要体现的内容 |
|---|---|
| Go 技术能力 | channel、取消、goroutine 生命周期、内存与性能诊断 |
| 数据库基础 | 索引、事务、隔离级别、锁范围与死锁 |
| 高并发与一致性 | 丢失更新、提交点校验、幂等与并发状态迁移 |
| Redis | 数据结构、到期任务、Big Key、容量和延迟 |
| 高并发系统 | 瓶颈证据、背压、有界并发和负载验证 |
| 分布式系统 | 消息持久性、顺序、重试、任务交接与恢复 |
| 架构设计 | 事实源、原子范围、服务边界、失败恢复与可观测性 |
| 实际问题解决 | 从现象到证据、定位、方案、验证的完整过程 |
| 沟通表达 | 先回答结论，再展开机制、选择理由和代价 |

面试中可以先用 30—60 秒说明核心判断，再根据追问展开。不要仅罗列组件名称，要能回答“这个步骤成功之后，进程马上崩溃会怎样”。

## Q1 余额 5 元，两次并发购买 3 元商品，为什么会出现两种错误

### 可以直接说

> 余额变成 -1，是余额检查与扣款分离，导致超额扣款；余额变成 +2，是两个请求都基于旧余额计算新值，后一次写入覆盖了前一次扣减效果。两种结果对应不同的错误执行逻辑。

### 结果 A 余额变成 -100 分

程序先查询和判断，之后无条件执行减法：

```sql
UPDATE account
SET balance_cent = balance_cent - 300
WHERE id = ?;
```

| 步骤 | 请求 A | 请求 B |
|---|---|---|
| 1 | 查询到 500 分 | |
| 2 | | 查询到 500 分 |
| 3 | 判断余额足够 | 判断余额足够 |
| 4 | 扣减 300 分，剩余 200 分 | |
| 5 | | 再扣减 300 分，剩余 -100 分 |

数据库可以用行锁把两次 UPDATE 串行执行，但第二次 UPDATE 没有重新检查余额，因此仍然执行。这里没有丢失更新，缺的是提交点的余额条件。

负余额结果假设字段允许负数，也没有有效的非负约束拒绝更新。如果字段为无符号类型或存在相关约束，第二次更新可能直接报错。

### 结果 B 余额变成 200 分

两个请求都在应用内计算新余额，然后把结果写回：

```sql
UPDATE account
SET balance_cent = 200
WHERE id = ?;
```

| 步骤 | 请求 A | 请求 B |
|---|---|---|
| 1 | 查询到 500 分 | |
| 2 | | 查询到 500 分 |
| 3 | 算出 500 - 300 = 200 | 算出 500 - 300 = 200 |
| 4 | 写入 200 分 | |
| 5 | | 再写入 200 分 |

两个商品都买到了，但余额只体现一次扣款，属于丢失更新。行锁不能阻止应用把过期的计算结果写回来；单纯增加 BEGIN / COMMIT 也不能修复这段逻辑。

## Q2 数据库层面怎样解决并发扣款

### 方案一 条件原子更新

```sql
UPDATE account
SET balance_cent = balance_cent - 300
WHERE id = ?
  AND balance_cent >= 300;
```

把检查和扣减放进同一条 SQL，在真正修改数据时裁决余额是否充足。只有成功更新 1 行才能继续本次购买；更新 0 行表示条件没有匹配，不能将购买当作成功。

两个请求竞争同一账户时，后执行的更新会面对已经变成 200 分的余额，不再满足 `>= 300`。必须先校验扣减金额大于零，金额用整数分，避免浮点金额误差。

完整购买通常需要本地事务：

```text
BEGIN
登记请求幂等键
条件扣减余额
更新订单状态
写入账务流水
写入需要可靠发布的 Outbox 事件
COMMIT
```

这些修改必须处于同一个本地事务能够覆盖的范围内。任一步失败应正确回滚；跨服务调用无法自然加入这个事务。

幂等键在请求重试时保持不变。遇到重复键，要查原处理结果并核对账户、金额等关键参数，不能把同一个键下的不同请求直接视为成功。

### 方案二 悲观锁

```sql
BEGIN;

SELECT balance_cent
FROM account
WHERE id = ?
FOR UPDATE;

-- 应用检查余额，不足则 ROLLBACK。
-- 足够则扣款，并写入订单与账务流水。

UPDATE account
SET balance_cent = balance_cent - 300
WHERE id = ?;

COMMIT;
```

FOR UPDATE 锁定目标记录，使竞争同一记录的修改等待。适合必须读取多个字段、执行复杂判断后更新的场景。查询、判断和修改要在同一事务内，事务中不要调用外部 RPC。

来源：[MySQL 锁定读](https://dev.mysql.com/doc/refman/8.4/en/innodb-locking-reads.html)。

### 方案三 乐观锁

```sql
UPDATE account
SET balance_cent = balance_cent - 300,
    version = version + 1
WHERE id = ?
  AND version = ?
  AND balance_cent >= 300;
```

版本不匹配或余额不满足时，更新 0 行。冲突后重新读取、重新判断，并在次数和总截止时间内决定是否重试。不能用同一个旧版本无限重试。

| 方案 | 适用场景 | 主要代价与约束 |
|---|---|---|
| 条件原子更新 | 余额、库存等简单条件修改 | 正确处理受影响行数 |
| 悲观锁 | 复杂读改写、竞争较明显 | 锁等待、事务长度与死锁 |
| 乐观锁 | 冲突较少，允许重新计算 | 高竞争下可能大量重试 |

进程内 mutex 不能保护其他实例；Redis 锁也不能替代数据库提交点的条件和事务。并发安全与请求幂等是两件事：即使余额足够，同一请求重试仍可能重复扣款。

## Q3 检查受影响行数到底是什么意思

### 先区分余额和行数

“受影响行数”回答的是本条 SQL 影响了多少条记录，余额回答的是账户还剩多少钱。两者不是同一个数字。

```sql
UPDATE account
SET balance_cent = balance_cent - 300
WHERE id = 123
  AND balance_cent >= 300;
```

假设 id 是主键且初始余额为 500 分：

| 执行者 | 执行前余额 | WHERE 是否满足 | 实际动作 | 受影响行数 | 执行后余额 |
|---|---:|---|---|---:|---:|
| 请求 A | 500 分 | 满足 | 扣减 300 分 | 1 行 | 200 分 |
| 请求 B | 200 分 | 不满足 | 不修改记录 | 0 行 | 200 分 |

请求 B 的 SQL 可以正常执行结束，但没有任何一条记录同时满足两个条件，所以返回 0 行。SQL 没有报错，并不等于扣款成功。

程序必须依次判断：

```text
SQL 执行报错 → 数据库错误，不能继续成功路径
SQL 执行成功，影响 1 行 → 本条扣款语句成功
SQL 执行成功，影响 0 行 → 没有扣款，不能继续成功路径
```

由于 id 是唯一键，预期只能是 0 或 1。如果返回其他数量，要当作不符合约定的结果处理。

0 行还可能是账户不存在。这条 SQL 本身只能裁决“没有匹配到可扣款账户”；如需区分展示文案，可以另外查询，但不能用后续查询代替原子扣款判断。

### Go 中怎样获取

以下片段接收已经开启的事务，只负责执行一次条件扣款。调用者负责完整业务事务、回滚、提交和请求幂等；`context`、`database/sql`、`fmt` 是片段所需导入。

```go
func deductInTx(
    ctx context.Context,
    tx *sql.Tx,
    accountID int64,
    amountCent int64,
) (bool, error) {
    if amountCent <= 0 {
        return false, fmt.Errorf("扣减金额必须大于零")
    }

    result, err := tx.ExecContext(ctx, `
        UPDATE account
        SET balance_cent = balance_cent - ?
        WHERE id = ? AND balance_cent >= ?`,
        amountCent, accountID, amountCent,
    )
    if err != nil {
        return false, fmt.Errorf("执行扣款失败: %w", err)
    }

    affected, err := result.RowsAffected()
    if err != nil {
        return false, fmt.Errorf("读取受影响行数失败: %w", err)
    }

    switch affected {
    case 0:
        return false, nil
    case 1:
        return true, nil
    default:
        return false, fmt.Errorf("扣款影响行数异常: %d", affected)
    }
}
```

这里 `true` 仅表示当前事务中这条语句已经扣减，后续写订单、流水或提交仍可能失败。只有完整事务确认提交后，才能把购买作为成功结果回复；提交结果未知时，使用原幂等键查证，不能另起新请求盲目扣款。

### 进阶追问 匹配行数与实际修改行数

MySQL 驱动的 `clientFoundRows` 配置会影响 UPDATE 返回“匹配记录数”还是“实际改变记录数”。例如把一个本来就是 200 的余额再次设置成 200，两种口径可能不同。

本例保证扣减金额大于零，匹配后就会实际改变余额，所以不依赖“写入相同值”的差异。不能把本例的 0/1 判断不加分析地套到任意 UPDATE。

来源：[Go sql.Result 与 RowsAffected](https://pkg.go.dev/database/sql#Result)、[go-sql-driver/mysql 的 clientFoundRows](https://github.com/go-sql-driver/mysql#clientfoundrows)。

## Q4 事务用来做什么，原子性和一致性怎样解释

### 可以直接说

> 事务把需要共同成立的一组数据库操作组织成一个提交单位。例如扣款成功，订单和账务流水也必须成功，不能只完成其中一部分。

| 性质 | 含义 | 购买示例 |
|---|---|---|
| 原子性 Atomicity | 数据库修改全部提交或全部撤销 | 扣款、订单和流水共同提交 |
| 一致性 Consistency | 提交前后满足定义好的数据与业务约束 | 余额不为负，同一请求不重复扣款 |
| 隔离性 Isolation | 控制并发事务的可见性与干扰 | 通过适当机制保护并发读改写 |
| 持久性 Durability | 已提交结果按配置与故障模型得到保存 | 故障恢复后能够恢复提交结果 |

原子性有范围：数据库回滚不能撤回已经发送的短信，也不能自动撤销外部支付。外部副作用需要幂等、可靠投递、查单、补偿与对账。

一致性也不等于“开事务就自动正确”。数据库不知道余额不能透支、两个账户资金必须守恒等全部业务规则。规则需要通过正确的 SQL、条件更新、并发控制以及唯一约束、外键、CHECK 等机制表达。

显式事务适用于多条语句必须共同成功，或读取结果需要被保护到后续修改完成的情况。单条 InnoDB DML 在 autocommit 下本身也是一个事务。

Go 中开启 sql.Tx 后，相关操作都应通过同一个 Tx 执行；中途使用 db.Exec 可能跑到另一条连接，脱离当前事务。每个错误分支都要正确结束事务，不能笼统认为任意 SQL 失败都会自动撤销整个事务。

来源：[MySQL 事务术语](https://dev.mysql.com/doc/refman/8.4/en/glossary.html)、[Go 官方事务指南](https://go.dev/doc/database/execute-transactions)。

## Q5 数据库有哪些锁，读取余额一定有读锁吗

### 先纠正前提

普通 SELECT 在 InnoDB 常见的 RC、RR 隔离级别下通常是 MVCC 快照读，不加记录共享锁。是否加锁要看语句形式、隔离级别和事务条件。

| 操作 | 典型行为 |
|---|---|
| 普通 SELECT | 通常为非锁定快照读 |
| SELECT ... FOR SHARE | 加共享锁 S |
| SELECT ... FOR UPDATE | 加排他锁 X |
| UPDATE、DELETE | 对相关索引记录加排他锁，范围取决于执行方式 |

普通 SELECT 仍可能持有元数据锁 MDL，阻止并发修改表结构。SERIALIZABLE 等条件下也需要另外分析。

来源：[一致性非锁定读](https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html)、[元数据锁](https://dev.mysql.com/doc/refman/8.4/en/metadata-locking.html)。

### 按不同维度解释锁

- 共享锁 S 与排他锁 X：表示访问模式；多个 S 可以共存，X 与其他事务的 S/X 冲突。
- 记录锁：锁住索引记录。
- 间隙锁：限制向某个索引区间插入。
- 临键锁：记录锁与其前方间隙的组合。
- 表级意向锁 IS、IX：协调表锁与行锁。
- 插入意向锁：插入前表达对间隙的插入意图。
- MDL：保护表结构等元数据。

实际加锁范围取决于 SQL、索引、执行计划和隔离级别。完整唯一键等值命中已有记录时通常只需记录锁，范围或非唯一索引查询可能涉及间隙锁、临键锁。

不要说“用了索引就只锁一行”，也不要把无索引更新描述成自动升级为表锁；它可能扫描并锁定大量记录。乐观锁是版本校验策略，其 UPDATE 仍然需要数据库写入锁机制。

来源：[InnoDB 锁类型](https://dev.mysql.com/doc/refman/8.4/en/innodb-locking.html)、[隔离级别](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html)。

### 索引的关联追问

需要能解释聚簇索引、二级索引、回表、覆盖索引、联合索引字段顺序，以及执行计划如何影响扫描与锁定范围。InnoDB 聚簇索引叶子保存行数据，二级索引记录包含主键值。

例如扫描超时待支付订单，可以评估 `(status, expire_at, id)`，让状态等值过滤与到期时间范围查询相配合；仍要结合真实数据分布和执行计划决定。

来源：[聚簇索引与二级索引](https://dev.mysql.com/doc/refman/8.4/en/innodb-index-types.html)、[联合索引](https://dev.mysql.com/doc/refman/8.4/en/multiple-column-indexes.html)、[EXPLAIN](https://dev.mysql.com/doc/refman/8.4/en/explain.html)。

## Q6 死锁怎么产生，怎样减少

### 可以直接说

> 死锁是事务之间形成循环等待。事务 A 持有账户 1 的锁并等待账户 2，事务 B 持有账户 2 并等待账户 1，双方都无法继续。长时间等待本身不等于死锁。

减少概率的方法：

1. 多资源按统一顺序访问，例如按账户 ID 升序加锁。
2. 缩短事务，事务内不做 RPC 或慢计算。
3. 建立适合条件的索引，减少扫描和锁定范围。
4. 缩小批量修改规模。
5. 明确需要修改时，可直接采用 FOR UPDATE，减少两个事务先持有 S 再同时申请 X 的冲突。
6. 结合业务评估隔离级别；RC 能减少部分间隙锁等争用，但不能消除所有死锁。

InnoDB 通常会选择一个事务回滚。应用应对适合重试的错误重新执行整个事务，重新读取和判断，设置有限次数、退避、抖动和总截止时间，并保持业务幂等。

锁等待超时与死锁不同：默认情况下前者通常只回滚当前语句，不能假设整个事务已经撤销；应用需要正确回滚或结束失败事务。

排查可以使用：

```sql
SHOW ENGINE INNODB STATUS;
```

结合 performance_schema.data_locks、data_lock_waits，以及相关 SQL、索引和事务时长分析。需要时记录全部死锁信息。

来源：[死锁处理](https://dev.mysql.com/doc/refman/8.4/en/innodb-deadlocks-handling.html)、[错误与回滚范围](https://dev.mysql.com/doc/refman/8.4/en/innodb-error-handling.html)。

## Q7 Redis 中有 30 分钟支付超时的订单，怎样发现并处理

### 可以直接说

> 我会把订单状态、缓存过期和任务触发分开。订单保留状态与到期时间，调度器发现到期任务，处理时重新检查当前状态，再通过条件状态迁移裁决支付和关闭的竞争。

先确认到期动作和时效：催付与关单是两种动作，例如第 25 分钟催付、第 30 分钟关闭；允许延迟几秒与要求更严格时效，会影响调度频率。

### 基础方案 ZSET 到期索引

```text
key：orders:timeout:{分片号}
member：订单 ID
score：到期时间戳
```

调度器周期性执行有界查询：

```text
ZRANGE orders:timeout:{0} -inf <当前时间戳> BYSCORE LIMIT 0 100
```

Redis 6.2 起可用 ZRANGE BYSCORE 按分数范围读取。轮询间隔会影响发现延迟，不应承诺恰好在到期瞬间执行。

来源：[ZRANGE](https://redis.io/docs/latest/commands/zrange/)。

### 多调度器与领取后崩溃

不能“查到任务 → 永久删除 → 执行业务”，否则删除后崩溃就丢任务。

```text
待处理 due
→ 原子领取，附带领取令牌与租约
→ 处理中 processing
→ 成功后确认；失败退避；租约到期后重新投递
```

可以用有界 Lua 脚本原子地把到期任务从 due 移到 processing。ACK 要校验领取令牌，防止旧 worker 删除新 worker 的任务；Redis Cluster 脚本涉及的 key 必须同槽。

也可以把到期任务放入 Redis Stream，通过消费组、待确认记录和 XAUTOCLAIM 接管失联消费者的任务。业务仍然要幂等，因为租约、重试和网络超时都可能带来重复执行。

来源：[XAUTOCLAIM](https://redis.io/docs/latest/commands/xautoclaim/)。

### 支付与超时同时发生

关单在持久订单记录上条件更新：

```sql
UPDATE orders
SET status = 'CLOSED'
WHERE id = ?
  AND status = 'PENDING'
  AND expire_at <= CURRENT_TIMESTAMP;
```

更新 1 行才代表本次关单成功；支付也必须按合法状态迁移。外部支付平台已经扣款而本地订单关闭的情况，需要查单、对账和退款，不能只靠本地 CAS 完结。

催付同样要重新确认订单未支付，并使用“订单 ID + 动作类型 + 业务版本”等稳定幂等键。通知渠道的最终副作用是否可去重，还取决于渠道接口能力。

### 任务遗漏与持久化兜底

支付订单通常需要持久存储作为事实源。如果订单只存 Redis，必须明确持久化、故障恢复和可接受的数据丢失边界。

建单成功但写 Redis 任务失败是跨存储双写问题，可以用订单与 Outbox 同事务提交，再可靠投递；另用 `(status, expire_at, id)` 等合适索引分批扫描超时待处理记录补漏。

按订单 hash 分片 ZSET，控制单集合增长。监控待处理数量、最老任务迟到时间、重试次数、租约回收和补漏数量。

### 为什么不只监听 key 过期

Redis 过期事件不保证 TTL 归零时立即发生；Pub/Sub 断线期间的消息会丢失，无法像可靠任务队列一样确认和重放。收到通知时，订单缓存也可能已经删除；Cluster 中还要考虑逐节点订阅。

它可以作为允许遗漏的提示或补充触发，但不能独立承担可靠关单。

来源：[Redis 键空间与过期通知](https://redis.io/docs/latest/develop/pubsub/keyspace-notifications/)。

## Q8 什么是 Big Key，怎样发现和治理

### 可以直接说

> Big Key 没有跨业务统一的固定阈值。我看单 key 内存、集合元素数量、命令复杂度和响应字节数，判断它是否影响延迟、带宽、迁移与内存预算。Hot Key 则看访问频率，小 key 也可能很热。

例如团队可以把 String 100KB、集合一万个元素作为初始告警线，再根据延迟预算调整；这不是 Redis 官方标准。

大 key 的影响包括命令执行延迟、大返回包、客户端解析与内存压力、主从同步及迁移成本，以及删除或回收内存的开销。

### 发现方法

```text
redis-cli --bigkeys -i 0.1
redis-cli --memkeys -i 0.1
redis-cli --keystats -i 0.1
MEMORY USAGE <key>
```

bigkeys 主要关注 String 长度和集合元素数量，memkeys 关注内存，较新客户端的 keystats 提供综合统计。先检查本机 redis-cli --help，不能假设旧客户端都有这些选项。

渐进扫描仍有成本，需要限速、避开高峰，并覆盖所有主节点。MEMORY USAGE 包含管理开销；嵌套结构可能采样估算，SAMPLES 0 的全量遍历不宜贸然用于超大集合。

来源：[redis-cli](https://redis.io/docs/latest/develop/tools/cli/)、[MEMORY USAGE](https://redis.io/docs/latest/commands/memory-usage/)。

### 治理方法

- 按访问粒度拆分，按时间或 hash 分桶。
- 给集合设置增长与保留上限。
- 按字段、分页读取，避免 HGETALL 或全量 LRANGE 等大返回。
- 大 String 可评估压缩，同时衡量 CPU、延迟和局部更新能力。
- 大集合删除优先考虑 UNLINK，把主要回收工作放在后台。
- 在写入端监控大小分布和超限次数，结合慢日志、命令延迟与出流量定位。

UNLINK 并非零成本，内存回收仍需资源和时间，也解决不了读取时的大返回包。拆分后要重新评估多 key 原子性和 Cluster 跨槽问题。

来源：[UNLINK](https://redis.io/docs/latest/commands/unlink/)。

## Q9 Go 有缓冲与无缓冲 channel 有什么区别

### 可以直接说

> 无缓冲 channel 是通信双方的同步交接；有缓冲 channel 允许发送方暂时领先接收方，但缓冲满后仍然阻塞。缓冲只能吸收短时突发，无法解决持续的生产速度大于消费速度。

| 情况 | 发送 | 接收 |
|---|---|---|
| 无缓冲 | 等待接收方就绪 | 等待发送方就绪 |
| 有缓冲 | 缓冲满时等待 | 缓冲空且未关闭时等待 |
| nil channel | 永久阻塞 | 永久阻塞 |
| 已关闭 channel | panic | 先取完缓冲，再返回零值及 ok=false |

如果生产每秒 1000 条、消费每秒 500 条，持续运行时任何有限缓冲最终都会满。还需要限制入口并发、队列长度与等待时间，并明确超载策略。

### 高频追问

- 发送成功不表示消费者已经完成业务；有缓冲时甚至不表示数据已被取走。
- channel 本身可并发访问，但发送指针后双方继续修改对象，仍可能数据竞争。
- 多 goroutine 并发发送没有预先确定的业务顺序。
- 重复关闭和关闭 nil channel 都会 panic。
- 由明确拥有关闭职责的发送方关闭，多发送方需协调后统一关闭。
- close 表示以后没有值，不能强制停止正在运行的业务。
- len(ch) 只是瞬时观察，不能用先检查再发送保证不阻塞。
- 无限制创建 goroutine 等待有界 channel，仍会把积压转移到内存中。

阻塞发送要响应取消，需要显式选择取消分支：

```go
select {
case jobs <- job:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

创建 context 本身不会自动打断普通发送或任意 I/O。channel 的同步关系由内存模型约定，也不等于业务处理已经完成。

来源：[Go 语言规范](https://go.dev/ref/spec#Channel_types)、[Go 内存模型](https://go.dev/ref/mem#Channel_communication)、[流水线与取消](https://go.dev/blog/pipelines)。

## Q10 怎样排查 Go 内存与 goroutine 泄漏

### 可以直接说

> 先看资源指标与业务负载的关系，再比较多个时刻的 profile，定位持续增长的分配点或阻塞栈，沿对象引用和退出路径找原因，最后在相同负载下验证资源是否回到稳定区间。

### 内存排查

Go 有 GC，但无用对象只要仍被引用，就不会被回收。常见原因是无界 map/cache、堆积的队列、小切片保留大底层数组，以及闭包或长期 goroutine 持有对象。

| 现象 | 解释与排查方向 |
|---|---|
| 累计分配持续增长 | 可能只是程序一直运行，不能单独证明泄漏 |
| 分配频繁但存活堆稳定 | 重点看分配抖动和 GC 成本 |
| GC 后存活堆持续增长 | 检查引用滞留与无界业务状态 |
| RSS 增长但 Go 堆稳定 | 继续检查 goroutine 栈、运行时、cgo、mmap 等 |

在可比负载与 GC 阶段采集多个 heap profile。inuse_space / inuse_objects 用于观察存活量，alloc_space / alloc_objects 是累计分配，包含已经被回收的对象。

流量回落后不释放也需要结合缓存策略和业务生命周期判断；一次快照无法独立证明泄漏。

来源：[runtime/pprof](https://pkg.go.dev/runtime/pprof)。

### goroutine 排查

重点看本应结束的任务为何没有退出。常见阻塞点包括无人消费的发送、永不结束的接收、缺少超时的网络调用、锁、连接池，以及未传递或未处理的取消信号。

1. 对照请求并发观察 goroutine 数量趋势。
2. 停止流量并排空请求，观察是否回落。
3. 抓多个时刻的 goroutine 栈，找持续增加的同类调用栈。
4. 沿路径检查超时、取消、关闭职责、I/O deadline 和清理。
5. 修复后验证稳定区间，并在测试中检查意外残留任务。

一次看到大量 goroutine 不一定是泄漏。测试可以使用 [Uber goleak](https://github.com/uber-go/goleak)，并正确区分框架常驻 goroutine 与本次任务。

## Q11 怎样使用 pprof，怎样做性能监控

### 监控与诊断各自解决什么

持续监控发现异常，profile 与 trace 解释原因。监控应覆盖请求量、错误率、P95/P99、CPU、RSS、存活堆、分配速率、GC、goroutine，以及数据库连接等待、Redis 延迟、Kafka lag 和业务成功率。

| 工具 | 主要回答的问题 |
|---|---|
| CPU profile | CPU 时间花在哪些调用路径 |
| heap profile | 哪些分配点留下较多存活对象 |
| allocs profile | 哪些位置产生大量分配 |
| goroutine profile | goroutine 在哪里，为什么阻塞 |
| block profile | 哪些同步操作造成等待 |
| mutex profile | 哪些锁竞争明显 |
| execution trace | 调度、GC、阻塞与并行度随时间怎样变化 |

服务已接入 net/http/pprof 时，可以使用以下 Windows 命令。两个堆快照应在可比较的负载与 GC 阶段采集，不能连续抓取后就认为有意义。

```powershell
# 采集 30 秒 CPU profile 并打开分析页面。
go tool pprof -http=127.0.0.1:8081 "http://127.0.0.1:6060/debug/pprof/profile?seconds=30"

# 在不同的可比较时刻采集堆快照。
curl.exe -o heap-before.pb.gz "http://127.0.0.1:6060/debug/pprof/heap"
curl.exe -o heap-after.pb.gz "http://127.0.0.1:6060/debug/pprof/heap"
go tool pprof -inuse_space -base heap-before.pb.gz heap-after.pb.gz

# 查看 goroutine 调用栈。
curl.exe "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2"

# 采集并分析调度追踪。
curl.exe -o trace.out "http://127.0.0.1:6060/debug/pprof/trace?seconds=5"
go tool trace trace.out
```

block 与 mutex profile 通常需要通过 runtime.SetBlockProfileRate、runtime.SetMutexProfileFraction 主动开启。采样有成本，要限定强度和持续时间。诊断端口应限制在管理访问范围。

CPU 不高但接口慢，可能是在等数据库、网络或锁；CPU profile 无法替代阻塞分析。pprof 中 flat 关注函数自身，cum 包含它调用的其他函数。

面试讲优化经历要给出证据链：异常现象 → 指标缩小范围 → profile 定位 → 改动原因 → 同口径验证 → 新代价。不要只说“加缓存后性能好了”。

来源：[net/http/pprof](https://pkg.go.dev/net/http/pprof)、[pprof 工具](https://github.com/google/pprof)。

## Q12 怎样讲一个架构案例，并回答顺序、限流、重试与熔断

### 选择自己真正参与的案例

可以选择登录排队削峰、玩家资产更新与异步存盘、业务提交与消息通知一致性，或下游故障引起的积压。先选一个问题讲完整，再展示系统全貌。

讲述顺序：业务场景与约束 → 旧设计 → 实际痛点与证据 → 方案及取舍 → 故障恢复 → 验证结果。容量、延迟和故障数字使用真实证据，不把目标设计当成历史成绩。

本仓压测案例可以从 [现有面试回答](mmorpg-stress-interview-qa.md) 选择：扩大连接池后仍受串行消费限制、回执接收循环同步 LPOP 导致排队、有界 worker，以及场景释放异步化。历史指标的分母和观测窗口不可省略。

### 通用案例 业务落库与通知解耦

旧流程为“修改数据库 → 调通知服务 → 返回”，会遇到下游慢拖累请求、数据库成功而通知失败、响应丢失导致重复，以及并发通知乱序。

可以设计为：

```text
本地事务：业务记录 + Outbox
→ 发布器或 CDC
→ Kafka，按业务实体分区
→ 消费端幂等处理
→ 通知渠道
```

Outbox 把业务结果与待发布事件一起提交；投递允许重试，所以消费者仍需幂等。业务事务里不等待通知渠道。对外短信、支付等最终副作用，还取决于目标接口是否支持幂等，不能仅用本地消费记录覆盖网络另一端。

来源：[Debezium Outbox Event Router](https://debezium.io/documentation/reference/stable/transformations/outbox-event-router.html)。

### 通知顺序怎么处理

先确认是全局顺序还是同一用户、订单内顺序。通常同实体使用同 key 进入稳定分区，由源头写者或业务版本建立顺序，分区内串行、分区间并行；更细并行可以按实体分组串行。

Kafka 顺序读取不等于 goroutine 顺序完成。版本号可以帮助识别重复、过期和缺失事件；跳过失败事件或独立重试 topic 可能破坏严格顺序，需要明确阻塞或补偿策略。

业务事务生成版本号，也不自动保证多个 Outbox 发布器按版本先后发送。发布端需要按实体串行投递，或由消费端明确处理缺号、等待、补拉与重排；版本号能发现问题，不能自动修复顺序。

消息处理顺序也不等于用户看到的顺序，外部推送、短信等渠道可能再次排序。

### 流量怎么控制

| 控制 | 回答的问题 | 常见实现 |
|---|---|---|
| 速率 | 每秒放多少请求 | 令牌桶等 |
| 并发 | 同时处理多少任务 | 固定 worker、信号量 |
| 队列 | 最多排多少任务 | 有界队列、等待期限 |

根据下游容量、延迟和错误反馈调整放行。队列满时明确拒绝、等待或降级，不把积压无限转移到 goroutine 或内存。单个极热实体仍受串行处理上限限制。

来源：[Google SRE 过载处理](https://sre.google/sre-book/handling-overload/)。

### 重试、熔断和恢复

重试只针对可重试且幂等的操作，有连接与请求超时、总截止时间、次数限制、退避和随机抖动。避免调用链每层都重试，造成流量乘法放大。

超时表示调用方未及时拿到结果，不证明下游没执行。资金等场景用原业务键查证或幂等重试，不能另起一笔。

熔断的典型状态是：关闭时正常放行并统计失败；打开时快速失败；半开时少量探测，成功后恢复，失败则继续打开。

恢复后逐步放量，限制积压重放速率，避免故障刚恢复就再次打满下游。需要监控熔断状态、重试量、队列年龄及恢复耗时。

来源：[AWS 超时、重试与抖动](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/)、[微软熔断模式](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker)。

## Q13 转盘活动怎样按权重随机奖品

| 奖品 | 权重 | 随机整数区间 |
|---|---:|---|
| A | 10 | [0, 10) |
| B | 30 | [10, 40) |
| C | 60 | [40, 100) |

总权重为 100，在 [0,100) 均匀生成整数，每个奖品占据的整数数量等于其权重，所以中奖概率是权重除以总权重。

通用算法是先计算前缀和，再找到第一个严格大于随机整数的前缀和：

```text
权重：   10   30   60
前缀和： 10   40  100
```

预处理 O(n)；顺序查找 O(n)，二分查找每次 O(log n)。权重较稳定、抽样次数极多时才进一步评估其他抽样结构，不必一开始增加复杂度。

Go 核心片段中，rand 指 crypto/rand，big 指 math/big；prefix 为校验过的前缀和，totalWeight 为正的 int64 总权重：

```go
r, err := rand.Int(rand.Reader, big.NewInt(totalWeight))
if err != nil {
    return 0, err
}

index := sort.Search(len(prefix), func(i int) bool {
    return prefix[i] > r.Int64()
})
```

价值奖品要评估随机数不可预测性。实现还需验证权重非负、总权重大于零且求和不溢出；同一次抽奖应幂等，有限库存要原子扣减。库存不足是否重抽必须明确，因为重抽会改变实际中奖分布。

来源：[Go crypto/rand](https://pkg.go.dev/crypto/rand#Int)。

## Q14 Kafka 怎样同时考虑不丢、顺序与 Exactly Once

### 开场先说明边界

> 先确认顺序是全局还是同一实体内顺序，Exactly Once 是 Kafka 输出效果，还是 MySQL 扣款等外部副作用。Kafka 的分区顺序与原生事务有明确范围，不能自动覆盖整个业务系统。

### 从业务源头保证可恢复投递

业务成功后，消息只在内存中就可能因进程崩溃而丢失。业务与 Outbox 同事务提交，由发布器检查最终发送结果，失败或结果未知时保留待投递记录，使用稳定事件 ID。

### Kafka 内部的持久性

常见组合是 topic 副本数 3、min.insync.replicas=2，producer 使用 acks=all、enable.idempotence=true，并合理分布故障域、避免落后副本的非正常选举。

acks=all 等待当前全部 ISR 确认；min.insync.replicas 是允许成功写入的最低 ISR 数量，不是只等待该数量的副本。

副本不足时应拒绝成功写入，并保留来源待投递数据。不能在任意网络分区和任意故障下，同时承诺无限可用与绝对不丢。

还要检查异步回调、最终错误、投递超时、重试和结果未知。生产者幂等要求 acks=all、retries>0、max.in.flight.requests.per.connection<=5 等配置相容；合法在途上限内可以保证顺序，不需要一概设成 1。Go 客户端具体配置名称按所用库核对。

幂等 producer 解决协议级重试重复，不能识别应用再次调用发送产生的业务重复。监控 ISR、磁盘、发送错误、最老 Outbox 与消费 lag；保留期覆盖允许的恢复窗口。

来源：[Producer 配置](https://kafka.apache.org/41/configuration/producer-configs/)、[Topic 配置](https://kafka.apache.org/41/configuration/topic-configs/)。

### 生产顺序与消费顺序

- 同实体用相同 key，并保持稳定分区映射。
- 多 producer 并发写同实体时，源头写者、版本或序号建立业务顺序。
- 版本号不能自动约束多个 Outbox 发布器，发布端需要按实体有序投递，或由消费端明确等待、补拉与重排策略。
- 分区内串行、分区间并行是较简单的基础方案。
- 并发处理时，只能提交连续完成范围的 offset，不能越过未完成事件。
- 严格顺序下，失败事件不能随意跳过，代价可能是队头阻塞。
- rebalance 时停止被撤销分区的新工作，协调在途任务与旧属主，不能假设旧任务自动消失。
- 扩分区可能改变 key 的分区映射，需要迁移与排空策略。
- 全局总序通常需要单分区或额外排序机制，限制吞吐和扩展。

### Kafka 输入处理后再输出到 Kafka

Kafka 原生事务可以协调输出记录与输入消费位置：

```text
使用稳定且正确独占的 transactional.id
关闭消费者自动提交
开始事务
处理输入并生产输出
将输入的下一消费位置加入同一事务
提交；失败则中止并恢复输入位置
```

下游使用 isolation.level=read_committed，避免读取中止事务记录。生产者初始化、fencing、组变化和事务超时都要按客户端契约处理。

处理代码可能执行多次，承诺的是最终可见效果。read_committed 不负责业务去重。写入 MySQL、调用支付或短信仍需要目标端参与的一致性机制。

来源：[Kafka 设计与投递语义](https://kafka.apache.org/41/design/design/)、[Consumer 配置](https://kafka.apache.org/41/configuration/consumer-configs/)。

## Q15 消费者怎样幂等，大流量能用布隆过滤器吗

### 唯一约束与业务修改同事务提交

```sql
CREATE TABLE consumed_event (
    consumer_name VARCHAR(64) NOT NULL,
    event_id VARCHAR(64) NOT NULL,
    PRIMARY KEY (consumer_name, event_id)
);
```

```text
BEGIN
插入 consumed_event
执行业务更新
COMMIT
提交 Kafka offset
```

| 崩溃位置 | 恢复行为 |
|---|---|
| 数据库提交前 | 回滚后重投，重新处理 |
| 数据库提交后、offset 提交前 | 重投命中唯一约束，不重复修改业务 |
| offset 提交后 | 业务已经持久化 |

如果先提交 offset 再写数据库，可能丢业务。去重记录与业务修改分两次提交也存在崩溃窗口，必须共同提交。

唯一键冲突只有在确认是本业务事件重复时才能跳过；其他数据库错误不能吞掉。事件 ID 应稳定，重复事件也要核对必要的业务参数。只用 topic / partition / offset 无法识别源端重发到不同 offset 的同一业务事件。

Redis SETNX 成功后再改数据库也有窗口：标记成功后崩溃，数据库未修改，重投却被错误判定已完成。已处理标记必须能够证明业务已经完成。

数据库提交后才能确认消息，提交结果未知时应查证或用原事件幂等重试。外部副作用仍需要目标 API 幂等键，或 Outbox 与后续可靠投递。

### 布隆过滤器的边界

布隆过滤器只能回答“确定不存在”或“可能存在”，有假阳性。如果看到可能存在就丢弃订单事件，会把尚未处理的事件误丢，无法用于严格不能漏的最终去重。

它可以辅助减少部分查找，最终仍由精确记录或业务状态裁决。即使过滤器认为不存在，也要防并发，保留数据库唯一约束。

高流量先评估精简记录和唯一索引、按业务键分片、缩短事务、合理批处理与状态机。归档去重记录要覆盖自动重试、人工补偿和历史回放窗口，不能删除仍可能重放的凭证。

来源：[Redis Bloom Filter](https://redis.io/docs/latest/data-types/probabilistic/bloom-filter/)。

## Q16 官方资料与 GitHub 阅读路线

前面各题的官方链接用于核对具体语义；下面适合动手练习与阅读实现。

| 资料 | 阅读重点 | 对应题目 |
|---|---|---|
| [dgryski/go-perfbook](https://github.com/dgryski/go-perfbook) | profiling、分配、benchmark，含中文版本 | Q10—Q11 |
| [High Performance Go Workshop](https://github.com/davecheney/high-performance-go-workshop) | profiling、benchmarking、execution tracer 练习 | Q11 |
| [Uber Go Style Guide](https://github.com/uber-go/guide) | goroutine 生命周期、并发约定和错误处理 | Q9—Q11 |
| [uber-go/goleak](https://github.com/uber-go/goleak) | 测试中检查未退出的 goroutine | Q10 |
| [google/pprof](https://github.com/google/pprof) | profile 对比、调用图和分析命令 | Q11 |
| [hibiken/asynq](https://github.com/hibiken/asynq) | 延迟调度、重试、超时与 worker 恢复 | Q7、Q12 |
| [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) | 驱动配置、clientFoundRows 与数据库行为 | Q3—Q4 |

阅读 asynq 时带着问题看：worker 把任务取走后突然崩溃，任务怎样重新被执行？这能串起领取、租约、恢复、至少一次执行与业务幂等。任务框架的去重选项不能直接替代资金业务的事务幂等。

备考可依次练习：并发扣款与事务 → 锁与死锁 → 可靠超时任务 → Kafka 与幂等 → Go 排障 → 一个有真实证据的架构案例。每题都准备一个失败时序，并解释提交点和恢复动作。
