package sqlpoolstats

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// PrimaryPool 是只有一个连接池的服务统一用的类别名:那个池就是本服务独占的逻辑库
// (AGENTS.md §7 第 6 条「逻辑库自治」)。有多个池的服务(go/db、data_service)按自己的类别常量报,不用它。
const PrimaryPool = "primary"

// Single 返回只报一个池的 Source。db 为 nil(本进程没有装配这条 SQL,例如没开的可选功能)时不报样本,
// 收集器照常输出 pools=0 的那组序列:面板上「没开」和「池掉了」分得开。
func Single(pool string, db *sql.DB) Source {
	return func() []Sample {
		if db == nil {
			return nil
		}
		return []Sample{{Pool: pool, Stats: db.Stats()}}
	}
}

// Register 用 NewCollector 构造收集器(参数口径相同,非法即 panic),注册到 prometheus 默认注册表:
// 各服务的 /metrics —— 自建的 promhttp.Handler() 或 go-zero 的 prometheus agent —— 暴露的都是它。
//
// 在进程入口调一次,不要放进单测会多次构造的 ServiceContext:默认注册表重复注册会失败。
// 注册失败只记 ERROR:指标是观测面,不挡业务启动(与 go/db、data_service 的 RegisterSQLPools 同口径)。
// 收集器在每次抓取时现取 sql.DBStats,所以注册早于或晚于 /metrics 起监听都不丢数据。
func Register(service string, pools []string, source Source) {
	if err := prometheus.Register(NewCollector(service, pools, source)); err != nil {
		logx.Errorf("[sqlpoolstats] sql_client=%s register failed, pool metrics unavailable: %v", service, err)
	}
}
