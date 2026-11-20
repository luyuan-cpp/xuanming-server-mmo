package com.game.gateway.dto;

import com.fasterxml.jackson.annotation.JsonInclude;

import java.util.List;

/**
 * {@code POST /api/account/delete} 与 {@code /api/account/delete/cancel} 的应答(docs/design/account-deletion.md §5.2,
 * 客户端可见契约,两版同批)。
 *
 * <p>{@code code} 与 /api/login 同一套业务码({@link LoginResponse} 的 CODE_*,HTTP 状态恒为 200):
 * <ul>
 *   <li>0 —— login 受理:申请后 state = 1 并带冷静期满的时刻;撤销后 state = 0;</li>
 *   <li>401 —— login 带内拒绝(tip 不是故障码),或请求在网关边界就不合法(message 是稳定的英文原因串,tip = 0);</li>
 *   <li>429 —— 前置限流(与 /api/login 同一个限流器,排队判定也回 429);</li>
 *   <li>500 —— login 不可达 / 内部错误,或 login 回了 Tip.xlsx 标为故障的码。</li>
 * </ul>
 *
 * <p>客户端按 {@code tip} 分支(0 = 没有),不解析 {@code message}。{@code state} / {@code execute_after} 只在 login 有带内
 * 结论时出现(拒绝时也带上 login 回的值,例如「已在执行」时的 state = 2);{@code blockers} 只在挂起清单非空时出现。
 * 时刻一律 Unix 秒。
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class AccountDeletionResponse {

    private int code;
    private String message;
    private int tip;
    private Integer state;
    private Long executeAfter;
    private List<Blocker> blockers;

    /** login 有带内结论(受理或带 tip 的拒绝)。挂起清单为空时不出现在 JSON 里。 */
    public static AccountDeletionResponse answered(int code, String message, int tip,
                                                   int state, long executeAfter, List<Blocker> blockers) {
        AccountDeletionResponse r = new AccountDeletionResponse();
        r.code = code;
        r.message = message;
        r.tip = tip;
        r.state = state;
        r.executeAfter = executeAfter;
        r.blockers = blockers == null || blockers.isEmpty() ? null : blockers;
        return r;
    }

    /** 网关侧的失败(参数不合法、限流、login 不可达):没有 login 的结论,tip = 0。 */
    public static AccountDeletionResponse error(int code, String message) {
        AccountDeletionResponse r = new AccountDeletionResponse();
        r.code = code;
        r.message = message;
        return r;
    }

    /** 预检没过时挂起的一名角色。reason 是低基数枚举串(guild_leader、role_listed 等),客户端认不出的按通用文案显示。 */
    public static class Blocker {
        private long playerId;
        private String reason;

        public long getPlayerId() { return playerId; }
        public void setPlayerId(long playerId) { this.playerId = playerId; }
        public String getReason() { return reason; }
        public void setReason(String reason) { this.reason = reason; }
    }

    public int getCode() { return code; }
    public void setCode(int code) { this.code = code; }
    public String getMessage() { return message; }
    public void setMessage(String message) { this.message = message; }
    public int getTip() { return tip; }
    public void setTip(int tip) { this.tip = tip; }
    public Integer getState() { return state; }
    public void setState(Integer state) { this.state = state; }
    public Long getExecuteAfter() { return executeAfter; }
    public void setExecuteAfter(Long executeAfter) { this.executeAfter = executeAfter; }
    public List<Blocker> getBlockers() { return blockers; }
    public void setBlockers(List<Blocker> blockers) { this.blockers = blockers; }
}
