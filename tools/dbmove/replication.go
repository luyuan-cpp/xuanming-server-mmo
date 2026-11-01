package main

// 复制进度。工具不起复制、不停复制(手册 §3),只读它走到了哪里。
//
//	-sync=ticdc:<openapi地址>:<changefeed>   TiDB 档:比 changefeed 的 checkpoint 和源库的 TSO
//	-sync=mysql-gtid[:<channel>]             MySQL 档:WAIT_FOR_EXECUTED_GTID_SET,channel 缺省 dbmove

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shared/authoritysql"
	"shared/dbplacement"
)

// errReplicationBroken:复制断了,等也等不来。切换窗口看到它立刻回滚,并清掉 verify 记录:修好之后这段时间的写有没有丢,只有重新校验才知道。
var errReplicationBroken = errors.New("replication is broken")

var errLagTooHigh = errors.New("replication lag is above the limit")

// verifyPoint 是 verify 用的一对位置。Source、Target 是 TiDB 的快照 TSO,MySQL 档为空(副本已经停在那里)。
type verifyPoint struct {
	Source, Target string
	Detail         string
	// Feed 是这对位置所属的复制流。
	Feed dbplacement.FeedIdentity
	// PrimaryTs、AtMs 是 TiCDC syncpoint 的 primary_ts 和它的物理时间。MySQL 档为 0,新鲜度从 verify 开始算。
	PrimaryTs uint64
	AtMs      uint64
}

type replication interface {
	// CheckLag 核复制在跑、延迟低于 maxLag。
	CheckLag(ctx context.Context, maxLag time.Duration) error
	// Mark 取冻结点。必须在写完哨兵之后调:冻结点要盖住哨兵。
	Mark(ctx context.Context) (string, error)
	// CaughtUp 报告目标是否追上了 mark。复制断开回 errReplicationBroken。
	CaughtUp(ctx context.Context, mark string) (bool, error)
	// TargetPoint 取目标当前的位置,audit 在这个位置上比。必须在打开目标闸门之前调。
	TargetPoint(ctx context.Context) (string, error)
	// VerifyPoint 找一对内容相同的位置。
	VerifyPoint(ctx context.Context) (verifyPoint, error)
	// RecheckVerifyPoint 校验和算完之后再核一次位置没动。
	RecheckVerifyPoint(ctx context.Context, point verifyPoint) error
	// Identity 读当前复制流的身份。verify 记下,cutover 比:流重建过,延迟回到 0 也可能缺一段写。
	Identity(ctx context.Context) (dbplacement.FeedIdentity, error)
}

const defaultGTIDChannel = "dbmove"

// verifySyncPointIntervals:syncpoint 落后这么多个间隔就不能拿来 verify。
// 落后太多说明 changefeed 卡住了或者 sync point 停了,读到的是旧 changefeed 或很久以前的位置,比过了也说明不了现在。
const verifySyncPointIntervals = 4

var (
	changefeedRe = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)
	channelRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type syncSpec struct {
	backend    authoritysql.Backend
	api        string // TiCDC OpenAPI 基址
	changefeed string
	channel    string // MySQL 复制通道
}

// parseSync 解析 -sync。地址里本身有冒号,changefeed 是最后一个冒号之后的部分(changefeed id 不含冒号)。
func parseSync(raw string) (syncSpec, error) {
	if raw == "mysql-gtid" {
		return syncSpec{backend: authoritysql.BackendMySQL, channel: defaultGTIDChannel}, nil
	}
	if channel, ok := strings.CutPrefix(raw, "mysql-gtid:"); ok {
		if !channelRe.MatchString(channel) {
			return syncSpec{}, fmt.Errorf("bad replication channel %q in -sync", channel)
		}
		return syncSpec{backend: authoritysql.BackendMySQL, channel: channel}, nil
	}
	rest, ok := strings.CutPrefix(raw, "ticdc:")
	if !ok {
		return syncSpec{}, errors.New("-sync must be ticdc:<openapi address>:<changefeed> or mysql-gtid[:<channel>]")
	}
	cut := strings.LastIndex(rest, ":")
	if cut <= 0 {
		return syncSpec{}, errors.New("-sync ticdc needs both the OpenAPI address and the changefeed")
	}
	api, changefeed := rest[:cut], rest[cut+1:]
	if !changefeedRe.MatchString(changefeed) {
		return syncSpec{}, fmt.Errorf("bad changefeed id %q in -sync", changefeed)
	}
	if !strings.Contains(api, "://") {
		api = "http://" + api
	}
	parsed, err := url.Parse(api)
	if err != nil || parsed.Host == "" {
		return syncSpec{}, errors.New("bad OpenAPI address in -sync")
	}
	return syncSpec{backend: authoritysql.BackendTiDB, api: strings.TrimRight(api, "/"), changefeed: changefeed}, nil
}

