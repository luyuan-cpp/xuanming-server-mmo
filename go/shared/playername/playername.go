// Package playername 实现角色名字的纯规则:规范化(算唯一键)、字符集与长度校验、
// 敏感词判定,以及空名时服务端生成默认名。
//
// 为什么单独成包:名字**全服唯一**,真源是 data_service 全局库的 player_name 表,
// 而建角流程在 login。两边必须跑**同一份**规范化与字符集实现 ——
//   - 规范化不同:两边算出不同的 name_norm,唯一键就挡不住重名;
//   - 字符集不同:login 放行、data_service 复检拒绝(或反过来),建角会在半路失败。
//
// 边界(故意做得很窄):本包不连库、不读配置文件、不打日志、不出指标、不依赖除
// Rules/GenerateSpec 之外的任何上下文对象。玩法数值(长度上下限、生成名前缀、随机
// 后缀位数)一律由调用方从 RoleNameRule 配表读出后传进来(用户决策:所有数值进配表),
// 包里只保留**结构**上限 StructuralMaxRunes —— 那是存储和客户端输入框的物理约束,
// 不是策划可调的玩法数值。
//
// 设计:docs/design/guild-phase2/03-names.md §3.8。
package playername

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	// 起别名而不是直接 import "golang.org/x/text/unicode/norm":本包里 norm(归一化
	// 后的唯一键)是一等概念,Normalize 的具名返回值就叫 norm,直接用包名 norm 会被
	// 具名返回值遮蔽,函数体里再也写不出 norm.NFKC。别名把这个坑一次性掐死。
	textnorm "golang.org/x/text/unicode/norm"
)

// StructuralMaxRunes 是结构上限(player_name.name 是 VARCHAR(64)、客户端输入框按
// 64 个 UTF-16 单元收),**不是玩法数值**:玩法长度在 RoleNameRule 配表里,由调用方
// 传进 Rules。32 个码点在最坏情况(全是扩展 A/B 区外的 BMP 汉字)也只占 96 字节 UTF-8、
// 32~64 个 UTF-16 单元,两端都装得下。
const StructuralMaxRunes = 32

// MaxGeneratedSuffixLen 是生成名随机后缀的位数上限。后缀只是防撞名的熵,6 位 [a-z0-9]
// 已有 36^6 ≈ 2.2e9 种;上限给到 16 是为了让「前缀+后缀」在任何合法 Rules 下都不至于
// 一定超长(MaxRunes ≤ 32),同时挡住配表把它填成天文数字。
const MaxGeneratedSuffixLen = 16

// 错误哨兵。调用方用 errors.Is 分类;具体哪一项不合法写在 wrap 的文本里(这些错误只在
// 进程启动/配表加载时出现,不进玩家可见路径,所以不需要更细的类型)。
var (
	// ErrInvalidRules:Rules 自身不自洽(见 Rules.Validate)。
	ErrInvalidRules = errors.New("playername: invalid rules")
	// ErrInvalidGenerateSpec:GenerateSpec 自身不自洽,或与 Rules 冲突(见 GenerateSpec.Validate)。
	ErrInvalidGenerateSpec = errors.New("playername: invalid generate spec")
	// ErrRandomSource:随机源读失败(含读到 EOF)。底层错误一并 wrap,可以继续 errors.Is(err, io.EOF)。
	ErrRandomSource = errors.New("playername: random source failed")
	// ErrGenerateExhausted:拒绝采样的次数上限被耗尽。正常随机源下概率约等于 0
	// (单次丢弃概率 4/256),出现它说明随机源坏了(例如恒定返回 0xFF),
	// 这时必须报错而不是死循环。
	ErrGenerateExhausted = errors.New("playername: random suffix generation exhausted")
)

// ReservePlayerNameResponse.result 的取值。data_service 产出、login 消费,
// 放在共享包里避免两边各写一套魔数。
const (
	ReserveOK      uint32 = 0 // 成功(含同 player_id 同名的幂等重试)
	ReserveTaken   uint32 = 1 // 该 name_norm 已被**别人**占用
	ReserveInvalid uint32 = 2 // 不合规(长度/字符集/敏感词)
)

// Rules 是玩法长度规则,来自 RoleNameRule 配表(min_chars / max_chars)。
// 单位是 Unicode 码点(rune),不是字节、也不是 UTF-16 单元:一个汉字算 1。
type Rules struct {
	MinRunes int
	MaxRunes int
}

// StructuralRules 是 data_service 侧**结构复检**用的规则:只保证存得下,不复述玩法长度。
// 为什么 data_service 不直接用配表规则:data_service 是全局服务,不加载 zone 的玩法配表;
// 玩法长度由 login(建角入口)把关,data_service 只兜住「写进 VARCHAR(64) 会不会炸」
// 和字符集这两件事,fail-closed 但不与配表耦合。
var StructuralRules = Rules{MinRunes: 1, MaxRunes: StructuralMaxRunes}

