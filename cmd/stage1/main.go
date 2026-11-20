package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"autobot-connector/internal/wire"
)

const controlSocket = "/control/runner.sock"
const statePath = "/state/lease.json"

var leaseName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type execRequest struct {
	Lease string
	Argv  []string
}

type stopRequest struct {
	Lease, End string
}

type event struct {
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	Error  string `json:"error,omitempty"`
}

func decode(w http.ResponseWriter, req *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}

func (r *runner) api() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readiness", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		snapshot := r.snapshot(req.URL.Query().Get("lease"))
		status := struct {
			Status, Active, Last, End string
			FreeGPUs                  int
			WorkProfile               string
		}{Status: "ready", FreeGPUs: snapshot.FreeGPUs, WorkProfile: workProfile}
		if r.dirty || r.reason != "" {
			status.Status = "unhealthy"
		}
		if snapshot.Active != nil {
			status.Status, status.Active = "running", snapshot.Active.Lease
		}
		if snapshot.Last != nil {
			status.Last, status.End = snapshot.Last.Lease, snapshot.Last.End
		}
		r.mu.Unlock()
		json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		status := "ready"
		if r.reason != "" || r.dirty {
			status = "unhealthy"
		} else if len(r.Leases) > 0 {
			status = "running"
		}
		data, err := json.Marshal(struct {
			Status string `json:"status"`
			Reason string `json:"reason,omitempty"`
			state
		}{status, r.reason, r.snapshot(req.URL.Query().Get("lease"))})
		r.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second))
		w.Header().Set("Content-Type", "application/json")
		w.Write(append(data, '\n'))
	})
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, req *http.Request) {
		var in leaseRequest
		if !decode(w, req, &in) {
			return
		}
		if !leaseName.MatchString(in.Lease) || !in.Deadline.After(time.Now()) || len(in.Salvage) > 16 || !wire.ValidGPUCount(in.GPUs) {
			http.Error(w, "invalid lease, deadline or salvage list", http.StatusBadRequest)
			return
		}
		for _, path := range in.Salvage {
			if !fs.ValidPath(path) || path == "." || len(path) > 255 {
				http.Error(w, "invalid salvage path", http.StatusBadRequest)
				return
			}
		}
		now := time.Now()
		in.Deadline = now.Add(in.Deadline.Sub(now))
		j, err := r.start(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(j)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, req *http.Request) {
		var in stopRequest
		if !decode(w, req, &in) {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		j := r.activeLease(in.Lease)
		if j == nil || (in.End != "cancel" && in.End != "complete") {
			http.Error(w, "invalid stop for active lease", http.StatusConflict)
			return
		}
		if j.End == "" {
			j.End = in.End
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /load", r.load)
	mux.HandleFunc("POST /exec", r.command)
	return mux
}

func (r *runner) command(w http.ResponseWriter, req *http.Request) {
	var in execRequest
	if !decode(w, req, &in) {
		return
	}
	if len(in.Argv) == 0 || in.Argv[0] == "" {
		http.Error(w, "command required", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	j := r.activeLease(in.Lease)
	if r.dirty || j == nil || j.Lease != in.Lease || j.End != "" || j.pending != "" || (j.Bundle && !j.loaded) || !time.Now().Before(j.Deadline) {
		r.mu.Unlock()
		http.Error(w, "lease cannot accept a command", http.StatusConflict)
		return
	}
	id, err := r.newExec(j.Container, append([]string{"/stage1", "command"}, in.Argv...))
	if err != nil {
		r.mu.Unlock()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), j.Deadline.Add(15*time.Second))
	defer cancel()
	timer := time.AfterFunc(3*time.Second, cancel)
	resp, err := r.openExec(ctx, id)
	if !timer.Stop() || err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		j.End = "cancel"
		r.mu.Unlock()
		http.Error(w, fmt.Sprint("starting command: ", err), http.StatusConflict)
		return
	}
	j.pending = id
	r.mu.Unlock()

	ctl := http.NewResponseController(w)
	encoder := json.NewEncoder(w)
	deliver := true
	emit := func(e event) {
		if deliver {
			ctl.SetWriteDeadline(time.Now().Add(time.Second))
			deliver = encoder.Encode(e) == nil && ctl.Flush() == nil
		}
	}
	err = readExec(resp, func(stream byte, data []byte) {
		name := "stdout"
		if stream == 2 {
			name = "stderr"
		}
		emit(event{Stream: name, Data: data})
	})

	r.mu.Lock()
	var code int
	var exitErr error
	if j.pending == id {
		code, exitErr = r.exit(id)
		fact := commandFact{}
		if exitErr == nil {
			fact.ExitCode = &code
		}
		j.Commands = append(j.Commands, fact)
		j.pending = ""
	} else if known := j.Commands[len(j.Commands)-1].ExitCode; known != nil {
		code = *known
	} else {
		exitErr = errors.New("command exit is unavailable")
	}
	r.mu.Unlock()
	if e := errors.Join(err, exitErr); e != nil {
		emit(event{Error: e.Error()})
	} else {
		emit(event{Exit: &code})
	}
}

func serve() error {
	if limitsErr != nil {
		return limitsErr
	}
	if !formatSHA(workImage) {
		return errors.New("invalid work image pin")
	}
	names, err := gpuQuery("--query-gpu=name")
	if err != nil {
		return fmt.Errorf("GPU model query: %w", err)
	}
	if err := checkGPUModel(gpuModel, names); err != nil {
		return err
	}
	r := runner{engine: engine{unixClient("/var/run/docker.sock")}, statePath: statePath, dirty: true, reason: "reconciling"}
	var saved state
	if data, err := os.ReadFile(statePath); err == nil {
		if err = json.Unmarshal(data, &saved); err == nil {
			r.state = saved
		} else {
			log.Printf("read runner state: %v", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		log.Printf("read runner state: %v", err)
	}
	if r.Leases == nil {
		r.Leases = map[string]*lease{}
	}
	if r.Active != nil {
		r.Leases[r.Active.Lease] = r.Active
		r.Active = nil
	}
	r.vram = map[string]int{}
	r.usedGPUs = map[string]bool{}
	r.results = map[string]*lease{}
	r.cleaned = map[string]bool{}
	if r.Last != nil {
		r.results[r.Last.Lease] = r.Last
	}
	if err := os.Remove(controlSocket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Chown(filepath.Dir(controlSocket), 1000, 1000); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(controlSocket), 0700); err != nil {
		return err
	}
	listener, err := net.Listen("unix", controlSocket)
	if err != nil {
		return err
	}
	if err = os.Chown(controlSocket, 1000, 1000); err != nil {
		return err
	}
	if err = os.Chmod(controlSocket, 0700); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			r.tick()
			<-ticker.C
		}
	}()
	srv := http.Server{Handler: r.api(), ReadHeaderTimeout: time.Second, ReadTimeout: 3 * time.Second}
	return srv.Serve(listener)
}

func fixture(args []string) (int, error) {
	if len(args) < 2 {
		return 1, errors.New("usage: stage1 fixture SOCKET status|run|exec|stop|finish [LEASE ...]")
	}
	client := unixClient(args[0])
	method, path := "POST", "/"+args[1]
	var input any
	switch args[1] {
	case "status":
		method = "GET"
	case "run":
		if len(args) < 4 {
			return 1, errors.New("run requires a lease and duration")
		}
		duration, err := time.ParseDuration(args[3])
		if err != nil {
			return 1, err
		}
		input = leaseRequest{Lease: args[2], Deadline: time.Now().Add(duration), Salvage: args[4:]}
	case "exec":
		if len(args) < 4 {
			return 1, errors.New("exec requires a lease and argv")
		}
		input = execRequest{args[2], args[3:]}
	case "stop", "finish":
		if len(args) != 3 {
			return 1, errors.New("stop or finish requires a lease")
		}
		end := "cancel"
		if args[1] == "finish" {
			end = "complete"
		}
		path, input = "/stop", stopRequest{args[2], end}
	default:
		return 1, errors.New("unknown fixture operation")
	}
	resp, err := request(context.Background(), client, method, path, input)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if path != "/exec" {
		_, err = io.Copy(os.Stdout, resp.Body)
		return 0, err
	}
	d := json.NewDecoder(resp.Body)
	for {
		var e event
		if err = d.Decode(&e); err != nil {
			return 1, err
		}
		if e.Exit != nil {
			return *e.Exit, nil
		}
		if e.Error != "" {
			return 1, errors.New(e.Error)
		}
		w := os.Stdout
		if e.Stream == "stderr" {
			w = os.Stderr
		}
		if _, err = w.Write(e.Data); err != nil {
			return 1, err
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("operation required")
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "runner":
		err = serve()
	case "fixture":
		var code int
		code, err = fixture(args)
		if err == nil {
			os.Exit(code)
		}
	case "keeper":
		keeper()
	case "command":
		err = command(args)
	case "profile":
		err = profile()
	case "load":
		err = load(args, os.Stdin)
	case "export":
		err = export(args)
	case "probe":
		err = probe(args)
	default:
		err = errors.New("unknown operation " + strconv.Quote(os.Args[1]))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
