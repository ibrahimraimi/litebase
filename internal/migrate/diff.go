package migrate

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrMergeConflict           = errors.New("schema merge conflict: object was modified in both parent and branch")
	ErrDropTableNotAllowed     = errors.New("dropping a table requires --allow-drop")
	ErrManualMigrationRequired = errors.New("table change requires a manual migration")
)

type DiffOptions struct {
	AllowDrop bool
}

func ComputeThreeWaySchemaDiff(
	baseSchema []SchemaObject,
	oursSchema []SchemaObject,
	theirsSchema []SchemaObject,
	opts DiffOptions,
) ([]string, error) {
	baseMap := indexSchemaByKey(baseSchema)
	oursMap := indexSchemaByKey(oursSchema)
	theirsMap := indexSchemaByKey(theirsSchema)

	var statements []string
	for _, targetObj := range theirsSchema {
		key := schemaKey(targetObj)
		baseObj, inBase := baseMap[key]
		oursObj, inOurs := oursMap[key]

		if inBase && sameObjectSQL(baseObj, targetObj) {
			continue
		}
		if err := checkOursConflict(key, inBase, baseObj, inOurs, oursObj, targetObj); err != nil {
			return nil, err
		}
		if inOurs && sameObjectSQL(oursObj, targetObj) {
			continue
		}
		if !inBase {
			statements = append(statements, ensureTrailingSemicolon(targetObj.SQL))
			continue
		}
		stmts, err := buildModifiedObjectDiff(baseObj, targetObj)
		if err != nil {
			return nil, err
		}
		statements = append(statements, stmts...)
	}

	dropStmts, err := collectDroppedObjectsDiff(baseSchema, oursMap, theirsMap, opts)
	if err != nil {
		return nil, err
	}
	return append(statements, dropStmts...), nil
}

func checkOursConflict(
	key string,
	inBase bool,
	baseObj SchemaObject,
	inOurs bool,
	oursObj SchemaObject,
	theirsObj SchemaObject,
) error {
	if !inBase {
		if inOurs && !sameObjectSQL(oursObj, theirsObj) {
			return fmt.Errorf("%w: %s was created differently in parent and branch", ErrMergeConflict, key)
		}
		return nil
	}
	if !inOurs {
		return fmt.Errorf("%w: %s was dropped in parent and modified in branch", ErrMergeConflict, key)
	}
	if !sameObjectSQL(baseObj, oursObj) && !sameObjectSQL(oursObj, theirsObj) {
		return fmt.Errorf("%w: %s was modified in both parent and branch", ErrMergeConflict, key)
	}
	return nil
}

func buildModifiedObjectDiff(baseObj SchemaObject, targetObj SchemaObject) ([]string, error) {
	if targetObj.Type != "table" {
		return []string{
			fmt.Sprintf("DROP %s IF EXISTS %s;", strings.ToUpper(targetObj.Type), targetObj.Name),
			ensureTrailingSemicolon(targetObj.SQL),
		}, nil
	}
	baseDefs, okBase := parseCreateTableDefinitions(baseObj.SQL)
	targetDefs, okTarget := parseCreateTableDefinitions(targetObj.SQL)
	if !okBase || !okTarget || len(targetDefs) <= len(baseDefs) {
		return nil, fmt.Errorf("%w: table %q was modified non-additively", ErrManualMigrationRequired, targetObj.Name)
	}
	for i := range baseDefs {
		if normalizeSQLWhitespace(baseDefs[i]) != normalizeSQLWhitespace(targetDefs[i]) {
			return nil, fmt.Errorf("%w: column or constraint %q in table %q changed", ErrManualMigrationRequired, baseDefs[i], targetObj.Name)
		}
	}
	var alterStmts []string
	for _, addedDef := range targetDefs[len(baseDefs):] {
		if isTableConstraintDef(addedDef) {
			return nil, fmt.Errorf("%w: table constraint added to %q", ErrManualMigrationRequired, targetObj.Name)
		}
		alterStmts = append(alterStmts, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", targetObj.Name, strings.TrimSpace(addedDef)))
	}
	return alterStmts, nil
}

func collectDroppedObjectsDiff(
	baseSchema []SchemaObject,
	oursMap map[string]SchemaObject,
	theirsMap map[string]SchemaObject,
	opts DiffOptions,
) ([]string, error) {
	var statements []string
	for _, baseObj := range baseSchema {
		key := schemaKey(baseObj)
		if _, inTheirs := theirsMap[key]; inTheirs {
			continue
		}
		oursObj, inOurs := oursMap[key]
		if !inOurs {
			continue
		}
		if !sameObjectSQL(baseObj, oursObj) {
			return nil, fmt.Errorf("%w: %s was modified in parent and dropped in branch", ErrMergeConflict, key)
		}
		if baseObj.Type == "table" && !opts.AllowDrop {
			return nil, fmt.Errorf("%w: table %q was dropped on branch", ErrDropTableNotAllowed, baseObj.Name)
		}
		statements = append(statements, fmt.Sprintf("DROP %s IF EXISTS %s;", strings.ToUpper(baseObj.Type), baseObj.Name))
	}
	return statements, nil
}

func parseCreateTableDefinitions(createSQL string) ([]string, bool) {
	openParen := strings.IndexByte(createSQL, '(')
	closeParen := strings.LastIndexByte(createSQL, ')')
	if openParen < 0 || closeParen <= openParen {
		return nil, false
	}
	body := createSQL[openParen+1 : closeParen]
	var defs []string
	depth := 0
	start := 0
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(body); i++ {
		ch := body[i]
		if ch == '\'' && !inDoubleQuote {
			inSingleQuote = !inSingleQuote
			continue
		}
		if ch == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			continue
		}
		if inSingleQuote || inDoubleQuote {
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				defs = append(defs, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	tail := strings.TrimSpace(body[start:])
	if tail != "" {
		defs = append(defs, tail)
	}
	return defs, len(defs) > 0
}

func isTableConstraintDef(def string) bool {
	fields := strings.Fields(strings.ToUpper(def))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN":
		return true
	default:
		return false
	}
}

func sameObjectSQL(a SchemaObject, b SchemaObject) bool {
	return normalizeSQLWhitespace(a.SQL) == normalizeSQLWhitespace(b.SQL)
}

func indexSchemaByKey(objects []SchemaObject) map[string]SchemaObject {
	indexed := make(map[string]SchemaObject, len(objects))
	for _, obj := range objects {
		indexed[schemaKey(obj)] = obj
	}
	return indexed
}

func schemaKey(obj SchemaObject) string {
	return obj.Type + ":" + obj.Name
}

func normalizeSQLWhitespace(sqlText string) string {
	return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(sqlText), ";")), " ")
}

func ensureTrailingSemicolon(sqlText string) string {
	trimmed := strings.TrimSpace(sqlText)
	if strings.HasSuffix(trimmed, ";") {
		return trimmed
	}
	return trimmed + ";"
}

