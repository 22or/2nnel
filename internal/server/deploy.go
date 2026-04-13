package server

import (
	"archive/tar"
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/22or/2nnel/internal/proto"
)

// deployedApp represents a project promoted to and running on the server via Docker.
type deployedApp struct {
	name        string
	subdomain   string
	containerID string
	listenAddr  string // host:port the app is actually accepting connections on
	port        int    // numeric port (for display)
	dir         string // extracted project dir on server
	startedAt   time.Time

	requests    atomic.Int64
	activeConns atomic.Int32
}

// pendingBuild tracks a nixpacks build in progress so the dashboard can show live logs.
type pendingBuild struct {
	name      string
	startedAt time.Time
	mu        sync.Mutex
	lines     []string
	errMsg    string // non-empty on failure
	done      bool
}

func (b *pendingBuild) appendLine(line string) {
	b.mu.Lock()
	b.lines = append(b.lines, line)
	b.mu.Unlock()
}

func (b *pendingBuild) finish(errMsg string) {
	b.mu.Lock()
	b.errMsg = errMsg
	b.done = true
	b.mu.Unlock()
}

// snapshot returns the last 50 lines plus current status.
func (b *pendingBuild) snapshot() (lines []string, errMsg string, done bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	src := b.lines
	if len(src) > 50 {
		src = src[len(src)-50:]
	}
	lines = make([]string, len(src))
	copy(lines, src)
	return lines, b.errMsg, b.done
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

	// Register a pending build so the dashboard can show live progress.
	build := &pendingBuild{name: name, startedAt: time.Now()}
	s.pendingBuilds.Store(name, build)

	// Build with Nixpacks — stream output line-by-line into build log.
	slog.Info("promote: nixpacks build", "name", name, "dir", dir)
	nixCmd := exec.Command("nixpacks", "build", dir, "--name", name)
	nixCmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	pr, pw := io.Pipe()
	nixCmd.Stdout = pw
	nixCmd.Stderr = pw
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			build.appendLine(sc.Text())
		}
	}()
	buildErr := nixCmd.Run()
	pw.Close()
	<-scanDone // ensure all output is captured before we inspect it

	if buildErr != nil {
		lines, _, _ := build.snapshot()
		buildLog := strings.Join(lines, "\n")
		slog.Error("promote: nixpacks build failed", "name", name, "err", buildErr)
		build.finish("nixpacks build failed: " + buildErr.Error())
		os.RemoveAll(dir)
		http.Error(w, fmt.Sprintf("nixpacks build failed:\n%s", buildLog), http.StatusInternalServerError)
		return
	}
	build.appendLine("✓ Build complete — starting container…")
	slog.Info("promote: nixpacks build complete", "name", name)

	// Build docker run args. Use --network host so the container shares the host's
	// network namespace — no port mapping needed, and we can detect the actual
	// listening port at runtime regardless of what the app uses.
	args := []string{
		"run", "-d", "--restart=unless-stopped",
		"--network", "host",
		"--name", "2nnel-" + name,
	}
	envFile := filepath.Join(dir, ".env")
	if _, statErr := os.Stat(envFile); statErr == nil {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, name)

	// Snapshot ports already listening on the host BEFORE the container starts.
	// With --network host the container shares host network, so we detect the app's
	// port by looking for a new port that wasn't there before docker run.
	knownPorts := snapshotListeningPorts()

	build.appendLine("Starting container…")
	slog.Info("promote: docker run", "name", name)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		build.finish("docker run failed: " + err.Error())
		os.RemoveAll(dir)
		http.Error(w, fmt.Sprintf("docker run failed: %v", err), http.StatusInternalServerError)
		return
	}
	containerID := strings.TrimSpace(string(out))

	// Poll host ports until a new one appears, then probe IPv4/IPv6 to find
	// the actual connectable address (app may bind to ::1 not 127.0.0.1).
	listenAddr, err := detectContainerPort(knownPorts, 60*time.Second)
	if err != nil {
		build.finish("container started but app never listened: " + err.Error())
		exec.Command("docker", "stop", containerID).Run()
		exec.Command("docker", "rm", containerID).Run()
		os.RemoveAll(dir)
		http.Error(w, "detect app port: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("promote: app listening", "name", name, "addr", listenAddr)

	// Parse port number from listenAddr for display.
	displayPort := 0
	if _, portStr, splitErr := net.SplitHostPort(listenAddr); splitErr == nil {
		displayPort, _ = strconv.Atoi(portStr)
	}

	app := &deployedApp{
		name:        name,
		subdomain:   name,
		containerID: containerID,
		listenAddr:  listenAddr,
		port:        displayPort,
		dir:         dir,
		startedAt:   time.Now(),
	}
	// Store deployed app so traffic routes to Docker immediately,
	// then remove the pending build — it's now visible as a deployed app.
	s.deployedApps.Store(name, app)
	s.pendingBuilds.Delete(name)

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

// serveDeployedApp reverse-proxies an HTTP request to a deployed app's local address.
func (s *Server) serveDeployedApp(w http.ResponseWriter, r *http.Request, app *deployedApp) {
	if app.listenAddr == "" {
		http.Error(w, "app not ready", http.StatusServiceUnavailable)
		return
	}

	app.requests.Add(1)
	app.activeConns.Add(1)
	defer app.activeConns.Add(-1)

	target := &url.URL{
		Scheme: "http",
		Host:   app.listenAddr,
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

// snapshotListeningPorts returns the set of TCP ports currently listening on
// the host. Used to establish a baseline before docker run so we can detect
// the app's port by diffing before/after (--network host shares host netns).
func snapshotListeningPorts() map[int]bool {
	ports := map[int]bool{}
	out, err := exec.Command("ss", "-Htln").Output()
	if err != nil {
		return ports
	}
	for _, line := range strings.Split(string(out), "\n") {
		if p := parseSSPort(line); p > 1024 {
			ports[p] = true
		}
	}
	return ports
}

// detectContainerPort polls the host's listening ports until a new one appears
// that wasn't in knownPorts, then probes both 127.0.0.1 and [::1] to find the
// address that actually accepts connections (app may bind IPv6-only or IPv4-only).
// Returns a host:port string ready for use as a reverse-proxy target.
func detectContainerPort(knownPorts map[int]bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("ss", "-Htln").Output()
		for _, line := range strings.Split(string(out), "\n") {
			p := parseSSPort(line)
			if p <= 1024 || knownPorts[p] {
				continue
			}
			// Found a new port — determine connectable address.
			for _, host := range []string{"127.0.0.1", "[::1]"} {
				addr := net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(p))
				conn, err := net.DialTimeout("tcp", addr, time.Second)
				if err == nil {
					conn.Close()
					return addr, nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("app did not start listening within %s", timeout)
}

// parseSSPort extracts the port number from one line of `ss -Htln` output.
// Local address field format: "0.0.0.0:4200", ":::3000", "*:8080".
func parseSSPort(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return 0
	}
	addr := fields[3]
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0
	}
	return p
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
