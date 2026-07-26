// Package lxd is a minimal LXD REST client speaking to the local unix socket.
// It covers only what the panel needs, which keeps the agent binary small and
// avoids pulling in the (very large) official LXD client module.
package lxd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to LXD over its unix socket.
type Client struct {
	http   *http.Client
	socket string
}

// New returns a Client bound to the given LXD unix socket path.
func New(socket string) *Client {
	return &Client{
		socket: socket,
		http: &http.Client{
			Timeout: 10 * time.Minute, // image downloads on first launch are slow
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// Socket exposes the socket path (the console proxy dials it directly).
func (c *Client) Socket() string { return c.socket }

type response struct {
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	StatusCode int             `json:"status_code"`
	Operation  string          `json:"operation"`
	Error      string          `json:"error"`
	ErrorCode  int             `json:"error_code"`
	Metadata   json.RawMessage `json:"metadata"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://lxd"+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lxd unreachable on %s: %w", c.socket, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("lxd returned non-JSON (%d): %s", res.StatusCode, truncate(string(raw), 200))
	}
	if r.Type == "error" || r.ErrorCode != 0 {
		return nil, fmt.Errorf("lxd: %s", r.Error)
	}
	return &r, nil
}

// sync issues a request and unmarshals the metadata of a synchronous reply.
func (c *Client) sync(ctx context.Context, method, path string, body, out any) error {
	r, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out != nil && len(r.Metadata) > 0 {
		return json.Unmarshal(r.Metadata, out)
	}
	return nil
}

// async issues a request and blocks until the resulting operation settles.
func (c *Client) async(ctx context.Context, method, path string, body any) error {
	r, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if r.Operation == "" {
		return nil
	}
	return c.WaitOp(ctx, r.Operation)
}

// WaitOp blocks until the referenced LXD operation completes.
func (c *Client) WaitOp(ctx context.Context, op string) error {
	// op arrives as a full path such as /1.0/operations/<uuid>.
	op = strings.TrimSuffix(op, "/wait")
	r, err := c.do(ctx, "GET", op+"/wait?timeout=600", nil)
	if err != nil {
		return err
	}
	var meta struct {
		Status     string `json:"status"`
		StatusCode int    `json:"status_code"`
		Err        string `json:"err"`
	}
	if len(r.Metadata) > 0 {
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			return err
		}
	}
	if meta.Err != "" {
		return fmt.Errorf("lxd operation failed: %s", meta.Err)
	}
	// 200..399 are the LXD success range; anything else is a failure.
	if meta.StatusCode >= 400 {
		return fmt.Errorf("lxd operation failed: %s", meta.Status)
	}
	return nil
}

// ---------- server ----------

// ServerInfo reports LXD environment details used for node health.
type ServerInfo struct {
	Environment struct {
		ServerVersion string `json:"server_version"`
		KernelVersion string `json:"kernel_version"`
	} `json:"environment"`
}

// Info returns LXD server metadata.
func (c *Client) Info(ctx context.Context) (*ServerInfo, error) {
	var si ServerInfo
	if err := c.sync(ctx, "GET", "/1.0", nil, &si); err != nil {
		return nil, err
	}
	return &si, nil
}

// ---------- instances ----------

// Device is one LXD device definition.
type Device map[string]string

// InstancePut is the mutable part of an instance record.
type InstancePut struct {
	Config  map[string]string `json:"config,omitempty"`
	Devices map[string]Device `json:"devices,omitempty"`
}

// InstanceSource describes where a new instance's rootfs comes from.
type InstanceSource struct {
	Type     string `json:"type"`
	Alias    string `json:"alias,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Server   string `json:"server,omitempty"`
}

// InstancesPost creates an instance.
type InstancesPost struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"` // "container"
	Source   InstanceSource    `json:"source"`
	Config   map[string]string `json:"config,omitempty"`
	Devices  map[string]Device `json:"devices,omitempty"`
	Profiles []string          `json:"profiles,omitempty"`
}

// Instance is an LXD instance record.
type Instance struct {
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Config  map[string]string `json:"config"`
	Devices map[string]Device `json:"devices"`
}

// State is the runtime state of an instance.
type State struct {
	Status  string `json:"status"`
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
		Counters struct {
			BytesReceived int64 `json:"bytes_received"`
			BytesSent     int64 `json:"bytes_sent"`
		} `json:"counters"`
	} `json:"network"`
	Memory struct {
		Usage int64 `json:"usage"`
	} `json:"memory"`
	Disk map[string]struct {
		Usage int64 `json:"usage"`
	} `json:"disk"`
	CPU struct {
		Usage int64 `json:"usage"`
	} `json:"cpu"`
	Processes int64  `json:"processes"`
	StartedAt string `json:"started_at"`
}

// Create provisions a new instance and waits for it to exist.
func (c *Client) Create(ctx context.Context, req InstancesPost) error {
	return c.async(ctx, "POST", "/1.0/instances", req)
}

// Get fetches an instance record.
func (c *Client) Get(ctx context.Context, name string) (*Instance, error) {
	var in Instance
	if err := c.sync(ctx, "GET", "/1.0/instances/"+url.PathEscape(name), nil, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// GetState fetches instance runtime state.
func (c *Client) GetState(ctx context.Context, name string) (*State, error) {
	var st State
	if err := c.sync(ctx, "GET", "/1.0/instances/"+url.PathEscape(name)+"/state", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// List returns the names of all instances on the node.
func (c *Client) List(ctx context.Context) ([]string, error) {
	var paths []string
	if err := c.sync(ctx, "GET", "/1.0/instances", nil, &paths); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, p[strings.LastIndex(p, "/")+1:])
	}
	return names, nil
}

// Update applies a config/device patch to an instance.
func (c *Client) Update(ctx context.Context, name string, put InstancePut) error {
	return c.async(ctx, "PATCH", "/1.0/instances/"+url.PathEscape(name), put)
}

type statePut struct {
	Action   string `json:"action"`
	Timeout  int    `json:"timeout"`
	Force    bool   `json:"force"`
	Stateful bool   `json:"stateful"`
}

// SetState drives an instance lifecycle action (start/stop/restart/freeze).
func (c *Client) SetState(ctx context.Context, name, action string, force bool) error {
	return c.async(ctx, "PUT", "/1.0/instances/"+url.PathEscape(name)+"/state",
		statePut{Action: action, Timeout: 60, Force: force})
}

// Delete removes an instance. The caller is expected to stop it first.
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.async(ctx, "DELETE", "/1.0/instances/"+url.PathEscape(name), nil)
}

// AddDevice merges one device into an instance (PATCH keeps the rest).
func (c *Client) AddDevice(ctx context.Context, name, devName string, dev Device) error {
	return c.async(ctx, "PATCH", "/1.0/instances/"+url.PathEscape(name),
		InstancePut{Devices: map[string]Device{devName: dev}})
}

// RemoveDevice drops one device by rewriting the full instance record (PATCH
// cannot delete a device key, so fetch-modify-PUT).
func (c *Client) RemoveDevice(ctx context.Context, name, devName string) error {
	in, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	if _, ok := in.Devices[devName]; !ok {
		return nil // already gone
	}
	delete(in.Devices, devName)
	return c.async(ctx, "PUT", "/1.0/instances/"+url.PathEscape(name),
		InstancePut{Config: in.Config, Devices: in.Devices})
}

// ---------- snapshots ----------

// Snapshot is one instance snapshot.
type Snapshot struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Size      int64  `json:"size"`
}

// Snapshots lists an instance's snapshots (recursion=1 for full records).
func (c *Client) Snapshots(ctx context.Context, name string) ([]Snapshot, error) {
	var snaps []Snapshot
	if err := c.sync(ctx, "GET", "/1.0/instances/"+url.PathEscape(name)+"/snapshots?recursion=1", nil, &snaps); err != nil {
		return nil, err
	}
	return snaps, nil
}

// SnapshotCreate takes a stateless snapshot.
func (c *Client) SnapshotCreate(ctx context.Context, name, snap string) error {
	return c.async(ctx, "POST", "/1.0/instances/"+url.PathEscape(name)+"/snapshots",
		map[string]any{"name": snap, "stateful": false})
}

// SnapshotDelete removes a snapshot.
func (c *Client) SnapshotDelete(ctx context.Context, name, snap string) error {
	return c.async(ctx, "DELETE", "/1.0/instances/"+url.PathEscape(name)+"/snapshots/"+url.PathEscape(snap), nil)
}

// SnapshotRestore rolls an instance back to a snapshot.
func (c *Client) SnapshotRestore(ctx context.Context, name, snap string) error {
	return c.async(ctx, "PUT", "/1.0/instances/"+url.PathEscape(name),
		map[string]any{"restore": snap})
}

// ---------- backup export / import (migration) ----------

// BackupCreate makes an optimized backup tarball ready to stream out.
func (c *Client) BackupCreate(ctx context.Context, name, backup string) error {
	return c.async(ctx, "POST", "/1.0/instances/"+url.PathEscape(name)+"/backups",
		map[string]any{
			"name":                  backup,
			"instance_only":         false,
			"optimized_storage":     false, // portable across storage drivers
			"compression_algorithm": "gzip",
		})
}

// BackupDelete removes a backup.
func (c *Client) BackupDelete(ctx context.Context, name, backup string) error {
	return c.async(ctx, "DELETE", "/1.0/instances/"+url.PathEscape(name)+"/backups/"+url.PathEscape(backup), nil)
}

// BackupExport streams a backup tarball to w.
func (c *Client) BackupExport(ctx context.Context, name, backup string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://lxd/1.0/instances/"+url.PathEscape(name)+"/backups/"+url.PathEscape(backup)+"/export", nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("backup export failed: HTTP %d", res.StatusCode)
	}
	_, err = io.Copy(w, res.Body)
	return err
}

