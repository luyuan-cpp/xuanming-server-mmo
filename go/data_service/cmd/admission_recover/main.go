// admission_recover 只消费已审定原请求及永久SQL维护收据，不接Scene关闭RPC或Redis临时证明。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"data_service/internal/store"
	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/encoding/protojson"
	dbpb "proto/common/database"
)

type recoveryOptions struct {
	requestFile, requestSHA, dsnEnv, schema, stores, mode, planFile, planSHA, localTarget, sqlTarget string
	maintenance                                                                                      bool
}

// 只封装执行目标/原文件摘要/校验得到的SQL摘要，不复制proto请求的业务模型。
type recoveryCLIPlan struct {
	Format      string `json:"format"`
	RequestSHA  string `json:"request_sha256"`
	Schema      string `json:"schema"`
	LocalTarget string `json:"local_target_id"`
	SQLTarget   string `json:"physical_sql_addr"`
	SQLProof    string `json:"sql_proof_sha256"`
}

func main() {
	var o recoveryOptions
	flag.StringVar(&o.requestFile, "request", "", "已审定原请求JSON；不包含连接凭据")
	flag.StringVar(&o.requestSHA, "request-sha256", "", "原请求文件SHA-256，不重编码替代")
	flag.StringVar(&o.dsnEnv, "dsn-env", "ADMISSION_RECOVER_DSN", "同TiDB全局库DSN所在进程环境变量名")
	flag.StringVar(&o.schema, "schema", "", "权威全局SQL库名")
	flag.StringVar(&o.stores, "storage-ids", "", "允许读取的真实storage_id，逗号分隔")
	flag.BoolVar(&o.maintenance, "controlled-maintenance", false, "确认停旧写者、禁止复活，并保持已审维护闸；不是工具代停声明")
	flag.StringVar(&o.mode, "mode", "plan", "plan默认只读；apply才实际退休指定旧holder")
	flag.StringVar(&o.planFile, "plan", "", "plan新建审定封套；apply只读")
	flag.StringVar(&o.planSHA, "plan-sha256", "", "apply必填封套原文件SHA-256")
	flag.StringVar(&o.localTarget, "local-target-id", "", "审定maintenance-recovery SQL目标的32位小写hex永久nonce；此历史参数名不授予调试写权限")
	flag.StringVar(&o.sqlTarget, "sql-target-address", "", "审定SQL物理host:port，必须逐字等于环境DSN和专用维护标记，不含凭据")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runRecovery(ctx, o, os.Stdout, os.Stderr, nil)
	stop()
	os.Exit(code)
}

type recoverHolder func(context.Context, *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error)

func runRecovery(ctx context.Context, o recoveryOptions, out, errOut io.Writer, execute recoverHolder) int {
	if o.mode == "" {
		o.mode = "plan"
	}
	in, ids, err := readRecoveryRequest(o)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	var expected []byte
	if o.mode == "apply" {
		expected, err = readRecoveryCLIPlan(o)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if execute == nil {
		// 从环境读DSN后只取连接配置，错误中绝不回显原DSN或密码。
		cfg, err := mysql.ParseDSN(os.Getenv(o.dsnEnv))
		if err != nil || cfg.DBName != o.schema || cfg.Net != "tcp" || cfg.Addr != o.sqlTarget {
			fmt.Fprintln(errOut, "恢复SQL连接参数无效")
			return 2
		}
		directory, err := store.NewAccountDirectoryStore(store.MySQLConfig{Host: cfg.Addr, User: cfg.User, Password: cfg.Passwd, DBName: o.schema}, true, 100, 1)
		if err != nil {
			fmt.Fprintln(errOut, "权威SQL强读或目录形状核对失败")
			return 2
		}
		defer directory.Close()
		admissions := store.NewAccountRoleAdmissionStore(directory)
		if err := admissions.CheckRecoverySchema(ctx); err != nil {
			fmt.Fprintln(errOut, "永久恢复收据形状不符；须先正式迁移")
			return 2
		}
		verifier, err := store.NewSQLRoleFreezeVerifier(ids)
		if err != nil {
			fmt.Fprintln(errOut, "落点白名单无效")
			return 2
		}
		execute = func(ctx context.Context, in *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
			if o.mode == "plan" {
				return admissions.PlanCrashedTradeHolder(ctx, in, verifier, store.MaintenanceRecoveryTarget{ID: o.localTarget, SQLAddr: o.sqlTarget})
			}
			return admissions.ApplyCrashedTradeHolder(ctx, in, verifier, store.MaintenanceRecoveryTarget{ID: o.localTarget, SQLAddr: o.sqlTarget}, expected)
		}
	}
	receipt, err := execute(ctx, in)
	if err != nil {
		fmt.Fprintln(errOut, "旧holder恢复未确认；可能为拒绝或提交未知，保持维护并仅按原请求重读")
		return 1
	}
	if receipt == nil || !bytes.Equal(receipt.RequestId, in.RequestId) || len(receipt.SqlProofDigest) != 32 || receipt.RequiredOwnerEpoch == 0 || receipt.RequiredSaveSeq == 0 {
		fmt.Fprintln(errOut, "恢复返回原件不完整")
		return 1
	}
	if o.mode == "apply" && !bytes.Equal(receipt.SqlProofDigest, expected) {
		fmt.Fprintln(errOut, "返回原件偏离审定SQL证明")
		return 1
	}
	if o.mode == "plan" {
		plan := recoveryCLIPlan{Format: "old-holder-recovery-cli-plan-v2", RequestSHA: o.requestSHA, Schema: o.schema, LocalTarget: o.localTarget, SQLTarget: o.sqlTarget, SQLProof: hex.EncodeToString(receipt.SqlProofDigest)}
		body, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "只读计划编码失败")
			return 1
		}
		body = append(body, '\n')
		f, err := os.OpenFile(o.planFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(errOut, "计划必须新建，不覆盖原件")
			return 2
		}
		_, err = f.Write(body)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			fmt.Fprintln(errOut, "只读计划写入失败")
			return 1
		}
		sum := sha256.Sum256(body)
		fmt.Fprintf(out, "plan_sha256=%s；完整SQL校验已只读回滚，未退休holder\n", hex.EncodeToString(sum[:]))
		return 0
	}
	body, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(receipt)
	if err != nil {
		fmt.Fprintln(errOut, "永久恢复原件编码失败")
		return 1
	}
	if _, err = fmt.Fprintln(out, string(body)); err != nil {
		fmt.Fprintln(errOut, "永久收据已存在但输出失败，仅原请求重读")
		return 1
	}
	return 0
}

