// ISO library and CD-ROM attach for KVM guests.
//
// The node keeps a small library of ISO files under cfg.ISODir. The panel
// pushes ISOs there by URL (the agent downloads them) and attaches one to a
// VM as a CD-ROM disk device; boot.priority lets the VM boot from it, e.g.
// to install a custom OS. Containers cannot mount ISOs, so attach is VM-only.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cubpanel/internal/lxd"
	"cubpanel/internal/shared"
)

// isoNameRe constrains stored filenames: no path separators, ends in .iso.
var isoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.iso$`)

func (s *Server) isoDir() string {
	if s.cfg.ISODir != "" {
		return s.cfg.ISODir
	}
	return "/var/lib/cub-panel/isos"
}

func (s *Server) handleISOList(w http.ResponseWriter, r *http.Request, _ []byte) {
	entries, err := os.ReadDir(s.isoDir())
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, []shared.ISOInfo{})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]shared.ISOInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".iso") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, shared.ISOInfo{Name: e.Name(), SizeBytes: info.Size()})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleISOPull downloads an ISO from a URL into the library. Runs against a
// background context because ISOs are large.
func (s *Server) handleISOPull(w http.ResponseWriter, r *http.Request, body []byte) {
	var req shared.ISOPullRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !isoNameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid iso filename (must end in .iso)")
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeErr(w, http.StatusBadRequest, "url must be http(s)")
		return
	}
	dir := s.isoDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dst := filepath.Join(dir, req.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, "GET", req.URL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := isoHTTP.Do(hreq)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "download failed: "+err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("download failed: HTTP %d", res.StatusCode))
		return
	}
	// Write to a temp file then rename, so a partial download never looks ready.
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		writeErr(w, http.StatusBadGateway, "download interrupted: "+err.Error())
		return
	}
	f.Close()
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "downloaded"})
}

var isoHTTP = &http.Client{Timeout: 60 * time.Minute}

func (s *Server) handleISODelete(w http.ResponseWriter, r *http.Request, _ []byte) {
	name := r.PathValue("name")
	if !isoNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid iso filename")
		return
	}
	if err := os.Remove(filepath.Join(s.isoDir(), name)); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleISOAttach mounts a library ISO on a VM as a CD-ROM.
func (s *Server) handleISOAttach(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req shared.ISOAttachRequest
	if err := json.Unmarshal(body, &req); err != nil || !isoNameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	path := filepath.Join(s.isoDir(), req.Name)
	if _, err := os.Stat(path); err != nil {
		writeErr(w, http.StatusNotFound, "iso not found in library")
		return
	}
	// A raw .iso disk source is presented to a VM as a read-only CD-ROM.
	dev := lxd.Device{"type": "disk", "source": path}
	if req.Boot {
		dev["boot.priority"] = "10" // higher than the root disk → boot from ISO
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.lxd.AddDevice(ctx, name, "panel-iso", dev); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

// handleISODetach removes the CD-ROM device from a VM.
func (s *Server) handleISODetach(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.lxd.RemoveDevice(ctx, name, "panel-iso"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}
