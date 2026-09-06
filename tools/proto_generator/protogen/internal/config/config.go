package _config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"protogen/logger" // global logger package

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Removed original local logger variable and InitLogger method.
// Using protogen/logger package's Global instance directly.

// Config is the global configuration for the code generator.
type Config struct {
	Paths          Paths                 `yaml:"paths"`
	DirectoryNames DirectoryNames        `yaml:"directory_names"` // directory naming constants
	FileExtensions FileExtensions        `yaml:"file_extensions"`
	Naming         Naming                `yaml:"naming"`
	PathLists      PathLists             `yaml:"path_lists"`
	Generators     Generators            `yaml:"generators"`
	Mappings       map[string]Mapping    `yaml:"mappings"`
	Parser         Parser                `yaml:"parser"`
	Log            LogConfig             `yaml:"log"`
	DomainMeta     map[string]DomainMeta `yaml:"domain_meta"`
}

// DirectoryNames holds directory naming constants (maps to YAML directory_names node).
type DirectoryNames struct {
	GeneratedRpcName     string `yaml:"generated_rpc_name"`       // generated RPC directory name
	ServiceInfoName      string `yaml:"service_info_name"`        // service metadata directory name
	ProtoDirName         string `yaml:"proto_dir_name"`           // proto directory name
	GoZeroProtoDirName   string `yaml:"go_zero_proto_dir_name"`   // go-zero proto directory name
	NormalGoProto        string `yaml:"normal_go_proto"`          // normal Go code directory name (maps to normal_go_dir_name in YAML)
	RobotProtoName       string `yaml:"robot_proto_name"`         // robot proto directory name constant
	RobotGoZeroProtoName string `yaml:"robot_go_zero_proto_name"` // robot go-zero proto directory name constant
	ModelDirName         string `yaml:"model_dir_name"`           // model directory name
	ServiceIncludeDir    string `yaml:"service_include_dir"`      // service include directory name
	GoGrpcBaseDirName    string `yaml:"go_grpc_base_dir_name"`    // Go GRPC service base directory path segment
}

// Paths holds path configuration (maps to YAML paths node).
type Paths struct {
	OutputRoot              string `yaml:"output_root"`
	ProtoDir                string `yaml:"proto_dir"`
	ProtoParentIncludePath  string `yaml:"proto_parent_include_path"`
	ThirdPartyDir           string `yaml:"third_party_dir"`
	GrpcDir                 string `yaml:"grpc_dir"`
	ProtobufDir             string `yaml:"protobuf_dir"`
	NodeGoDir               string `yaml:"node_go_dir"`
	NodeCppDir              string `yaml:"node_cpp_dir"`
	NodePkgDir              string `yaml:"node_pkg_dir"`
	NodeLibsEngine          string `yaml:"node_libs_engine"`
	NodeLibGame             string `yaml:"node_lib_game"`
	NodePkgLogicScene       string `yaml:"node_pkg_logic_scene"`
	SceneNodeDir            string `yaml:"scene_node_dir"`
	GateNodeDir             string `yaml:"gate_node_dir"`
	Robot                   string `yaml:"robot"`
	RobotGenerated          string `yaml:"robot_generated"`
	RobotProto              string `yaml:"robot_proto"`
	RobotGeneratedProto     string `yaml:"robot_generated_proto"`
	RobotGoZeroProto        string `yaml:"robot_go_zero_proto"`
	GeneratedDir            string `yaml:"generated_dir"`
	ToolDir                 string `yaml:"tool_dir"`
	TempFileGenDir          string `yaml:"temp_file_gen_dir"`
	ProtoBufDescDir         string `yaml:"proto_buf_desc_dir"`
	AllInOneDesc            string `yaml:"all_in_one_desc"`
	GrpcTempDir             string `yaml:"grpc_temp_dir"`
	ProtoBufCTempDir        string `yaml:"proto_buf_c_temp_dir"`
	PlayerStorageTempDir    string `yaml:"player_storage_temp_dir"`
	ProtoBufCProtoOutputDir string `yaml:"proto_buf_c_proto_output_dir"`
	GeneratedOutputDir      string `yaml:"generated_output_dir"`
	GrpcOutputDir           string `yaml:"grpc_output_dir"`
	GrpcProtoOutputDir      string `yaml:"grpc_proto_output_dir"`
	RobotGeneratedOutputDir string `yaml:"robot_generated_output_dir"`
	RobotGoGamePbDir        string `yaml:"robot_go_game_pb_dir"`
	RobotGoGenDir           string `yaml:"robot_go_gen_dir"`
	GeneratedRpcDir         string `yaml:"generated_rpc_dir"`
	ServiceInfoDir          string `yaml:"service_info_dir"`
	ProtoBufCLuaDir         string `yaml:"proto_buf_c_lua_dir"`
	CppGenGrpcDir           string `yaml:"cpp_gen_grpc_dir"`
	UtilGeneratorDir        string `yaml:"util_generator_dir"`
	GeneratorDir            string `yaml:"generator_dir"`
	GeneratorProtoDir       string `yaml:"generator_proto_dir"`
	ProjectGeneratedDir     string `yaml:"project_generated_dir"`
	TableGeneratorDir       string `yaml:"table_generator_dir"`
	GameRpcProtoPath        string `yaml:"game_rpc_proto_path"`
	ToolsDir                string `yaml:"tools_dir"`
	MessageIdFile           string `yaml:"message_id_file"`
	EventIdFile             string `yaml:"event_id_file"`
	RobotMessageIdFile      string `yaml:"robot_message_id_file"`
	ClientRpcRouterRouteTableFile string `yaml:"client_rpc_router_route_table_file"`
	RobotProtoImportPath    string `yaml:"robot_proto_import_path"`
	ServiceCppFile          string `yaml:"service_cpp_file"`
	ServiceHeaderFile       string `yaml:"service_header_file"`
	GrpcInitCppFile         string `yaml:"grpc_init_cpp_file"`
	GrpcInitHeadFile        string `yaml:"grpc_init_head_file"`
	GenUtilCppFile          string `yaml:"gen_util_cpp_file"`
	GenUtilHeadFile         string `yaml:"gen_util_head_file"`
	RobotMsgBodyHandlerFile string `yaml:"robot_msg_body_handler_file"`
	PlayerStorageSystemDir  string `yaml:"player_storage_system_dir"`
	PlayerDataLoaderFile    string `yaml:"player_data_loader_file"`

	// Unity client output paths.
	UnityClientDir            string `yaml:"unity_client_dir"`
	UnityHandlerDir           string `yaml:"unity_handler_dir"`
	UnityHandlerRegistryFile  string `yaml:"unity_handler_registry_file"`

	// paths from original config
	EventHandlerSourceDirectory    string `yaml:"event_handler_source_directory"`
	SceneNodeEventHandlerDirectory string `yaml:"scene_node_event_handler_directory"`
	GateNodeEventHandlerDirectory  string `yaml:"gate_node_event_handler_directory"`
	SceneAttributeSyncDir          string `yaml:"scene_attribute_sync_dir"`
}