// tsoPhysicalMs 取 TSO 的物理时间(毫秒)。低 18 位是逻辑计数。
func tsoPhysicalMs(tso uint64) int64 { return int64(tso >> 18) }

// readTiDBTSO 在一个显式事务里读 TSO:自动提交下这个变量是 0。事务开始的时间戳一定晚于之前已经提交的事务。
func readTiDBTSO(ctx context.Context, db *sql.DB) (uint64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var tso uint64
	if err := tx.QueryRowContext(ctx, "SELECT @@tidb_current_ts").Scan(&tso); err != nil {
		return 0, err
	}
	if tso == 0 {
		return 0, errors.New("TiDB returned TSO 0")
	}
	return tso, nil
}

// ticdcReplication 实现 replication,TiDB 档。source、target 是只读的原始连接。
type ticdcReplication struct {
	api, changefeed string
	client          *http.Client
	source, target  *sql.DB
	schema          string
}

// readTargetCreated 取目标库闸门表的建表时间,见 dbplacement.FeedIdentity。
func readTargetCreated(ctx context.Context, target *sql.DB, schema string) (string, error) {
	var created sql.NullString
	err := target.QueryRowContext(ctx, "SELECT CAST(CREATE_TIME AS CHAR) FROM information_schema.tables WHERE table_schema=? AND table_name='route_meta'", schema).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return "", refusedf("the target has no route_meta in %s yet: import the full copy and start replication first", schema)
	}
	if err != nil {
		return "", fmt.Errorf("read the creation time of route_meta on the target: %w", err)
	}
	if created.String == "" {
		return "", refusedf("the target reports no creation time for route_meta in %s: cannot tell whether it was rebuilt", schema)
	}
	return created.String, nil
}

type changefeedDetail struct {
	State        string `json:"state"`
	CheckpointTs uint64 `json:"checkpoint_ts"`
	StartTs      uint64 `json:"start_ts"`
	CreateTime   string `json:"create_time"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`
	Config *struct {
		EnableSyncPoint *bool `json:"enable_sync_point"`
		// 版本之间形状不同:新版是 "30s" 这样的字符串,旧版是纳秒整数。
		SyncPointInterval json.RawMessage `json:"sync_point_interval"`
	} `json:"config"`
}

// syncPointInterval 取 changefeed 配的 sync point 间隔。没开或者读不出来回错误:verify 就站在 syncpoint 上。
func (d changefeedDetail) syncPointInterval() (time.Duration, error) {
	if d.Config == nil || len(d.Config.SyncPointInterval) == 0 || (d.Config.EnableSyncPoint != nil && !*d.Config.EnableSyncPoint) {
		return 0, errors.New("sync point is not enabled")
	}
	var text string
	if err := json.Unmarshal(d.Config.SyncPointInterval, &text); err == nil {
		interval, err := time.ParseDuration(text)
		if err != nil || interval <= 0 {
			return 0, fmt.Errorf("bad sync_point_interval %q", text)
		}
		return interval, nil
	}
	var nanos int64
	if err := json.Unmarshal(d.Config.SyncPointInterval, &nanos); err != nil || nanos <= 0 {
		return 0, errors.New("bad sync_point_interval")
	}
	return time.Duration(nanos), nil
}

// changefeedIdentity 是 changefeed 的身份:id、start_ts、create_time。用同一个 id 删了重建,create_time 就变了。
func changefeedIdentity(changefeed string, d changefeedDetail) (dbplacement.FeedIdentity, error) {
	if d.StartTs == 0 || d.CreateTime == "" {
		return dbplacement.FeedIdentity{}, refusedf("TiCDC did not report start_ts and create_time of changefeed %s: cannot tell whether it was recreated", changefeed)
	}
	return dbplacement.FeedIdentity{Changefeed: changefeed, StartTs: d.StartTs, CreateTime: d.CreateTime}, nil
}

