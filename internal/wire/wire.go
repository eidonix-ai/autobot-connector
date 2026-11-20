package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	FrameCap     = 64 << 10
	ChunkCap     = 32 << 10
	OutputCap    = 8 << 20
	SalvageCap   = 1 << 20
	BundleCap    = 96 << 20
	ScratchCap   = 128 << 20
	MaxScratch   = 16 << 30
	FileCap      = 7500
	OperationCap = 32
	Heartbeat    = 2 * time.Second
	Silence      = 3 * Heartbeat
	MaxLease     = 2 * time.Hour
)

type Source struct {
	URL    string `json:"url"`
	Commit string `json:"commit"`
}

type File struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	GPUs    int      `json:"gpus,omitempty"`
	Source  *Source  `json:"source,omitempty"`
	Files   []File   `json:"files,omitempty"`
	Salvage []string `json:"salvage,omitempty"`
	// Bundle, when set, pins the assembled tar in addition to the input pins.
	Bundle string `json:"bundle,omitempty"`
}

func Path(s string) bool {
	return s != "." && len(s) <= 255 && fs.ValidPath(s) && !strings.ContainsRune(s, 0)
}

func Hex(s string, size int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == size && hex.EncodeToString(b) == s
}

func Digest(s string) bool { return len(s) == 71 && s[:7] == "sha256:" && Hex(s[7:], 32) }

func ID() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (m Manifest) Valid() bool {
	if !ValidGPUCount(m.GPUs) || len(m.Files) > 64 || len(m.Salvage) > 16 || (m.Bundle != "" && !Digest(m.Bundle)) {
		return false
	}
	if m.Source != nil && (!Hex(m.Source.Commit, 20) || len(m.Source.URL) > 2048) {
		return false
	}
	paths := map[string]bool{}
	for _, f := range m.Files {
		if !Path(f.Path) || !Hex(f.SHA256, 32) || len(f.URL) > 2048 || paths[f.Path] {
			return false
		}
		paths[f.Path] = true
	}
	for _, p := range m.Salvage {
		if !Path(p) {
			return false
		}
	}
	b, err := json.Marshal(m)
	return err == nil && len(b) <= FrameCap/2
}

type Offer struct {
	Nonce          string    `json:"nonce"`
	Head           string    `json:"head,omitempty"`
	Image          string    `json:"image"`
	ManifestDigest string    `json:"manifest_digest"`
	Manifest       Manifest  `json:"manifest"`
	Deadline       time.Time `json:"deadline"`
	Issued         time.Time `json:"issued"`
	Scratch        int64     `json:"scratch"`
}

