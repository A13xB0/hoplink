// Package corescope consumes the live packet feed of a CoreScope server
// (https://github.com/Kpa-clawbot/CoreScope) — a MeshCore packet analyzer
// fed by MQTT "observer" nodes spread across a region. Each time any
// observer hears a packet, CoreScope broadcasts that observation over its
// /ws WebSocket. hoplink uses these as remote evidence that one of its own
// transmissions propagated: observers hear what our own radio never can.
package corescope

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func logf(format string, args ...any) {
	log.Printf("[corescope] "+format, args...)
}

// Observation is one observer's reception of one packet.
type Observation struct {
	// Hash is the packet's MeshCore packet hash as 16 lowercase hex
	// characters — the same value as meshcore.PacketHash.String().
	Hash         string
	ObserverID   string
	ObserverName string
	// Hops are the repeater hop hashes (lowercase hex) in the path of the
	// copy this observer heard, in the order they were appended.
	Hops []string
	SNR  *float64
	RSSI *float64
}

// wsMessage mirrors CoreScope's broadcast envelope (cmd/server WSMessage)
// and the subset of its packet observation fields hoplink uses.
type wsMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type wsPacket struct {
	Hash         *string  `json:"hash"`
	ObserverID   *string  `json:"observer_id"`
	ObserverName *string  `json:"observer_name"`
	PathJSON     *string  `json:"path_json"` // a JSON-encoded array of hop hex strings
	Direction    *string  `json:"direction"`
	SNR          *float64 `json:"snr"`
	RSSI         *float64 `json:"rssi"`
}

// ParseMessage decodes one WebSocket frame. ok is false for anything that
// isn't a received-packet observation (heartbeats, other message types, an
// observer reporting its own transmission, malformed data).
func ParseMessage(frame []byte) (obs Observation, ok bool) {
	var msg wsMessage
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Type != "packet" {
		return Observation{}, false
	}
	var p wsPacket
	if err := json.Unmarshal(msg.Data, &p); err != nil || p.Hash == nil || len(*p.Hash) != 16 {
		return Observation{}, false
	}
	if p.Direction != nil && strings.EqualFold(*p.Direction, "tx") {
		return Observation{}, false // an observer's own transmission proves nothing about propagation
	}
	obs = Observation{Hash: strings.ToLower(*p.Hash), SNR: p.SNR, RSSI: p.RSSI}
	if p.ObserverID != nil {
		obs.ObserverID = *p.ObserverID
	}
	if p.ObserverName != nil {
		obs.ObserverName = *p.ObserverName
	}
	if p.PathJSON != nil && *p.PathJSON != "" {
		var hops []string
		if err := json.Unmarshal([]byte(*p.PathJSON), &hops); err == nil {
			for _, h := range hops {
				if validHop(h) {
					obs.Hops = append(obs.Hops, strings.ToLower(h))
				}
			}
		}
	}
	return obs, true
}

// validHop reports whether h looks like a MeshCore hop hash: 1-4 bytes of
// hex. The feed is remote input; anything else is dropped rather than fed
// into delivery records and logs.
func validHop(h string) bool {
	if len(h) < 2 || len(h) > 8 || len(h)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// maxFrameBytes caps one WebSocket frame. CoreScope observations are a few
// KB at most; this just keeps a misbehaving server from exhausting memory.
const maxFrameBytes = 1 << 20

// readTimeout bounds how long a connection may stay silent: CoreScope sends
// a heartbeat every 30s, so missing three means the connection is dead.
// A var so tests can shorten it.
var readTimeout = 90 * time.Second

const maxBackoff = 60 * time.Second

// Run connects to url (CoreScope's ws:// or wss:// .../ws endpoint) and
// calls handle for every observation until ctx is cancelled, reconnecting
// with exponential backoff whenever the connection fails or drops. handle
// runs on Run's goroutine and must not block for long.
func Run(ctx context.Context, url string, handle func(Observation)) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := runOnce(ctx, url, handle)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		logf("feed %s: %v (reconnecting in %s)", url, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runOnce(ctx context.Context, url string, handle func(Observation)) (connected bool, err error) {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("dial: %w (HTTP %d)", err, resp.StatusCode)
		}
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrameBytes)
	logf("connected to live feed %s", url)

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return true, fmt.Errorf("read: %w", err)
		}
		if obs, ok := ParseMessage(frame); ok {
			handle(obs)
		}
	}
}
