package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"login/internal/logic/pkg/nppa"
	"shared/antiaddiction"
)

// 国家新闻出版署「网络游戏防沉迷实名认证系统」的 mock(docs/design/real-name-anti-addiction.md §2;
// docs/ops/wechat-qq-sandbox-runbook.md §J)。
//
// 三个接口的路径与真系统相同(nppa.DefaultCheckURL / DefaultQueryURL / DefaultLoginoutURL)。请求按规范验签、解密,
// 用的是 login 的 nppa 包里同一份实现(nppa.ReadRequest):login 发出的报文在签名或加密布局上有错,这里就回系统错误。
//
// 实名结果只由提交的姓名和身份证号决定,同一个人每次得到同一个 pi:
//   - 身份证号过不了格式与校验码(antiaddiction.ValidIDNumber):errcode 2001;
//   - 姓名含「不匹配」:认证失败(status 2);
//   - 姓名含「认证中」:认证中(status 1),之后用同一个 ai 查询,得到成功(status 0)与 pi;
//   - 其余:认证成功(status 0)与 pi。
//
// pi 的前 6 位是身份证号里的出生日期,编码与真系统相同。所以提交一个未成年人的身份证号,就能一路走到时段拦截、
// 到点断开和充值限额。
//
// 上下线上报像真系统那样逐条核对:发生时刻与现在相差 180 秒以上的拒收,其余收下。
// GET /mock/nppa/behaviors 按到达顺序列出收下的记录,用来核对上线、下线成对。
// mock 的日志不打姓名、身份证号和 pi。

// sandboxNppaSecretKey 是技术规范 V1.9 里公开的样例密钥。login 这一侧把同一个值放进
// AntiAddiction.Nppa.SecretKeyEnv 指的环境变量;两边不一致时每个请求都验签失败。
const sandboxNppaSecretKey = "2836e95fcd10e04b0069bb1ee659955b"

// 规范里的错误码。系统类(1001–1012)和上报逐条的错误码在这里只是示例数值:login 对前者一律按系统故障处理,
// 对后者只看是不是 0,都不要拿来写断言。2001、2003、3001 是 login 认的(nppa.authResult、Client.Loginout)。
const (
	sandboxNppaSystemError     = 1011 // 验签失败、报文解不开
	sandboxNppaParamError      = 1012 // 参数不全
	sandboxNppaIDIllegal       = 2001 // 身份证号格式不合法
	sandboxNppaNoRecord        = 2003 // 查询的 ai 没有认证记录
	sandboxNppaPartialFailure  = 3001 // 一批上报里有被拒的条目
	sandboxNppaBehaviorIllegal = 3002 // 一条上报被拒
)

// sandboxNppaMaxBehaviorAge 是真系统的时限:发生时刻距上报 180 秒以上的行为不收。
const sandboxNppaMaxBehaviorAge = 180 * time.Second

type nppaStub struct {
	mu        sync.Mutex
	pending   map[string]string // ai → pi:认证中的提交,等查询来取
	behaviors []nppa.Behavior   // 收下的上下线,按到达顺序
}

func newNppaStub() *nppaStub {
	return &nppaStub{pending: map[string]string{}}
}

// read 核一条请求并把明文解进 into(GET 没有报文体,into 传 nil)。不通过时已经回了应答,返回 false。
func (s *nppaStub) read(w http.ResponseWriter, r *http.Request, method string, into any) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	plain, err := nppa.ReadRequest(sandboxNppaSecretKey, r)
	if err != nil {
		log.Printf("sandbox mock: nppa %s refused: %v", r.URL.Path, err)
		writeNppa(w, sandboxNppaSystemError, "SYS REQ PARTNER AUTH ERROR", nil)
		return false
	}
	if into != nil && json.Unmarshal(plain, into) != nil {
		writeNppa(w, sandboxNppaParamError, "SYS REQ PARAM CHECK ERROR", nil)
		return false
	}
	return true
}

