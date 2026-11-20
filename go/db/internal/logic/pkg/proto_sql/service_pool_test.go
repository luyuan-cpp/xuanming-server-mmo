package proto_sql

// 业务连接池的连接参数(launch-todo §2.3 第 9 条)。
//
// 改 config.AppConfig 的用例在 t.Cleanup 里还原,不能 t.Parallel。

import (
	"testing"
	"time"

	"db/internal/config"
)

// withDatabaseConfig 临时改写 AppConfig 的数据库段,用例结束时整份还原。
func withDatabaseConfig(t *testing.T, mutate func(d *config.DatabaseConfig)) {
	t.Helper()
	saved := config.AppConfig
	t.Cleanup(func() { config.AppConfig = saved })
	mutate(&config.AppConfig.ServerConfig.Database)
}

func TestNewServiceMysqlConfig_SetsTimeoutsAndSessionWaits(t *testing.T) {
	withDatabaseConfig(t, func(d *config.DatabaseConfig) {
		d.DialTimeoutSeconds = config.DefaultDatabaseDialTimeoutSeconds
		d.ReadTimeoutSeconds = config.DefaultDatabaseReadTimeoutSeconds
		d.WriteTimeoutSeconds = config.DefaultDatabaseWriteTimeoutSeconds
		d.SessionLockWaitSeconds = config.DefaultDatabaseSessionLockWaitSeconds
		d.SessionInnodbLockWaitSeconds = config.DefaultDatabaseSessionInnodbLockWaitSeconds
	})

	cfg := newServiceMysqlConfig()

	if cfg.Timeout != 5*time.Second || cfg.ReadTimeout != 30*time.Second || cfg.WriteTimeout != 30*time.Second {
		t.Fatalf("timeouts = dial %v read %v write %v, want 5s / 30s / 30s", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
	if got := cfg.Params["lock_wait_timeout"]; got != "10" {
		t.Fatalf("lock_wait_timeout = %q, want 10", got)
	}
	if got := cfg.Params["innodb_lock_wait_timeout"]; got != "10" {
		t.Fatalf("innodb_lock_wait_timeout = %q, want 10", got)
	}
	if got := cfg.Params["sql_mode"]; got != "'STRICT_TRANS_TABLES'" {
		t.Fatalf("sql_mode = %q, the strict session mode must be kept", got)
	}
}

// 负数表示不设:驱动不加超时,也不发对应的会话变量(例如 TiDB 不接受 lock_wait_timeout 时写 -1)。
func TestNewServiceMysqlConfig_NegativeDisables(t *testing.T) {
	withDatabaseConfig(t, func(d *config.DatabaseConfig) {
		d.DialTimeoutSeconds = -1
		d.ReadTimeoutSeconds = -1
		d.WriteTimeoutSeconds = -1
		d.SessionLockWaitSeconds = -1
		d.SessionInnodbLockWaitSeconds = -1
	})

	cfg := newServiceMysqlConfig()

	if cfg.Timeout != 0 || cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 {
		t.Fatalf("negative seconds must disable the timeouts, got dial %v read %v write %v", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
	for _, name := range []string{"lock_wait_timeout", "innodb_lock_wait_timeout"} {
		if v, ok := cfg.Params[name]; ok {
			t.Fatalf("%s = %q must not be sent when disabled", name, v)
		}
	}
}

// 迁移入口用的 NewMysqlConfig 不带业务池的超时与会话锁等待:DDL 可能合法地跑几分钟。
func TestNewMysqlConfig_HasNoServiceTimeouts(t *testing.T) {
	withDatabaseConfig(t, func(d *config.DatabaseConfig) {
		d.DialTimeoutSeconds = 5
		d.ReadTimeoutSeconds = 30
		d.WriteTimeoutSeconds = 30
		d.SessionLockWaitSeconds = 10
		d.SessionInnodbLockWaitSeconds = 10
	})

	cfg := NewMysqlConfig()

	if cfg.Timeout != 0 || cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 {
		t.Fatalf("migration config must not carry service timeouts, got dial %v read %v write %v", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
	for _, name := range []string{"lock_wait_timeout", "innodb_lock_wait_timeout"} {
		if _, ok := cfg.Params[name]; ok {
			t.Fatalf("migration config must not set %s", name)
		}
	}
}
