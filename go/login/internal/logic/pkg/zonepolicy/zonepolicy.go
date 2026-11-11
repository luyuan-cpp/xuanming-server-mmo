// Package zonepolicy 读本区的「区服策略」:区容量(登录排队的准入上限)与是否暂停创建新角色。
//
// 权威来源只有一处:Java 网关的区服目录 zone_config(运维经 /admin/zones 改 capacity 列与 create_role_closed 列)。
// 本区的网关副本(gateway.home-zone-ids)把这一行的两个字段写到 etcd 的 Key(zoneID),内容变了才写
// (java/gateway_node 的 ZonePolicyPublisher);本区 login 定时点读这一个键,在内存里留最后一次读到的值。
// 只读本区一个键,读量随 login 副本数线性增长,与 zone 总数无关。
//
// 读不到时的口径(fail-open,理由见各条):
//   - 键不存在:网关还没发布过,或区服目录里没有这一区。Current 回 ok=false:排队容量退回软上限(仍按 gate 承载封顶),
//     建角不拦。区服目录里没有这一区时,网关的 assign-gate 回 404,玩家本来就到不了 login。
//   - etcd 读失败、值不合法:保留上一次读到的值。策略是持久配置,不因一次读失败而改变;起服后一次都没读到时同上一条。
//
// 不做 watch:只有一个键、秒级生效就够,定时点读的代码最少,也不多占一条 watch 流。
package zonepolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// KeyPrefix 是区服策略在 etcd 里的前缀,后接十进制区号。网关的 ZonePolicyPublisher.KEY_PREFIX 必须与它逐字相同
// (Java 侧单测从本文件读这个常量核对)。
const KeyPrefix = "/mmorpg/zone_policy/zone/"

// Key 返回 zoneID 的区服策略键。
func Key(zoneID uint32) string {
	return KeyPrefix + strconv.FormatUint(uint64(zoneID), 10)
}

// Policy 是网关写进 etcd 的值(JSON)。字段名是两种语言之间的契约:testdata/policy.golden.json 是金样,
// 本包与网关的 ZonePolicyPublisherTest 都拿它核对。只做加法演进,不改名、不改类型。
type Policy struct {
	// Capacity 是区服目录的 capacity 列:本区同时在线的准入上限。0 = 目录没给(按没配处理)。
	Capacity uint32 `json:"capacity"`
	// CreateRoleClosed 是区服目录的 create_role_closed 列:true 时本区暂停创建新角色,已有角色照常登录进场。
	CreateRoleClosed bool `json:"createRoleClosed"`
}

// Parse 解析网关写的值。缺字段按零值(加法演进时旧值仍可读);不是 JSON 对象、类型不对(例如容量为负或超出 uint32)报错。
func Parse(raw []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, fmt.Errorf("zone policy %q: %w", raw, err)
	}
	return p, nil
}

// Getter 读 etcd 里的一个键;found=false 表示键不存在。生产实现在 svc(clientv3),单测用假实现。
type Getter func(ctx context.Context, key string) (value []byte, found bool, err error)

// Outcome 是一次 Refresh 的结果种类,也是指标 login_zone_policy_refresh_total 的 outcome 标签(封闭集合)。
type Outcome string

const (
	OutcomeOK      Outcome = "ok"      // 读到合法的值
	OutcomeMissing Outcome = "missing" // 键不存在:清掉内存里的值
	OutcomeInvalid Outcome = "invalid" // 值解析失败:保留上一次的值
	OutcomeError   Outcome = "error"   // etcd 读失败:保留上一次的值
)

// Reader 持有本区最后一次读到的区服策略。Current 可被任意 goroutine 并发调用;Refresh / Run 只由一个 goroutine 驱动。
type Reader struct {
	zoneID  uint32
	key     string
	get     Getter
	current atomic.Pointer[Policy] // nil = 没有可用的值
}

