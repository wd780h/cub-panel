// Self-update: the panel can tell an agent to replace its own binary with a
// published release build, so a fleet does not have to be upgraded by hand.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"cubpanel/internal/shared"
)

// defaultRepo is where release builds are published. A request may name a
// different fork, but only in owner/name form — never a bare URL.
const defaultRepo = "wd780h/cub-panel"

var (
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	versionRe = regexp.MustCompile(`^(latest|v[0-9]+\.[0-9]+\.[0-9]+)$`)
)

// assetName is the release asset matching this machine.
func assetName() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "cub-agent", nil
	case "arm64":
		return "cub-agent-arm64", nil
	}
	return "", fmt.Errorf("no release build for %s", runtime.GOARCH)
}

// handleUpdate downloads a release build, verifies it against the release's
// SHA256SUMS, swaps it in and restarts the service.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, body []byte) {
	var req shared.AgentUpdateRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad json")
			return
		}
	}
	repo, version := req.Repo, req.Version
	if repo == "" {
		repo = defaultRepo
	}
	if version == "" {
		version = "latest"
	}
	if !repoRe.MatchString(repo) || !versionRe.MatchString(version) {
		writeErr(w, http.StatusBadRequest, "invalid repo or version")
		return
	}

	newVer, err := s.selfUpdate(repo, version)
	if err != nil {
		s.log("self-update failed: %v", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shared.AgentUpdateResult{From: shared.Version, To: newVer})

	// Restart after the reply is on the wire, so the panel sees success rather
	// than a dropped connection.
	go func() {
		time.Sleep(time.Second)
		s.restartSelf()
	}()
}

// selfUpdate fetches, verifies and installs the release binary, returning the
// version that was installed.
func (s *Server) selfUpdate(repo, version string) (string, error) {
	asset, err := assetName()
	if err != nil {
		return "", err
	}
	base := "https://github.com/" + repo + "/releases/" + version + "/download/"
	if version != "latest" {
		base = "https://github.com/" + repo + "/releases/download/" + version + "/"
	}

	// The checksum list is mandatory: TLS proves who served the file, this
	// proves the release publisher meant to ship exactly these bytes.
	sums, err := httpGet(base+"SHA256SUMS", 1<<20)
	if err != nil {
		return "", fmt.Errorf("release has no SHA256SUMS (published before v0.1.21?): %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
			break
		}
	}
	if len(want) != 64 {
		return "", fmt.Errorf("SHA256SUMS has no entry for %s", asset)
	}

	blob, err := httpGet(base+asset, 64<<20)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("checksum mismatch for %s — refusing to install", asset)
	}

	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, _ = filepath.EvalSymlinks(self)

	// Stage beside the target so the rename stays on one filesystem, then run
	// the new binary once: a truncated or wrong-arch build fails here rather
	// than after it has replaced a working agent.
	tmp := self + ".new"
	if err := os.WriteFile(tmp, blob, 0o755); err != nil {
		return "", err
	}
	out, err := exec.Command(tmp, "-version").CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("downloaded binary does not run: %v", err)
	}
	newVer := strings.TrimSpace(string(out))
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return "", err
	}
	s.log("self-update: %s -> %s", shared.Version, newVer)
	return newVer, nil
}

// restartSelf hands over to the service manager, falling back to exiting and
// letting a supervisor restart us.
func (s *Server) restartSelf() {
	for _, c := range [][]string{
		{"systemctl", "restart", "cub-agent"},
		{"rc-service", "cub-agent", "restart"},
	} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		// The restart kills this process, so a clean return is not expected.
		_ = exec.Command(c[0], c[1:]...).Start()
		time.Sleep(3 * time.Second)
	}
	os.Exit(0)
}

// httpGet fetches a URL with a cap on the response size.
func httpGet(url string, limit int64) ([]byte, error) {
	cl := &http.Client{Timeout: 5 * time.Minute}
	res, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}
