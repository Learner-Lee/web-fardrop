package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	base                  = "/about/fardrop/"
	uploadChunkSize       = 1 << 20
	legacyUploadChunkSize = 16 << 20
	cacheChunkSize        = 4 << 20
	maxSize               = 5 << 30
	edgeHeader            = "X-Fardrop-Edge"
)

type File struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	CreatedAt   int64  `json:"createdAt"`
	DeleteAfter int64  `json:"deleteAfter,omitempty"`
}

type Share struct {
	ID        string `json:"id"`
	FileID    string `json:"fileId"`
	ExpiresAt int64  `json:"expiresAt"`
	RevokedAt int64  `json:"revokedAt"`
	Token     string `json:"token,omitempty"`
}

type Grant struct {
	File      File  `json:"file"`
	ExpiresAt int64 `json:"expiresAt"`
}

func randomID(n int) string {
	p := make([]byte, n)
	if _, err := rand.Read(p); err != nil {
		panic(err)
	}
	return hex.EncodeToString(p)
}

func hashString(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func validID(value string, size int) bool {
	if len(value) != size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func secretsMatch(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}

func fail(w http.ResponseWriter, err error) {
	log.Printf("operation failed: %v", err)
	problem(w, http.StatusInternalServerError, "服务器处理失败，请稍后再试。")
}

func parse(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil || decoder.Decode(new(any)) != io.EOF {
		problem(w, http.StatusBadRequest, "请求格式有误。")
		return false
	}
	return true
}

func partLength(u upload, index int) int64 {
	return min(int64(u.ChunkSize), u.Size-int64(index)*int64(u.ChunkSize))
}

func requireEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func requireSecret(name string) string {
	value := requireEnv(name)
	if !validID(value, 32) {
		log.Fatalf("%s must be a 32-byte hex token", name)
	}
	return value
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: fardrop edge|origin")
	}
	mode := os.Args[1]
	addr := requireEnv("FARDROP_LISTEN")
	secret := requireSecret("FARDROP_EDGE_TOKEN")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var handler http.Handler
	var shutdownOrigin func()
	var tunnelURL string
	switch mode {
	case "origin":
		admin := requireSecret("FARDROP_ADMIN_TOKEN")
		peer := requireEnv("FARDROP_EDGE_IP")
		if net.ParseIP(peer) == nil {
			log.Fatal("FARDROP_EDGE_IP must be an IP address")
		}
		origin, err := openOrigin(requireEnv("FARDROP_DATA"))
		if err != nil {
			log.Fatal(err)
		}
		handler = origin.routes(secret, admin, peer)
		tunnelURL = os.Getenv("FARDROP_TUNNEL_URL")
		if tunnelURL != "" {
			destination, err := url.Parse(tunnelURL)
			if err != nil || destination.Scheme != "wss" || destination.Host == "" || destination.Path != tunnelPath || destination.User != nil || destination.RawQuery != "" || destination.Fragment != "" {
				log.Fatal("FARDROP_TUNNEL_URL must use the FarDrop WSS route")
			}
			if addr != "127.0.0.1:18172" || peer != "127.0.0.1" {
				log.Fatal("Origin must listen on loopback when using the WSS tunnel")
			}
		}
		maintenanceDone := make(chan struct{})
		go func() {
			defer close(maintenanceDone)
			origin.maintainFiles(ctx)
		}()
		shutdownOrigin = func() {
			<-maintenanceDone
			_ = origin.db.Close()
		}
	case "edge":
		upstream, err := url.Parse(requireEnv("FARDROP_ORIGIN"))
		if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Path != "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
			log.Fatal("FARDROP_ORIGIN must be an HTTP(S) origin")
		}
		cacheMiB, err := strconv.Atoi(os.Getenv("FARDROP_CACHE_MIB"))
		if err != nil || cacheMiB < 16 || cacheMiB > 512 {
			log.Fatal("FARDROP_CACHE_MIB must be between 16 and 512")
		}
		handler = newEdge(upstream, secret, cacheMiB<<20).routes()
	default:
		log.Fatal("usage: fardrop edge|origin")
	}

	server := &http.Server{
		Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	if tunnelURL != "" {
		go keepOriginTunnel(ctx, tunnelURL, addr, secret)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		timeout, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = server.Shutdown(timeout)
	}()
	log.Printf("FarDrop %s listening on %s", mode, addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("listen: %w", err))
	}
	<-stopped
	if shutdownOrigin != nil {
		shutdownOrigin()
	}
}
