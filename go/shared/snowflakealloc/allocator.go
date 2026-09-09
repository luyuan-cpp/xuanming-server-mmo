// Package snowflakealloc 提供基于 etcd 的 Snowflake 槽位(worker id)分配器。
//
// 与服务发现层的 NodeInfo.NodeId 分配解耦:
//   - NodeInfo.NodeId 由 node allocator(go/<service>/internal/node) 管理,
//     用于 C++ 端服务发现路由。
//   - Snowflake 槽位由本包管理,用于 ID 生成器的 worker 段。
//
// 2026-09-08 改造(docs/design/node-id-overhaul-plan-20260908.md §3 / §5),核心三条:
//
//  1. **lease 只做活性,不拿 lease 证明唯一性。** 唯一性由三样东西保证:
//     持久水位 wm[slot](持有者每秒写,无 lease,既是继任者的地板也是墓碑)、
//     隔离期 Q(申领者跳过 now − wm < Q 的槽)、自 fence 期限 F = Q/2(持有者距上次
//     水位写成功超过 F 就停发)。不等式 `T_ack + F < wm + Q` 让前任与继任者永不同时发号。
//     TTL 从此只是活性参数,60 还是 180 不再影响 ID 正确性。
//  2. **选号是"最久未用 + 隔离期",不是"最小空闲"。** 见 selection.go。
//  3. **etcd 是弱依赖。** 水位写失败只记账;lease 丢了先重新挂回去(reclaim),只有
//     "槽被别人挂到了另一个 uuid"才算真的失去所有权;启动时 etcd 不通可以用本地缓存起。
//
// etcd key 协议(Go / C++ 共用):
//
//	/snowflake/<kind>/c<cluster>/slots/<slot>        = <holder uuid>   挂 lease(只表活性)
//	/snowflake/<kind>/c<cluster>/affinity/<host>     = <slot>          挂 lease(同主机优雅重启复用)
//	/snowflake/<kind>/c<cluster>/released/<host>     = "released"      挂 lease(前任已 fence 并优雅退出)
//	/snowflake/<kind>/c<cluster>/watermark/<slot>    = <epochSec>      **不挂 lease**,guard + 墓碑
//	/snowflake/<kind>/c<cluster>/watermark_ms/<slot> = <unixMs>        不挂 lease,login 的毫秒地板
//
// worker id = (cluster << SlotBits) | slot(layout.go);发号器只认合成后的 worker id,
// 对 cluster / slot 的切分一无所知。
//
// 用法:
//
//	h, err := snowflakealloc.AllocateWithKeepAlive(ctx, cli, "guild", hostname,
//	    snowflakealloc.Options{LeaseTTL: 60, ClusterID: cfg.ClusterId, LegacyPrefix: "/guild"})
//	sf := h.NewNode()
//	go func() { <-h.Lost(); sf.Fence(); os.Exit(1) }()
//
// **kind 必须按服务区分**,否则两个服务的槽池会互相干扰。
package snowflakealloc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/snowflake"
)

// ErrNoSlotAvailable 表示候选集为空:所有槽要么被活着的持有者占着,要么还在隔离期内。
// fail-closed —— 4096 个槽(login 1024)在现实中不可能耗尽,除非一小时内重启上千次。
var ErrNoSlotAvailable = errors.New("snowflakealloc: no slot available (all held or quarantined)")

// ErrInvalidOptions 表示 Options 自相矛盾(F ≥ Q、cluster 越界……),启动前就该发现。
var ErrInvalidOptions = errors.New("snowflakealloc: invalid options")

// Options 控制分配器行为。零值字段取默认。
type Options struct {
	// LeaseTTL 是 etcd lease 的 TTL(秒)。0 时默认 60。只影响服务发现多快摘掉死节点,
	// **不再影响 ID 正确性**(见包注释)。
	LeaseTTL int64

	// ClusterID 是部署级集群号(运维在 ConfigMap / env 一次性设定,策划不碰),默认 0。
	// 必须 < 1<<ClusterBits。
	ClusterID uint32

	// ClusterBits / SlotBits 是 worker 段的切分(见 layout.go)。0 时取本包默认的
	// ClusterBits / SlotBits(5 / 12)。login 的 bwmarrin 13 位 node 段传
	// PlayerIDClusterBits / PlayerIDSlotBits(3 / 10)。
	ClusterBits uint
	SlotBits    uint

	// MaxSlot 是槽号上限(含)。0 时默认 (1<<SlotBits)-1。只能调小不能调大。
	MaxSlot uint64

	// Quarantine 是隔离期 Q:一个槽的水位距今不足 Q 就不许被申领。0 时默认 4h。
	// 下界推导:Q ≥ 2 × (TTL_max 180s + 最大时钟偏差 + drain 预算 15s + 冻结感知延迟)。
	Quarantine time.Duration

	// FenceAfter 是自 fence 期限 F:距上次水位写成功超过 F,发号器拒发。0 时默认 2h。
	// 必须 < Quarantine(通常取 Q/2)。
	FenceAfter time.Duration

	// CachePath 是本地缓存文件(见 cache.go)。空 = 关闭。消费方用 DefaultCachePath 生成。
	// 写盘策略是固定的(设计稿 §7.5-5,用户拍板):申领 / reclaim 后的第一次 Ack 写一次,
	// Close() 写一次,etcd 水位写失败期间每拍写;稳态**一次都不写**,没有周期刷新可配。
	CachePath string

	// LegacyPrefix 是改造前的 etcd 前缀("/guild" / "/login" / "/match" / "/scene_manager")。
	// 非空且 ClusterID==0 时,申领会额外读旧布局的水位(snowflake_guard / guard_ms)取 max,
	// 并把旧布局仍活着的 worker id(snowflake_ids / snowflake_nodes)算作已占 —— 覆盖
	// 滚动升级期新老版本并存的窗口。**发布一版之后可以删掉这条兼容读**。
	LegacyPrefix string
}

// resolvedOptions 是校验并填好默认值后的 Options。
type resolvedOptions struct {
	ttl         int64
	cluster     uint32
	clusterBits uint
	slotBits    uint
	maxSlot     uint64
	quarantine  time.Duration
	fenceAfter  time.Duration
	cachePath   string
	legacy      string
}

const (
	defaultLeaseTTL   = 60
	defaultQuarantine = 4 * time.Hour
	defaultFenceAfter = 2 * time.Hour
)

func (o Options) resolve() (resolvedOptions, error) {
	r := resolvedOptions{
		ttl:         o.LeaseTTL,
		cluster:     o.ClusterID,
		clusterBits: o.ClusterBits,
		slotBits:    o.SlotBits,
		maxSlot:     o.MaxSlot,
		quarantine:  o.Quarantine,
		fenceAfter:  o.FenceAfter,
		cachePath:   o.CachePath,
		legacy:      o.LegacyPrefix,
	}
	if r.ttl <= 0 {
		r.ttl = defaultLeaseTTL
	}
	if r.clusterBits == 0 {
		r.clusterBits = ClusterBits
	}
	if r.slotBits == 0 {
		r.slotBits = SlotBits
	}
	if r.clusterBits+r.slotBits > 63 {
		return r, fmt.Errorf("%w: cluster%d+slot%d bits too wide", ErrInvalidOptions, r.clusterBits, r.slotBits)
	}
	if uint64(r.cluster) > (uint64(1)<<r.clusterBits)-1 {
		return r, fmt.Errorf("%w: ClusterID %d exceeds %d bits", ErrInvalidOptions, r.cluster, r.clusterBits)
	}
	slotCap := (uint64(1) << r.slotBits) - 1
	if r.maxSlot == 0 {
		r.maxSlot = slotCap
	}
	if r.maxSlot > slotCap {
		return r, fmt.Errorf("%w: MaxSlot %d exceeds %d bits", ErrInvalidOptions, r.maxSlot, r.slotBits)
	}
	if r.quarantine <= 0 {
		r.quarantine = defaultQuarantine
	}
	if r.fenceAfter <= 0 {
		r.fenceAfter = defaultFenceAfter
	}
	// F ≤ Q/2 —— 不是"F < Q"就够。Q − F 是整套协议的安全余量:前任最晚停发时刻
	// (T_ack + F)必须早于继任者最早申领时刻(wm + Q),中间那段 Q − F 要吃掉
	// 时钟偏差、优雅排空、以及"冻结的进程多久才感知到自己被冻结"。设计稿 §1.2 直接
	// 把它定成 F = Q/2(4h / 2h)。F = 0.9Q 这种配置在数学上仍满足 F < Q,却把余量
	// 压到几乎为零 —— 拒掉,别让它悄悄进配置文件。
	if r.fenceAfter > r.quarantine/2 {
		return r, fmt.Errorf("%w: FenceAfter %v must be <= Quarantine/2 (%v); Q-F is the margin that absorbs "+
			"clock skew, drain and freeze-detection latency", ErrInvalidOptions, r.fenceAfter, r.quarantine/2)
	}
	return r, nil
}

// legacyMirror 报告是否处于**滚动升级过渡期**:cluster 0 且配了改造前的 etcd 前缀。
//
// 过渡期里灰度的两半各说各话:新二进制读写 /snowflake/<kind>/c0/**,旧二进制只认
// <legacy>/snowflake_ids/ 与 <legacy>/snowflake_guard/。只让新版本"避开旧版本"是
// **单向**的 —— 旧二进制看不见新槽,照样能把同一个 worker id 分出去(见 allocate /
// reclaim / tryReuse 里的镜像写)。所以过渡期两边都要留脚印。
// **发布一版之后这个方法与它所有调用点一起删。**
func (r resolvedOptions) legacyMirror() bool { return r.cluster == 0 && r.legacy != "" }

// ---- key 布局 ---------------------------------------------------------------

func rootKey(kind string, cluster uint32) string {
	return fmt.Sprintf("/snowflake/%s/c%d", kind, cluster)
}

func slotsPrefix(kind string, cluster uint32) string { return rootKey(kind, cluster) + "/slots/" }
func slotKey(kind string, cluster uint32, slot uint64) string {
	return slotsPrefix(kind, cluster) + strconv.FormatUint(slot, 10)
}
func affinityKey(kind string, cluster uint32, host string) string {
	return rootKey(kind, cluster) + "/affinity/" + host
}