// BackupImport creates an instance from a backup tarball read from r.
func (c *Client) BackupImport(ctx context.Context, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, "POST", "http://lxd/1.0/instances", r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var resp response
	_ = json.Unmarshal(raw, &resp)
	if resp.Type == "error" || resp.ErrorCode != 0 {
		return fmt.Errorf("backup import: %s", resp.Error)
	}
	if resp.Operation != "" {
		return c.WaitOp(ctx, resp.Operation)
	}
	return nil
}

// ---------- images ----------

// Image is a locally cached image record.
type Image struct {
	Fingerprint string `json:"fingerprint"`
	Size        int64  `json:"size"`
	Type        string `json:"type"` // container | virtual-machine
	Aliases     []struct {
		Name string `json:"name"`
	} `json:"aliases"`
	Properties   map[string]string `json:"properties"`
	UploadedAt   string            `json:"uploaded_at"`
	UpdateSource *struct {
		Alias string `json:"alias"`
	} `json:"update_source,omitempty"`
}

// Images lists the images cached on the node.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var imgs []Image
	if err := c.sync(ctx, "GET", "/1.0/images?recursion=1", nil, &imgs); err != nil {
		return nil, err
	}
	return imgs, nil
}

type imagesPost struct {
	Source struct {
		Type      string `json:"type"`
		Mode      string `json:"mode"`
		Server    string `json:"server"`
		Protocol  string `json:"protocol"`
		Alias     string `json:"alias"`
		ImageType string `json:"image_type,omitempty"` // "" | virtual-machine
	} `json:"source"`
	Aliases []struct {
		Name string `json:"name"`
	} `json:"aliases,omitempty"`
	AutoUpdate bool `json:"auto_update"`
}

