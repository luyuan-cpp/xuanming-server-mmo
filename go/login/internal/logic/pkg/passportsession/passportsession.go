// Package passportsession 把官方通行证里的账号变化(改密、找回密码、封禁、删除)落到游戏登录上
// (docs/design/official-passport-registration.md §3.6、§3.7)。
//
// 三个入口共用同一套「查一次通行证、按结论吊销、吊销了就踢线」:
//   - CheckLogin:通行证登录时。身份令牌签发在改密之前、或用户被封被删,拒绝这次登录;改密之前的旧登录一并吊销。
//   - CheckRefresh:刷新令牌前(轮换之前)。这一家族的登录早于改密、或用户被封被删,不续期,吊销并踢线。
//   - HandleEvent:通行证回调(§3.7)。按回调点名的用户现查一次,同样吊销并踢线;回调的内容只用来找人,不拿来下结论。
//
// 吊销的口径(settle):被删、被封吊销账号的全部令牌;改过密码只吊销身份令牌签发时刻不晚于改密时刻的家族,
// 改密之后重新登录的设备不受影响。吊销了东西就把账号名下在线的角色踢下线(gate 弹 kLoginPassportCredentialChanged)。
//
// 故障口径:通行证核对不了时登录、刷新照常放行并计 error(通行证故障不能把在线玩家都踢下线);吊销失败时刷新不续期
// (旧令牌还在,下一次再来),回调回错误让通行证重投;踢线失败只计数(令牌已吊销,连接断开之后进不来)。
package passportsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

const (
	// lookupTimeout 是问一次通行证的时限。管理接口在集群内网,正常十几毫秒就回;超过它按核对不了处理,
	// 不把登录、刷新拖到 RPC 超时(login 缺省 4s)。
	lookupTimeout = time.Second
	// kickTimeout 是踢线(读角色列表、查会话、发 gate 命令)的时限。与请求的取消脱钩:客户端放弃等待不该让踢线半途而废。
	kickTimeout = 5 * time.Second
	// maxEventBytes 是通行证回调请求体的上限(网关先按同一上限截住)。Casdoor 的回调是一条记录加一名用户,远小于它。
	maxEventBytes = 64 << 10
	// maxEventUsers 是一次回调最多核对几名用户:回调里能点到的人只有操作人、目标用户与请求体里的那一个。
	maxEventUsers = 4
	// builtInOrganization 是 Casdoor 的管理组织,玩家不在里面;回调点到它的用户(管理员自己)不核对。
	builtInOrganization = "built-in"
)

// 指标 label。phase 是入口,result 是结论(auth.PassportSession* 的取值之外再加下面几个),上线后不改名。
const (
	phaseLogin   = "login"
	phaseRefresh = "refresh"
	phaseEvent   = "event"

	resultValid        = "valid"         // 照常
	resultError        = "error"         // 通行证核对不了,照常放行
	resultRevokeFailed = "revoke_failed" // 吊销没成功:刷新不续期,回调重投,登录照常
	resultKickFailed   = "kick_failed"   // 令牌已吊销,踢线没成功(与本次结论各计一次)
	resultIgnored      = "ignored"       // 回调里找不到要核对的用户(不是本组织的,或只剩名字而用户已删)
)

var checkTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "passport",
	Name:      "session_check_total",
	Help: "Official passport checks applied to game sessions, by phase (login, refresh, event = passport webhook) and result: " +
		"valid, user_gone / user_forbidden / password_changed (sessions revoked, online players kicked), revoke_failed, " +
		"kick_failed (tokens revoked, kicking online players failed), ignored (webhook names no checkable user) and " +
		"error (passport unreachable, the login or refresh goes through). " +
		"A sustained error rate means password changes and bans in the passport no longer log players out.",
	Labels: []string{"phase", "result"},
})

// Tokens 是 login 的令牌存储(*token.Manager)。
type Tokens interface {
	RevokeAll(ctx context.Context, account string) error
	RevokeIssuedBefore(ctx context.Context, account string, cutoff int64) (int64, error)
}

// Players 把账号名下在线的角色踢下线(*svc.ServiceContext 的 KickAccountPlayers)。
type Players interface {
	KickAccountPlayers(ctx context.Context, account string, reasonTip uint32) error
}

// Enforcer 落实通行证状态。零值不可用,用 New 构造;nil 表示没配通行证管理凭据(只有 dev/test 允许),三个入口都放行。
type Enforcer struct {
	admin   *auth.PassportAdmin
	tokens  Tokens
	players Players // nil = 不踢线(单测替身)
}

// New 构造 Enforcer。admin 为 nil 时回 nil(不核对)。
func New(admin *auth.PassportAdmin, tokens Tokens, players Players) *Enforcer {
	if admin == nil {
		return nil
	}
	return &Enforcer{admin: admin, tokens: tokens, players: players}
}

