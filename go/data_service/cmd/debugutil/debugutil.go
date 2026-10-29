// Package debugutil contains shared helpers for debug_fetch / debug_import CLI tools.
package debugutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"shared/authoritysql"
	"strings"
	"sync"
	"unicode"

	"data_service/internal/config"
	"data_service/internal/routing"
	"data_service/internal/store"
	"data_service/internal/svc"

	_ "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/conf"
)

// DBConfigFile mirrors the DB yaml structure shared by debug_fetch and debug_import.
//
// RedisClient / Placement.Redis 与 go/db 同义:前者是核心实例(父快照、存盘屏障、owner_epoch 所在),
// 后者是落点记录与 player:zone 所在的映射实例,不写时复用核心实例(go/db config.PlacementConfig.Redis)。
type DBConfigFile struct {
	ZoneId       uint32 `json:"ZoneId"`
	ServerConfig struct {
		Database struct {
			Hosts       string `json:"Hosts"`
			User        string `json:"User"`
			Passwd      string `json:"Passwd"`
			DBName      string `json:"DBName,optional"`
			MaxOpenConn int    `json:"MaxOpenConn"`
			MaxIdleConn int    `json:"MaxIdleConn"`
			Net         string `json:"Net"`
		} `json:"Database"`
		RedisClient RedisEndpoint `json:"RedisClient,optional"`
	} `json:"ServerConfig"`
	Placement struct {
		Redis *RedisEndpoint `json:"Redis,optional"`
		// Required / RequireRouteReady 与 go/db config.PlacementConfig 同义,缺省 false。工具不按它们改选库,只用来提示
		// 「线上 go/db 会以 missing_required 拒绝 / 会校验映射就绪标记」。
		Required          bool `json:"Required,optional"`
		RequireRouteReady bool `json:"RequireRouteReady,optional"`
	} `json:"Placement,optional"`
}

// ZoneMySQL 返回 db.yaml 的 MySQL 连接目标,库名由调用方给出。
func (c *DBConfigFile) ZoneMySQL(dbName string) MySQLEndpoint {
	db := c.ServerConfig.Database
	return MySQLEndpoint{Hosts: db.Hosts, User: db.User, Password: db.Passwd, Net: db.Net, DBName: dbName}
}

// CoreRedis 返回核心实例。
func (c *DBConfigFile) CoreRedis() RedisEndpoint { return c.ServerConfig.RedisClient }

// MappingRedis 返回映射实例;Placement.Redis 缺省时与 go/db 一样复用核心实例。
func (c *DBConfigFile) MappingRedis() RedisEndpoint {
	if c.Placement.Redis != nil && strings.TrimSpace(c.Placement.Redis.Hosts) != "" {
		return *c.Placement.Redis
	}
	return c.ServerConfig.RedisClient
}

// LoadDataServiceConfig 读 data_service.yaml(取 SnapshotMySQL 即全局库等字段)。
func LoadDataServiceConfig(path string) (config.Config, error) {
	var c config.Config
	if err := MustLoadConfig(path, &c); err != nil {
		return c, fmt.Errorf("load data_service config: %w", err)
	}
	return c, nil
}

// GlobalMySQL 返回 data_service 的全局库(SnapshotMySQL)连接目标。
func GlobalMySQL(c config.Config) MySQLEndpoint {
	return MySQLEndpoint{RequireTiDB: c.SnapshotMySQL.RequireTiDB, Hosts: c.SnapshotMySQL.Host, User: c.SnapshotMySQL.User,
		Password: c.SnapshotMySQL.Password, Net: "tcp", DBName: c.SnapshotMySQL.DBName}
}

// MustLoadConfig loads a YAML config file via go-zero, recovering panics.
func MustLoadConfig(path string, target any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	conf.MustLoad(path, target)
	return nil
}

// LoadDBConfig reads and parses a db.yaml style config.
func LoadDBConfig(path string) (*DBConfigFile, error) {
	var cfg DBConfigFile
	if err := MustLoadConfig(path, &cfg); err != nil {
		return nil, fmt.Errorf("load db config: %w", err)
	}
	return &cfg, nil
}

// OpenZoneDB opens a MySQL connection for a specific zone.
func OpenZoneDB(ctx context.Context, dbConfigPath string, zoneID uint32) (*sql.DB, uint32, string, error) {
	return OpenZoneDBNamed(ctx, dbConfigPath, zoneID, "")
}

