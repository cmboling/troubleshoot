package redact

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"regexp"
	"sync"

	"github.com/gobwas/glob"
	"github.com/pkg/errors"
	troubleshootv1beta1 "github.com/replicatedhq/troubleshoot/pkg/apis/troubleshoot/v1beta1"
)

const (
	MASK_TEXT = "***HIDDEN***"
)

var allRedactions RedactionList
var redactionListMut sync.Mutex
var pendingRedactions sync.WaitGroup

func init() {
	allRedactions = RedactionList{
		ByRedactor: map[string][]Redaction{},
		ByFile:     map[string][]Redaction{},
	}
}

type Redactor interface {
	Redact(input io.Reader) io.Reader
}

// Redactions are indexed both by the file affected and by the name of the redactor
type RedactionList struct {
	ByRedactor map[string][]Redaction
	ByFile     map[string][]Redaction
}

type Redaction struct {
	RedactorName      string
	CharactersRemoved int
	Line              int
	File              string
}

func Redact(input []byte, path string, additionalRedactors []*troubleshootv1beta1.Redact) ([]byte, error) {
	redactors, err := getRedactors(path)
	if err != nil {
		return nil, err
	}

	builtRedactors, err := buildAdditionalRedactors(path, additionalRedactors)
	if err != nil {
		return nil, errors.Wrap(err, "build custom redactors")
	}
	redactors = append(redactors, builtRedactors...)

	nextReader := io.Reader(bytes.NewReader(input))
	for _, r := range redactors {
		nextReader = r.Redact(nextReader)
	}

	redacted, err := ioutil.ReadAll(nextReader)
	if err != nil {
		return nil, err
	}

	return redacted, nil
}

func GetRedactionList() RedactionList {
	pendingRedactions.Wait()
	redactionListMut.Lock()
	defer redactionListMut.Unlock()
	return allRedactions
}

func ResetRedactionList() {
	redactionListMut.Lock()
	defer redactionListMut.Unlock()
	allRedactions = RedactionList{
		ByRedactor: map[string][]Redaction{},
		ByFile:     map[string][]Redaction{},
	}
}

func buildAdditionalRedactors(path string, redacts []*troubleshootv1beta1.Redact) ([]Redactor, error) {
	additionalRedactors := []Redactor{}
	for i, redact := range redacts {
		if redact == nil {
			continue
		}

		// check if redact matches path
		matches, err := redactMatchesPath(path, redact)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}

		withinRedactNum := 0 // give unique redaction names

		for _, re := range redact.Regex {
			r, err := NewSingleLineRedactor(re, MASK_TEXT, path, redactorName(i, withinRedactNum, redact.Name, "regex", ""))
			if err != nil {
				return nil, errors.Wrapf(err, "redactor %q", re)
			}
			additionalRedactors = append(additionalRedactors, r)
		}

		for _, literal := range redact.Values {
			additionalRedactors = append(additionalRedactors, literalString(literal, path, redactorName(i, withinRedactNum, redact.Name, "literal", "")))
		}

		for _, re := range redact.MultiLine {
			r, err := NewMultiLineRedactor(re.Selector, re.Redactor, MASK_TEXT, path, redactorName(i, withinRedactNum, redact.Name, "multiLine", ""))
			if err != nil {
				return nil, errors.Wrapf(err, "multiline redactor %+v", re)
			}
			additionalRedactors = append(additionalRedactors, r)
		}

		for _, yaml := range redact.Yaml {
			r := NewYamlRedactor(yaml, path, redactorName(i, withinRedactNum, redact.Name, "yaml", ""))
			additionalRedactors = append(additionalRedactors, r)
		}
	}
	return additionalRedactors, nil
}

func redactMatchesPath(path string, redact *troubleshootv1beta1.Redact) (bool, error) {
	if redact.File == "" && len(redact.Files) == 0 {
		return true, nil
	}

	globs := []glob.Glob{}

	if redact.File != "" {
		newGlob, err := glob.Compile(redact.File, '/')
		if err != nil {
			return false, errors.Wrapf(err, "invalid file glob string %q", redact.File)
		}
		globs = append(globs, newGlob)
	}

	for i, fileGlobString := range redact.Files {
		newGlob, err := glob.Compile(fileGlobString, '/')
		if err != nil {
			return false, errors.Wrapf(err, "invalid file glob string %d %q", i, fileGlobString)
		}
		globs = append(globs, newGlob)
	}

	for _, thisGlob := range globs {
		if thisGlob.Match(path) {
			return true, nil
		}
	}

	return false, nil
}