func ManifestDigest(m Manifest) string {
	b, _ := json.Marshal(m)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

func (o Offer) Valid(now time.Time) bool {
	head := ""
	if o.Manifest.Source != nil {
		head = o.Manifest.Source.Commit
	}
	return Hex(o.Nonce, 32) && o.Head == head && Digest(o.Image) && o.Manifest.Valid() &&
		o.ManifestDigest == ManifestDigest(o.Manifest) && o.Deadline.After(now) &&
		!o.Deadline.After(o.Issued.Add(MaxLease)) && !o.Issued.Before(now.Add(-2*Silence)) && !o.Issued.After(now.Add(Silence)) && o.Scratch > 0 && o.Scratch <= MaxScratch
}

type Command struct {
	Number int      `json:"number"`
	Argv   []string `json:"argv"`
}

func (c Command) Valid() bool {
	if c.Number < 1 || c.Number > OperationCap || len(c.Argv) == 0 || len(c.Argv) > 128 || c.Argv[0] == "" {
		return false
	}
	size := 0
	for _, a := range c.Argv {
		if bytes.ContainsRune([]byte(a), 0) {
			return false
		}
		size += len(a)
	}
	return size <= 8192
}

type GPU struct {
	UUID          string  `json:"uuid,omitempty"`
	MemoryUsedMiB *int    `json:"memory_used_mib,omitempty"`
	Name          string  `json:"name"`
	SM            string  `json:"sm"`
	Temperature   *int    `json:"temperature"`
	PowerMW       *int    `json:"power_mw"`
	PowerLimitMW  *int    `json:"power_limit_mw"`
	GraphicsMHz   *int    `json:"graphics_mhz"`
	MemoryMHz     *int    `json:"memory_mhz"`
	Throttle      *uint64 `json:"throttle"`
	Fan           *int    `json:"fan"`
	ECC           *uint64 `json:"ecc"`
	XIDCount      *uint64 `json:"xid_count"`
	LastXID       *uint64 `json:"last_xid"`
	MemoryMiB     *int    `json:"memory_mib"`
}

type Health struct {
	At           time.Time `json:"at"`
	TotalKiB     int64     `json:"total_kib"`
	AvailableKiB int64     `json:"available_kib"`
	GPUs         []GPU     `json:"gpus"`
	Reasons      []string  `json:"reasons,omitempty"`
}

type Status struct {
	FreeGPUs *int   `json:"free_gpus,omitempty"`
	Health   Health `json:"health"`
	Paused   bool   `json:"paused"`
	Ready    bool   `json:"ready"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	CPUQuota int    `json:"cpu_quota"`
	Memory   int64  `json:"memory"`
	PIDs     int    `json:"pids"`
	Image    string `json:"image"`
	Scratch  int64  `json:"scratch"`
}

type Started struct {
	Devices []string `json:"devices,omitempty"`
	Bundle  string   `json:"bundle"`
	Health  Health   `json:"health"`
}

type Chunk struct {
	Number int    `json:"number"`
	Stream string `json:"stream"`
	Data   []byte `json:"data"`
}

type CommandFact struct {
	ExitCode *int `json:"exit_code"`
	Signal   *int `json:"signal,omitempty"`
}

type Exit struct {
	Number int         `json:"number"`
	Fact   CommandFact `json:"fact"`
	Error  string      `json:"error,omitempty"`
}

type Reason struct {
	Reason string `json:"reason"`
}

type Facts struct {
	Devices      []string      `json:"devices,omitempty"`
	End          string        `json:"end"`
	RunnerEnd    string        `json:"runner_end,omitempty"`
	Commands     []CommandFact `json:"commands,omitempty"`
	OOMBefore    *int64        `json:"oom_before"`
	OOMAfter     *int64        `json:"oom_after"`
	OOMKilled    *bool         `json:"oom_killed"`
	Ready        bool          `json:"ready"`
	SalvageError string        `json:"salvage_error,omitempty"`
	Health       Health        `json:"health"`
}

type Done struct {
	Facts     Facts  `json:"facts"`
	Signature []byte `json:"signature"`
}

type Auth struct {
	Key       []byte `json:"key"`
	Code      string `json:"code,omitempty"`
	Signature []byte `json:"signature"`
}

type Challenge struct {
	Nonce string `json:"nonce"`
}

type Enrolled struct {
	Donor string `json:"donor"`
}

func AuthBytes(nonce, code string) []byte { return []byte("donor/auth/v1\x00" + nonce + "\x00" + code) }

type Message struct {
	Seq   uint64          `json:"seq"`
	Type  string          `json:"type"`
	Lease string          `json:"lease,omitempty"`
	Body  json.RawMessage `json:"body"`
}

func Strict(b []byte, dest any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func canonical(b []byte, dest any) error {
	if err := Strict(b, dest); err != nil {
		return err
	}
	enc, err := json.Marshal(dest)
	if err != nil || !bytes.Equal(b, enc) {
		return errors.New("noncanonical JSON")
	}
	return nil
}

func (m Message) Decode(dest any) error { return canonical(m.Body, dest) }

type Socket struct {
	*websocket.Conn
	mu      sync.Mutex
	in, out uint64
}

func Open(conn *websocket.Conn) *Socket {
	conn.SetReadLimit(FrameCap)
	conn.EnableWriteCompression(false)
	// Application heartbeats drive the deadline. Ignore unsolicited control pings.
	conn.SetPingHandler(func(string) error { return nil })
	s := &Socket{Conn: conn}
	s.Extend()
	return s
}

func (s *Socket) Extend() { s.SetReadDeadline(time.Now().Add(Silence)) }

func (s *Socket) Read() (Message, error) {
	kind, data, err := s.ReadMessage()
	if err != nil {
		return Message{}, err
	}
	var m Message
	if kind != websocket.TextMessage || canonical(data, &m) != nil || m.Seq != s.in+1 || m.Type == "" || (m.Lease != "" && !Hex(m.Lease, 32)) {
		return Message{}, errors.New("invalid, replayed or reordered frame")
	}
	s.in++
	return m, nil
}

func (s *Socket) Send(kind, lease string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := json.Marshal(Message{s.out + 1, kind, lease, b})
	if err != nil {
		return err
	}
	if len(m) > FrameCap {
		return errors.New("frame exceeds cap")
	}
	s.out++
	s.SetWriteDeadline(time.Now().Add(time.Second))
	return s.WriteMessage(websocket.TextMessage, m)
}

// Hash both directions in wire order. Output and exit events identify their command,
// so concurrent cancellation has a unique encoding independent of network timing.
type Record struct{ sent, received hash.Hash }

func NewRecord() *Record { return &Record{sha256.New(), sha256.New()} }

func (r *Record) Add(fromHub bool, kind string, v any) {
	h := r.received
	if fromHub {
		h = r.sent
	}
	json.NewEncoder(h).Encode(struct {
		Kind  string `json:"kind"`
		Value any    `json:"value"`
	}{kind, v})
}

func (r *Record) bytes(donor, lease string, facts Facts) []byte {
	b, _ := json.Marshal(struct {
		Domain, Donor, Lease, Sent, Received string
		Facts                                Facts
	}{"donor/transcript/v1", donor, lease, hex.EncodeToString(r.sent.Sum(nil)), hex.EncodeToString(r.received.Sum(nil)), facts})
	return b
}

func (r *Record) Sign(key ed25519.PrivateKey, donor, lease string, facts Facts) Done {
	return Done{facts, ed25519.Sign(key, r.bytes(donor, lease, facts))}
}

func (r *Record) Verify(key ed25519.PublicKey, donor, lease string, done Done) bool {
	return ed25519.Verify(key, r.bytes(donor, lease, done.Facts), done.Signature)
}
