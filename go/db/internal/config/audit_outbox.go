package config

import (
	"fmt"

	"shared/kafkautil"
)

// AuditOutboxConfig 是审计发件箱转发循环的配置(docs/design/player-audit-outbox.md §7)。
//
// 存盘事务照常往发件箱写,与这一段配没配无关;这一段只决定本副本投不投 Kafka。Brokers 为空时转发不起,
// db_audit_outbox_relay_enabled 报 0 并打 ERROR,发件箱只积不投(不丢),积压告警会响。
//
// TopicGeneration 与两个基名必须与 go/data_service 的 Kafka.TopicGeneration / TransactionLogTopic / SnapshotTopic 相同:
// 不同就是写进没人消费的 topic,保留期一到流水就没了。本机 yaml 由单测核对,K8s 由部署脚本从 data_service 那份渲染。
//
// go-zero 对整段 optional 且未出现的嵌套结构不回填 default,所以这里不写 default,由 Normalized 回填。
type AuditOutboxConfig struct {
	Brokers []string `json:"Brokers,optional"`
	// TopicGeneration:0 = 没配,按第一代 1(与 kafkautil.AuditTopicName 同一条规则)。
	TopicGeneration uint32 `json:"TopicGeneration,optional"`
	// 两个基名,空 = 缺省(transaction_log_topic / player_snapshot_topic)。
	TransactionLogTopic string `json:"TransactionLogTopic,optional"`
	SnapshotTopic       string `json:"SnapshotTopic,optional"`
	// IntervalMs:空闲时两轮扫描之间的间隔(毫秒),0 = 缺省 1000。整批读满时不等下一拍。
	IntervalMs int `json:"IntervalMs,optional"`
}

// 转发节拍的缺省值与边界。
const (
	DefaultAuditOutboxIntervalMs = 1000
	MinAuditOutboxIntervalMs     = 100
	MaxAuditOutboxIntervalMs     = 60000
)

// Normalized 返回回填了缺省值的副本。
func (a AuditOutboxConfig) Normalized() AuditOutboxConfig {
	if a.TopicGeneration == 0 {
		a.TopicGeneration = 1
	}
	if a.TransactionLogTopic == "" {
		a.TransactionLogTopic = kafkautil.DefaultTransactionLogTopicBase
	}
	if a.SnapshotTopic == "" {
		a.SnapshotTopic = kafkautil.DefaultPlayerSnapshotTopicBase
	}
	if a.IntervalMs == 0 {
		a.IntervalMs = DefaultAuditOutboxIntervalMs
	}
	return a
}

// Validate 核(通常是 Normalized 之后的)取值。越界拒绝,不悄悄夹紧。
func (a AuditOutboxConfig) Validate() error {
	if a.IntervalMs < MinAuditOutboxIntervalMs || a.IntervalMs > MaxAuditOutboxIntervalMs {
		return fmt.Errorf("AuditOutbox.IntervalMs must be in [%d, %d], got %d", MinAuditOutboxIntervalMs, MaxAuditOutboxIntervalMs, a.IntervalMs)
	}
	if a.TransactionLogTopic == a.SnapshotTopic {
		return fmt.Errorf("AuditOutbox.TransactionLogTopic and SnapshotTopic must differ, both are %q", a.TransactionLogTopic)
	}
	for _, broker := range a.Brokers {
		if broker == "" {
			return fmt.Errorf("AuditOutbox.Brokers contains an empty address")
		}
	}
	return nil
}

// EffectiveTransactionLogTopic、EffectiveSnapshotTopic 是转发真正投递的 topic 名(与 data_service 的消费端同一个函数拼)。
func (a AuditOutboxConfig) EffectiveTransactionLogTopic() string {
	return kafkautil.AuditTopicName(a.Normalized().TransactionLogTopic, a.TopicGeneration)
}

func (a AuditOutboxConfig) EffectiveSnapshotTopic() string {
	return kafkautil.AuditTopicName(a.Normalized().SnapshotTopic, a.TopicGeneration)
}