func getRedactors(path string) ([]Redactor, error) {
	// TODO: Make this configurable

	// (?i) makes it case insensitive
	// groups named with `?P<mask>` will be masked
	// groups named with `?P<drop>` will be removed (replaced with empty strings)
	singleLines := []string{
		// ipv4
		`(?P<mask>\b(?P<drop>25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?P<drop>25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?P<drop>25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.(?P<drop>25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b)`,
		// TODO: ipv6
		// aws secrets
		`(?i)(\\\"name\\\":\\\"[^\"]*SECRET_?ACCESS_?KEY\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		`(?i)(\\\"name\\\":\\\"[^\"]*ACCESS_?KEY_?ID\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		`(?i)(\\\"name\\\":\\\"[^\"]*OWNER_?ACCOUNT\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		// passwords in general
		`(?i)(\\\"name\\\":\\\"[^\"]*password[^\"]*\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		// tokens in general
		`(?i)(\\\"name\\\":\\\"[^\"]*token[^\"]*\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		`(?i)(\\\"name\\\":\\\"[^\"]*database[^\"]*\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		`(?i)(\\\"name\\\":\\\"[^\"]*user[^\"]*\\\",\\\"value\\\":\\\")(?P<mask>[^\"]*)(\\\")`,
		// connection strings with username and password
		// http://user:password@host:8888
		`(?i)(https?|ftp)(:\/\/)(?P<mask>[^:\"\/]+){1}(:)(?P<mask>[^@\"\/]+){1}(?P<host>@[^:\/\s\"]+){1}(?P<port>:[\d]+)?`,
		// user:password@tcp(host:3309)/db-name
		`\b(?P<mask>[^:\"\/]*){1}(:)(?P<mask>[^:\"\/]*){1}(@tcp\()(?P<mask>[^:\"\/]*){1}(?P<port>:[\d]*)?(\)\/)(?P<mask>[\w\d\S-_]+){1}\b`,
		// standard postgres and mysql connnection strings
		`(?i)(Data Source *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(location *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(User ID *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(password *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(Server *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(Database *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(Uid *= *)(?P<mask>[^\;]+)(;)`,
		`(?i)(Pwd *= *)(?P<mask>[^\;]+)(;)`,
	}

	redactors := make([]Redactor, 0)
	for i, re := range singleLines {
		r, err := NewSingleLineRedactor(re, MASK_TEXT, path, redactorName(-1, i, "", "defaultRegex", re))
		if err != nil {
			return nil, err // maybe skip broken ones?
		}
		redactors = append(redactors, r)
	}

	doubleLines := []struct {
		line1 string
		line2 string
	}{
		{
			line1: `(?i)"name": *"[^\"]*SECRET_?ACCESS_?KEY[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
		{
			line1: `(?i)"name": *"[^\"]*ACCESS_?KEY_?ID[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
		{
			line1: `(?i)"name": *"[^\"]*OWNER_?ACCOUNT[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
		{
			line1: `(?i)"name": *".*password[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
		{
			line1: `(?i)"name": *".*token[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
		{
			line1: `(?i)"name": *".*database[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
		{
			line1: `(?i)"name": *".*user[^\"]*"`,
			line2: `(?i)("value": *")(?P<mask>.*[^\"]*)(")`,
		},
	}

	for i, l := range doubleLines {
		r, err := NewMultiLineRedactor(l.line1, l.line2, MASK_TEXT, path, redactorName(-1, i, "", "defaultMultiLine", l.line1))
		if err != nil {
			return nil, err // maybe skip broken ones?
		}
		redactors = append(redactors, r)
	}

	return redactors, nil
}

func getReplacementPattern(re *regexp.Regexp, maskText string) string {
	substStr := ""
	for i, name := range re.SubexpNames() {
		if i == 0 { // index 0 is the entire string
			continue
		}
		if name == "" {
			substStr = fmt.Sprintf("%s$%d", substStr, i)
		} else if name == "mask" {
			substStr = fmt.Sprintf("%s%s", substStr, maskText)
		} else if name == "drop" {
			// no-op, string is just dropped from result
		} else {
			substStr = fmt.Sprintf("%s${%s}", substStr, name)
		}
	}
	return substStr
}

func readLine(r *bufio.Reader) (string, error) {
	var completeLine []byte
	for {
		var line []byte
		line, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}

		completeLine = append(completeLine, line...)
		if !isPrefix {
			break
		}
	}
	return string(completeLine), nil
}

func addRedaction(redaction Redaction) {
	pendingRedactions.Add(1)
	go func(redaction Redaction) {
		redactionListMut.Lock()
		defer redactionListMut.Unlock()
		defer pendingRedactions.Done()
		allRedactions.ByRedactor[redaction.RedactorName] = append(allRedactions.ByRedactor[redaction.RedactorName], redaction)
		allRedactions.ByFile[redaction.File] = append(allRedactions.ByFile[redaction.File], redaction)
	}(redaction)
}

func redactorName(redactorNum, withinRedactorNum int, redactorName, redactorType, redactorLiteral string) string {
	if redactorName != "" {
		return fmt.Sprintf("%s-%d", redactorName, withinRedactorNum)
	}
	if redactorLiteral == "" {
		return fmt.Sprintf("unnamed-%d.%d-%s", redactorNum, withinRedactorNum, redactorType)
	}
	return fmt.Sprintf("%s.%d-%q", redactorType, withinRedactorNum, redactorLiteral)
}
