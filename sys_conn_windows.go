//go:build windows

package quic

import (
	"encoding/binary"
	"errors"
	"log"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/windows"
)

// TO DO: Check if these are correct
const (
	ecnMask         = 0x3
	oobBufferSize   = 128
	IP_RECVECN      = 0x32 // https://microsoft.github.io/windows-docs-rs/doc/windows/Win32/Networking/WinSock/constant.IP_RECVECN.html
	IPV6_RECVECN    = 0x32 // https://microsoft.github.io/windows-docs-rs/doc/windows/Win32/Networking/WinSock/constant.IPV6_RECVECN.html
	IPV6_RECVTCLASS = 0x28 // https://github.com/tpn/winsdk-10/blob/master/Include/10.0.14393.0/shared/ws2ipdef.h
)

type batchConn interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
}

func newConn(c OOBCapablePacketConn, supportsDF bool) (*oobConn, error) {
	rawConn, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var needsPacketInfo bool
	if udpAddr, ok := c.LocalAddr().(*net.UDPAddr); ok && udpAddr.IP.IsUnspecified() {
		needsPacketInfo = true
	}
	// rawConn may be IPv4, IPv6 or both.
	var errECNIPv4, errECNIPv6, errPIIPv4, errPIIPv6 error
	if err := rawConn.Control(func(fd uintptr) {
		errECNIPv4 = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, IP_RECVECN, 1)
		errECNIPv6 = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, IPV6_RECVECN, 1)

		if needsPacketInfo {
			errPIIPv4 = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_PKTINFO, 1)
			errPIIPv6 = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, windows.IPV6_PKTINFO, 1)
		}
	}); err != nil {
		return nil, err
	}
	switch {
	case errECNIPv4 == nil && errECNIPv6 == nil:
		utils.DefaultLogger.Debugf("Activating reading of ECN bits for IPv4 and IPv6.")
	case errECNIPv4 == nil && errECNIPv6 != nil:
		utils.DefaultLogger.Debugf("Activating reading of ECN bits for IPv4.")
	case errECNIPv4 != nil && errECNIPv6 == nil:
		utils.DefaultLogger.Debugf("Activating reading of ECN bits for IPv6.")
	case errECNIPv4 != nil && errECNIPv6 != nil:
		return nil, errors.New("activating ECN failed for both IPv4 and IPv6")
	}
	if needsPacketInfo {
		switch {
		case errPIIPv4 == nil && errPIIPv6 == nil:
			utils.DefaultLogger.Debugf("Activating reading of packet info for IPv4 and IPv6.")
		case errPIIPv4 == nil && errPIIPv6 != nil:
			utils.DefaultLogger.Debugf("Activating reading of packet info bits for IPv4.")
		case errPIIPv4 != nil && errPIIPv6 == nil:
			utils.DefaultLogger.Debugf("Activating reading of packet info bits for IPv6.")
		case errPIIPv4 != nil && errPIIPv6 != nil:
			return nil, errors.New("activating packet info failed for both IPv4 and IPv6")
		}
	}

	// Allows callers to pass in a connection that already satisfies batchConn interface
	// to make use of the optimisation. Otherwise, ipv4.NewPacketConn would unwrap the file descriptor
	// via SyscallConn(), and read it that way, which might not be what the caller wants.
	var bc batchConn
	if ibc, ok := c.(batchConn); ok {
		bc = ibc
	} else {
		bc = ipv4.NewPacketConn(c)
	}

	msgs := make([]ipv4.Message, batchSize)
	for i := range msgs {
		// preallocate the [][]byte
		msgs[i].Buffers = make([][]byte, 1)
	}
	oobConn := &oobConn{
		OOBCapablePacketConn: c,
		batchConn:            bc,
		messages:             msgs,
		readPos:              batchSize,
		cap: connCapabilities{
			DF:  supportsDF,
			GSO: isGSOEnabled(rawConn),
			ECN: isECNEnabled(),
		},
	}
	for i := 0; i < batchSize; i++ {
		oobConn.messages[i].OOB = make([]byte, oobBufferSize)
	}
	return oobConn, nil
}

var invalidCmsgOnceV4, invalidCmsgOnceV6 sync.Once

