package api

// Dependency pin (P0 scaffold): the live cockpit WebSocket endpoints port
// the Python reference's FastAPI websockets using coder/websocket. The
// blank import keeps the module pinned in go.mod until those handlers land,
// avoiding dependency races across parallel build phases.
import (
	_ "github.com/coder/websocket"
)