// FileExtensions holds file extension configuration (suffixes/extensions only).
type FileExtensions struct {
	Proto           string `yaml:"proto"`
	PbCc            string `yaml:"pb_cc"`
	PbH             string `yaml:"pb_h"`
	GrpcPbCc        string `yaml:"grpc_pb_cc"`
	GrpcPbH         string `yaml:"grpc_pb_h"`
	Header          string `yaml:"header"`
	Cpp             string `yaml:"cpp"`
	HandlerH        string `yaml:"handler_h"`
	HandlerCpp      string `yaml:"handler_cpp"`
	RepliedHandlerH string `yaml:"replied_handler_h"`
	GrpcClient      string `yaml:"grpc_client"`
	GrpcClientH     string `yaml:"grpc_client_h"`
	GrpcClientCpp   string `yaml:"grpc_client_cpp"`
	GrpcHandlerH    string `yaml:"grpc_handler_h"`
	GrpcHandlerCpp  string `yaml:"grpc_handler_cpp"`
	LoaderCpp       string `yaml:"loader_cpp"`

	// extensions from original config
	RepliedHandlerHeaderExtension      string `yaml:"replied_handler_header_extension"`
	CppRepliedHandlerEx                string `yaml:"cpp_replied_handler_ex"`
	ModelSqlExtension                  string `yaml:"model_sql_extension"`
	MessageIdGoFile                    string `yaml:"message_id_go_file"`
	EventIdGoFile                      string `yaml:"event_id_go_file"`
	ServiceInfoExtension               string `yaml:"service_info_extension"`
	RegisterRepliedHandlerCppExtension string `yaml:"register_replied_handler_cpp_extension"`
	RegisterHandlerCppExtension        string `yaml:"register_handler_cpp_extension"`
}