// releasedKey 是 Close() 留下的"已 fence 并优雅释放"标记(挂在持有者当前 lease 上)。
// 同主机后继进程**只有看到它**才允许复用原槽;lease 死了但没标记(崩溃)→ 不复用,
// 走正常申领(受隔离期约束)。复用 Txn 必须同时删掉它(设计稿 §2.0b):否则
// A 退出写标记 → A' 复用成功但标记还在 → A” 第二个进程凭标记把活着的 A' 的槽抢走。
func releasedKey(kind string, cluster uint32, host string) string {
	return rootKey(kind, cluster) + "/released/" + host
}

func watermarkPrefix(kind string, cluster uint32) string {
	return rootKey(kind, cluster) + "/watermark/"
}

// watermarkKey 记录某个槽**最近一次发号所在的秒**(自 snowflake.Epoch 起,含前推)。
// 刻意**不挂 lease**:它必须比持有者活得久 —— 下一任要靠它把发号起点抬到前任高水位
// 之上,申领者要靠它判断隔离期。量级是每槽一行,被 MaxSlot 封死,不会无界增长。
func watermarkKey(kind string, cluster uint32, slot uint64) string {
	return watermarkPrefix(kind, cluster) + strconv.FormatUint(slot, 10)
}

// watermarkMsKey 是毫秒级持久水位,给 **bwmarrin 布局**的消费方(login PlayerId)用。
// 与 watermarkKey 的秒级 / snowflake.Epoch 口径完全独立:值统一存 **Unix 毫秒**。
func watermarkMsKey(kind string, cluster uint32, slot uint64) string {
	return rootKey(kind, cluster) + "/watermark_ms/" + strconv.FormatUint(slot, 10)
}

// 旧布局(改造前)的 key,只在 cluster 0 兼容读**与过渡期镜像写**。发布一版之后可删。
func legacyGuardPrefix(prefix string) string   { return prefix + "/snowflake_guard/" }
func legacyGuardMsPrefix(prefix string) string { return prefix + "/guard_ms/" }
func legacyIDPrefix(prefix string) string      { return prefix + "/snowflake_ids/" }
func legacyNodePrefix(prefix string) string    { return prefix + "/snowflake_nodes/" }

// legacyIDKey 是旧二进制的"槽位已占"占位键(<legacy>/snowflake_ids/<id> = <host>)。
// 过渡期新二进制申领成功时把它一并挂到自己的 lease 上:旧二进制的双 key CAS 条件
// 里有 CreateRevision(idKey)==0,这一把 Put 让它直接失败;它的 scanUsedWorkerIDs
// 也会把这个槽算成已占。**过渡专用,发布一版之后删。**
func legacyIDKey(prefix string, slot uint64) string {
	return legacyIDPrefix(prefix) + strconv.FormatUint(slot, 10)
}

// legacyGuardKey / legacyGuardMsKey 是旧二进制读地板用的水位键。过渡期把新水位镜像
// 过去,旧二进制接手这个槽时才有地板可用(它只会去 readGuard 旧 key)。
// **过渡专用,发布一版之后删。**
func legacyGuardKey(prefix string, slot uint64) string {
	return legacyGuardPrefix(prefix) + strconv.FormatUint(slot, 10)
}

func legacyGuardMsKey(prefix string, slot uint64) string {
	return legacyGuardMsPrefix(prefix) + strconv.FormatUint(slot, 10)
}

// newHolderID 生成持有者 uuid(128 bit 随机,hex)。不引 uuid 库:shared 模块里它只是
// 间接依赖,这里 16 字节随机数就够了。
func newHolderID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("snowflakealloc: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ---- 申领 ----------------------------------------------------------------------

// claim 是一次成功申领的结果。
type claim struct {
	lease       clientv3.LeaseID
	slot        uint64
	uuid        string
	incarnation int64 // slots key 的 mod_revision(= Txn 提交的 revision)
	reused      bool
}

// allocate 为 (kind, cluster, host) 申领一个槽。
//
//  1. 拿一个新 lease。
//  2. 同主机亲和复用:**只在** affinity/<host> 与 released/<host> 都在、且与 slots/<slot>
//     三者挂在同一个 lease 上时(= 前任那套完整的三元组,前任已 fence 并优雅退出)才复用。
//     Txn 把三者改挂到新 lease 并**删掉 released**。三元组不一致(affinity 被同主机的
//     另一个进程改挂过 / 前任崩溃没写 released)→ 不复用也不抢,走 3。
//  3. 扫 slots/(已占)与 watermark/(隔离期),按 selection.go 选槽,
//     CAS If CreateRev(slots/<slot>)==0 Then Put slots + affinity。失败重扫重试。
//     候选为空 → ErrNoSlotAvailable。
//
// 亲和键被别人占着**永远不会阻塞申领**:fresh 路径只 CAS slots key,affinity 直接覆盖
// (最新的进程拥有亲和;被覆盖的一方只损失"重启复用同槽"这一优化,不损失正确性 ——
// 复用前提是三元组同 lease,被覆盖的 affinity 与它的 released 不在同一个 lease 上)。
func allocate(ctx context.Context, cli *clientv3.Client, kind, host string, r resolvedOptions) (*claim, error) {
	if kind == "" || strings.ContainsAny(kind, "/ ") {
		return nil, fmt.Errorf("%w: bad kind %q", ErrInvalidOptions, kind)
	}
	if host == "" || strings.Contains(host, "/") {
		return nil, fmt.Errorf("%w: bad host %q", ErrInvalidOptions, host)
	}

	leaseResp, err := cli.Grant(ctx, r.ttl)
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: grant lease: %w", err)
	}
	lease := leaseResp.ID
	uuid := newHolderID()
	revoke := func() { _, _ = cli.Revoke(context.Background(), lease) }

	aKey := affinityKey(kind, r.cluster, host)
	rKey := releasedKey(kind, r.cluster, host)

	// 2) 亲和复用。
	if c, err := tryReuse(ctx, cli, kind, host, uuid, lease, aKey, rKey, r); err != nil {
		revoke()
		return nil, err
	} else if c != nil {
		return c, nil
	}

	// 3) 正常申领,CAS 失败重试。
	for {
		select {
		case <-ctx.Done():
			revoke()
			return nil, ctx.Err()
		default:
		}

		used, err := scanUsedSlots(ctx, cli, kind, r)
		if err != nil {
			revoke()
			return nil, err
		}
		wm, err := scanWatermarks(ctx, cli, kind, r)
		if err != nil {
			revoke()
			return nil, err
		}
		now := snowflake.NowEpochSec()
		slot, ok := selectSlot(used, wm, now, uint64(r.quarantine/time.Second), r.maxSlot)
		if !ok {
			revoke()
			return nil, fmt.Errorf("%w: kind=%s cluster=%d max_slot=%d used=%d quarantined=%d",
				ErrNoSlotAvailable, kind, r.cluster, r.maxSlot, len(used), len(wm))
		}

		sKey := slotKey(kind, r.cluster, slot)
		slotStr := strconv.FormatUint(slot, 10)
		cmps := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(sKey), "=", 0)}
		ops := []clientv3.Op{
			clientv3.OpPut(sKey, uuid, clientv3.WithLease(lease)),
			clientv3.OpPut(aKey, slotStr, clientv3.WithLease(lease)),
		}
		// 过渡期(见 legacyMirror):同一笔 Txn 里占住旧布局的 id 键。比较条件也一起加 ——
		// scanUsedSlots 已经把旧 id 算成占用,这条 CreateRevision==0 只为堵住"扫描之后、
		// 提交之前旧二进制抢先拿到同一个 id"的窗口,让互斥在两个方向上都是原子的。
		if r.legacyMirror() {
			lKey := legacyIDKey(r.legacy, slot)
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(lKey), "=", 0))
			ops = append(ops, clientv3.OpPut(lKey, host, clientv3.WithLease(lease)))
		}
		txn, err := cli.Txn(ctx).If(cmps...).Then(ops...).Commit()
		if err != nil {
			revoke()
			return nil, fmt.Errorf("snowflakealloc: claim txn: %w", err)
		}
		if txn.Succeeded {
			logx.Infof("[snowflakealloc] allocated kind=%s worker=c%d:%d inc=%d (host=%s, previous_watermark=%d, now=%d)",
				kind, r.cluster, slot, txn.Header.Revision, host, wm[slot], now)
			return &claim{lease: lease, slot: slot, uuid: uuid, incarnation: txn.Header.Revision}, nil
		}
		// CAS 失败 → 并发抢占,重扫。
	}
}

