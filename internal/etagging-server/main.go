package main

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const bindAddress = "localhost:8000"

func main() {
	// Create our server
	server := &http.Server{
		Addr:         bindAddress,
		Handler:      LoggingMiddleware(withEtag(http.FileServer(http.Dir(".")))),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start server
	log.Printf("Listening on %s", bindAddress)

	err := server.ListenAndServeTLS("localhost.dev.pem", "localhost.dev-key.pem") // Certs generated using mkcert usually
	if err != nil {
		panic(err)
	}
}

func withEtag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Generate an Etag for the requested file
		switch r.Method {
		case http.MethodHead, http.MethodGet:
			etag, err := generateEtag(r.URL.Path)
			if err == nil {
				w.Header().Set("Etag", strconv.Quote(etag))
			}
		}

		next.ServeHTTP(w, r)
	})
}

// generateEtag reads the file at the given path and generates an Etag for it
// based on stuffs.
func generateEtag(path string) (string, error) {
	absPath, err := filepath.Abs("./" + path)
	if err != nil {
		return "", err
	}

	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}

	etagBytes := make([]byte, 0, binary.MaxVarintLen64*2)
	etagBytes = binary.AppendVarint(etagBytes, fileInfo.ModTime().UnixNano())
	etagBytes = binary.AppendVarint(etagBytes, fileInfo.Size())

	hash := sha1.Sum(etagBytes)

	return hex.EncodeToString(hash[:]), nil
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Request received: %+v", r)
		next.ServeHTTP(w, r)
	})
}
