package main

import (
	"Lunnel/contrib"
	"Lunnel/crypto"
	"Lunnel/msg"
	"Lunnel/smux"
	"Lunnel/util"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
)

var maxIdlePipes int = 3
var maxStreams int = 6

var pingInterval time.Duration = time.Second * 8
var pingTimeout time.Duration = time.Second * 17
var cleanInterval time.Duration = time.Second * 5

var ControlMapLock sync.RWMutex
var ControlMap = make(map[crypto.UUID]*Control)

var subDomainIdx uint64

var TunnelMapLock sync.RWMutex
var TunnelMap = make(map[string]*Tunnel)

func NewControl(conn net.Conn, encryptMode string) *Control {
	ctl := &Control{
		ctlConn:     conn,
		pipeGet:     make(chan *smux.Session),
		pipeAdd:     make(chan *smux.Session),
		die:         make(chan struct{}),
		toDie:       make(chan struct{}),
		writeChan:   make(chan writeReq, 128),
		encryptMode: encryptMode,
	}
	return ctl
}

type writeReq struct {
	mType msg.MsgType
	body  interface{}
}

type Tunnel struct {
	tunnelConfig msg.TunnelConfig
	listener     net.Listener
	tunnelName   string
	ctl          *Control
}

type pipeNode struct {
	prev *pipeNode
	next *pipeNode
	pipe *smux.Session
}

type Control struct {
	ctlConn         net.Conn
	tunnels         []*Tunnel
	preMasterSecret []byte
	lastRead        uint64
	encryptMode     string

	busyPipes *pipeNode
	idleCount int
	idlePipes *pipeNode
	pipeAdd   chan *smux.Session
	pipeGet   chan *smux.Session

	die       chan struct{}
	toDie     chan struct{}
	writeChan chan writeReq

	ClientID crypto.UUID
}

func (c *Control) addIdlePipe(pipe *smux.Session) {
	pNode := &pipeNode{pipe: pipe, prev: nil, next: nil}
	if c.idlePipes != nil {
		c.idlePipes.prev = pNode
		pNode.next = c.idlePipes
	}
	c.idlePipes = pNode
	c.idleCount++

}

func (c *Control) addBusyPipe(pipe *smux.Session) {
	pNode := &pipeNode{pipe: pipe, prev: nil, next: nil}
	if c.busyPipes != nil {
		c.busyPipes.prev = pNode
		pNode.next = c.busyPipes
	}
	c.busyPipes = pNode
}

func (c *Control) removeIdleNode(pNode *pipeNode) {
	if pNode.prev == nil {
		c.idlePipes = pNode.next
		if c.idlePipes != nil {
			c.idlePipes.prev = nil
		}
	} else {
		pNode.prev.next = pNode.next
		if pNode.next != nil {
			pNode.next.prev = pNode.prev
		}
	}
	c.idleCount--
}

func (c *Control) removeBusyNode(pNode *pipeNode) {
	if pNode.prev == nil {
		c.busyPipes = pNode.next
		if c.busyPipes != nil {
			c.busyPipes.prev = nil
		}
	} else {
		pNode.prev.next = pNode.next
		if pNode.next != nil {
			pNode.next.prev = pNode.prev
		}
	}
}

func (c *Control) putPipe(p *smux.Session) {
	select {
	case c.pipeAdd <- p:
	case <-c.die:
		p.Close()
		return
	}
	return
}

func (c *Control) getPipe() *smux.Session {
	select {
	case p := <-c.pipeGet:
		return p
	case <-c.die:
		return nil
	}
}

