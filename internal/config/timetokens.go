package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// tokenRe matches {{now}}, {{now-90d}}, {{now+12h}} — with or without spaces.
var tokenRe = regexp.MustCompile(`\{\{\s*now\s*(?:([-+])\s*([0-9]+(?:\.[0-9]+)?[a-zA-Z]+)\s*)?\}\}`)

// leftoverRe catches anything else that looks like a template, so a typo such
// as {{niw-90d}} fails loudly instead of being sent to Mongo as a literal.
var leftoverRe = regexp.MustCompile(`\{\{[^}]*\}\}`)

const tokenTimeLayout = "2006-01-02T15:04:05.000Z"

// ResolveTimeTokens rewrites relative-time tokens in a filter into concrete
// RFC3339 timestamps, evaluated against now. This is what lets a filter mean
// "older than 90 days" on every run rather than freezing a date into the file.
func ResolveTimeTokens(raw json.RawMessage, now time.Time) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}

	var badToken error
	resolved := tokenRe.ReplaceAllFunc(raw, func(match []byte) []byte {
		groups := tokenRe.FindSubmatch(match)
		sign, offset := string(groups[1]), string(groups[2])

		instant := now.UTC()
		if offset != "" {
			d, err := ParseDuration(offset)
			if err != nil {
				badToken = fmt.Errorf("token %q: %w", match, err)
				return match
			}
			if sign == "-" {
				d = -d
			}
			instant = instant.Add(d)
		}
		return []byte(instant.Format(tokenTimeLayout))
	})

	if badToken != nil {
		return nil, badToken
	}
	if leftover := leftoverRe.Find(resolved); leftover != nil {
		return nil, fmt.Errorf("unrecognised template token %q (supported: {{now}}, {{now-90d}}, {{now+12h}})", leftover)
	}
	if !json.Valid(resolved) {
		return nil, fmt.Errorf("filter is no longer valid JSON after token substitution")
	}

	return resolved, nil
}

// KeyTemplateToken is one substitution an operator may use in a file name
// template, with the description the UI shows next to it.
type KeyTemplateToken struct {
	Token   string
	Meaning string
}

// KeyTemplateTokens is the full vocabulary of the file naming template. The
// name of every archive is the operator's to define; this is what they have to
// work with.
var KeyTemplateTokens = []KeyTemplateToken{
	{"{config}", "the retention config's name"},
	{"{db}", "source database"},
	{"{collection}", "source collection"},
	{"{runId}", "unique ID of this run"},
	{"{seq}", "batch number within the run, zero-padded to 5 digits"},
	{"{yyyy}", "year the run started, e.g. 2026"},
	{"{MM}", "month, 01-12"},
	{"{dd}", "day of month, 01-31"},
	{"{HH}", "hour, 00-23 (UTC)"},
	{"{mm}", "minute, 00-59 (UTC)"},
	{"{ts}", "full timestamp, e.g. 20260514T023000Z"},
	{"{ext}", "file extension implied by format and codec, e.g. .ndjson.gz"},
}

// ValidateKeyTemplate rejects a template containing a token feather does not
// know. Left unchecked, a typo like {colletion} would be written into the
// object key literally and only be noticed months later.
//
// It also rejects templates whose shape would produce a key nobody meant: a
// leading slash, a doubled slash, or a ".." segment. Object stores treat those
// as ordinary characters and file the archive somewhere surprising; Google
// Drive turns every segment into a real folder, so ".." would make one.
func ValidateKeyTemplate(template string) error {
	known := make(map[string]bool, len(KeyTemplateTokens))
	for _, t := range KeyTemplateTokens {
		known[t.Token] = true
	}

	for _, found := range keyTokenRe.FindAllString(template, -1) {
		if !known[found] {
			var supported []string
			for _, t := range KeyTemplateTokens {
				supported = append(supported, t.Token)
			}
			return fmt.Errorf("unknown variable %s in the file name template (supported: %s)",
				found, strings.Join(supported, " "))
		}
	}

	if strings.TrimSpace(template) == "" {
		return errors.New("the file name template is empty")
	}
	if strings.HasPrefix(template, "/") {
		return errors.New("the file name template must not start with a slash — keys are relative to the bucket or folder")
	}
	if strings.Contains(template, "//") {
		return errors.New("the file name template contains an empty path segment (//)")
	}
	for _, segment := range strings.Split(template, "/") {
		if segment == ".." || segment == "." {
			return fmt.Errorf("the file name template contains a %q path segment", segment)
		}
	}
	if strings.ContainsAny(template, "\\\x00") {
		return errors.New("the file name template contains a backslash or a null byte — use / to separate folders")
	}
	return nil
}

// RequiresRunID reports the reason a template cannot be trusted to produce a
// unique key on every run, or nil if it can.
//
// {runId} is mandatory, and {seq} is not a substitute for it. A template of
// "archive/{collection}/{seq}{ext}" renders identically on every run: run 2
// writes over run 1's objects. If run 1 deleted its documents from MongoDB —
// which is the whole point of the tool — the only copy of them is what run 2
// just overwrote. That is silent, total, unrecoverable loss of a batch, from a
// config that reads perfectly sensibly.
func RequiresRunID(template string) error {
	if !strings.Contains(template, "{runId}") {
		return errors.New("output.keyTemplate must contain {runId} so a later run cannot overwrite an earlier run's archive")
	}
	return nil
}

var keyTokenRe = regexp.MustCompile(`\{[^}/]*\}`)

// RenderKey expands an output key template for one batch.
func RenderKey(template string, vars KeyVars) string {
	at := vars.At.UTC()
	replacer := strings.NewReplacer(
		"{db}", vars.Database,
		"{collection}", vars.Collection,
		"{config}", vars.Config,
		"{runId}", vars.RunID,
		"{seq}", fmt.Sprintf("%05d", vars.Seq),
		"{ext}", vars.Extension,
		"{yyyy}", at.Format("2006"),
		"{MM}", at.Format("01"),
		"{dd}", at.Format("02"),
		"{HH}", at.Format("15"),
		"{mm}", at.Format("04"),
		"{ts}", at.Format("20060102T150405Z"),
	)
	return strings.TrimPrefix(replacer.Replace(template), "/")
}

// KeyVars are the substitutions available to an output.keyTemplate.
type KeyVars struct {
	Database   string
	Collection string
	Config     string
	RunID      string
	Seq        int
	Extension  string
	At         time.Time
}
