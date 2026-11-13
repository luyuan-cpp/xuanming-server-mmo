// Package sessionfence 是 login 的 gate 连接会话栅栏(docs/design/client-request-ordering.md §6.2 / O12)。
//
// 它要关掉的是三种与「gate 连接已关闭 / 已离开游戏」交错的竞态,gate 侧的排队都管不住:
//
//   - Disconnect 抢跑仍在途的 Login:Login 晚落地,留下没人清理的 login_session 与设备集条目;
//   - EnterGame 已受理(回包只表示受理)、后台链还没跑完时断线:gate 合成的 Disconnect 带 player_id = 0,
//     原先在 playerID == 0 处早退;后台链随后写入 ONLINE 会话,却没有任何租约,玩家永久卡在线。
//     这一条**顺序正确也会发生**;
//   - EnterGame 之后的 LeaveGame:LeaveGame 不拿玩家锁,与后台链并发,后台链在 LeaveGame 之后又把会话与位置写回去;
//     同一条连接连发两次 EnterGame(换角 / 重试),两条后台链都跑完,一个会话绑了两个角色。
//
// 记录是一个 Redis hash,键按「gate 连接」建:gate_instance_id(每次 gate 进程启动生成的 UUID)+
// session_incarnation(gate 进程内单调、不回绕)。不按 session_id 建键:它在 gate 重启后从头发号、17 位序号也会
// 回绕,上一个进程留下的关闭标记会误拒新连接。字段:
//
//	closed  "1" = 这条连接已断开(Disconnect 写)
//	gen     LeaveGame 的次数(LeaveGame 递增);EnterGame 入口记下当时的值,后台链据此识别「期间已离开游戏」
//	player  这条连接上已绑定的玩家(后台链绑定时写,LeaveGame 删)
//	left_player  最近一次 LeaveGame 解除的玩家(Leave 写),只给 Leave 的重放返回同一个要收尾的玩家,别的操作不读
//
// 正确性靠两段原子脚本的「双边握手」:Close 先写 closed、再读 player;BindPlayer 先查 closed、再写 player。
// 两段都在同一个 Redis 上原子执行,天然可串行化:关闭在前,绑定一定看到 closed,由后台链补发断线租约;
// 绑定在前,关闭一定读到 player,由 Disconnect 补发。两种顺序都收敛到「带租约的 DISCONNECTING」。
//
// 寿命分两种(2026-11-05,docs/design/data-overwrite-rollback-audit-20261018.md §4 login 第 2 条):
//
//   - 没绑定玩家、或连接已关闭的记录:调用方给的 ttl(TTLFor(后台链总预算)),只需比还可能读它的在途操作活得久;
//   - 绑定了玩家、连接未关闭的记录:绑定只由 Leave(离开游戏)与 Close(断线)解除,两者把记录收回到 ttl。
//     记录本身用 boundRecordTTL 兜底,登录提交(登录态续期)、进游戏入口、绑定都把它续满;连接活着期间 gate 还会
//     定期续租(Renew,docs/design/gate-online-identity.md §10.4),所以活着的连接不会因为兜底寿命到期而丢绑定。
//
// 以前绑定与其余字段共用 ttl:绑定后 6 分钟内没有 LeaveGame / Disconnect,player 随记录一起过期,
// 同一连接再 EnterGame 就能绑上第二个角色,断线时也读不到绑定的玩家。
//
// 记录在登录域(LoginRedis,生产是 Redis Cluster)。键带连接身份的 hash tag,所以 CommitLogin 只能碰本连接的栅栏与
// login_session;账号设备集按账号分布,在另一个槽,由调用方在 CommitLogin 成功之后单独写(docs/design/redis-player-store-cluster.md §10.2)。
package sessionfence

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	login_proto_common "proto/common/base"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
)

// Key 标识一条 gate 连接。
type Key struct {
	GateInstanceID string
	Incarnation    uint64
}

// KeyOf 从 gate 带来的 SessionDetails 取栅栏键。没有 session_incarnation 或缺 gate_instance_id 时返回 ok = false:
// 这种连接没有全局唯一的身份,调用方必须拒绝,不存在跳过栅栏的路径。
func KeyOf(details *login_proto_common.SessionDetails) (Key, bool) {
	if details == nil || details.GetSessionIncarnation() == 0 || details.GetGateInstanceId() == "" {
		return Key{}, false
	}
	return Key{GateInstanceID: details.GetGateInstanceId(), Incarnation: details.GetSessionIncarnation()}, true
}

