package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	// 只为识别 1213(isMySQLDeadlock)。驱动本来就在 go.mod 的直接 require 里
	// (internal/svc/servicecontext.go 空白导入注册),这里不新增依赖。
	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	// Redis 句柄统一用 go-zero 的封装(契约 §4):同进程混用 go-zero 与裸 go-redis
	// 会出现两套连接池、两套超时与熔断语义,排障时指标和日志对不上账。
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// ── 哨兵错误与它们在 logic 层的映射(改这里必须同步 logic 的 switch)──────────────
//
// 本层只回"发生了哪件事",**不碰 tip 码**(tip 码要 import internal/constants,那是 logic 的职责)。
// 下面这张表是 logic 层唯一的映射依据。
//
// ⚠ 两个"好友数满"的哨兵按**角色**命名,不是按"我 / 对方":
// Sender = 申请的发起方,Acceptor = 申请的接收方。于是同一个哨兵在两个 RPC 里落到**不同**的
// tip 码上 —— AddFriend 的调用者是 Sender,AcceptFriend 的调用者是 Acceptor:
//
//	哨兵                     AddFriend(me=Sender)         AcceptFriend(me=Acceptor)
//	ErrBlocked               ErrBlocked                   ErrBlocked
//	ErrAlreadyFriends        ErrAlreadyFriends            —
//	ErrRequestAlreadySent    ErrRequestAlreadySent        —
//	ErrTooManyPending        ErrTooManyPending            —
//	ErrTargetInboxFull       ErrTargetInboxFull           —
//	ErrSenderFriendsFull     ErrFriendListFull(我满了)    ErrTargetFriendListFull(对方满了)
//	ErrAcceptorFriendsFull   ErrTargetFriendListFull      ErrFriendListFull(我满了)
//	ErrRequestNotFound       —                            ErrNoPendingRequest
//	ErrBlockListFull         Block → ErrBlockListFull
//
// 这两行**必须按 RPC 分别映射**,把 AcceptFriend 那套 switch 照抄进 AddFriend 会得到
// 正好相反的码(玩家自己的列表满了,却被告知"对方的好友列表满了"),而且编译器与
// 单测都看不出来 —— 所以新增写路径时先回来看这张表。
var (
	// ErrBlocked:两人之间存在任一方向的拉黑。刻意**不区分方向** —— 告诉申请人"是对方拉黑了你"
	// 等于把别人的拉黑设置泄露给他(AGENTS §11.3 敏感信息最少暴露)。
	ErrBlocked = errors.New("blocked between players")

	ErrAlreadyFriends     = errors.New("already friends")
	ErrRequestAlreadySent = errors.New("friend request already pending")
	// ErrTooManyPending:申请发起方**出站**的 pending 条数达到 MaxPendingRequests。
	ErrTooManyPending = errors.New("too many outgoing friend requests")
	// ErrTargetInboxFull:接收方**入站**的 pending 条数达到 MaxIncomingRequests。
	// 与出站分开两个上限:入站量不由本人控制,共用一个上限会让被骚扰者先被自己的收件箱打满。
	ErrTargetInboxFull = errors.New("target friend request inbox full")

	// ErrSenderFriendsFull / ErrAcceptorFriendsFull:好友数上限(见上表的分 RPC 映射)。
	ErrSenderFriendsFull   = errors.New("sender friend list full")
	ErrAcceptorFriendsFull = errors.New("acceptor friend list full")

	// ErrRequestNotFound:不存在处于 pending 状态的好友申请。
	// AcceptFriend / RejectFriend 以此 fail-closed —— 没有申请就不允许建立关系。
	ErrRequestNotFound = errors.New("no pending friend request")

	// ErrBlockListFull:拉黑名单达到 MaxBlocks(实现见 block_repo.go)。
	ErrBlockListFull = errors.New("block list full")
)

// errInvalidPlayerPair 是"目标 id 为 0 或等于操作者自己"的**防御性**错误,刻意不做成哨兵。
//
// logic 层已经在事务外挡住这两种入参(§3.1/§3.4 的参数校验,回 ErrInvalidParameter);
// 真走到这里说明调用方漏了校验,那是服务端缺陷而不是玩家的错 —— 让它落到 logic 的
// default 分支、被定性成 ErrStorage(本域唯一的 fault 码)去触发告警,比伪装成
// "参数错误"的业务拒绝更容易发现。**不要**给它加哨兵让调用方吞掉。
func errInvalidPlayerPair(a, b uint64) error {
	return fmt.Errorf("invalid friend player pair (%d, %d): caller must reject zero/self target before calling", a, b)
}

// friend_request.status 的取值。与 friend.proto 的 FriendRequestStatus 同码
// (friend_table.proto 里 status 是 uint32 而不是 enum,理由见那边的注释)。
// 写成常量而不是在 SQL 里散落 1/2/3:sweep 的 `status IN (?, ?)` 与本文件的三处状态迁移
// 必须指的是同一套编号,数字散在字符串里没法 grep、也没法让编译器帮忙。
const (
	requestStatusPending  = 1
	requestStatusAccepted = 2
	requestStatusRejected = 3
)

// AddFriendLimits 收齐 AddFriendRequest 的三个上限。
//
// 不用三个并排的 uint32:MaxPendingRequests(出站)与 MaxIncomingRequests(入站)同类型、
// 方向相反,位置传反不会报错,只会让两条上限张冠李戴。
type AddFriendLimits struct {
	MaxFriends          uint32
	MaxPendingRequests  uint32 // 出站:我挂着的 pending
	MaxIncomingRequests uint32 // 入站:别人发给我的 pending
}

