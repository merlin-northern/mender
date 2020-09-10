package mc

import (
	"bufio"
	"flag"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/kr/pty"
	"github.com/vmihailenco/msgpack"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 1 * time.Second
	// Maximum message size allowed from peer.
	maxMessageSize = 8192
	// Time allowed to read the next pong message from the peer.
	defaultPingWait = 10 * time.Second
	// Shell is the default shell when connecting via device connect
	shell     = "/bin/bash"
	shellTerm = "xterm-256color"
	// CmdShell is the websocket command used to communicate with the shell
	CmdShell = "shell"
)

// initialize tenant and device running:
// $ curl http://localhost:8080/api/internal/v1/deviceconnect/tenants -X POST -d '{"tenant_id": "abcd"}'
// $ curl http://localhost:8080/api/internal/v1/deviceconnect/tenants/abcd/devices -X POST -d '{"device_id": "1234567890"}'
// constants
const (
	deviceConnectPath = "/api/devices/v1/deviceconnect/connect"

//	JWT               = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibWVuZGVyLmRldmljZSI6dHJ1ZSwibWVuZGVyLnBsYW4iOiJlbnRlcnByaXNlIiwibWVuZGVyLnRlbmFudCI6ImFiY2QifQ.Q4bIDhEx53FLFUMipjJUNgEmEf48yjcaFxlh8XxZFVw"
)

var running = true
var cmd *exec.Cmd

// Message represents a message between the device and the backend
type Message struct {
	Cmd  string `json:"cmd" msgpack:"cmd"`
	Data []byte `json:"data" msgpack:"data"`
}

func StopLiveConnect() {
	running = false
	if cmd != nil {
		cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(1 * time.Second)
		cmd.Process.Signal(syscall.SIGKILL)
	}
	log.Printf("StopLiveConnect: starting.")
}

func IsRunning() bool {
	return running
}

func StartLiveConnect(token string, deviceConnectUrl string) {
	flag.Parse()
	log.SetFlags(0)

	// connect to the websocket
	u := url.URL{Scheme: "ws", Host: deviceConnectUrl, Path: deviceConnectPath}
	log.Printf("connecting to %s token: %s", u.String(), token)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token) // JWT)
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer ws.Close()
	// ping-pong
	ws.SetReadLimit(maxMessageSize)
	ws.SetReadDeadline(time.Now().Add(defaultPingWait))
	ws.SetPingHandler(func(message string) error {
		pongWait, _ := strconv.Atoi(message)
		ws.SetReadDeadline(time.Now().Add(time.Duration(pongWait) * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte{}, time.Now().Add(writeWait))
	})
	// start the shell
	cmd = exec.Command(shell)
	cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", shellTerm))
	f, err := pty.Start(cmd)
	if err != nil {
		panic(err)
	}
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct {
			h, w, x, y uint16
		}{
			uint16(40), uint16(80), 0, 0,
		})))
	go pipeStdout(ws, f)
	go pipeStdin(ws, f)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, unix.SIGINT)
	log.Printf("StartLiveConnect: after Notify")
	<-quit
}

func pipeStdout(ws *websocket.Conn, r io.Reader) {
	s := bufio.NewReader(r)
	for {
		log.Printf("pipeStdout: main loop '%v'.", running)
		if !running {
			return
		}
		raw := make([]byte, 255)
		n, err := s.Read(raw)
		if err != nil {
			log.Printf("error: %v", err)
			break
		}
		m := &Message{
			Cmd:  CmdShell,
			Data: raw[:n],
		}
		log.Printf("> cmd: %s, data: %v", m.Cmd, m.Data)
		data, err := msgpack.Marshal(m)
		if err != nil {
			log.Printf("error: %v", err)
			break
		}
		ws.SetWriteDeadline(time.Now().Add(writeWait))
		ws.WriteMessage(websocket.BinaryMessage, data)
	}
}

func pipeStdin(ws *websocket.Conn, w io.Writer) {
	for {
		log.Printf("pipeStdin: main loop '%v'.", running)
		if !running {
			return
		}
		_, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("error: %v", err)
			break
		}
		m := &Message{}
		err = msgpack.Unmarshal(data, m)
		if err != nil {
			log.Printf("error: %v", err)
			break
		}
		if m.Cmd == CmdShell {
			log.Printf("< cmd: %s, data: %v", m.Cmd, m.Data)
			if _, err := w.Write(m.Data); err != nil {
				break
			}
		}
	}
}
