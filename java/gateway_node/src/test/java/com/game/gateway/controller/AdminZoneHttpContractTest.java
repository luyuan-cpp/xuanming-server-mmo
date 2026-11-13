package com.game.gateway.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.PropertyNamingStrategies;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpHeaders;
import org.springframework.http.MediaType;
import org.springframework.http.converter.json.Jackson2ObjectMapperBuilder;
import org.springframework.http.converter.json.MappingJackson2HttpMessageConverter;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import org.springframework.transaction.support.TransactionOperations;

import java.time.LocalDateTime;
import java.util.List;
import java.util.Optional;

import static org.hamcrest.Matchers.containsString;
import static org.hamcrest.Matchers.not;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.put;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.content;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.header;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/**
 * 区服运维接口的 HTTP 契约(2026-11-12,数据审计遗留):ETag / If-Match / If-None-Match 三个头,以及 400 / 412 / 428 的
 * problem details 错误体,在 MockMvc 上走一遍真实的参数绑定、异常处理与 JSON 渲染(Jackson 与线上同一种配置:
 * Spring 的构建器加 SNAKE_CASE)。控制器自身的分支由 {@link AdminZoneControllerTest} 验,语句与库语义由
 * {@code ZoneConfigRepositoryWiringTest} 验;这里只钉调用方在线上看到的形状。
 *
 * <p>仓库是 mock(缺省:加锁点查回空列表 = 行不在,条件 UPDATE 回 0 = 版本不符),事务直通;
 * AdminApiKeyFilter 不在这里。
 */
class AdminZoneHttpContractTest {

    private static final long ZONE = 7L;
    private static final long VERSION = 41L;
    private static final String ETAG = "\"41\"";
    private static final LocalDateTime NOW = LocalDateTime.of(2026, 11, 12, 10, 0, 0);
    /** 一份完整且合法的整行请求体(线上 JSON 名)。 */
    private static final String COMPLETE_BODY = "{\"name\":\"z\",\"manual_status\":1,\"capacity\":8000,"
            + "\"maintenance_msg\":\"\",\"open_time\":null,\"recommended\":true,\"sort_order\":3}";

