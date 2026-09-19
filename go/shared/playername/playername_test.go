package playername

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// gameRules 是 RoleNameRule.xlsx 当前的玩法规则(min_chars=2、max_chars=12)。
// 写死在测试里而不是读配表:本包不依赖配表,测试也不该依赖 —— 配表改了数值,
// 改的是 login 的行为,不是本包的规则。
var gameRules = Rules{MinRunes: 2, MaxRunes: 12}

// generateSpec 是 RoleNameRule.xlsx 当前的生成名参数(generated_prefix=道友、
// generated_suffix_len=6)。
var generateSpec = GenerateSpec{Prefix: "道友", SuffixLen: 6}

func TestNormalize(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		rules       Rules
		wantVerdict Verdict
		wantDisplay string
		wantNorm    string
	}{
		{
			name: "首尾空白被 TrimSpace 去掉", raw: "  云中君 ", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "云中君", wantNorm: "云中君",
		},
		{
			// NFKC 的主力用途:全角字母数字折成半角,否则「ＡＢ１２」与「AB12」是两个名字。
			name: "全角字母数字被 NFKC 折成半角", raw: "ＡＢ１２", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "AB12", wantNorm: "ab12",
		},
		{
			// display 保留玩家输入的大小写,唯一性只看 norm。
			name: "大写保留在 display, norm 转小写", raw: "Ab12", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "Ab12", wantNorm: "ab12",
		},
		{
			name: "只有大小写不同的另一个写法", raw: "aB12", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "aB12", wantNorm: "ab12",
		},
		{
			name: "1 字短于 MinRunes", raw: "云", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "13 字超过 MaxRunes", raw: strings.Repeat("云", 13), rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			// 首尾空白会被去掉,中间的不会:空格不是允许字符。
			name: "名字中间的空格", raw: "云 中君", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "ASCII 标点", raw: "云中君!", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "emoji", raw: "\U0001F600\U0001F600", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "西里尔字母", raw: "Привет", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			// 下面四个都在 unicode.Han 里却 NFKC 不折叠,正是本包用显式区间表的理由。
			name: "U+3005 叠字符号 々", raw: "々々", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "U+3021 杭州码一 〡", raw: "〡〡", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "U+2E80 部首补充 ⺀", raw: "⺀⺀", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			// 兼容汉字里没有折叠映射的那十几个之一:NFKC 后原样保留,两端一致拒绝。
			name: "U+FA0E 不折叠的兼容汉字 﨎", raw: "﨎﨎", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			// 必须在 NFKC 之前挡住:NFKC 会把坏字节换成 U+FFFD,判定就糊成「字符集不合法」,
			// 日志里看不出真正的原因。
			name: "非法 UTF-8 字节", raw: "\xff\xfe", rules: gameRules,
			wantVerdict: VerdictInvalid,
		},
		{
			// 与上面三个相反:U+3038 会被 NFKC 折成 U+5341 十,折叠后与正字同 norm,
			// 冒不了名,所以应当放行。
			name: "U+3038 杭州码十 〸 被折成 十", raw: "〸〸", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "十十", wantNorm: "十十",
		},
		{
			// 空名不是错误:建角时服务端会生成默认名(机器人与无 UI 路径)。
			name: "全空白等于空名", raw: "   ", rules: gameRules,
			wantVerdict: VerdictEmpty,
		},
		{
			name: "全角空格也等于空名", raw: "　　", rules: gameRules,
			wantVerdict: VerdictEmpty,
		},
		{
			name: "结构规则下 1 字合法", raw: "云", rules: StructuralRules,
			wantVerdict: VerdictOK, wantDisplay: "云", wantNorm: "云",
		},
		{
			name: "结构规则下 32 字合法", raw: strings.Repeat("云", 32), rules: StructuralRules,
			wantVerdict: VerdictOK, wantDisplay: strings.Repeat("云", 32), wantNorm: strings.Repeat("云", 32),
		},
		{
			name: "结构规则下 33 字非法", raw: strings.Repeat("云", 33), rules: StructuralRules,
			wantVerdict: VerdictInvalid,
		},
		{
			name: "中文敏感词按子串命中", raw: "官方小助手", rules: gameRules,
			wantVerdict: VerdictSensitive, wantDisplay: "官方小助手", wantNorm: "官方小助手",
		},
		{
			// ASCII 敏感词按前缀命中;norm 已转小写,所以大写 GM 一样命中。
			name: "ASCII 敏感词按前缀命中", raw: "GM01", rules: gameRules,
			wantVerdict: VerdictSensitive, wantDisplay: "GM01", wantNorm: "gm01",
		},
		{
			// 反例:ASCII 词若按子串匹配,sigma 会被误杀。
			name: "sigma 中段含 gm 但不该命中", raw: "sigma", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "sigma", wantNorm: "sigma",
		},
		{
			// 边界:恰好 MinRunes。
			name: "恰好 2 字", raw: "云中", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "云中", wantNorm: "云中",
		},
		{
			// 边界:恰好 MaxRunes。
			name: "恰好 12 字", raw: strings.Repeat("云", 12), rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: strings.Repeat("云", 12), wantNorm: strings.Repeat("云", 12),
		},
		{
			// 〇 不在 4E00-9FFF 里,单独放行的那一条。
			name: "汉字数字零 〇", raw: "〇〇", rules: gameRules,
			wantVerdict: VerdictOK, wantDisplay: "〇〇", wantNorm: "〇〇",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			display, norm, v := Normalize(c.raw, c.rules)
			if v != c.wantVerdict {
				t.Fatalf("Normalize(%q, %+v) verdict = %v, want %v", c.raw, c.rules, v, c.wantVerdict)
			}
			if display != c.wantDisplay {
				t.Errorf("display = %q, want %q", display, c.wantDisplay)
			}
			if norm != c.wantNorm {
				t.Errorf("norm = %q, want %q", norm, c.wantNorm)
			}
		})
	}
}

