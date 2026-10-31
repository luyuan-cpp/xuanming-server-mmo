package svc

import (
	"context"
	"fmt"
	"os"
	"scene_manager/internal/config"
	"time"

	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"
)

// RoleDirectory只观察SQL原修订；角色准入仍由scene的RegisterRoleAdmission提交。
type RoleDirectory interface {
	Lookup(context.Context, uint64) (*dspb.AccountRoleOwnership, error)
}

// PlayerStoreHolderReleaser 在持有节点判死后释放写权,见 data_service 的 ReleaseDeadPlayerStoreHolder。
type PlayerStoreHolderReleaser interface {
	ReleaseDeadPlayerStoreHolder(ctx context.Context, player, ownerEpoch uint64) error
}

const RoleDirectorySecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER"

func roleDirectorySecret(c config.Config, getenv func(string) string) (string, error) {
	rpc := c.DataServiceRpc
	if rpc.Etcd.Key == "" && len(rpc.Endpoints) == 0 && rpc.Target == "" {
		return "", fmt.Errorf("scene_manager必须配置DataServiceRpc")
	}
	secret := getenv(RoleDirectorySecretEnv)
	if _, err := serviceauth.New(map[string]string{"scene_manager": secret}); err != nil {
		return "", fmt.Errorf("scene_manager独立只读目录密钥缺失或非法")
	}
	return secret, nil
}

func configuredRoleDirectorySecret(c config.Config) string {
	secret, err := roleDirectorySecret(c, os.Getenv)
	if err != nil {
		panic(err.Error())
	}
	return secret
}

type roleDirectoryClient struct {
	rpc    dspb.DataServiceClient
	secret string
}

// ReleaseDeadPlayerStoreHolder 见 data_service 同名 RPC。调用方拿到错误就保留 location 重试,预算比只读查询宽:这是一个 SQL 事务。
func (c *roleDirectoryClient) ReleaseDeadPlayerStoreHolder(ctx context.Context, player, ownerEpoch uint64) error {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in := &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: player, OwnerEpoch: ownerEpoch}
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"ReleaseDeadPlayerStoreHolder", in, "scene_manager", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return fmt.Errorf("写权释放证明不可用")
	}
	_, err := c.rpc.ReleaseDeadPlayerStoreHolder(call, in)
	return err
}

func (c *roleDirectoryClient) Lookup(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	call, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	in := &dspb.LookupAccountRoleRequest{PlayerId: player}
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"LookupAccountRole", in, "scene_manager", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, fmt.Errorf("角色目录只读证明不可用")
	}
	return c.rpc.LookupAccountRole(call, in)
}
