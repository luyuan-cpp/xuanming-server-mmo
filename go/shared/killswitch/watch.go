package killswitch

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/safego"
)

// watchPoint 是 safego 的点位名,同时也是 safego_panic_total 的 label。
const watchPoint = "killswitch.watch"

// Start 启动后台 list-watch。**非阻塞**,立刻返回。
//
// cli 为 nil 时直接返回:没接 etcd 就是永远放行,这本身是合法配置,
// 不是错误 —— 不要在这里 fatal,更不要阻塞服务启动。
//
// 形态与 scene_manager 的 LoadReporter 一致:先全量 Get 建基线,
// 再从该 revision 起 Watch;watch 断了就回去重新全量同步。
func (s *Switch) Start(ctx context.Context, cli *clientv3.Client) {
	if cli == nil {
		logx.Infof("[killswitch] 未配置 etcd 客户端,全部方法放行(prefix=%s)", s.cfg.prefix())
		return
	}
	safego.Go(watchPoint, func() { s.run(ctx, cli) })
}

// run 是 list-watch 主循环,直到 ctx 结束才返回。
func (s *Switch) run(ctx context.Context, cli *clientv3.Client) {
	prefix := s.cfg.prefix()
	logx.Infof("[killswitch] 开始 watch prefix=%s", prefix)

	for {
		rules, rev, err := s.fullSync(ctx, cli)
		if err != nil {
			incSyncFail()
			logx.Errorf("[killswitch] 全量同步失败(不影响放行): %v", err)
			// 失联期间快照按 StaleAfter 自然过期,这里不主动清 ——
			// 网络抖一下不该把正在生效的止血阀松开。
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.cfg.resyncBackoff()):
				continue
			}
		}

		s.setRules(rules, s.now())
		logx.Infof("[killswitch] 全量同步完成: %d 条规则, rev=%d", len(rules), rev)

		s.watch(ctx, cli, rules, rev)

		if ctx.Err() != nil {
			return
		}
		logx.Info("[killswitch] watch 中断,重新全量同步")
	}
}

// fullSync 全量拉一次前缀下的规则,返回规则表与 etcd revision。
func (s *Switch) fullSync(ctx context.Context, cli *clientv3.Client) (map[string]Rule, int64, error) {
	prefix := s.cfg.prefix()
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, fmt.Errorf("etcd get %s: %w", prefix, err)
	}

	rules := make(map[string]Rule, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		pattern, rule, ok := s.parseKV(string(kv.Key), kv.Value)
		if !ok {
			continue
		}
		rules[pattern] = rule
	}
	return rules, resp.Header.Revision, nil
}

// watch 从 rev+1 起消费事件,增量维护 rules 并整体发布新快照。
// 返回即表示需要重新全量同步(或 ctx 已结束)。
func (s *Switch) watch(ctx context.Context, cli *clientv3.Client, rules map[string]Rule, rev int64) {
	prefix := s.cfg.prefix()
	ch := cli.Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithRev(rev+1))

	// 与 etcd 保持连接期间,快照的 syncedAt 需要持续续期,
	// 否则 StaleAfter 会把一份其实是最新的快照判成过期。
	renew := time.NewTicker(s.cfg.resyncBackoff())
	defer renew.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-renew.C:
			// 只续期,不改内容。
			s.setRules(rules, s.now())

		case resp, ok := <-ch:
			if !ok {
				logx.Info("[killswitch] watch 通道已关闭")
				return
			}
			if err := resp.Err(); err != nil {
				incSyncFail()
				logx.Errorf("[killswitch] watch 出错(不影响放行): %v", err)
				return
			}

			if s.applyEvents(rules, resp.Events) {
				s.setRules(rules, s.now())
			}
		}
	}
}

// applyEvents 把一批 etcd 事件**就地**应用到 rules 上,返回内容是否真的变了。
//
// 单独抽出来是为了可测:etcd 的连接、重连、revision 推进都无法在单测里复现,
// 但"一批事件怎么改规则表"是纯逻辑,而它恰恰是会出错的那一块。
func (s *Switch) applyEvents(rules map[string]Rule, events []*clientv3.Event) bool {
	changed := false
	for _, ev := range events {
		if ev == nil || ev.Kv == nil {
			continue
		}
		key := string(ev.Kv.Key)

		switch ev.Type {
		case clientv3.EventTypePut:
			pattern, rule, valid := s.parseKV(key, ev.Kv.Value)
			if !valid {
				// 值坏了就当这条事件没发生,保留原有规则 —— fail-open。
				continue
			}
			if old, exists := rules[pattern]; exists && old == rule {
				continue
			}
			rules[pattern] = rule
			changed = true
			logx.Infof("[killswitch] 规则更新: %s deny=%v reason=%q",
				pattern, rule.Deny, rule.Reason)

		case clientv3.EventTypeDelete:
			pattern, ok := PatternFromKey(s.cfg.prefix(), key)
			if !ok {
				continue
			}
			if _, exists := rules[pattern]; exists {
				delete(rules, pattern)
				changed = true
				logx.Infof("[killswitch] 规则移除: %s", pattern)
			}
		}
	}
	return changed
}

// parseKV 把一个 etcd 键值对翻成 (匹配模式, 规则)。
// 任一环节不可信就返回 false,整条丢弃 —— fail-open。
func (s *Switch) parseKV(key string, value []byte) (string, Rule, bool) {
	pattern, ok := PatternFromKey(s.cfg.prefix(), key)
	if !ok {
		return "", Rule{}, false
	}
	rule, ok := ParseRule(value)
	if !ok {
		logx.Errorf("[killswitch] 规则值无法解析,已忽略: key=%s value=%q", key, string(value))
		return "", Rule{}, false
	}
	return pattern, rule, true
}
