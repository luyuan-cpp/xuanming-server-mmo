package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;
import java.nio.charset.StandardCharsets;
import org.junit.jupiter.api.Test;

class StorageAndAuthTest {
    @Test void schemaGateRequiresVersionUniqueKeysAndBinaryIdentifiers() throws Exception {
        try (var f = new CommerceFixture()) { SchemaVerifier.verify(f.jdbc, f.source); f.jdbc.update("UPDATE commerce_schema_version SET storage_contract='foreign-mall'"); assertThrows(IllegalStateException.class, () -> SchemaVerifier.verify(f.jdbc, f.source)); }
        try (var f = new CommerceFixture()) { f.jdbc.execute("DROP TABLE commerce_payment_receipt");f.jdbc.execute("CREATE TABLE commerce_payment_receipt(channel VARBINARY(32),transaction_id VARBINARY(128),order_sn VARCHAR(36),amount_minor BIGINT,currency VARCHAR(16))"); assertThrows(IllegalStateException.class, () -> SchemaVerifier.verify(f.jdbc, f.source)); }
        try (var f = new CommerceFixture()) { f.jdbc.execute("ALTER TABLE oms_order RENAME TO oms_order_binary_fixture");f.jdbc.execute("CREATE VIEW oms_order AS SELECT id,order_sn,buyer_id,CAST(request_id AS CHAR(128)) AS request_id,sku_id,quantity,pay_amount,currency,status,create_time_ms,payment_deadline_ms,payment_time_ms FROM oms_order_binary_fixture"); assertThrows(IllegalStateException.class, () -> SchemaVerifier.verify(f.jdbc, f.source)); }
    }
    // 2026-11-05 之前布局的库(付款回执订单列 NOT NULL、没有异常付款表、退款两表按订单号)记不下已付订单的第二笔钱,必须拒启;
    // 2026-11-05 ~ 11-10 布局的库只有 commerce_duplicate_payment(没有 reason 列),记不下金额或币种不符的付款,同样拒启。
    @Test void schemaGateRefusesLayoutsThatCannotRecordAbnormalPayments() throws Exception {
        try (var f = new CommerceFixture()) { f.jdbc.execute("ALTER TABLE commerce_payment_receipt MODIFY COLUMN order_sn VARCHAR(36) NOT NULL"); assertThrows(IllegalStateException.class, () -> SchemaVerifier.verify(f.jdbc, f.source)); }
        try (var f = new CommerceFixture()) {
            f.jdbc.execute("DROP TABLE commerce_abnormal_payment");
            f.jdbc.execute("CREATE TABLE commerce_duplicate_payment (channel VARBINARY(32) NOT NULL,transaction_id VARBINARY(128) NOT NULL,order_sn VARCHAR(36) NOT NULL,created_ms BIGINT NOT NULL,PRIMARY KEY (channel,transaction_id))");
            assertThrows(Exception.class, () -> SchemaVerifier.verify(f.jdbc, f.source));
        }
        try (var f = new CommerceFixture()) {
            f.jdbc.execute("DROP TABLE commerce_abnormal_payment");
            f.jdbc.execute("CREATE TABLE commerce_abnormal_payment (channel VARBINARY(32) NOT NULL,transaction_id VARBINARY(128) NOT NULL,order_sn VARCHAR(36) NOT NULL,created_ms BIGINT NOT NULL,PRIMARY KEY (channel,transaction_id))");
            assertThrows(Exception.class, () -> SchemaVerifier.verify(f.jdbc, f.source), "缺 reason 列");
        }
        // 在旧库上重跑新 schema.sql 能补出新表,但旧库退款待办的 CHECK 仍只认三种来源;闸门测不出 CHECK,靠合同戳拒启。
        try (var f = new CommerceFixture()) {
            f.jdbc.update("UPDATE commerce_schema_version SET storage_contract='mall-game-official-sku-v1-minor-units'");
            assertThrows(IllegalStateException.class, () -> SchemaVerifier.verify(f.jdbc, f.source), "11-10 之前的合同戳");
        }
        try (var f = new CommerceFixture()) {
            f.jdbc.execute("DROP TABLE commerce_refund_receipt");
            f.jdbc.execute("CREATE TABLE commerce_refund_receipt (order_sn VARCHAR(36) PRIMARY KEY,channel VARCHAR(32) NOT NULL,transaction_id VARCHAR(128) NOT NULL,amount_minor BIGINT NOT NULL,currency VARCHAR(16) NOT NULL,UNIQUE(channel,transaction_id))");
            assertThrows(Exception.class, () -> SchemaVerifier.verify(f.jdbc, f.source));
        }
    }
    @Test void nonceIsPersistentAcrossInstancesAndIsCaseSensitive() {
        try (var f = new CommerceFixture()) {
            NonceStore first = new NonceStore(f.jdbc, f.props, f.manager); NonceStore second = new NonceStore(f.jdbc, f.props, f.manager); long now = System.currentTimeMillis();
            first.claim("gateway", "NonceCase00000001", now); assertEquals(409, assertThrows(CommerceException.class, () -> second.claim("gateway", "NonceCase00000001", now)).status());
            second.claim("gateway", "noncecase00000001", now); assertEquals(2, f.count("commerce_request_nonce"));
            assertEquals(401, assertThrows(CommerceException.class, () -> second.claim("gateway", "expired0000000001", now - 500_000)).status());
            assertEquals(401, assertThrows(CommerceException.class, () -> second.claim("gateway", "future00000000001", now + 500_000)).status());
            assertEquals(2, f.count("commerce_request_nonce"));
        }
    }
    @Test void signatureBindsEveryInputAndPlayerIdsPreserveUint64() {
        byte[] body = "{\"quantity\":1}".getBytes(StandardCharsets.UTF_8); String secret = "authentication-test-only-key-0000000001";
        String original = HmacAuthentication.sign(secret, "gateway", "18446744073709551615", "1760000000000", "nonce-00000000001", "POST", "/api/orders", body);
        assertNotEquals(original, HmacAuthentication.sign(secret, "payment", "18446744073709551615", "1760000000000", "nonce-00000000001", "POST", "/api/orders", body));
        assertNotEquals(original, HmacAuthentication.sign(secret, "gateway", "1", "1760000000000", "nonce-00000000001", "POST", "/api/orders", body));
        assertNotEquals(original, HmacAuthentication.sign(secret, "gateway", "18446744073709551615", "1760000000000", "nonce-00000000001", "POST", "/internal/payments", body));
        assertNotEquals(original, HmacAuthentication.sign(secret, "gateway", "18446744073709551615", "1760000000000", "nonce-00000000001", "POST", "/api/orders", new byte[0]));
        assertDoesNotThrow(() -> HmacAuthentication.requirePlayer("18446744073709551615"));
        for (String invalid : new String[]{"0", "-1", "01", "18446744073709551616"}) { assertThrows(CommerceException.class, () -> HmacAuthentication.requirePlayer(invalid)); }
    }
    @Test void missingAndEqualSecretsFailClosed() {
        CommerceProperties properties = new CommerceProperties(); assertThrows(IllegalStateException.class, properties::validate);
        properties.setGatewaySecret("test-only-same-signing-key-00000000001"); properties.setPaymentSecret(properties.getGatewaySecret()); assertThrows(IllegalStateException.class, properties::validate);
    }
    @Test void productionCannotDisableTidb() {
        CommerceProperties properties = new CommerceProperties();
        properties.setGatewaySecret("production-gate-test-only-key-000000001"); properties.setPaymentSecret("production-pay-test-only-key-000000002");
        properties.setRequireTidb(false); assertThrows(IllegalStateException.class, properties::validate);
        properties.setRunMode("dev"); assertDoesNotThrow(properties::validate);
        properties.setRunMode("production"); assertThrows(IllegalStateException.class, properties::validate);
    }
}