// TestNormalize_CaseOnlyDifferenceSharesNorm 钉住唯一键的核心承诺:
// 只有大小写不同的两个名字算同一个名字,data_service 的唯一索引会挡住第二个。
func TestNormalize_CaseOnlyDifferenceSharesNorm(t *testing.T) {
	_, a, va := Normalize("Ab12", gameRules)
	_, b, vb := Normalize("aB12", gameRules)
	if va != VerdictOK || vb != VerdictOK {
		t.Fatalf("verdict = %v / %v, want ok", va, vb)
	}
	if a != b {
		t.Fatalf("norm = %q / %q, 只有大小写不同的名字必须得到同一个 norm", a, b)
	}
}

// TestNormalize_NFKCFoldedCharSharesNormWithCanonical 钉住 U+3038 这一类:
// 折叠后与正字同 norm,所以它既不是冒名通道、也不该被拒。
func TestNormalize_NFKCFoldedCharSharesNormWithCanonical(t *testing.T) {
	_, folded, vf := Normalize("〸〸", gameRules) // 〸〸
	_, plain, vp := Normalize("十十", gameRules)
	if vf != VerdictOK || vp != VerdictOK {
		t.Fatalf("verdict = %v / %v, want ok", vf, vp)
	}
	if folded != plain {
		t.Fatalf("norm = %q / %q, NFKC 折叠后必须与正字同 norm", folded, plain)
	}
}

// charsetVector 是 testdata/charset_vectors.json 里的一条向量。
type charsetVector struct {
	CP  string `json:"cp"`  // 十六进制码点,不带 U+ 前缀
	Why string `json:"why"` // 为什么该通过 / 该拒
}

type charsetVectors struct {
	Source   string          `json:"source"`
	Schema   string          `json:"schema"`
	Allowed  []charsetVector `json:"allowed"`
	Rejected []charsetVector `json:"rejected"`
}

