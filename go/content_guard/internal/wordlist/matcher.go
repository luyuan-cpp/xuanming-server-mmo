package wordlist

import (
	"sort"

	"content_guard/internal/textnorm"
)

// builtinDeny 是不依赖词库、永远生效的几个词:防冒充官方身份。
// 和 go/shared/playername 的内置词同一批,那边只管角色名,这里让帮会名、宠物名也有同样的底线。
// 词库里再写同样的词会被算作重复而忽略。
var builtinDeny = []Entry{
	{Word: "管理员", Category: CategoryImpersonation, Mode: ModeSub},
	{Word: "客服", Category: CategoryImpersonation, Mode: ModeSub},
	{Word: "官方", Category: CategoryImpersonation, Mode: ModeSub},
	{Word: "系统", Category: CategoryImpersonation, Mode: ModeSub},
	{Word: "运营", Category: CategoryImpersonation, Mode: ModeSub},
	{Word: "gm", Category: CategoryImpersonation, Mode: ModePrefix},
}

// Report 是一次编译的统计,给日志、指标和发布工具用。
type Report struct {
	DenyWords  int // 编进字典树的拒绝词(含内置词,已去重)
	AllowWords int // 编进字典树的白名单词
	Dropped    int // 折叠后为空(全是符号)或字段越界而被丢弃的词条
	Duplicates int // 折叠后与更早的词条相同(同一种匹配方式)而被忽略的词条
}

// Matcher 是编译好的词库,只读,可以被任意多个 goroutine 同时使用。
type Matcher struct {
	deny  trie
	allow trie
}

// Hit 是一处命中。Start、End 是原文的字符下标(按 rune 计),闭区间。
type Hit struct {
	Start, End int32
	Category   Category
}

// Compile 把词库编成 Matcher。lex 为 nil 时只有内置词。
// 词条先经 textnorm.FoldWord 折叠;同一个折叠结果、同一种匹配方式只保留最先出现的那一条。
func Compile(lex *Lexicon) (*Matcher, Report) {
	var report Report
	denyRoot, allowRoot := &buildNode{}, &buildNode{}
	insert := func(root *buildNode, word string, mode Mode, category Category) bool {
		if mode >= modeEnd || category == CategoryUnspecified || category >= categoryEnd {
			report.Dropped++
			return false
		}
		folded := textnorm.FoldWord(word)
		if len(folded) == 0 {
			report.Dropped++
			return false
		}
		node := root
		for _, r := range folded {
			node = node.child(r)
		}
		if node.term[mode] != CategoryUnspecified {
			report.Duplicates++
			return false
		}
		node.term[mode] = category
		return true
	}
	for _, e := range builtinDeny {
		if insert(denyRoot, e.Word, e.Mode, e.Category) {
			report.DenyWords++
		}
	}
	if lex != nil {
		for _, e := range lex.Deny {
			if insert(denyRoot, e.Word, e.Mode, e.Category) {
				report.DenyWords++
			}
		}
		for _, word := range lex.Allow {
			// 白名单只需要「是不是词尾」这一个信息,借子串那一格存一个非零值。
			if insert(allowRoot, word, ModeSub, CategoryOther) {
				report.AllowWords++
			}
		}
	}
	return &Matcher{deny: flatten(denyRoot), allow: flatten(allowRoot)}, report
}

