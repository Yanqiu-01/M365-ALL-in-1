//go:build pprofdiag

package main

import (
	"log"
	"net/http"
	_ "net/http/pprof"
)

// Loopback-only profiling endpoint, gated behind a build tag so the shipped
// binary never carries it.
func init() {
	go func() {
		log.Printf("[pprof] listening on 127.0.0.1:6161")
		err := http.ListenAndServe("127.0.0.1:6161", nil)
		log.Printf("[pprof] exited: %v", err)
	}()
}
