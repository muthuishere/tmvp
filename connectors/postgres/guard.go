package postgres

import (
	"strings"
	"unicode"
)

// Destructive returns a reason when sql contains a destructive statement:
// DROP, TRUNCATE, DELETE/UPDATE without a WHERE at their own parenthesis
// depth (so a data-modifying CTE cannot borrow the outer query's WHERE),
// ALTER ... DROP/RENAME, DO/EXECUTE/CALL (opaque code), VACUUM FULL, CLUSTER,
// REVOKE. Comments, string literals, quoted identifiers and dollar-quoted
// bodies are ignored. It is a guard against accidents, not a parser:
// read-only credentials and BEGIN READ ONLY remain the real read-side
// protection.
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
			case "DO", "EXECUTE", "CALL", "CLUSTER", "REVOKE":
				if i == 0 {
					return t
				}
			case "VACUUM":
				if i == 0 && contains(toks, "FULL") {
					return "VACUUM FULL"
				}
			case "ALTER":
				if i == 0 && contains(toks, "DROP") {
					return "ALTER ... DROP"
				}
				if i == 0 && contains(toks, "RENAME") {
					return "ALTER ... RENAME"
				}
			case "DELETE", "UPDATE":
				// a DELETE/UPDATE needs its own WHERE (CTEs allowed before)
				if t == "UPDATE" && i > 0 && toks[i-1] == "FOR" { // SELECT ... FOR UPDATE
					continue
				}
				if t == "UPDATE" && i > 0 && (toks[i-1] == "DO" || toks[i-1] == "ON") { // ON CONFLICT DO UPDATE
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

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// stripSQL blanks comments, '…' strings, "…" identifiers and $tag$…$tag$ bodies.
func stripSQL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			depth := 0
			for ; i < len(s); i++ {
				if s[i] == '/' && i+1 < len(s) && s[i+1] == '*' {
					depth++
					i++
				} else if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
					depth--
					i++
					if depth == 0 {
						break
					}
				}
			}
			b.WriteByte(' ')
		case c == '\'' || c == '"':
			q := c
			for i++; i < len(s); i++ {
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q { // doubled quote
						i++
						continue
					}
					break
				}
			}
			b.WriteString(" x ")
		case c == '$':
			j := i + 1
			for j < len(s) && (s[j] == '_' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			if j < len(s) && s[j] == '$' {
				tag := s[i : j+1]
				if end := strings.Index(s[j+1:], tag); end >= 0 {
					i = j + 1 + end + len(tag) - 1
					b.WriteString(" x ")
					continue
				}
			}
			b.WriteByte(c)
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