// String 返回这条 gate 连接的全局唯一标识。栅栏键、login_session 键与设备集成员都用它,同一条连接的记录始终指向同一个身份。
func (k Key) String() string {
	return redisslot.LoginConnectionID(k.GateInstanceID, k.Incarnation)
}

func (k Key) redisKey() string {
	return redisslot.SessionFenceKey(k.String())
}

// TTLFor 由 EnterGame 后台链的总预算派生**没绑定玩家或已关闭**的记录的寿命:记录必须比所有还可能读它的操作活得久,
// 最长的是后台链本身;多留 1 分钟吸收时钟与调度误差。不用 SessionExpireMin(那是登录态的寿命,不是在途操作的寿命)。
// 绑定了玩家的记录不用它,见 boundRecordTTL。
func TTLFor(enterGameChainBudget time.Duration) time.Duration {
	return enterGameChainBudget + time.Minute
}

// boundRecordTTL 是绑定了玩家、连接未关闭的记录的兜底寿命。绑定不靠它到期,只由 Leave / Close 解除;
// 它只回收 Disconnect 没送到(gate 进程崩溃、断线时 login 不可用)之后再没人会读的记录,让这类记录有界。
// 这条连接上的栅栏操作(登录提交即登录态续期、进游戏入口、绑定)都把它续满,gate 的定期续租(Renew)也续满。
//
// gate 连接没有寿命上限,所以活着的连接靠续租保住绑定(2026-11-12 起;之前一条连接在游戏里连续停满 boundRecordTTL、
// 期间没有任何栅栏操作时,login 这一层会丢绑定)。续租间隔是登录态寿命的三分之一,要连续失败满 boundRecordTTL 才会到期;
// 真到了那一步,同一连接换角仍由 gate 会话层拒绝(cpp/nodes/gate/handler/rpc/login_admission.h EnterGameRoleSwitchRejectTip,
// gate 的会话与连接同寿命)。取一周:要回收的只是已死 gate 进程的记录,内存代价可以忽略。
const boundRecordTTL = 7 * 24 * time.Hour

// Outcome 是栅栏判定的结果。
type Outcome uint8

const (
	// OutcomeOK:连接仍在、没有离开过游戏、没有绑定别的角色。
	OutcomeOK Outcome = iota
	// OutcomeClosed:连接已断开。
	OutcomeClosed
	// OutcomeLeft:EnterGame 入口之后发生过 LeaveGame。
	OutcomeLeft
	// OutcomeOtherPlayer:这条连接上已经绑定了另一个角色。
	OutcomeOtherPlayer
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeClosed:
		return "closed"
	case OutcomeLeft:
		return "left"
	case OutcomeOtherPlayer:
		return "other_player"
	}
	return "unknown"
}

// ErrClosed:CommitLogin 时连接已断开,Login 不写任何状态。
var ErrClosed = errors.New("gate connection already closed")

// KEYS = {栅栏, login_session};ARGV = {account, sessionTTL(ms), boundRecordTTL(ms)}。两个键带同一个连接 tag,必然同槽。
var commitLoginScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'closed') == '1' then
  return 0
end
redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2])
if redis.call('HEXISTS', KEYS[1], 'player') == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

// CommitLogin 原子地「检查连接未关闭 → 写 login_session」。检查与写入必须在同一段脚本里,否则仍有
// 「检查通过、Disconnect 插进来、再写入」的窗口。连接已关闭返回 ErrClosed,什么都不写。
//
// 登录态续期时同步续栅栏:这条连接上已绑定了玩家(游戏里在同一连接上重新 Login)时,记录续满 boundRecordTTL,
// 绑定不会因为登录态续上了、栅栏却到期而丢。没有记录或没绑定玩家时不建记录、不改寿命:那样的记录只服务在途操作。
//
// 设备集按账号分布、不同槽,进不了这段脚本,由调用方在成功之后写(loginsession.RegisterDevice)。两步之间的结局见
// loginsession 包注释,都能自愈。
func CommitLogin(ctx context.Context, rdb redis.UniversalClient, key Key, account string, sessionTTL time.Duration) error {
	// 脚本里是 SET ... PX,0 / 负值会被 Redis 以 invalid expire time 拒绝。这里显式报错,让误配置在第一次登录时就暴露。
	if sessionTTL <= 0 {
		return fmt.Errorf("session fence commit login: non-positive session ttl %s", sessionTTL)
	}
	res, err := commitLoginScript.Run(ctx, rdb, []string{key.redisKey(), redisslot.LoginSessionKey(key.String())},
		account, sessionTTL.Milliseconds(), boundRecordTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return err
	}
	if res == 0 {
		return ErrClosed
	}
	return nil
}

