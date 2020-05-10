package backup

import (
	"fmt"

	"github.com/greenplum-db/gp-common-go-libs/dbconn"
	"github.com/greenplum-db/gp-common-go-libs/gplog"
	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
)

/*
 * This file contains functions related to validating user input.
 */

func validateFilterLists(opts *options.Options) {
	gplog.Verbose("Validating Tables and Schemas exist in Database")
	ValidateTablesExist(connectionPool, opts.GetIncludedTables(), false)
	ValidateTablesExist(connectionPool, opts.GetExcludedTables(), true)
	ValidateSchemasExist(connectionPool, opts.GetIncludedSchemas(), false)
	ValidateSchemasExist(connectionPool, opts.GetExcludedSchemas(), true)
}

/* 同 DBValidate 类似, 只不过是验证 schema 是否存在. */
func ValidateSchemasExist(connectionPool *dbconn.DBConn, schemaList []string, excludeSet bool) {
	if len(schemaList) == 0 {
		return
	}

	quotedSchemasStr := utils.SliceToQuotedString(schemaList)
	query := fmt.Sprintf("SELECT nspname AS string FROM pg_namespace WHERE nspname IN (%s)", quotedSchemasStr)
	resultSchemas := dbconn.MustSelectStringSlice(connectionPool, query)
	if len(resultSchemas) < len(schemaList) {
		schemaSet := utils.NewSet(resultSchemas)
		for _, schema := range schemaList {
			if !schemaSet.MatchesFilter(schema) {
				if excludeSet {
					gplog.Warn(`Excluded schema %s does not exist`, schema)
				} else {
					gplog.Fatal(nil, "Schema %s does not exist", schema)
				}
			}
		}
	}
}

/* 验证 tableList 中表名是否存在.
 * tableList, 此时应该是 schema.name 这种格式.
 * excludeSet, 控制着当表名不存在时报错行为. 若为 true, 则此时输出 warning, 否则输出 fatal.
 */
func ValidateTablesExist(conn *dbconn.DBConn, tableList []string, excludeSet bool) {
	if len(tableList) == 0 {
		return
	}

	quotedIncludeRelations, err := options.QuoteTableNames(connectionPool, tableList)
	gplog.FatalOnError(err)
	// todo perhaps store quoted list in options??

	quotedTablesStr := utils.SliceToQuotedString(quotedIncludeRelations)
	query := fmt.Sprintf(`
	SELECT
		c.oid,
		n.nspname || '.' || c.relname AS name
	FROM pg_namespace n
	JOIN pg_class c ON n.oid = c.relnamespace
	WHERE quote_ident(n.nspname) || '.' || quote_ident(c.relname) IN (%s)`, quotedTablesStr)
	resultTables := make([]struct {
		Oid  uint32
		Name string
	}, 0)
	err = conn.Select(&resultTables, query)
	gplog.FatalOnError(err, fmt.Sprintf("Query was: %s", query))
	tableMap := make(map[string]uint32)
	for _, table := range resultTables {
		tableMap[table.Name] = table.Oid
	}

	partTableMap := GetPartitionTableMap(conn)
	for _, table := range tableList {
		tableOid := tableMap[table]
		if tableOid == 0 {
			if excludeSet {
				gplog.Warn("Excluded table %s does not exist", table)
			} else {
				gplog.Fatal(nil, "Table %s does not exist", table)
			}
		}
		if partTableMap[tableOid].Level == "i" {
			gplog.Fatal(nil, "Cannot filter on %s, as it is an intermediate partition table.  Only parent partition tables and leaf partition tables may be specified.", table)
		}
	}
}

func validateFlagCombinations(flags *pflag.FlagSet) {
	options.CheckExclusiveFlags(flags, options.DEBUG, options.QUIET, options.VERBOSE)
	options.CheckExclusiveFlags(flags, options.DATA_ONLY, options.METADATA_ONLY, options.INCREMENTAL)
	options.CheckExclusiveFlags(flags, options.INCLUDE_SCHEMA, options.INCLUDE_SCHEMA_FILE, options.INCLUDE_RELATION, options.INCLUDE_RELATION_FILE)
	options.CheckExclusiveFlags(flags, options.EXCLUDE_SCHEMA, options.EXCLUDE_SCHEMA_FILE, options.INCLUDE_SCHEMA, options.INCLUDE_SCHEMA_FILE)
	options.CheckExclusiveFlags(flags, options.EXCLUDE_SCHEMA, options.EXCLUDE_SCHEMA_FILE, options.EXCLUDE_RELATION, options.INCLUDE_RELATION, options.EXCLUDE_RELATION_FILE, options.INCLUDE_RELATION_FILE)
	options.CheckExclusiveFlags(flags, options.EXCLUDE_RELATION, options.EXCLUDE_RELATION_FILE, options.LEAF_PARTITION_DATA)
	options.CheckExclusiveFlags(flags, options.JOBS, options.METADATA_ONLY, options.SINGLE_DATA_FILE)
	options.CheckExclusiveFlags(flags, options.METADATA_ONLY, options.LEAF_PARTITION_DATA)
	options.CheckExclusiveFlags(flags, options.NO_COMPRESSION, options.COMPRESSION_LEVEL)
	options.CheckExclusiveFlags(flags, options.PLUGIN_CONFIG, options.BACKUP_DIR)
	if MustGetFlagString(options.FROM_TIMESTAMP) != "" && !MustGetFlagBool(options.INCREMENTAL) {
		gplog.Fatal(errors.Errorf("--from-timestamp must be specified with --incremental"), "")
	}
	if MustGetFlagBool(options.INCREMENTAL) && !MustGetFlagBool(options.LEAF_PARTITION_DATA) {
		gplog.Fatal(errors.Errorf("--leaf-partition-data must be specified with --incremental"), "")
	}
}

func validateFlagValues() {
	err := utils.ValidateFullPath(MustGetFlagString(options.BACKUP_DIR))
	gplog.FatalOnError(err)
	err = utils.ValidateFullPath(MustGetFlagString(options.PLUGIN_CONFIG))
	gplog.FatalOnError(err)
	err = utils.ValidateCompressionLevel(MustGetFlagInt(options.COMPRESSION_LEVEL))
	gplog.FatalOnError(err)
	if MustGetFlagString(options.FROM_TIMESTAMP) != "" && !filepath.IsValidTimestamp(MustGetFlagString(options.FROM_TIMESTAMP)) {
		gplog.Fatal(errors.Errorf("Timestamp %s is invalid.  Timestamps must be in the format YYYYMMDDHHMMSS.",
			MustGetFlagString(options.FROM_TIMESTAMP)), "")
	}
}

func validateFromTimestamp(fromTimestamp string) {
	fromTimestampFPInfo := filepath.NewFilePathInfo(globalCluster, globalFPInfo.UserSpecifiedBackupDir,
		fromTimestamp, globalFPInfo.UserSpecifiedSegPrefix)
	if MustGetFlagString(options.PLUGIN_CONFIG) != "" {
		// The config file needs to be downloaded from the remote system into the local filesystem
		pluginConfig.MustRestoreFile(fromTimestampFPInfo.GetConfigFilePath())
	}
	fromBackupConfig := history.ReadConfigFile(fromTimestampFPInfo.GetConfigFilePath())

	if !matchesIncrementalFlags(fromBackupConfig, &backupReport.BackupConfig) {
		gplog.Fatal(errors.Errorf("The flags of the backup with timestamp = %s does not match "+
			"that of the current one. Please refer to the report to view the flags supplied for the"+
			"previous backup.", fromTimestampFPInfo.Timestamp), "")
	}
}