// tryReuse 实现同主机亲和复用;不满足前提时返回 (nil, nil) 让调用方走正常申领。
func tryReuse(ctx context.Context, cli *clientv3.Client, kind, host, uuid string, lease clientv3.LeaseID,
	aKey, rKey string, r resolvedOptions) (*claim, error) {
	// 一次 Txn 里读三把 key,拿到同一 revision 下的快照。
	read, err := cli.Txn(ctx).Then(clientv3.OpGet(aKey), clientv3.OpGet(rKey)).Commit()
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: read affinity: %w", err)
	}
	aKvs := read.Responses[0].GetResponseRange().Kvs
	rKvs := read.Responses[1].GetResponseRange().Kvs
	if len(aKvs) == 0 {
		return nil, nil
	}
	if len(rKvs) == 0 {
		// 亲和键在、没有 released:前任要么还活着(本地 -Zone 双进程,2026-09-02 事故),
		// 要么崩溃了 lease 未到期。两种都不抢、不复用;fresh 路径也不会被它挡住。
		logx.Infof("[snowflakealloc] affinity %s exists without a released marker; not reusing (kind=%s)", host, kind)
		return nil, nil
	}
	predLease := aKvs[0].Lease
	if predLease == 0 || rKvs[0].Lease != predLease {
		logx.Infof("[snowflakealloc] affinity/released for %s are on different leases (%x vs %x); not reusing (kind=%s)",
			host, aKvs[0].Lease, rKvs[0].Lease, kind)
		return nil, nil
	}
	slotStr := string(aKvs[0].Value)
	slot, perr := strconv.ParseUint(slotStr, 10, 64)
	if perr != nil || slot > r.maxSlot {
		return nil, nil
	}
	sKey := slotKey(kind, r.cluster, slot)
	sResp, err := cli.Get(ctx, sKey)
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: read slot for reuse: %w", err)
	}
	if len(sResp.Kvs) == 0 || sResp.Kvs[0].Lease != predLease {
		return nil, nil
	}
	predUUID := string(sResp.Kvs[0].Value)

	ops := []clientv3.Op{
		clientv3.OpPut(sKey, uuid, clientv3.WithLease(lease)),
		clientv3.OpPut(aKey, slotStr, clientv3.WithLease(lease)),
		// 2.0b:复用即消费掉 released,同主机的第三个进程不能再凭它接管活着的槽。
		clientv3.OpDelete(rKey),
	}
	// 过渡期(见 legacyMirror):把旧布局的 id 键改挂到新 lease。这里用无条件 Put 而不是
	// CreateRevision==0 —— 前任(同样是新二进制)大概率已经挂着它,而上面六条比较已经
	// 证明整个三元组还是前任那一套,这个槽此刻没有第二个主人。
	if r.legacyMirror() {
		ops = append(ops, clientv3.OpPut(legacyIDKey(r.legacy, slot), host, clientv3.WithLease(lease)))
	}
	txn, err := cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.Value(aKey), "=", slotStr),
			clientv3.Compare(clientv3.LeaseValue(aKey), "=", clientv3.LeaseID(predLease)),
			clientv3.Compare(clientv3.CreateRevision(rKey), ">", 0),
			clientv3.Compare(clientv3.LeaseValue(rKey), "=", clientv3.LeaseID(predLease)),
			clientv3.Compare(clientv3.Value(sKey), "=", predUUID),
			clientv3.Compare(clientv3.LeaseValue(sKey), "=", clientv3.LeaseID(predLease)),
		).
		Then(ops...).
		Commit()
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: reuse txn: %w", err)
	}
	if !txn.Succeeded {
		return nil, nil
	}
	logx.Infof("[snowflakealloc] reused kind=%s worker=c%d:%d inc=%d (host=%s, predecessor=%s)",
		kind, r.cluster, slot, txn.Header.Revision, host, predUUID)
	return &claim{lease: lease, slot: slot, uuid: uuid, incarnation: txn.Header.Revision, reused: true}, nil
}

// scanUsedSlots 返回 slots/ 下有 lease 的槽;cluster 0 兼容期还把旧布局的活 id 算进去。
func scanUsedSlots(ctx context.Context, cli *clientv3.Client, kind string, r resolvedOptions) (map[uint64]bool, error) {
	used := make(map[uint64]bool)
	p := slotsPrefix(kind, r.cluster)
	resp, err := cli.Get(ctx, p, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: scan slots: %w", err)
	}
	for _, kv := range resp.Kvs {
		if s, perr := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), p), 10, 64); perr == nil {
			used[s] = true
		}
	}
	if r.cluster != 0 || r.legacy == "" {
		return used, nil
	}
	// 旧布局:snowflake_ids/<id> 与 snowflake_nodes/<host>=<id>。发布一版之后可删。
	idP := legacyIDPrefix(r.legacy)
	idResp, err := cli.Get(ctx, idP, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: scan legacy ids: %w", err)
	}
	for _, kv := range idResp.Kvs {
		if s, perr := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), idP), 10, 64); perr == nil {
			used[s] = true
		}
	}
	nodeResp, err := cli.Get(ctx, legacyNodePrefix(r.legacy), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: scan legacy nodes: %w", err)
	}
	for _, kv := range nodeResp.Kvs {
		if s, perr := strconv.ParseUint(string(kv.Value), 10, 64); perr == nil {
			used[s] = true
		}
	}
	return used, nil
}

// scanWatermarks 读 watermark/ 全表;cluster 0 兼容期与旧布局的 snowflake_guard /
// guard_ms 按槽取 max。发布一版之后可删兼容读。
func scanWatermarks(ctx context.Context, cli *clientv3.Client, kind string, r resolvedOptions) (map[uint64]uint64, error) {
	wm, err := scanSecMap(ctx, cli, watermarkPrefix(kind, r.cluster), 1)
	if err != nil {
		return nil, err
	}
	if r.cluster != 0 || r.legacy == "" {
		return wm, nil
	}
	legacySec, err := scanSecMap(ctx, cli, legacyGuardPrefix(r.legacy), 1)
	if err != nil {
		return nil, err
	}
	wm = mergeWatermarks(wm, legacySec)
	legacyMs, err := scanSecMap(ctx, cli, legacyGuardMsPrefix(r.legacy), 1000)
	if err != nil {
		return nil, err
	}
	// guard_ms 是 Unix 毫秒,换算成 snowflake.Epoch 起的秒。
	for slot, unixSec := range legacyMs {
		if unixSec > snowflake.Epoch {
			legacyMs[slot] = unixSec - snowflake.Epoch
		} else {
			delete(legacyMs, slot)
		}
	}
	return mergeWatermarks(wm, legacyMs), nil
}

// scanSecMap 把 <prefix><slot>=<n> 读成 map[slot]=n/divisor。
func scanSecMap(ctx context.Context, cli *clientv3.Client, prefix string, divisor uint64) (map[uint64]uint64, error) {
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: scan %s: %w", prefix, err)
	}
	out := make(map[uint64]uint64, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		slot, perr := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), prefix), 10, 64)
		if perr != nil {
			continue
		}
		v, perr := strconv.ParseUint(string(kv.Value), 10, 64)
		if perr != nil {
			continue
		}
		out[slot] = v / divisor
	}
	return out, nil
}

// readWatermarkFloor 读某个槽的持久水位(新 key,cluster 0 兼容期与旧 key 取 max)。
// key 不存在返回 (0, 0, nil)(此前没人用过)。第二个返回值是**新 key** 的 ModRevision,
// 给水位写的 CAS 当起点(见 putWatermarkLocked),0 = key 不存在。
//
// 抽成变量只为让测试注入读失败(F4 的 fail-closed 分支);生产路径恒等于下面这个实现。
var readWatermarkFloorFn = readWatermarkFloor

func readWatermarkFloor(ctx context.Context, cli *clientv3.Client, kind string, r resolvedOptions, slot uint64) (uint64, int64, error) {
	floor, rev, err := readUintRev(ctx, cli, watermarkKey(kind, r.cluster, slot))
	if err != nil {
		return 0, 0, err
	}
	if r.cluster == 0 && r.legacy != "" {
		if legacy, err := readUint(ctx, cli, legacyGuardKey(r.legacy, slot)); err != nil {
			return 0, 0, err
		} else if legacy > floor {
			floor = legacy
		}
	}
	return floor, rev, nil
}

// watermarkFloorReadAttempts 是申领后读地板的重试次数。地板是复用路径**唯一**的防线
// (复用刻意跳过隔离期),一次 RPC 抖动就把它读丢太脆。
const watermarkFloorReadAttempts = 3

// readWatermarkFloorRetry 带界重试地读地板。
//
// 刻意**不继承调用方的 ctx**:消费方给整个申领 10s,走到这里预算可能已经花光,
// 而"外层表快到点了"不是放弃读地板的理由 —— 读不到就只能 fail-closed(见调用点)。
// 每次尝试用一个新的 etcdOpTimeout。
func readWatermarkFloorRetry(cli *clientv3.Client, kind string, r resolvedOptions, slot uint64) (uint64, int64, error) {
	var lastErr error
	for i := 0; i < watermarkFloorReadAttempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), etcdOpTimeout)
		floor, rev, err := readWatermarkFloorFn(ctx, cli, kind, r, slot)
		cancel()
		if err == nil {
			return floor, rev, nil
		}
		lastErr = err
		logx.Errorf("[snowflakealloc] read watermark floor attempt %d/%d failed (kind=%s cluster=%d slot=%d): %v",
			i+1, watermarkFloorReadAttempts, kind, r.cluster, slot, err)
		if i+1 < watermarkFloorReadAttempts {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return 0, 0, lastErr
}

func readUint(ctx context.Context, cli *clientv3.Client, key string) (uint64, error) {
	v, _, err := readUintRev(ctx, cli, key)
	return v, err
}

// readUintRev 读一个十进制数值 key,同时返回它的 ModRevision(key 不存在时都是 0)。
func readUintRev(ctx context.Context, cli *clientv3.Client, key string) (uint64, int64, error) {
	resp, err := cli.Get(ctx, key)
	if err != nil {
		return 0, 0, err
	}
	if len(resp.Kvs) == 0 {
		return 0, 0, nil
	}
	v, perr := strconv.ParseUint(string(resp.Kvs[0].Value), 10, 64)
	if perr != nil {
		return 0, 0, fmt.Errorf("malformed value %q at %s: %w", resp.Kvs[0].Value, key, perr)
	}
	return v, resp.Kvs[0].ModRevision, nil
}

// ---- Handle --------------------------------------------------------------------

// Fencer 是"能被永久停用"的发号器:shared/snowflake.Node 与 login 的 PlayerIDGen 都满足。
type Fencer interface{ Fence() }

