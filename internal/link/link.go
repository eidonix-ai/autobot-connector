package link

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"autobot-connector/internal/inputs"
	"autobot-connector/internal/wire"

	"github.com/gorilla/websocket"
)

type Config struct {
	Hub, Certificate, State, Socket, Image, NVML string
	TestHub                                      bool
	MaxTemperature                               int
}

type Link struct {
	config      Config
	runner      runner
	workProfile string
	dialer      websocket.Dialer
	key         ed25519.PrivateKey
	state       localState
}

type session struct {
	*Link
	conn  *wire.Socket
	donor string
}

func Run(ctx context.Context, conf Config) error {
	u, err := url.Parse(conf.Hub)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Hostname() == "" ||
		u.Path != "/donor" || u.Fragment != "" || u.RawQuery != "" || !wire.Digest(conf.Image) {
		return errors.New("invalid hub URL or work image pin")
	}
	if conf.MaxTemperature < 0 || conf.MaxTemperature > 83 ||
		(conf.MaxTemperature != 0 && conf.MaxTemperature < 11) {
		return errors.New("invalid temperature threshold")
	}
	certPEM, err := os.ReadFile(conf.Certificate)
	if err != nil {
		return err
	}
	block, rest := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("expected one pinned hub certificate")
	}
	pin, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if err := pin.CheckSignature(pin.SignatureAlgorithm, pin.RawTBSCertificate, pin.Signature); err != nil {
		return errors.New("hub certificate must be self-signed")
	}
	roots := x509.NewCertPool()
	roots.AddCert(pin)
	dial := inputs.PublicDial
	if conf.TestHub {
		dial = (&net.Dialer{Timeout: 4 * time.Second}).DialContext
	}
	l := Link{config: conf, runner: newRunner(conf.Socket), dialer: websocket.Dialer{
		NetDialContext: dial, HandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, ServerName: u.Hostname(),
			VerifyConnection: func(c tls.ConnectionState) error {
				if len(c.PeerCertificates) != 1 || len(c.VerifiedChains) == 0 || len(c.VerifiedChains[0]) != 1 ||
					!bytes.Equal(c.PeerCertificates[0].Raw, pin.Raw) {
					return errors.New("hub certificate is not the pinned self-signed leaf")
				}
				return nil
			}},
	}}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	var readiness runnerReadiness
	err = l.runner.json(readCtx, "GET", "/readiness", nil, &readiness)
	cancel()
	if err != nil {
		return fmt.Errorf("read runner work profile: %w", err)
	}
	if readiness.WorkProfile == nil {
		return errors.New("runner did not report its work profile")
	}
	l.workProfile = *readiness.WorkProfile
	if _, err := wire.WorkLimits(l.workProfile); err != nil {
		return err
	}
	l.key, err = privateKey(conf.State)
	if err != nil {
		return err
	}
	if err = l.restoreHealth(); err != nil {
		return err
	}
	go l.monitor(ctx)
	backoff := time.Second
	tryCode := true
	for ctx.Err() == nil {
		started := time.Now()
		err := l.session(ctx, tryCode)
		if ctx.Err() != nil {
			break
		}
		log.Printf("hub session: %q", err)
		if time.Since(started) > wire.Silence {
			backoff = time.Second
		}
		tryCode = !tryCode
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
	return nil
}

func privateKey(state string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(state, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(state, "key")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != ed25519.SeedSize {
			return nil, errors.New("invalid link key file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(key.Seed())
	return key, errors.Join(err, f.Sync(), f.Close())
}

func (l *Link) session(ctx context.Context, useCode bool) error {
	conn, response, err := l.dialer.DialContext(ctx, l.config.Hub, nil)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	socket := wire.Open(conn)
	defer socket.Close()
	closeOnSignal := context.AfterFunc(ctx, func() { socket.Close() })
	defer closeOnSignal()
	message, err := socket.Read()
	var challenge wire.Challenge
	if err != nil || message.Type != "challenge" || message.Lease != "" || message.Decode(&challenge) != nil || !wire.Hex(challenge.Nonce, 32) {
		return errors.New("invalid hub challenge")
	}
	code := ""
	if useCode {
		b, err := os.ReadFile(filepath.Join(l.config.State, "code"))
		if err == nil {
			code = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	public := l.key.Public().(ed25519.PublicKey)
	auth := wire.Auth{Key: public, Code: code, Signature: ed25519.Sign(l.key, wire.AuthBytes(challenge.Nonce, code))}
	if err := socket.Send("auth", "", auth); err != nil {
		return err
	}
	message, err = socket.Read()
	if err != nil {
		return err
	}
	if message.Type == "denied" && message.Lease == "" {
		var reason wire.Reason
		if message.Decode(&reason) == nil {
			return errors.New(reason.Reason)
		}
	}
	var reply wire.Enrolled
	if message.Type != "enrolled" || message.Lease != "" || message.Decode(&reply) != nil ||
		reply.Donor != fmt.Sprintf("%x", sha256.Sum256(public)) {
		return errors.New("invalid authentication reply")
	}
	if err := os.Remove(filepath.Join(l.config.State, "code")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s := session{l, socket, reply.Donor}
	log.Printf("connected donor %s", s.donor)
	defer func() {
		socket.Close()
		jobs := l.activeJobs()
		for _, j := range jobs {
			j.interrupt("lost", wire.Health{})
		}
		for _, j := range jobs {
			<-j.finished
		}
	}()
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.heartbeats(heartbeatCtx)
	return s.receive()
}

func (s session) heartbeats(ctx context.Context) {
	t := time.NewTicker(wire.Heartbeat)
	defer t.Stop()
	for {
		if err := s.conn.Send("heartbeat", "", s.status(ctx)); err != nil {
			s.conn.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s session) receive() error {
	for {
		m, err := s.conn.Read()
		if err != nil {
			return err
		}
		if m.Type == "heartbeat" && m.Lease == "" && m.Decode(&struct{}{}) == nil {
			s.conn.Extend()
			continue
		}
		if m.Type == "offer" {
			var offer wire.Offer
			if m.Lease == "" || m.Decode(&offer) != nil || !offer.Valid(time.Now()) || offer.Image != s.config.Image ||
				s.active(m.Lease) != nil {
				return errors.New("invalid, mismatched or overlapping lease")
			}
			if err := s.offer(m.Lease, offer); err != nil {
				return err
			}
			continue
		}
		j := s.active(m.Lease)
		if j == nil || j.id != m.Lease || !j.receive(m) {
			return errors.New("unexpected, malformed or out-of-lease operation")
		}
	}
}
