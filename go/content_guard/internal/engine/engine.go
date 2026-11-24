// Package engine 持有当前生效的词库,并按场景把命中判成放行、打码或拒绝
// (docs/design/rename-and-content-safety.md §6.3)。
//
// 词库整体替换:新词库编译成功后一次原子替换,正在进行的判定继续用它开始时拿到的那一份。
// 本包不连库、不打日志;词库从哪来由 lexstore 负责。
package engine

import (
	"sync/atomic"
	"time"

	"content_guard/internal/contact"
	"content_guard/internal/textnorm"
	"content_guard/internal/wordlist"
)

// SceneClass 是场景的大类,决定命中之后的动作。proto 的 TextScene 到它的映射在 server 包。
type SceneClass uint8

const (
	// SceneName 名字(角色名、帮会名、宠物名、寄售标题):任何类别命中都拒绝,并启用联系方式广告检测。
	SceneName SceneClass = 0
	// SceneNotice 长期展示的自由文本(帮会公告):冒充官方那一类不管,其余命中与引流广告都拒绝。
	SceneNotice SceneClass = 1
	// SceneChat 聊天:冒充官方那一类不管,辱骂打码后放行,其余命中与引流广告拒绝。
	SceneChat SceneClass = 2
	// ScenePrivateLabel 玩家仅自己可见的私有标签(预设方案名等):走词库拒绝规则,不做联系方式广告启发式拦截。
	ScenePrivateLabel SceneClass = 3
	sceneClassEnd     SceneClass = 4
)

type action uint8

const (
	actionIgnore action = iota
	actionMask
	actionReject
)

// actionFor 是全部的动作表。
// 冒充官方的词(官方、客服、系统……)在聊天和公告里是正常用语,只有出现在名字里才构成冒充,所以只对名字类生效。
func actionFor(class SceneClass, category wordlist.Category) action {
	switch class {
	case SceneChat:
		switch category {
		case wordlist.CategoryImpersonation:
			return actionIgnore
		case wordlist.CategoryAbuse:
			return actionMask
		}
		return actionReject
	case SceneNotice:
		if category == wordlist.CategoryImpersonation {
			return actionIgnore
		}
		return actionReject
	}
	// 名字类,以及任何不认识的大类:一律拒绝(fail-closed)。
	return actionReject
}

// Verdict 是判定结局。
type Verdict uint8

const (
	VerdictPass   Verdict = 1
	VerdictMasked Verdict = 2
	VerdictReject Verdict = 3
)

// Result 是一次判定的结果。
type Result struct {
	Verdict Verdict
	// Masked 仅在 VerdictMasked 时有值:原文里命中的段换成 *,一个字符一个 *,其余原样。字节数不会比原文多。
	Masked string
	// Category 是决定结局的类别;放行时为 CategoryUnspecified。
	Category wordlist.Category
	// Version 是判定用的词库版本;0 = 只有内置词。
	Version uint64
}

// Snapshot 是一份生效中的词库。
type Snapshot struct {
	Version  uint64
	Matcher  *wordlist.Matcher
	Report   wordlist.Report
	LoadedAt time.Time
}

// Engine 可以被任意多个 goroutine 同时调用。
type Engine struct {
	current atomic.Pointer[Snapshot]
}

// New 返回只带内置词的 Engine(版本 0)。
func New() *Engine {
	e := &Engine{}
	matcher, report := wordlist.Compile(nil)
	e.current.Store(&Snapshot{Matcher: matcher, Report: report, LoadedAt: time.Now()})
	return e
}

// Swap 编译并换上一份词库,返回编译统计。version 必须非 0(0 留给「只有内置词」)。
// 编译在调用方的 goroutine 上做,期间判定照常用旧词库。
func (e *Engine) Swap(version uint64, lex *wordlist.Lexicon) wordlist.Report {
	matcher, report := wordlist.Compile(lex)
	e.current.Store(&Snapshot{Version: version, Matcher: matcher, Report: report, LoadedAt: time.Now()})
	return report
}

// Current 返回当前生效的词库快照,只读。
func (e *Engine) Current() *Snapshot {
	return e.current.Load()
}

// maxCombiningRun 允许的连续组合记号数上限(移植自 xuanming-server/pkg/namecheck)。
// 正常带音标字符(如重音字母/越南语)最多叠加 2 个组合记号;超过即属 Zalgo 刷屏,会撑爆客户端 UI。
const maxCombiningRun = 2

// Check 判定一段文字。空文字直接放行。
func (e *Engine) Check(class SceneClass, text string) Result {
	snapshot := e.current.Load()
	result := Result{Verdict: VerdictPass, Version: snapshot.Version}
	if text == "" {
		return result
	}
	elems, orig := textnorm.Fold(text)
	if hasExcessiveCombiningMarks(orig, maxCombiningRun) {
		return Result{Verdict: VerdictReject, Category: wordlist.CategoryOther, Version: snapshot.Version}
	}
	hits := snapshot.Matcher.Find(elems)
	masked := false
	for _, hit := range hits {
		switch actionFor(class, hit.Category) {
		case actionReject:
			return Result{Verdict: VerdictReject, Category: hit.Category, Version: snapshot.Version}
		case actionMask:
			if !masked {
				masked = true
				result.Category = hit.Category
			}
			maskSpan(orig, hit.Start, hit.End)
		}
	}
	if class != ScenePrivateLabel {
		allowSpans := snapshot.Matcher.AllowOrigSpans(elems)
		if contact.ContainsAd(text, elems, func(startOrig, endOrig int32) bool {
			return wordlist.OrigCovered(allowSpans, startOrig, endOrig)
		}) {
			return Result{Verdict: VerdictReject, Category: wordlist.CategoryAd, Version: snapshot.Version}
		}
	}
	if masked {
		result.Verdict = VerdictMasked
		result.Masked = string(orig)
	}
	return result
}

// hasExcessiveCombiningMarks 判断原文是否存在超过上限的连续组合记号(Zalgo 文本)。
func hasExcessiveCombiningMarks(orig []rune, maxRun int) bool {
	run := 0
	for _, r := range orig {
		if textnorm.IsMark(r) {
			run++
			if run > maxRun {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// maskSpan 把 orig[start..end] 换成 *,并把紧跟其后的组合记号一并盖掉(不让它落在 * 上)。
// 一个字符换一个 *:* 只占 1 字节,所以打码后的字节数不会比原文多。
func maskSpan(orig []rune, start, end int32) {
	last := int(end)
	for last+1 < len(orig) && textnorm.IsMark(orig[last+1]) {
		last++
	}
	for i := int(start); i <= last && i < len(orig); i++ {
		orig[i] = '*'
	}
}
