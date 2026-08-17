// secrets.go —— login 的 HMAC 密钥治理。
//
// 它解决三件已核实的事:
//
//  1. **一把钥匙开全楼**:gate token 与排队 token 原来共用同一个
//     GateTokenSecret(理由记在 docs/design/login-queue-2026-05.md:194),
//     信任域没拆 —— 任何一侧泄露,另一侧同时失守。现在按用途拆成
//     GateToken / QueueToken / InternalAuth 三把,生产模式下禁止复用。
//  2. **换不了钥匙**:配置类型是单个 string,轮换必须停服。现在每把钥匙
//     都是「主密钥 + 若干只验不签的附加密钥」,支持三段式不停服轮换:
//     ① 新密钥先进 Additional(全部实例都能验) →
//     ② 提升为主密钥(开始用新密钥签) →
//     ③ 旧密钥从 Additional 摘掉。
//     每一步都能单独滚动发布,任意时刻新旧密钥签出的凭据都验得过。
//  3. **占位串上生产**:全仓 7 个文件写着同一个
//     "change-me-in-production-use-a-strong-random-key"。它进过 git、
//     进过部署脚本、进过文档,等于公开值。现在生产模式启动期直接拒绝启动。
//
// 校验强度按运行模式分档:dev/test 只打 WARN(本地起服不能被密钥卡住),
// 其余模式(rt/pre/pro)一律 fail-fast。判定口径与
// svc/auth_init.go 的 validateDevelopmentPasswordMode 保持一致:
// **只有 dev 和 test 是宽松档**,其它全按生产处理。
package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/service"
)

// MinSecretLen 是生产模式下 HMAC 密钥的最短字节数。
// HMAC-SHA256 的输出是 32 字节,密钥短于 32 字节时暴力搜索的空间
// 直接塌到密钥长度上,所以 32 是下限而不是建议值。
const MinSecretLen = 32

// LegacyPlaceholderSecret 是仓库里散落 7 处的占位密钥原文。
// 导出是为了让测试能引用同一个常量,不用再抄一遍字面量。
const LegacyPlaceholderSecret = "change-me-in-production-use-a-strong-random-key"

// placeholderPrefixes 是「一眼就是占位符」的前缀集合(小写比较)。
// 宁可误伤也不能放过:真随机密钥不会以这些词开头,而误伤的代价只是
// 运维换一个真密钥。
var placeholderPrefixes = []string{
	"change-me", "changeme", "change_me",
	"please-change", "replace-me",
	"placeholder", "example", "sample",
	"your-secret", "yoursecret",
	"test-secret", "dev-secret", "secret",
	"123456", "password",
	// etc/login.yaml 里的本地开发默认值统一以 "local-dev-" 开头。
	// 它们进过 git = 等于公开值,所以在这里显式钉死:生产模式绝不放行。
	"local-dev",
}

// IsPlaceholderSecret 判断一个密钥是否是占位串。
func IsPlaceholderSecret(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return false // 空由「未配置」分支管,不算占位
	}
	if s == LegacyPlaceholderSecret {
		return true
	}
	for _, p := range placeholderPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// IsRelaxedMode 返回该运行模式是否允许密钥校验降级为 WARN。
// 只有 go-zero 的 dev / test 两档宽松,其余(含空串默认值)按生产处理。
//
// ⚠️ 空 Mode 也算生产:go-zero 的 RpcServerConf.Mode 默认是 "pro",
// 但配置里显式写空串时不能落到宽松档 —— fail-closed 方向。
func IsRelaxedMode(mode string) bool {
	return mode == service.DevMode || mode == service.TestMode
}