// ── 全局锁序与隔离级别(F2 §2;**改任何写路径之前必须读完这一段**)────────────────
//
// 本域所有写事务逐字遵守同一条锁序,**顺序本身就是正确性**,不是风格。
// 骨架只有一份实现 —— runGuardedWrite;四条写路径(AddFriendRequest / AcceptFriend /
// RemoveFriend / Block)都只提供"守卫之后做什么"的 body,不各自再写一遍外层:
//
//	事务外:各方法自己的前置判定(普通读;不进 runGuardedWrite,也就不进重试)
//	runGuardedWrite(至多 capacityGuardMaxAttempts 遍,见 (5)):
//	  事务外:按 player_id 升序补齐双方 friend_capacity 行(INSERT … ON DUPLICATE KEY UPDATE,自动提交)
//	  BeginTx(READ COMMITTED)
//	    ① 容量守卫:按 player_id 升序逐行 SELECT ... FROM friend_capacity WHERE player_id = ? FOR UPDATE
//	    body:
//	      ② 一切与"能不能做"有关的判定读(拉黑 / 好友边 / 申请行)都在守卫之后
//	      ③ 写入
//	  Commit
//	提交后:失效缓存(invalidateCachesAfterCommit);S2C 推送由 logic 层在事务外做
//	        ↑ **提交之后的任何失败都不得改变方法的返回值**:写已经落库且不可撤销,
//	          把失效失败上抛会让一次成功的写被定性成 fault 码 ErrStorage(理由见该方法)。
//
// 前四条"为什么",每条都对应一次真实的 1213;第五条是容量行回收带来的新约束:
//
// (1) **ensure 必须在事务外**。把双方的补行 INSERT 放进事务里,多个请求各持有自己刚插入的
//     新行、又都去抢同一个接收者行,真实 InnoDB 会形成 insert-intention 死锁。
//
// (2) **容量守卫必须是事务里的第一把锁**。这是 A 仓 2026-08-11 在真 MySQL 8.4 上压测出 1213 的
//     根因结论(A: services/social/friend/internal/data/friend_repo.go 的 acquirePlayerGuard 注释):
//     "我只锁了本对玩家的行"这个前提**不成立** —— 未命中的锁定读在 RR 下锁的是该键所在的
//     **间隙**,间隙是跨玩家对共享的;于是守卫之前的任何锁定读都把锁按不受控的顺序拿在了手上。
//     纪律因此收敛成一句:**拿到容量守卫之前不做任何锁定读**。
//
// (3) F2 把 AddFriend 改成权威事务之后,它与 F1 交付的 AcceptFriend 正好互为 ABBA:
//         AcceptFriend 持 friend_request(A,B) 的行锁,等 friend_capacity(A)/(B);
//         AddFriend    持 friend_capacity(A)/(B),等 friend_request(A,B)。
//     裁定(F2 §2.2)是全局统一"容量守卫最先",所以 AcceptFriend 的申请行 FOR UPDATE
//     **已从守卫之前下移到守卫之后**。这不会把 B 仓当初修掉的 1213 引回来:那次修的是
//     "过早把 status 从 1 改成 2,会让并发事务在 idx_to_player 前缀上删/插而触发 1213",
//     修法是**把 UPDATE 延后到容量锁之后**;UPDATE 现在仍在守卫之后,被下移的只是
//     按 (from_player_id,to_player_id) 主键的单行 SELECT。
//     ⚠ 谁要把这个顺序改回去,先去看 friend_guard_lock_order_mysql_test.go 里
//     "同一对玩家并发 AddFriend 与 AcceptFriend"那条用例 —— 顺序写错它必挂。
//
// (4) 容量守卫行同时是**"这一对玩家"的串行化载体**:凡是会改动这两人之间关系的写事务
//     (AddFriend / AcceptFriend / Block / RemoveFriend)都锁同一对容量行,于是两两互斥。
//     RC 下的探针自己**挡不住**并发插入(没有间隙锁),挡住并发的是这把守卫。
//     新增任何写路径时**先拿守卫**,否则本文件所有"权威判定"会静默退化成 check-then-act。
//
// (5) **回收与写路径的关系**。sweep 的 SweepIdleCapacityRows(sweep_repo.go)会删掉
//     "friend_count = 0 且 created_ms 早于保留期"的容量行,它是本表唯一的删除方。
//     - 为什么不会成环:回收**逐行、自动提交、按主键**删,任一时刻至多持有一把守卫锁,
//       且持锁时不再等任何别的锁 —— 一个不"持有并等待"的参与者不可能处在等待环里。
//       写成一条批量 `DELETE ... LIMIT ?` 就不行:那条语句在一个语句事务里按**二级索引序**
//       锁多行守卫行,与这里"按 player_id 升序"的取锁顺序不同,可以成环(1213)。
//     - 为什么缺行要重试、且至多 3 遍(capacityGuardMaxAttempts)严格充分:ensure 在事务外自动提交,
//       回收可以插进两个窗口 —— "ensure(行已在,补行是 no-op)→ 回收删行 → 取守卫缺行",
//       以及"FOR UPDATE 正等着这行、回收提交后该行消失"。这是预期内的竞态,不是故障:
//       runGuardedWrite 回到事务外重新 ensure 再跑一遍。缺行只可能发生在事务的**第一条语句**
//       (守卫),此时事务还没有任何副作用,整遍重跑是安全的。
//       为什么恰好是 3:一次写至多涉及两行守卫行;本表唯一的删除方是回收,而回收的 DELETE 在
//       提交点复核 `created_ms < 截止点`,被 ensure 重建出来的行 created_ms 是当前时刻,在本次请求的
//       时间尺度内不可能再被回收 —— 所以**每行至多被删一次**(副本再多也一样:每行在被重建之前
//       只能被删一次)。最坏时序是双方都是陈旧零好友行、各被删一次:第 1、2 遍缺行,第 3 遍必过。
//       前提:各副本(跑写路径的 friend 进程与跑 sweep 的进程)之间的墙钟偏差小于 RetentionDays
//       (config.Validate 保证它至少 1 天);sweeper 时钟错到 1970 由 sweepCutoffMs 的"截止点非正"挡住。
//       第 3 遍仍缺行 = 有回收之外的东西在删这张表,或时钟前提被破坏 —— 那才是不变量破裂,
//       照旧 fail-closed(ErrStorage)。相对"只重试一次",代价只是 fault 之前多一次 ensure 和一个空事务。
//       1213 不属于缺行哨兵,不在本重试范围内(ensure 自己那条 1213 见下一条)。
//     - 回收给 ensure 带来的 1213(**已从根上消掉**,2026-09-21):HEAD 从不删容量行,并发补行同键时后到者
//       看到的是**活的**重复键,成不了环。有了回收的 DELETE 就会出现 delete-marked 的主键记录,补行对它做
//       重复键检查、发现是已提交的删除标记后要就地复活(取 X)。补行原先是 INSERT IGNORE,重复键检查取 **S**:
//       "**至少两个**补行同时排队等同一条被 X 压住的主键记录"(压住的一方可以是 sweeper 的 DELETE,也可以是
//       另一个事务的守卫 FOR UPDATE),一提交等待者就同时拿到 S、又都要 X,互等(手册 "Deadlocks in InnoDB"
//       的例子),真库复现过。现在补行是 `INSERT … ON DUPLICATE KEY UPDATE player_id = player_id`:重复键检查
//       直接取 **X**,等待者只能一个一个拿到,不存在"都持 S 再升 X"的形状;连接池又统一为 RC(svc.BuildDSN),
//       重复键检查只取记录锁、不取 next-key。ensure 是自动提交的单条语句,整个生命周期至多持有这一把锁,
//       不"持有并等待",进不了任何环。ensureFriendCapacityRows 对 1213 的有上限重试(ensureDeadlockMaxAttempts)
//       保留为纵深防御并计数(ensureDeadlockRetries);回归用例场景 (g) 断言它一次都没被触发。
//     - 为什么删行不放宽好友数上限:回收只删 friend_count = 0 的行(即权威边数为 0),而 ensure 补行时
//       从 friend 表的权威边数重算初值、绝不猜 0(见 ensureFriendCapacityRows)。行缺失期间任何加边
//       路径都必须先 ensure 把行建回来,所以一个读到较小 COUNT 的陈旧 ensure 不可能落地成偏小的计数。
//     - 为什么删行也不会让计数**偏大**(需要 created_ms 配合):ensure 的 COUNT 与 INSERT 是两条各自
//       自动提交的语句,不原子。交错"ensure 读到 COUNT=1 → RemoveFriend 提交(边数 0、行变成
//       friend_count=0)→ 回收删行 → 陈旧的补行 INSERT (P, 1) 落地"会留下 friend_count=1 而真实
//       边数为 0 的行:之后无边可删、减不到它,friend_count≠0 的行也永不再进回收 —— 上限永久少 1、
//       零报错。挡法:deleteFriendEdges 减计数时**同时把 created_ms 刷成当前时刻**,于是"刚减到 0 的行"
//       要再过一个完整保留期才是回收候选,上面那条交错里的"回收删行"不可能落进 ensure 两条语句
//       之间。残留:陈旧窗口(COUNT 与 INSERT 之间)跨过整个 RetentionDays 才可能复现,视为不可达。
//
// (6) **守卫之后的锁定读 / 锁定写,一律写成"完整主键的等值点查 / 点更新"**,不许用 OR / IN / 前缀范围。
//     守卫只串行化"这一对玩家",所以锁集必须**恰好**落在这一对的行上;锁集由执行计划决定,而执行计划由
//     优化器按统计信息挑 —— 写成 OR / IN 等于把正确性押在优化器身上。2026-09-21 首次真库回归(MySQL 26.7,
//     小表)实证:
//     - blockedEitherWay 原先的 `(a,b) OR (b,a) ... FOR UPDATE` 被规划成 **idx_blocked_player 覆盖索引全扫描**,
//       锁住了**别的玩家对**那一行的二级索引项,再去等它的主键;而那一对的 Unblock(`DELETE` 按主键)先主键、
//       后二级索引 —— 取锁顺序相反,1213。friendEdgeExistsForUpdate 同形(idx_friend_player),与 RemoveFriend 的
//       删边 DELETE 成环,同样复现了 1213。LATEST DETECTED DEADLOCK 原文见交接文档 §9。
//     - lockCapacityRows 原先的 `IN (...) ORDER BY ... FOR UPDATE` 被规划成 **PRIMARY 全索引扫描**:按主键序取锁
//       所以不成环,但每个守卫都要排在所有"更小 player_id 上正被持有的守卫"之后,等于按玩家号把写路径串行化。
//     - Block 取消双向 pending 的 `status=? AND ((…) OR (…))` UPDATE 走的是 idx_status_updated 的 status 前缀
//       (type=range),扫的是**全服**所有 pending 行,先二级索引后主键,与别的玩家对上"先主键"的写同样可以成环。
//     完整主键等值的执行计划是 const(UPDATE 为 key=PRIMARY、用满两列),与统计信息无关;RC 下未命中不加锁,
//     命中只锁那一行主键 —— 不碰任何二级索引项,所以与"先主键后二级索引"的 DELETE / UPDATE 不可能反序。
//     前提:WHERE 不能同时钉死**另一个唯一索引**的全部列 —— 否则优化器可能在两个 const 路径里选中唯一二级索引,
//     又变成先二级后主键。本服务四张表按 D-14 零 UNIQUE KEY,前提成立;新增唯一键时回来重核(回归用例断言
//     key = PRIMARY,真被规划到唯一二级索引上会变红)。
//     代价:一对玩家的"任一方向"判定从一次往返变成两次。回归:friend_guard_lock_order_mysql_test.go 的
//     TestLockingStatementsArePrimaryKeyPointLookups 对下面这几个 SQL 常量逐条 EXPLAIN(确定性),
//     TestCapacityRowReclaimRacesWithGuardedWrites 负责并发实证(概率性)。
//
// 隔离级别固定 READ COMMITTED(照 A 仓):
//   - RR 的间隙锁会让"同一玩家并发拉黑 16 个不同目标"这类**只碰不同行**的事务互相挡:
//     未命中的 FOR UPDATE 拿到的是同一个间隙锁(间隙锁彼此相容,N 个事务都能拿到),
//     真正的排他点在守卫行,谁抢到守卫谁去 INSERT,而插入意向被其余事务的间隙锁挡住 → 成环。
//     A 仓真 MySQL 8.4 实测必炸,且只在 MySQL 上炸(TiDB 没有间隙锁,双后端跑才看得见)。
//   - RC 没有间隙锁,正确性由"守卫串行化 + 唯一键"提供,两者在 RC 下都成立。
//   - 代价:RC 下同一事务内两次普通 SELECT 可能读到不同结果(每条语句各取一份新快照)。
//     所以"判定读必须在守卫之后"不是可选优化;守卫之后的探针用锁定读还是普通读,
//     取决于该次读的对象是否已被守卫覆盖 —— 逐处理由写在 AddFriendRequest 的 ⑤⑥ 旁边。
//   - 事务**之外**的自动提交语句(ensure 的补行、Unblock / sweep 的 DELETE)不经过 BeginTx,
//     它们的隔离级别来自连接会话:svc.BuildDSN 用 transaction_isolation 把整个连接池设成 RC,
//     否则它们会落回服务器全局默认的 REPEATABLE-READ、重新拿间隙锁 / next-key 锁。
//   - 前提是 binlog_format=ROW(RC + STATEMENT 格式的 binlog 不安全)。
//     ⚠ 已核对本仓配置:`deploy/k8s/manifests/infra/mysql.yaml:60` 显式写了 `binlog_format = ROW`
//     (同段还有 `binlog_row_image = FULL`);`deploy/` 下**没有别的**地方设置这一项,
//     本地 docker-compose 未显式设置,取 MySQL 8.4 的默认值(也是 ROW)。

// friendWriteTxIsolation 是本域写事务的隔离级别,理由见上面那段「隔离级别固定 READ COMMITTED」。
// (上面的空行是刻意的:那段说明若紧贴本常量就成了 doc comment,gofmt 会把其中用空格缩进的
// 续行当成代码块重排,把分条论证打散。)
const friendWriteTxIsolation = sql.LevelReadCommitted