func (s *nppaStub) handleCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AI    string `json:"ai"`
		Name  string `json:"name"`
		IDNum string `json:"idNum"`
	}
	if !s.read(w, r, http.MethodPost, &in) {
		return
	}
	if in.AI == "" || in.Name == "" {
		writeNppa(w, sandboxNppaParamError, "SYS REQ PARAM CHECK ERROR", nil)
		return
	}
	id := antiaddiction.NormalizeIDNumber(in.IDNum)
	if !antiaddiction.ValidIDNumber(id, time.Now()) {
		writeNppa(w, sandboxNppaIDIllegal, "BUS AUTH IDNUM ILLEGAL", nil)
		return
	}
	switch {
	case strings.Contains(in.Name, "不匹配"):
		writeNppaAuthResult(w, nppa.AuthFailed, "")
	case strings.Contains(in.Name, "认证中"):
		s.mu.Lock()
		s.pending[in.AI] = sandboxPI(id)
		s.mu.Unlock()
		writeNppaAuthResult(w, nppa.AuthPending, "")
	default:
		writeNppaAuthResult(w, nppa.AuthOK, sandboxPI(id))
	}
}

// handleQuery 回认证中的那次提交的结果。记录不删:login 取到结果却没写进账号目录时,再查一次还在。
func (s *nppaStub) handleQuery(w http.ResponseWriter, r *http.Request) {
	if !s.read(w, r, http.MethodGet, nil) {
		return
	}
	s.mu.Lock()
	pi, ok := s.pending[r.URL.Query().Get("ai")]
	s.mu.Unlock()
	if !ok {
		writeNppa(w, sandboxNppaNoRecord, "BUS AUTH CODE NO AUTH RECORD", nil)
		return
	}
	writeNppaAuthResult(w, nppa.AuthOK, pi)
}

func (s *nppaStub) handleLoginout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Collections []nppa.Behavior `json:"collections"`
	}
	if !s.read(w, r, http.MethodPost, &in) {
		return
	}
	if len(in.Collections) == 0 || len(in.Collections) > nppa.MaxBatch {
		writeNppa(w, sandboxNppaParamError, "SYS REQ PARAM CHECK ERROR", nil)
		return
	}
	type itemResult struct {
		No      int    `json:"no"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	var rejected []itemResult
	now := time.Now()
	s.mu.Lock()
	for _, b := range in.Collections {
		if !acceptableBehavior(b, now) {
			rejected = append(rejected, itemResult{No: b.No, ErrCode: sandboxNppaBehaviorIllegal, ErrMsg: "BUS COLL BEHAVIOR ILLEGAL"})
			continue
		}
		s.behaviors = append(s.behaviors, b)
	}
	s.mu.Unlock()
	if len(rejected) > 0 {
		writeNppa(w, sandboxNppaPartialFailure, "BUS COLL PARTIAL ERROR", map[string]any{"results": rejected})
		return
	}
	writeNppa(w, 0, "OK", nil)
}

// acceptableBehavior 是真系统对一条上下线的要求里 login 可能写错的那几样:会话标识、上下线取值、pi 的格式,
// 以及发生时刻用的是 Unix 秒、离现在不到 180 秒。
func acceptableBehavior(b nppa.Behavior, now time.Time) bool {
	if b.SI == "" || len(b.SI) > 32 || (b.BT != nppa.BehaviorLogin && b.BT != nppa.BehaviorLogout) || !antiaddiction.ValidPI(b.PI) {
		return false
	}
	age := now.Sub(time.Unix(b.OT, 0))
	return age > -sandboxNppaMaxBehaviorAge && age < sandboxNppaMaxBehaviorAge
}

func (s *nppaStub) handleBehaviors(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	behaviors := append([]nppa.Behavior{}, s.behaviors...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSON(w, map[string]any{"behaviors": behaviors})
}

// sandboxPI 从身份证号派生 pi:前 6 位是出生日期(真系统的编码),后 32 位是号码的 SHA-256 前缀。
// 只对过了 ValidIDNumber 的号码调用。
func sandboxPI(id string) string {
	year, _ := strconv.Atoi(id[6:10])
	month, _ := strconv.Atoi(id[10:12])
	day, _ := strconv.Atoi(id[12:14])
	sum := sha256.Sum256([]byte(id))
	return antiaddiction.PIBirthPrefix(year, month, day) + hex.EncodeToString(sum[:16])
}

func writeNppaAuthResult(w http.ResponseWriter, status nppa.AuthStatus, pi string) {
	result := map[string]any{"status": int(status)}
	if pi != "" {
		result["pi"] = pi
	}
	writeNppa(w, 0, "OK", map[string]any{"result": result})
}

func writeNppa(w http.ResponseWriter, errcode int, errmsg string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body := map[string]any{"errcode": errcode, "errmsg": errmsg}
	if data != nil {
		body["data"] = data
	}
	writeJSON(w, body)
}
