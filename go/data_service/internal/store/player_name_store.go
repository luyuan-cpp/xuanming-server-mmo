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
//  1. **名字全服唯一**。唯一性只由 player_name.name_norm 上的 UNIQUE KEY 保证(见
//     schema.go 的 playerNameBootstrapDDL)。本 store 从不用"先 SELECT 看有没有人占、
//     没人占就 INSERT"来判重 —— 那在并发建角下必漏:两个请求同时查到"没人占"。
//     判重的唯一形式是"直接写,撞了唯一键再去看撞在谁身上"(写法是 no-op 的 ODKU,撞键 = 受影响 0 行,
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
//     purge 滞后,会在唯一键上第一次 Release 留下的删除标记项上取锁、可能排队,见 Release 的「锁序」;
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
	//   - 写法改成 ODKU(playerNameReserveSQL)只拆掉了"两个排队者都已持 S、再互等插入意向锁"那一种环,
	//     Reserve 仍会收到 1213,来源有两类(见 Reserve 的「残余」):A = InnoDB 固有的锁继承;B = 新项要插在
	//     唯一键的删除标记项**之前**(胜者 player_id < 删除标记原主人,号段 id 抢存量角色释放的名字时是常态),
	//     按源码静态推演会成环、未经真库确认。两类里被牺牲方都要靠**至少一次**重来才能拿到正确终局 Taken;
	//     上限为 1 时 1213 会直接作为存储错误(RPC 失败)返回给 login,玩家看到的是建角失败而不是"名字已被占用"。
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

// playerNameReleaseSQL 是 Release 的条件删除。强制走 uk_player_name(先锁唯一键项、后锁聚簇记录),
// 与 Reserve 撞唯一键时的取锁顺序一致;单表 DELETE 不接受索引提示,所以写成多表 DELETE 语法,
// 删除语义与受影响行数与单表写法相同。理由见 Release 的「锁序」,真库用例用 EXPLAIN 钉住。
const playerNameReleaseSQL = "DELETE player_name FROM player_name FORCE INDEX (uk_player_name)" +
	" WHERE player_id = ? AND name_norm = ? AND created_ms >= ?"

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

	// lockRetries 累计 Reserve / Release 因 1213/1205 重来的次数(按实例,不是全局状态)。
	// 只给日志与真库回归用例用:只看"最终结果正确"分不清是写法真的不成环,还是环被重试悄悄吸收了。
	// 用例据此区分:竞争者 player_id 都大于删除标记原主人时必须一次都没有;反之(Reserve 的「残余 B」)
	// 允许至多一次,并记录实际次数。
	lockRetries atomic.Uint64
}