// Naming holds naming convention configuration (business logic naming constants).
type Naming struct {
	MessageId            string `yaml:"message_id"`
	EventId              string `yaml:"event_id"`
	MethodIndex          string `yaml:"method_index"`
	HandlerFile          string `yaml:"handler_file"`
	RepliedHandlerFile   string `yaml:"replied_handler_file"`
	EventHandlerBase     string `yaml:"event_handler_base"`
	EmptyResponse        string `yaml:"empty_response"`
	MysqlName            string `yaml:"mysql_name"`
	PlayerService        string `yaml:"player_service"`
	PlayerRepliedService string `yaml:"player_replied_service"`
	CompleteQueue        string `yaml:"complete_queue"`
	DatabasePrefix       string `yaml:"database_prefix"`
	DbProtoFile          string `yaml:"db_proto_file"`
	DbTableFile          string `yaml:"db_table_file"`
	DbTableListJson      string `yaml:"db_table_list_json"`
	GameRpcProto         string `yaml:"game_rpc_proto"`
	GrpcName             string `yaml:"grpc_name"`

	// naming constants from original config
	PlayerServiceIncludeName        string `yaml:"player_service_include_name"`
	PlayerServiceRepliedIncludeName string `yaml:"player_service_replied_include_name"`
	MacroReturnIncludeName          string `yaml:"macro_return_include_name"`
	NodeEnumName                    string `yaml:"node_enum_name"`
	NodeServiceSuffix               string `yaml:"node_service_suffix"`
	GoPackage                       string `yaml:"go_package"`
	GoRobotPackage                  string `yaml:"go_robot_package"`
	GoogleMethodController          string `yaml:"google_method_controller"`
	PlayerMethodController          string `yaml:"player_method_controller"`
	IncludeEndLine                  string `yaml:"include_end_line"`
	IncludeBegin                    string `yaml:"include_begin"`
	YourCodeBegin                   string `yaml:"your_code_begin"`
	YourCodeEnd                     string `yaml:"your_code_end"`
	YourCodePair                    string `yaml:"your_code_pair"`
	Model                           string `yaml:"model_dir_name"`
}

// PathLists holds path list configuration.
type PathLists struct {
	ProtoDirectories         []string          `yaml:"proto_directories"`
	RobotProtoDirectories    []string          `yaml:"robot_proto_directories"`
	MethodHandlerDirectories MethodHandlerDirs `yaml:"method_handler_directories"`
	NodeTypes                NodeTypes         `yaml:"node_types"`
	ProtoDirs                ProtoDirs         `yaml:"proto_dirs"`
}

type ProtoDirs struct {
	Common         string `yaml:"common"`
	LogicComponent string `yaml:"logic_component"`
	LogicEvent     string `yaml:"logic_event"`
	ContractsKafka string `yaml:"contracts_kafka"`
	Logic          string `yaml:"logic"`
	Constants      string `yaml:"constants"`
	Database       string `yaml:"database"`

	PlayerLocator string `yaml:"player_locator"`
	Login         string `yaml:"login"`
	DB            string `yaml:"db"`
	Chat          string `yaml:"chat"`
	Team          string `yaml:"team"`
	Mail          string `yaml:"mail"`

	Scene string `yaml:"scene"`
	Gate  string `yaml:"gate"`

	Etcd string `yaml:"etcd"`
}

// NodeTypes holds node type configuration.
type NodeTypes struct {
	TcpNode  uint32 `yaml:"tcp_node"`
	GrpcNode uint32 `yaml:"grpc_node"`
	HttpNode uint32 `yaml:"http_node"`
}

// MethodHandlerDirs holds method handler directory mappings.
type MethodHandlerDirs struct {
	Robot                  string `yaml:"robot"`
	SceneNode              string `yaml:"scene_node"`
	SceneNodePlayer        string `yaml:"scene_node_player"`
	SceneNodeReplied       string `yaml:"scene_node_replied"`
	SceneNodePlayerReplied string `yaml:"scene_node_player_replied"`
	SceneNodeGrpc          string `yaml:"scene_node_grpc"`
	GateNode               string `yaml:"gate_node"`
	GateNodeReplied        string `yaml:"gate_node_replied"`
	GateNodePlayer         string `yaml:"gate_node_player"`
	GateNodePlayerReplied  string `yaml:"gate_node_player_replied"`
}

// Generators holds generator toggle configuration.
type Generators struct {
	EnableCpp           bool `yaml:"enable_cpp"`
	EnableGo            bool `yaml:"enable_go"`
	EnableHandler       bool `yaml:"enable_handler"`
	EnableRpcResponse   bool `yaml:"enable_rpc_response"`
	EnableRobotProto    bool `yaml:"enable_robot_proto"`
	EnableRobotGoZero   bool `yaml:"enable_robot_go_zero"`
	EnableAttributeSync bool `yaml:"enable_attribute_sync"`
	EnableUnityClient   bool `yaml:"enable_unity_client"`
}

// Mapping holds template mapping configuration.
type Mapping struct {
	Path     string `yaml:"path"`
	Template string `yaml:"template"`
	Lang     string `yaml:"lang"`
}

