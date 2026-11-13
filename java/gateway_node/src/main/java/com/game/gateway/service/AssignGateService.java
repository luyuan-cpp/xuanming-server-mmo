package com.game.gateway.service;

import com.game.gateway.dto.AssignGateRequest;
import com.game.gateway.dto.AssignGateResponse;
import com.game.gateway.dto.ManualZoneStatus;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.gateway.repository.ZoneConfigRepository;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.dao.QueryTimeoutException;
import org.springframework.stereotype.Service;

import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

/**
 * Translates {@code POST /api/assign-gate} payloads into the go-zero
 * {@code LoginPreGate.AssignGate} gRPC and back to HTTP DTO.
 *
 * <p><b>Why the local sign-token implementation went away.</b> Before the
 * 2026-05 login-queue work, this service contained a faithful copy of the
 * Go-side "select least-loaded gate, build GateTokenPayload, HMAC-sign"
 * code. Two implementations of the same algorithm in two languages is a
 * drift hazard — and the queue feature requires a single authoritative
 * source for capacity / re-entry / reconnect-bypass decisions, all of
 * which live in go-zero login. The Java side is now a transport shim:
 *
 * <ul>
 *   <li>Token-bucket / wave / IP rate-limiting still runs on the controller
 *       (first-line filter against retry storms — keeps the gRPC fanout
 *       cheap).</li>
 *   <li>Anything past the rate limiter goes straight to go-zero and the
 *       response is mapped 1:1 onto {@link AssignGateResponse}.</li>
 * </ul>
 *
 * <p>Status mapping (go-zero status → HTTP code):
 * <ul>
 *   <li>0 ADMITTED → code=0, gateIp/port/token* populated</li>
 *   <li>1 QUEUEING → code=100 (queueSource=login), queueToken/queueRank/queueTotal populated</li>
 *   <li>2 ERROR    → code=500-ish via {@code error} string</li>
 *   <li>3 EXPIRED  → code=410 (intentionally "Gone") so client UI knows to
 *                    restart from /assign-gate rather than retry /queue-status</li>
 *   <li>4 UNAUTHENTICATED → code=401: access_token 无效,或没带令牌而 login 已关匿名分配;客户端重新登录
 *                    (或先用 refresh token 换新的 access token)。不能落进 default:那里按 ADMITTED 处理,会给客户端一个空地址。</li>
 * </ul>
 */
@Service
public class AssignGateService {

    private static final Logger log = LoggerFactory.getLogger(AssignGateService.class);

    /** Mirrors loginqueue.Status enum in go/login/.../loginqueue/queue.go. */
    private static final int STATUS_ADMITTED = 0;
    private static final int STATUS_QUEUEING = 1;
    private static final int STATUS_ERROR    = 2;
    private static final int STATUS_EXPIRED  = 3;
    private static final int STATUS_UNAUTHENTICATED = 4;

    /** HTTP-shape codes used by the existing {@link AssignGateResponse}. */
    private static final int HTTP_OK             = 0;
    private static final int HTTP_QUEUE          = 100;
    private static final int HTTP_QUEUE_EXPIRED  = 410;
    private static final int HTTP_AUTH_REJECTED  = 401;
    private static final int HTTP_INTERNAL       = 500;

    /**
     * 排队客户端默认约每 2 秒轮询一次。若每次都直查 zone_config,数据库 QPS
     * 会随排队人数线性增长。手工开关区要求近实时生效,因此这里只做 1 秒短缓存。
     */
    private static final long ADMISSION_CACHE_TTL_NANOS = TimeUnit.SECONDS.toNanos(1);
    /** 防止外部请求用随机 zoneId 撑大负缓存。 */
    private static final int ADMISSION_CACHE_MAX_ENTRIES = 4096;

