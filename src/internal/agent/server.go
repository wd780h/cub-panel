// Package agent is the lightweight node-side daemon. It exposes a small
// HMAC-authenticated HTTP API that the panel drives, and translates it into
// LXD calls on the local unix socket.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cubpanel/internal/lxd"
	"cubpanel/internal/shared"
)

// Config is the agent's runtime configuration.
type Config struct {
	Listen      string
	Secret      string
	LXDSocket   string
	StoragePool string
	ImageServer string
	ISODir      string
	Verbose     bool
}

// Server is the agent HTTP service.
type Server struct {
	cfg    Config
	lxd    *lxd.Client
	nonces *nonceCache
	remote remoteCache
	hasIPT bool // host can take agent-managed DNAT rules
	// natListenIP is the address DNAT proxy devices bind. "0.0.0.0" (the
	// default) reaches every host address; it degrades to the host's primary
	// IP only on Incus builds that reject a wildcard listen in nat mode.
	natListenIP string
}

// New builds an agent server.
func New(cfg Config) *Server {
	return &Server{
		cfg:         cfg,
		lxd:         lxd.New(cfg.LXDSocket),
		nonces:      newNonceCache(),
		hasIPT:      hasIptables(),
		natListenIP: "0.0.0.0",
	}
}

func (s *Server) log(format string, args ...any) {
	if s.cfg.Verbose {
		log.Printf(format, args...)
	}
}

// nameRe constrains instance names to the LXD-legal subset. Every handler runs
// its path parameter through this before it reaches a URL or a device name.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}$`)

// Handler returns the agent's routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.wrap(s.handleHealth))
	mux.HandleFunc("POST /v1/instances", s.wrap(s.handleCreate))
	mux.HandleFunc("GET /v1/instances/{name}/state", s.wrap(s.handleState))
	mux.HandleFunc("POST /v1/instances/{name}/action", s.wrap(s.handleAction))
	mux.HandleFunc("POST /v1/instances/{name}/password", s.wrap(s.handlePassword))
	mux.HandleFunc("POST /v1/instances/{name}/resize", s.wrap(s.handleResize))
	mux.HandleFunc("DELETE /v1/instances/{name}", s.wrap(s.handleDelete))
	mux.HandleFunc("GET /v1/instances/{name}/snapshots", s.wrap(s.handleSnapshots))
	mux.HandleFunc("POST /v1/instances/{name}/snapshots", s.wrap(s.handleSnapshotCreate))
	mux.HandleFunc("DELETE /v1/instances/{name}/snapshots/{snap}", s.wrap(s.handleSnapshotDelete))
	mux.HandleFunc("POST /v1/instances/{name}/snapshots/restore", s.wrap(s.handleSnapshotRestore))
	mux.HandleFunc("POST /v1/instances/{name}/backup", s.wrap(s.handleBackupCreate))
	mux.HandleFunc("GET /v1/instances/{name}/backup/export", s.wrap(s.handleBackupExport))
	mux.HandleFunc("DELETE /v1/instances/{name}/backup", s.wrap(s.handleBackupDelete))
	mux.HandleFunc("POST /v1/instances/{name}/reconfigure", s.wrap(s.handleReconfigure))
	mux.HandleFunc("POST /v1/import/{name}", s.handleImportStream)
	mux.HandleFunc("GET /v1/isos", s.wrap(s.handleISOList))
	mux.HandleFunc("POST /v1/isos", s.wrap(s.handleISOPull))
	mux.HandleFunc("DELETE /v1/isos/{name}", s.wrap(s.handleISODelete))
	mux.HandleFunc("POST /v1/instances/{name}/iso", s.wrap(s.handleISOAttach))
	mux.HandleFunc("DELETE /v1/instances/{name}/iso", s.wrap(s.handleISODetach))
	mux.HandleFunc("POST /v1/update", s.wrap(s.handleUpdate))
	mux.HandleFunc("GET /v1/storage", s.wrap(s.handleStorage))
	mux.HandleFunc("POST /v1/storage/{pool}/resize", s.wrap(s.handleStorageResize))
	mux.HandleFunc("GET /v1/images", s.wrap(s.handleImages))
	mux.HandleFunc("GET /v1/images/remote", s.wrap(s.handleImagesRemote))
	mux.HandleFunc("POST /v1/images/pull", s.wrap(s.handleImagePull))
	mux.HandleFunc("DELETE /v1/images/{fingerprint}", s.wrap(s.handleImageDelete))
	// The console upgrades to a websocket, so it authenticates inside the
	// handler rather than through the body-buffering wrapper.
	mux.HandleFunc("GET /v1/instances/{name}/console", s.handleConsole)
	return mux
}