var closeScript = redis.NewScript(`
redis.call('HSET', KEYS[1], 'closed', '1')
redis.call('PEXPIRE', KEYS[1], ARGV[1])
local p = redis.call('HGET', KEYS[1], 'player')
if p then
  return p
end
return '0'
`)

// Close 标记连接已断开,并返回这条连接上已绑定的玩家(没有为 0)。Disconnect 在清理 login_session 之前调用;
// 会话里的 player_id 为 0(BindSession 还没到 gate)时,用这里返回的玩家补发断线租约。
// 关闭是终态:记录收回到 ttl(绑定随之解除),之后只剩在途操作会读它。
func Close(ctx context.Context, rdb redis.UniversalClient, key Key, ttl time.Duration) (uint64, error) {
	raw, err := closeScript.Run(ctx, rdb, []string{key.redisKey()}, ttl.Milliseconds()).Text()
	if err != nil {
		return 0, err
	}
	return parsePlayer(raw)
}

// 入口先建记录(gen 缺省写 0)并续期,再读:TTL 从最后一次写入算起,只读不续期时,入口记下 gen = N 之后
// 记录可能在后台链绑定之前过期 —— 绑定读到 gen = 0 误判「期间已离开」;或过期后又来一次 LeaveGame,
// HINCRBY 从空记录重建出 gen = 1 恰好等于入口的旧值,这次离开被漏掉。续期之后记录至少从入口起活一个 ttl,
// 而 ttl = TTLFor(后台链总预算) 大于后台链本身。
//
// 续期按记录状态取寿命(ARGV = {ttl(ms), boundRecordTTL(ms)}):绑定了玩家且连接未关闭 → boundRecordTTL;其余 → ttl。
// 绑定的记录不能续成 ttl:同一角色重进、被拒的换角都会走到这里,续成 ttl 就把绑定的寿命压回 ttl,到期后换角照样放行。
var checkEnterGameScript = redis.NewScript(`
redis.call('HSETNX', KEYS[1], 'gen', '0')
local v = redis.call('HMGET', KEYS[1], 'closed', 'gen', 'player')
if v[3] and v[1] ~= '1' then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
else
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return {v[1] or '', v[2] or '0', v[3] or ''}
`)

// CheckEnterGame 是 EnterGame 入口的判定:连接已关闭、或已经绑定了别的角色就拒绝;否则返回当前的 gen,
// 后台链绑定时用它识别「期间已离开游戏」。ttl 与其余操作同一个值(TTLFor 派生):没绑定玩家的记录续期到 ttl,
// 绑定了玩家的续满 boundRecordTTL(见包注释「寿命」)。
func CheckEnterGame(ctx context.Context, rdb redis.UniversalClient, key Key, playerID uint64, ttl time.Duration) (Outcome, uint64, error) {
	vals, err := checkEnterGameScript.Run(ctx, rdb, []string{key.redisKey()},
		ttl.Milliseconds(), boundRecordTTL.Milliseconds(),
	).StringSlice()
	if err != nil {
		return OutcomeOK, 0, err
	}
	if len(vals) != 3 {
		return OutcomeOK, 0, fmt.Errorf("session fence check: unexpected reply length %d", len(vals))
	}
	if vals[0] == "1" {
		return OutcomeClosed, 0, nil
	}
	gen, err := parseCounter(vals[1])
	if err != nil {
		return OutcomeOK, 0, err
	}
	bound, err := parsePlayer(vals[2])
	if err != nil {
		return OutcomeOK, 0, err
	}
	if bound != 0 && bound != playerID {
		return OutcomeOtherPlayer, gen, nil
	}
	return OutcomeOK, gen, nil
}