// Handle 把槽位、lease、keepalive / 水位 / reclaim 循环打包,方便调用方在进程退出时清理。
type Handle struct {
	// Kind / Cluster / Slot / WorkerID / UUID 分配之后只读。
	Kind     string
	Cluster  uint32
	Slot     uint64
	WorkerID uint64 // = ComposeWorkerID(Cluster, Slot),发号器吃的就是它
	UUID     string
	// GuardEpochSec 是这个槽上**前任**最后记录的水位(自 snowflake.Epoch 起,含前推)。
	// NewNode() 会把它当地板注入;0 表示此前没人用过。分配之后只读。
	GuardEpochSec uint64

	r      resolvedOptions
	cli    *clientv3.Client
	host   string
	cancel context.CancelFunc

	// lease 是当前挂着 slots key 的 lease(reclaim 会换);incarnation 是最近一次
	// 成功挂上 slots key 的 Txn revision(全局单调唯一,日志按它关联)。
	lease       atomic.Int64
	incarnation atomic.Int64
	// registered = slots key 此刻确实挂在我们的 lease 上(本地缓存启动时为 false,
	// 注册成功后置 true)。
	registered atomic.Bool
	// needReclaim = 有信号表明 slots key 可能不在我们的 lease 上,循环要去重新挂。
	needReclaim atomic.Bool

	// fence 是按水位年龄自 fence 的闸,每次水位写成功 Ack;发号器共用它。
	fence *snowflake.FenceClock

	fencersMu sync.Mutex
	fencers   []Fencer

	// guardMu 串行化水位推进;guardWritten / msWritten 是本进程**已成功落盘**的最大水位。
	// 必须串行:NewNode() 的同步首写(调用方协程)与循环 goroutine 的 tick 并发;
	// 锁跨 Txn 持有保证"值的顺序 = 落盘的顺序",水位绝不倒退。
	//
	// guardRev / msRev 是两把水位 key 已知的 ModRevision,给写入的 CAS 用
	// (见 putWatermarkLocked:etcd 的值比较是按字节的,"9" > "10",没法拿 Value 比大小)。
	guardMu      sync.Mutex
	guardWritten uint64
	guardRev     int64
	msWritten    uint64
	msRev        int64
	// 本地缓存的落盘状态(guardMu 下;设计稿 §7.5-5,用户拍板:**稳态零写盘**)。
	// cache 是缓存文件的内存映像,每次 Ack / 每拍都更新;磁盘只在下面三种时刻写:
	//   - cacheWrittenInc ≠ 当前 incarnation(申领 / reclaim 之后的第一次 Ack:身份落盘);
	//   - watermarkDegraded(故障模式:上一次水位 Txn 出错 / 超时 / 归属不成立 / 缓存启动
	//     尚未注册):故障期本地高水位是同主机 F 内重启时唯一罩得住已发号段的地板,每一拍
	//     都要在 Txn **之前**落盘,第一次失败则在失败一返回时立刻写;
	//   - Close()(最终值)。
	// 稳态(上一拍已 Ack)一次都不写:继任者的地板在 etcd 里,磁盘那份只在"etcd 不通时起服"
	// 才被读。代价是磁盘上的 LastAckWall 在健康运行期间不刷新 —— 健康跑了很久之后崩溃、
	// 且重启那一刻 etcd 恰好也不通,缓存会因 LastAckWall 早于 F 而不可用,只能等 etcd;
	// 这是双重故障,§7.5-5 明确接受。
	// cacheDirty = 内存记录里有磁盘上还没有的变化,让"补写"不会重复写同一份内容。
	// watermarkDegraded 的进入 / 退出各记一条日志(不是每拍),便于按日志对齐故障窗口。
	cache             cacheRecord
	cacheDirty        bool
	cacheWrittenInc   int64
	watermarkDegraded bool
	lastCacheErr      time.Time

	// node 是经 NewNode() 构造出的发号器(为空 = 调用方还没构造 / login 用的是 bwmarrin)。
	// advanceGuard 靠它读真实高水位:借位时 lastTime 会跑到墙钟前面。
	node atomic.Pointer[snowflake.Node]

	closing  atomic.Bool
	lost     chan struct{}
	lostOnce sync.Once
}

// Lost 在本进程**真的不再持有**槽位时关闭:slots key 被挂到了别的 uuid 上
// (运维手动清理 / 水位机制被绕过 / 旧版本二进制)。lease 丢了不算 —— 循环会先 reclaim。
//
// 消费方契约不变:收到即 Fence 发号器并退出,让编排重拉。Close() 引起的正常关闭不触发。
func (h *Handle) Lost() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.lost
}

func (h *Handle) markLost() {
	h.lostOnce.Do(func() {
		// 防御纵深:不等消费方,先把挂在本 Handle 上的发号器都关掉。
		h.fenceAll()
		close(h.lost)
	})
}

func (h *Handle) lostClosed() bool {
	select {
	case <-h.lost:
		return true
	default:
		return false
	}
}

// Lease 返回当前 lease(本地缓存启动且尚未注册时为 0)。
func (h *Handle) Lease() clientv3.LeaseID { return clientv3.LeaseID(h.lease.Load()) }

// Incarnation 返回 slots key 最近一次成功挂上时的 revision。
func (h *Handle) Incarnation() int64 { return h.incarnation.Load() }

// Registered 返回 slots key 此刻是否确认挂在我们的 lease 上。
func (h *Handle) Registered() bool { return h.registered.Load() }

// FenceClock 返回自 fence 闸,给不走 NewNode 的发号器(login PlayerIDGen)共用。
func (h *Handle) FenceClock() *snowflake.FenceClock { return h.fence }

// LogFields 返回日志关联三字段:kind / worker=c<cluster>:<slot> / inc=<revision>。
// slot 会随重启变,inc 全局单调唯一 —— 排查撞号按 inc。
func (h *Handle) LogFields() string {
	s := fmt.Sprintf("kind=%s worker=c%d:%d inc=%d", h.Kind, h.Cluster, h.Slot, h.Incarnation())
	if !h.registered.Load() {
		s += " (cached, unregistered)"
	}
	return s
}

// AttachFencer 登记一个发号器:Close() 与 markLost 会先 Fence 它们。NewNode 自动登记;
// login 把 PlayerIDGen 挂上来。
func (h *Handle) AttachFencer(f Fencer) {
	if f == nil {
		return
	}
	h.fencersMu.Lock()
	h.fencers = append(h.fencers, f)
	h.fencersMu.Unlock()
}

func (h *Handle) fenceAll() {
	h.fencersMu.Lock()
	fs := append([]Fencer(nil), h.fencers...)
	h.fencersMu.Unlock()
	for _, f := range fs {
		f.Fence()
	}
}

func (h *Handle) sKey() string { return slotKey(h.Kind, h.Cluster, h.Slot) }
func (h *Handle) aKey() string { return affinityKey(h.Kind, h.Cluster, h.host) }

// AllocateWithKeepAlive 申领槽位并在后台启动 keepalive / 水位 / reclaim 循环。
//
// 启动期弱依赖(设计稿 §3.5):申领失败且原因不是"候选为空 / 参数错"时,若本地缓存
// 说 now − lastAckWall < F,就用缓存的槽起来,后台每 2s 重试注册;注册成功前水位
// 不会 Ack,所以 F 规则仍然兜着。etcd 可达时缓存只做日志关联。
func AllocateWithKeepAlive(ctx context.Context, cli *clientv3.Client, kind, hostname string, opts Options) (*Handle, error) {
	r, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	workerFor := func(slot uint64) (uint64, error) {
		return ComposeWorkerID(r.cluster, slot, r.clusterBits, r.slotBits)
	}

	cl, err := allocate(ctx, cli, kind, hostname, r)
	if err != nil {
		if errors.Is(err, ErrNoSlotAvailable) || errors.Is(err, ErrInvalidOptions) || r.cachePath == "" {
			return nil, err
		}
		rec, cerr := readCacheFile(r.cachePath)
		if cerr != nil {
			logx.Errorf("[snowflakealloc] allocation failed (%v) and local cache %s unusable (%v); cannot start",
				err, r.cachePath, cerr)
			return nil, err
		}
		age := time.Since(time.UnixMilli(rec.LastAckWall))
		if rec.Kind != kind || rec.Cluster != r.cluster || rec.Slot > r.maxSlot || age < 0 || age >= r.fenceAfter {
			logx.Errorf("[snowflakealloc] allocation failed (%v); local cache %s is not usable "+
				"(kind=%s cluster=%d slot=%d ack_age=%v F=%v); cannot start",
				err, r.cachePath, rec.Kind, rec.Cluster, rec.Slot, age.Truncate(time.Second), r.fenceAfter)
			return nil, err
		}
		worker, werr := workerFor(rec.Slot)
		if werr != nil {
			return nil, werr
		}
		h := newHandle(cli, kind, hostname, r, worker, rec.Slot, rec.UUID)
		h.incarnation.Store(rec.Incarnation)
		// 地板 = max(最后一次写成功的水位, 本地高水位) + lead。
		//
		// 取 max 是必须的:前任(= 上一次的本进程)在 etcd 不通期间照样发号,那段时间
		// LastWatermark 是**冻住的**,只有 LocalHighWaterSec 跟着走。少了它,同主机在 F 内
		// 重启就会以一个罩不住前任借位 / 时钟回拨的地板起来。再 +lead 吃掉"最后一次落盘
		// 之后又发了 <1s 号"的窗口;SetGuardTime 是 floor 语义,自动与 now 取 max。
		floor := rec.LastWatermark
		if rec.LocalHighWaterSec > floor {
			floor = rec.LocalHighWaterSec
		}
		h.GuardEpochSec = floor + guardLeadSec
		h.guardWritten = floor
		h.msWritten = rec.LastWatermarkMs
		if rec.LocalHighWaterMs > h.msWritten {
			h.msWritten = rec.LocalHighWaterMs
		}
		h.cache = *rec
		h.fence.SeedAck(time.UnixMilli(rec.LastAckWall))
		h.needReclaim.Store(true)
		// 能走到这里就是 etcd 不通:这一世还没有任何一次 Ack,发出去的号只有本地高水位能罩,
		// 所以从第一拍起就按故障模式对待(每拍在 Txn 之前落盘),不等第一次 Txn 超时才发现要写。
		// 进入故障模式的日志就是下面这条,ackLocked 退出时再记一条。
		h.watermarkDegraded = true
		logx.Errorf("[snowflakealloc] etcd unreachable (%v); booting from local cache %s: %s, last ack %v ago "+
			"(F=%v) — registration will be retried every %v; minting stops if no ack lands within F; "+
			"entering watermark outage mode: local high water is persisted every tick until a watermark lands",
			err, r.cachePath, h.LogFields(), age.Truncate(time.Second), r.fenceAfter, reclaimRetryInterval)
		kaCtx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.run(kaCtx, &loopState{})
		return h, nil
	}

	worker, err := workerFor(cl.slot)
	if err != nil {
		_, _ = cli.Revoke(context.Background(), cl.lease)
		return nil, err
	}

	// 读前任在这个槽上留下的高水位(带界重试:地板读丢一次就重放前任的号,代价太大)。
	floor, floorRev, ferr := readWatermarkFloorRetry(cli, kind, r, cl.slot)
	if ferr != nil && cl.reused {
		// **复用路径 fail-closed**:复用是刻意跳过隔离期的(前任已 fence 并优雅退出),
		// 于是水位地板是这条路径上**唯一**的跨重启防线;读不到它就等于没有防线。
		// 撤销 lease 直接失败 —— 进程重启后会走正常申领,拿一个受隔离期保护的新槽。
		_, _ = cli.Revoke(context.Background(), cl.lease)
		return nil, fmt.Errorf("snowflakealloc: reused slot c%d:%d but its watermark floor is unreadable "+
			"(kind=%s host=%s): %w; refusing to mint without the predecessor's floor", r.cluster, cl.slot, kind, hostname, ferr)
	}

	h := newHandle(cli, kind, hostname, r, worker, cl.slot, cl.uuid)
	h.lease.Store(int64(cl.lease))
	h.incarnation.Store(cl.incarnation)
	h.registered.Store(true)
	if ferr != nil {
		// 全新申领:槽刚过完隔离期 Q,前任(如果有)早就停发 F 之前的事了,退化成只有
		// 启动 guard 的旧强度不阻断启动。但**不能**把 guardWritten 当成 0 记下来 ——
		// 那会让第一次 advanceGuard 以为"etcd 里没有更高的值",把别人写的高水位盖低。
		// 留它不设(=0 且 guardRev=0),putWatermarkLocked 的 CAS 会去 etcd 里问真值。
		logx.Errorf("[snowflakealloc] read watermark floor failed (%s): %v; falling back to boot-guard only — "+
			"a clock-skewed takeover could replay the previous holder's ids", h.LogFields(), ferr)
		// 兜一层本地缓存:同主机上一次就用的这个槽时,缓存里的水位同样是合法地板。
		floor = h.floorFromCache(cl.slot)
	} else {
		h.guardWritten = floor
		h.guardRev = floorRev
	}
	h.GuardEpochSec = floor

	kaCtx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	ka, err := cli.KeepAlive(kaCtx, cl.lease)
	if err != nil {
		cancel()
		_, _ = cli.Revoke(context.Background(), cl.lease)
		return nil, fmt.Errorf("snowflakealloc: keepalive: %w", err)
	}

	// **申领后立刻同步写一次水位**(带槽归属校验):任何被持有的槽都必须有 ≥ 申领时刻的
	// 水位,否则它在申领者眼里仍是"从没用过",隔离期对它无效。写失败只告警,闸不 Ack,
	// 发号器会拒发到下一次写成功为止(fail-closed)。
	h.advanceGuard(ctx)

	// 所有权由 **slots key 的 value** 表达,KeepAlive 只观测 lease —— 两者错配就是双发号盲区
	// (etcd 的 Put 只把 key 从旧 lease 摘下改挂新 lease,不撤销也不通知旧 lease)。所以
	// watch slots key:改挂到别的 uuid 立刻 markLost;DELETE 则先 reclaim 再定。
	// 起点 = 申领 Txn 的 revision + 1,申领与 watch 之间没有窗口。
	watch := cli.Watch(kaCtx, h.sKey(), clientv3.WithRev(cl.incarnation+1))
	go h.run(kaCtx, &loopState{ka: ka, watch: watch})
	return h, nil
}

