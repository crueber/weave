// Command weave-cli is the slim Weave client: the same CLI as `weave`, without the server and
// database. Release archives ship this binary as `weave`.
package main

import (
	"os"

	"github.com/crueber/weave/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
