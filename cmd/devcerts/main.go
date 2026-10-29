// Command devcerts writes development certificates for mutual TLS.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/chxragsaxena/pullwise-knowledge/internal/devcert"
)

func main() {
	out := flag.String("out", "certs", "directory to write the certificates to")
	hosts := flag.String("hosts", "localhost,127.0.0.1,knowledge", "comma-separated names and IPs the server certificate is valid for")
	flag.Parse()

	if err := devcert.Generate(*out, strings.Split(*hosts, ",")); err != nil {
		fmt.Fprintln(os.Stderr, "devcerts:", err)
		os.Exit(1)
	}
	fmt.Println(devcert.Fprint(*out))
}
