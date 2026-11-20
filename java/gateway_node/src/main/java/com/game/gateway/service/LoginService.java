package com.game.gateway.service;

import com.game.gateway.dto.AntiAddictionStatus;
import com.game.gateway.dto.LoginRequest;
import com.game.gateway.dto.LoginResponse;
import com.game.gateway.dto.RealNameRequest;
import com.game.gateway.dto.RealNameResponse;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.table.tip.TipFaults;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Service;

/**
 * Translates {@code POST /api/login} payloads into the go-zero
 * {@code ClientPlayerLogin.Login} gRPC call and back.
 *
 * <p>The HTTP layer is intentionally narrow: rate-limit decisions and
 * controller plumbing live in {@link com.game.gateway.controller.LoginController};
 * this service is the gRPC adapter.
 */
@Service
public class LoginService {

    private static final Logger log = LoggerFactory.getLogger(LoginService.class);

    private final LoginRpcClient rpc;

    public LoginService(LoginRpcClient rpc) {
        this.rpc = rpc;
    }

    public LoginResponse login(LoginRequest req) {
        var rpcReq = new LoginRpcClient.LoginRequestProto(
                req.getAccount(),
                req.getPassword(),
                req.getAuthType(),
                req.getAuthToken()
        );
        try {
            // Zone-aware route: each go-zero login.rpc instance only watches
            // its own zone's player_locator / scene_manager etc., so
            // round-robin would scatter Login calls across the wrong zones
            // and produce sporadic "no gate available" / session-bind errors.
            // See LoginRpcClient.unaryCall(method, req, zoneId) and the
            // 2026-05-24 stress postmortem §I.
            LoginRpcClient.LoginResponseProto rsp = rpc.login(rpcReq, (int) req.getZoneId());
            if (rsp.hasError()) {
                int upstream = rsp.errorCode == null ? -1 : rsp.errorCode;
                String msg = rsp.errorMessage != null ? rsp.errorMessage : ("upstream_err=" + upstream);
                int code = httpCodeForUpstreamTip(rsp.errorCode);
                if (code == LoginResponse.CODE_INTERNAL) {
                    log.warn("login.rpc in-band fault tip={}", upstream);
                }
                return LoginResponse.error(code, msg);
            }
            LoginResponse out = LoginResponse.ok(
                    rsp.players,
                    rsp.accessToken,
                    rsp.refreshToken,
                    rsp.accessTokenExpire,
                    rsp.refreshTokenExpire
            );
            // 账号注销状态原样带给客户端(docs/design/account-deletion.md §4.3、§4.5)。
            out.setAccountDeletionState(rsp.accountDeletionState);
            out.setAccountDeletionExecuteAfter(rsp.accountDeletionExecuteAfter);
            out.setPreviousAccountDeletedAt(rsp.previousAccountDeletedAt);
            out.setAntiAddiction(AntiAddictionStatus.from(rsp.antiAddiction));
            return out;
        } catch (StatusRuntimeException e) {
            Status.Code c = e.getStatus().getCode();
            log.warn("login.rpc failed: {} — {}", c, e.getStatus().getDescription());
            return LoginResponse.error(httpCodeForRpcStatus(c), messageForRpcStatus(c));
        } catch (Exception e) {
            log.error("login.rpc unexpected error", e);
            return LoginResponse.error(LoginResponse.CODE_INTERNAL, "internal_error");
        }
    }

