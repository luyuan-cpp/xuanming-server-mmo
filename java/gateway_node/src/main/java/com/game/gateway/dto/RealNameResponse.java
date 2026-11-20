package com.game.gateway.dto;

import com.fasterxml.jackson.annotation.JsonInclude;

/**
 * POST /api/real-name 的应答(docs/design/real-name-anti-addiction.md §5.2)。HTTP 状态恒为 200,结论看 code:
 * <ul>
 *   <li>0:已实名,或本渠道不需要游戏方实名;anti_addiction 是最新状态;</li>
 *   <li>401:access token 不能用,回到登录;</li>
 *   <li>422:实名没通过,tip 说明原因(信息有误、认证中、次数用完、已绑定别的实名),客户端按 tip 出文案;</li>
 *   <li>429:网关限流;500:服务端故障,稍后重试。</li>
 * </ul>
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class RealNameResponse {
    public static final int CODE_OK = 0;
    public static final int CODE_REJECTED = 422;

    private int code;
    private String message;
    /** login 回的 tip 码(导表码);成功时省略。 */
    private Integer tip;
    private AntiAddictionStatus antiAddiction;

    public static RealNameResponse of(int code, String message, Integer tip, AntiAddictionStatus antiAddiction) {
        RealNameResponse r = new RealNameResponse();
        r.code = code;
        r.message = message;
        r.tip = tip;
        r.antiAddiction = antiAddiction;
        return r;
    }

    public int getCode() { return code; }
    public void setCode(int code) { this.code = code; }
    public String getMessage() { return message; }
    public void setMessage(String message) { this.message = message; }
    public Integer getTip() { return tip; }
    public void setTip(Integer tip) { this.tip = tip; }
    public AntiAddictionStatus getAntiAddiction() { return antiAddiction; }
    public void setAntiAddiction(AntiAddictionStatus antiAddiction) { this.antiAddiction = antiAddiction; }
}
