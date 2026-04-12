package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/22or/2nnel/internal/proto"
)

// deployedApp represents a project promoted to and running on the server via Docker.
type deployedApp struct {
	name        string
	subdomain   string
	containerID string
	port        int
	dir         string // extracted project dir on server
	startedAt   time.Time

	requests    atomic.Int64
	activeConns atomic.Int32
}

// handlePromoteUpload handles POST /_2nnel/promote — receives a tarball, builds with
// Nixpacks, and runs the resulting Docker image permanently.
func (s *Server) handlePromoteUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(500 << 20); err != nil {
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

	if _, exists := s.deployedApps.Load(name); exists {
		http.Error(w, fmt.Sprintf("app %q already deployed — stop it first", name), http.StatusConflict)
		return
	}

	tarFile, _, err := r.FormFile("tarball")
	if err != nil {
		http.Error(w, "tarball file required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer tarFile.Close()

	baseDir := s.cfg.DeployDir
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	dir, err := os.MkdirTemp(baseDir, "2nnel-promote-*")
	if err != nil {
		http.Error(w, "create temp dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := extractTarball(tarFile, dir); err != nil {
		os.RemoveAll(dir)
		http.Error(w, "extract tarball: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build with Nixpacks. Disable BuildKit to avoid requiring docker-buildx.
	slog.Info("promote: nixpacks build", "name", name, "dir", dir)
	nixCmd := exec.Command("nixpacks", "build", dir, "--name", name)
	nixCmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	buildOut, err := nixCmd.CombinedOutput()
	if err != nil {
		slog.Error("promote: nixpacks build failed", "name", name, "err", err, "output", string(buildOut))
		os.RemoveAll(dir)
		http.Error(w, fmt.Sprintf("nixpacks build failed:\n%s", buildOut), http.StatusInternalServerError)
		return
	}
	slog.Info("promote: nixpacks build complete", "name", name)

	port, err := pickFreePort()
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "pick port: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build docker run args.
	args := []string{
		"run", "-d", "--restart=unless-stopped",
		"--name", "2nnel-" + name,
		"-e", fmt.Sprintf("PORT=%d", port),
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
	}
	envFile := filepath.Join(dir, ".env")
	if _, statErr := os.Stat(envFile); statErr == nil {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, name)

	slog.Info("promote: docker run", "name", name, "port", port)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, fmt.Sprintf("docker run failed: %v", err), http.StatusInternalServerError)
		return
	}
	containerID := strings.TrimSpace(string(out))

	app := &deployedApp{
		name:        name,
		subdomain:   name,
		containerID: containerID,
		port:        port,
		dir:         dir,
		startedAt:   time.Now(),
	}
	// Store deployed app first so traffic routes to Docker immediately.
	s.deployedApps.Store(name, app)

	// Evict the client tunnel using this subdomain (if any) — it handed off to Docker.
	s.registry.mu.RLock()
	evictEntry := s.registry.bySubdomain[name]
	s.registry.mu.RUnlock()
	if evictEntry != nil {
		_ = evictEntry.sendMsg(proto.TypeRemoveTunnel, proto.RemoveTunnel{Name: name})
		s.registry.removeTunnelByName(evictEntry.id, name)
		slog.Info("tunnel evicted after promote", "name", name, "client", evictEntry.id)
	}

	publicURL := s.buildPublicHTTPURL(name)
	slog.Info("app promoted", "name", name, "url", publicURL, "container", containerID)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"url":%q}`, publicURL)
}

// stopApp stops and removes a deployed app's container, image, and files.
func (s *Server) stopApp(name string) bool {
	val, ok := s.deployedApps.Load(name)
	if !ok {
		return false
	}
	app := val.(*deployedApp)

	exec.Command("docker", "stop", "2nnel-"+app.name).Run()
	exec.Command("docker", "rm", "2nnel-"+app.name).Run()
	exec.Command("docker", "rmi", app.name).Run()
	os.RemoveAll(app.dir)
	s.deployedApps.Delete(name)
	slog.Info("deployed app stopped", "name", name)
	return true
}

// stopAllDeployedApps stops all running containers on server shutdown.
func (s *Server) stopAllDeployedApps() {
	s.deployedApps.Range(func(k, v any) bool {
		app := v.(*deployedApp)
		exec.Command("docker", "stop", "2nnel-"+app.name).Run()
		return true
	})
}

// handleDeleteDeploy handles DELETE /_2nnel/promote/<name>.
func (s *Server) handleDeleteDeploy(w http.ResponseWriter, r *http.Request, name string) {
	if !s.stopApp(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleDeployLogs handles GET /_2nnel/promote/<name>/logs — returns last 50 log lines.
func (s *Server) handleDeployLogs(w http.ResponseWriter, r *http.Request, name string) {
	val, ok := s.deployedApps.Load(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	app := val.(*deployedApp)
	out, _ := exec.Command("docker", "logs", "--tail", "50", "2nnel-"+app.name).CombinedOutput()
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(lines)
	_, _ = w.Write(b)
}

// serveDeployedApp reverse-proxies an HTTP request to a deployed app's local port.
func (s *Server) serveDeployedApp(w http.ResponseWriter, r *http.Request, app *deployedApp) {
	if app.port == 0 {
		http.Error(w, "app not ready", http.StatusServiceUnavailable)
		return
	}

	app.requests.Add(1)
	app.activeConns.Add(1)
	defer app.activeConns.Add(-1)

	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("127.0.0.1:%d", app.port),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("deployed app proxy error", "name", app.name, "err", err)
		http.Error(w, "app unavailable: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// extractTarball extracts a gzipped tar archive into dir.
func extractTarball(r io.Reader, dir string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	cleanDir := filepath.Clean(dir) + string(os.PathSeparator)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		// Sanitize path — prevent traversal.
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			continue
		}

		target := filepath.Join(dir, clean)
		if !strings.HasPrefix(target, cleanDir) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		default: // treat everything else as a regular file
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent: %w", err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			f.Close()
		}
	}
	return nil
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