func (c *Control) clean() {
	busy := c.busyPipes
	for {
		if busy == nil {
			break
		}
		if busy.pipe.IsClosed() {
			c.removeBusyNode(busy)
		} else if busy.pipe.NumStreams() < maxStreams {
			c.removeBusyNode(busy)
			c.addIdlePipe(busy.pipe)
		}
		busy = busy.next
	}
	idle := c.idlePipes
	for {
		if idle == nil {
			return
		}
		if idle.pipe.IsClosed() {
			c.removeIdleNode(idle)
		} else if idle.pipe.NumStreams() == 0 && c.idleCount > maxIdlePipes {
			log.WithFields(log.Fields{"time": time.Now().Unix(), "pipe": fmt.Sprintf("%p", idle.pipe)}).Infoln("remove and close idle")
			c.removeIdleNode(idle)
			idle.pipe.Close()
		}
		idle = idle.next
	}
	return

}
func (c *Control) getIdleFast() (idle *pipeNode) {
	idle = c.idlePipes
	for {
		if idle == nil {
			return
		}
		if idle.pipe.IsClosed() {
			c.removeIdleNode(idle)
			idle = idle.next
		} else {
			c.removeIdleNode(idle)
			return
		}
	}
	return
}

func (c *Control) pipeManage() {
	var available *smux.Session
	ticker := time.NewTicker(cleanInterval)
	defer ticker.Stop()
	for {
	Prepare:
		if available == nil || available.IsClosed() {
			available = nil
			idle := c.getIdleFast()
			if idle == nil {
				c.clean()
				idle := c.getIdleFast()
				c.writeChan <- writeReq{msg.TypePipeReq, nil}
				if idle == nil {
					pipeGetTimeout := time.After(time.Second * 12)
					for {
						select {
						case <-ticker.C:
							c.clean()
							idle := c.getIdleFast()
							if idle != nil {
								available = idle.pipe
								goto Available
							}
						case p := <-c.pipeAdd:
							if !p.IsClosed() {
								if p.NumStreams() < maxStreams {
									available = p
									goto Available
								} else {
									c.addBusyPipe(p)
								}
							}
						case <-c.die:
							return
						case <-pipeGetTimeout:
							goto Prepare
						}
					}
				} else {
					available = idle.pipe
				}
			} else {
				available = idle.pipe
			}
		}
	Available:
		select {
		case <-ticker.C:
			c.clean()
		case c.pipeGet <- available:
			log.WithFields(log.Fields{"pipe": fmt.Sprintf("%p", available)}).Infoln("dispatch pipe to consumer")
			available = nil
		case p := <-c.pipeAdd:
			if !p.IsClosed() {
				if p.NumStreams() < maxStreams {
					c.addIdlePipe(p)
				} else {
					c.addBusyPipe(p)
				}
			}
		case <-c.die:
			return
		}
	}
}

func (c *Control) Close() {
	select {
	case c.toDie <- struct{}{}:
	default:
	}
	log.WithField("time", time.Now().UnixNano()).Infoln("control closing")
	return
}

func (c *Control) IsClosed() bool {
	select {
	case <-c.die:
		return true
	default:
		return false
	}
}

func (c *Control) moderator() {
	_ = <-c.toDie
	log.WithFields(log.Fields{"ClientId": c.ClientID}).Infoln("client going to close")
	close(c.die)
	for _, t := range c.tunnels {
		if t.listener != nil {
			t.listener.Close()
		}
		TunnelMapLock.Lock()
		delete(TunnelMap, t.tunnelConfig.RemoteAddr())
		TunnelMapLock.Unlock()
		if serverConf.NotifyEnable {
			err := contrib.RemoveMember(serverConf.ServerDomain, t.tunnelConfig.RemoteAddr())
			if err != nil {
				log.WithFields(log.Fields{"err": err}).Errorln("notify remove member failed!")
			}
		}
	}
	idle := c.idlePipes
	for {
		if idle == nil {
			break
		}
		if !idle.pipe.IsClosed() {
			idle.pipe.Close()
		}
		idle = idle.next
	}
	busy := c.busyPipes
	for {
		if busy == nil {
			break
		}
		if !busy.pipe.IsClosed() {
			busy.pipe.Close()
		}
		busy = busy.next
	}
	c.ctlConn.Close()
}