// checkSyncPoint 判一个 syncpoint 能不能拿来 verify。nowTs 取源库的 TSO,不用工具所在机器的时钟。
//   - primary_ts 早于 start_ts:这一行是同名的旧 changefeed 留下的,它和目标现在的内容没有关系。
//   - 太旧:复制落后或者 sync point 停了。
func checkSyncPoint(changefeed string, primaryTs, startTs, nowTs uint64, interval time.Duration) error {
	if primaryTs < startTs {
		return refusedf("the newest syncpoint of changefeed %s (primary_ts=%d) is older than its start_ts=%d: it belongs to an earlier changefeed with the same id. Wait one sync-point-interval and run verify again", changefeed, primaryTs, startTs)
	}
	age := time.Duration(tsoPhysicalMs(nowTs)-tsoPhysicalMs(primaryTs)) * time.Millisecond
	if limit := interval * verifySyncPointIntervals; age > limit {
		return refusedf("the newest syncpoint of changefeed %s is %s old, more than %d sync point intervals (%s): replication is behind or sync point stopped", changefeed, age.Round(time.Second), verifySyncPointIntervals, limit)
	}
	return nil
}

// changefeedBroken:这几种状态下 checkpoint 不会再走。
func changefeedBroken(state string) bool {
	switch state {
	case "stopped", "error", "failed", "finished", "removed":
		return true
	}
	return false
}

func (r *ticdcReplication) detail(ctx context.Context) (changefeedDetail, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.api+"/api/v2/changefeeds/"+url.PathEscape(r.changefeed), nil)
	if err != nil {
		return changefeedDetail{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return changefeedDetail{}, fmt.Errorf("query TiCDC: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return changefeedDetail{}, fmt.Errorf("query TiCDC: %w", err)
	}
	// 查不存在的 changefeed,TiCDC 回 400 加错误码,不是 404(tiflow cdc/api/util.go 的 httpBadRequestError)。
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode == http.StatusBadRequest && strings.Contains(string(body), "ErrChangeFeedNotExists")) {
		return changefeedDetail{}, fmt.Errorf("%w: changefeed %s does not exist", errReplicationBroken, r.changefeed)
	}
	if resp.StatusCode != http.StatusOK {
		return changefeedDetail{}, fmt.Errorf("query TiCDC: HTTP %d", resp.StatusCode)
	}
	var d changefeedDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return changefeedDetail{}, fmt.Errorf("query TiCDC: bad response: %w", err)
	}
	if changefeedBroken(d.State) {
		message := ""
		if d.Error != nil {
			message = d.Error.Message
		}
		return changefeedDetail{}, fmt.Errorf("%w: changefeed %s is %s %s", errReplicationBroken, r.changefeed, d.State, message)
	}
	return d, nil
}

func (r *ticdcReplication) CheckLag(ctx context.Context, maxLag time.Duration) error {
	d, err := r.detail(ctx)
	if err != nil {
		return err
	}
	now, err := readTiDBTSO(ctx, r.source)
	if err != nil {
		return fmt.Errorf("read the source TSO: %w", err)
	}
	lag := time.Duration(tsoPhysicalMs(now)-tsoPhysicalMs(d.CheckpointTs)) * time.Millisecond
	if lag > maxLag {
		return fmt.Errorf("%w: %s behind, limit %s", errLagTooHigh, lag, maxLag)
	}
	return nil
}

func (r *ticdcReplication) Mark(ctx context.Context) (string, error) {
	tso, err := readTiDBTSO(ctx, r.source)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(tso, 10), nil
}

func (r *ticdcReplication) CaughtUp(ctx context.Context, mark string) (bool, error) {
	want, err := strconv.ParseUint(mark, 10, 64)
	if err != nil {
		return false, fmt.Errorf("bad freeze point %q", mark)
	}
	d, err := r.detail(ctx)
	if err != nil {
		return false, err
	}
	return d.CheckpointTs >= want, nil
}

func (r *ticdcReplication) TargetPoint(ctx context.Context) (string, error) {
	tso, err := readTiDBTSO(ctx, r.target)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(tso, 10), nil
}

// Identity 见 replication。changefeed 断开时回 errReplicationBroken。
func (r *ticdcReplication) Identity(ctx context.Context) (dbplacement.FeedIdentity, error) {
	d, err := r.detail(ctx)
	if err != nil {
		return dbplacement.FeedIdentity{}, err
	}
	return r.identityOf(ctx, d)
}