// CheckLogin 在通行证登录签发令牌之前调,回 0 = 放行,否则是拒绝这次登录的 tip。issuedAt 是这次身份令牌的签发时刻。
// 不论放不放行,改密之前的旧登录都在这里吊销:玩家改完密码回来登录,盗号者的会话当场失效、在线的被踢下线。
func (e *Enforcer) CheckLogin(ctx context.Context, account string, issuedAt int64) uint32 {
	sub, ok := auth.PassportSubject(account)
	if e == nil || !ok {
		return 0
	}
	status, err := e.lookup(ctx, sub)
	if err != nil {
		checkTotal.Inc(phaseLogin, resultError)
		logx.WithContext(ctx).Errorf("[passport] login check failed, letting the login through: account=%s: %v", account, err)
		return 0
	}
	if _, err := e.settle(ctx, phaseLogin, account, status); err != nil {
		// 旧登录没吊销掉:它们各自下一次刷新时还会被判出来,这次登录本身不受影响。
		checkTotal.Inc(phaseLogin, resultRevokeFailed)
		logx.WithContext(ctx).Errorf("[passport] revoking earlier sessions at login failed: account=%s: %v", account, err)
	}
	verdict := status.SessionVerdict(issuedAt)
	if verdict == "" {
		checkTotal.Inc(phaseLogin, resultValid)
		return 0
	}
	checkTotal.Inc(phaseLogin, verdict)
	logx.WithContext(ctx).Infof("[passport] login refused: account=%s reason=%s", account, verdict)
	return uint32(table.LoginError_kLoginPassportCredentialChanged)
}

// CheckRefresh 在轮换之前调,回 true = 不续期。data 是要刷新的那枚 refresh token 的载荷;不是通行证登录签的直接放行。
//
// 放在轮换之前:吊销没成功就不续期,旧令牌还在,下一次刷新再核对、再吊销。放在之后的话,新的一对已经写下,
// 客户端重试走宽限窗口的重放会原样拿到它,绕过核对。载荷里的签发时刻跨轮换不变,核对不了时续出来的令牌下一次照样判得出来。
func (e *Enforcer) CheckRefresh(ctx context.Context, data *token.TokenData) bool {
	if e == nil || data == nil || data.AuthType != auth.PassportAuthType {
		return false
	}
	sub, ok := auth.PassportSubject(data.Account)
	if !ok {
		return false
	}
	status, err := e.lookup(ctx, sub)
	if err != nil {
		checkTotal.Inc(phaseRefresh, resultError)
		logx.WithContext(ctx).Errorf("[passport] session check failed, renewing anyway: account=%s: %v", data.Account, err)
		return false
	}
	verdict := status.SessionVerdict(data.IdentityIssuedAt)
	if verdict == "" {
		checkTotal.Inc(phaseRefresh, resultValid)
		return false
	}
	if _, err := e.settle(ctx, phaseRefresh, data.Account, status); err != nil {
		checkTotal.Inc(phaseRefresh, resultRevokeFailed)
		logx.WithContext(ctx).Errorf("[passport] revoking sessions failed, refresh refused: account=%s reason=%s: %v", data.Account, verdict, err)
		return true
	}
	checkTotal.Inc(phaseRefresh, verdict)
	logx.WithContext(ctx).Infof("[passport] refresh refused, sessions revoked: account=%s reason=%s", data.Account, verdict)
	return true
}

// HandleEvent 处理一条通行证回调(§3.7),回核对了几名用户。回调只用来找人:从记录的操作人 / 目标用户、扩展用户与请求体里
// 取出用户 id 或「组织/用户名」,逐个现查通行证,按查到的状态吊销并踢线(同 settle)。回调内容是伪造的也只会多查几次,
// 改不了任何人的状态。核对不了或吊销失败回错误(通行证会重投);回调格式不对回错误但不该重投(ErrBadEvent)。
func (e *Enforcer) HandleEvent(ctx context.Context, payload []byte) (int, error) {
	if e == nil {
		return 0, errors.New("passport admin client is not configured")
	}
	users, err := eventUsers(payload)
	if err != nil {
		checkTotal.Inc(phaseEvent, resultIgnored)
		return 0, err
	}
	checked := 0
	seen := map[string]bool{}
	for _, ref := range users {
		status, err := e.lookupRef(ctx, ref)
		if err != nil {
			checkTotal.Inc(phaseEvent, resultError)
			return checked, err
		}
		if status.Sub == "" || seen[status.Sub] {
			continue // 只给了名字而用户已删(找不到是谁),或同一个人点了两次
		}
		seen[status.Sub] = true
		account := auth.PassportAccount(status.Sub)
		revoked, err := e.settle(ctx, phaseEvent, account, status)
		if err != nil {
			checkTotal.Inc(phaseEvent, resultRevokeFailed)
			return checked, fmt.Errorf("revoke sessions of %s: %w", account, err)
		}
		checked++
		result := resultValid
		switch {
		case status.Gone:
			result = auth.PassportSessionUserGone
		case status.Forbidden:
			result = auth.PassportSessionUserForbidden
		case revoked:
			result = auth.PassportSessionPasswordChanged
		}
		checkTotal.Inc(phaseEvent, result)
	}
	if checked == 0 {
		checkTotal.Inc(phaseEvent, resultIgnored)
	}
	return checked, nil
}