// OpenZoneDBNamed 同 OpenZoneDB;dbOverride 非空时直接连这个库(同一实例上的全局库、其他服务的独占库)。
// 库名优先级:dbOverride > db.yaml 的 DBName > zone_{zone}_db。
func OpenZoneDBNamed(ctx context.Context, dbConfigPath string, zoneID uint32, dbOverride string) (*sql.DB, uint32, string, error) {
	return OpenZoneDBNamedWithPermission(ctx, dbConfigPath, zoneID, dbOverride, authoritysql.Reader())
}

func OpenZoneDBNamedWithPermission(ctx context.Context, dbConfigPath string, zoneID uint32, dbOverride string, permission authoritysql.Permission) (*sql.DB, uint32, string, error) {
	cfg, err := LoadDBConfig(dbConfigPath)
	if err != nil {
		return nil, 0, "", err
	}

	if zoneID == 0 {
		zoneID = cfg.ZoneId
	}
	dbName := strings.TrimSpace(dbOverride)
	if dbName == "" {
		dbName = strings.TrimSpace(cfg.ServerConfig.Database.DBName)
	}
	if dbName == "" && zoneID == 0 {
		return nil, 0, "", errors.New("-zone is required when db config does not include ZoneId")
	}
	if dbName == "" {
		dbName = fmt.Sprintf("zone_%d_db", zoneID)
	}

	network := strings.TrimSpace(cfg.ServerConfig.Database.Net)
	if network == "" {
		network = "tcp"
	}

	dsn := fmt.Sprintf("%s:%s@%s(%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		cfg.ServerConfig.Database.User,
		cfg.ServerConfig.Database.Passwd,
		network,
		cfg.ServerConfig.Database.Hosts,
		dbName,
	)

	db, err := authoritysql.Open(ctx, dsn, dbName, permission.Kind() == "writer", 1, permission)
	if err != nil {
		return nil, 0, "", fmt.Errorf("open mysql connection: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, 0, "", errors.Join(fmt.Errorf("ping mysql: %w", err), db.Close())
	}

	return db, zoneID, dbName, nil
}

// QuoteIdentifier validates and quotes a SQL identifier.
func QuoteIdentifier(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("sql identifier must not be empty")
	}
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return "", fmt.Errorf("unsafe sql identifier %q", name)
		}
	}
	return "`" + name + "`", nil
}

// NewServiceContext builds a lightweight ServiceContext with Router (no snapshot store).
//
// RouteStore.Mode=db_first 时同时装上映射提交点的库(debug_import 的 RegisterPlayerZone 与 data_service 走同一条
// db_first 路径,docs/design/player-route-mapping-durability-proposal.md §4.3 W1「含 debug_import」),cleanup 时关闭。
// 装不起来就返回 error,不退化成 redis 模式:工具与服务对同一份 yaml 必须是同一个提交点。
func NewServiceContext(configPath string) (*svc.ServiceContext, func(), error) {
	return NewServiceContextWithPermission(configPath, authoritysql.Reader())
}

func NewServiceContextWithPermission(configPath string, permission authoritysql.Permission) (*svc.ServiceContext, func(), error) {
	svcCtx, closeReport, err := NewServiceContextWithPermissionAndReport(configPath, permission)
	if err != nil {
		return nil, nil, err
	}
	return svcCtx, func() {
		if err := closeReport(); err != nil {
			fmt.Fprintf(os.Stderr, "debugutil资源关闭失败: %v\n", err)
		}
	}, nil
}

