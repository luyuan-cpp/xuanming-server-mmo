package platform

import (
	"context"
	"encoding/json"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// DouyinTokenKey 是各副本共用的抖音调用凭据在 etcd 里的键。
const DouyinTokenKey = "/mmorpg/content_guard/platform_token/douyin"

// TokenStore 让内容安全服务的各副本共用一份平台调用凭据(docs/design/rename-and-content-safety.md §6.5)。
//
// 抖音的调用凭据是「重新取一次,上一次取的就提前失效」那一类(官方的说法是旧的有效期缩短为 5 分钟):各副本各取各的,
// 会隔几分钟互相挤掉一次,每挤一次多一次取凭据、多一次失败的审核调用,副本越多越频繁,还会撞上取凭据的次数上限。
// 平台推荐的做法是一处取、大家用(「中控」)。这里用已经连着的 etcd 做那一处:谁发现凭据要换了谁去取,
// 取到之后按版本号比较着写回;两个副本同时取,只有一个写得进去,另一个改用写进去的那个,大家很快用上同一个。
// 读写 etcd 失败时退回各副本自己取:这是省调用量的优化,不是安全边界,不为它拒绝审核。
type TokenStore interface {
	// Load 读当前共用的凭据。没有时 token 为空。revision 是写回时要比较的版本(键不存在为 0)。
	Load(ctx context.Context) (token string, expiresAt time.Time, revision int64, err error)
	// CompareAndStore 在版本仍是 revision 时写入新凭据;版本变了(别的副本先写了)返回 false。
	CompareAndStore(ctx context.Context, revision int64, token string, expiresAt time.Time) (bool, error)
}

type sharedToken struct {
	Token       string `json:"token"`
	ExpiresAtMs int64  `json:"expires_at_ms"`
}

// EtcdTokenStore 把共用凭据存在 etcd 的一个键里。值里只有凭据与到期时刻;AppSecret 不进 etcd。
type EtcdTokenStore struct {
	kv  clientv3.KV
	key string
}

func NewEtcdTokenStore(kv clientv3.KV, key string) *EtcdTokenStore {
	return &EtcdTokenStore{kv: kv, key: key}
}

func (s *EtcdTokenStore) Load(ctx context.Context) (string, time.Time, int64, error) {
	resp, err := s.kv.Get(ctx, s.key)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	if len(resp.Kvs) == 0 {
		return "", time.Time{}, 0, nil
	}
	var value sharedToken
	if json.Unmarshal(resp.Kvs[0].Value, &value) != nil {
		// 值坏了当作没有,带着版本号:下一个取到新凭据的副本能把它覆盖掉。
		return "", time.Time{}, resp.Kvs[0].ModRevision, nil
	}
	return value.Token, time.UnixMilli(value.ExpiresAtMs), resp.Kvs[0].ModRevision, nil
}

func (s *EtcdTokenStore) CompareAndStore(ctx context.Context, revision int64, token string, expiresAt time.Time) (bool, error) {
	value, err := json.Marshal(sharedToken{Token: token, ExpiresAtMs: expiresAt.UnixMilli()})
	if err != nil {
		return false, err
	}
	resp, err := s.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(s.key), "=", revision)).
		Then(clientv3.OpPut(s.key, string(value))).
		Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}
