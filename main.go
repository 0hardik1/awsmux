package main

import (
	"os"

	"awsmux/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
