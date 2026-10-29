package playerstore

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

// ErrUntaggedKey:玩家域命令里出现不带 hash tag 的键。玩家域的键一律带 {<pid>} 或按 slot 索引 tag(设计 §5.1)。
var ErrUntaggedKey = errors.New("playerstore: 玩家域键缺少 hash tag")

// ErrCrossSlot:一条多键命令的键不在同一 slot。Cluster 会回 CROSSSLOT;单实例不会报错,但上了 Cluster 必然失败。
var ErrCrossSlot = errors.New("playerstore: 多键命令的键不在同一 slot")

// GuardHook 在命令发出前检查键:带 tag、同 slot。不认识键位置的命令放行(由服务端与集群路由兜底),
// 所以守卫只会拒绝确定违规的命令,不会误拒。
//
// 管道(含事务管道)另有一条规则:非幂等命令不得走管道(见 ErrNonIdempotentInPipeline)。
// 事务管道里不同命令之间是否同 slot 不在这里检查:Cluster 会拒绝跨槽事务,单实例下仍须由调用方保证。
type GuardHook struct{}

var _ redis.Hook = GuardHook{}

func (GuardHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (GuardHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := CheckCommand(cmd.Args()); err != nil {
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// ErrNonIdempotentInPipeline:非幂等命令出现在管道(含事务管道)里。go-redis 的 ClusterClient 在管道内部遇到
// EOF / 超时会自动重发整批命令(v9.16 osscluster.go 的 processPipelineNodeConn 直接用 shouldRetry 判原始错误,
// 钩子拦不到),已执行的 Lua 或计数命令会被再跑一遍。玩家域的这类命令一律单条发送。
var ErrNonIdempotentInPipeline = errors.New("playerstore: 非幂等命令不得走管道")

// nonIdempotentCommands 是重放会改变结果或语义的命令。EVAL 族一律算作非幂等(CAS 类脚本重放会得到不同结局)。
var nonIdempotentCommands = map[string]struct{}{
	"eval": {}, "evalsha": {}, "fcall": {},
	"incr": {}, "incrby": {}, "incrbyfloat": {}, "decr": {}, "decrby": {}, "hincrby": {}, "hincrbyfloat": {}, "zincrby": {},
	"lpush": {}, "rpush": {}, "lpop": {}, "rpop": {}, "spop": {}, "getdel": {}, "getset": {}, "rpoplpush": {}, "lmove": {},
	"append": {}, "setrange": {},
}

func (GuardHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			err := CheckCommand(cmd.Args())
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

// CommandKeys 按命令名取出命令里的键;ok=false 表示不认识键位置(调用方应放行)。
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
			return nil, false, fmt.Errorf("playerstore: %s 的 numkeys 不合法", name)
		}
		keys = make([]string, 0, n)
		for _, a := range args[3 : 3+n] {
			keys = append(keys, argString(a))
		}
		return keys, true, nil
	}
	return nil, false, nil
}

// CheckCommand 是守卫的判定:键都带 tag,且全部同 slot。不认识的命令放行。
func CheckCommand(args []interface{}) error {
	keys, ok, err := CommandKeys(args)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return CheckKeys(keys)
}

// CheckKeys 校验一组将要在同一条命令里出现的键。错误文本带出违规的键名(键名不含敏感信息)。
func CheckKeys(keys []string) error {
	for i, key := range keys {
		if !redisslot.HasHashTag(key) {
			return fmt.Errorf("%w: %q", ErrUntaggedKey, key)
		}
		if i > 0 && redisslot.HashSlot(key) != redisslot.HashSlot(keys[0]) {
			return fmt.Errorf("%w: %q 与 %q", ErrCrossSlot, keys[0], key)
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
