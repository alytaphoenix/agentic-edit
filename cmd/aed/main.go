// Command aed is an agentic file-editing CLI. It turns natural-language edit
// requests into safe awk/sed/grep operations, optionally assisted by Laya.
package main

import (
	"os"

	"agentic-edit/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
