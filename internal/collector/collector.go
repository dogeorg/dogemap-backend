package collector

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"code.dogecoin.org/governor"

	core "code.dogecoin.org/dogemap-backend/internal/core"
	"code.dogecoin.org/dogemap-backend/internal/spec"
)

// Current Core Node version
const CurrentProtocolVersion = 70015

// Minimum height accepted by other nodes
const MinimumBlockHeight = 700000

// Our DogeMap Node services
const DogeMapServices = 0

func New(store spec.Store, fromAddr spec.Address, maxTime time.Duration, isLocal bool) *Collector {
	c := &Collector{_store: store, Address: fromAddr, maxTime: maxTime, isLocal: isLocal}
	return c
}

type Collector struct {
	governor.ServiceCtx
	_store  spec.Store
	store   spec.Store
	mutex   sync.Mutex
	conn    net.Conn
	Address spec.Address
	maxTime time.Duration
	isLocal bool
}

func (c *Collector) Stop() {
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()

	if conn != nil {
		// must close net.Conn to interrupt blocking read/write.
		conn.Close()
	}
}

// goroutine
func (c *Collector) Run() {
	who := c.Address.String()
	for {
		c.store = c._store.WithCtx(c.Context) // Service Context is first available here
		// choose the next node to connect to
		remoteNode := c.Address
		for !remoteNode.IsValid() {
			var err error
			remoteNode, err = c.store.ChooseCoreNode()
			if err != nil {
				log.Printf("[%s] ChooseCoreNode: %v", who, err)
			} else if remoteNode.IsValid() {
				break
			}
			// none available, wait for local listener to add nodes
			c.Sleep(5 * time.Second)
		}
		// collect addresses from the node until the timeout
		c.collectAddresses(remoteNode)
		// avoid spamming on connect errors
		if c.Sleep(10 * time.Second) {
			// context was cancelled
			return
		}
	}
}

func (c *Collector) collectAddresses(nodeAddr spec.Address) {
	who := nodeAddr.String()
	//fmt.Printf("[%s] Connecting to node: %s\n", who, nodeAddr)

	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(c.Context, "tcp", nodeAddr.String())
	if err != nil {
		fmt.Printf("[%s] Error connecting to Core node [%v]: %v\n", who, nodeAddr, err)
		return
	}
	defer conn.Close()

	c.mutex.Lock()
	c.conn = conn // for shutdown
	c.mutex.Unlock()

	// set a time limit on waiting for addresses per node
	if c.maxTime != 0 {
		conn.SetReadDeadline(time.Now().Add(c.maxTime))
	}
	reader := bufio.NewReader(conn)

	// send our 'version' message
	_, err = conn.Write(core.EncodeMessage("version", makeVersion(CurrentProtocolVersion))) // nodeVer
	if err != nil {
		fmt.Printf("[%s] Error sending version message: %v\n", who, err)
		return
	}

	//fmt.Printf("[%s] Sent 'version' message\n", who)

	// expect the version message from the node
	version, err := expectVersion(reader)
	if err != nil {
		fmt.Printf("[%s] %v\n", who, err)
		return
	}

	//fmt.Printf("[%s] Received 'version': %v\n", who, version)

	nodeVer := version.Version // other node's version
	if nodeVer >= 209 {
		// send 'verack' in response
		_, err = conn.Write(core.EncodeMessage("verack", []byte{}))
		if err != nil {
			fmt.Printf("[%s] failed to send 'verack': %v\n", who, err)
			return
		}
		//fmt.Printf("[%s] Sent 'verack'\n", who)
	}

	// successful connection: update the node's timestamp.
	if !c.isLocal {
		c.store.UpdateCoreTime(nodeAddr)
	}

	addresses := 0
	total := 0
	for {
		cmd, payload, err := core.ReadMessage(reader)
		if err != nil {
			fmt.Printf("[%s] Error reading message: %v\n", who, err)
			return
		}

		switch cmd {
		case "ping":
			//fmt.Printf("[%s] Ping received.\n", who)
			sendPong(conn, payload, who) // keep-alive

			// request a list of known addresses (seed nodes)
			sendGetAddr(conn, who)
			//fmt.Printf("[%s] Sent getaddr.\n", who)

		case "reject":
			re := core.DecodeReject(payload)
			fmt.Printf("[%s] Reject: %v %v %v\n", who, re.CodeName(), re.Message, re.Reason)

		case "addr":
			addr := core.DecodeAddrMsg(payload, nodeVer)
			_, oldLen, err := c.store.CoreStats()
			if err != nil {
				fmt.Printf("[%s] CoreStats: %v\n", who, err)
				break
			}
			kept := 0
			unixTimeSec := time.Now().Unix()
			validAfter := unixTimeSec - spec.MaxCoreNodeDays*spec.SecondsPerDay
			for _, a := range addr.AddrList {
				unixTimeSec := int64(a.Time)
				if unixTimeSec > validAfter {
					c.store.AddCoreNode(spec.Address{Host: net.IP(a.Address), Port: a.Port}, unixTimeSec, a.Services)
					kept++
				}
			}
			dbSize, newLen, err := c.store.CoreStats()
			if err != nil {
				fmt.Printf("[%s] CoreStats: %v\n", who, err)
				break
			}
			fmt.Printf("[%s] Addresses: %d received, %d expired, %d new, %d in DB\n", who, len(addr.AddrList), len(addr.AddrList)-kept, (newLen - oldLen), dbSize)
			addresses += len(addr.AddrList)
			total += (newLen - oldLen)
			if addresses >= 1000 {
				// done: try the next node (or reconnect to local node)
				// a node will only respond once to the 'addr' request
				conn.Close()
				// back off as the number of kept nodes falls towards zero
				wait := 60 - total
				if wait < 1 {
					wait = 1
				}
				//fmt.Printf("[%s] Sleeping for %v\n", who, wait)
				c.Sleep(time.Duration(wait) * time.Second)
				return
			}

		default:
			//fmt.Printf("Command '%s' payload: %s\n", cmd, hex.EncodeToString(payload))
			//fmt.Printf("[%s] Received: %v\n", who, cmd)
		}
	}
}

