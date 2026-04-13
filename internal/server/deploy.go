package server

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
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

// ── State persistence ─────────────────────────────────────────────────────────

// persistedApp is the JSON-serializable form of a deployedApp.
type persistedApp struct {
	Name        string    `json:"name"`
	ContainerID string    `json:"container_id"`
	ListenAddr  string    `json:"listen_addr"`
	Port        int       `json:"port"`
	Dir         string    `json:"dir"`
	StartedAt   time.Time `json:"started_at"`
}

func (s *Server) stateFile() string {
	dir := s.cfg.DeployDir
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "2nnel-state.json")
}

// saveState persists all current deployedApps to disk.
func (s *Server) saveState() {
	var apps []persistedApp
	s.deployedApps.Range(func(k, v any) bool {
		app := v.(*deployedApp)
		apps = append(apps, persistedApp{
			Name:        app.name,
			ContainerID: app.containerID,
			ListenAddr:  app.listenAddr,
			Port:        app.port,
			Dir:         app.dir,
			StartedAt:   app.startedAt,
		})
		return true
	})
	b, err := json.Marshal(apps)
	if err != nil {
		slog.Error("saveState: marshal", "err", err)
		return
	}
	if err := os.WriteFile(s.stateFile(), b, 0600); err != nil {
		slog.Error("saveState: write", "err", err)
	}
}

// loadState reads state.json on startup and re-populates deployedApps,
// skipping any containers that are no longer running in Docker.
func (s *Server) loadState() {
	data, err := os.ReadFile(s.stateFile())
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		slog.Warn("loadState: read", "err", err)
		return
	}
	var apps []persistedApp
	if err := json.Unmarshal(data, &apps); err != nil {
		slog.Warn("loadState: parse", "err", err)
		return
	}
	for _, pa := range apps {
		// Verify container is still running.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(ctx, "docker", "inspect",
			"--format", "{{.State.Status}}", "2nnel-"+pa.Name).Output()
		cancel()
		if err != nil || strings.TrimSpace(string(out)) != "running" {
			slog.Info("loadState: container gone, skipping", "name", pa.Name)
			continue
		}
		app := &deployedApp{
			name:        pa.Name,
			subdomain:   pa.Name,
			containerID: pa.ContainerID,
			listenAddr:  pa.ListenAddr,
			port:        pa.Port,
			dir:         pa.Dir,
			startedAt:   pa.StartedAt,
		}
		s.deployedApps.Store(pa.Name, app)
		slog.Info("loadState: restored deployed app", "name", pa.Name, "addr", pa.ListenAddr)
	}
}

