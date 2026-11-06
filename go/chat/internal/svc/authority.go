package svc

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"chat/internal/authority"
	"chat/internal/config"
	dspb "proto/data_service"
	"shared/authoritysql"
	"shared/dbplacement"
	"shared/rolefence"

	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
)

// RoleFenceCaller 是聊天读角色护栏权威状态时的调用方身份,密钥在 rolefence.SecretEnv(RoleFenceCaller)。
const RoleFenceCaller = "chat"

// openDirectory 建 data_service 客户端和角色权威状态读取。NonBlock:data_service 晚起时聊天照常起服,校验读不到按故障回。
// 调用方密钥缺失拒启:校验过不了,聊天发不出去,不如在启动时暴露。
func openDirectory(c config.Config) (*grpc.ClientConn, rolefence.StateReader, error) {
	client, err := zrpc.NewClient(c.DataServiceRpc)
	if err != nil {
		return nil, nil, fmt.Errorf("聊天目录客户端装配失败: %w", err)
	}
	states, err := rolefence.NewDirectoryReader(dspb.NewDataServiceClient(client.Conn()), RoleFenceCaller, os.Getenv(rolefence.SecretEnv(RoleFenceCaller)))
	if err != nil {
		_ = client.Conn().Close()
		return nil, nil, fmt.Errorf("聊天调用方密钥 %s 不可用: %w", rolefence.SecretEnv(RoleFenceCaller), err)
	}
	return client.Conn(), states, nil
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
func OpenAuthorityWithPermission(ctx context.Context, c config.Config, migrate bool, permission authoritysql.Permission, states rolefence.StateReader) (*sql.DB, *authority.Store, error) {
	backend, err := authoritysql.ParseBackend(c.Backend)
	if err != nil {
		return nil, nil, err
	}
	var db *sql.DB
	if migrate {
		// 搬库期间(state=MOVING)拒绝 DDL;库搬走之后连当前集群。
		options, placementErr := dbplacement.ForMigration(ctx, c.DbPlacement, authority.DatabaseName)
		if placementErr != nil {
			return nil, nil, placementErr
		}
		db, err = authoritysql.OpenForMigration(ctx, os.Getenv(c.DSNEnv), authority.DatabaseName, backend, permission, options...)
	} else {
		placement, placementErr := dbplacement.Start(ctx, c.DbPlacement)
		if placementErr != nil {
			return nil, nil, placementErr
		}
		db, err = authoritysql.Open(ctx, os.Getenv(c.DSNEnv), authority.DatabaseName, backend, c.MaxOpenConns, permission, placement.OpenOptions()...)
	}
	if err != nil {
		return nil, nil, err
	}
	if migrate {
		if err = authority.Migrate(ctx, db); err != nil {
			db.Close()
			return nil, nil, err
		}
		return db, nil, nil
	}
	store, err := authority.New(ctx, db, states, backend, authority.Rules{MaxContentBytes: c.MaxContentBytes, RateLimitPerSecond: c.RateLimitPerSecond, HistoryMaxEntries: c.HistoryMaxEntries, HistoryTTLSeconds: c.HistoryTTLSeconds, HistoryDefaultLimit: c.HistoryDefaultLimit, HistoryMaxLimit: c.HistoryMaxLimit, SensitiveWords: c.SensitiveWords, ActorVerifyTTLSeconds: c.ActorVerifyTTLSeconds, WorldHistoryCacheMs: c.WorldHistoryCacheMs})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, store, nil
}
