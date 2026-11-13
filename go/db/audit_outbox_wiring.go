package main

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"db/internal/auditoutbox"
	"db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	"db/internal/playerstore"

	"proto/common/base"
	"shared/drainproof"
	"shared/placement"
	"shared/releasetrack"
	"shared/workshard"
)

// auditRelayStopTimeout 是停机时等转发循环退出的上限。在途的一批最多投 15 秒(auditoutbox 的 produceTimeout);
// 等不到就不等了:投出去还没删的行留在发件箱,下一个进程重投,消费端去重。
const auditRelayStopTimeout = 20 * time.Second

// auditOutboxRelay 是装配好的审计发件箱转发循环(docs/design/player-audit-outbox.md §7)。没起时 Stop 是空操作。
type auditOutboxRelay struct {
	stop func()
}

// Stop 停下转发循环并关掉 Kafka 写端。停机路径调用一次。
func (a *auditOutboxRelay) Stop() {
	if a != nil && a.stop != nil {
		a.stop()
	}
}

// startAuditOutboxRelay 按配置装配并启动转发循环。relay 是维护排空的 RELAY 会话(internal/drain 关它、等它);
// selfUUID 是本进程在 etcd 注册拿到的 node_uuid,分工表里的身份(为空时,成员表里只要有别的副本,本副本一片都不做)。
//
// 不起的情形都打日志,db_audit_outbox_relay_enabled 报 0:canary 副本(不跑全局后台,release-track-gray-canary.md §2.5)、
// 没配 AuditOutbox.Brokers。发件箱照常随存盘写入,只积不投、不丢,积压年龄告警会响(deploy/k8s/audit-outbox-alerts.yaml)。
// etcd 为 nil 时成员表一直为空,本副本做全部片;多副本之间仍由每片的命名锁互斥。
func startAuditOutboxRelay(c *config.Config, stores *proto_sql.StoreRegistry, relay *drainproof.Session, etcd *clientv3.Client, selfUUID string) *auditOutboxRelay {
	metrics.PrimeAuditOutbox(playerstore.AuditSkipReasons)
	metrics.SetAuditOutboxRelayEnabled(false)
	track, err := releasetrack.FromEnv()
	if err != nil {
		logx.Errorf("[audit_outbox] Invalid release track; the audit outbox relay is not started: %v", err)
		return &auditOutboxRelay{}
	}
	if !track.RunsGlobalBackgroundWork() {
		logx.Infof("[audit_outbox] release_track=%s: audit outbox relay disabled (a canary serves player store RPCs only)", track)
		return &auditOutboxRelay{}
	}
	cfg := c.AuditOutbox.Normalized()
	if len(cfg.Brokers) == 0 {
		logx.Errorf("[audit_outbox] AuditOutbox.Brokers is empty: committed audit records stay in %s and are not forwarded until Kafka is configured (db_audit_outbox_relay_enabled=0)",
			playerstore.AuditOutboxTable)
		return &auditOutboxRelay{}
	}
	producer, err := auditoutbox.NewKafkaProducer(cfg.Brokers, cfg.EffectiveTransactionLogTopic(), cfg.EffectiveSnapshotTopic())
	if err != nil {
		logx.Errorf("[audit_outbox] Audit outbox relay not started: %v", err)
		return &auditOutboxRelay{}
	}
	members := workshard.NewMembership(playerStoreMembersPrefix())
	members.SetSelf(selfUUID)
	loop, err := auditoutbox.New(auditoutbox.Options{
		StorageIDs: stores.RelayStorageIDs,
		Open: func(ctx context.Context, storageID uint32) (auditoutbox.Store, error) {
			store, err := stores.Store(ctx, storageID)
			if err != nil {
				return nil, err
			}
			name, _ := placement.StoreDBName(storageID)
			return auditoutbox.NewSQLStore(store.DB, name), nil
		},
		Owns:     members.Owns,
		Relay:    relay,
		Producer: producer,
		Interval: time.Duration(cfg.IntervalMs) * time.Millisecond,
	})
	if err != nil {
		_ = producer.Close()
		logx.Errorf("[audit_outbox] Audit outbox relay not started: %v", err)
		return &auditOutboxRelay{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	membersDone, loopDone := make(chan struct{}), make(chan struct{})
	// 成员表先起:第一拍读到之前成员表为空,本副本做全部片(保活优先),正确性靠每片的命名锁与消费端去重。
	go func() { defer close(membersDone); members.Run(ctx, etcd) }()
	go func() { defer close(loopDone); loop.Run(ctx) }()
	logx.Infof("[audit_outbox] Audit outbox relay started: brokers=%v transaction_log_topic=%s snapshot_topic=%s interval=%dms members_prefix=%s",
		cfg.Brokers, cfg.EffectiveTransactionLogTopic(), cfg.EffectiveSnapshotTopic(), cfg.IntervalMs, playerStoreMembersPrefix())
	return &auditOutboxRelay{stop: func() {
		cancel()
		deadline := time.NewTimer(auditRelayStopTimeout)
		defer deadline.Stop()
		for _, done := range []chan struct{}{loopDone, membersDone} {
			select {
			case <-done:
			case <-deadline.C:
				logx.Errorf("[audit_outbox] Audit outbox relay did not stop within %v; rows it already sent but did not delete are forwarded again by the next process", auditRelayStopTimeout)
				return
			}
		}
		if err := producer.Close(); err != nil {
			logx.Errorf("[audit_outbox] Close the audit outbox Kafka writers: %v", err)
		}
	}}
}

// playerStoreMembersPrefix 圈住本地域全部 zone 注册的 go/db:registerPlayerStoreNode 的注册前缀 <eNodeType 名>.rpc
// 下 zone/<zone>/… 的键。go/db 能写本地域的每个存档库,所以单位可以跨 zone 分;canary 由 workshard.StableMembers 排除。
func playerStoreMembersPrefix() string {
	return base.ENodeType_name[int32(playerStoreNodeType)] + ".rpc/zone/"
}
