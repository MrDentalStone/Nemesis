package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// The JSON configs we touch (Claude Desktop, Gemini CLI, ~/.claude.json)
// belong to other programs and their users. Go maps would sort the keys, so
// objects are edited as ordered key/raw-value lists: only our one entry
// changes, everything else keeps its value and its place.

type member struct {
	key string
	val json.RawMessage
}

func parseObject(data []byte) ([]member, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v", tok)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, member{k, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func writeObject(ms []member) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func indent(raw []byte) []byte {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return raw
	}
	b.WriteByte('\n')
	return b.Bytes()
}

func find(ms []member, key string) int {
	for i, m := range ms {
		if m.key == key {
			return i
		}
	}
	return -1
}

// SetServer puts entry at data[section][name] (section is "mcpServers"),
// creating the file content, the section or the entry as needed.
func SetServer(data []byte, section, name string, entry any) ([]byte, error) {
	top, err := parseObject(data)
	if err != nil {
		return nil, err
	}
	var servers []member
	i := find(top, section)
	if i >= 0 && !bytes.Equal(bytes.TrimSpace(top[i].val), []byte("null")) {
		if servers, err = parseObject(top[i].val); err != nil {
			return nil, fmt.Errorf("%s: %w", section, err)
		}
	}
	val, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	if j := find(servers, name); j >= 0 {
		servers[j].val = val
	} else {
		servers = append(servers, member{name, val})
	}
	if i >= 0 {
		top[i].val = writeObject(servers)
	} else {
		top = append(top, member{section, writeObject(servers)})
	}
	return indent(writeObject(top)), nil
}

// RemoveServer deletes data[section][name]; changed is false when it was
// not there.
func RemoveServer(data []byte, section, name string) (out []byte, changed bool, err error) {
	top, err := parseObject(data)
	if err != nil {
		return nil, false, err
	}
	i := find(top, section)
	if i < 0 {
		return data, false, nil
	}
	servers, err := parseObject(top[i].val)
	if err != nil {
		return nil, false, err
	}
	j := find(servers, name)
	if j < 0 {
		return data, false, nil
	}
	servers = append(servers[:j], servers[j+1:]...)
	top[i].val = writeObject(servers)
	return indent(writeObject(top)), true, nil
}

// HasServer reports whether data[section][name] exists.
func HasServer(data []byte, section, name string) bool {
	top, err := parseObject(data)
	if err != nil {
		return false
	}
	i := find(top, section)
	if i < 0 {
		return false
	}
	servers, err := parseObject(top[i].val)
	return err == nil && find(servers, name) >= 0
}
