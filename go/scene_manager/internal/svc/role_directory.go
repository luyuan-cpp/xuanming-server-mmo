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

// PlayerStoreHolderReleaser 只按代次释放写权,见 data_service 的 ReleaseDeadPlayerStoreHolder(不带 seen)。
// 只剩无资格撤销在用:撤销之前按 E−1 围写权行(logic.fenceUnclaimedPlayerStore,location-release-binding.md §17.9);
// LeaveScene 判死释放用 PlayerStoreDeadHolderFence。
type PlayerStoreHolderReleaser interface {
	ReleaseDeadPlayerStoreHolder(ctx context.Context, player, ownerEpoch uint64) error
}

// PlayerStoreDeadHolderFence 是 LeaveScene 判死释放的两个 data_service 步骤:先读写权行,调用方复核持有实例仍不在册之后,
// 再按读到的那一份比较并写(logic.releaseDeadPlayerStoreHolder,docs/design/player-save-direct-commit.md §5.5)。
type PlayerStoreDeadHolderFence interface {
	// ReadPlayerStoreHolder 普通读写权行判持有者用的三列。Found=false:没有路由或写权行,没有东西要释放。
	ReadPlayerStoreHolder(ctx context.Context, player uint64) (*dspb.ReadPlayerStoreHolderResponse, error)
	// ReleaseDeadPlayerStoreHolderIfUnchanged 写权行还是 seen、代次不比 ownerEpoch 新才释放;读之后有人认领过回 FailedPrecondition。
	ReleaseDeadPlayerStoreHolderIfUnchanged(ctx context.Context, player, ownerEpoch uint64, seen *dspb.PlayerStoreHolder) error
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
	return c.releaseDeadPlayerStoreHolder(ctx, &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: player, OwnerEpoch: ownerEpoch})
}

// ReleaseDeadPlayerStoreHolderIfUnchanged 是带 seen 的 ReleaseDeadPlayerStoreHolder:data_service 按读到的写权行比较并写。
func (c *roleDirectoryClient) ReleaseDeadPlayerStoreHolderIfUnchanged(ctx context.Context, player, ownerEpoch uint64, seen *dspb.PlayerStoreHolder) error {
	if seen == nil {
		return fmt.Errorf("判死释放缺少读到的写权行")
	}
	return c.releaseDeadPlayerStoreHolder(ctx, &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: player, OwnerEpoch: ownerEpoch, Seen: seen})
}

func (c *roleDirectoryClient) releaseDeadPlayerStoreHolder(ctx context.Context, in *dspb.ReleaseDeadPlayerStoreHolderRequest) error {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"ReleaseDeadPlayerStoreHolder", in, "scene_manager", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return fmt.Errorf("写权释放证明不可用")
	}
	_, err := c.rpc.ReleaseDeadPlayerStoreHolder(call, in)
	return err
}

// ReadPlayerStoreHolder 见 data_service 同名 RPC,判死释放的第一步。只读,预算与 Lookup 相同。
func (c *roleDirectoryClient) ReadPlayerStoreHolder(ctx context.Context, player uint64) (*dspb.ReadPlayerStoreHolderResponse, error) {
	call, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	in := &dspb.ReadPlayerStoreHolderRequest{PlayerId: player}
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"ReadPlayerStoreHolder", in, "scene_manager", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, fmt.Errorf("写权行读取证明不可用")
	}
	return c.rpc.ReadPlayerStoreHolder(call, in)
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
