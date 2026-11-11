package clusterredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"shared/redisslot"
)

// KeyPolicy 是一个客户端所属的 Redis 域,决定发送前守卫接受哪些键(docs/design/redis-cluster-all-domains-20261110.md §3.1、§5.1)。
//
// 两个域靠 hash tag 的形状区分,不再靠「有没有 tag」:控制集群登记过的族 tag(redisslot.IsControlTag,形如 {ctl:7})
// 只属于控制集群;其余 tag 形状(玩家 id、slot-tag、登录连接身份)只属于玩家域。开发环境两个域可能指向同一个单实例,
// 守卫不拦的话,键发错句柄要到上了集群才暴露,表现是读写悄悄落在两边。
type KeyPolicy uint8

const (
	// PlayerDomainKeys:玩家域与登录域。每个键都带 hash tag,多键命令的键同 slot,不得出现控制集群的族 tag。
	// 它是零值,所以 GuardHook{} 与挪包之前的守卫行为相同。
	PlayerDomainKeys KeyPolicy = iota
	// ControlClusterKeys:控制集群。带 tag 的键只能用登记过的族 tag;多键命令的每个键都带 tag,且是同一个 tag;
	// 单键命令可以不带 tag(选主锁、去重键这类单键族)。
	ControlClusterKeys

	keyPolicyCount
)

var keyPolicyNames = [keyPolicyCount]string{"player_domain", "control_cluster"}

func (p KeyPolicy) valid() bool { return p < keyPolicyCount }

// String 返回策略名,用于日志与错误文本。
func (p KeyPolicy) String() string {
	if !p.valid() {
		return "unknown(" + strconv.Itoa(int(p)) + ")"
	}
	return keyPolicyNames[p]
}

// ErrUntaggedKey:键缺少 hash tag。玩家域的每个键、控制集群多键命令的每个键都必须带 tag。
var ErrUntaggedKey = errors.New("clusterredis: key has no hash tag")

// ErrCrossSlot:玩家域一条多键命令的键不在同一 slot。Cluster 会回 CROSSSLOT;单实例不会报错,但上了 Cluster 必然失败。
var ErrCrossSlot = errors.New("clusterredis: keys of a multi-key command are in different slots")

// ErrWrongDomainTag:键的 hash tag 属于另一个域。控制集群的族 tag 发到了玩家域客户端,或者玩家域的 tag(以及没登记的 tag)
// 发到了控制集群客户端。
var ErrWrongDomainTag = errors.New("clusterredis: key hash tag belongs to another Redis domain")

// ErrMixedTags:控制集群一条多键命令的键带着不同的 tag。控制集群的原子操作只在一个族、一个身份之内(设计 §3.1)。
// 不同的 tag 碰巧同 slot 时 Redis 会照常执行,代码就依赖了哈希巧合,换一个 zone 号或帮会号就变成 CROSSSLOT。
var ErrMixedTags = errors.New("clusterredis: keys of a multi-key command carry different hash tags")

// ErrNonIdempotentInPipeline:非幂等命令出现在管道(含事务管道)里。go-redis 的 ClusterClient 在管道内部遇到
// EOF / 超时会自动重发整批命令(v9.16 osscluster.go 的 processPipelineNodeConn 直接用 shouldRetry 判原始错误,
// 钩子拦不到),已执行的 Lua 或计数命令会被再跑一遍。两个域的这类命令都一律单条发送。
var ErrNonIdempotentInPipeline = errors.New("clusterredis: non-idempotent command must not be pipelined")

// GuardHook 在命令发出前按 Policy 检查键。不认识键位置的命令放行(由服务端与集群路由兜底),所以守卫只会拒绝
// 确定违规的命令,不会误拒。零值是玩家域守卫。
//
// 管道(含事务管道)另有一条规则:非幂等命令不得走管道(见 ErrNonIdempotentInPipeline)。
// 事务管道里不同命令之间是否同 slot 不在这里检查:Cluster 会拒绝跨槽事务,单实例下仍须由调用方保证。
type GuardHook struct {
	Policy KeyPolicy
}

