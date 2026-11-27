// mcping asks a Minecraft Java server for its status, over TCP or over the
// websocket minecraft-server serves at /ws, and prints the server's answer.
//
//	mcping tcp://127.0.0.1:25565
//	mcping wss://minecraft.example.com/ws
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/websocket"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mcping tcp://host:port | ws://host/ws | wss://host/ws")
		os.Exit(2)
	}
	answer, took, err := ping(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcping:", err)
		os.Exit(1)
	}
	fmt.Printf("%s\nanswered in %s\n", answer, took.Round(time.Millisecond))
}

func ping(target string) (string, time.Duration, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", 0, err
	}
	began := time.Now()
	var conn io.ReadWriteCloser
	switch u.Scheme {
	case "tcp":
		conn, err = net.DialTimeout("tcp", u.Host, 10*time.Second)
	case "ws", "wss":
		origin := "http://" + u.Host
		var ws *websocket.Conn
		ws, err = websocket.Dial(target, "", origin)
		if err == nil {
			ws.PayloadType = websocket.BinaryFrame
			ws.SetDeadline(time.Now().Add(15 * time.Second))
			conn = ws
		}
	default:
		err = errors.New("the address must start with tcp://, ws:// or wss://")
	}
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()

	host := u.Hostname()
	// Handshake (packet 0x00): protocol version, host, port, next state 1 (status).
	var hs bytes.Buffer
	writeVarInt(&hs, 0x00)
	writeVarInt(&hs, 0)
	writeVarInt(&hs, len(host))
	hs.WriteString(host)
	hs.Write([]byte{0x63, 0xdd}) // 25565
	writeVarInt(&hs, 1)
	var out bytes.Buffer
	writeVarInt(&out, hs.Len())
	out.Write(hs.Bytes())
	out.Write([]byte{0x01, 0x00}) // status request: length 1, packet 0x00
	if _, err := conn.Write(out.Bytes()); err != nil {
		return "", 0, err
	}

	r := bufio.NewReader(conn)
	if _, err := readVarInt(r); err != nil { // packet length
		return "", 0, err
	}
	if id, err := readVarInt(r); err != nil || id != 0 {
		return "", 0, fmt.Errorf("unexpected packet %d: %v", id, err)
	}
	n, err := readVarInt(r)
	if err != nil {
		return "", 0, err
	}
	if n < 0 || n > 1<<20 {
		return "", 0, fmt.Errorf("status of %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", 0, err
	}
	return string(body), time.Since(began), nil
}

func writeVarInt(w *bytes.Buffer, v int) {
	u := uint32(v)
	for {
		b := byte(u & 0x7f)
		u >>= 7
		if u != 0 {
			b |= 0x80
		}
		w.WriteByte(b)
		if u == 0 {
			return
		}
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int(int32(v)), nil
		}
	}
	return 0, errors.New("a VarInt longer than 5 bytes")
}
