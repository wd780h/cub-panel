// Image management: list the node's cached images, pull ("pre-warm") an
// image from the configured simplestreams server, delete a cached image and
// enumerate what the remote server currently offers.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"cubpanel/internal/shared"
)

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// handleImages returns the images cached on this node.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request, _ []byte) {
	imgs, err := s.lxd.Images(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]shared.LocalImage, 0, len(imgs))
	for _, im := range imgs {
		li := shared.LocalImage{
			Fingerprint: im.Fingerprint,
			Type:        im.Type,
			OS:          im.Properties["os"],
			Release:     im.Properties["release"],
			Variant:     im.Properties["variant"],
			Arch:        im.Properties["architecture"],
			SizeBytes:   im.Size,
		}
		for _, a := range im.Aliases {
			li.Aliases = append(li.Aliases, a.Name)
		}
		// Images pulled without an explicit alias still carry their source
		// alias; surface it so the operator can tell what the image is.
		if len(li.Aliases) == 0 && im.UpdateSource != nil && im.UpdateSource.Alias != "" {
			li.Aliases = append(li.Aliases, im.UpdateSource.Alias)
		}
		if t, err := time.Parse(time.RFC3339, im.UploadedAt); err == nil {
			li.UploadedAt = t.Unix()
		}
		out = append(out, li)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleImagePull downloads one image from the configured image server.
func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Alias string `json:"alias"`
		Type  string `json:"type"` // "" | container | virtual-machine
	}
	if err := json.Unmarshal(body, &req); err != nil || !imageRe.MatchString(req.Alias) {
		writeErr(w, http.StatusBadRequest, "invalid image alias")
		return
	}
	if req.Type != "" && req.Type != "container" && req.Type != "virtual-machine" {
		writeErr(w, http.StatusBadRequest, "invalid image type")
		return
	}
	// Downloads outlive panel-side timeouts, same rationale as instance
	// creation: run against a background context.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := s.lxd.ImagePull(ctx, s.cfg.ImageServer, req.Alias, req.Type); err != nil {
		s.log("image pull %s failed: %v", req.Alias, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pulled"})
}

// handleImageDelete removes one cached image by fingerprint.
func (s *Server) handleImageDelete(w http.ResponseWriter, r *http.Request, _ []byte) {
	fp := r.PathValue("fingerprint")
	if !fingerprintRe.MatchString(fp) {
		writeErr(w, http.StatusBadRequest, "invalid fingerprint")
		return
	}
	if err := s.lxd.ImageDelete(r.Context(), fp); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleImagesRemote lists the aliases the image server offers for this
// node's architecture.
func (s *Server) handleImagesRemote(w http.ResponseWriter, r *http.Request, _ []byte) {
	list, err := s.remoteImages(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---------- simplestreams catalogue ----------

// remoteCache avoids hammering the image server: the product list only
// changes a few times a day.
type remoteCache struct {
	mu      sync.Mutex
	fetched time.Time
	list    []shared.RemoteImage
}

var ssHTTP = &http.Client{Timeout: 30 * time.Second}

// remoteImages fetches (with a 10-minute cache) the simplestreams index and
// turns its product names into instance-launchable aliases.
func (s *Server) remoteImages(ctx context.Context) ([]shared.RemoteImage, error) {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	if time.Since(s.remote.fetched) < 10*time.Minute && s.remote.list != nil {
		return s.remote.list, nil
	}

	// The index alone carries the product names ("debian:12:amd64:default"),
	// which is all we need — no multi-megabyte images.json download.
	u := strings.TrimRight(s.cfg.ImageServer, "/") + "/streams/v1/index.json"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	res, err := ssHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image server unreachable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image server returned HTTP %d", res.StatusCode)
	}
	var idx struct {
		Index map[string]struct {
			Products []string `json:"products"`
		} `json:"index"`
	}
	if err := json.NewDecoder(res.Body).Decode(&idx); err != nil {
		return nil, fmt.Errorf("bad simplestreams index: %w", err)
	}

	arch := ssArch()
	seen := map[string]bool{}
	var list []shared.RemoteImage
	for _, entry := range idx.Index {
		for _, p := range entry.Products {
			// Product names are os:release:arch:variant.
			f := strings.Split(p, ":")
			if len(f) != 4 || f[2] != arch || seen[p] {
				continue
			}
			seen[p] = true
			alias := f[0] + "/" + f[1]
			if f[3] != "default" {
				alias += "/" + f[3]
			}
			list = append(list, shared.RemoteImage{Alias: alias, OS: f[0], Release: f[1], Variant: f[3]})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Alias < list[j].Alias })

	s.remote.fetched = time.Now()
	s.remote.list = list
	return list, nil
}

// ssArch maps the agent's GOARCH onto simplestreams architecture names.
func ssArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "armhf"
	case "386":
		return "i386"
	default:
		return runtime.GOARCH
	}
}
