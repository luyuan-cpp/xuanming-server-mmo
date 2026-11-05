// Package loginsession 是读登录态的只读接缝;不拥有登录凭据或写权限。键形在 shared/redisslot,这里只有
// 「从 gate 带来的连接身份得到键」这一个入口。
package loginsession

import (
	"context"
	"errors"
	"reflect"

	base "proto/common/base"
	"shared/accountdirectory"
	"shared/callerauth"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// ConnectionKey 是一条 gate 连接的登录态键。只受理具有进程 UUID 与单调代次的连接,没有代次的连接没有登录态。
func ConnectionKey(in *base.SessionDetails) (string, error) {
	if in == nil || in.GetSessionId() == 0 || in.GetGateInstanceId() == "" || in.GetSessionIncarnation() == 0 {
		return "", errors.New("登录连接身份不完整")
	}
	return redisslot.LoginSessionKey(redisslot.LoginConnectionID(in.GetGateInstanceId(), in.GetSessionIncarnation())), nil
}

// Reader 是读登录态需要的最小能力,redis.UniversalClient 满足它。
type Reader interface {
	Get(context.Context, string) *redis.StringCmd
}

// Account 优先复用 gate 验签的原身份；未携带原账号的旧内部协议仍只读登录域。
// 有声明却没有验签证明时直接拒绝，不能借 Redis 或当前 owner 把它升级成可信身份。
func Account(ctx context.Context, r Reader, in *base.SessionDetails) (string, error) {
	if in.GetAuthenticatedAccount() != "" {
		verified, ok := callerauth.AuthenticatedSession(ctx)
		if !ok || !proto.Equal(verified, in) || in.GetPlayerId() == 0 || in.GetOwnerRevision() == 0 ||
			!accountdirectory.ValidAccount(in.GetAuthenticatedAccount()) {
			return "", errors.New("在线原身份缺少完整的调用方验签证明")
		}
		return verified.GetAuthenticatedAccount(), nil
	}
	key, err := ConnectionKey(in)
	if err != nil || r == nil {
		return "", errors.New("登录态依赖或连接身份不可用")
	}
	value := reflect.ValueOf(r)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice:
		if value.IsNil() {
			return "", errors.New("登录态只读连接未装配")
		}
	}
	account, err := r.Get(ctx, key).Result()
	if err != nil {
		return "", err
	}
	if !accountdirectory.ValidAccount(account) {
		return "", errors.New("登录态账号非法")
	}
	return account, nil
}
