package serverbase

import (
	"reflect"
	"sync"
)

// CodeSource 标识业务错误码是从响应体的哪个字段读出来的。
//
// 这个区分不是装饰:两个字段背后是**不同的码表**(见包注释),
// 判定函数不能混用,所以取值必须带着码一起往下传。
type CodeSource uint8

const (
	// SourceNone 表示这条响应根本不带业务错误码
	// (Empty、纯数据响应、或字段名不是这两种)。
	SourceNone CodeSource = iota

	// SourceErrorCode 来自 `uint32 error_code = N;` 生成的 GetErrorCode()。
	// 码表**由各服务自己定义**,与 tip 码表无关。
	SourceErrorCode

	// SourceTipInfo 来自 `TipInfoMessage error_message = N;` 的 id。
	// TipInfoMessage 这个类型本身就把码表钉死成 tip 那一套。
	SourceTipInfo
)

func (s CodeSource) String() string {
	switch s {
	case SourceErrorCode:
		return "error_code"
	case SourceTipInfo:
		return "tip_info"
	default:
		return "none"
	}
}

// BizCode 是从一条响应里提出来的业务错误码及其出处。
type BizCode struct {
	Code   uint32
	Source CodeSource
}

// errorCodeGetter 对应 `uint32 error_code = N;` 生成的 getter。
// 这条是快路径:纯接口断言,零反射。
type errorCodeGetter interface{ GetErrorCode() uint32 }

// tipIDGetter 对应 TipInfoMessage 生成的 GetId()。
//
// 为什么不直接断言 `interface{ GetErrorMessage() *base.TipInfoMessage }`:
// shared 是独立 module,go.mod 里没有(也不该有)对 go/proto 的依赖,
// 拿不到 *base.TipInfoMessage 这个具体类型,而 Go 的接口方法签名必须精确匹配,
// 无法用通配写法把返回值抽象掉。所以只能反射调一次 GetErrorMessage(),
// 再对它的返回值做 GetId() 断言 —— GetId 的签名不含任何外部类型,可以断言。
type tipIDGetter interface{ GetId() uint32 }

// tipMethodIndex 缓存"某个响应类型有没有可用的 GetErrorMessage 方法",
// 值是 reflect 方法下标,-1 表示没有。
//
// 没有缓存的话每次 RPC 都要按名字查一遍方法表;而绝大多数响应类型
// 压根没有这个方法(data_service / scene_manager 全走 error_code 快路径),
// 缓存让它们从第二次调用起彻底不碰反射。
var tipMethodIndex sync.Map // reflect.Type -> int

// ExtractBizCode 从 handler 返回的 resp 里提取业务错误码。
//
// 取值优先级:
//  1. error_code 非 0 → 直接用它(快路径,零反射);
//  2. 否则看 error_message.id,非 0 → 用它;
//  3. 两个都是 0 → Code=0,Source 记成实际存在的那个字段;
//  4. 两个字段都没有 → SourceNone。
//
// 之所以是"取第一个非 0"而不是"只看 error_code":match 的
// JoinQueueResponse 两个字段都有,服务端可能只填了 tip 那个,
// 只看 error_code 会把失败读成成功。
//
// 本函数对 resp 的类型不做任何假设,反射调用途中的任何 panic 都兜住,
// 当作"没有业务码"处理 —— 观测代码绝不能因为读别人的对象而打死一次 RPC。
func ExtractBizCode(resp any) (bc BizCode) {
	defer func() {
		if r := recover(); r != nil {
			bc = BizCode{}
		}
	}()

	if resp == nil {
		return BizCode{}
	}

	rv := reflect.ValueOf(resp)
	if !rv.IsValid() {
		return BizCode{}
	}
	// typed nil:生成的 getter 自己会做 nil 判断,但测试替身/手写类型不一定,
	// 这里直接短路,别赌。
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return BizCode{}
	}

	found := BizCode{}
	if g, ok := resp.(errorCodeGetter); ok {
		code := g.GetErrorCode()
		if code != 0 {
			return BizCode{Code: code, Source: SourceErrorCode}
		}
		found = BizCode{Code: 0, Source: SourceErrorCode}
	}

	if code, ok := extractTipID(rv); ok {
		if code != 0 || found.Source == SourceNone {
			return BizCode{Code: code, Source: SourceTipInfo}
		}
	}
	return found
}

// extractTipID 反射调用 rv 的 GetErrorMessage(),再从返回值上读 tip id。
// 第二个返回值表示"这个类型确实带 TipInfoMessage 形状的 error_message"。
func extractTipID(rv reflect.Value) (uint32, bool) {
	rt := rv.Type()

	idx := -1
	if cached, ok := tipMethodIndex.Load(rt); ok {
		idx = cached.(int)
	} else {
		// reflect.Type.Method 的 Type 把接收者算进 NumIn,所以是 1 不是 0。
		if m, ok := rt.MethodByName("GetErrorMessage"); ok &&
			m.Type.NumIn() == 1 && m.Type.NumOut() == 1 {
			idx = m.Index
		}
		tipMethodIndex.Store(rt, idx)
	}
	if idx < 0 {
		return 0, false
	}

	out := rv.Method(idx).Call(nil)[0]
	switch out.Kind() {
	case reflect.Pointer, reflect.Interface:
		if out.IsNil() {
			// 有 error_message 字段但没填 —— 等价于码 0(成功)。
			return 0, true
		}
	}

	tip, ok := out.Interface().(tipIDGetter)
	if !ok {
		// scene_manager 的 error_message 是 string,不是 TipInfoMessage,
		// 走到这里说明"同名不同物",当作没有 tip 码。
		return 0, false
	}
	return tip.GetId(), true
}
