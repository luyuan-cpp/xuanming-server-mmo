-- 只供独立schema主体在fresh-prestart执行；normal JPA只validate，维护run内不做DDL。
CREATE TABLE users(id BIGINT NOT NULL AUTO_INCREMENT,create_time DATETIME(6),last_login DATETIME(6),is_guest BIT,avatar VARCHAR(255),nickname VARCHAR(255),PRIMARY KEY(id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE user_oauth(id BIGINT NOT NULL AUTO_INCREMENT,user_id BIGINT,provider VARCHAR(255),open_id VARCHAR(255),PRIMARY KEY(id),UNIQUE KEY uq_provider_open_id(provider,open_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE user_password(id BIGINT NOT NULL AUTO_INCREMENT,user_id BIGINT,password_hash VARCHAR(255),PRIMARY KEY(id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
