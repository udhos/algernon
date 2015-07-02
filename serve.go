package main

import (
	"github.com/bradfitz/http2"
	log "github.com/sirupsen/logrus"
	"github.com/tylerb/graceful"
	"net/http"
	"os"
	"sync"
	"time"
)

// Configuration for serving HTTP, HTTPS and/or HTTP/2
type algernonServerConfig struct {
	productionMode      bool
	serverHost          string
	serverAddr          string
	serverCert          string
	serverKey           string
	serveJustHTTP       bool
	serveJustHTTP2      bool
	shutdownTimeout     time.Duration
	internalLogFilename string
}

// List of functions to run at shutdown
var (
	shutdownFunctions [](func())
	mut               sync.Mutex
	completed         bool
)

// Add a function to the list of functions that will be ran at shutdown
func atShutdown(shutdownFunction func()) {
	mut.Lock()
	defer mut.Unlock()
	shutdownFunctions = append(shutdownFunctions, shutdownFunction)
}

// Run the shutdown functions
func runShutdown() {
	mut.Lock()
	defer mut.Unlock()

	if completed {
		// The shutdown functions have already been called
		return
	}

	if verboseMode {
		log.Info("Initating shutdown")
	}

	// Call the shutdown functions in chronological order (FIFO)
	for _, shutdownFunction := range shutdownFunctions {
		shutdownFunction()
	}

	// Call the shutdown functions in reverse chronological order (LIFO)
	//for i := len(shutdownFunctions) - 1; i >= 0; i-- {
	//	shutdownFunctions[i]()
	//}

	completed = true

	// TODO: Figure out why this sometimes does not happen, while the above lines do happen
	if verboseMode {
		log.Info("Shutdown complete")
	}

	// One final flush
	os.Stdout.Sync()
}

// Create a new graceful server configuration
func newGracefulServer(mux *http.ServeMux, http2support bool, addr string, shutdownTimeout time.Duration) *graceful.Server {
	// Server configuration
	s := &http.Server{
		Addr:    addr,
		Handler: mux,

		// The timeout values is also the maximum time it can take
		// for a complete page of Server-Sent Events (SSE).
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,

		MaxHeaderBytes: 1 << 20,
	}
	if http2support {
		// Enable HTTP/2 support
		http2.ConfigureServer(s, nil)
	}
	gracefulServer := &graceful.Server{
		Server:  s,
		Timeout: shutdownTimeout,
	}
	// Handle ctrl-c
	gracefulServer.ShutdownInitiated = runShutdown
	return gracefulServer
}

// Serve HTTP, HTTP/2 and/or HTTPS. Returns an error if unable to serve, or nil when done serving.
func serve(conf *algernonServerConfig, mux *http.ServeMux, done, ready chan bool) error {

	// If we are not writing internal logs to a file, reduce the verboseness
	http2.VerboseLogs = (conf.internalLogFilename != os.DevNull)

	// Channel to wait and see if we should just serve regular HTTP instead
	justServeRegularHTTP := make(chan bool)

	// Goroutine that wait for a message to just serve regular HTTP, if needed
	go func() {
		<-justServeRegularHTTP // Wait for a message to just serve regular HTTP
		log.Info("Serving HTTP on " + conf.serverAddr)
		HTTPserver := newGracefulServer(mux, false, conf.serverAddr, conf.shutdownTimeout)
		// Start serving. Shut down gracefully at exit.
		if err := HTTPserver.ListenAndServe(); err != nil {
			// If we can't serve regular HTTP on port 80, give up
			fatalExit(err)
		}
	}()

	// Decide which protocol to listen to
	switch {
	case conf.productionMode:
		// Listen for both HTTPS+HTTP/2 and HTTP requests, on different ports
		log.Info("Serving HTTPS + HTTP/2 on " + conf.serverHost + ":443")
		go func() {
			// Start serving. Shut down gracefully at exit.
			// Listen for HTTPS + HTTP/2 requests
			HTTPS2server := newGracefulServer(mux, true, conf.serverHost+":443", conf.shutdownTimeout)
			// Start serving. Shut down gracefully at exit.
			if err := HTTPS2server.ListenAndServeTLS(conf.serverCert, conf.serverKey); err != nil {
				log.Error(err)
			}
		}()
		log.Info("Serving HTTP on " + conf.serverHost + ":80")
		go func() {
			HTTPserver := newGracefulServer(mux, false, conf.serverHost+":80", conf.shutdownTimeout)
			if err := HTTPserver.ListenAndServe(); err != nil {
				// If we can't serve regular HTTP on port 80, give up
				fatalExit(err)
			}
		}()
	case conf.serveJustHTTP2: // It's unusual to serve HTTP/2 withoutHTTPS
		log.Info("Serving HTTP/2 on " + conf.serverAddr)
		go func() {
			// Listen for HTTP/2 requests
			HTTP2server := newGracefulServer(mux, true, conf.serverAddr, conf.shutdownTimeout)
			// Start serving. Shut down gracefully at exit.
			if err := HTTP2server.ListenAndServe(); err != nil {
				justServeRegularHTTP <- true
			}
		}()
	case !(conf.serveJustHTTP2 || conf.serveJustHTTP):
		log.Info("Serving HTTPS + HTTP/2 on " + conf.serverAddr)
		// Listen for HTTPS + HTTP/2 requests
		HTTPS2server := newGracefulServer(mux, true, conf.serverAddr, conf.shutdownTimeout)
		// Start serving. Shut down gracefully at exit.
		go func() {
			if err := HTTPS2server.ListenAndServeTLS(conf.serverCert, conf.serverKey); err != nil {
				log.Error("Not serving HTTPS: ", err)
				log.Info("Use the -t flag for serving regular HTTP")
				// If HTTPS failed (perhaps the key + cert are missing),
				// serve plain HTTP instead
				justServeRegularHTTP <- true
			}
		}()
	default:
		justServeRegularHTTP <- true
	}

	ready <- true // Send a "ready" message to the REPL
	<-done        // Wait for a "done" message from the REPL (or just keep waiting)

	return nil // Done serving
}