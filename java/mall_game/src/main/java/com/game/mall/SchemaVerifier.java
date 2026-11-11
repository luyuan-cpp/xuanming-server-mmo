package com.game.mall;

import java.sql.*;
import java.util.*;
import javax.sql.DataSource;
import org.springframework.jdbc.core.JdbcTemplate;

final class SchemaVerifier {
    private static final Map<String, String> COLUMNS = Map.ofEntries(
        Map.entry("commerce_schema_version", "version,storage_contract"),
        Map.entry("commerce_buyer_guard", "buyer_id"),
        Map.entry("pms_sku_stock", "id,product_id,sku_code,price_minor,currency,stock,lock_stock,published,catalog_revision"),
        Map.entry("commerce_admin_command", "actor,request_id,command_type,request_digest,result_json,created_ms"),
        Map.entry("oms_order", "id,order_sn,buyer_id,request_id,sku_id,quantity,pay_amount,currency,status,create_time_ms,payment_deadline_ms,payment_time_ms"),
        Map.entry("oms_order_item", "order_sn,product_id,product_sku_id,product_sku_code,product_quantity,product_price_minor"),
        Map.entry("commerce_official_owner", "order_sn,buyer_account,buyer_owner_revision"),
        Map.entry("commerce_payment_receipt", "channel,transaction_id,order_sn,amount_minor,currency"),
        Map.entry("commerce_delivery_intent", "order_sn,buyer_id,sku_id,quantity,status,created_ms"),
        Map.entry("commerce_refund_intent", "channel,transaction_id,order_sn,source,status"),
        Map.entry("commerce_refund_receipt", "channel,payment_transaction_id,transaction_id,order_sn,amount_minor,currency"),
        Map.entry("commerce_abnormal_payment", "channel,transaction_id,order_sn,reason,created_ms"),
        Map.entry("commerce_mock_transaction", "kind,order_sn,request_id,transaction_id,payment_transaction_id,amount_minor,currency,created_ms,applied"),
        Map.entry("commerce_consignment", "listing_id,seller_player_id,seller_account,market_zone,asset_kind,asset_currency_type,escrow_reference,escrow_proof,request_digest,title,price_fen,fee_bps,cooling_ms,notice_end_ms,sale_end_ms,state,current_order_sn,created_ms"),
        Map.entry("commerce_consignment_order", "order_sn,listing_id,buyer_account,buyer_owner_revision,request_id,seller_account,escrow_reference,escrow_proof,fee_bps,cooling_ms"),
        Map.entry("commerce_consignment_return", "listing_id,return_proof"),
        Map.entry("commerce_seller_guard", "account"),
        Map.entry("commerce_seller_income", "order_sn,account,gross_fen,fee_fen,net_fen,reserved_fen,paid_fen,available_ms"),
        Map.entry("commerce_withdrawal", "withdrawal_id,account,request_id,amount_fen,state,channel,transaction_id,created_ms"),
        Map.entry("commerce_withdrawal_allocation", "withdrawal_id,order_sn,amount_fen"),
        Map.entry("commerce_withdrawal_receipt", "withdrawal_id,channel,transaction_id,amount_fen,successful"),
        Map.entry("commerce_mock_payout", "withdrawal_id,request_id,transaction_id,amount_fen,successful,created_ms,applied"),
        Map.entry("commerce_request_nonce", "caller_role,nonce,expires_ms"));
    /**
     * 存储合同戳,与 schema.sql 末行一致。2026-11-10 起换新:之前装的库里,退款待办的 CHECK 只认三种来源,
     * 记不下金额或币种不符的付款(那笔待办插不进去,整笔回滚);下面只核表、列和唯一键,CHECK 的取值测不出,靠换戳强制重装。
     */
    static final String STORAGE_CONTRACT = "mall-game-official-sku-v1-minor-units-abnormal-payment";
    private SchemaVerifier() {}
    static void verify(JdbcTemplate jdbc, DataSource source) throws SQLException {
        List<Map<String, Object>> version = jdbc.queryForList("SELECT version,storage_contract FROM commerce_schema_version");
        if (version.size() != 1 || ((Number)version.get(0).get("version")).intValue() != 1 || !STORAGE_CONTRACT.equals(version.get(0).get("storage_contract"))) {
            throw new IllegalStateException("商城存储版本不兼容，必须使用独立迁移后的商城库");
        }
        try (Connection connection = source.getConnection()) {
            for(String table:List.of("commerce_sku_grant","commerce_order_grant","commerce_delivery_receipt","commerce_refund_intent","commerce_refund_receipt"))try(var s=connection.createStatement();var rows=s.executeQuery("SELECT * FROM "+table+" WHERE 1=0")){rows.getMetaData();}
            if (connection.getTransactionIsolation() != Connection.TRANSACTION_READ_COMMITTED) { throw new IllegalStateException("商城连接必须使用 READ-COMMITTED"); }
            for (var table : COLUMNS.entrySet()) {
                try (PreparedStatement statement = connection.prepareStatement("SELECT " + table.getValue() + " FROM " + table.getKey() + " WHERE 1=0"); ResultSet rows = statement.executeQuery()) {
                    ResultSetMetaData metadata = rows.getMetaData();
                    for (int i = 1; i <= metadata.getColumnCount(); i++) {
                        String column = metadata.getColumnName(i).toLowerCase(Locale.ROOT);
                        if (Set.of("actor", "account", "buyer_account", "buyer_owner_revision", "seller_account", "request_id", "channel", "transaction_id", "payment_transaction_id", "nonce").contains(column) && metadata.getColumnType(i) != Types.VARBINARY) {
                            throw new IllegalStateException("商城不透明标识列必须使用 VARBINARY: " + table.getKey() + "." + column);
                        }
                        if (column.equals("buyer_owner_revision") && metadata.getPrecision(i) != 20) {
                            throw new IllegalStateException("订单归属修订必须为规范uint64的VARBINARY(20)");
                        }
                        // 异常付款(重复付款、金额或币种不符)记成 order_sn 为 NULL 的付款回执;列若仍是 NOT NULL,这些钱会在运行时写不进来。
                        if (table.getKey().equals("commerce_payment_receipt") && column.equals("order_sn")
                                && metadata.isNullable(i) != ResultSetMetaData.columnNullable) {
                            throw new IllegalStateException("付款回执的订单列必须可空，才能记录异常付款");
                        }
                        if (Set.of("price_minor", "pay_amount", "amount_minor", "product_price_minor", "price_fen", "gross_fen", "fee_fen", "net_fen", "reserved_fen", "paid_fen", "amount_fen").contains(column) && metadata.getColumnType(i) != Types.BIGINT) {
                            throw new IllegalStateException("商城金额列必须为最小单位 BIGINT: " + table.getKey() + "." + column);
                        }
                    }
                }
            }
            unique(connection, "commerce_schema_version", List.of("version"));
            unique(connection, "commerce_buyer_guard", List.of("buyer_id"));
            unique(connection, "pms_sku_stock", List.of("id"));
            unique(connection, "oms_order", List.of("order_sn")); unique(connection, "oms_order", List.of("buyer_id", "request_id"));
            unique(connection, "oms_order_item", List.of("order_sn"));
            unique(connection, "commerce_official_owner", List.of("order_sn"));
            unique(connection, "commerce_payment_receipt", List.of("channel", "transaction_id")); unique(connection, "commerce_payment_receipt", List.of("order_sn"));
            unique(connection, "commerce_abnormal_payment", List.of("channel", "transaction_id"));
            // 退款按被退付款的交易键幂等:待办一笔一条、回执一笔一条,渠道退款流水也不能复用。
            unique(connection, "commerce_refund_intent", List.of("channel", "transaction_id"));
            unique(connection, "commerce_refund_receipt", List.of("channel", "payment_transaction_id"));
            unique(connection, "commerce_refund_receipt", List.of("channel", "transaction_id"));
            unique(connection, "commerce_delivery_intent", List.of("order_sn"));
            if(indexes(connection,"commerce_delivery_intent",false).values().stream().noneMatch(cols->cols.size()>=3&&cols.subList(0,3).equals(List.of("status","created_ms","order_sn"))))throw new IllegalStateException("商城交付待办缺可恢复扫描索引");
            unique(connection, "commerce_request_nonce", List.of("caller_role", "nonce"));
            unique(connection, "commerce_admin_command", List.of("actor", "request_id"));
            unique(connection, "commerce_mock_transaction", List.of("kind", "order_sn"));
            unique(connection, "commerce_mock_transaction", List.of("kind", "request_id"));
            unique(connection, "commerce_mock_transaction", List.of("transaction_id"));
            unique(connection, "commerce_consignment", List.of("listing_id"));
            unique(connection, "commerce_consignment", List.of("escrow_reference"));
            unique(connection, "commerce_consignment_order", List.of("order_sn"));
            unique(connection, "commerce_consignment_order", List.of("buyer_account", "request_id"));
            unique(connection, "commerce_consignment_return", List.of("listing_id"));
            unique(connection, "commerce_seller_guard", List.of("account"));
            unique(connection, "commerce_seller_income", List.of("order_sn"));
            unique(connection, "commerce_withdrawal", List.of("withdrawal_id"));
            unique(connection, "commerce_withdrawal", List.of("account", "request_id"));
            unique(connection, "commerce_withdrawal_allocation", List.of("withdrawal_id", "order_sn"));
            unique(connection, "commerce_withdrawal_receipt", List.of("withdrawal_id"));
            unique(connection, "commerce_withdrawal_receipt", List.of("channel", "transaction_id"));
            unique(connection, "commerce_mock_payout", List.of("withdrawal_id"));
            unique(connection, "commerce_mock_payout", List.of("request_id"));
            unique(connection, "commerce_mock_payout", List.of("transaction_id"));
            for (var required : Map.of("commerce_mock_payout", List.of("applied", "withdrawal_id"),
                    "commerce_consignment", List.of("state", "listing_id"),
                    "commerce_seller_income", List.of("account", "order_sn")).entrySet()) {
                if (indexes(connection, required.getKey(), false).values().stream().noneMatch(columns ->
                        columns.size() >= required.getValue().size()
                        && columns.subList(0, required.getValue().size()).equals(required.getValue()))) {
                    throw new IllegalStateException("寄售恢复缺必要扫描索引: " + required.getKey());
                }
            }
            if (indexes(connection, "commerce_mock_transaction", false).values().stream().noneMatch(cols -> cols.size() >= 3 && cols.subList(0, 3).equals(List.of("kind", "applied", "order_sn")))) {
                throw new IllegalStateException("mock交易缺可恢复扫描索引");
            }
            if (indexes(connection, "commerce_refund_intent", false).values().stream().noneMatch(cols -> cols.size() >= 2 && cols.subList(0, 2).equals(List.of("status", "order_sn")))) {
                throw new IllegalStateException("商城退款待办缺可恢复扫描索引");
            }
            if (indexes(connection, "oms_order", false).values().stream().noneMatch(cols -> cols.size() >= 2 && cols.subList(0, 2).equals(List.of("buyer_id", "status")))) {
                throw new IllegalStateException("商城未支付订单计数缺买家状态索引");
            }
            if (indexes(connection, "commerce_request_nonce", false).values().stream().noneMatch(cols -> !cols.isEmpty() && cols.get(0).equals("expires_ms"))) {
                throw new IllegalStateException("商城 nonce 缺过期清理索引");
            }
        }
    }
    private static void unique(Connection connection, String table, List<String> expected) throws SQLException {
        if (!indexes(connection, table, true).containsValue(expected)) { throw new IllegalStateException("商城缺必要唯一约束: " + table + expected); }
    }
    private static Map<String, List<String>> indexes(Connection connection, String table, boolean unique) throws SQLException {
        DatabaseMetaData meta = connection.getMetaData();
        String actual = meta.storesUpperCaseIdentifiers() ? table.toUpperCase(Locale.ROOT) : table;
        Map<String, SortedMap<Integer, String>> collected = new HashMap<>();
        try (ResultSet rows = meta.getIndexInfo(connection.getCatalog(), connection.getSchema(), actual, unique, false)) {
            while (rows.next()) {
                String name = rows.getString("INDEX_NAME"); String column = rows.getString("COLUMN_NAME");
                if (name != null && column != null) { collected.computeIfAbsent(name, ignored -> new TreeMap<>()).put(rows.getInt("ORDINAL_POSITION"), column.toLowerCase(Locale.ROOT)); }
            }
        }
        Map<String, List<String>> result = new HashMap<>(); collected.forEach((name, values) -> result.put(name, List.copyOf(values.values()))); return result;
    }
}
