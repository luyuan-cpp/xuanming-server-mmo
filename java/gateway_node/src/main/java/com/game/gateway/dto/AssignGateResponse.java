package com.game.gateway.dto;

import com.fasterxml.jackson.annotation.JsonInclude;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class AssignGateResponse {
    /**
     * Top-level HTTP-shape code (preserves backwards compatibility with the
     * pre-queue rate-limit path):
     *   0    = OK (gateIp/gatePort/token* populated)
     *   100  = QUEUEING from Bucket4j rate-limit OR from go-zero login real queue.
     *          Disambiguate via {@link #queueSource}: "ratelimit" vs "login".
     *          When source=login, {@link #queueToken}/{@link #queueRank}/
     *          {@link #queueTotal} carry the authoritative position; when
     *          source=ratelimit, only retryAfterMs is meaningful.
     *   101  = QUEUE_TIMEOUT
     *   429  = RATE_LIMITED
     *   other = legacy error string in {@link #error}
     */
    private int code;
    private String gateIp;
    private int gatePort;
    private byte[] tokenPayload;
    private byte[] tokenSignature;
    private long tokenDeadline;
    private String error;

    // queue payload (only populated when code=100/101)
    private Long retryAfterMs;
    private Long queuePos;

    // ── Real-queue fields (populated when go-zero login returns status=1) ──
    // Distinguishes "you are in the authoritative login queue" from "Bucket4j
    // says retry later". Clients SHOULD show the queue UI (rank/total) when
    // queueSource="login"; for queueSource="ratelimit" a generic "please wait"
    // is enough since Bucket4j has no real position.
    private String queueSource;  // "login" | "ratelimit" | null
    private String queueToken;   // opaque, pass to /api/queue-status
    private Long queueRank;      // 0-based; 0 = "you are next"
    private Long queueTotal;     // total queue length in this zone

    public static AssignGateResponse queueing(long retryAfterMs, long queuePos) {
        AssignGateResponse r = new AssignGateResponse();
        r.code = 100;
        r.retryAfterMs = retryAfterMs;
        r.queuePos = queuePos;
        r.queueSource = "ratelimit";
        return r;
    }

    /**
     * Builds a code=100 response from a real go-zero login queue admission.
     * Carries the authoritative queue token + rank/total so the client UI
     * can show "前面还有 N 人".
     */
    public static AssignGateResponse loginQueueing(String queueToken,
                                                   long queueRank,
                                                   long queueTotal,
                                                   long retryAfterMs) {
        AssignGateResponse r = new AssignGateResponse();
        r.code = 100;
        r.queueSource = "login";
        r.queueToken = queueToken;
        r.queueRank = queueRank;
        r.queueTotal = queueTotal;
        r.retryAfterMs = retryAfterMs;
        // queuePos kept null — clients should read queueRank for the real value.
        return r;
    }

    public static AssignGateResponse rateLimited(String reason) {
        AssignGateResponse r = new AssignGateResponse();
        r.code = 429;
        r.error = reason;
        return r;
    }

    /** code=404:请求的 zone 在 zone_config 里不存在(选服目录之外的区)。 */
    public static AssignGateResponse zoneNotFound() {
        AssignGateResponse r = new AssignGateResponse();
        r.code = 404;
        r.error = "zone_not_found";
        return r;
    }

    /**
     * code=503:zone 存在但当前不可进(维护/关闭/未开放)。error 是机器可读
     * tag("zone_maintenance"/"zone_closed"/"zone_not_open"),客户端据此
     * 显示本地化文案;运维公告文本走 /api/server-list 的 maintenance_msg。
     */
    public static AssignGateResponse zoneUnavailable(String tag) {
        AssignGateResponse r = new AssignGateResponse();
        r.code = 503;
        r.error = tag;
        return r;
    }

    public int getCode() { return code; }
    public void setCode(int code) { this.code = code; }

    public String getGateIp() { return gateIp; }
    public void setGateIp(String gateIp) { this.gateIp = gateIp; }

    public int getGatePort() { return gatePort; }
    public void setGatePort(int gatePort) { this.gatePort = gatePort; }

    public byte[] getTokenPayload() { return tokenPayload; }
    public void setTokenPayload(byte[] tokenPayload) { this.tokenPayload = tokenPayload; }

    public byte[] getTokenSignature() { return tokenSignature; }
    public void setTokenSignature(byte[] tokenSignature) { this.tokenSignature = tokenSignature; }

    public long getTokenDeadline() { return tokenDeadline; }
    public void setTokenDeadline(long tokenDeadline) { this.tokenDeadline = tokenDeadline; }

    public String getError() { return error; }
    public void setError(String error) { this.error = error; }

    public Long getRetryAfterMs() { return retryAfterMs; }
    public void setRetryAfterMs(Long retryAfterMs) { this.retryAfterMs = retryAfterMs; }

    public Long getQueuePos() { return queuePos; }
    public void setQueuePos(Long queuePos) { this.queuePos = queuePos; }

    public String getQueueSource() { return queueSource; }
    public void setQueueSource(String queueSource) { this.queueSource = queueSource; }

    public String getQueueToken() { return queueToken; }
    public void setQueueToken(String queueToken) { this.queueToken = queueToken; }

    public Long getQueueRank() { return queueRank; }
    public void setQueueRank(Long queueRank) { this.queueRank = queueRank; }

    public Long getQueueTotal() { return queueTotal; }
    public void setQueueTotal(Long queueTotal) { this.queueTotal = queueTotal; }
}