// Parser holds proto parser configuration.
type Parser struct {
	IncludePaths []string `yaml:"include_paths"`
	IgnoreFiles  []string `yaml:"ignore_files"`
}

// LogConfig holds logging configuration.
type LogConfig struct {
	Level    string `yaml:"level"`
	Output   string `yaml:"output"`
	FilePath string `yaml:"file_path"`
}

// RpcMeta represents the RPC type.
type RpcMeta struct {
	Type string `yaml:"type"` // grpc | rpc | both | none | etcd
}

// DomainMeta holds domain metadata.
type DomainMeta struct {
	Source  string                       `yaml:"source"`
	Rpc     RpcMeta                      `yaml:"rpc"`     // unified object
	Outputs map[string]map[string]string `yaml:"outputs"` // lang -> type -> dir
}

var (
	// Global is the global config instance.
	Global Config

	// initOnce guards initialization.
	initOnce sync.Once

	// initError stores initialization error.
	initError error
)

// Load loads the configuration file (thread-safe).
func Load() error {
	initOnce.Do(func() {
		initError = loadConfig()
	})
	return initError
}

// loadConfig performs the actual config loading logic.
func loadConfig() error {
	// Check if global logger is initialized
	if logger.Global == nil {
		// Defensive: create a temporary logger (in case main program hasn't initialized)
		tempLogger, err := zap.NewProduction()
		if err != nil {
			return fmt.Errorf("global logger not initialized and failed to create temp logger: %w", err)
		}
		tempLogger.Warn("Global logger not initialized, using temporary logger for config loading")
		defer tempLogger.Sync()

		// Use temporary logger for config loading
		return loadConfigWithLogger(tempLogger)
	}

	// Use global logger for config loading
	return loadConfigWithLogger(logger.Global)
}

// loadConfigWithLogger loads config with the given logger (extracted for reuse).
func loadConfigWithLogger(log *zap.Logger) error {
	// Determine config file path
	filePath := os.Getenv("PROTO_GEN_CONFIG_PATH")
	if filePath == "" {
		// Try current directory first, then etc directory
		if _, err := os.Stat("proto_gen.yaml"); err == nil {
			filePath = "proto_gen.yaml"
		} else if _, err := os.Stat("etc/proto_gen.yaml"); err == nil {
			filePath = "etc/proto_gen.yaml"
		} else {
			return fmt.Errorf("config file not found: proto_gen.yaml missing from both current directory and etc/")
		}
	}

	logger.Global.Info("Loading config file", zap.String("file_path", filePath))

	// Read config file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	if err := yaml.Unmarshal(data, &Global); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	// Resolve path variables (supports nested variables)
	if err := resolvePathVariables(); err != nil {
		return fmt.Errorf("failed to resolve path variables: %w", err)
	}

	// Convert to absolute paths
	if err := resolveAbsolutePaths(); err != nil {
		return fmt.Errorf("failed to resolve absolute paths: %w", err)
	}

	// Set defaults
	setDefaults()

	// Calculate derived constants from original config
	calculateDerivedConstants()

	// Validate config
	if err := validateConfig(); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	// Create required directories
	if err := createRequiredDirs(); err != nil {
		logger.Global.Warn("Failed to create required directories",
			zap.Error(err),
		)
	}

	logger.Global.Info("Config file loaded successfully", zap.String("output_root", Global.Paths.OutputRoot))
	return nil
}

// calculateDerivedConstants computes derived constant values.
func calculateDerivedConstants() {
	// Calculate YourCodePair
	if Global.Naming.YourCodePair == "" && Global.Naming.YourCodeBegin != "" && Global.Naming.YourCodeEnd != "" {
		Global.Naming.YourCodePair = Global.Naming.YourCodeBegin + "\n" + Global.Naming.YourCodeEnd
	}

	// Calculate register handler extensions
	if Global.FileExtensions.RegisterRepliedHandlerCppExtension == "" {
		Global.FileExtensions.RegisterRepliedHandlerCppExtension = "register" + Global.FileExtensions.CppRepliedHandlerEx
	}
	if Global.FileExtensions.RegisterHandlerCppExtension == "" {
		Global.FileExtensions.RegisterHandlerCppExtension = "register" + Global.FileExtensions.HandlerCpp
	}
}

func replaceVariablesInDomainMeta(vars map[string]string) error {
	for domain, meta := range Global.DomainMeta {

		// Replace Source
		meta.Source = replaceVariables(meta.Source, vars)

		// Replace Output
		for lang, outputs := range meta.Outputs {
			for k, v := range outputs {
				meta.Outputs[lang][k] = replaceVariables(v, vars)
			}
		}

		Global.DomainMeta[domain] = meta
	}
	return nil
}

