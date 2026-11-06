// Package metrics 只观测现役 SQL 排行榜和持久奖励工作器；不恢复已删除的 Redis 消费者指标。
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const subsystem = "rank"

const (
	OperationOpen   = "open"
	OperationSubmit = "submit"
	OperationFreeze = "freeze"
	OperationRemove = "remove"
	OperationDelete = "delete"
	WriteOK         = "ok"
	WriteReplay     = "replay"
	WriteRejected   = "rejected"
	WriteError      = "error"
	RewardDelivered = "delivered"
	RewardUnknown   = "unknown"
	RewardRejected  = "rejected"
	RewardInvalid   = "invalid"
	RewardBusy      = "busy"
	RewardError     = "error"
	// RewardAbandoned 本次明确拒收后奖励转放弃终态(已过期或尝试次数到顶),工作器不再投,需要人工核对重投。
	RewardAbandoned = "abandoned"
)

type collectors struct {
	write    *prometheus.CounterVec
	dispatch *prometheus.CounterVec
	scan     *prometheus.CounterVec
	dueDelay *prometheus.GaugeVec
}

func newCollectors() *collectors {
	return &collectors{
		write: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "write_total",
			Help:      "SQL域写API最终结果；ok为确认提交，原回执重放单列replay；未知提交只计error。",
		}, []string{"operation", "outcome"}),
		dispatch: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "reward_dispatch_total",
			Help:      "持久奖励工作器最终结果；delivered必须同时收到邮件ID并确认SQL奖励终态提交；abandoned为明确拒收且已过期或次数到顶后转放弃终态。",
		}, []string{"outcome"}),
		scan: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "reward_scan_total",
			Help:      "按现有status,next_attempt_ms索引读取可领取奖励的结果；错误不清除上次延迟观测。",
		}, []string{"outcome"}),
		// 无标签 GaugeVec 在首次真实成功扫描前没有样本，不能把尚未查询伪装成空队列。
		dueDelay: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "reward_due_delay_seconds",
			Help:      "最近成功SQL扫描中最早可领取奖励距其next_attempt_ms的延迟；真实空结果为0，失败保旧。",
		}, nil),
	}
}

var current = newCollectors()
var registerOnce sync.Once

// Register 由本进程入口显式调用；promhttp 沿既有 domainhost 暴露默认 registry。
func Register() {
	registerOnce.Do(func() { current.register(prometheus.DefaultRegisterer) })
}

func (c *collectors) register(reg prometheus.Registerer) {
	reg.MustRegister(c.write, c.dispatch, c.scan, c.dueDelay)
	for _, op := range []string{OperationOpen, OperationSubmit, OperationFreeze, OperationRemove, OperationDelete} {
		for _, outcome := range []string{WriteOK, WriteReplay, WriteRejected, WriteError} {
			c.write.WithLabelValues(op, outcome)
		}
	}
	for _, outcome := range []string{RewardDelivered, RewardUnknown, RewardRejected, RewardInvalid, RewardBusy, RewardError, RewardAbandoned} {
		c.dispatch.WithLabelValues(outcome)
	}
	c.scan.WithLabelValues("ok")
	c.scan.WithLabelValues("error")
}

func (c *collectors) observeWrite(operation, outcome string) bool {
	switch operation {
	case OperationOpen, OperationSubmit, OperationFreeze, OperationRemove, OperationDelete:
	default:
		return false
	}
	switch outcome {
	case WriteOK, WriteReplay, WriteRejected, WriteError:
	default:
		return false
	}
	c.write.WithLabelValues(operation, outcome).Inc()
	return true
}

func ObserveWrite(operation, outcome string) bool { return current.observeWrite(operation, outcome) }

func (c *collectors) observeDispatch(outcome string) bool {
	switch outcome {
	case RewardDelivered, RewardUnknown, RewardRejected, RewardInvalid, RewardBusy, RewardError, RewardAbandoned:
		c.dispatch.WithLabelValues(outcome).Inc()
		return true
	default:
		return false
	}
}

func ObserveDispatch(outcome string) bool { return current.observeDispatch(outcome) }

func (c *collectors) observeScan(now, oldest uint64, hasDue bool, err error) {
	if err != nil || (hasDue && oldest > now) {
		c.scan.WithLabelValues("error").Inc()
		return
	}
	c.scan.WithLabelValues("ok").Inc()
	if !hasDue {
		c.dueDelay.WithLabelValues().Set(0)
		return
	}
	// oldest 来自同一 SQL 查询且 WHERE next_attempt_ms<=now，避免无符号减法下溢。
	c.dueDelay.WithLabelValues().Set(float64(now-oldest) / 1000)
}

func ObserveScan(now, oldest uint64, hasDue bool, err error) {
	current.observeScan(now, oldest, hasDue, err)
}