// ARGV = {入口记下的 gen, playerID, boundRecordTTL(ms)}。
var bindPlayerScript = redis.NewScript(`
local v = redis.call('HMGET', KEYS[1], 'closed', 'gen', 'player')
if v[1] == '1' then
  return 'closed'
end
local gen = v[2] or '0'
if gen ~= ARGV[1] then
  return 'left'
end
if v[3] and v[3] ~= ARGV[2] then
  return 'other_player'
end
redis.call('HSET', KEYS[1], 'player', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 'ok'
`)

// BindPlayer 是后台链会话持久化成功之后、向 gate 发 BindSession 之前的绑定点:先查 closed 再写 player
// (与 Close 构成双边握手)。绑定成功后记录续满 boundRecordTTL:绑定只由 Leave / Close 解除,不随在途操作的 ttl 到期
// (包注释「寿命」),所以这里不收 ttl。返回 OutcomeOK 才算绑定成功;其余结果不写记录,由调用方补偿:
//
//	OutcomeClosed:连接已断开 → 补发断线租约(markPlayerSessionDisconnecting)后中止;
//	OutcomeLeft / OutcomeOtherPlayer → 删除本次写入的会话(MarkOffline CAS)后中止。
func BindPlayer(ctx context.Context, rdb redis.UniversalClient, key Key, playerID, expectGen uint64) (Outcome, error) {
	raw, err := bindPlayerScript.Run(ctx, rdb, []string{key.redisKey()},
		strconv.FormatUint(expectGen, 10), strconv.FormatUint(playerID, 10), boundRecordTTL.Milliseconds(),
	).Text()
	if err != nil {
		return OutcomeOK, err
	}
	return parseOutcome(raw), nil
}

// Recheck 是后台链发 EnterScene 之前的复查:连接已关闭或期间离开过游戏就不再进场,
// 防止在 DISCONNECTING / 已离线之后再写入位置。
func Recheck(ctx context.Context, rdb redis.UniversalClient, key Key, expectGen uint64) (Outcome, error) {
	vals, err := rdb.HMGet(ctx, key.redisKey(), "closed", "gen").Result()
	if err != nil {
		return OutcomeOK, err
	}
	if asString(vals[0]) == "1" {
		return OutcomeClosed, nil
	}
	gen, err := parseCounter(asString(vals[1]))
	if err != nil {
		return OutcomeOK, err
	}
	if gen != expectGen {
		return OutcomeLeft, nil
	}
	return OutcomeOK, nil
}

// 解除绑定时把玩家记进 left_player:同一次离开被重放(脚本已执行、回包丢了,Leave 报错、LeaveGame 回错误之后客户端重发;
// 登录域客户端自己不重发,shared/playerredis MaxRetries=-1,换成会重发的客户端也落在这里)时 player 已经删掉,
// 返回 left_player,调用方照样拿到要收尾的玩家。
var leaveScript = redis.NewScript(`
redis.call('HINCRBY', KEYS[1], 'gen', 1)
local p = redis.call('HGET', KEYS[1], 'player')
if p then
  redis.call('HSET', KEYS[1], 'left_player', p)
  redis.call('HDEL', KEYS[1], 'player')
else
  p = redis.call('HGET', KEYS[1], 'left_player')
end
redis.call('PEXPIRE', KEYS[1], ARGV[1])
if p then
  return p
end
return '0'
`)

// Leave 记一次离开游戏(gen + 1)并解除绑定,返回原先绑定的玩家(没有为 0)。LeaveGame 在清理会话之前调用;
// 会话里的 player_id 为 0 时,用这里返回的玩家删除会话。解除绑定后记录收回到 ttl:gen 只需活过还在途的后台链。
//
// 可以重放(F4,client-request-ordering.md §6.2):Leave 报错时 LeaveGame 不收尾、回错误,客户端重发;脚本可能已经执行、
// 只是回包丢了。重放时 gen 再 + 1(后台链只比较「是否等于入口记下的值」,多加一次无害),记录里已没有绑定时返回
// 上一次解除的玩家(left_player),不会因为第一次已经删掉 player 而漏掉收尾。之后又绑定了别的角色时返回新绑定的那个。
// 对上一次解除的玩家再收尾一次是安全的:下游 DeleteSession 按 session_id + gate 实例做 CAS,对已不在本连接上的玩家是 no-op;
// 它若此刻正被本连接的后台链写成在线(写会话之后、绑定之前),这次离开递增了 gen,后台链在绑定点照样会删掉它。
func Leave(ctx context.Context, rdb redis.UniversalClient, key Key, ttl time.Duration) (uint64, error) {
	raw, err := leaveScript.Run(ctx, rdb, []string{key.redisKey()}, ttl.Milliseconds()).Text()
	if err != nil {
		return 0, err
	}
	return parsePlayer(raw)
}

