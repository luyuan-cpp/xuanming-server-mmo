// Package loginsession manages login session state in the login-domain Redis.
//
// 登录态按「gate 连接」记,一条连接一个键(键形在 shared/redisslot/login_keys.go):
//
//	login_session:{<gate_instance_id>:<session_incarnation>} → account
//	login_account_sessions:{<AccountTag>}:<account>          → 设备集,成员是连接身份
//
// 为什么不用 session_id 建键(docs/design/data-overwrite-rollback-audit-20261018.md §2.3、
// client-request-ordering.md §7.2 第 2 条):session_id = node_id<<17 | 进程内序号,gate 被 kill -9 后
// 以同一个 node_id 重启、序号从 0 开始,新连接会撞上旧进程留下、TTL 内仍有效的 login_session。
// 跳过 Login 直接发 CreatePlayer / EnterGame 的客户端于是继承了别人的账号。gate_instance_id 是每次进程启动
// 生成的 UUID,session_incarnation 在进程内单调,两者合起来全局唯一 —— 与会话栅栏(sessionfence)同一个身份。
// 没有这个身份的连接没有登录态,RefOf 拒绝。
//
// 登录提交分两步,因为登录态按连接分布、设备集按账号分布,不同槽,进不了同一段脚本
// (docs/design/redis-player-store-cluster.md §10.2):
//
//  1. sessionfence.CommitLogin:连接没关才写登录态,检查与写入在同一段脚本里。
//  2. RegisterDevice:SADD + PEXPIRE 设备集。
//
// 两步之间的结局都能自愈,设备数上限本来就是软限制,不影响资产和写权:
//   - 第 2 步失败:这次登录按 Redis 故障回失败,登录态随 TTL 过期;
//   - Disconnect 抢在第 2 步之前:设备集留一个陈旧成员,它的登录态已不存在,下一次 PruneStaleFromDeviceSet 剔除;
//   - Disconnect 的清理(Cleanup)同样是两步,SREM 之后 DEL 之前中断时登录态随 TTL 过期,连接已关闭,没有人会再用它。
//
// Operations:
//
//	RefOf:          从 gate 带来的 SessionDetails 取本连接的登录态身份。
//	RegisterDevice: 登录提交的第 2 步。
//	GetAccount:     读本连接的账号。
//	Cleanup:        删本连接的登录态并移出设备集。
package loginsession

import (
	"context"
	"errors"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/sessionfence"
	login_proto_common "proto/common/base"
	"shared/playerredis"
	"shared/redisslot"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// cleanupMissTotal counts Cleanup() calls where the session key was already
// absent in Redis (legitimate race: Disconnect arrived before BindSession
// committed, or a duplicate Disconnect). Surfaced as a metric so the previous
// Error-level spam can stay at Debug without losing observability.
var cleanupMissTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "loginsession_cleanup_miss_total",
	Help: "Cleanup() called but session key absent (Disconnect/BindSession race).",
}, []string{"caller"})

// Ref 是一条 gate 连接在 login_session 里的身份,由 RefOf 构造;零值不可用。
type Ref struct {
	fence sessionfence.Key
}

// RefOf 从 gate 带来的 SessionDetails 取本连接的登录态身份。没有 (gate_instance_id, session_incarnation) 时
// ok = false,调用方拒绝这条连接。判定与会话栅栏共用 sessionfence.KeyOf。
func RefOf(details *login_proto_common.SessionDetails) (Ref, bool) {
	fenceKey, ok := sessionfence.KeyOf(details)
	if !ok {
		return Ref{}, false
	}
	return Ref{fence: fenceKey}, true
}

// Fence 返回本连接的会话栅栏键。
func (r Ref) Fence() sessionfence.Key {
	return r.fence
}

// Key 返回本连接 login_session 的 Redis 键。
func (r Ref) Key() string {
	return redisslot.LoginSessionKey(r.fence.String())
}

// DeviceMember 返回本连接在账号设备集里的成员值。
func (r Ref) DeviceMember() string {
	return r.fence.String()
}

// TTL 返回 login_session 与设备集的寿命(Node.SessionExpireMin)。
func TTL() time.Duration {
	return time.Duration(config.AppConfig.Node.SessionExpireMin) * time.Minute
}

