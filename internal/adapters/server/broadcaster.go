package server

import (
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

// LogBroadcaster manages WebSocket clients and broadcasts log messages.
type LogBroadcaster struct {
	clients   map[*websocket.Conn]bool
	broadcast chan []byte
	mutex     sync.Mutex
}

// NewLogBroadcaster creates a new LogBroadcaster.
func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan []byte, 256), // Buffer to prevent blocking
	}
}

// Start begins the broadcasting loop. Run this in a goroutine.
func (b *LogBroadcaster) Start() {
	for message := range b.broadcast {
		b.mutex.Lock()
		for client := range b.clients {
			if err := client.WriteMessage(websocket.TextMessage, message); err != nil {
				client.Close()
				delete(b.clients, client)
			}
		}
		b.mutex.Unlock()
	}
}

// HandleWebsocket handles incoming WebSocket requests.
func (b *LogBroadcaster) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	b.mutex.Lock()
	b.clients[conn] = true
	b.mutex.Unlock()

	// Keep connection open and handle close
	go func() {
		defer func() {
			b.mutex.Lock()
			delete(b.clients, conn)
			b.mutex.Unlock()
			conn.Close()
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}()
}

// Broadcast sends a message to all connected clients.
// It accepts a byte slice (e.g., JSON log entry).
func (b *LogBroadcaster) Write(p []byte) (n int, err error) {
	// We need to copy the slice because the underlying array might be modified
	// or reused by the logger buffer before the channel consumes it.
	msg := make([]byte, len(p))
	copy(msg, p)
	
	select {
	case b.broadcast <- msg:
	default:
		// Drop message if buffer is full to avoid blocking the application
	}
	return len(p), nil
}
