package svc

import (
	"context"
	"errors"

	"data_service/internal/config"
	"data_service/internal/routing"
	"data_service/internal/store"

	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
)

// SnapshotStore 描述 logic 层实际依赖的快照/审计能力。使用接口既让启动失败时
// 可以明确保持 nil，也允许单测注入失败实现验证 fail-closed 行为。
type SnapshotStore interface {
	Close() error
	InsertSnapshot(context.Context, *store.SnapshotRow) (uint64, error)
	GetSnapshotByID(context.Context, uint64) (*store.SnapshotRow, error)
	GetLatestSnapshotBefore(context.Context, uint64, uint64) (*store.SnapshotRow, error)
	ListSnapshotsMeta(context.Context, uint64, uint64, uint32) ([]*store.SnapshotMeta, error)
	GetSnapshotPlayerIDsByZone(context.Context, uint32, uint64) ([]uint64, error)
	InsertAuditLog(context.Context, *store.AuditLogRow) error
}

type TransactionLogStore interface {
	Close() error
	QueryLog(context.Context, *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error)
}

// ErrRollbackTargetOnline 由 RollbackFence 实现在目标仍在线、正在登录，或旧
// epoch 写入尚未排空时返回。logic 层据此向调用方返回 ErrCodePlayerOnline。
var ErrRollbackTargetOnline = errors.New("rollback target is online or has active writes")

// RollbackFence 是应用级回档执行前必须持有的跨服务离线 epoch。
// Acquire* 成功后，直到 release 被调用，登录/Scene 激活和任何旧 epoch 存盘都
// 必须被阻断。一次普通的“当前是否在线”查询不满足这个接口（存在 TOCTOU）。
// 当前仓库尚无跨 login/player_locator/Scene/db 的实现，因此生产 ServiceContext
// 故意保持 nil，所有回档写 RPC fail-closed；测试可注入 fake 验证其余逻辑。
type RollbackFence interface {
	AcquirePlayer(context.Context, uint64) (release func(), err error)
	AcquireZone(context.Context, uint32) (release func(), err error)
	AcquireServer(context.Context, []uint32) (release func(), err error)
}

type ServiceContext struct {
	Config           config.Config
	Router           *routing.Router
	SnapshotStore    SnapshotStore
	TxLogStore       TransactionLogStore
	RollbackFence    RollbackFence
	LoginAdminClient loginpb.LoginAdminClient // nil when not configured
}

func NewServiceContext(c config.Config) *ServiceContext {
	ss, err := store.NewSnapshotStore(store.MySQLConfig{
		Host:        c.SnapshotMySQL.Host,
		User:        c.SnapshotMySQL.User,
		Password:    c.SnapshotMySQL.Password,
		DBName:      c.SnapshotMySQL.DBName,
		MaxOpenConn: c.SnapshotMySQL.MaxOpenConn,
		MaxIdleConn: c.SnapshotMySQL.MaxIdleConn,
	})
	if err != nil {
		logx.Errorf("[ServiceContext] snapshot store init failed (rollback disabled): %v", err)
	}
	var snapshotStore SnapshotStore
	if err == nil && ss != nil {
		snapshotStore = ss
	}

	txLog, err := store.NewTransactionLogStore(store.MySQLConfig{
		Host:        c.SnapshotMySQL.Host,
		User:        c.SnapshotMySQL.User,
		Password:    c.SnapshotMySQL.Password,
		DBName:      c.SnapshotMySQL.DBName,
		MaxOpenConn: c.SnapshotMySQL.MaxOpenConn,
		MaxIdleConn: c.SnapshotMySQL.MaxIdleConn,
	})
	if err != nil {
		logx.Errorf("[ServiceContext] transaction log store init failed (recall/query disabled): %v", err)
	}
	var transactionLogStore TransactionLogStore
	if err == nil && txLog != nil {
		transactionLogStore = txLog
	}

	// Optional: login admin gRPC client for orphan account cleanup during rollback
	var loginClient loginpb.LoginAdminClient
	if c.LoginAdminRpc.Etcd.Key != "" {
		conn := zrpc.MustNewClient(c.LoginAdminRpc)
		loginClient = loginpb.NewLoginAdminClient(conn.Conn())
		logx.Info("[ServiceContext] login admin RPC client connected")
	}

	return &ServiceContext{
		Config:           c,
		Router:           routing.NewRouter(c),
		SnapshotStore:    snapshotStore,
		TxLogStore:       transactionLogStore,
		LoginAdminClient: loginClient,
	}
}
