package config

import (
	"fmt"
	"sort"
	"strings"

	"shared/authoritysql"
	"shared/placement"
)

const RoleProofDatabaseAllowlistEnv = "DB_ALLOWED_DATABASES"
const MaxRoleProofStorageIDs = 1024

// ResolveGlobalAuthority固定服务启动合同；迁移/route_tool保留自己的显式工具入口。
// 只读取库名白名单，不接凭据。storage_id仅从正规外部库名反解，不从请求、Regions或本实例Zone补齐。
func (c Config) ResolveGlobalAuthority(getenv func(string) string) (Config, error) {
	if _, err := authoritysql.ParseBackend(c.SnapshotMySQL.Backend); err != nil {
		return c, fmt.Errorf("SnapshotMySQL.Backend: %w", err)
	}
	c.RouteStore = c.RouteStore.Normalize()
	if !c.Store.Required || !c.AccountDirectory.RoleTradeSQLProof.Enabled {
		return c, fmt.Errorf("必须启用Store.Required与RoleTradeSQLProof")
	}
	if err := c.RouteStore.Validate(); err != nil {
		return c, err
	}
	proof := c.AccountDirectory.RoleTradeSQLProof
	envName := proof.AllowedDatabasesEnv
	if envName == "" {
		envName = RoleProofDatabaseAllowlistEnv
	}
	for n, ch := range envName {
		if !(ch >= 'A' && ch <= 'Z' || ch == '_' || n > 0 && ch >= '0' && ch <= '9') {
			return c, fmt.Errorf("角色SQL证明库白名单环境变量名非法")
		}
	}
	ids := append([]uint32(nil), proof.AllowedStorageIDs...)
	if value := getenv(envName); value != "" {
		fromEnv, err := StorageIDsFromDatabases(value)
		if err != nil {
			return c, err
		}
		if len(ids) > 0 {
			original, err := checkedStorageIDs(ids)
			if err != nil || len(original) != len(fromEnv) {
				return c, fmt.Errorf("角色证明与玩家数据库白名单不一致")
			}
			for n := range original {
				if original[n] != fromEnv[n] {
					return c, fmt.Errorf("角色证明与玩家数据库白名单不一致")
				}
			}
		}
		ids = fromEnv
	}
	ids, err := checkedStorageIDs(ids)
	if err != nil {
		return c, err
	}
	c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs = ids
	c.AccountDirectory.RoleTradeSQLProof.AllowedDatabasesEnv = envName
	return c, nil
}

func StorageIDsFromDatabases(value string) ([]uint32, error) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > MaxRoleProofStorageIDs {
		return nil, fmt.Errorf("玩家库白名单必须包含1..1024个正规落点库")
	}
	ids := make([]uint32, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		id, ok := placement.StorageIDFromDBName(name)
		canonical, valid := placement.StoreDBName(id)
		if !ok || !valid || canonical != name {
			return nil, fmt.Errorf("玩家库白名单含非正规落点库名")
		}
		ids = append(ids, id)
	}
	return checkedStorageIDs(ids)
}

func checkedStorageIDs(ids []uint32) ([]uint32, error) {
	if len(ids) < 1 || len(ids) > MaxRoleProofStorageIDs {
		return nil, fmt.Errorf("角色SQL证明须从已配置玩家库提供1..1024个storage_id")
	}
	out := append([]uint32(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	for n, id := range out {
		if _, ok := placement.StoreDBName(id); !ok || n > 0 && out[n-1] == id {
			return nil, fmt.Errorf("角色SQL证明storage_id非法或重复")
		}
	}
	return out, nil
}
