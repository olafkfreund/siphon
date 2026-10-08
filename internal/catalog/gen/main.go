// Command gen writes nix/catalog-packages.json from the catalogue.
package main

import (
	"fmt"
	"os"

	"github.com/olafkfreund/siphon/internal/catalog"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen <out.json>")
		os.Exit(2)
	}
	b, err := catalog.NixPackagesJSON()
	if err == nil {
		err = os.WriteFile(os.Args[1], b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
