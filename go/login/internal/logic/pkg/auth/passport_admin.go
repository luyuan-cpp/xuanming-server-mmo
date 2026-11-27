package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// 官方通行证(Casdoor)的管理接口:账号注销时删用户(docs/design/official-passport-registration.md §3.5、docs/design/account-deletion.md §6.2),
// 登录、刷新游戏令牌与通行证回调时查玩家有没有在通行证里改密、被封或被删(§3.6、§3.7,判定在 PassportUserStatus.SessionVerdict,
// 落到游戏令牌上在 pkg/passportsession)。
//
// 通行证账号的邮箱、手机号、口令哈希存在 Casdoor 自己的库里,游戏这边只有 passport_<sub>。账号注销删掉游戏账号之前,
// login 用一个机密应用的凭据调 Casdoor 的管理接口把这名用户删掉:只删游戏账号的话,个人信息还留在通行证里,不算注销。
//
// 删用户三步(Casdoor v4.27.1 的接口;成功与拒绝都回 HTTP 200,成败看应答体的 status):
//  1. GET /api/get-user?userId=<sub>:按不可变的用户 id 取用户,拿到它所在的组织与当前用户名。用户名可以改,所以每次现查。
//     data 为 null = 用户已经不在,按删完处理(执行者会重试,这一步必须可以重做)。
//  2. POST /api/delete-user {owner, name}:data 为 "Affected" 才算删掉。回别的说明第 1 步之后用户名变了或被别人删了,
//     回错误让执行者下一轮重新按 id 查,不能当成删完。
//  3. 再按 id 查一次,查不到才回成功(组织开了软删除时第 2 步只打标记,见 DeleteUser)。
//
// 凭据走 HTTP Basic(应用的 ClientId / ClientSecret)。Casdoor 把认不出的 Basic 凭据当匿名处理而不是报错:匿名查到用户会被拒绝
// (status=error),查不到与真的查不到一样回 null。所以凭据写错只会让删除一直失败并告警,不会把还在的用户当成不在。
//
// 失败一律标 ErrProviderUnavailable(通行证或我们自己配置的问题,与玩家无关)。错误文案不带凭据;
// Casdoor 的错误消息截短后带上,排障要看。

const (
	passportGetUserPath    = "/api/get-user"
	passportDeleteUserPath = "/api/delete-user"
	// passportUserRemoved 是 delete-user 确实删掉一行时 data 的取值(Casdoor controllers/base.go 的 wrapActionResponse)。
	passportUserRemoved = "Affected"
)

// SessionVerdict 的结论:空串 = 照常;其余是不再认这次登录的原因,也是指标 label 的取值,上线后不改名。
const (
	PassportSessionUserGone        = "user_gone"        // 通行证里查不到这名用户,或已被标记删除
	PassportSessionUserForbidden   = "user_forbidden"   // 被封禁
	PassportSessionPasswordChanged = "password_changed" // 这次登录之后改过密码(含找回密码、管理员重设)
)

// PassportAdmin 调官方通行证的管理接口。零值不可用,用 NewPassportAdmin 构造;可并发使用。
type PassportAdmin struct {
	baseURL      string
	clientID     string
	clientSecret string
	http         *http.Client // nil 用默认 5s 客户端
}

// NewPassportAdmin:issuer / endpoint 同 NewPassportProvider(endpoint 为空就用 issuer)。clientID / clientSecret 是 Casdoor 里
// 与玩家同一组织的机密应用的凭据(Casdoor 让应用凭据在它自己的组织里有管理权限),不是客户端里那个公开应用。
func NewPassportAdmin(issuer, endpoint, clientID, clientSecret string, client *http.Client) (*PassportAdmin, error) {
	if issuer == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("passport: Issuer, AdminClientId and the admin client secret are all required for the passport admin API")
	}
	return &PassportAdmin{baseURL: idTokenBaseURL(endpoint, issuer), clientID: clientID, clientSecret: clientSecret, http: client}, nil
}