// NewReader 建一个只读 zoneID 那一个键的读者。get 不能为 nil。
func NewReader(zoneID uint32, get Getter) *Reader {
	if get == nil {
		panic("zonepolicy: nil Getter")
	}
	return &Reader{zoneID: zoneID, key: Key(zoneID), get: get}
}

// ZoneID 返回本读者负责的区号;nil 读者回 0。
func (r *Reader) ZoneID() uint32 {
	if r == nil {
		return 0
	}
	return r.zoneID
}

// Current 返回最后一次读到的策略;ok=false 表示一次都没读到,或者最近一次读到的是「键不存在」。nil 读者回 ok=false。
func (r *Reader) Current() (Policy, bool) {
	if r == nil {
		return Policy{}, false
	}
	p := r.current.Load()
	if p == nil {
		return Policy{}, false
	}
	return *p, true
}

// Refresh 读一次 etcd 并按 Outcome 更新 Current。err 只在 OutcomeInvalid / OutcomeError 时非 nil。
func (r *Reader) Refresh(ctx context.Context) (Outcome, error) {
	raw, found, err := r.get(ctx, r.key)
	if err != nil {
		return OutcomeError, fmt.Errorf("etcd get %s: %w", r.key, err)
	}
	if !found {
		r.current.Store(nil)
		return OutcomeMissing, nil
	}
	p, err := Parse(raw)
	if err != nil {
		return OutcomeInvalid, err
	}
	r.current.Store(&p)
	return OutcomeOK, nil
}

// errorLogEvery:读失败、值不合法两类日志的最小间隔。每 interval 读一次,etcd 不可用时不节流就是每 2 秒一条。
const errorLogEvery = time.Minute

// Run 立刻读一次,之后每 interval 读一次,直到 ctx 结束。策略变化(含首次读到、变成不存在)打一条 INFO,
// 读失败与值不合法按 errorLogEvery 节流打 ERROR。
func (r *Reader) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var (
		last         *Policy // 上一次记下的状态,nil = 没有可用的值
		known        bool    // last 是否已经有过一次确定结果(区分「起服还没读到」与「键不存在」)
		lastErrorLog time.Time
	)
	for {
		outcome, err := r.Refresh(ctx)
		refreshTotal.Inc(string(outcome))
		switch outcome {
		case OutcomeOK, OutcomeMissing:
			cur, ok := r.Current()
			if changed(known, last, cur, ok) {
				logChange(r.zoneID, r.key, last, cur, ok)
			}
			if ok {
				last = &cur
			} else {
				last = nil
			}
			known = true
		default:
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return
			}
			if time.Since(lastErrorLog) >= errorLogEvery {
				lastErrorLog = time.Now()
				logx.Errorf("[ZonePolicy] zone %d: reading %s failed (%s), keeping the last known policy: %v; "+
					"further reports are suppressed for %s, see login_zone_policy_refresh_total{outcome=%q}",
					r.zoneID, r.key, outcome, err, errorLogEvery, outcome)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// changed 报告这次读到的状态是否与上一次记下的不同;起服后的第一次确定结果一律算变化,好把初值打进日志。
func changed(known bool, last *Policy, cur Policy, ok bool) bool {
	if !known {
		return true
	}
	if (last != nil) != ok {
		return true
	}
	return ok && *last != cur
}

func logChange(zoneID uint32, key string, last *Policy, cur Policy, ok bool) {
	if !ok {
		logx.Errorf("[ZonePolicy] zone %d has no policy at %s: the login queue falls back to the soft cap (still capped by "+
			"the gates) and character creation is not restricted; check that this zone's gateway publishes zone_config",
			zoneID, key)
		return
	}
	if last == nil {
		logx.Infof("[ZonePolicy] zone %d policy loaded: capacity=%d createRoleClosed=%v", zoneID, cur.Capacity, cur.CreateRoleClosed)
		return
	}
	logx.Infof("[ZonePolicy] zone %d policy changed: capacity=%d createRoleClosed=%v (was capacity=%d createRoleClosed=%v)",
		zoneID, cur.Capacity, cur.CreateRoleClosed, last.Capacity, last.CreateRoleClosed)
}
