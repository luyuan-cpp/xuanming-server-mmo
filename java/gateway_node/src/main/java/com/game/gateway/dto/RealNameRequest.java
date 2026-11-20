package com.game.gateway.dto;

/**
 * POST /api/real-name 的请求体(docs/design/real-name-anti-addiction.md §5.2)。JSON:access_token、name、id_number、zone_id。
 * 姓名和身份证号只转给 login 核验:网关不记日志,toString 不带它们。
 */
public class RealNameRequest {
    private String accessToken;
    private String name;
    private String idNumber;
    /** /api/login 时选的区,用来选 login 实例(与 /api/login 同一套选路)。 */
    private long zoneId;

    public String getAccessToken() { return accessToken; }
    public void setAccessToken(String accessToken) { this.accessToken = accessToken; }
    public String getName() { return name; }
    public void setName(String name) { this.name = name; }
    public String getIdNumber() { return idNumber; }
    public void setIdNumber(String idNumber) { this.idNumber = idNumber; }
    public long getZoneId() { return zoneId; }
    public void setZoneId(long zoneId) { this.zoneId = zoneId; }

    @Override
    public String toString() {
        return "RealNameRequest[zoneId=" + zoneId + ", redacted]";
    }
}