func (c *Control) recvLoop() {
	atomic.StoreUint64(&c.lastRead, uint64(time.Now().UnixNano()))
	for {
		if c.IsClosed() {
			return
		}
		mType, body, err := msg.ReadMsgWithoutTimeout(c.ctlConn)
		if err != nil {
			log.WithFields(log.Fields{"err": err}).Warningln("ReadMsgWithoutTimeout failed")
			c.Close()
			return
		}

		atomic.StoreUint64(&c.lastRead, uint64(time.Now().UnixNano()))
		switch mType {
		case msg.TypeAddTunnels:
			go c.ServerAddTunnels(body.(*msg.AddTunnels))
		case msg.TypePong:
		case msg.TypePing:
			c.writeChan <- writeReq{msg.TypePong, nil}
		}
	}
}

func (c *Control) writeLoop() {
	lastWrite := time.Now()
	idx := 0
	for {
		if c.IsClosed() {
			return
		}
		select {
		case msgBody := <-c.writeChan:
			if msgBody.mType == msg.TypePing || msgBody.mType == msg.TypePong {
				if time.Now().Before(lastWrite.Add(pingInterval / 2)) {
					continue
				}
			}
			if msgBody.mType == msg.TypePipeReq {
				idx++
			}
			lastWrite = time.Now()
			log.WithFields(log.Fields{"mType": msgBody.mType, "body": fmt.Sprintf("%v", msgBody.body)}).Infoln("send msg to client")
			err := msg.WriteMsg(c.ctlConn, msgBody.mType, msgBody.body)
			if err != nil {
				c.Close()
				return
			}
		case <-c.die:
			return
		}
	}

}

func (c *Control) Serve() {
	go c.moderator()
	go c.recvLoop()
	go c.writeLoop()
	go c.pipeManage()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if (uint64(time.Now().UnixNano()) - atomic.LoadUint64(&c.lastRead)) > uint64(pingTimeout) {
				c.Close()
				return
			}
			select {
			case c.writeChan <- writeReq{msg.TypePing, nil}:
			case <-c.die:
				return
			}
		case <-c.die:
			return
		}
	}
}

func proxyConn(userConn net.Conn, c *Control, tunnelName string) {
	defer userConn.Close()
	p := c.getPipe()
	if p == nil {
		return
	}
	stream, err := p.OpenStream(tunnelName)
	if err != nil {
		c.putPipe(p)
		return
	}
	defer stream.Close()
	c.putPipe(p)
	p1die := make(chan struct{})
	p2die := make(chan struct{})
	go func() {
		io.Copy(stream, userConn)
		close(p1die)
	}()
	go func() {
		io.Copy(userConn, stream)
		close(p2die)
	}()
	select {
	case <-p1die:
	case <-p2die:
	}
	return
}

func (c *Control) ServerAddTunnels(sstm *msg.AddTunnels) error {
	for name, _ := range sstm.Tunnels {
		tunnelConfig := sstm.Tunnels[name]
		tunnelConfig.Hostname = serverConf.ServerDomain
		var lis net.Listener = nil

		if tunnelConfig.Protocol == "tcp" || tunnelConfig.Protocol == "udp" {
			if tunnelConfig.Protocol == "tcp" {
				lis, err = net.Listen("tcp", fmt.Sprintf("%s:0", serverConf.ListenIP))
				if err != nil {
					return errors.Wrap(err, "binding TCP listener")
				}
			} else {
				lis, err = net.Listen("udp", fmt.Sprintf("%s:0", serverConf.ListenIP))
				if err != nil {
					return errors.Wrap(err, "binding udp listener")
				}
			}
			go func(tunnelName string) {
				for {
					if c.IsClosed() {
						return
					}
					conn, err := lis.Accept()
					if err != nil {
						return
					}
					go proxyConn(conn, c, tunnelName)
				}
			}(name)
			//todo: port allocated by server not by OS
			addr := lis.Addr().(*net.TCPAddr)
			tunnelConfig.RemotePort = uint16(addr.Port)
		} else if tunnelConfig.Protocol == "http" || tunnelConfig.Protocol == "https" {
			subDomain := util.Int2Short(atomic.AddUint64(&subDomainIdx, 1))
			tunnelConfig.Subdomain = string(subDomain)
			if tunnelConfig.Protocol == "http" {
				tunnelConfig.RemotePort = serverConf.HttpPort
			} else {
				tunnelConfig.RemotePort = serverConf.HttpsPort
			}
		}
		sstm.Tunnels[name] = tunnelConfig
		if serverConf.NotifyEnable {
			err = contrib.AddMember(serverConf.ServerDomain, tunnelConfig.RemoteAddr())
			if err != nil {
				log.WithFields(log.Fields{"err": err}).Errorln("notify add member failed!")
			}
		}

		tunnel := Tunnel{tunnelConfig: tunnelConfig, listener: lis, ctl: c, tunnelName: name}
		c.tunnels = append(c.tunnels, &tunnel)
		TunnelMapLock.Lock()
		TunnelMap[tunnel.tunnelConfig.RemoteAddr()] = &tunnel
		TunnelMapLock.Unlock()
	}
	err = msg.WriteMsg(c.ctlConn, msg.TypeAddTunnels, *sstm)
	if err != nil {
		return errors.Wrap(err, "WriteMsg sstm")
	}
	return nil
}

