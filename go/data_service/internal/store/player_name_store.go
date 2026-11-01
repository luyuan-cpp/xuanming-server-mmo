package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// 玩家名字注册表(设计 docs/design/guild-phase2/03-names.md §3.3 / §3.4,分片见 docs/design/horizontal-scale-logical-db.md §5.5)。
//
// 两张表,可能不在一个库,不同事务:
//   - player_name 按 name_norm 分片,主键 name_norm。名字归谁只看这一行。
//   - player_name_owner 按 player_id 分片,主键 player_id。它是按玩家读名字的索引,也是「一个角色只登记一个名字」的实现。
//     两边不一致时以 player_name 为准。
//
// 不变量,按重要性排序:
//  1. **名字全服唯一**。唯一性只由 player_name 的**主键 name_norm** 保证(见 schema.go 的
//     playerNameBootstrapDDL)。本 store 从不用"先 SELECT 看有没有人占、没人占就 INSERT"来判重 ——
//     那在并发建角下必漏:两个请求同时查到"没人占"。判重的唯一形式是"直接写,撞了键再去看撞在谁身上"
//     (写法是 no-op 的 ODKU,撞键 = 受影响 0 行,见 Reserve)。
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
// 【崩溃窗口】(§3.7)名字先占、角色后建,两步不在一个事务里。中间崩掉会留下"名字被不存在的角色占住"的
// 孤儿行。补偿路径**全部在 login**:
//   - 已提交、login 超时 → login 以同 (player_id, name) 重试一次,拿到 ReserveAlreadyOwned = 成功;
//   - 仍然结果未知 / 后续步骤失败 → login 发一次条件 Release,并在约 10s 后再发一次
//     (覆盖"Release 先于在途 INSERT 提交"的竞态,两次都落在 10 分钟窗口内)。第二次 Release 若赶上
//     purge 滞后,会在第一次 Release 留下的删除标记记录上取锁、可能排队,单向等,不成环;
//   - 两次都失败或 login 自己也崩了 → ERROR 日志 + orphan 计数,运维带 x-admin-token
//     调 ReleasePlayerName(minCreatedMs=0,不限登记时间)清掉。
//
// 服务端**刻意不做**"ctx 一超时就补偿删除":login 的同名重试可能恰好在补偿 DELETE
// 之前拿到 AlreadyOwned(=建角成功),随后那条补偿就会把在役角色的名字删掉。补偿只能
// 由知道"这次建角到底成没成"的 login 发起。
//
// 同理**不做**自动孤儿清扫:只凭这张表看不出角色是否建成,照它批量释放会拿走在役角色的名字。

const (
	// PlayerNameBatchLimit 是 BatchGet 单次可查的 id 上限(契约 §3.1)。
	// 数字只在这里写一次:logic / server 层要拒绝超限请求时引用本常量,别再抄一份 500。
	PlayerNameBatchLimit = 500

	// playerNameReserveAttempts 是写 player_name 的尝试上限。会重来的只有两种可预期情况:
	// MySQL 1213/1205(锁冲突,isRetryableMySQL;重来前按 playerNameLockRetryBackoffMin/Max 抖动退避)
	// 与"撞键之后读不到这一行"(并发 Release 把行删了,名字已空出,这一轮直接重插、不退避)。
	// 其余一律立即返回。
	//
	// **下限是 2,不许调成 1**(player_name_store_test.go 机械守住):名字抢注那一类环已经在**结构上**根治了
	// (见 Reserve 的「根治」),但 1213 并没有归零,剩下的是锁继承(InnoDB 固有,见 Reserve 的「残余」)。
	// 被牺牲的那一方要靠**至少一次**重来才能拿到正确终局 Taken;上限为 1 时 1213 会直接作为存储错误(RPC 失败)
	// 返回给 login,玩家看到的是建角失败而不是"名字已被占用"。
	// "撞键后行已消失"的重插与锁冲突重试共用这一个预算,两者叠加在同一次调用里时只剩一次重来;
	// 所以取 3 而不是恰好 2,留一次余量。
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
	// Reserve 用 ODKU 之后撞键不再以 1062 报出(见 playerNameReserveSQL),这里只剩纵深防御用途。
	mysqlErrDupEntry = 1062

	// playerNameShapeProbeTimeout 是建 store 时逐片核表形状的时限。
	playerNameShapeProbeTimeout = 5 * time.Second
)