// resolvePathVariables resolves path variables (supports nested variables).
func resolvePathVariables() error {
	// Collect all replaceable variables
	vars := make(map[string]string)

	// Collect Paths variables
	collectStructVariables(reflect.ValueOf(&Global.Paths).Elem(), vars)

	// Collect FileExtensions variables
	collectStructVariables(reflect.ValueOf(&Global.FileExtensions).Elem(), vars)

	// Collect Naming variables
	collectStructVariables(reflect.ValueOf(&Global.Naming).Elem(), vars)

	// Replace variables in all structs
	if err := replaceVariablesInAllStructs(vars); err != nil {
		return err
	}

	// Replace variables in slices
	if err := replaceVariablesInAllSlices(vars); err != nil {
		return err
	}

	return nil
}

// collectStructVariables collects variables from struct fields.
func collectStructVariables(val reflect.Value, vars map[string]string) {
	valType := val.Type()
	for i := 0; i < valType.NumField(); i++ {
		field := valType.Field(i)
		yamlTag := field.Tag.Get("yaml")
		if yamlTag == "" {
			continue
		}
		if val.Field(i).Kind() == reflect.String {
			vars["{{"+yamlTag+"}}"] = val.Field(i).String()
		}
	}
}

// replaceVariablesInAllStructs replaces variables in all structs.
func replaceVariablesInAllStructs(vars map[string]string) error {
	// Replace variables in Paths
	if err := replacePlaceholderInStruct(reflect.ValueOf(&Global.Paths).Elem(), vars); err != nil {
		return err
	}

	// Replace variables in DirectoryNames
	if err := replacePlaceholderInStruct(reflect.ValueOf(&Global.DirectoryNames).Elem(), vars); err != nil {
		return err
	}

	// Replace variables in FileExtensions
	if err := replacePlaceholderInStruct(reflect.ValueOf(&Global.FileExtensions).Elem(), vars); err != nil {
		return err
	}

	// Replace variables in Naming
	if err := replacePlaceholderInStruct(reflect.ValueOf(&Global.Naming).Elem(), vars); err != nil {
		return err
	}

	// Replace variables in MethodHandlerDirectories
	if err := replacePlaceholderInStruct(reflect.ValueOf(&Global.PathLists.MethodHandlerDirectories).Elem(), vars); err != nil {
		return err
	}

	if err := replaceVariablesInDomainMeta(vars); err != nil {
		return err
	}

	return nil
}

// replaceVariablesInAllSlices replaces variables in all slices.
func replaceVariablesInAllSlices(vars map[string]string) error {
	if err := replaceVariablesInSlice(reflect.ValueOf(&Global.PathLists.RobotProtoDirectories), vars); err != nil {
		return err
	}

	if err := replaceVariablesInSlice(reflect.ValueOf(&Global.Parser.IncludePaths), vars); err != nil {
		return err
	}

	if err := replaceVariablesInSlice(reflect.ValueOf(&Global.Parser.IgnoreFiles), vars); err != nil {
		return err
	}

	return nil
}

// replacePlaceholderInStruct replaces variables in a struct's fields.
func replacePlaceholderInStruct(val reflect.Value, vars map[string]string) error {
	valType := val.Type()
	maxIterations := 10 // prevent infinite loop

	for iter := 0; iter < maxIterations; iter++ {
		changed := false

		for i := 0; i < valType.NumField(); i++ {
			field := val.Field(i)
			if field.Kind() != reflect.String {
				continue
			}

			original := field.String()
			resolved := replaceVariables(original, vars)

			if resolved != original {
				field.SetString(resolved)
				changed = true

				// Update vars for subsequent nested variable replacement
				yamlTag := valType.Field(i).Tag.Get("yaml")
				if yamlTag != "" {
					vars["{{"+yamlTag+"}}"] = resolved
				}
			}
		}

		if !changed {
			break
		}

		if iter == maxIterations-1 {
			return fmt.Errorf("variable replacement exceeded max iterations, possible circular reference")
		}
	}

	return nil
}

// replaceVariablesInSlice replaces variables in a string slice.
func replaceVariablesInSlice(val reflect.Value, vars map[string]string) error {
	if val.Kind() != reflect.Ptr || val.Elem().Kind() != reflect.Slice {
		return nil
	}

	slice := val.Elem()
	for i := 0; i < slice.Len(); i++ {
		elem := slice.Index(i)
		if elem.Kind() != reflect.String {
			continue
		}

		original := elem.String()
		resolved := replaceVariables(original, vars)

		if resolved != original {
			elem.SetString(resolved)
		}
	}

	return nil
}

