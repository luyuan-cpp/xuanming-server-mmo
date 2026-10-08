package kafka

// 同道历练对局结果消费者(设计 docs/design/guild-phase2/06-activities.md §6.28)。
//
// battle 节点打完一局,把 contracts.kafka.BattleResultEvent 发到全局 topic match-results(key = battle_id,
// payload 直接是事件、不套 *Command 信封)。match 用消费组 match-rating 读它算评分;guild 用**另一个**消费组
// guild-trial 读同一个 topic,只挑出帮会同道历练的对局交给结算。两个消费组各有各的位点,互不影响。
//
// 分工(只有一处权威,别在这里重复实现):
//
//	本文件   拉取 → 反序列化 → 过滤(是不是帮会历练)→ 调 handler → 决定"提交位点"还是"退避后重调"
//	handler  logic.GuildLogic.SettleTrialResult:结算事务、幂等闸门、毒消息标记、**销账**(删 SharedRedis 上的
//	         battle:activity_result:{battle_id})全在它里面。本文件不碰 MySQL、不碰 Redis、不删结果键。
//
// 四条纪律:
//
//  1. **至少一次 + 幂等**。位点在 handler 回 nil 之后才提交(CommitInterval=0,逐条同步提交)。提交前崩溃、
//     重平衡、battle 未销账的重发(每 10s 一次,至多 30 次)都会让同一局再来一遍,由 handler 的幂等闸门
//     (对局行 SETTLED)挡住,回"已结算过",照常提交。
//  2. **暂时性失败不提交、不跳过、不设次数上限**(与 AGENTS §11.3"重试要有次数上限"的偏离,理由如下)。
//     handler 回 error = MySQL / Redis 故障、写冲突、合服闸门、号段不可用。这些都会自己好,而跳过 = 这一局的
//     帮贡、资金、物品永久不发(玩家资产路径,fail-closed)。所以按 1s、2s、4s … 封顶 30s 退避,一直重调到成功
//     或进程退出。
//     代价是**本进程的历练结果消费整个停在这条消息上,不只是它所在的那个分区**:消费是单协程串行的
//     (consume 是唯一的循环,重调就发生在这个协程里),而消费组 reader 把本进程分到的全部分区汇进同一个
//     FetchMessage —— 卡住期间一条也不再取,别的分区、别的 zone、别的帮的结果都跟着等。只有一个 guild 副本时
//     (它独占全部分区)等的是全服的历练结算;多副本时是这个副本名下的那几个分区(reader 的心跳在它自己的协程里
//     照常发,卡住不会触发重平衡把分区让给别的副本)。这段时间里:
//       - 已登记的对局,由巡检器在登记 ResultOverdue(默认 420s)之后从 battle 落下的结果记录兜底结算
//         (巡检器在另一个协程,走同一个 SettleTrialResult);
//       - 没有登记行的对局(开战时登记失败、或 match 超时而实际已开战)只能等这里恢复。
//     不丢奖,只是晚到。它是可见的:guild_trial_result_total{result="handler_retry"} 持续上涨、消费组在本进程
//     名下**各个分区**的 lag 一起上涨,每次重调一条 ERROR。人工出口见 06 §6.35a 场景一。
//     为什么不按分区隔离(v1 的取舍):topic 的 key 是 battle_id,分区与 zone / 帮会没有对应关系 —— 最现实的
//     长时间卡住是某个 zone 合服,它的在途对局散在每一个分区上,隔离换不来"别的 zone 不受影响";而 kafka-go 的
//     消费组 reader 不能按分区暂停,要隔离就得在进程内无上限地缓存被卡分区的后续消息。
//     不加抖动:每个进程同一时刻至多在重调 1 条,形不成惊群。
//     确定性失败(溢出、奖励包坏、上下文畸形)由 handler 自己标成毒消息并回 nil,不会走到这条路上。
//  3. **与 guild 无关的消息只提交、不处理**。match-results 里绝大多数是普通对局(没有活动上下文)或别的活动类型;
//     它们的结果记录也不归 guild 销账。
//  4. **单次 handler 的 panic 只算一次暂时性失败**(safego.Run)。让它把消费 goroutine 带走的后果更糟:
//     reader 关闭 → 重平衡 → 别的副本接手同一条消息 → 同样 panic,最后全服没有消费者,而进程都还活着。
//
// 本文件不 import internal/logic 与 internal/config:svc 已经 import 了本包(GateCommandBuilder),
// 再从这里指回 logic,logic 就永远不能依赖 svc。结算入口与两个计数出口由 guild.go 以回调注入
// (写法同 go/match/internal/kafka/result_consumer.go)。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	"shared/kafkautil"
	"shared/safego"
)

