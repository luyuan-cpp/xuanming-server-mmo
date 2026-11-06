package com.game.gateway.controller;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.dto.ZoneSettingsRequest;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.dao.CannotAcquireLockException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.json.Jackson2ObjectMapperBuilder;
import org.springframework.transaction.support.TransactionOperations;

import java.sql.SQLTransactionRollbackException;
import java.time.LocalDateTime;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.Mockito.*;

/**
 * AdminZoneController 的契约测试(死锁审计 #18 同类项的控制器侧)。
 *
 * <p>只验控制器自己的承诺:create 走「只插不改」+ 回读、与请求逐列一致回 200、不一致回 409,且只对 1213 做有上限的
 * 整事务重试;create / update 的请求体必须给全每个业务字段,缺字段或取值越界回 400 且不碰库;update /
 * setMaintenance / setOpen 走按主键的单条 UPDATE、存在性由回读判定(不看行数)、绝不插入;delete 按删除行数回
 * 204 / 404;时间戳取自注入的时钟。「ODKU 在真库上不成环」由 {@code ZoneConfigUpsertLockOrderMySqlTest} 验,
 * 语句能否在 Spring Data 上执行、已存在的行是否真的一列不改由 {@code ZoneConfigRepositoryWiringTest} 验,这里替代不了。
 *
 * <p>请求体经与线上同一种 Jackson 配置(Spring 的构建器:未知字段忽略、注册 java.time)从 JSON 反序列化,
 * 「字段给没给」由 JSON 决定,不在测试里手搭对象绕过绑定。
 *
 * <p>事务用 {@link TransactionOperations#withoutTransaction()} 直通:这里不验事务边界。
 */
class AdminZoneControllerTest {

    private static final long ZONE = 7L;
    private static final LocalDateTime NOW = LocalDateTime.of(2026, 9, 21, 12, 0, 0);
    private static final ObjectMapper JSON = Jackson2ObjectMapperBuilder.json().build();
    /** 完整资源的每一个业务字段;PUT 的请求体可以不带 zoneId(以路径为准),POST 必须带。 */
    private static final List<String> BUSINESS_FIELDS =
            List.of("name", "manualStatus", "capacity", "maintenanceMsg", "openTime", "recommended", "sortOrder");