// RegisterDevice 是登录提交的第 2 步:把本连接登记进账号设备集,在 sessionfence.CommitLogin 成功之后调用。
// 失败时调用方按 Redis 故障回失败,见包注释。
func RegisterDevice(ctx context.Context, rdb redis.UniversalClient, account string, ref Ref, ttl time.Duration) error {
	deviceSetKey := redisslot.AccountDeviceSetKey(account)
	_, err := rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.SAdd(ctx, deviceSetKey, ref.DeviceMember())
		pipe.PExpire(ctx, deviceSetKey, ttl)
		return nil
	})
	return err
}

// PruneStaleFromDeviceSet scans the per-account device set and removes any
// members whose backing login_session key no longer exists. Returns the number
// of entries removed. 成员来自不同连接、不同槽,按 slot 分组读(playerredis.MGetBySlot)。
//
// This is a best-effort self-heal called from Login to recover from the
// race where Gate's socket-closed → login.Disconnect RPC is still in
// flight when the same account reconnects — without it, a heavy-reconnect
// workload (stress robot) can trip MaxDevicesPerAccount on stale entries
// that Cleanup never got around to removing. 登录提交两步之间被 Disconnect 抢先留下的成员也由它剔除。
//
// 读失败时什么都不剔除:读不到不等于登录态不存在,把活连接移出设备集比留一个陈旧成员更糟。
func PruneStaleFromDeviceSet(ctx context.Context, rdb redis.UniversalClient, deviceSetKey string) int {
	members, err := rdb.SMembers(ctx, deviceSetKey).Result()
	if err != nil || len(members) == 0 {
		return 0
	}
	sessionKeys := make([]string, len(members))
	for i, member := range members {
		sessionKeys[i] = redisslot.LoginSessionKey(member)
	}
	sessions, err := playerredis.MGetBySlot(ctx, rdb, sessionKeys)
	if err != nil {
		logx.Errorf("PruneStaleFromDeviceSet read sessions failed: key=%s err=%v", deviceSetKey, err)
		return 0
	}
	stale := make([]interface{}, 0, len(members))
	for i, session := range sessions {
		if session == nil {
			stale = append(stale, members[i])
		}
	}
	if len(stale) == 0 {
		return 0
	}
	if err := rdb.SRem(ctx, deviceSetKey, stale...).Err(); err != nil {
		logx.Errorf("PruneStaleFromDeviceSet SRem failed: key=%s err=%v", deviceSetKey, err)
		return 0
	}
	logx.Infof("Pruned %d stale sessions from device set %s", len(stale), deviceSetKey)
	return len(stale)
}

// GetAccount 返回本连接登录的账号;本连接没有成功 Login 过(或登录态已过期 / 已清理)时返回 redis.Nil。
func GetAccount(ctx context.Context, rdb redis.UniversalClient, ref Ref) (string, error) {
	return rdb.Get(ctx, ref.Key()).Result()
}

// Cleanup removes the connection's login state and its device-set entry.
//
// `redis: nil` here is a legitimate race (Disconnect arriving before BindSession
// committed, or a second Cleanup for the same connection). Round 14 demotes that
// case to Debug and surfaces it via a Prometheus counter so the login ERROR log
// is no longer flooded under high reconnect churn (45k-bot stress saw tens of
// thousands of these per minute, drowning out real errors). Real Redis errors
// (connection drop, command timeout) stay at Error level.
func Cleanup(ctx context.Context, rdb redis.UniversalClient, ref Ref, logicTag string) {
	key := ref.Key()
	account, err := rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			cleanupMissTotal.WithLabelValues(logicTag).Inc()
			logx.Debugf("[%s] cleanup miss (session not in redis, expected race): session=%s", logicTag, ref.DeviceMember())
		} else {
			logx.Errorf("[%s] get account failed for cleanup: session=%s err=%v", logicTag, ref.DeviceMember(), err)
		}
	} else {
		rdb.SRem(ctx, redisslot.AccountDeviceSetKey(account), ref.DeviceMember())
	}
	rdb.Del(ctx, key)
	logx.Infof("[%s] session cleanup: session=%s", logicTag, ref.DeviceMember())
}
