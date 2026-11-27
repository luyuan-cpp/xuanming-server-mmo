package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"chat/internal/authority"
	"chat/internal/config"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/contentguard"
	"shared/dbplacement"
	"shared/dsclient"
	"shared/rolefence"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
)

// RoleFenceCaller 是聊天读角色护栏权威状态时的调用方身份,密钥在 rolefence.SecretEnv(RoleFenceCaller)。
const RoleFenceCaller = "chat"

// openDirectory 建 data_service 客户端、角色权威状态读取和账号档案读取。NonBlock:data_service 晚起时聊天照常起服。
func openDirectory(c config.Config) (*grpc.ClientConn, rolefence.StateReader, authority.ProfileReader, error) {
	client, err := zrpc.NewClient(dsclient.Conf(c.DataServiceRpc))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("聊天目录客户端装配失败: %w", err)
	}
	dsClient := dspb.NewDataServiceClient(client.Conn())
	// 读角色护栏与读玩家画像用同一把密钥:密钥按调用方发,签名里带着方法名(accountdirectory.ChatCaller 的注释)。
	secret := os.Getenv(rolefence.SecretEnv(RoleFenceCaller))
	states, err := rolefence.NewDirectoryReader(dsClient, RoleFenceCaller, secret)
	if err != nil {
		_ = client.Conn().Close()
		return nil, nil, nil, fmt.Errorf("聊天调用方密钥 %s 不可用: %w", rolefence.SecretEnv(RoleFenceCaller), err)
	}
	profiles := authority.ProfileReaderFunc(func(callCtx context.Context, playerID uint64) (authority.PlayerProfile, error) {
		req := &dspb.GetPlayerChannelIdentityRequest{PlayerId: playerID}
		if signErr := accountdirectory.SignChannelIdentityRequest(req, accountdirectory.ChatCaller, secret); signErr != nil {
			return authority.PlayerProfile{}, fmt.Errorf("sign player profile lookup: %w", signErr)
		}
		resp, rpcErr := dsClient.GetPlayerChannelIdentity(callCtx, req)
		if rpcErr != nil {
			return authority.PlayerProfile{}, rpcErr
		}
		if resp.GetRetry() {
			return authority.PlayerProfile{}, errors.New("directory profile lookup signaled retry")
		}
		if !resp.GetFound() {
			return authority.PlayerProfile{}, nil
		}
		return authority.PlayerProfile{
			Level:            resp.GetPlayerLevel(),
			AccountCreatedMs: resp.GetAccountCreatedMs(),
		}, nil
	})
	return client.Conn(), states, profiles, nil
}

func OpenAuthority(ctx context.Context, c config.Config, migrate bool) (*sql.DB, *authority.Store, error) {
	// 先于消费一次性 writer 资格:档位写错不能白白烧掉一份 boot grant。
	if _, err := authoritysql.ParseBackend(c.Backend); err != nil {
		return nil, nil, err
	}
	var permission authoritysql.Permission
	var loadErr error
	if migrate {
		permission, loadErr = authoritysql.LoadSchemaPermission(os.Getenv)
		if loadErr != nil {
			return nil, nil, loadErr
		}

	} else {
		writer, loadErr := authoritysql.LoadWriterConfig(os.Getenv)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		permission = authoritysql.Writer(writer)
	}
	return OpenAuthorityWithPermission(ctx, c, migrate, permission, nil)
}

// OpenAuthorityWithPermission 由进程显式传同一已消费 boot，避免 SQL 与排空身份分裂。迁移不校验身份,states 传 nil。
func OpenAuthorityWithPermission(ctx context.Context, c config.Config, migrate bool, permission authoritysql.Permission, states rolefence.StateReader, opts ...authority.Option) (*sql.DB, *authority.Store, error) {
	db, store, _, err := openAuthorityWithGuard(ctx, c, migrate, permission, states, opts...)
	return db, store, err
}