func newHandle(cli *clientv3.Client, kind, host string, r resolvedOptions, worker, slot uint64, uuid string) *Handle {
	h := &Handle{
		Kind:     kind,
		Cluster:  r.cluster,
		Slot:     slot,
		WorkerID: worker,
		UUID:     uuid,
		r:        r,
		cli:      cli,
		host:     host,
		fence:    snowflake.NewFenceClock(r.fenceAfter),
		lost:     make(chan struct{}),
	}
	h.cache = cacheRecord{Kind: kind, Cluster: r.cluster, Slot: slot, UUID: uuid}
	return h
}

// floorFromCache 在 etcd 里的水位读不到时,退回本地缓存文件里的水位当地板。
//
// 只有 (kind, cluster, slot) 三者都对上才作数 —— 那说明这台机器上一次跑的就是这个槽,
// 缓存里的值是**本机前任**真实发过的秒,和 etcd 里那份是同一个含义。对不上就返回 0
// (缓存属于别的槽,拿来当地板毫无意义)。读不到文件同样返回 0。
func (h *Handle) floorFromCache(slot uint64) uint64 {
	if h.r.cachePath == "" {
		return 0
	}
	rec, err := readCacheFile(h.r.cachePath)
	if err != nil || rec.Kind != h.Kind || rec.Cluster != h.Cluster || rec.Slot != slot {
		return 0
	}
	floor := rec.LastWatermark
	if rec.LocalHighWaterSec > floor {
		floor = rec.LocalHighWaterSec
	}
	if floor > 0 {
		logx.Errorf("[snowflakealloc] using the local cache watermark %d as the floor (%s): etcd read failed",
			floor, h.LogFields())
	}
	return floor
}

// ---- 后台循环:keepalive 排空 / 水位 / watch / reclaim ---------------------------------

const (
	// guardWriteInterval 是持久水位的写入节拍(同时是循环的心跳)。
	guardWriteInterval = time.Second
	// guardLeadSec 是水位的**前推量**(秒),必须 **> 写入间隔**。
	//
	// 水位契约:任何时刻,已持久化的水位 ≥ 本进程可能发到的最大逻辑秒。
	// 写入间隔 1s、前推 2s ⇒ 两次写入之间即使写不出去,上一次写的 (now+2) 也仍然
	// 覆盖着这 1s 内能发到的秒。上界同样有约束:继任者拿它当地板,前推过大会撞上
	// snowflake 的借位预算(maxBorrowAheadSec=10)直接发不出号。
	guardLeadSec = 2
	// reclaimRetryInterval 是 reclaim / 注册失败后的重试间隔。
	reclaimRetryInterval = 2 * time.Second
	// etcdOpTimeout 是循环内单次 etcd 操作(申领 / reclaim / 读地板 / released)的超时。
	etcdOpTimeout = 3 * time.Second
	// watermarkTxnTimeout 是 advanceGuard / PutMsWatermark 里**水位 Txn 专用**的超时,
	// 覆盖 putWatermarkLocked 的整个调用(含 CAS 重试),刻意比 etcdOpTimeout 短。
	// 它必须 **< guardLeadSec**,这是本地缓存"稳态零写盘"(设计稿 §7.5-5)的正确性前提:
	//
	//	稳态下缓存文件不再每拍预写本地高水位,于是从"上一次 Ack"到"发现这一拍写失败"之间
	//	发出去的号,只有 etcd 里那条已 Ack 的水位罩着。上一拍在墙钟 T−1 写成功的值是
	//	(T−1)+lead = T+1;这一拍在 T 发起的 Txn 最晚 T+1.5 返回失败,期间发出的号逻辑秒
	//	≤ T+1 ≤ 已 Ack 水位 —— 仍被罩住。失败一返回,advanceGuard 立刻把本地高水位
	//	(这一拍的 target = T+2)落盘并进入故障模式:此后每拍都在 Txn **之前**写,后面
	//	发出的号全归本地高水位罩。两段拼起来没有空档。
	//	若超时 ≥ lead(比如沿用 3s),T+2 这一秒的号会落在"etcd 没有、本地也没有"的窗口里。
	//
	// 借位不破坏这个论证:每拍 target 取 max(墙钟, 发号器高水位)+lead;借位速率 ≤ 1 逻辑秒 /
	// snowflake.waitBudget(3s),而两次 Ack 之间最长 guardWriteInterval + watermarkTxnTimeout
	// = 2.5s < 3s,所以失败窗口内高水位最多再涨 1 秒,仍在上一拍 lead(2s)之内。
	watermarkTxnTimeout = 1500 * time.Millisecond
)

// 编译期钉死 watermarkTxnTimeout < guardLeadSec·1s:差值为负时对 uint 的常量转换不合法,
// 改错任一常量直接编不过,不必等测试。
const _ = uint(guardLeadSec*time.Second - watermarkTxnTimeout - 1)

type loopState struct {
	ka    <-chan *clientv3.LeaseKeepAliveResponse
	watch clientv3.WatchChan
	ticks uint64
}

// run 是唯一会改 lease / watch / keepalive channel 的 goroutine。
//
// 失租处理(设计稿 §1.3 / §3.4):keepalive 流结束、slots key 被删,都**不再**直接 markLost,
// 而是 reclaim —— 重新 Grant 一个 lease,把 slots key 挂回去(If CreateRev==0 或
// Value==uuid)。只有 key 被别的 uuid 占着才是真的失去所有权。
func (h *Handle) run(kaCtx context.Context, st *loopState) {
	ticker := time.NewTicker(guardWriteInterval)
	defer ticker.Stop()
	for {
		select {
		case <-kaCtx.Done():
			return
		case _, ok := <-st.ka:
			if !ok {
				if h.closing.Load() {
					return
				}
				// keepalive 流结束 = lease 没了(过期 / 被撤销 / 客户端判过期)。
				// 只是活性信号丢失,不等于所有权丢失:去看 slots key。
				st.ka = nil
				h.needReclaim.Store(true)
				logx.Errorf("[snowflakealloc] keepalive stream ended (%s); attempting to reclaim the slot instead of giving up",
					h.LogFields())
				h.reclaim(kaCtx, st, "keepalive stream ended")
			}
		case <-ticker.C:
			if h.closing.Load() {
				return
			}
			st.ticks++
			if h.needReclaim.Load() {
				if st.ticks%uint64(reclaimRetryInterval/guardWriteInterval) == 0 {
					h.reclaim(kaCtx, st, "periodic retry")
				}
			} else {
				if st.watch == nil {
					h.rewatch(kaCtx, st)
				}
				// 顺带推进持久水位:按 tick 粒度写,借位后最多一个 tick 内水位追平。
				h.advanceGuard(kaCtx)
				if h.needReclaim.Load() {
					h.reclaim(kaCtx, st, "watermark txn found the slot not on our uuid")
				}
			}
		case wr, ok := <-st.watch:
			if h.closing.Load() {
				return
			}
			if !ok || wr.Err() != nil {
				// watch 断了不等于失去所有权(弱依赖):下一拍先 Get 校验归属再重新 watch。
				logx.Errorf("[snowflakealloc] ownership watch ended (closed=%v err=%v; %s); will re-verify and re-watch",
					!ok, wr.Err(), h.LogFields())
				st.watch = nil
				continue
			}
			for _, ev := range wr.Events {
				if ev.Type == clientv3.EventTypeDelete {
					h.needReclaim.Store(true)
					h.reclaim(kaCtx, st, "slot key deleted")
					continue
				}
				if ev.Kv == nil {
					continue
				}
				if string(ev.Kv.Value) != h.UUID {
					h.onOwnershipLost(fmt.Sprintf("slot key now holds uuid %s on lease %x", ev.Kv.Value, ev.Kv.Lease))
					return
				}
				// 被接管时 value 可能不变(有人拿我们的 uuid 重挂),**变的是 lease** ——
				// 我们自己的 reclaim 在提交前就已把新 lease 记下,所以这里不会误判自己。
				if clientv3.LeaseID(ev.Kv.Lease) != h.Lease() {
					h.onOwnershipLost(fmt.Sprintf("slot key re-attached to lease %x (ours is %x)", ev.Kv.Lease, h.Lease()))
					return
				}
			}
		}
		if h.lostClosed() {
			return
		}
	}
}

