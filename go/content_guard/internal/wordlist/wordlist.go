// Package wordlist 是词库:词条的形状、文本格式,以及编成字典树之后的匹配
// (docs/design/rename-and-content-safety.md §6.3、§6.4)。
//
// 本包只回答「这段文字里命中了哪些词、各是什么类别」。命中之后是拒绝还是打码由 engine 按场景决定。
// 不连库、不读配置、不打日志。
package wordlist

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Category 是词条的类别,决定命中后的动作。数值与 proto 的 TextCategory 相同(server 包的测试核对),
// 这里另写一份是为了让核心包不依赖生成代码。
type Category uint8

const (
	CategoryUnspecified   Category = 0
	CategoryPolitics      Category = 1 // 政治
	CategoryPorn          Category = 2 // 色情
	CategoryAbuse         Category = 3 // 辱骂
	CategoryAd            Category = 4 // 广告引流
	CategoryImpersonation Category = 5 // 冒充官方,只对名字类场景生效
	CategoryOther         Category = 6 // 其他违规
	categoryEnd           Category = 7
)

// categoryNames 是词库文本格式和指标标签里用的名字。数组长度就是 categoryEnd,加类别时两处一起改。
var categoryNames = [categoryEnd]string{"", "politics", "porn", "abuse", "ad", "impersonation", "other"}

// String 返回类别名;未指定或越界返回 "none"(只会出现在指标标签里,词库格式不接受它)。
func (c Category) String() string {
	if c == CategoryUnspecified || c >= categoryEnd {
		return "none"
	}
	return categoryNames[c]
}

// ParseCategory 按名字解析类别。
func ParseCategory(name string) (Category, bool) {
	for c := CategoryPolitics; c < categoryEnd; c++ {
		if categoryNames[c] == name {
			return c, true
		}
	}
	return CategoryUnspecified, false
}

// Mode 是词条的匹配方式。
type Mode uint8

const (
	// ModeSub 子串:出现在任何位置都算,字和字之间可以夹着可跳过的字符(空格、标点、符号)。中文词用它。
	ModeSub Mode = 0
	// ModeToken 整词:必须连续出现,而且前后不能紧挨着英文字母或数字。英文短词用它,否则 "gm" 会命中 "sigma"。
	ModeToken Mode = 1
	// ModePrefix 开头:只在整段文字的开头命中(开头的可跳过字符不算),中间可以夹可跳过的字符。
	// 给名字用:冒充 GM 的名字都是 "GM01" 这种以它开头的形态。
	ModePrefix Mode = 2
	modeEnd    Mode = 3
)

var modeNames = [modeEnd]string{"sub", "token", "prefix"}

func (m Mode) String() string {
	if m >= modeEnd {
		return "invalid"
	}
	return modeNames[m]
}

// ParseMode 按名字解析匹配方式。
func ParseMode(name string) (Mode, bool) {
	for m := ModeSub; m < modeEnd; m++ {
		if modeNames[m] == name {
			return m, true
		}
	}
	return ModeSub, false
}

// DefaultMode 是词条没写匹配方式时的取法:不超过 3 个字符的纯英文数字词按整词,其余按子串。
// 公开词库里有大量两三个字母的词,按子串会把正常的英文和拼音打掉一大片。
func DefaultMode(word string) Mode {
	count := 0
	for _, r := range word {
		if r >= utf8.RuneSelf {
			return ModeSub
		}
		count++
	}
	if count <= 3 {
		return ModeToken
	}
	return ModeSub
}

// Entry 是一条拒绝词。
type Entry struct {
	Word     string
	Category Category
	Mode     Mode
}

// Lexicon 是一份完整的词库:拒绝词加白名单。白名单词按子串匹配,命中段完全落在白名单词里面的拒绝词不算数。
type Lexicon struct {
	Deny  []Entry
	Allow []string
}

const (
	// MaxWordRunes 是单个词条的字符数上限。再长的「词」其实是一句话,不该进词库。
	MaxWordRunes = 64
	// MaxEntries 是一份词库的条数上限(拒绝词加白名单),只为挡住失控的输入。
	MaxEntries = 1_000_000
	// formatHeader 是词库文本的第一行。格式变了就换版本号,旧版本的服务会拒收而不是误读。
	formatHeader = "#content_guard_lexicon 1"
)

var (
	// ErrFormat 词库文本不符合格式。
	ErrFormat = errors.New("wordlist: malformed lexicon text")
	// ErrWord 词条本身不合法(空、过长、含制表符或换行、类别或匹配方式越界)。
	ErrWord = errors.New("wordlist: invalid word")
)

