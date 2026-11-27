package com.game.mall;

import java.util.regex.Pattern;

/**
 * 后台(java/gm_admin)双人复核的批准记录(docs/design/admin-identity-and-dual-control.md §5.4)。
 * 它在请求正文里,所以被 HMAC 签名覆盖。商城只做同一条规则的只读复查,口径与 go/shared/dualcontrol.Require 相同,不建公共库(设计 §5.5)。
 */
public record AdminApproval(String approvalId, String proposer, String approver, long approvedAtMs) {
    /** 与后台登录时对身份提供方主体的限制相同(GmOidcProvider.SUBJECT)。 */
    private static final Pattern SUBJECT = Pattern.compile("[A-Za-z0-9._@-]{1,64}");
    /** 后台流水号:63 位以内的十进制正整数文本。 */
    private static final Pattern APPROVAL_ID = Pattern.compile("[1-9][0-9]{0,18}");

    /** 带批准记录的管理写命令:请求号与批准记录。 */
    public interface Command {
        String requestId();
        AdminApproval approval();
    }

    /**
     * 合格返回 null,否则返回英文拒绝原因(只点出哪一项不合格,不回显内容)。
     * 商城请求里没有操作人字段,与 trade 的「发起人就是请求里的操作人」对应的是「请求号就是批准号」:一次批准只能执行一次。
     */
    static String refusal(Command command) {
        AdminApproval approval = command.approval();
        if (approval == null) return "dual-control approval record is missing";
        if (approval.approvalId() == null || !APPROVAL_ID.matcher(approval.approvalId()).matches())
            return "dual-control approval_id must be a positive decimal number";
        if (approval.proposer() == null || !SUBJECT.matcher(approval.proposer()).matches())
            return "dual-control proposer is not a valid operator identifier";
        if (approval.approver() == null || !SUBJECT.matcher(approval.approver()).matches())
            return "dual-control approver is not a valid operator identifier";
        if (approval.approvedAtMs() <= 0) return "dual-control approved_at_ms is missing";
        if (approval.proposer().equals(approval.approver())) return "dual-control proposer and approver are the same person";
        if (!approval.approvalId().equals(command.requestId())) return "dual-control requestId must be the approval id";
        return null;
    }
}
