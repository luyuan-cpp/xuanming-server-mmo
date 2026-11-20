package logic

import "time"

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