    /**
     * (2026-10-16)加入同一 zone 在途准入查询的等待方最多等多久(毫秒)。此前用无上限 {@code join()}:查库方挂在
     * 已取到的连接上(半开 TCP、zone_config 上的 MDL 排队)时,同 zone 的 assign-gate / queue-status 请求全部无限期
     * 挂住,Tomcat 线程随挂住时长无界增长,而探针在 8082 上照样是绿的。与 Hikari connection-timeout、
     * {@link ServerListService#ZONE_ROWS_LOAD_WAIT_MILLIS} 同口径写死 3000,不从配置读(Hikari 把 0 当无限等);
     * 改 connection-timeout 时三处同步改。到点按查库失败处理:fail-closed 回 500 zone_admission_unavailable。
     */
    static final long ADMISSION_LOAD_WAIT_MILLIS = 3000L;

    private final LoginRpcClient rpc;
    private final ZoneConfigRepository zoneConfigRepo;
    /** 等待方加入在途准入查询后的时间预算(毫秒),见 {@link #ADMISSION_LOAD_WAIT_MILLIS}。 */
    private final long admissionLoadWaitMillis;
    private final ConcurrentHashMap<Long, AdmissionCacheEntry> admissionCache = new ConcurrentHashMap<>();
    /** 同一 zone 的并发 miss 共用一个 DB 查询；失败 future 会立即移除，不做错误缓存。 */
    private final ConcurrentHashMap<Long, CompletableFuture<AdmissionState>> admissionLoads =
            new ConcurrentHashMap<>();

    @Autowired
    public AssignGateService(LoginRpcClient rpc, ZoneConfigRepository zoneConfigRepo) {
        this(rpc, zoneConfigRepo, ADMISSION_LOAD_WAIT_MILLIS);
    }

    // 包级注入点(同 ServerListService):超时用例用短预算,并发用例用远大于屏障上限的预算。
    AssignGateService(LoginRpcClient rpc, ZoneConfigRepository zoneConfigRepo, long admissionLoadWaitMillis) {
        this.rpc = rpc;
        this.zoneConfigRepo = zoneConfigRepo;
        this.admissionLoadWaitMillis = admissionLoadWaitMillis;
    }

    public AssignGateResponse assignGate(AssignGateRequest req) {
        AssignGateResponse gatecheck = checkZoneAdmission(req.getZoneId());
        if (gatecheck != null) {
            return gatecheck;
        }
        var rpcReq = new LoginRpcClient.AssignGateRequestProto(
                req.getZoneId(),
                req.getQueueToken(),
                req.getAccessToken(),
                req.getDeviceId()
        );
        try {
            var rsp = rpc.assignGate(rpcReq, req.getZoneId());
            return mapAssignGate(rsp);
        } catch (StatusRuntimeException e) {
            Status.Code c = e.getStatus().getCode();
            log.warn("assigngate.rpc failed: {} — {}", c, e.getStatus().getDescription());
            AssignGateResponse out = new AssignGateResponse();
            out.setCode(HTTP_INTERNAL);
            out.setError(c == Status.Code.UNAVAILABLE
                    || c == Status.Code.DEADLINE_EXCEEDED ? "login_unavailable" : c.name());
            return out;
        } catch (Exception e) {
            log.error("assigngate.rpc unexpected error", e);
            AssignGateResponse out = new AssignGateResponse();
            out.setCode(HTTP_INTERNAL);
            out.setError("internal_error");
            return out;
        }
    }

