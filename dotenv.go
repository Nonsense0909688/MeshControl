package main

// Minimal .env loader (no dependencies).
//
// Supported: KEY=value, `export KEY=value`, # comments (full-line and inline
// after whitespace), "double quoted" values with \n \r \t \" \\ escapes, and
// 'single quoted' literal values. Variables already set in the real
// environment always win over the file.

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func unquoteDouble(s string) (val string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), true
		case '\\':
			if i+1 >= len(s) {
				return "", false
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default: // \" \\ and anything else: keep the character itself
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", false // no closing quote
}

func parseEnvValue(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(v, `"`):
		return unquoteDouble(v[1:])
	case strings.HasPrefix(v, `'`):
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", false
		}
		return v[1 : 1+end], true
	}
	for i := 1; i < len(v); i++ { // inline comment: whitespace followed by #
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			v = v[:i]
			break
		}
	}
	if strings.HasPrefix(v, "#") {
		v = ""
	}
	return strings.TrimSpace(v), true
}

// loadDotEnv reads path and sets every variable that is not already set.
// A missing file is fine unless required is true. Returns how many variables
// were applied from the file.
func loadDotEnv(path string, required bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	applied := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // UTF-8 BOM
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !envKeyRe.MatchString(key) {
			return applied, fmt.Errorf("%s:%d: expected KEY=value", path, n)
		}
		val, ok := parseEnvValue(raw)
		if !ok {
			return applied, fmt.Errorf("%s:%d: unterminated quote", path, n)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // real environment wins
		}
		if err := os.Setenv(key, val); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, sc.Err()
}