func openAuthorityWithGuard(ctx context.Context, c config.Config, migrate bool, permission authoritysql.Permission, states rolefence.StateReader, extraOpts ...authority.Option) (*sql.DB, *authority.Store, *contentguard.Client, error) {
	backend, err := authoritysql.ParseBackend(c.Backend)
	if err != nil {
		return nil, nil, nil, err
	}
	var db *sql.DB
	if migrate {
		// 搬库期间(state=MOVING)拒绝 DDL;库搬走之后连当前集群。
		options, placementErr := dbplacement.ForMigration(ctx, c.DbPlacement, authority.DatabaseName)
		if placementErr != nil {
			return nil, nil, nil, placementErr
		}
		db, err = authoritysql.OpenForMigration(ctx, os.Getenv(c.DSNEnv), authority.DatabaseName, backend, permission, options...)
	} else {
		placement, placementErr := dbplacement.Start(ctx, c.DbPlacement)
		if placementErr != nil {
			return nil, nil, nil, placementErr
		}
		db, err = authoritysql.Open(ctx, os.Getenv(c.DSNEnv), authority.DatabaseName, backend, c.MaxOpenConns, permission, placement.OpenOptions()...)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if migrate {
		if err = authority.Migrate(ctx, db); err != nil {
			db.Close()
			return nil, nil, nil, err
		}
		return db, nil, nil, nil
	}
	// 正文送检(docs/design/rename-and-content-safety.md §6.6):没配目标又不是本机的 Optional 时拒启。
	guard, err := contentguard.New("chat", c.ContentGuard)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	opts := append([]authority.Option{authority.WithContentGuard(guard)}, extraOpts...)
	rules := authority.Rules{
		MaxContentBytes:             c.MaxContentBytes,
		RateLimitPerSecond:          c.RateLimitPerSecond,
		HistoryMaxEntries:           c.HistoryMaxEntries,
		HistoryTTLSeconds:           c.HistoryTTLSeconds,
		HistoryDefaultLimit:         c.HistoryDefaultLimit,
		HistoryMaxLimit:             c.HistoryMaxLimit,
		ActorVerifyTTLSeconds:       c.ActorVerifyTTLSeconds,
		WorldHistoryCacheMs:         c.WorldHistoryCacheMs,
		WorldChannelCooldownMs:      c.WorldChannelCooldownMs,
		WorldMinLevel:               c.WorldMinLevel,
		DuplicateSpamWindowMs:       c.DuplicateSpamWindowMs,
		DuplicateSpamMaxSenders:     c.DuplicateSpamMaxSenders,
		DuplicateSpamMaxPerSender:   c.DuplicateSpamMaxPerSender,
		NewAccountAgeHours:          c.NewAccountAgeHours,
		NewAccountDailyPrivateLimit: c.NewAccountDailyPrivateLimit,
	}
	store, err := authority.New(ctx, db, states, backend, rules, opts...)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	reportRetention(ctx, db, backend)
	return db, store, guard, nil
}

// reportRetention 在 TiDB 档核对原件表的保留期属性(authority/schema.go 的 TTL),不在或没开就打一行 ERROR,不拒启:
// 后果是表只增不减,数据没有错。MySQL 没有 TTL,开发用 MySQL 时不查。
func reportRetention(ctx context.Context, db *sql.DB, backend authoritysql.Backend) {
	if backend != authoritysql.BackendTiDB {
		return
	}
	enabled, err := authority.RetentionEnabled(ctx, db)
	if err != nil {
		logx.Errorf("[chat] Could not read the retention attribute of chat_send_authority: %v", err)
		return
	}
	if !enabled {
		logx.Error("[chat] chat_send_authority has no enabled TTL attribute: chat originals are never deleted and the table grows without bound. Recreate the table with -migrate on an empty database, or restore the TTL attribute.")
		return
	}
	logx.Info("[chat] chat_send_authority retention is enabled (TiDB TTL)")
}