// NewPlayerNameStore 在全局库上开自己的池(与其它 store 同一套 openMySQL)。
func NewPlayerNameStore(cfg MySQLConfig) (*PlayerNameStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}
	logx.Infof("[PlayerNameStore] connected to %s/%s max_open_conn=%d max_idle_conn=%d",
		cfg.Host, cfg.DBName, cfg.MaxOpenConn, cfg.MaxIdleConn)
	return &PlayerNameStore{db: db}, nil
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
// 判重仍然只靠唯一键("直接写,撞了再看撞在谁身上"),只是"撞了"从 1062 错误变成了受影响 0 行:
//   - 1 行 = 插入成功 → ReserveInserted。
//   - 0 行 = 撞上已有行,no-op 更新一个字节都没改。撞在哪一行(表上两个唯一约束:PK player_id、uk name_norm):
//     只撞主键(这个 player_id 已登记过,同名重试或别的名字)→ 更新落在 player_id 那一行;
//     只撞唯一键(名字被别人占了)→ 更新落在占用者那一行;
//     两者都撞且不是同一行 → InnoDB 先插聚簇索引、主键先报重复,更新落在 player_id 那一行。
//     三种都是 no-op、都回 0 行;随后照旧用两条等值读(ownedNormOf → ownerOfNorm)分辨 AlreadyOwned /
//     Conflict / Taken / 行已消失(重插)—— 与改写前 1062 之后的分支相同,对外返回值、日志、ID 复用告警都不变。
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
// # 为什么是 ODKU:重复检查从 S 改成 X(审计 #12)—— 只拆掉了一半
//
// 改写前是普通 INSERT,重复检查取 **S**。MySQL 手册里那条三方交错:Release 的 DELETE 持着 'abc' 的 X
// 尚未提交,两个同名 Reserve 的重复检查都排在它后面等 S;DELETE 一提交,两边**同时**拿到 'abc'(已是
// 删除标记记录)上的 S next-key,再各自申请插入意向锁,被对方**已授予**的 S 挡住 → 1213,而且与 player_id
// 的大小顺序无关。ODKU 的重复检查直接取 **X**(next-key):排队者只能一个一个拿到,"两边都已持锁、再互等插入
// 意向锁"这一种形状不再出现。
//
// **这不等于删除标记项上的环都没了。** 后到者的 X 请求**正在排队**时,先到者插入的新项若落在删除标记项之前,
// 插入意向锁仍会被这个排队中的请求挡住,两方成环 —— 见「残余 B」。不许把本节读成"S→X 环已消失"。
// 相比普通 INSERT,ODKU 在任何 player_id 顺序下都不更差(普通 INSERT 两种顺序都会成环,ODKU 只剩「残余 B」
// 那种顺序),所以保留;要在 SQL 层根除须改 schema,见「残余 B」。
//
// # 锁序:先唯一键、后聚簇 —— Release 必须同序
//
// 撞唯一键时 ODKU 先在 uk_player_name 上取 X(重复检查),再按唯一键读回占用者那一行、取它**聚簇**记录的 X
// (要对它执行那条 no-op 更新)。所以同表另一个写者 Release 也必须"先唯一键、后聚簇";按主键删(先聚簇、
// 后唯一键)会与这里反序 —— Release 持占用者的聚簇 X 等唯一键,Reserve 持唯一键 X 等聚簇 → 1213,
// 而且只要两方就够。playerNameReleaseSQL 因此强制走 uk_player_name(见 Release)。撞主键时只碰聚簇记录。
//
// # 与其它写者的交错(本表的写者只有 Reserve / Release;三条读都是不加锁的一致性读)
//   - Release(P_old,'abc') 未提交 + 两个 Reserve 抢 'abc':X 排队。先拿到 X 的胜者 player_id > P_old 时,新项插在
//     删除标记项之后,单向等、不成环;< P_old 时落入「残余 B」形状 1。
//   - Release(Q,'abc') 与撞上 Q 那一行(活行)的 Reserve:两边都先唯一键后聚簇,只会单向等。
//   - Release 走唯一键碰上未 purge 的删除标记项(名字已被释放过,例如 login 的第二次补偿 Release 赶上 purge 滞后):
//     在该项上排队,可能与正持有该项、要插在它前面的 Reserve 成环 →「残余 B」形状 2。
//   - 两个 Reserve 抢同一个名字:唯一键上**没有**这个名字的任何项(连删除标记项也没有)时,先插入者的新项带隐式锁,
//     后到者的重复检查单向等它提交 → no-op → Taken。名字刚被释放、删除标记项还没被 purge 时,**不需要**有待提交的
//     Release,两个 Reserve 自己就会排在删除标记项上,与第一条同形(胜者 < P_old 时落入「残余 B」形状 1)。
//   - 同一 player_id 的并发 Reserve / Release:都要这一行的聚簇 X(Release 先唯一键,Reserve 撞主键时不碰
//     唯一键),单向等。
//   - Reserve 插入成功时先插自己的新聚簇记录、后碰唯一键;没有别的写者会持着唯一键去等这条新记录
//     (能指向它的唯一键项只能是它自己刚插的,或已是删除标记 —— 加锁读会跳过删除标记项,不回表)。
//
// # 残余:两类 1213,都由有界重试兜住
//
// A. 锁继承(InnoDB 固有,SQL 层去不掉)。排队者都在等某条记录上的锁时,这条记录被**物理删除** —— 先到的插入者
// 回滚(ctx 取消断连、被选为别处死锁的牺牲者),或 purge 在排队期间清掉了删除标记记录 —— InnoDB 会把它上面的锁
// 继承成后继记录上的**间隙锁**(RR 下;间隙锁彼此兼容,X / S 都一样),于是 ≥2 个排队者各持间隙锁、再互等插入
// 意向锁 → 1213。这是手册"Locks Set by Different SQL Statements"三会话例的同类,与写法无关。
//
// B. 新项插在删除标记项之前(按 InnoDB 源码静态推演,**未经真库确认**,在确认前一律按"会成环"对待)。
// uk_player_name 的二级索引项实际按 (name_norm, player_id) 排序。胜者 T1 在删除标记项 ('abc', P_old) 上拿到
// X next-key 之后要插 ('abc', P_new):P_new < P_old 时,插入位置的后继记录正是 ('abc', P_old)。插入意向锁检查
// 后继记录时,把别的事务在其上的 next-key 请求当作冲突,不管它已授予还是**还在等待**(lock_rec_insert_check_and_lock
// 源码注释的原话是 "waiting or granted");而那个排队中的请求等的正是 T1 已授予的 X → 两方成环。REPLACE 与 ODKU
// 的重复检查都取 X、走同一条路径,已知的"并发 REPLACE INTO 撞删除标记记录"死锁与此同形。三种形状:
//  1. 另一个抢同名的 Reserve 排在该删除标记项上,胜者 player_id < 原主人(有无待提交的 Release 都一样);
//  2. Release 走唯一键排在该删除标记项上(见 Release 的「锁序」);
//  3. 跨名字:相邻两个名字都是删除标记,抢前一个名字的 Reserve 扫到后继名字的删除标记项时排在抢后一个名字的
//     Reserve 后面,而后者的插入位置正好在这一项之前。
//
// 这在生产里是常态,不是罕见交错:号段发出的 player_id 都 < 2^55(IdSegmentCap),存量 snowflake id 都 > 2^55,
// 新玩家抢老角色释放的名字时 P_new < P_old 恒成立;不同 login 实例各持一个号段,彼此的大小顺序也是任意的。
// 反方向的可能:较新的 MySQL 8.0 在"请求方自己已授予的锁正挡着那个排队请求"时允许越过它(源码中的
// has_granted_blocker / CAN_BYPASS,Bug#11745929 一线),那样 B 不成环 —— 这一点凭源码记忆、版本边界未核实,
// 以真库用例 TestPlayerName_ReleaseRacesTwoReservesConverges 的 (b)(c) 子用例在所部署版本上的实际结果为准。
// SQL 层能根除 B 的路线要改 schema(把名字放进以 name_norm 为聚簇主键的表:聚簇索引撞删除标记记录是原地改写,
// 不需要插入意向锁);按名字 GET_LOCK 串行化消不掉形状 3。是否改 schema 由用户决策(超出本组范围),决策前
// 靠下面的有界重试兜住。
//
// 处理(A、B 相同):isRetryableMySQL 命中时按 playerNameLockRetryBackoffMin/Max 抖动退避(ctx 可取消)后重来,
// 总次数不超过 playerNameReserveAttempts。收敛论证:被牺牲方的语句自动提交,整条回滚、锁全部放掉;另一方随即
// 前进 —— B 里若牺牲的是排队者,插入者完成插入并提交;若牺牲的是插入者,排队者接过删除标记项上的 X、自己插入
// 并提交。牺牲方退避几十毫秒后的下一轮看见活行 → no-op → Taken(Release 则看到行已不属于自己 → Absent),
// 通常一次重来即收敛。要再成环,必须在这几十毫秒里**又**出现一个被抢的删除标记项加一个排队者、或又一次先到者
// 回滚 / purge 恰好清掉排队中的记录;每成一次环都有一方前进,有界;预算用尽按存储错误返回(login fail-closed,
// 拒绝建角),不无限转。
//
// TiDB(悲观事务)没有间隙锁与插入意向锁,A、B 两类形状都不存在(按文档推断,未在 TiDB 上实测)。
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
// # 锁序:先唯一键、后聚簇(playerNameReleaseSQL)
//
// DELETE 走哪条索引,决定它先锁哪条记录:按主键 → 先聚簇、后唯一键项;按 uk_player_name → 先唯一键项、
// 后聚簇。Reserve 的 ODKU 撞唯一键时是"唯一键 → 聚簇"(见 Reserve 的「锁序」),Release 必须同序,否则两者
// 在占用者那一行上反序取锁成环。WHERE 里主键与唯一键都是等值条件,优化器按代价可能挑主键(聚簇读省一次
// 回表),所以不能交给优化器:用 FORCE INDEX (uk_player_name) 钉死。单表 DELETE 不接受索引提示,只能写成
// 多表 DELETE 语法,删除语义与受影响行数不变。真库用例 TestPlayerName_ReleaseStatementLocksUniqueKeyFirst
// 用 EXPLAIN 钉住 key = uk_player_name。
// 走唯一键时,按唯一键上碰到的项分三种:
//   - 活行:在唯一键项与聚簇记录上各取 X(记录锁);player_id / created_ms 两个条件在读到那一行之后判定,
//     不匹配时 RR 下那两把锁持有到语句结束(自动提交,极短),不删任何东西。
//   - 名字完全不存在(唯一键上连删除标记项都没有):RR 下只在唯一键上留一把间隙锁,插入者只会单向等它。
//   - **未 purge 的删除标记项**(名字已被释放过):在该项上取 X next-key、在后继记录上取 X 间隙锁,并可能在该项上
//     **排队**。改写前按主键删只碰聚簇记录,碰不到这一项;uk 优先是为了消掉上面那个两方反序环,代价是 Release 自己
//     可能成为 Reserve「残余 B」形状 2 的一方:正持有这一项、且新项要插在它前面(新 player_id < 该项原主人)的
//     Reserve,插入意向锁会被本条排队中的请求挡住 → 两方成环。典型触发是 login 约 10s 后的第二次补偿 Release
//     赶上 purge 滞后(文件头「崩溃窗口」)。由下面的有界重试兜住;是否从 schema 上根除见 Reserve 的「残余 B」。
//
// 锁冲突(1213/1205)按 playerNameLockRetryBackoffMin/Max 抖动退避后重来(ctx 可取消),次数不超过
// playerNameReleaseAttempts。DELETE 幂等,重来安全;形状 2 里被牺牲的若是本条 Release,它要删的行早已不在,
// 下一轮看到的是新主人的活行(player_id 对不上,不删)或仍只有删除标记项 → ReleaseAbsent,不会误删新主人的行。
func (s *PlayerNameStore) Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error) {
	if playerID == 0 || norm == "" {
		return 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}

	for attempt := 1; attempt <= playerNameReleaseAttempts; attempt++ {
		res, err := s.db.ExecContext(ctx, playerNameReleaseSQL, playerID, norm, minCreatedMs)
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

// ownedNormOf 按主键读这个 player_id 已登记的 name_norm。found=false 表示没有行。
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

// ownerOfNorm 按唯一键读这个 name_norm 的占用者。found=false 表示这个名字没人占。
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