// Find 返回文字里全部的命中,按起点从前到后。
//
// 每个起点上所有成立的词都报告,不只报最长的:同一个起点上短词和长词的类别可能不同(一个打码、一个拒绝),
// 只报最长的会漏掉该拒绝的那一个。命中段完全落在某个白名单词里面的不报告。
func (m *Matcher) Find(elems []textnorm.Elem) []Hit {
	firstReal := -1
	for i := range elems {
		if !elems[i].Skip {
			firstReal = i
			break
		}
	}
	if firstReal < 0 {
		return nil
	}
	allowed := m.allowSpans(elems)
	var hits []Hit
	for start := firstReal; start < len(elems); start++ {
		if elems[start].Skip {
			continue
		}
		m.deny.walk(elems, start, func(end int, term [modeEnd]Category, contiguous bool) {
			if covered(allowed, start, end) {
				return
			}
			var reported [modeEnd]Category
			count := 0
			report := func(category Category) {
				for i := 0; i < count; i++ {
					if reported[i] == category {
						return
					}
				}
				reported[count] = category
				count++
				hits = append(hits, Hit{Start: elems[start].Orig, End: elems[end].Orig, Category: category})
			}
			if c := term[ModeSub]; c != CategoryUnspecified {
				report(c)
			}
			if c := term[ModePrefix]; c != CategoryUnspecified && start == firstReal {
				report(c)
			}
			if c := term[ModeToken]; c != CategoryUnspecified && contiguous && tokenBounded(elems, start, end) {
				report(c)
			}
		})
	}
	// 叠字绕过防御(移植自 xuanming-server/pkg/namecheck/variants.go v2 变体):
	// 仅在输入存在连续相同非跳过字符时,对折叠连续重复后的序列再扫一遍(如 "管理理员" / "笨笨蛋" / "aavv"),
	// 词库字典树本身不折叠,因此不影响本身含叠字的词条。
	if collapsed, hasCollapse := collapseRepeats(elems); hasCollapse {
		allowOrig := spansToOrig(elems, allowed)
		seen := make(map[Hit]struct{}, len(hits))
		for _, h := range hits {
			seen[h] = struct{}{}
		}
		added := false
		for start := range collapsed {
			m.deny.walkCollapsed(collapsed, start, func(end int, term [modeEnd]Category, contiguous bool) {
				origStart, origEnd := collapsed[start].startOrig, collapsed[end].endOrig
				if OrigCovered(allowOrig, origStart, origEnd) {
					return
				}
				add := func(category Category) {
					h := Hit{Start: origStart, End: origEnd, Category: category}
					if _, dup := seen[h]; dup {
						return
					}
					seen[h] = struct{}{}
					hits = append(hits, h)
					added = true
				}
				if c := term[ModeSub]; c != CategoryUnspecified {
					add(c)
				}
				if c := term[ModePrefix]; c != CategoryUnspecified && start == 0 {
					add(c)
				}
				if c := term[ModeToken]; c != CategoryUnspecified && contiguous && collapsedTokenBounded(collapsed, start, end) {
					add(c)
				}
			})
		}
		if added {
			sort.SliceStable(hits, func(i, j int) bool {
				if hits[i].Start != hits[j].Start {
					return hits[i].Start < hits[j].Start
				}
				return hits[i].End < hits[j].End
			})
		}
	}
	return hits
}

type collapsedElem struct {
	r          rune
	startOrig  int32
	endOrig    int32
	skipBefore bool
	skipInside bool
}

func collapseRepeats(elems []textnorm.Elem) ([]collapsedElem, bool) {
	out := make([]collapsedElem, 0, len(elems))
	hasCollapse := false
	pendingSkip := false
	for _, e := range elems {
		if e.Skip {
			pendingSkip = true
			continue
		}
		if n := len(out); n > 0 && out[n-1].r == e.R {
			out[n-1].endOrig = e.Orig
			if pendingSkip {
				out[n-1].skipInside = true
			}
			hasCollapse = true
			pendingSkip = false
			continue
		}
		out = append(out, collapsedElem{
			r:          e.R,
			startOrig:  e.Orig,
			endOrig:    e.Orig,
			skipBefore: pendingSkip,
		})
		pendingSkip = false
	}
	return out, hasCollapse
}

func spansToOrig(elems []textnorm.Elem, spans []span) []OrigSpan {
	if len(spans) == 0 {
		return nil
	}
	out := make([]OrigSpan, 0, len(spans))
	for _, s := range spans {
		out = append(out, OrigSpan{Start: elems[s.start].Orig, End: elems[s.end].Orig})
	}
	return out
}

func collapsedTokenBounded(elems []collapsedElem, start, end int) bool {
	if start > 0 && !elems[start].skipBefore && textnorm.IsASCIIAlnum(elems[start-1].r) {
		return false
	}
	if end+1 < len(elems) && !elems[end+1].skipBefore && textnorm.IsASCIIAlnum(elems[end+1].r) {
		return false
	}
	return true
}

func (t *trie) walkCollapsed(elems []collapsedElem, start int, visit func(end int, term [modeEnd]Category, contiguous bool)) {
	node := int32(0)
	contiguous := true
	for i := start; i < len(elems); i++ {
		if (i > start && elems[i].skipBefore) || elems[i].skipInside {
			contiguous = false
		}
		next, ok := t.child(node, elems[i].r)
		if !ok {
			return
		}
		node = next
		if t.term[node] != ([modeEnd]Category{}) {
			visit(i, t.term[node], contiguous)
		}
	}
}

// tokenBounded 判断 [start, end] 前后是否都不紧挨英文字母或数字(「整词」的边界)。
func tokenBounded(elems []textnorm.Elem, start, end int) bool {
	if start > 0 && textnorm.IsASCIIAlnum(elems[start-1].R) {
		return false
	}
	if end+1 < len(elems) && textnorm.IsASCIIAlnum(elems[end+1].R) {
		return false
	}
	return true
}