// playerNameReserveSQL 是 Reserve 写名字行。ODKU 的更新 `player_id = player_id` 引用的是**已有行**自己的列值
// (不是 VALUES()),是 no-op:撞键时一个字节都不改(created_ms 也不刷新),只把"撞了"从 1062 错误变成
// 受影响 0 行,并让重复键检查取 X 而不是 S。行数口径与前提(连接不带 CLIENT_FOUND_ROWS)见 Reserve 的注释。
// 抽成常量是为了让真库回归用例执行**同一段文本**。
const playerNameReserveSQL = "INSERT INTO player_name (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)" +
	" ON DUPLICATE KEY UPDATE player_id = player_id"

// playerNameOwnerUpsertSQL 是 Reserve 写 owner 行。撞键 = 这个玩家已经有 owner 行,no-op,随后读出来比名字。
const playerNameOwnerUpsertSQL = "INSERT INTO player_name_owner (player_id, name_norm, name) VALUES (?, ?, ?)" +
	" ON DUPLICATE KEY UPDATE player_id = player_id"

// playerNameOwnerDeleteSQL 按名字删 owner 行:这个玩家名下已经是别的名字时不动。
const playerNameOwnerDeleteSQL = "DELETE FROM player_name_owner WHERE player_id = ? AND name_norm = ?"

// playerNameReleaseForcedIndex 是 Release 钉死的索引名。name_norm 是聚簇主键,所以是 PRIMARY。
const playerNameReleaseForcedIndex = "PRIMARY"

// playerNameReleaseSQL 是 Release 的条件删除。单表 DELETE 不接受索引提示,所以写成多表 DELETE 语法,
// 删除语义与受影响行数与单表写法相同。真库用例用 EXPLAIN 钉住 key = PRIMARY 且是单行点查。
const playerNameReleaseSQL = "DELETE player_name FROM player_name FORCE INDEX (" + playerNameReleaseForcedIndex + ")" +
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

// PlayerNameStore 是 player_name、player_name_owner 的读写。
//
// 连接池独立于目录的其余 store:建角路径上每个请求都要同步走几条写,与快照、角色目录挤同一个池会互相拖慢。
// 每片一个池,池大小由装配层用 config 的 PlayerName 段覆盖 MySQLConfig.MaxOpenConn/MaxIdleConn 后传进来。
type PlayerNameStore struct {
	pools *shardPools

	// lockRetries 累计写名字行因 1213/1205 重来的次数(按实例,不是全局状态)。
	// 只给日志与真库回归用例用:只看"最终结果正确"分不清是写法真的不成环,还是环被重试悄悄吸收了。
	// "抢同一个名字"这条路上的判据只有一条:三种 player_id 顺序都必须是 0 次
	// (真库用例 TestPlayerName_ReserveBehindPendingReleaseOutcome)。
	lockRetries atomic.Uint64
}

// NewPlayerNameStore 逐片开自己的池(与其它 store 同一套 openMySQL),核两张表的主键。认不出来就建不出 store:
// 主键不对时 ODKU 撞不到键,"名字全服唯一"和"一个角色只登记一个名字"都会静默失效。
func NewPlayerNameStore(cfg MySQLConfig) (*PlayerNameStore, error) {
	pools, err := openShardPools(cfg, func(one MySQLConfig) (*sql.DB, error) {
		db, err := openMySQL(one)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), playerNameShapeProbeTimeout)
		defer cancel()
		if err := assertPlayerNameKeys(ctx, db, one.DBName); err != nil {
			_ = db.Close()
			return nil, err
		}
		return db, nil
	})
	if err != nil {
		return nil, err
	}
	logx.Infof("[PlayerNameStore] connected to %s/%s logical_shards=%d max_open_conn=%d max_idle_conn=%d",
		cfg.Host, cfg.DBName, pools.k, cfg.MaxOpenConn, cfg.MaxIdleConn)
	return &PlayerNameStore{pools: pools}, nil
}

