package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// A PostgreSQL retention config's filter is the body of a WHERE clause, typed
// by the operator: `created_at < {{now-90d}} AND status = 'closed'`.
//
// Feather splices it into every statement a run issues, wrapped in
// parentheses, and that includes the DELETE. So the one property that matters
// is that the text cannot escape its parentheses. A filter of
//
//	1=1) OR (1=1
//
// spliced into `WHERE id = ANY($1) AND (<filter>)` becomes
// `WHERE id = ANY($1) AND (1=1) OR (1=1)`, which deletes every row in the
// table. Nobody types that on purpose, but an unbalanced paren pasted from a
// longer query does exactly the same thing. A trailing `--` comment would
// swallow the guard that follows it, and a `;` would start a second statement.
//
// LintSQLFilter therefore tokenises the filter just far enough to tell code
// from string literals and quoted identifiers, and refuses anything whose
// structure it cannot vouch for. It is not a SQL parser and does not try to be:
// whether the filter is valid SQL is for PostgreSQL to say, and it will.
//
// The lexer assumes standard_conforming_strings, under which a backslash in
// '...' is an ordinary character. The Postgres client sets that on every
// connection rather than trusting the server default, so the assumption holds.

// LintSQLFilter reports why a SQL filter cannot be safely embedded, or nil.
func LintSQLFilter(filter string) error {
	_, err := rewriteSQL(filter, nil)
	return err
}

// sqlTokenHandler is called for each {{...}} found outside a quoted
// identifier. inString says whether it sat inside a '...' literal. It returns
// the replacement text.
type sqlTokenHandler func(token string, inString bool) (string, error)

// rewriteSQL walks the filter enforcing the structural rules and, when handle
// is non-nil, rewrites time tokens. It stops at the first problem.
func rewriteSQL(filter string, handle sqlTokenHandler) (string, error) {
	var (
		out   strings.Builder
		depth int
		i     int
	)
	n := len(filter)

	for i < n {
		c := filter[i]
		switch {
		case c == '\'':
			// An E'...' literal treats backslash as an escape, so \' does not
			// end it. This lexer does not model that, so it refuses the form
			// rather than misjudge where the literal stops.
			if i > 0 && (filter[i-1] == 'E' || filter[i-1] == 'e') && (i == 1 || !isIdentChar(filter[i-2])) {
				return "", errors.New("escape-string literals (E'...') are not supported in a filter — use a standard '...' literal")
			}
			end, body, err := readQuoted(filter, i, '\'', handle)
			if err != nil {
				return "", err
			}
			out.WriteByte('\'')
			out.WriteString(body)
			out.WriteByte('\'')
			i = end
			continue

		case c == '"':
			end := closingQuote(filter, i, '"')
			if end < 0 {
				return "", errors.New("the filter has an unterminated quoted identifier (\")")
			}
			out.WriteString(filter[i:end])
			i = end
			continue

		case c == '{' && i+1 < n && filter[i+1] == '{':
			close := strings.Index(filter[i:], "}}")
			if close < 0 {
				return "", errors.New("the filter has an unterminated {{ token")
			}
			token := filter[i : i+close+2]
			replacement := token
			if handle != nil {
				var err error
				if replacement, err = handle(token, false); err != nil {
					return "", err
				}
			}
			out.WriteString(replacement)
			i += close + 2
			continue

		case c == ';':
			return "", errors.New("the filter contains a semicolon — it must be a single condition, not a statement")
		case c == '-' && i+1 < n && filter[i+1] == '-':
			return "", errors.New("the filter contains a -- comment, which would hide the conditions Feather adds after it")
		case c == '/' && i+1 < n && filter[i+1] == '*':
			return "", errors.New("the filter contains a /* comment */ — remove it")
		case c == '$':
			// Covers $1-style parameters, which would collide with Feather's
			// own, and $$ dollar quoting, which this lexer does not model.
			return "", errors.New("the filter contains a $ — parameters and dollar-quoted strings are not supported; quote the value with '...'")
		case c == '\\':
			return "", errors.New("the filter contains a backslash outside a string literal")
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				return "", errors.New("the filter closes a parenthesis it never opened — it would escape the parentheses Feather wraps it in")
			}
		}
		out.WriteByte(c)
		i++
	}

	if depth != 0 {
		return "", fmt.Errorf("the filter has %d unclosed parenthesis(es)", depth)
	}
	return out.String(), nil
}

// readQuoted reads a '...' literal starting at filter[start], with ” as the
// escaped quote, rewriting any {{tokens}} inside it. It returns the index just
// past the closing quote and the literal's body.
func readQuoted(filter string, start int, quote byte, handle sqlTokenHandler) (int, string, error) {
	var body strings.Builder
	i := start + 1
	for i < len(filter) {
		c := filter[i]
		if c == quote {
			if i+1 < len(filter) && filter[i+1] == quote {
				body.WriteString(filter[i : i+2])
				i += 2
				continue
			}
			return i + 1, body.String(), nil
		}
		if c == '{' && i+1 < len(filter) && filter[i+1] == '{' && handle != nil {
			if close := strings.Index(filter[i:], "}}"); close >= 0 {
				replacement, err := handle(filter[i:i+close+2], true)
				if err != nil {
					return 0, "", err
				}
				body.WriteString(replacement)
				i += close + 2
				continue
			}
		}
		body.WriteByte(c)
		i++
	}
	return 0, "", errors.New("the filter has an unterminated string literal (')")
}

// closingQuote returns the index just past the quote that closes the one at
// start, honouring doubled quotes, or -1.
func closingQuote(filter string, start int, quote byte) int {
	for i := start + 1; i < len(filter); i++ {
		if filter[i] != quote {
			continue
		}
		if i+1 < len(filter) && filter[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return -1
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ResolveSQLTimeTokens rewrites {{now-90d}}-style tokens in a SQL filter into
// timestamps, after checking the filter is safe to embed.
//
// A bare token becomes a typed literal, TIMESTAMPTZ '2026-01-01T00:00:00.000Z',
// so `created_at < {{now-90d}}` works without the operator having to know how
// Postgres wants a timestamp spelled. A token already inside quotes,
// `created_at < '{{now-90d}}'`, becomes just the timestamp text, so that form
// works too.
func ResolveSQLTimeTokens(filter string, now time.Time) (string, error) {
	resolved, err := rewriteSQL(filter, func(token string, inString bool) (string, error) {
		instant, err := resolveToken(token, now)
		if err != nil {
			return "", err
		}
		stamp := instant.Format(tokenTimeLayout)
		if inString {
			return stamp, nil
		}
		return "TIMESTAMPTZ '" + stamp + "'", nil
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resolved), nil
}

// resolveToken evaluates one {{now±offset}} token.
func resolveToken(token string, now time.Time) (time.Time, error) {
	groups := tokenRe.FindStringSubmatch(token)
	if groups == nil || groups[0] != token {
		return time.Time{}, fmt.Errorf("unrecognised template token %q (supported: {{now}}, {{now-90d}}, {{now+12h}})", token)
	}
	instant := now.UTC()
	if groups[2] == "" {
		return instant, nil
	}
	d, err := ParseDuration(groups[2])
	if err != nil {
		return time.Time{}, fmt.Errorf("token %q: %w", token, err)
	}
	if groups[1] == "-" {
		d = -d
	}
	return instant.Add(d), nil
}
