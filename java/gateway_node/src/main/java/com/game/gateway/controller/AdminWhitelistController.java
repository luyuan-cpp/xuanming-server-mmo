package com.game.gateway.controller;

import com.game.gateway.entity.ZoneWhitelist;
import com.game.gateway.repository.ZoneWhitelistRepository;
import org.springframework.http.ResponseEntity;
import org.springframework.transaction.support.TransactionOperations;
import org.springframework.web.bind.annotation.*;

import java.util.List;

@RestController
@RequestMapping("/admin/whitelist")
public class AdminWhitelistController {

    private final ZoneWhitelistRepository whitelistRepo;
    private final TransactionOperations tx;

    public AdminWhitelistController(ZoneWhitelistRepository whitelistRepo, TransactionOperations tx) {
        this.whitelistRepo = whitelistRepo;
        this.tx = tx;
    }

    @GetMapping("/{zoneId}")
    public List<ZoneWhitelist> listByZone(@PathVariable Long zoneId) {
        return whitelistRepo.findByZoneId(zoneId);
    }

    /**
     * 把 (zone_id, account_id) 加进白名单,返回库里的那一行。成功时仍是 200 + ZoneWhitelist JSON,返回形状不变。
     *
     * <p>相对 2026-09-21 之前的 {@code save(entry)},以下变化都是<b>有意</b>的(死锁审计 #18):
     * <ul>
     *   <li><b>幂等</b>:同一 (zone_id, account_id) 重复 add 不再因唯一键冲突回 500,而是回 200 + 已有那一行,
     *       note 以最后一次 add 为准。写入改用 ODKU 才拆得掉 #18 的环(见 {@link ZoneWhitelistRepository#upsert}),
     *       ODKU 天然就是幂等语义。</li>
     *   <li><b>忽略请求体里的 id</b>,只按业务键定位。旧写法带 id 时 save() 走 merge,会按主键把那一行改成另一组
     *       (zone_id, account_id) —— 既是越过业务键改数据,也是「先主键、后唯一键」的反向取锁。</li>
     *   <li>缺 zone_id / account_id 回 400(旧写法是 Hibernate 非空校验失败,冒成 500)。</li>
     * </ul>
     *
     * <p>upsert 与回读在同一个事务里:upsert 持有该行的 X 直到提交,其间并发的 remove 删不掉它,回读必然命中;
     * 回读是普通一致性读、不加锁,整个事务的锁集与单条 upsert 相同。
     *
     * <p>整事务包在 {@link InnoDbDeadlockRetry#execute} 里:ODKU 已拆掉 #18 的环,那里只兜 InnoDB 固有的
     * 1213,收敛论证也在那里。
     */
    @PostMapping
    public ResponseEntity<ZoneWhitelist> add(@RequestBody ZoneWhitelist entry) {
        if (entry.getZoneId() == null || entry.getAccountId() == null) {
            return ResponseEntity.badRequest().build();
        }
        long zoneId = entry.getZoneId();
        long accountId = entry.getAccountId();
        String note = entry.getNote();
        ZoneWhitelist stored = InnoDbDeadlockRetry.execute(tx, "白名单 add", zoneId, () -> {
            whitelistRepo.upsert(zoneId, accountId, note);
            return whitelistRepo.findByZoneIdAndAccountId(zoneId, accountId)
                    .orElseThrow(() -> new IllegalStateException(
                            "zone_whitelist upsert 之后同一事务内回读不到该行,违反事务可见性,zone_id=" + zoneId));
        });
        return ResponseEntity.ok(stored);
    }

    /**
     * 按业务键删除;本来就不在也回 204,与旧行为一致。
     *
     * <p>删除是 {@link ZoneWhitelistRepository#deleteByZoneIdAndAccountId} 的单条语句、自带事务,控制器不再包事务:
     * 包了也只是同一条语句。不重试:它只取一处业务键的锁,顺序与 upsert 相同(uk 项 → 主键),没有需要兜底的固有情形。
     */
    @DeleteMapping("/{zoneId}/{accountId}")
    public ResponseEntity<Void> remove(@PathVariable Long zoneId, @PathVariable Long accountId) {
        whitelistRepo.deleteByZoneIdAndAccountId(zoneId, accountId);
        return ResponseEntity.noContent().build();
    }
}