    /**
     * Polls the queue for status. Used by {@code POST /api/queue-status}.
     *
     * <p>{@code zoneId} is the routing hint (see {@link com.game.gateway.dto.QueueStatusRequest}):
     * the poll must reach the same login.rpc instance that issued the
     * queue_token, otherwise the lookup misses the queue entry and falsely
     * returns EXPIRED.
     */
    public AssignGateResponse queryQueueStatus(String queueToken, int zoneId) {
        if (queueToken == null || queueToken.isBlank()) {
            AssignGateResponse out = new AssignGateResponse();
            out.setCode(HTTP_QUEUE_EXPIRED);
            out.setError("missing_queue_token");
            return out;
        }
        // 排队中运维把区切成维护/关闭时,轮询方也要被拦下来(fail-closed),
        // 而不是继续推进队列拿到 gate token。
        AssignGateResponse gatecheck = checkZoneAdmission(zoneId);
        if (gatecheck != null) {
            return gatecheck;
        }
        var rpcReq = new LoginRpcClient.QueryQueueStatusRequestProto(queueToken);
        try {
            var rsp = rpc.queryQueueStatus(rpcReq, zoneId);
            return mapQueueStatus(rsp, queueToken);
        } catch (StatusRuntimeException e) {
            Status.Code c = e.getStatus().getCode();
            log.warn("queryqueuestatus.rpc failed: {} — {}", c, e.getStatus().getDescription());
            AssignGateResponse out = new AssignGateResponse();
            out.setCode(HTTP_INTERNAL);
            out.setError(c == Status.Code.UNAVAILABLE
                    || c == Status.Code.DEADLINE_EXCEEDED ? "login_unavailable" : c.name());
            return out;
        } catch (Exception e) {
            log.error("queryqueuestatus.rpc unexpected error", e);
            AssignGateResponse out = new AssignGateResponse();
            out.setCode(HTTP_INTERNAL);
            out.setError("internal_error");
            return out;
        }
    }

    /**
     * 区服准入闭环:此前 zone_config 的 MAINTENANCE/CLOSED/PREVIEW 只在
     * /api/server-list 展示层生效,直接打 /api/assign-gate 仍可进维护区。
     * 这里按运维手工状态 fail-closed:
     * <ul>
     *   <li>zoneId<=0(自动选区)→ 放行,由 login 决定目标 zone</li>
     *   <li>zone_config 无此 zone → 404 zone_not_found</li>
     *   <li>MAINTENANCE → 503 zone_maintenance;CLOSED → 503 zone_closed;
     *       PREVIEW → 503 zone_not_open(白名单表 account 维度对不上现有
     *       string 账号体系,暂不消费,开放前先修表结构)</li>
     * </ul>
     * 自动探测态(etcd DOWN)不在这里二次裁决:节点真不可用时 go-zero login
     * 的实时选 gate 本身就是 fail-closed 的,双重判定只会放大 etcd 抖动。
     *
     * @return null = 放行;非 null = 拦截应答
     */
    private AssignGateResponse checkZoneAdmission(long zoneId) {
        if (zoneId <= 0) {
            return null;
        }
        final AdmissionState state;
        try {
            state = getZoneAdmission(zoneId);
        } catch (RuntimeException e) {
            // 本次改动前 assign-gate 链路完全不接触 MySQL。DB 抖动不能变成
            // 未捕获异常穿透 controller(会打破「恒 HTTP 200 + body.code」的
            // 客户端契约);也不能 fail-open 放行——维护窗口(回档/合服)期间
            // 放人进去是数据一致性事故,fail-closed 返回可重试的 500。
            log.error("zone admission lookup failed for zone={}", zoneId, e);
            AssignGateResponse out = new AssignGateResponse();
            out.setCode(HTTP_INTERNAL);
            out.setError("zone_admission_unavailable");
            return out;
        }
        return switch (state) {
            case NOT_FOUND  -> AssignGateResponse.zoneNotFound();
            case MAINTENANCE -> AssignGateResponse.zoneUnavailable("zone_maintenance");
            case CLOSED      -> AssignGateResponse.zoneUnavailable("zone_closed");
            case PREVIEW     -> AssignGateResponse.zoneUnavailable("zone_not_open");
            case OPEN        -> null;
        };
    }

