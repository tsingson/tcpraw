package tcpraw

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	errOpNotImplemented = errors.New("operation not implemented")
	source              = rand.NewSource(time.Now().UnixNano())
)

type Packet struct {
	bts  []byte
	addr net.Addr
}

// TCPConn defines a TCP-packet oriented connection
type TCPConn struct {
	ready   chan struct{}
	die     chan struct{}
	dieOnce sync.Once
	tcpconn *net.TCPConn

	// gopacket
	handle       *pcap.Handle
	packetSource *gopacket.PacketSource
	chPacket     chan Packet                // incoming packets channel
	linkLayer    gopacket.SerializableLayer // link layer header
	networkLayer gopacket.SerializableLayer // network layer header

	// important TCP header information
	seq uint32
	ack uint32
}

// Dial connects to the remote TCP port
func Dial(network, address string) (*TCPConn, error) {
	// remote address resolve
	raddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// create a dummy UDP socket, to get routing information
	dummy, err := net.Dial("udp", address)
	if err != nil {
		return nil, err
	}

	// get iface name from the dummy connection, eg. eth0, lo0
	ifaces, err := pcap.FindAllDevs()
	if err != nil {
		return nil, err
	}

	var ifaceName string
	for _, iface := range ifaces {
		for _, addr := range iface.Addresses {
			if addr.IP.Equal(dummy.LocalAddr().(*net.UDPAddr).IP) {
				ifaceName = iface.Name
			}
		}
	}
	if ifaceName == "" {
		return nil, errors.New("cannot find correct interface")
	}

	// pcap init
	handle, err := pcap.OpenLive(ifaceName, 65536, true, time.Second)
	if err != nil {
		return nil, err
	}

	// TCP local address reuses the same address from UDP
	laddr, err := net.ResolveTCPAddr(network, dummy.LocalAddr().String())
	if err != nil {
		return nil, err
	}
	dummy.Close()

	// apply filter for incoming data
	filter := fmt.Sprintf("tcp and dst host %v and dst port %v and src host %v and src port %v", laddr.IP, laddr.Port, raddr.IP, raddr.Port)
	if err := handle.SetBPFFilter(filter); err != nil {
		return nil, err
	}

	// create an established tcp connection
	// will hack this tcp connection for packet transmission
	tcpconn, err := net.DialTCP(network, laddr, raddr)
	if err != nil {
		return nil, err
	}

	// prevent tcpconn from sending ACKs
	if laddr.IP.To4() == nil {
		ipv6.NewConn(tcpconn).SetHopLimit(0)
	} else {
		ipv4.NewConn(tcpconn).SetTTL(0)
	}

	// fields
	conn := new(TCPConn)
	conn.die = make(chan struct{})
	conn.handle = handle
	conn.tcpconn = tcpconn
	conn.startCapture(gopacket.NewPacketSource(handle, handle.LinkType()))

	// discards data flow on tcp conn, to keep the window slides
	go io.Copy(ioutil.Discard, tcpconn)

	return conn, nil
}