// ── Promote upload ────────────────────────────────────────────────────────────

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
	if _, exists := s.pendingBuilds.Load(name); exists {
		http.Error(w, fmt.Sprintf("app %q is already building", name), http.StatusConflict)
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

	// Register pending build for dashboard visibility.
	build := &pendingBuild{name: name, startedAt: time.Now()}
	s.pendingBuilds.Store(name, build)

	// Snapshot existing ports before container starts (Fix 4).
	knownPorts, err := snapshotListeningPorts()
	if err != nil {
		s.pendingBuilds.Delete(name)
		os.RemoveAll(dir)
		http.Error(w, "ss failed — cannot detect app port: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build with Nixpacks — stream output line-by-line into build log.
	slog.Info("promote: nixpacks build", "name", name, "dir", dir)
	nixCmd := exec.Command("nixpacks", "build", dir, "--name", name)
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
	<-scanDone

	if buildErr != nil {
		lines, _, _ := build.snapshot()
		slog.Error("promote: nixpacks build failed", "name", name, "err", buildErr)
		build.finish("nixpacks build failed: " + buildErr.Error())
		os.RemoveAll(dir)
		http.Error(w, fmt.Sprintf("nixpacks build failed:\n%s", strings.Join(lines, "\n")), http.StatusInternalServerError)
		return
	}
	build.appendLine("✓ Build complete — starting container…")
	slog.Info("promote: nixpacks build complete", "name", name)

	// Force-remove any stale container with this name before starting a new one (Fix 5/8).
	ctx30 := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if out, err := exec.CommandContext(rmCtx, "docker", "rm", "-f", "2nnel-"+name).CombinedOutput(); err != nil {
		slog.Debug("promote: pre-run rm (expected if no stale container)", "name", name, "out", strings.TrimSpace(string(out)))
	}
	rmCancel()

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

	build.appendLine("Starting container…")
	slog.Info("promote: docker run", "name", name)
	runCtx, runCancel := ctx30()
	out, err := exec.CommandContext(runCtx, "docker", append([]string{}, args...)...).Output()
	runCancel()
	if err != nil {
		build.finish("docker run failed: " + err.Error())
		cleanupContainer(name, "")
		os.RemoveAll(dir)
		http.Error(w, fmt.Sprintf("docker run failed: %v", err), http.StatusInternalServerError)
		return
	}
	containerID := strings.TrimSpace(string(out))

	// Poll host ports for a new one, verify it accepts HTTP (Fix 6/7).
	listenAddr, err := detectContainerPort(knownPorts, 5*time.Minute)
	if err != nil {
		build.finish("app never started listening: " + err.Error())
		cleanupContainer(name, containerID)
		os.RemoveAll(dir)
		http.Error(w, "detect app port: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("promote: app listening", "name", name, "addr", listenAddr)

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
	s.deployedApps.Store(name, app)
	s.pendingBuilds.Delete(name)
	s.saveState() // Fix 1

	// Evict any client tunnel using this subdomain.
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

// cleanupContainer stops and removes the Docker container for a named app.
func cleanupContainer(name, containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "stop", "2nnel-"+name).CombinedOutput(); err != nil {
		slog.Warn("cleanup: docker stop failed", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if out, err := exec.CommandContext(ctx2, "docker", "rm", "2nnel-"+name).CombinedOutput(); err != nil {
		slog.Warn("cleanup: docker rm failed", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
}

// stopApp stops and removes a deployed app's container, image, and files.
func (s *Server) stopApp(name string) bool {
	val, ok := s.deployedApps.Load(name)
	if !ok {
		return false
	}
	app := val.(*deployedApp)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if out, err := exec.CommandContext(ctx, "docker", "stop", "2nnel-"+app.name).CombinedOutput(); err != nil {
		slog.Warn("stopApp: docker stop", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
	cancel()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	if out, err := exec.CommandContext(ctx2, "docker", "rm", "2nnel-"+app.name).CombinedOutput(); err != nil {
		slog.Warn("stopApp: docker rm", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
	cancel2()

	ctx3, cancel3 := context.WithTimeout(context.Background(), 15*time.Second)
	if out, err := exec.CommandContext(ctx3, "docker", "rmi", app.name).CombinedOutput(); err != nil {
		slog.Warn("stopApp: docker rmi", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
	cancel3()

	if err := os.RemoveAll(app.dir); err != nil {
		slog.Warn("stopApp: remove dir", "name", name, "dir", app.dir, "err", err)
	}
	s.deployedApps.Delete(name)
	s.saveState() // Fix 1
	slog.Info("deployed app stopped", "name", name)
	return true
}

// stopAllDeployedApps stops all running containers on server shutdown.
func (s *Server) stopAllDeployedApps() {
	s.deployedApps.Range(func(k, v any) bool {
		app := v.(*deployedApp)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		exec.CommandContext(ctx, "docker", "stop", "2nnel-"+app.name).Run()
		cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "50", "2nnel-"+app.name).CombinedOutput()
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

// ── Port detection ────────────────────────────────────────────────────────────

// knownDebugPorts lists ports that apps commonly open for debugging/monitoring
// before their main HTTP server — skip these during port detection (Fix 7).
var knownDebugPorts = map[int]bool{
	9229: true, // Node.js --inspect
	9230: true, // Node.js --inspect (alternate)
	9090: true, // Prometheus metrics
	8125: true, // StatsD
}

// snapshotListeningPorts returns the set of TCP ports currently listening on
// the host, or an error if ss is unavailable (Fix 4).
func snapshotListeningPorts() (map[int]bool, error) {
	out, err := exec.Command("ss", "-Htln").Output()
	if err != nil {
		return nil, fmt.Errorf("ss: %w", err)
	}
	ports := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if p := parseSSPort(line); p > 0 {
			ports[p] = true
		}
	}
	return ports, nil
}

// detectContainerPort polls the host's listening ports until a new one appears
// that wasn't in knownPorts and accepts TCP connections (Fix 6 — 5 min timeout).
// Skips known debug/metrics ports (Fix 7).
func detectContainerPort(knownPorts map[int]bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("ss", "-Htln").Output()
		for _, line := range strings.Split(string(out), "\n") {
			p := parseSSPort(line)
			if p <= 1024 || knownPorts[p] || knownDebugPorts[p] {
				continue
			}
			// Probe both IPv4 and IPv6 — return whichever accepts connections.
			for _, host := range []string{"127.0.0.1", "::1"} {
				addr := net.JoinHostPort(host, strconv.Itoa(p))
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
// Handles IPv4 (0.0.0.0:port), IPv6 (:::port, [::1]:port), and wildcard (*:port).
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

// ── Tarball extraction ────────────────────────────────────────────────────────

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
		default:
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

// ── Misc ──────────────────────────────────────────────────────────────────────

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