// ValidateWord 校验一个词能否写进词库文本:非空、合法 UTF-8、不超过 MaxWordRunes、不含制表符和换行。
func ValidateWord(word string) error {
	if word == "" {
		return fmt.Errorf("%w: empty", ErrWord)
	}
	if !utf8.ValidString(word) {
		return fmt.Errorf("%w: not valid UTF-8", ErrWord)
	}
	if strings.ContainsAny(word, "\t\r\n") {
		return fmt.Errorf("%w: contains a tab or line break", ErrWord)
	}
	if n := utf8.RuneCountInString(word); n > MaxWordRunes {
		return fmt.Errorf("%w: %d characters, the limit is %d", ErrWord, n, MaxWordRunes)
	}
	return nil
}

func validateEntry(e Entry) error {
	if err := ValidateWord(e.Word); err != nil {
		return err
	}
	if e.Category == CategoryUnspecified || e.Category >= categoryEnd {
		return fmt.Errorf("%w: category %d out of range", ErrWord, e.Category)
	}
	if e.Mode >= modeEnd {
		return fmt.Errorf("%w: mode %d out of range", ErrWord, e.Mode)
	}
	return nil
}

// Marshal 把词库写成文本。格式(制表符分隔,一行一条):
//
//	#content_guard_lexicon 1
//	D	<类别>	<匹配方式>	<词>
//	A	<白名单词>
//
// 顺序就是切片里的顺序;要稳定的输出由调用方先排好。任何一条不合法就整体失败。
func (l *Lexicon) Marshal() ([]byte, error) {
	if len(l.Deny)+len(l.Allow) > MaxEntries {
		return nil, fmt.Errorf("%w: %d entries, the limit is %d", ErrFormat, len(l.Deny)+len(l.Allow), MaxEntries)
	}
	var b bytes.Buffer
	b.WriteString(formatHeader)
	b.WriteByte('\n')
	for _, e := range l.Deny {
		if err := validateEntry(e); err != nil {
			return nil, err
		}
		b.WriteString("D\t")
		b.WriteString(e.Category.String())
		b.WriteByte('\t')
		b.WriteString(e.Mode.String())
		b.WriteByte('\t')
		b.WriteString(e.Word)
		b.WriteByte('\n')
	}
	for _, word := range l.Allow {
		if err := ValidateWord(word); err != nil {
			return nil, err
		}
		b.WriteString("A\t")
		b.WriteString(word)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// Parse 解析 Marshal 写出的文本。任何一行不认识就整体失败:宁可继续用旧词库,也不加载半份。
func Parse(text []byte) (*Lexicon, error) {
	if !utf8.Valid(text) {
		return nil, fmt.Errorf("%w: not valid UTF-8", ErrFormat)
	}
	lines := strings.Split(string(text), "\n")
	if strings.TrimRight(lines[0], "\r") != formatHeader {
		return nil, fmt.Errorf("%w: first line must be %q", ErrFormat, formatHeader)
	}
	lex := &Lexicon{}
	for i, raw := range lines[1:] {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		lineNo := i + 2
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "D":
			if len(fields) != 4 {
				return nil, fmt.Errorf("%w: line %d: a deny entry needs 4 tab-separated fields", ErrFormat, lineNo)
			}
			category, ok := ParseCategory(fields[1])
			if !ok {
				return nil, fmt.Errorf("%w: line %d: unknown category %q", ErrFormat, lineNo, fields[1])
			}
			mode, ok := ParseMode(fields[2])
			if !ok {
				return nil, fmt.Errorf("%w: line %d: unknown mode %q", ErrFormat, lineNo, fields[2])
			}
			entry := Entry{Word: fields[3], Category: category, Mode: mode}
			if err := validateEntry(entry); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			lex.Deny = append(lex.Deny, entry)
		case "A":
			if len(fields) != 2 {
				return nil, fmt.Errorf("%w: line %d: an allow entry needs 2 tab-separated fields", ErrFormat, lineNo)
			}
			if err := ValidateWord(fields[1]); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			lex.Allow = append(lex.Allow, fields[1])
		default:
			return nil, fmt.Errorf("%w: line %d: unknown record type %q", ErrFormat, lineNo, fields[0])
		}
		if len(lex.Deny)+len(lex.Allow) > MaxEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrFormat, MaxEntries)
		}
	}
	return lex, nil
}
