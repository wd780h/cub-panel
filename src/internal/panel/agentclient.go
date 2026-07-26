package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"cubpanel/internal/shared"
	"cubpanel/internal/store"
)

// agentHTTP is shared by every plain-HTTP node call; the long timeout covers
// image pulls.
var agentHTTP = &http.Client{Timeout: 20 * time.Minute}

// tlsClients caches one HTTP client per pinned fingerprint so connections
// still pool while each node is verified against its own certificate.
var tlsClients sync.Map // normalized fingerprint -> *http.Client

// normalizeFP lowercases a fingerprint and strips colons/spaces, accepting
// both "ab:cd:…" and bare-hex forms.
func normalizeFP(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
}

// agentTLSConfig accepts the agent's self-signed certificate. Authenticity
// comes from the pinned fingerprint (and every request is HMAC-signed on top
// of TLS); an empty pin means encrypt-only.
func agentTLSConfig(fp string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // self-signed by design; see VerifyPeerCertificate
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if fp == "" {
				return nil
			}
			if len(raw) == 0 {
				return errors.New("agent presented no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != fp {
				return errors.New("agent certificate fingerprint mismatch")
			}
			return nil
		},
	}
}

// agentHTTPClient returns the client appropriate for a node's endpoint.
func agentHTTPClient(node *store.Node) *http.Client {
	if !strings.HasPrefix(node.Endpoint, "https://") {
		return agentHTTP
	}
	fp := normalizeFP(node.CertFP)
	if c, ok := tlsClients.Load(fp); ok {
		return c.(*http.Client)
	}
	c := &http.Client{
		Timeout:   20 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: agentTLSConfig(fp)},
	}
	actual, _ := tlsClients.LoadOrStore(fp, c)
	return actual.(*http.Client)
}

// nonce returns a fresh random request nonce.
func nonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// agentURL validates and joins a node endpoint with a request path.
func agentURL(node *store.Node, path string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(node.Endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("node %s has a malformed endpoint", node.Name)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("node %s endpoint must be http or https", node.Name)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("node %s endpoint has no host", node.Name)
	}
	u := *base
	u.Path = path
	return &u, nil
}

// callAgent performs a signed request against a node and decodes the reply.
func callAgent(ctx context.Context, node *store.Node, method, path string, in, out any) error {
	u, err := agentURL(node, path)
	if err != nil {
		return err
	}

	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	n := nonce()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(shared.HeaderTimestamp, ts)
	req.Header.Set(shared.HeaderNonce, n)
	req.Header.Set(shared.HeaderSignature, shared.Sign(node.Secret, method, path, ts, n, body))

	res, err := agentHTTPClient(node).Do(req)
	if err != nil {
		return fmt.Errorf("node %s unreachable: %w", node.Name, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e shared.APIError
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("node %s: %s", node.Name, e.Error)
		}
		return fmt.Errorf("node %s returned HTTP %d", node.Name, res.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// ---------- typed wrappers ----------

func agentHealth(ctx context.Context, node *store.Node) (*shared.NodeInfo, error) {
	var info shared.NodeInfo
	if err := callAgent(ctx, node, "GET", "/v1/health", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func agentCreate(ctx context.Context, node *store.Node, req *shared.CreateRequest) error {
	return callAgent(ctx, node, "POST", "/v1/instances", req, nil)
}

func agentState(ctx context.Context, node *store.Node, name string) (*shared.InstanceState, error) {
	var st shared.InstanceState
	if err := callAgent(ctx, node, "GET", "/v1/instances/"+name+"/state", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func agentAction(ctx context.Context, node *store.Node, name, action string, force bool) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/action",
		map[string]any{"action": action, "force": force}, nil)
}

func agentPassword(ctx context.Context, node *store.Node, name, password string) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/password",
		map[string]string{"password": password}, nil)
}

func agentResize(ctx context.Context, node *store.Node, name string, cpu, memoryMB, diskGB, rateDown, rateUp int) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/resize",
		shared.ResizeRequest{CPU: cpu, MemoryMB: memoryMB, DiskGB: diskGB,
			RateDownMbps: rateDown, RateUpMbps: rateUp}, nil)
}

func agentDelete(ctx context.Context, node *store.Node, name string) error {
	return callAgent(ctx, node, "DELETE", "/v1/instances/"+name, nil, nil)
}

// ---------- snapshots ----------

func agentSnapshots(ctx context.Context, node *store.Node, name string) ([]shared.SnapshotInfo, error) {
	var out []shared.SnapshotInfo
	if err := callAgent(ctx, node, "GET", "/v1/instances/"+name+"/snapshots", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func agentSnapshotCreate(ctx context.Context, node *store.Node, name, snap string) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/snapshots",
		shared.SnapshotRequest{Snapshot: snap}, nil)
}

func agentSnapshotDelete(ctx context.Context, node *store.Node, name, snap string) error {
	return callAgent(ctx, node, "DELETE", "/v1/instances/"+name+"/snapshots/"+snap, nil, nil)
}

func agentSnapshotRestore(ctx context.Context, node *store.Node, name, snap string) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/snapshots/restore",
		shared.SnapshotRequest{Snapshot: snap}, nil)
}

// ---------- migration (cross-node) ----------

func agentBackupCreate(ctx context.Context, node *store.Node, name string) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/backup", nil, nil)
}

func agentBackupDelete(ctx context.Context, node *store.Node, name string) error {
	return callAgent(ctx, node, "DELETE", "/v1/instances/"+name+"/backup", nil, nil)
}

