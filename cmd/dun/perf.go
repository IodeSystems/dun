package main

import (
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on the handler below
	"os"
	"time"
)

// startPprof serves net/http/pprof when DUN_PPROF is set to an address. Opt-in
// and local: it exposes the process's internals, so it must never be on by
// default or bound anywhere but a loopback address the user chose.
func startPprof() {
	addr := os.Getenv("DUN_PPROF")
	if addr == "" {
		return
	}
	go func() {
		log.Printf("dun: pprof on http://%s/debug/pprof/", addr)
		srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("dun: pprof: %v", err)
		}
	}()
}
