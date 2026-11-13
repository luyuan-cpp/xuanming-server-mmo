package com.game.gateway.dto;

/**
 * 请求校验查出的第一处问题(2026-11-12):出问题的字段、原因码、给人看的英文说明。管理接口把它原样放进 400 的错误体
 * (见 {@code com.game.gateway.controller.AdminProblems}),所以三项都只能描述<b>请求的形状</b>:字段名与取值约束,
 * 不含请求里的值、库里的值或任何内部细节。
 *
 * @param field  出问题的 JSON 字段名(snake_case,与线上 JSON 一致);问题在整个请求体(没给、不是合法 JSON)时为 null
 * @param reason 原因码,见 {@link Reason}
 * @param detail 英文说明,同时进运行日志
 */
public record FieldViolation(String field, Reason reason, String detail) {

    /** 原因码。{@link #code()} 是错误体里 {@code reason} 的取值,对外契约:只增不改。 */
    public enum Reason {
        /** 必填字段没给,或显式给了 null。field 为 null 时表示整个请求体没给。 */
        MISSING("missing"),
        /** 给了但不合法:越界、空白、未定义的状态码、带小数秒、类型不对;field 为 null 时表示请求体不是合法 JSON。 */
        INVALID("invalid"),
        /** 超过列宽(按字符计)。 */
        TOO_LONG("too_long"),
        /** 与路径不一致,例如请求体里的 zone_id 不是路径上的区服号。 */
        MISMATCH("mismatch");

        private final String code;

        Reason(String code) {
            this.code = code;
        }

        public String code() {
            return code;
        }
    }

    public static FieldViolation missing(String field, String detail) {
        return new FieldViolation(field, Reason.MISSING, detail);
    }

    public static FieldViolation invalid(String field, String detail) {
        return new FieldViolation(field, Reason.INVALID, detail);
    }

    public static FieldViolation tooLong(String field, String detail) {
        return new FieldViolation(field, Reason.TOO_LONG, detail);
    }

    public static FieldViolation mismatch(String field, String detail) {
        return new FieldViolation(field, Reason.MISMATCH, detail);
    }
}