func (r *ticdcReplication) identityOf(ctx context.Context, d changefeedDetail) (dbplacement.FeedIdentity, error) {
	feed, err := changefeedIdentity(r.changefeed, d)
	if err != nil {
		return dbplacement.FeedIdentity{}, err
	}
	feed.TargetCreated, err = readTargetCreated(ctx, r.target, r.schema)
	if err != nil {
		return dbplacement.FeedIdentity{}, err
	}
	return feed, nil
}

// VerifyPoint 读 TiCDC syncpoint 记下的最近一对 TSO:上下游在这一对时间戳上内容相同。changefeed 要开 enable-sync-point(手册 §3)。
func (r *ticdcReplication) VerifyPoint(ctx context.Context) (verifyPoint, error) {
	d, err := r.detail(ctx)
	if err != nil {
		return verifyPoint{}, err
	}
	feed, err := r.identityOf(ctx, d)
	if err != nil {
		return verifyPoint{}, err
	}
	interval, err := d.syncPointInterval()
	if err != nil {
		return verifyPoint{}, refusedf("changefeed %s: %v: create it with enable-sync-point=true", r.changefeed, err)
	}
	var primary, secondary, created string
	err = r.target.QueryRowContext(ctx, "SELECT primary_ts,secondary_ts,CAST(created_at AS CHAR) FROM tidb_cdc.syncpoint_v1 WHERE changefeed=? ORDER BY created_at DESC,primary_ts DESC LIMIT 1", r.changefeed).
		Scan(&primary, &secondary, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return verifyPoint{}, refusedf("tidb_cdc.syncpoint_v1 has no row for changefeed %s: create the changefeed with enable-sync-point=true and wait one sync-point-interval", r.changefeed)
	}
	if err != nil {
		return verifyPoint{}, fmt.Errorf("read tidb_cdc.syncpoint_v1 on the target: %w", err)
	}
	primaryTs, err := strconv.ParseUint(primary, 10, 64)
	if err != nil {
		return verifyPoint{}, fmt.Errorf("bad primary_ts %q in tidb_cdc.syncpoint_v1", primary)
	}
	now, err := readTiDBTSO(ctx, r.source)
	if err != nil {
		return verifyPoint{}, fmt.Errorf("read the source TSO: %w", err)
	}
	if err := checkSyncPoint(r.changefeed, primaryTs, d.StartTs, now, interval); err != nil {
		return verifyPoint{}, err
	}
	return verifyPoint{
		Source: primary, Target: secondary, Feed: feed, PrimaryTs: primaryTs, AtMs: uint64(tsoPhysicalMs(primaryTs)),
		Detail: fmt.Sprintf("syncpoint primary_ts=%s secondary_ts=%s created_at=%s", primary, secondary, created),
	}, nil
}

// RecheckVerifyPoint:快照 TSO 不会动。
func (r *ticdcReplication) RecheckVerifyPoint(context.Context, verifyPoint) error { return nil }

// gtidReplication 实现 replication,MySQL 档。source 是源主库,target 是目标主库,verifySource 是源库的复制副本(只有 verify 用)。
type gtidReplication struct {
	channel                      string
	source, target, verifySource *sql.DB
	schema                       string
}

func readGTIDExecuted(ctx context.Context, db *sql.DB) (string, error) {
	var set string
	if err := db.QueryRowContext(ctx, "SELECT @@global.gtid_executed").Scan(&set); err != nil {
		return "", err
	}
	return normalizeGTIDSet(set), nil
}

// normalizeGTIDSet 去掉服务器输出里的换行和空白。
func normalizeGTIDSet(set string) string {
	return strings.Join(strings.Fields(set), "")
}

// gtidUUIDs 取集合里出现的来源 UUID。
func gtidUUIDs(set string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(normalizeGTIDSet(set), ",") {
		if uuid, _, ok := strings.Cut(part, ":"); ok && uuid != "" {
			out[strings.ToLower(uuid)] = true
		}
	}
	return out
}

// checkChannel 核目标上的复制通道在跑。IO 线程 CONNECTING 是抖动,不算断。
func (r *gtidReplication) checkChannel(ctx context.Context) error {
	var applier string
	err := r.target.QueryRowContext(ctx, "SELECT SERVICE_STATE FROM performance_schema.replication_applier_status WHERE CHANNEL_NAME=?", r.channel).Scan(&applier)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: the target has no replication channel %s", errReplicationBroken, r.channel)
	}
	if err != nil {
		return fmt.Errorf("read the replication state: %w", err)
	}
	if applier != "ON" {
		return fmt.Errorf("%w: applier of channel %s is %s", errReplicationBroken, r.channel, applier)
	}
	var connection string
	err = r.target.QueryRowContext(ctx, "SELECT SERVICE_STATE FROM performance_schema.replication_connection_status WHERE CHANNEL_NAME=?", r.channel).Scan(&connection)
	if err != nil {
		return fmt.Errorf("read the replication state: %w", err)
	}
	if connection == "OFF" {
		return fmt.Errorf("%w: receiver of channel %s is OFF", errReplicationBroken, r.channel)
	}
	return nil
}

