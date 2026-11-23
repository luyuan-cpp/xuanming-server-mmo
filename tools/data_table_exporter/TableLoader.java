
package ;

import java.util.Set;
import java.util.concurrent.CountDownLatch;

/**
 * Auto-generated aggregated table loader.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TableLoader {

    public static final String SCHEMA_DIGEST = "db59930c8d215166";

    private static final Set<String> HOT_RELOADABLE_TABLES = Set.of(
            "Reward"
    );

    public static boolean isHotReloadable(String tableName) {
        return tableName != null && HOT_RELOADABLE_TABLES.contains(tableName);
    }

    private static Runnable loadSuccessCallback;

    /**
     * Load all config tables synchronously.
     * @param useBinary true to load .pb (proto binary), false to load .json.
     */
    public static void loadTables(String configDir, boolean useBinary) throws Exception {

        MissionTableManager.getInstance().load(configDir, useBinary);

        RewardTableManager.getInstance().load(configDir, useBinary);

        if (loadSuccessCallback != null) {
            loadSuccessCallback.run();
        }
    }

    /** Backward-compatible overload — defaults to JSON. */
    public static void loadTables(String configDir) throws Exception {
        loadTables(configDir, false);
    }

    /**
     * Load all config tables asynchronously using threads.
     * @param useBinary true to load .pb (proto binary), false to load .json.
     */
    public static void loadTablesAsync(String configDir, boolean useBinary) throws Exception {
        CountDownLatch latch = new CountDownLatch(2);

        new Thread(() -> {
            try {
                MissionTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Mission table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                RewardTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Reward table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        latch.await();

        if (loadSuccessCallback != null) {
            loadSuccessCallback.run();
        }
    }

    /**
     * Register a callback invoked after all tables load.
     */
    public static void onTablesLoadSuccess(Runnable callback) {
        loadSuccessCallback = callback;
    }

    /**
     * Reload all config tables (hot-reload safe).
     * @param useBinary true to load .pb (proto binary), false to load .json.
     */
    public static void reloadTables(String configDir, boolean useBinary) throws Exception {

        MissionTableManager.getInstance().load(configDir, useBinary);

        RewardTableManager.getInstance().load(configDir, useBinary);

        if (loadSuccessCallback != null) {
            loadSuccessCallback.run();
        }
    }

    /** Backward-compatible overload — defaults to JSON. */
    public static void reloadTables(String configDir) throws Exception {
        reloadTables(configDir, false);
    }
}