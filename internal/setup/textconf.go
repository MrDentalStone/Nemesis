package setup

import (
	"regexp"
	"strings"
)

// Codex keeps its servers in TOML and the agent its key in a KEY=value file.
// Both are edited as text so comments, ordering and everything we do not own
// survive untouched.

var tableHeader = regexp.MustCompile(`^\s*\[\[?\s*([^\]]+?)\s*\]\]?\s*(#.*)?$`)

// tableName normalises a header's dotted name: mcp_servers."nemesis" and
// mcp_servers.nemesis are the same table.
func tableName(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// splitTable cuts text into the lines before, inside and after the table
// `name` (its sub-tables like name.env count as inside). found is false when
// the table is not there.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

func ownsHeader(line, name string) bool {
	m := tableHeader.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	n := tableName(m[1])
	return n == name || strings.HasPrefix(n, name+".")
}

func isHeader(line string) bool { return tableHeader.MatchString(line) }

// removeTables drops every block owned by name and reports whether any was.
func removeTables(lines []string, name string) ([]string, bool, int) {
	var out []string
	removed, at := false, -1
	for i := 0; i < len(lines); {
		if ownsHeader(lines[i], name) {
			if at < 0 {
				at = len(out)
			}
			removed = true
			i++
			for i < len(lines) && !isHeader(lines[i]) {
				i++
			}
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return out, removed, at
}

// SetTOMLTable replaces table `name` (and its sub-tables) with body, a list
// of key = value lines, or appends it when it is not there.
func SetTOMLTable(text, name string, body []string) string {
	lines, _, at := removeTables(splitLines(text), name)
	block := append([]string{"[" + name + "]"}, body...)
	if at < 0 {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, block...)
	} else {
		block = append(block, "")
		lines = append(lines[:at], append(block, lines[at:]...)...)
	}
	return finish(lines)
}

// RemoveTOMLTable drops table `name` and its sub-tables.
func RemoveTOMLTable(text, name string) (string, bool) {
	lines, removed, _ := removeTables(splitLines(text), name)
	if !removed {
		return text, false
	}
	return finish(lines), true
}

func finish(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	// collapse the double blank line a removed block can leave behind
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// TOMLString quotes s: a literal string when it can be (Windows paths need
// no escaping that way), a basic string otherwise.
func TOMLString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// TOMLArray renders a string array.
func TOMLArray(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = TOMLString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// SetEnv sets KEY=value lines in an env file; an empty value removes the
// key. Comments and other keys stay as they are.
func SetEnv(text string, vals map[string]string) string {
	lines := splitLines(text)
	done := map[string]bool{}
	var out []string
	for _, l := range lines {
		k, _, ok := strings.Cut(strings.TrimSpace(l), "=")
		k = strings.TrimSpace(k)
		if v, mine := vals[k]; ok && mine && !strings.HasPrefix(k, "#") {
			if done[k] || v == "" {
				continue
			}
			out = append(out, k+"="+v)
			done[k] = true
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	for _, k := range sortedKeys(vals) {
		if !done[k] && vals[k] != "" {
			out = append(out, k+"="+vals[k])
		}
	}
	return finish(out)
}

// GetEnv reads KEY from an env file's text.
func GetEnv(text, key string) string {
	for _, l := range splitLines(text) {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
