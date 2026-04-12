package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// deployedApp represents a binary deployed to and running on the server.
type deployedApp struct {
	name       string
	subdomain  string
	binaryPath string
	dir        string
	env        []string

	mu        sync.Mutex
	cmd       *exec.Cmd
	port      int
	restarts  int
	lastStart time.Time
	stopped   bool

	requests    atomic.Int64
	activeConns atomic.Int32

	logMu  sync.Mutex
	logBuf []string // ring buffer, cap 200
}

// handleDeploy handles POST /_2nnel/deploy — receives a binary, saves it, and runs it.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if !isValidDeployName(name) {
		http.Error(w, "name must be alphanumeric+hyphen, max 40 chars", http.StatusBadRequest)
		return
	}

	if s.registry.isSubdomainTaken(name) {
		http.Error(w, fmt.Sprintf("subdomain %q already in use by a tunnel client", name), http.StatusConflict)
		return
	}
	if _, exists := s.deployedApps.Load(name); exists {
		http.Error(w, fmt.Sprintf("app %q already deployed — stop it first", name), http.StatusConflict)
		return
	}

	file, _, err := r.FormFile("binary")
	if err != nil {
		http.Error(w, "binary file required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	baseDir := s.cfg.DeployDir
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	dir, err := os.MkdirTemp(baseDir, "2nnel-deploy-*")
	if err != nil {
		http.Error(w, "create temp dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	binaryPath := filepath.Join(dir, name)
	f, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "create binary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(f, file); err != nil {
		f.Close()
		os.RemoveAll(dir)
		http.Error(w, "write binary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	f.Close()

	var envVars []string
	if r.MultipartForm != nil {
		envVars = r.MultipartForm.Value["env"]
	}

	app := &deployedApp{
		name:       name,
		subdomain:  name,
		binaryPath: binaryPath,
		dir:        dir,
		env:        envVars,
	}

	if err := s.startApp(app); err != nil {
		os.RemoveAll(dir)
		http.Error(w, "start app: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.deployedApps.Store(name, app)

	publicURL := s.buildPublicHTTPURL(name)
	slog.Info("app deployed", "name", name, "url", publicURL)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"name":%q,"url":%q}`, name, publicURL)
}

// startApp launches the binary, setting up log capture and supervision.
func (s *Server) startApp(app *deployedApp) error {
	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("pick port: %w", err)
	}

	cmd := exec.Command(app.binaryPath)
	cmd.Dir = app.dir
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
	cmd.Env = append(cmd.Env, app.env...)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("exec: %w", err)
	}

	app.mu.Lock()
	app.cmd = cmd
	app.port = port
	app.lastStart = time.Now()
	app.mu.Unlock()

	go app.readLogs(pr)
	go s.supervise(app, cmd, pw)

	slog.Info("deployed app started", "name", app.name, "port", port, "pid", cmd.Process.Pid)
	return nil
}

// supervise watches the process and restarts it with exponential backoff on exit.
func (s *Server) supervise(app *deployedApp, cmd *exec.Cmd, pw *io.PipeWriter) {
	err := cmd.Wait()
	pw.Close()

	app.mu.Lock()
	stopped := app.stopped
	restarts := app.restarts
	app.mu.Unlock()

	if stopped {
		return
	}

	slog.Warn("deployed app exited, restarting", "name", app.name, "err", err, "restarts", restarts)

	delay := time.Duration(math.Min(
		float64(time.Second)*math.Pow(2, float64(min(restarts, 6))),
		float64(60*time.Second),
	))
	time.Sleep(delay)

	app.mu.Lock()
	if app.stopped {
		app.mu.Unlock()
		return
	}
	app.restarts++
	app.mu.Unlock()

	if err := s.startApp(app); err != nil {
		slog.Error("restart failed", "name", app.name, "err", err)
	}
}

// stopApp kills a deployed app and removes its files.
func (s *Server) stopApp(name string) bool {
	val, ok := s.deployedApps.Load(name)
	if !ok {
		return false
	}
	app := val.(*deployedApp)

	app.mu.Lock()
	app.stopped = true
	cmd := app.cmd
	dir := app.dir
	app.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		_ = cmd.Process.Kill()
	}
	os.RemoveAll(dir)
	s.deployedApps.Delete(name)
	slog.Info("deployed app stopped", "name", name)
	return true
}

// stopAllDeployedApps signals all deployed apps on server shutdown.
func (s *Server) stopAllDeployedApps() {
	s.deployedApps.Range(func(k, v any) bool {
		app := v.(*deployedApp)
		app.mu.Lock()
		app.stopped = true
		cmd := app.cmd
		app.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Process.Kill()
		}
		return true
	})
}

// serveDeployedApp reverse-proxies an HTTP request to a deployed app's local port.
func (s *Server) serveDeployedApp(w http.ResponseWriter, r *http.Request, app *deployedApp) {
	app.mu.Lock()
	port := app.port
	app.mu.Unlock()

	if port == 0 {
		http.Error(w, "app not ready", http.StatusServiceUnavailable)
		return
	}

	app.requests.Add(1)
	app.activeConns.Add(1)
	defer app.activeConns.Add(-1)

	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("127.0.0.1:%d", port),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("deployed app proxy error", "name", app.name, "err", err)
		http.Error(w, "app unavailable: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// handleDeleteDeploy handles DELETE /_2nnel/deploy/<name>.
func (s *Server) handleDeleteDeploy(w http.ResponseWriter, r *http.Request, name string) {
	if !s.stopApp(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleDeployLogs handles GET /_2nnel/deploy/<name>/logs — returns last 200 log lines.
func (s *Server) handleDeployLogs(w http.ResponseWriter, r *http.Request, name string) {
	val, ok := s.deployedApps.Load(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	app := val.(*deployedApp)
	app.logMu.Lock()
	logs := make([]string, len(app.logBuf))
	copy(logs, app.logBuf)
	app.logMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(logs)
	_, _ = w.Write(b)
}

// readLogs feeds process stdout/stderr into the ring buffer.
func (app *deployedApp) readLogs(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		app.logMu.Lock()
		if len(app.logBuf) >= 200 {
			app.logBuf = app.logBuf[1:]
		}
		app.logBuf = append(app.logBuf, line)
		app.logMu.Unlock()
	}
}

// pickFreePort finds a free localhost TCP port by binding to :0.
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// isValidDeployName returns true if name is alphanumeric+hyphen and ≤40 chars.
func isValidDeployName(name string) bool {
	if len(name) == 0 || len(name) > 40 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}