// KEYS = {栅栏, login_session};ARGV = {sessionTTL(ms), boundRecordTTL(ms)}。两个键带同一个连接 tag,必然同槽。
// 返回 {还有没有被续期的记录(1 / 0), 登录态里的账号(没有登录态为空串)}。
var renewScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'closed') == '1' then
  return {0, ''}
end
local kept = 0
local account = redis.call('GET', KEYS[2])
if account then
  redis.call('PEXPIRE', KEYS[2], ARGV[1])
  kept = 1
end
if redis.call('HEXISTS', KEYS[1], 'player') == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  kept = 1
end
return {kept, account or ''}
`)

// Renewal 是 Renew 的结果。
type Renewal struct {
	// Kept:这条连接还有记录(登录态,或已绑定的角色)并已续期。false = 连接已关闭,或两样都没有,什么都没写。
	Kept bool
	// Account:登录态里的账号;登录态已不存在时为空。调用方据此续账号设备集(不同槽,进不了这段脚本)。
	Account string
}

// Renew 为仍然连着的连接续期它的按连接记录(docs/design/gate-online-identity.md §10.4),由 gate 定期代发的 RenewSession 调用:
// 登录态续到 sessionTTL;已绑定角色、连接未关闭的栅栏记录续满 boundRecordTTL。只续已有的记录,从不新建:
// 登录态只能由 Login 建(CommitLogin),绑定只能由后台链建(BindPlayer)。
//
// 先查 closed 再续,与 CommitLogin 同一个理由:Disconnect 已经处理过的连接,迟到的续租不得把它的登录态续活。
// 检查与续期在同一段脚本里,没有「检查通过、Disconnect 插进来、再续期」的窗口。
func Renew(ctx context.Context, rdb redis.UniversalClient, key Key, sessionTTL time.Duration) (Renewal, error) {
	// PEXPIRE 的 0 / 负值会让 Redis 立刻删键:SessionExpireMin 漏配时续租会变成清登录态。这里显式报错,什么都不写。
	if sessionTTL <= 0 {
		return Renewal{}, fmt.Errorf("session fence renew: non-positive session ttl %s", sessionTTL)
	}
	vals, err := renewScript.Run(ctx, rdb, []string{key.redisKey(), redisslot.LoginSessionKey(key.String())},
		sessionTTL.Milliseconds(), boundRecordTTL.Milliseconds(),
	).Slice()
	if err != nil {
		return Renewal{}, err
	}
	if len(vals) != 2 {
		return Renewal{}, fmt.Errorf("session fence renew: unexpected reply length %d", len(vals))
	}
	kept, ok := vals[0].(int64)
	if !ok {
		return Renewal{}, fmt.Errorf("session fence renew: unexpected reply type %T", vals[0])
	}
	return Renewal{Kept: kept == 1, Account: asString(vals[1])}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func parseCounter(raw string) (uint64, error) {
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseUint(raw, 10, 64)
}

func parsePlayer(raw string) (uint64, error) {
	if raw == "" || raw == "0" {
		return 0, nil
	}
	return strconv.ParseUint(raw, 10, 64)
}

func parseOutcome(raw string) Outcome {
	switch raw {
	case "ok":
		return OutcomeOK
	case "closed":
		return OutcomeClosed
	case "left":
		return OutcomeLeft
	case "other_player":
		return OutcomeOtherPlayer
	}
	// 脚本只会回上面四种;回别的说明脚本被改坏了,按最保守的「已关闭」处理(fail-closed)。
	return OutcomeClosed
}