func agentReconfigure(ctx context.Context, node *store.Node, name string, req *shared.CreateRequest) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+name+"/reconfigure", req, nil)
}

// streamMigrate pipes a backup tarball from the source agent straight into
// the destination agent's import endpoint. The panel is the trusted middle,
// so per-node secrets stay on the master. Both legs use empty-body HMAC (the
// tarball itself is protected by TLS), matching the agent handlers.
func streamMigrate(ctx context.Context, src, dst *store.Node, name string) error {
	getPath := "/v1/instances/" + name + "/backup/export"
	gu, err := agentURL(src, getPath)
	if err != nil {
		return err
	}
	getReq, err := http.NewRequestWithContext(ctx, "GET", gu.String(), nil)
	if err != nil {
		return err
	}
	signEmpty(getReq, src.Secret, "GET", getPath)
	getRes, err := agentHTTPClient(src).Do(getReq)
	if err != nil {
		return fmt.Errorf("export from %s: %w", src.Name, err)
	}
	defer getRes.Body.Close()
	if getRes.StatusCode != http.StatusOK {
		return fmt.Errorf("export from %s: HTTP %d", src.Name, getRes.StatusCode)
	}

	postPath := "/v1/import/" + name
	pu, err := agentURL(dst, postPath)
	if err != nil {
		return err
	}
	postReq, err := http.NewRequestWithContext(ctx, "POST", pu.String(), getRes.Body)
	if err != nil {
		return err
	}
	postReq.Header.Set("Content-Type", "application/octet-stream")
	signEmpty(postReq, dst.Secret, "POST", postPath)
	postRes, err := agentHTTPClient(dst).Do(postReq)
	if err != nil {
		return fmt.Errorf("import to %s: %w", dst.Name, err)
	}
	defer postRes.Body.Close()
	if postRes.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(postRes.Body, 4<<10))
		var e shared.APIError
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("import to %s: %s", dst.Name, e.Error)
		}
		return fmt.Errorf("import to %s: HTTP %d", dst.Name, postRes.StatusCode)
	}
	return nil
}

// signEmpty stamps the empty-body HMAC headers on a streaming request.
func signEmpty(req *http.Request, secret, method, path string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	n := nonce()
	req.Header.Set(shared.HeaderTimestamp, ts)
	req.Header.Set(shared.HeaderNonce, n)
	req.Header.Set(shared.HeaderSignature, shared.Sign(secret, method, path, ts, n, nil))
}

func agentImages(ctx context.Context, node *store.Node) ([]shared.LocalImage, error) {
	var list []shared.LocalImage
	if err := callAgent(ctx, node, "GET", "/v1/images", nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func agentRemoteImages(ctx context.Context, node *store.Node) ([]shared.RemoteImage, error) {
	var list []shared.RemoteImage
	if err := callAgent(ctx, node, "GET", "/v1/images/remote", nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func agentImagePull(ctx context.Context, node *store.Node, alias, imageType string) error {
	return callAgent(ctx, node, "POST", "/v1/images/pull",
		map[string]string{"alias": alias, "type": imageType}, nil)
}

func agentImageDelete(ctx context.Context, node *store.Node, fingerprint string) error {
	return callAgent(ctx, node, "DELETE", "/v1/images/"+fingerprint, nil, nil)
}

// ---------- ISO library ----------

func agentISOs(ctx context.Context, node *store.Node) ([]shared.ISOInfo, error) {
	var out []shared.ISOInfo
	if err := callAgent(ctx, node, "GET", "/v1/isos", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func agentISOPull(ctx context.Context, node *store.Node, url, name string) error {
	return callAgent(ctx, node, "POST", "/v1/isos", shared.ISOPullRequest{URL: url, Name: name}, nil)
}

func agentISODelete(ctx context.Context, node *store.Node, name string) error {
	return callAgent(ctx, node, "DELETE", "/v1/isos/"+name, nil, nil)
}

func agentISOAttach(ctx context.Context, node *store.Node, inst, iso string, boot bool) error {
	return callAgent(ctx, node, "POST", "/v1/instances/"+inst+"/iso",
		shared.ISOAttachRequest{Name: iso, Boot: boot}, nil)
}

func agentISODetach(ctx context.Context, node *store.Node, inst string) error {
	return callAgent(ctx, node, "DELETE", "/v1/instances/"+inst+"/iso", nil, nil)
}

// agentConsole opens a signed websocket to the node's console endpoint.
func agentConsole(ctx context.Context, node *store.Node, name string, cols, rows int) (*websocket.Conn, error) {
	path := "/v1/instances/" + name + "/console"
	u, err := agentURL(node, path)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	q := url.Values{}
	q.Set("cols", strconv.Itoa(cols))
	q.Set("rows", strconv.Itoa(rows))
	u.RawQuery = q.Encode()

	// The signature covers the path only, matching what the agent verifies.
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	n := nonce()
	hdr := http.Header{}
	hdr.Set(shared.HeaderTimestamp, ts)
	hdr.Set(shared.HeaderNonce, n)
	hdr.Set(shared.HeaderSignature, shared.Sign(node.Secret, "GET", path, ts, n, nil))

	dialer := &websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	if u.Scheme == "wss" {
		dialer.TLSClientConfig = agentTLSConfig(normalizeFP(node.CertFP))
	}
	conn, res, err := dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("console attach failed (HTTP %d)", res.StatusCode)
		}
		return nil, errors.New("console attach failed: node unreachable")
	}
	return conn, nil
}