// Validate 校验 1 ≤ MinRunes ≤ MaxRunes ≤ StructuralMaxRunes。
// 调用方应在配表加载时调用一次并让进程启动失败,而不是等到玩家建角才发现。
func (r Rules) Validate() error {
	if r.MinRunes < 1 {
		return fmt.Errorf("%w: MinRunes=%d, 必须 ≥ 1", ErrInvalidRules, r.MinRunes)
	}
	if r.MaxRunes > StructuralMaxRunes {
		return fmt.Errorf("%w: MaxRunes=%d, 超过结构上限 %d", ErrInvalidRules, r.MaxRunes, StructuralMaxRunes)
	}
	if r.MinRunes > r.MaxRunes {
		return fmt.Errorf("%w: MinRunes=%d > MaxRunes=%d", ErrInvalidRules, r.MinRunes, r.MaxRunes)
	}
	return nil
}

// Verdict 是 Normalize 的判定结果。
type Verdict uint8

const (
	VerdictOK        Verdict = iota // 合规
	VerdictEmpty                    // 去空白后为空:建角时由服务端生成默认名(见 Generate)
	VerdictInvalid                  // 长度 / 字符集 / 非法 UTF-8
	VerdictSensitive                // 命中敏感词
)

// String 只用于日志与测试失败信息,不进协议、不进配表。
func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictEmpty:
		return "empty"
	case VerdictInvalid:
		return "invalid"
	case VerdictSensitive:
		return "sensitive"
	default:
		return fmt.Sprintf("verdict(%d)", uint8(v))
	}
}

// Normalize 按固定顺序把玩家输入的原始名字归一化,并给出判定。
//
// 返回:
//   - display:入库 / 展示用的名字(NFKC + TrimSpace 之后的形态);
//   - norm:唯一键 name_norm(display 再 ToLower),data_service 的唯一索引建在它上面;
//   - v:判定。
//
// VerdictOK 与 VerdictSensitive 会返回 display/norm(敏感词也返回,便于日志打出到底
// 是什么名字被拒);VerdictEmpty / VerdictInvalid 一律返回空串 —— 调用方拿不到一个
// 「没过校验却看着能用」的字符串,免得顺手写进库。
//
// 步骤顺序是契约的一部分,和帮会名(go/guild/internal/data.GuildNameNorm)保持同一套
// 公式(NFKC → TrimSpace → ToLower),客户端预校验也照抄:
//  1. 非法 UTF-8 → Invalid(先挡住,后面的 NFKC 会把坏字节换成 U+FFFD,判定就糊了);
//  2. NFKC:全角字母数字 ＡＢ１２ → AB12、表意空格 U+3000 → 空格、多数兼容汉字 → 正字。
//     这一步是防「看着一样、码点不同」的冒名的主力;
//  3. TrimSpace;空 → Empty;
//  4. 码点数落在 [MinRunes, MaxRunes] 之外 → Invalid;
//  5. 逐 rune 过 IsAllowedRune → Invalid;
//  6. display = s,norm = ToLower(s);
//  7. 敏感词 → Sensitive。
//
// 注意 rules 不在这里做自检:Normalize 在玩家每次建角时都会跑,自检属于启动期的事
// (Rules.Validate)。后果是「忘了加载配表」会得到 Rules{0,0},于是任何非空名都
// 落在 [0,0] 之外被判 Invalid —— 方向是 fail-closed(拒绝建角),不会放行脏数据。
func Normalize(raw string, rules Rules) (display, norm string, v Verdict) {
	if !utf8.ValidString(raw) {
		return "", "", VerdictInvalid
	}
	s := textnorm.NFKC.String(raw)
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", VerdictEmpty
	}
	if n := utf8.RuneCountInString(s); n < rules.MinRunes || n > rules.MaxRunes {
		return "", "", VerdictInvalid
	}
	for _, r := range s {
		if !IsAllowedRune(r) {
			return "", "", VerdictInvalid
		}
	}
	display = s
	norm = strings.ToLower(s)
	if DefaultSensitive != nil && DefaultSensitive.Contains(norm) {
		return display, norm, VerdictSensitive
	}
	return display, norm, VerdictOK
}

