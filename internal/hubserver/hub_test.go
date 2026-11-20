package hubserver

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autobot-connector/internal/wire"

	"github.com/gorilla/websocket"
)

const image = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type testHub struct {
	*Hub
	server *httptest.Server
	dialer websocket.Dialer
}

func newTestHub(t *testing.T) testHub {
	t.Helper()
	h, err := New(t.TempDir(), image)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewTLSServer(h.WebSocket())
	t.Cleanup(s.Close)
	return testHub{h, s, websocket.Dialer{TLSClientConfig: s.Client().Transport.(*http.Transport).TLSClientConfig}}
}

func (h testHub) dial() (*websocket.Conn, *http.Response, error) {
	return h.dialer.Dial("wss"+strings.TrimPrefix(h.server.URL, "https")+"/donor", nil)
}

func (h testHub) auth(t *testing.T, key ed25519.PrivateKey, code, expected string) (*wire.Socket, string) {
	t.Helper()
	ws, _, err := h.dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	socket := wire.Open(ws)
	var challenge wire.Challenge
	m, err := socket.Read()
	if err != nil || m.Type != "challenge" || m.Decode(&challenge) != nil {
		t.Fatalf("challenge: %v %v", m, err)
	}
	a := wire.Auth{Key: key.Public().(ed25519.PublicKey), Code: code, Signature: ed25519.Sign(key, wire.AuthBytes(challenge.Nonce, code))}
	if err := socket.Send("auth", "", a); err != nil {
		t.Fatal(err)
	}
	m, err = socket.Read()
	if err != nil || m.Type != expected {
		t.Fatalf("auth = %v, %v; want %s", m, err, expected)
	}
	var enrolled wire.Enrolled
	if expected == "enrolled" {
		if err := m.Decode(&enrolled); err != nil {
			t.Fatal(err)
		}
	} else {
		var why wire.Reason
		if err := m.Decode(&why); err != nil {
			t.Fatal(err)
		}
		return socket, why.Reason
	}
	return socket, enrolled.Donor
}

func key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	for stop := time.Now().Add(time.Second); time.Now().Before(stop); time.Sleep(time.Millisecond) {
		if fn() {
			return
		}
	}
	t.Fatal("timed out waiting for session state")
}

func (h testHub) readySession(t *testing.T, socket *wire.Socket, donor string, sample wire.Health) *session {
	t.Helper()
	if err := socket.Send("heartbeat", "", wire.Status{Ready: true, Health: sample, Arch: "arm64", Image: image, Scratch: wire.ScratchCap}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	s := h.sessions[donor]
	h.mu.Unlock()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.status.Ready })
	return s
}

func TestEnrollment(t *testing.T) {
	h := newTestHub(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	h.now = func() time.Time { return time.Unix(0, clock.Load()) }
	secret, err := h.code()
	if err != nil {
		t.Fatal(err)
	}
	k := key(t)
	s, id := h.auth(t, k, secret, "enrolled")
	_, reason := h.auth(t, key(t), secret, "denied")
	if reason != "enrollment code refused" {
		t.Fatal(reason)
	}
	t.Log("single-use code refused on second enrollment")
	s.Close()
	eventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.sessions[id] == nil })
	again, reconnected := h.auth(t, k, "", "enrolled")
	if id != reconnected {
		t.Fatal("donor ID changed")
	}
	t.Log("same donor reconnected with a newly signed challenge")
	server := h.readySession(t, again, id, wire.Health{At: h.now().UTC()})
	job, err := server.begin(wire.Manifest{}, time.Minute, wire.ScratchCap)
	if err != nil {
		t.Fatal(err)
	}
	if offer, err := again.Read(); err != nil || offer.Type != "offer" {
		t.Fatalf("offer: %v %v", offer, err)
	}
	if err := h.revoke(id); err != nil {
		t.Fatal(err)
	}
	if result := job.result; result.Verified || result.Facts.End != "lost" || result.Error != "donor revoked" {
		t.Fatalf("revoked lease result: %+v", result)
	}
	if _, err := again.Read(); err == nil {
		t.Fatal("revocation left socket open")
	}
	_, reason = h.auth(t, k, "", "denied")
	if reason != "donor key refused" {
		t.Fatal(reason)
	}
	t.Logf("revoked lease recorded lost and unverified with error %q; key refused", job.result.Error)
	expiring, err := h.code()
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(time.Hour))
	_, reason = h.auth(t, key(t), expiring, "denied")
	if reason != "enrollment code refused" {
		t.Fatal(reason)
	}
	t.Log("code refused at one-hour expiry")
	reloaded, err := New(h.dir, image)
	if err != nil || !reloaded.state.Donors[id].Revoked ||
		!reloaded.state.Codes[keyID([]byte(secret))].IsZero() {
		t.Fatalf("registry persistence: %v", err)
	}
}

