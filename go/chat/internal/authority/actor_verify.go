package authority

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"shared/accountdirectory"
	"shared/placement"
	"shared/roleidentity"
)

// 聊天的身份校验(docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
//
// 旧做法是每条消息都在发送事务里 LockRoleRoutes 再对 account_role 加 FOR UPDATE。聊天是全服事务量最大的路径,
// 而且把聊天库和角色目录绑进了同一个事务,拆多集群时这笔事务会跨集群(§8.2)。现在校验放在发送事务之外:
// 普通读 player_route 和 account_role,成功结果按 (actor, account, owner_revision) 在进程内缓存 TTL 秒。
//
// 代价,聊天不是资产,这些都可以接受:
//   - 角色被挂牌或过户之后,旧会话最多还能发 TTL 秒的聊天、拉 TTL 秒的历史。校验不再和过户串行,所以 TTL=0 也有
//     一条消息的窗口:校验读到旧归属之后、提交之前过户完成,这条消息照样落库,记在角色名下,新主能看到。
//   - 同一 request_id 的并发首发不再靠行锁排队,由主键裁决,输家撞 1062 转读赢家的原件(Store.Send)。
//   - 限流从精确变成近似。计数和插入之间没有锁,同一玩家同时在途的请求可能都看到未满,超额上限是这名玩家同时在途的
//     请求数。这是第二道闸,第一道是 gate 按会话和消息号限频;要精确就得给每个玩家再锁一行,不值。
const (
	// actorVerifyMaxEntries 是缓存条目上限。一条大约 100~200 字节,65536 条约十几 MB。
	// 超了就整张清掉:实现最简单,代价只是这一刻在说话的玩家各多读一次权威行,不影响对错。
	actorVerifyMaxEntries = 1 << 16
)

// actorVerifyKey 是一次校验的身份。字段都来自原登录会话,不是当前归属:过户之后 owner_revision 变了,
// 新主的身份不会命中旧条目,旧会话的条目到期后重读才被拒。
type actorVerifyKey struct {
	player   uint64
	account  string
	revision uint64
}

// actorVerifyCache 是校验结果的进程内缓存,只记成功:失败不写入,改好之后下一次就能通过。
// 并发安全;所有时间都取注入的单调时钟,墙钟回拨不会让条目复活或提前失效。
type actorVerifyCache struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Duration

	mu      sync.Mutex
	expires map[actorVerifyKey]time.Duration
}

func newActorVerifyCache(ttl time.Duration, maxEntries int, now func() time.Duration) *actorVerifyCache {
	return &actorVerifyCache{ttl: ttl, maxEntries: maxEntries, now: now, expires: map[actorVerifyKey]time.Duration{}}
}

// monotonicSinceStart 返回进程内单调时钟:time.Since 用的是 time.Now 自带的单调读数,与墙钟无关。
func monotonicSinceStart() func() time.Duration {
	start := time.Now()
	return func() time.Duration { return time.Since(start) }
}

// verify 判断这个身份能不能用:缓存未过期直接放行,否则调 read 读权威行,read 成功才写缓存。
// 到期时间从读之前算起,所以「读到旧归属到过期」不会超过 TTL。ttl <= 0 时每次都读。
func (c *actorVerifyCache) verify(ctx context.Context, key actorVerifyKey, read func(context.Context, actorVerifyKey) error) error {
	if c.ttl <= 0 {
		return read(ctx, key)
	}
	started := c.now()
	if c.fresh(key, started) {
		return nil
	}
	if err := read(ctx, key); err != nil {
		return err
	}
	c.remember(key, started+c.ttl)
	return nil
}

func (c *actorVerifyCache) fresh(key actorVerifyKey, now time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.expires[key]
	if !ok {
		return false
	}
	if now >= expiry {
		delete(c.expires, key)
		return false
	}
	return true
}

func (c *actorVerifyCache) remember(key actorVerifyKey, expiry time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.expires[key]; !exists && len(c.expires) >= c.maxEntries {
		clear(c.expires)
	}
	c.expires[key] = expiry
}

// verifyActor 校验原会话身份,返回 actor 和它的归属。聊天只修改/授权 caller;peer 只是会话选择器,删除 peer 不能抹掉 caller 已有历史。
func (s *Store) verifyActor(ctx context.Context) (uint64, role, error) {
	original, ok := roleidentity.Original(ctx)
	if !ok || original.Role.GetPlayerId() == 0 {
		return 0, role{}, ErrOriginal
	}
	key := actorVerifyKey{player: original.Role.PlayerId, account: original.Account, revision: original.OwnerRevision}
	if err := s.actors.verify(ctx, key, s.readActor); err != nil {
		return 0, role{}, err
	}
	return key.player, role{account: key.account, revision: key.revision}, nil
}

// readActor 普通读路由行和角色目录,不取任何锁。两条读放进一个短事务只为只过一次写者闸门,读完回滚即可,没有东西要提交;
// 读已提交下每条语句各取快照,两行之间没有一致性要求:任何一行不允许就拒绝。
func (s *Store) readActor(ctx context.Context, key actorVerifyKey) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = placement.CheckRoleRoute(ctx, tx, s.roleSchema, key.player); err != nil {
		return err
	}
	var account []byte
	var revision, listing uint64
	if err = tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+s.roleSchema+"`.account_role WHERE player_id=?", key.player).Scan(&account, &revision, &listing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOriginal
		}
		return err
	}
	if !accountdirectory.ValidAccount(string(account)) || revision == 0 || string(account) != key.account || revision != key.revision || listing != 0 {
		return ErrOriginal
	}
	return nil
}