// CheckLag:源库当前的 GTID 集合在 maxLag 之内能在目标上执行完,延迟就低于 maxLag。
func (r *gtidReplication) CheckLag(ctx context.Context, maxLag time.Duration) error {
	if err := r.checkChannel(ctx); err != nil {
		return err
	}
	set, err := readGTIDExecuted(ctx, r.source)
	if err != nil {
		return fmt.Errorf("read the source position: %w", err)
	}
	caught, err := r.waitFor(ctx, set, int(math.Ceil(maxLag.Seconds())))
	if err != nil {
		return err
	}
	if !caught {
		return fmt.Errorf("%w: the target did not reach the source position within %s", errLagTooHigh, maxLag)
	}
	return nil
}

func (r *gtidReplication) waitFor(ctx context.Context, set string, seconds int) (bool, error) {
	var result sql.NullInt64
	if err := r.target.QueryRowContext(ctx, "SELECT WAIT_FOR_EXECUTED_GTID_SET(?,?)", set, seconds).Scan(&result); err != nil {
		return false, fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET: %w", err)
	}
	return result.Valid && result.Int64 == 0, nil
}

func (r *gtidReplication) Mark(ctx context.Context) (string, error) {
	return readGTIDExecuted(ctx, r.source)
}

// gtidWaitSeconds 是一次等待的时长:等待之间要回去核通道状态,复制断了才能立刻回滚,不用耗满预算。
const gtidWaitSeconds = 1

func (r *gtidReplication) CaughtUp(ctx context.Context, mark string) (bool, error) {
	if err := r.checkChannel(ctx); err != nil {
		return false, err
	}
	return r.waitFor(ctx, mark, gtidWaitSeconds)
}

func (r *gtidReplication) TargetPoint(ctx context.Context) (string, error) {
	return readGTIDExecuted(ctx, r.target)
}

// Identity 见 replication。通道的身份是通道名和它的来源;AUTO_POSITION 必须开着:按文件位置复制能跳过事务,GTID 集合对不上它也看不出来。
func (r *gtidReplication) Identity(ctx context.Context) (dbplacement.FeedIdentity, error) {
	var autoPosition, host, sourceUUID string
	var port uint32
	err := r.target.QueryRowContext(ctx, "SELECT c.AUTO_POSITION,c.HOST,c.PORT,s.SOURCE_UUID FROM performance_schema.replication_connection_configuration c JOIN performance_schema.replication_connection_status s ON s.CHANNEL_NAME=c.CHANNEL_NAME WHERE c.CHANNEL_NAME=?", r.channel).
		Scan(&autoPosition, &host, &port, &sourceUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return dbplacement.FeedIdentity{}, fmt.Errorf("%w: the target has no replication channel %s", errReplicationBroken, r.channel)
	}
	if err != nil {
		return dbplacement.FeedIdentity{}, fmt.Errorf("read the replication channel: %w", err)
	}
	feed, err := channelIdentity(r.channel, autoPosition, host, port, sourceUUID)
	if err != nil {
		return dbplacement.FeedIdentity{}, err
	}
	feed.TargetCreated, err = readTargetCreated(ctx, r.target, r.schema)
	if err != nil {
		return dbplacement.FeedIdentity{}, err
	}
	return feed, nil
}

func channelIdentity(channel, autoPosition, host string, port uint32, sourceUUID string) (dbplacement.FeedIdentity, error) {
	if autoPosition != "1" {
		return dbplacement.FeedIdentity{}, refusedf("replication channel %s does not use SOURCE_AUTO_POSITION=1: file positions can skip transactions and nothing here would notice", channel)
	}
	if sourceUUID == "" {
		return dbplacement.FeedIdentity{}, refusedf("replication channel %s has not connected to its source yet: start it and try again", channel)
	}
	return dbplacement.FeedIdentity{Channel: channel, SourceUUID: strings.ToLower(sourceUUID), SourceHost: host, SourcePort: port}, nil
}