// SecretConf 描述一把**按用途隔离**的 HMAC 密钥,并原生支持三段式轮换。
//
// 密钥来源优先级:Env > Value。生产环境应当只填 Env,
// 把密钥留在 K8s Secret / 环境变量里,不进受管配置文件
// —— 与 PasswordAuth.DSNEnv、DevPasswordAuth.SharedSecretEnv 同一套纪律。
type SecretConf struct {
	// Env 是**环境变量名**(不是密钥本身)。非空时优先于 Value 读取。
	Env string `json:"Env,optional"`
	// Value 是内联明文密钥,仅供本地开发;生产请改用 Env。
	Value string `json:"Value,optional"`
	// AdditionalEnv 列出「只验不签」的旧密钥所在的环境变量名。
	AdditionalEnv []string `json:"AdditionalEnv,optional"`
	// Additional 列出「只验不签」的旧密钥明文,同样仅供本地开发。
	Additional []string `json:"Additional,optional"`
}

// SecretsConf 汇总本服务用到的全部 HMAC 密钥,一个用途一把。
type SecretsConf struct {
	// GateToken 签发给客户端、由 cpp gate 校验的连接票据。
	// login 只签不验,所以 Additional 对它没有实际作用(留着是为了口径统一)。
	GateToken SecretConf `json:"GateToken,optional"`
	// QueueToken 是登录排队的不透明 token,login 自签自验,
	// Additional 在轮换期真正起作用。
	QueueToken SecretConf `json:"QueueToken,optional"`
	// InternalAuth 是内部调用方身份声明的签名密钥(SessionInterceptor 用)。
	// login 只验不签,Additional 让上游可以先切新密钥再摘旧密钥。
	InternalAuth SecretConf `json:"InternalAuth,optional"`
}

// SecretSet 是一把密钥解析后的运行期形态:一条主密钥 + 若干只验不签的旧密钥。
//
// 零值(或 nil)表示「没配」,Configured() 返回 false。所有方法都对 nil
// 接收者安全,免得调用点到处判空。
type SecretSet struct {
	primary    []byte
	additional [][]byte
}

// NewSecretSet 直接从明文构造,供测试与内部回落路径使用。
func NewSecretSet(primary string, additional ...string) *SecretSet {
	s := &SecretSet{}
	if primary != "" {
		s.primary = []byte(primary)
	}
	for _, a := range additional {
		if a != "" {
			s.additional = append(s.additional, []byte(a))
		}
	}
	return s
}

// Configured 表示这把密钥是否有可用的主密钥。
func (s *SecretSet) Configured() bool {
	return s != nil && len(s.primary) > 0
}

// Primary 返回签名用的主密钥。未配置时返回 nil
// —— 调用方必须自己判空,绝不能拿 nil 去签(那等于用空密钥签)。
func (s *SecretSet) Primary() []byte {
	if s == nil {
		return nil
	}
	return s.primary
}

// Candidates 返回校验时要依次尝试的全部密钥:主密钥在前,附加密钥在后。
// 轮换期新旧凭据都能验过,靠的就是这个列表。
func (s *SecretSet) Candidates() [][]byte {
	if s == nil || len(s.primary) == 0 {
		return nil
	}
	out := make([][]byte, 0, 1+len(s.additional))
	out = append(out, s.primary)
	out = append(out, s.additional...)
	return out
}

// Sign 用主密钥算 HMAC-SHA256(原始字节,不做十六进制编码)。
// 未配置时返回 nil,调用方应当把它当成「不能签」而不是「签出了空值」。
func (s *SecretSet) Sign(data []byte) []byte {
	if !s.Configured() {
		return nil
	}
	mac := hmac.New(sha256.New, s.primary)
	mac.Write(data)
	return mac.Sum(nil)
}

// Verify 用主密钥 + 全部附加密钥逐一常数时间比对。
//
// **不做短路**:即使第一把就匹配上了也把剩下的算完,免得从耗时差
// 反推出「命中的是哪一把密钥」这种旁路信息。密钥条数是个位数,代价可忽略。
func (s *SecretSet) Verify(mac, data []byte) bool {
	if !s.Configured() || len(mac) == 0 {
		return false
	}
	matched := false
	for _, key := range s.Candidates() {
		h := hmac.New(sha256.New, key)
		h.Write(data)
		if hmac.Equal(mac, h.Sum(nil)) {
			matched = true
		}
	}
	return matched
}

