package mysql

import (
	"strings"
	"unicode"
)

// Destructive returns a reason when sql contains a destructive statement:
// DROP, TRUNCATE, DELETE/UPDATE without a WHERE at their own parenthesis
// depth, ALTER ... DROP/RENAME, RENAME TABLE, CALL/PREPARE/EXECUTE (opaque
// code), REVOKE. Comments and quoted strings/identifiers are ignored; MySQL
// executable comments (/*! ... */) are treated as code because the server
// runs them.
func Destructive(sql string) string {
	for _, stmt := range splitStatements(stripSQL(sql)) {
		toks := ptokens(stmt)
		for i, t := range toks {
			switch t {
			case "DROP":
				if i == 0 {
					return "DROP"
				}
			case "TRUNCATE":
				return "TRUNCATE"
			case "CALL", "PREPARE", "EXECUTE", "RENAME", "REVOKE":
				if i == 0 {
					return t
				}
			case "ALTER":
				if i == 0 && contains(toks, "DROP") {
					return "ALTER ... DROP"
				}
				if i == 0 && contains(toks, "RENAME") {
					return "ALTER ... RENAME"
				}
			case "DELETE", "UPDATE":
				if t == "UPDATE" && i > 0 && (toks[i-1] == "FOR" || toks[i-1] == "KEY") { // FOR UPDATE, ON DUPLICATE KEY UPDATE
					continue
				}
				if !hasWhere(toks[i+1:]) {
					return t + " without WHERE"
				}
			}
		}
	}
	return ""
}

// readOnlyStart reports whether sql starts with a statement kind that is
// allowed through the read verb (the server-side READ ONLY transaction is the
// real protection; this catches DDL, which MySQL would auto-commit).
func readOnlyStart(sql string) bool {
	toks := tokens(stripSQL(sql))
	if len(toks) == 0 {
		return false
	}
	switch toks[0] {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH", "TABLE", "VALUES":
		return !contains(toks, "INTO") || !(contains(toks, "OUTFILE") || contains(toks, "DUMPFILE"))
	}
	return false
}

// isDDL reports statements MySQL commits implicitly (cannot be previewed).
func isDDL(sql string) bool {
	toks := tokens(stripSQL(sql))
	if len(toks) == 0 {
		return false
	}
	switch toks[0] {
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE", "LOCK", "UNLOCK", "ANALYZE", "OPTIMIZE", "REPAIR":
		return true
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// stripSQL blanks comments ('-- ', '#', /* */) and '…', "…", `…` quoted text.
func stripSQL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-', c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+2 < len(s) && s[i+1] == '*' && s[i+2] == '!':
			// executable comment: keep the body, drop the markers/version
			i += 3
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			i--
			b.WriteByte(' ')
		case c == '*' && i+1 < len(s) && s[i+1] == '/':
			i++
			b.WriteByte(' ')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				i = len(s)
			} else {
				i = i + 2 + end + 1
			}
			b.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`':
			q := c
			for i++; i < len(s); i++ {
				if s[i] == '\\' && q != '`' {
					i++
					continue
				}
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						i++
						continue
					}
					break
				}
			}
			b.WriteString(" x ")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func splitStatements(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func tokens(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	for i := range f {
		f[i] = strings.ToUpper(f[i])
	}
	return f
}

// hasWhere reports whether toks (starting just after a DELETE/UPDATE) contain
// a WHERE at the statement's own parenthesis depth, before that depth closes.
// A WHERE inside a subquery or after the enclosing CTE body does not count.
func hasWhere(toks []string) bool {
	depth := 0
	for _, t := range toks {
		switch t {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
		case "WHERE":
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// ptokens is tokens plus "(" and ")" as their own tokens.
func ptokens(s string) []string {
	var out []string
	start := -1
	flush := func(i int) {
		if start >= 0 {
			out = append(out, strings.ToUpper(s[start:i]))
			start = -1
		}
	}
	for i, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			if start < 0 {
				start = i
			}
		case r == '(' || r == ')':
			flush(i)
			out = append(out, string(r))
		default:
			flush(i)
		}
	}
	flush(len(s))
	return out
}
