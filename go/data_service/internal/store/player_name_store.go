package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// 玩家名字注册表(设计 docs/design/guild-phase2/03-names.md §3.3 / §3.4)。
//
// 不变量,按重要性排序:
//  1. **名字全服唯一**。唯一性只由 player_name 的**主键 name_norm** 保证(见 schema.go 的
//     playerNameBootstrapDDL;2026-09-29 之前它是 name_norm 上的一条二级 UNIQUE KEY,
//     换成主键是为了根治名字抢注的死锁,理由与实测数据都在那里)。本 store 从不用"先 SELECT
//     看有没有人占、没人占就 INSERT"来判重 —— 那在并发建角下必漏:两个请求同时查到"没人占"。
//     判重的唯一形式是"直接写,撞了键再去看撞在谁身上"(写法是 no-op 的 ODKU,撞键 = 受影响 0 行,
//     见 Reserve)。
//  2. **占用与释放都幂等**。同 player_id 同 name_norm 再 Reserve 一次是成功
//     (ReserveAlreadyOwned),不是错误 —— login 对超时会原样重试。Release 删不到行
//     也是成功(ReleaseAbsent)。
//  3. **Taken 必须能回出 owner**。login 靠它识别"上一次建角的响应丢了、这是同一个人在重试"
//     (§3.11 第 6c' 步)。owner 只给服务端判定用,**禁止下发客户端**(会变成按名字查人)。
//  4. **释放有时间窗**。无 admin token 的释放只能删 created_ms 落在窗口内的行
//     (minCreatedMs > 0),防的是"删掉别人刚建好的号的名字"。窗口由 logic 按
//     PlayerName.ReleaseWindow 算好后传进来,本层只忠实执行。
//
// 归一化不在这一层:display(name)与 norm(name_norm)都由 go/shared/playername 的
// Normalize 一次算出后传进来,库只做逐字节比较(表 COLLATE=utf8mb4_bin)。本 store
// 刻意不 import 规则包,免得"什么算同一个名字"出现第二个答案。
//
// 【崩溃窗口】(§3.7)名字先占、角色后建,两步不在一个事务里(跨 MySQL 全局库与
// Redis 账号 blob,本来也做不成一个事务)。中间崩掉会留下"名字被不存在的角色占住"的
// 孤儿行。补偿路径**全部在 login**:
//   - INSERT 已提交、login 超时 → login 以同 (player_id, name) 重试一次,拿到
//     ReserveAlreadyOwned = 成功;
//   - 仍然结果未知 / 后续步骤失败 → login 发一次条件 Release,并在约 10s 后再发一次
//     (覆盖"Release 先于在途 INSERT 提交"的竞态,两次都落在 10 分钟窗口内)。第二次 Release 若赶上
//     purge 滞后,会在第一次 Release 留下的删除标记记录上取锁、可能排队,见 Release 的「锁序」;
//   - 两次都失败或 login 自己也崩了 → ERROR 日志 + orphan 计数,运维带 x-admin-token
//     调 ReleasePlayerName(minCreatedMs=0,不限登记时间)清掉。
//
// 服务端**刻意不做**"ctx 一超时就补偿删除":login 的同名重试可能恰好在补偿 DELETE
// 之前拿到 AlreadyOwned(=建角成功),随后那条补偿就会把在役角色的名字删掉。补偿只能
// 由知道"这次建角到底成没成"的 login 发起。
//
// 同理**不做**自动孤儿清扫:没有权威的"角色是否存在"信号(账号 blob 在 Redis 12h 过期、
// player_to_account 写失败不阻断建角),照它们批量释放会拿走在役角色的名字。

const (
	// PlayerNameBatchLimit 是 BatchGet 单次可查的 id 上限(契约 §3.1)。
	// 数字只在这里写一次:logic / server 层要拒绝超限请求时引用本常量,别再抄一份 500。
	PlayerNameBatchLimit = 500

	// playerNameReserveAttempts Reserve 的尝试上限。会重来的只有两种可预期情况:
	// MySQL 1213/1205(锁冲突,isRetryableMySQL;重来前按 playerNameLockRetryBackoffMin/Max 抖动退避)
	// 与"撞键之后两条等值读都为空"(并发 Release 把行删了,名字已空出,这一轮直接重插、不退避)。
	// 其余一律立即返回。
	//
	// **下限是 2,不许调成 1**(player_name_store_test.go 机械守住):
	//   - 名字抢注那一类环已经在**结构上**根治了(name_norm 当聚簇主键 + ODKU,真库实测 0/15,
	//     见 schema.go 的 playerNameBootstrapDDL),但 1213 并没有归零:剩下的是锁继承(InnoDB 固有)
	//     与"同一个 player_id 上的并发**不同名**写入"两类,后者只在 ID 复用事故下可达,见 Reserve 的「残余」。被牺牲的那一方要靠
	//     **至少一次**重来才能拿到正确终局 Taken;上限为 1 时 1213 会直接作为存储错误(RPC 失败)返回给 login,
	//     玩家看到的是建角失败而不是"名字已被占用"。
	//   - 还有一条与死锁无关:迁移没跑过的库(或 TiDB)上表仍是旧主键形态,那里"竞争者 id 小于删除标记项
	//     原主人"必然成环(实测 5/5),重试是那个窗口里唯一的兜底。
	//   - "撞键后行已消失"的重插与锁冲突重试共用这一个预算,两者叠加在同一次调用里时只剩一次重来;
	//     所以取 3 而不是恰好 2,留一次余量。
	playerNameReserveAttempts = 3

	// playerNameReleaseAttempts Release 的尝试上限。DELETE 本身幂等,重试安全;
	// 多争取一次成功能直接少一条孤儿(失败的释放就是孤儿,见上面的崩溃窗口)。
	// 会重来的有两种可预期情况:MySQL 1213/1205(锁冲突,isRetryableMySQL;重来前抖动退避),
	// 以及"没删到、但行在且落在窗口内"(在途 INSERT 在 DELETE 之后才提交,见 Release;不退避)。
	playerNameReleaseAttempts = 3

	// playerNameLockRetryBackoffMin / Max:Reserve / Release 因 1213/1205 重来之前的等待,在 [Min, Max] 内
	// 均匀取值(jitteredBackoff)。预算:单次调用最坏多睡 (attempts-1)×Max = 100ms,是 login 单次
	// Reserve / Release 预算(playernamereg.DefaultReserveTimeout / DefaultReleaseTimeout = 1s)的十分之一;
	// 等待可被 ctx 取消,预算用完立即返回,不会睡满再去撞一个注定超时的语句。
	playerNameLockRetryBackoffMin = 10 * time.Millisecond
	playerNameLockRetryBackoffMax = 50 * time.Millisecond

	// mysqlErrDupEntry 1062 ER_DUP_ENTRY:撞主键或唯一键。
	// Reserve 改成 ODKU 之后撞键不再以 1062 报出(见 playerNameReserveSQL),这里只剩纵深防御用途。
	mysqlErrDupEntry = 1062
)

// playerNameReserveSQL 是 Reserve 的写入。ODKU 的更新 `player_id = player_id` 引用的是**已有行**自己的列值
// (不是 VALUES()),是 no-op:撞键时一个字节都不改(created_ms 也不刷新),只把"撞了"从 1062 错误变成
// 受影响 0 行,并让重复键检查取 X 而不是 S。行数口径、锁序与前提(连接不带 CLIENT_FOUND_ROWS、
// binlog_format = ROW)见 Reserve 的注释。抽成常量是为了让真库回归用例执行**同一段文本**。
const playerNameReserveSQL = "INSERT INTO player_name (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)" +
	" ON DUPLICATE KEY UPDATE player_id = player_id"

// playerNameReleaseForcedIndex 是 Release 钉死的索引名。新结构里 name_norm **就是聚簇主键**
// (schema.go 的 playerNameBootstrapDDL),所以是 PRIMARY。
//
// 为什么还要钉:WHERE 里 player_id 与 name_norm 都是等值条件,而两者各有一条唯一约束 ——
// 优化器完全可能挑 uk_player_name_owner(player_id),那就变成"先二级索引、后聚簇",与 Reserve 的
// 取锁顺序(聚簇在先)反序,两方即可成环。钉死 PRIMARY 之后两者同序。真库用例用 EXPLAIN 验。
const playerNameReleaseForcedIndex = "PRIMARY"