// TrialResultConsumerConfig 是消费者参数,由 guild.go 从 config.Config 装配。
type TrialResultConsumerConfig struct {
	// Brokers 是 Kafka 地址(= guild 的 Kafka.Brokers)。
	Brokers []string
	// Topic 必须与 match 的 ResultTopic、battle 的 kMatchResultsTopic 相同(默认 match-results,全局、不带 zone 段)。
	Topic string
	// GroupID 是消费组(默认 guild-trial)。guild 的所有副本必须同组:同组分摊分区,每条结果只被一个副本处理。
	GroupID string
	// Partitions 必须与 match 的 ResultTopicPartitions 相同。topic 由先起的一方建出来,分区数建后不可变;
	// 与 broker 现状不一致时 EnsureTopics 报错,消费者不启动(每 30s 重试并打 ERROR),让部署问题暴露出来。
	Partitions int32
}

// TrialResultHandler 处理一条帮会历练的对局结果。
//
//   - 返回 nil:这一局有了定论(已结算 / 此前已结算过 / 帮会已解散 / 上下文不可用 / 已标成毒消息)。消费者提交位点。
//   - 返回 error:暂时性失败,什么都没提交。消费者**不提交位点**,退避后带同一条消息重调,直到成功或 ctx 结束。
//
// 必须幂等(同一条消息会被调任意多次),必须在 ctx 结束时尽快返回。每次调用的 ctx 带 trialResultHandleTimeout 的截止时间。
type TrialResultHandler func(ctx context.Context, ev *kafkapb.BattleResultEvent) error

// TrialResultCounters 是消费者自己的两个计数出口。指标在 logic 包注册(guild_trial_result_total),
// 本包不许另建同名指标(重复注册会 panic),所以经回调计数。字段为 nil = 不计(单测)。
type TrialResultCounters struct {
	// DecodeError:一条消息反序列化失败(已提交位点、不再处理)。
	DecodeError func()
	// HandlerRetry:handler 回了暂时性错误,消费者将退避后重调(每次重调计一次)。
	HandlerRetry func()
}

const (
	// trialResultTopicRetentionMs:topic 保留 7 天。与 match 的 resultTopicRetentionMs、battle 结果记录的 TTL 同值 ——
	// 两个服务启动时都会把这个 topic 的 retention.ms 设成自己的值,不同值会互相覆盖。
	trialResultTopicRetentionMs = 7 * 24 * 3600 * 1000
	// trialResultFetchBackoff:拉取失败(broker 抖动、重平衡)后的固定退避。
	trialResultFetchBackoff = time.Second
	// handler 暂时性失败的退避:1s 起、逐次翻倍、封顶 30s(06 §6.28 第 4 步)。
	trialResultRetryBackoffMin = time.Second
	trialResultRetryBackoffMax = 30 * time.Second
	// trialResultStartRetryInterval:启动时确保 topic 失败(Kafka 未就绪、分区数不符)后多久再试。
	// 不拒绝 guild 启动:帮会的其余功能不依赖 Kafka 消费(写法同 match_service.go 的结果消费者)。
	trialResultStartRetryInterval = 30 * time.Second
	// trialResultHandleTimeout:单次调 handler 的上限(纵深防御)。结算正常在几秒内结束
	// (结算事务有自己的子预算,提交后的同步投递合计封顶约 3.5s);某个依赖把调用挂住时,到点按一次暂时性失败处理,
	// 换来一条日志与一次计数,而不是一个不出声的、永远停住的消费者(纪律 2:停住的是本进程名下的全部分区)。
	// 到点时事务回滚(或提交结果不明),重调由幂等闸门兜住。
	trialResultHandleTimeout = 30 * time.Second
)

// safego 点位名:直接成为 safego_panic_total 的 label,必须是常量。
const (
	trialResultHandlePoint      = "guild.kafka.trial_results.handle"
	trialResultEnsureTopicPoint = "guild.kafka.trial_results.ensure_topic"
)

// errTrialHandlerPanicked:handler panic 被兜住后折成的暂时性错误(纪律 4)。栈由 safego 打在日志里。
var errTrialHandlerPanicked = errors.New("trial result handler panicked (see goroutine_panic log)")