// makeVersion creates a version message to send to the peer
func makeVersion(remoteVersion int32) []byte {
	if remoteVersion > CurrentProtocolVersion {
		remoteVersion = CurrentProtocolVersion // min
	}
	version := core.VersionMsg{
		Version:   remoteVersion,
		Services:  DogeMapServices,
		Timestamp: time.Now().Unix(),
		RemoteAddr: core.NetAddr{
			Services: DogeMapServices,
			Address:  []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 14, 1, 84, 159},
			Port:     22556,
		},
		LocalAddr: core.NetAddr{
			Services: DogeMapServices,
			// NOTE: dogecoin nodes ignore these address fields.
			Address: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			Port:    0,
		},
		Agent:  "/DogeBox: DogeMap Service/",
		Nonce:  23972479,
		Height: MinimumBlockHeight,
		Relay:  false,
	}
	return core.EncodeVersion(version)
}

func expectVersion(reader *bufio.Reader) (core.VersionMsg, error) {
	// Core Node implementation: if connection is inbound, send Version immediately.
	// This means we'll receive the Node's version before `verack` for our Version,
	// however this is undocumented, so other nodes might ack first.
	cmd, payload, err := core.ReadMessage(reader)
	if err != nil {
		return core.VersionMsg{}, fmt.Errorf("error reading message: %v", err)
	}
	if cmd == "version" {
		return core.DecodeVersion(payload), nil
	}
	if cmd == "reject" {
		re := core.DecodeReject(payload)
		return core.VersionMsg{}, fmt.Errorf("reject: %s %s %s", re.CodeName(), re.Message, re.Reason)
	}
	return core.VersionMsg{}, fmt.Errorf("expected 'version' message from node, but received: %s", cmd)
}

func sendPong(conn net.Conn, pingPayload []byte, who string) {
	// reply with 'pong', same payload (nonce)
	_, err := conn.Write(core.EncodeMessage("pong", pingPayload))
	if err != nil {
		fmt.Printf("[%s] failed to send 'pong': %v\n", who, err)
		return
	}
}

func sendGetAddr(conn net.Conn, who string) {
	_, err := conn.Write(core.EncodeMessage("getaddr", []byte{}))
	if err != nil {
		fmt.Printf("[%s] failed to send 'getaddr': %v\n", who, err)
		return
	}
}
