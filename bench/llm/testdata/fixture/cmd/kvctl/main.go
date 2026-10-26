// Command kvctl reads and writes keys on a kvsvc server.
//
//	kvctl [-server URL] get KEY
//	kvctl [-server URL] put KEY VALUE
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"example.com/kvsvc/client"
)

func main() {
	base := flag.String("server", "http://localhost:8080", "server URL")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}
	c := client.New(*base)
	ctx := context.Background()
	switch args[0] {
	case "get":
		if len(args) != 2 {
			usage()
		}
		v, err := c.Fetch(ctx, args[1])
		if err != nil {
			fail(err)
		}
		fmt.Println(v)
	case "put":
		if len(args) != 3 {
			usage()
		}
		if err := c.Put(ctx, args[1], args[2]); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kvctl [-server URL] get KEY | put KEY VALUE")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "kvctl:", err)
	os.Exit(1)
}