// trialReader 是消费循环对 Kafka reader 的全部要求;*kafkago.Reader 满足它。抽成接口只为可测(不连真实 Kafka)。
type trialReader interface {
	FetchMessage(ctx context.Context) (kafkago.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafkago.Message) error
	Close() error
}

// TrialResultConsumer 是历练对局结果的消费者。构造后只被 Run 的那一个 goroutine 使用。
type TrialResultConsumer struct {
	cfg      TrialResultConsumerConfig
	handle   TrialResultHandler
	counters TrialResultCounters

	// 以下三个是与外界的接缝:生产取 NewTrialResultConsumer 填的默认实现,单测换成替身。
	ensureTopics func(brokers []string, specs []kafkautil.TopicSpec) error
	openReader   func(cfg TrialResultConsumerConfig) trialReader
	// sleep 可中断地等待 d;ctx 结束返回 false。
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewTrialResultConsumer 校验参数并建消费者,不连 Kafka(连接发生在 Run 里)。
// 参数非法只可能是装配或配置错误,返回 error 由调用方决定拒启。
func NewTrialResultConsumer(cfg TrialResultConsumerConfig, handle TrialResultHandler, counters TrialResultCounters) (*TrialResultConsumer, error) {
	if handle == nil {
		return nil, errors.New("guild trial result consumer: nil handler")
	}
	brokers := make([]string, 0, len(cfg.Brokers))
	for _, broker := range cfg.Brokers {
		if strings.TrimSpace(broker) != "" {
			brokers = append(brokers, broker)
		}
	}
	if len(brokers) == 0 {
		return nil, errors.New("guild trial result consumer: Kafka.Brokers 为空,无法消费对局结果")
	}
	if strings.TrimSpace(cfg.Topic) == "" || strings.TrimSpace(cfg.GroupID) == "" {
		return nil, errors.New("guild trial result consumer: Topic / GroupID 不能为空")
	}
	if cfg.Partitions < 1 {
		return nil, fmt.Errorf("guild trial result consumer: Partitions(%d)必须 ≥ 1,且与 match 的 ResultTopicPartitions 相同", cfg.Partitions)
	}
	cfg.Brokers = brokers
	return &TrialResultConsumer{
		cfg:          cfg,
		handle:       handle,
		counters:     counters,
		ensureTopics: kafkautil.EnsureTopics,
		openReader:   openTrialResultReader,
		sleep:        sleepTrialResult,
	}, nil
}

// openTrialResultReader 建消费组 reader(参数照 match 的结果消费者)。
func openTrialResultReader(cfg TrialResultConsumerConfig) trialReader {
	return kafkago.NewReader(kafkago.ReaderConfig{
		Brokers: cfg.Brokers,
		GroupID: cfg.GroupID,
		Topic:   cfg.Topic,
		// 新消费组从最早开始:guild 晚于 battle 启动、或首次上线时,已经发出的历练结果不能丢。
		// 普通对局在过滤那一步就跳过;带上下文而未结算的是真实的对局,补发是对的(06 §6.34 T12)。
		StartOffset: kafkago.FirstOffset,
		MinBytes:    1,
		MaxBytes:    1 << 20,
		MaxWait:     500 * time.Millisecond,
		// CommitInterval=0:每条处理完同步提交。崩溃最多重放一条,由幂等闸门兜住;
		// 也让"消费组 lag = 0"成为可信的验收判据(异步批量提交会让空闲时的 lag 停在非 0)。
		CommitInterval: 0,
	})
}

// sleepTrialResult 可中断的等待;ctx 结束返回 false。
func sleepTrialResult(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Run 阻塞运行消费者,直到 ctx 结束:确保 topic 存在(失败每 30s 重试,不放弃)→ 建 reader → 逐条消费。
//
// 调用方负责把它放进自己的 goroutine(guild.go 用 safego.Go("guild.kafka.trial_results"))。
// ctx 结束后在有界时间内返回:等待与拉取都可中断;正在进行的 handler 靠它自己响应 ctx;
// 正在进行的"确保 topic"(sarama 的管理连接不接受 ctx)会被丢下,由它自己的套接字超时收尾。
// 返回前关闭 reader(离开消费组,分区交还给别的副本)。Run 只应调用一次。
func (c *TrialResultConsumer) Run(ctx context.Context) {
	if !c.waitForTopic(ctx) {
		return
	}
	reader := c.openReader(c.cfg)
	defer func() {
		if err := reader.Close(); err != nil {
			logx.Errorf("[GuildTrial] 关闭历练结果 reader 失败: %v", err)
		}
	}()
	// 这一行是部署验收的判据(06 §6.46 B6b-srv2):topic 与 group 都**不带** _zN 后缀。改文案要同步改文档。
	logx.Infof("[GuildTrial] 历练结果消费者启动 topic=%s group=%s partitions=%d brokers=%v",
		c.cfg.Topic, c.cfg.GroupID, c.cfg.Partitions, c.cfg.Brokers)
	c.consume(ctx, reader)
	logx.Info("[GuildTrial] 历练结果消费者退出")
}

// waitForTopic 确保 topic 存在且分区数与配置一致,失败就每 trialResultStartRetryInterval 重试。
// 返回 false = ctx 已结束(从未成功)。
func (c *TrialResultConsumer) waitForTopic(ctx context.Context) bool {
	for attempt := 1; ; attempt++ {
		err := c.ensureTopicWithin(ctx)
		if err == nil {
			if attempt > 1 {
				logx.Infof("[GuildTrial] 历练结果 topic %s 已就绪(第 %d 次尝试)", c.cfg.Topic, attempt)
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		// 这段时间里历练照常能开战,结果先落在 topic 与 battle 的结果记录里,消费者起来后从最早位点补上。
		logx.Errorf("[GuildTrial] 确保历练结果 topic %s(partitions=%d)失败,结果消费未启动,%v 后重试(第 %d 次): %v",
			c.cfg.Topic, c.cfg.Partitions, trialResultStartRetryInterval, attempt, err)
		if !c.sleep(ctx, trialResultStartRetryInterval) {
			return false
		}
	}
}

// ensureTopicWithin 调一次 ensureTopics,但不让它拖住退出:管理连接不接受 ctx,连不上时要等到它自己的拨号超时。
// 所以放进单独的协程,本协程在"结果到达"与"ctx 结束"之间择先。被丢下的协程不会泄漏:通道带 1 格缓冲,
// 它最迟在套接字超时后返回(写法同 logic 的 lockedBattleID)。
func (c *TrialResultConsumer) ensureTopicWithin(ctx context.Context) error {
	done := make(chan error, 1)
	specs := []kafkautil.TopicSpec{{
		Name:        c.cfg.Topic,
		Partitions:  c.cfg.Partitions,
		RetentionMs: trialResultTopicRetentionMs,
	}}
	safego.Go(trialResultEnsureTopicPoint, func() {
		// 先放一个兜底值:ensureTopics 若 panic,defer 仍把它送出去,等的一方不会永远等下去。
		err := errors.New("ensure topics panicked (see goroutine_panic log)")
		defer func() { done <- err }()
		err = c.ensureTopics(c.cfg.Brokers, specs)
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// consume 是消费主循环:拉取 → 处理 → 提交,直到 ctx 结束或 reader 被关闭。
//
// 它是本进程里**唯一**取消息的地方,严格串行:process 不返回就不取下一条。reader 把本进程分到的全部分区汇成一路,
// 所以一条消息在 settle 里重调期间,别的分区的消息同样不被取走(文件头纪律 2 的影响面就来自这里)。
func (c *TrialResultConsumer) consume(ctx context.Context, reader trialReader) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			// 只认自己的 ctx:它没结束而错误恰好是 context.Canceled 时(reader 内部的取消),按普通拉取失败退避重试,
			// 不能据此退出 —— 那会让消费者不出声地停掉。
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				// reader 已被关闭(只可能是别处误关):继续拉只会空转,退出并留下一条 ERROR。
				logx.Error("[GuildTrial] 历练结果 reader 已关闭,消费循环退出(本进程不再消费历练结果,需重启)")
				return
			}
			logx.Errorf("[GuildTrial] 拉取历练结果失败,%v 后重试: %v", trialResultFetchBackoff, err)
			if !c.sleep(ctx, trialResultFetchBackoff) {
				return
			}
			continue
		}
		if !c.process(ctx, reader, msg) {
			return
		}
	}
}

// process 处理一条消息。返回 false = ctx 已结束,**这条消息没有提交**,调用方应退出。
func (c *TrialResultConsumer) process(ctx context.Context, reader trialReader, msg kafkago.Message) bool {
	ev := &kafkapb.BattleResultEvent{}
	if err := proto.Unmarshal(msg.Value, ev); err != nil {
		// 坏消息跳过并提交:留在 topic 里只会每次重启都撞一遍(match 的消费者对同一条消息也是这样处理)。
		// 它若真是一局历练,battle 落在 Redis 的结果记录还在,巡检器会在结果超时后从那里取回来处理。
		logx.Errorf("[GuildTrial] 对局结果反序列化失败,跳过 partition=%d offset=%d key=%s: %v",
			msg.Partition, msg.Offset, string(msg.Key), err)
		countIfSet(c.counters.DecodeError)
		c.commit(ctx, reader, msg)
		return true
	}
	if !isGuildTrialResult(ev) {
		c.commit(ctx, reader, msg)
		return true
	}
	if key := string(msg.Key); key != "" && key != strconv.FormatUint(ev.GetBattleId(), 10) {
		// key 契约是 battle_id(同一局的重发落在同一分区、保持有序)。不一致只记日志,以 payload 里的 battle_id 为准。
		logx.Errorf("[GuildTrial] 历练结果 key=%s 与 battle_id=%d 不一致 partition=%d offset=%d",
			key, ev.GetBattleId(), msg.Partition, msg.Offset)
	}
	if !c.settle(ctx, msg, ev) {
		return false
	}
	c.commit(ctx, reader, msg)
	return true
}

// isGuildTrialResult 报告一条结果是否属于帮会同道历练。没有活动上下文(普通对局)、或是别的活动类型 → false。
// 上下文残缺(各 id 为 0)但 kind 是历练的仍算:交给 handler 判成"上下文不可用"并销账,不在这里悄悄吞掉。
func isGuildTrialResult(ev *kafkapb.BattleResultEvent) bool {
	return ev.GetActivityContext().GetKind() == battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL
}

// settle 调 handler 直到它回 nil(纪律 2)。返回 false = ctx 已结束、handler 仍未成功,调用方不得提交。
// 重调发生在消费协程里:它不返回,本进程名下**全部分区**的历练结果都不再被取走(不只是 msg.Partition)。
func (c *TrialResultConsumer) settle(ctx context.Context, msg kafkago.Message, ev *kafkapb.BattleResultEvent) bool {
	backoff := trialResultRetryBackoffMin
	for attempt := 1; ; attempt++ {
		err := c.attempt(ctx, ev)
		if err == nil {
			if attempt > 1 {
				logx.Infof("[GuildTrial] 历练结果在第 %d 次处理成功 battle=%d partition=%d offset=%d",
					attempt, ev.GetBattleId(), msg.Partition, msg.Offset)
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		countIfSet(c.counters.HandlerRetry)
		logx.Errorf("[GuildTrial] 历练结果处理失败(第 %d 次),位点不提交,%v 后重调;本进程的历练结果消费(它分到的全部分区)在这一条成功之前不前进 battle=%d guild=%d partition=%d offset=%d: %v",
			attempt, backoff, ev.GetBattleId(), ev.GetActivityContext().GetGuildId(), msg.Partition, msg.Offset, err)
		if !c.sleep(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, trialResultRetryBackoffMax)
	}
}

// attempt 调一次 handler:带单次上限,并把 panic 折成暂时性错误(纪律 4)。
func (c *TrialResultConsumer) attempt(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
	attemptCtx, cancel := context.WithTimeout(ctx, trialResultHandleTimeout)
	defer cancel()
	var err error
	if ok := safego.Run(trialResultHandlePoint, func() { err = c.handle(attemptCtx, ev) }); !ok {
		return errTrialHandlerPanicked
	}
	return err
}

// commit 同步提交一条消息的位点。失败只记日志:下一次提交会覆盖它;在那之前崩溃则重放到上次成功提交处,
// 由幂等闸门兜住。ctx 已结束时的失败不记(进程正在退出,重启后重放一条)。
func (c *TrialResultConsumer) commit(ctx context.Context, reader trialReader, msg kafkago.Message) {
	if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
		logx.Errorf("[GuildTrial] 提交历练结果位点失败 partition=%d offset=%d: %v", msg.Partition, msg.Offset, err)
	}
}

// countIfSet 调一个可为 nil 的计数回调。
func countIfSet(fn func()) {
	if fn != nil {
		fn()
	}
}
