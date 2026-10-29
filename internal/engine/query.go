package engine

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type preparedExecution struct {
	sqlText    string
	driverArgs []any
	maxRows    int
	startTime  time.Time
}

func executeStatementOnConn(ctx context.Context, conn *sql.Conn, stmt Statement, maxRows int) (QueryResult, error) {
	if err := ValidateSingleStatement(stmt.SQL); err != nil {
		return QueryResult{}, err
	}
	driverArgs, err := ConvertParamsToDriverArgs(stmt.Params)
	if err != nil {
		return QueryResult{}, err
	}
	execSpec := preparedExecution{
		sqlText:    stmt.SQL,
		driverArgs: driverArgs,
		maxRows:    maxRows,
		startTime:  time.Now(),
	}
	if producesResultRows(stmt.SQL) {
		return executeRowQuery(ctx, conn, execSpec)
	}
	return executeMutationStatement(ctx, conn, execSpec)
}

func executeMutationStatement(
	ctx context.Context,
	conn *sql.Conn,
	spec preparedExecution,
) (QueryResult, error) {
	execOutcome, err := conn.ExecContext(ctx, spec.sqlText, spec.driverArgs...)
	if err != nil {
		return QueryResult{}, normalizeSQLiteError(err)
	}
	rowsAffected, _ := execOutcome.RowsAffected()
	var lastInsertID *int64
	if extractLeadingKeyword(spec.sqlText) == "insert" {
		if id, idErr := execOutcome.LastInsertId(); idErr == nil {
			lastInsertID = &id
		}
	}
	return QueryResult{
		Columns:         []string{},
		Rows:            [][]TypedValue{},
		RowsAffected:    rowsAffected,
		LastInsertRowID: lastInsertID,
		DurationMS:      elapsedMilliseconds(spec.startTime),
	}, nil
}

func executeRowQuery(
	ctx context.Context,
	conn *sql.Conn,
	spec preparedExecution,
) (QueryResult, error) {
	rowsCursor, err := conn.QueryContext(ctx, spec.sqlText, spec.driverArgs...)
	if err != nil {
		return QueryResult{}, normalizeSQLiteError(err)
	}
	defer rowsCursor.Close()

	columns, err := rowsCursor.Columns()
	if err != nil {
		return QueryResult{}, normalizeSQLiteError(err)
	}
	collectedRows, isTruncated, err := scanRowsWithCap(rowsCursor, len(columns), spec.maxRows)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{
		Columns:    columns,
		Rows:       collectedRows,
		Truncated:  isTruncated,
		DurationMS: elapsedMilliseconds(spec.startTime),
	}, nil
}

func scanRowsWithCap(rowsCursor *sql.Rows, columnCount int, maxRows int) ([][]TypedValue, bool, error) {
	collected := make([][]TypedValue, 0)
	rawCells := make([]any, columnCount)
	scanTargets := make([]any, columnCount)
	for i := range rawCells {
		scanTargets[i] = &rawCells[i]
	}

	isTruncated := false
	for rowsCursor.Next() {
		if maxRows > 0 && len(collected) >= maxRows {
			isTruncated = true
			break
		}
		if err := rowsCursor.Scan(scanTargets...); err != nil {
			return nil, false, normalizeSQLiteError(err)
		}
		typedRow, err := convertScannedRow(rawCells)
		if err != nil {
			return nil, false, err
		}
		collected = append(collected, typedRow)
	}
	if err := rowsCursor.Err(); err != nil {
		return nil, false, normalizeSQLiteError(err)
	}
	return collected, isTruncated, nil
}

func convertScannedRow(rawCells []any) ([]TypedValue, error) {
	typedRow := make([]TypedValue, len(rawCells))
	for i, rawCell := range rawCells {
		converted, err := FromDriverValue(rawCell)
		if err != nil {
			return nil, err
		}
		typedRow[i] = converted
	}
	return typedRow, nil
}

func producesResultRows(sqlText string) bool {
	keyword := extractLeadingKeyword(sqlText)
	switch keyword {
	case "select", "pragma", "values", "explain":
		return true
	case "with":
		return containsTopLevelKeyword(sqlText, "select") || containsTopLevelKeyword(sqlText, "returning")
	default:
		return containsTopLevelKeyword(sqlText, "returning")
	}
}

func containsTopLevelKeyword(sqlText string, targetKeyword string) bool {
	tokens := extractUnquotedTokens(sqlText)
	for _, token := range tokens {
		if strings.EqualFold(token, targetKeyword) {
			return true
		}
	}
	return false
}

func extractUnquotedTokens(sqlText string) []string {
	var tokens []string
	idx := 0
	length := len(sqlText)
	for idx < length {
		ch := sqlText[idx]
		if ch == '\'' || ch == '"' || ch == '`' {
			idx = advancePastQuotedLiteral(sqlText, idx, ch)
			continue
		}
		if idx+1 < length && ch == '-' && sqlText[idx+1] == '-' {
			idx = advancePastLineComment(sqlText, idx+2)
			continue
		}
		if idx+1 < length && ch == '/' && sqlText[idx+1] == '*' {
			idx = advancePastBlockComment(sqlText, idx+2)
			continue
		}
		if isIdentifierChar(ch) {
			start := idx
			for idx < length && isIdentifierChar(sqlText[idx]) {
				idx++
			}
			tokens = append(tokens, sqlText[start:idx])
			continue
		}
		idx++
	}
	return tokens
}

func isIdentifierChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_'
}

func elapsedMilliseconds(startTime time.Time) float64 {
	return float64(time.Since(startTime).Microseconds()) / 1000.0
}
