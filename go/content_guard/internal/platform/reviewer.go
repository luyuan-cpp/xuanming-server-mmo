// Package platform 实现内容安全服务的外部平台审核层(docs/design/rename-and-content-safety.md §6.5)。
//
// 按玩家渠道路由:
//   - 微信小游戏(channel=wechat_minigame / account 前缀 "wx_"):调用微信 msgSecCheck v2(需 AppID + Secret + openid);
//   - 抖音小游戏(channel=douyin_minigame / account 前缀 "dy_"):调用抖音 antidirt(需 AppID + Secret);
//   - 其余渠道或未配置凭据:直接回 OutcomeSkip,仅走本地词库层。
//
// 失败口径(§6.5):
//   - 名字与公告类场景:同步调用(默认 1500ms);外部不可用回 OutcomeUnavailable,由调用方拒绝并提示稍后重试;
//   - 世界频道:asyncWorld=true 时短超时(默认 250ms)先行判定,超时或不可用回 OutcomePending,
//     由 chat 落库广播后异步调用 CheckPlatformText(ReviewSync,默认 2500ms)补审并在违规时撤回;
//   - 队伍、私聊、私有标签:不走外部平台审核(避免配额耗尽与延迟放大)。
package platform

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	cgpb "proto/content_guard"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	ProviderWeChat = "wechat"
	ProviderDouyin = "douyin"

	defaultSyncTimeout   = 1500 * time.Millisecond
	defaultWorldTimeout  = 250 * time.Millisecond
	defaultAsyncTimeout  = 2500 * time.Millisecond
	defaultLookupTimeout = 150 * time.Millisecond
	defaultIdentityTTL   = 5 * time.Minute
	identityCacheMaxSize = 65536
)

type Outcome uint8

const (
	OutcomeSkip        Outcome = iota // 本场景或本渠道无需外部审核
	OutcomePass                       // 外部平台审核通过
	OutcomeReject                     // 外部平台判定违规
	OutcomeUnavailable                // 外部平台超时或故障
	OutcomePending                    // 世界频道短超时先放行,由调用方异步补审
)

type ReviewResult struct {
	Outcome  Outcome
	Category cgpb.TextCategory
	Provider string
}

// Identity 描述玩家登录渠道与平台用户标识,从 account 前缀解析或由 data_service 查询。
type Identity struct {
	Channel string
	OpenID  string
	Account string
}

// Reviewer 是 server 层调用外部审核的最小接口。
type Reviewer interface {
	Review(ctx context.Context, scene cgpb.TextScene, playerID uint64, account, text string, asyncWorld bool) ReviewResult
	ReviewSync(ctx context.Context, scene cgpb.TextScene, playerID uint64, account, text string) ReviewResult
}

// Adapter 是单个平台(微信 / 抖音 / 商业第三方)的审核实现。
type Adapter interface {
	Provider() string
	Check(ctx context.Context, scene cgpb.TextScene, openID, text string) ReviewResult
}

// IdentityLookup 根据 player_id 查询玩家渠道与 openid。
type IdentityLookup interface {
	LookupIdentity(ctx context.Context, playerID uint64) (Identity, error)
}

// IdentityLookupFunc 函数适配器。
type IdentityLookupFunc func(ctx context.Context, playerID uint64) (Identity, error)

func (f IdentityLookupFunc) LookupIdentity(ctx context.Context, playerID uint64) (Identity, error) {
	return f(ctx, playerID)
}

// ParseChannelAccount 从登录系统写入的账号名解析渠道和 openid。
func ParseChannelAccount(account string) (provider, openID string) {
	switch {
	case strings.HasPrefix(account, "wx_") && len(account) > 3:
		return ProviderWeChat, account[3:]
	case strings.HasPrefix(account, "dy_") && len(account) > 3:
		return ProviderDouyin, account[3:]
	default:
		return "", ""
	}
}

