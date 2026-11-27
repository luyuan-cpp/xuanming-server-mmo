package com.game.gateway.controller;

import com.game.gateway.grpc.LoginRpcClient;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RestController;

/**
 * 官方通行证(Casdoor)回调入口 {@code POST /internal/passport/events}(docs/design/official-passport-registration.md §3.7)。
 *
 * <p>Casdoor 的回调只能带固定请求头,没有签名:请求头 {@value #SECRET_HEADER} 必须等于 {@code passport.webhook-secret}
 * (按 SHA-256 摘要常量时间比较)。没配密钥时整个入口回 404(关着)。请求体原样转给 login 的
 * {@code LoginPreGate.HandlePassportEvent}:login 只拿它找人(哪名通行证用户变了),按通行证此刻的状态吊销并踢线,
 * 所以密钥即便泄露,伪造的回调也只会让 login 多查几次通行证,改不了任何人的状态。
 *
 * <p>状态码给 Casdoor 的重投用:200 处理完;400 请求体不对,不必重投;401 密钥不对;503 login 暂时处理不了,按重投设置再来。
 * 这个入口只给集群内的 Casdoor 用,公网入口不转发 {@code /internal/**}(deploy/k8s/passport/README.md)。
 */
@RestController
public class PassportEventController {

    private static final Logger log = LoggerFactory.getLogger(PassportEventController.class);
    static final String SECRET_HEADER = "X-Passport-Webhook-Secret";
    /** 与 login 的上限相同(passportsession.maxEventBytes):一条记录加一名用户,远小于它。 */
    static final int MAX_BODY_BYTES = 64 * 1024;
    private static final int MIN_SECRET_BYTES = 32;

    private final LoginRpcClient login;
    /** null = 没配密钥,入口关着。 */
    private final byte[] secretDigest;

    public PassportEventController(LoginRpcClient login, @Value("${passport.webhook-secret:}") String secret) {
        boolean enabled = secret != null && !secret.isBlank();
        if (enabled && secret.getBytes(StandardCharsets.UTF_8).length < MIN_SECRET_BYTES) {
            throw new IllegalStateException("passport.webhook-secret must be at least " + MIN_SECRET_BYTES + " bytes");
        }
        this.login = login;
        this.secretDigest = enabled ? sha256(secret) : null;
    }

    @PostMapping("/internal/passport/events")
    public ResponseEntity<Void> handle(@RequestHeader(value = SECRET_HEADER, required = false) String secret,
                                       @RequestBody(required = false) byte[] body) {
        if (secretDigest == null) {
            return ResponseEntity.notFound().build();
        }
        if (secret == null || !MessageDigest.isEqual(secretDigest, sha256(secret))) {
            log.warn("passport webhook rejected: missing or wrong {}", SECRET_HEADER);
            return ResponseEntity.status(HttpStatus.UNAUTHORIZED).build();
        }
        if (body == null || body.length == 0 || body.length > MAX_BODY_BYTES) {
            log.warn("passport webhook rejected: body size {}", body == null ? 0 : body.length);
            return ResponseEntity.badRequest().build();
        }
        try {
            LoginRpcClient.PassportEventResponseProto rsp = login.handlePassportEvent(body);
            log.info("passport webhook handled: checked_users={}", rsp.checkedUsers);
            return ResponseEntity.ok().build();
        } catch (StatusRuntimeException e) {
            Status.Code code = e.getStatus().getCode();
            if (code == Status.Code.INVALID_ARGUMENT) {
                log.warn("passport webhook rejected by login: not a Casdoor record");
                return ResponseEntity.badRequest().build();
            }
            log.warn("passport webhook not handled, the passport will redeliver: login status={}", code);
            return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE).build();
        }
    }

    private static byte[] sha256(String value) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8));
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException(e);
        }
    }
}
