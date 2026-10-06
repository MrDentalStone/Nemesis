// build-mod packages mod/Nemesis.lua into a Tabletop Simulator Saved Object
// at dist/Nemesis.json. Drop it into
// Documents\My Games\Tabletop Simulator\Saves\Saved Objects\ and spawn it
// from Objects > Saved Objects. -install does the copy for you.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MrDentalStone/Nemesis/internal/padobj"
)

func main() {
	out := flag.String("out", filepath.Join("dist", "Nemesis.json"), "output path")
	install := flag.Bool("install", false, "also copy into this PC's TTS Saved Objects folder")
	flag.Parse()

	// read the file, not the embedded copy, so an edit needs no rebuild first
	lua, err := os.ReadFile(filepath.Join("mod", "Nemesis.lua"))
	if err != nil {
		fatal(err)
	}
	data, err := padobj.SaveFile(string(lua))
	if err != nil {
		fatal(err)
	}
	write := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", path)
	}
	write(*out)
	if *install {
		write(filepath.Join(padobj.SavedObjectsDir(""), "Nemesis.json"))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "build-mod:", err)
	os.Exit(1)
}
