package debugutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"shared/authoritysql"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// ── 连接参数与覆盖 ─────────────────────────────────────────────────────
//
// 线上排查的固定做法是 port-forward 到工作站,再用仓库外的一份 yaml 副本连过去。yaml 里的地址是集群内
// 地址、口令也不该明文落盘,所以每类连接都允许用命令行覆盖地址、用环境变量覆盖口令(只读环境变量名,
// 口令本身不上命令行、不进输出)。

// MySQLEndpoint 是一个 MySQL / TiDB 连接目标。
type MySQLEndpoint struct {
	RequireTiDB bool
	Hosts       string
	User        string
	Password    string
	Net         string
	DBName      string
}

// RedisEndpoint 是一个 Redis 连接目标(db.yaml 的 RedisClient / Placement.Redis 形状)。
type RedisEndpoint struct {
	Hosts    string `json:"Hosts,optional"`
	Password string `json:"Password,optional"`
	DB       int    `json:"DB,optional"`
}

// Describe 返回不含口令的 host:port/db,写进导出包的 sources。
func (e RedisEndpoint) Describe() string { return fmt.Sprintf("%s/%d", e.Hosts, e.DB) }

// Describe 返回不含口令的 host/库名。
func (e MySQLEndpoint) Describe() string { return e.Hosts + "/" + e.DBName }

// PasswordFromEnv 读环境变量里的口令;envName 为空时返回 (fallback, nil)。变量名给了却没设置是操作失误,报错而不是退回 yaml。
func PasswordFromEnv(envName, fallback string) (string, error) {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return fallback, nil
	}
	value, ok := os.LookupEnv(envName)
	if !ok {
		return "", fmt.Errorf("环境变量 %s 未设置", envName)
	}
	return value, nil
}

// OpenMySQL 打开一个连接池并 ping。库名写进 DSN(不靠 USE 切库);maxOpen 至少为 1。
//
// interpolateParams 保持默认关闭:参数走预处理语句的二进制协议,BIGINT UNSIGNED 读成 uint64,不经十进制字符串往返。
func OpenMySQL(ctx context.Context, ep MySQLEndpoint, maxOpen int) (*sql.DB, error) {
	return OpenMySQLWithPermission(ctx, ep, maxOpen, authoritysql.Reader())
}

// OpenMySQLWithPermission 保留唯一池装配；写工具必须传入已消费一次的 boot 能力，
// 读工具沿 OpenMySQL，不能因为共享 helper 而自动取得写权限。
func OpenMySQLWithPermission(ctx context.Context, ep MySQLEndpoint, maxOpen int, permission authoritysql.Permission) (*sql.DB, error) {
	cfg, err := mysqlConfig(ep)
	if err != nil {
		return nil, err
	}
	if maxOpen < 1 {
		maxOpen = 1
	}
	db, err := authoritysql.Open(ctx, cfg.FormatDSN(), ep.DBName, ep.RequireTiDB, maxOpen, permission)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("ping mysql %s: %w", ep.Describe(), err), db.Close())
	}
	return db, nil
}

// mysqlConfig 构造驱动配置。字符集必须走 mysql.Charset 选项:直接塞进 Params 的键会在每条连接上被原样拼成
// `SET charset = utf8mb4` 执行(v1.8 起只有 ParseDSN 会把 charset 转成 SET NAMES),服务端报错、建连全部失败。
func mysqlConfig(ep MySQLEndpoint) (*mysql.Config, error) {
	cfg := mysql.NewConfig()
	cfg.User = ep.User
	cfg.Passwd = ep.Password
	cfg.Net = strings.TrimSpace(ep.Net)
	if cfg.Net == "" {
		cfg.Net = "tcp"
	}
	cfg.Addr = ep.Hosts
	cfg.DBName = ep.DBName
	cfg.ParseTime = true
	if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return nil, fmt.Errorf("set mysql charset: %w", err)
	}
	return cfg, nil
}

const (
	// tidbVersionQuery 与 shared/authoritysql 同一判据:tidb_version() 只有 TiDB 有,且不受 tidb.toml
	// server-version 改写 VERSION() 的影响。
	tidbVersionQuery = "SELECT tidb_version()"
	serverUUIDQuery  = "SELECT @@server_uuid"
)

