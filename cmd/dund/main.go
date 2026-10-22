// Command dund is dun's session service; it is also `dun dund`. See
// internal/dundcli.
package main

import (
	"os"

	"github.com/iodesystems/dun/internal/dundcli"
)

func main() { os.Exit(dundcli.Main(os.Args[1:])) }
