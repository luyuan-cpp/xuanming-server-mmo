package com.game.gateway.repository;

import com.game.gateway.controller.AdminZoneController;
import com.game.gateway.dto.ZoneSettingsRequest;
import com.game.gateway.entity.ZoneConfig;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.jdbc.AutoConfigureTestDatabase;
import org.springframework.boot.test.autoconfigure.orm.jpa.DataJpaTest;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.test.context.ActiveProfiles;
import org.springframework.transaction.IllegalTransactionStateException;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.annotation.Propagation;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionTemplate;

import java.time.LocalDateTime;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

/**
 * ZoneConfigRepository 与 AdminZoneController 的 Spring Data 接线测试(死锁审计 #18 同类项的另一半证据)。
 *
 * <p>验「语句经 Spring Data / Hibernate 真的能执行、语义对」:native ODKU 的命名参数绑定(含 open_time 为 null、
 * 同名参数 :now 出现两次)、已存在的行一列不改(第一轮数据审计 §4 第 18 条)、JPQL 批量 UPDATE / DELETE、
 * COALESCE 保留维护文案,以及控制器各写接口的 200 / 204 / 400 / 404 / 409 与「update 绝不插入」。
 * <b>不验锁行为</b>:H2 没有 InnoDB 的锁语义,
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
    void insertIfAbsentInsertsOnceAndNeverOverwritesExistingRow() {
        repo.insertIfAbsent(zone(ZONE, "first", 1, 5000, "维护中", null, true, 1), T1);
        ZoneConfig inserted = repo.findById(ZONE).orElseThrow();
        assertEquals("first", inserted.getName());
        assertEquals(1, inserted.getManualStatus());
        assertTrue(inserted.isRecommended());
        assertNull(inserted.getOpenTime(), "open_time 绑 null 必须原样落库");
        assertEquals(T1, inserted.getCreatedAt());
        assertEquals(T1, inserted.getUpdatedAt());

        // 第一轮数据审计 §4 第 18 条的 POST 变种:同键第二次写入曾覆盖全部业务列,把维护中的区服改回 OPEN。
        LocalDateTime openTime = LocalDateTime.of(2026, 10, 1, 9, 0, 0);
        repo.insertIfAbsent(zone(ZONE, "second", 0, 8000, "", openTime, false, 7), T2);
        ZoneConfig kept = repo.findById(ZONE).orElseThrow();

        assertEquals("first", kept.getName());
        assertEquals(1, kept.getManualStatus(), "已存在的行一列都不能改,维护中不能被改回 OPEN");
        assertEquals(5000, kept.getCapacity());
        assertEquals("维护中", kept.getMaintenanceMsg());
        assertNull(kept.getOpenTime());
        assertTrue(kept.isRecommended());
        assertEquals(1, kept.getSortOrder());
        assertEquals(T1, kept.getCreatedAt(), "不得改 created_at(ServerListService 靠它判「新服」)");
        assertEquals(T1, kept.getUpdatedAt(), "没有改写就不刷新 updated_at");
        assertEquals(1, repo.findAll().stream().filter(z -> z.getZoneId() == ZONE).count(), "同键不能另插一行");
    }

    @Test
    void updateSettingsNeverInsertsAndKeepsCreatedAt() {
        repo.updateSettings(ABSENT_ZONE, zone(ABSENT_ZONE, "ghost", 0, 1, "", null, false, 0), T1);
        assertTrue(repo.findById(ABSENT_ZONE).isEmpty(), "更新不存在的区服不得把它插进来");

        repo.insertIfAbsent(zone(ZONE, "a", 0, 5000, "", null, false, 0), T1);
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
        repo.insertIfAbsent(zone(ZONE, "keep-me", 0, 6000, "", null, true, 9), T1);

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

    // 暂停创建新角色(2026-11-10):新建区服取库缺省(开着,原生 INSERT 不写这一列);开关只改这一列,PUT 整行替换不碰它。
    @Test
    void createRoleClosedDefaultsOpenAndOnlyItsToggleChangesIt() {
        repo.insertIfAbsent(zone(ZONE, "keep-me", 0, 6000, "", null, true, 9), T1);
        assertFalse(repo.findById(ZONE).orElseThrow().isCreateRoleClosed(), "a new zone accepts new characters");

        repo.updateCreateRoleClosed(ZONE, true, T2);
        ZoneConfig closed = repo.findById(ZONE).orElseThrow();
        assertTrue(closed.isCreateRoleClosed());
        assertEquals("keep-me", closed.getName(), "the toggle must not touch other columns");
        assertEquals(6000, closed.getCapacity());
        assertEquals(0, closed.getManualStatus());
        assertEquals(T2, closed.getUpdatedAt());

        repo.updateSettings(ZONE, zone(ZONE, "renamed", 0, 7000, "", null, false, 1), T2);
        assertTrue(repo.findById(ZONE).orElseThrow().isCreateRoleClosed(), "a full-row PUT keeps the toggle");

        repo.updateCreateRoleClosed(ZONE, false, T2);
        assertFalse(repo.findById(ZONE).orElseThrow().isCreateRoleClosed());
        repo.updateCreateRoleClosed(ABSENT_ZONE, true, T2);
        assertTrue(repo.findById(ABSENT_ZONE).isEmpty(), "an absent zone is never inserted");
    }

    // 本区角色上限(2026-11-12):新区缺省 0;只有它自己的写改它;按上限关闭是比较并写 —— 上限变了、为 0、
    // 开关已关都一行不改(返回 0),命中才关上(返回 1),其余列不动。
    @Test
    void roleCapacityDefaultsZeroAndCloseAtCapacityIsConditional() {
        repo.insertIfAbsent(zone(ZONE, "keep-me", 0, 6000, "", null, true, 9), T1);
        assertEquals(0, repo.findById(ZONE).orElseThrow().getRoleCapacity(), "a new zone has no role capacity");
        assertEquals(0, repo.closeCreateRoleAtRoleCapacity(ZONE, 0, T2), "no capacity, nothing to close");

        repo.updateRoleCapacity(ZONE, 30000, T2);
        ZoneConfig capped = repo.findById(ZONE).orElseThrow();
        assertEquals(30000, capped.getRoleCapacity());
        assertEquals("keep-me", capped.getName(), "the capacity write must not touch other columns");
        assertFalse(capped.isCreateRoleClosed());

        repo.updateSettings(ZONE, zone(ZONE, "renamed", 0, 7000, "", null, false, 1), T2);
        assertEquals(30000, repo.findById(ZONE).orElseThrow().getRoleCapacity(), "a full-row PUT keeps the capacity");

        assertEquals(0, repo.closeCreateRoleAtRoleCapacity(ZONE, 20000, T2), "a decision made with a stale capacity is void");
        assertFalse(repo.findById(ZONE).orElseThrow().isCreateRoleClosed());

        assertEquals(1, repo.closeCreateRoleAtRoleCapacity(ZONE, 30000, T2));
        ZoneConfig closed = repo.findById(ZONE).orElseThrow();
        assertTrue(closed.isCreateRoleClosed());
        assertEquals(30000, closed.getRoleCapacity());
        assertEquals(7000, closed.getCapacity(), "closing must not touch other columns");
        assertEquals(0, repo.closeCreateRoleAtRoleCapacity(ZONE, 30000, T2), "already closed: nothing changes");

        assertEquals(0, repo.closeCreateRoleAtRoleCapacity(ABSENT_ZONE, 30000, T2));
        repo.updateRoleCapacity(ABSENT_ZONE, 10, T2);
        assertTrue(repo.findById(ABSENT_ZONE).isEmpty(), "an absent zone is never inserted");
    }

    @Test
    void deleteByZoneIdRunsInItsOwnTransactionAndReportsAffectedRows() {
        repo.insertIfAbsent(zone(ZONE, "x", 0, 5000, "", null, false, 0), T1);

        assertEquals(1, repo.deleteByZoneId(ZONE));
        assertTrue(repo.findById(ZONE).isEmpty());
        assertEquals(0, repo.deleteByZoneId(ZONE), "删不存在的区服是 0 行,不报错");
    }

    @Test
    void lockZoneIdForUpdateFindsOnlyExistingRowAndRequiresCallerTransaction() {
        repo.insertIfAbsent(zone(ZONE, "z", 0, 5000, "", null, false, 0), T1);
        TransactionTemplate tx = new TransactionTemplate(txManager);

        assertEquals(List.of(ZONE), tx.execute(status -> repo.lockZoneIdForUpdate(ZONE)), "行在:返回它的主键");
        assertTrue(tx.execute(status -> repo.lockZoneIdForUpdate(ABSENT_ZONE)).isEmpty(), "行不在:空列表");
        // 自动提交下加锁读读完就放锁,判了也白判;没有调用方事务必须直接报错,不能悄悄执行。
        assertThrows(IllegalTransactionStateException.class, () -> repo.lockZoneIdForUpdate(ZONE));
    }

    @Test
    void controllerWritePathsEndToEnd() {
        AdminZoneController controller = new AdminZoneController(repo, new TransactionTemplate(txManager));

        ZoneConfig created = controller.create(settings(ZONE, "z", 1, 5000, "维护中", null, false, 0)).getBody();
        assertNotNull(created);
        assertNotNull(created.getCreatedAt(), "插入分支写入 created_at,回读得到的是库里的值");

        ResponseEntity<ZoneConfig> replay = controller.create(settings(ZONE, "z", 1, 5000, "维护中", null, false, 0));
        assertEquals(HttpStatus.OK, replay.getStatusCode(), "同一份请求重放是幂等的 200");
        assertEquals(created.getCreatedAt(), replay.getBody().getCreatedAt());

        ResponseEntity<ZoneConfig> conflict = controller.create(settings(ZONE, "z2", 0, 9000, "", null, true, 5));
        assertEquals(HttpStatus.CONFLICT, conflict.getStatusCode(), "已存在且内容不同:不覆盖,回 409");
        ZoneConfig untouched = repo.findById(ZONE).orElseThrow();
        assertEquals("z", untouched.getName());
        assertEquals(1, untouched.getManualStatus(), "维护中的区服不能被一次 create 改回 OPEN");
        assertEquals(5000, untouched.getCapacity());
        assertEquals("维护中", untouched.getMaintenanceMsg());

        ZoneSettingsRequest partial = new ZoneSettingsRequest();
        partial.setName("only-name");
        assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, partial).getStatusCode(), "只给 name 的 PUT 回 400");
        assertEquals(1, repo.findById(ZONE).orElseThrow().getManualStatus(), "被拒的 PUT 一列都不写");

        assertEquals(HttpStatus.OK, controller.update(ZONE, settings(null, "z3", 1, 5000, "维护中", null, false, 0)).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND,
                controller.update(ABSENT_ZONE, settings(null, "ghost", 0, 5000, "", null, false, 0)).getStatusCode());
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

    /** 控制器的请求体:每个业务字段都给(openTime 经 setter 记为「给了」,null 就是没有开服时间)。 */
    private static ZoneSettingsRequest settings(Long zoneId, String name, int manualStatus, int capacity,
                                                String maintenanceMsg, LocalDateTime openTime, boolean recommended,
                                                int sortOrder) {
        ZoneSettingsRequest s = new ZoneSettingsRequest();
        s.setZoneId(zoneId);
        s.setName(name);
        s.setManualStatus(manualStatus);
        s.setCapacity(capacity);
        s.setMaintenanceMsg(maintenanceMsg);
        s.setOpenTime(openTime);
        s.setRecommended(recommended);
        s.setSortOrder(sortOrder);
        return s;
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
