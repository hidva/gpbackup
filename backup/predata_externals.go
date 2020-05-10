package backup

/*
 * This file contains structs and functions related to backing up metadata on the
 * master for objects that connect to external data (external tables and external
 * protocols).
 */

import (
	"fmt"
	"strings"

	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
)

const (
	// Type of external table
	READABLE = iota
	READABLE_WEB
	WRITABLE
	WRITABLE_WEB
	// Protocol external table is using
	FILE
	GPFDIST
	GPHDFS
	HTTP
	S3
)

// ExternalTableDefinition, 存放着一个 external table 相关元信息.
type ExternalTableDefinition struct {
	Oid             uint32
	Type            int
	Protocol        int
	Location        string
	ExecLocation    string
	FormatType      string
	FormatOpts      string
	Options         string
	Command         string
	RejectLimit     int
	RejectLimitType string
	ErrTableName    string
	ErrTableSchema  string
	Encoding        string
	Writable        bool
	URIs            []string
}

func PrintExternalTableCreateStatement(metadataFile *utils.FileWithByteCount, toc *toc.TOC, table Table) {
	start := metadataFile.ByteCount
	tableTypeStrMap := map[int]string{
		READABLE:     "READABLE EXTERNAL",
		READABLE_WEB: "READABLE EXTERNAL WEB",
		WRITABLE:     "WRITABLE EXTERNAL",
		WRITABLE_WEB: "WRITABLE EXTERNAL WEB",
	}
	extTableDef := table.ExtTableDef
	extTableDef.Type, extTableDef.Protocol = DetermineExternalTableCharacteristics(extTableDef)
	metadataFile.MustPrintf("\n\nCREATE %s TABLE %s (\n", tableTypeStrMap[extTableDef.Type], table.FQN())
	printColumnDefinitions(metadataFile, table.ColumnDefs, "")
	metadataFile.MustPrintf(") ")
	PrintExternalTableStatements(metadataFile, table.FQN(), extTableDef)
	if extTableDef.Writable {
		metadataFile.MustPrintf("\n%s", table.DistPolicy)
	}
	metadataFile.MustPrintf(";")
	if toc != nil {
		section, entry := table.GetMetadataEntry()
		toc.AddMetadataEntry(section, entry, start, metadataFile.ByteCount)
	}
}

func DetermineExternalTableCharacteristics(extTableDef ExternalTableDefinition) (int, int) {
	isWritable := extTableDef.Writable
	var tableType int
	tableProtocol := -1
	if extTableDef.Location == "" { // EXTERNAL WEB tables may have EXECUTE instead of LOCATION
		tableProtocol = HTTP
		if isWritable {
			tableType = WRITABLE_WEB
		} else {
			tableType = READABLE_WEB
		}
	} else {
		/*
		 * All data sources must use the same protocol, so we can use Location to determine
		 * the table's protocol even though it only holds one data source URI.
		 */
		isWeb := strings.HasPrefix(extTableDef.Location, "http")
		if isWeb && isWritable {
			tableType = WRITABLE_WEB
		} else if isWeb && !isWritable {
			tableType = READABLE_WEB
		} else if !isWeb && isWritable {
			tableType = WRITABLE
		} else {
			tableType = READABLE
		}
		prefix := extTableDef.Location[0:strings.Index(extTableDef.Location, "://")]
		switch prefix {
		case "file":
			tableProtocol = FILE
		case "gpfdist":
			tableProtocol = GPFDIST
		case "gpfdists":
			tableProtocol = GPFDIST
		case "gphdfs":
			tableProtocol = GPHDFS
		case "http":
			tableProtocol = HTTP
		case "https":
			tableProtocol = HTTP
		case "s3":
			tableProtocol = S3
		}
	}
	return tableType, tableProtocol
}

func generateExecuteStatement(extTableDef ExternalTableDefinition) string {
	var executeStatement string

	extTableDef.Command = strings.Replace(extTableDef.Command, `'`, `''`, -1)
	executeStatement += fmt.Sprintf("EXECUTE '%s'", extTableDef.Command)
	execType := strings.Split(extTableDef.ExecLocation, ":")
	switch execType[0] {
	case "ALL_SEGMENTS": // Default case, don't print anything else
	case "HOST":
		executeStatement += fmt.Sprintf(" ON HOST '%s'", execType[1])
	case "MASTER_ONLY":
		executeStatement += " ON MASTER"
	case "PER_HOST":
		executeStatement += " ON HOST"
	case "SEGMENT_ID":
		executeStatement += fmt.Sprintf(" ON SEGMENT %s", execType[1])
	case "TOTAL_SEGS":
		executeStatement += fmt.Sprintf(" ON %s", execType[1])
	}

	return executeStatement
}