// resolve 把配置里的「环境变量名 / 内联明文」解析成运行期密钥。
//
// 显式指定了 Env 但环境变量缺失/为空 —— 这是**配置写错了**,任何模式下
// 都直接报错,不静默回落到 Value。与 passwordAuthDSNFromConfig 同一口径。
func (c SecretConf) resolve(name string, lookup func(string) (string, bool)) (*SecretSet, []string, error) {
	var warnings []string
	set := &SecretSet{}

	if env := strings.TrimSpace(c.Env); env != "" {
		v, ok := lookup(env)
		if !ok || strings.TrimSpace(v) == "" {
			return nil, nil, fmt.Errorf("密钥 %s 指定了 Env=%q,但该环境变量缺失或为空", name, env)
		}
		set.primary = []byte(v)
		if c.Value != "" {
			warnings = append(warnings,
				fmt.Sprintf("密钥 %s 同时配了 Env 和 Value,以 Env 为准;请把 Value 删掉", name))
		}
	} else if c.Value != "" {
		set.primary = []byte(c.Value)
	}

	for _, env := range c.AdditionalEnv {
		env = strings.TrimSpace(env)
		if env == "" {
			continue
		}
		v, ok := lookup(env)
		if !ok || strings.TrimSpace(v) == "" {
			return nil, nil, fmt.Errorf("密钥 %s 的 AdditionalEnv 列出了 %q,但该环境变量缺失或为空", name, env)
		}
		set.additional = append(set.additional, []byte(v))
	}
	for _, v := range c.Additional {
		if v == "" {
			continue
		}
		set.additional = append(set.additional, []byte(v))
	}

	if len(set.primary) == 0 && len(set.additional) > 0 {
		return nil, nil, fmt.Errorf("密钥 %s 只配了 Additional 没配主密钥:轮换期也必须始终有一条主密钥用于签名", name)
	}
	return set, warnings, nil
}

// validate 按运行模式检查一把密钥的强度。
//
// required=false 时「没配」直接放过 —— 例如队列没开就不需要 QueueToken。
// 但只要配了,强度检查一视同仁:一条弱的**附加**密钥同样能签出可信凭据,
// 所以附加密钥和主密钥用同一把尺子量。
func (s *SecretSet) validate(name string, relaxed, required bool) (warnings []string, err error) {
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if relaxed {
			warnings = append(warnings, msg)
			return
		}
		if err == nil {
			err = fmt.Errorf("%s", msg)
		}
	}

	if !s.Configured() {
		if required {
			fail("密钥 %s 未配置:生产模式下这把密钥是必需的(配 Secrets.%s.Env 指向环境变量)", name, name)
		}
		return warnings, err
	}

	for i, key := range s.Candidates() {
		slot := "主密钥"
		if i > 0 {
			slot = fmt.Sprintf("附加密钥#%d", i)
		}
		if IsPlaceholderSecret(string(key)) {
			fail("密钥 %s 的%s是占位串:它已经进过 git / 部署脚本 / 文档,等于公开值", name, slot)
			continue
		}
		if len(key) < MinSecretLen {
			fail("密钥 %s 的%s只有 %d 字节,低于 %d 字节下限", name, slot, len(key), MinSecretLen)
		}
	}
	return warnings, err
}

// ResolvedSecrets 是解析 + 校验之后的全部运行期密钥。
type ResolvedSecrets struct {
	GateToken    *SecretSet
	QueueToken   *SecretSet
	InternalAuth *SecretSet
}

// ActiveSecrets 是进程启动期解析好的密钥集合,由 main 在 conf.MustLoad
// 之后立刻赋值。与 AppConfig 同一套「进程级单例」惯例。
//
// 之所以不放进 ServiceContext:密钥校验必须**早于**任何外部连接
// (Redis / Kafka / etcd),密钥不合格时进程要在建连之前就退出。
var ActiveSecrets *ResolvedSecrets

// EnforceInternalAuth 返回是否强制校验内部调用方签名。
//
// 生产模式恒为 true 且**关不掉** —— 配置里没有任何开关能在生产放行未签名
// 的身份声明。ForceEnforce 只用于在 dev/test 提前把强制档打开做联调。
func EnforceInternalAuth(cfg *Config) bool {
	if !IsRelaxedMode(cfg.Mode) {
		return true
	}
	return cfg.InternalAuth.ForceEnforce
}

