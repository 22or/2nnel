package client

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/22or/2nnel/internal/proto"
)

// handlePromote is called when the server requests a promote upload for a tunnel.
func (s *session) handlePromote(p proto.Promote) {
	s.cfgMu.Lock()
	var dir string
	for _, t := range s.cfg.Tunnels {
		if t.Name == p.TunnelName {
			dir = t.Dir
			break
		}
	}
	authToken := s.cfg.AuthToken
	serverURL := s.cfg.Server
	s.cfgMu.Unlock()

	if dir == "" {
		slog.Error("promote: no dir configured for tunnel — start client with --dir", "tunnel", p.TunnelName)
		_ = s.ctrl.Send(proto.TypePromoteError, proto.PromoteError{
			TunnelName: p.TunnelName,
			Error:      "no project directory configured — start client with --dir",
		})
		return
	}

	dir = expandHome(dir)
	slog.Info("promote: creating tarball", "tunnel", p.TunnelName, "dir", dir)
	tarball, err := createTarball(dir)
	if err != nil {
		slog.Error("promote: create tarball failed", "tunnel", p.TunnelName, "err", err)
		_ = s.ctrl.Send(proto.TypePromoteError, proto.PromoteError{
			TunnelName: p.TunnelName,
			Error:      "tarball failed: " + err.Error(),
		})
		return
	}

	baseURL := wsToHTTP(serverURL)
	endpoint := strings.TrimRight(baseURL, "/") + "/_2nnel/promote"

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", p.TunnelName)
	fw, err := mw.CreateFormFile("tarball", p.TunnelName+".tar.gz")
	if err != nil {
		slog.Error("promote: create form file", "err", err)
		return
	}
	if _, err := io.Copy(fw, tarball); err != nil {
		slog.Error("promote: copy tarball", "err", err)
		return
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, endpoint, &buf)
	if err != nil {
		slog.Error("promote: build request", "err", err)
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	slog.Info("promote: uploading tarball", "tunnel", p.TunnelName, "url", endpoint)
	httpClient := &http.Client{Timeout: 10 * time.Minute}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("promote: upload failed", "tunnel", p.TunnelName, "err", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		slog.Error("promote: server error", "tunnel", p.TunnelName, "status", resp.StatusCode, "body", string(body))
		return
	}
	slog.Info("promote: deployed successfully", "tunnel", p.TunnelName, "response", string(body))
}

// createTarball packages dir into a gzipped tar archive.
// Skips common non-essential directories and respects .gitignore (but always includes .env).
func createTarball(dir string) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	ignorePatterns := parseGitignore(filepath.Join(dir, ".gitignore"))

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "__pycache__": true,
		".venv": true, "venv": true, "dist": true, "build": true,
		".next": true, ".nuxt": true, "coverage": true, ".turbo": true,
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		base := filepath.Base(path)

		// Always skip these files.
		if base == ".DS_Store" || strings.HasSuffix(base, ".pyc") {
			return nil
		}

		// Skip known heavy/generated directories.
		if d.IsDir() && skipDirs[base] {
			return filepath.SkipDir
		}

		// Apply gitignore only to directories — individual source files are always
		// included regardless of gitignore (gitignore is "don't commit", not "don't build";
		// many frameworks require gitignored files like environment configs for builds).
		if d.IsDir() && isGitignored(rel, base, ignorePatterns) {
			return filepath.SkipDir
		}

		// Directories themselves don't need tar entries — files implicitly create them.
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("tar header %s: %w", path, err)
		}
		hdr.Name = rel

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", rel, err)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})

	if err != nil {
		return nil, err
	}
	tw.Close()
	gw.Close()
	return buf, nil
}

// parseGitignore reads .gitignore lines into a pattern slice (comments and blanks excluded).
func parseGitignore(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// isGitignored returns true if rel (or its base) matches any gitignore pattern.
func isGitignored(rel, base string, patterns []string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, base); matched {
			return true
		}
		if matched, _ := filepath.Match(p, rel); matched {
			return true
		}
	}
	return false
}

// expandHome replaces a leading ~ with the current user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// wsToHTTP converts wss:// → https://, ws:// → http://, leaves https/http unchanged.
func wsToHTTP(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	return u.String()
}
