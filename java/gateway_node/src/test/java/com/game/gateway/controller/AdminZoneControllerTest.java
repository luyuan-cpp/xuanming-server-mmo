package com.game.gateway.controller;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.PropertyNamingStrategies;
import com.game.gateway.dto.ZoneSettingsRequest;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.function.Executable;
import org.mockito.ArgumentCaptor;
import org.mockito.InOrder;
import org.springframework.dao.CannotAcquireLockException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ProblemDetail;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.json.Jackson2ObjectMapperBuilder;
import org.springframework.transaction.support.TransactionOperations;
import org.springframework.web.ErrorResponseException;

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
 * <p>只验控制器自己的承诺:create 走「只插不改」+ 加锁回读、与请求逐列一致回 200、不一致回 409,且只对 1213 做有上限的
 * 整事务重试,新行的起始版本取自注入的来源;create / update 的请求体必须给全每个业务字段,缺字段或取值越界回 400
 * (problem details,字段名 + 原因码,不回显请求里的值)且不碰库;update 必须带 If-Match(没带 428、写法不对 400、
 * 版本不符 412,都不写);update / setMaintenance / setOpen 先按主键加锁点查判存在(不看行数、不靠回读),在才走按主键的
 * 单条 UPDATE 再回读,不在回 404 且不写,绝不插入;GET 回 ETag;delete 按删除行数回
 * 204 / 404;时间戳取自注入的时钟。「ODKU 在真库上不成环」由 {@code ZoneConfigUpsertLockOrderMySqlTest} 验,
 * 语句能否在 Spring Data 上执行、已存在的行是否真的一列不改、版本条件是否真的挡住旧版本由
 * {@code ZoneConfigRepositoryWiringTest} 验,HTTP 上的头与错误体由 {@code AdminZoneHttpContractTest} 验,这里替代不了。
 *
 * <p>请求体经与线上同一种 Jackson 配置(Spring 的构建器:未知字段忽略、注册 java.time,加上 application.yaml 的
 * {@code spring.jackson.property-naming-strategy: SNAKE_CASE})从 JSON 反序列化,「字段给没给」由 JSON 决定,
 * 不在测试里手搭对象绕过绑定。
 *
 * <p>事务用 {@link TransactionOperations#withoutTransaction()} 直通:这里不验事务边界。
 */
class AdminZoneControllerTest {

    private static final long ZONE = 7L;
    private static final LocalDateTime NOW = LocalDateTime.of(2026, 9, 21, 12, 0, 0);
    /** 注入的新行起始版本。 */
    private static final long NEW_ROW_VERSION = 9_001L;
    /** update 用例里库中那一行的当前版本,以及 GET 会发出的 ETag(If-Match 原样带回)。 */
    private static final long VERSION = 41L;
    private static final String ETAG = "\"41\"";
    private static final ObjectMapper JSON = Jackson2ObjectMapperBuilder.json()
            .propertyNamingStrategy(PropertyNamingStrategies.SNAKE_CASE).build();
    /** 完整资源的每一个业务字段(线上 JSON 名);PUT 的请求体可以不带 zone_id(以路径为准),POST 必须带。 */
    private static final List<String> BUSINESS_FIELDS =
            List.of("name", "manual_status", "capacity", "maintenance_msg", "open_time", "recommended", "sort_order");

    private ZoneConfigRepository repo;
    private AdminZoneController controller;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneConfigRepository.class);
        controller = new AdminZoneController(repo, TransactionOperations.withoutTransaction(), () -> NOW,
                () -> NEW_ROW_VERSION);
    }

    @Test
    void createInsertsRequestedRowAndReturnsStoredRow() {
        ZoneConfig stored = storedCopyOf(fields(ZONE));
        when(repo.findByIdForUpdate(ZONE)).thenReturn(Optional.of(stored));

        ResponseEntity<ZoneConfig> resp = controller.create(body(fields(ZONE)));

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody(), "返回库里回读的那一行,而不是回显请求体");
        assertNull(resp.getHeaders().getETag(), "create 的响应不带 ETag:要改先 GET");
        ArgumentCaptor<ZoneConfig> written = ArgumentCaptor.forClass(ZoneConfig.class);
        verify(repo).insertIfAbsent(written.capture(), eq(NOW));
        assertWrittenEquals(fields(ZONE), ZONE, written.getValue());
        assertEquals(NEW_ROW_VERSION, written.getValue().getVersion(), "新行从注入的起始版本起,不从 1 起");
        // 回读是加锁读(2026-11-12):已存在分支里 ODKU 一列不改,普通读在 TiDB 的 RR 下可能看不见别人刚提交的那一行。
        verify(repo).findByIdForUpdate(ZONE);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void createNeverOverwritesExistingZoneAndAnswersConflict() {
        // 第一轮数据审计 §4 第 18 条的 POST 变种:zone_id 已存在时旧 create 走 ODKU 全列覆盖,维护中的区服会被一个
        // 「新建」请求改回 OPEN。现在只插不改;库里那一行与请求不一致就回 409 并带回原行,一列都不动。
        ZoneConfig existing = storedCopyOf(fields(ZONE));
        existing.setManualStatus(1);
        existing.setMaintenanceMsg("维护中");
        when(repo.findByIdForUpdate(ZONE)).thenReturn(Optional.of(existing));
        Map<String, Object> open = fields(ZONE);
        open.put("manual_status", 0);
        open.put("maintenance_msg", "");

        ResponseEntity<ZoneConfig> resp = controller.create(body(open));

        assertEquals(HttpStatus.CONFLICT, resp.getStatusCode());
        assertSame(existing, resp.getBody(), "409 带回库里那一行,便于运维改走 PUT");
        verify(repo).insertIfAbsent(any(), eq(NOW));
        verify(repo, never()).updateSettings(any(), any(), anyLong(), any());
        verify(repo, never()).updateManualStatus(anyLong(), anyInt(), any(), any());
    }

    @Test
    void createReplayWithIdenticalSettingsIsIdempotent() {
        when(repo.findByIdForUpdate(ZONE)).thenReturn(Optional.of(storedCopyOf(fields(ZONE))));

        assertEquals(HttpStatus.OK, controller.create(body(fields(ZONE))).getStatusCode());
        assertEquals(HttpStatus.OK, controller.create(body(fields(ZONE))).getStatusCode(), "同一份请求重放仍是 200");
        verify(repo, times(2)).insertIfAbsent(any(), eq(NOW));
    }

    @Test
    void createRejectsMissingZoneIdOrAnyMissingFieldWithoutTouchingDb() {
        assertProblem(HttpStatus.BAD_REQUEST, "missing", "zone_id", () -> controller.create(body(fields(null))));
        Map<String, Object> zeroId = fields(ZONE);
        zeroId.put("zone_id", 0);
        assertProblem(HttpStatus.BAD_REQUEST, "invalid", "zone_id", () -> controller.create(body(zeroId)));
        for (String field : BUSINESS_FIELDS) {
            Map<String, Object> missing = fields(ZONE);
            missing.remove(field);
            assertProblem(HttpStatus.BAD_REQUEST, "missing", field, () -> controller.create(body(missing)));
        }
        assertProblem(HttpStatus.BAD_REQUEST, "missing", null, () -> controller.create(null));
        verifyNoInteractions(repo);
    }

    @Test
    void createRetriesDeadlockVictimAsWholeUnitThenSucceeds() {
        when(repo.insertIfAbsent(any(), eq(NOW))).thenThrow(deadlock()).thenReturn(1);
        when(repo.findByIdForUpdate(ZONE)).thenReturn(Optional.of(storedCopyOf(fields(ZONE))));

        assertEquals(HttpStatus.OK, controller.create(body(fields(ZONE))).getStatusCode());
        verify(repo, times(2)).insertIfAbsent(any(), eq(NOW));
        verify(repo, times(1)).findByIdForUpdate(ZONE);
    }

    @Test
    void createGivesUpAfterBoundedAttemptsAndRethrows() {
        CannotAcquireLockException dl = deadlock();
        when(repo.insertIfAbsent(any(), any())).thenThrow(dl);

        CannotAcquireLockException thrown =
                assertThrows(CannotAcquireLockException.class, () -> controller.create(body(fields(ZONE))));

        assertSame(dl, thrown, "用尽重试后原样抛出,不包装、不吞");
        verify(repo, times(InnoDbDeadlockRetry.MAX_ATTEMPTS)).insertIfAbsent(any(), any());
        verify(repo, never()).findByIdForUpdate(anyLong());
    }

    @Test
    void createFailsLoudlyWhenRowIsInvisibleAfterInsert() {
        when(repo.findByIdForUpdate(ZONE)).thenReturn(Optional.empty());

        assertThrows(IllegalStateException.class, () -> controller.create(body(fields(ZONE))));
    }

    @Test
    void getByIdAnswersStrongEtagOfTheRowVersion() {
        ZoneConfig row = zone(ZONE, "z");
        row.setVersion(VERSION);
        when(repo.findById(ZONE)).thenReturn(Optional.of(row));

        ResponseEntity<ZoneConfig> resp = controller.getById(ZONE);

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(row, resp.getBody());
        assertEquals(ETAG, resp.getHeaders().getETag(), "强标签,引号里是这一行的版本");
        ResponseEntity<ZoneConfig> absent = controller.getById(ZONE + 1);
        assertEquals(HttpStatus.NOT_FOUND, absent.getStatusCode());
        assertNull(absent.getHeaders().getETag());
    }

    @Test
    void updateOverwritesEverySettingFromCompleteBodyAndReturnsStoredRow() {
        Map<String, Object> complete = fields(null); // 请求体可以不带 zone_id,以路径为准
        complete.put("open_time", "2026-10-01T09:00:00");
        ZoneConfig stored = storedCopyOf(complete);
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.updateSettings(eq(ZONE), any(), eq(VERSION), eq(NOW))).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.of(stored));

        ResponseEntity<ZoneConfig> resp = controller.update(ZONE, ETAG, body(complete));

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody());
        assertNull(resp.getHeaders().getETag(), "PUT 的 200 不带 ETag(RFC 9110 §9.3.4),要连着再改就再 GET");
        ArgumentCaptor<ZoneConfig> written = ArgumentCaptor.forClass(ZoneConfig.class);
        verify(repo).updateSettings(eq(ZONE), written.capture(), eq(VERSION), eq(NOW));
        assertWrittenEquals(complete, ZONE, written.getValue());
        verify(repo, never()).insertIfAbsent(any(), any());

        // 带 zone_id 也行,只要与路径一致(GET 回来的整行原样 PUT 回去,连 created_at / updated_at 一起)。
        Map<String, Object> roundTrip = fields(ZONE);
        roundTrip.put("created_at", "2026-09-01T00:00:00");
        roundTrip.put("updated_at", "2026-09-02T00:00:00");
        assertEquals(HttpStatus.OK, controller.update(ZONE, ETAG, body(roundTrip)).getStatusCode());
    }

    @Test
    void updateRejectsPartialBodyWithoutTouchingDb() {
        // 第一轮数据审计 §4 第 18 条:只给 name 的 PUT 曾把维护中的区服改成 OPEN(manualStatus 基本类型缺省 0),
        // 容量 / 推荐 / 排序 / 开服时间 / 维护文案一并被初值覆盖。完整资源语义:缺任何一个业务字段都回 400,
        // 错误体点名第一个缺的字段。
        assertProblem(HttpStatus.BAD_REQUEST, "missing", "manual_status",
                () -> controller.update(ZONE, ETAG, body(Map.of("name", "renamed"))));
        for (String field : BUSINESS_FIELDS) {
            Map<String, Object> missing = fields(null);
            missing.remove(field);
            assertProblem(HttpStatus.BAD_REQUEST, "missing", field, () -> controller.update(ZONE, ETAG, body(missing)));
            if (!field.equals("open_time")) {
                Map<String, Object> explicitNull = fields(null);
                explicitNull.put(field, null);
                assertProblem(HttpStatus.BAD_REQUEST, "missing", field,
                        () -> controller.update(ZONE, ETAG, body(explicitNull)));
            }
        }
        assertProblem(HttpStatus.BAD_REQUEST, "missing", null, () -> controller.update(ZONE, ETAG, null));
        // 线上 JSON 是 snake_case:驼峰键名是未知字段、被静默忽略,等同全都没给,同样只能是 400,不能落成初值。
        Map<String, Object> camelCase = new LinkedHashMap<>();
        camelCase.put("name", "z");
        camelCase.put("manualStatus", 1);
        camelCase.put("capacity", 8000);
        camelCase.put("maintenanceMsg", "");
        camelCase.put("openTime", null);
        camelCase.put("recommended", true);
        camelCase.put("sortOrder", 3);
        assertProblem(HttpStatus.BAD_REQUEST, "missing", "manual_status",
                () -> controller.update(ZONE, ETAG, body(camelCase)));
        verifyNoInteractions(repo);
    }

    @Test
    void updateRejectsBodyZoneIdThatDiffersFromPath() {
        // 把 zone 99 的整行误 PUT 到 zone 7 上,会用别的区服的设置整行覆盖本区服;路径与请求体不一致一律拒绝。
        assertProblem(HttpStatus.BAD_REQUEST, "mismatch", "zone_id", () -> controller.update(ZONE, ETAG, body(fields(99L))));
        verifyNoInteractions(repo);
    }

    @Test
    void writesRejectUndefinedStatusAndOutOfRangeValuesWithoutTouchingDb() {
        Object[][] invalid = {
                {"manual_status", 4, "invalid"}, {"manual_status", -1, "invalid"}, {"capacity", 0, "invalid"},
                {"capacity", -5, "invalid"}, {"name", " ", "invalid"}, {"name", "x".repeat(65), "too_long"},
                {"maintenance_msg", "m".repeat(257), "too_long"}, {"open_time", "2026-10-01T09:00:00.5", "invalid"}};
        for (Object[] bad : invalid) {
            String field = (String) bad[0];
            String reason = (String) bad[2];
            Map<String, Object> post = fields(ZONE);
            post.put(field, bad[1]);
            assertProblem(HttpStatus.BAD_REQUEST, reason, field, () -> controller.create(body(post)));
            Map<String, Object> put = fields(null);
            put.put(field, bad[1]);
            assertProblem(HttpStatus.BAD_REQUEST, reason, field, () -> controller.update(ZONE, ETAG, body(put)));
        }
        verifyNoInteractions(repo);
    }

    @Test
    void badRequestNamesTheFieldButNeverEchoesTheRequestValue() {
        // 错误体回给调用方,也进日志:只说哪个字段、为什么,不把请求里的值原样带出去。
        Map<String, Object> longName = fields(ZONE);
        longName.put("name", "LEAK-" + "x".repeat(70));
        ProblemDetail tooLong = assertProblem(HttpStatus.BAD_REQUEST, "too_long", "name", () -> controller.create(body(longName)));
        assertFalse(tooLong.getDetail().contains("LEAK"), tooLong.getDetail());
        Map<String, Object> negative = fields(null);
        negative.put("capacity", -987_654);
        ProblemDetail invalid = assertProblem(HttpStatus.BAD_REQUEST, "invalid", "capacity",
                () -> controller.update(ZONE, ETAG, body(negative)));
        assertFalse(invalid.getDetail().contains("987"), invalid.getDetail());
        verifyNoInteractions(repo);
    }

    @Test
    void updateWithoutUsableIfMatchIsPreconditionRequiredAndNeverTouchesDb() {
        // 没带 If-Match 就是「不知道自己基于哪个版本」:两个运维先后 GET 再 PUT 时后写覆盖前写,正是要挡的情形。
        // * 只要求行存在,同样不指明版本。
        for (String ifMatch : new String[]{null, "", "  ", "*", ETAG + ", *"}) {
            assertProblem(HttpStatus.PRECONDITION_REQUIRED, "if_match_required", "If-Match",
                    () -> controller.update(ZONE, ifMatch, body(fields(ZONE))));
        }
        verifyNoInteractions(repo);
    }

    @Test
    void updateWithMalformedIfMatchIsBadRequestAndNeverTouchesDb() {
        // 不加引号不是实体标签;一次只能基于一个版本,带两个标签不接受。
        for (String ifMatch : new String[]{"41", "\"41\", \"42\""}) {
            assertProblem(HttpStatus.BAD_REQUEST, "if_match_invalid", "If-Match",
                    () -> controller.update(ZONE, ifMatch, body(fields(ZONE))));
        }
        verifyNoInteractions(repo);
    }

    @Test
    void updateWithStaleVersionIsPreconditionFailedAndWritesNothing() {
        // 版本条件在 UPDATE 的 WHERE 里:行在(点查锁住了)而 UPDATE 一行没改,就是版本不符。412,不回读、不插入。
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.updateSettings(eq(ZONE), any(), eq(40L), eq(NOW))).thenReturn(0);

        assertProblem(HttpStatus.PRECONDITION_FAILED, "version_mismatch", "If-Match",
                () -> controller.update(ZONE, "\"40\"", body(fields(ZONE))));

        verify(repo).updateSettings(eq(ZONE), any(), eq(40L), eq(NOW));
        verify(repo, never()).findById(any());
        verify(repo, never()).insertIfAbsent(any(), any());
    }

    @Test
    void updateWithTagThatCanNeverMatchIsPreconditionFailedWithoutIssuingTheUpdate() {
        // If-Match 用强比较:弱标签与任何版本都不相等;引号里不是本服务发出的版本号(前导零、0、超出 long、别的格式)也一样。
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        for (String ifMatch : new String[]{"W/" + ETAG, "\"abc\"", "\"041\"", "\"0\"", "\"9999999999999999999\""}) {
            assertProblem(HttpStatus.PRECONDITION_FAILED, "version_mismatch", "If-Match",
                    () -> controller.update(ZONE, ifMatch, body(fields(ZONE))));
        }
        verify(repo, never()).updateSettings(any(), any(), anyLong(), any());
    }

    @Test
    void updateDecidesNotFoundByLockedLookupAndNeverWrites() {
        // 存在性以 UPDATE 之前的加锁点查为准(mock 的点查缺省返回空列表 = 行不在):回 404,UPDATE 不执行,也绝不插入。
        // 不看 UPDATE 的行数;也不靠回读 —— RC 与 TiDB 下并发 create 能插在
        // UPDATE 与回读之间,回读看见的那一行不是这次写的。这里让回读「看得见」一行,结论仍必须是 404。
        // 区服不在时 404 优先于 If-Match 的比较(RFC 9110 §13.2.1),带着永远对不上的弱标签也是 404。
        when(repo.updateSettings(eq(ZONE), any(), anyLong(), eq(NOW))).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "inserted concurrently")));

        assertEquals(HttpStatus.NOT_FOUND, controller.update(ZONE, ETAG, body(fields(ZONE))).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.update(ZONE, "W/" + ETAG, body(fields(ZONE))).getStatusCode());
        verify(repo, times(2)).lockZoneIdForUpdate(ZONE);
        verify(repo, never()).updateSettings(any(), any(), anyLong(), any());
        verify(repo, never()).insertIfAbsent(any(), any());
        verify(repo, never()).findById(any());
    }

    @Test
    void writesToExistingZoneLockTheRowBeforeWritingThenReadBack() {
        // 先加锁点查、再写、再回读,三步在同一个事务里按这个顺序:锁住了才写,存在性才与隔离级别无关。
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.updateSettings(eq(ZONE), any(), eq(VERSION), eq(NOW))).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.update(ZONE, ETAG, body(fields(ZONE))).getStatusCode());
        assertEquals(HttpStatus.OK, controller.setOpen(ZONE).getStatusCode());

        InOrder order = inOrder(repo);
        order.verify(repo).lockZoneIdForUpdate(ZONE);
        order.verify(repo).updateSettings(eq(ZONE), any(), eq(VERSION), eq(NOW));
        order.verify(repo).findById(ZONE);
        order.verify(repo).lockZoneIdForUpdate(ZONE);
        order.verify(repo).updateManualStatus(ZONE, 0, "", NOW);
        order.verify(repo).findById(ZONE);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void setMaintenanceKeepsMessageWhenBodyOrMessageAbsent() {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setMaintenance(ZONE, null).getStatusCode());
        assertEquals(HttpStatus.OK,
                controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest(null)).getStatusCode());

        verify(repo, times(2)).updateManualStatus(ZONE, 1, null, NOW);
    }

    @Test
    void setMaintenanceWritesGivenMessage() {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest("14:00 恢复"));

        verify(repo).updateManualStatus(ZONE, 1, "14:00 恢复", NOW);
    }

    @Test
    void setOpenClearsMessage() {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setOpen(ZONE).getStatusCode());
        verify(repo).updateManualStatus(ZONE, 0, "", NOW);
    }

    @Test
    void manualStatusActionsReturnNotFoundWhenRowAbsent() {
        // 加锁点查没命中(mock 缺省返回空列表):回 404,状态不写、也不插入。
        assertEquals(HttpStatus.NOT_FOUND, controller.setMaintenance(ZONE, null).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.setOpen(ZONE).getStatusCode());
        verify(repo, times(2)).lockZoneIdForUpdate(ZONE);
        verify(repo, never()).updateManualStatus(anyLong(), anyInt(), any(), any());
        verify(repo, never()).insertIfAbsent(any(), any());
    }

    // 暂停 / 恢复创建新角色(2026-11-10):同样先加锁判存在,再只改这一列,再回读;其余列不动。
    @Test
    void createRoleTogglesLockThenWriteOnlyThatColumnThenReadBack() {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.closeRoleCreation(ZONE).getStatusCode());
        assertEquals(HttpStatus.OK, controller.openRoleCreation(ZONE).getStatusCode());

        InOrder order = inOrder(repo);
        order.verify(repo).lockZoneIdForUpdate(ZONE);
        order.verify(repo).updateCreateRoleClosed(ZONE, true, NOW);
        order.verify(repo).findById(ZONE);
        order.verify(repo).lockZoneIdForUpdate(ZONE);
        order.verify(repo).updateCreateRoleClosed(ZONE, false, NOW);
        order.verify(repo).findById(ZONE);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void createRoleTogglesReturnNotFoundWhenRowAbsent() {
        // 加锁点查没命中(mock 缺省返回空列表):回 404,开关不写、也不插入。
        assertEquals(HttpStatus.NOT_FOUND, controller.closeRoleCreation(ZONE).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.openRoleCreation(ZONE).getStatusCode());
        verify(repo, times(2)).lockZoneIdForUpdate(ZONE);
        verify(repo, never()).updateCreateRoleClosed(anyLong(), anyBoolean(), any());
        verify(repo, never()).insertIfAbsent(any(), any());
    }

    // 本区角色上限(2026-11-12,区服满员处理 §6):先按主键加锁判存在,只写这一列,再回读;0 = 取消上限。
    @Test
    void roleCapacityLocksThenWritesOnlyThatColumnThenReadsBack() throws JsonProcessingException {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setRoleCapacity(ZONE, roleCapacityBody("{\"role_capacity\":30000}")).getStatusCode());
        assertEquals(HttpStatus.OK, controller.setRoleCapacity(ZONE, roleCapacityBody("{\"role_capacity\":0}")).getStatusCode());

        InOrder order = inOrder(repo);
        order.verify(repo).lockZoneIdForUpdate(ZONE);
        order.verify(repo).updateRoleCapacity(ZONE, 30000, NOW);
        order.verify(repo).findById(ZONE);
        order.verify(repo).lockZoneIdForUpdate(ZONE);
        order.verify(repo).updateRoleCapacity(ZONE, 0, NOW);
        order.verify(repo).findById(ZONE);
        verifyNoMoreInteractions(repo);
    }

    // 缺字段(含写成驼峰、被 SNAKE_CASE 绑定忽略)或为负:400 且不碰库。漏传不能被当成 0(那等于取消上限)。
    // 2026-11-12 起 400 带错误体:缺字段是 missing,为负是 invalid,字段名都是 role_capacity。
    @Test
    void roleCapacityRejectsMissingOrNegativeValueWithoutTouchingTheDatabase() throws JsonProcessingException {
        assertProblem(HttpStatus.BAD_REQUEST, "missing", "role_capacity", () -> controller.setRoleCapacity(ZONE, null));
        AdminZoneController.RoleCapacityRequest empty = roleCapacityBody("{}");
        assertProblem(HttpStatus.BAD_REQUEST, "missing", "role_capacity", () -> controller.setRoleCapacity(ZONE, empty));
        AdminZoneController.RoleCapacityRequest camelCase = roleCapacityBody("{\"roleCapacity\":30000}");
        assertProblem(HttpStatus.BAD_REQUEST, "missing", "role_capacity", () -> controller.setRoleCapacity(ZONE, camelCase));
        AdminZoneController.RoleCapacityRequest negative = roleCapacityBody("{\"role_capacity\":-1}");
        assertProblem(HttpStatus.BAD_REQUEST, "invalid", "role_capacity", () -> controller.setRoleCapacity(ZONE, negative));
        verifyNoInteractions(repo);
    }

    @Test
    void roleCapacityReturnsNotFoundWhenRowAbsent() throws JsonProcessingException {
        // 加锁点查没命中(mock 缺省返回空列表):回 404,上限不写、也不插入。
        assertEquals(HttpStatus.NOT_FOUND, controller.setRoleCapacity(ZONE, roleCapacityBody("{\"role_capacity\":10}")).getStatusCode());
        verify(repo).lockZoneIdForUpdate(ZONE);
        verify(repo, never()).updateRoleCapacity(anyLong(), anyInt(), any());
        verify(repo, never()).insertIfAbsent(any(), any());
    }

    private static AdminZoneController.RoleCapacityRequest roleCapacityBody(String json) throws JsonProcessingException {
        return JSON.readValue(json, AdminZoneController.RoleCapacityRequest.class);
    }

    @Test
    void updatePathsDoNotRetryDeadlockClassifiedFailures() {
        // update 路径的第一把锁是加锁点查,之后的 UPDATE 改同一条已持锁的记录、不再等锁,不可能是死锁牺牲者;
        // 万一底层报了,也原样抛出,不做重试。
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
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

    /**
     * 控制器以 problem details 拒绝:核对状态、原因码与字段名(field 为 null 表示错误体里不该有这一项),返回错误体供进一步断言。
     * 控制器直接抛 {@link ErrorResponseException},由 AdminProblemAdvice 渲染;渲染出的 HTTP 形状见 AdminZoneHttpContractTest。
     */
    private static ProblemDetail assertProblem(HttpStatus status, String reason, String field, Executable call) {
        ErrorResponseException refused = assertThrows(ErrorResponseException.class, call);
        assertEquals(status, refused.getStatusCode());
        ProblemDetail problem = refused.getBody();
        assertEquals(status.value(), problem.getStatus());
        assertNotNull(problem.getProperties(), "错误体必须带原因码");
        assertEquals(reason, problem.getProperties().get("reason"));
        assertEquals(field, problem.getProperties().get("field"));
        assertNotNull(problem.getDetail(), "给人看的说明不能空");
        return problem;
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
            f.put("zone_id", zoneId);
        }
        f.put("name", "z");
        f.put("manual_status", 1);
        f.put("capacity", 8000);
        f.put("maintenance_msg", "14:00 恢复");
        f.put("open_time", null);
        f.put("recommended", true);
        f.put("sort_order", 3);
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
        Object id = fields.get("zone_id");
        ZoneConfig z = zone(id == null ? ZONE : ((Number) id).longValue(), (String) fields.get("name"));
        z.setManualStatus((Integer) fields.get("manual_status"));
        z.setCapacity((Integer) fields.get("capacity"));
        z.setMaintenanceMsg((String) fields.get("maintenance_msg"));
        Object openTime = fields.get("open_time");
        z.setOpenTime(openTime == null ? null : LocalDateTime.parse((String) openTime));
        z.setRecommended((Boolean) fields.get("recommended"));
        z.setSortOrder((Integer) fields.get("sort_order"));
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
