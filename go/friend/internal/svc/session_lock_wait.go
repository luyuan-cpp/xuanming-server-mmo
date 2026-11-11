package svc

// 好友库会话的锁等待上限与启动读回探针(客户端请求按会话保序,docs/design/client-request-ordering.md §6.1)。
//
// 为什么要有上限:请求截止时 go-sql-driver 只断开连接、不发 KILL QUERY。服务端那条正在等锁的语句不知道对端已经走了,
// 会一直等到拿到锁或等锁超时;它所在事务已经拿到的行锁也一直占着,把同一行上后到的请求一起拖住。
// 业界标准做法是让库端按会话变量把等锁的语句以 1205 中止(MySQL 与 TiDB 悲观事务都认 innodb_lock_wait_timeout),
// 不靠应用侧补发 KILL。
//
// 截止之后不提交另有两道闸,不靠这里:写者池(shared/authoritysql)把事务外的写也包进事务、ctx 结束后不发 COMMIT,
// database/sql 的 Tx.Commit 同样先看 ctx。这里管的是「被放弃的语句还能占锁多久」以及「截止之前单条语句最多等多久」。
//
// 取值依据:friend 的整请求预算是 Timeout(4000)− InBandReplyReserve(500)= 3500 ms;单条语句等任何锁最多 1 s,
// 遇 1205 时原样上抛、in-band 回 ErrStorage(事务已回滚,没有结局未知),仍留出 2.5 s 给其余语句、提交与回包。
// 与 guild、data_service 路由库、player_inbox 同值;
// Java 版 friend 同样逐连接设(JDBC sessionVariables 的 innodb_lock_wait_timeout=3,game-friend application.yaml),
// 取值不同,但同样小于请求预算并留出提交余量;它没有设 lock_wait_timeout(元数据锁),登记为 Java 待做。

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"shared/authoritysql"
)

const (
	// sessionInnodbLockWaitSeconds 行锁等待上限(innodb_lock_wait_timeout;MySQL 缺省 50 s,TiDB 悲观事务同名同义)。
	//
	// 它**不写进 DSN**:authoritysql 的连接器在每条物理连接建立时、以及每次借出连接时都执行
	// SET SESSION innodb_lock_wait_timeout=1(两档都发,见 go/shared/authoritysql/pool.go 的 pin),
	// DSN 里的值会在建连后立刻被它覆盖,写了只是多一个看起来生效、实际不生效的第二来源。
	// 这里只供启动读回核对:那边改了值,本服务拒启,逼着按上面的预算重新核一遍。
	sessionInnodbLockWaitSeconds = 1

	// mysqlMetadataLockWaitSeconds 元数据锁等待上限(lock_wait_timeout;MySQL 缺省 31536000 s,即一年),只在 mysql 档经 DSN 设置
	// (authoritysql 只剥掉 DSN 里的 tidb_* / transaction_isolation / sql_mode,这一项由驱动在建连时 SET,之后不被覆盖)。
	//
	// MySQL 上一条 DDL 排队取排他元数据锁时,同表后到的 DML 全部排在它后面;没有上限时,这些 DML 在客户端早已放弃之后
	// 仍会执行。取与行锁同一个值:任何一条语句等任何锁最多 1 s,超时同样回 1205。
	// 迁移会话由 schemamigrate 自己 SET SESSION lock_wait_timeout(缺省 5 s),不受这里影响。
	// TiDB 档不发:TiDB 的元数据锁是 DDL 等旧事务、不让 DML 排在 DDL 后面,lock_wait_timeout 在 TiDB 上不约束 DML【未核实,依据 TiDB 元数据锁文档】。
	mysqlMetadataLockWaitSeconds = 1
)

const (
	probeLockWaitMySQLSQL = "SELECT @@SESSION.innodb_lock_wait_timeout, @@SESSION.lock_wait_timeout"
	probeLockWaitTiDBSQL  = "SELECT @@SESSION.innodb_lock_wait_timeout"
)

// sessionLockWaitDSNParams 是 BuildDSN 要追加的锁等待会话变量:只有 mysql 档的元数据锁上限,TiDB 档为空串。
func sessionLockWaitDSNParams(backend authoritysql.Backend) string {
	if backend != authoritysql.BackendMySQL {
		return ""
	}
	return "&lock_wait_timeout=" + strconv.Itoa(mysqlMetadataLockWaitSeconds)
}

// verifySessionLockWaits 在建池之后读回一条池连接上的锁等待会话变量,与上面的常量不符就返回错误(调用方拒启)。
// 每条物理连接走同一段初始化(DSN 参数 + authoritysql 的 pin),核一条即可,与 data_service route store 的探针同口径。
// 只读语句,写者池启动期冻结时也放行,不消费写资格。错误不含 DSN。
func verifySessionLockWaits(ctx context.Context, db *sql.DB, backend authoritysql.Backend) error {
	var innodbWait, metadataWait int64
	var err error
	if backend == authoritysql.BackendMySQL {
		err = db.QueryRowContext(ctx, probeLockWaitMySQLSQL).Scan(&innodbWait, &metadataWait)
	} else {
		err = db.QueryRowContext(ctx, probeLockWaitTiDBSQL).Scan(&innodbWait)
	}
	if err != nil {
		return fmt.Errorf("read session lock wait timeouts: %w", err)
	}
	if innodbWait != sessionInnodbLockWaitSeconds {
		return fmt.Errorf("session innodb_lock_wait_timeout=%d, want %d (pinned on every connection by shared/authoritysql; "+
			"re-check the friend request budget before changing it)", innodbWait, sessionInnodbLockWaitSeconds)
	}
	if backend == authoritysql.BackendMySQL && metadataWait != mysqlMetadataLockWaitSeconds {
		return fmt.Errorf("session lock_wait_timeout=%d, want %d (the lock_wait_timeout parameter of svc.BuildDSN did not take effect)",
			metadataWait, mysqlMetadataLockWaitSeconds)
	}
	return nil
}
