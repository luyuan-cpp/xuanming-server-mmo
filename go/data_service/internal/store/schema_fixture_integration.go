//go:build integration

package store

import (
	"context"
	"database/sql"
	"shared/authoritysql/sqltest"
)

// MigrateOwnedFixtureSchema仅编进集成测试，保留既有迁移引擎的幂等与旧结构回归。
// 生产二进制没有此入口；Schema能力和持久归属均由测试夹具自己持有。
func MigrateOwnedFixtureSchema(ctx context.Context, fixture *sqltest.Fixture, schema string, opts MigrateOptions) error {
	return fixture.PrepareOwnedSchema(ctx, schema, func(ctx context.Context, db *sql.DB) error {
		release, err := acquireMigrateLock(ctx, db, schema, opts.LockWait)
		if err != nil {
			return err
		}
		defer release()
		return migrateSchemaOn(ctx, db, schema, opts)
	})
}