// MySQLServerIdentity 返回 MySQL 实例身份(@@server_uuid,存在数据目录的 auto.cnf 里,重启不变)。
// 取不到时返回空串;调用方必须按「身份未知」处理(debug_import 对未知身份 fail-closed),不得当成「不是同一个实例」。
//
// TiDB 一律返回空串(2026-10-22,r2-ds#1):TiDB 是多个 tidb-server 挂在负载均衡后面的集群,即便某个变量能取到值,
// 也只能标识单个 tidb-server 进程;导出与导入经同一个 port-forward 打到不同 Pod 时会得到不同的值,把同一个集群
// 误判成两个实例,闸门因此放行。TiDB 上能唯一标识整个集群的变量 / 系统表本仓库没有核实过,所以不猜:
// TiDB 写目标改由本地调试环境标记(HasLocalTargetMarker)证明自己是本地库。
//
// 只有 tidb_version() 明确报「函数不存在」(MySQL 1305)才算确认不是 TiDB;超时、断连等其它失败分不清是不是 TiDB,
// 也按身份未知返回,不去取 TiDB 上可能答得出来的单进程值。
func MySQLServerIdentity(ctx context.Context, q Queryer) string {
	var tidbVersion sql.NullString
	if !isFunctionMissing(queryOne(ctx, q, tidbVersionQuery, &tidbVersion)) {
		return ""
	}
	var id sql.NullString
	if queryOne(ctx, q, serverUUIDQuery, &id) != nil {
		return ""
	}
	return strings.TrimSpace(id.String)
}

// mysqlErrFunctionMissing 是 MySQL 的 ER_SP_DOES_NOT_EXIST(「FUNCTION <库>.tidb_version does not exist」)。
const mysqlErrFunctionMissing = 1305

// isFunctionMissing 判断 tidb_version() 探测的结果是不是「服务端明确没有这个函数」;成功(是 TiDB)与其它失败都返回 false。
func isFunctionMissing(err error) bool {
	var serverErr *mysql.MySQLError
	return errors.As(err, &serverErr) && serverErr.Number == mysqlErrFunctionMissing
}

// 本地调试环境标记:一张只在本地隔离环境里手工建的表,debug_import 在取不到实例身份、无法与导出来源比对时,
// 凭它确认写目标是本地库。标记建在导入要写的每个库里(落点库 zone_<N>_db 与全局库),因为导入只连这些库、
// 只有这些库的权限。
//
// 不放进 deploy/mysql-init:K8s 的 mysql-init-sql ConfigMap 原样带入那份脚本,标记会跟着出现在集群 MySQL 上,
// 也就证明不了「这是本地」。建标记必须是操作者对本地库的一次有意操作(语句见 LocalTargetMarkerSetupSQL)。
const (
	LocalTargetMarkerTable = "debug_import_local_target"
	LocalTargetMarkerValue = "local-debug-import-target"

	// LocalTargetMarkerSetupSQL 写进拒绝信息与运维手册;只在确认是本地隔离环境的库上执行。
	LocalTargetMarkerSetupSQL = "CREATE TABLE IF NOT EXISTS " + LocalTargetMarkerTable + " (marker VARCHAR(64) NOT NULL PRIMARY KEY); " +
		"INSERT IGNORE INTO " + LocalTargetMarkerTable + " (marker) VALUES ('" + LocalTargetMarkerValue + "');"

	localTargetMarkerQuery = "SELECT COUNT(*) FROM " + LocalTargetMarkerTable + " WHERE marker = '" + LocalTargetMarkerValue + "'"
)

// HasLocalTargetMarker 判断当前连接的库里有没有本地调试环境标记。查询失败(表不存在、无权限、连接错误)一律按
// 「没有」处理:标记只用来放行,读不到就不能放行。
func HasLocalTargetMarker(ctx context.Context, q Queryer) bool {
	var count int64
	if queryOne(ctx, q, localTargetMarkerQuery, &count) != nil {
		return false
	}
	return count > 0
}

// Redis 的本地调试环境标记(2026-10-22,r2-ds#1 评审补充):Redis 的 run_id 证明不了写目标不是线上实例
// (见 RedisServerIdentity),Redis 写目标与 MySQL 一样要靠一个只在本地手工建的标记。标记是一把键,值固定为
// LocalTargetMarkerValue;建在导入要写的每个 Redis 实例上,并且是客户端配置里选的那个 DB 号(SET 只写当前 DB)。
// 不设 TTL;本地 FLUSHALL / FLUSHDB(AGENTS §6.2 压测与复现前清库)会把它一起清掉,之后要重建。
// 与 MySQL 标记同理,不放进任何部署脚本或初始化清单。
const (
	LocalRedisMarkerKey = "debug_import:local_target"

	// LocalRedisMarkerSetupCommand 写进拒绝信息与运维手册;只在确认是本地隔离环境的 Redis 上执行。
	LocalRedisMarkerSetupCommand = "SET " + LocalRedisMarkerKey + " " + LocalTargetMarkerValue
)

