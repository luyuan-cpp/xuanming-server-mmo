package com.game.gateway.repository;

import com.game.gateway.controller.AdminWhitelistController;
import com.game.gateway.entity.ZoneWhitelist;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.jdbc.AutoConfigureTestDatabase;
import org.springframework.boot.test.autoconfigure.orm.jpa.DataJpaTest;
import org.springframework.test.context.ActiveProfiles;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.annotation.Propagation;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionTemplate;

import static org.junit.jupiter.api.Assertions.*;

/**
 * ZoneWhitelistRepository 的 Spring Data 接线测试(死锁审计 #18 改动的另一半证据)。
 *
 * <p>验「语句经 Spring Data / Hibernate 真的能执行、语义对、事务边界在该在的地方」:native ODKU 的命名参数绑定、
 * JPQL 批量删除、按业务键回读,以及控制器 add 的幂等与忽略请求体 id。
 * <b>不验锁行为</b>:H2 没有 InnoDB 的锁语义,锁序由 {@link ZoneWhitelistUpsertLockOrderMySqlTest} 在真 MySQL 上验。
 *
 * <h2>为什么用 NOT_SUPPORTED</h2>
 * {@code @DataJpaTest} 默认给每个用例包一层测试框架的事务(结束即回滚),被测方法只会<b>加入</b>它,从不以事务
 * 发起者身份运行 —— 那样测不到接口方法上的 {@code @Transactional}。而控制器的 remove 已不再自己包事务,
 * JPQL 批量删除能不能执行完全依赖仓库接口方法上的 {@code @Transactional}(没有事务时 Hibernate 抛
 * TransactionRequiredException)。所以这里关掉外层事务:仓库方法、控制器的 TransactionTemplate 都真实开启并
 * 提交自己的事务,数据由 {@link #cleanUp} 按键删掉。
 *
 * <p>H2 开 MySQL 兼容模式,才认 {@code ON DUPLICATE KEY UPDATE … VALUES(col)};唯一键由实体上的
 * {@code @UniqueConstraint(zone_id, account_id)} 经 create-drop 建出,与 uk_zone_account 同列。
 */
@DataJpaTest(properties = "spring.datasource.url=jdbc:h2:mem:whitelist_wiring;MODE=MySQL;DB_CLOSE_DELAY=-1")
@AutoConfigureTestDatabase(replace = AutoConfigureTestDatabase.Replace.NONE)
@ActiveProfiles("test")
@Transactional(propagation = Propagation.NOT_SUPPORTED)
class ZoneWhitelistRepositoryWiringTest {

    private static final long ZONE = 3L;
    private static final long ACCOUNT = 1234L;

    @Autowired private ZoneWhitelistRepository repo;
    @Autowired private PlatformTransactionManager txManager;

    /** 没有外层回滚事务,每个用例写下的行要自己删;删本身也走接口方法上的 @Transactional。 */
    @AfterEach
    void cleanUp() {
        repo.deleteByZoneIdAndAccountId(ZONE, ACCOUNT);
    }

    @Test
    void upsertInsertsThenOnlyUpdatesNoteOnSameNaturalKey() {
        repo.upsert(ZONE, ACCOUNT, "first");
        ZoneWhitelist inserted = repo.findByZoneIdAndAccountId(ZONE, ACCOUNT).orElseThrow();
        assertEquals("first", inserted.getNote());

        repo.upsert(ZONE, ACCOUNT, "second");
        ZoneWhitelist updated = repo.findByZoneIdAndAccountId(ZONE, ACCOUNT).orElseThrow();

        assertEquals(inserted.getId(), updated.getId(), "同键第二次 upsert 必须改原行,不能另插一行");
        assertEquals("second", updated.getNote(), "note 以最后一次为准;读到 first 说明 ODKU 没走到更新分支");
        assertEquals(1, repo.findByZoneId(ZONE).size());
    }

    @Test
    void deleteByNaturalKeyRunsInItsOwnTransactionAndReportsAffectedRows() {
        repo.upsert(ZONE, ACCOUNT, "x");

        // 这里没有任何外层事务:能删成功,就证明接口方法上的 @Transactional 生效了。
        assertEquals(1, repo.deleteByZoneIdAndAccountId(ZONE, ACCOUNT));
        assertTrue(repo.findByZoneIdAndAccountId(ZONE, ACCOUNT).isEmpty());
        assertEquals(0, repo.deleteByZoneIdAndAccountId(ZONE, ACCOUNT), "删不存在的键是 0 行,不报错");
    }

    @Test
    void controllerAddIsIdempotentAndIgnoresBodyId() {
        AdminWhitelistController controller = new AdminWhitelistController(repo, new TransactionTemplate(txManager));

        ZoneWhitelist first = controller.add(body(999L, "a")).getBody();
        ZoneWhitelist second = controller.add(body(999L, "b")).getBody();

        assertNotNull(first);
        assertNotNull(second);
        assertNotEquals(999L, first.getId(), "请求体里的 id 必须被忽略,id 由库分配");
        assertEquals(first.getId(), second.getId(), "重复 add 返回的是同一行(幂等),不再是唯一键冲突");
        assertEquals("b", second.getNote());
        assertEquals(1, repo.findByZoneId(ZONE).size());
    }

    @Test
    void controllerRemoveWithoutOwnTransactionDeletesCommittedRow() {
        AdminWhitelistController controller = new AdminWhitelistController(repo, new TransactionTemplate(txManager));
        controller.add(body(null, "to-remove"));

        assertEquals(204, controller.remove(ZONE, ACCOUNT).getStatusCode().value());
        assertTrue(repo.findByZoneIdAndAccountId(ZONE, ACCOUNT).isEmpty(), "remove 返回 204 之后行必须已删除并提交");
    }

    private static ZoneWhitelist body(Long id, String note) {
        ZoneWhitelist w = new ZoneWhitelist();
        w.setId(id);
        w.setZoneId(ZONE);
        w.setAccountId(ACCOUNT);
        w.setNote(note);
        return w;
    }
}