func providerFromIdentity(id Identity) (provider, openID string) {
	switch strings.ToLower(strings.TrimSpace(id.Channel)) {
	case "wechat", "wechat_minigame":
		return ProviderWeChat, id.OpenID
	case "douyin", "douyin_minigame":
		return ProviderDouyin, id.OpenID
	}
	if p, oid := ParseChannelAccount(id.Account); p != "" {
		if id.OpenID != "" {
			oid = id.OpenID
		}
		return p, oid
	}
	return "", ""
}

type identityEntry struct {
	id     Identity
	expiry time.Time
}

type identityCache struct {
	ttl     time.Duration
	timeout time.Duration
	lookup  IdentityLookup
	mu      sync.Mutex
	entries map[uint64]identityEntry
}

func newIdentityCache(lookup IdentityLookup, ttl, timeout time.Duration) *identityCache {
	if ttl <= 0 {
		ttl = defaultIdentityTTL
	}
	if timeout <= 0 {
		timeout = defaultLookupTimeout
	}
	return &identityCache{
		ttl:     ttl,
		timeout: timeout,
		lookup:  lookup,
		entries: make(map[uint64]identityEntry),
	}
}

func (c *identityCache) get(ctx context.Context, playerID uint64) (Identity, error) {
	if c == nil || c.lookup == nil || playerID == 0 {
		return Identity{}, nil
	}
	now := time.Now()
	c.mu.Lock()
	if ent, ok := c.entries[playerID]; ok && now.Before(ent.expiry) {
		c.mu.Unlock()
		return ent.id, nil
	}
	c.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	id, err := c.lookup.LookupIdentity(callCtx, playerID)
	if err != nil {
		return Identity{}, err
	}
	if id.Channel != "" || id.Account != "" || id.OpenID != "" {
		c.mu.Lock()
		if len(c.entries) >= identityCacheMaxSize {
			clear(c.entries)
		}
		c.entries[playerID] = identityEntry{id: id, expiry: now.Add(c.ttl)}
		c.mu.Unlock()
	}
	return id, nil
}

// RouterOptions 装配外部平台审核路由器。
type RouterOptions struct {
	Lookup        IdentityLookup
	SyncTimeout   time.Duration
	WorldTimeout  time.Duration
	AsyncTimeout  time.Duration
	LookupTimeout time.Duration
	Adapters      []Adapter
}

// Router 按场景与玩家渠道分发到对应平台适配器。
type Router struct {
	adapters     map[string]Adapter
	identities   *identityCache
	syncTimeout  time.Duration
	worldTimeout time.Duration
	asyncTimeout time.Duration
}

func NewRouter(opts RouterOptions) *Router {
	adapters := make(map[string]Adapter, len(opts.Adapters))
	for _, a := range opts.Adapters {
		if a != nil && a.Provider() != "" {
			adapters[a.Provider()] = a
		}
	}
	syncT := opts.SyncTimeout
	if syncT <= 0 {
		syncT = defaultSyncTimeout
	}
	worldT := opts.WorldTimeout
	if worldT <= 0 {
		worldT = defaultWorldTimeout
	}
	asyncT := opts.AsyncTimeout
	if asyncT <= 0 {
		asyncT = defaultAsyncTimeout
	}
	var cache *identityCache
	if opts.Lookup != nil {
		cache = newIdentityCache(opts.Lookup, defaultIdentityTTL, opts.LookupTimeout)
	}
	return &Router{
		adapters:     adapters,
		identities:   cache,
		syncTimeout:  syncT,
		worldTimeout: worldT,
		asyncTimeout: asyncT,
	}
}

func isPlatformSkippedScene(scene cgpb.TextScene) bool {
	switch scene {
	case cgpb.TextScene_TEXT_SCENE_PRIVATE_LABEL,
		cgpb.TextScene_TEXT_SCENE_CHAT_TEAM,
		cgpb.TextScene_TEXT_SCENE_CHAT_PRIVATE:
		return true
	}
	return false
}