// ErrBadEvent:回调请求体不是 Casdoor 的记录格式(或超长)。网关回 400,通行证不必重投。
var ErrBadEvent = errors.New("passport webhook payload is not a Casdoor record")

// userRef 是回调里点到的一名用户:要么有用户 id,要么只有「组织/用户名」。
type userRef struct {
	sub         string
	owner, name string
}

// casdoorEvent 是 Casdoor 回调请求体里用到的几项(object/record.go 的 Record,Webhook.isUserExtended 时多一个 extendedUser)。
// 改密(set-password)时 organization / user 是目标用户;改资料(update-user)时是操作人,目标用户在 object(请求体原文)里。
type casdoorEvent struct {
	Organization string          `json:"organization"`
	User         string          `json:"user"`
	Object       string          `json:"object"`
	ExtendedUser *casdoorUserRef `json:"extendedUser"`
}

type casdoorUserRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	Id    string `json:"id"`
}

// eventUsers 从回调里取出要核对的用户,管理组织(built-in)的不要,最多 maxEventUsers 个。
func eventUsers(payload []byte) ([]userRef, error) {
	if len(payload) == 0 || len(payload) > maxEventBytes {
		return nil, ErrBadEvent
	}
	var event casdoorEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, ErrBadEvent
	}
	var refs []userRef
	add := func(u casdoorUserRef) {
		if u.Owner == builtInOrganization || len(refs) >= maxEventUsers {
			return
		}
		switch {
		case u.Id != "":
			refs = append(refs, userRef{sub: u.Id})
		case u.Owner != "" && u.Name != "":
			refs = append(refs, userRef{owner: u.Owner, name: u.Name})
		}
	}
	if event.ExtendedUser != nil {
		add(*event.ExtendedUser)
	}
	add(casdoorUserRef{Owner: event.Organization, Name: event.User})
	var object casdoorUserRef
	if event.Object != "" && json.Unmarshal([]byte(event.Object), &object) == nil {
		add(object)
	}
	return refs, nil
}

func (e *Enforcer) lookupRef(ctx context.Context, ref userRef) (auth.PassportUserStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	if ref.sub != "" {
		return e.admin.LookupUser(ctx, ref.sub)
	}
	return e.admin.LookupUserByName(ctx, ref.owner, ref.name)
}

// settle 把一名用户的当前状态落到游戏令牌上:被删、被封吊销全部;改过密码只吊销身份令牌签发时刻不晚于改密时刻的家族。
// 吊销了东西就踢线。回有没有吊销(被删、被封一律算吊销了)和吊销的错误;踢线失败只计数。
func (e *Enforcer) settle(ctx context.Context, phase, account string, status auth.PassportUserStatus) (bool, error) {
	revoked := false
	switch {
	case status.Gone || status.Forbidden:
		if err := e.tokens.RevokeAll(ctx, account); err != nil {
			return false, err
		}
		revoked = true
	case status.PasswordChangedAt > 0:
		n, err := e.tokens.RevokeIssuedBefore(ctx, account, status.PasswordChangedAt)
		if err != nil {
			return false, err
		}
		revoked = n > 0
	}
	if revoked {
		// 安全审计:三个入口的吊销都记在这一行(登录时顺带吊销旧会话的那一种,别处没有日志)。
		logx.WithContext(ctx).Infof("[passport] sessions revoked: phase=%s account=%s gone=%t forbidden=%t password_changed_at=%d",
			phase, account, status.Gone, status.Forbidden, status.PasswordChangedAt)
		e.kick(ctx, phase, account)
	}
	return revoked, nil
}

func (e *Enforcer) kick(ctx context.Context, phase, account string) {
	if e.players == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kickTimeout)
	defer cancel()
	if err := e.players.KickAccountPlayers(ctx, account, uint32(table.LoginError_kLoginPassportCredentialChanged)); err != nil {
		checkTotal.Inc(phase, resultKickFailed)
		logx.WithContext(ctx).Errorf("[passport] kicking online players failed (tokens are revoked, the connections stay until they drop): account=%s: %v", account, err)
	}
}

func (e *Enforcer) lookup(ctx context.Context, sub string) (auth.PassportUserStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return e.admin.LookupUser(ctx, sub)
}
