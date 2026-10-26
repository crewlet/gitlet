// Command kvsvc runs the key-value service.
package main

import (
	"flag"
	"log"
	"net/http"

	"example.com/kvsvc/config"
	"example.com/kvsvc/server"
	"example.com/kvsvc/store"
)

func main() {
	path := flag.String("config", "", "configuration file")
	flag.Parse()
	cfg := config.Default()
	if *path != "" {
		c, err := config.Load(*path)
		if err != nil {
			log.Fatal(err)
		}
		cfg = c
	}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	st := store.New(cfg.MaxKeys)
	log.Printf("listening on %s", cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, server.New(cfg, st).Handler()))
}
