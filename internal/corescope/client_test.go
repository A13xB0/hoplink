package corescope

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const sampleObs = `{"type":"packet","data":{"id":12,"hash":"3F9C0A6E1B2D7C48","observer_id":"obs-1","observer_name":"Hilltop",` +
	`"path_json":"[\"A1B2C3\",\"d4e5f6\"]","direction":"rx","snr":7.5,"rssi":-98,"observation_count":3}}`

func TestParseMessage_Observation(t *testing.T) {
	obs, ok := ParseMessage([]byte(sampleObs))
	if !ok {
		t.Fatal("expected an observation")
	}
	if obs.Hash != "3f9c0a6e1b2d7c48" || obs.ObserverID != "obs-1" || obs.ObserverName != "Hilltop" {
		t.Errorf("got %+v", obs)
	}
	if strings.Join(obs.Hops, ",") != "a1b2c3,d4e5f6" {
		t.Errorf("Hops = %v, want lowercase [a1b2c3 d4e5f6]", obs.Hops)
	}
	if obs.SNR == nil || *obs.SNR != 7.5 {
		t.Errorf("SNR = %v", obs.SNR)
	}
}

func TestParseMessage_Rejects(t *testing.T) {
	for name, frame := range map[string]string{
		"heartbeat":  `{"type":"heartbeat"}`,
		"other type": `{"type":"message","data":{"hash":"3f9c0a6e1b2d7c48"}}`,
		"no hash":    `{"type":"packet","data":{"observer_id":"x"}}`,
		"short hash": `{"type":"packet","data":{"hash":"abc"}}`,
		"own tx":     `{"type":"packet","data":{"hash":"3f9c0a6e1b2d7c48","direction":"tx"}}`,
		"not json":   `nope`,
	} {
		if _, ok := ParseMessage([]byte(frame)); ok {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseMessage_EmptyOrNullPath(t *testing.T) {
	for _, frame := range []string{
		`{"type":"packet","data":{"hash":"3f9c0a6e1b2d7c48","path_json":"[]"}}`,
		`{"type":"packet","data":{"hash":"3f9c0a6e1b2d7c48","path_json":null}}`,
		`{"type":"packet","data":{"hash":"3f9c0a6e1b2d7c48"}}`,
	} {
		obs, ok := ParseMessage([]byte(frame))
		if !ok || len(obs.Hops) != 0 {
			t.Errorf("%s: got %+v ok=%v, want an observation with no hops", frame, obs, ok)
		}
	}
}

// wsServer serves each connection the given frames, then closes it.
func wsServer(t *testing.T, frames ...string) (url string, conns chan struct{}) {
	t.Helper()
	conns = make(chan struct{}, 16)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		conns <- struct{}{}
		for _, f := range frames {
			_ = c.WriteMessage(websocket.TextMessage, []byte(f))
		}
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", conns
}

func TestRun_DeliversObservationsAndReconnects(t *testing.T) {
	url, conns := wsServer(t, `{"type":"heartbeat"}`, sampleObs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan Observation, 8)
	done := make(chan struct{})
	go func() {
		Run(ctx, url, func(o Observation) { got <- o })
		close(done)
	}()

	select {
	case o := <-got:
		if o.Hash != "3f9c0a6e1b2d7c48" {
			t.Errorf("Hash = %q", o.Hash)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation delivered")
	}
	// The server closes each connection after its frames: Run must redial.
	<-conns
	select {
	case <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not reconnect after the server closed the connection")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

func TestRun_ReturnsOnCancelWhileServerUnreachable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, "ws://127.0.0.1:1/ws", func(Observation) {})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

func TestParseMessage_DropsNonHexHops(t *testing.T) {
	obs, ok := ParseMessage([]byte(`{"type":"packet","data":{"hash":"3f9c0a6e1b2d7c48",` +
		`"path_json":"[\"a1b2c3\",\"<script>\",\"zz\",\"abc\",\"0102030405\",\"\",\"d4e5\"]"}}`))
	if !ok {
		t.Fatal("expected an observation")
	}
	if strings.Join(obs.Hops, ",") != "a1b2c3,d4e5" {
		t.Errorf("Hops = %v, want only the well-formed hex hop hashes", obs.Hops)
	}
}
