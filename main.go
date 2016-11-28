package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

var cfgFile = flag.String("conf", "", "configuration file")
var listen = flag.String("listen", ":443", "listening port")

var config Config

func main() {
	flag.Parse()

	if err := config.ReadFile(*cfgFile); err != nil {
		log.Fatalf("Failed to read config %q: %s", *cfgFile, err)
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("Failed to listen: %s", err)
	}

	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatalf("Error while accepting: %s", err)
		}

		conn := &Conn{TCPConn: c.(*net.TCPConn)}
		go conn.proxy()
	}
}

type Conn struct {
	*net.TCPConn

	tlsMinor    int
	hostname    string
	backend     string
	backendConn *net.TCPConn
}

func (c *Conn) log(msg string, args ...interface{}) {
	msg = fmt.Sprintf(msg, args...)
	log.Printf("%s <> %s: %s", c.RemoteAddr(), c.LocalAddr(), msg)
}

func (c *Conn) abort(alert byte, msg string, args ...interface{}) {
	c.log(msg, args...)
	alertMsg := []byte{21, 3, byte(c.tlsMinor), 0, 2, 2, alert}
	if _, err := c.Write(alertMsg); err != nil {
		c.log("error while sending alert: %s", err)
	}
}

func (c *Conn) internalError(msg string, args ...interface{}) { c.abort(80, msg, args...) }
func (c *Conn) sniFailed(msg string, args ...interface{})     { c.abort(112, msg, args...) }

func (c *Conn) proxy() {
	defer c.Close()

	var (
		err          error
		handshakeBuf bytes.Buffer
	)
	c.hostname, c.tlsMinor, err = extractSNI(io.TeeReader(c, &handshakeBuf))
	if err != nil {
		c.internalError("Extracting SNI: %s", err)
		return
	}

	c.backend = config.Match(c.hostname)
	if c.backend == "" {
		c.sniFailed("no backend found for %q", c.hostname)
		return
	}

	c.log("routing %q to %q", c.hostname, c.backend)
	backend, err := net.DialTimeout("tcp", c.backend, 10*time.Second)
	if err != nil {
		c.internalError("failed to dial backend %q for %q: %s", c.backend, c.hostname, err)
		return
	}
	defer backend.Close()

	c.backendConn = backend.(*net.TCPConn)

	// Replay the piece of the handshake we had to read to do the
	// routing, then blindly proxy any other bytes.
	if _, err = io.Copy(c.backendConn, &handshakeBuf); err != nil {
		c.internalError("failed to replay handshake to %q: %s", c.backend, err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go proxy(&wg, c.TCPConn, c.backendConn)
	go proxy(&wg, c.backendConn, c.TCPConn)
	wg.Wait()
}

func proxy(wg *sync.WaitGroup, a, b net.Conn) {
	defer wg.Done()
	atcp, btcp := a.(*net.TCPConn), b.(*net.TCPConn)
	if _, err := io.Copy(atcp, btcp); err != nil {
		log.Printf("%s<>%s -> %s<>%s: %s", atcp.RemoteAddr(), atcp.LocalAddr(), btcp.LocalAddr(), btcp.RemoteAddr(), err)
	}
	btcp.CloseWrite()
	atcp.CloseRead()
}
