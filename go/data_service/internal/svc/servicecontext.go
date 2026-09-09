package svc

import (
	"context"
	"errors"
	"time"

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
	// InsertSnapshotIfGuidAbsent 是 player_snapshot_topic 消费者的落库入口(source=1,按 guid 去重)。
	InsertSnapshotIfGuidAbsent(context.Context, *store.SnapshotRow) (uint64, bool, error)
	GetSnapshotByID(context.Context, uint64) (*store.SnapshotRow, error)
	GetLatestSnapshotBefore(context.Context, uint64, uint64) (*store.SnapshotRow, error)
	ListSnapshotsMeta(context.Context, uint64, uint64, uint32) ([]*store.SnapshotMeta, error)
	GetSnapshotPlayerIDsByZone(context.Context, uint32, uint64) ([]uint64, error)
	InsertAuditLog(context.Context, *store.AuditLogRow) error
}

type TransactionLogStore interface {
	Close() error
	QueryLog(context.Context, *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error)
	// InsertBatchIgnore 是 transaction_log_topic 消费者的落库入口(tx_id 主键幂等)。
	InsertBatchIgnore(context.Context, []*store.TransactionLogRow) (int64, error)
}

// IdSegmentStore 是 AllocateIdSegment 的号段发号源(设计 §6.2)。
type IdSegmentStore interface {
	Close() error
	Allocate(ctx context.Context, bizTag string, step uint32) (lo, hi uint64, err error)
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
	IdSegmentStore   IdSegmentStore
	RollbackFence    RollbackFence
	LoginAdminClient loginpb.LoginAdminClient // nil when not configured
}

// autoMigrateTimeout 启动路径建表/补列的总时限。CreateOrUpdateTable 对存量大表的
// MODIFY COLUMN 会重建表,dev 库量小够用;生产不走这条路(Schema.AutoMigrate=false,
// 部署阶段跑 `data_service -migrate`,那条路不设时限)。
const autoMigrateTimeout = 5 * time.Minute

// MySQLConfigOf 把 yaml 的 SnapshotMySQL 段转成 store 的连接参数;main 的 -migrate 入口也用它。
func MySQLConfigOf(c config.Config) store.MySQLConfig {
	return store.MySQLConfig{
		Host:        c.SnapshotMySQL.Host,
		User:        c.SnapshotMySQL.User,
		Password:    c.SnapshotMySQL.Password,
		DBName:      c.SnapshotMySQL.DBName,
		MaxOpenConn: c.SnapshotMySQL.MaxOpenConn,
		MaxIdleConn: c.SnapshotMySQL.MaxIdleConn,
	}
}

func NewServiceContext(c config.Config) *ServiceContext {
	mysqlCfg := MySQLConfigOf(c)

	// 表结构只有 proto 一个真源,建表/补列集中在 store.MigrateSchema(见 store/schema.go)。
	// 迁移失败时三个 store 全部保持 nil(fail-closed):列缺失的表上跑业务 SQL 只会得到
	// 更难定位的运行期错误,不如让所有依赖它的 RPC 立刻返回 *DBError。
	// ShouldAutoMigrate 而不是 c.Schema.AutoMigrate:整段 `Schema:` 缺失时 go-zero
	// 不回填结构里的 default,安全默认(建表)只能由那个方法给(见 config.SchemaConfig)。
	schemaReady := true
	if c.ShouldAutoMigrate() {
		ctx, cancel := context.WithTimeout(context.Background(), autoMigrateTimeout)
		// 启动路径与 -migrate 传同一份 BootstrapTags:号段行在生产只能由迁移创建
		// (运行期补种关着),两条路径少传一处就会让 AllocateIdSegment 对那个 tag 直接拒绝。
		if err := store.MigrateSchema(ctx, mysqlCfg, store.MigrateOptions{BootstrapTags: c.IdSegment.EffectiveBootstrapTags()}); err != nil {
			logx.Errorf("[ServiceContext] schema auto-migrate failed; snapshot/txlog/idsegment stores disabled: %v", err)
			schemaReady = false
		}
		cancel()
	} else {
		logx.Infof("[ServiceContext] Schema.AutoMigrate=false: startup runs no DDL and seeds no id_segment rows; run `data_service -f <yaml> -migrate` during deploy (bootstraps %v)",
			c.IdSegment.EffectiveBootstrapTags())
	}

	var (
		snapshotStore       SnapshotStore
		transactionLogStore TransactionLogStore
		idSegmentStore      IdSegmentStore
	)
	if schemaReady {
		if ss, err := store.NewSnapshotStore(mysqlCfg); err != nil {
			logx.Errorf("[ServiceContext] snapshot store init failed (rollback disabled): %v", err)
		} else if ss != nil {
			snapshotStore = ss
		}

		if txLog, err := store.NewTransactionLogStore(mysqlCfg); err != nil {
			logx.Errorf("[ServiceContext] transaction log store init failed (recall/query disabled): %v", err)
		} else if txLog != nil {
			transactionLogStore = txLog
		}

		if seg, err := store.NewIdSegmentStore(mysqlCfg, store.IdSegmentOptions{AllowAutoSeed: c.IdSegment.AllowAutoSeed}); err != nil {
			logx.Errorf("[ServiceContext] id segment store init failed (AllocateIdSegment disabled): %v", err)
		} else if seg != nil {
			idSegmentStore = seg
		}
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
		IdSegmentStore:   idSegmentStore,
		LoginAdminClient: loginClient,
	}
}

// Close releases every store and the router. nil-safe on each member.
func (s *ServiceContext) Close() {
	if s == nil {
		return
	}
	if s.Router != nil {
		s.Router.Close()
	}
	if s.SnapshotStore != nil {
		_ = s.SnapshotStore.Close()
	}
	if s.TxLogStore != nil {
		_ = s.TxLogStore.Close()
	}
	if s.IdSegmentStore != nil {
		_ = s.IdSegmentStore.Close()
	}
}