func (c *oobConn) ReadPacket() (receivedPacket, error) {
	if len(c.messages) == int(c.readPos) { // all messages read. Read the next batch of messages.
		c.messages = c.messages[:batchSize] // what is happening here?
		// replace buffers data buffers up to the packet that has been consumed during the last ReadBatch call
		for i := uint8(0); i < c.readPos; i++ {
			buffer := getPacketBuffer()
			buffer.Data = buffer.Data[:protocol.MaxPacketBufferSize]
			c.buffers[i] = buffer
			c.messages[i].Buffers[0] = c.buffers[i].Data
		}
		c.readPos = 0

		n, err := c.batchConn.ReadBatch(c.messages, 0)
		if n == 0 || err != nil {
			return receivedPacket{}, err
		}
		c.messages = c.messages[:n]
	}

	msg := c.messages[c.readPos]
	buffer := c.buffers[c.readPos]
	c.readPos++

	data := msg.OOB[:msg.NN]
	p := receivedPacket{
		remoteAddr: msg.Addr,
		rcvTime:    time.Now(),
		data:       msg.Buffers[0][:msg.N],
		buffer:     buffer,
	}
	for len(data) > 0 {
		hdr, body, remainder, err := ParseOneSocketControlMessage(data)
		if err != nil {
			return receivedPacket{}, err
		}
		if hdr.Level == windows.IPPROTO_IP {
			switch hdr.Type {
			case windows.IP_TOS:
				p.ecn = protocol.ParseECNHeaderBits(body[0] & ecnMask)
			case windows.IP_PKTINFO:
				ip, ifIndex, ok := parseIPv4PktInfo(body)
				if ok {
					p.info.addr = ip
					p.info.ifIndex = ifIndex
				} else {
					invalidCmsgOnceV4.Do(func() {
						log.Printf("Received invalid IPv4 packet info control message: %+x. "+
							"This should never occur, please open a new issue and include details about the architecture.", body)
					})
				}
			}
		}
		if hdr.Level == windows.IPPROTO_IPV6 {
			switch hdr.Type {
			case IPV6_RECVTCLASS:
				p.ecn = protocol.ParseECNHeaderBits(body[0] & ecnMask)
			case windows.IPV6_PKTINFO:
				// struct in6_pktinfo {
				// 	IN6_ADDR ipi6_addr;
				// 	ULONG    ipi6_ifindex;
				// };
				if len(body) == 20 {
					p.info.addr = netip.AddrFrom16(*(*[16]byte)(body[:16])).Unmap()
					p.info.ifIndex = binary.LittleEndian.Uint32(body[16:])
				} else {
					invalidCmsgOnceV6.Do(func() {
						log.Printf("Received invalid IPv6 packet info control message: %+x. "+
							"This should never occur, please open a new issue and include details about the architecture.", body)
					})
				}
			}
		}
		data = remainder
	}
	return p, nil
}

// WritePacket writes a new packet.
func (c *oobConn) WritePacket(b []byte, addr net.Addr, packetInfoOOB []byte, gsoSize uint16, ecn protocol.ECN) (int, error) {
	oob := packetInfoOOB
	if gsoSize > 0 {
		if !c.capabilities().GSO {
			panic("GSO disabled")
		}
		oob = appendUDPSegmentSizeMsg(oob, gsoSize)
	}
	if ecn != protocol.ECNUnsupported {
		if !c.capabilities().ECN {
			panic("tried to send an ECN-marked packet although ECN is disabled")
		}
		if remoteUDPAddr, ok := addr.(*net.UDPAddr); ok {
			if remoteUDPAddr.IP.To4() != nil {
				oob = appendIPv4ECNMsg(oob, ecn)
			} else {
				oob = appendIPv6ECNMsg(oob, ecn)
			}
		}
	}
	n, _, err := c.OOBCapablePacketConn.WriteMsgUDP(b, oob, addr.(*net.UDPAddr))
	return n, err
}

func (c *oobConn) capabilities() connCapabilities {
	return c.cap
}

func appendIPv4ECNMsg(b []byte, val protocol.ECN) []byte {
	startLen := len(b)
	const dataLen = 4
	b = append(b, make([]byte, cmsgLen(dataLen))...)
	h := (*Cmsghdr)(unsafe.Pointer(&b[startLen]))
	h.Level = windows.IPPROTO_IP
	h.Type = windows.IP_TOS
	h.Len = cmsgLen(dataLen)

	offset := startLen + int(cmsgSpace(0))
	b[offset] = val.ToHeaderBits()
	return b
}
func appendIPv6ECNMsg(b []byte, val protocol.ECN) []byte {
	startLen := len(b)
	const dataLen = 4
	b = append(b, make([]byte, cmsgLen(dataLen))...)
	h := (*Cmsghdr)(unsafe.Pointer(&b[startLen]))
	h.Level = windows.IPPROTO_IPV6
	h.Type = IPV6_RECVTCLASS
	h.Len = cmsgLen(dataLen)

	offset := startLen + int(cmsgSpace(0))
	b[offset] = val.ToHeaderBits()
	return b
}

func inspectReadBuffer(c syscall.RawConn) (int, error) {
	var size int
	var serr error
	if err := c.Control(func(fd uintptr) {
		size, serr = windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_RCVBUF)
	}); err != nil {
		return 0, err
	}
	return size, serr
}

func inspectWriteBuffer(c syscall.RawConn) (int, error) {
	var size int
	var serr error
	if err := c.Control(func(fd uintptr) {
		size, serr = windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_SNDBUF)
	}); err != nil {
		return 0, err
	}
	return size, serr
}

type oobConn struct {
	OOBCapablePacketConn
	batchConn batchConn

	readPos uint8
	// Packets received from the kernel, but not yet returned by ReadPacket().
	messages []ipv4.Message
	buffers  [batchSize]*packetBuffer

	cap connCapabilities
}

type packetInfo struct {
	addr    netip.Addr
	ifIndex uint32
}

func (i *packetInfo) OOB() []byte { return nil }