func (c *Control) GenerateClientId() crypto.UUID {
	c.ClientID = crypto.GenUUID()
	return c.ClientID
}

func (c *Control) ServerHandShake() error {
	var shello msg.ControlServerHello
	var chello *msg.ControlClientHello
	mType, body, err := msg.ReadMsg(c.ctlConn)
	if err != nil {
		return errors.Wrap(err, "msg.ReadMsg")
	}
	if mType != msg.TypeControlClientHello {
		return errors.Errorf("invalid msg type(%d),expect(%d)", mType, msg.TypeControlClientHello)
	}
	chello = body.(*msg.ControlClientHello)
	if serverConf.AuthEnable {
		isok, err := contrib.Auth(chello.AuthToken)
		if err != nil {
			return errors.Wrap(err, "contrib.Auth")
		}
		if !isok {
			return errors.Errorf("auth failed!token:%s", chello.AuthToken)
		}
	}

	if c.encryptMode != "none" {
		priv, keyMsg := crypto.GenerateKeyExChange()
		if keyMsg == nil || priv == nil {
			return errors.Errorf("crypto.GenerateKeyExChange error ,exchange key is nil")
		}
		preMasterSecret, err := crypto.ProcessKeyExchange(priv, chello.CipherKey)
		if err != nil {
			return errors.Wrap(err, "crypto.ProcessKeyExchange")
		}
		c.preMasterSecret = preMasterSecret
		shello.CipherKey = keyMsg
	}
	shello.ClientID = c.GenerateClientId()
	err = msg.WriteMsg(c.ctlConn, msg.TypeControlServerHello, shello)
	if err != nil {
		return errors.Wrap(err, "Write ClientId")
	}

	ControlMapLock.Lock()
	ControlMap[c.ClientID] = c
	ControlMapLock.Unlock()
	return nil
}

func PipeHandShake(conn net.Conn, phs *msg.PipeClientHello) error {
	ControlMapLock.RLock()
	ctl := ControlMap[phs.ClientID]
	ControlMapLock.RUnlock()
	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = 4194304
	var err error
	var sess *smux.Session
	if ctl.encryptMode != "none" {
		prf := crypto.NewPrf12()
		var masterKey []byte = make([]byte, 16)
		prf(masterKey, ctl.preMasterSecret, phs.ClientID[:], phs.Once[:])
		cryptoConn, err := crypto.NewCryptoConn(conn, masterKey)
		if err != nil {
			return errors.Wrap(err, "crypto.NewCryptoConn")
		}
		//server endpoint is the pipe connection source,so we use smux.Client
		sess, err = smux.Client(cryptoConn, smuxConfig)
		if err != nil {
			return errors.Wrap(err, "smux.Client")
		}
	} else {
		sess, err = smux.Client(conn, smuxConfig)
		if err != nil {
			return errors.Wrap(err, "smux.Client")
		}
	}
	ctl.putPipe(sess)
	return nil
}
