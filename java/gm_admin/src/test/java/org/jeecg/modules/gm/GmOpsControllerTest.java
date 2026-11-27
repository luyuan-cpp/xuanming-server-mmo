package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.apache.shiro.authz.UnauthorizedException;
import org.apache.shiro.subject.Subject;
import org.apache.shiro.util.ThreadContext;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

/** 运营处置的目录是唯一白名单:发起人只能填目录里的字段,操作人、原因、冻结时刻由服务端填,能否重试随申请冻结。 */
class GmOpsControllerTest {
    private static final String UUID = "123e4567-e89b-42d3-a456-426614174000";
    private final ObjectMapper json = new ObjectMapper();
    private final Subject subject = mock(Subject.class);
    private final GmCommandJournal journal = mock(GmCommandJournal.class);
    private final GameMallAdmin mall = mock(GameMallAdmin.class);
    private final GameOpsRpc rpc = mock(GameOpsRpc.class);
    private GmOpsController controller;

    public static final class Principal { public String getUsername() { return "alice"; } }

    @BeforeEach void setup() {
        ThreadContext.bind(subject);
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.getPrincipal()).thenReturn(new Principal());
        when(journal.prepareOps(anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "77"));
        controller = new GmOpsController(json, journal, mall, rpc);
    }

    @AfterEach void clear() { ThreadContext.unbindSubject(); }

    private ObjectNode frozenDraft() {
        ArgumentCaptor<ObjectNode> frozen = ArgumentCaptor.forClass(ObjectNode.class);
        verify(journal).prepareOps(eq("alice"), eq(UUID), eq("工单 9"), frozen.capture());
        return frozen.getValue();
    }

    @Test void serverFieldsAreFilledFromTheSessionAndTheRetryabilityIsFrozen() throws Exception {
        controller.prepare("{\"clientRequestId\":\"" + UUID + "\",\"target\":\"inbox\",\"command\":\"GmSanctionPlayer\",\"reason\":\"工单 9\","
                + "\"body\":{\"playerId\":\"42\",\"blockGain\":{}}}");
        ObjectNode frozen = frozenDraft();
        assertEquals("inbox", frozen.path("target").asText());
        assertTrue(frozen.path("retryable").asBoolean());
        assertEquals("alice", frozen.path("body").path("operator").asText());
        assertEquals("工单 9", frozen.path("body").path("reason").asText());
        assertTrue(frozen.path("body").path("issuedAtMs").asText().matches("[1-9][0-9]+"), "冻结时刻随申请冻结,重试时摘要不变");
        verify(subject).checkPermission("gm:inbox:prepare");
    }

    @Test void commandsWhoseReplayIsRefusedAreFrozenAsNotRetryable() {
        controller.prepare("{\"clientRequestId\":\"" + UUID + "\",\"target\":\"match\",\"command\":\"ResolveStrandedFormation\",\"reason\":\"工单 9\","
                + "\"body\":{\"battleId\":\"5\",\"expectDigest\":\"ab\",\"expectTarget\":\"staging\"}}");
        ObjectNode frozen = frozenDraft();
        assertFalse(frozen.path("retryable").asBoolean());
        assertEquals("STRANDED_FORMATION_MODE_ABORT", frozen.path("body").path("mode").asText());
        assertEquals("alice", frozen.path("body").path("operatorName").asText());
    }

    @Test void theProposerCannotFillServerFieldsOrFieldsOutsideTheCatalog() {
        for (String body : new String[] {
                "{\"playerId\":\"42\",\"operator\":\"bob\"}",
                "{\"playerId\":\"42\",\"reason\":\"x\"}",
                "{\"playerId\":\"42\",\"requestId\":\"1\"}",
                "{\"playerId\":\"42\",\"approval\":{}}"}) {
            String draft = "{\"clientRequestId\":\"" + UUID + "\",\"target\":\"inbox\",\"command\":\"GmSanctionPlayer\",\"reason\":\"工单 9\",\"body\":" + body + "}";
            assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepare(draft)).getStatusCode().value(), body);
        }
        String unknown = "{\"clientRequestId\":\"" + UUID + "\",\"target\":\"inbox\",\"command\":\"Purge\",\"reason\":\"工单 9\",\"body\":{}}";
        assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.prepare(unknown)).getStatusCode().value());
        verify(journal, never()).prepareOps(anyString(), anyString(), anyString(), any());
    }

    @Test void permissionsArePerTargetAndPreviewNeedsOnlyRead() {
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:rank:prepare");
        assertThrows(UnauthorizedException.class, () -> controller.prepare("{\"clientRequestId\":\"" + UUID
                + "\",\"target\":\"rank\",\"command\":\"RemoveRankEntry\",\"reason\":\"工单 9\",\"body\":{\"boardId\":\"1\",\"generation\":\"2\",\"entityId\":\"3\"}}"));
        when(rpc.preview(any(), any())).thenReturn(json.createObjectNode().put("planDigest", "aa"));
        controller.preview("{\"target\":\"recall\",\"command\":\"BatchRecallItems\",\"body\":{\"playerIds\":[\"42\"]}}");
        ArgumentCaptor<ObjectNode> sent = ArgumentCaptor.forClass(ObjectNode.class);
        verify(rpc).preview(eq(GmOpsController.COMMANDS.get("recall/BatchRecallItems")), sent.capture());
        assertTrue(sent.getValue().path("dryRun").asBoolean(), "预览覆盖成演练");
        verify(subject).checkPermission("gm:recall:read");
        // 没有预览的命令不能预览。
        assertEquals(400, assertThrows(ResponseStatusException.class,
                () -> controller.preview("{\"target\":\"rank\",\"command\":\"RemoveRankEntry\",\"body\":{}}")).getStatusCode().value());
        verify(journal, never()).prepareOps(anyString(), anyString(), anyString(), any());
    }

    @Test void sendingDispatchesTheFrozenCommandToItsOwnTarget() {
        ObjectNode command = json.createObjectNode().put("id", "77");
        command.putObject("request").put("target", "rank").put("command", "RemoveRankEntry");
        when(journal.get("77", "OPS")).thenReturn(command);
        when(rpc.enabled("rank")).thenReturn(true);
        when(journal.sendOps(eq("77"), eq("alice"), any())).thenReturn(command);
        controller.send("77");
        verify(subject).checkPermission("gm:rank:approve");
        // 目录里没有的命令(下线了的)一律拒绝,不发。
        ObjectNode retired = json.createObjectNode().put("id", "78");
        retired.putObject("request").put("target", "rank").put("command", "Purge");
        when(journal.get("78", "OPS")).thenReturn(retired);
        assertEquals(409, assertThrows(ResponseStatusException.class, () -> controller.send("78")).getStatusCode().value());
        verify(journal, never()).sendOps(eq("78"), anyString(), any());
    }
}
