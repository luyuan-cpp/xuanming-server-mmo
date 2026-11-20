package com.game.gateway.service;

import com.game.gateway.dto.AccountDeletionRequest;
import com.game.gateway.dto.AccountDeletionResponse;
import com.game.gateway.dto.CancelAccountDeletionRequest;
import com.game.gateway.dto.LoginResponse;
import com.game.gateway.grpc.LoginRpcClient;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Service;

import java.util.function.Supplier;

/**
 * {@code POST /api/account/delete}、{@code /api/account/delete/cancel} → {@code LoginPreGate.RequestAccountDeletion} /
 * {@code CancelAccountDeletion} 的 gRPC 适配(docs/design/account-deletion.md §4.2、§4.4、§5.2)。
 *
 * <p>业务码映射与 {@link LoginService} 完全相同(带内 tip 按 Tip.xlsx 的 fault 列分 500 / 401,gRPC status 只有身份类回 401),
 * 另把 tip 码原样放进 {@code tip}、把 login 回的 state / execute_after / 挂起清单带给客户端。参数校验与限流在
 * {@link com.game.gateway.controller.AccountDeletionController}。
 */
@Service
public class AccountDeletionService {

    private static final Logger log = LoggerFactory.getLogger(AccountDeletionService.class);

    private final LoginRpcClient rpc;

    public AccountDeletionService(LoginRpcClient rpc) {
        this.rpc = rpc;
    }

    public AccountDeletionResponse requestDeletion(AccountDeletionRequest req) {
        var rpcReq = new LoginRpcClient.AccountDeletionRequestProto(
                req.getAccount(),
                req.getPassword(),
                req.getAuthType(),
                req.getAuthToken(),
                req.getAuthorizationCode()
        );
        return call("request_account_deletion", () -> rpc.requestAccountDeletion(rpcReq, (int) req.getZoneId()));
    }

    public AccountDeletionResponse cancelDeletion(CancelAccountDeletionRequest req) {
        var rpcReq = new LoginRpcClient.CancelAccountDeletionRequestProto(req.getAccessToken());
        return call("cancel_account_deletion", () -> rpc.cancelAccountDeletion(rpcReq, (int) req.getZoneId()));
    }

    /** 两个接口共用的应答映射;op 只进日志。日志里没有凭据、令牌与授权码。 */
    private static AccountDeletionResponse call(String op, Supplier<LoginRpcClient.AccountDeletionResponseProto> rpcCall) {
        try {
            LoginRpcClient.AccountDeletionResponseProto rsp = rpcCall.get();
            int code = LoginResponse.CODE_OK;
            String message = null;
            if (rsp.hasError()) {
                int upstream = rsp.errorCode == null ? -1 : rsp.errorCode;
                message = rsp.errorMessage != null ? rsp.errorMessage : ("upstream_err=" + upstream);
                code = LoginService.httpCodeForUpstreamTip(rsp.errorCode);
                if (code == LoginResponse.CODE_INTERNAL) {
                    log.warn("{}.rpc in-band fault tip={}", op, upstream);
                }
            }
            int tip = rsp.errorCode == null ? 0 : rsp.errorCode;
            return AccountDeletionResponse.answered(code, message, tip, rsp.state, rsp.executeAfter, rsp.blockers);
        } catch (StatusRuntimeException e) {
            Status.Code c = e.getStatus().getCode();
            log.warn("{}.rpc failed: {} - {}", op, c, e.getStatus().getDescription());
            return AccountDeletionResponse.error(LoginService.httpCodeForRpcStatus(c), LoginService.messageForRpcStatus(c));
        } catch (Exception e) {
            log.error("{}.rpc unexpected error", op, e);
            return AccountDeletionResponse.error(LoginResponse.CODE_INTERNAL, "internal_error");
        }
    }
}