func TestProtocolRefusals(t *testing.T) {
	for _, name := range []string{"out-of-lease", "oversized", "malformed", "replayed", "reordered"} {
		t.Run(name, func(t *testing.T) {
			h := newTestHub(t)
			secret, err := h.code()
			if err != nil {
				t.Fatal(err)
			}
			s, _ := h.auth(t, key(t), secret, "enrolled")
			switch name {
			case "out-of-lease":
				err = s.Send("exit", wire.ID(), wire.Exit{Number: 1})
			case "oversized":
				err = s.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte{'a'}, wire.FrameCap+1))
			case "malformed":
				err = s.WriteMessage(websocket.TextMessage, []byte(`{"seq":2,"seq":2,"type":"heartbeat","body":{}}`))
			case "replayed", "reordered":
				seq := uint64(1)
				if name == "reordered" {
					seq = 3
				}
				data, _ := json.Marshal(wire.Message{Seq: seq, Type: "heartbeat", Body: json.RawMessage("{}")})
				err = s.WriteMessage(websocket.TextMessage, data)
			}
			if err != nil {
				t.Fatal(err)
			}
			s.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := s.Read(); err == nil {
				t.Fatal("invalid message was accepted")
			} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
				t.Fatal("hub did not reject the message before timeout")
			} else {
				t.Logf("%s frame refused: %v", name, err)
			}
		})
	}
}

func TestConnectionFlood(t *testing.T) {
	h := newTestHub(t)
	var admitted, limited int
	for range 30 {
		ws, r, err := h.dial()
		if err == nil {
			admitted++
			ws.Close()
		} else if r != nil && r.StatusCode == http.StatusTooManyRequests {
			limited++
			r.Body.Close()
		} else {
			t.Fatalf("dial: %v", err)
		}
	}
	if admitted != 12 || limited != 18 {
		t.Fatalf("admitted %d limited %d", admitted, limited)
	}
	t.Logf("30 attempts: %d admitted, %d HTTP 429", admitted, limited)
}

