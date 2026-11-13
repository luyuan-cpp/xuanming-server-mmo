package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// 整段缺省:世代按第一代、两个基名取缺省、1 秒一拍;没有 Brokers(转发不起,发件箱只积不丢)。
func TestAuditOutboxDefaults(t *testing.T) {
	got := AuditOutboxConfig{}.Normalized()
	if got.TopicGeneration != 1 || got.TransactionLogTopic != "transaction_log_topic" || got.SnapshotTopic != "player_snapshot_topic" ||
		got.IntervalMs != DefaultAuditOutboxIntervalMs || len(got.Brokers) != 0 {
		t.Fatalf("defaults = %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.EffectiveTransactionLogTopic() != "transaction_log_topic_g1" || got.EffectiveSnapshotTopic() != "player_snapshot_topic_g1" {
		t.Fatalf("effective topics = %s / %s", got.EffectiveTransactionLogTopic(), got.EffectiveSnapshotTopic())
	}
}

// 越界拒绝,不悄悄夹紧;两个 topic 同名、Brokers 里有空地址也拒绝。
func TestAuditOutboxValidateRejects(t *testing.T) {
	cases := map[string]AuditOutboxConfig{
		"interval too small": {IntervalMs: MinAuditOutboxIntervalMs - 1},
		"interval too large": {IntervalMs: MaxAuditOutboxIntervalMs + 1},
		"same topic":         {TransactionLogTopic: "audit", SnapshotTopic: "audit"},
		"empty broker":       {Brokers: []string{"127.0.0.1:9092", ""}},
	}
	for name, c := range cases {
		if err := c.Normalized().Validate(); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
	// Config.Validate 在回填之前也按回填之后的值核(conf.Load 在 Normalize 之前就调它)。
	var c Config
	c.AuditOutbox.IntervalMs = MaxAuditOutboxIntervalMs + 1
	if err := c.Validate(); err == nil {
		t.Fatal("Config.Validate must reject an out-of-range AuditOutbox.IntervalMs")
	}
}

// 本机 go/db 投的 topic 必须是 data_service 消费的那个:世代与两个基名逐字相同,否则流水写进没人消费的 topic,
// 保留期一到就没了,两边都不报错。K8s 的 ConfigMap 由部署脚本从 data_service 那份渲染,契约测试另核。
func TestEtcYamlAuditOutboxMatchesDataServiceConsumer(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/db.yaml", &c); err != nil {
		t.Fatalf("load etc/db.yaml: %v", err)
	}
	var consumer struct {
		Kafka struct {
			Brokers             []string `json:"Brokers,optional"`
			TopicGeneration     uint32   `json:"TopicGeneration,optional"`
			TransactionLogTopic string   `json:"TransactionLogTopic,optional"`
			SnapshotTopic       string   `json:"SnapshotTopic,optional"`
		} `json:"Kafka,optional"`
	}
	if err := conf.Load("../../../data_service/etc/data_service.yaml", &consumer); err != nil {
		t.Fatalf("load go/data_service/etc/data_service.yaml: %v", err)
	}
	// 键名写错会静默保持零值、再被缺省值盖住:先核原始值都写出来了,样例才说明了这几个键。
	raw := c.AuditOutbox
	if len(raw.Brokers) == 0 || raw.TopicGeneration == 0 || raw.TransactionLogTopic == "" || raw.SnapshotTopic == "" || raw.IntervalMs == 0 {
		t.Fatalf("etc/db.yaml must spell out the AuditOutbox keys, got %+v", raw)
	}
	got, want := raw.Normalized(), consumer.Kafka
	if got.TopicGeneration != want.TopicGeneration || got.TransactionLogTopic != want.TransactionLogTopic || got.SnapshotTopic != want.SnapshotTopic {
		t.Fatalf("go/db AuditOutbox (gen %d, %s, %s) != data_service Kafka (gen %d, %s, %s)",
			got.TopicGeneration, got.TransactionLogTopic, got.SnapshotTopic, want.TopicGeneration, want.TransactionLogTopic, want.SnapshotTopic)
	}
	if len(want.Brokers) > 0 && (len(got.Brokers) != len(want.Brokers) || got.Brokers[0] != want.Brokers[0]) {
		t.Fatalf("local brokers differ: go/db %v, data_service %v", got.Brokers, want.Brokers)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}