// VerifyPoint 核源副本和目标停在同一个位置:源副本的全部事务目标都有,目标上来自同一批来源的事务也不比它多。
// 目标是别的库的主库,自己的 UUID 上还有事务,所以只比源副本出现过的 UUID。
func (r *gtidReplication) VerifyPoint(ctx context.Context) (verifyPoint, error) {
	feed, err := r.Identity(ctx)
	if err != nil {
		return verifyPoint{}, err
	}
	set, err := r.alignedSet(ctx, feed)
	if err != nil {
		return verifyPoint{}, err
	}
	return verifyPoint{Detail: "GTID set " + set, Feed: feed}, nil
}

func (r *gtidReplication) alignedSet(ctx context.Context, feed dbplacement.FeedIdentity) (string, error) {
	if r.verifySource == nil {
		return "", refusedf("the mysql backend verifies on a replica of the source: set %s", envVerifySourceDSN)
	}
	var replicaUUID, targetUUID string
	if err := r.verifySource.QueryRowContext(ctx, "SELECT @@server_uuid").Scan(&replicaUUID); err != nil {
		return "", fmt.Errorf("read the source replica identity: %w", err)
	}
	if err := r.target.QueryRowContext(ctx, "SELECT @@server_uuid").Scan(&targetUUID); err != nil {
		return "", fmt.Errorf("read the target identity: %w", err)
	}
	sourceSet, err := readGTIDExecuted(ctx, r.verifySource)
	if err != nil {
		return "", fmt.Errorf("read the source replica position: %w", err)
	}
	if err := checkVerifySource(replicaUUID, targetUUID, feed.SourceUUID, sourceSet); err != nil {
		return "", err
	}
	targetSet, err := readGTIDExecuted(ctx, r.target)
	if err != nil {
		return "", fmt.Errorf("read the target position: %w", err)
	}
	var missing, extra string
	if err := r.target.QueryRowContext(ctx, "SELECT GTID_SUBTRACT(?,?),GTID_SUBTRACT(?,?)", sourceSet, targetSet, targetSet, sourceSet).Scan(&missing, &extra); err != nil {
		return "", fmt.Errorf("compare GTID sets: %w", err)
	}
	if err := checkGTIDAligned(sourceSet, targetUUID, missing, extra); err != nil {
		return "", err
	}
	return sourceSet, nil
}

// checkVerifySource 核源副本的连接串连的确实是源库这一边。指到目标自己的话,GTID 集合和校验和都相同,verify 等于没比。
func checkVerifySource(replicaUUID, targetUUID, feedSourceUUID, replicaSet string) error {
	if strings.EqualFold(replicaUUID, targetUUID) {
		return refusedf("%s points at the target itself: verify would compare the target with itself", envVerifySourceDSN)
	}
	if !gtidUUIDs(replicaSet)[strings.ToLower(feedSourceUUID)] {
		return refusedf("%s has no transactions from the replication source %s: it is not a replica of the source", envVerifySourceDSN, feedSourceUUID)
	}
	return nil
}

// checkGTIDAligned:missing 是源副本有、目标没有的;extra 是目标有、源副本没有的。extra 里只能有目标自己的 UUID。
// 目标的 UUID 也可能出现在源副本的集合里(这个库以前是从目标搬过来的),所以要按目标的 UUID 排除,不能只看「源副本有没有这个 UUID」。
func checkGTIDAligned(sourceSet, targetUUID, missing, extra string) error {
	if normalizeGTIDSet(missing) != "" {
		return refusedf("the target is behind the source replica by %s: stop both at the same position as in docs/ops/db-move-runbook.md §4", normalizeGTIDSet(missing))
	}
	sourceUUIDs := gtidUUIDs(sourceSet)
	for uuid := range gtidUUIDs(extra) {
		if sourceUUIDs[uuid] && !strings.EqualFold(uuid, targetUUID) {
			return refusedf("the target is ahead of the source replica on %s: stop both at the same position as in docs/ops/db-move-runbook.md §4", uuid)
		}
	}
	return nil
}

// RecheckVerifyPoint:校验和算的这段时间副本要是在动,比出来的结果不算数。
func (r *gtidReplication) RecheckVerifyPoint(ctx context.Context, point verifyPoint) error {
	set, err := r.alignedSet(ctx, point.Feed)
	if err != nil {
		return err
	}
	if "GTID set "+set != point.Detail {
		return refusedf("the source replica moved during verify (%s, now %s): keep it stopped until verify ends", point.Detail, set)
	}
	return nil
}