// startCapture capture all packets flow and track necessary information
func (conn *TCPConn) startCapture(source *gopacket.PacketSource) {
	conn.chPacket = make(chan Packet)
	conn.ready = make(chan struct{})

	go func() {
		var once sync.Once
		for packet := range source.Packets() {
			transport := packet.TransportLayer().(*layers.TCP)
			// store sn from ack, sn is updated from remote
			// and will increase monotonically for each outgoing packet
			atomic.StoreUint32(&conn.seq, transport.Ack)

			once.Do(func() {
				// initialization of link layer & network layer data for outgoing packets,
				// suppose these 2 layers will not change during the conversation.
				// link layer
				if layer := packet.Layer(layers.LayerTypeEthernet); layer != nil {
					ethLayer := layer.(*layers.Ethernet)
					conn.linkLayer = &layers.Ethernet{
						EthernetType: ethLayer.EthernetType,
						SrcMAC:       ethLayer.DstMAC,
						DstMAC:       ethLayer.SrcMAC,
					}
				} else if layer := packet.Layer(layers.LayerTypeLoopback); layer != nil {
					loopLayer := layer.(*layers.Loopback)
					conn.linkLayer = &layers.Loopback{Family: loopLayer.Family}
				}

				// network layer
				if layer := packet.Layer(layers.LayerTypeIPv4); layer != nil {
					network := layer.(*layers.IPv4)
					conn.networkLayer = &layers.IPv4{
						SrcIP:    network.DstIP,
						DstIP:    network.SrcIP,
						Protocol: network.Protocol,
						Version:  network.Version,
						Id:       network.Id,
						Flags:    layers.IPv4DontFragment,
						TTL:      64,
					}
				} else if layer := packet.Layer(layers.LayerTypeIPv6); layer != nil {
					network := layer.(*layers.IPv6)
					conn.networkLayer = &layers.IPv6{
						Version:    network.Version,
						NextHeader: network.NextHeader,
						SrcIP:      network.DstIP,
						DstIP:      network.SrcIP,
						HopLimit:   64,
					}
				}

				// record the ISN for ack
				atomic.StoreUint32(&conn.ack, transport.Seq)

				close(conn.ready)
			})

			if transport.SYN {
				atomic.AddUint32(&conn.ack, 1)
			}
			if transport.PSH {
				// build packet address in net.Addr format
				var ip []byte
				if layer := packet.Layer(layers.LayerTypeIPv4); layer != nil {
					network := layer.(*layers.IPv4)
					ip = make([]byte, len(network.SrcIP))
					copy(ip, network.SrcIP)
				} else if layer := packet.Layer(layers.LayerTypeIPv6); layer != nil {
					network := layer.(*layers.IPv6)
					ip = make([]byte, len(network.SrcIP))
					copy(ip, network.SrcIP)
				}
				atomic.AddUint32(&conn.ack, uint32(len(transport.Payload)))

				select {
				case conn.chPacket <- Packet{transport.Payload, &net.TCPAddr{IP: ip, Port: int(transport.SrcPort)}}:
				case <-conn.die:
					return
				}
			}
		}
	}()
}

// ReadFrom implements the PacketConn ReadFrom method.
func (conn *TCPConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case <-conn.die:
		return 0, nil, io.EOF
	case packet := <-conn.chPacket:
		n = copy(p, packet.bts)
		return n, packet.addr, nil
	}
}

// WriteTo implements the PacketConn WriteTo method.
func (conn *TCPConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	select {
	case <-conn.ready: // wait until initialization
		tcpaddr, err := net.ResolveTCPAddr("tcp", addr.String())
		if err != nil {
			return 0, err
		}

		buf := gopacket.NewSerializeBuffer()
		opts := gopacket.SerializeOptions{
			FixLengths:       true,
			ComputeChecksums: true,
		}
		tcp := &layers.TCP{
			SrcPort: layers.TCPPort(conn.tcpconn.LocalAddr().(*net.TCPAddr).Port),
			DstPort: layers.TCPPort(tcpaddr.Port),
			Window:  uint16(source.Int63()),
			Ack:     atomic.LoadUint32(&conn.ack),
			Seq:     atomic.LoadUint32(&conn.seq),
			PSH:     true,
			ACK:     true,
		}
		tcp.SetNetworkLayerForChecksum(conn.networkLayer.(gopacket.NetworkLayer))
		payload := gopacket.Payload(p)

		gopacket.SerializeLayers(buf, opts, conn.linkLayer, conn.networkLayer, tcp, payload)
		if err := conn.handle.WritePacketData(buf.Bytes()); err != nil {
			return 0, err
		}

		atomic.AddUint32(&conn.seq, uint32(len(p)))
		return len(p), nil
	case <-conn.die:
		return 0, io.EOF
	}
}

// Close closes the connection.
func (conn *TCPConn) Close() error {
	var err error
	conn.dieOnce.Do(func() {
		close(conn.die)
		conn.handle.Close()
		err = conn.tcpconn.Close()
	})
	return err
}

// LocalAddr returns the local network address.
func (conn *TCPConn) LocalAddr() net.Addr { return conn.tcpconn.LocalAddr() }