// HasLocalRedisMarker 判断当前连接的 Redis(客户端选中的 DB)里有没有本地调试环境标记,值必须恰好等于
// LocalTargetMarkerValue。任何错误(键不存在、类型不对、连接失败、Cluster 重定向)一律按「没有」处理:
// 标记只用来放行,读不到就不能放行。
func HasLocalRedisMarker(ctx context.Context, client redis.Cmdable) bool {
	value, err := client.Get(ctx, LocalRedisMarkerKey).Result()
	return err == nil && value == LocalTargetMarkerValue
}

// queryOne 执行一条只返回一行一列的查询并扫进 dest;没有行也算失败。
func queryOne(ctx context.Context, q Queryer, query string, dest any) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dest); err != nil {
		return err
	}
	return rows.Close()
}

// RedisServerIdentity 返回 INFO server 里的 run_id(进程级唯一);取不到时返回空串。
//
// run_id 只能正面认出「同一个进程」,证明不了「不是同一个部署」(2026-10-22,r2-ds#1 评审补充):线上 Redis 是
// HA 形态(svc/redis 经 haproxy 指向当前主,Pod 滚动时 prestop 主动 failover),单实例重启也会换 run_id;导出之后
// 切一次主、重启一次,或导出时直连了从库 Pod,导入时线上 Redis 的 run_id 就和包里不同。所以 debug_import 只拿它
// 拒绝「相同」,Redis 写目标是不是本地另由 HasLocalRedisMarker 证明。
func RedisServerIdentity(ctx context.Context, client redis.Cmdable) string {
	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return value
		}
	}
	return ""
}

// OpenRedis 打开一个单实例客户端并 PING。导出与导入都只连单实例:父键、owner 与屏障在同一实例
// (login parent_commit_barrier 的前提),指向 Redis Cluster 时 MULTI 跨槽会直接报错。
func OpenRedis(ctx context.Context, ep RedisEndpoint) (*redis.Client, error) {
	if strings.TrimSpace(ep.Hosts) == "" {
		return nil, errors.New("redis 地址为空")
	}
	if strings.Contains(ep.Hosts, ",") {
		return nil, fmt.Errorf("redis 地址 %q 含多个节点;本工具只连单实例", ep.Hosts)
	}
	client := redis.NewClient(&redis.Options{Addr: ep.Hosts, Password: ep.Password, DB: ep.DB})
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("ping redis %s: %w", ep.Describe(), err), client.Close())
	}
	return client, nil
}

// ── 写目标守卫 ─────────────────────────────────────────────────────────

// CheckLocalWriteTarget 是导入工具的写前闸门:目标主机必须是本机回环地址,或在 allowHosts(逗号分隔,
// 精确匹配主机名)里显式放行。线上数据只许进隔离环境,把生产 yaml 误当本地 yaml 是最可能的事故。
//
// 它挡不住「127.0.0.1 上是 port-forward 出来的生产实例」:导入端还要比对实例身份(导出包 sources 里记的
// Redis run_id / MySQL server_uuid),目标就是导出来源时拒绝;MySQL 任一侧身份取不到(TiDB、旧版导出包)时,
// 以及所有 Redis 目标(run_id 不同证明不了什么),都要带本地调试环境标记或显式放行,见 debug_import 的
// target_identity.go。
func CheckLocalWriteTarget(what, hostPort, allowHosts string) error {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return fmt.Errorf("%s 目标地址为空", what)
	}
	if IsLoopbackHost(host) {
		return nil
	}
	for _, allowed := range strings.Split(allowHosts, ",") {
		if allowed = strings.TrimSpace(allowed); allowed != "" && strings.EqualFold(allowed, host) {
			return nil
		}
	}
	return fmt.Errorf("%s 目标 %q 不是本机回环地址;导入只写隔离环境。确是本地 docker 服务名时用 -allow-target-host=%s 显式放行", what, hostPort, host)
}

// IsLoopbackHost 只认 localhost 与回环 IP;不做 DNS 解析(解析结果可被篡改,也会把线上域名误判成本机)。
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
