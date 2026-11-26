package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.util.Set;
import org.apache.shiro.authz.UnauthorizedException;
import org.apache.shiro.subject.Subject;
import org.apache.shiro.util.ThreadContext;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

class GmControllerTest {
    private final ObjectMapper json = new ObjectMapper();
    private final Subject subject = mock(Subject.class);
    private final GameServices game = mock(GameServices.class);
    private final GmCommandJournal journal = mock(GmCommandJournal.class);
    private GmController controller;
    public static final class Principal { public String getUsername() { return "gm-operator"; } }
    @BeforeEach void setup() {
        ThreadContext.bind(subject);
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.getPrincipal()).thenReturn(new Principal());
        var currencies = json.createArrayNode();
        currencies.addObject().put("id", 0).put("label", "CURRENCY_TYPE_GOLD");
        currencies.addObject().put("id", 1).put("label", "CURRENCY_TYPE_DIAMOND");
        currencies.addObject().put("id", 2).put("label", "CURRENCY_TYPE_BIND_DIAMOND");
        currencies.addObject().put("id", 3).put("label", "CURRENCY_TYPE_BIND_GOLD");
        when(game.currencyTypes()).thenReturn(currencies);
        when(game.supportsCurrency(anyInt())).thenAnswer(inv -> Set.of(0, 1, 2, 3).contains((Integer) inv.getArgument(0)));
        controller = new GmController(json, game, journal, "http://127.0.0.1:8081", "", "127.0.0.1:50900", "x".repeat(32));
    }
    @AfterEach void clear() { ThreadContext.unbindSubject(); }
    @Test void anonymousCannotReadOrWrite() {
        when(subject.isAuthenticated()).thenReturn(false);
        assertEquals(401, assertThrows(ResponseStatusException.class, () -> controller.player("123")).getStatusCode().value());
        assertEquals(401, assertThrows(ResponseStatusException.class, () -> controller.send("123")).getStatusCode().value());
        verifyNoInteractions(game, journal);
    }
    @Test void permissionIsCheckedBeforeGameCall() {
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:mail:send");
        assertThrows(UnauthorizedException.class, () -> controller.send("123"));
        verifyNoInteractions(game, journal);
    }
    @Test void frozenDraftTakesActorFromSessionAndKeepsIntegerPrecision() throws Exception {
        when(game.player(anyString())).thenReturn(json.createObjectNode().put("found", true));
        when(journal.prepare(anyString(), anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "42"));
        controller.prepare(draft());
        verify(journal).prepare(eq("gm-operator"), anyString(), eq("9223372036854775807"), eq("测试工单"), argThat(n ->
                n.path("attachments").path("currencies").get(0).path("amount").asText().equals("9223372036854775807")
                && !n.has("expireMs") && !n.has("requestId")));
    }
    @Test void mailDraftSupportsAllAuthoritativeCurrenciesAndRejectsUnknown() throws Exception {
        when(game.player(anyString())).thenReturn(json.createObjectNode().put("found", true));
        when(journal.prepare(anyString(), anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "42"));
        controller.prepare(draft().replace("\"currencyType\":0", "\"currencyType\":2"));
        verify(journal).prepare(eq("gm-operator"), anyString(), eq("9223372036854775807"), eq("测试工单"), argThat(n ->
                n.path("attachments").path("currencies").get(0).path("currencyType").asInt() == 2));
        clearInvocations(game, journal);
        assertEquals(400, assertThrows(ResponseStatusException.class,
                () -> controller.prepare(draft().replace("\"currencyType\":0", "\"currencyType\":99"))).getStatusCode().value());
        verify(game, never()).player(anyString());
        verifyNoInteractions(journal);
    }
    @Test void duplicateFieldsAndSpoofedActorAreRejected() {
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"title\":", "\"title\":\"x\",\"title\":")));
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"body\":", "\"operator\":\"admin\",\"body\":")));
        verifyNoInteractions(game, journal);
    }
    @Test void unsafeNumbersAndUnknownFieldsAreRejected() {
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"currencyAmount\":\"9223372036854775807\"", "\"currencyAmount\":9223372036854775807")));
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"validDays\":7", "\"validDays\":31")));
        verifyNoInteractions(game, journal);
    }
    @Test void absentOrWhitespaceMailConfigurationFailsClosed() {
        var disabled = new GmController(json, game, journal, "http://127.0.0.1:8081", "", "", "");
        assertEquals(503, assertThrows(ResponseStatusException.class, () -> disabled.prepare(draft())).getStatusCode().value());
        var blankSecret = new GmController(json, game, journal, "http://127.0.0.1:8081", "", "127.0.0.1:50900", " ".repeat(40));
        assertEquals(503, assertThrows(ResponseStatusException.class, () -> blankSecret.prepare(draft())).getStatusCode().value());
        verifyNoInteractions(game, journal);
    }
    @Test void overviewExposesCurrenciesAndAuditEndpointsCheckPermission() throws Exception {
        when(subject.isPermitted("gm:audit:read")).thenReturn(true);
        var overview = json.readTree(controller.overview()).path("result");
        assertEquals(3, overview.path("currencies").size());
        when(journal.list(2, 20, "1001", "DELIVERED")).thenReturn(json.createObjectNode().put("total", 0));
        controller.commands(2, 20, "1001", "DELIVERED");
        verify(journal).list(2, 20, "1001", "DELIVERED");
        when(journal.get("42", "MAIL")).thenReturn(json.createObjectNode().put("id", "42"));
        controller.command("42");
        verify(journal).get("42", "MAIL");
        when(journal.auditHistory("42", "MAIL")).thenReturn(json.createObjectNode().put("commandId", "42"));
        controller.commandAudit("42");
        verify(journal).auditHistory("42", "MAIL");
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:audit:read");
        assertThrows(UnauthorizedException.class, () -> controller.command("42"));
        assertThrows(UnauthorizedException.class, () -> controller.commandAudit("42"));
    }

    @Test void personalMailSupportsMultiAttachmentsAndRejectsDuplicatesOrMixing() throws Exception {
        when(game.player(anyString())).thenReturn(json.createObjectNode().put("found", true));
        when(journal.prepare(anyString(), anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "43"));
        String multi = """
          {"clientRequestId":"05ee173b-67ed-4aef-836a-61180b26869a","playerId":"1001",
           "title":"多附件补偿","body":"多种货币与道具","reason":"补偿工单","validDays":7,
           "currencies":[{"currencyType":0,"amount":"100"},{"currencyType":2,"amount":"500"}],
           "items":[{"configId":1001,"count":10},{"configId":1002,"count":20}]}
          """;
        controller.prepare(multi);
        verify(journal).prepare(eq("gm-operator"), anyString(), eq("1001"), eq("补偿工单"), argThat(n ->
                n.path("attachments").path("currencies").size() == 2
                && n.path("attachments").path("items").size() == 2));

        clearInvocations(game, journal);
        // 重复币种
        assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepare(
                multi.replace("{\"currencyType\":2,\"amount\":\"500\"}", "{\"currencyType\":0,\"amount\":\"500\"}"))).getStatusCode().value());
        // 物品总数超 4096
        assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepare(
                multi.replace("\"count\":10", "\"count\":4080"))).getStatusCode().value());
        // 混用数组与单字段
        assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepare(
                multi.replace("\"validDays\":7,", "\"validDays\":7,\"currencyAmount\":\"10\","))).getStatusCode().value());
        verifyNoInteractions(journal);
    }

    @Test void systemMailAndCampaignEndpointsEnforcePermissionsAndBuildManifest() throws Exception {
        String sysDraft = """
          {"clientRequestId":"05ee173b-67ed-4aef-836a-61180b26869a","title":"维护公告",
           "body":"今晚维护","reason":"例行维护","validDays":3}
          """;
        when(journal.prepareSystemMail(anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "50"));
        controller.prepareSystemMail(sysDraft);
        verify(journal).prepareSystemMail(eq("gm-operator"), anyString(), eq("例行维护"), argThat(n ->
                n.path("senderKind").asInt() == 1 && !n.has("attachments") && n.path("validDays").asInt() == 3));
        // 纯文本系统邮件不允许携带附件字段。
        assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepareSystemMail(
                sysDraft.replace("\"validDays\":3", "\"validDays\":3,\"currencies\":[]"))).getStatusCode().value());

        String campDraft = """
          {"clientRequestId":"15ee173b-67ed-4aef-836a-61180b26869b","title":"版本福利",
           "body":"感谢支持","reason":"版本更新补偿","validDays":7,
           "currencies":[{"currencyType":1,"amount":"200"}],
           "items":[{"configId":3001,"count":5}],
           "recipientPlayerIds":["20","5","20","100"]}
          """;
        when(journal.prepareMailCampaign(anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "51"));
        controller.prepareMailCampaign(campDraft);
        verify(journal).prepareMailCampaign(eq("gm-operator"), anyString(), eq("版本更新补偿"), argThat(n ->
                n.path("mail").path("expectedRecipientCount").asText().equals("3")
                && n.path("mail").path("expectedShardCount").asInt() == 1
                && n.path("mail").path("manifestHash").asText().length() == 64
                && n.path("shards").get(0).get(0).asText().equals("5")
                && n.path("shards").get(0).get(1).asText().equals("20")
                && n.path("shards").get(0).get(2).asText().equals("100")));

        // 活动发奖必须至少包含一种附件。
        assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepareMailCampaign("""
          {"clientRequestId":"15ee173b-67ed-4aef-836a-61180b26869b","title":"无附件活动",
           "body":"内容","reason":"测试","validDays":7,"recipientPlayerIds":["100"]}
          """)).getStatusCode().value());

        when(journal.sendSystemMail(eq("50"), eq("gm-operator"), any())).thenReturn(json.createObjectNode().put("id", "50"));
        controller.sendSystemMail("50");
        verify(journal).sendSystemMail(eq("50"), eq("gm-operator"), any());
        when(journal.sendMailCampaign(eq("51"), eq("gm-operator"), any())).thenReturn(json.createObjectNode().put("id", "51"));
        controller.sendMailCampaign("51");
        verify(journal).sendMailCampaign(eq("51"), eq("gm-operator"), any());

        when(journal.listSystemMails(1, 20, "DELIVERED")).thenReturn(json.createObjectNode().put("total", 0));
        controller.systemMailCommands(1, 20, "DELIVERED");
        verify(journal).listSystemMails(1, 20, "DELIVERED");
        when(journal.listMailCampaigns(1, 20, null)).thenReturn(json.createObjectNode().put("total", 0));
        controller.mailCampaignCommands(1, 20, null);
        verify(journal).listMailCampaigns(1, 20, null);
        when(journal.get("50", "SYSTEM_MAIL")).thenReturn(json.createObjectNode().put("id", "50"));
        controller.systemMailCommand("50");
        verify(journal).get("50", "SYSTEM_MAIL");
        when(journal.auditHistory("51", "MAIL_CAMPAIGN")).thenReturn(json.createObjectNode().put("commandId", "51"));
        controller.mailCampaignAudit("51");
        verify(journal).auditHistory("51", "MAIL_CAMPAIGN");

        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:system-mail:prepare");
        assertThrows(UnauthorizedException.class, () -> controller.prepareSystemMail(sysDraft));
        assertThrows(UnauthorizedException.class, () -> controller.prepareMailCampaign(campDraft));
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:system-mail:send");
        assertThrows(UnauthorizedException.class, () -> controller.sendSystemMail("50"));
        assertThrows(UnauthorizedException.class, () -> controller.sendMailCampaign("51"));
    }
    private String draft() {
        return """
          {"clientRequestId":"05ee173b-67ed-4aef-836a-61180b26869a","playerId":"9223372036854775807",
           "title":"测试发放","body":"领取后到账","reason":"测试工单","currencyType":0,
           "currencyAmount":"9223372036854775807","validDays":7}
          """;
    }
}
