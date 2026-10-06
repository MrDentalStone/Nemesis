// Package padobj builds the Nemesis pad as a Tabletop Simulator object from
// the embedded mod/Nemesis.lua: the Saved Object file build-mod writes, the
// object the server spawns on a table that has no pad, and the file the
// installer drops into TTS's Saved Objects folder.
package padobj

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/MrDentalStone/Nemesis"
)

// Lua is the pad's script as built into this binary.
func Lua() string {
	raw, err := fs.ReadFile(nemesis.Files, "mod/Nemesis.lua")
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(raw)
}

// Object is the pad as one TTS object state, at (x, y, z) facing yaw.
func Object(lua string, x, y, z, yaw float64) map[string]any {
	return map[string]any{
		"Name": "BlockSquare",
		"Transform": map[string]float64{
			"posX": x, "posY": y, "posZ": z,
			"rotX": 0, "rotY": yaw, "rotZ": 0,
			"scaleX": 5, "scaleY": 0.3, "scaleZ": 3.2,
		},
		"Nickname": "Nemesis",
		"Description": "The AI player's seat at the table. Pick a free colour with < >, then use the buttons to " +
			"set the play mode, flag new players, hand the AI the turn, answer its questions, or STOP it. " +
			"The AI can only ever see its own hand.",
		"ColorDiffuse":   map[string]float64{"r": 0.12, "g": 0.10, "b": 0.16},
		"Locked":         false,
		"Grid":           true,
		"Snap":           true,
		"Tooltip":        true,
		"LuaScript":      lua,
		"LuaScriptState": "",
		"XmlUI":          "",
	}
}

// SaveFile is the Saved Object file (Nemesis.json) for the given script.
func SaveFile(lua string) ([]byte, error) {
	save := map[string]any{
		"SaveName": "Nemesis", "Date": "", "VersionNumber": "", "GameMode": "", "GameType": "",
		"Tags": []string{}, "Gravity": 0.5, "PlayArea": 0.5, "Table": "", "Sky": "", "Note": "", "Rules": "",
		"LuaScript": "", "LuaScriptState": "", "XmlUI": "",
		"ObjectStates": []any{Object(lua, 0, 1, 0, 0)},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(save); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// SavedObjectsDir is TTS's Saved Objects folder under the given Documents
// folder (Windows; pass "" to use ~/Documents) or the platform default.
func SavedObjectsDir(documents string) string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Tabletop Simulator", "Saves", "Saved Objects")
	case "linux":
		return filepath.Join(home, ".local", "share", "Tabletop Simulator", "Saves", "Saved Objects")
	}
	if documents == "" {
		documents = filepath.Join(home, "Documents")
	}
	return filepath.Join(documents, "My Games", "Tabletop Simulator", "Saves", "Saved Objects")
}
