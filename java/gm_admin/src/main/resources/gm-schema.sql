CREATE TABLE IF NOT EXISTS gm_command (
    id VARCHAR(20) PRIMARY KEY,
    command_type VARCHAR(16) NOT NULL DEFAULT 'MAIL',
    operator_name VARBINARY(512) NOT NULL,
    client_request_id VARBINARY(128) NOT NULL,
    player_id VARCHAR(20) NOT NULL,
    reason TEXT NOT NULL,
    input_json LONGTEXT NOT NULL,
    request_json LONGTEXT NOT NULL,
    result_json LONGTEXT,
    status VARCHAR(16) NOT NULL,
    attempt BIGINT NOT NULL DEFAULT 0,
    created_ms BIGINT NOT NULL,
    updated_ms BIGINT NOT NULL,
    CONSTRAINT uq_gm_command_request UNIQUE (operator_name, client_request_id)
);

CREATE TABLE IF NOT EXISTS gm_command_audit (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    command_id VARCHAR(20) NOT NULL,
    operator_name VARBINARY(512) NOT NULL,
    event VARCHAR(16) NOT NULL,
    attempt BIGINT NOT NULL,
    result_json LONGTEXT,
    created_ms BIGINT NOT NULL,
    CONSTRAINT fk_gm_command_audit FOREIGN KEY (command_id) REFERENCES gm_command(id)
);
