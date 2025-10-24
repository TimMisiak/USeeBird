package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	maxMessage = 4096
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type hub struct {
	clients    map[*client]struct{}
	register   chan *client
	unregister chan *client
	broadcast  chan []byte
}

func NewHub() *hub {
	return &hub{
		clients:    make(map[*client]struct{}),
		register:   make(chan *client),
		unregister: make(chan *client),
		broadcast:  make(chan []byte, 32),
	}
}

type client struct {
	id   string
	hub  *hub
	conn *websocket.Conn
	send chan []byte
}

type message struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ID         string `json:"id,omitempty"`
	SentAt     string `json:"sentAt,omitempty"`
	ServerTime string `json:"serverTime,omitempty"`
	Sender     string `json:"sender,omitempty"`
}

func (h *hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = struct{}{}
			log.Printf("client %s connected", c.id)
		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				log.Printf("client %s disconnected", c.id)
			}
		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					close(c.send)
					delete(h.clients, c)
				}
			}
		}
	}
}

func serveWebsocket(h *hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}

	c := &client{
		id:   randomID(),
		hub:  h,
		conn: conn,
		send: make(chan []byte, 16),
	}
	h.register <- c

	go c.writePump()

	welcome := message{
		Type:       "system",
		Text:       "connected",
		ID:         randomID(),
		ServerTime: time.Now().UTC().Format(time.RFC3339Nano),
		Sender:     c.id,
	}
	if data, err := json.Marshal(welcome); err == nil {
		c.send <- data
	} else {
		log.Printf("failed to marshal welcome message: %v", err)
	}

	c.readPump()
}

func (c *client) readPump() {
	defer func() {
		c.hub.unregister <- c
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessage)
	if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		log.Printf("set read deadline failed: %v", err)
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("unexpected websocket close: %v", err)
			}
			break
		}

		outgoing := c.prepareBroadcast(payload)
		if len(outgoing) == 0 {
			continue
		}
		c.hub.broadcast <- outgoing
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker((pongWait * 9) / 10)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				log.Printf("set write deadline failed: %v", err)
			}
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Printf("write message failed: %v", err)
				return
			}
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				log.Printf("set write deadline failed: %v", err)
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *client) prepareBroadcast(payload []byte) []byte {
	var msg message
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("invalid message from %s: %v", c.id, err)
		return nil
	}

	if msg.Type == "" {
		return nil
	}

	switch msg.Type {
	case "chat":
		msg.Text = strings.TrimSpace(msg.Text)
		if msg.Text == "" {
			return nil
		}
	case "ping":
	default:
		log.Printf("unknown message type %q from %s", msg.Type, c.id)
		return nil
	}

	if msg.ID == "" {
		msg.ID = randomID()
	}

	msg.Sender = c.id
	msg.ServerTime = time.Now().UTC().Format(time.RFC3339Nano)

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("failed to encode message: %v", err)
		return nil
	}

	return data
}

func randomID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		ts := time.Now().UnixNano()
		return hex.EncodeToString([]byte{byte(ts >> 8), byte(ts)})
	}
	return hex.EncodeToString(buf)
}
