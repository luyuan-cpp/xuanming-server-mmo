package kafkautil

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// GroupLagTarget 是 lag 探针要探的一个消费组在一个 topic 上的积压。
type GroupLagTarget struct {
	Group      string
	Topic      string
	Partitions int32
}

// GroupLagObserver 接收一轮里一个目标的结局。err 非 nil 时 lag 无意义：调用方记失败计数，gauge 保留上一次的值
// （陈值由调用方按实例的「探针失明」告警点名）。observe 在探针 goroutine 里同步调用，不要在里面阻塞。
type GroupLagObserver func(target GroupLagTarget, lag GroupLag, err error)

// RunGroupLagProbe 每 interval 探一次各目标在 broker 上的积压，直到 ctx 结束；第一轮在 interval 之后。
// 只读 broker 元数据（computeGroupLag），不碰消费与提交，可以和任意副本数的消费者并存：每个副本报同一个组的值，告警取 max。
// 任何一个目标探失败都关掉连接、下一轮重建（sarama 的连接坏了没有可靠的就地自愈判据），写法同 data_service 的 runLagProbe。
// 交给 observe 的 lag.CommitStall 由本循环跨轮算出（有积压就计时，不要求新消息到达，见 CommitStall）。
// point 是 safego 点位名（常量）：每一轮各自 recover，一轮 panic 不杀循环；panic 的那一轮停在哪一步不知道，它持有的连接
// （含这一轮刚建的）一律关掉、下一轮重建。返回前关掉手上的连接；调用方要在关停时等本函数返回（match 的 StopAndReport）。
// interval <= 0 视为装配错误，记一条日志后不启动。
func RunGroupLagProbe(ctx context.Context, brokers []string, interval time.Duration, point string, targets []GroupLagTarget, observe GroupLagObserver) {
	if interval <= 0 || observe == nil || len(targets) == 0 {
		logx.Errorf("[kafka] group lag probe %s not started: interval=%v targets=%d observer=%t", point, interval, len(targets), observe != nil)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runGroupLagProbe(ctx, ticker.C, point, targets, observe, func() (groupLagProbeConn, error) {
		prober, err := NewGroupLagProber(brokers)
		if err != nil {
			return nil, err
		}
		return prober, nil
	})
}

// groupLagProbeConn 是探针循环对连接的全部要求；*GroupLagProber 满足它。单测换成替身，不连 Kafka。
type groupLagProbeConn interface {
	ProbeOffsets(group, topic string, partitions int32) (GroupLag, []PartitionOffsets, error)
	Close() error
}

func runGroupLagProbe(ctx context.Context, ticks <-chan time.Time, point string, targets []GroupLagTarget, observe GroupLagObserver, connect func() (groupLagProbeConn, error)) {
	loop := &groupLagProbeLoop{point: point, targets: targets, observe: observe, connect: connect, stalls: make([]CommitStall, len(targets))}
	defer loop.closeConn("close")
	for {
		var now time.Time
		select {
		case <-ctx.Done():
			return
		case now = <-ticks:
		}
		if !safego.Run(point, func() { loop.round(now) }) {
			// 这一轮 panic(多半在 observe 里):停在哪一步不知道,手上的连接(含这一轮刚建的)关掉,下一轮重建。
			loop.closeConn("close after panic")
		}
	}
}

// groupLagProbeLoop 是一个探针 goroutine 的全部状态,只在该 goroutine 里读写。
type groupLagProbeLoop struct {
	point   string
	targets []GroupLagTarget
	observe GroupLagObserver
	connect func() (groupLagProbeConn, error)
	// prober 是当前连接。一轮里新建的连接一建好就记在这里,那一轮半途 panic 时 closeConn 照样关得到它
	// (以前新连接只在一轮正常返回时才赋给外层变量,panic 一次就漏一条 broker 连接)。
	prober groupLagProbeConn
	// stalls 与 targets 一一对应,跨轮记各分区已提交位点停了多久;连接重建不影响它。
	stalls []CommitStall
}

// closeConn 关掉当前连接并清空;没有连接时什么都不做。
func (l *groupLagProbeLoop) closeConn(why string) {
	if l.prober == nil {
		return
	}
	if err := l.prober.Close(); err != nil {
		logx.Errorf("[kafka] group lag probe %s %s: %v", l.point, why, err)
	}
	l.prober = nil
}

// round 探一轮。没有连接时先建,建好立即记下;连不上,或任何一个目标探失败,都不留连接(探失败的先关掉),下一轮重建。
// now 是这一轮的时刻(ticker 给的,带单调时钟读数),只用来给停滞计时;探失败的目标不推进停滞计时。
func (l *groupLagProbeLoop) round(now time.Time) {
	if l.prober == nil {
		next, err := l.connect()
		if err != nil {
			for _, target := range l.targets {
				l.observe(target, GroupLag{}, err)
			}
			return
		}
		l.prober = next
	}
	healthy := true
	for i, target := range l.targets {
		lag, offsets, err := l.prober.ProbeOffsets(target.Group, target.Topic, target.Partitions)
		if err == nil {
			lag.CommitStall = l.stalls[i].Observe(now, offsets)
		} else {
			healthy = false
		}
		l.observe(target, lag, err)
	}
	if !healthy {
		l.closeConn("close after failure")
	}
}

// CommitStall 跟踪一个消费组「有积压的分区里,已提交位点最久多长时间没动」,探针每轮成功探测后调一次 Observe。
// 只在探针 goroutine 里用,不加锁;零值可用。判据是「组内已提交位点低于日志末端(high watermark)且停着」,不依赖新消息到达:
// 安静分区上卡在一条上(毒丸、消费者停了)也报。
//
// 为什么不看合计积压是否归零:逐条提交的消费者在持续高负载下,每次探测都有刚到、还没处理的消息,合计积压几乎从不为 0,
// 而已提交位点一直在前进;停了或卡住时位点不动。为什么按分区:一个分区卡在毒丸上、其余分区照常前进时,
// 组的合计位点照样在动,按合计看会漏掉它。
//
// 两种模式都没有「已落库却一直挂着」的正常积压:handler 模式代次还在就原地重试同一条,代次结束交还、下一代次立刻重投;
// reader 模式提交失败交还已落库的记录时结束本代次,下一代次立即重投并提交(BoundConsumerGroup.releaseUncommitted)。
// 2026-11-12 以前 reader 模式不结束代次,低流量分区上交还的几条会挂着积压直到新消息到达,所以当时另有 RequireArrivals
// 只认「位点停下之后新到的消息」,代价是安静分区卡住看不见;根因修掉之后去掉了这个开关。
type CommitStall struct {
	marks map[int32]commitStallMark
}

// commitStallMark 是一个有积压的分区最近一次看到已提交位点变化(或第一次看到这段积压)时的状态。
type commitStallMark struct {
	committed int64     // 那一轮读到的已提交位点
	since     time.Time // 开始计时的时刻
}

// Observe 用一轮读到的各分区位点更新状态,返回有积压的分区里已提交位点最久没动的时长,没有停滞返回 0。
// 位点一动、或积压清零,那个分区从头计;本轮没报的分区丢掉状态。now 生产传 time.Now()(带单调时钟读数),单测传手拨的时刻。
func (s *CommitStall) Observe(now time.Time, partitions []PartitionOffsets) time.Duration {
	if s.marks == nil {
		s.marks = make(map[int32]commitStallMark, len(partitions))
	}
	seen := make(map[int32]bool, len(partitions))
	var longest time.Duration
	for _, p := range partitions {
		seen[p.Partition] = true
		if p.Backlog() == 0 {
			delete(s.marks, p.Partition)
			continue
		}
		mark, tracked := s.marks[p.Partition]
		if !tracked || mark.committed != p.Committed {
			mark = commitStallMark{committed: p.Committed, since: now}
			s.marks[p.Partition] = mark
		}
		longest = max(longest, now.Sub(mark.since))
	}
	for partition := range s.marks {
		if !seen[partition] {
			delete(s.marks, partition)
		}
	}
	return longest
}