    /**
     * 返回 zone 的短期准入快照。同一 zone 的并发 cache miss 通过 future 合并;
     * DB 异常会传播给全部等待方,并在 finally 中移除 future,让下一次请求重新
     * 查询而不是复用一次失败。不同 zone 仍可并行查询。
     */
    private AdmissionState getZoneAdmission(long zoneId) {
        AdmissionCacheEntry cached = admissionCache.get(zoneId);
        long now = System.nanoTime();
        if (cached != null && cached.expiresAtNanos() > now) {
            return cached.state();
        }

        CompletableFuture<AdmissionState> mine = new CompletableFuture<>();
        CompletableFuture<AdmissionState> active = admissionLoads.putIfAbsent(zoneId, mine);
        if (active != null) {
            return awaitAdmission(active);
        }

        try {
            // 在 fast-path miss 与 putIfAbsent 之间,上一轮加载可能刚完成并移除
            // future；成为 loader 后再检查一次,避免紧邻的重复 DB 查询。
            cached = admissionCache.get(zoneId);
            now = System.nanoTime();
            if (cached != null && cached.expiresAtNanos() > now) {
                mine.complete(cached.state());
                return cached.state();
            }

            ZoneConfig zone = zoneConfigRepo.findById(zoneId).orElse(null);
            AdmissionState loaded = zone == null
                    ? AdmissionState.NOT_FOUND
                    : AdmissionState.from(ManualZoneStatus.fromCode(zone.getManualStatus()));
            cacheAdmission(zoneId, loaded);
            mine.complete(loaded);
            return loaded;
        } catch (RuntimeException | Error e) {
            mine.completeExceptionally(e);
            throw e;
        } finally {
            admissionLoads.remove(zoneId, mine);
        }
    }

