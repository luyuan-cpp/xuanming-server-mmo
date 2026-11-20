package com.game.config.service;

import com.game.table.TableLoader;
import com.google.protobuf.MessageOrBuilder;
import com.google.protobuf.util.JsonFormat;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Service;

import java.lang.reflect.Method;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Manages config table lifecycle: initial load and hot-reload.
 */
@Service
public class TableService {

    private static final Logger log = LoggerFactory.getLogger(TableService.class);

    @Value("${config.table-dir:./config}")
    private String configDir;

    private final AtomicLong loadVersion = new AtomicLong(0);
    private volatile long lastLoadTimeMs;

    public void loadAll() {
        try {
            long start = System.currentTimeMillis();
            TableLoader.loadTables(configDir);
            lastLoadTimeMs = System.currentTimeMillis() - start;
            loadVersion.incrementAndGet();
            log.info("All tables loaded (version={}, took={}ms)", loadVersion.get(), lastLoadTimeMs);
        } catch (Exception e) {
            log.error("Failed to load tables", e);
            throw new RuntimeException("Table load failed", e);
        }
    }

    public void reload() {
        try {
            long start = System.currentTimeMillis();
            TableLoader.reloadTables(configDir);
            lastLoadTimeMs = System.currentTimeMillis() - start;
            loadVersion.incrementAndGet();
            log.info("Tables reloaded (version={}, took={}ms)", loadVersion.get(), lastLoadTimeMs);
        } catch (Exception e) {
            log.error("Failed to reload tables", e);
            throw new RuntimeException("Table reload failed", e);
        }
    }

    public long getLoadVersion() {
        return loadVersion.get();
    }

    public long getLastLoadTimeMs() {
        return lastLoadTimeMs;
    }

    /**
     * Look up a table manager by table name and query by ID.
     * Returns JSON representation or null.
     */
    public String queryById(String tableName, int id) {
        try {
            String className = "com.game.table." + tableName + "TableManager";
            Class<?> clazz = Class.forName(className);
            Method getInstance = clazz.getMethod("getInstance");
            Object manager = getInstance.invoke(null);
            Method getById = clazz.getMethod("getById", int.class);
            Object row = getById.invoke(manager, id);
            if (row == null) {
                return null;
            }
            return JsonFormat.printer().print((MessageOrBuilder) row);
        } catch (Exception e) {
            log.error("Query failed for table={} id={}", tableName, id, e);
            return null;
        }
    }
}
