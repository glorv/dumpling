// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	tcontext "github.com/pingcap/dumpling/v4/context"

	"github.com/coreos/go-semver/semver"
	"github.com/docker/go-units"
	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/errors"
	filter "github.com/pingcap/tidb-tools/pkg/table-filter"
	"github.com/pingcap/tidb-tools/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
)

const (
	flagDatabase                 = "database"
	flagTablesList               = "tables-list"
	flagHost                     = "host"
	flagUser                     = "user"
	flagPort                     = "port"
	flagPassword                 = "password"
	flagAllowCleartextPasswords  = "allow-cleartext-passwords"
	flagThreads                  = "threads"
	flagFilesize                 = "filesize"
	flagStatementSize            = "statement-size"
	flagOutput                   = "output"
	flagLoglevel                 = "loglevel"
	flagLogfile                  = "logfile"
	flagLogfmt                   = "logfmt"
	flagConsistency              = "consistency"
	flagSnapshot                 = "snapshot"
	flagNoViews                  = "no-views"
	flagStatusAddr               = "status-addr"
	flagRows                     = "rows"
	flagWhere                    = "where"
	flagEscapeBackslash          = "escape-backslash"
	flagFiletype                 = "filetype"
	flagNoHeader                 = "no-header"
	flagNoSchemas                = "no-schemas"
	flagNoData                   = "no-data"
	flagCsvNullValue             = "csv-null-value"
	flagSQL                      = "sql"
	flagFilter                   = "filter"
	flagCaseSensitive            = "case-sensitive"
	flagDumpEmptyDatabase        = "dump-empty-database"
	flagTidbMemQuotaQuery        = "tidb-mem-quota-query"
	flagCA                       = "ca"
	flagCert                     = "cert"
	flagKey                      = "key"
	flagCsvSeparator             = "csv-separator"
	flagCsvDelimiter             = "csv-delimiter"
	flagOutputFilenameTemplate   = "output-filename-template"
	flagCompleteInsert           = "complete-insert"
	flagParams                   = "params"
	flagReadTimeout              = "read-timeout"
	flagTransactionalConsistency = "transactional-consistency"
	flagCompress                 = "compress"

	// FlagHelp represents the help flag
	FlagHelp = "help"
)

// Config is the dump config for dumpling
type Config struct {
	storage.BackendOptions

	AllowCleartextPasswords  bool
	SortByPk                 bool
	NoViews                  bool
	NoHeader                 bool
	NoSchemas                bool
	NoData                   bool
	CompleteInsert           bool
	TransactionalConsistency bool
	EscapeBackslash          bool
	DumpEmptyDatabase        bool
	PosAfterConnect          bool
	CompressType             storage.CompressType

	Host     string
	Port     int
	Threads  int
	User     string
	Password string `json:"-"`
	Security struct {
		CAPath   string
		CertPath string
		KeyPath  string
	}

	LogLevel      string
	LogFile       string
	LogFormat     string
	OutputDirPath string
	StatusAddr    string
	Snapshot      string
	Consistency   string
	CsvNullValue  string
	SQL           string
	CsvSeparator  string
	CsvDelimiter  string
	Databases     []string

	TableFilter        filter.Filter `json:"-"`
	Where              string
	FileType           string
	ServerInfo         ServerInfo
	Logger             *zap.Logger        `json:"-"`
	OutputFileTemplate *template.Template `json:"-"`
	Rows               uint64
	ReadTimeout        time.Duration
	TiDBMemQuotaQuery  uint64
	FileSize           uint64
	StatementSize      uint64
	SessionParams      map[string]interface{}
	Labels             prometheus.Labels `json:"-"`
	Tables             DatabaseTables
}