// wrap buffers the body, verifies the HMAC signature and rejects replays.
func (s *Server) wrap(h func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "unreadable body")
			return
		}
		if err := shared.Verify(s.cfg.Secret, r, body); err != nil {
			s.log("auth reject %s %s: %v", r.Method, r.URL.Path, err)
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !s.nonces.use(r.Header.Get(shared.HeaderNonce)) {
			writeErr(w, http.StatusUnauthorized, "replayed request")
			return
		}
		h(w, r, body)
	}
}

// instName extracts and validates the {name} path parameter.
func instName(w http.ResponseWriter, r *http.Request) (string, bool) {
	n := r.PathValue("name")
	if !nameRe.MatchString(n) {
		writeErr(w, http.StatusBadRequest, "invalid instance name")
		return "", false
	}
	return n, true
}

// ---------- handlers ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request, _ []byte) {
	ctx := r.Context()
	info := shared.NodeInfo{Agent: "cub-agent/" + shared.Version, CPUCount: runtimeCPUs()}
	if _, err := os.Stat("/dev/kvm"); err == nil {
		info.KVMReady = true
	}
	if si, err := s.lxd.Info(ctx); err == nil {
		info.LXDVersion = si.Environment.ServerVersion
		info.Kernel = si.Environment.KernelVersion
	} else {
		writeErr(w, http.StatusServiceUnavailable, "lxd unreachable")
		return
	}
	if names, err := s.lxd.List(ctx); err == nil {
		info.Instances = len(names)
	}
	info.MemTotal, info.MemFree = memInfo()
	info.Load1 = loadAvg1()
	// Pool names let the panel catch a missing configured pool at probe time.
	if pools, err := s.lxd.StoragePools(ctx); err == nil {
		for _, p := range pools {
			info.Pools = append(info.Pools, p.Name)
		}
	}
	// Managed bridge subnets let the panel catch NAT-subnet mismatches at
	// probe time instead of at the first failed provision.
	if nets, err := s.lxd.Networks(ctx); err == nil {
		for _, n := range nets {
			if n.Managed && n.Config["ipv4.address"] != "" {
				if info.Bridges == nil {
					info.Bridges = map[string]string{}
				}
				info.Bridges[n.Name] = n.Config["ipv4.address"]
			}
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	var req shared.CreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !nameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid instance name")
		return
	}
	if err := validateCreate(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Provisioning outlives the client request; run it against a background
	// context so a panel-side timeout does not abort a half-built instance.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	if err := s.Create(ctx, &req); err != nil {
		s.log("create %s failed: %v", req.Name, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "created"})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	st, err := s.lxd.GetState(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "instance not found")
		return
	}
	out := shared.InstanceState{Name: name, Status: st.Status, MemUsed: st.Memory.Usage, CPUTime: st.CPU.Usage}
	for iface, n := range st.Network {
		if iface == "lo" {
			continue
		}
		out.NetRx += n.Counters.BytesReceived
		out.NetTx += n.Counters.BytesSent
		for _, a := range n.Addresses {
			if a.Scope != "global" {
				continue
			}
			if a.Family == "inet" && out.IPv4 == "" {
				out.IPv4 = a.Address
			}
			if a.Family == "inet6" && out.IPv6 == "" {
				out.IPv6 = a.Address
			}
		}
	}
	if d, ok := st.Disk["root"]; ok {
		out.DiskUse = d.Usage
	}
	if t, err := time.Parse(time.RFC3339, st.StartedAt); err == nil && !t.IsZero() {
		out.Uptime = int64(time.Since(t).Seconds())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
		Force  bool   `json:"force"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	// Allow-list, never pass the client string straight to LXD.
	switch req.Action {
	case "start", "stop", "restart", "freeze", "unfreeze":
	default:
		writeErr(w, http.StatusBadRequest, "unsupported action")
		return
	}
	if err := s.lxd.SetState(r.Context(), name, req.Action, req.Force); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Password) < 8 || len(req.Password) > 128 {
		writeErr(w, http.StatusBadRequest, "invalid password")
		return
	}
	err := s.lxd.ExecScript(r.Context(), name,
		`echo "root:$ICP_PW" | chpasswd`,
		map[string]string{"ICP_PW": req.Password, "PATH": "/usr/sbin:/usr/bin:/sbin:/bin"})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleResize applies new resource limits to a live instance.
func (s *Server) handleResize(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req shared.ResizeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	switch {
	case req.CPU < 1 || req.CPU > 64:
		writeErr(w, http.StatusBadRequest, "cpu out of range")
		return
	case req.MemoryMB < 64 || req.MemoryMB > 262144:
		writeErr(w, http.StatusBadRequest, "memory out of range")
		return
	case req.DiskGB < 1 || req.DiskGB > 4096:
		writeErr(w, http.StatusBadRequest, "disk out of range")
		return
	case req.RateDownMbps < 0 || req.RateDownMbps > 100000 ||
		req.RateUpMbps < 0 || req.RateUpMbps > 100000:
		writeErr(w, http.StatusBadRequest, "bandwidth out of range")
		return
	}
	put := lxd.InstancePut{
		Config: map[string]string{
			"limits.cpu":    fmt.Sprintf("%d", req.CPU),
			"limits.memory": fmt.Sprintf("%dMB", req.MemoryMB),
		},
		Devices: map[string]lxd.Device{
			// PATCH replaces the named device wholly, so restate the full disk.
			"root": {
				"type": "disk",
				"path": "/",
				"pool": s.cfg.StoragePool,
				"size": fmt.Sprintf("%dGiB", req.DiskGB),
			},
		},
	}
	// Bandwidth caps ride on the NIC devices; restate each NIC from its
	// current definition with the limits keys swapped out.
	if in, err := s.lxd.Get(r.Context(), name); err == nil {
		for devName, d := range in.Devices {
			if d["type"] != "nic" {
				continue
			}
			nic := lxd.Device{}
			for k, v := range d {
				if k == "limits.ingress" || k == "limits.egress" {
					continue
				}
				nic[k] = v
			}
			put.Devices[devName] = withRateLimits(nic, req.RateDownMbps, req.RateUpMbps)
		}
	}
	if err := s.lxd.Update(r.Context(), name, put); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resized"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// The root device names the pool that also holds any data volumes; read
	// it before the instance record disappears.
	diskPool := s.cfg.StoragePool
	if in, err := s.lxd.Get(ctx, name); err == nil {
		if root, ok := in.Devices["root"]; ok && root["pool"] != "" {
			diskPool = root["pool"]
		}
	}
	// Force-stop first; LXD refuses to delete a running instance.
	_ = s.lxd.SetState(ctx, name, "stop", true)
	if err := s.lxd.Delete(ctx, name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Data volumes die with the instance (no-op when none exist).
	s.deleteExtraDisks(ctx, diskPool, name)
	// Withdraw any agent-managed DNAT rules (no-op for proxy-device setups).
	if s.hasIPT {
		s.removeDNAT(ctx, name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------- validation ----------

func validateCreate(req *shared.CreateRequest) error {
	switch {
	case req.CPU < 1 || req.CPU > 64:
		return errf("cpu out of range")
	case req.MemoryMB < 64 || req.MemoryMB > 262144:
		return errf("memory out of range")
	case req.DiskGB < 1 || req.DiskGB > 4096:
		return errf("disk out of range")
	case !imageRe.MatchString(req.Image):
		return errf("invalid image alias")
	case req.NATBridge != "" && !ifaceRe.MatchString(req.NATBridge):
		return errf("invalid nat bridge")
	case req.V6Bridge != "" && !ifaceRe.MatchString(req.V6Bridge):
		return errf("invalid ipv6 bridge")
	case req.NATAddr != "" && !ipv4Re.MatchString(req.NATAddr):
		return errf("invalid nat address")
	case req.SSHPort != 0 && (req.SSHPort < 1024 || req.SSHPort > 65535):
		return errf("ssh port out of range")
	case req.PortFrom != 0 && (req.PortFrom < 1024 || req.PortTo > 65535 || req.PortTo < req.PortFrom):
		return errf("port range invalid")
	}
	if !shared.ValidMode(req.Mode) {
		return errf("unsupported network mode")
	}
	switch req.InstanceType {
	case "", shared.TypeContainer, shared.TypeVM:
	default:
		return errf("unsupported instance type")
	}
	if req.InstanceType == shared.TypeVM {
		if _, err := os.Stat("/dev/kvm"); err != nil {
			return errf("this node has no /dev/kvm (KVM unavailable)")
		}
	}
	for _, f := range req.Features {
		if !shared.AllowedFeatures[f] {
			return errf("unsupported feature flag")
		}
	}
	if req.RateDownMbps < 0 || req.RateDownMbps > 100000 ||
		req.RateUpMbps < 0 || req.RateUpMbps > 100000 {
		return errf("bandwidth out of range")
	}
	if shared.ModeHasNAT(req.Mode) {
		if req.NATBridge == "" {
			return errf("nat bridge required")
		}
		// Unmanaged bridges get their address configured inside the guest and
		// therefore need a gateway and prefix from the panel.
		if !req.NATManaged && req.NATAddr != "" {
			if !ipv4Re.MatchString(req.NATGW) {
				return errf("invalid nat gateway")
			}
			if req.NATPrefix < 8 || req.NATPrefix > 30 {
				return errf("invalid nat prefix")
			}
		}
	}
	if shared.ModeHasDV4(req.Mode) {
		if req.V4Bridge == "" {
			return errf("ipv4 bridge required")
		}
		if !ipv4Re.MatchString(req.V4Addr) || !ipv4Re.MatchString(req.V4GW) {
			return errf("invalid dedicated ipv4 address or gateway")
		}
		if req.V4Prefix < 8 || req.V4Prefix > 32 {
			return errf("invalid dedicated ipv4 prefix")
		}
	}
	if shared.ModeHasDV6(req.Mode) {
		if req.V6Bridge == "" {
			return errf("ipv6 bridge required")
		}
		if !ipv6Re.MatchString(req.V6Addr) {
			return errf("invalid ipv6 address")
		}
		if req.V6Prefix < 32 || req.V6Prefix > 128 {
			return errf("invalid ipv6 prefix")
		}
		if req.V6GW != "" && !ipv6Re.MatchString(req.V6GW) {
			return errf("invalid ipv6 gateway")
		}
	}
	if req.DNS != "" {
		toks := strings.FieldsFunc(req.DNS, func(r rune) bool { return r == ' ' || r == ',' })
		if len(toks) == 0 || len(toks) > 8 {
			return errf("invalid dns list")
		}
		for _, t := range toks {
			if !ipv4Re.MatchString(t) && !ipv6Re.MatchString(t) {
				return errf("invalid dns server")
			}
		}
	}
	if len(req.RootPassword) < 8 || len(req.RootPassword) > 128 {
		return errf("invalid root password")
	}
	return nil
}

var (
	imageRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{1,63}$`)
	ifaceRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)
	ipv4Re  = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)
	ipv6Re  = regexp.MustCompile(`^[0-9a-fA-F:]{2,45}$`)
)

type strErr string

func (e strErr) Error() string { return string(e) }
func errf(s string) error      { return strErr(s) }

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, shared.APIError{Error: msg})
}

func runtimeCPUs() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "processor\t:")
}

func memInfo() (total, free int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			free = v * 1024
		}
	}
	return
}

func loadAvg1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// ---------- nonce replay cache ----------

type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceCache() *nonceCache {
	c := &nonceCache{seen: make(map[string]time.Time)}
	go c.reap()
	return c
}

// use records a nonce, returning false if it was already used.
func (c *nonceCache) use(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.seen[n]; dup {
		return false
	}
	c.seen[n] = time.Now()
	return true
}

func (c *nonceCache) reap() {
	for range time.Tick(time.Minute) {
		cutoff := time.Now().Add(-2 * shared.MaxClockSkew)
		c.mu.Lock()
		for k, t := range c.seen {
			if t.Before(cutoff) {
				delete(c.seen, k)
			}
		}
		c.mu.Unlock()
	}
}