// playerNameReleaseSQL 是 Release 的条件删除(**新结构**:name_norm 是聚簇主键)。
// 单表 DELETE 不接受索引提示,所以写成多表 DELETE 语法,删除语义与受影响行数与单表写法相同。
// 理由见 Release 的「锁序」,真库用例用 EXPLAIN 钉住 key = PRIMARY 且是单行点查。
const playerNameReleaseSQL = "DELETE player_name FROM player_name FORCE INDEX (" + playerNameReleaseForcedIndex + ")" +
	" WHERE player_id = ? AND name_norm = ? AND created_ms >= ?"

// PlayerNameLegacyNormUniqueKey 是**旧**结构里建在 name_norm 上那条唯一键的索引名
// (旧结构 = PRIMARY KEY(player_id) + UNIQUE KEY uk_player_name(name_norm))。
//
// 生产表跑完 `data_service -migrate` 之后这条键已经不存在(名字成了主键);它只服务于
// "新二进制遇上还没迁移的表"这一个窗口 —— 见 playerNameReleaseLegacySQL 与 NewPlayerNameStore。
const PlayerNameLegacyNormUniqueKey = "uk_player_name"

// playerNameReleaseLegacySQL 是**旧结构**上的 Release(主键还是 player_id 时用)。
//
// 旧结构里名字只是二级唯一索引,Reserve 的 ODKU 撞名时先锁 uk_player_name 项、再锁聚簇记录;
// 所以 Release 也必须先唯一键、后聚簇,否则两方反序成环。按主键删(优化器的默认选择,聚簇读省一次
// 回表)正好是反的,只能 FORCE INDEX 钉死。
//
// 占位符顺序与 playerNameReleaseSQL **逐字相同**(player_id, name_norm, created_ms):
// 两条语句由同一段 Release 代码绑参,顺序一旦分叉就是"删谁"与"时间窗下界"错位,而且不会报错。
// player_name_store_test.go 的 TestPlayerNameReleaseStatementsShareArgumentOrder 机械守住。
const playerNameReleaseLegacySQL = "DELETE player_name FROM player_name FORCE INDEX (" + PlayerNameLegacyNormUniqueKey + ")" +
	" WHERE player_id = ? AND name_norm = ? AND created_ms >= ?"

// playerNameShapeProbeTimeout 是建 store 时探一次表形状的时限(与 SnapshotStore 的
// uniqueGuidKeyProbeTimeout 同口径):两条 INFORMATION_SCHEMA 点查,不该把服务启动拖住。
const playerNameShapeProbeTimeout = 5 * time.Second

var (
	// ErrPlayerNameInvalidArgument 入参在库层就被拒:player_id=0 或 name_norm 为空。
	//
	// 这是纵深防御,不是主校验(主校验在 logic 的 Normalize + server 的参数检查):
	// player_id=0 一旦插进去,就是一行永远没有主人、也没人会去释放的行,而它照样占着
	// 一个名字。宁可在最靠近库的地方 fail-closed。
	ErrPlayerNameInvalidArgument = errors.New("player_name: player_id must be non-zero and name_norm must be non-empty")

	// ErrPlayerNameBatchTooLarge 一次 BatchGet 给的 id 超过 PlayerNameBatchLimit。
	// 正常路径下 logic 已经去重、去 0 并截断,走到这里说明调用方漏了那一步。
	ErrPlayerNameBatchTooLarge = errors.New("player_name: batch get id count exceeds PlayerNameBatchLimit")
)

// ReserveOutcome 是一次 Reserve 在**库里**的终局。
//
// 它刻意不等于 RPC 的 result(playername.ReserveOK/Taken/Invalid):那层映射属于 logic,
// 而且两边基数不同 —— Inserted 与 AlreadyOwned 在 RPC 上都是 0,但指标和日志需要分开看
// (AlreadyOwned 突然变多 = 上游在大量重试)。
type ReserveOutcome uint8

const (
	// ReserveInserted 本次新插入,名字从此归这个 player_id。
	ReserveInserted ReserveOutcome = iota + 1
	// ReserveAlreadyOwned 同 player_id 同 name_norm 的行已经在了:幂等重试,算成功。
	ReserveAlreadyOwned
	// ReserveTaken name_norm 已被**别的** player_id 占用,占用者随返回值给出。
	ReserveTaken
	// ReserveConflict 这个 player_id 已经登记了**另一个**名字。
	// v1 不支持改名,所以它只可能来自 player_id 复用(发号器被重置)这类事故,
	// 调用方必须当故障处理,绝不能"那就把旧的删了"—— 旧名字属于一个在役角色。
	ReserveConflict
)

