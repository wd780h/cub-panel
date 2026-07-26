// Package shared holds the wire contract between the panel (master) and the
// node agent, plus the HMAC scheme that authenticates it.
package shared

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderTimestamp = "X-Cub-Panel-Ts"
	HeaderNonce     = "X-Cub-Panel-Nonce"
	HeaderSignature = "X-Cub-Panel-Sign"

	// MaxClockSkew bounds how far an agent request timestamp may drift. Paired
	// with the agent's nonce cache this makes captured requests non-replayable.
	MaxClockSkew = 90 * time.Second
)

// Sign returns the hex HMAC-SHA256 over the canonical request string. Body is
// hashed rather than concatenated so binary payloads cannot shift field
// boundaries.
func Sign(secret, method, path, ts, nonce string, body []byte) string {
	bodySum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", method, path, ts, nonce, hex.EncodeToString(bodySum[:]))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks an inbound agent request signature in constant time.
func Verify(secret string, r *http.Request, body []byte) error {
	ts := r.Header.Get(HeaderTimestamp)
	nonce := r.Header.Get(HeaderNonce)
	got := r.Header.Get(HeaderSignature)
	if ts == "" || nonce == "" || got == "" {
		return errors.New("missing auth headers")
	}
	epoch, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	if d := time.Since(time.Unix(epoch, 0)); d > MaxClockSkew || d < -MaxClockSkew {
		return errors.New("timestamp outside allowed skew")
	}
	want := Sign(secret, r.Method, r.URL.Path, ts, nonce, body)
	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}

// ---------- wire types ----------

// NetMode selects how an instance is reachable from the outside.
type NetMode string

const (
	// NetNAT places the instance on the NAT bridge and DNATs a port block to it.
	NetNAT NetMode = "nat"
	// NetIPv6 additionally attaches a routed/bridged interface with a dedicated
	// IPv6 address taken from the node's configured range.
	NetIPv6 NetMode = "ipv6"
	// NetV6Only skips NAT entirely: the instance's single NIC sits on the IPv6
	// bridge with a dedicated address. No IPv4, no port forwards.
	NetV6Only NetMode = "ipv6only"
	// NetIPv4 is NAT plus a dedicated public IPv4 on a second NIC.
	NetIPv4 NetMode = "ipv4"
	// NetV4Only is a dedicated public IPv4 only (no NAT, no IPv6).
	NetV4Only NetMode = "ipv4only"
	// NetDual is NAT plus a dedicated public IPv4 and a dedicated IPv6.
	NetDual NetMode = "ipv4v6"
)

// ValidMode reports whether m is a supported network mode.
func ValidMode(m NetMode) bool {
	switch m {
	case NetNAT, NetIPv6, NetV6Only, NetIPv4, NetV4Only, NetDual:
		return true
	}
	return false
}

// ModeHasNAT reports whether the mode places the instance on the NAT bridge
// (internal IPv4 + forwarded ports).
func ModeHasNAT(m NetMode) bool {
	switch m {
	case NetNAT, NetIPv6, NetIPv4, NetDual:
		return true
	}
	return false
}

// ModeHasDV4 reports whether the mode assigns a dedicated public IPv4.
func ModeHasDV4(m NetMode) bool {
	return m == NetIPv4 || m == NetV4Only || m == NetDual
}

// ModeHasDV6 reports whether the mode assigns a dedicated IPv6.
func ModeHasDV6(m NetMode) bool {
	return m == NetIPv6 || m == NetV6Only || m == NetDual
}

// Features the panel may enable per plan; the agent whitelists this set.
// Container features: tun/fuse/privileged/nesting. VM features: nesting
// (nested virt), aes (host AES-NI passthrough), cpuhide (generic CPU model to
// mask the hypervisor).
var AllowedFeatures = map[string]bool{
	"tun":        true, // /dev/net/tun for VPN workloads (container)
	"fuse":       true, // /dev/fuse (container)
	"privileged": true, // security.privileged=true (container)
	"nesting":    true, // nested containers / nested virtualization
	"aes":        true, // VM: expose host AES-NI
	"cpuhide":    true, // VM: generic cpu model, hide hypervisor vendor
}

// OSFamily drives which in-guest provisioning recipe the agent uses.
type OSFamily string

const (
	OSDebian OSFamily = "debian" // debian & ubuntu: apt, systemd
	OSAlpine OSFamily = "alpine" // apk, openrc
)

// Instance types. VMs (KVM) are beta: same bridges, quotas and rate limits
// as containers, but container-only knobs (features, in-guest static v4
// tricks that rely on shared kernels) degrade gracefully.
const (
	TypeContainer = "container"
	TypeVM        = "vm"
)

// CreateRequest is the panel's instruction to materialise an instance.
type CreateRequest struct {
	Name string `json:"name"`
	// InstanceType selects container (default) or KVM virtual machine (beta).
	InstanceType string   `json:"instance_type,omitempty"`
	Image        string   `json:"image"` // simplestreams alias, e.g. "debian/12"
	Family       OSFamily `json:"family"`
	CPU          int      `json:"cpu"`       // cores
	MemoryMB     int      `json:"memory_mb"` // hard limit
	DiskGB       int      `json:"disk_gb"`
	// StoragePool is the node's pool from the panel config; empty falls back
	// to the agent's CUB_AGENT_POOL. Disk quotas need lvm/btrfs/zfs — the dir
	// driver silently ignores the root size.
	StoragePool string  `json:"storage_pool,omitempty"`
	Mode        NetMode `json:"mode"`
	// Optional per-plan features (tun, fuse, privileged, nesting).
	Features []string `json:"features,omitempty"`
	// NIC bandwidth caps in Mbps (limits.ingress/egress); 0 = unlimited.
	RateDownMbps int `json:"rate_down_mbps,omitempty"`
	RateUpMbps   int `json:"rate_up_mbps,omitempty"`
	// ExtraBridges attaches additional bridged NICs (eth2, eth3…) — VM multi-NIC.
	ExtraBridges []string `json:"extra_bridges,omitempty"`
	// Mounts binds host directories into the guest (admin-defined per plan).
	Mounts []MountSpec `json:"mounts,omitempty"`
	// VNC (VM only): host port + password for the SPICE/VNC console.
	VNCPort int    `json:"vnc_port,omitempty"`
	VNCPass string `json:"vnc_pass,omitempty"`
	// NAT. NATAddr is a static lease on NATBridge so the DNAT proxy devices
	// keep pointing at the right place across reboots.
	NATBridge string `json:"nat_bridge"`
	NATAddr   string `json:"nat_addr"`
	// NATManaged is true when the bridge is Incus-managed (static leases via
	// ipv4.address). On a foreign bridge (docker0, br0…) the address is
	// configured inside the guest instead, using NATGW/NATPrefix, and port
	// forwards fall back to userspace proxying.
	NATManaged bool   `json:"nat_managed"`
	NATGW      string `json:"nat_gw,omitempty"`
	NATPrefix  int    `json:"nat_prefix,omitempty"`
	SSHPort    int    `json:"ssh_port"`  // host port DNAT'd to :22
	PortFrom   int    `json:"port_from"` // inclusive host port block
	PortTo     int    `json:"port_to"`   // inclusive
	// KeepSourceIP: NAT forwards preserve the real client source IP (DNAT).
	// When false the agent uses a userspace proxy that hides it behind the host.
	KeepSourceIP bool `json:"keep_source_ip"`
	// IPv6 (dedicated): a NIC on an operator-chosen bridge.
	V6Bridge string `json:"v6_bridge"`
	V6Addr   string `json:"v6_addr"` // address only, no prefix length
	V6Prefix int    `json:"v6_prefix"`
	V6GW     string `json:"v6_gw"`
	// Dedicated public IPv4: a NIC on the node's v4 bridge, statically
	// configured in the guest.
	V4Bridge string `json:"v4_bridge,omitempty"`
	V4Addr   string `json:"v4_addr,omitempty"` // dedicated public IPv4
	V4Prefix int    `json:"v4_prefix,omitempty"`
	V4GW     string `json:"v4_gw,omitempty"`
	// DNS resolvers written into the guest (space/comma separated). Empty
	// keeps the distro/bridge default (v6-only instances still get a sane
	// public default from the agent).
	DNS string `json:"dns,omitempty"`
	// Credentials
	RootPassword string `json:"root_password"`
}

// ResizeRequest adjusts a live instance's resource limits. Disk can only
// grow — most storage drivers cannot shrink a filesystem safely.
type ResizeRequest struct {
	CPU          int `json:"cpu"`
	MemoryMB     int `json:"memory_mb"`
	DiskGB       int `json:"disk_gb"`
	RateDownMbps int `json:"rate_down_mbps"` // 0 = unlimited
	RateUpMbps   int `json:"rate_up_mbps"`   // 0 = unlimited
}

// SnapshotRequest names a snapshot to create.
type SnapshotRequest struct {
	Snapshot string `json:"snapshot"`
}

// SnapshotInfo is one snapshot as reported by the agent.
type SnapshotInfo struct {
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// ExportResponse streams an instance image for migration (handled specially,
// not JSON). MigrateImportRequest re-creates an instance from a pulled image
// on the destination node.
type MigrateImportRequest struct {
	Name       string        `json:"name"`
	SourceURL  string        `json:"source_url"`  // https URL on the source agent
	SourceFP   string        `json:"source_fp"`   // pinned cert of the source
	SourceAuth string        `json:"source_auth"` // one-shot bearer for the export
	Create     CreateRequest `json:"create"`      // network/limits on the destination
}

// InstanceState is the agent's view of one instance.
type InstanceState struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	IPv4    string `json:"ipv4"`
	IPv6    string `json:"ipv6"`
	CPUTime int64  `json:"cpu_time_ns"`
	MemUsed int64  `json:"mem_used"`
	DiskUse int64  `json:"disk_used"`
	NetRx   int64  `json:"net_rx"`
	NetTx   int64  `json:"net_tx"`
	Uptime  int64  `json:"uptime_s"`
}

// NodeInfo is returned by the agent health endpoint.
type NodeInfo struct {
	Agent      string  `json:"agent"`
	KVMReady   bool    `json:"kvm_ready"` // /dev/kvm present on the node
	LXDVersion string  `json:"lxd_version"`
	Kernel     string  `json:"kernel"`
	CPUCount   int     `json:"cpu_count"`
	MemTotal   int64   `json:"mem_total"`
	MemFree    int64   `json:"mem_free"`
	Load1      float64 `json:"load1"`
	Instances  int     `json:"instances"`
}

// LocalImage describes one image cached on a node.
type LocalImage struct {
	Fingerprint string   `json:"fingerprint"`
	Type        string   `json:"type"` // container | virtual-machine
	Aliases     []string `json:"aliases"`
	OS          string   `json:"os"`
	Release     string   `json:"release"`
	Variant     string   `json:"variant"`
	Arch        string   `json:"arch"`
	SizeBytes   int64    `json:"size_bytes"`
	UploadedAt  int64    `json:"uploaded_at"`
}

// RemoteImage is one alias offered by the node's simplestreams image server.
type RemoteImage struct {
	Alias   string `json:"alias"`
	OS      string `json:"os"`
	Release string `json:"release"`
	Variant string `json:"variant"`
}

// ISOInfo is one ISO file in a node's library.
// MountSpec is one host-directory bind mount for an instance. Admin-defined
// only — exposing host paths to tenants is a host-security decision.
type MountSpec struct {
	Source   string `json:"source"` // absolute host path
	Path     string `json:"path"`   // absolute guest path
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ParseMounts parses "hostpath:guestpath[:ro]" specs, one per line or
// comma-separated. Both paths must be absolute and clean; the guest path may
// not be "/". At most 8 mounts.
func ParseMounts(s string) ([]MountSpec, error) {
	var out []MountSpec
	for _, item := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("挂载格式应为 宿主机路径:容器路径[:ro] — %q", item)
		}
		m := MountSpec{Source: strings.TrimSpace(parts[0]), Path: strings.TrimSpace(parts[1])}
		if len(parts) == 3 {
			switch strings.TrimSpace(parts[2]) {
			case "ro":
				m.ReadOnly = true
			case "rw", "":
			default:
				return nil, fmt.Errorf("挂载选项只支持 ro/rw — %q", item)
			}
		}
		for _, p := range []string{m.Source, m.Path} {
			if !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.Contains(p, "..") {
				return nil, fmt.Errorf("路径必须是干净的绝对路径 — %q", p)
			}
		}
		if m.Path == "/" {
			return nil, fmt.Errorf("容器路径不能是 / — %q", item)
		}
		out = append(out, m)
		if len(out) > 8 {
			return nil, fmt.Errorf("最多 8 个挂载")
		}
	}
	return out, nil
}

type ISOInfo struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// ISOPullRequest downloads an ISO into the node library from a URL.
type ISOPullRequest struct {
	URL  string `json:"url"`
	Name string `json:"name"` // stored filename, must end in .iso
}

// ISOAttachRequest mounts a library ISO on a VM as a CD-ROM. Boot=true makes
// the VM boot from the ISO first (higher boot.priority).
type ISOAttachRequest struct {
	Name string `json:"name"`
	Boot bool   `json:"boot"`
}

// APIError is the uniform agent error envelope.
type APIError struct {
	Error string `json:"error"`
}