var _ redis.Hook = GuardHook{}

func (GuardHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h GuardHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := h.Policy.CheckCommand(cmd.Args()); err != nil {
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// nonIdempotentCommands 是重放会改变结果或语义的命令。EVAL 族一律算作非幂等(CAS 类脚本重放会得到不同结局)。
var nonIdempotentCommands = map[string]struct{}{
	"eval": {}, "evalsha": {}, "fcall": {},
	"incr": {}, "incrby": {}, "incrbyfloat": {}, "decr": {}, "decrby": {}, "hincrby": {}, "hincrbyfloat": {}, "zincrby": {},
	"lpush": {}, "rpush": {}, "lpop": {}, "rpop": {}, "spop": {}, "getdel": {}, "getset": {}, "rpoplpush": {}, "lmove": {},
	"append": {}, "setrange": {},
}

func (h GuardHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			err := h.Policy.CheckCommand(cmd.Args())
			if err == nil && has(nonIdempotentCommands, strings.ToLower(argString(firstArg(cmd.Args())))) {
				err = fmt.Errorf("%w: %s", ErrNonIdempotentInPipeline, strings.ToLower(argString(firstArg(cmd.Args()))))
			}
			if err != nil {
				// 整批不发:管道里任何一条违规都说明调用方的键形有缺陷,部分发送只会制造半截状态。
				for _, c := range cmds {
					c.SetErr(err)
				}
				return err
			}
		}
		return next(ctx, cmds)
	}
}

// 键位置分类。只收录本仓库实际用到、键位置确定的命令;其余命令放行。
var (
	// 第 1 个参数起全部是键。
	allKeysCommands = map[string]struct{}{
		"del": {}, "unlink": {}, "exists": {}, "touch": {}, "mget": {}, "watch": {},
		"sinter": {}, "sunion": {}, "sdiff": {}, "pfcount": {},
	}
	// 第 1 个参数是唯一的键。
	singleKeyCommands = map[string]struct{}{
		"get": {}, "set": {}, "setnx": {}, "setex": {}, "psetex": {}, "getdel": {}, "getex": {}, "getset": {},
		"incr": {}, "incrby": {}, "decr": {}, "decrby": {}, "append": {}, "strlen": {},
		"expire": {}, "pexpire": {}, "expireat": {}, "pexpireat": {}, "persist": {}, "ttl": {}, "pttl": {}, "type": {},
		"hget": {}, "hset": {}, "hsetnx": {}, "hmget": {}, "hmset": {}, "hdel": {}, "hgetall": {}, "hkeys": {},
		"hvals": {}, "hlen": {}, "hexists": {}, "hincrby": {}, "hscan": {},
		"zadd": {}, "zrem": {}, "zrange": {}, "zrangebyscore": {}, "zrevrange": {}, "zrevrangebyscore": {},
		"zscore": {}, "zcard": {}, "zcount": {}, "zincrby": {}, "zrank": {}, "zremrangebyscore": {},
		"zremrangebyrank": {}, "zscan": {}, "zmscore": {},
		"sadd": {}, "srem": {}, "smembers": {}, "sismember": {}, "smismember": {}, "scard": {}, "spop": {},
		"srandmember": {}, "sscan": {},
		"lpush": {}, "rpush": {}, "lpop": {}, "rpop": {}, "llen": {}, "lrange": {}, "lrem": {}, "ltrim": {},
		"lindex": {}, "lset": {},
	}
	// EVAL 族:第 2 个参数是 numkeys,随后 numkeys 个键。
	evalCommands = map[string]struct{}{
		"eval": {}, "evalsha": {}, "eval_ro": {}, "evalsha_ro": {}, "fcall": {}, "fcall_ro": {},
	}
)