// String 仅供日志与指标 label(低基数,四个固定值)。
func (o ReserveOutcome) String() string {
	switch o {
	case ReserveInserted:
		return "inserted"
	case ReserveAlreadyOwned:
		return "already_owned"
	case ReserveTaken:
		return "taken"
	case ReserveConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// ReleaseOutcome 是一次 Release 在库里的终局。
type ReleaseOutcome uint8

const (
	// ReleaseDeleted 行被删掉了。
	ReleaseDeleted ReleaseOutcome = iota + 1
	// ReleaseAbsent 没有 (player_id, name_norm) 这一行(或早已被删):幂等,算成功。
	ReleaseAbsent
	// ReleaseOutsideWindow 行在,但 created_ms 早于调用方给的窗口下界。
	// 调用方必须当拒绝处理(需要 x-admin-token 才能不限时间地删)。
	ReleaseOutsideWindow
)

// String 仅供日志与指标 label(低基数,三个固定值)。
func (o ReleaseOutcome) String() string {
	switch o {
	case ReleaseDeleted:
		return "deleted"
	case ReleaseAbsent:
		return "absent"
	case ReleaseOutsideWindow:
		return "outside_window"
	default:
		return "unknown"
	}
}

// PlayerNameStore 是 player_name 表的读写。
//
// 连接池独立于 snapshot / transaction_log / id_segment:建角路径上每个请求都要同步走一条
// INSERT,与快照落库、号段续段挤同一个默认 5 连接的池会互相拖慢。池大小由装配层用
// config 的 PlayerName 段覆盖 MySQLConfig.MaxOpenConn/MaxIdleConn 后传进来。
type PlayerNameStore struct {
	db *sql.DB

	// releaseSQL 是本实例要用的 Release 语句,建 store 时按探到的主键形态选定,运行期不再变
	// (与 SnapshotStore.uniqueGuidKey 同一口径:结构能力只探一次,不让写路径的行为在同一进程里跳来跳去)。
	// 新结构 = playerNameReleaseSQL,旧结构 = playerNameReleaseLegacySQL,两者占位符顺序相同。
	releaseSQL string

	// legacyKeyedByPlayerID = 探到这张表还是旧主键形态(PRIMARY KEY(player_id)),迁移没跑过或在 TiDB 上被跳过。
	// 只给日志与用例用:旧形态下名字抢注的环回到"生产常态那一路必成环"的老样子,靠有界重试吸收。
	legacyKeyedByPlayerID bool

	// lockRetries 累计 Reserve / Release 因 1213/1205 重来的次数(按实例,不是全局状态)。
	// 只给日志与真库回归用例用:只看"最终结果正确"分不清是写法真的不成环,还是环被重试悄悄吸收了。
	//
	// **新结构(name_norm 是聚簇主键)下,"抢同一个名字"这条路上的判据只有一条:三种 player_id 顺序都必须是 0 次。**
	// 2026-09-29 真库探针实测 0/15(见 schema.go 的 playerNameBootstrapDDL 里那张表),
	// 根因是聚簇索引上撞同键的删除标记记录走原地改写、根本不申请插入意向锁 —— 环的那条边不存在了。
	// 真库用例 TestPlayerName_ReserveBehindPendingReleaseOutcome 对三种顺序一律断言 0。
	// 判据**只覆盖抢名字这条路**:「残余」B 那两形(同一 player_id 上的并发不同名写入)不在它的范围里,
	// 那里的重试次数是多少本轮没有真库数据。
	//
	// 旧结构(迁移未跑 / TiDB)下不适用:那里"竞争者 id 小于删除标记项原主人"必然成环(实测 5/5),
	// 是生产常态,重试次数会是 1。用例只在新结构上跑,所以不为旧结构写判据。
	lockRetries atomic.Uint64
}

// NewPlayerNameStore 在全局库上开自己的池(与其它 store 同一套 openMySQL),并探一次
// **表的主键形态**来选定 Release 语句。形状认不出来就建不出 store(fail-closed)。
//
// 为什么要探,而不是假定表已经迁移过:Release 必须与 Reserve 的 ODKU **同序取锁**,而"同序"
// 在两种表形态下是两条不同的语句 ——
//   - 新结构 PRIMARY KEY(name_norm):名字就是聚簇键,两者都先聚簇后二级,Release 钉 PRIMARY;
//   - 旧结构 PRIMARY KEY(player_id):名字是二级唯一索引,Reserve 撞名时先锁 uk_player_name 再锁聚簇,
//     Release 必须 FORCE INDEX (uk_player_name) 才不反序。
//
// 猜错的代价是不对称的,所以只能探不能猜:在旧表上发新语句 = 与 Reserve 反序取锁,凭空多出一类两方环;
// 在新表上发旧语句 = uk_player_name 不存在,每一条 Release 以 ER_KEY_DOES_NOT_EXIST(1176)失败,
// 建角补偿释放全线失效、孤儿名字只涨不消。
//
// 探到旧形态时**不拒绝启动**:旧结构在功能上完全正确(名字仍然全服唯一),只是更容易成环,
// 而那条路上的环由有界重试兜住。拿一次确定的业务中断(CreatePlayer 全线拒绝)去换它不划算 ——
// 打一条 Error 指明"跑 -migrate 才能根治"即可。TiDB 上迁移本来就会跳过(见 ensurePlayerNameKeyedByName),
// 拒绝启动会让 player_name 在 TiDB 上彻底不可用。
//
// 新形态下还顺带探一次"name_norm 上有没有残留的二级唯一索引"(回滚做了一半的中间态)。
// 那一条只打日志、不拒启动:它不改变任何对外语义,只是把死锁形状带回来,而修它的是 -migrate。
//
// 旧形态下还要确认 uk_player_name 这个**索引名**真的在(hasUniqueIndex 同时要求 NON_UNIQUE=0):
// 存量库上 name_norm 的唯一性可能由别名索引承担,而启动期的结构守卫只看列、不看索引名。
// 名字对不上时 FORCE INDEX 会 1176,所以这一支必须 fail-closed。
func NewPlayerNameStore(cfg MySQLConfig) (*PlayerNameStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), playerNameShapeProbeTimeout)
	defer cancel()

	pkCols, err := primaryKeyColumns(ctx, db, cfg.DBName, PlayerNameTableName)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("player_name: 探测 %s 的主键形态失败(Release 的取锁顺序依赖它): %w",
			PlayerNameTableName, err)
	}

	st := &PlayerNameStore{db: db}
	switch playerNamePKShapeOf(pkCols) {
	case playerNamePKByName:
		st.releaseSQL = playerNameReleaseSQL
		// 主键对了**不等于**形状对了:回滚只做了一半、或手工只执行了 ADD PRIMARY KEY 时,
		// name_norm 上可能还残留着旧的二级唯一索引。它不改变任何对外语义(名字仍全服唯一、
		// Release 仍钉 PRIMARY),但会让 Reserve 插入新名字时多往一个二级唯一索引里插项 ——
		// 那条路上有插入意向锁,正是改主键消掉的死锁形状。
		//
		// 这里只报告、不拒启动,也不下发 DDL:与下面"探到旧形态不拒启动"同一条取舍(功能正确,
		// 拿一次确定的业务中断去换一个结构问题不划算),而修它的是 `-migrate`
		// (schema.go 的 convergePlayerNameKeyShape 会把它 DROP 掉,幂等)。
		// 探测本身失败也只打日志:releaseSQL 的选择不依赖它,fail-closed 在这里没有收益。
		if strays, strayErr := playerNameStrayNormUniqueIndexes(ctx, db, cfg.DBName); strayErr != nil {
			logx.Errorf("[PlayerNameStore] 探测 %s 上 name_norm 的多余二级唯一索引失败(不影响本进程的语句选择,"+
				"但也就无从确认形状是否收敛,跑一次 `data_service -f <yaml> -migrate` 会顺带收敛): %v",
				PlayerNameTableName, strayErr)
		} else if len(strays) > 0 {
			logx.Errorf("[PlayerNameStore] %s 的主键已是 name_norm,但 name_norm 上还残留着二级唯一索引 %v。"+
				"名字仍然全服唯一、对外语义不变,但 Reserve 插入新名字时会多走一次二级唯一索引的插入路径,"+
				"把改主键消掉的死锁形状带回来(症状是 1213 重试次数重新上涨,功能全对)。"+
				"处置:跑一次 `data_service -f <yaml> -migrate`(会自动 DROP 掉,幂等),"+
				"或人工 `ALTER TABLE %s DROP INDEX <名字>`",
				PlayerNameTableName, strays, PlayerNameTableName)
		}
		logx.Infof("[PlayerNameStore] connected to %s/%s max_open_conn=%d max_idle_conn=%d "+
			"(%s 的主键是 name_norm:名字抢注的死锁已在结构上根治,Release 钉 %s)",
			cfg.Host, cfg.DBName, cfg.MaxOpenConn, cfg.MaxIdleConn, PlayerNameTableName, playerNameReleaseForcedIndex)
	case playerNamePKByPlayerID:
		hasKey, keyErr := hasUniqueIndex(ctx, db, cfg.DBName, PlayerNameTableName, PlayerNameLegacyNormUniqueKey)
		if keyErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("player_name: %s 仍是旧主键形态,但探测 %s 失败(旧形态的 Release 靠它钉锁序): %w",
				PlayerNameTableName, PlayerNameLegacyNormUniqueKey, keyErr)
		}
		if !hasKey {
			_ = db.Close()
			return nil, fmt.Errorf("player_name: %s 仍是旧主键形态 (player_id),但 %s 这条索引不存在。"+
				"旧形态下 Release 必须 FORCE INDEX (%s) 才能与 Reserve 的 ON DUPLICATE KEY 同序取锁,"+
				"索引名缺失时每一条 Release 都会以 1176 失败、建角补偿释放全线失效。两条出路(推荐第一条):"+
				"(1) 跑 `data_service -f <yaml> -migrate` 把主键换成 name_norm(根治,之后不再依赖任何索引名);"+
				"(2) 先 `SHOW INDEX FROM %s` 确认名字唯一性是谁承担的,再 `ALTER TABLE %s ADD UNIQUE KEY %s (name_norm)` 补上并重启",
				PlayerNameTableName, PlayerNameLegacyNormUniqueKey, PlayerNameLegacyNormUniqueKey,
				PlayerNameTableName, PlayerNameTableName, PlayerNameLegacyNormUniqueKey)
		}
		st.releaseSQL = playerNameReleaseLegacySQL
		st.legacyKeyedByPlayerID = true
		logx.Errorf("[PlayerNameStore] connected to %s/%s:%s 还是**旧主键形态** PRIMARY KEY(player_id)。"+
			"功能正确(名字仍全服唯一),但名字抢注在生产常态那一路(新号段 id 抢存量角色释放的名字)必然成环 —— "+
			"实测 5/5,只能靠有界 1213 重试吸收。跑一次 `data_service -f <yaml> -migrate` 把主键换成 name_norm 即可根治;"+
			"本进程这期间用带 FORCE INDEX (%s) 的旧 Release 语句保持锁序一致",
			cfg.Host, cfg.DBName, PlayerNameTableName, PlayerNameLegacyNormUniqueKey)
	default:
		_ = db.Close()
		return nil, fmt.Errorf("player_name: %s 的主键是 %v(空 = 表不存在或没有主键,先确认迁移跑过、库名配对了),"+
			"既不是 name_norm(迁移后)也不是 player_id(迁移前)。"+
			"Release 的取锁顺序必须按形态选语句,认不出来就不敢发 —— 拒绝建 store(CreatePlayer 会当场拒绝)。"+
			"先 `SHOW CREATE TABLE %s` 看清现状,改回两种形态之一并重跑 `data_service -f <yaml> -migrate`",
			PlayerNameTableName, pkCols, PlayerNameTableName)
	}
	return st, nil
}

// Close 关闭连接池。
func (s *PlayerNameStore) Close() error {
	return s.db.Close()
}