func TestTranscript(t *testing.T) {
	for _, name := range []string{"valid", "tampered", "replayed", "stale", "reordered", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			h := newTestHub(t)
			secret, err := h.code()
			if err != nil {
				t.Fatal(err)
			}
			k := key(t)
			s, donor := h.auth(t, k, secret, "enrolled")
			sample := wire.Health{At: time.Now().UTC()}
			server := h.readySession(t, s, donor, sample)
			seq := uint64(2) // Authentication and the readiness heartbeat precede the lease.
			var frames [][]byte
			send := func(kind, lease string, v any) {
				t.Helper()
				body, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				seq++
				frame, err := json.Marshal(wire.Message{Seq: seq, Type: kind, Lease: lease, Body: body})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.WriteMessage(websocket.TextMessage, frame); err != nil {
					t.Fatal(err)
				}
				frames = append(frames, frame)
			}
			read := func(kind string) wire.Message {
				t.Helper()
				m, err := s.Read()
				if err != nil || m.Type != kind {
					t.Fatalf("want %s, received %s (%v)", kind, m.Type, err)
				}
				return m
			}
			job, err := server.begin(wire.Manifest{}, time.Minute, wire.ScratchCap)
			if err != nil {
				t.Fatal(err)
			}
			record := wire.NewRecord()
			var offer wire.Offer
			if err := read("offer").Decode(&offer); err != nil {
				t.Fatal(err)
			}
			record.Add(true, "offer", offer)
			begun := wire.Started{Bundle: image, Health: sample}
			record.Add(false, "started", begun)
			send("started", job.id, begun)
			select {
			case err := <-job.start:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("start timed out")
			}
			events, _, err := server.command(job.id, []string{"echo", "two chunks"})
			if err != nil {
				t.Fatal(err)
			}
			var command wire.Command
			if err := read("exec").Decode(&command); err != nil {
				t.Fatal(err)
			}
			record.Add(true, "exec", command)
			first := wire.Chunk{Number: 1, Stream: "stdout", Data: []byte("first")}
			second := wire.Chunk{Number: 1, Stream: "stdout", Data: []byte("second")}
			send("output", job.id, first)
			send("output", job.id, second)
			if name == "reordered" {
				record.Add(false, "output", second)
			}
			record.Add(false, "output", first)
			if name != "reordered" && name != "incomplete" {
				record.Add(false, "output", second)
			}
			code := 0
			exit := wire.Exit{Number: 1, Fact: wire.CommandFact{ExitCode: &code}}
			record.Add(false, "exit", exit)
			send("exit", job.id, exit)
			for range events {
			}
			if _, err := server.end(job.id, "finish"); err != nil {
				t.Fatal(err)
			}
			read("finish")
			record.Add(true, "finish", struct{}{})
			reason := wire.Reason{Reason: "complete"}
			record.Add(false, "ending", reason)
			send("ending", job.id, reason)
			read("seal")
			record.Add(true, "seal", struct{}{})
			facts := wire.Facts{End: "complete", RunnerEnd: "complete", Ready: true, Health: sample, Commands: []wire.CommandFact{exit.Fact}}
			if name == "stale" {
				facts.Health.At = offer.Issued.Add(-time.Minute)
			}
			done := record.Sign(k, donor, job.id, facts)
			if name == "tampered" {
				done.Facts.Ready = false
			}
			send("done", job.id, done)
			select {
			case <-job.done:
			case <-time.After(time.Second):
				t.Fatal("transcript response timed out")
			}
			valid := name == "valid" || name == "replayed"
			if job.result.Verified != valid || (!valid && !strings.Contains(job.result.Error, "transcript refused")) {
				t.Fatalf("unexpected transcript result: %+v", job.result)
			}
			if name == "replayed" {
				h.refuseTranscriptReplay(t, s, k, donor, job, frames)
				return
			}
			t.Logf("%s transcript: verified=%t result=%q", name, job.result.Verified, job.result.Error)
		})
	}
}

func (h testHub) refuseTranscriptReplay(t *testing.T, socket *wire.Socket, k ed25519.PrivateKey, donor string, original *lease, frames [][]byte) {
	t.Helper()
	replay := func(s *wire.Socket) {
		t.Helper()
		for i, frame := range frames {
			if err := s.WriteMessage(websocket.TextMessage, frame); err != nil {
				if i == 0 {
					t.Fatalf("could not send first replay frame: %v", err)
				}
				break // The hub can close the socket as soon as it sees the first frame.
			}
		}
		s.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := s.Read(); err == nil {
			t.Fatal("replay left the connection open")
		} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
			t.Fatal("hub did not reject the replay before timeout")
		}
	}
	replay(socket)
	t.Logf("original verified=%t; exact replay of %d lease frames refused on the same socket", original.result.Verified, len(frames))
	eventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.sessions[donor] == nil })
	again, id := h.auth(t, k, "", "enrolled")
	if id != donor {
		t.Fatal("donor ID changed after reconnect")
	}
	server := h.readySession(t, again, id, wire.Health{At: time.Now().UTC()})
	next, err := server.begin(wire.Manifest{}, time.Minute, wire.ScratchCap)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := again.Read(); err != nil || m.Type != "offer" {
		t.Fatalf("new offer: %v %v", m, err)
	}
	if next.id == original.id || next.offer.Nonce == original.offer.Nonce {
		t.Fatal("hub reissued the completed lease ID or nonce")
	}
	// Sequence numbers restart after reconnect, so these bytes now have valid frame numbers.
	replay(again)
	select {
	case <-next.done:
	case <-time.After(time.Second):
		t.Fatal("hub did not end the new lease after replay")
	}
	if next.result.Verified || next.result.Facts.End != "lost" || !strings.Contains(next.result.Error, "out-of-lease") {
		t.Fatalf("unexpected result for replay into a fresh lease: %+v", next.result)
	}
	t.Logf("same transcript replayed after reconnect: verified=%t, end=%s, error=%q", next.result.Verified, next.result.Facts.End, next.result.Error)
}