// CommandKeys 按命令名取出命令里的键;ok=false 表示不认识键位置(调用方应放行)。与域无关。
// args 是 go-redis 的 Cmder.Args():args[0] 是命令名。
func CommandKeys(args []interface{}) (keys []string, ok bool, err error) {
	if len(args) == 0 {
		return nil, false, nil
	}
	name := strings.ToLower(argString(args[0]))
	switch {
	case has(allKeysCommands, name):
		keys = make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			keys = append(keys, argString(a))
		}
		return keys, true, nil
	case has(singleKeyCommands, name):
		if len(args) < 2 {
			return nil, false, nil
		}
		return []string{argString(args[1])}, true, nil
	case name == "mset" || name == "msetnx":
		for i := 1; i < len(args); i += 2 {
			keys = append(keys, argString(args[i]))
		}
		return keys, true, nil
	case has(evalCommands, name):
		if len(args) < 3 {
			return nil, false, nil
		}
		n, convErr := strconv.Atoi(argString(args[2]))
		if convErr != nil || n < 0 || 3+n > len(args) {
			return nil, false, fmt.Errorf("clusterredis: invalid numkeys for %s", name)
		}
		keys = make([]string, 0, n)
		for _, a := range args[3 : 3+n] {
			keys = append(keys, argString(a))
		}
		return keys, true, nil
	}
	return nil, false, nil
}

// CheckCommand 是守卫的判定:按本域的规则检查命令里的键。不认识的命令放行。
func (p KeyPolicy) CheckCommand(args []interface{}) error {
	keys, ok, err := CommandKeys(args)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return p.CheckKeys(keys)
}

// CheckKeys 校验一组将要在同一条命令里出现的键。错误文本带出违规的键名(键名不含敏感信息)。
// 绕开 GuardHook 直接发命令的通道(NewPinned)在发送前自己调它。
func (p KeyPolicy) CheckKeys(keys []string) error {
	switch p {
	case PlayerDomainKeys:
		return checkPlayerDomainKeys(keys)
	case ControlClusterKeys:
		return checkControlClusterKeys(keys)
	default:
		return fmt.Errorf("clusterredis: unknown key policy %d", p)
	}
}

// checkPlayerDomainKeys:每个键都带 tag、tag 不是控制集群的族 tag,全部同 slot。
func checkPlayerDomainKeys(keys []string) error {
	for i, key := range keys {
		tag, tagged := redisslot.HashTag(key)
		if !tagged {
			return fmt.Errorf("%w: %q", ErrUntaggedKey, key)
		}
		if redisslot.IsControlTag(tag) {
			return fmt.Errorf("%w: %q in the player domain", ErrWrongDomainTag, key)
		}
		if i > 0 && redisslot.HashSlot(key) != redisslot.HashSlot(keys[0]) {
			return fmt.Errorf("%w: %q and %q", ErrCrossSlot, keys[0], key)
		}
	}
	return nil
}

// checkControlClusterKeys:带 tag 的键只能是登记过的族 tag;单键可以不带 tag;多键命令每个键都带同一个 tag。
func checkControlClusterKeys(keys []string) error {
	firstTag := ""
	for i, key := range keys {
		tag, tagged := redisslot.HashTag(key)
		if tagged && !redisslot.IsControlTag(tag) {
			return fmt.Errorf("%w: %q in the control cluster", ErrWrongDomainTag, key)
		}
		if len(keys) == 1 {
			return nil
		}
		if !tagged {
			return fmt.Errorf("%w: %q in a multi-key command", ErrUntaggedKey, key)
		}
		if i == 0 {
			firstTag = tag
			continue
		}
		if tag != firstTag {
			return fmt.Errorf("%w: %q and %q", ErrMixedTags, keys[0], key)
		}
	}
	return nil
}

func firstArg(args []interface{}) interface{} {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func has(set map[string]struct{}, name string) bool {
	_, ok := set[name]
	return ok
}

func argString(a interface{}) string {
	switch v := a.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	default:
		return fmt.Sprint(v)
	}
}