// Reserve 把 (playerID, name, norm) 登记进表,返回终局与(仅 ReserveTaken 时的)占用者。
//
// 契约:
//   - name 是展示名、norm 是归一化后的判重键,两者必须由同一次 playername.Normalize 得出;
//     本层不校验它们是否自洽(那属于规则包,库层做不了)。
//   - nowMs 是登记时刻(Unix 毫秒),由调用方显式传入 —— 释放窗口靠这一列,时间不能藏在库里。
//   - 除 ReserveTaken 外,owner 一律返回 0:"占用者是谁"只在被别人占住时才有意义,
//     其余情况返回 playerID 只会诱使调用方把它当成别的语义。
//   - 无显式事务:单条语句自己就是提交点。返回 ReserveInserted / ReserveAlreadyOwned 时
//     数据**已经提交**,调用方可以安全地接着写缓存。
//
// # 写法:INSERT … ON DUPLICATE KEY UPDATE player_id = player_id(playerNameReserveSQL)
//
// 判重仍然只靠库上的唯一约束("直接写,撞了再看撞在谁身上"),只是"撞了"从 1062 错误变成了受影响 0 行:
//   - 1 行 = 插入成功 → ReserveInserted。
//   - 0 行 = 撞上已有行,no-op 更新一个字节都没改。撞在哪一行(表上两个唯一约束:PK name_norm、
//     uk_player_name_owner player_id):
//     只撞主键(名字被别人占了)→ 更新落在占用者那一行;
//     只撞 player_id 那条唯一键(这个 player_id 已登记过别的名字)→ 更新落在它原来那一行;
//     两者都撞且不是同一行 → InnoDB 先插聚簇索引、主键先报重复,更新落在名字那一行。
//     三种都是 no-op、都回 0 行;随后照旧用两条等值读(ownedNormOf → ownerOfNorm)分辨 AlreadyOwned /
//     Conflict / Taken / 行已消失(重插)—— 与改主键前、与更早的 1062 分支都相同,
//     对外返回值、日志、ID 复用告警**一个字都没变**(契约用例 TestReserveOutcomeContract 钉住)。
//   - 其它行数(2 = 真的改了一行)在 no-op 更新下不可能出现;出现即说明语句被改,fail-closed 报错。
//
// 前提(任何一条不成立,上面的判定都会**安静地**错):
//  1. 连接**不**带 CLIENT_FOUND_ROWS(DSN 的 clientFoundRows)。带了之后 no-op 的 ODKU 也回 1(找到的行数),
//     与"插入成功"无法区分,撞名会被当成 Inserted —— 而名字其实属于别人。buildDSN 不设它,
//     player_name_store_test.go 的 TestStoreDSNDoesNotSetClientFoundRows 机械守住。
//  2. binlog_format = ROW。表上有两个唯一约束时,ODKU"更新哪一行"取决于引擎检查唯一键的顺序,MySQL 把它
//     标为基于语句的复制不安全(STATEMENT 下从库可能更新另一行);ROW 下 no-op 不产生行事件,主从一致。
//     MySQL 8 默认 ROW,deploy/k8s/manifests/infra/mysql.yaml 显式写了 binlog_format = ROW。
//  3. 迁 TiDB 时要回归行数口径与 ODKU 语义(按文档与 MySQL 同口径,未实测)。
//
// # 根治:名字是聚簇主键 + ODKU,两个条件缺一不可(审计 #12,用户 2026-09-29 拍板)
//
// 2026-09-29 在本机真 MySQL 26.7.0 上跑了一支独立探针(与本表的 Reserve / Release 同形的编排:
// RR 只读事务当 purge 挡板 → 原主人占住名字 → 未提交的 Release 删它 → 两个竞争者同时抢 → 放锁 →
// 统计两人拿到的错误;每格重复 5 次)。下面这张表是**实测**,不是推演,推翻了前几轮的静态结论:
//
//	表结构                                      写法          竞争者都小于原主人  都大于  一大一小
//	PRIMARY KEY(player_id) + UNIQUE(name_norm)   普通 INSERT   5/5 死锁          2/5    5/5
//	PRIMARY KEY(player_id) + UNIQUE(name_norm)   ODKU          5/5 死锁          0/5    2/5
//	PRIMARY KEY(name_norm) + UNIQUE(player_id)   普通 INSERT   5/5 死锁          5/5    5/5
//	PRIMARY KEY(name_norm) + UNIQUE(player_id)   **ODKU**      **0/5**           0/5    0/5
//
// 三条可以直接读出来的事实:
//  1. **只改写法不够**。旧结构配 ODKU 在"竞争者 id 小于原主人"那一路仍然 5/5 成环,而那正是生产常态:
//     号段发的 player_id 都 < 2^55(IdSegmentCap),存量角色是更大的 snowflake id,新号抢老角色释放的
//     名字时恒满足。前几轮注释里"ODKU 拆掉了一半"只在"都大于"那一路成立。
//  2. **只改结构也不够**。新结构配普通 INSERT 是 15/15 全成环 —— 聚簇记录上的重复检查取 S,
//     两个 S 同时被授予之后各自要把它升级成 X(原地改写),互相挡住(手册 E1 的三会话例就建在聚簇主键上)。
//  3. **两个一起上才是 0/15**。这就是现在的形态:name_norm 当主键(schema.go 的 playerNameBootstrapDDL)
//     + playerNameReserveSQL 的 ODKU。
//
// 机理:聚簇索引撞上同键的**删除标记记录**时走 row_ins_must_modify_rec → row_ins_clust_index_entry_by_modify,
// 是**原地改写**,取的是 LOCK_X + LOCK_REC_NOT_GAP(不带间隙),**根本不申请插入意向锁**。
// 而前几轮追查到的那条环,每一种形状的最后一步都是"插入意向锁被对方排队中的 next-key 请求挡住"(E3/E4)——
// 没有插入意向锁,这条边就不存在。名字只要还是**二级**唯一索引,重复检查就发生在二级索引上,
// 新项要真的插进去(而不是原地改写),那条边就必然回来。
//
// 可以推广的判据(别的表照此自查):**当"被争抢的键"是二级唯一索引、且新记录的主键可能小于
// 删除标记那条记录的主键时,ODKU 不够用 —— 要么把被争抢的键做成聚簇主键,要么承认这条环。**
//
// # 证据(2026-09-28 逐条按来源核实,不是凭记忆;探针把其中的推论变成了实测)
//
// lock0lock.cc 的三段在 mysql-server 的 8.0 / 8.4 / trunk 三个分支上**逐字相同**,所以结论覆盖本仓
// 部署的两个版本(deploy/k8s/manifests/infra/mysql.yaml 的 mysql:8.0 与 deploy/docker-compose.yml 的 mysql:latest):
//
//	E1 手册 "Locks Set by Different SQL Statements in InnoDB"
//	   (https://dev.mysql.com/doc/refman/8.0/en/innodb-locks-set.html):
//	   普通 INSERT 撞键时"a shared lock on the duplicate index record is set",并给出三会话例
//	   (建在 CREATE TABLE t1 (i INT, PRIMARY KEY (i)) 上:S1 INSERT 未提交 → S2 S3 撞键排队要 S →
//	   S1 ROLLBACK 放掉 X 之后两个 S **同时**被授予 → "Neither can acquire an exclusive lock for the row
//	   because of the shared lock held by the other" → 死锁)。**它走的正是聚簇主键** —— 也就是上面第 2 条
//	   "新结构 + 普通 INSERT = 15/15"的出处:结构换了,写法退回普通 INSERT 照样成环。
//	   同页对 ODKU:"an exclusive lock rather than a shared lock is placed on the row
//	   to be updated when a duplicate-key error occurs. An exclusive index-record lock is taken for a duplicate
//	   primary key value. An exclusive next-key lock is taken for a duplicate unique key value."
//	   —— **撞主键只拿记录锁(不带间隙)**,撞二级唯一键拿 X next-key。本表撞的是主键,所以是前者。
//	E2 源码 storage/innobase/row/row0ins.cc row_ins_scan_sec_index_for_duplicate(**二级**索引那条路):
//	   allow_duplicates(= REPLACE / ODKU)那一支取 row_ins_set_rec_lock(LOCK_X, lock_type, ...),
//	   lock_type = skip_gap_locks ? LOCK_REC_NOT_GAP : LOCK_ORDINARY;skip_gap_locks 只对 DD / SDI 表为真
//	   (include/dict0mem.h 的注释"GAP locks are skipped for DD tables and SDI tables"),所以二级唯一键上拿的是
//	   **X next-key**。普通 INSERT 那一支取 LOCK_S,并且对"第一条不相等的记录"只取 LOCK_GAP,源码注释原话:
//	   "We don't need to lock the first unequal record after the gap, just the gap before it."
//	   (行级引用未能独立复核 —— raw.githubusercontent 上这个文件被 WebFetch 截断,拿到的只有注释本身。
//	   本表改成聚簇主键之后 E2 只用于解释"为什么二级索引那条路会成环",不再是生产路径的依据。)
//	E3 源码 storage/innobase/lock/lock0lock.cc lock_rec_insert_check_and_lock:
//	   "If another transaction has an explicit lock request which locks the gap, waiting or granted, on the
//	   successor, the insert has to wait." 唯一例外是对方那把锁本身是为了插入而排队的间隙锁(插入意向锁)。
//	   **"排队中的请求"算冲突** —— 这是那条环的关键一步,而它只在"真的要插入"时才走到。
//	E4 源码 lock0lock.cc rec_lock_check_conflict:能让请求方"越过"排队者的 CAN_BYPASS / has_granted_blocker
//	   那一支,前提写死成 !(type_mode & LOCK_INSERT_INTENTION),注释原话:"This is very important that
//	   LOCK_INSERT_INTENTION should not overtake a WAITING Gap or Next-Key lock on the same heap_no, because
//	   the following insertion of the record would split the gap duplicating the waiting lock, violating the rule
//	   that a transaction can have at most one waiting lock."lock_rec_insert_check_and_lock 还用
//	   ut_a(!conflicting.bypassed) 把它钉成硬断言。所以"换个版本插入意向锁也许能越过排队者"是**错的**:
//	   那是被设计明确禁止的。二级索引那条环因此与版本无关,不能靠升级 MySQL 躲掉 —— 只能改结构。
//	E5 手册 "InnoDB Locking"(https://dev.mysql.com/doc/refman/8.0/en/innodb-locking.html):间隙锁
//	   "purely inhibitive"、"Gap locks can co-exist"、S 与 X 间隙锁"do not conflict with each other"。
//	   这解释的是"为什么两个 S next-key 能被同时授予"(S 与 S 模式相容),与"间隙锁可以共存"是两件事,不要合并成一句。
//	E6 源码 row0ins.cc row_ins_sec_index_entry_low:二级索引的重复扫描只在
//	   dict_index_is_unique(index) && (cursor.low_match >= n_unique || cursor.up_match >= n_unique) 时才调用。
//
// # 锁序:先聚簇(名字)、后二级(player_id) —— Release 必须同序
//
// Reserve 的 ODKU 一律**从聚簇索引开始**:先在 name_norm 上做重复检查(撞到就是 X 记录锁 + 原地改写,
// 没撞到就是一次真插入),再去写 / 查 uk_player_name_owner 那条二级唯一键。所以同表另一个写者 Release
// 也必须"先聚簇、后二级":按 player_id 走二级索引删(先二级、后聚簇)会与这里反序 —— Release 持二级项
// 等聚簇行、Reserve 持聚簇行等二级项 → 1213,而且只要两方就够。
// WHERE 里 player_id 与 name_norm 都是等值条件、又各有一条唯一约束,优化器挑哪条不由我们说了算,
// 所以 playerNameReleaseSQL 用 FORCE INDEX (PRIMARY) 钉死(见 Release 的「锁序」,真库用例用 EXPLAIN 验)。
//
// # 与其它写者的交错(本表的写者只有 Reserve / Release;三条读都是不加锁的一致性读)
//
// 先把两条语句各自的取锁**顺序**摆清楚,下面每一条都按它推:
//   - Reserve 撞**名字**(最常见的一路):先在 name_norm 这条聚簇记录上取 X,随后按**撞上的是活行、
//     还是未 purge 的删除标记记录**分成两条路 —— 两者碰不碰二级索引不一样,**不能合成一句**
//     (2026-09-29 复审指出:原先这里只写了活行那一路):
//     (1) 撞**活行**:走 SQL 层的 ODKU 更新,更新子句 `player_id = player_id` 引用已有行自己的列值,
//     一个字节都不改 —— 没有任何序数列被改写,所以**不碰二级索引**,全程只有这一条聚簇记录。
//     (2) 撞**未 purge 的删除标记记录**(Release 刚删完、purge 还没跟上;名字抢注的常态就是这一路):
//     InnoDB 在聚簇索引上遇到同键的删除标记记录时**不报重复键**,而是把这次插入改写成对那条记录的
//     更新(复活):清掉删除标记,写进新的 player_id / name / created_ms;ODKU 的 no-op 更新子句
//     **压根轮不到执行**。而 player_id 是 uk_player_name_owner 的键列,它被改写就意味着
//     **这条二级唯一索引要重写**:旧主人的 (P_old) 项删除标记、新主人的 (P_new) 项**插入**。
//     所以这一路**会碰二级索引,而且那一步是真插入、要走插入意向锁**。
//   - Reserve 撞 **player_id**(同一个 player_id 去登记另一个名字):先把新行插进聚簇索引(新记录带隐式 X),
//     再写 uk_player_name_owner 时撞上已有的 (P) 项 → 在它上面取 X(E2 的 allow_duplicates 支,
//     二级唯一键上是 next-key)→ 服务层顺着这条二级项定位到**旧名字那一行**做 no-op 更新 → 取那条聚簇记录的 X。
//     **顺序:新名字的聚簇记录 → 二级 (P) → 旧名字的聚簇记录。**
//   - Reserve 插入成功:先插聚簇记录、后插 uk_player_name_owner 的项。**顺序:聚簇 → 二级。**
//   - Release:聚簇记录 name_norm 上 X → 连带把 uk_player_name_owner 上的 (P) 项删除标记(那一步同样要在
//     二级项上取 X)。**顺序:聚簇 → 二级。**
//
// 逐对:
//   - Release(P_old,'abc') 未提交 + 任意多个 Reserve 抢 'abc':全部排在 'abc' 这条聚簇记录的 X 上,
//     **一个一个过**。放锁后第一个拿到 X 的看到的是删除标记记录 → 走上面的 (2) 复活那一路 → 插入成功;
//     其余看到的已经是活行 → 走 (1) no-op → Taken。
//     所以**不能**说"整条路上没有插入意向锁":复活那一步要往 uk_player_name_owner 插 (P_new) 项,那是一次
//     真插入、会申请插入意向锁。它之所以没把环凑出来,是因为竞争者已经被那条聚簇记录的 X **串行化**了 ——
//     同一时刻只有一个事务走在二级索引那一步,凑不出"两个排队者互挡插入意向"的形状。
//     **上面这句是推演,能拿出来的只有实测**:2026-09-29 真库探针(MySQL 26.7.0,全局 RR,purge 挡板,
//     编排与本段同形,Reserve=ODKU、Release=FORCE INDEX 走名字那条索引)在三种 player_id 顺序上各跑 5 次,
//     **0/15 未复现 1213**;同一个 player_id 的两个名字那两形(B1/B2,见「残余」一节)各 20 次,
//     **0/20 未复现**。写"未复现"而不是"不可能":15 次 / 20 次是目前全部的证据,不是证明。
//   - 两个 Reserve 抢同一个名字(没有待提交的 Release):名字那条聚簇记录不存在时,先插入者的新记录带隐式锁,
//     后到者的重复检查单向等它提交 → no-op → Taken;名字刚被释放、删除标记记录还没被 purge 时,两者排在
//     那条记录上,与上一条同形。
//   - **Reserve(P, N2) × Release(P, N1):同一个 player_id、两个不同名字 —— 这是改主键**新引入**的一条
//     反序对,旧结构上不存在。** 取锁序对照上面两条:Reserve 走「聚簇 N2 → 二级 (P) → 聚簇 N1」,
//     Release 走「聚簇 N1 → 二级 (P)」,在 (P) 与 N1 这两处恰好互为反序 —— Reserve 持二级 (P) 等聚簇 N1、
//     Release 持聚簇 N1 等二级 (P),**两方就够成环**。
//     旧结构上没有这一形:那时 player_id 是聚簇主键,Reserve(P,N2) 撞的就是聚簇记录 P,取完 X 原地改写即结束,
//     **根本不碰** uk_player_name,所以 Release 只会单向等它。也就是说这次根治在消掉一条环的同时新引入了一条,
//     登记在这里是因为"穷举清单漏项"本身不可接受(2026-09-29 复审指出)。
//     可达性(按 HEAD 的 go/login/internal/logic/clientplayerlogin/createplayerlogic.go 逐条核过):
//     正常路径**到不了** —— 生成名循环只在 nameTaken 时 continue(那一路没有写入、也不发 Release),
//     结果未知的那一支一律 releaseName 之后直接放弃建角,不会对同一个 playerID 再登记第二个名字;
//     每次建角都新铸一个 player_id,所以"同一个 P 同时在登记 N2 和释放 N1"只能来自
//     **ID 复用事故 × login 延迟约 10s 的第二次补偿 Release** 的叠加(第二次 Release 在后台 goroutine 里发,
//     那时建角流程早已返回)。与「残余 B」同一档:由有界重试兜住,并在 ReserveConflict 处打 Error 告警。
//     **真库实测:0/20 未复现**(2026-09-29 v3 探针,"同一个 player_id 的并发写"编排,两个写者直接对撞,
//     语句形状与生产一致;现行主键与候选主键两种表结构下各 20 次,都是 0)。所以上面那条环目前的状态是
//     **推演出来、真库上没能复现** —— 不写"不可能",20 次不是证明。真库用例
//     TestPlayerName_ReserveOtherNameRacesReleaseSameOwner 用手工编排把这个交错摆死(生产并发下撞不到),
//     **只记录不断言**,用来在换 MySQL 版本时复核。
//     若哪天实测确实成环,候选的根治是把 Release 也改成"先二级、后聚簇"(FORCE INDEX (uk_player_name_owner)),
//     那样两者在 (P) 上同序、不再反向;**现在不改**,因为它会让名字抢注那组真库证据(探针的 0/15,是在
//     "Release 钉名字那条索引"的形状上测的)失效,必须先在真库上把两组形状都重测一遍才能换。
//   - Reserve('abc') 与 Release(Q,'xyz')(两个不同的名字、不同的 player_id):**按目标行的状态分两种**,
//     不能合成一句"互不相干"。
//     (1) 'xyz' 是**活行**:手册 "Locks Set by Different SQL Statements in InnoDB" 对 DELETE 的免间隙优化
//     写的是 "only an index record lock is required for statements that lock rows using a unique index to
//     search for a unique row",这里正是唯一索引 + 唯一等值条件 → 只取 LOCK_REC_NOT_GAP,挡不住别的名字的插入。
//     (2) 'xyz' 是**未 purge 的删除标记记录**(login 约 10s 后的第二次补偿 Release 赶上 purge 滞后,
//     是常规路径不是边角):手册那句限定语只说"定位到唯一行",**没有**说明删除标记记录算不算,
//     而 InnoDB 的免间隙分支按公开资料还额外要求记录未被删除标记(落到删除标记记录上退回 next-key)——
//     本轮 WebFetch 只能取到手册原文,row0sel.cc 的那一行**未能独立复核**(raw 文件被截断)。
//     所以这里按**保守**口径记:这一路取到的**可能是带间隙的 next-key**,会挡住排在 'xyz' 之前的相邻名字
//     的插入意向锁 —— 恰恰不是"互不相干"。
//     **未在新结构上实测**:探针"删后继名字时两个竞争者不排队(0/2)"那一次是在**旧结构**(名字是二级
//     唯一索引)上做的(probe-results.md 第 2 条),不能外推到新结构。真库用例
//     TestPlayerName_ReleaseRacesTwoReservesConverges 的「后继名字」现场目前按"不排队"编排,它是**观察**
//     而不是证明 —— 真跑出"被挡住",要改的是那条编排,不是这里的结论。
//     即便退回 next-key,这一路也只是**单向等**(Reserve 等 Release 放锁),不构成环:Release 不会反过来
//     去要 Reserve 持有的任何锁(它要的二级 (P) 项属于 Q,不是 Reserve 那个 player_id)。
//   - Release 走聚簇碰上未 purge 的删除标记记录(同上一条的情形 2):在那条记录上取 X、可能排队,单向等,
//     不成环(它不会反过来去要别人的锁)。
//   - 同一 player_id、**同一个名字**的并发 Reserve / Release(login 的幂等重试就是这一形):都要这一行的
//     聚簇 X,单向等。不同名字的那一形见上面新加的那条。
//
// # 残余:仍然可能出现的 1213(都不是名字抢注那一类)
//
// A. **锁继承**(InnoDB 固有,SQL 层去不掉,与写法和结构都无关)。排队者都在等某条记录上的锁时,这条记录被
// **物理删除** —— 先到的插入者回滚(ctx 取消断连、被选为别处死锁的牺牲者),或 purge 在排队期间清掉了删除标记
// 记录 —— InnoDB 会把它上面的锁继承成后继记录上的**间隙锁**(RR 下;间隙锁彼此兼容,X / S 都一样,E5),
// 于是 ≥2 个排队者各持间隙锁、再互等插入意向锁 → 1213。探针的编排刻意用 purge 挡板排除了这一类,
// 所以 0/15 是"名字抢注这条路上 0 次",不是"这张表上永远不会再有 1213"。
//
// B. **同一个 player_id 上的并发"不同名"写入** —— 两种形状,**都是改主键新引入的**,旧结构上不存在
// (那时 player_id 是聚簇主键,撞它走原地改写、不碰任何二级索引):
//
//	B1. Reserve(P, N2) × Reserve(P, N3):两个 Reserve 都要往 uk_player_name_owner 的 (P) 项上撞。
//	    uk_player_name_owner 是二级唯一索引,E2/E3/E4 那条"二级索引上的插入意向锁被排队者挡住"的环
//	    原样搬到 player_id 这条轴上。
//	B2. Reserve(P, N2) × Release(P, N1):**反序对**,见上面「与其它写者的交错」里那一条的完整推导 ——
//	    Reserve 走「聚簇 N2 → 二级 (P) → 聚簇 N1」,Release 走「聚簇 N1 → 二级 (P)」,两方即可成环。
//
// 两形的可达性相同:v1 没有改名,正常路径下 login 的重试用的是**同一个** (player_id, name)、撞的是同一条
// 记录、只会单向等(可达性论证见上面那一条,按 HEAD 的 createplayerlogic.go 核过),
// 所以它们只能来自 **ID 复用事故**(发号器被重置),B2 还要再叠加 login 延迟约 10s 的第二次补偿 Release。
// 记在这里是为了把清单**穷举完整**,不是说它们常见;ID 复用本身已经由 ReserveConflict 那条 Error 日志告警。
// **两形都已在真库上测过,都没复现**:2026-09-29 的 v3 探针按"同一个 player_id 的并发写"编排
// (B1 = 持锁方未提交地压着,两个写者登记两个不同名字;B2 = 登记新名与释放旧名两个写者直接对撞;
// 语句形状与生产一致),在现行主键与候选主键两种表结构上**各 20 次,全部 0/20**。
// 所以 B 现在的定性是"**推演成环、实测未复现**" —— 既不是"已证实会成环",也不是"不可能成环":20 次不是证明。
// B2 的真库用例见 TestPlayerName_ReserveOtherNameRacesReleaseSameOwner(手工编排、只记录不断言)。
//
// 处理(A、B 相同,B 的两形也相同):isRetryableMySQL 命中时按 playerNameLockRetryBackoffMin/Max 抖动退避(ctx 可取消)后重来,
// 总次数不超过 playerNameReserveAttempts。收敛论证:被牺牲方的语句自动提交,整条回滚、锁全部放掉;另一方随即
// 前进并提交,牺牲方退避几十毫秒后的下一轮看见活行 → no-op → Taken(Release 则看到行已不属于自己 → Absent)。
// 每成一次环都有一方前进,有界;预算用尽按存储错误返回(login fail-closed,拒绝建角),不无限转。
// **重试现在只兜 A(InnoDB 固有)与 B(只在 ID 复用事故下可达)两类**,不再是"明知会成环、靠重试扛过去"——
// 后者是用户明确不接受的形态,所以才做了改主键这次根治。
// 但要说清楚:**B 不是"消不掉"的** —— B2 有一个候选根治(把 Release 也改成 FORCE INDEX (uk_player_name_owner),
// 让两者在 (P) 上同序),它会让探针那份 0/15 的证据失效,必须先在真库上把两组形状重测一遍才能换。
// 在那之前,B 留在重试里是一次**按实测取的舍**:两形都是 0/20 未复现,而 20 次不足以把它从清单里划掉,
// 所以仍按"可能成环"兜着,而不是宣布它不存在。
//
// # 迁移还没跑过的库 / TiDB:退回旧形态,行为写明在这里
//
// 表仍是 PRIMARY KEY(player_id) 时(忘了跑 `data_service -migrate`,或对端是 TiDB —— 迁移在那里会跳过,
// 见 schema.go 的 ensurePlayerNameKeyedByName),本 store 在建店时就探到了,Release 会自动改用
// playerNameReleaseLegacySQL 保持同序。**那种表上名字抢注的环仍在**(生产常态那一路实测 5/5),
// 由上面那套有界重试兜住,并在启动日志里打一条 Error 指明"跑 -migrate 才能根治"。
// TiDB(悲观事务)按官方文档没有间隙锁与插入意向锁,上述几类形状都不存在(**未在 TiDB 上实测**)。
func (s *PlayerNameStore) Reserve(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (ReserveOutcome, uint64, error) {
	if playerID == 0 || norm == "" {
		return 0, 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}

	for attempt := 1; attempt <= playerNameReserveAttempts; attempt++ {
		res, err := s.db.ExecContext(ctx, playerNameReserveSQL, playerID, name, norm, nowMs)
		switch {
		case err == nil:
			inserted, err := reserveInserted(res, playerID)
			if err != nil {
				return 0, 0, err
			}
			if inserted {
				return ReserveInserted, 0, nil
			}
			// 0 行:撞上了已有行,落到下面分辨撞在谁身上。
		case isRetryableMySQL(err):
			if attempt < playerNameReserveAttempts {
				logx.Infof("[PlayerNameStore] player_id=%d reserve retry %d/%d after lock error: %v",
					playerID, attempt, playerNameReserveAttempts, err)
				if waitErr := s.pauseBeforeLockRetry(ctx); waitErr != nil {
					return 0, 0, fmt.Errorf("player_name reserve insert player_id=%d: %w", playerID, errors.Join(err, waitErr))
				}
				continue
			}
			return 0, 0, fmt.Errorf("player_name reserve insert player_id=%d: %w", playerID, err)
		case isDuplicateEntryMySQL(err):
			// ODKU 下撞键不会以 1062 报出(no-op 更新不可能再撞另一个唯一键)。仍按"撞上已有行"处理,
			// 只是纵深防御:语句万一被改回普通 INSERT,对外语义也不变。
		default:
			return 0, 0, fmt.Errorf("player_name reserve insert player_id=%d: %w", playerID, err)
		}

		// 撞键:撞的是主键还是唯一键,两条等值读分开问。
		// 刻意不写成一条 `WHERE player_id = ? OR name_norm = ?`:OR 会让优化器放弃两条索引
		// 改走全表扫,而且返回多行时还要在 Go 里再分辨一次,不如直接问两次各走各的索引。
		ownedNorm, hasOwn, err := s.ownedNormOf(ctx, playerID)
		if err != nil {
			return 0, 0, err
		}
		if hasOwn {
			if ownedNorm == norm {
				return ReserveAlreadyOwned, 0, nil
			}
			// ID 复用事故:这个 player_id 名下已经有另一个名字。打 Error 是因为本层
			// 掌握的信息(旧 norm)比调用方多,而且这条日志是事后追查的唯一线索。
			logx.Errorf("[PlayerNameStore] player_id=%d already holds name_norm=%q, refused to register %q: "+
				"v1 has no rename, so this means the player id was reused (id allocator reset?); do NOT delete the old row, it belongs to a live character",
				playerID, ownedNorm, norm)
			return ReserveConflict, 0, nil
		}

		owner, hasOwner, err := s.ownerOfNorm(ctx, norm)
		if err != nil {
			return 0, 0, err
		}
		if hasOwner {
			return ReserveTaken, owner, nil
		}

		// 两条都空:撞键那一刻存在的行,被并发的 Release 在这两次读之间删掉了。
		// 名字现在是空的,下一轮直接重插(没有锁冲突,不退避)。
		logx.Infof("[PlayerNameStore] player_id=%d reserve retry %d/%d: duplicate row vanished (concurrent release)",
			playerID, attempt, playerNameReserveAttempts)
	}
	return 0, 0, fmt.Errorf("player_name reserve: player_id=%d name_norm=%q exhausted %d attempts",
		playerID, norm, playerNameReserveAttempts)
}

// Release 条件删除 (playerID, name_norm) 这一行。
//
// minCreatedMs 是登记时刻的下界(Unix 毫秒,含):
//   - 0 = 不限时间。**只有**已经通过 admin token 校验的运维路径能传 0。
//   - > 0 = 只删这么新的登记。login 的建角补偿走这一支(now - PlayerName.ReleaseWindow),
//     这道窗拦的是"拿别人刚建好的号的名字来释放"—— 少了它,任何能发 RPC 的内部调用方
//     都能把在役角色的名字删掉,而名字一旦释放就可能立刻被别人占走,不可回滚。
//
// 幂等:行不在(或早被删)返回 ReleaseAbsent,不是错误。
//
// # 锁序:与 Reserve 同序,两种表形态各有一条语句
//
// DELETE 走哪条索引,决定它先锁哪条记录。Reserve 的 ODKU 一律从**聚簇索引**开始,所以 Release 也必须
// 先聚簇、后二级;反序(先二级、后聚簇)只要两方就能成环。WHERE 里 player_id 与 name_norm 都是等值条件、
// 各自又都有一条唯一约束,优化器挑哪条不由我们说了算,所以两条语句都用 FORCE INDEX 钉死。
// 单表 DELETE 不接受索引提示,只能写成多表 DELETE 语法,删除语义与受影响行数与单表写法相同。
//   - 新结构(PRIMARY KEY(name_norm),playerNameReleaseSQL):钉 PRIMARY —— 名字就是聚簇键。
//   - 旧结构(PRIMARY KEY(player_id),playerNameReleaseLegacySQL):钉 uk_player_name —— 那时名字是二级
//     唯一索引,Reserve 撞名时先锁它、再锁聚簇行,Release 必须同序。
//
// 用哪一条由建 store 时的一次形态探测决定(见 NewPlayerNameStore),运行期不变;两条语句的占位符顺序
// 完全相同,所以下面这段绑参代码对两者通用。真库用例 TestPlayerName_ReleaseStatementLocksClusteredKeyFirst
// 用 EXPLAIN 钉住新结构下 key = PRIMARY 且是单行点查。
//
// 走聚簇(新结构)时,按碰到的记录分三种:
//   - 活行:在聚簇记录上取 X **记录锁、不带间隙** —— 手册 "Locks Set by Different SQL Statements in InnoDB"
//     对 DELETE 的原话是 "sets an exclusive next-key lock on every record the search encounters. However,
//     only an index record lock is required for statements that lock rows using a unique index to search for a
//     unique row",这里正是"唯一索引 + 唯一等值条件"。所以它挡不住别人往相邻间隙插入,只会与要改同一行的
//     写者单向排队。删掉这一行的同时会把 uk_player_name_owner 上的对应项一并删除标记 —— 那是"先聚簇、后二级",
//     与 Reserve 同序。player_id / created_ms 两个条件在读到那一行之后判定,不匹配时 RR 下那把锁持有到
//     语句结束(自动提交,极短),不删任何东西。
//   - 名字完全不存在:没有"唯一行"可锁,退回一般规则,RR 下在聚簇索引上留间隙锁,插入者只会单向等它。
//   - **未 purge 的删除标记记录**(名字已被释放过,典型触发是 login 约 10s 后的第二次补偿 Release 赶上
//     purge 滞后,见文件头「崩溃窗口」):在那条记录上取 X、可能**排队**,但只是单向等 —— 抢同一个名字的
//     Reserve 在聚簇索引上做的是原地改写,不申请插入意向锁,不会反过来卡住本条(这正是改主键根治掉的那条边,
//     见 Reserve 的「根治」)。旧结构下这里会与 Reserve 成环,那是 playerNameReleaseLegacySQL 仍要靠
//     有界重试兜住的原因。
//     **这一路拿到的未必是"不带间隙"的记录锁**:上面手册那句免间隙优化限定在"用唯一索引定位唯一行",
//     对"这一行是删除标记记录时算不算"手册没有交代,而 InnoDB 的免间隙分支按公开资料还额外要求记录未被
//     删除标记(退回 next-key);本轮只复核到手册原文,源码那一行未能独立复核(raw 文件被截断)。
//     所以按保守口径记:这里可能带间隙,会挡住**相邻名字**的插入意向锁(即挡住别人的 Reserve),
//     但仍是单向等、不成环。**未在新结构上实测** —— 探针那次"删后继名字不排队(0/2)"是在旧结构上做的。
//
// 第三种情形还有一条与「残余 B2」相关:Release 删掉聚簇行之后,要接着把 uk_player_name_owner 上的 (P) 项
// 删除标记(同样取 X)。那一步与"同一个 player_id 正在登记另一个名字的 Reserve"互为反序,
// 见 Reserve 的「与其它写者的交错」里 Reserve(P,N2) × Release(P,N1) 那一条。
//
// 锁冲突(1213/1205)按 playerNameLockRetryBackoffMin/Max 抖动退避后重来(ctx 可取消),次数不超过
// playerNameReleaseAttempts。DELETE 幂等,重来安全;被牺牲的若是本条 Release,它要删的行早已不在,
// 下一轮看到的是新主人的活行(player_id 对不上,不删)或仍只有删除标记记录 → ReleaseAbsent,
// 不会误删新主人的行。
func (s *PlayerNameStore) Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error) {
	if playerID == 0 || norm == "" {
		return 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}

	for attempt := 1; attempt <= playerNameReleaseAttempts; attempt++ {
		res, err := s.db.ExecContext(ctx, s.releaseSQL, playerID, norm, minCreatedMs)
		if err != nil {
			if isRetryableMySQL(err) && attempt < playerNameReleaseAttempts {
				logx.Infof("[PlayerNameStore] player_id=%d release retry %d/%d after lock error: %v",
					playerID, attempt, playerNameReleaseAttempts, err)
				if waitErr := s.pauseBeforeLockRetry(ctx); waitErr != nil {
					return 0, fmt.Errorf("player_name release player_id=%d: %w", playerID, errors.Join(err, waitErr))
				}
				continue
			}
			return 0, fmt.Errorf("player_name release player_id=%d: %w", playerID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("player_name release player_id=%d rows affected: %w", playerID, err)
		}
		if n > 0 {
			return ReleaseDeleted, nil
		}

		// 没删到。不限时间时"没删到"只能是行不存在(DELETE 的时间条件恒真)。
		if minCreatedMs == 0 {
			return ReleaseAbsent, nil
		}

		// 限时间时"没删到"有三种成因,必须按 created_ms 分开 —— 只探"行在不在"会把第三种
		// 误判成第二种,给调用方一个 FailedPrecondition 加一条指向错误方向的 ERROR。
		createdMs, exists, err := s.createdMsOf(ctx, playerID, norm)
		if err != nil {
			return 0, err
		}
		switch {
		case !exists:
			// 1. 行不存在(从没登记过,或已被删):幂等成功。
			return ReleaseAbsent, nil
		case createdMs < minCreatedMs:
			// 2. 行在,登记时刻早于窗口下界:这正是窗口要拦的"删别人在役角色的名字",拒绝。
			return ReleaseOutsideWindow, nil
		default:
			// 3. 行在且落在窗口内,却没被刚才那条 DELETE 删到 —— 只能是这行在 DELETE 执行
			//    **之后**才提交(在途 INSERT 与本次 Release 交叉)。而这恰恰是 login 两次补偿
			//    释放要覆盖的竞态(见文件头「崩溃窗口」):此刻不重删就会留下一条孤儿行。
			//    循环上限 playerNameReleaseAttempts 兜住"对端一直在重插"的极端情况,
			//    用尽后按错误返回(见循环外的 exhausted),不无限转。
			logx.Infof("[PlayerNameStore] player_id=%d release retry %d/%d: row committed after the delete "+
				"(created_ms=%d >= min_created_ms=%d), deleting again",
				playerID, attempt, playerNameReleaseAttempts, createdMs, minCreatedMs)
		}
	}
	return 0, fmt.Errorf("player_name release: player_id=%d name_norm=%q exhausted %d attempts",
		playerID, norm, playerNameReleaseAttempts)
}

// BatchGet 按 player_id 批量读展示名。
//
// 契约:
//   - 缺席的 id **不出现**在结果 map 里,不是错误(与 BatchGetPlayerHomeZone 同口径);
//     名字是展示数据,读侧 fail-open。
//   - 去重与"读缓存、回填缓存"属于 logic;本层只负责一次 IN 查询。
//   - id 为 0 的项被跳过(0 不是合法 player_id,也不可能有行),不算错误。
//   - 超过 PlayerNameBatchLimit 直接拒绝:一条无上限的 IN 会把单次查询变成事实上的全表读。
//
// 访问路径:走 uk_player_name_owner(player_id)每个 id 一次点查,再按主键 name_norm 回表取展示名。
// 主键改成 name_norm 之后这条路多了一次回表(原来 player_id 是聚簇键,一次就够),
// 但仍是"每个 id 一次点查",没有范围扫;而且是一致性读,不取任何锁,与写者互不影响。
func (s *PlayerNameStore) BatchGet(ctx context.Context, playerIDs []uint64) (map[uint64]string, error) {
	out := make(map[uint64]string, len(playerIDs))
	if len(playerIDs) == 0 {
		return out, nil
	}
	if len(playerIDs) > PlayerNameBatchLimit {
		return nil, fmt.Errorf("%w: got %d, limit %d", ErrPlayerNameBatchTooLarge, len(playerIDs), PlayerNameBatchLimit)
	}

	placeholders := make([]string, 0, len(playerIDs))
	args := make([]any, 0, len(playerIDs))
	for _, id := range playerIDs {
		if id == 0 {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}

	query := "SELECT player_id, name FROM player_name WHERE player_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("player_name batch get (%d ids): %w", len(args), err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uint64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("player_name batch get scan: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("player_name batch get iterate: %w", err)
	}
	return out, nil
}

// ownedNormOf 读这个 player_id 已登记的 name_norm。found=false 表示没有行。
//
// 走 uk_player_name_owner(player_id)的唯一索引点查:player_id 是唯一的,所以最多一行 ——
// 这也正是"一个角色只登记一个名字"那条唯一性在读侧的体现。**去掉那条唯一键之后本函数会静默变坏**
// (QueryRow 取第一行,多名字时回出任意一个,Reserve 的 ID 复用告警随之失效),
// 所以 schema.go 的 bootstrapUniqueColumns 把 player_id 也列进了启动期守卫。
// 顺带:name_norm 就在索引项里(它是主键),这条读**不需要回表**。
func (s *PlayerNameStore) ownedNormOf(ctx context.Context, playerID uint64) (norm string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT name_norm FROM player_name WHERE player_id = ?`, playerID).Scan(&norm)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("player_name lookup by player_id=%d: %w", playerID, err)
	}
	return norm, true, nil
}

// ownerOfNorm 按**主键**读这个 name_norm 的占用者。found=false 表示这个名字没人占。
// 聚簇索引上的单行点查,一致性读、不取锁(改主键之前它是二级唯一索引点查 + 回表,现在省了那一次)。
func (s *PlayerNameStore) ownerOfNorm(ctx context.Context, norm string) (owner uint64, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT player_id FROM player_name WHERE name_norm = ?`, norm).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("player_name lookup by name_norm=%q: %w", norm, err)
	}
	return owner, true, nil
}

