package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.util.Map;
import org.apache.shiro.authz.AuthorizationException;
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

class GmPlayerCommandControllerTest {
    final ObjectMapper json = new ObjectMapper();
    final Subject subject = mock(Subject.class);
    final GameServices game = mock(GameServices.class);
    final GmCommandJournal journal = mock(GmCommandJournal.class);
    GmPlayerCommandController controller;
    public static class Principal { public String getUsername() { return "gm-operator"; } }
    @BeforeEach void setup() {
        ThreadContext.bind(subject);
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.getPrincipal()).thenReturn(new Principal());
        when(game.playerCommandsEnabled()).thenReturn(true);
        var currencies = json.createArrayNode();
        currencies.addObject().put("id", 0); currencies.addObject().put("id", 1); currencies.addObject().put("id", 2);
        when(game.currencyTypes()).thenReturn(currencies);
        var player = json.createObjectNode().put("found", true).put("sessionFound", true);
        player.putObject("session").put("state", "SESSION_STATE_ONLINE");
        when(game.player(anyString())).thenReturn(player);
        when(journal.preparePlayerCommand(anyString(), anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "42"));
        controller = new GmPlayerCommandController(json, game, journal);
    }
    @AfterEach void cleanup() { ThreadContext.unbindSubject(); }
    ObjectNode draft(String operation, String arguments) throws Exception {
        ObjectNode input = json.createObjectNode().put("clientRequestId", "05ee173b-67ed-4aef-836a-61180b26869a")
                .put("playerId", "9007199254740993").put("reason", "客服工单").put("operation", operation);
        input.set("arguments", json.readTree(arguments));
        return input;
    }
    void http(int expected, Runnable action) {
        assertEquals(expected, assertThrows(ResponseStatusException.class, action::run).getStatusCode().value());
    }
    @Test void anonymousAndRevokedOperationPermissionNeverCreateOrSendCommands() throws Exception {
        String input = draft("currency.add", "{\"currencyType\":0,\"amount\":\"1\"}").toString();
        when(subject.isAuthenticated()).thenReturn(false);
        http(401, () -> controller.prepare(input));
        http(401, () -> controller.send("42"));
        when(subject.isAuthenticated()).thenReturn(true);
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:currency:add");
        assertThrows(UnauthorizedException.class, () -> controller.prepare(input));
        verifyNoInteractions(journal);
        verify(game, never()).player(anyString());
    }
    @Test void sevenOperationsRequireTheirOwnPermissionAndFreezeOnlyValidatedParameters() throws Exception {
        Map<String, String> cases = Map.of(
                "currency.add", "{\"currencyType\":1,\"amount\":\"9007199254740993\"}",
                "currency.deduct", "{\"currencyType\":2,\"amount\":\"1\"}",
                "currency.block", "{\"currencyType\":0}", "currency.unblock", "{\"currencyType\":1}",
                "player.level", "{\"level\":40}", "pet.grant", "{\"petTableId\":3}",
                "item.grant", "{\"configId\":12,\"count\":10}");
        for (var entry : cases.entrySet()) {
            controller.prepare(draft(entry.getKey(), entry.getValue()).toString());
            verify(subject).checkPermission("gm:" + entry.getKey().replace('.', ':'));
            verify(journal).preparePlayerCommand(eq("gm-operator"), anyString(), eq("9007199254740993"), eq("客服工单"),
                    argThat(request -> request.path("operation").asText().equals(entry.getKey())
                            && !request.has("operator") && !request.has("requestId")));
        }
    }
    @Test void forgedActorAddressUnsafeAmountAndUnknownCurrencyFailBeforeGameLookup() throws Exception {
        ObjectNode input = draft("currency.add", "{\"currencyType\":0,\"amount\":\"1\"}");
        String actor = input.deepCopy().put("operator", "admin").toString();
        http(400, () -> controller.prepare(actor));
        ObjectNode args = (ObjectNode) input.path("arguments");
        args.put("target", "attacker.example:1234");
        http(400, () -> controller.prepare(input.toString()));
        args.remove("target"); args.put("amount", 9007199254740993L);
        http(400, () -> controller.prepare(input.toString()));
        args.put("amount", "1").put("currencyType", 999);
        http(400, () -> controller.prepare(input.toString()));
        verify(game, never()).player(anyString()); verifyNoInteractions(journal);
    }
    @Test void levelCatalogAndDraftsMatchSceneWritableRange() throws Exception {
        when(subject.isPermitted("gm:player:level")).thenReturn(true);
        var limits = json.readTree(controller.catalog()).path("result").path("limits");
        assertEquals(85, limits.path("maxLevel").asInt());
        assertEquals(99, limits.path("maxItemCount").asInt());
        for (int level : new int[]{1, 85}) {
            controller.prepare(draft("player.level", "{\"level\":" + level + "}").toString());
            verify(journal).preparePlayerCommand(anyString(), anyString(), anyString(), anyString(),
                    argThat(request -> request.path("arguments").path("level").asInt() == level));
        }
        clearInvocations(game, journal);
        for (String level : new String[]{"0", "86", "2147483647", "85.5", "\"85\""}) {
            String input = draft("player.level", "{\"level\":" + level + "}").toString();
            http(400, () -> controller.prepare(input));
        }
        verify(game, never()).player(anyString());
        verifyNoInteractions(journal);
    }
    @Test void offlinePlayerAndMissingTransportCannotCreateDraft() throws Exception {
        String input = draft("pet.grant", "{\"petTableId\":3}").toString();
        when(game.playerCommandsEnabled()).thenReturn(false);
        http(503, () -> controller.prepare(input));
        when(game.playerCommandsEnabled()).thenReturn(true);
        when(game.player(anyString())).thenReturn(json.createObjectNode().put("found", true).put("sessionFound", false));
        http(400, () -> controller.prepare(input));
        verifyNoInteractions(journal);
    }
    @Test void expiredDraftCannotBeSentAndPermissionIsRecheckedAtSend() {
        when(subject.isPermitted("gm:pet:grant")).thenReturn(true);
        ObjectNode command = json.createObjectNode().put("id", "42").put("commandType", "PLAYER")
                .put("operation", "pet.grant").put("status", "DRAFT");
        command.putObject("request").put("issuedAtMs", Long.toString(System.currentTimeMillis() - 600_000));
        when(journal.get("42")).thenReturn(command);
        http(409, () -> controller.send("42"));
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:pet:grant");
        assertThrows(UnauthorizedException.class, () -> controller.send("42"));
        verify(journal, never()).sendPlayerCommand(anyString(), anyString(), any());
    }

    @Test void unprivilegedUserCannotProbeCommandIdBeforeAuthorization() {
        assertThrows(AuthorizationException.class, () -> controller.send("42"));
        verifyNoInteractions(journal);
    }

    @Test void auditPaginationAndTimelineRequireAuditPermission() {
        when(journal.listPlayerCommands(2, 25, "1001", "EXECUTED")).thenReturn(json.createObjectNode().put("total", 0));
        controller.list(2, 25, "1001", "EXECUTED");
        verify(subject).checkPermission("gm:audit:read");
        verify(journal).listPlayerCommands(2, 25, "1001", "EXECUTED");

        when(journal.auditHistory("42", "PLAYER")).thenReturn(json.createObjectNode().put("commandId", "42"));
        controller.audit("42");
        verify(journal).auditHistory("42", "PLAYER");
    }
}