// assertPlayerNameKeys 核 player_name 的主键是 name_norm、player_name_owner 的主键是 player_id。
func assertPlayerNameKeys(ctx context.Context, db *sql.DB, schema string) error {
	for _, shape := range []struct{ table, key string }{
		{PlayerNameTableName, "name_norm"},
		{PlayerNameOwnerTableName, "player_id"},
	} {
		keys, err := primaryKeyColumns(ctx, db, schema, shape.table)
		if err != nil {
			return fmt.Errorf("read primary key of %s: %w", shape.table, err)
		}
		if !sameColumnSequence(keys, []string{shape.key}) {
			return fmt.Errorf("table %s primary key is %v, want (%s); empty means the table is missing, run `data_service -f <yaml> -migrate` first",
				shape.table, keys, shape.key)
		}
	}
	return nil
}

// Close 关闭全部片的连接池。
func (s *PlayerNameStore) Close() error {
	return s.pools.Close()
}

// Reserve 把 (playerID, name, norm) 登记进两张表,返回终局与(仅 ReserveTaken 时的)占用者。
//
// 契约:
//   - name 是展示名、norm 是归一化后的判重键,两者必须由同一次 playername.Normalize 得出;
//     本层不校验它们是否自洽(那属于规则包,库层做不了)。
//   - nowMs 是登记时刻(Unix 毫秒),由调用方显式传入 —— 释放窗口靠这一列,时间不能藏在库里。
//   - 除 ReserveTaken 外,owner 一律返回 0:"占用者是谁"只在被别人占住时才有意义,
//     其余情况返回 playerID 只会诱使调用方把它当成别的语义。
//   - 返回 ReserveInserted / ReserveAlreadyOwned 时两张表都**已经提交**,调用方可以安全地接着写缓存。
//
// # 顺序(两张表可能不在一个库,每步一条自动提交语句)
//
//  0. 读 owner 行(玩家分片)。指着别的名字时去名字分片核:确实是自己的 → ReserveConflict,什么都不写;
//     已经没了或归了别人 → 这条 owner 行是上次 Release 没清掉的,按名字删掉。
//  1. 写名字行(名字分片,ODKU)。**这是提交点**:名字归谁只看这一行。撞上别人 → ReserveTaken,不写 owner 行;
//     这时第 0 步如果读到 owner 行指着这个名字,它也是没清掉的,顺手删(删失败只记日志,结局不变)。
//  2. 写 owner 行(玩家分片,ODKU)。
//
// 中断和重放的结局:
//   - 第 2 步失败:回错误,名字行留着。login 按结局未知处理:同参数重试走 AlreadyOwned 把 owner 行补上,或者 Release 把两边都清掉。
//   - 第 2 步发现 owner 行已是别的名字:第 0 步之后这个 player_id 又登记了另一个名字,只有 ID 复用事故到得了。
//     本次新插入的名字行撤销,回 ReserveConflict。撤销失败回错误,不回 ReserveConflict:那会映射成确定性的拒绝,
//     而名字行其实留下了(规格 §3.6)。日志照记,由运维释放。
//   - 第 2 步发现 owner 行没了:并发的 Release 删的,回错误,login 按结局未知处理。
//   - 同参数重放:每步幂等,结局 ReserveAlreadyOwned。
//
// # 写法:INSERT … ON DUPLICATE KEY UPDATE player_id = player_id
//
// 判重只靠库上的主键("直接写,撞了再看撞在谁身上"),"撞了"从 1062 错误变成受影响 0 行:
//   - 1 行 = 插入成功 → ReserveInserted。
//   - 0 行 = 撞上已有行,no-op 更新一个字节都没改。读这一行:是自己 → AlreadyOwned;是别人 → Taken;
//     读不到(并发 Release 把行删了,名字已空出)→ 重插。
//   - 其它行数(2 = 真的改了一行)在 no-op 更新下不可能出现;出现即说明语句被改,fail-closed 报错。
//
// 前提:连接**不**带 CLIENT_FOUND_ROWS(DSN 的 clientFoundRows)。带了之后 no-op 的 ODKU 也回 1(找到的行数),
// 与"插入成功"无法区分,撞名会被当成 Inserted —— 而名字其实属于别人。buildDSN 不设它,
// player_name_store_test.go 的 TestStoreDSNDoesNotSetClientFoundRows 机械守住。
//
// # 根治:名字是聚簇主键 + ODKU,两个条件缺一不可(审计 #12,用户 2026-09-29 拍板)
//
// 实测表见 schema.go 的 playerNameBootstrapDDL。三条可以直接读出来的事实:
//  1. **只改写法不够**。二级唯一索引配 ODKU 在"竞争者 id 小于原主人"那一路仍然 5/5 成环。
//  2. **只改结构也不够**。聚簇主键配普通 INSERT 是 15/15 全成环 —— 聚簇记录上的重复检查取 S,
//     两个 S 同时被授予之后各自要把它升级成 X,互相挡住(手册 "Locks Set by Different SQL Statements in InnoDB"
//     的三会话例就建在聚簇主键上)。
//  3. **两个一起上才是 0/15**。这就是现在的形态。
//
// 机理:聚簇索引撞上同键的**删除标记记录**时走 row_ins_must_modify_rec → row_ins_clust_index_entry_by_modify,
// 是**原地改写**,取的是 LOCK_X + LOCK_REC_NOT_GAP(不带间隙),**根本不申请插入意向锁**。
// 而二级索引上那条环每一种形状的最后一步都是"插入意向锁被对方排队中的 next-key 请求挡住";
// 没有插入意向锁,这条边就不存在。手册对 ODKU 的原话是撞主键只拿记录锁,撞二级唯一键拿 X next-key。
//
// 可以推广的判据(别的表照此自查):**当"被争抢的键"是二级唯一索引、且新记录的主键可能小于
// 删除标记那条记录的主键时,ODKU 不够用 —— 要么把被争抢的键做成聚簇主键,要么承认这条环。**
//
// # 残余:仍然可能出现的 1213
//
// **锁继承**(InnoDB 固有,SQL 层去不掉,与写法和结构都无关)。排队者都在等某条记录上的锁时,这条记录被
// **物理删除** —— 先到的插入者回滚(ctx 取消断连、被选为别处死锁的牺牲者),或 purge 在排队期间清掉了删除标记
// 记录 —— InnoDB 会把它上面的锁继承成后继记录上的**间隙锁**(RR 下;间隙锁彼此兼容),
// 于是 ≥2 个排队者各持间隙锁、再互等插入意向锁 → 1213。探针的编排刻意用 purge 挡板排除了这一类,
// 所以 0/15 是"名字抢注这条路上 0 次",不是"这张表上永远不会再有 1213"。
//
// 处理:isRetryableMySQL 命中时按 playerNameLockRetryBackoffMin/Max 抖动退避(ctx 可取消)后重来,
// 总次数不超过 playerNameReserveAttempts。被牺牲方的语句自动提交,整条回滚、锁全部放掉;另一方随即
// 前进并提交,牺牲方退避几十毫秒后的下一轮看见活行 → no-op → Taken。预算用尽按存储错误返回(login fail-closed,拒绝建角)。
//
// TiDB(悲观事务)按官方文档没有间隙锁与插入意向锁,上述几类形状都不存在(**未在 TiDB 上实测**)。
func (s *PlayerNameStore) Reserve(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (ReserveOutcome, uint64, error) {
	if playerID == 0 || norm == "" {
		return 0, 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}

	ownedNorm, hasOwn, err := s.ownedNormOf(ctx, playerID)
	if err != nil {
		return 0, 0, err
	}
	if hasOwn && ownedNorm != norm {
		holder, held, err := s.ownerOfNorm(ctx, ownedNorm)
		if err != nil {
			return 0, 0, err
		}
		if held && holder == playerID {
			logPlayerNameConflict(playerID, ownedNorm, norm)
			return ReserveConflict, 0, nil
		}
		if err := s.dropOwnerRow(ctx, playerID, ownedNorm); err != nil {
			return 0, 0, err
		}
		hasOwn = false
	}

	outcome, owner, err := s.reserveNameRow(ctx, playerID, name, norm, nowMs)
	if err != nil {
		return 0, 0, err
	}
	if outcome == ReserveTaken {
		if hasOwn {
			if err := s.dropOwnerRow(ctx, playerID, norm); err != nil {
				logx.Errorf("[PlayerNameStore] player_id=%d stale owner row of name_norm=%q left in place: %v", playerID, norm, err)
			}
		}
		return ReserveTaken, owner, nil
	}

	otherNorm, err := s.writeOwnerRow(ctx, playerID, name, norm)
	if err != nil {
		return 0, 0, err
	}
	if otherNorm != "" {
		logPlayerNameConflict(playerID, otherNorm, norm)
		if outcome == ReserveInserted {
			if _, err := s.pools.forName(norm).ExecContext(ctx, playerNameReleaseSQL, playerID, norm, nowMs); err != nil {
				logx.Errorf("[PlayerNameStore] player_id=%d failed to undo name_norm=%q after conflict; release it by hand: %v", playerID, norm, err)
				return 0, 0, fmt.Errorf("player_name undo player_id=%d: %w", playerID, err)
			}
		}
		return ReserveConflict, 0, nil
	}
	return outcome, 0, nil
}

// logPlayerNameConflict 记 ID 复用事故。打 Error 是因为本层掌握的信息(旧 norm)比调用方多,而且这条日志是事后追查的唯一线索。
func logPlayerNameConflict(playerID uint64, ownedNorm, norm string) {
	logx.Errorf("[PlayerNameStore] player_id=%d already holds name_norm=%q, refused to register %q: "+
		"v1 has no rename, so this means the player id was reused (id allocator reset?); do NOT delete the old row, it belongs to a live character",
		playerID, ownedNorm, norm)
}

// reserveNameRow 写名字行,回 Inserted、AlreadyOwned 或 Taken(带占用者)。
func (s *PlayerNameStore) reserveNameRow(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (ReserveOutcome, uint64, error) {
	db := s.pools.forName(norm)
	for attempt := 1; attempt <= playerNameReserveAttempts; attempt++ {
		res, err := db.ExecContext(ctx, playerNameReserveSQL, playerID, name, norm, nowMs)
		switch {
		case err == nil:
			inserted, err := reserveInserted(res, playerID)
			if err != nil {
				return 0, 0, err
			}
			if inserted {
				return ReserveInserted, 0, nil
			}
			// 0 行:撞上了已有行,落到下面看撞在谁身上。
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
			// ODKU 下撞键不会以 1062 报出。仍按"撞上已有行"处理,只是纵深防御:语句万一被改回普通 INSERT,对外语义也不变。
		default:
			return 0, 0, fmt.Errorf("player_name reserve insert player_id=%d: %w", playerID, err)
		}

		owner, hasOwner, err := s.ownerOfNorm(ctx, norm)
		if err != nil {
			return 0, 0, err
		}
		if hasOwner {
			if owner == playerID {
				return ReserveAlreadyOwned, 0, nil
			}
			return ReserveTaken, owner, nil
		}

		// 撞键那一刻存在的行,被并发的 Release 在这两步之间删掉了。名字现在是空的,下一轮直接重插(没有锁冲突,不退避)。
		logx.Infof("[PlayerNameStore] player_id=%d reserve retry %d/%d: duplicate row vanished (concurrent release)",
			playerID, attempt, playerNameReserveAttempts)
	}
	return 0, 0, fmt.Errorf("player_name reserve: player_id=%d name_norm=%q exhausted %d attempts",
		playerID, norm, playerNameReserveAttempts)
}

// writeOwnerRow 写 owner 行。otherNorm 非空 = 这个玩家名下已经是另一个名字,什么都没写。
func (s *PlayerNameStore) writeOwnerRow(ctx context.Context, playerID uint64, name, norm string) (otherNorm string, err error) {
	res, err := s.pools.forPlayer(playerID).ExecContext(ctx, playerNameOwnerUpsertSQL, playerID, norm, name)
	if err != nil {
		return "", fmt.Errorf("player_name_owner upsert player_id=%d: %w", playerID, err)
	}
	inserted, err := reserveInserted(res, playerID)
	if err != nil {
		return "", err
	}
	if inserted {
		return "", nil
	}
	owned, found, err := s.ownedNormOf(ctx, playerID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("player_name_owner upsert player_id=%d: duplicate row vanished (concurrent release)", playerID)
	}
	if owned == norm {
		return "", nil
	}
	return owned, nil
}

// dropOwnerRow 按名字删 owner 行。删不到不算错。
func (s *PlayerNameStore) dropOwnerRow(ctx context.Context, playerID uint64, norm string) error {
	if _, err := s.pools.forPlayer(playerID).ExecContext(ctx, playerNameOwnerDeleteSQL, playerID, norm); err != nil {
		return fmt.Errorf("player_name_owner delete player_id=%d: %w", playerID, err)
	}
	return nil
}

// Release 条件删除 (playerID, name_norm):先删名字行,再删 owner 行。
//
// minCreatedMs 是登记时刻的下界(Unix 毫秒,含):
//   - 0 = 不限时间。**只有**已经通过 admin token 校验的运维路径能传 0。
//   - > 0 = 只删这么新的登记。login 的建角补偿走这一支(now - PlayerName.ReleaseWindow),
//     这道窗拦的是"拿别人刚建好的号的名字来释放"—— 少了它,任何能发 RPC 的内部调用方
//     都能把在役角色的名字删掉,而名字一旦释放就可能立刻被别人占走,不可回滚。
//
// 幂等:行不在(或早被删)返回 ReleaseAbsent,不是错误。
//
// 顺序:名字行优先,那是真相;ReleaseOutsideWindow 时两边都不动。删 owner 行失败回错误,调用方重放时名字行已不在,
// 走 ReleaseAbsent 接着删 owner 行。一直删不掉也只留下一条指向空名字的 owner 行:这个角色建角失败了,没有人读它的名字;
// 同一个 player_id 再 Reserve 时第 0 步会清掉。
//
// # 锁序
//
// DELETE 走聚簇索引(playerNameReleaseSQL 钉 PRIMARY),按碰到的记录分三种:
//   - 活行:在聚簇记录上取 X **记录锁、不带间隙** —— 手册对 DELETE 的原话是唯一索引定位唯一行时只需要记录锁。
//     所以它挡不住别人往相邻间隙插入,只会与要改同一行的写者单向排队。
//   - 名字完全不存在:没有"唯一行"可锁,退回一般规则,RR 下在聚簇索引上留间隙锁,插入者只会单向等它。
//   - **未 purge 的删除标记记录**(名字已被释放过,典型触发是 login 约 10s 后的第二次补偿 Release 赶上
//     purge 滞后):在那条记录上取 X、可能排队;抢同一个名字的 Reserve 在聚簇索引上做的是原地改写,不申请插入意向锁,
//     不会反过来卡住本条。这一路按保守口径记可能带间隙,会挡住相邻名字的插入,但仍是单向等、不成环。
//
// 锁冲突(1213/1205)按 playerNameLockRetryBackoffMin/Max 抖动退避后重来(ctx 可取消),次数不超过
// playerNameReleaseAttempts。DELETE 幂等,重来安全;被牺牲的若是本条 Release,下一轮看到的是新主人的活行
// (player_id 对不上,不删)或仍只有删除标记记录 → ReleaseAbsent,不会误删新主人的行。
func (s *PlayerNameStore) Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error) {
	if playerID == 0 || norm == "" {
		return 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}
	outcome, err := s.releaseNameRow(ctx, playerID, norm, minCreatedMs)
	if err != nil {
		return 0, err
	}
	if outcome == ReleaseOutsideWindow {
		return outcome, nil
	}
	if err := s.dropOwnerRow(ctx, playerID, norm); err != nil {
		return 0, err
	}
	return outcome, nil
}

func (s *PlayerNameStore) releaseNameRow(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error) {
	db := s.pools.forName(norm)
	for attempt := 1; attempt <= playerNameReleaseAttempts; attempt++ {
		res, err := db.ExecContext(ctx, playerNameReleaseSQL, playerID, norm, minCreatedMs)
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

// BatchGet 按 player_id 批量读展示名:按玩家分片拆组读 player_name_owner,不做全分片扫描。任一片失败整批报错,不回半份。
//
// 契约:
//   - 缺席的 id **不出现**在结果 map 里,不是错误(与 BatchGetPlayerHomeZone 同口径)。
//   - 去重与"读缓存、回填缓存"属于 logic;本层只负责查询。
//   - id 为 0 的项被跳过(0 不是合法 player_id,也不可能有行),不算错误。
//   - 超过 PlayerNameBatchLimit 直接拒绝:一条无上限的 IN 会把单次查询变成事实上的全表读。
//
// 不回名字分片核对:两边不一致只发生在建角失败的角色上(Release 没清完 owner 行),没有人读它们的名字。
// 判定名字归属的路径(Reserve)都以 player_name 为准。
func (s *PlayerNameStore) BatchGet(ctx context.Context, playerIDs []uint64) (map[uint64]string, error) {
	out := make(map[uint64]string, len(playerIDs))
	if len(playerIDs) == 0 {
		return out, nil
	}
	if len(playerIDs) > PlayerNameBatchLimit {
		return nil, fmt.Errorf("%w: got %d, limit %d", ErrPlayerNameBatchTooLarge, len(playerIDs), PlayerNameBatchLimit)
	}

	ids := make([]uint64, 0, len(playerIDs))
	for _, id := range playerIDs {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	var mu sync.Mutex
	err := s.pools.eachPlayerGroup(ids, func(db *sql.DB, players []uint64) error {
		return batchGetOwnerRows(ctx, db, players, out, &mu)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func batchGetOwnerRows(ctx context.Context, db *sql.DB, players []uint64, out map[uint64]string, mu *sync.Mutex) error {
	placeholders := make([]string, 0, len(players))
	args := make([]any, 0, len(players))
	for _, id := range players {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	query := "SELECT player_id, name FROM player_name_owner WHERE player_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("player_name batch get (%d ids): %w", len(args), err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uint64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("player_name batch get scan: %w", err)
		}
		mu.Lock()
		out[id] = name
		mu.Unlock()
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("player_name batch get iterate: %w", err)
	}
	return nil
}

// ownedNormOf 读这个 player_id 的 owner 行。found=false 表示没有行。主键点查,最多一行。
func (s *PlayerNameStore) ownedNormOf(ctx context.Context, playerID uint64) (norm string, found bool, err error) {
	err = s.pools.forPlayer(playerID).QueryRowContext(ctx,
		`SELECT name_norm FROM player_name_owner WHERE player_id = ?`, playerID).Scan(&norm)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("player_name_owner lookup by player_id=%d: %w", playerID, err)
	}
	return norm, true, nil
}

// ownerOfNorm 按**主键**读这个 name_norm 的占用者。found=false 表示这个名字没人占。一致性读、不取锁。
func (s *PlayerNameStore) ownerOfNorm(ctx context.Context, norm string) (owner uint64, found bool, err error) {
	err = s.pools.forName(norm).QueryRowContext(ctx,
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
	err = s.pools.forName(norm).QueryRowContext(ctx,
		`SELECT created_ms FROM player_name WHERE player_id = ? AND name_norm = ?`, playerID, norm).Scan(&createdMs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("player_name probe player_id=%d name_norm=%q: %w", playerID, norm, err)
	}
	return createdMs, true, nil
}

// reserveInserted 按 ODKU 的受影响行数判定"插入成功"(1)还是"撞上已有行、no-op"(0)。
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
			"ON DUPLICATE KEY UPDATE 必须保持 no-op", playerID, n)
	}
}

// pauseBeforeLockRetry 记一次锁冲突重来,并做可取消的抖动退避。返回非 nil 表示 ctx 已结束,调用方应放弃重来。
func (s *PlayerNameStore) pauseBeforeLockRetry(ctx context.Context) error {
	s.lockRetries.Add(1)
	return sleepCtx(ctx, jitteredBackoff(playerNameLockRetryBackoffMin, playerNameLockRetryBackoffMax))
}

// isDuplicateEntryMySQL 判断是不是 1062(撞主键或唯一键)。
// Reserve 用 ODKU 之后它只剩纵深防御用途(见 reserveNameRow)。
func isDuplicateEntryMySQL(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDupEntry
}
