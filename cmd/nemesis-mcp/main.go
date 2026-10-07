// nemesis-mcp is the stdio MCP server that lets an AI play at a Tabletop
// Simulator Commander table through the Nemesis pad.
//
//	claude mcp add nemesis -- C:\path\to\nemesis-mcp.exe
//
// See docs/CLIENTS.md for other MCP clients.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/app"
)

// set by the release build (-ldflags "-X main.version=...")
var version = "v1.1.0"

func main() {
	opts := app.RegisterFlags(flag.CommandLine)
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("nemesis-mcp", version)
		return
	}

	log.SetOutput(os.Stderr)
	log.SetPrefix("nemesis: ")

	rt, err := app.Start(opts, version, log.Printf)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	mode := "play"
	if opts.Dev {
		mode = "DEV (exec_lua enabled)"
	}
	fmt.Fprintln(os.Stderr, rt.Summary(version, mode))
	if err := rt.Server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