// replaceVariables replaces variables in a string.
func replaceVariables(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// resolveAbsolutePaths converts all paths to absolute paths, using / separator and preserving trailing slashes.
func resolveAbsolutePaths() error {
	// Convert Paths fields to absolute paths
	if err := resolveAbsolutePathsInStruct(reflect.ValueOf(&Global.Paths).Elem()); err != nil {
		return err
	}

	// Convert MethodHandlerDirectories fields to absolute paths
	if err := resolveAbsolutePathsInStruct(reflect.ValueOf(&Global.PathLists.MethodHandlerDirectories).Elem()); err != nil {
		return err
	}

	// Convert slice paths to absolute paths
	if err := resolveAbsolutePathsInSlice(reflect.ValueOf(&Global.PathLists.RobotProtoDirectories)); err != nil {
		return err
	}

	if err := resolveAbsolutePathsInSlice(reflect.ValueOf(&Global.Parser.IncludePaths)); err != nil {
		return err
	}

	// Convert DomainMeta.Source paths to absolute paths
	for domain, meta := range Global.DomainMeta {
		if meta.Source == "" {
			continue
		}
		absPath, err := filepath.Abs(meta.Source)
		if err != nil {
			return fmt.Errorf("failed to resolve DomainMeta[%s].Source path '%s': %w", domain, meta.Source, err)
		}
		meta.Source = formatPathWithSlash(absPath, meta.Source)
		Global.DomainMeta[domain] = meta
	}

	return nil
}

// resolveAbsolutePathsInStruct converts struct field paths to absolute paths.
func resolveAbsolutePathsInStruct(val reflect.Value) error {
	valType := val.Type()

	for i := 0; i < valType.NumField(); i++ {
		field := val.Field(i)
		if field.Kind() != reflect.String {
			continue
		}

		path := field.String()
		if path == "" {
			continue
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("failed to resolve path '%s': %w", path, err)
		}

		// Normalize to / separator and preserve trailing slash
		absPath = formatPathWithSlash(absPath, path)
		field.SetString(absPath)
	}

	return nil
}

// resolveAbsolutePathsInSlice converts string slice paths to absolute paths.
func resolveAbsolutePathsInSlice(val reflect.Value) error {
	if val.Kind() != reflect.Ptr || val.Elem().Kind() != reflect.Slice {
		return nil
	}

	slice := val.Elem()
	for i := 0; i < slice.Len(); i++ {
		elem := slice.Index(i)
		if elem.Kind() != reflect.String {
			continue
		}

		path := elem.String()
		if path == "" {
			continue
		}

		absPath, err := filepath.Abs(path)
		if err == nil {
			absPath = formatPathWithSlash(absPath, path)
			elem.SetString(absPath)
		}
	}

	return nil
}

// formatPathWithSlash normalizes path separator to / and preserves trailing slash based on original path.
func formatPathWithSlash(absPath, originalPath string) string {
	// Replace system separator with /
	absPath = strings.ReplaceAll(absPath, string(filepath.Separator), "/")

	// Check if original path ends with / or system separator
	hasTrailingSlash := strings.HasSuffix(originalPath, "/") ||
		strings.HasSuffix(originalPath, string(filepath.Separator))

	// Append trailing slash if original had one and current doesn't
	if hasTrailingSlash && !strings.HasSuffix(absPath, "/") {
		absPath += "/"
	}

	return absPath
}

// setDefaults sets default values for unconfigured fields.
func setDefaults() {
	// Handler path defaults
	if Global.PathLists.MethodHandlerDirectories.Robot == "" {
		Global.PathLists.MethodHandlerDirectories.Robot = filepath.Join(Global.Paths.OutputRoot, Global.Paths.Robot, "logic/handler/")
	}

	// Logging defaults
	if Global.Log.Level == "" {
		Global.Log.Level = "info"
	}
	if Global.Log.Output == "" {
		Global.Log.Output = "stdout"
	}
	if Global.Log.FilePath == "" && Global.Log.Output == "file" {
		Global.Log.FilePath = "protogen.log"
	}

	// Parser defaults
	if Global.Paths.AllInOneDesc == "" && Global.Paths.ProtoBufDescDir != "" {
		Global.Paths.AllInOneDesc = filepath.Join(Global.Paths.ProtoBufDescDir, "all_in_one.desc")
	}

	// Set DirectoryNames defaults
	if Global.DirectoryNames.GeneratedRpcName == "" {
		Global.DirectoryNames.GeneratedRpcName = "rpc/"
	}
	if Global.DirectoryNames.ServiceInfoName == "" {
		Global.DirectoryNames.ServiceInfoName = "service_metadata/"
	}
	if Global.DirectoryNames.ProtoDirName == "" {
		Global.DirectoryNames.ProtoDirName = "proto/"
	}
	if Global.DirectoryNames.GoZeroProtoDirName == "" {
		Global.DirectoryNames.GoZeroProtoDirName = "go-zero_proto/"
	}
	if Global.DirectoryNames.ModelDirName == "" {
		Global.DirectoryNames.ModelDirName = "model/"
	}
	if Global.DirectoryNames.ServiceIncludeDir == "" {
		Global.DirectoryNames.ServiceIncludeDir = "rpc/"
	}

	// Robot-related defaults
	if Global.Paths.Robot != "" {
		// Use directory names from DirectoryNames config
		protoDirName := Global.DirectoryNames.ProtoDirName
		if protoDirName == "" {
			protoDirName = "proto/"
		}

		goZeroProtoDirName := Global.DirectoryNames.GoZeroProtoDirName
		if goZeroProtoDirName == "" {
			goZeroProtoDirName = "go-zero_proto/"
		}

		if Global.DirectoryNames.RobotProtoName == "" {
			Global.DirectoryNames.RobotProtoName = filepath.Join(Global.Paths.RobotGenerated, protoDirName)
		}
		if Global.DirectoryNames.RobotGoZeroProtoName == "" {
			Global.DirectoryNames.RobotGoZeroProtoName = filepath.Join(Global.DirectoryNames.RobotProtoName, goZeroProtoDirName)
		}

		if Global.Paths.RobotGenerated == "" {
			Global.Paths.RobotGenerated = filepath.Join(Global.Paths.OutputRoot, Global.Paths.Robot, "generated/")
		}
		if Global.Paths.RobotProto == "" {
			Global.Paths.RobotProto = Global.DirectoryNames.RobotProtoName
		}
		if Global.Paths.RobotGeneratedProto == "" {
			Global.Paths.RobotGeneratedProto = Global.DirectoryNames.RobotProtoName
		}
		if Global.Paths.RobotGeneratedOutputDir == "" {
			Global.Paths.RobotGeneratedOutputDir = filepath.Join(Global.Paths.OutputRoot, Global.Paths.Robot, "generated/")
		}
		if Global.Paths.RobotGoGenDir == "" {
			Global.Paths.RobotGoGenDir = filepath.Join(Global.Paths.OutputRoot, Global.Paths.RobotGenerated)
		}
		if Global.Paths.RobotProtoImportPath == "" {
			Global.Paths.RobotProtoImportPath = filepath.Join(Global.Paths.OutputRoot, Global.Paths.RobotProto)
		}
	}

	// Generator toggle defaults
	if !Global.Generators.EnableRobotProto && Global.Paths.Robot != "" {
		Global.Generators.EnableRobotProto = true
	}
	if !Global.Generators.EnableRobotGoZero && Global.Paths.Robot != "" {
		Global.Generators.EnableRobotGoZero = true
	}
	if !Global.Generators.EnableAttributeSync {
		Global.Generators.EnableAttributeSync = true
	}

	if Global.Paths.EventHandlerSourceDirectory == "" {
		Global.Paths.EventHandlerSourceDirectory = "handler/event/"
	}
	if Global.Paths.SceneNodeEventHandlerDirectory == "" && Global.Paths.SceneNodeDir != "" {
		Global.Paths.SceneNodeEventHandlerDirectory = filepath.Join(Global.Paths.SceneNodeDir, Global.Paths.EventHandlerSourceDirectory)
	}
	if Global.Paths.GateNodeEventHandlerDirectory == "" && Global.Paths.GateNodeDir != "" {
		Global.Paths.GateNodeEventHandlerDirectory = filepath.Join(Global.Paths.GateNodeDir, Global.Paths.EventHandlerSourceDirectory)
	}
	if Global.Paths.SceneAttributeSyncDir == "" {
		// matches YAML value: cpp/libs/services/scene/generated/attribute/
		Global.Paths.SceneAttributeSyncDir = filepath.Join(
			Global.Paths.NodeLibGame,
			"scene/generated/attribute/",
		)
	}
}

// validateConfig validates the completeness and correctness of the config.
func validateConfig() error {
	// Check for unresolved variables in key paths
	if err := validatePaths(); err != nil {
		return err
	}

	// Check required directories are configured
	if Global.Paths.OutputRoot == "" {
		return fmt.Errorf("output_root not configured")
	}

	if Global.Paths.ProtoDir == "" {
		return fmt.Errorf("proto_dir not configured")
	}

	// Robot config checks
	if Global.Generators.EnableRobotProto && Global.Paths.Robot == "" {
		return fmt.Errorf("robot proto generation enabled but robot_dir not configured")
	}

	// Logging config checks
	if Global.Log.Output == "file" && Global.Log.FilePath == "" {
		return fmt.Errorf("log output set to file but file_path is empty")
	}

	return nil
}

// validatePaths checks for unresolved variables in paths.
func validatePaths() error {
	// Check Paths struct
	if err := validateStructPaths(reflect.ValueOf(&Global.Paths).Elem()); err != nil {
		return err
	}

	// Check DirectoryNames struct
	if err := validateStructPaths(reflect.ValueOf(&Global.DirectoryNames).Elem()); err != nil {
		return err
	}

	// Check FileExtensions struct
	if err := validateStructPaths(reflect.ValueOf(&Global.FileExtensions).Elem()); err != nil {
		return err
	}

	// Check Naming struct
	if err := validateStructPaths(reflect.ValueOf(&Global.Naming).Elem()); err != nil {
		return err
	}

	// Check handler directories
	if err := validateStructPaths(reflect.ValueOf(&Global.PathLists.MethodHandlerDirectories).Elem()); err != nil {
		return err
	}

	// Check string slices
	if err := validateSlicePaths(reflect.ValueOf(&Global.PathLists.ProtoDirectories), "ProtoDirectories"); err != nil {
		return err
	}

	if err := validateSlicePaths(reflect.ValueOf(&Global.PathLists.RobotProtoDirectories), "RobotProtoDirectories"); err != nil {
		return err
	}

	if err := validateSlicePaths(reflect.ValueOf(&Global.Parser.IncludePaths), "Parser.IncludePaths"); err != nil {
		return err
	}

	return nil
}

// validateStructPaths validates paths in a struct for unresolved variables.
func validateStructPaths(val reflect.Value) error {
	valType := val.Type()

	for i := 0; i < valType.NumField(); i++ {
		field := valType.Field(i)
		value := val.Field(i).String()

		if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
			return fmt.Errorf("path field '%s' contains unresolved variable: %s", field.Name, value)
		}
	}

	return nil
}

