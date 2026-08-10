// internal/etcd/registry.go
package etcd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/safego"
)

type NodeRegistry struct {
	client *clientv3.Client
	Lease  clientv3.LeaseID
	ttl    int64

	// registeredKeys tracks key-value pairs for re-registration after lease loss.
	registeredKeys map[string]string
}

func NewNodeRegistry(client *clientv3.Client, ttl int64) (*NodeRegistry, error) {
	resp, err := client.Grant(context.Background(), ttl)
	if err != nil {
		return nil, fmt.Errorf("failed to grant Lease: %v", err)
	}

	return &NodeRegistry{
		client:         client,
		Lease:          resp.ID,
		ttl:            ttl,
		registeredKeys: make(map[string]string),
	}, nil
}

func (r *NodeRegistry) RegisterNode(key string, value string) error {
	_, err := r.client.Put(context.Background(), key, value, clientv3.WithLease(r.Lease))
	if err != nil {
		return fmt.Errorf("failed to register node: %v", err)
	}
	r.registeredKeys[key] = value
	return nil
}

// TrackKey 登记一个**已经由别处写进 etcd**的 key,只记进重注册清单,不重复写。
//
// 为什么需要它:login 的 allocKey / infoKey 是 NodeAllocator 用一次 Txn 直接 CAS 写的
// (那次 CAS 本身就是 node_id 唯一性的闸,不能拆成两次 Put),因此绕过了 RegisterNode。
// 结果是 registeredKeys 恒空,reRegister 遍历 0 个 key、却打印 "completed successfully" ——
// 失租后本节点从服务发现里永久消失,而 gRPC 端口仍在、livenessProbe 仍过,k8s 不会重启它,
// 日志里还写着成功。TrackKey 就是把这两个 key 补登记进来,让重注册真的有东西可重放。
func (r *NodeRegistry) TrackKey(key, value string) {
	r.registeredKeys[key] = value
}

func (r *NodeRegistry) KeepAlive(ctx context.Context) {
	ch, err := r.client.KeepAlive(ctx, r.Lease)
	if err != nil {
		logx.Errorf("Failed to keep alive Lease: %v", err)
		return
	}

	// safego:这条 goroutine 一旦 panic,旧写法会直接打死进程,而且日志里
	// 只剩一段 runtime 栈看不出是租约续期炸的。
	safego.Go("login.etcd_lease_keepalive", func() {
		for {
			select {
			case ka := <-ch:
				if ka == nil {
					logx.Error("Lease keep alive channel closed, attempting re-registration")
					r.reRegister(ctx)
					return
				}
				logx.Debugf("Lease TTL: %d", ka.TTL)
			case <-ctx.Done():
				logx.Info("KeepAlive context canceled")
				return
			}
		}
	})
}

// reRegister grants a new lease, re-puts all registered keys, and restarts KeepAlive.
func (r *NodeRegistry) reRegister(ctx context.Context) {
	const maxRetries = 0 // unlimited
	backoff := time.Second

	for attempt := 1; maxRetries == 0 || attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			logx.Info("Re-registration aborted: context canceled")
			return
		default:
		}

		logx.Infof("Re-registration attempt %d: granting new lease (ttl=%d)", attempt, r.ttl)

		resp, err := r.client.Grant(ctx, r.ttl)
		if err != nil {
			logx.Errorf("Re-registration: failed to grant lease: %v, retrying in %v", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		r.Lease = resp.ID
		logx.Infof("Re-registration: new lease granted, id=%d", r.Lease)

		// 没有任何待重放的 key,说明登记链断了(TrackKey 没被调到)。
		// 这时候什么都不做却打印"成功"是最坏的结果:节点已经从服务发现里消失,
		// 而所有人都以为它恢复了。fail-closed 报错退出,交给编排重拉重新注册。
		if len(r.registeredKeys) == 0 {
			logx.Error("Re-registration: nothing to re-register — the node is gone from service discovery " +
				"and cannot recover in-process; exiting to let the orchestrator restart it")
			logx.Close()
			os.Exit(1)
		}

		allOk := true
		for key, value := range r.registeredKeys {
			// **不能无条件 Put**:租约过期期间别的副本可能已经抢走了这个 node_id。
			// 无条件覆盖会把别人的槽位改成自己,制造真正的双占(两个进程同一个 node_id)。
			// 只在"key 不存在"或"还是自己写的那份"时才重新挂上新租约。
			txnResp, err := r.client.Txn(ctx).
				If(clientv3.Compare(clientv3.Version(key), "=", 0)).
				Then(clientv3.OpPut(key, value, clientv3.WithLease(r.Lease))).
				Else(clientv3.OpGet(key)).
				Commit()
			if err != nil {
				logx.Errorf("Re-registration: failed to re-put key=%s: %v", key, err)
				allOk = false
				break
			}
			if !txnResp.Succeeded {
				// key 还在。是自己的残留就改挂新租约,是别人的就必须退让。
				existing := txnResp.Responses[0].GetResponseRange()
				if len(existing.Kvs) == 1 && string(existing.Kvs[0].Value) == value {
					if _, perr := r.client.Put(ctx, key, value, clientv3.WithLease(r.Lease)); perr != nil {
						logx.Errorf("Re-registration: failed to re-attach own key=%s: %v", key, perr)
						allOk = false
						break
					}
				} else {
					logx.Errorf("Re-registration: key=%s is now held by someone else; this node_id was taken over. "+
						"Exiting so the orchestrator restarts us and we allocate a fresh one.", key)
					logx.Close()
					os.Exit(1)
				}
			}
			logx.Infof("Re-registration: key=%s re-registered with new lease", key)
		}

		if !allOk {
			logx.Errorf("Re-registration: partial failure, retrying in %v", backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		// Restart KeepAlive with the new lease
		r.KeepAlive(ctx)
		logx.Info("Re-registration: completed successfully")
		return
	}

	logx.Error("Re-registration: exhausted retries, node is no longer registered")
}

// RevokeLease revokes the etcd lease.
func (r *NodeRegistry) RevokeLease() error {
	_, err := r.client.Revoke(context.Background(), r.Lease)
	if err != nil {
		return fmt.Errorf("failed to revoke lease: %v", err)
	}
	logx.Info("Lease revoked successfully")
	return nil
}
