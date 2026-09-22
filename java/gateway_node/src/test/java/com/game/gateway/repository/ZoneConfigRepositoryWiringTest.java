package com.game.gateway.repository;

import com.game.gateway.controller.AdminZoneController;
import com.game.gateway.entity.ZoneConfig;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.jdbc.AutoConfigureTestDatabase;
import org.springframework.boot.test.autoconfigure.orm.jpa.DataJpaTest;
import org.springframework.http.HttpStatus;
import org.springframework.test.context.ActiveProfiles;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.annotation.Propagation;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionTemplate;

import java.time.LocalDateTime;

import static org.junit.jupiter.api.Assertions.*;

/**
 * ZoneConfigRepository 与 AdminZoneController 的 Spring Data 接线测试(死锁审计 #18 同类项的另一半证据)。
 *
 * <p>验「语句经 Spring Data / Hibernate 真的能执行、语义对」:native ODKU 的命名参数绑定(含 open_time 为 null、
 * 同名参数 :now 出现两次)、覆盖分支保留 created_at、JPQL 批量 UPDATE / DELETE、COALESCE 保留维护文案,
 * 以及控制器各写接口的 200 / 204 / 404 与「update 绝不插入」。<b>不验锁行为</b>:H2 没有 InnoDB 的锁语义,
 * 锁序由 {@link ZoneConfigUpsertLockOrderMySqlTest} 在真 MySQL 上验。
 *
 * <p>用 NOT_SUPPORTED 关掉 {@code @DataJpaTest} 的外层回滚事务,理由同 {@link ZoneWhitelistRepositoryWiringTest}:
 * 仓库接口方法上的 {@code @Transactional}、控制器的 TransactionTemplate 都以事务发起者身份真实提交
 * (delete 接口不包事务,全靠接口方法上的 {@code @Transactional})。数据由 {@link #cleanUp} 按键删掉。
 *
 * <p>仓库层用例的时间一律用整秒:H2 的 TIMESTAMP 精度与 MySQL DATETIME 不同,整秒在两边都原样往返。控制器用例
 * 走真实时钟(包级时钟注入点在另一个包里够不着),只比较两次回读得到的库内值。
 */
@DataJpaTest(properties = "spring.datasource.url=jdbc:h2:mem:zone_config_wiring;MODE=MySQL;DB_CLOSE_DELAY=-1")
@AutoConfigureTestDatabase(replace = AutoConfigureTestDatabase.Replace.NONE)
@ActiveProfiles("test")
@Transactional(propagation = Propagation.NOT_SUPPORTED)
class ZoneConfigRepositoryWiringTest {

    private static final long ZONE = 42L;
    private static final long ABSENT_ZONE = 43L;
    private static final LocalDateTime T1 = LocalDateTime.of(2026, 9, 21, 10, 0, 0);
    private static final LocalDateTime T2 = LocalDateTime.of(2026, 9, 21, 11, 30, 0);

    @Autowired private ZoneConfigRepository repo;
    @Autowired private PlatformTransactionManager txManager;

    @AfterEach
    void cleanUp() {
        repo.deleteByZoneId(ZONE);
        repo.deleteByZoneId(ABSENT_ZONE);
    }

    @Test
    void upsertInsertsThenOverwritesBusinessColumnsButKeepsCreatedAt() {
        repo.upsert(zone(ZONE, "first", 0, 5000, "", null, true, 1), T1);
        ZoneConfig inserted = repo.findById(ZONE).orElseThrow();
        assertEquals("first", inserted.getName());
        assertTrue(inserted.isRecommended());
        assertNull(inserted.getOpenTime(), "open_time 绑 null 必须原样落库");
        assertEquals(T1, inserted.getCreatedAt());
        assertEquals(T1, inserted.getUpdatedAt());

        LocalDateTime openTime = LocalDateTime.of(2026, 10, 1, 9, 0, 0);
        repo.upsert(zone(ZONE, "second", 1, 8000, "维护中", openTime, false, 7), T2);
        ZoneConfig overwritten = repo.findById(ZONE).orElseThrow();

        assertEquals("second", overwritten.getName());
        assertEquals(1, overwritten.getManualStatus());
        assertEquals(8000, overwritten.getCapacity());
        assertEquals("维护中", overwritten.getMaintenanceMsg());
        assertEquals(openTime, overwritten.getOpenTime());
        assertFalse(overwritten.isRecommended());
        assertEquals(7, overwritten.getSortOrder());
        assertEquals(T1, overwritten.getCreatedAt(), "覆盖分支不得改 created_at(ServerListService 靠它判「新服」)");
        assertEquals(T2, overwritten.getUpdatedAt());
        assertEquals(1, repo.findAll().stream().filter(z -> z.getZoneId() == ZONE).count(), "同键不能另插一行");
    }