// passportUser 是 get-user 应答里用到的几项:id 是令牌的 sub,owner / name 是 delete-user 认的主键,其余给 PassportUserStatus。
// 应用凭据在本组织算管理员,封禁、删除标记这些只给管理员看的字段也会回来(Casdoor controllers/user.go GetUser)。
type passportUser struct {
	Owner       string `json:"owner"`
	Name        string `json:"name"`
	Id          string `json:"id"`
	IsForbidden bool   `json:"isForbidden"`
	IsDeleted   bool   `json:"isDeleted"`
	// LastChangePasswordTime:最近一次改密的时刻,RFC 3339,通行证的时钟;从没改过为空。改密、找回密码、管理员重设都会更新它
	// (Casdoor controllers/user.go 的 SetPassword、UpdateUser),注册不写。
	LastChangePasswordTime string `json:"lastChangePasswordTime"`
}

// passportReply 是 Casdoor 接口的统一应答(controllers.Response)。data 的形状随接口不同,先留原文。
type passportReply struct {
	Status string          `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

// PassportUserStatus 是通行证里一名用户此刻的状态(LookupUser / LookupUserByName)。
type PassportUserStatus struct {
	Sub       string // 用户 id(令牌的 sub);按名字查且查不到时为空
	Gone      bool   // 查不到,或已被标记删除
	Forbidden bool   // 被封禁
	// PasswordChangedAt 是最近一次改密的时刻(unix 秒,通行证的时钟);0 = 从没改过。改密、找回密码、管理员重设都会更新它。
	PasswordChangedAt int64
}

// SessionVerdict 判断一次身份令牌签发于 issuedAt(令牌的 iat,通行证的时钟)的登录还认不认:回空串 = 认;
// 回 PassportSession* 之一 = 不认(登录拒绝、刷新不续期)。
//
// 改密时刻与 iat 都取自通行证自己的时钟,不受 login 与通行证之间的时钟偏差影响。两者都只到秒:同一秒里分不出先后,
// 按改密在后处理(注册不写改密时刻,改完密码再回游戏登录不会落在同一秒,误判的只能是改密那一秒签出的令牌)。
// issuedAt 为 0(这个字段加上之前签的令牌)当成最早,只要改过密码就不认。
func (s PassportUserStatus) SessionVerdict(issuedAt int64) string {
	switch {
	case s.Gone:
		return PassportSessionUserGone
	case s.Forbidden:
		return PassportSessionUserForbidden
	case s.PasswordChangedAt > 0 && s.PasswordChangedAt >= issuedAt:
		return PassportSessionPasswordChanged
	}
	return ""
}

// LookupUser 按用户 id(令牌的 sub)查这名用户的状态。查不到回 Gone,不是错误;核对不了(通行证或网络故障、应答认不出)回错误。
func (a *PassportAdmin) LookupUser(ctx context.Context, sub string) (PassportUserStatus, error) {
	user, err := a.findUser(ctx, sub)
	if err != nil {
		return PassportUserStatus{}, err
	}
	if user == nil {
		return PassportUserStatus{Sub: sub, Gone: true}, nil
	}
	return passportStatusOf(user)
}

// LookupUserByName 按「组织/用户名」查(通行证回调只点了名字时用,§3.7):查到时 Sub 取应答里的用户 id;查不到回 Gone、Sub 为空。
// 应答里的组织、用户名与问的对不上、或缺用户 id 时回错误,不拿别人的状态当这名用户的。
func (a *PassportAdmin) LookupUserByName(ctx context.Context, owner, name string) (PassportUserStatus, error) {
	user, err := a.getUser(ctx, url.Values{"id": {owner + "/" + name}})
	if err != nil {
		return PassportUserStatus{}, err
	}
	if user == nil {
		return PassportUserStatus{Gone: true}, nil
	}
	if user.Owner != owner || user.Name != name || user.Id == "" {
		return PassportUserStatus{}, fmt.Errorf("%s: get-user returned an unexpected user: %w", PassportAuthType, ErrProviderUnavailable)
	}
	return passportStatusOf(user)
}

// passportStatusOf 把查到的用户换成状态。改密时刻认不出按核对不了回错误:不当成「没改过」(放过盗号者),也不当成「刚改过」(误踢)。
func passportStatusOf(user *passportUser) (PassportUserStatus, error) {
	status := PassportUserStatus{Sub: user.Id, Gone: user.IsDeleted, Forbidden: user.IsForbidden}
	if user.LastChangePasswordTime != "" {
		changedAt, err := time.Parse(time.RFC3339, user.LastChangePasswordTime)
		if err != nil {
			return PassportUserStatus{}, fmt.Errorf("%s: get-user lastChangePasswordTime is not RFC 3339: %w", PassportAuthType, ErrProviderUnavailable)
		}
		status.PasswordChangedAt = changedAt.Unix()
	}
	return status, nil
}

// DeleteUser 删掉 sub 对应的通行证用户;用户已经不在回 nil。可以重复调。
func (a *PassportAdmin) DeleteUser(ctx context.Context, sub string) error {
	user, err := a.findUser(ctx, sub)
	if err != nil || user == nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"owner": user.Owner, "name": user.Name})
	if err != nil {
		return providerFault(fmt.Errorf("%s: encode delete-user request: %w", PassportAuthType, err))
	}
	reply, err := a.call(ctx, http.MethodPost, passportDeleteUserPath, nil, body)
	if err != nil {
		return err
	}
	var outcome string
	if json.Unmarshal(reply.Data, &outcome) != nil || outcome != passportUserRemoved {
		return fmt.Errorf("%s: delete-user removed nothing (user renamed or removed concurrently), the next attempt looks it up again: %w",
			PassportAuthType, ErrProviderUnavailable)
	}
	// 删完再查一次才算数:组织要是被人开了软删除(enableSoftDeletion),第一次 delete-user 只打删除标记,个人信息还在,
	// 对已打标记的用户再删一次才真删。还查得到就回错误,执行者下一轮再删。
	remaining, err := a.findUser(ctx, sub)
	if err != nil {
		return err
	}
	if remaining != nil {
		return fmt.Errorf("%s: user still present after delete-user (soft deletion enabled on the organization?), the next attempt deletes it again: %w",
			PassportAuthType, ErrProviderUnavailable)
	}
	return nil
}

// findUser 按 sub 取用户;不在回 (nil, nil)。应答里的 id 与 sub 对不上、或缺组织 / 用户名时回错误:宁可删不掉,不删错人。
func (a *PassportAdmin) findUser(ctx context.Context, sub string) (*passportUser, error) {
	user, err := a.getUser(ctx, url.Values{"userId": {sub}})
	if err != nil || user == nil {
		return nil, err
	}
	if user.Id != sub || user.Owner == "" || user.Name == "" {
		return nil, fmt.Errorf("%s: get-user returned an unexpected user: %w", PassportAuthType, ErrProviderUnavailable)
	}
	return user, nil
}

// getUser 调一次 get-user;data 为 null 回 (nil, nil)。
func (a *PassportAdmin) getUser(ctx context.Context, query url.Values) (*passportUser, error) {
	reply, err := a.call(ctx, http.MethodGet, passportGetUserPath, query, nil)
	if err != nil {
		return nil, err
	}
	if len(reply.Data) == 0 || bytes.Equal(reply.Data, []byte("null")) {
		return nil, nil
	}
	var user passportUser
	if err := json.Unmarshal(reply.Data, &user); err != nil {
		return nil, fmt.Errorf("%s: get-user returned an unexpected user: %w", PassportAuthType, ErrProviderUnavailable)
	}
	return &user, nil
}

// call 发一次管理接口请求并解出统一应答。网络错误、非 2xx、应答超长、不是 JSON、status 不是 ok 都按渠道故障回。
func (a *PassportAdmin) call(ctx context.Context, method, path string, query url.Values, body []byte) (*passportReply, error) {
	target := a.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, providerFault(oauthRequestError(PassportAuthType, "build request", err))
	}
	req.SetBasicAuth(a.clientID, a.clientSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClientOrDefault(a.http).Do(req)
	if err != nil {
		return nil, providerFault(oauthRequestError(PassportAuthType, "http error", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s: admin api http status %d: %w", PassportAuthType, resp.StatusCode, ErrProviderUnavailable)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return nil, providerFault(oauthRequestError(PassportAuthType, "read body", err))
	}
	if len(raw) > maxOAuthResponseBytes {
		return nil, fmt.Errorf("%s: admin api response exceeds %d bytes: %w", PassportAuthType, maxOAuthResponseBytes, ErrProviderUnavailable)
	}
	var reply passportReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("%s: bad admin api json: %w", PassportAuthType, ErrProviderUnavailable)
	}
	if reply.Status != "ok" {
		// %.120q:消息是给人看的整句,截短并转义后再进错误文案。
		return nil, fmt.Errorf("%s: admin api refused the request (%.120q): %w", PassportAuthType, reply.Msg, ErrProviderUnavailable)
	}
	return &reply, nil
}
