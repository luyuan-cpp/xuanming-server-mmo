package serverbase

import "testing"

// ---------------------------------------------------------------------------
// 测试替身:复刻本仓四种真实响应形状
//
// 之所以不直接用生成的 pb 类型:shared 是独立 module,go.mod 里没有对
// go/proto 的依赖(也不该加)。这些替身只需要和生成代码的**方法签名**一致,
// 而拦截器认的正是方法签名。
// ---------------------------------------------------------------------------

// fakeTip 复刻 base.TipInfoMessage,拦截器只用到它的 GetId()。
type fakeTip struct{ id uint32 }

func (t *fakeTip) GetId() uint32 {
	if t == nil {
		return 0
	}
	return t.id
}

// respErrorCode 复刻 data_service / scene_manager 的 `uint32 error_code`。
type respErrorCode struct{ code uint32 }

func (r *respErrorCode) GetErrorCode() uint32 {
	if r == nil {
		return 0
	}
	return r.code
}

// respTip 复刻 login / friend / guild 的 `TipInfoMessage error_message`。
type respTip struct{ tip *fakeTip }

func (r *respTip) GetErrorMessage() *fakeTip {
	if r == nil {
		return nil
	}
	return r.tip
}

// respBoth 复刻 match.JoinQueueResponse:error_code 与 error_message 都有。
type respBoth struct {
	code uint32
	tip  *fakeTip
}

func (r *respBoth) GetErrorCode() uint32      { return r.code }
func (r *respBoth) GetErrorMessage() *fakeTip { return r.tip }

// respStringErrMsg 复刻 scene_manager.CreateSceneResponse:
// 同名的 error_message 是 string,不是 TipInfoMessage —— 同名不同物。
type respStringErrMsg struct{ msg string }

func (r *respStringErrMsg) GetErrorMessage() string { return r.msg }

// respPlain 复刻 Empty / 纯数据响应。
type respPlain struct{ payload int }

// respPanicTip 复刻"getter 自己炸了"的极端情况。
type respPanicTip struct{}

func (r *respPanicTip) GetErrorMessage() *fakeTip { panic("getter 炸了") }

// respWrongShape 有同名方法但签名不对(带参数),必须被忽略。
type respWrongShape struct{}

func (r *respWrongShape) GetErrorMessage(int) *fakeTip { return nil }

func TestExtractBizCode(t *testing.T) {
	tests := []struct {
		name string
		resp any
		want BizCode
	}{
		{
			name: "nil 响应",
			resp: nil,
			want: BizCode{},
		},
		{
			name: "typed nil 指针",
			resp: (*respErrorCode)(nil),
			want: BizCode{},
		},
		{
			name: "无任何业务码字段",
			resp: &respPlain{payload: 7},
			want: BizCode{},
		},
		{
			name: "error_code 成功",
			resp: &respErrorCode{code: 0},
			want: BizCode{Code: 0, Source: SourceErrorCode},
		},
		{
			name: "error_code 失败",
			resp: &respErrorCode{code: 8},
			want: BizCode{Code: 8, Source: SourceErrorCode},
		},
		{
			name: "tip 成功(error_message 未填)",
			resp: &respTip{},
			want: BizCode{Code: 0, Source: SourceTipInfo},
		},
		{
			name: "tip 失败",
			resp: &respTip{tip: &fakeTip{id: 106}},
			want: BizCode{Code: 106, Source: SourceTipInfo},
		},
		{
			name: "tip 填了但 id 是 0",
			resp: &respTip{tip: &fakeTip{id: 0}},
			want: BizCode{Code: 0, Source: SourceTipInfo},
		},
		{
			name: "两个字段都有:error_code 非 0 优先",
			resp: &respBoth{code: 3, tip: &fakeTip{id: 106}},
			want: BizCode{Code: 3, Source: SourceErrorCode},
		},
		{
			// 这条是 match 那种响应的真实陷阱:只填了 tip。
			// 若只看 error_code 会把失败读成成功。
			name: "两个字段都有:error_code 为 0 则回落到 tip",
			resp: &respBoth{code: 0, tip: &fakeTip{id: 41}},
			want: BizCode{Code: 41, Source: SourceTipInfo},
		},
		{
			name: "两个字段都有且都成功",
			resp: &respBoth{code: 0, tip: nil},
			want: BizCode{Code: 0, Source: SourceErrorCode},
		},
		{
			name: "error_message 是 string(同名不同物)当作没有业务码",
			resp: &respStringErrMsg{msg: "no available node"},
			want: BizCode{},
		},
		{
			name: "getter panic 被兜住,当作没有业务码",
			resp: &respPanicTip{},
			want: BizCode{},
		},
		{
			name: "同名但签名不对的方法被忽略",
			resp: &respWrongShape{},
			want: BizCode{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractBizCode(tt.resp)
			if got != tt.want {
				t.Fatalf("ExtractBizCode() = %+v, 期望 %+v", got, tt.want)
			}
		})
	}
}

// TestExtractBizCodeCacheStable 确认反射方法下标缓存不会在多次调用后失准
// （第一次走查表、之后走缓存,两条路径必须给出同样的结果）。
func TestExtractBizCodeCacheStable(t *testing.T) {
	resp := &respTip{tip: &fakeTip{id: 55}}
	for i := 0; i < 3; i++ {
		got := ExtractBizCode(resp)
		want := BizCode{Code: 55, Source: SourceTipInfo}
		if got != want {
			t.Fatalf("第 %d 次 ExtractBizCode() = %+v, 期望 %+v", i+1, got, want)
		}
	}

	// 无该方法的类型同样要稳定(缓存里存的是 -1)。
	plain := &respPlain{}
	for i := 0; i < 3; i++ {
		if got := ExtractBizCode(plain); got != (BizCode{}) {
			t.Fatalf("第 %d 次 ExtractBizCode(plain) = %+v, 期望零值", i+1, got)
		}
	}
}

func TestCodeSourceString(t *testing.T) {
	tests := []struct {
		src  CodeSource
		want string
	}{
		{SourceNone, "none"},
		{SourceErrorCode, "error_code"},
		{SourceTipInfo, "tip_info"},
		{CodeSource(99), "none"},
	}
	for _, tt := range tests {
		if got := tt.src.String(); got != tt.want {
			t.Fatalf("CodeSource(%d).String() = %q, 期望 %q", tt.src, got, tt.want)
		}
	}
}
