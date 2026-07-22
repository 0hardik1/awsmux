// Command awsmux runs one AWS CLI command across a whole fleet of accounts,
// with identity preflight, risk classification, and an approval boundary for
// anything that mutates.
package main

import (
	"os"

	"awsmux/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
