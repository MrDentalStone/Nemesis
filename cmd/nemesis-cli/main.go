// nemesis-cli drives nemesis-mcp from a terminal for smoke tests:
//
//	go run ./cmd/nemesis-cli table_state
//	go run ./cmd/nemesis-cli say '{"text":"hello table"}'
//	go run ./cmd/nemesis-cli -dev exec_lua '{"script":"return 1+1"}'
//	go run ./cmd/nemesis-cli -list
//
// It launches bin/nemesis-mcp(.exe) (or -bin) over stdio, the way an MCP client would.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	def := "bin/nemesis-mcp"
	if runtime.GOOS == "windows" {
		def += ".exe"
	}
	bin := flag.String("bin", def, "server binary")
	dev := flag.Bool("dev", false, "start the server with --dev")
	list := flag.Bool("list", false, "list tools and exit")
	timeout := flag.Duration("timeout", 90*time.Second, "call timeout")
	flag.Parse()

	args := []string{}
	if *dev {
		args = append(args, "--dev")
	}
	cmd := exec.Command(*bin, args...)
	cmd.Stderr = os.Stderr

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "nemesis-cli", Version: "dev"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fail(err)
	}
	defer sess.Close()

	if *list {
		tools, err := sess.ListTools(ctx, nil)
		if err != nil {
			fail(err)
		}
		for _, t := range tools.Tools {
			fmt.Printf("%-16s %s\n", t.Name, t.Description)
		}
		return
	}
	if flag.NArg() < 1 {
		fail(fmt.Errorf("usage: nemesis-cli [-dev] <tool> [json-args|@file] [-- <tool> [json-args] ...]"))
	}
	// Several calls in one server session: "tool1 '{...}' -- tool2 -- tool3 '{...}'".
	// Events emitted between calls are kept, as they are for a long-lived client.
	rest := flag.Args()
	exit := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		var argsJSON any = map[string]any{}
		if len(rest) > 0 && rest[0] != "--" {
			raw := []byte(rest[0])
			if strings.HasPrefix(rest[0], "@") { // @file: JSON args too big for a command line
				b, err := os.ReadFile(rest[0][1:])
				if err != nil {
					fail(fmt.Errorf("%s: %w", name, err))
				}
				raw = b
			}
			if err := json.Unmarshal(raw, &argsJSON); err != nil {
				fail(fmt.Errorf("%s: args must be JSON: %w", name, err))
			}
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		fmt.Printf("### %s\n", name)
		if name == "sleep" { // test helper: sleep '{"s": seconds}'
			secs, _ := argsJSON.(map[string]any)["s"].(float64)
			time.Sleep(time.Duration(secs * float64(time.Second)))
			continue
		}
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: argsJSON})
		if err != nil {
			fail(err)
		}
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				fmt.Println(t.Text)
			}
		}
		if res.IsError {
			exit = 2
		}
	}
	os.Exit(exit)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "nemesis-cli:", err)
	os.Exit(1)
}
