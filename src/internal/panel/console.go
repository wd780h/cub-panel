package panel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// browserUpgrader accepts the tenant's terminal websocket. Unlike the agent
// side, this one enforces the origin check: the request carries the session
// cookie, so a foreign page must not be able to open it.
var browserUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return false
		}
		h, err := parseURLHost(origin)
		return err == nil && strings.EqualFold(h, r.Host)
	},
}

// handleConsoleWS bridges the tenant's browser to the node's console socket.
func (s *Server) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	if inst.Status == "expired" {
		s.jsonErr(w, http.StatusForbidden, "实例已到期")
		return
	}

	cols := formInt(r, "cols", 80, 20, 500)
	rows := formInt(r, "rows", 24, 5, 300)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Dial the node first so a failure is still a clean HTTP error.
	dialCtx, dialCancel := context.WithTimeout(ctx, 25*time.Second)
	up, err := agentConsole(dialCtx, node, inst.Name, cols, rows)
	dialCancel()
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer up.Close()

	down, err := browserUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer down.Close()

	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "instance.console", inst.Name, clientIP(r))

	var once sync.Once
	stop := func() { once.Do(func() { cancel(); down.Close(); up.Close() }) }

	// node -> browser
	go func() {
		defer stop()
		for {
			mt, data, err := up.ReadMessage()
			if err != nil {
				return
			}
			if err := down.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}()

	// browser -> node. The read limit bounds a hostile client's memory use.
	down.SetReadLimit(1 << 20)
	for {
		mt, data, err := down.ReadMessage()
		if err != nil {
			break
		}
		if err := up.WriteMessage(mt, data); err != nil {
			break
		}
	}
	stop()
}
