package cdpproxy

import (
	"net/http"

	"github.com/coder/websocket"
)

// SetAccept replaces the WebSocket handshake until the returned function restores it.
func SetAccept(f func(http.ResponseWriter, *http.Request, *websocket.AcceptOptions) (*websocket.Conn, error)) func() {
	old := accept
	accept = f
	return func() { accept = old }
}
