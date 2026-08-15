package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// form is a thin reader over a posted form that collects its own errors, so a
// handler can parse a whole page of fields and then report every problem at
// once instead of one per submission.
type form struct {
	request *http.Request
	Errors  []string
}

func newForm(r *http.Request) (*form, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return &form{request: r}, nil
}

func (f *form) String(field string) string {
	return strings.TrimSpace(f.request.PostFormValue(field))
}

// Raw keeps whitespace, for anything where indentation is meaningful.
func (f *form) Raw(field string) string {
	return f.request.PostFormValue(field)
}

func (f *form) Bool(field string) bool {
	switch strings.ToLower(f.String(field)) {
	case "on", "true", "1", "yes":
		return true
	default:
		return false
	}
}

func (f *form) Int(field string, fallback int) int {
	raw := f.String(field)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		f.fail("%s must be a whole number", label(field))
		return fallback
	}
	return value
}

func (f *form) Strings(field string) []string {
	values := f.request.PostForm[field]
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (f *form) Require(field string) string {
	value := f.String(field)
	if value == "" {
		f.fail("%s is required", label(field))
	}
	return value
}

func (f *form) fail(format string, args ...any) {
	f.Errors = append(f.Errors, fmt.Sprintf(format, args...))
}

func (f *form) OK() bool { return len(f.Errors) == 0 }

func (f *form) Error() string { return strings.Join(f.Errors, " ") }

// label turns a field name into something worth showing a person:
// "deleteBatchSize" becomes "Delete batch size".
func label(field string) string {
	var b strings.Builder
	for i, r := range field {
		switch {
		case i == 0:
			b.WriteString(strings.ToUpper(string(r)))
		case r >= 'A' && r <= 'Z':
			b.WriteByte(' ')
			b.WriteString(strings.ToLower(string(r)))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