// 允许的汉字区间。写成显式区间而不是 unicode.Han,理由:
//   - unicode.Han 还包含部首补充 U+2E80–2EF3(除 U+2E9F、U+2EF3 外 NFKC **不**折叠)、
//     々 U+3005、杭州码 U+3021–3029、〻 U+303B。这些字形与正字极像却折叠不到正字,
//     norm 不同 → 唯一键拦不住,正好用来冒名(「官方」vs「官⺁」这类)。
//     U+3038–303A 虽然也在 Han 里,但 NFKC 会折叠成 十/卄/卅,折叠后与正字同 norm,
//     所以它们无害 —— 被 IsAllowedRune 拒的是**未折叠前**的码点,Normalize 里它们
//     早已变成正字,不会走到这里。
//   - 扩展 B 及以后(U+20000+)v1 不开放:客户端字体覆盖没核过,放开会出方块名。
//   - 未被 NFKC 折叠的兼容汉字(如 U+FA0E)天然不在区间内,login 与 data_service
//     两端会一致拒绝。
const (
	runeIdeographicZero = 0x3007 // 〇:汉字数字零,不在 U+4E00–9FFF 里,单独放行
	runeExtAFirst       = 0x3400 // CJK 扩展 A 起
	runeExtALast        = 0x4DBF // CJK 扩展 A 止
	runeBasicFirst      = 0x4E00 // CJK 基本区起
	runeBasicLast       = 0x9FFF // CJK 基本区止
)

// IsAllowedRune 判断单个码点是否允许出现在角色名里(**已归一化之后**的码点)。
// 客户端有一份逐行对照的实现,向量表 testdata/charset_vectors.json 两端共用。
func IsAllowedRune(r rune) bool {
	return (r >= '0' && r <= '9') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		r == runeIdeographicZero ||
		(r >= runeExtAFirst && r <= runeExtALast) ||
		(r >= runeBasicFirst && r <= runeBasicLast)
}

// GenerateSpec 是服务端生成默认名的参数,来自 RoleNameRule 配表
// (generated_prefix / generated_suffix_len)。
//
// 什么时候会用到:建角必填名字是客户端界面的约束,服务端仍会收到空名 ——
// 机器人(robot 发空 CreatePlayerRequest)和任何绕过 UI 的路径。这时服务端生成
// 「前缀 + 随机后缀」,而不是拒绝建角。
type GenerateSpec struct {
	Prefix    string // 默认名前缀,配表默认「道友」
	SuffixLen int    // 随机后缀位数,配表默认 6
}

// Validate 校验生成参数在给定 Rules 下能产出一个必定合规的名字:
//   - rules 自身合法;
//   - Prefix 非空(见下方「为什么不许空前缀」);
//   - Prefix 每个 rune 都 IsAllowedRune(顺带挡住非法 UTF-8:range 会给出 U+FFFD);
//   - Prefix 不命中敏感词;
//   - 1 ≤ SuffixLen ≤ MaxGeneratedSuffixLen;
//   - 前缀码点数 + SuffixLen ∈ [MinRunes, MaxRunes]。
//
// 为什么不许空前缀:后缀字母表是 [a-z0-9],若前缀为空,生成名可能自己撞上敏感词规则
// (ASCII 词 "gm" 是**前缀**匹配,"gm3f7k" 会被判 Sensitive),于是服务端生成的名字
// 过不了服务端自己的校验。要求前缀非空、且前缀本身不敏感,就把这条路堵死了 ——
// 中文前缀下,[a-z0-9] 拼不出任何中文敏感词,ASCII 前缀规则也不会在中段命中。
//
// 这个方法应该在配表加载时调用一次并让启动失败,而不是等到有人建角。
func (g GenerateSpec) Validate(rules Rules) error {
	if err := rules.Validate(); err != nil {
		return err
	}
	if g.Prefix == "" {
		return fmt.Errorf("%w: Prefix 为空", ErrInvalidGenerateSpec)
	}
	for _, r := range g.Prefix {
		if !IsAllowedRune(r) {
			return fmt.Errorf("%w: Prefix %q 含不允许的字符 U+%04X", ErrInvalidGenerateSpec, g.Prefix, r)
		}
	}
	if DefaultSensitive != nil && DefaultSensitive.Contains(strings.ToLower(g.Prefix)) {
		return fmt.Errorf("%w: Prefix %q 命中敏感词", ErrInvalidGenerateSpec, g.Prefix)
	}
	if g.SuffixLen < 1 || g.SuffixLen > MaxGeneratedSuffixLen {
		return fmt.Errorf("%w: SuffixLen=%d, 必须在 [1, %d]", ErrInvalidGenerateSpec, g.SuffixLen, MaxGeneratedSuffixLen)
	}
	total := utf8.RuneCountInString(g.Prefix) + g.SuffixLen
	if total < rules.MinRunes || total > rules.MaxRunes {
		return fmt.Errorf("%w: 前缀 %d 字 + 后缀 %d 位 = %d 字, 不在 [%d, %d]",
			ErrInvalidGenerateSpec, utf8.RuneCountInString(g.Prefix), g.SuffixLen, total, rules.MinRunes, rules.MaxRunes)
	}
	return nil
}

