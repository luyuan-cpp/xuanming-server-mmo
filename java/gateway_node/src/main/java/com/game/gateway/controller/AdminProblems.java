package com.game.gateway.controller;

import com.game.gateway.dto.FieldViolation;
import org.springframework.http.HttpHeaders;
import org.springframework.http.HttpStatus;
import org.springframework.http.ProblemDetail;
import org.springframework.web.ErrorResponseException;

/**
 * 管理接口({@code /admin/zones}、{@code /admin/whitelist})的错误体(2026-11-12)。之前 400 一律不带响应体,原因只在
 * 网关日志里,调用方只能去翻日志。
 *
 * <h2>形状</h2>
 * RFC 9457 problem details,用 Spring 自带的 {@link ProblemDetail},Content-Type 为 {@code application/problem+json}
 * (由 {@link AdminProblemAdvice} 渲染)。标准成员 {@code type}(about:blank)、{@code title}(状态短语)、{@code status}、
 * {@code detail}、{@code instance}(请求路径),外加两个扩展成员:
 * <ul>
 *   <li>{@code reason}:原因码,小写下划线,对外契约,只增不改。调用方按它分支,不要解析 detail。</li>
 *   <li>{@code field}:出问题的 JSON 字段名(snake_case),或请求头名 {@code If-Match};问题在整个请求体时不出现。</li>
 * </ul>
 * detail 是英文说明,只描述请求的形状(字段名与取值约束),不含请求里的值、库里的值、异常类名、SQL 或堆栈。
 *
 * <h2>原因码</h2>
 * <table>
 *   <caption>reason 的取值</caption>
 *   <tr><th>reason</th><th>状态</th><th>含义</th></tr>
 *   <tr><td>missing</td><td>400</td><td>必填字段没给或为 null;没有 field 时是整个请求体没给</td></tr>
 *   <tr><td>invalid</td><td>400</td><td>取值不合法(越界、空白、未定义的状态码、带小数秒、类型不对);没有 field 时是请求体不是合法 JSON</td></tr>
 *   <tr><td>too_long</td><td>400</td><td>超过列宽</td></tr>
 *   <tr><td>mismatch</td><td>400</td><td>请求体里的 zone_id 与路径不一致</td></tr>
 *   <tr><td>if_match_invalid</td><td>400</td><td>If-Match 不是一个实体标签,或带了不止一个</td></tr>
 *   <tr><td>if_match_required</td><td>428</td><td>整行 PUT 没带 If-Match,或带的是 {@code *}</td></tr>
 *   <tr><td>version_mismatch</td><td>412</td><td>If-Match 不是这一行的当前版本:GET 之后有人改过,重新 GET 再改</td></tr>
 * </table>
 * 前四个就是 {@link FieldViolation.Reason}。404(区服不在)与 409(create 撞上内容不同的已有区服,响应体是库里那一行)
 * 保持原样,不走这里。
 */
final class AdminProblems {

    static final String IF_MATCH_REQUIRED = "if_match_required";
    static final String IF_MATCH_INVALID = "if_match_invalid";
    static final String VERSION_MISMATCH = "version_mismatch";

    private AdminProblems() {
    }

    /** 请求体或请求参数不合法:400。 */
    static ErrorResponseException badRequest(FieldViolation violation) {
        return problem(HttpStatus.BAD_REQUEST, violation.field(), violation.reason().code(), violation.detail());
    }

    /** 整行 PUT 没带可用的 If-Match:428(RFC 6585 §3)。 */
    static ErrorResponseException ifMatchRequired(String detail) {
        return problem(HttpStatus.PRECONDITION_REQUIRED, HttpHeaders.IF_MATCH, IF_MATCH_REQUIRED, detail);
    }

    /** If-Match 的写法不对:400。 */
    static ErrorResponseException ifMatchInvalid(String detail) {
        return problem(HttpStatus.BAD_REQUEST, HttpHeaders.IF_MATCH, IF_MATCH_INVALID, detail);
    }

    /** If-Match 与当前版本不符,什么都没写:412(RFC 9110 §15.5.13)。 */
    static ErrorResponseException versionMismatch() {
        return problem(HttpStatus.PRECONDITION_FAILED, HttpHeaders.IF_MATCH, VERSION_MISMATCH,
                "If-Match is not the current version of this zone; GET it again, re-apply the change and retry");
    }

    private static ErrorResponseException problem(HttpStatus status, String field, String reason, String detail) {
        ProblemDetail body = ProblemDetail.forStatusAndDetail(status, detail);
        body.setProperty("reason", reason);
        if (field != null) {
            body.setProperty("field", field);
        }
        return new ErrorResponseException(status, body, null);
    }
}
