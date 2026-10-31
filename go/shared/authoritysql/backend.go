package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Backend 是权威SQL的部署档位(docs/design/player-data-tidb-scaling-plan.md §9):预算紧用 MySQL 主从,
// 预算够用 TiDB。两档的代码和表结构相同,差别只在连接时核对什么、固定哪些会话变量。
// 值就是配置里写的字符串,可以进启动日志与错误信息,不进指标 label。
type Backend string

const (
	// BackendTiDB 是默认档:配置不写、零值都按它处理,落在核对更严的一边。
	BackendTiDB Backend = "tidb"
	// BackendMySQL 只连 MySQL 主库:不发任何 tidb_* 语句,每条物理连接核对对端不是 TiDB、也不是只读实例。
	BackendMySQL Backend = "mysql"
)

// BackendEnv 是命令行工具选档位的环境变量:这些工具不读服务 yaml。常驻服务一律走自己 yaml 的 Backend 键。
const BackendEnv = "MMORPG_SQL_BACKEND"

// ErrBackendMismatch 表示连上的服务器不符合配置的档位。这是部署错误,不是身份问题,错误文本不含 DSN,
// 可以原样进启动日志;物理连接核验失败时只有这一类保留原因,运维才分得清「连到了从库」和「账号不对」。
var ErrBackendMismatch = errors.New("权威SQL服务器与后端档位不符")

// ParseBackend 解析配置值。空串按 tidb:go-zero 不下钻整段 optional 且缺席的嵌套结构,里面的 default 不回填,
// 这里必须接住「没写」。其余值一律报错,不折叠大小写、不去空白:写错的档位不能被猜成任何一档。
func ParseBackend(raw string) (Backend, error) {
	switch Backend(raw) {
	case "", BackendTiDB:
		return BackendTiDB, nil
	case BackendMySQL:
		return BackendMySQL, nil
	default:
		return "", fmt.Errorf("权威SQL后端档位非法(只接受 tidb 或 mysql): %q", raw)
	}
}

// LoadBackend 给不读服务 yaml 的命令行工具用,形状同 LoadSchemaPermission;必须先于消费一次性 writer 资格调用。
func LoadBackend(getenv func(string) string) (Backend, error) {
	if getenv == nil {
		return "", errors.New("缺少SQL后端档位装配")
	}
	return ParseBackend(getenv(BackendEnv))
}

// 版本串两个都看:TiDB 配了 server-version 时 VERSION() 里没有 TiDB 字样,@@version_comment 不受影响
// (2026-10-13 在 v8.5.2 上实测,见 go/friend/internal/sqldialect)。这里不调 tidb_version():MySQL 档不发 tidb_* 语句。
const (
	// mysqlServerSQL 是逐物理连接的核对(connector.pin):主从切换后同一地址可能已经是从库。
	mysqlServerSQL = "SELECT VERSION(),@@version_comment,@@read_only,@@super_read_only"
	// mysqlSessionSQL 在其上读回隔离级别,给业务池投入使用前的 AssertCurrentSession 用。
	mysqlSessionSQL = "SELECT VERSION(),@@version_comment,@@read_only,@@super_read_only,@@transaction_isolation"
)

// checkMySQLServer 是两条核对路径共用的纯判定。只读实例一律拒绝:read_only=1 时普通账号写不进去,
// 但持 SUPER / CONNECTION_ADMIN 的账号写得进去,写进从库就是静默分叉;读也不行,权威读要读到最新提交。
func checkMySQLServer(version, comment string, readOnly, superReadOnly uint64) error {
	if strings.Contains(strings.ToLower(version+" "+comment), "tidb") {
		return fmt.Errorf("%w: 配置是 mysql 档,连到的是 TiDB", ErrBackendMismatch)
	}
	if readOnly != 0 || superReadOnly != 0 {
		return fmt.Errorf("%w: mysql 档必须连主库(read_only=%d super_read_only=%d)", ErrBackendMismatch, readOnly, superReadOnly)
	}
	return nil
}

// verifyMySQLConnection 在一条物理连接上做 checkMySQLServer。服务器明确报错(比如 1193 没有这个变量)算档位不符;
// 连接类错误原样返回,由调用方按连接失败处理,不把网络抖动说成部署错误。
func verifyMySQLConnection(ctx context.Context, conn driver.Conn) error {
	values, err := rawOne(ctx, conn, mysqlServerSQL, nil, 4)
	if err != nil {
		var server *mysql.MySQLError
		if errors.As(err, &server) {
			return fmt.Errorf("%w: 读不到版本或只读开关(MariaDB 与 5.7.8 之前的 MySQL 没有 super_read_only)", ErrBackendMismatch)
		}
		return err
	}
	readOnly, readOnlyErr := unsignedValue(values[2])
	superReadOnly, superReadOnlyErr := unsignedValue(values[3])
	if readOnlyErr != nil || superReadOnlyErr != nil {
		return fmt.Errorf("%w: 只读开关读回值无法解析", ErrBackendMismatch)
	}
	return checkMySQLServer(stringValue(values[0]), stringValue(values[1]), readOnly, superReadOnly)
}

// assertMySQLSession 是 AssertCurrentSession 的 mysql 档:经业务池走一遍,同时证明池里的连接确实固定成了 RC。
func assertMySQLSession(ctx context.Context, db *sql.DB) error {
	var version, isolation string
	var comment sql.NullString // 正常不会是 NULL;真是 NULL 时只是少一个判据
	var readOnly, superReadOnly uint64
	if err := db.QueryRowContext(ctx, mysqlSessionSQL).Scan(&version, &comment, &readOnly, &superReadOnly, &isolation); err != nil {
		return fmt.Errorf("权威SQL会话读回失败: %w", err)
	}
	if err := checkMySQLServer(version, comment.String, readOnly, superReadOnly); err != nil {
		return err
	}
	if isolation != "READ-COMMITTED" {
		return errors.New("权威SQL隔离级别不是READ-COMMITTED")
	}
	return nil
}
