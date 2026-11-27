package logic

import (
	"time"

	"match/internal/metrics"

	"shared/authoritysql"

	"github.com/zeromicro/go-zero/core/logx"
)

// match 的两个保留期清理循环(观战意图 global_spectator.go、剧本审核流水 battle_script_review_purge.go)共用的一轮口径:
// 一分钟一轮,一轮里连续清页。一轮只清一页时吞吐封顶在每分钟一页(一天约 144 万行),而且不随副本数增长(各副本扫的是同一段索引);
// 观战意图的请求头行(每名看过观战的玩家一行)删不掉、每一遍都要翻过去,行数上去以后一遍要翻几天,入库再快一点就永远追不上。
// 20 页 × 1000 行,一天能翻约 2880 万行;时间上限保证库变慢时一轮也在十几秒内结束(循环停摆告警按 300 秒判)。
const (
	retentionPurgePagesPerRound = 20
	retentionPurgeRoundBudget   = 10 * time.Second
)

// drainRetentionPages 在一轮里连续清页:page 清一页,回「后面还有没有」与错误。扫完、出错、清满页数上限或超过时间上限就结束这一轮,
// 没清完的下一轮接着游标清。时间上限只在两页之间看,正在清的一页由它自己的预算收尾。
func drainRetentionPages(page func() (more bool, err error)) error {
	started := time.Now()
	for i := 0; i < retentionPurgePagesPerRound && time.Since(started) < retentionPurgeRoundBudget; i++ {
		more, err := page()
		if err != nil || !more {
			return err
		}
	}
	return nil
}

// observeRetentionRound 记一轮清理的结局(purge 取 metrics.RetentionPurge* 之一,日志里也用它,和告警的 purge 标签对得上)。
// 失败的一轮下一轮重试:清理只做幂等删除,没有未知结局,不进 HoldRelay;持续失败由告警 MatchRetentionPurgeFailing 报
// (deploy/k8s/match-alerts.yaml)。
// 搬库的冻结窗口里写者池上的读写都被闸门拒绝(authoritysql.IsWriterFrozen:这一轮确定没写),那不是清理坏了:只记 Info、不计失败,
// 口径同 HoldRelay 与终局发件箱。冻结里混着别的错误时照常计失败。
func observeRetentionRound(purge string, err error) {
	switch {
	case err == nil:
	case authoritysql.IsWriterFrozen(err):
		logx.Infof("[retention] %s purge paused: writer gate frozen; retrying next round", purge)
	default:
		metrics.ObserveRetentionPurgeFailure(purge)
		logx.Errorf("[retention] %s purge failed; retrying next round: %v", purge, err)
	}
}