    @Test
    void updateSettingsNeverInsertsAndKeepsCreatedAt() {
        repo.updateSettings(ABSENT_ZONE, zone(ABSENT_ZONE, "ghost", 0, 1, "", null, false, 0), T1);
        assertTrue(repo.findById(ABSENT_ZONE).isEmpty(), "更新不存在的区服不得把它插进来");

        repo.upsert(zone(ZONE, "a", 0, 5000, "", null, false, 0), T1);
        repo.updateSettings(ZONE, zone(ZONE, "b", 2, 100, "关服", null, true, 3), T2);

        ZoneConfig z = repo.findById(ZONE).orElseThrow();
        assertEquals("b", z.getName());
        assertEquals(2, z.getManualStatus());
        assertEquals(100, z.getCapacity());
        assertEquals("关服", z.getMaintenanceMsg());
        assertTrue(z.isRecommended());
        assertEquals(3, z.getSortOrder());
        assertEquals(T1, z.getCreatedAt());
        assertEquals(T2, z.getUpdatedAt());
    }

    @Test
    void updateManualStatusKeepsMessageWhenNullAndLeavesOtherColumnsAlone() {
        repo.upsert(zone(ZONE, "keep-me", 0, 6000, "", null, true, 9), T1);

        repo.updateManualStatus(ZONE, 1, "14:00 恢复", T2);
        repo.updateManualStatus(ZONE, 1, null, T2);
        ZoneConfig maintenance = repo.findById(ZONE).orElseThrow();
        assertEquals(1, maintenance.getManualStatus());
        assertEquals("14:00 恢复", maintenance.getMaintenanceMsg(), "文案参数为 null 时必须保留原文案");
        assertEquals("keep-me", maintenance.getName(), "改状态不得动其余列");
        assertEquals(6000, maintenance.getCapacity());
        assertEquals(9, maintenance.getSortOrder());

        repo.updateManualStatus(ZONE, 0, "", T2);
        ZoneConfig open = repo.findById(ZONE).orElseThrow();
        assertEquals(0, open.getManualStatus());
        assertEquals("", open.getMaintenanceMsg());
    }

    @Test
    void deleteByZoneIdRunsInItsOwnTransactionAndReportsAffectedRows() {
        repo.upsert(zone(ZONE, "x", 0, 5000, "", null, false, 0), T1);

        assertEquals(1, repo.deleteByZoneId(ZONE));
        assertTrue(repo.findById(ZONE).isEmpty());
        assertEquals(0, repo.deleteByZoneId(ZONE), "删不存在的区服是 0 行,不报错");
    }

    @Test
    void controllerWritePathsEndToEnd() {
        AdminZoneController controller = new AdminZoneController(repo, new TransactionTemplate(txManager));

        ZoneConfig created = controller.create(zone(ZONE, "z", 0, 5000, "", null, false, 0)).getBody();
        assertNotNull(created);
        assertNotNull(created.getCreatedAt(), "插入分支写入 created_at,回读得到的是库里的值");

        ZoneConfig recreated = controller.create(zone(ZONE, "z2", 0, 5000, "", null, false, 0)).getBody();
        assertNotNull(recreated);
        assertEquals("z2", recreated.getName(), "已存在即覆盖,与旧 save() 语义一致");
        assertEquals(created.getCreatedAt(), recreated.getCreatedAt(), "覆盖分支保留并返回库里的 created_at");

        assertEquals(HttpStatus.OK, controller.update(ZONE, zone(ZONE, "z3", 0, 5000, "", null, false, 0)).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND,
                controller.update(ABSENT_ZONE, zone(ABSENT_ZONE, "ghost", 0, 5000, "", null, false, 0)).getStatusCode());
        assertTrue(repo.findById(ABSENT_ZONE).isEmpty(), "update 不存在的区服回 404,且不得插入");

        ZoneConfig maintenance = controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest("m")).getBody();
        assertNotNull(maintenance);
        assertEquals(1, maintenance.getManualStatus());
        assertEquals("z3", maintenance.getName());
        assertEquals(HttpStatus.NOT_FOUND, controller.setMaintenance(ABSENT_ZONE, null).getStatusCode());

        ZoneConfig open = controller.setOpen(ZONE).getBody();
        assertNotNull(open);
        assertEquals(0, open.getManualStatus());
        assertEquals("", open.getMaintenanceMsg());
        assertEquals(HttpStatus.NOT_FOUND, controller.setOpen(ABSENT_ZONE).getStatusCode());

        assertEquals(HttpStatus.NO_CONTENT, controller.delete(ZONE).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.delete(ZONE).getStatusCode());
        assertTrue(repo.findById(ZONE).isEmpty());
    }

    private static ZoneConfig zone(long zoneId, String name, int manualStatus, int capacity, String maintenanceMsg,
                                   LocalDateTime openTime, boolean recommended, int sortOrder) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(zoneId);
        z.setName(name);
        z.setManualStatus(manualStatus);
        z.setCapacity(capacity);
        z.setMaintenanceMsg(maintenanceMsg);
        z.setOpenTime(openTime);
        z.setRecommended(recommended);
        z.setSortOrder(sortOrder);
        return z;
    }
}