// DefaultConfig returns the default export Config for dumpling
func DefaultConfig() *Config {
	allFilter, _ := filter.Parse([]string{"*.*"})
	return &Config{
		Databases:          nil,
		Host:               "127.0.0.1",
		User:               "root",
		Port:               3306,
		Password:           "",
		Threads:            4,
		Logger:             nil,
		StatusAddr:         ":8281",
		FileSize:           UnspecifiedSize,
		StatementSize:      DefaultStatementSize,
		OutputDirPath:      ".",
		ServerInfo:         ServerInfoUnknown,
		SortByPk:           true,
		Tables:             nil,
		Snapshot:           "",
		Consistency:        consistencyTypeAuto,
		NoViews:            true,
		Rows:               UnspecifiedSize,
		Where:              "",
		FileType:           "",
		NoHeader:           false,
		NoSchemas:          false,
		NoData:             false,
		CsvNullValue:       "\\N",
		SQL:                "",
		TableFilter:        allFilter,
		DumpEmptyDatabase:  true,
		SessionParams:      make(map[string]interface{}),
		OutputFileTemplate: DefaultOutputFileTemplate,
		PosAfterConnect:    false,
	}
}

// String returns dumpling's config in json format
func (conf *Config) String() string {
	cfg, err := json.Marshal(conf)
	if err != nil && conf.Logger != nil {
		conf.Logger.Error("fail to marshal config to json", zap.Error(err))
	}
	return string(cfg)
}

// GetDSN generates DSN from Config
func (conf *Config) GetDSN(db string) string {
	// maxAllowedPacket=0 can be used to automatically fetch the max_allowed_packet variable from server on every connection.
	// https://github.com/go-sql-driver/mysql#maxallowedpacket
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?collation=utf8mb4_general_ci&readTimeout=%s&writeTimeout=30s&interpolateParams=true&maxAllowedPacket=0",
		conf.User, conf.Password, conf.Host, conf.Port, db, conf.ReadTimeout)
	if len(conf.Security.CAPath) > 0 {
		dsn += "&tls=dumpling-tls-target"
	}
	if conf.AllowCleartextPasswords {
		dsn += "&allowCleartextPasswords=1"
	}
	return dsn
}

func timestampDirName() string {
	return fmt.Sprintf("./export-%s", time.Now().Format(time.RFC3339))
}

// DefineFlags defines flags of dumpling's configuration
func (conf *Config) DefineFlags(flags *pflag.FlagSet) {
	storage.DefineFlags(flags)
	flags.StringSliceP(flagDatabase, "B", nil, "Databases to dump")
	flags.StringSliceP(flagTablesList, "T", nil, "Comma delimited table list to dump; must be qualified table names")
	flags.StringP(flagHost, "h", "127.0.0.1", "The host to connect to")
	flags.StringP(flagUser, "u", "root", "Username with privileges to run the dump")
	flags.IntP(flagPort, "P", 4000, "TCP/IP port to connect to")
	flags.StringP(flagPassword, "p", "", "User password")
	flags.Bool(flagAllowCleartextPasswords, false, "Allow passwords to be sent in cleartext (warning: don't use without TLS)")
	flags.IntP(flagThreads, "t", 4, "Number of goroutines to use, default 4")
	flags.StringP(flagFilesize, "F", "", "The approximate size of output file")
	flags.Uint64P(flagStatementSize, "s", DefaultStatementSize, "Attempted size of INSERT statement in bytes")
	flags.StringP(flagOutput, "o", timestampDirName(), "Output directory")
	flags.String(flagLoglevel, "info", "Log level: {debug|info|warn|error|dpanic|panic|fatal}")
	flags.StringP(flagLogfile, "L", "", "Log file `path`, leave empty to write to console")
	flags.String(flagLogfmt, "text", "Log `format`: {text|json}")
	flags.String(flagConsistency, consistencyTypeAuto, "Consistency level during dumping: {auto|none|flush|lock|snapshot}")
	flags.String(flagSnapshot, "", "Snapshot position (uint64 from pd timestamp for TiDB). Valid only when consistency=snapshot")
	flags.BoolP(flagNoViews, "W", true, "Do not dump views")
	flags.String(flagStatusAddr, ":8281", "dumpling API server and pprof addr")
	flags.Uint64P(flagRows, "r", UnspecifiedSize, "Split table into chunks of this many rows, default unlimited")
	flags.String(flagWhere, "", "Dump only selected records")
	flags.Bool(flagEscapeBackslash, true, "use backslash to escape special characters")
	flags.String(flagFiletype, "", "The type of export file (sql/csv)")
	flags.Bool(flagNoHeader, false, "whether not to dump CSV table header")
	flags.BoolP(flagNoSchemas, "m", false, "Do not dump table schemas with the data")
	flags.BoolP(flagNoData, "d", false, "Do not dump table data")
	flags.String(flagCsvNullValue, "\\N", "The null value used when export to csv")
	flags.StringP(flagSQL, "S", "", "Dump data with given sql. This argument doesn't support concurrent dump")
	_ = flags.MarkHidden(flagSQL)
	flags.StringSliceP(flagFilter, "f", []string{"*.*", DefaultTableFilter}, "filter to select which tables to dump")
	flags.Bool(flagCaseSensitive, false, "whether the filter should be case-sensitive")
	flags.Bool(flagDumpEmptyDatabase, true, "whether to dump empty database")
	flags.Uint64(flagTidbMemQuotaQuery, UnspecifiedSize, "The maximum memory limit for a single SQL statement, in bytes.")
	flags.String(flagCA, "", "The path name to the certificate authority file for TLS connection")
	flags.String(flagCert, "", "The path name to the client certificate file for TLS connection")
	flags.String(flagKey, "", "The path name to the client private key file for TLS connection")
	flags.String(flagCsvSeparator, ",", "The separator for csv files, default ','")
	flags.String(flagCsvDelimiter, "\"", "The delimiter for values in csv files, default '\"'")
	flags.String(flagOutputFilenameTemplate, "", "The output filename template (without file extension)")
	flags.Bool(flagCompleteInsert, false, "Use complete INSERT statements that include column names")
	flags.StringToString(flagParams, nil, `Extra session variables used while dumping, accepted format: --params "character_set_client=latin1,character_set_connection=latin1"`)
	flags.Bool(FlagHelp, false, "Print help message and quit")
	flags.Duration(flagReadTimeout, 15*time.Minute, "I/O read timeout for db connection.")
	_ = flags.MarkHidden(flagReadTimeout)
	flags.Bool(flagTransactionalConsistency, true, "Only support transactional consistency")
	_ = flags.MarkHidden(flagTransactionalConsistency)
	flags.StringP(flagCompress, "c", "", "Compress output file type, support 'gzip', 'no-compression' now")
}

