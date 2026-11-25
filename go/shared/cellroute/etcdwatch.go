package cellroute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/etcdwatch"
	"shared/safego"
)

const watchReconnectBackoff = 2 * time.Second

// BuildRouter constructs an optional Router from Conf.
// Returns (nil, nil, nil) when cfg.Mode == "".
func BuildRouter(ctx context.Context, cfg Conf, etcdClient *clientv3.Client) (*Router, func(), error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	switch cfg.Mode {
	case "":
		return nil, nil, nil
	case "static":
		entries, regions, err := BuildBalancedEntries(cfg.Cells)
		if err != nil {
			return nil, nil, err
		}
		tbl, err := NewStaticTable(entries, regions)
		if err != nil {
			return nil, nil, err
		}
		r, err := NewRouter(tbl)
		return r, nil, err
	case "etcd":
		if etcdClient == nil {
			return nil, nil, errors.New("cellroute: etcd mode requires non-nil etcd client")
		}
		initial, rev, err := loadInitialFromEtcd(ctx, etcdClient, cfg)
		if err != nil {
			return nil, nil, err
		}
		at, err := NewAtomicTable(initial)
		if err != nil {
			return nil, nil, err
		}
		r, err := NewRouter(at)
		if err != nil {
			return nil, nil, err
		}
		stop := WatchEtcd(context.Background(), etcdClient, cfg.EtcdKey, at, rev, cfg.ResyncInterval)
		return r, stop, nil
	default:
		return nil, nil, fmt.Errorf("%w: unsupported Mode %q", ErrInvalidConfig, cfg.Mode)
	}
}

func loadInitialFromEtcd(ctx context.Context, cli *clientv3.Client, cfg Conf) (*StaticTable, int64, error) {
	resp, err := cli.Get(ctx, cfg.EtcdKey)
	if err == nil && len(resp.Kvs) > 0 {
		tbl, _, perr := ParseTableJSON(resp.Kvs[0].Value)
		if perr != nil {
			return nil, 0, fmt.Errorf("cellroute: initial etcd key %s invalid: %w", cfg.EtcdKey, perr)
		}
		return tbl, resp.Header.Revision, nil
	}
	if len(cfg.Cells) > 0 {
		entries, regions, berr := BuildBalancedEntries(cfg.Cells)
		if berr != nil {
			return nil, 0, berr
		}
		tbl, serr := NewStaticTable(entries, regions)
		if serr != nil {
			return nil, 0, serr
		}
		var rev int64
		if resp != nil {
			rev = resp.Header.Revision
		}
		return tbl, rev, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("cellroute: failed to load initial etcd key %s: %w", cfg.EtcdKey, err)
	}
	return nil, 0, fmt.Errorf("cellroute: initial etcd key %s not found and no seed Cells configured", cfg.EtcdKey)
}

// WatchEtcd keeps target updated from etcdKey using shared/etcdwatch (RequireLeader + resync).
// Malformed payloads are rejected fail-closed while preserving the last valid table.
func WatchEtcd(parent context.Context, cli *clientv3.Client, key string, target *AtomicTable, startRev int64, resyncInterval time.Duration) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	safego.Go("cellroute.etcd_watch", func() {
		defer close(done)
		rev := startRev
		for {
			if ctx.Err() != nil {
				return
			}
			var opts []clientv3.OpOption
			if rev > 0 {
				opts = append(opts, clientv3.WithRev(rev+1))
			}
			wch, resyncCh, stopWatch := etcdwatch.Open(ctx, cli, resyncInterval, key, opts...)
			needResync := false
			for !needResync {
				select {
				case <-ctx.Done():
					stopWatch()
					return
				case <-resyncCh:
					needResync = true
				case wr, ok := <-wch:
					if !ok || wr.Canceled {
						if wr.Err() != nil {
							logx.Errorf("[cellroute] watch canceled on key=%s: %v", key, wr.Err())
						}
						needResync = true
						break
					}
					if wr.Header.Revision > rev {
						rev = wr.Header.Revision
					}
					for _, ev := range wr.Events {
						if ev.Type != clientv3.EventTypePut {
							continue
						}
						applyPayload(key, ev.Kv.Value, target)
					}
				}
			}
			stopWatch()

			// Periodic full resync or reconnect recovery.
			getCtx, getCancel := context.WithTimeout(ctx, 5*time.Second)
			resp, gerr := cli.Get(getCtx, key)
			getCancel()
			if gerr != nil {
				logx.Errorf("[cellroute] resync get failed on key=%s: %v", key, gerr)
				timer := time.NewTimer(watchReconnectBackoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			if resp.Header.Revision > rev {
				rev = resp.Header.Revision
			}
			if len(resp.Kvs) > 0 {
				applyPayload(key, resp.Kvs[0].Value, target)
			}
		}
	})
	return func() {
		cancel()
		<-done
	}
}

func applyPayload(key string, raw []byte, target *AtomicTable) {
	tbl, version, err := ParseTableJSON(raw)
	if err != nil {
		logx.Errorf("[cellroute] rejected invalid routing table update on key=%s (keeping previous table): %v", key, err)
		return
	}
	if err := target.Swap(tbl); err != nil {
		logx.Errorf("[cellroute] failed to swap routing table on key=%s: %v", key, err)
		return
	}
	logx.Infof("[cellroute] reloaded routing table on key=%s version=%d", key, version)
}
