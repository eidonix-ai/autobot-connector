package hubserver

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"autobot-connector/internal/wire"

	"github.com/gorilla/websocket"
)

type donor struct {
	Key     []byte `json:"key"`
	Revoked bool   `json:"revoked"`
}

type registry struct {
	Codes  map[string]time.Time
	Donors map[string]donor
}

type Hub struct {
	mu                  sync.Mutex
	state               registry
	dir                 string
	images              map[string]string
	now                 func() time.Time
	sessions            map[string]*session
	byIP                map[string]int
	window              time.Time
	attempts, connected int
}

func workImages(pins string) (map[string]string, error) {
	images := map[string]string{}
	for _, pin := range strings.Split(pins, ",") {
		arch, digest, ok := strings.Cut(pin, "=")
		if !ok || (arch != "arm64" && arch != "amd64") || !wire.Digest(digest) || images[arch] != "" {
			return nil, errors.New("invalid work image pin")
		}
		images[arch] = digest
	}
	return images, nil
}

func New(dir, pins string) (*Hub, error) {
	images, err := workImages(pins)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "leases"), 0700); err != nil {
		return nil, err
	}
	h := &Hub{dir: dir, images: images, now: time.Now, sessions: map[string]*session{}, byIP: map[string]int{},
		state: registry{map[string]time.Time{}, map[string]donor{}}}
	b, err := os.ReadFile(filepath.Join(dir, "registry.json"))
	if err == nil {
		if err = wire.Strict(b, &h.state); err != nil {
			return nil, err
		}
		if h.state.Codes == nil || h.state.Donors == nil {
			return nil, errors.New("invalid donor registry")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return h, nil
}

func keyID(key []byte) string { return fmt.Sprintf("%x", sha256.Sum256(key)) }

func (h *Hub) save() error {
	tmp, err := os.CreateTemp(h.dir, ".registry-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = errors.Join(json.NewEncoder(tmp).Encode(h.state), tmp.Sync(), tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(h.dir, "registry.json"))
}

func (h *Hub) code() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for id, expiry := range h.state.Codes {
		if !now.Before(expiry) {
			delete(h.state.Codes, id)
		}
	}
	if len(h.state.Codes) >= 256 {
		return "", errors.New("too many outstanding codes")
	}
	code := wire.ID()
	id := keyID([]byte(code))
	h.state.Codes[id] = now.Add(time.Hour)
	if err := h.save(); err != nil {
		delete(h.state.Codes, id)
		return "", err
	}
	return code, nil
}

func (h *Hub) authenticate(a wire.Auth, nonce string, conn *wire.Socket) (*session, error) {
	if len(a.Key) != ed25519.PublicKeySize || (a.Code != "" && !wire.Hex(a.Code, 32)) ||
		!ed25519.Verify(ed25519.PublicKey(a.Key), wire.AuthBytes(nonce, a.Code), a.Signature) {
		return nil, errors.New("invalid authentication")
	}
	id := keyID(a.Key)
	h.mu.Lock()
	defer h.mu.Unlock()
	current, exists := h.state.Donors[id]
	if a.Code == "" {
		if !exists || current.Revoked {
			return nil, errors.New("donor key refused")
		}
	} else {
		k := keyID([]byte(a.Code))
		expires, found := h.state.Codes[k]
		if exists || !found || !h.now().Before(expires) || len(h.state.Donors) >= 4096 {
			return nil, errors.New("enrollment code refused")
		}
		delete(h.state.Codes, k)
		h.state.Donors[id] = donor{Key: a.Key}
		if err := h.save(); err != nil {
			h.state.Codes[k] = expires
			delete(h.state.Donors, id)
			return nil, err
		}
	}
	if h.sessions[id] != nil {
		return nil, errors.New("donor already connected")
	}
	s := &session{id: id, key: a.Key, conn: conn, hub: h, closed: make(chan struct{})}
	h.sessions[id] = s
	return s, nil
}

func (h *Hub) revoke(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.state.Donors[id]
	if !ok {
		return errors.New("donor not found")
	}
	if d.Revoked {
		return nil
	}
	d.Revoked = true
	h.state.Donors[id] = d
	if err := h.save(); err != nil {
		d.Revoked = false
		h.state.Donors[id] = d
		return err
	}
	if s := h.sessions[id]; s != nil {
		s.disconnect("donor revoked")
	}
	return nil
}

func (h *Hub) admit(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.window.IsZero() || now.Sub(h.window) >= 10*time.Second {
		h.window, h.attempts, h.byIP = now, 0, map[string]int{}
	}
	if h.connected >= 128 || h.attempts >= 64 || h.byIP[ip] >= 12 {
		return false
	}
	h.connected++
	h.attempts++
	h.byIP[ip]++
	return true
}

func (h *Hub) WebSocket() http.Handler {
	mux := http.NewServeMux()
	upgrader := websocket.Upgrader{HandshakeTimeout: 3 * time.Second, ReadBufferSize: 4096, WriteBufferSize: 4096}
	mux.HandleFunc("GET /donor", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.Header.Get("Origin") != "" || r.URL.RawQuery != "" {
			http.Error(w, "TLS donor connection required", http.StatusBadRequest)
			return
		}
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !h.admit(ip) {
			http.Error(w, "connection rate limit", http.StatusTooManyRequests)
			return
		}
		defer func() { h.mu.Lock(); h.connected--; h.mu.Unlock() }()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		socket := wire.Open(conn)
		nonce := wire.ID()
		if socket.Send("challenge", "", wire.Challenge{Nonce: nonce}) != nil {
			return
		}
		message, err := socket.Read()
		var auth wire.Auth
		if err != nil || message.Type != "auth" || message.Lease != "" || message.Decode(&auth) != nil {
			return
		}
		s, err := h.authenticate(auth, nonce, socket)
		if err != nil {
			socket.Send("denied", "", wire.Reason{Reason: err.Error()})
			return
		}
		defer func() {
			s.disconnect("donor disconnected")
			h.mu.Lock()
			delete(h.sessions, s.id)
			h.mu.Unlock()
		}()
		if socket.Send("enrolled", "", wire.Enrolled{Donor: s.id}) != nil {
			return
		}
		s.run()
	})
	return mux
}