// ParseFromFlags parses dumpling's export.Config from flags
// nolint: gocyclo
func (conf *Config) ParseFromFlags(flags *pflag.FlagSet) error {
	var err error
	conf.Databases, err = flags.GetStringSlice(flagDatabase)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Host, err = flags.GetString(flagHost)
	if err != nil {
		return errors.Trace(err)
	}
	conf.User, err = flags.GetString(flagUser)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Port, err = flags.GetInt(flagPort)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Password, err = flags.GetString(flagPassword)
	if err != nil {
		return errors.Trace(err)
	}
	conf.AllowCleartextPasswords, err = flags.GetBool(flagAllowCleartextPasswords)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Threads, err = flags.GetInt(flagThreads)
	if err != nil {
		return errors.Trace(err)
	}
	conf.StatementSize, err = flags.GetUint64(flagStatementSize)
	if err != nil {
		return errors.Trace(err)
	}
	conf.OutputDirPath, err = flags.GetString(flagOutput)
	if err != nil {
		return errors.Trace(err)
	}
	conf.LogLevel, err = flags.GetString(flagLoglevel)
	if err != nil {
		return errors.Trace(err)
	}
	conf.LogFile, err = flags.GetString(flagLogfile)
	if err != nil {
		return errors.Trace(err)
	}
	conf.LogFormat, err = flags.GetString(flagLogfmt)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Consistency, err = flags.GetString(flagConsistency)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Snapshot, err = flags.GetString(flagSnapshot)
	if err != nil {
		return errors.Trace(err)
	}
	conf.NoViews, err = flags.GetBool(flagNoViews)
	if err != nil {
		return errors.Trace(err)
	}
	conf.StatusAddr, err = flags.GetString(flagStatusAddr)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Rows, err = flags.GetUint64(flagRows)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Where, err = flags.GetString(flagWhere)
	if err != nil {
		return errors.Trace(err)
	}
	conf.EscapeBackslash, err = flags.GetBool(flagEscapeBackslash)
	if err != nil {
		return errors.Trace(err)
	}
	conf.FileType, err = flags.GetString(flagFiletype)
	if err != nil {
		return errors.Trace(err)
	}
	conf.NoHeader, err = flags.GetBool(flagNoHeader)
	if err != nil {
		return errors.Trace(err)
	}
	conf.NoSchemas, err = flags.GetBool(flagNoSchemas)
	if err != nil {
		return errors.Trace(err)
	}
	conf.NoData, err = flags.GetBool(flagNoData)
	if err != nil {
		return errors.Trace(err)
	}
	conf.CsvNullValue, err = flags.GetString(flagCsvNullValue)
	if err != nil {
		return errors.Trace(err)
	}
	conf.SQL, err = flags.GetString(flagSQL)
	if err != nil {
		return errors.Trace(err)
	}
	conf.DumpEmptyDatabase, err = flags.GetBool(flagDumpEmptyDatabase)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Security.CAPath, err = flags.GetString(flagCA)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Security.CertPath, err = flags.GetString(flagCert)
	if err != nil {
		return errors.Trace(err)
	}
	conf.Security.KeyPath, err = flags.GetString(flagKey)
	if err != nil {
		return errors.Trace(err)
	}
	conf.CsvSeparator, err = flags.GetString(flagCsvSeparator)
	if err != nil {
		return errors.Trace(err)
	}
	conf.CsvDelimiter, err = flags.GetString(flagCsvDelimiter)
	if err != nil {
		return errors.Trace(err)
	}
	conf.CompleteInsert, err = flags.GetBool(flagCompleteInsert)
	if err != nil {
		return errors.Trace(err)
	}
	conf.ReadTimeout, err = flags.GetDuration(flagReadTimeout)
	if err != nil {
		return errors.Trace(err)
	}
	conf.TransactionalConsistency, err = flags.GetBool(flagTransactionalConsistency)
	if err != nil {
		return errors.Trace(err)
	}
	conf.TiDBMemQuotaQuery, err = flags.GetUint64(flagTidbMemQuotaQuery)
	if err != nil {
		return errors.Trace(err)
	}

	if conf.Threads <= 0 {
		return errors.Errorf("--threads is set to %d. It should be greater than 0", conf.Threads)
	}
	if len(conf.CsvSeparator) == 0 {
		return errors.New("--csv-separator is set to \"\". It must not be an empty string")
	}

	if conf.SessionParams == nil {
		conf.SessionParams = make(map[string]interface{})
	}

	tablesList, err := flags.GetStringSlice(flagTablesList)
	if err != nil {
		return errors.Trace(err)
	}
	fileSizeStr, err := flags.GetString(flagFilesize)
	if err != nil {
		return errors.Trace(err)
	}
	filters, err := flags.GetStringSlice(flagFilter)
	if err != nil {
		return errors.Trace(err)
	}
	caseSensitive, err := flags.GetBool(flagCaseSensitive)
	if err != nil {
		return errors.Trace(err)
	}
	outputFilenameFormat, err := flags.GetString(flagOutputFilenameTemplate)
	if err != nil {
		return errors.Trace(err)
	}
	params, err := flags.GetStringToString(flagParams)
	if err != nil {
		return errors.Trace(err)
	}

	conf.TableFilter, err = ParseTableFilter(tablesList, filters)
	if err != nil {
		return errors.Errorf("failed to parse filter: %s", err)
	}

	if !caseSensitive {
		conf.TableFilter = filter.CaseInsensitive(conf.TableFilter)
	}

	conf.FileSize, err = ParseFileSize(fileSizeStr)
	if err != nil {
		return errors.Trace(err)
	}

	if outputFilenameFormat == "" && conf.SQL != "" {
		outputFilenameFormat = DefaultAnonymousOutputFileTemplateText
	}
	tmpl, err := ParseOutputFileTemplate(outputFilenameFormat)
	if err != nil {
		return errors.Errorf("failed to parse output filename template (--output-filename-template '%s')\n", outputFilenameFormat)
	}
	conf.OutputFileTemplate = tmpl

	compressType, err := flags.GetString(flagCompress)
	if err != nil {
		return errors.Trace(err)
	}
	conf.CompressType, err = ParseCompressType(compressType)
	if err != nil {
		return errors.Trace(err)
	}

	for k, v := range params {
		conf.SessionParams[k] = v
	}

	err = conf.BackendOptions.ParseFromFlags(pflag.CommandLine)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

// ParseFileSize parses file size from tables-list and filter arguments
func ParseFileSize(fileSizeStr string) (uint64, error) {
	if len(fileSizeStr) == 0 {
		return UnspecifiedSize, nil
	} else if fileSizeMB, err := strconv.ParseUint(fileSizeStr, 10, 64); err == nil {
		fmt.Printf("Warning: -F without unit is not recommended, try using `-F '%dMiB'` in the future\n", fileSizeMB)
		return fileSizeMB * units.MiB, nil
	} else if size, err := units.RAMInBytes(fileSizeStr); err == nil {
		return uint64(size), nil
	}
	return 0, errors.Errorf("failed to parse filesize (-F '%s')", fileSizeStr)
}

// ParseTableFilter parses table filter from tables-list and filter arguments
func ParseTableFilter(tablesList, filters []string) (filter.Filter, error) {
	if len(tablesList) == 0 {
		return filter.Parse(filters)
	}

	// only parse -T when -f is default value. otherwise bail out.
	if !sameStringArray(filters, []string{"*.*", DefaultTableFilter}) {
		return nil, errors.New("cannot pass --tables-list and --filter together")
	}

	tableNames := make([]filter.Table, 0, len(tablesList))
	for _, table := range tablesList {
		parts := strings.SplitN(table, ".", 2)
		if len(parts) < 2 {
			return nil, errors.Errorf("--tables-list only accepts qualified table names, but `%s` lacks a dot", table)
		}
		tableNames = append(tableNames, filter.Table{Schema: parts[0], Name: parts[1]})
	}

	return filter.NewTablesFilter(tableNames...), nil
}

// ParseCompressType parses compressType string to storage.CompressType
func ParseCompressType(compressType string) (storage.CompressType, error) {
	switch compressType {
	case "", "no-compression":
		return storage.NoCompression, nil
	case "gzip", "gz":
		return storage.Gzip, nil
	default:
		return storage.NoCompression, errors.Errorf("unknown compress type %s", compressType)
	}
}

func (conf *Config) createExternalStorage(ctx context.Context) (storage.ExternalStorage, error) {
	b, err := storage.ParseBackend(conf.OutputDirPath, &conf.BackendOptions)
	if err != nil {
		return nil, errors.Trace(err)
	}
	httpClient := http.DefaultClient
	httpClient.Timeout = 30 * time.Second
	maxIdleConnsPerHost := http.DefaultMaxIdleConnsPerHost
	if conf.Threads > maxIdleConnsPerHost {
		maxIdleConnsPerHost = conf.Threads
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = maxIdleConnsPerHost
	httpClient.Transport = transport

	return storage.New(ctx, b, &storage.ExternalStorageOptions{
		HTTPClient: httpClient,
	})
}

const (
	// UnspecifiedSize means the filesize/statement-size is unspecified
	UnspecifiedSize = 0
	// DefaultStatementSize is the default statement size
	DefaultStatementSize = 1000000
	// TiDBMemQuotaQueryName is the session variable TiDBMemQuotaQuery's name in TiDB
	TiDBMemQuotaQueryName = "tidb_mem_quota_query"
	// DefaultTableFilter is the default exclude table filter. It will exclude all system databases
	DefaultTableFilter = "!/^(mysql|sys|INFORMATION_SCHEMA|PERFORMANCE_SCHEMA|METRICS_SCHEMA|INSPECTION_SCHEMA)$/.*"

	defaultDumpThreads        = 128
	defaultDumpGCSafePointTTL = 5 * 60
	defaultEtcdDialTimeOut    = 3 * time.Second

	dumplingServiceSafePointPrefix = "dumpling"
)

var (
	gcSafePointVersion = semver.New("4.0.0")
	tableSampleVersion = semver.New("5.0.0-nightly")
)

// ServerInfo is the combination of ServerType and ServerInfo
type ServerInfo struct {
	ServerType    ServerType
	ServerVersion *semver.Version
}

// ServerInfoUnknown is the unknown database type to dumpling
var ServerInfoUnknown = ServerInfo{
	ServerType:    ServerTypeUnknown,
	ServerVersion: nil,
}

var (
	versionRegex     = regexp.MustCompile(`^\d+\.\d+\.\d+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
	tidbVersionRegex = regexp.MustCompile(`-[v]?\d+\.\d+\.\d+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
)

// ParseServerInfo parses exported server type and version info from version string
func ParseServerInfo(tctx *tcontext.Context, src string) ServerInfo {
	tctx.L().Debug("parse server info", zap.String("server info string", src))
	lowerCase := strings.ToLower(src)
	serverInfo := ServerInfo{}
	switch {
	case strings.Contains(lowerCase, "tidb"):
		serverInfo.ServerType = ServerTypeTiDB
	case strings.Contains(lowerCase, "mariadb"):
		serverInfo.ServerType = ServerTypeMariaDB
	case versionRegex.MatchString(lowerCase):
		serverInfo.ServerType = ServerTypeMySQL
	default:
		serverInfo.ServerType = ServerTypeUnknown
	}

	tctx.L().Info("detect server type",
		zap.String("type", serverInfo.ServerType.String()))

	var versionStr string
	if serverInfo.ServerType == ServerTypeTiDB {
		versionStr = tidbVersionRegex.FindString(src)[1:]
		versionStr = strings.TrimPrefix(versionStr, "v")
	} else {
		versionStr = versionRegex.FindString(src)
	}

	var err error
	serverInfo.ServerVersion, err = semver.NewVersion(versionStr)
	if err != nil {
		tctx.L().Warn("fail to parse version",
			zap.String("version", versionStr))
		return serverInfo
	}

	tctx.L().Info("detect server version",
		zap.String("version", serverInfo.ServerVersion.String()))
	return serverInfo
}

// ServerType represents the type of database to export
type ServerType int8

// String implements Stringer.String
func (s ServerType) String() string {
	if s >= ServerTypeAll {
		return ""
	}
	return serverTypeString[s]
}

const (
	// ServerTypeUnknown represents unknown server type
	ServerTypeUnknown = iota
	// ServerTypeMySQL represents MySQL server type
	ServerTypeMySQL
	// ServerTypeMariaDB represents MariaDB server type
	ServerTypeMariaDB
	// ServerTypeTiDB represents TiDB server type
	ServerTypeTiDB

	// ServerTypeAll represents All server types
	ServerTypeAll
)

var serverTypeString = []string{
	ServerTypeUnknown: "Unknown",
	ServerTypeMySQL:   "MySQL",
	ServerTypeMariaDB: "MariaDB",
	ServerTypeTiDB:    "TiDB",
}

func adjustConfig(conf *Config, fns ...func(*Config) error) error {
	for _, f := range fns {
		err := f(conf)
		if err != nil {
			return err
		}
	}
	return nil
}

func registerTLSConfig(conf *Config) error {
	if len(conf.Security.CAPath) > 0 {
		tlsConfig, err := utils.ToTLSConfig(conf.Security.CAPath, conf.Security.CertPath, conf.Security.KeyPath)
		if err != nil {
			return errors.Trace(err)
		}
		err = mysql.RegisterTLSConfig("dumpling-tls-target", tlsConfig)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func validateSpecifiedSQL(conf *Config) error {
	if conf.SQL != "" && conf.Where != "" {
		return errors.New("can't specify both --sql and --where at the same time. Please try to combine them into --sql")
	}
	return nil
}

func adjustFileFormat(conf *Config) error {
	conf.FileType = strings.ToLower(conf.FileType)
	switch conf.FileType {
	case "":
		if conf.SQL != "" {
			conf.FileType = FileFormatCSVString
		} else {
			conf.FileType = FileFormatSQLTextString
		}
	case FileFormatSQLTextString:
		if conf.SQL != "" {
			return errors.Errorf("unsupported config.FileType '%s' when we specify --sql, please unset --filetype or set it to 'csv'", conf.FileType)
		}
	case FileFormatCSVString:
	default:
		return errors.Errorf("unknown config.FileType '%s'", conf.FileType)
	}
	return nil
}
