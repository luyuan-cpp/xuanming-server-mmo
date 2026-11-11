package kafkautil

import "fmt"

// 两个审计 topic 的基名缺省值,与 C++ kTransactionLogTopicBase、go/data_service 配置 Kafka.TransactionLogTopic /
// SnapshotTopic 的 default 相同。实际发送与消费用的是 AuditTopicName 拼出来的有效名,不是基名。
const (
	DefaultTransactionLogTopicBase = "transaction_log_topic"
	DefaultPlayerSnapshotTopicBase = "player_snapshot_topic"
)

// AuditTopicName 拼审计 topic(transaction_log / player_snapshot)的有效名 `<基名>_g<世代>`。
//
// go/db 的审计发件箱转发(生产者,docs/design/player-audit-outbox.md §7.3)与 go/data_service 的落库消费者共用这一个函数,
// 不各拼各的:拼得不一样,消息就进了没人消费的 topic,保留期一到就没了,两边都不报错。C++ 一侧是
// cpp/libs/modules/audit/audit_topic.h 的 AuditTopicName,规则逐字一致:
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