// TestIsAllowedRune_Vectors 跑共享向量表。客户端(C#)有一份逐字节相同的副本,
// 两端判定必须一致 —— 不一致会出现 login 放行、data_service 复检拒绝的半路失败。
func TestIsAllowedRune_Vectors(t *testing.T) {
	const path = "testdata/charset_vectors.json"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var vectors charsetVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if len(vectors.Allowed) == 0 || len(vectors.Rejected) == 0 {
		t.Fatalf("向量表为空:allowed=%d rejected=%d", len(vectors.Allowed), len(vectors.Rejected))
	}

	check := func(t *testing.T, list []charsetVector, want bool, bucket string) {
		t.Helper()
		for _, vec := range list {
			// 每条都必须写明理由:这张表是两端共用的契约,没有理由的向量日后没人敢改。
			if strings.TrimSpace(vec.Why) == "" {
				t.Errorf("%s 的 %q 缺 why", bucket, vec.CP)
			}
			cp, err := strconv.ParseUint(vec.CP, 16, 32)
			if err != nil {
				t.Errorf("%s 的 %q 不是合法十六进制码点: %v", bucket, vec.CP, err)
				continue
			}
			if got := IsAllowedRune(rune(cp)); got != want {
				t.Errorf("IsAllowedRune(U+%s) = %v, want %v (%s)", vec.CP, got, want, vec.Why)
			}
		}
	}
	check(t, vectors.Allowed, true, "allowed")
	check(t, vectors.Rejected, false, "rejected")
}

func TestRulesValidate(t *testing.T) {
	cases := []struct {
		name    string
		rules   Rules
		wantErr bool
	}{
		{name: "配表当前值", rules: gameRules},
		{name: "结构规则", rules: StructuralRules},
		{name: "最窄的合法区间", rules: Rules{MinRunes: 1, MaxRunes: 1}},
		{name: "MinRunes 为 0", rules: Rules{MinRunes: 0, MaxRunes: 12}, wantErr: true},
		{name: "MaxRunes 超过结构上限", rules: Rules{MinRunes: 2, MaxRunes: 33}, wantErr: true},
		{name: "Min 大于 Max", rules: Rules{MinRunes: 5, MaxRunes: 3}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.rules.Validate()
			if c.wantErr {
				if err == nil {
					t.Fatalf("Validate(%+v) = nil, want error", c.rules)
				}
				if !errors.Is(err, ErrInvalidRules) {
					t.Fatalf("err = %v, want errors.Is(err, ErrInvalidRules)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", c.rules, err)
			}
		})
	}
}

func TestGenerateSpecValidate(t *testing.T) {
	cases := []struct {
		name       string
		spec       GenerateSpec
		rules      Rules
		wantErr    bool
		wantTarget error // 期望的错误哨兵;nil 表示不校验具体哨兵
	}{
		{name: "配表当前值", spec: generateSpec, rules: gameRules},
		{
			// 2 + 11 = 13 > MaxRunes(12):生成出来的名字自己过不了校验。
			name: "前缀加后缀超过 MaxRunes", spec: GenerateSpec{Prefix: "道友", SuffixLen: 11}, rules: gameRules,
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			name: "前缀加后缀短于 MinRunes", spec: GenerateSpec{Prefix: "道", SuffixLen: 1}, rules: Rules{MinRunes: 5, MaxRunes: 12},
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			name: "后缀 0 位", spec: GenerateSpec{Prefix: "道友", SuffixLen: 0}, rules: gameRules,
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			name: "后缀超过上限", spec: GenerateSpec{Prefix: "道友", SuffixLen: MaxGeneratedSuffixLen + 1}, rules: Rules{MinRunes: 2, MaxRunes: 32},
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			name: "前缀含不允许的字符", spec: GenerateSpec{Prefix: "道_友", SuffixLen: 6}, rules: gameRules,
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			name: "前缀命中敏感词", spec: GenerateSpec{Prefix: "官方", SuffixLen: 6}, rules: gameRules,
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			// 空前缀 + [a-z0-9] 后缀可能生成出 "gm...." 这种自己判自己敏感的名字。
			name: "空前缀", spec: GenerateSpec{Prefix: "", SuffixLen: 6}, rules: gameRules,
			wantErr: true, wantTarget: ErrInvalidGenerateSpec,
		},
		{
			// rules 先自检:否则 Rules{0,0} 下长度区间检查等于没做。
			name: "rules 本身非法", spec: generateSpec, rules: Rules{MinRunes: 0, MaxRunes: 12},
			wantErr: true, wantTarget: ErrInvalidRules,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate(c.rules)
			if !c.wantErr {
				if err != nil {
					t.Fatalf("Validate(%+v, %+v) = %v, want nil", c.spec, c.rules, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%+v, %+v) = nil, want error", c.spec, c.rules)
			}
			if c.wantTarget != nil && !errors.Is(err, c.wantTarget) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, c.wantTarget)
			}
		})
	}
}

