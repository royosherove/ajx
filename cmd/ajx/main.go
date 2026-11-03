// Command ajx is the native AJX command line. It reads and writes the same
// trial files and output folders as the Python CLI in skills/ajx/bin/ajx.
package main

import (
	"os"

	"github.com/royosherove/ajx/internal/cli"

	// Built-in harness, auth, runner and check adapters register in init().
	_ "github.com/royosherove/ajx/internal/builtin"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