// reclaim 把 slots key 重新挂到我们的 lease 上。
//
// 判定顺序:
//  1. Get slots key。value 是别人的 uuid → 所有权已丢,markLost。
//  2. value 是我们、且 lease 正是当前 lease 且仍存活 → 虚惊(客户端侧误判),恢复 keepalive 即可。
//  3. 否则 Grant 新 lease,**一笔** Txn:If CreateRev==0(key 已过期)Then Put;
//     Else 内层 Txn If Value==uuid(还在旧 lease 上)Then Put。两支都不成立才去看是谁拿走了。
//
// ⚠️ 这里必须是**一笔**嵌套 Txn,不能拆成两笔"先比 CreateRev、再比 Value"。拆开时旧 lease 的
// revoke 只要落在两笔中间,第二笔的 Value 比较就会在一把**已经不存在**的 key 上失败,
// 随后的 Get 读到 0 条记录,代码却把它当成"被别人抢走"→ markLost → 消费方 Fence + 退出。
// 而这恰恰是 reclaim 存在的理由(lease 抖动),没有任何人拿走这个槽。所以:
// 只有 follow-up Get 读到一条 value ≠ 我们 uuid 的记录,才算真的失去所有权。
//
// etcd 不可达时什么都不改,留着 needReclaim 下一拍再试;这段时间发号器由 F 兜底。
func (h *Handle) reclaim(kaCtx context.Context, st *loopState, reason string) {
	ctx, cancel := context.WithTimeout(kaCtx, etcdOpTimeout)
	defer cancel()

	resp, err := h.cli.Get(ctx, h.sKey())
	if err != nil {
		logx.Errorf("[snowflakealloc] reclaim (%s) cannot read slot key (%s): %v; will retry", reason, h.LogFields(), err)
		return
	}
	cur := h.Lease()
	if len(resp.Kvs) == 1 {
		kv := resp.Kvs[0]
		if string(kv.Value) != h.UUID {
			h.onOwnershipLost(fmt.Sprintf("reclaim (%s): slot key held by uuid %s on lease %x", reason, kv.Value, kv.Lease))
			return
		}
		if cur != 0 && clientv3.LeaseID(kv.Lease) == cur {
			if ttl, terr := h.cli.TimeToLive(ctx, cur); terr == nil && ttl.TTL > 0 {
				if st.ka == nil {
					if ch, kerr := h.cli.KeepAlive(kaCtx, cur); kerr == nil {
						st.ka = ch
					}
				}
				h.needReclaim.Store(false)
				h.registered.Store(true)
				if st.watch == nil {
					st.watch = h.cli.Watch(kaCtx, h.sKey(), clientv3.WithRev(resp.Header.Revision+1))
				}
				logx.Infof("[snowflakealloc] reclaim (%s): slot still on our live lease %x, nothing to do (%s)",
					reason, cur, h.LogFields())
				return
			}
		}
	}

	lease, err := h.cli.Grant(ctx, h.r.ttl)
	if err != nil {
		logx.Errorf("[snowflakealloc] reclaim (%s) cannot grant lease (%s): %v; will retry", reason, h.LogFields(), err)
		return
	}
	slotStr := strconv.FormatUint(h.Slot, 10)
	puts := []clientv3.Op{
		clientv3.OpPut(h.sKey(), h.UUID, clientv3.WithLease(lease.ID)),
		clientv3.OpPut(h.aKey(), slotStr, clientv3.WithLease(lease.ID)),
	}
	// 过渡期(见 legacyMirror):旧布局的 id 键跟着换到新 lease,否则它随旧 lease 过期后
	// 灰度中的旧二进制就看不见这个槽被占了。
	if h.r.legacyMirror() {
		puts = append(puts, clientv3.OpPut(legacyIDKey(h.r.legacy, h.Slot), h.host, clientv3.WithLease(lease.ID)))
	}
	txn, err := h.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(h.sKey()), "=", 0)).
		Then(puts...).
		Else(clientv3.OpTxn(
			[]clientv3.Cmp{clientv3.Compare(clientv3.Value(h.sKey()), "=", h.UUID)},
			puts,
			nil,
		)).
		Commit()
	if err != nil {
		_, _ = h.cli.Revoke(context.Background(), lease.ID)
		logx.Errorf("[snowflakealloc] reclaim (%s) txn failed (%s): %v; will retry", reason, h.LogFields(), err)
		return
	}
	// recreated = key 当时已经不存在了,是我们重新 Create 出来的。这条信息决定要不要
	// 重读地板(见下面的 refloor):key 消失意味着我们的 lease 曾经过期,而过期时长
	// 可能已经越过隔离期 Q —— 那期间别人可以合法申领这个槽、发一批号、再释放。
	recreated := txn.Succeeded
	inner := txn.Responses[0].GetResponseTxn()
	if !txn.Succeeded && !inner.GetSucceeded() {
		_, _ = h.cli.Revoke(context.Background(), lease.ID)
		g, gerr := h.cli.Get(ctx, h.sKey())
		switch {
		case gerr != nil:
			logx.Errorf("[snowflakealloc] reclaim (%s) lost both compares but the follow-up read failed (%s): %v; "+
				"assuming a blip and retrying", reason, h.LogFields(), gerr)
		case len(g.Kvs) == 1 && string(g.Kvs[0].Value) != h.UUID:
			h.onOwnershipLost(fmt.Sprintf("reclaim (%s): slot key taken by uuid %s on lease %x",
				reason, g.Kvs[0].Value, g.Kvs[0].Lease))
		default:
			// key 不见了 / 又变回我们的 uuid:没有任何人接管的证据,不许 markLost
			// (那会让消费方 Fence + 退出)。留着 needReclaim,下一拍重试。
			logx.Errorf("[snowflakealloc] reclaim (%s) raced with the key's own expiry (%s); nobody took the slot, retrying",
				reason, h.LogFields())
		}
		return
	}

	prevInc := h.incarnation.Load()
	h.lease.Store(int64(lease.ID))
	h.incarnation.Store(txn.Header.Revision)
	h.needReclaim.Store(false)
	h.registered.Store(true)
	// 重新 Create 出来的 key,或本进程已经因水位过期自 fence 过 —— 两者都意味着"这中间
	// 可能有别人当过这个槽的主人",必须把 etcd 里现在的水位重新吃成地板。
	if stale, _ := h.fence.Stale(); recreated || stale {
		h.refloorFromWatermark(ctx, reason, recreated)
	}
	if ch, kerr := h.cli.KeepAlive(kaCtx, lease.ID); kerr == nil {
		st.ka = ch
	} else {
		// 开不了 keepalive 流:lease 会在 TTL 后过期 → DELETE 事件 → 再来一次 reclaim。
		st.ka = nil
		logx.Errorf("[snowflakealloc] reclaim: keepalive on new lease %x failed: %v", lease.ID, kerr)
	}
	if st.watch == nil {
		st.watch = h.cli.Watch(kaCtx, h.sKey(), clientv3.WithRev(txn.Header.Revision+1))
	}
	logx.Errorf("[snowflakealloc] reclaimed slot (%s): %s, incarnation %d -> %d, lease %x -> %x",
		reason, h.LogFields(), prevInc, txn.Header.Revision, cur, lease.ID)
}

// refloorFromWatermark 在"槽 key 曾经消失过"或"本进程曾经因水位过期自 fence"之后,
// 把 etcd 里当前的水位重新当地板吃回来(秒级喂 SetGuardTime,毫秒级抬 msWritten)。
//
// 为什么申领时读一次不够:reclaim 的 CreateRev==0 分支是一次**重新申领** —— key 之所以
// 不存在,是因为我们的 lease 早就过期了。如果那段失联超过隔离期 Q,继任者 B 可以合法拿走
// 这个槽、发一批号、再释放;等我们回来把 key 重新 Create 出来时,发号器还停在自己那口
// (可能落后于 B 的)时钟上,没有任何东西把它抬到 B 的高水位之上。
//
// 读失败时只告警不阻断:putWatermarkLocked 的 CAS 是最后一道保险 —— 它发现 etcd 里的
// 存值更高时会把存值当地板吃下去,而**闸只在那条路径上 Ack**,所以在地板补齐之前
// 发号器根本不会恢复发号。
func (h *Handle) refloorFromWatermark(ctx context.Context, reason string, recreated bool) {
	floor, rev, err := readWatermarkFloorFn(ctx, h.cli, h.Kind, h.r, h.Slot)
	if err != nil {
		logx.Errorf("[snowflakealloc] reclaim (%s) could not re-read the watermark floor (%s): %v; "+
			"the next watermark write will adopt whatever etcd holds before un-fencing", reason, h.LogFields(), err)
		return
	}
	ms, msErr := readMsWatermarkRaw(ctx, h)

	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	if floor > h.guardWritten {
		h.guardWritten = floor
	}
	h.guardRev = rev
	if msErr == nil && ms > h.msWritten {
		h.msWritten = ms
	}
	if n := h.node.Load(); n != nil && floor > 0 {
		// 地板语义:只在比当前高水位更晚时才生效,绝不把发号器往回拨。
		n.SetGuardTime(floor)
	}
	logx.Errorf("[snowflakealloc] reclaim (%s) re-applied the watermark floor sec=%d ms=%d (%s, recreated=%v): "+
		"another holder may have used this slot while we were away", reason, floor, h.msWritten, h.LogFields(), recreated)
}

// readMsWatermarkRaw 读本槽的毫秒水位(新 key 与过渡期旧 key 取 max)。与 ReadMsWatermark
// 的区别是它不碰 guardMu(调用方自己持锁),也不退回本地缓存。
func readMsWatermarkRaw(ctx context.Context, h *Handle) (uint64, error) {
	ms, err := readUint(ctx, h.cli, watermarkMsKey(h.Kind, h.Cluster, h.Slot))
	if err != nil {
		return 0, err
	}
	if h.r.legacyMirror() {
		legacy, lerr := readUint(ctx, h.cli, legacyGuardMsKey(h.r.legacy, h.Slot))
		if lerr != nil {
			return 0, lerr
		}
		if legacy > ms {
			ms = legacy
		}
	}
	return ms, nil
}

