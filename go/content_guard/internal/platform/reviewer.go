// Package platform 实现内容安全服务的外部平台审核层(docs/design/rename-and-content-safety.md §6.5)。
//
// 按玩家最近一次登录的渠道路由(渠道与 openid 都从目录查,见 providerFromIdentity):
//   - 微信小游戏(channel=wechat_minigame):调用微信 msgSecCheck v2(需 AppID + Secret + 本小游戏下的 openid);
//   - 抖音小游戏(channel=douyin_minigame):调用抖音 antidirt(需 AppID + Secret);
//   - 其余渠道(含 App 端的微信登录)或未配置凭据:直接回 OutcomeSkip,仅走本地词库层。
//
// 失败口径(§6.5):
//   - 名字与公告类场景:同步调用(默认 1500ms);外部不可用回 OutcomeUnavailable,由调用方拒绝并提示稍后重试;
//   - 世界频道:asyncWorld=true 时短超时(默认 250ms)先行判定,超时或不可用回 OutcomePending,
//     由 chat 落库广播后异步调用 CheckPlatformText(ReviewSync,默认 2500ms)补审并在违规时撤回;
//   - 队伍、私聊、私有标签:不走外部平台审核(避免配额耗尽与延迟放大)。
package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	cgpb "proto/content_guard"

	"github.com/zeromicro/go-zero/core/logx"

	"content_guard/internal/metrics"
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

// Identity 描述玩家最近一次登录的渠道与平台用户标识,由 data_service 的账号目录给出。
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

// IdentityLookup 查填写者的渠道与 openid:有 player_id 按角色查;player_id 为 0 时按账号查(建角时角色还没发号)。
type IdentityLookup interface {
	LookupIdentity(ctx context.Context, playerID uint64, account string) (Identity, error)
}

// IdentityLookupFunc 函数适配器。
type IdentityLookupFunc func(ctx context.Context, playerID uint64, account string) (Identity, error)

func (f IdentityLookupFunc) LookupIdentity(ctx context.Context, playerID uint64, account string) (Identity, error) {
	return f(ctx, playerID, account)
}

// 渠道名是 login 记进目录的 auth_type(go/login/internal/logic/pkg/auth/minigame_providers.go)。
const (
	channelWeChatMiniGame = "wechat_minigame"
	channelDouyinMiniGame = "douyin_minigame"
)

// providerFromIdentity 按最近一次登录的渠道选平台,只认两个小游戏渠道,用户标识只用目录里记的那个。
//
// 微信的审核接口要的是「本小游戏 AppID 下的 openid」。账号名(wx_ 加后缀)的后缀是 unionid;App 端的微信登录
// (channel=wechat)记下的 openid 属于 App 那个 AppID。拿这两个去调,平台都按 openid 无效拒掉,而名字类场景对平台不可用的
// 口径是拒绝:这些玩家会一直起不了名、建不了角。所以不从账号名猜,也不认 App 渠道。
// App 渠道走哪家第三方还没定(设计 §6.5),定之前只走本地词库。
func providerFromIdentity(id Identity) (provider, openID string) {
	switch id.Channel {
	case channelWeChatMiniGame:
		return ProviderWeChat, id.OpenID
	case channelDouyinMiniGame:
		return ProviderDouyin, id.OpenID
	}
	return "", ""
}

// identityKey:有 player_id 时只按它;account 只在 player_id 为 0 时填。
type identityKey struct {
	playerID uint64
	account  string
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
	entries map[identityKey]identityEntry
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
		entries: make(map[identityKey]identityEntry),
	}
}

func (c *identityCache) get(ctx context.Context, playerID uint64, account string) (Identity, error) {
	if c == nil || c.lookup == nil || (playerID == 0 && account == "") {
		return Identity{}, nil
	}
	key := identityKey{playerID: playerID}
	if playerID == 0 {
		key.account = account
	}
	now := time.Now()
	c.mu.Lock()
	if ent, ok := c.entries[key]; ok && now.Before(ent.expiry) {
		c.mu.Unlock()
		return ent.id, nil
	}
	c.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	id, err := c.lookup.LookupIdentity(callCtx, key.playerID, key.account)
	if err != nil {
		return Identity{}, err
	}
	if id.Channel != "" || id.Account != "" || id.OpenID != "" {
		c.mu.Lock()
		if len(c.entries) >= identityCacheMaxSize {
			clear(c.entries)
		}
		c.entries[key] = identityEntry{id: id, expiry: now.Add(c.ttl)}
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
				// 先行的短预算没等到不算平台故障:chat 落库后用完整预算补审(ReviewSync),那一次再记结局。
				metrics.ObservePlatformReview(adapter.Provider(), metrics.PlatformOutcomeDeferred)
				return ReviewResult{Outcome: OutcomePending, Provider: res.Provider}
			}
			observePlatformReview(adapter.Provider(), res.Outcome)
			return ReviewResult{Outcome: OutcomeSkip, Provider: res.Provider}
		}
		observePlatformReview(adapter.Provider(), res.Outcome)
		return res
	}

	callCtx, cancel := context.WithTimeout(ctx, r.syncTimeout)
	defer cancel()
	res := adapter.Check(callCtx, scene, openID, text)
	observePlatformReview(adapter.Provider(), res.Outcome)
	return res
}

// observePlatformReview 记一次平台调用的最终结局(platform_reviews_total)。世界频道的平台不可用会被改成先放行、后台补审,
// 补审再不可用只打日志,所以不在这里记的话,平台挂了没有任何指标看得见(docs/design/rename-and-content-safety.md §6.9)。
func observePlatformReview(provider string, outcome Outcome) {
	name := metrics.PlatformOutcomeSkip
	switch outcome {
	case OutcomePass:
		name = metrics.PlatformOutcomePass
	case OutcomeReject:
		name = metrics.PlatformOutcomeReject
	case OutcomeUnavailable:
		name = metrics.PlatformOutcomeUnavailable
	}
	metrics.ObservePlatformReview(provider, name)
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
	res := adapter.Check(callCtx, scene, openID, text)
	observePlatformReview(adapter.Provider(), res.Outcome)
	return res
}

// resolveAdapter 选出审这位填写者的平台和要带给平台的用户标识;返回 nil 适配器 = 这位填写者不走平台审核。
func (r *Router) resolveAdapter(ctx context.Context, playerID uint64, account string) (Adapter, string, error) {
	if playerID == 0 && account == "" {
		// 没有填写者的系统文本:只有不需要用户标识的平台能审。
		if dy, ok := r.adapters[ProviderDouyin]; ok {
			return dy, "", nil
		}
		return nil, "", nil
	}
	if r.identities == nil {
		return nil, "", nil
	}
	id, err := r.identities.get(ctx, playerID, account)
	if err != nil {
		return nil, "", err
	}
	provider, openID := providerFromIdentity(id)
	if adapter, ok := r.adapters[provider]; ok {
		return adapter, openID, nil
	}
	return nil, "", nil
}

func newDefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// withoutURL 去掉 net/http 请求错误里的 URL,只留操作和原因。http.Client 的错误是 *url.Error,它的文本带着完整的请求 URL;
// 平台接口把调用凭据放在查询串里(微信的 access_token),原样打进日志就是把凭据写进了日志。
func withoutURL(err error) error {
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		return fmt.Errorf("%s: %w", requestErr.Op, requestErr.Err)
	}
	return err
}
