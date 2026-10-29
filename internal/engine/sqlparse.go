package engine

import (
	"strings"
	"unicode"
)

func stripLeadingCommentsAndSpace(sqlText string) string {
	idx := 0
	length := len(sqlText)
	for idx < length {
		if unicode.IsSpace(rune(sqlText[idx])) {
			idx++
			continue
		}
		if idx+1 < length && sqlText[idx] == '-' && sqlText[idx+1] == '-' {
			idx = advancePastLineComment(sqlText, idx+2)
			continue
		}
		if idx+1 < length && sqlText[idx] == '/' && sqlText[idx+1] == '*' {
			idx = advancePastBlockComment(sqlText, idx+2)
			continue
		}
		break
	}
	return strings.TrimSpace(sqlText[idx:])
}

func advancePastLineComment(sqlText string, start int) int {
	newlineOffset := strings.IndexByte(sqlText[start:], '\n')
	if newlineOffset < 0 {
		return len(sqlText)
	}
	return start + newlineOffset + 1
}

func advancePastBlockComment(sqlText string, start int) int {
	closeOffset := strings.Index(sqlText[start:], "*/")
	if closeOffset < 0 {
		return len(sqlText)
	}
	return start + closeOffset + 2
}

func extractLeadingKeyword(sqlText string) string {
	trimmed := stripLeadingCommentsAndSpace(sqlText)
	end := 0
	for end < len(trimmed) && (unicode.IsLetter(rune(trimmed[end])) || trimmed[end] == '_') {
		end++
	}
	return strings.ToLower(trimmed[:end])
}

func hasMultipleStatements(sqlText string) bool {
	statements := SplitSQLStatements(sqlText)
	return len(statements) > 1
}

func SplitSQLStatements(sqlText string) []string {
	var statements []string
	stmtStart := 0
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
		if ch == ';' {
			candidate := strings.TrimSpace(sqlText[stmtStart:idx])
			if stripLeadingCommentsAndSpace(candidate) != "" {
				statements = append(statements, candidate)
			}
			stmtStart = idx + 1
		}
		idx++
	}
	trailing := strings.TrimSpace(sqlText[stmtStart:])
	if stripLeadingCommentsAndSpace(trailing) != "" {
		statements = append(statements, trailing)
	}
	return statements
}

func advancePastQuotedLiteral(sqlText string, openIndex int, quoteChar byte) int {
	idx := openIndex + 1
	length := len(sqlText)
	for idx < length {
		if sqlText[idx] == quoteChar {
			if idx+1 < length && sqlText[idx+1] == quoteChar {
				idx += 2
				continue
			}
			return idx + 1
		}
		idx++
	}
	return length
}