// rewatch 在 watch 断掉后重建:先 Get 校验归属(不是我们的就交给 reclaim),再从当前
// revision 之后开始 watch。etcd 不可达就留到下一拍。
func (h *Handle) rewatch(kaCtx context.Context, st *loopState) {
	ctx, cancel := context.WithTimeout(kaCtx, etcdOpTimeout)
	defer cancel()
	resp, err := h.cli.Get(ctx, h.sKey())
	if err != nil {
		return
	}
	if len(resp.Kvs) != 1 || string(resp.Kvs[0].Value) != h.UUID || clientv3.LeaseID(resp.Kvs[0].Lease) != h.Lease() {
		h.needReclaim.Store(true)
		return
	}
	st.watch = h.cli.Watch(kaCtx, h.sKey(), clientv3.WithRev(resp.Header.Revision+1))
}

// onOwnershipLost 在"slots key 归属已经不是自己"时调用。lease 可能仍然活着、KeepAlive
// 也仍在成功 —— 但槽已经被别人接管,继续发号就是确定性撞号。
func (h *Handle) onOwnershipLost(reason string) {
	if h.closing.Load() {
		return
	}
	logx.Errorf("[snowflakealloc] slot ownership lost (%s, host=%s): %s; another process now owns this worker id — "+
		"generators fenced, caller MUST stop and let the orchestrator restart", h.LogFields(), h.host, reason)
	h.markLost()
}

// ---- 水位 ------------------------------------------------------------------------

// NewNode 用本 Handle 的 worker id 构造发号器,注入前任水位作地板、挂上自 fence 闸,
// 并**同步写一次水位再把发号器交出去**。
//
// 请一律用它取代裸 snowflake.NewNode:裸构造只有"不在构造秒发号"的点排除,顶不住跨机
// 时钟偏斜接管、本机时钟回拨、前任借过逻辑秒这三类情况;也没有自 fence。
func (h *Handle) NewNode() *snowflake.Node {
	n := snowflake.NewNode(h.WorkerID)
	// 地板取"申领时读到的前任水位"与"已确认落盘的水位"的**较大者**:申领时那次读可能
	// 失败过(降级分支只有启动 guard),而在那之后的第一次水位写会通过 CAS 把 etcd 里的
	// 真值吃进 guardWritten —— 那时发号器还没构造出来,SetGuardTime 无处可打。
	h.guardMu.Lock()
	floor := h.GuardEpochSec
	if h.guardWritten > floor {
		floor = h.guardWritten
	}
	h.guardMu.Unlock()
	if floor > 0 {
		// 地板语义 + 等待真实时钟越过(不借位),这是跨重启屏障的一部分,见 snowflake.go。
		n.SetGuardTime(floor)
	}
	n.SetFenceClock(h.fence)
	h.node.Store(n)
	h.AttachFencer(n)

	// 同步首写:从这里到循环的第一个 tick 之间(≤1s)我们已经在发号,etcd 里必须已经有
	// 覆盖这段的水位。写失败只告警,闸不 Ack ⇒ 发号器拒发到下一次写成功为止。
	ctx, cancel := context.WithTimeout(context.Background(), etcdOpTimeout)
	h.advanceGuard(ctx)
	cancel()
	return n
}

// SyncWatermark 同步写一次秒级水位(带槽归属校验),供不走 NewNode 的消费方(login)
// 在开始服务前调用。返回是否写成功(= 闸已 Ack)。
func (h *Handle) SyncWatermark(ctx context.Context) bool {
	h.advanceGuard(ctx)
	stale, _ := h.fence.Stale()
	return !stale
}

// advanceGuard 把本槽的持久水位推进到"墙钟秒与发号器真实高水位的较大者 + 前推量",
// 并以 **slots key 仍是我们的 uuid** 为 Txn 条件。写成功 = 同时证明了所有权 ⇒ Ack 闸。
// 返回值 = 这一次水位是否已经落定(闸 Ack 了),Close() 用它决定要不要写 released。
//
// 取 max 而不能只写墙钟:发号器在 step 耗尽 / 时钟停摆时会**借位**(lastTime 跑到墙钟
// 前面),只写墙钟的水位罩不住借位期间发出的号。水位单调不回退,写失败只告警:
// 水位是"下一任的地板",本进程自己的唯一性由地板 + 高水位单调 + F 保证。
func (h *Handle) advanceGuard(ctx context.Context) bool {
	target := snowflake.NowEpochSec()
	if n := h.node.Load(); n != nil {
		if hw := n.HighWaterEpochSec(); hw > target {
			target = hw
		}
	}
	target += guardLeadSec

	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	if target < h.guardWritten {
		// 继承的地板比现在还高(前任时钟超前):不回退,原值重写一遍换一次 Ack。
		target = h.guardWritten
	}
	// 本地高水位先记下(故障模式下这一步就落盘,稳态只改内存),再去写 etcd。稳态不预写
	// 之所以安全,靠的是 Txn 超时 watermarkTxnTimeout < 前推量 guardLeadSec(见常量注释):
	// 这一拍失败被发现之前发出的号仍在上一拍已 Ack 水位的 lead 之内。
	h.noteLocalHighWaterLocked(target, 0)

	legacyKey := ""
	if h.r.legacyMirror() {
		legacyKey = legacyGuardKey(h.r.legacy, h.Slot)
	}
	persisted, owned, err := h.putWatermarkLocked(ctx, watermarkKey(h.Kind, h.Cluster, h.Slot), legacyKey, &h.guardRev, target)
	if err != nil {
		logx.Errorf("[snowflakealloc] advance watermark failed (%s): %v", h.LogFields(), err)
		h.noteWatermarkWriteFailedLocked("sec watermark txn failed: " + err.Error())
		return false
	}
	if !owned {
		// 槽不在我们的 uuid 上(lease 过期被删 / 被接管 / 缓存启动尚未注册):不 Ack,
		// 交给 reclaim 判定。这里不 markLost —— 过期被删是可恢复的。
		h.needReclaim.Store(true)
		h.noteWatermarkWriteFailedLocked("sec watermark txn: slot not on our uuid")
		return false
	}
	if persisted > target {
		// etcd 里的存值比我们要写的还高 —— 只可能是"这个槽在我们失联期间被别人当过主人"。
		// 把它当地板吃下去再 Ack,否则解除自 fence 之后第一个号就落在前任发过的秒里。
		if n := h.node.Load(); n != nil {
			n.SetGuardTime(persisted)
		}
		logx.Errorf("[snowflakealloc] adopted a higher stored watermark %d (ours was %d; %s): "+
			"another holder used this slot while we were away", persisted, target, h.LogFields())
	}
	h.guardWritten = persisted
	h.ackLocked()
	return true
}

// putWatermarkLocked 写一把持久水位 key(秒级或毫秒级)。调用方必须持 guardMu。
// 返回 (etcd 里现在的值, 槽是否仍归我们, 错误)。
//
// 三件事一起做:
//  1. **归属**:外层 Txn 条件 Value(slots/<slot>)==uuid —— 提交成功即证明所有权,可以 Ack 闸。
//  2. **相对 etcd 单调**:内层 Txn 用 ModRevision CAS。本进程自己的 guardWritten 只能挡住
//     自己写的值倒退,挡不住"申领时地板没读到 / 失联期间别人写过更高的值"这两类;
//     真正的存值必须由 etcd 说了算。CAS 落空时 Else 分支把当前值读回来:存值 ≥ target
//     就干脆不写(它已经罩住我们),否则用新 revision 再试一次。
//     ⚠️ 不能用 Compare(Value, "<") 代替 —— etcd 的值比较是**按字节**的,"9" > "10"。
//  3. **过渡镜像**(见 legacyMirror):同一笔 Txn 里把值写进旧布局的 key,灰度期的旧二进制
//     接手这个槽时才有地板可读。**发布一版之后把 legacyKey 参数与它的 Put 一起删。**
//
// 超时是 **一个** watermarkTxnTimeout 罩住整个调用(两次 CAS 尝试共用一个 deadline),
// 不是每次尝试各一个:稳态零写盘的论证要求"从发起到发现失败"整体 < guardLeadSec,
// 两次各 1.5s 就是 3s,又把窗口撑回去了。
func (h *Handle) putWatermarkLocked(ctx context.Context, key, legacyKey string, rev *int64, target uint64) (uint64, bool, error) {
	value := strconv.FormatUint(target, 10)
	putCtx, cancel := context.WithTimeout(ctx, watermarkTxnTimeout)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		ops := []clientv3.Op{clientv3.OpPut(key, value)}
		if legacyKey != "" {
			ops = append(ops, clientv3.OpPut(legacyKey, value))
		}
		txn, err := h.cli.Txn(putCtx).
			If(clientv3.Compare(clientv3.Value(h.sKey()), "=", h.UUID)).
			Then(clientv3.OpTxn(
				[]clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", *rev)},
				ops,
				[]clientv3.Op{clientv3.OpGet(key)},
			)).
			Commit()
		if err != nil {
			return 0, true, err
		}
		if !txn.Succeeded {
			return 0, false, nil
		}
		inner := txn.Responses[0].GetResponseTxn()
		if inner.GetSucceeded() {
			*rev = txn.Header.Revision
			return target, true, nil
		}
		kvs := inner.Responses[0].GetResponseRange().Kvs
		if len(kvs) == 0 {
			// key 刚被删掉(运维清理):按"不存在"重来一次,这次 CAS 会成立。
			*rev = 0
			continue
		}
		*rev = kvs[0].ModRevision
		stored, perr := strconv.ParseUint(string(kvs[0].Value), 10, 64)
		if perr != nil {
			return 0, true, fmt.Errorf("snowflakealloc: malformed watermark %q at %s: %w", kvs[0].Value, key, perr)
		}
		if stored >= target {
			// 存值已经罩住我们:不写。归属已经被外层 Txn 证明,所以调用方照样可以 Ack。
			return stored, true, nil
		}
	}
	return 0, true, fmt.Errorf("snowflakealloc: watermark %s lost the CAS twice in a row (concurrent writer?)", key)
}

