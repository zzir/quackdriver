package localserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Server represents a running local DuckDB Quack server process.
type Server struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	port     int
	token    string
	dbPath   string // canonical absolute path, or "" for in-memory
	refCount int
	done     chan struct{}
	err      error
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int { return s.port }

// Token returns the authentication token for this server.
func (s *Server) Token() string { return s.token }

var (
	registryMu sync.Mutex
	registry   = map[string]*Server{}
)

// Acquire returns a running Server for the given dbPath. If a server for
// this path already exists, its reference count is incremented and the
// existing server is returned. Otherwise a new DuckDB CLI process is started.
//
// If requestedToken is "auto" or empty, a cryptographically random token
// is generated. The timeout controls how long to wait for the server to
// become ready.
func Acquire(dbPath string, requestedToken string, timeout time.Duration) (*Server, error) {
	canonical, err := canonicalize(dbPath)
	if err != nil {
		return nil, err
	}

	registryMu.Lock()
	if srv, ok := registry[canonical]; ok {
		srv.mu.Lock()
		srv.refCount++
		srv.mu.Unlock()
		registryMu.Unlock()
		return srv, nil
	}
	registryMu.Unlock()

	binary, err := FindBinary()
	if err != nil {
		return nil, err
	}
	if _, _, _, err := CheckVersion(binary); err != nil {
		return nil, err
	}

	port, err := pickFreePort()
	if err != nil {
		return nil, err
	}

	token := requestedToken
	if token == "" || token == "auto" {
		token = generateToken()
	}

	srv, err := startProcess(binary, canonical, port, token)
	if err != nil {
		return nil, err
	}

	if err := waitForReady(port, timeout); err != nil {
		srv.kill()
		return nil, err
	}

	registryMu.Lock()
	// Double-check: another goroutine may have registered while we were starting.
	if existing, ok := registry[canonical]; ok {
		registryMu.Unlock()
		srv.kill()
		existing.mu.Lock()
		existing.refCount++
		existing.mu.Unlock()
		return existing, nil
	}
	registry[canonical] = srv
	registryMu.Unlock()

	return srv, nil
}

// Release decrements the reference count. When it reaches zero the DuckDB
// process is stopped and the server is removed from the registry.
func (s *Server) Release() error {
	s.mu.Lock()
	s.refCount--
	if s.refCount > 0 {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	registryMu.Lock()
	delete(registry, s.dbPath)
	registryMu.Unlock()

	return s.shutdown()
}

// ReleaseAll stops all managed local servers. Intended for test cleanup
// and application shutdown.
func ReleaseAll() {
	registryMu.Lock()
	servers := make([]*Server, 0, len(registry))
	for _, s := range registry {
		servers = append(servers, s)
	}
	registry = map[string]*Server{}
	registryMu.Unlock()

	for _, s := range servers {
		s.shutdown()
	}
}

func startProcess(binary, dbPath string, port int, token string) (*Server, error) {
	startupSQL := fmt.Sprintf(
		"INSTALL quack; LOAD quack; CALL quack_serve('quack:127.0.0.1:%d', token=>'%s');",
		port, token,
	)

	args := []string{}
	if dbPath != "" && dbPath != ":memory:" {
		args = append(args, dbPath)
	}
	args = append(args, "-init", os.DevNull, "-cmd", startupSQL)

	cmd := exec.Command(binary, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("localserver: stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("localserver: start duckdb: %w", err)
	}

	srv := &Server{
		cmd:      cmd,
		stdin:    stdin,
		port:     port,
		token:    token,
		dbPath:   dbPath,
		refCount: 1,
		done:     make(chan struct{}),
	}

	go func() {
		srv.err = cmd.Wait()
		close(srv.done)
	}()

	return srv, nil
}

func (s *Server) shutdown() error {
	if s.stdin != nil {
		s.stdin.Close()
	}

	select {
	case <-s.done:
		return nil
	case <-time.After(5 * time.Second):
	}

	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	<-s.done
	return nil
}

func (s *Server) kill() {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	<-s.done
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("localserver: pick free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("localserver: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func waitForReady(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("localserver: server on port %d did not become ready within %v", port, timeout)
}

func canonicalize(dbPath string) (string, error) {
	if dbPath == "" || dbPath == ":memory:" {
		return dbPath, nil
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("localserver: resolve path %q: %w", dbPath, err)
	}
	return abs, nil
}
