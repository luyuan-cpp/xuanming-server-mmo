package com.example.auth.config;

import com.game.authority.AuthorityDataSource;
import com.game.authority.WriterAuthority;
import com.zaxxer.hikari.HikariDataSource;
import java.util.*;
import org.springframework.beans.factory.config.BeanPostProcessor;
import org.springframework.context.annotation.*;
import org.springframework.core.env.Environment;

/** normal主体只消费一次boot资格；所有Hikari物理连接通过独立DataSource受管。 */
@Configuration(proxyBeanMethods=false)
public class AuthAuthorityConfiguration {
    @Bean static BeanPostProcessor normalSqlAuthority(Environment environment){
        return new org.springframework.beans.factory.config.DestructionAwareBeanPostProcessor(){
            private WriterAuthority boot;
            @Override public synchronized void postProcessBeforeDestruction(Object bean,String name){if(bean instanceof javax.sql.DataSource&&boot!=null)boot.seal();}
            @Override public synchronized Object postProcessBeforeInitialization(Object bean,String name){
                if(!(bean instanceof javax.sql.DataSource))return bean;
                if(!(bean instanceof HikariDataSource pool))throw new IllegalStateException("未知SQL池缺少physical受管接缝："+name);
                try{
                    if(pool.getHikariPoolMXBean()!=null||pool.getDataSource()!=null)throw new IllegalStateException("SQL池已启动或已有未知DataSource");
                    boolean required=environment.getProperty("mmorpg.sql.require-tidb",Boolean.class,true);
                    String mode=environment.getProperty("mmorpg.run-mode","prod");
                    if(!required&&!Set.of("dev","test").contains(mode))throw new IllegalStateException("生产不能关闭TiDB强会话核验");
                    if(boot==null)boot=WriterAuthority.consumeEnvironment();
                    Properties credentials=new Properties();credentials.putAll(pool.getDataSourceProperties());
                    if(pool.getUsername()!=null)credentials.setProperty("user",pool.getUsername());
                    if(pool.getPassword()!=null)credentials.setProperty("password",pool.getPassword());
                    String init=pool.getConnectionInitSql();
                    pool.setDataSource(new AuthorityDataSource(pool.getJdbcUrl(),credentials,boot,required,init==null?List.of():List.of(init)));
                    pool.setConnectionInitSql(null);
                    return bean;
                }catch(java.io.IOException|java.sql.SQLException failure){throw new IllegalStateException("SQL启动grant或physical封口失败",failure);}
            }
        };
    }
}
