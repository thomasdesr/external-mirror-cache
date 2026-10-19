// Package main provides a test file server with auto-generated ETags.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
)

const bindAddress = "localhost:8000"

func main() {
	root, err := os.OpenRoot(".")
	if err != nil {
		panic(err)
	}

	// Create our server
	server := &http.Server{
		Addr:         bindAddress,
		Handler:      LoggingMiddleware(withEtag(root, http.FileServer(http.Dir(".")))),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start server
	log.Printf("Listening on %s", bindAddress)

	err = server.ListenAndServeTLS("localhost.dev.pem", "localhost.dev-key.pem") // Certs generated using mkcert usually
	if err != nil {
		panic(err)
	}
}

func withEtag(root *os.Root, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Generate an Etag for the requested file
		switch r.Method {
		case http.MethodHead, http.MethodGet:
			etag, err := generateEtag(root, r.URL.Path)
			if err == nil {
				w.Header().Set("Etag", strconv.Quote(etag))
			}
		}

		next.ServeHTTP(w, r)
	})
}

// generateEtag stats the file at urlPath within root and generates an Etag for
// it based on stuffs. root rejects paths that escape the served directory.
func generateEtag(root *os.Root, urlPath string) (string, error) {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		name = "."
	}

	fileInfo, err := root.Stat(name)
	if err != nil {
		return "", errorutil.Wrapf(err, "stat %s", name)
	}

	etagBytes := make([]byte, 0, binary.MaxVarintLen64*2)
	etagBytes = binary.AppendVarint(etagBytes, fileInfo.ModTime().UnixNano())
	etagBytes = binary.AppendVarint(etagBytes, fileInfo.Size())

	hash := sha256.Sum256(etagBytes)

	return hex.EncodeToString(hash[:]), nil
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Request received: %q %q", r.Method, r.URL) //nolint:gosec // G706: %q-quoting escapes control chars
		next.ServeHTTP(w, r)
	})
}
