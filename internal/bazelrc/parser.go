// Package bazelrc parses .bazelrc files.
//
// Bazel's .bazelrc format is documented at
// https://bazel.build/run/bazelrc. Each non-blank, non-comment line has the
// shape:
//
//	<command>[:<config>] <arg1> <arg2> ...
//
// Commands include the usual bazel commands (build, test, run, query, ...) as
// well as the pseudo-commands `startup`, `common`, `always`, `import`, and
// `try-import`. Lines can be continued with a trailing backslash, and comments
// start with `#`.
//
// Arguments are tokenized with shell-like rules: whitespace separates tokens,
// and single/double quotes group tokens. A backslash escapes the next
// character. This mirrors Bazel's own rc-file tokenizer.
package bazelrc

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Entry is one parsed logical line from a .bazelrc file. A logical line spans
// physical lines joined by trailing backslash continuations.
type Entry struct {
	// Command is the bazel command the line applies to, e.g. "build",
	// "startup", "common", "import", "try-import". Never empty in a
	// well-formed entry.
	Command string

	// Config is the config suffix (`build:linux` → "linux"). Empty when
	// the line has no suffix.
	Config string

	// Args are the tokenized arguments following the command.
	Args []string

	// Source is the file the entry came from (as provided to Parse).
	Source string

	// Line is the 1-based line number of the start of the logical line.
	Line int
}

// Parse reads a single .bazelrc file's contents from r and returns its
// entries. The filename is used for error messages and Entry.Source.
//
// Parse does not follow `import` or `try-import` directives; those come back
// as entries and are the caller's job to expand. See Loader for the full
// discovery/expansion pipeline.
func Parse(filename string, r io.Reader) ([]Entry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var out []Entry
	var buf strings.Builder
	startLine := 0
	lineno := 0

	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		raw := buf.String()
		buf.Reset()
		tokens, err := tokenize(raw)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", filename, startLine, err)
		}
		if len(tokens) == 0 {
			return nil
		}
		cmd, cfg := splitCommand(tokens[0])
		if cmd == "" {
			return fmt.Errorf("%s:%d: empty command", filename, startLine)
		}
		out = append(out, Entry{
			Command: cmd,
			Config:  cfg,
			Args:    tokens[1:],
			Source:  filename,
			Line:    startLine,
		})
		return nil
	}

	for scanner.Scan() {
		lineno++
		line := stripComment(scanner.Text())
		trimmed := strings.TrimRight(line, " \t")
		continued := strings.HasSuffix(trimmed, `\`)
		if continued {
			trimmed = strings.TrimSuffix(trimmed, `\`)
		}
		if buf.Len() == 0 {
			startLine = lineno
			// leading whitespace on the first physical line is
			// insignificant.
			trimmed = strings.TrimLeft(trimmed, " \t")
			if trimmed == "" && !continued {
				continue
			}
		} else {
			// join continuations with a single space
			buf.WriteByte(' ')
		}
		buf.WriteString(trimmed)
		if !continued {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseFile parses the .bazelrc file at path.
func ParseFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(path, f)
}

// stripComment removes a `#` comment from the end of a line. Bazel treats `#`
// as a comment only at the start of a line (after optional whitespace) or
// after whitespace, so `--foo=bar#baz` is not a comment. To keep the parser
// simple and match Bazel's behavior, we only strip a `#` that follows
// whitespace or begins the line.
func stripComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i]
			}
		}
	}
	return line
}

// splitCommand splits `build:linux` into ("build", "linux"). If there's no
// colon, config is empty.
func splitCommand(tok string) (cmd, config string) {
	if i := strings.IndexByte(tok, ':'); i >= 0 {
		return tok[:i], tok[i+1:]
	}
	return tok, ""
}

// tokenize splits s into shell-style tokens. Whitespace separates tokens.
// Single and double quotes group tokens. Backslash escapes the next character
// outside single quotes.
func tokenize(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	inToken := false

	commit := func() {
		if inToken {
			out = append(out, cur.String())
			cur.Reset()
			inToken = false
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(s):
			cur.WriteByte(s[i+1])
			inToken = true
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			inToken = true
		case c == '"' && !inSingle:
			inDouble = !inDouble
			inToken = true
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			commit()
		default:
			cur.WriteByte(c)
			inToken = true
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quoted string")
	}
	commit()
	return out, nil
}

// ExtractFlag returns the value of a --flag from args. Both `--flag=value` and
// `--flag value` forms are recognized. When the flag appears multiple times,
// the last occurrence wins (matching Bazel's semantics). ok is false if the
// flag is absent.
func ExtractFlag(args []string, name string) (value string, ok bool) {
	prefix := "--" + name
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == prefix:
			if i+1 < len(args) {
				value = args[i+1]
				ok = true
				i++
			}
		case strings.HasPrefix(a, prefix+"="):
			value = strings.TrimPrefix(a, prefix+"=")
			ok = true
		}
	}
	return value, ok
}

// HasFlag reports whether a bare flag (e.g. `--nosystem_rc`) appears in args.
// The flag is matched as an exact token. `--flag=value` does not match.
func HasFlag(args []string, name string) bool {
	prefix := "--" + name
	for _, a := range args {
		if a == prefix {
			return true
		}
	}
	return false
}