// 写工具使用可返回实际关闭错误的接缝；只读旧调用面保留，不能靠 void cleanup 签成功前沿。
func NewServiceContextWithPermissionAndReport(configPath string, permission authoritysql.Permission) (result *svc.ServiceContext, closeReport func() error, resultErr error) {
	var routeStore *store.RouteStore
	var ownedRouter *routing.Router
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			closeReport = nil
			resultErr = fmt.Errorf("debugutil Router装配失败: %v", recovered)
			if ownedRouter != nil {
				resultErr = errors.Join(resultErr, ownedRouter.CloseAndReport())
			}
			if routeStore != nil {
				resultErr = errors.Join(resultErr, routeStore.Close())
			}
		}
	}()
	var c config.Config
	if err := MustLoadConfig(configPath, &c); err != nil {
		return nil, nil, fmt.Errorf("load data_service config: %w", err)
	}
	// routing.NewRouter 对非法 RouteStore 段会 panic;工具这里先校验,给出可读的错误而不是 panic 栈。
	c.RouteStore = c.RouteStore.Normalize()
	if err := c.RouteStore.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid RouteStore config: %w", err)
	}

	// 在打开 RouteStore 前完成 Router 会拒绝的纯配置检查，避免后续 panic 泄漏已建 SQL 池。
	if c.DevRedis.PoolSize < 0 {
		return nil, nil, errors.New("DevRedis.PoolSize 必须非负")
	}
	if c.DevRedis.Host == "" {
		if err := config.ValidateRegions(c.Regions); err != nil {
			return nil, nil, err
		}
	}
	var routerOpts []routing.RouterOption
	if c.RouteStore.DBFirst() {
		routeCfg := svc.MySQLConfigOf(c)
		routeCfg.Permission = permission
		routeCfg.MaxOpenConn = c.RouteStore.MaxOpenConn
		routeCfg.MaxIdleConn = c.RouteStore.MaxIdleConn
		host, _ := os.Hostname() // 取不到时 NewRouteWriterID 用 "unknown"
		rs, err := store.NewRouteStore(routeCfg, store.RouteStoreOptions{
			FenceShards: uint32(c.RouteStore.FenceShards),
			Writer:      store.NewRouteWriterID("data_service_debug", host),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("RouteStore.Mode=db_first but the route store cannot be opened (%s): %w",
				store.OpenRouteStoreHint(configPath, err), err)
		}
		routeStore = rs
		// 同一个实例同时作写入点与回源读者(与服务的 svc.routeRouterOptions 一致):db_first 下 R7 GetAllPlayerIDsInZone
		// 一律读库,没有回源读者就直接 ErrRouteReadUnavailable(debug_fetch 的按 zone 枚举会报错);
		// ReadRequiresReady=true 时 R2 / R3 / R6 的回源同理。
		routerOpts = append(routerOpts, routing.WithRouteStore(rs), routing.WithRouteReader(rs))
	}

	ownedRouter = routing.NewRouter(c, routerOpts...)
	svcCtx := &svc.ServiceContext{
		Config: c,
		Router: ownedRouter,
	}
	if routeStore != nil {
		// 只在非 nil 时赋给接口字段,免得包出「非 nil 接口包着 nil 指针」。
		svcCtx.PlayerRouteStore = routeStore
	}

	var closeOnce sync.Once
	var closeErr error
	cleanup := func() error {
		closeOnce.Do(func() {
			if svcCtx.Router != nil {
				closeErr = errors.Join(closeErr, svcCtx.Router.CloseAndReport())
			}
			if svcCtx.PlayerRouteStore != nil {
				closeErr = errors.Join(closeErr, svcCtx.PlayerRouteStore.Close())
			}
			// mappingRedis 命令在调用栈内同步完成；共享 idle 池不发行资产，不在此关闭全局他人连接。
			// 调用者仍须等实际写结果；网络 UNKNOWN 与上面的 owned 资源关闭错误不能排除。
		})
		return closeErr
	}

	return svcCtx, cleanup, nil
}

// NewServiceContextWithSnapshot builds a ServiceContext that also initialises SnapshotStore.
func NewServiceContextWithSnapshot(configPath string) (*svc.ServiceContext, func(), error) {
	svcCtx, baseCleanup, err := NewServiceContext(configPath)
	if err != nil {
		return nil, nil, err
	}

	c := svcCtx.Config
	if strings.TrimSpace(c.SnapshotMySQL.Host) != "" {
		ss, err := store.NewSnapshotStore(store.MySQLConfig{
			Permission:  authoritysql.Reader(),
			RequireTiDB: c.SnapshotMySQL.RequireTiDB,
			Host:        c.SnapshotMySQL.Host,
			User:        c.SnapshotMySQL.User,
			Password:    c.SnapshotMySQL.Password,
			DBName:      c.SnapshotMySQL.DBName,
			MaxOpenConn: c.SnapshotMySQL.MaxOpenConn,
			MaxIdleConn: c.SnapshotMySQL.MaxIdleConn,
		})
		if err != nil {
			baseCleanup()
			return nil, nil, err
		}
		svcCtx.SnapshotStore = ss
	}

	cleanup := func() {
		if svcCtx.SnapshotStore != nil {
			svcCtx.SnapshotStore.Close()
		}
		baseCleanup()
	}

	return svcCtx, cleanup, nil
}
