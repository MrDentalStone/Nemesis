package padobj

import (
	"regexp"
	"strings"
	"testing"
)

// TTS runs the pad on MoonSharp, which gives up with "pattern too complex"
// when a lazy '.-' backtracks over long oracle text ('^(.-):' on a line with
// no colon). Find separators with string.find(s, sep, 1, true) instead. A
// line that only ever sees short text can opt out with "pattern-ok".
func TestLuaHasNoLazyPatterns(t *testing.T) {
	lazy := regexp.MustCompile(`'[^']*\.-[^']*'`)
	for i, line := range strings.Split(Lua(), "\n") {
		code, _, _ := strings.Cut(line, "--")
		if strings.Contains(line, "pattern-ok") || !lazy.MatchString(code) {
			continue
		}
		t.Errorf("mod/Nemesis.lua:%d uses a lazy '.-' pattern: %s", i+1, strings.TrimSpace(line))
	}
}