    /**
     * 等待别人发起的那次准入查询,最多等 {@link #admissionLoadWaitMillis}。查库失败时抛查库方的原异常;到点未完成抛
     * {@link QueryTimeoutException}(查库方不受影响,继续查,结果照常写缓存)。两者都由 checkZoneAdmission 的
     * catch 转成 500 zone_admission_unavailable。限时等待可被中断,中断时恢复中断标记后失败。
     */
    private AdmissionState awaitAdmission(CompletableFuture<AdmissionState> active) {
        try {
            return active.get(admissionLoadWaitMillis, TimeUnit.MILLISECONDS);
        } catch (ExecutionException e) {
            Throwable cause = e.getCause();
            if (cause instanceof RuntimeException runtime) {
                throw runtime;
            }
            if (cause instanceof Error error) {
                throw error;
            }
            throw new IllegalStateException("zone admission load failed", cause);
        } catch (TimeoutException e) {
            throw new QueryTimeoutException(
                    "zone admission load still in flight after " + admissionLoadWaitMillis + "ms", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted while waiting for zone admission load", e);
        }
    }

    /**
     * 仅供同包测试(只读):zoneId 当前在途准入查询上挂着的等待方数;没有在途查询时为 0。语义与限制同
     * {@code ServerListService.inflightWaiterCountForTest}:等待方超时离开会清栈,只在等待方不会超时的用例里当屏障用。
     */
    int inflightAdmissionWaiterCountForTest(long zoneId) {
        CompletableFuture<AdmissionState> active = admissionLoads.get(zoneId);
        return active == null ? 0 : active.getNumberOfDependents();
    }

    private void cacheAdmission(long zoneId, AdmissionState state) {
        long now = System.nanoTime();
        if (!admissionCache.containsKey(zoneId)
                && admissionCache.size() >= ADMISSION_CACHE_MAX_ENTRIES) {
            admissionCache.entrySet().removeIf(entry -> entry.getValue().expiresAtNanos() <= now);
        }
        // 极端随机 zoneId 洪泛下,缓存满时允许本次查询完成但不继续增长缓存。
        if (admissionCache.containsKey(zoneId)
                || admissionCache.size() < ADMISSION_CACHE_MAX_ENTRIES) {
            admissionCache.put(zoneId,
                    new AdmissionCacheEntry(state, now + ADMISSION_CACHE_TTL_NANOS));
        }
    }

    private enum AdmissionState {
        OPEN,
        MAINTENANCE,
        CLOSED,
        PREVIEW,
        NOT_FOUND;

        static AdmissionState from(ManualZoneStatus status) {
            return switch (status) {
                case OPEN -> OPEN;
                case MAINTENANCE -> MAINTENANCE;
                case CLOSED -> CLOSED;
                case PREVIEW -> PREVIEW;
            };
        }
    }

    private record AdmissionCacheEntry(AdmissionState state, long expiresAtNanos) {}

    // ── Mapping helpers ───────────────────────────────────────────

    private static AssignGateResponse mapAssignGate(LoginRpcClient.AssignGateResponseProto rsp) {
        switch (rsp.status) {
            case STATUS_QUEUEING:
                return AssignGateResponse.loginQueueing(
                        rsp.queueToken,
                        rsp.queueRank,
                        rsp.queueTotal,
                        rsp.retryAfterMs > 0 ? rsp.retryAfterMs : 2000
                );
            case STATUS_ERROR: {
                AssignGateResponse out = new AssignGateResponse();
                out.setCode(HTTP_INTERNAL);
                out.setError(rsp.error != null ? rsp.error : "unknown_error");
                return out;
            }
            case STATUS_EXPIRED: {
                AssignGateResponse out = new AssignGateResponse();
                out.setCode(HTTP_QUEUE_EXPIRED);
                out.setError(rsp.error != null ? rsp.error : "queue_token_expired");
                return out;
            }
            case STATUS_UNAUTHENTICATED: {
                AssignGateResponse out = new AssignGateResponse();
                out.setCode(HTTP_AUTH_REJECTED);
                out.setError(rsp.error != null && !rsp.error.isEmpty() ? rsp.error : "unauthenticated");
                return out;
            }
            case STATUS_ADMITTED:
            default:
                return admittedResponse(rsp.ip, rsp.port, rsp.tokenPayload, rsp.tokenSignature, rsp.tokenDeadline, rsp.error);
        }
    }

    private static AssignGateResponse mapQueueStatus(LoginRpcClient.QueryQueueStatusResponseProto rsp, String queueToken) {
        switch (rsp.status) {
            case STATUS_QUEUEING: {
                // Echo the same queueToken back so the client can keep polling
                // without parsing the response — a small UX nicety that also
                // matches what AssignGate did on the initial enqueue.
                AssignGateResponse out = AssignGateResponse.loginQueueing(
                        queueToken,
                        rsp.queueRank,
                        rsp.queueTotal,
                        rsp.retryAfterMs > 0 ? rsp.retryAfterMs : 2000
                );
                return out;
            }
            case STATUS_EXPIRED: {
                AssignGateResponse out = new AssignGateResponse();
                out.setCode(HTTP_QUEUE_EXPIRED);
                out.setError(rsp.error != null ? rsp.error : "queue_token_expired");
                return out;
            }
            case STATUS_ERROR: {
                AssignGateResponse out = new AssignGateResponse();
                out.setCode(HTTP_INTERNAL);
                out.setError(rsp.error != null ? rsp.error : "unknown_error");
                return out;
            }
            case STATUS_ADMITTED:
            default:
                return admittedResponse(rsp.ip, rsp.port, rsp.tokenPayload, rsp.tokenSignature, rsp.tokenDeadline, rsp.error);
        }
    }

    private static AssignGateResponse admittedResponse(String ip, int port, byte[] payload, byte[] sig, long deadline, String err) {
        AssignGateResponse out = new AssignGateResponse();
        if (ip == null || ip.isBlank()) {
            // Defensive: ADMITTED without an endpoint shouldn't happen, but if
            // a future protocol bug leaves these blank we surface it as error
            // rather than handing the client an empty connect target.
            out.setCode(HTTP_INTERNAL);
            out.setError(err != null && !err.isBlank() ? err : "admitted_without_endpoint");
            return out;
        }
        out.setCode(HTTP_OK);
        out.setGateIp(ip);
        out.setGatePort(port);
        out.setTokenPayload(payload);
        out.setTokenSignature(sig);
        out.setTokenDeadline(deadline);
        return out;
    }
}
