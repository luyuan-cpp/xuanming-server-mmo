package com.game.gateway.config;

import jakarta.annotation.PostConstruct;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Locale;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.core.env.Environment;
import org.springframework.stereotype.Component;

/**
 * 启动期校验 {@code gate.token-secret}:CDN 下载链接的 HMAC 签名密钥,与 gate 连接令牌是同一把(部署注入 {@code MMORPG_GATE_TOKEN_SECRET})。
 *
 * <p>为什么单独校验:{@link GateProperties} 是 {@code @ConfigurationProperties} 绑定,Spring Boot 对解析不了的占位<b>保留原文</b>,
 * 不像 {@code @Value} 那样拒启。K8s 上 ConfigMap 只写 {@code "${MMORPG_GATE_TOKEN_SECRET}"},变量缺失时这里拿到的就是那串字面量;
 * jar 内的缺省值又是公开的占位串。两种情况都等于拿公开常量签 URL。生产档({@code mmorpg.run-mode} 不是 dev / test)遇到即拒启;
 * dev / test 只告警,本机开发不必配真密钥。判定口径与 login 的 {@code go/login/internal/config/secrets.go}
 * ({@code MinSecretLen}、{@code IsPlaceholderSecret})一致,改一边要同步另一边。见 docs/design/workload-secrets-via-k8s-secret.md §4.3。
 */
@Component
public class GateTokenSecretGuard {
    private static final Logger log = LoggerFactory.getLogger(GateTokenSecretGuard.class);

    /** 与 go/login/internal/config/secrets.go 的 MinSecretLen 相同。 */
    static final int MIN_SECRET_BYTES = 32;

    /** 与 go/login/internal/config/secrets.go 的 placeholderPrefixes 相同(去首尾空白、小写后按前缀比较)。 */
    private static final List<String> PLACEHOLDER_PREFIXES = List.of(
            "change-me", "changeme", "change_me",
            "please-change", "replace-me",
            "placeholder", "example", "sample",
            "your-secret", "yoursecret",
            "test-secret", "dev-secret", "secret",
            "123456", "password",
            "local-dev");

    private final GateProperties gate;
    private final String runMode;

    public GateTokenSecretGuard(GateProperties gate, Environment environment) {
        this.gate = gate;
        // 与 AuthorityDatabaseConfiguration 读同一个键、同一个缺省值。
        this.runMode = environment.getProperty("mmorpg.run-mode", "prod");
    }

    /** 不合格时:生产档 throw(拒启),dev / test 只告警。报错与日志只说问题,不带密钥值。 */
    @PostConstruct
    void validate() {
        String problem = problemOf(gate.getTokenSecret());
        if (problem == null) {
            return;
        }
        if (isRelaxedRunMode(runMode)) {
            log.warn("gate.token-secret {} (mmorpg.run-mode={}); allowed only for local dev / test, production refuses to start", problem, runMode);
            return;
        }
        throw new IllegalStateException("gate.token-secret " + problem + " (mmorpg.run-mode=" + runMode + "); refusing to start");
    }

    /** 密钥的问题描述(不含值);合格返回 null。 */
    static String problemOf(String secret) {
        if (secret == null || secret.trim().isEmpty()) {
            return "is empty";
        }
        if (secret.contains("${")) {
            return "is an unresolved placeholder (is the environment variable injected?)";
        }
        String normalized = secret.trim().toLowerCase(Locale.ROOT);
        for (String prefix : PLACEHOLDER_PREFIXES) {
            if (normalized.startsWith(prefix)) {
                return "is a public placeholder";
            }
        }
        if (secret.getBytes(StandardCharsets.UTF_8).length < MIN_SECRET_BYTES) {
            return "is shorter than " + MIN_SECRET_BYTES + " bytes";
        }
        return null;
    }

    /** 只有 dev / test 档放行不合格的密钥(只告警);其余档位,包括空串,一律按生产处理。 */
    static boolean isRelaxedRunMode(String runMode) {
        String mode = runMode == null ? "" : runMode.trim().toLowerCase(Locale.ROOT);
        return mode.equals("dev") || mode.equals("test");
    }
}
