package idsegment

// Conf 是服务 yaml 里的 `IdSegment:` 块,login / guild 共用一份定义,字段语义只解释一次。
//
// # 为什么 Enabled 没有 default=true
//
// go-zero 的 conf.MustLoad 对标了 optional 的结构体字段**整块缺失**时不会递归填内层
// default(见 login/internal/config.KillSwitchConf 的注释)。若写 `default=true`,
// 就会出现「块写了但漏了 Enabled → 开;块整个没写 → 关」这种看配置猜不出来的分叉。
// 所以规则收成一条:**只有显式写了 `Enabled: true` 号段才开**;没写块 = 关 = 纯 snowflake
// 老路径(这正是设计稿 §6.4 说的「保留一个版本做回滚开关」)。
type Conf struct {
	// Enabled 打开号段发号;false 时不拨 data_service,完全走 snowflake。
	Enabled bool `json:"Enabled,optional"`
	// Step 是**初始**领段长度(player / guild 100)。之后每段按消耗时长在 [MinStep, MaxStep]
	// 内自适应:不到 15 分钟用完翻倍、超过 30 分钟才用完减半(Leaf 口径,设计稿 §7.5 第 4 条)。
	Step uint32 `json:"Step,default=100"`
	// MinStep / MaxStep 是动态 step 的下 / 上限;不写(0)取 10 / 1000(默认值夹不住 Step 时
	// 收成 Step 本身)。建角 / 建帮速率很小,上限压在 1000 是为了让每天重启一次的浪费
	// (≤ 2×step)可以忽略。显式给值必须满足 MinStep ≤ Step ≤ MaxStep,否则起服即拒绝 ——
	// 配错了要炸在启动日志里,不能静默改成别的数。
	MinStep uint32 `json:"MinStep,optional"`
	MaxStep uint32 `json:"MaxStep,optional"`
	// FallbackToSnowflake:号段失败时回退 snowflake 发号。回退是安全的(号段值域与存量
	// snowflake 号的精确边界见 client.go 包注释「与存量 snowflake 号不撞」),但会让
	// snowflake 机器永远留着 —— 默认关,号段失败即本次建角 / 建帮整体失败(设计稿 §6.4)。
	FallbackToSnowflake bool `json:"FallbackToSnowflake,default=false"`
}

// StepOrDefault 兜住 Step=0(块缺失时 default 不生效),回到 100。
func (c Conf) StepOrDefault() uint32 {
	if c.Step == 0 {
		return 100
	}
	return c.Step
}

// MinStepOrDefault 兜住 MinStep=0:取 DefaultMinStep(10),但不高于 StepOrDefault(),
// 与 idsegment.New 对 0 值的处理一致 —— 起服日志打出来的界就是客户端实际用的界。
func (c Conf) MinStepOrDefault() uint32 {
	if c.MinStep == 0 {
		return min(DefaultMinStep, c.StepOrDefault())
	}
	return c.MinStep
}

// MaxStepOrDefault 兜住 MaxStep=0:取 DefaultMaxStep(1000),但不低于 StepOrDefault()。
func (c Conf) MaxStepOrDefault() uint32 {
	if c.MaxStep == 0 {
		return max(DefaultMaxStep, c.StepOrDefault())
	}
	return c.MaxStep
}
