package svc

// 合服映射的 etcd 副本(docs/design/merged-zone-etcd-copy.md):data_service 这一侧的读法。
//
// 真源仍是目录第 0 片的 zone_merged_into:只有 tools/merge_zone 写它,和 zone_merge_history 同一个事务,提交之后它也发布副本。
// data_service 每读到一个新版本,先把它发布进 etcd 里的副本,再交给合服映射缓存;第 0 片读不到时按副本回答。
// 所以副本总是各写者读到过的最高版本:读不到第 0 片的新副本从它起,不会比任何在跑的副本旧(设计稿 §5)。
// 键、编码、读第 0 片的语句和发布判定都在 shared/mergedzonecopy,和 merge_zone 共用。
//
// 第 0 片、副本、本进程已回答的那一份,三者之间对不上(世代不同、版本变小、同版本内容不同)一律拒绝:谁对拿不准。

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/authoritysql"
	"shared/dirshard"
	"shared/etcdwatch"
	"shared/mergedzonecopy"
	"shared/placement"
)

// mergedZoneCopyEtcdDialTimeout 与节点注册、热关停的 etcd 客户端同值。
const mergedZoneCopyEtcdDialTimeout = 5 * time.Second

// mergedZoneTruth 是真源:目录第 0 片。*store.RouteStore 逐字满足,单测注入假实现。
type mergedZoneTruth interface {
	ReadMergedZoneMapping(ctx context.Context) (store.MergedZoneMapping, error)
}

// mergedZoneSource 是合服映射缓存(placement.MergedZoneCache)背后的读数:每一拍按设计稿 §4.2 先读副本、再读第 0 片、
// 该发布的先发布,再回答。只由缓存的启动读与定时任务调用(同一时刻至多一个),mu 只是兜底。
type mergedZoneSource struct {
	truth  mergedZoneTruth
	copies mergedzonecopy.Copies

	mu sync.Mutex
	// answered 是上一次交给缓存的那一份(世代、版本和映射);nil = 还没回答过,也就是启动那一次。
	answered *store.MergedZoneMapping
	// fromCopy 是上一次是不是按副本回答的,只为回到第 0 片时打一行日志。
	fromCopy bool
}

// read 是缓存的读数函数(placement.MergedZoneReader)。回错误 = 这一拍失败:启动时拒启,运行中保留旧映射并计 error。
func (s *mergedZoneSource) read(ctx context.Context) (map[uint32]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.copies.Key()
	for attempt := 1; ; attempt++ {
		// 先读副本、后读第 0 片(mergedzonecopy.Decide 的前提):副本里只有第 0 片已经提交过的版本,这样读到「第 0 片落后于副本」
		// 只可能是第 0 片回档了,不会是两次读之间夹了一次合服。
		published, revision, found, err := s.copies.Get(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the etcd copy %s of the merged zone mapping: %w", key, err)
		}
		truth, truthErr := s.truth.ReadMergedZoneMapping(ctx)
		if truthErr != nil {
			return s.answerFromCopy(published, found, truthErr)
		}
		publish, err := s.checkTruth(truth, published, found)
		if err != nil {
			return nil, err
		}
		if publish {
			written, err := s.copies.Put(ctx, revision, truth)
			if err != nil {
				return nil, fmt.Errorf("publish version %d of the merged zone mapping to the etcd copy %s (outcome unknown, read again next time): %w",
					truth.Version, key, err)
			}
			if !written {
				if attempt < mergedzonecopy.PublishAttempts {
					continue // 别的写者刚发布过:重读再比
				}
				return nil, fmt.Errorf("the etcd copy %s of the merged zone mapping changed under each of %d publish attempts", key, attempt)
			}
		}
		if s.fromCopy {
			logx.Infof("[MergedZoneCache] directory shard 0 is readable again at version %d of the merged zone mapping", truth.Version)
		}
		return s.answer(truth, false), nil
	}
}

// checkTruth 是设计稿 §4.3 判定表的上半张:第 0 片读到了 truth。publish=true 要先发布再回答;回错误 = 对不上,这一拍不回答。
// 发布判定和 merge_zone 共用(mergedzonecopy.Decide,含坏映射不发布);这里多一条进程内的 I3。
func (s *mergedZoneSource) checkTruth(truth, published store.MergedZoneMapping, found bool) (bool, error) {
	if err := s.checkAgainstAnswered(truth, "directory shard 0"); err != nil {
		return false, fmt.Errorf("%w: restart this replica", err)
	}
	return mergedzonecopy.Decide(s.copies.Key(), published, found, truth)
}