// FriendEntry 是好友列表缓存里的一条(JSON 形状,不是表结构的事实源)。
// 在线态与最后活跃时刻都**不在**本结构里 —— 它们由 logic 层从 session_reader 现取
// (见 logic/friend_logic.go 的 GetFriendList)。本结构会被整个 json.Marshal 进 30 分钟 TTL 的缓存,
// 任何每次请求都会变的展示态一旦放进来,就等于故意返回过期的在线状态。
// 存量缓存里的旧 payload 可能还带着已删除的 last_active_ms 键:encoding/json 忽略未知字段,
// 解码兼容,不需要为此清 Redis。
type FriendEntry struct {
	FriendPlayerID uint64 `json:"friend_player_id"`
	SinceMs        int64  `json:"since_ms"`
}

type FriendRequestEntry struct {
	FromPlayerID  uint64 `json:"from_player_id"`
	ToPlayerID    uint64 `json:"to_player_id"`
	RequestTimeMs int64  `json:"request_time_ms"`
	Status        int32  `json:"status"` // 1=pending, 2=accepted, 3=rejected
}

// FriendRepo 是 mmorpg_friend 的存储访问层:MySQL 是权威,FriendRedis 只做 cache-aside。
type FriendRepo struct {
	rdb        *redis.Redis
	db         *sql.DB
	defaultTTL time.Duration
	// listReadHardLimit 是列表类读的 SQL LIMIT(Friend.ListReadHardLimit)。
	// 放在 repo 上而不是每次调用传:缓存里存的是**已经截断过**的列表,
	// 逐调用传值会让"缓存里那份按谁的上限截的"取决于谁先回填,排查时无从下手。
	listReadHardLimit uint32
	// cacheLoadMu 是**按 cacheKey 分组**的回填合并锁(cacheKey -> *sync.Mutex),
	// 不是 x/sync 的 singleflight —— 语义等价于"同一个 key 只打一次 MySQL",
	// 但不引入新依赖(go.mod 不在本批清单里)。
	// 千万别退回成一把进程级 sync.Mutex:那会把全进程所有玩家、两条最高频读路径的回填
	// 串行成一条队列,缓存冷时每个请求都要排在一次完整 MySQL 往返后面。
	cacheLoadMu sync.Map
}

// NewFriendRepo 构造。
//
// ⚠ 签名比 F1 多了 listReadHardLimit(F2 §4 的列表读硬上限):调用方是
// logic.NewDeps,值取 svcCtx.Config.Friend.ListReadHardLimit(config.Validate 已拒 0)。
func NewFriendRepo(rdb *redis.Redis, db *sql.DB, defaultTTL time.Duration, listReadHardLimit uint32) *FriendRepo {
	return &FriendRepo{
		rdb:               rdb,
		db:                db,
		defaultTTL:        defaultTTL,
		listReadHardLimit: listReadHardLimit,
	}
}

// ── Redis key helpers ──────────────────────────────────────────
//
// 键形状 `friend:{f:<pid>}:<域>:v3`,两处讲究:
//
//  1. **hash tag `{f:<pid>}`**(F2-11):Redis Cluster 按 `{}` 内的内容算 slot,所以
//     `friend:{f:7}:list:v3` 与 `friend:{f:7}:list:v3:generation` 必定同 slot ——
//     下面两条 Lua 各自只动"同一个玩家、同一个域"的两个键,因此在 Cluster 上不会 CROSSSLOT。
//     没有 hash tag 时这两个键会落在不同 slot,脚本在 Cluster 上直接报错(而单机 Redis 上
//     一切正常 —— 这种缺陷只在换成 Cluster 的那天暴露)。
//  2. **版本号 v3**:键名变了,旧的 v2 键不再被任何代码读到。未上线、库里没有数据,
//     所以不做迁移(旧键靠自身 TTL 过期)。以后再改键形状同样只抬版本号,不做原地兼容。
func friendListKey(playerID uint64) string {
	return fmt.Sprintf("friend:{f:%d}:list:v3", playerID)
}

func pendingRequestsKey(playerID uint64) string {
	return fmt.Sprintf("friend:{f:%d}:req:v3", playerID)
}

// 两条 Lua 以**脚本正文**保存而不是 redis.NewScript 包装:go-zero 的执行入口是
// EvalCtx(ctx, script, keys, args...)(仓内先例 go/chat/internal/logic/chat_logic.go、
// go/data_service/internal/routing/router.go),它自己吃脚本字符串,不接 *redis.Script。
const invalidateFriendCacheScript = `
redis.call("INCR", KEYS[1])
redis.call("DEL", KEYS[2])
return 1
`

const fillFriendCacheScript = `
local generation = redis.call("GET", KEYS[1])
if not generation then generation = "0" end
if generation ~= ARGV[1] then return 0 end
if tonumber(ARGV[3]) > 0 then
    redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
else
    redis.call("SET", KEYS[2], ARGV[2])
end
return 1
`

func friendCacheGenerationKey(cacheKey string) string {
	// 直接追加后缀:cacheKey 里的 hash tag 一起被带过来,所以 generation 与数据键同 slot。
	return cacheKey + ":generation"
}

// ── Read (cache-aside + singleflight) ──────────────────────────

func (r *FriendRepo) GetFriendList(ctx context.Context, playerID uint64) ([]FriendEntry, error) {
	return loadVersionedFriendCache(
		ctx, r, friendListKey(playerID),
		func(ctx context.Context) ([]FriendEntry, error) { return r.loadFriendListFromMySQL(ctx, playerID) },
	)
}

func (r *FriendRepo) GetPendingRequests(ctx context.Context, playerID uint64) ([]FriendRequestEntry, error) {
	return loadVersionedFriendCache(
		ctx, r, pendingRequestsKey(playerID),
		func(ctx context.Context) ([]FriendRequestEntry, error) {
			return r.loadPendingRequestsFromMySQL(ctx, playerID)
		},
	)
}

// ── Write operations ───────────────────────────────────────────