// validateSlicePaths validates paths in a string slice for unresolved variables.
func validateSlicePaths(val reflect.Value, name string) error {
	if val.Kind() != reflect.Ptr || val.Elem().Kind() != reflect.Slice {
		return nil
	}

	slice := val.Elem()
	for i := 0; i < slice.Len(); i++ {
		value := slice.Index(i).String()
		if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
			return fmt.Errorf("%s[%d] contains unresolved variable: %s", name, i, value)
		}
	}

	return nil
}

// createRequiredDirs creates necessary directories.
func createRequiredDirs() error {
	dirs := []string{
		Global.Paths.GeneratedOutputDir,
		Global.Paths.GrpcOutputDir,
		Global.Paths.ProtoBufCLuaDir,
		Global.Paths.TempFileGenDir,
		Global.Paths.ProtoBufDescDir,
		Global.Paths.SceneNodeEventHandlerDirectory,
		Global.Paths.GateNodeEventHandlerDirectory,
	}

	// Add DirectoryNames directories
	if Global.DirectoryNames.RobotProtoName != "" {
		dirs = append(dirs, Global.DirectoryNames.RobotProtoName)
	}
	if Global.DirectoryNames.RobotGoZeroProtoName != "" {
		dirs = append(dirs, Global.DirectoryNames.RobotGoZeroProtoName)
	}

	// Add robot-related directories
	if Global.Generators.EnableRobotProto {
		dirs = append(dirs,
			Global.Paths.RobotGenerated,
			Global.Paths.RobotProto,
			Global.Paths.RobotGeneratedProto,
			Global.Paths.RobotGeneratedOutputDir,
			Global.Paths.RobotGoGamePbDir,
		)
	}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// GetTemplatePath returns the full path of a template.
func (c *Config) GetTemplatePath(mappingName string) (string, error) {
	mapping, ok := c.Mappings[mappingName]
	if !ok {
		return "", fmt.Errorf("template mapping '%s' not found", mappingName)
	}
	return mapping.Template, nil
}

// GetOutputPath returns the output path for a generated file.
func (c *Config) GetOutputPath(mappingName, protoDir, protoFile string) (string, error) {
	mapping, ok := c.Mappings[mappingName]
	if !ok {
		return "", fmt.Errorf("template mapping '%s' not found", mappingName)
	}

	path := replaceVariables(mapping.Path, map[string]string{
		"{{proto_dir}}":      protoDir,
		"{{proto_file}}":     protoFile,
		"{{proto_dir_name}}":  c.DirectoryNames.ProtoDirName, // from DirectoryNames config
	})

	return path, nil
}
