package com.game.gateway.config;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;

import org.junit.jupiter.api.Test;
import org.springframework.mock.env.MockEnvironment;

/**
 * 钉住 gate.token-secret 的启动期校验(2026-11-11,docs/design/workload-secrets-via-k8s-secret.md §4.3):
 * {@code @ConfigurationProperties} 宽松绑定下,变量缺失时拿到的是字面量 "${...}",jar 缺省值又是公开占位串;生产档必须拒启,dev / test 只告警。
 */
class GateTokenSecretGuardTest {
    private static final String STRONG = "gate-token-0123456789abcdef0123456789ab";

    @Test
    void acceptsAStrongSecret() {
        assertNull(GateTokenSecretGuard.problemOf(STRONG));
    }

    @Test
    void rejectsEmptyUnresolvedPlaceholderPublicPlaceholderAndShortSecrets() {
        assertNotNull(GateTokenSecretGuard.problemOf(null));
        assertNotNull(GateTokenSecretGuard.problemOf("   "));
        assertNotNull(GateTokenSecretGuard.problemOf("${MMORPG_GATE_TOKEN_SECRET}"));
        assertNotNull(GateTokenSecretGuard.problemOf("change-me-in-production-use-a-strong-random-key"));
        assertNotNull(GateTokenSecretGuard.problemOf("Change-Me-0123456789abcdef0123456789"));
        assertNotNull(GateTokenSecretGuard.problemOf("short-but-not-a-placeholder"));
    }

    @Test
    void productionRefusesToStartWithoutEchoingTheSecret() {
        IllegalStateException failure = assertThrows(IllegalStateException.class,
                () -> guard("change-me-in-production-use-a-strong-random-key", "prod").validate());
        assertFalse(failure.getMessage().contains("change-me"), "the error must not echo the secret");
        // 空 run-mode 也按生产处理(fail-closed)。
        assertThrows(IllegalStateException.class, () -> guard("${MMORPG_GATE_TOKEN_SECRET}", "").validate());
    }

    @Test
    void devAndTestOnlyWarn() {
        assertDoesNotThrow(() -> guard("change-me-in-production-use-a-strong-random-key", "dev").validate());
        assertDoesNotThrow(() -> guard("test-secret", "TEST").validate());
    }

    @Test
    void productionAcceptsAStrongSecret() {
        assertDoesNotThrow(() -> guard(STRONG, "prod").validate());
    }

    private static GateTokenSecretGuard guard(String secret, String runMode) {
        GateProperties properties = new GateProperties();
        properties.setTokenSecret(secret);
        MockEnvironment environment = new MockEnvironment();
        environment.setProperty("mmorpg.run-mode", runMode);
        return new GateTokenSecretGuard(properties, environment);
    }
}