// AddFriendRequest 是"发起好友申请"的**权威事务**(F2 §3.1)。
//
// F1 之前它是一条自动提交的 upsert,四道上限判定(已是好友 / 列表满 / 重复申请 / 出站配额)
// 全在事务外的 logic 层做 —— 那是典型的 check-then-act:两个请求并发时都能通过检查。
// 现在所有判定都在同一把容量守卫内完成,logic 层保留的预检只是省掉明显不可能成功的开库,
// **不得**替代这里的复核(AGENTS §11.3)。
//
// 返回的哨兵见文件顶部的映射表。方法名保留 AddFriendRequest:它写的是一行**申请**,
// 不是好友关系(真正建边在 AcceptFriend),与 RPC 名 AddFriend 刻意不同名。
//
// ⚠ 三个上限收在 AddFriendLimits 里,字段名自解释,不再有位置参数 —— 出站与入站两条上限
// 类型相同、方向相反(出站 50 / 入站 200 是两个默认值),做成并排的 uint32 时传反了不会报错,
// 只会让两条上限张冠李戴。
func (r *FriendRepo) AddFriendRequest(ctx context.Context, fromPlayerID, toPlayerID uint64,
	lim AddFriendLimits) error {
	if toPlayerID == 0 || fromPlayerID == 0 || fromPlayerID == toPlayerID {
		return errInvalidPlayerPair(fromPlayerID, toPlayerID)
	}

	// 容量行在主事务之前、按 player_id 升序自动提交地补齐(理由见顶部锁序说明 (1))。
	// AddFriend 自己不改 friend_count,补行只为拿到守卫载体;副作用是"任意 target 都会
	// 被建出一行 friend_capacity"—— 挡增长**速度**的是 §3.6 的每分钟配额与 MaxPendingRequests,
	// 不是这里(friend 服务没有玩家名册可以验证 target 是否真实存在);建出来的零好友行
	// 由 sweep 的 SweepIdleCapacityRows 在保留期后回收(sweep_repo.go)。
	// ensure、BeginTx、① 与 Commit 都在 runGuardedWrite 里(本域写事务的唯一骨架)。
	//
	// ① 容量守卫:本事务的第一把锁(runGuardedWrite 取),顺带把双方当前的 friend_count 读回来、
	//    以 counts 传进闭包(守卫锁在手,这两个值到 Commit 之前不可能被别人改,⑦ 直接用,不必再查一次)。
	// 闭包里 return 的哨兵由 runGuardedWrite 回滚后原样上抛。
	err := r.runGuardedWrite(ctx, fromPlayerID, toPlayerID, func(ctx context.Context, tx *sql.Tx, counts map[uint64]uint32) error {
		// ② 拉黑(两个方向,锁定读)。任一方向命中就拒:被拉黑的人不该能继续发申请,
		//    拉黑了别人的人也不该收到对方的申请。
		blocked, err := blockedEitherWay(ctx, tx, fromPlayerID, toPlayerID)
		if err != nil {
			return err
		}
		if blocked {
			return ErrBlocked
		}

		// ③ 已是好友(任一方向的边存在即算,锁定读)。双向两行本该同增同减,
		//    但只要有一边在就不该再发申请 —— 单边残留应由修复流程处理,不该靠再发一次申请去补。
		alreadyFriends, err := friendEdgeExistsForUpdate(ctx, tx, fromPlayerID, toPlayerID)
		if err != nil {
			return err
		}
		if alreadyFriends {
			return ErrAlreadyFriends
		}

		// ④ 本方向的申请行(按主键单行锁定读)。已经 pending 就直接拒,不刷新时间 ——
		//    否则"反复点加好友"能把自己的申请一直顶到对方列表最前面。
		var existingStatus int32
		err = tx.QueryRowContext(ctx,
			"SELECT status FROM friend_request WHERE from_player_id=? AND to_player_id=? FOR UPDATE",
			fromPlayerID, toPlayerID).Scan(&existingStatus)
		switch {
		case err == nil:
			if existingStatus == requestStatusPending {
				return ErrRequestAlreadySent
			}
		case errors.Is(err, sql.ErrNoRows):
			// 无历史申请,走下面的 INSERT 分支。
		default:
			return fmt.Errorf("lock friend request %d->%d: %w", fromPlayerID, toPlayerID, err)
		}

		// ⑤⑥ 两个 pending 计数。**这两条刻意用普通读,不加 FOR UPDATE**,与 §2.2 ② 的字面要求
		// 有一处偏离,理由必须看完再改:
		//
		//   - 为什么普通读在这里就是权威读:RC 下**每条语句各取一份新快照**,所以这条 SELECT 看得到
		//     守卫等待期间别的事务已提交的写(这与 RR 不同,RR 的快照固定在事务第一条普通 SELECT)。
		//     而能让这两个计数**变大**的写语句全仓只有一条:AddFriendRequest 自己的 ⑧ upsert,
		//     它必须先持有对应玩家的容量守卫行 —— 我们此刻正握着 from 与 to 两行,所以在 Commit 之前
		//     谁也插不进新的 pending。其余对 friend_request 的写(AcceptFriend 的正 / 反向 UPDATE、
		//     RejectFriend 的 CAS、Block 取消双向 pending、sweep 的 DELETE)都只让 pending 变小
		//     或只碰终态行 —— AcceptFriend 增加的是 friend_count,不是 pending 数 ——
		//     只会让上限更宽松,不影响 fail-closed。
		//   - 为什么不能加 FOR UPDATE:两条 COUNT 的加锁集合分别是"from_player_id=A 的行"与
		//     "to_player_id=T 的行",它们**跨玩家对交叉**,而容量守卫只串行化共享玩家的事务。
		//     于是两个完全不相干的申请可以成环:
		//         TRX1 = AddFriend(A→T) 先锁 (A,*) 里的 (A,C),再要 (*,T) 里的 (B,T);
		//         TRX2 = AddFriend(B→C) 先锁 (B,*) 里的 (B,T),再要 (*,C) 里的 (A,C)。
		//     两者没有共享的容量行,守卫拦不住 → 1213。按 player_id 排序也解不掉:
		//     两个集合的索引维度不同(一个按 from、一个按 to),不存在统一的全序。
		//   - 因此这里维持一条**必须被后来者保住的不变量**:任何会让 pending 计数**增加**的写路径,
		//     都必须先持有对应玩家的容量守卫行。新增写路径时若违反它,这两处判定会静默退化。
		if lim.MaxPendingRequests > 0 {
			var outgoing uint32
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND status=?",
				fromPlayerID, requestStatusPending).Scan(&outgoing); err != nil {
				return fmt.Errorf("count outgoing pending %d: %w", fromPlayerID, err)
			}
			// >= 而不是 >:本次要新增一条,等于上限时再加就超了。
			if outgoing >= lim.MaxPendingRequests {
				return ErrTooManyPending
			}
		}
		if lim.MaxIncomingRequests > 0 {
			var incoming uint32
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM friend_request WHERE to_player_id=? AND status=?",
				toPlayerID, requestStatusPending).Scan(&incoming); err != nil {
				return fmt.Errorf("count incoming pending %d: %w", toPlayerID, err)
			}
			if incoming >= lim.MaxIncomingRequests {
				return ErrTargetInboxFull
			}
		}

		// ⑦ 好友数上限。双方都查:自己满了发了也没用,对方满了同意不了 ——
		//    让申请挂在那里比当场拒更糟(对方每次打开列表都看到一条注定失败的申请)。
		//    哨兵按**角色**回(见顶部映射表):这里的 from 是 Sender、to 是 Acceptor。
		if lim.MaxFriends > 0 {
			if counts[fromPlayerID] >= lim.MaxFriends {
				return ErrSenderFriendsFull
			}
			if counts[toPlayerID] >= lim.MaxFriends {
				return ErrAcceptorFriendsFull
			}
		}

		// ⑧ upsert 申请行。历史终态行(rejected / accepted)复用同一行改回 pending,
		//    靠主键天然幂等;request_time_ms 刷新成本次发起时刻(这是**新一次**申请,
		//    列表按它排序,沿用首次时间会把重新申请排到陈旧位置)。
		//    updated_ms 必须一起写(F2-14):sweep 用 (status,updated_ms) 判保留期,
		//    漏写会让这一行永远停在 0、在 delete 模式下被当成"早就过期"。
		//    ⚠ 用 VALUES() 取待插入值而不是 MySQL 8.0.20+ 新增的行别名(`... AS new`):
		//    VALUES() 虽被标记 deprecated,但四张表都带着 TiDB 打散选项(friend_table.proto),
		//    数据层迁 TiDB 是既定方向,而行别名语法的 TiDB 支持面不确定 —— 这里选可移植的那个。
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status, updated_ms)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE status=VALUES(status), request_time_ms=VALUES(request_time_ms), updated_ms=VALUES(updated_ms)`,
			fromPlayerID, toPlayerID, now, requestStatusPending, now); err != nil {
			return fmt.Errorf("upsert friend request %d->%d: %w", fromPlayerID, toPlayerID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// generation + Lua CAS 会阻止并发 miss 把写入前读到的旧 DB 快照回填。
	// 只失效接收者的申请缓存:申请行落在 to_player_id 的收件箱里,申请人这边没有缓存视图。
	r.invalidateCachesAfterCommit(ctx, pendingRequestsKey(toPlayerID))
	return nil
}

// AcceptFriend 接受 fromPlayerID 发给 toPlayerID 的好友申请(toPlayerID 必须是会话里的 me)。
func (r *FriendRepo) AcceptFriend(ctx context.Context, fromPlayerID, toPlayerID uint64, maxFriends uint32) error {
	if fromPlayerID == 0 || toPlayerID == 0 || fromPlayerID == toPlayerID {
		return errInvalidPlayerPair(fromPlayerID, toPlayerID)
	}
	// 事务外预检只用于避免恶意无申请调用制造无界 capacity 空行(与 RemoveFriend 的
	// F2-15 前置判定同一个目的);真正的安全门禁仍是事务内守卫之后的锁定读复核。
	var pending int
	if err := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM friend_request WHERE from_player_id=? AND to_player_id=? AND status=?",
		fromPlayerID, toPlayerID, requestStatusPending).Scan(&pending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRequestNotFound
		}
		return err
	}
	cacheKeys := []string{
		friendListKey(fromPlayerID),
		friendListKey(toPlayerID),
		pendingRequestsKey(toPlayerID),
		// 反向 pending(to→from)落在 **from** 的收件箱里,本事务会把它一并置为 accepted,
		// 所以 from 的申请缓存也必须失效。漏掉它会让申请人的"待处理列表"里一直挂着
		// 一条已经不存在的申请,直到 TTL 到期。
		pendingRequestsKey(fromPlayerID),
	}
	// capacity 行在主事务前逐行、按 player_id 顺序自动提交地确保存在(顶部锁序说明 (1))。
	// 缺行不能猜 0:ensure 用普通一致性读从 friend 表的权威边数算出初值(见 ensureFriendCapacityRows)。
	// ensure、BeginTx、① 与 Commit 都在 runGuardedWrite 里;上面的 pending 预检在它之外,
	// 不随回收竞态的重试重跑(重跑的那一遍由事务内 ③ 的锁定读复核兜住)。
	//
	// ① 容量守卫先行(runGuardedWrite 取,读回的 friend_count 以 counts 传进闭包)。
	// 好友数上限由 friend_capacity 行保护,不依赖
	// COUNT ... FOR UPDATE 的 gap-lock 行为(它会随隔离级别变化);双方始终按 player_id
	// 升序加锁,避免反向申请同时接受时 ABBA。
	err := r.runGuardedWrite(ctx, fromPlayerID, toPlayerID, func(ctx context.Context, tx *sql.Tx, counts map[uint64]uint32) error {
		// ② 拉黑复核(F2 §3.2 新增)。没有这一步,"Block 删边 → Accept 插边"可以交错出
		// "既是好友又在黑名单里"的状态:Block 与 Accept 现在锁同一对容量行,任一先行另一必见其果。
		blocked, err := blockedEitherWay(ctx, tx, fromPlayerID, toPlayerID)
		if err != nil {
			return err
		}
		if blocked {
			return ErrBlocked
		}

		// ③ 申请行按主键单行锁定读并复核仍是 pending。
		//
		// ⚠ 这条 SELECT **必须留在容量守卫之后**(F2 §2.2 的裁定)。它原先排在守卫之前,
		// 与 F2 新增的 AddFriend 权威事务正好互为 ABBA(顶部锁序说明 (3))。
		// 下移不会把 B 仓当初修掉的 1213 引回来:那次的根因是"过早把 status 从 1 改成 2,
		// 让并发事务在 idx_to_player 前缀上删/插",修法是**把 UPDATE 延后到容量锁之后** ——
		// 下面的 UPDATE 仍在守卫之后,这里下移的只是一条主键单行的 SELECT,
		// 它锁的是 (from,to) 这一行、不碰 idx_to_player 的范围。
		var requestStatus int32
		if err := tx.QueryRowContext(ctx,
			"SELECT status FROM friend_request WHERE from_player_id=? AND to_player_id=? FOR UPDATE",
			fromPlayerID, toPlayerID).Scan(&requestStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrRequestNotFound
			}
			return err
		}
		if requestStatus != requestStatusPending {
			return ErrRequestNotFound
		}

		// ④ 上限判定用守卫读回来的 friend_count(锁在手,值不会变)。
		// maxFriends == 0 按"未配置"放行,与 AddFriendRequest ⑦、Block ③ 的约定一致 ——
		// 生产不会出现(config.Validate 拒 0 阈值),但本层只许有一种约定:0 = 不限,不是 0 = 一个都不许。
		if maxFriends > 0 {
			if counts[fromPlayerID] >= maxFriends {
				return ErrSenderFriendsFull
			}
			if counts[toPlayerID] >= maxFriends {
				return ErrAcceptorFriendsFull
			}
		}

		now := time.Now().UnixMilli()

		// ⑤ 推进申请状态。保留 RowsAffected 的 fail-closed 门禁(WHERE status=pending),
		// 并补写 updated_ms(F2-14:不写它 sweep 就没有可用的保留期依据)。
		res, err := tx.ExecContext(ctx,
			"UPDATE friend_request SET status=?, updated_ms=? WHERE from_player_id=? AND to_player_id=? AND status=?",
			requestStatusAccepted, now, fromPlayerID, toPlayerID, requestStatusPending)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrRequestNotFound
		}

		// ⑥ 反向 pending 一并收敛成 accepted(F2 §3.2 新增)。A→B 与 B→A 可以各自 pending;
		// 本次接受已经让双方成为好友,反向申请的结果同样是"好友已建立"。不收敛的话它会永远挂在
		// from 的收件箱里,而且被接受时会对已是好友的两人再走一遍建边流程。
		// 这里不校验 RowsAffected:没有反向申请是最常见的情况,0 行是正常结果。
		if _, err := tx.ExecContext(ctx,
			"UPDATE friend_request SET status=?, updated_ms=? WHERE from_player_id=? AND to_player_id=? AND status=?",
			requestStatusAccepted, now, toPlayerID, fromPlayerID, requestStatusPending); err != nil {
			return fmt.Errorf("resolve reverse pending %d->%d: %w", toPlayerID, fromPlayerID, err)
		}

		// ⑦ 分向插入并按实际 RowsAffected 更新各自容量;已存在的方向不会重复计数。
		for _, edge := range [][2]uint64{{fromPlayerID, toPlayerID}, {toPlayerID, fromPlayerID}} {
			result, err := tx.ExecContext(ctx,
				"INSERT IGNORE INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, ?)",
				edge[0], edge[1], now)
			if err != nil {
				return err
			}
			inserted, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if inserted > 1 {
				return fmt.Errorf("unexpected friend insert count %d for %d -> %d", inserted, edge[0], edge[1])
			}
			if inserted == 1 {
				if _, err := tx.ExecContext(ctx,
					"UPDATE friend_capacity SET friend_count = friend_count + 1 WHERE player_id = ?",
					edge[0]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	r.invalidateCachesAfterCommit(ctx, cacheKeys...)
	return nil
}

// RejectFriend 拒绝一条 pending 申请。单行 CAS,不碰容量、不需要事务:
// 它只让 pending 计数变小,不可能越过任何上限(顶部锁序说明 (4) 的例外说明)。
func (r *FriendRepo) RejectFriend(ctx context.Context, fromPlayerID, toPlayerID uint64) error {
	if fromPlayerID == 0 || toPlayerID == 0 || fromPlayerID == toPlayerID {
		return errInvalidPlayerPair(fromPlayerID, toPlayerID)
	}
	key := pendingRequestsKey(toPlayerID)
	// updated_ms 与 status 同一条语句里写(F2-14):sweep 的保留期靠它,
	// 而"被拒"恰恰是 sweep 主要要清的那一类行。
	result, err := r.db.ExecContext(ctx,
		"UPDATE friend_request SET status=?, updated_ms=? WHERE from_player_id=? AND to_player_id=? AND status=?",
		requestStatusRejected, time.Now().UnixMilli(), fromPlayerID, toPlayerID, requestStatusPending)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrRequestNotFound
	}
	r.invalidateCachesAfterCommit(ctx, key)
	return nil
}

// RemoveFriend 删双向好友边并同步 friend_count。幂等:不是好友时返回成功。
func (r *FriendRepo) RemoveFriend(ctx context.Context, playerID, targetPlayerID uint64) error {
	if playerID == targetPlayerID {
		return nil
	}
	if playerID == 0 || targetPlayerID == 0 {
		return errInvalidPlayerPair(playerID, targetPlayerID)
	}

	// F2-15:**先判是不是好友,再 ensure 容量行**。
	// 原先直接 ensure,于是任意 target_player_id 每次调用都会凭空造出 2 行 friend_capacity ——
	// 客户端可以用互不相同的 target 撑大那张表。零好友的容量行现在由 sweep 的
	// SweepIdleCapacityRows 在保留期后回收(sweep_repo.go),但这道前置判定仍然保留:
	// 它挡的是回收周期**之内**的增长速度,回收只管得了保留期之后的存量。
	//
	// 这一探是**普通读**而不是锁定读,有意为之:此刻还没有容量守卫,按顶部锁序 (2)
	// 不允许做锁定读。漏判的唯一情形是"边正好在这一瞬间由 AcceptFriend 提交",
	// 那等价于删好友发生在结为好友之前 —— 幂等语义下返回成功即正确。
	// 反过来"边存在却读不到"不可能:已提交的边在 RC 的新快照里一定可见。
	exists, err := r.friendEdgeExists(ctx, playerID, targetPlayerID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	cacheKeys := []string{friendListKey(playerID), friendListKey(targetPlayerID)}
	// 容量守卫(runGuardedWrite 取)。这里不需要 friend_count 的值(只做增减,所以闭包不收 counts),
	// 但锁必须照样先拿:它同时是"这一对玩家"的串行化载体,Block 与 AcceptFriend 都锁同一对行。
	// 上面的 friendEdgeExists 在 runGuardedWrite 之外,不随回收竞态的重试重跑。
	err = r.runGuardedWrite(ctx, playerID, targetPlayerID, func(ctx context.Context, tx *sql.Tx, _ map[uint64]uint32) error {
		// 时刻取在闭包里:它属于真正拿到守卫的那一遍尝试(与 Block 同一个口径)。
		return deleteFriendEdges(ctx, tx, playerID, targetPlayerID, time.Now().UnixMilli())
	})
	if err != nil {
		return err
	}
	r.invalidateCachesAfterCommit(ctx, cacheKeys...)
	return nil
}

// ── 写事务的公共步骤(block_repo.go 也用这几个,别各写一份)──────────────────────

// errCapacityRowsMissing:容量守卫发现缺行。包内哨兵,**不导出**、不进 logic 的映射表 ——
// 它只服务于 runGuardedWrite 的"重新 ensure 并有上限地重试"(至多重试两次);用尽后仍缺行时原样上抛,
// logic 层照旧把它定性成 ErrStorage(fail-closed 不变)。
var errCapacityRowsMissing = errors.New("friend capacity rows missing")

// capacityGuardMaxAttempts:ensure + 写事务最多跑几遍(= 1 次正常 + 至多 2 次回收竞态重试)。
// 为什么恰好是 3,见顶部锁序说明 (5)。
const capacityGuardMaxAttempts = 3

// ensureDeadlockMaxAttempts:ensureFriendCapacityRows 对**单个玩家**的"COUNT + 补行 INSERT"
// 撞上 1213 时最多跑几遍,用尽后 fail-closed(1213 原样上抛,logic 定性 ErrStorage)。
//
// 2026-09-21 起这条重试只剩**纵深防御**:原先的成因是"回收留下 delete-marked 记录后,多个并发的
// INSERT IGNORE 各拿 S 锁、再都要升 X"(真库复现过),现已从根上消掉 —— 补行改用
// INSERT … ON DUPLICATE KEY UPDATE(主键重复时直接取 X、不走 S→X),连接池又统一为 RC(不拿间隙锁,
// 见 svc.BuildDSN)。回归用例 TestEnsureCapacityRows_ConcurrentInsertOnReclaimedRowSurvivesDeadlock
// 断言这条重试一次都没被触发(ensureDeadlockRetries 不变)。
const ensureDeadlockMaxAttempts = 3

// ensureDeadlockRetries 累计 ensure 因 1213 进入重试的次数。
// 正常运行时应恒为 0;非 0 说明出现了新的成环形态。包内测试读它来证明"不是靠重试吸收的,而是根本没死锁"。
var ensureDeadlockRetries atomic.Int64

// guardedWriteBody 是写事务在**已持有容量守卫**之后要做的事。counts 是守卫读回的双方 friend_count。
// 返回 nil → 提交;返回任何 error → 回滚并原样上抛(哨兵照旧穿透)。
//
// body 在一次 runGuardedWrite 里**至多执行一次**:重试只由守卫缺行触发,而守卫在 body 之前。
// 但 body 返回 nil 之后 Commit 仍可能失败,所以里面只许有**本事务之内**的副作用 ——
// 缓存失效、推送一律由调用方在 runGuardedWrite 返回 nil 之后做。
type guardedWriteBody func(ctx context.Context, tx *sql.Tx, counts map[uint64]uint32) error

// runGuardedWrite 是本域所有"要拿容量守卫的写事务"的唯一骨架:
//
//	事务外 ensure(升序、自动提交)→ BeginTx(RC)→ ① lockCapacityRows → body → Commit。
//
// 守卫缺行(errCapacityRowsMissing)时回到事务外重新 ensure,至多重试到 capacityGuardMaxAttempts。
// 调用方在它返回 nil 之后自己做 invalidateCachesAfterCommit。
//
// 为什么重试是安全的:缺行只可能由事务的**第一条语句**(守卫)报出来,此时 body 还没跑、
// 事务没有任何副作用,回滚掉的是一个空事务。**只有**这一种错误会触发重试 —— body 返回的
// 任何 error(含 1213、哨兵)都原样上抛,不在这里吞,也不在这里重试:
// 后台靠节拍自愈、在线请求靠客户端重试,是本域既有的口径。
// (ensure 自己那条 1213 是另一回事:它在事务外、没有副作用,由 ensureFriendCapacityRows 内部
// 有上限地重试,用尽后照样原样上抛到这里、再原样上抛出去。)
// 调用方各自的事务外前置判定(AcceptFriend 的 pending 预检、RemoveFriend 的 friendEdgeExists、
// Block 的名额探针)刻意留在本函数之外:它们挡的是"要不要 ensure",重试时不必重判 ——
// 权威复核都在 body 里、守卫之后。
func (r *FriendRepo) runGuardedWrite(ctx context.Context, playerA, playerB uint64, body guardedWriteBody) error {
	var lastErr error
	for attempt := 1; attempt <= capacityGuardMaxAttempts; attempt++ {
		err := r.runGuardedWriteOnce(ctx, playerA, playerB, body)
		if !errors.Is(err, errCapacityRowsMissing) {
			return err
		}
		lastErr = err
		if attempt < capacityGuardMaxAttempts {
			// 让"回收竞态"可观测:正常情况下这条日志应当极少出现,持续出现说明回收的保留期
			// 配得太短,或者有回收之外的东西在删 friend_capacity。玩家 id 只进日志、不进指标 label。
			// 按仓内先例用 Errorf 打 "WARN " 前缀。
			logx.WithContext(ctx).Errorf("[friend] WARN 容量守卫缺行(多半是 sweep 回收竞态),重新 ensure 后重试 "+
				"players=(%d,%d) attempt=%d/%d: %v", playerA, playerB, attempt, capacityGuardMaxAttempts, err)
		}
	}
	// 每一遍都缺行:两行各至多被回收一次,3 遍之内必过(顶部锁序说明 (5)),
	// 所以这不是回收竞态能解释的,照旧 fail-closed(logic 定性 ErrStorage)。
	return lastErr
}

// runGuardedWriteOnce 跑一遍"ensure → 事务"。拆成独立函数是为了让 defer tx.Rollback()
// 在**每一遍**结束时就执行 —— 写在 runGuardedWrite 的循环体里,defer 会攒到整个函数返回,
// 前面各遍那些已经没用的事务(连同它们的连接)会被一直占到后续各遍结束。
func (r *FriendRepo) runGuardedWriteOnce(ctx context.Context, playerA, playerB uint64, body guardedWriteBody) error {
	// 容量行在主事务之前、按 player_id 升序自动提交地补齐(顶部锁序说明 (1))。
	if err := r.ensureFriendCapacityRows(ctx, playerA, playerB); err != nil {
		return err
	}

	tx, err := r.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	// Commit 成功之后的 Rollback 返回 sql.ErrTxDone,无害,照 database/sql 的惯用法不检查。
	defer tx.Rollback()

	// ① 容量守卫:本事务的第一把锁(顶部锁序说明 (2)(4))。
	counts, err := lockCapacityRows(ctx, tx, playerA, playerB)
	if err != nil {
		return err
	}
	if err := body(ctx, tx, counts); err != nil {
		return err
	}
	return tx.Commit()
}

// beginWriteTx 开一个 friend 写事务。隔离级别固定 READ COMMITTED,理由见 friendWriteTxIsolation。
func (r *FriendRepo) beginWriteTx(ctx context.Context) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, &sql.TxOptions{Isolation: friendWriteTxIsolation})
}

// lockCapacityRows 取容量守卫:对给定玩家的 friend_capacity 行按 player_id **升序**加 X 锁,
// 返回各自当前的 friend_count。
//
// 这是每个写事务的第一把锁(顶部锁序说明 (2)(4))。升序是防 ABBA 的全部依据:
// 只要所有路径都按同一个全序取锁,涉及同一个玩家的事务就不可能互等成环。
// 缺行返回错误(用 %w 包着 errCapacityRowsMissing)而不是当 0。ensure 刚刚补过行、此时还缺行,
// 现在有一个**预期内**的来源:sweep 的 SweepIdleCapacityRows 会回收陈旧的零好友行,它可以
// 插在 ensure 与守卫之间,也可以在守卫的 FOR UPDATE 正等着这行时把它删掉。单次缺行因此
// 不是故障 —— runGuardedWrite 认这个哨兵,回到事务外重新 ensure 并重试(至多重试两次,
// 共 capacityGuardMaxAttempts 遍)。每一遍都缺行才是不变量破裂(有回收之外的东西在删这张表,或者表被别的服务动了):
// 哨兵原样上抛、logic 定性 ErrStorage,fail-closed。
// 无论哪种,都绝不能"当作 0"继续写,那会把好友硬上限凭空放宽一轮。
//
// 实现是**逐个 id 的主键点查**(顶部锁序说明 (6)),不是一条 `IN (...) FOR UPDATE`:后者在小表上被规划成
// PRIMARY 全索引扫描,锁集越出这一对玩家。点查的顺序就是取锁顺序,所以这里按 ascendingUniqueIDs 的升序逐个查。
// 遇到第一个缺行就返回:事务随后由 runGuardedWriteOnce 回滚,不必再去锁后面的行。
func lockCapacityRows(ctx context.Context, tx *sql.Tx, playerIDs ...uint64) (map[uint64]uint32, error) {
	ids := ascendingUniqueIDs(playerIDs)
	counts := make(map[uint64]uint32, len(ids))
	for _, id := range ids {
		var count uint32
		err := tx.QueryRowContext(ctx, lockCapacityRowSQL, id).Scan(&count)
		switch {
		case err == nil:
			counts[id] = count
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("%w: players %v (missing %d)", errCapacityRowsMissing, ids, id)
		default:
			return nil, fmt.Errorf("lock friend capacity row %d: %w", id, err)
		}
	}
	return counts, nil
}

// 守卫之后的三条锁定点查。写成包级常量是为了让
// friend_guard_lock_order_mysql_test.go 的 TestLockingStatementsArePrimaryKeyPointLookups 对**生产代码
// 实际执行的那一条** SQL 做 EXPLAIN —— 测试里另抄一份,两边漂移时这条回归就失去意义。
// 三条的 WHERE 都必须是**完整主键的等值条件**(顶部锁序说明 (6))。
const (
	lockCapacityRowSQL   = "SELECT friend_count FROM friend_capacity WHERE player_id = ? FOR UPDATE"
	lockBlockRowSQL      = "SELECT 1 FROM friend_block WHERE player_id = ? AND blocked_player_id = ? FOR UPDATE"
	lockFriendEdgeRowSQL = "SELECT 1 FROM friend WHERE player_id = ? AND friend_player_id = ? FOR UPDATE"
)

// ensureCapacityRowSQL:事务外补容量行。ODKU 的 no-op 更新保证"行已存在时什么都不改",
// 用它而不是 INSERT IGNORE 的原因见 ensureFriendCapacityRow。
const ensureCapacityRowSQL = "INSERT INTO friend_capacity (player_id, friend_count, created_ms) VALUES (?, ?, ?)" +
	" ON DUPLICATE KEY UPDATE player_id = player_id"

// lockRowExists 执行一条"完整主键等值 + FOR UPDATE"的点查,报告该行是否存在。
// RC 下未命中不加任何锁;命中只锁这一行的主键记录,不碰二级索引。
func lockRowExists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var probe int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&probe)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// blockedEitherWay 报告两人之间是否存在任一方向的拉黑(守卫之后的锁定读)。
//
// **两次主键点查,不是一条 OR**(顶部锁序说明 (6))。原先的 `(a,b) OR (b,a)` 在真库上被规划成
// idx_blocked_player 覆盖索引全扫描,锁住了别的玩家对的二级索引项,与那一对的 Unblock 反序成环(1213,已实证)。
// 两个方向的点查都落在"本对玩家"的主键行上,而所有写这两行的事务都持有同一对容量守卫 ——
// 唯一不拿守卫的写者是 Unblock(单条按主键 DELETE,先主键后二级索引),本函数只锁主键、不锁二级索引,
// 与它不可能反序。先查哪个方向无关紧要:同一对玩家的守卫已把这类事务串行化。
// 任一方向命中即返回 true(调用方随即回滚),另一方向不再查。
func blockedEitherWay(ctx context.Context, tx *sql.Tx, a, b uint64) (bool, error) {
	for _, dir := range [2][2]uint64{{a, b}, {b, a}} {
		found, err := lockRowExists(ctx, tx, lockBlockRowSQL, dir[0], dir[1])
		if err != nil {
			return false, fmt.Errorf("check block %d->%d: %w", dir[0], dir[1], err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// friendEdgeExistsForUpdate 报告两人之间是否存在任一方向的好友边(守卫之后的锁定读)。
// 与 blockedEitherWay 同理写成两次主键点查:原先的 OR 形式被规划成 idx_friend_player 覆盖索引全扫描,
// 与 RemoveFriend / Block 删边的 DELETE(先主键后二级索引)反序成环,真库上复现过 1213。
func friendEdgeExistsForUpdate(ctx context.Context, tx *sql.Tx, a, b uint64) (bool, error) {
	for _, dir := range [2][2]uint64{{a, b}, {b, a}} {
		found, err := lockRowExists(ctx, tx, lockFriendEdgeRowSQL, dir[0], dir[1])
		if err != nil {
			return false, fmt.Errorf("check friend edge %d->%d: %w", dir[0], dir[1], err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// friendEdgeExists 是事务外的普通读版本(RemoveFriend 的 F2-15 前置判定用,见那里的理由)。
func (r *FriendRepo) friendEdgeExists(ctx context.Context, a, b uint64) (bool, error) {
	var probe int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM friend
		 WHERE (player_id=? AND friend_player_id=?) OR (player_id=? AND friend_player_id=?)
		 LIMIT 1`, a, b, b, a).Scan(&probe)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("probe friend edge %d-%d: %w", a, b, err)
}

