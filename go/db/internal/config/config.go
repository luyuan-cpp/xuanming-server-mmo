package config

import (
	"fmt"

	"github.com/zeromicro/go-zero/zrpc"
)

// Config is the global configuration.
type Config struct {
	zrpc.RpcServerConf
	ZoneId       uint32       `json:"ZoneId"`
	ServerConfig ServerConfig `json:"ServerConfig"`

	// MetricsListenAddr is the bind address for the Prometheus /metrics
	// HTTP endpoint. Leave empty to disable (production default; opt-in
	// via deploy config). Dev default is set in etc/db.yaml so stress
	// runs can scrape per-task stage histograms alongside login :9101
	// and scene_manager :9150.
	MetricsListenAddr string `json:"MetricsListenAddr,optional"`
}

// ServerConfig holds core service settings.
type ServerConfig struct {
	Database    DatabaseConfig `json:"Database"`
	RedisClient RedisConfig    `json:"RedisClient"`
	JsonPath    string         `json:"JsonPath"`
	Kafka       KafkaConfig    `json:"Kafka"`
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Hosts       string `json:"Hosts"`
	User        string `json:"User"`
	Passwd      string `json:"Passwd"`
	DBName      string `json:"DBName,optional"` // Derived from ZoneId at startup
	MaxOpenConn int    `json:"MaxOpenConn"`
	MaxIdleConn int    `json:"MaxIdleConn"`
	Net         string `json:"Net"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Hosts             string `json:"Hosts"`
	DefaultTTLSeconds int    `json:"DefaultTTLSeconds"`
	Password          string `json:"Password"`
	DB                int    `json:"DB"`
}

// KafkaConfig holds Kafka consumer settings.
type KafkaConfig struct {
	Brokers         []string `json:"Brokers"`                      // Broker addresses
	GroupID         string   `json:"GroupID"`                      // Consumer group ID
	Topic           string   `json:"Topic,optional"`               // Derived from ZoneId at startup
	TopicGeneration uint32   `json:"TopicGeneration,default=1"`    // Immutable routing generation; partition changes require a new topic
	PartitionCnt    int32    `json:"PartitionCnt"`                 // Partition count
	RetentionMs     int64    `json:"RetentionMs,default=86400000"` // Topic retention in ms (default 24h; matches login)
	// P1 数据安全加固 2026-06-03: 旧值 300000 (5min)
	// db service 卡 5min+ 会丢数据,24h 给运维事故充足窗口
	IsOfflineExpand bool `json:"IsOfflineExpand"` // Offline expansion: true = maintenance mode

	// SubShardCount controls intra-partition parallelism. 0 or 1 = legacy
	// behaviour (single goroutine per partition, max parallelism =
	// PartitionCnt). N>1 = each partition's worker becomes a router that
	// fans tasks across N sub-goroutines by hash(task.Key), so per-key
	// ordering is preserved while overall throughput multiplies by N.
	//
	// Why this exists: under MaxOpenConn=30 the MySQL pool was massively
	// underused because partition=10 = strictly 10 in-flight queries.
	// With SubShardCount=4, effective parallelism becomes 10×4=40 and the
	// pool actually fills. See 2026-05-28 stress maxopenconn doc §3.
	SubShardCount int `json:"SubShardCount,optional"`
}

var AppConfig Config

// DbTaskTopic returns the zone-specific Kafka topic for DB tasks.
func DbTaskTopic(zoneId uint32) string {
	return fmt.Sprintf("db_task_zone_%d", zoneId)
}

func DbTaskTopicForGeneration(zoneId, generation uint32) string {
	base := DbTaskTopic(zoneId)
	if generation <= 1 {
		return base
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}

// ZoneDBName returns the zone-specific MySQL database name.
func ZoneDBName(zoneId uint32) string {
	return fmt.Sprintf("zone_%d_db", zoneId)
}