/*
 * This function is adapted from dumputils.c
 *
 * Escape backslashes and apostrophes in EXTERNAL TABLE format strings.
 * Returns a list of unquoted keyword and escaped quoted string tokens
 *
 * The fmtopts field of a pg_exttable tuple has an odd encoding -- it is
 * partially parsed and contains "string" values that aren't legal SQL.
 * Each string value is delimited by apostrophes and is usually, but not
 * always, a single character.	The fmtopts field is typically something
 * like {delimiter '\x09' null '\N' escape '\'} or
 * {delimiter ',' null '' escape '\' quote '''}.  Each backslash and
 * apostrophe in a string must be escaped and each string must be
 * prepended with an 'E' denoting an "escape syntax" string.
 *
 * Usage note: A field value containing an apostrophe followed by a space
 * will throw this algorithm off -- it presumes no embedded spaces.
 */
func tokenizeAndEscapeFormatOpts(formatOpts string) []string {
	inString := false
	resultList := make([]string, 0)
	currString := ""

	for i := 0; i < len(formatOpts); i++ {
		switch formatOpts[i] {
		case '\'':
			if inString {
				/*
				 * Escape apostrophes *within* the string.	If the
				 * apostrophe is at the end of the source string or is
				 * followed by a space, it is presumed to be a closing
				 * apostrophe and is not escaped.
				 */
				if (i+1) == len(formatOpts) || formatOpts[i+1] == ' ' {
					inString = false
				} else {
					currString += "\\"
				}
			} else {
				currString = "E"
				inString = true
			}
		case '\\':
			currString += "\\"
		case ' ':
			if !inString {
				resultList = append(resultList, currString)
				currString = ""
				continue
			}
		}
		currString += string(formatOpts[i])
	}
	resultList = append(resultList, currString)

	return resultList
}

/*
 * Format options to use `a = b` format because this format is required
 * when using CUSTOM format.
 *
 * We do this for CUSTOM, AVRO and PARQUET, but not CSV or TEXT because
 * CSV and TEXT have some multi-word options that are difficult
 * to parse into this format
 */
func makeCustomFormatOpts(tokens []string) string {
	var key string
	var value string
	resultOpts := make([]string, 0)

	for i := 0; i < len(tokens)-1; i += 2 {
		key = tokens[i]
		value = tokens[i+1]
		opt := fmt.Sprintf(`%s = %s`, key, value)
		resultOpts = append(resultOpts, opt)
	}
	return strings.Join(resultOpts, ", ")
}

func GenerateFormatStatement(extTableDef ExternalTableDefinition) string {
	var formatStatement string
	formatType := ""
	switch extTableDef.FormatType {
	case "t":
		formatType = "TEXT"
	case "c":
		formatType = "CSV"
	case "b":
		formatType = "CUSTOM"
	case "a":
		formatType = "AVRO"
	case "p":
		formatType = "PARQUET"
	}
	formatStatement += fmt.Sprintf("FORMAT '%s'", formatType)

	if extTableDef.FormatOpts != "" {
		formatTokens := tokenizeAndEscapeFormatOpts(strings.TrimSpace(extTableDef.FormatOpts))
		formatOptsString := ""
		if formatType == "TEXT" || formatType == "CSV" {
			formatOptsString = strings.Join(formatTokens, " ")
		} else {
			formatOptsString = makeCustomFormatOpts(formatTokens)
		}
		formatStatement += fmt.Sprintf(" (%s)", formatOptsString)
	}

	return formatStatement
}

/*
 * If an external table is created using LOG ERRORS instead of LOG ERRORS INTO [tablename],
 * the value of pg_exttable.fmterrtbl will match the table's own name.
 */
func generateLogErrorStatement(extTableDef ExternalTableDefinition, tableFQN string) string {
	var logErrorStatement string
	errTableFQN := utils.MakeFQN(extTableDef.ErrTableSchema, extTableDef.ErrTableName)
	if errTableFQN == tableFQN {
		logErrorStatement += "\nLOG ERRORS"
	} else if extTableDef.ErrTableName != "" {
		logErrorStatement += fmt.Sprintf("\nLOG ERRORS INTO %s", errTableFQN)
	}
	if extTableDef.RejectLimit != 0 {
		logErrorStatement += fmt.Sprintf("\nSEGMENT REJECT LIMIT %d ", extTableDef.RejectLimit)
		switch extTableDef.RejectLimitType {
		case "r":
			logErrorStatement += "ROWS"
		case "p":
			logErrorStatement += "PERCENT"
		}
	}

	return logErrorStatement
}