// deleteFriendEdges 删双向好友边,并**按 RowsAffected 给对应的 friend_count 减 1**。
//
// ⚠ 这个减法是 RemoveFriend 与 Block(F2 §3.4 ⑤)共用的同一段业务真相,所以只写一份:
// 漏减会让计数单调偏高,玩家永远加不满好友,而且没有任何报错 —— 表现是"明明只有 3 个好友,
// 却说列表满了"。按 RowsAffected 判而不是无条件减:单边残留时只减真正删掉的那一边。
// 下溢保护写成 `AND friend_count > 0` 而不是 IF():uint 列减到负数会回绕成天文数字、
// 直接把上限判定废掉,两种写法都挡得住,但 WHERE 版本能通过 RowsAffected **看见**这件事
// (详见下面那条日志)。调用方必须已持有双方的容量守卫。
//
// nowMs 由调用方给(RemoveFriend 在 body 里取,Block 复用它那个"一个事务只取一次"的时刻),
// 减计数时一并写进 created_ms:保留期要从"最近一次 friend_count 减少"起算,否则老玩家删掉最后
// 一个好友的那一刻,他的行(0, 很老的 created_ms)立即就是回收候选,陈旧 ensure 会把计数
// 永久写大 1(完整交错见顶部锁序说明 (5) 的"为什么删行也不会让计数偏大")。
// 这偏离了收尾批冻结规格里"created_ms 只在 INSERT 写"的表述,原因即上述交错,已登记进交付说明。
func deleteFriendEdges(ctx context.Context, tx *sql.Tx, a, b uint64, nowMs int64) error {
	for _, edge := range [][2]uint64{{a, b}, {b, a}} {
		result, err := tx.ExecContext(ctx,
			"DELETE FROM friend WHERE player_id=? AND friend_player_id=?", edge[0], edge[1])
		if err != nil {
			return fmt.Errorf("delete friend edge %d->%d: %w", edge[0], edge[1], err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted == 1 {
			// 用 `AND friend_count > 0` 代替 IF() 做下溢保护:两者的净效果一样(减不到负数),
			// 但这样能通过 RowsAffected 看见"被夹住"这件事。夹住 = 计数与 friend 表的权威边数
			// 已经脱节,是本域最要命的那类不变量破裂,静默抹平会让它只能靠玩家报"好友加不满"暴露。
			// 不 fail-closed:此刻边已经删掉了,回滚整个事务反而把一次正确的删好友变成失败。
			res, err := tx.ExecContext(ctx,
				"UPDATE friend_capacity SET friend_count = friend_count - 1, created_ms = ? WHERE player_id = ? AND friend_count > 0",
				nowMs, edge[0])
			if err != nil {
				return fmt.Errorf("decrement friend count %d: %w", edge[0], err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				logx.WithContext(ctx).Errorf("[friend] friend_count 下溢被拦截:player=%d 已删掉一条边但计数已经是 0,"+
					"说明 friend_capacity 与 friend 表的权威边数脱节(排查:SELECT COUNT(*) FROM friend WHERE player_id=%d)",
					edge[0], edge[0])
			}
		}
	}
	return nil
}

// ascendingUniqueIDs 把玩家 id 规整成升序去重切片。锁序纪律的唯一实现点:
// ensure 与守卫都用它,两处的顺序必然一致(各写一遍排序才是危险的)。
func ascendingUniqueIDs(playerIDs []uint64) []uint64 {
	switch len(playerIDs) {
	case 0:
		return nil
	case 1:
		return []uint64{playerIDs[0]}
	}
	first, second := playerIDs[0], playerIDs[1]
	if first > second {
		first, second = second, first
	}
	if first == second {
		return []uint64{first}
	}
	// 本域所有写路径至多涉及两个玩家;传进第三个 id 是调用方写错了,
	// 与其悄悄忽略,不如让它在这里就明显不对(多余的 id 不会被加锁,守卫的 len 校验会报错)。
	return []uint64{first, second}
}

func (r *FriendRepo) invalidateCaches(ctx context.Context, keys ...string) error {
	var result error
	for _, key := range keys {
		// 脚本固定返回 1,返回值本来就丢弃;真正要传播的是"失效没做成"这件事 ——
		// 吞掉它会让下一次 miss 把写入前的旧快照按旧 generation 合法回填。
		// 两个 KEYS 共享同一个 hash tag(见 friendListKey 的说明),所以在 Cluster 上同 slot。
		if _, err := r.rdb.EvalCtx(
			ctx,
			invalidateFriendCacheScript,
			[]string{friendCacheGenerationKey(key), key},
		); err != nil {
			result = errors.Join(result, fmt.Errorf("invalidate friend cache key %s: %w", key, err))
		}
	}
	return result
}

// invalidateCachesAfterCommit 用在**写事务已经提交之后**。
//
// 与 invalidateCaches 的唯一区别是它恒返回 nil,只把失败打进日志。理由:此刻写入已经
// 落库且不可撤销,把失效失败上抛会让 logic 把一次**成功的写**定性成 fault 码 ErrStorage,
// 后果是三重的 —— 客户端看到"存储故障"并重试(撞上 ErrRequestAlreadySent)、
// S2C 推送被一起跳过、rpc_inband_fault 多一条假告警。那比缓存陈旧严重得多。
// 代价:失效没做成时该键最多陈旧一个 CacheTTL(默认 30 分钟)——
// generation CAS 只保证"不被更旧的快照覆盖",不保证及时刷新,所以陈旧窗口的上界就是 TTL。
// 这条降级按 AGENTS §11.3 是"记录并可观测"而不是静默:日志带上失败的键名。
func (r *FriendRepo) invalidateCachesAfterCommit(ctx context.Context, keys ...string) {
	if err := r.invalidateCaches(ctx, keys...); err != nil {
		logx.WithContext(ctx).Errorf("[friend] 写已提交但缓存失效失败(该键最多陈旧一个 CacheTTL)keys=%v: %v", keys, err)
	}
}

// ensureFriendCapacityRows 在进主事务之前把双方的 friend_capacity 行补齐。
//
// 这里原先还有一道"回填就绪闸"(读 guild_schema_migration 的 friend_capacity_backfill_v1 标记),
// 已按 F1 §6.1 退役,原因是它在本服务的任何合法配置下都不可能生效、也不再有它要挡的风险:
//   - 它读的台账表按 D-14 第 8 条"存量不动"留在旧共享库 mmorpg,而 friend 独占库 mmorpg_friend
//     由 go/schemamigrate 从基线一次建全,库里没有这张表;config.Validate 又断言
//     MySQL.DBName == data.DatabaseName,所以那条查询恒为"表不存在"(D-14 第 6 条也禁跨库访问)。
//   - 它要挡的是"在 mmorpg 里给历史 friend 边原地回填 friend_capacity、DDL 隐式提交造成半迁移"
//     这一个具体场景。新库里没有任何历史边,不存在需要回填的存量,也就没有"半份 0"可暴露。
//   - 一个名字叫"就绪闸"、实际永远放行的机制比没有它更坏:下一个人会以为写路径有这层保护
//     (AGENTS.md §11.2 KISS/YAGNI、§11.3 不得静默降级)。
//
// **退役的只有这道闸**。D-10 的实质不变量原样保留:friend_capacity 的显式计数锁行仍是好友数的
// 硬上限;缺行时仍按 friend 表的权威边数建行、**绝不猜 0**(猜 0 会让已满的列表被再撑大一轮);
// 先在事务外补齐行、再在事务内按 player_id 升序 FOR UPDATE 锁双方容量行的顺序也不变。
//
// created_ms 是"本行被(重新)建出、或最近一次 friend_count 减少的时刻";回收的保留期从这一刻起算。
// 写它的只有两处:这里的 INSERT,以及 deleteFriendEdges 减计数的那条 UPDATE。补行撞上已有行时
// ODKU 的更新是 no-op(player_id = player_id),既有行的 created_ms 不会被 ensure 刷新(刷新会让陈旧的零好友行永远
// 不过期)。为什么减计数也要写:本函数的 COUNT 与 INSERT 是两条各自自动提交的语句、不原子,
// 不刷新的话"刚减到 0 的老行"立刻可回收,陈旧的 INSERT 会把计数永久写大 1 —— 完整交错与残留
// (陈旧窗口跨过整个 RetentionDays 才可能复现,视为不可达)见顶部锁序说明 (5)。
// 它只服务于 sweep 的容量行回收(SweepIdleCapacityRows:friend_count = 0 且 created_ms 早于保留期
// 的行),不参与任何业务判定。
//
// 1213:已知形态(回收留下的 delete-marked 记录上并发补行 S→X)已由 ODKU + RC 从根上消掉(顶部锁序说明 (5)),
// 下面的重试只是纵深防御。万一出现新的成环形态,此时还在事务外、没有任何副作用,所以对**单个玩家**的
// "COUNT + INSERT"这一对语句有上限地重跑(ensureDeadlockMaxAttempts):COUNT 必须一起重跑,
// 否则重试写下去的是上一遍的陈旧边数。只认 1213,其它错误一律原样上抛;每遍之前先看 ctx。
func (r *FriendRepo) ensureFriendCapacityRows(ctx context.Context, playerIDs ...uint64) error {
	for _, playerID := range ascendingUniqueIDs(playerIDs) {
		var err error
		for attempt := 1; attempt <= ensureDeadlockMaxAttempts; attempt++ {
			// 请求已超时 / 已取消就别再去抢锁:带着取消的 ctx 重试只会换来一条含糊的驱动错误,
			// 把真正的原因(上一遍的 1213)盖掉。
			if ctxErr := ctx.Err(); ctxErr != nil {
				if err != nil {
					return fmt.Errorf("%w(重试前 ctx 已结束: %v)", err, ctxErr)
				}
				return ctxErr
			}
			err = r.ensureFriendCapacityRow(ctx, playerID)
			if !isMySQLDeadlock(err) {
				break
			}
			ensureDeadlockRetries.Add(1)
			if attempt < ensureDeadlockMaxAttempts {
				// 2026-09-21 起 ensure 改用 ODKU + 连接级 RC,已知会走到这里的形态(回收后并发补行的
				// S→X 升级)已从根上消掉;这条重试只剩纵深防御的意义。**正常运行时应当永远看不到它** ——
				// 看到了就说明出现了新的成环形态,要按事故报告的方法抓 LATEST DETECTED DEADLOCK 查清。
				logx.WithContext(ctx).Errorf("[friend] WARN ensure 容量行撞上 1213(预期不应再发生,请抓 INNODB STATUS 排查),"+
					"重跑 COUNT+INSERT player=%d attempt=%d/%d: %v", playerID, attempt, ensureDeadlockMaxAttempts, err)
			}
		}
		if err != nil {
			// 含"重试用尽仍是 1213":fail-closed,原样上抛(logic 定性 ErrStorage)。
			return err
		}
	}
	return nil
}

// ensureFriendCapacityRow 对单个玩家跑一遍"按权威边数算初值 → 补行(ODKU)"。
// 拆出来只为让 ensureFriendCapacityRows 的 1213 重试能把这一对语句**整体**重跑。
//
// 补行用 `INSERT … ON DUPLICATE KEY UPDATE player_id = player_id`,**不是** INSERT IGNORE:
// 两者在"行已存在"时都什么也不改(ODKU 的更新是 no-op,不会覆盖已有的 friend_count / created_ms),
// 区别只在锁 —— 主键重复时 INSERT IGNORE 拿 **S** 锁,ODKU 直接拿 **X**。回收刚删掉这一行(delete-marked、
// 尚未提交)时,多个并发补行若各拿 S、之后都要升 X 就会互等成环(2026-09-21 真库复现);都直接要 X 则只是排队,
// 先到者插入、后到者看到它的新行走 no-op 更新。代价:行已存在时并发补行也串行(自动提交,持锁极短)。
func (r *FriendRepo) ensureFriendCapacityRow(ctx context.Context, playerID uint64) error {
	// 缺行几乎都是从未有过好友的新玩家(COUNT 返回 0)。但初值仍然只能从 friend 表
	// 的权威边数来,**绝不能直接写 0**:将来若有任何路径先写了 friend 边再补容量行,
	// 猜 0 会让这个玩家的硬上限凭空放宽一轮,且全程零报错。
	// 普通一致性读不持有 gap lock;所有写路径都先 ensure 同一 capacity 行,
	// 再在事务里加锁并更新计数。
	var authoritativeCount uint32
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend WHERE player_id = ?", playerID).Scan(&authoritativeCount); err != nil {
		return fmt.Errorf("count authoritative friends for player %d: %w", playerID, err)
	}
	if _, err := r.db.ExecContext(ctx, ensureCapacityRowSQL,
		playerID, authoritativeCount, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("ensure friend capacity row for player %d: %w", playerID, err)
	}
	return nil
}

// isMySQLDeadlock 报告 err 是不是 InnoDB 的死锁牺牲(错误号 1213)。errors.As 能穿透本层的 %w 包装。
// 只认错误号、不做文本兜底:生产路径上的包装全是 %w,链不会断;文本匹配会把别处拼进来的
// "1213" 字样误判成可重试。
func isMySQLDeadlock(err error) bool {
	var my *drivermysql.MySQLError
	return errors.As(err, &my) && my.Number == 1213
}

// ── Cache helpers ──────────────────────────────────────────────

// cacheKeyMutex 取该 cacheKey 专属的回填锁。
//
// 原先这里是**一把进程级互斥锁**,把全进程所有玩家、两条读路径的回填串行成一条队列:
// 缓存冷时(进程刚起、FriendRedis 重启、写路径刚失效一批 generation)每个请求都要排在
// 一次完整 MySQL 往返后面 —— 而 GetFriendList 是每次登录都会发生的。
// 按 key 分组之后,不同玩家的回填互不阻塞,防击穿的语义(同一个 key 只打一次 MySQL)不变。
// generation + Lua CAS 的竞态防护与这把锁正交,一行都不用动。
// 锁条目不回收:key 的基数上界是在线玩家数 × 2,远小于会成为问题的量级(KISS)。
func (r *FriendRepo) cacheKeyMutex(cacheKey string) *sync.Mutex {
	mu, _ := r.cacheLoadMu.LoadOrStore(cacheKey, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// loadVersionedFriendCache 阻止经典 cache-aside 竞态:reader miss 后读到旧
// MySQL,writer 提交并失效缓存,reader 最后才 SET 旧快照。reader 只有在 DB
// 读取前后 generation 未变化时才能回填;写路径提交后原子 INCR+DEL。
//
// 失败语义的取舍(AGENTS §11.3):**读缓存失败与读 generation 失败保持 fail-closed**,
// 理由是防 MySQL 雪崩 —— 改成"降级为 miss 直连 MySQL"的话,Redis 整体不可用时会把全服的
// 列表读同时压到 MySQL 上。代价是 FriendRedis 的可用性直接决定好友列表的可用性,
// 所以 FriendRedis 必须是独立实例且配 noeviction(etc/friend.yaml 已注明)。
// **回填失败不在此列**:那时权威结果已经在手,见下方那处的说明。
func loadVersionedFriendCache[T any](
	ctx context.Context,
	r *FriendRepo,
	cacheKey string,
	loader func(context.Context) (T, error),
) (T, error) {
	var zero T
	readCache := func() (T, bool, error) {
		// ⚠ go-zero 的 GetCtx 把 redis.Nil **吞成 ("", nil)**(core/stores/redis/redis.go
		// 的 GetCtx:errors.Is(err, red.Nil) → return "", nil),所以这里不能再用
		// err == redis.Nil 判未命中 —— 那条分支永远走不到。
		// 必须先判 err(真故障),再把空串当未命中:JSON 序列化的结果最短也是 "null" /
		// "[]" / "{}",永不为空串,所以"空串"与"命中了一个空值"不会混。
		payload, err := r.rdb.GetCtx(ctx, cacheKey)
		if err != nil {
			return zero, false, fmt.Errorf("read friend cache %s: %w", cacheKey, err)
		}
		if payload == "" {
			return zero, false, nil
		}
		var value T
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return zero, false, fmt.Errorf("decode friend cache %s: %w", cacheKey, err)
		}
		return value, true, nil
	}

	if value, found, err := readCache(); err != nil || found {
		return value, err
	}
	mu := r.cacheKeyMutex(cacheKey)
	mu.Lock()
	defer mu.Unlock()
	if value, found, err := readCache(); err != nil || found {
		return value, err
	}

	generation, err := r.rdb.GetCtx(ctx, friendCacheGenerationKey(cacheKey))
	if err != nil {
		return zero, fmt.Errorf("read friend cache generation %s: %w", cacheKey, err)
	}
	// generation key 从未被写过时 GetCtx 返回 ("", nil)(见 readCache 里的说明),
	// 这里**必须**显式补成 "0":fill 脚本里 GET 不到 generation 时也按 "0" 比较,
	// 把空串传下去会和脚本里的 "0" 比不上 → 脚本恒 return 0、缓存**永远写不进去**,
	// 而且一个错都不报(EvalCtx 成功、返回值本来就丢弃)。这是纯静默故障,不能省。
	if generation == "" {
		generation = "0"
	}
	value, err := loader(ctx)
	if err != nil {
		return zero, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return zero, fmt.Errorf("encode friend cache %s: %w", cacheKey, err)
	}
	// ARGV 一律显式转成 string:go-zero 直接把 args 交给 go-redis 的 Eval,而脚本里
	// ARGV[1] 是拿来和 GET 回来的 generation 做**字符串**比较的,ARGV[3] 也要 tonumber 得动。
	if _, err := r.rdb.EvalCtx(
		ctx,
		fillFriendCacheScript,
		[]string{friendCacheGenerationKey(cacheKey), cacheKey},
		generation,
		string(payload),
		strconv.FormatInt(r.defaultTTL.Milliseconds(), 10),
	); err != nil {
		// 回填失败**不影响本次返回**:MySQL 的权威结果已经在手,丢掉它只会让调用方拿到
		// fault 码 ErrStorage(本域唯一的 fault),表现成"好友系统挂了"——
		// 而真正挂的只是一个纯加速用的私有缓存。代价:下一次读仍是 miss,再打一次 MySQL。
		// 与 session_reader.go 顶部的降级口径一致:展示路径上的 Redis 故障不得让列表整体失败。
		logx.WithContext(ctx).Errorf("[friend] 回填缓存失败,本次直接返回 MySQL 权威结果 key=%s: %v", cacheKey, err)
	}
	return value, nil
}

// ── MySQL queries ──────────────────────────────────────────────
//
// 列表类读一律带 `LIMIT ?`(F2-7,值是 Friend.ListReadHardLimit)。
// 它是**防御性**上限而不是分页:正常数据量由 MaxFriends / MaxIncomingRequests 封顶,远低于它;
// 真被截断说明容量表或上限判定已经坏了,此时宁可少返回几行,也不要把几万行塞进一个 gate 包体。
// 刻意不加 ORDER BY:两条查询都是索引区间扫描(friend 走 PK 的 player_id 前缀、
// friend_request 走 idx_to_player),返回顺序本身就是稳定的,再加排序只是给每次读增加一次 filesort。

func (r *FriendRepo) loadFriendListFromMySQL(ctx context.Context, playerID uint64) ([]FriendEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT friend_player_id, since_ms FROM friend WHERE player_id = ? LIMIT ?",
		playerID, r.listReadHardLimit)
	if err != nil {
		return nil, fmt.Errorf("query friends %d: %w", playerID, err)
	}
	defer rows.Close()

	var friends []FriendEntry
	for rows.Next() {
		var f FriendEntry
		// since_ms 在表里是 uint64、在 wire proto 里是 int64;这里扫进 int64 的
		// FriendEntry.SinceMs,靠的是"毫秒时间戳不会超过 2^63"这个前提(约 2.9 亿年)。
		if err := rows.Scan(&f.FriendPlayerID, &f.SinceMs); err != nil {
			return nil, fmt.Errorf("scan friend: %w", err)
		}
		friends = append(friends, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if friends == nil {
		friends = []FriendEntry{}
	}
	return friends, nil
}

func (r *FriendRepo) loadPendingRequestsFromMySQL(ctx context.Context, playerID uint64) ([]FriendRequestEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT from_player_id, to_player_id, request_time_ms, status
		 FROM friend_request WHERE to_player_id = ? AND status = ? LIMIT ?`,
		playerID, requestStatusPending, r.listReadHardLimit)
	if err != nil {
		return nil, fmt.Errorf("query pending requests %d: %w", playerID, err)
	}
	defer rows.Close()

	var requests []FriendRequestEntry
	for rows.Next() {
		var req FriendRequestEntry
		if err := rows.Scan(&req.FromPlayerID, &req.ToPlayerID, &req.RequestTimeMs, &req.Status); err != nil {
			return nil, fmt.Errorf("scan friend request: %w", err)
		}
		requests = append(requests, req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if requests == nil {
		requests = []FriendRequestEntry{}
	}
	return requests, nil
}
