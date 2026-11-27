// minecraft-server runs a Paper Minecraft server on Infrared: it fetches a
// pinned Java runtime and Paper onto the environment's volume, runs the server,
// and serves a status page and a websocket to the game port on the one HTTPS
// address an environment has.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
)

const gameAddr = "127.0.0.1:25565"

// The server's state, as the status page and /status.json show it.
type status struct {
	mu sync.Mutex

	Phase     string    `json:"phase"` // fetching, waiting, starting, running, stopping, stopped, failed
	Detail    string    `json:"detail,omitempty"`
	Paper     string    `json:"paper"`
	Java      string    `json:"java,omitempty"`
	DataDir   string    `json:"dataDir"`
	HeapMB    int       `json:"heapMB"`
	Started   time.Time `json:"started"`
	FetchSecs float64   `json:"fetchSeconds,omitempty"`
	BootSecs  float64   `json:"bootSeconds,omitempty"`
}

func (s *status) set(phase, detail string) {
	s.mu.Lock()
	s.Phase, s.Detail = phase, detail
	s.mu.Unlock()
	log.Printf("phase %s %s", phase, detail)
}

func (s *status) snapshot() status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return status{Phase: s.Phase, Detail: s.Detail, Paper: s.Paper, Java: s.Java, DataDir: s.DataDir,
		HeapMB: s.HeapMB, Started: s.Started, FetchSecs: s.FetchSecs, BootSecs: s.BootSecs}
}

func main() {
	log.SetFlags(log.LUTC | log.Ldate | log.Ltime)
	port := envOr("PORT", "8080")
	st := &status{Paper: paperJar.name, Started: time.Now().UTC(), HeapMB: heapMB(os.Getenv("MEMORY_LIMIT"))}
	st.DataDir = dataDir(envOr("DATA_DIR", "/data"))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { page.Execute(w, st.snapshot()) })
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st.snapshot())
	})
	// The game port over a websocket: each binary frame carries the bytes a
	// Minecraft client would send over TCP. No Origin check, as a game client
	// is not a browser page of ours.
	mux.Handle("GET /ws", websocket.Server{Handler: proxyGame})
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, st)
	}()

	<-ctx.Done()
	<-done
	shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(shut)
}

// run fetches what the server needs, starts it, and stops it cleanly when ctx
// ends. It returns when the server has exited or could not start; the status
// page keeps serving either way, so the reason can be read.
func run(ctx context.Context, st *status) {
	began := time.Now()
	st.set("fetching", "the Java runtime")
	runtimeDir := filepath.Join(st.DataDir, "runtime")
	java, err := ensureJava(runtimeDir)
	if err != nil {
		st.set("failed", err.Error())
		return
	}
	st.mu.Lock()
	st.Java = java
	st.mu.Unlock()
	st.set("fetching", paperJar.name)
	jar := filepath.Join(runtimeDir, paperJar.name)
	if err := fetch(paperJar, jar); err != nil {
		st.set("failed", err.Error())
		return
	}
	st.mu.Lock()
	st.FetchSecs = time.Since(began).Seconds()
	st.mu.Unlock()

	serverDir := filepath.Join(st.DataDir, "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		st.set("failed", err.Error())
		return
	}
	// Minecraft's End User License Agreement is the operator's to accept:
	// https://aka.ms/MinecraftEULA. Nothing starts until EULA=true says so.
	if os.Getenv("EULA") != "true" {
		st.set("waiting", "set the environment variable EULA=true to accept Minecraft's EULA (https://aka.ms/MinecraftEULA)")
		return
	}
	if err := os.WriteFile(filepath.Join(serverDir, "eula.txt"), []byte("eula=true\n"), 0o644); err != nil {
		st.set("failed", err.Error())
		return
	}
	props := filepath.Join(serverDir, "server.properties")
	if _, err := os.Stat(props); os.IsNotExist(err) {
		// The game port answers inside the pod only: players come in over /ws.
		os.WriteFile(props, []byte("server-ip=127.0.0.1\nserver-port=25565\nmotd=A Minecraft server on Infrared\n"), 0o644)
	}

	heap := strconv.Itoa(st.HeapMB) + "M"
	cmd := exec.Command(java, "-Xms"+heap, "-Xmx"+heap, "-XX:+UseG1GC", "-Djava.io.tmpdir="+os.TempDir(),
		"-jar", jar, "--nogui")
	cmd.Dir = serverDir
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	booted := time.Now()
	st.set("starting", "Paper")
	if err := cmd.Start(); err != nil {
		st.set("failed", err.Error())
		return
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			fmt.Println(line)
			if strings.Contains(line, "Done (") && strings.Contains(line, "For help") {
				st.mu.Lock()
				st.BootSecs = time.Since(booted).Seconds()
				st.mu.Unlock()
				st.set("running", "")
			}
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		st.set("failed", fmt.Sprintf("the server exited: %v", err))
	case <-ctx.Done():
		// Paper saves the world on stop. Kubernetes gives a pod 30 seconds.
		st.set("stopping", "saving the world")
		io.WriteString(stdin, "stop\n")
		select {
		case <-exited:
			st.set("stopped", "")
		case <-time.After(25 * time.Second):
			cmd.Process.Kill()
			st.set("stopped", "killed after 25 seconds")
		}
	}
}