    private ZoneConfigRepository repo;
    private AdminZoneController controller;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneConfigRepository.class);
        controller = new AdminZoneController(repo, TransactionOperations.withoutTransaction(), () -> NOW);
    }

    @Test
    void createInsertsRequestedRowAndReturnsStoredRow() {
        ZoneConfig stored = storedCopyOf(fields(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(stored));

        ResponseEntity<ZoneConfig> resp = controller.create(body(fields(ZONE)));

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody(), "返回库里回读的那一行,而不是回显请求体");
        ArgumentCaptor<ZoneConfig> written = ArgumentCaptor.forClass(ZoneConfig.class);
        verify(repo).insertIfAbsent(written.capture(), eq(NOW));
        assertWrittenEquals(fields(ZONE), ZONE, written.getValue());
        verify(repo).findById(ZONE);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void createNeverOverwritesExistingZoneAndAnswersConflict() {
        // 第一轮数据审计 §4 第 18 条的 POST 变种:zone_id 已存在时旧 create 走 ODKU 全列覆盖,维护中的区服会被一个
        // 「新建」请求改回 OPEN。现在只插不改;库里那一行与请求不一致就回 409 并带回原行,一列都不动。
        ZoneConfig existing = storedCopyOf(fields(ZONE));
        existing.setManualStatus(1);
        existing.setMaintenanceMsg("维护中");
        when(repo.findById(ZONE)).thenReturn(Optional.of(existing));
        Map<String, Object> open = fields(ZONE);
        open.put("manualStatus", 0);
        open.put("maintenanceMsg", "");

        ResponseEntity<ZoneConfig> resp = controller.create(body(open));

        assertEquals(HttpStatus.CONFLICT, resp.getStatusCode());
        assertSame(existing, resp.getBody(), "409 带回库里那一行,便于运维改走 PUT");
        verify(repo).insertIfAbsent(any(), eq(NOW));
        verify(repo, never()).updateSettings(any(), any(), any());
        verify(repo, never()).updateManualStatus(anyLong(), anyInt(), any(), any());
    }

    @Test
    void createReplayWithIdenticalSettingsIsIdempotent() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(storedCopyOf(fields(ZONE))));

        assertEquals(HttpStatus.OK, controller.create(body(fields(ZONE))).getStatusCode());
        assertEquals(HttpStatus.OK, controller.create(body(fields(ZONE))).getStatusCode(), "同一份请求重放仍是 200");
        verify(repo, times(2)).insertIfAbsent(any(), eq(NOW));
    }

    @Test
    void createRejectsMissingZoneIdOrAnyMissingFieldWithoutTouchingDb() {
        assertEquals(HttpStatus.BAD_REQUEST, controller.create(body(fields(null))).getStatusCode(), "缺 zoneId");
        Map<String, Object> zeroId = fields(ZONE);
        zeroId.put("zoneId", 0);
        assertEquals(HttpStatus.BAD_REQUEST, controller.create(body(zeroId)).getStatusCode(), "zoneId 必须为正");
        for (String field : BUSINESS_FIELDS) {
            Map<String, Object> missing = fields(ZONE);
            missing.remove(field);
            assertEquals(HttpStatus.BAD_REQUEST, controller.create(body(missing)).getStatusCode(), "缺 " + field);
        }
        verifyNoInteractions(repo);
    }

    @Test
    void createRetriesDeadlockVictimAsWholeUnitThenSucceeds() {
        when(repo.insertIfAbsent(any(), eq(NOW))).thenThrow(deadlock()).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.of(storedCopyOf(fields(ZONE))));

        assertEquals(HttpStatus.OK, controller.create(body(fields(ZONE))).getStatusCode());
        verify(repo, times(2)).insertIfAbsent(any(), eq(NOW));
        verify(repo, times(1)).findById(ZONE);
    }

    @Test
    void createGivesUpAfterBoundedAttemptsAndRethrows() {
        CannotAcquireLockException dl = deadlock();
        when(repo.insertIfAbsent(any(), any())).thenThrow(dl);

        CannotAcquireLockException thrown =
                assertThrows(CannotAcquireLockException.class, () -> controller.create(body(fields(ZONE))));

        assertSame(dl, thrown, "用尽重试后原样抛出,不包装、不吞");
        verify(repo, times(InnoDbDeadlockRetry.MAX_ATTEMPTS)).insertIfAbsent(any(), any());
        verify(repo, never()).findById(anyLong());
    }

    @Test
    void createFailsLoudlyWhenRowIsInvisibleAfterInsert() {
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        assertThrows(IllegalStateException.class, () -> controller.create(body(fields(ZONE))));
    }

    @Test
    void updateOverwritesEverySettingFromCompleteBodyAndReturnsStoredRow() {
        Map<String, Object> complete = fields(null); // 请求体可以不带 zoneId,以路径为准
        complete.put("openTime", "2026-10-01T09:00:00");
        ZoneConfig stored = storedCopyOf(complete);
        when(repo.findById(ZONE)).thenReturn(Optional.of(stored));

        ResponseEntity<ZoneConfig> resp = controller.update(ZONE, body(complete));

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody());
        ArgumentCaptor<ZoneConfig> written = ArgumentCaptor.forClass(ZoneConfig.class);
        verify(repo).updateSettings(eq(ZONE), written.capture(), eq(NOW));
        assertWrittenEquals(complete, ZONE, written.getValue());
        verify(repo, never()).insertIfAbsent(any(), any());

        // 带 zoneId 也行,只要与路径一致(GET 回来的整行原样 PUT 回去,连 createdAt / updatedAt 一起)。
        Map<String, Object> roundTrip = fields(ZONE);
        roundTrip.put("createdAt", "2026-09-01T00:00:00");
        roundTrip.put("updatedAt", "2026-09-02T00:00:00");
        assertEquals(HttpStatus.OK, controller.update(ZONE, body(roundTrip)).getStatusCode());
    }

    @Test
    void updateRejectsPartialBodyWithoutTouchingDb() {
        // 第一轮数据审计 §4 第 18 条:只给 name 的 PUT 曾把维护中的区服改成 OPEN(manualStatus 基本类型缺省 0),
        // 容量 / 推荐 / 排序 / 开服时间 / 维护文案一并被初值覆盖。完整资源语义:缺任何一个业务字段都回 400。
        assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, body(Map.of("name", "renamed"))).getStatusCode());
        for (String field : BUSINESS_FIELDS) {
            Map<String, Object> missing = fields(null);
            missing.remove(field);
            assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, body(missing)).getStatusCode(), "缺 " + field);
            if (!field.equals("openTime")) {
                Map<String, Object> explicitNull = fields(null);
                explicitNull.put(field, null);
                assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, body(explicitNull)).getStatusCode(),
                        field + " 显式 null 等同没给");
            }
        }
        assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, null).getStatusCode(), "没有请求体");
        verifyNoInteractions(repo);
    }

    @Test
    void updateRejectsBodyZoneIdThatDiffersFromPath() {
        // 把 zone 99 的整行误 PUT 到 zone 7 上,会用别的区服的设置整行覆盖本区服;路径与请求体不一致一律拒绝。
        assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, body(fields(99L))).getStatusCode());
        verifyNoInteractions(repo);
    }

    @Test
    void writesRejectUndefinedStatusAndOutOfRangeValuesWithoutTouchingDb() {
        Object[][] invalid = {
                {"manualStatus", 4}, {"manualStatus", -1}, {"capacity", 0}, {"capacity", -5},
                {"name", " "}, {"name", "x".repeat(65)}, {"maintenanceMsg", "m".repeat(257)},
                {"openTime", "2026-10-01T09:00:00.5"}};
        for (Object[] bad : invalid) {
            Map<String, Object> post = fields(ZONE);
            post.put((String) bad[0], bad[1]);
            assertEquals(HttpStatus.BAD_REQUEST, controller.create(body(post)).getStatusCode(), bad[0] + "=" + bad[1]);
            Map<String, Object> put = fields(null);
            put.put((String) bad[0], bad[1]);
            assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, body(put)).getStatusCode(), bad[0] + "=" + bad[1]);
        }
        verifyNoInteractions(repo);
    }

    @Test
    void updateDecidesNotFoundByReadBackNotByRowCountAndNeverInserts() {
        // 行数报 1(found rows)也不能当作存在:以同一事务里的回读为准。
        when(repo.updateSettings(eq(ZONE), any(), eq(NOW))).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        assertEquals(HttpStatus.NOT_FOUND, controller.update(ZONE, body(fields(ZONE))).getStatusCode());
        verify(repo, never()).insertIfAbsent(any(), any());
    }

    @Test
    void setMaintenanceKeepsMessageWhenBodyOrMessageAbsent() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setMaintenance(ZONE, null).getStatusCode());
        assertEquals(HttpStatus.OK,
                controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest(null)).getStatusCode());

        verify(repo, times(2)).updateManualStatus(ZONE, 1, null, NOW);
    }

    @Test
    void setMaintenanceWritesGivenMessage() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest("14:00 恢复"));

        verify(repo).updateManualStatus(ZONE, 1, "14:00 恢复", NOW);
    }

    @Test
    void setOpenClearsMessage() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setOpen(ZONE).getStatusCode());
        verify(repo).updateManualStatus(ZONE, 0, "", NOW);
    }

    @Test
    void manualStatusActionsReturnNotFoundWhenRowAbsent() {
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        assertEquals(HttpStatus.NOT_FOUND, controller.setMaintenance(ZONE, null).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.setOpen(ZONE).getStatusCode());
        verify(repo, never()).insertIfAbsent(any(), any());
    }

    @Test
    void updatePathsDoNotRetryDeadlockClassifiedFailures() {
        // update 路径只有一把锁、拿到后不再等,不可能是死锁牺牲者;万一底层报了,也原样抛出,不做重试。
        when(repo.updateManualStatus(anyLong(), anyInt(), any(), any())).thenThrow(deadlock());

        assertThrows(CannotAcquireLockException.class, () -> controller.setOpen(ZONE));
        verify(repo, times(1)).updateManualStatus(anyLong(), anyInt(), any(), any());
    }

    @Test
    void deleteMapsAffectedRowsToNoContentOrNotFound() {
        when(repo.deleteByZoneId(ZONE)).thenReturn(1, 0);

        assertEquals(HttpStatus.NO_CONTENT, controller.delete(ZONE).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.delete(ZONE).getStatusCode());
        verify(repo, times(2)).deleteByZoneId(ZONE);
        verifyNoMoreInteractions(repo);
    }

    private static CannotAcquireLockException deadlock() {
        // Connector/J 对 1213 抛的就是 SQLTransactionRollbackException 的子类。
        return new CannotAcquireLockException("deadlock", new SQLTransactionRollbackException(
                "Deadlock found when trying to get lock; try restarting transaction", "40001", 1213));
    }

    private static ZoneConfig zone(Long zoneId, String name) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(zoneId);
        z.setName(name);
        return z;
    }

    /** 一份完整且合法的请求体字段(openTime 显式 null = 没有开服时间);zoneId 为 null 时不带这个字段。 */
    private static Map<String, Object> fields(Long zoneId) {
        Map<String, Object> f = new LinkedHashMap<>();
        if (zoneId != null) {
            f.put("zoneId", zoneId);
        }
        f.put("name", "z");
        f.put("manualStatus", 1);
        f.put("capacity", 8000);
        f.put("maintenanceMsg", "14:00 恢复");
        f.put("openTime", null);
        f.put("recommended", true);
        f.put("sortOrder", 3);
        return f;
    }

    private static ZoneSettingsRequest body(Map<String, ?> fields) {
        try {
            return JSON.readValue(JSON.writeValueAsString(fields), ZoneSettingsRequest.class);
        } catch (JsonProcessingException e) {
            throw new AssertionError("测试请求体反序列化失败: " + fields, e);
        }
    }

    /** 库里与这份请求体逐列相同的那一行(回读替身)。 */
    private static ZoneConfig storedCopyOf(Map<String, Object> fields) {
        Object id = fields.get("zoneId");
        ZoneConfig z = zone(id == null ? ZONE : ((Number) id).longValue(), (String) fields.get("name"));
        z.setManualStatus((Integer) fields.get("manualStatus"));
        z.setCapacity((Integer) fields.get("capacity"));
        z.setMaintenanceMsg((String) fields.get("maintenanceMsg"));
        Object openTime = fields.get("openTime");
        z.setOpenTime(openTime == null ? null : LocalDateTime.parse((String) openTime));
        z.setRecommended((Boolean) fields.get("recommended"));
        z.setSortOrder((Integer) fields.get("sortOrder"));
        return z;
    }

    /** 交给仓库绑定的那一行必须逐列等于请求体,不能有任何一列落成实体的基本类型初值。 */
    private static void assertWrittenEquals(Map<String, Object> fields, long zoneId, ZoneConfig written) {
        ZoneConfig expected = storedCopyOf(fields);
        assertEquals(zoneId, written.getZoneId());
        assertEquals(expected.getName(), written.getName());
        assertEquals(expected.getManualStatus(), written.getManualStatus());
        assertEquals(expected.getCapacity(), written.getCapacity());
        assertEquals(expected.getMaintenanceMsg(), written.getMaintenanceMsg());
        assertEquals(expected.getOpenTime(), written.getOpenTime());
        assertEquals(expected.isRecommended(), written.isRecommended());
        assertEquals(expected.getSortOrder(), written.getSortOrder());
    }
}