// SetDeadline implements the Conn SetDeadline method.
func (conn *TCPConn) SetDeadline(t time.Time) error { return errOpNotImplemented }

// SetReadDeadline implements the Conn SetReadDeadline method.
func (conn *TCPConn) SetReadDeadline(t time.Time) error { return errOpNotImplemented }

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (conn *TCPConn) SetWriteDeadline(t time.Time) error { return errOpNotImplemented }

// tcp flow information
type tcpFlow struct {
	seq uint32
	ack uint32
}

// Listener defines a TCP-packet oriented listener connection
type Listener struct {
	ready    chan struct{}
	die      chan struct{}
	dieOnce  sync.Once
	listener *net.TCPListener

	// gopacket
	handle       *pcap.Handle
	packetSource *gopacket.PacketSource
	chPacket     chan Packet                // incoming packets channel
	linkLayer    gopacket.SerializableLayer // link layer header
	networkLayer gopacket.SerializableLayer // network layer header

	// important TCP header information
	flows     map[string]tcpFlow
	flowsLock sync.Mutex
}

// TCPListener returns a TCP-packet oriented listener
func Listen(network, address string) (*Listener, error) {
	laddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// get iface name from the dummy connection, eg. eth0, lo0
	ifaces, err := pcap.FindAllDevs()
	if err != nil {
		return nil, err
	}

	var ifaceName string
	for _, iface := range ifaces {
		for _, addr := range iface.Addresses {
			if addr.IP.Equal(laddr.IP) {
				ifaceName = iface.Name
			}
		}
	}
	if ifaceName == "" {
		return nil, errors.New("cannot find correct interface")
	}

	// pcap init
	handle, err := pcap.OpenLive(ifaceName, 65536, true, time.Second)
	if err != nil {
		return nil, err
	}

	// start listening
	l, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	// apply filter for incoming data
	filter := fmt.Sprintf("tcp and dst host %v and dst port %v", laddr.IP, laddr.Port)
	if err := handle.SetBPFFilter(filter); err != nil {
		return nil, err
	}

	// fields
	conn := new(Listener)
	conn.handle = handle
	conn.flows = make(map[string]tcpFlow)
	conn.die = make(chan struct{})
	conn.listener = l
	conn.startCapture(gopacket.NewPacketSource(handle, handle.LinkType()))

	// discard everything in original connection
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			// prevent conn from sending ACKs
			if laddr.IP.To4() == nil {
				ipv6.NewConn(conn).SetHopLimit(0)
			} else {
				ipv4.NewConn(conn).SetTTL(0)
			}

			go io.Copy(ioutil.Discard, conn)
		}
	}()

	return conn, nil
}

// Close closes the connection.
func (conn *Listener) Close() error {
	var err error
	conn.dieOnce.Do(func() {
		close(conn.die)
		conn.handle.Close()
		err = conn.listener.Close()
	})
	return err
}

// LocalAddr returns the local network address.
func (conn *Listener) LocalAddr() net.Addr { return conn.listener.Addr() }

// SetDeadline implements the Conn SetDeadline method.
func (conn *Listener) SetDeadline(t time.Time) error { return errOpNotImplemented }

// SetReadDeadline implements the Conn SetReadDeadline method.
func (conn *Listener) SetReadDeadline(t time.Time) error { return errOpNotImplemented }

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (conn *Listener) SetWriteDeadline(t time.Time) error { return errOpNotImplemented }

func (conn *Listener) lockflow(addr net.Addr, f func(*tcpFlow)) {
	conn.flowsLock.Lock()
	e := conn.flows[addr.String()]
	f(&e)
	conn.flows[addr.String()] = e
	conn.flowsLock.Unlock()
}

