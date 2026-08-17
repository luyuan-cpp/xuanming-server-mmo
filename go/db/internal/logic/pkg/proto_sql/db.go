package proto_sql

import (
	"database/sql"
	"db/internal/config"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	_ "proto/common/database"
	"regexp"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type GameDB struct {
	SqlModel *proto2mysql.DB
	DB       *sql.DB
}

var DB *GameDB

func newMysqlConfig() *mysql.Config {
	myCnf := mysql.NewConfig()
	myCnf.User = config.AppConfig.ServerConfig.Database.User
	myCnf.Passwd = config.AppConfig.ServerConfig.Database.Passwd
	myCnf.Addr = config.AppConfig.ServerConfig.Database.Hosts
	myCnf.Net = config.AppConfig.ServerConfig.Database.Net
	myCnf.DBName = config.AppConfig.ServerConfig.Database.DBName
	// 强制会话级严格模式:玩家权威数据是 MEDIUMBLOB 里的 protobuf,非严格模式下超长写入
	// 会 err=nil 静默截断,下次 proto.Unmarshal 必失败且无任何日志可追溯,损失面是整个 zone。
	// 用连接参数在每条连接建立时 SET(会话级覆盖 global),而非仅做启动断言——这样即使
	// MySQL 服务端 global 未配严格模式,本服务的写入路径依然是严格的。
	myCnf.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES'"}
	return myCnf
}

func CreateDatabase() error {
	mysqlConfig := newMysqlConfig()
	mysqlConfig.DBName = ""

	conn, err := mysql.NewConnector(mysqlConfig)
	if err != nil {
		return fmt.Errorf("error creating MySQL connector: %w", err)
	}

	tempDB := sql.OpenDB(conn)
	defer func() {
		if err := tempDB.Close(); err != nil {
			logx.Errorf("error closing temp database connection: %v", err)
		}
	}()

	_, err = tempDB.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", config.AppConfig.ServerConfig.Database.DBName))
	if err != nil {
		logx.Errorf("error creating database: %v", err)
		return err
	}

	return nil
}

func isUnknownDatabaseError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1049
	}
	return false
}

func openDB() error {
	mysqlConfig := newMysqlConfig()
	conn, err := mysql.NewConnector(mysqlConfig)
	if err != nil {
		log.Fatal(err)
		return err
	}

	databaseHandle := sql.OpenDB(conn)
	if err := databaseHandle.Ping(); err != nil {
		_ = databaseHandle.Close()
		if !isUnknownDatabaseError(err) {
			return err
		}

		if err := CreateDatabase(); err != nil {
			return err
		}

		conn, err = mysql.NewConnector(mysqlConfig)
		if err != nil {
			log.Fatal(err)
			return err
		}

		databaseHandle = sql.OpenDB(conn)
		if err := databaseHandle.Ping(); err != nil {
			_ = databaseHandle.Close()
			return err
		}
	}

	DB = &GameDB{
		DB:       databaseHandle,
		SqlModel: proto2mysql.NewDB(),
	}

	DB.DB.SetMaxOpenConns(config.AppConfig.ServerConfig.Database.MaxOpenConn)
	DB.DB.SetMaxIdleConns(config.AppConfig.ServerConfig.Database.MaxIdleConn)
	DB.DB.SetConnMaxLifetime(5 * time.Minute)

	err = DB.SqlModel.OpenDB(DB.DB, mysqlConfig.DBName)
	if err != nil {
		return err
	}
	return nil
}

func InitDB() {
	if err := openDB(); err != nil {
		log.Fatalf("error opening database: %v", err)
	}
	CreateOrUpdateTable()
}

// readJSONFile reads a JSON file and returns the message type names.
func readJSONFile(filePath string) ([]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var jsonData struct {
		Messages []string `json:"messages"`
	}

	err = json.Unmarshal(data, &jsonData)
	if err != nil {
		return nil, err
	}

	return jsonData.Messages, nil
}

func getTablesFromJSON() ([]proto.Message, error) {
	messageNames, err := readJSONFile(config.AppConfig.ServerConfig.JsonPath)
	if err != nil {
		return nil, fmt.Errorf("read JSON file failed: %v", err)
	}

	var messages []proto.Message

	for _, typeName := range messageNames {
		msgType, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(typeName))
		if err != nil {
			logx.Error(err)
			continue
		}

		msg := msgType.New().Interface()
		messages = append(messages, msg)
	}

	return messages, nil
}

// ddlTableNameRegex 从生成的建表语句里解出表名,用于启动期表名守卫。
var ddlTableNameRegex = regexp.MustCompile("CREATE TABLE IF NOT EXISTS `([^`]+)`")

// assertTableNameLocked 表名守卫:proto2mysql 新版默认表名 = proto full name(含点号),
// 一旦 OptionTableName 因任何原因未被读到,schema sync 会静默建出新表,存量数据对业务
// "消失"。这里强校验每张表生成 DDL 的表名与 proto 里声明的 OptionTableName 完全一致,
// 不一致直接拒绝启动(全局数据层决策风险 #3)。
func assertTableNameLocked(table proto.Message) {
	md := table.ProtoReflect().Descriptor()
	declared, ok := proto2mysql.TableNameFromDescriptor(md)
	if !ok || declared == "" {
		log.Fatalf("表名守卫: 消息 %s 未声明 OptionTableName,拒绝按默认表名建表", md.FullName())
	}
	ddl := DB.SqlModel.GetCreateTableSQL(table)
	m := ddlTableNameRegex.FindStringSubmatch(ddl)
	if m == nil {
		log.Fatalf("表名守卫: 无法从 %s 的建表语句中解析表名: %s", md.FullName(), ddl)
	}
	if m[1] != declared {
		log.Fatalf("表名守卫: 消息 %s 生成的表名 %q 与 proto 声明 %q 不一致,拒绝启动", md.FullName(), m[1], declared)
	}
}

func CreateOrUpdateTable() {
	tables, err := getTablesFromJSON()
	if err != nil {
		log.Fatalf("error getting tables: %v", err)
		return
	}

	for _, table := range tables {
		DB.SqlModel.RegisterTable(table)
		assertTableNameLocked(table)
		err := DB.SqlModel.CreateOrUpdateTable(table)
		if err != nil {
			log.Fatal(err)
			return
		}
	}
}