func PrintExternalTableStatements(metadataFile *utils.FileWithByteCount, tableName string, extTableDef ExternalTableDefinition) {
	if extTableDef.Type != WRITABLE_WEB {
		if len(extTableDef.URIs) > 0 {
			metadataFile.MustPrintf("LOCATION (\n\t'%s'\n)", strings.Join(extTableDef.URIs, "',\n\t'"))
		}
	}
	if extTableDef.Type == READABLE || (extTableDef.Type == WRITABLE_WEB && extTableDef.Protocol == S3) {
		if extTableDef.ExecLocation == "MASTER_ONLY" {
			metadataFile.MustPrintf(" ON MASTER")
		}
	}
	if extTableDef.Type == READABLE_WEB || extTableDef.Type == WRITABLE_WEB {
		if extTableDef.Command != "" {
			metadataFile.MustPrint(generateExecuteStatement(extTableDef))
		}
	}
	metadataFile.MustPrintln()
	metadataFile.MustPrint(GenerateFormatStatement(extTableDef))
	metadataFile.MustPrintln()
	if extTableDef.Options != "" {
		metadataFile.MustPrintf("OPTIONS (\n\t%s\n)\n", extTableDef.Options)
	}
	metadataFile.MustPrintf("ENCODING '%s'", extTableDef.Encoding)
	if extTableDef.Type == READABLE || extTableDef.Type == READABLE_WEB {
		metadataFile.MustPrint(generateLogErrorStatement(extTableDef, tableName))
	}
}

func PrintCreateExternalProtocolStatement(metadataFile *utils.FileWithByteCount, toc *toc.TOC, protocol ExternalProtocol, funcInfoMap map[uint32]FunctionInfo, protoMetadata ObjectMetadata) {
	start := metadataFile.ByteCount
	funcOidList := []uint32{protocol.ReadFunction, protocol.WriteFunction, protocol.Validator}
	hasUserDefinedFunc := false
	for _, funcOid := range funcOidList {
		if funcInfo, ok := funcInfoMap[funcOid]; ok && !funcInfo.IsInternal {
			hasUserDefinedFunc = true
		}
	}
	if !hasUserDefinedFunc {
		return
	}

	protocolFunctions := make([]string, 0)
	if protocol.ReadFunction != 0 {
		protocolFunctions = append(protocolFunctions, fmt.Sprintf("readfunc = %s", funcInfoMap[protocol.ReadFunction].QualifiedName))
	}
	if protocol.WriteFunction != 0 {
		protocolFunctions = append(protocolFunctions, fmt.Sprintf("writefunc = %s", funcInfoMap[protocol.WriteFunction].QualifiedName))
	}
	if protocol.Validator != 0 {
		protocolFunctions = append(protocolFunctions, fmt.Sprintf("validatorfunc = %s", funcInfoMap[protocol.Validator].QualifiedName))
	}

	metadataFile.MustPrintf("\n\nCREATE ")
	if protocol.Trusted {
		metadataFile.MustPrintf("TRUSTED ")
	}
	metadataFile.MustPrintf("PROTOCOL %s (%s);\n", protocol.Name, strings.Join(protocolFunctions, ", "))

	section, entry := protocol.GetMetadataEntry()
	toc.AddMetadataEntry(section, entry, start, metadataFile.ByteCount)
	PrintObjectMetadata(metadataFile, toc, protoMetadata, protocol, "")
}

// 该函数为 extPartitions 中每一个外表生成对应的 EXCHANGE PARTITION 信息.
// 这里 extPartitions,partInfoMap 对应着 GetExternalPartitionInfo() 返回值.
func PrintExchangeExternalPartitionStatements(metadataFile *utils.FileWithByteCount, toc *toc.TOC, extPartitions []PartitionInfo, partInfoMap map[uint32]PartitionInfo, tables []Table) {
	tableNameMap := make(map[uint32]string, len(tables))
	for _, table := range tables {
		tableNameMap[table.Oid] = table.FQN()
	}
	for _, externalPartition := range extPartitions {
		extPartRelationName := tableNameMap[externalPartition.RelationOid]
		if extPartRelationName == "" {
			continue //Not included in the list of tables to back up
		}
		parentRelationName := utils.MakeFQN(externalPartition.ParentSchema, externalPartition.ParentRelationName)
		start := metadataFile.ByteCount
		// 构造从根表到指定叶子外表的路径.
		alterPartitionStr := ""
		currentPartition := externalPartition
		for currentPartition.PartitionParentRuleOid != 0 {
			parent := partInfoMap[currentPartition.PartitionParentRuleOid]
			if parent.PartitionName == "" {
				alterPartitionStr = fmt.Sprintf("ALTER PARTITION FOR (RANK(%d)) ", parent.PartitionRank) + alterPartitionStr
			} else {
				alterPartitionStr = fmt.Sprintf("ALTER PARTITION %s ", parent.PartitionName) + alterPartitionStr
			}
			currentPartition = parent
		}
		metadataFile.MustPrintf("\n\nALTER TABLE %s %s", parentRelationName, alterPartitionStr)
		if externalPartition.PartitionName == "" {
			metadataFile.MustPrintf("EXCHANGE PARTITION FOR (RANK(%d)) ", externalPartition.PartitionRank)
		} else {
			metadataFile.MustPrintf("EXCHANGE PARTITION %s ", externalPartition.PartitionName)
		}
		metadataFile.MustPrintf("WITH TABLE %s WITHOUT VALIDATION;", extPartRelationName)
		metadataFile.MustPrintf("\n\nDROP TABLE %s;", extPartRelationName)

		section, entry := externalPartition.GetMetadataEntry()
		toc.AddMetadataEntry(section, entry, start, metadataFile.ByteCount)
	}
}
