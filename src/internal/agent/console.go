package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"cubpanel/internal/shared"
)

// consoleShell picks bash when the guest has it and falls back to sh, which
// covers both the Debian and Alpine images we offer.
var consoleShell = []string{"/bin/sh", "-c",
	"if command -v bash >/dev/null 2>&1; then exec bash; fi; exec /bin/sh"}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// The only client is the panel, which authenticates by HMAC before the
	// upgrade. Origin is meaningless here, so accept it and rely on the
	// signature instead.
	CheckOrigin: func(*http.Request) bool { return true },
}

// ctlMsg is the JSON control frame exchanged with the panel.
type ctlMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// handleConsole bridges the panel's websocket to an interactive LXD exec.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	// Websocket upgrades carry no body, so the signature covers an empty one.
	if err := shared.Verify(s.cfg.Secret, r, nil); err != nil {
		s.log("console auth reject: %v", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.nonces.use(r.Header.Get(shared.HeaderNonce)) {
		writeErr(w, http.StatusUnauthorized, "replayed request")
		return
	}
	name, ok := instName(w, r)
	if !ok {
		return
	}

	cols, rows := clampDim(r.URL.Query().Get("cols"), 80), clampDim(r.URL.Query().Get("rows"), 24)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	op, err := s.lxd.ExecInteractive(ctx, name, consoleShell, cols, rows)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "exec failed: "+err.Error())
		return
	}

	// Attach to LXD's stdio and control channels before upgrading the client,
	// so a failure here still yields a clean HTTP error.
	dialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return net.DialTimeout("unix", s.lxd.Socket(), 10*time.Second)
		},
		HandshakeTimeout: 15 * time.Second,
	}
	base := "ws://lxd/1.0/operations/" + url.PathEscape(op.ID) + "/websocket?secret="
	term, _, err := dialer.Dial(base+url.QueryEscape(op.Terminal), nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "terminal attach failed")
		return
	}
	defer term.Close()

	var ctl *websocket.Conn
	if op.Control != "" {
		if c, _, err := dialer.Dial(base+url.QueryEscape(op.Control), nil); err == nil {
			ctl = c
			defer ctl.Close()
		}
	}

	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()

	// ctlWrite serialises writes to the LXD control channel.
	var ctlMu sync.Mutex
	resize := func(c, rws int) {
		if ctl == nil {
			return
		}
		ctlMu.Lock()
		defer ctlMu.Unlock()
		_ = ctl.WriteJSON(map[string]any{
			"command": "window-resize",
			"args":    map[string]string{"width": itoa(c), "height": itoa(rws)},
		})
	}

	var once sync.Once
	stop := func() { once.Do(func() { cancel(); client.Close(); term.Close() }) }

	// LXD -> browser
	go func() {
		defer stop()
		for {
			mt, data, err := term.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage || mt == websocket.TextMessage {
				// A zero-length binary frame is LXD signalling EOF.
				if len(data) == 0 {
					return
				}
				if err := client.WriteMessage(websocket.BinaryMessage, data); err != nil {
					return
				}
			}
		}
	}()

	// browser -> LXD
	client.SetReadLimit(1 << 20)
	for {
		mt, data, err := client.ReadMessage()
		if err != nil {
			break
		}
		switch mt {
		case websocket.TextMessage:
			var m ctlMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				resize(clampInt(m.Cols, 1, 500, 80), clampInt(m.Rows, 1, 300, 24))
			}
		case websocket.BinaryMessage:
			if err := term.WriteMessage(websocket.BinaryMessage, data); err != nil {
				stop()
				return
			}
		}
	}
	stop()
}

func clampDim(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' || n > 10000 {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return clampInt(n, 1, 500, def)
}

func clampInt(v, lo, hi, def int) int {
	if v < lo || v > hi {
		return def
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