    /**
     * POST /api/real-name(docs/design/real-name-anti-addiction.md §5.2)。姓名和身份证号只转给 login,不进日志。
     * code:0 成功;401 令牌不能用(login 回 UNAUTHENTICATED);422 实名没通过,tip 给原因;500 故障(tip 的 fault 列)。
     */
    public RealNameResponse submitRealName(RealNameRequest req) {
        var rpcReq = new LoginRpcClient.SubmitRealNameRequestProto(req.getAccessToken(), req.getName(), req.getIdNumber());
        try {
            LoginRpcClient.SubmitRealNameResponseProto rsp = rpc.submitRealName(rpcReq, (int) req.getZoneId());
            AntiAddictionStatus status = AntiAddictionStatus.from(rsp.antiAddiction);
            if (rsp.errorCode == null || rsp.errorCode == 0) {
                return RealNameResponse.of(RealNameResponse.CODE_OK, null, null, status);
            }
            if (TipFaults.isFault(rsp.errorCode)) {
                log.warn("login.rpc SubmitRealName in-band fault tip={}", rsp.errorCode);
                return RealNameResponse.of(LoginResponse.CODE_INTERNAL, "upstream_err=" + rsp.errorCode, rsp.errorCode, status);
            }
            return RealNameResponse.of(RealNameResponse.CODE_REJECTED, "upstream_err=" + rsp.errorCode, rsp.errorCode, status);
        } catch (StatusRuntimeException e) {
            Status.Code c = e.getStatus().getCode();
            log.warn("login.rpc SubmitRealName failed: {}", c);
            return RealNameResponse.of(httpCodeForRpcStatus(c), messageForRpcStatus(c), null, null);
        } catch (IllegalArgumentException e) {
            // zone_id 不合法(选不到 login 实例)
            return RealNameResponse.of(LoginResponse.CODE_INTERNAL, "invalid_zone", null, null);
        } catch (Exception e) {
            log.error("login.rpc SubmitRealName unexpected error: {}", e.getClass().getSimpleName());
            return RealNameResponse.of(LoginResponse.CODE_INTERNAL, "internal_error", null, null);
        }
    }

    /**
     * login.rpc 以 gRPC status 失败时的 HTTP 业务码。login 的业务拒绝一律走 in-band tip（上面的 hasError 分支），
     * 以 status 失败只可能是传输、热关停闸门或服务端内部错误：只有 UNAUTHENTICATED / PERMISSION_DENIED 是
     * 「调用方身份不被接受」，回 401；其余（UNKNOWN、INTERNAL 等）回 500。以前除 UNAVAILABLE / DEADLINE_EXCEEDED
     * 外一律回 401，login 内部故障（例如账号目录读 Redis 失败变成 UNKNOWN）会让玩家被要求重走 OAuth。
     * {@link RefreshTokenService} 用同一套映射。
     */
    static int httpCodeForRpcStatus(Status.Code c) {
        return c == Status.Code.UNAUTHENTICATED || c == Status.Code.PERMISSION_DENIED
                ? LoginResponse.CODE_AUTH_REJECTED
                : LoginResponse.CODE_INTERNAL;
    }

    /**
     * login 回 in-band tip 码时的 HTTP 业务码。分类只认 Tip.xlsx 的 fault 列（导表器生成的 {@link TipFaults}，
     * 与 Go serverbase 的 {@code tip.IsFault} 同一份数据），网关里不另写故障码集合：
     * <ul>
     *   <li>故障码（渠道 / 存储故障、令牌签发失败……）→ 500：服务端出错，客户端稍后重试，凭证不一定有问题；</li>
     *   <li>其余码（凭证无效等业务拒绝）、没有码只有 message、本二进制不认识的新码 → 401：客户端重新认证（原行为）。</li>
     * </ul>
     * message 不受影响，上游 tip 保留用于排障；当前客户端只按 JSON code 分支。{@link RefreshTokenService} 用同一套映射。
     */
    static int httpCodeForUpstreamTip(Integer tipId) {
        return tipId != null && TipFaults.isFault(tipId)
                ? LoginResponse.CODE_INTERNAL
                : LoginResponse.CODE_AUTH_REJECTED;
    }

    /** 可重试的传输类失败统一给 login_unavailable（客户端既有文案），其余带 status 名便于排障。 */
    static String messageForRpcStatus(Status.Code c) {
        return switch (c) {
            case UNAVAILABLE, DEADLINE_EXCEEDED, RESOURCE_EXHAUSTED -> "login_unavailable";
            default -> c.name();
        };
    }
}
