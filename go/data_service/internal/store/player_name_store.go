package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// 玩家名字注册表(设计 docs/design/guild-phase2/03-names.md §3.3 / §3.4)。
//
// 不变量,按重要性排序:
//  1. **名字全服唯一**。唯一性只由 player_name.name_norm 上的 UNIQUE KEY 保证(见
//     schema.go 的 playerNameBootstrapDDL)。本 store 从不用"先 SELECT 看有没有人占、
//     没人占就 INSERT"来判重 —— 那在并发建角下必漏:两个请求同时查到"没人占"。
//     判重的唯一形式是"直接 INSERT,撞了 1062 再去看撞在谁身上"。
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
//     (覆盖"Release 先于在途 INSERT 提交"的竞态,两次都落在 10 分钟窗口内);
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
	// MySQL 1213/1205(锁冲突,isRetryableMySQL)与"1062 之后两条等值读都为空"
	// (并发 Release 把行删了,这一轮可以直接重插)。其余一律立即返回。
	playerNameReserveAttempts = 3

	// playerNameReleaseAttempts Release 的尝试上限。DELETE 本身幂等,重试安全;
	// 多争取一次成功能直接少一条孤儿(失败的释放就是孤儿,见上面的崩溃窗口)。
	playerNameReleaseAttempts = 3

	// mysqlErrDupEntry 1062 ER_DUP_ENTRY:撞主键或唯一键。
	// 它在本 store 里是**正常控制流**(判重的实现方式),不是故障。
	mysqlErrDupEntry = 1062
)

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
//   - 无显式事务:单条 INSERT 自己就是提交点。返回 ReserveInserted / ReserveAlreadyOwned 时
//     数据**已经提交**,调用方可以安全地接着写缓存。
//
// 并发抢同名:InnoDB 的唯一键检查会让后到的 INSERT 等在先到者的锁上;先到者提交后,
// 后到者拿到 1062。autocommit 下随后的每条 SELECT 都是新快照,一定读得到刚提交的那行,
// 所以"1062 之后去查是谁占的"不会查空(真查空只可能是并发 Release 删掉了,那就重插)。
func (s *PlayerNameStore) Reserve(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (ReserveOutcome, uint64, error) {
	if playerID == 0 || norm == "" {
		return 0, 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}

	for attempt := 1; attempt <= playerNameReserveAttempts; attempt++ {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO player_name (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)`,
			playerID, name, norm, nowMs)
		if err == nil {
			return ReserveInserted, 0, nil
		}

		if isRetryableMySQL(err) {
			if attempt < playerNameReserveAttempts {
				logx.Infof("[PlayerNameStore] player_id=%d reserve retry %d/%d after lock error: %v",
					playerID, attempt, playerNameReserveAttempts, err)
				continue
			}
			return 0, 0, fmt.Errorf("player_name reserve insert player_id=%d: %w", playerID, err)
		}
		if !isDuplicateEntryMySQL(err) {
			return 0, 0, fmt.Errorf("player_name reserve insert player_id=%d: %w", playerID, err)
		}

		// 1062:撞的是主键还是唯一键,两条等值读分开问。
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

		// 两条都空:1062 那一刻存在的行,被并发的 Release 在这两次读之间删掉了。
		// 名字现在是空的,下一轮直接重插。
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
func (s *PlayerNameStore) Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error) {
	if playerID == 0 || norm == "" {
		return 0, fmt.Errorf("%w (player_id=%d name_norm=%q)", ErrPlayerNameInvalidArgument, playerID, norm)
	}

	for attempt := 1; attempt <= playerNameReleaseAttempts; attempt++ {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM player_name WHERE player_id = ? AND name_norm = ? AND created_ms >= ?`,
			playerID, norm, minCreatedMs)
		if err != nil {
			if isRetryableMySQL(err) && attempt < playerNameReleaseAttempts {
				logx.Infof("[PlayerNameStore] player_id=%d release retry %d/%d after lock error: %v",
					playerID, attempt, playerNameReleaseAttempts, err)
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

		// 没删到。不限时间时"没删到"只能是行不存在;限时间时还要分清
		// "行不存在"与"行在、只是太老" —— 后者必须让调用方拿到拒绝,而不是误以为释放成功。
		if minCreatedMs == 0 {
			return ReleaseAbsent, nil
		}
		exists, err := s.hasRow(ctx, playerID, norm)
		if err != nil {
			return 0, err
		}
		if exists {
			return ReleaseOutsideWindow, nil
		}
		return ReleaseAbsent, nil
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

// hasRow 判断 (playerID, name_norm) 这一行在不在(不看 created_ms)。
// 只给 Release 区分"行不存在"与"行太老"用。
func (s *PlayerNameStore) hasRow(ctx context.Context, playerID uint64, norm string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM player_name WHERE player_id = ? AND name_norm = ?`, playerID, norm).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("player_name probe player_id=%d name_norm=%q: %w", playerID, norm, err)
	}
	return true, nil
}

// isDuplicateEntryMySQL 判断是不是 1062(撞主键或唯一键)。
// 在本 store 里它是正常控制流:判重就是靠"插进去撞不撞",不是故障。
func isDuplicateEntryMySQL(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDupEntry
}