// generateAlphabet 是随机后缀的字母表:36 个字符,全部 IsAllowedRune 为 true,
// 且全是小写 —— display 与 norm 在后缀部分完全一致,不会因为大小写产生「看着不同、
// norm 相同」的两个生成名。
const generateAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// generateRejectFrom = 252 = 36 × 7。随机字节 ≥ 252 直接丢弃重取(拒绝采样):
// 256 不是 36 的整数倍,直接 b % 36 会让前 4 个字符的概率高 1/7,长期看生成名的分布
// 有偏,撞名率上升。丢掉那 4/256 的取值就没有偏了。
const generateRejectFrom = 252

// Generate 生成「Prefix + SuffixLen 位 [a-z0-9]」的默认名。
//
// 随机源由调用方注入:生产传 crypto/rand.Reader(名字要不可预测,否则能提前把别人
// 想要的名字抢注掉);测试传固定字节,结果就是确定的(AGENTS.md §11.4)。
// 绝不使用全局 math/rand。
//
// 这里只做最低限度的自检(reader 非 nil、SuffixLen 在范围内);Prefix 与 Rules 的
// 一致性属于启动期的 GenerateSpec.Validate,不在每次生成时重复跑。
func Generate(r io.Reader, spec GenerateSpec) (string, error) {
	if r == nil {
		return "", fmt.Errorf("%w: reader 为 nil", ErrInvalidGenerateSpec)
	}
	if spec.SuffixLen < 1 || spec.SuffixLen > MaxGeneratedSuffixLen {
		return "", fmt.Errorf("%w: SuffixLen=%d, 必须在 [1, %d]", ErrInvalidGenerateSpec, spec.SuffixLen, MaxGeneratedSuffixLen)
	}

	var b strings.Builder
	b.Grow(len(spec.Prefix) + spec.SuffixLen)
	b.WriteString(spec.Prefix)

	// maxAttempts 是死循环保险丝:正常随机源下单次被丢弃的概率是 4/256,取 64 倍余量
	// 之后「合法失败」的概率小到可以忽略;真触发了一定是随机源坏了(比如恒返回 0xFF)。
	maxAttempts := spec.SuffixLen*64 + 64
	var buf [1]byte
	for got, attempts := 0, 0; got < spec.SuffixLen; {
		if attempts >= maxAttempts {
			return "", fmt.Errorf("%w: 取 %d 位后缀试了 %d 次", ErrGenerateExhausted, spec.SuffixLen, attempts)
		}
		attempts++
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", fmt.Errorf("%w: %w", ErrRandomSource, err)
		}
		if buf[0] >= generateRejectFrom {
			continue
		}
		b.WriteByte(generateAlphabet[int(buf[0])%len(generateAlphabet)])
		got++
	}
	return b.String(), nil
}

// SensitiveChecker 判断归一化后的名字是否命中敏感词。
// 入参是 Normalize 产出的 norm(已 NFKC + TrimSpace + ToLower),实现不必再自己归一化。
type SensitiveChecker interface {
	Contains(norm string) bool
}

// DefaultSensitive 是全包共用的敏感词实现。
//
// 只允许在进程启动、开始对外服务**之前**替换(例如换成从运营词库加载的实现):
// 它是普通包变量,没有锁,运行期改会和正在建角的请求产生数据竞争。
// 置为 nil 表示不做敏感词判定(Normalize 与 GenerateSpec.Validate 都会跳过这一步)。
var DefaultSensitive SensitiveChecker = builtinSensitive{}

// builtinSensitiveSubstrings:中文词按**子串**匹配。中文没有词边界,冒充「官方」的名字
// 通常是「官方客服小助手」这种带前后缀的形态,只做相等匹配等于没做。
var builtinSensitiveSubstrings = []string{"管理员", "客服", "官方", "系统", "运营"}

// builtinSensitivePrefixes:ASCII 词只做**前缀**匹配。做子串会误伤正常名字 ——
// "sigma"、"magma" 里都有 "gm",而冒充 GM 的名字基本都是 "GM01"、"gm-service" 这种
// 以它开头的形态。
var builtinSensitivePrefixes = []string{"gm"}

// builtinSensitive 是占位词表:v1 先把最容易冒充官方身份的几个词堵上。
// 风险已知:词表来源未定(设计 §3.8 E1 未决),覆盖面远不够,后续应换成运营词库实现;
// 换实现时请保持「入参已归一化」这条契约。
type builtinSensitive struct{}

func (builtinSensitive) Contains(norm string) bool {
	for _, w := range builtinSensitiveSubstrings {
		if strings.Contains(norm, w) {
			return true
		}
	}
	for _, p := range builtinSensitivePrefixes {
		if strings.HasPrefix(norm, p) {
			return true
		}
	}
	return false
}