// createdMsOf 读 (playerID, name_norm) 这一行的登记时刻。found=false 表示没有行。
//
// 只给 Release 在"DELETE 一行没删到"之后定位成因用。刻意读 created_ms 而不是 `SELECT 1`:
// 少了这一列就分不清"行太老"(该拒绝)与"行是 DELETE 之后才提交的"(该重删),
// 而后者是 login 补偿路径的常规竞态,不是异常。
//
// 这里**不**钉索引:它是一致性读,不取任何锁,走 PRIMARY 还是走 uk_player_name_owner 都与锁序无关
// (两个条件都是等值、两条都是唯一约束,不管选哪条都是单行点查)。只有取锁的语句才需要 FORCE INDEX。
func (s *PlayerNameStore) createdMsOf(ctx context.Context, playerID uint64, norm string) (createdMs uint64, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT created_ms FROM player_name WHERE player_id = ? AND name_norm = ?`, playerID, norm).Scan(&createdMs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("player_name probe player_id=%d name_norm=%q: %w", playerID, norm, err)
	}
	return createdMs, true, nil
}

// reserveInserted 按 playerNameReserveSQL 的受影响行数判定"插入成功"(1)还是"撞上已有行、no-op"(0)。
// 其它值只可能是 ODKU 的更新被改成了真正改数据的写法(2 = 改了一行),此时判定口径已失效,fail-closed 报错,
// 绝不猜。口径的前提(连接不带 CLIENT_FOUND_ROWS)见 Reserve。
func reserveInserted(res sql.Result, playerID uint64) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("player_name reserve player_id=%d rows affected: %w", playerID, err)
	}
	switch n {
	case 1:
		return true, nil
	case 0:
		return false, nil
	default:
		return false, fmt.Errorf("player_name reserve player_id=%d: 受影响行数 %d 不在 {1=插入, 0=撞键 no-op} 之内,"+
			"playerNameReserveSQL 的 ON DUPLICATE KEY UPDATE 必须保持 no-op", playerID, n)
	}
}

// pauseBeforeLockRetry 记一次锁冲突重来,并做可取消的抖动退避。返回非 nil 表示 ctx 已结束,调用方应放弃重来。
func (s *PlayerNameStore) pauseBeforeLockRetry(ctx context.Context) error {
	s.lockRetries.Add(1)
	return sleepCtx(ctx, jitteredBackoff(playerNameLockRetryBackoffMin, playerNameLockRetryBackoffMax))
}

// isDuplicateEntryMySQL 判断是不是 1062(撞主键或唯一键)。
// Reserve 改成 ODKU 之后它只剩纵深防御用途(见 Reserve)。
func isDuplicateEntryMySQL(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDupEntry
}