// packet startCapture
func (conn *Listener) startCapture(source *gopacket.PacketSource) {
	conn.chPacket = make(chan Packet)
	conn.ready = make(chan struct{})

	go func() {
		var once sync.Once
		for packet := range source.Packets() {
			transport := packet.TransportLayer().(*layers.TCP)
			var ip []byte
			if layer := packet.Layer(layers.LayerTypeIPv4); layer != nil {
				network := layer.(*layers.IPv4)
				ip = make([]byte, len(network.SrcIP))
				copy(ip, network.SrcIP)
			} else if layer := packet.Layer(layers.LayerTypeIPv6); layer != nil {
				network := layer.(*layers.IPv6)
				ip = make([]byte, len(network.SrcIP))
				copy(ip, network.SrcIP)
			}
			addr := &net.TCPAddr{IP: ip, Port: int(transport.SrcPort)}

			conn.lockflow(addr, func(e *tcpFlow) {
				e.seq = transport.Ack // seq update
			})

			once.Do(func() {
				// link layer
				if layer := packet.Layer(layers.LayerTypeEthernet); layer != nil {
					ethLayer := layer.(*layers.Ethernet)
					conn.linkLayer = &layers.Ethernet{
						EthernetType: ethLayer.EthernetType,
						SrcMAC:       ethLayer.DstMAC,
						DstMAC:       ethLayer.SrcMAC,
					}
				} else if layer := packet.Layer(layers.LayerTypeLoopback); layer != nil {
					loopLayer := layer.(*layers.Loopback)
					conn.linkLayer = &layers.Loopback{Family: loopLayer.Family}
				}

				// network layer
				if layer := packet.Layer(layers.LayerTypeIPv4); layer != nil {
					network := layer.(*layers.IPv4)
					conn.networkLayer = &layers.IPv4{
						SrcIP:    network.DstIP,
						DstIP:    network.SrcIP,
						Protocol: network.Protocol,
						Version:  network.Version,
						Id:       network.Id,
						Flags:    layers.IPv4DontFragment,
						TTL:      0x40,
					}
				} else if layer := packet.Layer(layers.LayerTypeIPv6); layer != nil {
					network := layer.(*layers.IPv6)
					conn.networkLayer = &layers.IPv6{
						Version:    network.Version,
						NextHeader: network.NextHeader,
						SrcIP:      network.DstIP,
						DstIP:      network.SrcIP,
						HopLimit:   0x40,
					}
				}

				// ISN
				conn.lockflow(addr, func(e *tcpFlow) { e.ack = transport.Seq })

				close(conn.ready)
			})

			if transport.SYN {
				conn.lockflow(addr, func(e *tcpFlow) { e.ack++ })
			} else if transport.PSH {
				conn.lockflow(addr, func(e *tcpFlow) { e.ack += uint32(len(transport.Payload)) })
				select {
				case conn.chPacket <- Packet{transport.Payload, addr}:
				case <-conn.die:
					return
				}
			} else if transport.FIN {
				conn.lockflow(addr, func(e *tcpFlow) { delete(conn.flows, addr.String()) })
			}
		}
	}()
}

// ReadFrom implements the PacketConn ReadFrom method.
func (conn *Listener) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case <-conn.die:
		return 0, nil, io.EOF
	case packet := <-conn.chPacket:
		n = copy(p, packet.bts)
		return n, packet.addr, nil
	}
}

// WriteTo implements the PacketConn WriteTo method.
func (conn *Listener) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	<-conn.ready
	tcpaddr, err := net.ResolveTCPAddr("tcp", addr.String())
	if err != nil {
		return 0, err
	}

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}

	var flow tcpFlow
	conn.lockflow(addr, func(e *tcpFlow) {
		flow = *e
	})

	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(conn.listener.Addr().(*net.TCPAddr).Port),
		DstPort: layers.TCPPort(tcpaddr.Port),
		Window:  12580,
		Ack:     flow.ack,
		Seq:     flow.seq,
		PSH:     true,
		ACK:     true,
	}

	tcp.SetNetworkLayerForChecksum(conn.networkLayer.(gopacket.NetworkLayer))

	payload := gopacket.Payload(p)

	gopacket.SerializeLayers(buf, opts, conn.linkLayer, conn.networkLayer, tcp, payload)
	if err := conn.handle.WritePacketData(buf.Bytes()); err != nil {
		return 0, err
	}

	conn.lockflow(addr, func(e *tcpFlow) { e.seq += uint32(len(p)) })
	return len(p), nil
}