// proxyGame carries one websocket's binary frames to the game port and back.
func proxyGame(ws *websocket.Conn) {
	ws.PayloadType = websocket.BinaryFrame
	game, err := net.DialTimeout("tcp", gameAddr, 5*time.Second)
	if err != nil {
		ws.WriteClose(1013) // try again later: the server is not up
		return
	}
	defer game.Close()
	go func() {
		io.Copy(game, ws)
		game.Close()
	}()
	io.Copy(ws, game)
}

// dataDir returns dir when it can be written, and a folder under the temporary
// directory when it cannot, so a missing volume is seen on the status page
// and not as a crash.
func dataDir(dir string) string {
	if err := os.MkdirAll(dir, 0o755); err == nil {
		probe := filepath.Join(dir, ".write-check")
		if err := os.WriteFile(probe, nil, 0o644); err == nil {
			os.Remove(probe)
			return dir
		}
	}
	fallback := filepath.Join(os.TempDir(), "minecraft-data")
	log.Printf("%s cannot be written: using %s, which does not outlive the pod", dir, fallback)
	os.MkdirAll(fallback, 0o755)
	return fallback
}

// heapMB is the Java heap for a container memory limit in bytes: 70 percent
// of it, leaving the rest for the JVM's own memory and this process. 1024
// when the limit is unknown.
func heapMB(limit string) int {
	n, err := strconv.ParseInt(limit, 10, 64)
	if err != nil || n <= 0 {
		return 1024
	}
	mb := int(n / (1024 * 1024) * 70 / 100)
	if mb < 256 {
		mb = 256
	}
	return mb
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Minecraft server</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#10151c;color:#e8edf2}
main{max-width:34rem;margin:0 auto;padding:3rem 1rem}
h1{font-size:1.4rem;margin:0 0 1.5rem}
.phase{display:inline-block;padding:.15rem .6rem;border-radius:1rem;background:#243447;font-weight:600}
.running{background:#1f6f43}.failed{background:#8a2c2c}
dl{display:grid;grid-template-columns:auto 1fr;gap:.4rem 1.2rem;margin:1.5rem 0}
dt{color:#9fb0c0}dd{margin:0;overflow-wrap:anywhere}
</style></head><body><main>
<h1>Minecraft server</h1>
<span class="phase {{.Phase}}">{{.Phase}}</span> {{.Detail}}
<dl>
<dt>Server</dt><dd>{{.Paper}}</dd>
<dt>Java heap</dt><dd>{{.HeapMB}} MB</dd>
<dt>World kept in</dt><dd>{{.DataDir}}</dd>
{{if .FetchSecs}}<dt>Fetched in</dt><dd>{{printf "%.0f" .FetchSecs}} s</dd>{{end}}
{{if .BootSecs}}<dt>Started in</dt><dd>{{printf "%.0f" .BootSecs}} s</dd>{{end}}
<dt>Up since</dt><dd>{{.Started.Format "2006-01-02 15:04 UTC"}}</dd>
</dl>
</main></body></html>
`))