    private ZoneConfigRepository repo;
    private MockMvc mvc;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneConfigRepository.class);
        ObjectMapper json = Jackson2ObjectMapperBuilder.json()
                .propertyNamingStrategy(PropertyNamingStrategies.SNAKE_CASE).build();
        AdminZoneController controller =
                new AdminZoneController(repo, TransactionOperations.withoutTransaction(), () -> NOW, () -> 9_001L);
        mvc = MockMvcBuilders.standaloneSetup(controller)
                .setControllerAdvice(new AdminProblemAdvice())
                .setMessageConverters(new MappingJackson2HttpMessageConverter(json))
                .build();
    }

    @Test
    void getAnswersStrongEtagAndNotModifiedWhileTheVersionStays() throws Exception {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(VERSION)));

        mvc.perform(get("/admin/zones/{id}", ZONE))
                .andExpect(status().isOk())
                .andExpect(header().string(HttpHeaders.ETAG, ETAG))
                .andExpect(jsonPath("$.name").value("z"))
                .andExpect(jsonPath("$.version").doesNotExist());
        // 版本没变 = 表示没变:带上次的 ETag 条件 GET 回 304;版本对不上照常 200。
        mvc.perform(get("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_NONE_MATCH, ETAG))
                .andExpect(status().isNotModified());
        mvc.perform(get("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_NONE_MATCH, "\"40\""))
                .andExpect(status().isOk());
    }

    @Test
    void putWithoutIfMatchIs428ProblemAndNeverTouchesTheDatabase() throws Exception {
        mvc.perform(put("/admin/zones/{id}", ZONE).contentType(MediaType.APPLICATION_JSON).content(COMPLETE_BODY))
                .andExpect(status().isPreconditionRequired())
                .andExpect(content().contentTypeCompatibleWith(MediaType.APPLICATION_PROBLEM_JSON))
                .andExpect(jsonPath("$.status").value(428))
                .andExpect(jsonPath("$.reason").value("if_match_required"))
                .andExpect(jsonPath("$.field").value("If-Match"))
                .andExpect(jsonPath("$.detail").isNotEmpty());
        verifyNoInteractions(repo);
    }

    @Test
    void putWithStaleIfMatchIs412ProblemAndWritesNothing() throws Exception {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));

        mvc.perform(put("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_MATCH, "\"40\"")
                        .contentType(MediaType.APPLICATION_JSON).content(COMPLETE_BODY))
                .andExpect(status().isPreconditionFailed())
                .andExpect(content().contentTypeCompatibleWith(MediaType.APPLICATION_PROBLEM_JSON))
                .andExpect(jsonPath("$.status").value(412))
                .andExpect(jsonPath("$.reason").value("version_mismatch"))
                .andExpect(jsonPath("$.field").value("If-Match"));
        verify(repo).updateSettings(eq(ZONE), any(), eq(40L), eq(NOW));
        verify(repo, never()).findById(anyLong());
    }

    @Test
    void putWithCurrentIfMatchWritesAndAnswersTheStoredRowWithoutAValidator() throws Exception {
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.updateSettings(eq(ZONE), any(), eq(VERSION), eq(NOW))).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(VERSION + 1)));

        mvc.perform(put("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_MATCH, ETAG)
                        .contentType(MediaType.APPLICATION_JSON).content(COMPLETE_BODY))
                .andExpect(status().isOk())
                .andExpect(header().doesNotExist(HttpHeaders.ETAG))
                .andExpect(jsonPath("$.name").value("z"));
    }

    @Test
    void putToAbsentZoneIs404EvenWithIfMatch() throws Exception {
        mvc.perform(put("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_MATCH, ETAG)
                        .contentType(MediaType.APPLICATION_JSON).content(COMPLETE_BODY))
                .andExpect(status().isNotFound());
        verify(repo, never()).updateSettings(any(), any(), anyLong(), any());
    }

    @Test
    void invalidBodyIs400ProblemNamingTheFieldWithoutEchoingTheValue() throws Exception {
        String negativeCapacity = COMPLETE_BODY.replace("\"capacity\":8000", "\"capacity\":-987654");

        mvc.perform(put("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_MATCH, ETAG)
                        .contentType(MediaType.APPLICATION_JSON).content(negativeCapacity))
                .andExpect(status().isBadRequest())
                .andExpect(content().contentTypeCompatibleWith(MediaType.APPLICATION_PROBLEM_JSON))
                .andExpect(jsonPath("$.status").value(400))
                .andExpect(jsonPath("$.reason").value("invalid"))
                .andExpect(jsonPath("$.field").value("capacity"))
                .andExpect(content().string(not(containsString("987654"))));
        verifyNoInteractions(repo);
    }

    @Test
    void unreadableBodyIs400ProblemWithoutParserDetails() throws Exception {
        // 字段类型不对:点名字段;不是合法 JSON:不点字段。两种都不带 Jackson 的报错原文(Java 类名、解析位置)和请求里的值。
        String wrongType = COMPLETE_BODY.replace("\"capacity\":8000", "\"capacity\":\"lots\"");
        mvc.perform(put("/admin/zones/{id}", ZONE).header(HttpHeaders.IF_MATCH, ETAG)
                        .contentType(MediaType.APPLICATION_JSON).content(wrongType))
                .andExpect(status().isBadRequest())
                .andExpect(content().contentTypeCompatibleWith(MediaType.APPLICATION_PROBLEM_JSON))
                .andExpect(jsonPath("$.reason").value("invalid"))
                .andExpect(jsonPath("$.field").value("capacity"))
                .andExpect(content().string(not(containsString("java."))))
                .andExpect(content().string(not(containsString("lots"))));
        mvc.perform(post("/admin/zones").contentType(MediaType.APPLICATION_JSON).content("{\"zone_id\": 7,"))
                .andExpect(status().isBadRequest())
                .andExpect(jsonPath("$.reason").value("invalid"))
                .andExpect(jsonPath("$.field").doesNotExist())
                .andExpect(content().string(not(containsString("java."))));
        verifyNoInteractions(repo);
    }

    @Test
    void missingBodyIs400ProblemWithoutField() throws Exception {
        mvc.perform(post("/admin/zones").contentType(MediaType.APPLICATION_JSON))
                .andExpect(status().isBadRequest())
                .andExpect(jsonPath("$.reason").value("missing"))
                .andExpect(jsonPath("$.field").doesNotExist());
        verifyNoInteractions(repo);
    }

    @Test
    void partialCommandsDoNotEvaluateIfMatch() throws Exception {
        // 命令写的是目标值,不回写读到的旧整行,不要求也不看 If-Match(理由见 AdminZoneController 类注释):带一个早就过期的
        // 标签照样执行。
        when(repo.lockZoneIdForUpdate(ZONE)).thenReturn(List.of(ZONE));
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(VERSION + 1)));

        mvc.perform(post("/admin/zones/{id}/maintenance", ZONE).header(HttpHeaders.IF_MATCH, "\"1\"")
                        .contentType(MediaType.APPLICATION_JSON).content("{\"maintenance_msg\":\"m\"}"))
                .andExpect(status().isOk());
        verify(repo).updateManualStatus(ZONE, 1, "m", NOW);
    }

    private static ZoneConfig zone(long version) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(ZONE);
        z.setName("z");
        z.setVersion(version);
        return z;
    }
}
