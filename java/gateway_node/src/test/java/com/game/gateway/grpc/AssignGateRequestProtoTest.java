package com.game.gateway.grpc;

import com.google.protobuf.CodedInputStream;
import com.google.protobuf.WireFormat;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.util.HashMap;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

/**
 * 手写的 AssignGateRequest 编码与 proto/login/login.proto 对齐(2026-11-12):access_token 写字段 5;
 * 字段 3(曾是客户端自报的 account)已 reserved,任何输入都不写它。
 */
class AssignGateRequestProtoTest {

    @Test
    void writesAccessTokenAtFieldFiveAndNeverFieldThree() throws IOException {
        var req = new LoginRpcClient.AssignGateRequestProto(7, "queue-token", "access-token", "device-1");

        Map<Integer, Object> fields = decode(req.toBytes());

        assertEquals(7L, fields.get(1));
        assertEquals("queue-token", fields.get(2));
        assertFalse(fields.containsKey(3), "字段 3 已 reserved,不能再写");
        assertEquals("device-1", fields.get(4));
        assertEquals("access-token", fields.get(5));
    }

    @Test
    void omitsEmptyFields() throws IOException {
        var req = new LoginRpcClient.AssignGateRequestProto(0, null, null, null);

        assertEquals(0, req.toBytes().length, "全空的请求编码成空消息,与 proto3 缺省一致");
    }

    // 只解本消息用到的两种线类型(varint、length-delimited),字符串按 UTF-8 读。
    private static Map<Integer, Object> decode(byte[] bytes) throws IOException {
        Map<Integer, Object> out = new HashMap<>();
        CodedInputStream in = CodedInputStream.newInstance(bytes);
        while (!in.isAtEnd()) {
            int tag = in.readTag();
            int field = WireFormat.getTagFieldNumber(tag);
            switch (WireFormat.getTagWireType(tag)) {
                case WireFormat.WIRETYPE_VARINT -> out.put(field, in.readUInt64());
                case WireFormat.WIRETYPE_LENGTH_DELIMITED -> out.put(field, in.readStringRequireUtf8());
                default -> fail("unexpected wire type for field " + field);
            }
        }
        return out;
    }
}