// ResolveSecrets 解析并校验全部密钥。返回的 warnings 由调用方打日志;
// 返回 error 表示配置不可用,调用方应当直接拒绝启动。
//
// lookup 参数化只为可测,生产固定传 os.LookupEnv。
func ResolveSecrets(cfg *Config, lookup func(string) (string, bool)) (*ResolvedSecrets, []string, error) {
	relaxed := IsRelaxedMode(cfg.Mode)

	var warnings []string
	collect := func(w []string) { warnings = append(warnings, w...) }

	gate, w, err := cfg.Secrets.GateToken.resolve("GateToken", lookup)
	if err != nil {
		return nil, warnings, err
	}
	collect(w)

	queue, w, err := cfg.Secrets.QueueToken.resolve("QueueToken", lookup)
	if err != nil {
		return nil, warnings, err
	}
	collect(w)

	internal, w, err := cfg.Secrets.InternalAuth.resolve("InternalAuth", lookup)
	if err != nil {
		return nil, warnings, err
	}
	collect(w)

	// 兼容旧部署:顶层 GateTokenSecret(单个 string)仍然可用,但只回落给
	// gate token 这一把。deploy/*.yaml 与 k8s_deploy.ps1 目前还在写这个字段,
	// 删掉它会让那些部署静默变成「没配密钥」。
	if !gate.Configured() && cfg.GateTokenSecret != "" {
		gate = NewSecretSet(cfg.GateTokenSecret)
		warnings = append(warnings,
			"顶层 GateTokenSecret 已废弃(单个 string 无法不停服轮换),请改用 Secrets.GateToken;本次按兼容路径读取")
	}

	// QueueToken 没配时的回落:**只在宽松模式下**沿用 gate 密钥,保持本地
	// 开发的现状行为;生产模式一律不回落,由下面的 required 检查报错。
	// 复用密钥正是这次要修的问题之一,不能让它悄悄延续到生产。
	if !queue.Configured() && relaxed && gate.Configured() {
		queue = &SecretSet{primary: gate.Primary(), additional: gate.additional}
		warnings = append(warnings,
			"QueueToken 未配置,开发模式回落到 GateToken 密钥;生产必须配独立的 Secrets.QueueToken")
	}

	// 谁是必需的:
	//   - GateToken:AssignGate 每次都要签票据,恒必需。
	//   - QueueToken:只有开了登录排队才会签/验排队 token。
	//   - InternalAuth:只有强制验签时才必需(生产恒强制)。
	var firstErr error
	keep := func(w []string, e error) {
		collect(w)
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	keep(gate.validate("GateToken", relaxed, true))
	keep(queue.validate("QueueToken", relaxed, cfg.Queue.Enabled))
	keep(internal.validate("InternalAuth", relaxed, EnforceInternalAuth(cfg)))
	if firstErr != nil {
		return nil, warnings, firstErr
	}

	// 不同用途不许复用同一把主密钥。生产直接拒绝启动;开发只打 WARN
	// (本地经常图省事三把填一样)。
	pairs := []struct {
		a, b string
		x, y *SecretSet
	}{
		{"GateToken", "QueueToken", gate, queue},
		{"GateToken", "InternalAuth", gate, internal},
		{"QueueToken", "InternalAuth", queue, internal},
	}
	for _, p := range pairs {
		if !p.x.Configured() || !p.y.Configured() {
			continue
		}
		if !hmac.Equal(p.x.Primary(), p.y.Primary()) {
			continue
		}
		msg := fmt.Sprintf("密钥 %s 与 %s 主密钥相同:不同用途必须用不同密钥,否则一处泄露即全线失守", p.a, p.b)
		if relaxed {
			warnings = append(warnings, msg)
			continue
		}
		return nil, warnings, fmt.Errorf("%s", msg)
	}

	return &ResolvedSecrets{GateToken: gate, QueueToken: queue, InternalAuth: internal}, warnings, nil
}
