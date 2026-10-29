package engine

import (
	"strings"
	"sync/atomic"

	"github.com/mattn/go-sqlite3"
)

// mattn/go-sqlite3 leaves SQLITE_RECURSIVE commented out in sqlite3.go;
// SQLite's C header defines it as 33 for recursive CTE queries.
const sqliteRecursiveAction = 33

const reservedTablePrefix = "_litebase_"

var allowedReadOnlyPragmas = map[string]struct{}{
	"application_id":     {},
	"busy_timeout":       {},
	"collation_list":     {},
	"compile_options":    {},
	"data_version":       {},
	"database_list":      {},
	"encoding":           {},
	"foreign_key_check":  {},
	"foreign_key_list":   {},
	"foreign_keys":       {},
	"freelist_count":     {},
	"function_list":      {},
	"index_info":         {},
	"index_list":         {},
	"index_xinfo":        {},
	"integrity_check":    {},
	"journal_mode":       {},
	"module_list":        {},
	"page_count":         {},
	"page_size":          {},
	"pragma_list":        {},
	"query_only":         {},
	"quick_check":        {},
	"schema_version":     {},
	"synchronous":        {},
	"table_info":         {},
	"table_list":         {},
	"table_xinfo":        {},
	"temp_store":         {},
	"user_version":       {},
	"wal_autocheckpoint": {},
}

var forbiddenSQLFunctions = map[string]struct{}{
	"load_extension": {},
	"fts3_tokenizer": {},
}

// SQLite's VACUUM INTO internally invokes SQLITE_ATTACH on the target file,
// and wal_checkpoint mutates WAL state via PRAGMA. Writer connections toggle
// privileged mode only while the writer actor runs internal maintenance.
type connectionSecurityState struct {
	isPrivileged atomic.Bool
}

func registerReaderAuthorizer(conn *sqlite3.SQLiteConn) {
	conn.RegisterAuthorizer(authorizeReaderAction)
}

func registerWriterAuthorizer(conn *sqlite3.SQLiteConn, state *connectionSecurityState) {
	conn.RegisterAuthorizer(func(action int, arg1, arg2, arg3 string) int {
		if state != nil && state.isPrivileged.Load() {
			return sqlite3.SQLITE_OK
		}
		return authorizeWriterAction(action, arg1, arg2, arg3)
	})
}

func authorizeReaderAction(action int, arg1, arg2, _ string) int {
	switch action {
	case sqlite3.SQLITE_SELECT, sqlite3.SQLITE_READ, sqliteRecursiveAction:
		return sqlite3.SQLITE_OK
	case sqlite3.SQLITE_FUNCTION:
		return authorizeFunctionCall(arg1, arg2)
	case sqlite3.SQLITE_PRAGMA:
		return authorizePragmaRead(arg1, arg2)
	default:
		return sqlite3.SQLITE_DENY
	}
}

func authorizeWriterAction(action int, arg1, arg2, _ string) int {
	switch action {
	case sqlite3.SQLITE_ATTACH, sqlite3.SQLITE_DETACH:
		return sqlite3.SQLITE_DENY
	case sqlite3.SQLITE_FUNCTION:
		return authorizeFunctionCall(arg1, arg2)
	case sqlite3.SQLITE_PRAGMA:
		return authorizePragmaRead(arg1, arg2)
	case sqlite3.SQLITE_SELECT, sqlite3.SQLITE_READ, sqliteRecursiveAction,
		sqlite3.SQLITE_TRANSACTION, sqlite3.SQLITE_SAVEPOINT,
		sqlite3.SQLITE_REINDEX, sqlite3.SQLITE_ANALYZE:
		return sqlite3.SQLITE_OK
	case sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE:
		return authorizeTableMutation(arg1)
	case sqlite3.SQLITE_ALTER_TABLE:
		return authorizeTableMutation(arg2)
	default:
		return authorizeDDLAction(action, arg1)
	}
}

func authorizeFunctionCall(arg1, arg2 string) int {
	funcName := strings.ToLower(strings.TrimSpace(arg2))
	if funcName == "" {
		funcName = strings.ToLower(strings.TrimSpace(arg1))
	}
	if _, isForbidden := forbiddenSQLFunctions[funcName]; isForbidden {
		return sqlite3.SQLITE_DENY
	}
	return sqlite3.SQLITE_OK
}

func authorizePragmaRead(pragmaName, pragmaArg string) int {
	normalized := strings.ToLower(strings.TrimSpace(pragmaName))
	if _, isAllowed := allowedReadOnlyPragmas[normalized]; !isAllowed {
		return sqlite3.SQLITE_DENY
	}
	// Non-empty pragmaArg indicates PRAGMA name = value or PRAGMA name(arg),
	// except integrity_check/quick_check/table_info/index_info/foreign_key_list
	// which pass the inspected table or row limit in arg2.
	if pragmaArg != "" && !isParameterizedInspectionPragma(normalized) {
		return sqlite3.SQLITE_DENY
	}
	return sqlite3.SQLITE_OK
}

func isParameterizedInspectionPragma(pragmaName string) bool {
	switch pragmaName {
	case "table_info", "table_xinfo", "index_list", "index_info", "index_xinfo",
		"foreign_key_list", "foreign_key_check", "integrity_check", "quick_check":
		return true
	default:
		return false
	}
}

func authorizeTableMutation(tableName string) int {
	normalized := strings.ToLower(strings.TrimSpace(tableName))
	if strings.HasPrefix(normalized, reservedTablePrefix) {
		return sqlite3.SQLITE_DENY
	}
	return sqlite3.SQLITE_OK
}

func authorizeDDLAction(action int, targetName string) int {
	switch action {
	case sqlite3.SQLITE_CREATE_INDEX, sqlite3.SQLITE_CREATE_TABLE,
		sqlite3.SQLITE_CREATE_TEMP_INDEX, sqlite3.SQLITE_CREATE_TEMP_TABLE,
		sqlite3.SQLITE_CREATE_TEMP_TRIGGER, sqlite3.SQLITE_CREATE_TEMP_VIEW,
		sqlite3.SQLITE_CREATE_TRIGGER, sqlite3.SQLITE_CREATE_VIEW,
		sqlite3.SQLITE_CREATE_VTABLE,
		sqlite3.SQLITE_DROP_INDEX, sqlite3.SQLITE_DROP_TABLE,
		sqlite3.SQLITE_DROP_TEMP_INDEX, sqlite3.SQLITE_DROP_TEMP_TABLE,
		sqlite3.SQLITE_DROP_TEMP_TRIGGER, sqlite3.SQLITE_DROP_TEMP_VIEW,
		sqlite3.SQLITE_DROP_TRIGGER, sqlite3.SQLITE_DROP_VIEW,
		sqlite3.SQLITE_DROP_VTABLE:
		return authorizeTableMutation(targetName)
	default:
		return sqlite3.SQLITE_DENY
	}
}
