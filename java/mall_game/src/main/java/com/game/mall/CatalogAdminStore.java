package com.game.mall;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Clock;
import java.util.HexFormat;
import java.util.List;
import java.util.Objects;
import java.util.UUID;
import java.util.function.Supplier;
import org.springframework.context.annotation.DependsOn;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Repository;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;

/** 管理请求行→SKU主键的固定锁序；成功响应快照与业务写入同事务提交。 */
@Repository
@DependsOn("commerceSchemaReady")
// 保留Repository异常翻译的类代理，与同域CommerceStore一致。
public class CatalogAdminStore {
    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final ObjectMapper json;
    private final Clock clock;

    public CatalogAdminStore(JdbcTemplate jdbc, PlatformTransactionManager manager, ObjectMapper json, Clock clock) {
        this.jdbc = jdbc;
        this.json = json;
        this.clock = clock;
        jdbc.setQueryTimeout(5);
        tx = new TransactionTemplate(manager);
        tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        tx.setTimeout(5);
    }

    public CatalogAdminApi.Skus list(DeliveryApi.SkuPage page) {
        if (page == null || page.afterSkuId() < 0 || page.limit() < 1 || page.limit() > 100) {
            throw new CommerceException(400, "管理目录分页非法");
        }
        return new CatalogAdminApi.Skus(query("WHERE s.id>? ORDER BY s.id LIMIT ?", page.afterSkuId(), page.limit()));
    }

    public CatalogAdminApi.Sku create(String actor, CatalogAdminApi.Create input) {
        if (input == null || input.skuId() <= 0 || input.stock() < 0) {
            throw new CommerceException(400, "新增SKU参数非法");
        }
        validate(input.terms());
        return command(actor, input.requestId(), "create", input, input.skuId(), () -> {
            var terms = input.terms();
            jdbc.update("INSERT INTO pms_sku_stock(id,product_id,sku_code,price_minor,currency,stock,lock_stock,published,catalog_revision)"
                + " VALUES(?,?,?,?,?,?,0,?,1)", input.skuId(), terms.productId(), terms.skuCode(), terms.priceMinor(),
                terms.currency(), input.stock(), terms.published() ? 1 : 0);
            writeGrant(input.skuId(), terms);
            return read(input.skuId());
        });
    }

    public CatalogAdminApi.Sku update(String actor, CatalogAdminApi.Update input) {
        if (input == null || input.skuId() <= 0) {
            throw new CommerceException(400, "修改SKU参数非法");
        }
        validate(input.terms());
        return command(actor, input.requestId(), "update", input, input.skuId(), () -> {
            lockRevision(input.skuId(), input.expectedRevision());
            var terms = input.terms();
            // 不出现stock/lock_stock，运营条款更新不能覆盖下单或付款刚提交的数量。
            int changed = jdbc.update("UPDATE pms_sku_stock SET product_id=?,sku_code=?,price_minor=?,currency=?,published=?,"
                + "catalog_revision=catalog_revision+1 WHERE id=? AND catalog_revision=?",
                terms.productId(), terms.skuCode(), terms.priceMinor(), terms.currency(), terms.published() ? 1 : 0,
                input.skuId(), input.expectedRevision());
            if (changed != 1) { throw new CommerceException(409, "管理目录修订已变化"); }
            writeGrant(input.skuId(), terms);
            return read(input.skuId());
        });
    }

    public CatalogAdminApi.Sku adjustStock(String actor, CatalogAdminApi.AdjustStock input) {
        if (input == null || input.skuId() <= 0 || input.delta() == 0) {
            throw new CommerceException(400, "库存调整参数非法");
        }
        return command(actor, input.requestId(), "stock", input, input.skuId(), () -> {
            lockRevision(input.skuId(), input.expectedRevision());
            var current = read(input.skuId());
            try {
                validate(current.terms());
            } catch (CommerceException incomplete) {
                throw new CommerceException(409, "请先修复SKU条款和发物映射再调整库存");
            }
            long after = (long) current.stock() + input.delta();
            if (after < current.lockedStock() || after > Integer.MAX_VALUE) {
                throw new CommerceException(409, "库存调整越界或侵占未付款预占");
            }
            int changed = jdbc.update("UPDATE pms_sku_stock SET stock=stock+?,catalog_revision=catalog_revision+1 "
                + "WHERE id=? AND catalog_revision=? AND stock+?>=lock_stock AND stock+?<=?",
                input.delta(), input.skuId(), input.expectedRevision(), input.delta(), input.delta(), Integer.MAX_VALUE);
            if (changed != 1) { throw new CommerceException(409, "库存或管理修订已变化"); }
            return read(input.skuId());
        });
    }