// TestGenerate_FixedBytes:随机源是注入的,所以结果完全确定。
// 这条同时钉住拒绝采样:250、251 被接受,252-255 被跳过。
func TestGenerate_FixedBytes(t *testing.T) {
	src := []byte{250, 251, 252, 253, 254, 255, 0, 1, 2, 3}
	// 250 % 36 = 34 → '8';251 % 36 = 35 → '9';252-255 跳过;0,1,2,3 → 'a','b','c','d'。
	const want = "道友89abcd"
	got, err := Generate(bytes.NewReader(src), generateSpec)
	if err != nil {
		t.Fatalf("Generate = %v", err)
	}
	if got != want {
		t.Fatalf("Generate = %q, want %q", got, want)
	}
}

// TestGenerate_RejectionSamplingCoversAllBytes 把 0-255 全喂一遍:
// 必须恰好接受 252 个(36×7)、跳过 4 个,且每个被接受的字节 b 映射到 alphabet[b%36]。
// 这正是「去偏」的定义:每个字母被 7 个字节命中,概率相等。
func TestGenerate_RejectionSamplingCoversAllBytes(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	r := bytes.NewReader(all)

	const prefix = "道友"
	var got []byte
	var lastErr error
	for {
		name, err := Generate(r, GenerateSpec{Prefix: prefix, SuffixLen: 1})
		if err != nil {
			lastErr = err
			break
		}
		got = append(got, name[len(prefix):]...)
	}

	if len(got) != generateRejectFrom {
		t.Fatalf("接受了 %d 个字节, want %d(其余 %d 个应被拒绝采样丢弃)",
			len(got), generateRejectFrom, 256-generateRejectFrom)
	}
	for i, c := range got {
		if want := generateAlphabet[i%len(generateAlphabet)]; c != want {
			t.Fatalf("字节 %d 映射成 %q, want %q", i, string(c), string(want))
		}
	}
	// 4 个 ≥252 的字节被跳过后读到 EOF,必须报错而不是返回短名字。
	if !errors.Is(lastErr, ErrRandomSource) {
		t.Fatalf("末次错误 = %v, want errors.Is(err, ErrRandomSource)", lastErr)
	}
	if !errors.Is(lastErr, io.EOF) {
		t.Fatalf("末次错误 = %v, 应当 wrap 住底层 io.EOF", lastErr)
	}
}

// alwaysReader 恒返回同一个字节,用来模拟坏掉的随机源。
type alwaysReader struct{ b byte }

func (a alwaysReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = a.b
	}
	return len(p), nil
}