// Review 根据场景与玩家渠道调用对应平台审核。
// 世界频道在 asyncWorld=true 时只占用短预算 WorldTimeout;超时或暂时不可用返回 OutcomePending 供调用方后台异步复检。
func (r *Router) Review(ctx context.Context, scene cgpb.TextScene, playerID uint64, account, text string, asyncWorld bool) ReviewResult {
	if r == nil || len(r.adapters) == 0 || text == "" || isPlatformSkippedScene(scene) {
		return ReviewResult{Outcome: OutcomeSkip}
	}

	adapter, openID, lookupErr := r.resolveAdapter(ctx, playerID, account)
	if lookupErr != nil {
		if scene == cgpb.TextScene_TEXT_SCENE_CHAT_WORLD {
			logx.Errorf("[content_guard/platform] lookup identity failed for world chat: player=%d err=%v", playerID, lookupErr)
			if asyncWorld {
				return ReviewResult{Outcome: OutcomePending}
			}
			return ReviewResult{Outcome: OutcomeSkip}
		}
		logx.Errorf("[content_guard/platform] lookup identity failed for name/notice review: player=%d err=%v", playerID, lookupErr)
		return ReviewResult{Outcome: OutcomeUnavailable}
	}
	if adapter == nil {
		return ReviewResult{Outcome: OutcomeSkip}
	}
	if adapter.Provider() == ProviderWeChat && openID == "" {
		return ReviewResult{Outcome: OutcomeSkip, Provider: ProviderWeChat}
	}

	if scene == cgpb.TextScene_TEXT_SCENE_CHAT_WORLD {
		callCtx, cancel := context.WithTimeout(ctx, r.worldTimeout)
		defer cancel()
		res := adapter.Check(callCtx, scene, openID, text)
		if res.Outcome == OutcomeUnavailable {
			if asyncWorld {
				return ReviewResult{Outcome: OutcomePending, Provider: res.Provider}
			}
			return ReviewResult{Outcome: OutcomeSkip, Provider: res.Provider}
		}
		return res
	}

	callCtx, cancel := context.WithTimeout(ctx, r.syncTimeout)
	defer cancel()
	return adapter.Check(callCtx, scene, openID, text)
}

// ReviewSync 使用完整异步/同步预算同步执行外部平台审核(CheckPlatformText RPC 使用)。
func (r *Router) ReviewSync(ctx context.Context, scene cgpb.TextScene, playerID uint64, account, text string) ReviewResult {
	if r == nil || len(r.adapters) == 0 || text == "" || isPlatformSkippedScene(scene) {
		return ReviewResult{Outcome: OutcomeSkip}
	}
	adapter, openID, lookupErr := r.resolveAdapter(ctx, playerID, account)
	if lookupErr != nil {
		logx.Errorf("[content_guard/platform] lookup identity failed during sync platform review: player=%d err=%v", playerID, lookupErr)
		return ReviewResult{Outcome: OutcomeUnavailable}
	}
	if adapter == nil {
		return ReviewResult{Outcome: OutcomeSkip}
	}
	if adapter.Provider() == ProviderWeChat && openID == "" {
		return ReviewResult{Outcome: OutcomeSkip, Provider: ProviderWeChat}
	}
	budget := r.syncTimeout
	if scene == cgpb.TextScene_TEXT_SCENE_CHAT_WORLD {
		budget = r.asyncTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return adapter.Check(callCtx, scene, openID, text)
}

func (r *Router) resolveAdapter(ctx context.Context, playerID uint64, account string) (Adapter, string, error) {
	if account != "" {
		if p, oid := ParseChannelAccount(account); p != "" {
			if a := r.adapters[p]; a != nil {
				return a, oid, nil
			}
			return nil, "", nil
		}
	}
	if playerID != 0 && r.identities != nil {
		id, err := r.identities.get(ctx, playerID)
		if err != nil {
			return nil, "", err
		}
		p, oid := providerFromIdentity(id)
		if p != "" {
			return r.adapters[p], oid, nil
		}
		return nil, "", nil
	}
	if playerID == 0 && account == "" {
		if dy, ok := r.adapters[ProviderDouyin]; ok {
			return dy, "", nil
		}
	}
	return nil, "", nil
}

func newDefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}
