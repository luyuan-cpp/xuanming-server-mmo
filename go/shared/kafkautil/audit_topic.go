package kafkautil

import "fmt"

// 两个审计 topic 的基名缺省值,与 go/db 配置 AuditOutbox.TransactionLogTopic / SnapshotTopic、go/data_service 配置
// Kafka.TransactionLogTopic / SnapshotTopic 的缺省值相同。实际发送与消费用的是 AuditTopicName 拼出来的有效名,不是基名。
const (
	DefaultTransactionLogTopicBase = "transaction_log_topic"
	DefaultPlayerSnapshotTopicBase = "player_snapshot_topic"
)

// AuditTopicName 拼审计 topic(transaction_log / player_snapshot)的有效名 `<基名>_g<世代>`。
//
// go/db 的审计发件箱转发(唯一的生产者,docs/design/player-audit-outbox.md §7.3)与 go/data_service 的落库消费者共用这一个函数,
// 不各拼各的:拼得不一样,消息就进了没人消费的 topic,保留期一到就没了,两边都不报错。C++ 从 2026-11-12 起不再直接发审计 topic
// (流水与快照请求随存盘交给 go/db),原来的 C++ 版本 audit_topic.h 已删。规则:
//   - 第一代也带后缀(broker 开着自动建 topic,裸名字会被生产者建成 1 分区);
//   - generation 为 0 按 1(整段配置缺席时 go-zero 不回填 default);
//   - base 为空返回空,交给调用方 fail-closed,不拼出一个只有后缀的名字。
func AuditTopicName(base string, generation uint32) string {
	if base == "" {
		return ""
	}
	if generation == 0 {
		generation = 1
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}
