package com.game.gateway.controller;

import com.fasterxml.jackson.databind.JsonMappingException;
import com.game.gateway.dto.FieldViolation;
import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.http.ProblemDetail;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.HttpMessageNotReadableException;
import org.springframework.web.ErrorResponseException;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;

import java.util.List;
import java.util.Map;

/**
 * 把管理接口的错误渲染成 problem details(形状与原因码见 {@link AdminProblems},2026-11-12)。
 *
 * <p>只作用于两个管理控制器:全局打开 {@code spring.mvc.problemdetails.enabled} 会连带改掉玩家面 {@code /api/**} 与
 * 商城管理代理的错误体,前者是客户端契约,后者的原因文案是中文、给运维看的,都不在这次改动的范围里。
 */
@RestControllerAdvice(assignableTypes = {AdminZoneController.class, AdminWhitelistController.class})
class AdminProblemAdvice {

    private static final Logger log = LoggerFactory.getLogger(AdminProblemAdvice.class);

    /**
     * 控制器抛出的 400 / 412 / 428:原样回状态、头与错误体,同时记一行 WARN(路径里有区服号,错误体里只有字段名与原因码,
     * 都不含请求里的值)。
     */
    @ExceptionHandler(ErrorResponseException.class)
    ResponseEntity<ProblemDetail> problem(ErrorResponseException e, HttpServletRequest request) {
        ProblemDetail body = e.getBody();
        log.warn("admin request refused: method={} path={} status={} field={} reason={}", request.getMethod(),
                request.getRequestURI(), body.getStatus(), property(body, "field"), property(body, "reason"));
        return ResponseEntity.status(e.getStatusCode()).headers(e.getHeaders()).body(body);
    }

    /**
     * 请求体读不出来:不是合法 JSON,或某个字段的类型不对。Jackson 的报错原文带 Java 类名与解析位置,不回给调用方,
     * 只回能定位到的字段名和原因码。
     */
    @ExceptionHandler(HttpMessageNotReadableException.class)
    ResponseEntity<ProblemDetail> unreadable(HttpMessageNotReadableException e, HttpServletRequest request) {
        return problem(AdminProblems.badRequest(unreadableViolation(e)), request);
    }

    static FieldViolation unreadableViolation(HttpMessageNotReadableException e) {
        Throwable cause = e.getCause();
        if (cause == null) {
            // 没有底层解析异常:Spring 的必填请求体检查,请求体整个没给。
            return FieldViolation.missing(null, "request body is missing");
        }
        if (cause instanceof JsonMappingException mapping) {
            String field = lastFieldName(mapping.getPath());
            if (field != null) {
                return FieldViolation.invalid(field, field + " has the wrong type or format");
            }
        }
        return FieldViolation.invalid(null, "request body is not valid JSON");
    }

    private static Object property(ProblemDetail body, String name) {
        Map<String, Object> properties = body.getProperties();
        return properties == null ? null : properties.get(name);
    }

    /** 出错位置上最内层的 JSON 字段名(按线上 JSON 的 snake_case 写法);定位不到字段返回 null。 */
    private static String lastFieldName(List<JsonMappingException.Reference> path) {
        for (int i = path.size() - 1; i >= 0; i--) {
            String name = path.get(i).getFieldName();
            if (name != null) {
                return name;
            }
        }
        return null;
    }
}