func TestGenerate_Errors(t *testing.T) {
	t.Run("随机源恒返回被拒的字节时报错而不是死循环", func(t *testing.T) {
		_, err := Generate(alwaysReader{b: 0xFF}, generateSpec)
		if !errors.Is(err, ErrGenerateExhausted) {
			t.Fatalf("err = %v, want errors.Is(err, ErrGenerateExhausted)", err)
		}
	})
	t.Run("随机源字节不够", func(t *testing.T) {
		_, err := Generate(bytes.NewReader([]byte{1, 2, 3}), generateSpec)
		if !errors.Is(err, ErrRandomSource) {
			t.Fatalf("err = %v, want errors.Is(err, ErrRandomSource)", err)
		}
	})
	t.Run("reader 为 nil", func(t *testing.T) {
		_, err := Generate(nil, generateSpec)
		if !errors.Is(err, ErrInvalidGenerateSpec) {
			t.Fatalf("err = %v, want errors.Is(err, ErrInvalidGenerateSpec)", err)
		}
	})
	t.Run("后缀位数越界", func(t *testing.T) {
		for _, n := range []int{0, -1, MaxGeneratedSuffixLen + 1} {
			if _, err := Generate(bytes.NewReader(make([]byte, 64)), GenerateSpec{Prefix: "道友", SuffixLen: n}); !errors.Is(err, ErrInvalidGenerateSpec) {
				t.Fatalf("SuffixLen=%d: err = %v, want errors.Is(err, ErrInvalidGenerateSpec)", n, err)
			}
		}
	})
}

// TestGenerate_CryptoRandAlwaysNormalizesOK 是闭环:生产随机源跑 1000 次,
// 每个生成名都必须能过本包自己的校验。断言的是性质而不是具体值,所以仍然是确定的测试。
func TestGenerate_CryptoRandAlwaysNormalizesOK(t *testing.T) {
	if err := generateSpec.Validate(gameRules); err != nil {
		t.Fatalf("配表当前的生成参数就不合法: %v", err)
	}
	shape := regexp.MustCompile(`^道友[a-z0-9]{6}$`)
	for i := 0; i < 1000; i++ {
		name, err := Generate(cryptorand.Reader, generateSpec)
		if err != nil {
			t.Fatalf("第 %d 次 Generate = %v", i, err)
		}
		if !shape.MatchString(name) {
			t.Fatalf("第 %d 次生成 %q, 不匹配 %s", i, name, shape)
		}
		display, norm, v := Normalize(name, gameRules)
		if v != VerdictOK {
			t.Fatalf("第 %d 次生成 %q, Normalize verdict = %v, want ok", i, name, v)
		}
		// 生成名全是小写 + 数字,归一化不该改动它:display/norm 都等于原串。
		if display != name || norm != name {
			t.Fatalf("第 %d 次生成 %q, display=%q norm=%q, 生成名必须已是规范形态", i, name, display, norm)
		}
	}
}

// fixedSensitive 是注入用的假词表,证明 SensitiveChecker 可替换
// (运营词库上线时 data_service / login 会在启动期换掉 DefaultSensitive)。
type fixedSensitive struct{ word string }

func (f fixedSensitive) Contains(norm string) bool { return strings.Contains(norm, f.word) }

func TestDefaultSensitiveIsReplaceable(t *testing.T) {
	original := DefaultSensitive
	defer func() { DefaultSensitive = original }()

	DefaultSensitive = fixedSensitive{word: "云中"}
	if _, _, v := Normalize("云中君", gameRules); v != VerdictSensitive {
		t.Fatalf("换词表后 verdict = %v, want sensitive", v)
	}
	if _, _, v := Normalize("官方小助手", gameRules); v != VerdictOK {
		t.Fatalf("内置词表应已被替换, verdict = %v, want ok", v)
	}

	// nil 表示不做敏感词判定:Normalize 与 GenerateSpec.Validate 都不能因此 panic。
	DefaultSensitive = nil
	if _, _, v := Normalize("官方小助手", gameRules); v != VerdictOK {
		t.Fatalf("DefaultSensitive 为 nil 时 verdict = %v, want ok", v)
	}
	if err := (GenerateSpec{Prefix: "官方", SuffixLen: 6}).Validate(gameRules); err != nil {
		t.Fatalf("DefaultSensitive 为 nil 时前缀不该被判敏感: %v", err)
	}
}

func TestVerdictString(t *testing.T) {
	cases := map[Verdict]string{
		VerdictOK:        "ok",
		VerdictEmpty:     "empty",
		VerdictInvalid:   "invalid",
		VerdictSensitive: "sensitive",
		Verdict(99):      "verdict(99)",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", uint8(v), got, want)
		}
	}
}