// noteLocalHighWaterLocked 在 guardMu 下记下"本进程打算发到的最高时刻",**不等 etcd**。
// sec / ms 传 0 表示这一维不动。
//
// 这是 etcd 长时间不可达时唯一还在推进的地板来源:LastWatermark 冻在最后一次 Ack,
// 而发号器还能再发 F 那么久(还可能借位、可能撞上墙钟回拨)。
//
// 稳态只改内存不写盘(§7.5-5):etcd 每秒都在确认水位,继任者的地板在 etcd 里;磁盘那份
// 只在"etcd 不通时起服"才被读。故障模式(上一拍没 Ack 起)则每拍在 Txn **之前**落盘,
// 之后发出的号全归它罩。第一次失败那一拍事先无从知道会失败,只能在失败返回后立刻补写
// (noteWatermarkWriteFailedLocked);这段"发起到失败"的空档由 watermarkTxnTimeout <
// guardLeadSec 保证仍在上一拍已 Ack 水位之内 —— 见常量注释。
func (h *Handle) noteLocalHighWaterLocked(sec, ms uint64) {
	if h.r.cachePath == "" {
		return
	}
	changed := false
	if sec > h.cache.LocalHighWaterSec {
		h.cache.LocalHighWaterSec = sec
		changed = true
	}
	if ms > h.cache.LocalHighWaterMs {
		h.cache.LocalHighWaterMs = ms
		changed = true
	}
	if !changed {
		return
	}
	h.cacheDirty = true
	if h.watermarkDegraded {
		h.writeCacheLocked()
	}
}

// noteWatermarkWriteFailedLocked 在 guardMu 下记录一次没有 Ack 的水位写(Txn 出错 / 超时 /
// 槽不在我们 uuid 上):进入故障模式,并把 Txn 之前记下的本地高水位立刻落盘 —— 从这一刻
// 起,直到下一次 Ack,磁盘上的这份就是同主机在 F 内重启时唯一罩得住已发号段的地板。
//
// 已经在故障模式里再次失败:这一拍的本地高水位在 Txn 之前就写过了(cacheDirty 已清),
// flush 是空操作;日志只在**进入**时记一条,不然故障期每秒一行淹掉别的。
func (h *Handle) noteWatermarkWriteFailedLocked(reason string) {
	if !h.watermarkDegraded {
		h.watermarkDegraded = true
		logx.Errorf("[snowflakealloc] entering watermark outage mode (%s): %s; local high water will be "+
			"persisted to %s every tick before the txn until a watermark lands", h.LogFields(), reason, h.r.cachePath)
	}
	h.flushCacheLocked()
}

// ackLocked 在 guardMu 下记录一次成功的水位写:Ack 闸 + 退出故障模式 + 更新本地缓存(内存)。
//
// 落盘只在身份(incarnation)变了的第一次 Ack:申领 / reclaim 之后老的 incarnation 已经没有
// 意义,缓存启动路径靠它关联日志。其余 Ack **不写盘**(§7.5-5 稳态零写盘);恢复那一拍也不
// 因为"刚从故障模式出来"而额外写 —— 它的本地高水位在 Txn 之前已经写过,LastAckWall 留给
// Close() 或下一次故障的首写去刷新。
func (h *Handle) ackLocked() {
	h.fence.Ack()
	if h.watermarkDegraded {
		h.watermarkDegraded = false
		logx.Infof("[snowflakealloc] leaving watermark outage mode (%s): watermark acked; local cache %s back to "+
			"steady state (no writes until close, reclaim or the next failure)", h.LogFields(), h.r.cachePath)
	}
	if h.r.cachePath == "" {
		return
	}
	inc := h.incarnation.Load()
	h.cache.Incarnation = inc
	h.cache.LastWatermark = h.guardWritten
	h.cache.LastWatermarkMs = h.msWritten
	h.cache.LastAckWall = h.fence.LastAckWall().UnixMilli()
	h.cacheDirty = true
	if inc != h.cacheWrittenInc {
		h.writeCacheLocked()
	}
}

// flushCacheLocked 在 guardMu 下把内存里尚未落盘的变化写出去;没有变化就不碰磁盘。
func (h *Handle) flushCacheLocked() {
	if h.r.cachePath == "" || !h.cacheDirty {
		return
	}
	h.writeCacheLocked()
}

func (h *Handle) writeCacheLocked() {
	if err := writeCacheFileFn(h.r.cachePath, &h.cache); err != nil {
		// 缓存只影响"下次 etcd 不通时能不能起",限频告警即可。cacheDirty 留着,下一个
		// 落盘时机再补写。
		if time.Since(h.lastCacheErr) > time.Minute {
			h.lastCacheErr = time.Now()
			logx.Errorf("[snowflakealloc] write local cache %s failed: %v (boot without etcd will not be possible)",
				h.r.cachePath, err)
		}
		return
	}
	h.cacheDirty = false
	h.cacheWrittenInc = h.cache.Incarnation
}

// ReadMsWatermark 读本槽的毫秒级持久水位(Unix ms);key 不存在返回 0。cluster 0 兼容期
// 与旧布局 <LegacyPrefix>/guard_ms/<slot> 取 max。读失败必须返回错误让调用方 fail-closed
// —— 水位是防跨重启重放的唯一地板;只有本地缓存启动(尚未注册)时才退回缓存值。
func (h *Handle) ReadMsWatermark(ctx context.Context) (uint64, error) {
	ms, err := readUint(ctx, h.cli, watermarkMsKey(h.Kind, h.Cluster, h.Slot))
	if err == nil && h.r.legacyMirror() {
		var legacy uint64
		legacy, err = readUint(ctx, h.cli, legacyGuardMsKey(h.r.legacy, h.Slot))
		if legacy > ms {
			ms = legacy
		}
	}
	h.guardMu.Lock()
	cached := h.msWritten
	registered := h.registered.Load()
	h.guardMu.Unlock()
	if err != nil {
		if !registered && cached > 0 {
			return cached, nil
		}
		return 0, err
	}
	if cached > ms {
		ms = cached
	}
	return ms, nil
}

// PutMsWatermark 写毫秒级持久水位(带槽归属校验,成功即 Ack 闸)。写入节奏与前推量由
// 调用方决定(它才知道自己发号器的时钟语义);单调不回退。写失败只回错误不重试。
func (h *Handle) PutMsWatermark(ctx context.Context, ms uint64) error {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	if ms < h.msWritten {
		ms = h.msWritten
	}
	// 与 advanceGuard 同理:本地高水位先记下(故障模式落盘),再去写 etcd;Txn 同样受
	// watermarkTxnTimeout 约束 —— 调用方(login)自己定前推量,须保证它 > 该超时 + 写入间隔。
	h.noteLocalHighWaterLocked(0, ms)

	legacyKey := ""
	if h.r.legacyMirror() {
		legacyKey = legacyGuardMsKey(h.r.legacy, h.Slot)
	}
	persisted, owned, err := h.putWatermarkLocked(ctx, watermarkMsKey(h.Kind, h.Cluster, h.Slot), legacyKey, &h.msRev, ms)
	if err != nil {
		h.noteWatermarkWriteFailedLocked("ms watermark txn failed: " + err.Error())
		return err
	}
	if !owned {
		h.needReclaim.Store(true)
		h.noteWatermarkWriteFailedLocked("ms watermark txn: slot not on our uuid")
		return fmt.Errorf("snowflakealloc: slot %s not held by our uuid; ms watermark not written", h.LogFields())
	}
	h.msWritten = persisted
	h.ackLocked()
	return nil
}

// ---- Close ---------------------------------------------------------------------------

// Close 优雅释放。顺序(设计稿 §3.3):
//  1. Fence 所有登记的发号器 —— 之后一个号都发不出去;
//  2. 同步写最终水位(带归属校验);
//  3. 写 released/<host>(挂当前 lease):同主机后继进程据此复用原槽;
//  4. 取消 keepalive / watch。
//
// **刻意不 Revoke lease**:槽在 lease 自然过期(TTL)后才从 slots/ 消失,而且之后还要
// 过隔离期 Q 才能被别人申领;优雅退出与崩溃退出因此行为一致。曾经这里 Revoke 过,
// 配合"最小空闲"选号让刚释放的槽成为下一个启动者优先拿到的那个 —— 真实缺陷。
// 多次调用安全。
func (h *Handle) Close() {
	if h == nil || h.closing.Swap(true) {
		return
	}
	h.fenceAll()

	ctx, cancel := context.WithTimeout(context.Background(), etcdOpTimeout)
	defer cancel()
	if h.cli != nil && h.registered.Load() {
		// released 是"后继进程可以**跳过隔离期**复用这个槽"的通行证,而复用路径唯一的
		// 防线就是水位地板 —— 所以最终水位没写成功时**不许**发这张通行证:让后继进程走
		// 正常申领,拿一个受隔离期保护的新槽,宁可换个槽也不要在没有地板的情况下复用。
		finalWatermarkOK := h.advanceGuard(ctx)
		if lease := h.Lease(); lease != 0 && finalWatermarkOK {
			// 标记必须晚于 fence 与最终水位,否则同主机后继进程复用时前任还在发号。
			// 尽力而为:写失败只损失"重启复用同槽"这一优化,不影响正确性。
			txn, err := h.cli.Txn(ctx).
				If(clientv3.Compare(clientv3.Value(h.sKey()), "=", h.UUID)).
				Then(clientv3.OpPut(releasedKey(h.Kind, h.Cluster, h.host), "released", clientv3.WithLease(lease))).
				Commit()
			if err != nil || !txn.Succeeded {
				logx.Errorf("[snowflakealloc] write released marker failed (%s, host=%s): err=%v succeeded=%v",
					h.LogFields(), h.host, err, txn != nil && txn.Succeeded)
			}
		} else if !finalWatermarkOK {
			logx.Errorf("[snowflakealloc] final watermark write failed on close (%s, host=%s); NOT writing the "+
				"released marker — the next process on this host will take a fresh, quarantine-protected slot",
				h.LogFields(), h.host)
		}
	}
	// 稳态 Ack 不落盘,最后一次 Ack 只在内存里:退出前把它写出去(§7.5-5 的"Close 写一次"),
	// 磁盘上的 LastAckWall 才是真实的"最后一次确认",同主机随后离线重启能用满 F 的资格窗口。
	h.guardMu.Lock()
	h.flushCacheLocked()
	h.guardMu.Unlock()
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}