// answerFromCopy 是判定表的下半张:第 0 片读不到,决定用不用副本。照旧回答已有的那一份也算失败,
// 让刷新失败的告警照常响:这个副本确认不了真源。
func (s *mergedZoneSource) answerFromCopy(published store.MergedZoneMapping, found bool, truthErr error) (map[uint32]uint32, error) {
	key := s.copies.Key()
	if s.answered == nil && !shardZeroUnreachable(truthErr) {
		// 启动时只有「连不上」才从副本起,和按需打开的池同一口径(docs/design/data-service-startup-isolation.md §5):
		// 连上了却读不对(权限、缺表、语句错)是确定的部署错误,照旧拒启,滚动发布停在第一个副本。
		return nil, fmt.Errorf("directory shard 0 failed with an error that is not a connectivity failure, so this process does not start from the etcd copy %s: %w",
			key, truthErr)
	}
	if !found {
		return nil, fmt.Errorf("directory shard 0 is unreadable and etcd holds no copy %s of the merged zone mapping yet: %w", key, truthErr)
	}
	if _, err := placement.NewMergedZones(published.Merges); err != nil {
		// 只发布过校验通过的映射:走到这里说明副本被别人改过。
		return nil, fmt.Errorf("directory shard 0 is unreadable and the etcd copy %s is broken at version %d (%v): %w",
			key, published.Version, err, truthErr)
	}
	if s.answered == nil {
		logx.Errorf("[MergedZoneCache] directory shard 0 is unreadable at startup (%v); starting from version %d in the etcd copy %s",
			truthErr, published.Version, key)
		return s.answer(published, true), nil
	}
	if err := s.checkAgainstAnswered(published, "the etcd copy "+key); err != nil {
		return nil, fmt.Errorf("directory shard 0 is unreadable and %w: %w", err, truthErr)
	}
	if published.Version == s.answered.Version {
		return nil, fmt.Errorf("directory shard 0 is unreadable; keeping version %d of the merged zone mapping, which is also the newest in the etcd copy %s: %w",
			s.answered.Version, key, truthErr)
	}
	logx.Errorf("[MergedZoneCache] directory shard 0 is unreadable (%v); adopting version %d from the etcd copy %s",
		truthErr, published.Version, key)
	return s.answer(published, true), nil
}

// checkAgainstAnswered 守住一个进程之内的两条:回答的版本只增不减,同一个版本只对应一份映射;世代变了(第 0 片换了一个库)也拒绝。
// 这三种只会在回档、删过副本键或者指错库之后出现:这个进程手里的那一份和读到的不是同一条历史,谁对拿不准。source 只进错误文案。
func (s *mergedZoneSource) checkAgainstAnswered(next store.MergedZoneMapping, source string) error {
	prev := s.answered
	switch {
	case prev == nil:
		return nil
	case next.Generation != prev.Generation:
		return fmt.Errorf("%s is directory generation %s, but this process answers from generation %s",
			source, next.Generation, prev.Generation)
	case next.Version < prev.Version:
		return fmt.Errorf("%s is at version %d of the merged zone mapping, behind version %d that this process already answers with "+
			"(after a restore, or the etcd copy was deleted while this replica ran)", source, next.Version, prev.Version)
	case next.Version == prev.Version && !maps.Equal(next.Merges, prev.Merges):
		return fmt.Errorf("%s holds version %d of the merged zone mapping with contents different from what this process answers with",
			source, next.Version)
	default:
		return nil
	}
}

// shardZeroUnreachable 报告第 0 片的错误是不是「连不上」:池启动时就连不上、至今关着(authoritysql.ErrUnreachable),
// 或者这一代没有它的写资格、池一直关着(store.ErrNoWriterGrant)。
func shardZeroUnreachable(err error) bool {
	return errors.Is(err, authoritysql.ErrUnreachable) || errors.Is(err, store.ErrNoWriterGrant)
}

// answer 记下交给缓存的那一份,回映射。
func (s *mergedZoneSource) answer(mapping store.MergedZoneMapping, fromCopy bool) map[uint32]uint32 {
	s.answered = &store.MergedZoneMapping{Generation: mapping.Generation, Version: mapping.Version, Merges: maps.Clone(mapping.Merges)}
	s.fromCopy = fromCopy
	return mapping.Merges
}

// openMergedZoneCopies 建副本的 etcd 客户端,用 RpcServerConf.Etcd:就是节点注册那一套 etcd,那条注册失败本来就拒启,
// 所以不多一个启动依赖。副本键按目录第 0 片的库名(和 merge_zone 的 -route-db-dsn 指的是同一个库)。
// clientv3.New 不带 WithBlock,这里不拨号;连不通要到启动读副本时才暴露,那一读失败、拒启。客户端归调用方关。
func openMergedZoneCopies(c config.Config) (*clientv3.Client, *mergedzonecopy.EtcdCopies, error) {
	if len(c.Etcd.Hosts) == 0 {
		return nil, nil, errors.New("Etcd.Hosts is empty: the etcd copy of the merged zone mapping lives in the etcd that the node registration uses")
	}
	if c.SnapshotMySQL.DBName == "" || c.Directory.Shards() < 1 {
		return nil, nil, errors.New("SnapshotMySQL.DBName or Directory.LogicalShards is missing: the etcd copy of the merged zone mapping is keyed by the name of directory shard 0")
	}
	cfg := clientv3.Config{Endpoints: c.Etcd.Hosts, DialTimeout: mergedZoneCopyEtcdDialTimeout}
	if c.Etcd.HasAccount() {
		cfg.Username, cfg.Password = c.Etcd.User, c.Etcd.Pass
	}
	// keepalive:半开连接尽快换成员重连,不至于每一拍都等满时限(etcdwatch 包注释)。
	client, err := clientv3.New(etcdwatch.WithKeepAlive(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("create the etcd client for the merged zone copy: %w", err)
	}
	shardZero := dirshard.DirectoryDBName(c.SnapshotMySQL.DBName, 0, c.Directory.Shards())
	return client, mergedzonecopy.NewEtcdCopies(client, shardZero), nil
}