    private CatalogAdminApi.Sku command(String actor, String requestId, String kind, Object input, long skuId,
        Supplier<CatalogAdminApi.Sku> mutation) {
        if (actor == null || !actor.matches("[a-z][a-z0-9-]{2,63}") || requestId == null
            || !requestId.matches("[A-Za-z0-9_-]{8,128}")) {
            throw new CommerceException(400, "管理主体或请求号非法");
        }
        String digest = digest(kind, input);
        byte[] actorKey = actor.getBytes(StandardCharsets.US_ASCII);
        byte[] requestKey = requestId.getBytes(StandardCharsets.US_ASCII);
        String pending = "pending:" + UUID.randomUUID();
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                // 同主键ODKU先串行化；失败事务不会留下独立的半完成请求行。
                jdbc.update("INSERT INTO commerce_admin_command(actor,request_id,command_type,request_digest,result_json,created_ms)"
                    + " VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE request_id=request_id",
                    actorKey, requestKey, kind, digest, pending, clock.millis());
                var rows = jdbc.queryForList("SELECT command_type,request_digest,result_json FROM commerce_admin_command"
                    + " WHERE actor=? AND request_id=? FOR UPDATE", actorKey, requestKey);
                if (rows.size() != 1) { throw new CommerceException(503, "管理请求原件缺失"); }
                var stored = rows.getFirst();
                if (!kind.equals(stored.get("command_type")) || !digest.equals(stored.get("request_digest"))) {
                    throw new CommerceException(409, "管理请求号已用于不同条款");
                }
                if (!pending.equals(stored.get("result_json"))) {
                    if (stored.get("result_json") == null) { throw new CommerceException(503, "管理请求响应原件缺失"); }
                    var replay = decode(stored.get("result_json").toString());
                    if (replay.skuId() != skuId) { throw new CommerceException(503, "管理请求响应绑定了不同SKU"); }
                    return replay;
                }
                var result = validResponse(mutation.get());
                if (jdbc.update("UPDATE commerce_admin_command SET result_json=? WHERE actor=? AND request_id=? AND result_json=?",
                        encode(result), actorKey, requestKey, pending) != 1) {
                    throw new CommerceException(503, "管理命令响应记录失败");
                }
                return result;
            }));
        } catch (DuplicateKeyException conflict) {
            throw new CommerceException(409, "SKU已存在或管理请求冲突");
        }
    }

    private void lockRevision(long skuId, long expected) {
        if (expected < 0 || expected == Long.MAX_VALUE) { throw new CommerceException(400, "管理修订非法或已耗尽"); }
        var rows = jdbc.queryForList("SELECT catalog_revision FROM pms_sku_stock WHERE id=? FOR UPDATE", skuId);
        if (rows.isEmpty()) { throw new CommerceException(404, "SKU不存在"); }
        if (((Number) rows.getFirst().get("catalog_revision")).longValue() != expected) {
            throw new CommerceException(409, "管理目录修订已变化");
        }
    }

    private CatalogAdminApi.Sku read(long id) {
        var rows = query("WHERE s.id=?", id);
        if (rows.size() != 1) { throw new CommerceException(503, "SKU读回失败"); }
        return rows.getFirst();
    }

    private List<CatalogAdminApi.Sku> query(String clause, Object... parameters) {
        return jdbc.query("SELECT s.*,g.item_config_id,g.count_per_unit,g.title,g.currency_type,g.currency_amount FROM pms_sku_stock s "
            + "LEFT JOIN commerce_sku_grant g ON g.sku_id=s.id " + clause,
            (row, index) -> new CatalogAdminApi.Sku(row.getLong("id"), row.getLong("catalog_revision"),
                row.getLong("product_id"), row.getString("sku_code"), row.getLong("price_minor"), row.getString("currency"),
                row.getInt("stock"), row.getInt("lock_stock"), row.getInt("published") == 1,
                row.getInt("item_config_id"), row.getInt("count_per_unit"), row.getString("title"),
                row.getInt("currency_type"), row.getLong("currency_amount")), parameters);
    }

    private void writeGrant(long id, CatalogAdminApi.Terms terms) {
        jdbc.update("INSERT INTO commerce_sku_grant(sku_id,item_config_id,count_per_unit,currency_type,currency_amount,title) VALUES(?,?,?,?,?,?) "
            + "ON DUPLICATE KEY UPDATE item_config_id=VALUES(item_config_id),count_per_unit=VALUES(count_per_unit),"
            + "currency_type=VALUES(currency_type),currency_amount=VALUES(currency_amount),title=VALUES(title)",
            id, terms.itemConfigId(), terms.countPerUnit(), terms.currencyType(), terms.currencyAmount(), terms.title());
    }

    private static void validate(CatalogAdminApi.Terms terms) {
        if (terms == null || terms.productId() <= 0 || terms.skuCode() == null
            || !terms.skuCode().matches("[A-Za-z0-9_.-]{1,64}") || terms.priceMinor() < 1
            || terms.priceMinor() > CommerceStore.MAX_AMOUNT_MINOR || terms.currency() == null
            || !terms.currency().matches("[A-Z][A-Z0-9_]{2,15}") || !(itemGrant(terms) || currencyGrant(terms))
            || terms.title() == null || terms.title().isBlank() || terms.title().length() > 128) {
            throw new CommerceException(400, "SKU条款非法");
        }
    }

    /** 物品发放:每份 1..4096 件,货币字段为 0。与 commerce_sku_grant 的 CHECK 同一口径。 */
    private static boolean itemGrant(CatalogAdminApi.Terms terms) {
        return terms.itemConfigId() >= 1 && terms.countPerUnit() >= 1 && terms.countPerUnit() <= 4096
            && terms.currencyType() == 0 && terms.currencyAmount() == 0;
    }

    /** 货币发放(充值包):每份 1..MAX_CURRENCY_GRANT,物品字段为 0;货币种类 0 是银两,有没有货币看数量。种类是否存在由 trade 收货时核对。 */
    private static boolean currencyGrant(CatalogAdminApi.Terms terms) {
        return terms.itemConfigId() == 0 && terms.countPerUnit() == 0 && terms.currencyType() >= 0
            && terms.currencyAmount() >= 1 && terms.currencyAmount() <= DeliveryStore.MAX_CURRENCY_GRANT;
    }

    private String digest(String kind, Object input) {
        try {
            byte[] body = (kind + "\n" + json.writeValueAsString(input)).getBytes(StandardCharsets.UTF_8);
            return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(body));
        } catch (JsonProcessingException | NoSuchAlgorithmException invalid) {
            throw new IllegalStateException("管理请求摘要不可用", invalid);
        }
    }

    private String encode(CatalogAdminApi.Sku result) {
        try { return json.writeValueAsString(result); }
        catch (JsonProcessingException invalid) { throw new IllegalStateException("管理响应无法编码", invalid); }
    }

    private CatalogAdminApi.Sku decode(String raw) {
        try {
            return validResponse(json.readerFor(CatalogAdminApi.Sku.class)
                .with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS).readValue(raw));
        } catch (JsonProcessingException | CommerceException corrupt) {
            throw new CommerceException(503, "管理请求响应原件损坏");
        }
    }

    private CatalogAdminApi.Sku validResponse(CatalogAdminApi.Sku result) {
        if (result == null || result.skuId() <= 0 || result.revision() <= 0 || result.stock() < 0
            || result.lockedStock() < 0 || result.lockedStock() > result.stock()) {
            throw new CommerceException(503, "管理请求响应缺少合法SKU");
        }
        try {
            validate(result.terms());
        } catch (CommerceException incomplete) {
            throw new CommerceException(503, "管理请求响应缺少合法条款");
        }
        return result;
    }
}