// OrigSpan 是原文的字符闭区间(按 rune 计),供外围启发式检测对齐白名单词豁免。
type OrigSpan struct{ Start, End int32 }

// AllowOrigSpans 返回折叠序列命中的全部白名单词在原文里的 rune 区间。
func (m *Matcher) AllowOrigSpans(elems []textnorm.Elem) []OrigSpan {
	if len(elems) == 0 {
		return nil
	}
	spans := m.allowSpans(elems)
	if len(spans) == 0 {
		return nil
	}
	out := make([]OrigSpan, 0, len(spans))
	for _, s := range spans {
		out = append(out, OrigSpan{Start: elems[s.start].Orig, End: elems[s.end].Orig})
	}
	return out
}

// OrigCovered 报告原文字符区间 [startOrig, endOrig] 是否完全落在任意白名单段内。
func OrigCovered(spans []OrigSpan, startOrig, endOrig int32) bool {
	for _, s := range spans {
		if s.Start <= startOrig && endOrig <= s.End {
			return true
		}
	}
	return false
}

// span 是折叠序列里的一段,闭区间。
type span struct{ start, end int }

func (m *Matcher) allowSpans(elems []textnorm.Elem) []span {
	if len(m.allow.labels) == 0 {
		return nil
	}
	var spans []span
	for start := range elems {
		if elems[start].Skip {
			continue
		}
		m.allow.walk(elems, start, func(end int, _ [modeEnd]Category, _ bool) {
			spans = append(spans, span{start: start, end: end})
		})
	}
	return spans
}

func covered(spans []span, start, end int) bool {
	for _, s := range spans {
		if s.start <= start && end <= s.end {
			return true
		}
	}
	return false
}

// buildNode 是编译期的临时字典树节点;编完压平成 trie 后丢弃。
type buildNode struct {
	children map[rune]*buildNode
	term     [modeEnd]Category
}

func (n *buildNode) child(r rune) *buildNode {
	if next, ok := n.children[r]; ok {
		return next
	}
	if n.children == nil {
		n.children = make(map[rune]*buildNode, 1)
	}
	next := &buildNode{}
	n.children[r] = next
	return next
}

// trie 是压平的字典树。节点 i 的出边是 labels[first[i]:first[i+1]](按字符升序),对应的子节点号在 next 的同一下标。
// term[i] 记节点 i 是哪些匹配方式的词尾、各是什么类别;全零表示不是词尾。根节点是 0。
// 用平铺数组而不是每个节点一个 map:几万个词的词库,map 的开销是数组的几十倍。
type trie struct {
	first  []int32
	labels []rune
	next   []int32
	term   [][modeEnd]Category
}

// flatten 按广度优先给节点编号并压平。队列下标就是节点号。
func flatten(root *buildNode) trie {
	var t trie
	queue := []*buildNode{root}
	for i := 0; i < len(queue); i++ {
		node := queue[i]
		t.first = append(t.first, int32(len(t.labels)))
		t.term = append(t.term, node.term)
		keys := make([]rune, 0, len(node.children))
		for r := range node.children {
			keys = append(keys, r)
		}
		sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
		for _, r := range keys {
			t.labels = append(t.labels, r)
			t.next = append(t.next, int32(len(queue)))
			queue = append(queue, node.children[r])
		}
	}
	t.first = append(t.first, int32(len(t.labels)))
	return t
}

// child 在节点 node 的出边里二分查找字符 r。
func (t *trie) child(node int32, r rune) (int32, bool) {
	lo, hi := t.first[node], t.first[node+1]
	for lo < hi {
		mid := lo + (hi-lo)/2
		switch label := t.labels[mid]; {
		case label == r:
			return t.next[mid], true
		case label < r:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return 0, false
}

// walk 从 elems[start] 起沿字典树往下走,每到一个词尾回调一次。
// 可跳过的字符不消耗字典树的边;contiguous 表示从 start 到这个词尾之间没有夹着被跳过的字符。
func (t *trie) walk(elems []textnorm.Elem, start int, visit func(end int, term [modeEnd]Category, contiguous bool)) {
	node := int32(0)
	contiguous := true
	skipped := false
	for i := start; i < len(elems); i++ {
		if elems[i].Skip {
			skipped = true
			continue
		}
		next, ok := t.child(node, elems[i].R)
		if !ok {
			return
		}
		if skipped {
			contiguous = false
		}
		node = next
		if t.term[node] != ([modeEnd]Category{}) {
			visit(i, t.term[node], contiguous)
		}
	}
}
