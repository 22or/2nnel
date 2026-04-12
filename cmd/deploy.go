package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var deployCmd = &cobra.Command{
	Use:   "deploy <binary>",
	Short: "Deploy a binary to the server and run it permanently under a subdomain",
	Args:  cobra.ExactArgs(1),
	RunE:  runDeploy,
}

func init() {
	deployCmd.Flags().String("server", "", "Server URL (https:// or wss:// accepted)")
	deployCmd.Flags().String("auth-token", "", "Auth token")
	deployCmd.Flags().String("name", "", "App name / subdomain (required)")
	deployCmd.Flags().StringArray("env", nil, "Extra env vars for the app (KEY=VALUE, repeatable)")
	_ = deployCmd.MarkFlagRequired("server")
	_ = deployCmd.MarkFlagRequired("name")
}

type deployResponse struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

func runDeploy(cmd *cobra.Command, args []string) error {
	binaryPath := args[0]
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("binary not found: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory, not a binary", binaryPath)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("%q is not executable", binaryPath)
	}

	f := cmd.Flags()
	serverURL, _ := f.GetString("server")
	authToken, _ := f.GetString("auth-token")
	name, _ := f.GetString("name")
	envVars, _ := f.GetStringArray("env")

	// Convert ws(s):// → http(s):// for the upload endpoint.
	baseURL := toHTTPURL(serverURL)

	// Build multipart body.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	for _, e := range envVars {
		_ = mw.WriteField("env", e)
	}
	fw, err := mw.CreateFormFile("binary", filepath.Base(binaryPath))
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	fh, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open binary: %w", err)
	}
	defer fh.Close()
	if _, err := io.Copy(fw, fh); err != nil {
		return fmt.Errorf("read binary: %w", err)
	}
	mw.Close()

	endpoint := strings.TrimRight(baseURL, "/") + "/_2nnel/deploy"
	req, err := http.NewRequest(http.MethodPost, endpoint, &buf)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	fmt.Printf("Uploading %q to %s …\n", filepath.Base(binaryPath), baseURL)

	httpClient := &http.Client{Timeout: 10 * time.Minute}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result deployResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Printf("Deployed: %s\n", result.URL)
	return nil
}

// toHTTPURL converts wss:// → https://, ws:// → http://, passes https/http through.
func toHTTPURL(s string) string {
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
