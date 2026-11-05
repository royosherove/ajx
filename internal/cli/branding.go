package cli

import (
	"fmt"

	ajx "github.com/royosherove/ajx"
)

func printBanner() {
	fmt.Fprintf(Stdout, "%sv%s\n\n", ajx.Logo, ajx.Version)
}