func readRecoveryRequest(o recoveryOptions) (*dbpb.AccountRoleAdmissionRecovery_Request, []uint32, error) {
	if !o.maintenance || o.requestFile == "" || o.dsnEnv == "" || o.schema == "" || len(o.schema) > 64 || strings.Trim(o.schema, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" || len(o.requestSHA) != 64 || o.planFile == "" || !(store.MaintenanceRecoveryTarget{ID: o.localTarget, SQLAddr: o.sqlTarget}).Valid() || (o.mode != "plan" && o.mode != "apply") || (o.mode == "plan" && o.planSHA != "") || (o.mode == "apply" && len(o.planSHA) != 64) {
		return nil, nil, errors.New("须指定审定请求、具名维护目标、plan/apply及受控维护窗口")
	}
	var ids []uint32
	seen := map[uint64]bool{}
	for _, s := range strings.Split(o.stores, ",") {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n == 0 || seen[n] || strconv.FormatUint(n, 10) != s {
			return nil, nil, errors.New("storage白名单必须为规范唯一正整数")
		}
		seen[n] = true
		ids = append(ids, uint32(n))
	}
	if len(ids) > 1024 {
		return nil, nil, errors.New("storage白名单过大")
	}
	f, err := os.Open(o.requestFile)
	if err != nil {
		return nil, nil, errors.New("无法读取审定请求")
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(body) > 16384 {
		return nil, nil, errors.New("审定请求超过预算")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != o.requestSHA {
		return nil, nil, errors.New("审定请求原bytes摘要不符")
	}
	in := &dbpb.AccountRoleAdmissionRecovery_Request{}
	if err := protojson.Unmarshal(body, in); err != nil {
		return nil, nil, errors.New("审定请求JSON契约不符")
	}
	return in, ids, nil
}

func readRecoveryCLIPlan(o recoveryOptions) ([]byte, error) {
	f, err := os.Open(o.planFile)
	if err != nil {
		return nil, errors.New("无法读取审定执行封套")
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(body) > 4096 {
		return nil, errors.New("执行封套超出预算")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != o.planSHA {
		return nil, errors.New("执行封套原SHA不符")
	}
	var p recoveryCLIPlan
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil {
		return nil, errors.New("执行封套格式不符")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("执行封套有额外内容")
	}
	proof, err := hex.DecodeString(p.SQLProof)
	if err != nil || len(proof) != 32 || hex.EncodeToString(proof) != p.SQLProof || p.Format != "old-holder-recovery-cli-plan-v2" || p.RequestSHA != o.requestSHA || p.Schema != o.schema || p.LocalTarget != o.localTarget || p.SQLTarget != o.sqlTarget {
		return nil, errors.New("执行封套与当前请求/SQL实例不符")
	}
	return proof, nil
}
