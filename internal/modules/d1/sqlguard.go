package d1

import "strings"

type SQLClass uint8

const (
	SQLUnknown SQLClass = iota
	SQLRead
	SQLWrite
)

func ClassifySQL(sql string) SQLClass {
	statements := splitStatements(stripComments(sql))
	if len(statements) == 0 {
		return SQLUnknown
	}
	class := SQLRead
	for _, statement := range statements {
		fields := strings.Fields(statement)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "SELECT", "EXPLAIN":
		case "PRAGMA":
			if !isReadOnlyPragma(statement) {
				return SQLWrite
			}
		case "INSERT", "UPDATE", "DELETE", "REPLACE", "CREATE", "ALTER", "DROP", "VACUUM", "ATTACH", "DETACH", "REINDEX", "ANALYZE":
			return SQLWrite
		default:
			class = SQLUnknown
		}
	}
	return class
}

func stripComments(sql string) string {
	var out strings.Builder
	var quote byte
	bracketed := false
	for i := 0; i < len(sql); {
		if quote != 0 {
			out.WriteByte(sql[i])
			if sql[i] == quote {
				if i+1 < len(sql) && sql[i+1] == quote {
					i++
					out.WriteByte(sql[i])
				} else {
					quote = 0
				}
			}
			i++
			continue
		}
		if bracketed {
			out.WriteByte(sql[i])
			if sql[i] == ']' {
				bracketed = false
			}
			i++
			continue
		}
		if sql[i] == '\'' || sql[i] == '"' || sql[i] == '`' {
			quote = sql[i]
			out.WriteByte(sql[i])
			i++
			continue
		}
		if sql[i] == '[' {
			bracketed = true
			out.WriteByte(sql[i])
			i++
			continue
		}
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < len(sql) {
				i += 2
			}
			out.WriteByte(' ')
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String()
}

func isReadOnlyPragma(statement string) bool {
	value := strings.TrimSpace(statement[len("PRAGMA"):])
	if value == "" || strings.Contains(value, "=") {
		return false
	}
	if index := strings.IndexAny(value, "( \t\r\n"); index >= 0 {
		value = value[:index]
	}
	if index := strings.LastIndex(value, "."); index >= 0 {
		value = value[index+1:]
	}
	switch strings.ToLower(value) {
	case "table_info", "table_xinfo", "index_list", "index_info", "index_xinfo",
		"foreign_key_list", "database_list", "compile_options", "quick_check",
		"integrity_check", "schema_version", "freelist_count":
		return true
	default:
		return false
	}
}

func splitStatements(sql string) []string {
	var statements []string
	start := 0
	var quote byte
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if quote != 0 {
			if ch == quote {
				if i+1 < len(sql) && sql[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			continue
		}
		if ch == ';' {
			if statement := strings.TrimSpace(sql[start:i]); statement != "" {
				statements = append(statements, statement)
			}
			start = i + 1
		}
	}
	if statement := strings.TrimSpace(sql[start:]); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}
