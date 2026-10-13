// Command stub stands in for auth, edge and central in the rollout e2e: it
// binds the same ports and answers the same probes, and every reply carries
// the version baked into the image, so a traffic client can see which side of
// a rollout served it. It does nothing else -- no database, no config.
//
// Environment (the real services' names):
//
//	STUB_ROLE         auth | edge | central (baked into the image)
//	STUB_VERSION      the version this image stands for (baked in)
//	BIND_HOST         address to listen on (default: all)
//	PORT, DPORT       main and diagnostics listeners (all roles)
//	APORT, MPORT      auth only: plain "additional" and TLS "mutual" listeners
//	                  (default 8082 and 8083, like the real service)
//	STUB_READY_DELAY  seconds /readiness answers 503 after start (default 2)
//	STUB_TERM_DELAY   seconds between SIGTERM and exit (default 0), to see a
//	                  slow shutdown the way a JVM's is
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func main() {
	role, version := os.Getenv("STUB_ROLE"), os.Getenv("STUB_VERSION")
	if role == "" || version == "" {
		log.Fatal("STUB_ROLE and STUB_VERSION must be set")
	}
	// BIND_HOST, like the real services: compose sets it to 127.0.0.1 on vps.
	bind := os.Getenv("BIND_HOST")
	start := time.Now()
	readyAfter := seconds("STUB_READY_DELAY", 2)

	var servers []*http.Server
	serve := func(name, port string, h http.Handler, useTLS bool) {
		if port == "" {
			log.Fatalf("%s is not set", name)
		}
		s := &http.Server{Addr: bind + ":" + port, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		if useTLS {
			s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{selfSigned()}}
		}
		servers = append(servers, s)
		go func() {
			var err error
			if useTLS {
				err = s.ListenAndServeTLS("", "")
			} else {
				err = s.ListenAndServe()
			}
			if err != http.ErrServerClosed {
				log.Fatalf("%s on :%s: %v", name, port, err)
			}
		}()
	}

	diag := http.NewServeMux()
	diag.HandleFunc("/readiness", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < readyAfter {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	serve("DPORT", os.Getenv("DPORT"), diag, false)
	// Like the real services, auth binds 8082/8083 unless told otherwise, so a
	// missed override fails here the way it would in production (a clash).
	aport, mport := os.Getenv("APORT"), os.Getenv("MPORT")
	if aport == "" {
		aport = "8082"
	}
	if mport == "" {
		mport = "8083"
	}

	handler := echo(role, version)
	serve("PORT", os.Getenv("PORT"), handler, false)
	if role == "auth" {
		serve("APORT", aport, echo(role+"-additional", version), false)
		serve("MPORT", mport, echo(role+"-mtls", version), true)
	}
	log.Printf("%s %s up (ready in %s)", role, version, readyAfter)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	time.Sleep(seconds("STUB_TERM_DELAY", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *http.Server) { defer wg.Done(); _ = s.Shutdown(ctx) }(s)
	}
	wg.Wait()
}

// echo answers the probes the CLI and the proxy rely on, and anything else
// (GET or POST) with 200 and the request's body size, so a client can check a
// POST made it through whole.
func echo(role, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stub-Version", version)
		w.Header().Set("X-Stub-Role", role)
		switch {
		case role == "auth" && r.URL.Path == "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"issuer":"stub","version":%q}`, version)
		case r.URL.Path == "/slow":
			// A request that stays in flight for ?ms=N, so a connection cut
			// while it is open is an error, not a silent reconnect.
			ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
			time.Sleep(time.Duration(ms) * time.Millisecond)
			fmt.Fprintf(w, "%s %s slow=%d\n", role, version, ms)
		case role == "edge" && r.URL.Path == "/permissions/me":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Fprintf(w, "%s %s %s body=%d\n", role, version, r.Method, n)
		}
	})
}

func seconds(name string, def int) time.Duration {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(def) * time.Second
}

// selfSigned is a throwaway certificate for the mutual-TLS listener: the
// traffic client does not verify it, and the stub asks for no client
// certificate -- what the e2e measures is the connection surviving, not
// authentication.
func selfSigned() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "stub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
