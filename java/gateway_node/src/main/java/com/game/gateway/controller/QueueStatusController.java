package com.game.gateway.controller;

import com.game.gateway.dto.AssignGateResponse;
import com.game.gateway.dto.QueueStatusRequest;
import com.game.gateway.service.AssignGateService;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

/**
 * Polling endpoint for the AssignGate login queue. 持有 queue_token 的客户端一律轮询本端点,直到回
 * {@code code=0}(放行,带 gate 票)或 {@code code=410}(token 失效:丢掉 token,不带 token 从
 * /api/assign-gate 重来);回 503 zone_* 或 404 zone_not_found(区服准入拦截)是终态,按错误结束、不重入。
 * 轮询间隔不是简单取 {@code retryAfterMs}:有 2s 下限、[0, 20%] 抖动和连续失败时的指数退避(封顶 30s);
 * 本端点失败(网络错误 / HTTP 非 200 / code=500)时才带 token 重调一次 /api/assign-gate 作恢复。
 * 契约全文见 AssignGateRequest 的 queueToken 字段注释(2026-10-15 定)。
 *
 * <p>Response shape mirrors {@link AssignGateResponse} so clients can reuse
 * the same parsing path as the initial /assign-gate response. The duplication
 * is intentional: a client that received {@code queueSource="login"} from
 * /assign-gate switches to /queue-status without changing its expected shape.
 *
 * <p>This endpoint deliberately bypasses {@code AssignGateRateLimiter}: the
 * caller is already in the queue (already throttled by definition), and the
 * only thing rate-limiting their polls would do is delay rank updates,
 * which is a UX regression with no security upside. The limiter stays in front
 * of /assign-gate where it actually filters new entrants.
 */
@RestController
@RequestMapping("/api")
public class QueueStatusController {

    private final AssignGateService assignGateService;

    public QueueStatusController(AssignGateService assignGateService) {
        this.assignGateService = assignGateService;
    }

    @PostMapping("/queue-status")
    public AssignGateResponse queueStatus(@RequestBody QueueStatusRequest req) {
        return assignGateService.queryQueueStatus(req.getQueueToken(), req.getZoneId());
    }
}
