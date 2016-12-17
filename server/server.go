package main

import (
	"crypto/sha1"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/klauspost/compress/snappy"
	kcp "github.com/xtaci/kcp-go"
	"github.com/xtaci/smux"
	"golang.org/x/crypto/pbkdf2"
)

const (
	noDelay        = 0
	interval       = 40
	resend         = 0
	noCongestion   = 0
	sockBuf        = 4194304
	noComp         = true
	dataShard      = 10
	parityShard    = 3
	udpSegmentSize = 1472
)

var (
	// VERSION is injected by buildflags
	VERSION = "SELFBUILD"
	// SALT is use for pbkdf2 key expansion
	SALT = "kcp-go"
)

type compStream struct {
	conn net.Conn
	w    *snappy.Writer
	r    *snappy.Reader
}

func (c *compStream) Read(p []byte) (n int, err error) {
	return c.r.Read(p)
}

func (c *compStream) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	err = c.w.Flush()
	return n, err
}

func (c *compStream) Close() error {
	return c.conn.Close()
}

func newCompStream(conn net.Conn) *compStream {
	c := new(compStream)
	c.conn = conn
	c.w = snappy.NewBufferedWriter(conn)
	c.r = snappy.NewReader(conn)
	return c
}

func main() {
	pass := pbkdf2.Key([]byte("asdnanan"), []byte(SALT), 4096, 32, sha1.New)
	block, _ := kcp.NewNoneBlockCrypt(pass)
	lis, err := kcp.ListenWithOptions("192.168.100.103:8888", block, dataShard, parityShard)
	if err != nil {
		panic(err)
	}

	if err := lis.SetDSCP(0); err != nil {
		log.Println("SetDSCP:", err)
	}
	if err := lis.SetReadBuffer(sockBuf); err != nil {
		log.Println("SetReadBuffer:", err)
	}
	if err := lis.SetWriteBuffer(sockBuf); err != nil {
		log.Println("SetWriteBuffer:", err)
	}
	for {
		if conn, err := lis.AcceptKCP(); err == nil {
			log.Println("remote address:", conn.RemoteAddr())
			conn.SetStreamMode(true)
			conn.SetNoDelay(noDelay, interval, resend, noCongestion)
			conn.SetMtu(udpSegmentSize)
			conn.SetWindowSize(1024, 1024)
			conn.SetACKNoDelay(true)
			conn.SetKeepAlive(10)

			if noComp {
				go handleMux(conn)
			} else {
				go handleMux(newCompStream(conn))
			}
		} else {
			panic(err)
		}
	}
}

func handleMux(conn io.ReadWriteCloser) {
	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = sockBuf
	mux, err := smux.Server(conn, smuxConfig)
	if err != nil {
		log.Println(err)
		return
	}
	defer mux.Close()
	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			log.Println("accpect failed!", err)
			return
		}
		tlsConfig := &tls.Config{}
		tlsConfig.Certificates = make([]tls.Certificate, 1)
		tlsConfig.Certificates[0], err = tls.LoadX509KeyPair("ec.crt", "ec.uncrypted.pem")
		if err != nil {
			panic(err)
			return
		}
		tlsConfig.ServerName = "www.longxboy.com"
		tlsConn := tls.Server(stream, tlsConfig)

		var buff []byte = make([]byte, 20000)
		nRead, err := tlsConn.Read(buff)
		if err != nil {
			log.Println(err)
			return
		}
		fmt.Println("read:", nRead)
		nRead, err = tlsConn.Read(buff)
		if err != nil {
			log.Println(err)
			return
		}
		fmt.Println("read:", nRead)
		nWrite, err := tlsConn.Write([]byte("xixixixi"))
		fmt.Println(nWrite)
		tlsConn.Close()
	}
}
