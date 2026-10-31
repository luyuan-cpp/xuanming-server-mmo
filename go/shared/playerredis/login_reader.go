package playerredis

// LoginReaderConfig 是读登录态的服务(chat、friend、guild、match、mail、trade)的 LoginRedis 配置段,必须指向 login 的 LoginRedis
// (docs/design/redis-player-store-cluster.md §10.2)。login 自己用 Config:它没写地址时落到自己的 Node.RedisClient,
// 这几个服务不一样,必须显式声明 login 的实例,不能由别的段猜。
type LoginReaderConfig struct {
	Addrs   []string `json:",optional"`
	Cluster bool     `json:",optional"`
	// Password、PasswordEnv 语义同 Config。
	Password    string `json:",optional"`
	PasswordEnv string `json:",optional"`
	// DB 只对单实例有意义,Cluster 下写非 0 拒启(Config.Validate)。
	DB              int `json:",optional"`
	PoolSizePerNode int `json:",optional"`
}

// HasAddr 报告这一段写了地址。没写地址的服务没有登录态可读,由各服务在启动校验里拒绝。
func (c LoginReaderConfig) HasAddr() bool {
	return len(c.Addrs) > 0
}

// ClientConfig 把这一段与调用方自己的超时、缺省连接池合成客户端配置。base 里的地址、口令、库号会被这一段覆盖。
func (c LoginReaderConfig) ClientConfig(base Config) Config {
	base.Addrs = c.Addrs
	base.Cluster = c.Cluster
	base.Password = c.Password
	base.PasswordEnv = c.PasswordEnv
	base.DB = c.DB
	if c.PoolSizePerNode != 0 {
		base.PoolSizePerNode = c.PoolSizePerNode
	}
	return base
}