// ImagePull downloads an image from a simplestreams server into the local
// cache and tags it with its own alias, so later instance creation is instant.
// imageType "virtual-machine" fetches the KVM disk image; "" (or "container")
// fetches the container rootfs. Under one alias simplestreams carries both.
func (c *Client) ImagePull(ctx context.Context, server, alias, imageType string) error {
	var req imagesPost
	req.Source.Type = "image"
	req.Source.Mode = "pull"
	req.Source.Server = server
	req.Source.Protocol = "simplestreams"
	req.Source.Alias = alias
	if imageType == "virtual-machine" {
		req.Source.ImageType = "virtual-machine"
		// Container and VM images cannot share a local alias, and VM creates
		// match by fingerprint anyway, so the VM image caches without one
		// (its source alias is still recorded for display).
	} else {
		req.Aliases = []struct {
			Name string `json:"name"`
		}{{Name: alias}}
	}
	req.AutoUpdate = true
	return c.async(ctx, "POST", "/1.0/images", req)
}

// ImageDelete removes a cached image by fingerprint.
func (c *Client) ImageDelete(ctx context.Context, fingerprint string) error {
	return c.async(ctx, "DELETE", "/1.0/images/"+url.PathEscape(fingerprint), nil)
}

// ---------- exec ----------

// ExecPost requests command execution inside an instance.
type ExecPost struct {
	Command      []string          `json:"command"`
	Environment  map[string]string `json:"environment,omitempty"`
	WaitForWS    bool              `json:"wait-for-websocket"`
	Interactive  bool              `json:"interactive"`
	Width        int               `json:"width,omitempty"`
	Height       int               `json:"height,omitempty"`
	RecordOutput bool              `json:"record-output"`
}

// ExecOperation identifies the websockets backing an interactive exec.
type ExecOperation struct {
	ID       string
	Control  string // secret for the control channel
	Terminal string // secret for the combined stdio channel (interactive mode)
}

// ExecInteractive starts an interactive exec and returns the operation and the
// per-channel secrets needed to attach websockets.
func (c *Client) ExecInteractive(ctx context.Context, name string, cmd []string, w, h int) (*ExecOperation, error) {
	r, err := c.do(ctx, "POST", "/1.0/instances/"+url.PathEscape(name)+"/exec", ExecPost{
		Command:     cmd,
		WaitForWS:   true,
		Interactive: true,
		Width:       w,
		Height:      h,
		Environment: map[string]string{
			"TERM": "xterm-256color",
			"HOME": "/root",
			"USER": "root",
			"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
	})
	if err != nil {
		return nil, err
	}
	var meta struct {
		Metadata struct {
			Fds map[string]string `json:"fds"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(r.Metadata, &meta); err != nil {
		return nil, err
	}
	id := r.Operation[strings.LastIndex(r.Operation, "/")+1:]
	return &ExecOperation{
		ID:       id,
		Control:  meta.Metadata.Fds["control"],
		Terminal: meta.Metadata.Fds["0"],
	}, nil
}

// ExecOnce runs a command to completion, discarding output.
func (c *Client) ExecOnce(ctx context.Context, name string, cmd []string, env map[string]string) error {
	return c.async(ctx, "POST", "/1.0/instances/"+url.PathEscape(name)+"/exec", ExecPost{
		Command:     cmd,
		WaitForWS:   false,
		Interactive: false,
		Environment: env,
	})
}

// ExecScript runs a POSIX shell script inside the instance. The script text is
// passed as an argv element, not through a shell, and all dynamic values
// should be supplied via env so nothing is interpolated into shell syntax.
func (c *Client) ExecScript(ctx context.Context, name, script string, env map[string]string) error {
	return c.ExecOnce(ctx, name, []string{"/bin/sh", "-c", script}, env)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
