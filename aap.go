package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	afBluetooth  = 31
	btprotoL2CAP = 0
	l2capPSM     = 0x1001

	aapCmdBattery = 0x04

	aapBatTypeRight  = 0x02
	aapBatTypeLeft   = 0x04
	aapBatTypeSingle = 0x01
	aapBatTypeCase   = 0x08

	aapChrgDisconnected = 0x04
)

var (
	pktInit    = []byte{0x00, 0x00, 0x04, 0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	pktNotif1  = []byte{0x04, 0x00, 0x04, 0x00, 0x0f, 0x00, 0xff, 0xff, 0xef, 0xff}
	pktNotif2  = []byte{0x04, 0x00, 0x04, 0x00, 0x0f, 0x00, 0xff, 0xff, 0xff, 0xff}
	pktInitExt = []byte{0x04, 0x00, 0x04, 0x00, 0x4d, 0x00, 0x0e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
)

// l2sockaddr mirrors struct sockaddr_l2 from <bluetooth/l2cap.h> (14 bytes on amd64).
type l2sockaddr struct {
	family     uint16
	psm        uint16
	bdaddr     [6]byte
	cid        uint16
	bdaddrType uint8
	_          byte // pad to 14
}

// aapSession represents an open L2CAP/AAP connection to a device.
type aapSession struct {
	fd   int
	once sync.Once
	done chan struct{}
}

// close unblocks the reader goroutine and waits for it to exit.
func (s *aapSession) close() {
	s.once.Do(func() {
		syscall.Shutdown(s.fd, syscall.SHUT_RDWR)
	})
	<-s.done
}

// initExtSupported returns true for models that require the AapInitExt packet.
func initExtSupported(productID uint16) bool {
	switch productID {
	case 0x2014, // AirPods Pro 2
		0x2027, // AirPods Pro 3
		0x2024, // AirPods Pro USB-C
		0x201b, // AirPods 4 ANC
		0x202d: // AirPods Max 2
		return true
	}
	return false
}

// isAppleHeadphone returns the Apple product ID if the BlueZ Modalias indicates
// an Apple device (vendor 0x004C). Returns (0, false) otherwise.
func isAppleHeadphone(modalias string) (uint16, bool) {
	const prefix = "bluetooth:v004Cp"
	idx := strings.Index(modalias, prefix)
	if idx < 0 {
		return 0, false
	}
	s := modalias[idx+len(prefix):]
	if len(s) < 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[:4], 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// strToBA converts "20:F4:D4:95:2B:F9" to bdaddr_t (bytes stored in reverse order).
func strToBA(addr string) ([6]byte, error) {
	parts := strings.Split(addr, ":")
	if len(parts) != 6 {
		return [6]byte{}, fmt.Errorf("invalid address: %s", addr)
	}
	var b [6]byte
	for i, p := range parts {
		v, err := strconv.ParseUint(strings.TrimSpace(p), 16, 8)
		if err != nil {
			return [6]byte{}, err
		}
		b[5-i] = byte(v)
	}
	return b, nil
}

// l2capDial opens a SEQPACKET L2CAP socket and connects to addr on the given PSM.
func l2capDial(addr string, psm uint16) (int, error) {
	fd, err := syscall.Socket(afBluetooth, syscall.SOCK_SEQPACKET, btprotoL2CAP)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}

	bdaddr, err := strToBA(addr)
	if err != nil {
		syscall.Close(fd)
		return -1, err
	}

	sa := l2sockaddr{
		family: afBluetooth,
		psm:    psm,
		bdaddr: bdaddr,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&sa)),
		unsafe.Sizeof(sa),
	)
	if errno != 0 {
		syscall.Close(fd)
		return -1, fmt.Errorf("connect: %w", errno)
	}
	return fd, nil
}

// parseBattery parses an AAP packet and returns the representative battery
// percentage for the device. Returns (0, false) if not a battery packet.
//
// From AapBatteryWatcher.h: each entry is 5 bytes starting at data[7]:
//
//	[+0] type: 0x01=Single 0x02=Right 0x04=Left 0x08=Case
//	[+1] unknown
//	[+2] battery 0–100
//	[+3] charging: 0x00=Undefined 0x01=Charging 0x02=NotCharging 0x04=Disconnected
//	[+4] unknown
func parseBattery(data []byte) (uint8, bool) {
	if len(data) < 11 {
		return 0, false
	}
	if data[4] != aapCmdBattery {
		return 0, false
	}

	count := int(data[6])
	off := 7

	type slot struct{ bat uint8 }
	var earSlots []slot

	for i := 0; i < count; i++ {
		if off+4 > len(data) {
			break
		}
		typ := data[off]
		bat := data[off+2]
		chrg := data[off+3]
		off += 5

		if typ == aapBatTypeCase {
			continue // exclude case from the upower value
		}
		if chrg == aapChrgDisconnected {
			continue
		}
		if bat == 0 || bat > 100 {
			continue
		}
		earSlots = append(earSlots, slot{bat})
	}

	if len(earSlots) == 0 {
		return 0, false
	}

	// Return the minimum — the earbud that's running out first.
	min := earSlots[0].bat
	for _, s := range earSlots[1:] {
		if s.bat < min {
			min = s.bat
		}
	}
	return min, true
}

// fdWrite writes all bytes to the file descriptor.
func fdWrite(fd int, data []byte) {
	for len(data) > 0 {
		n, err := syscall.Write(fd, data)
		if err != nil {
			return
		}
		data = data[n:]
	}
}

const aapConnectAttempts = 5

// startAAP dials L2CAP, performs the AAP handshake, and starts a background
// goroutine that calls onBattery for each battery update.
func startAAP(addr string, productID uint16, onBattery func(uint8)) (*aapSession, error) {
	var fd int
	var err error
	for i := 0; i < aapConnectAttempts; i++ {
		fd, err = l2capDial(addr, l2capPSM)
		if err == nil {
			break
		}
		log.Printf("AAP %s connect attempt %d: %v", addr, i+1, err)
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}

	s := &aapSession{
		fd:   fd,
		done: make(chan struct{}),
	}

	go func() {
		defer close(s.done)
		defer syscall.Close(fd)

		fdWrite(fd, pktInit)
		fdWrite(fd, pktNotif1)
		fdWrite(fd, pktNotif2)
		if initExtSupported(productID) {
			fdWrite(fd, pktInitExt)
		}

		buf := make([]byte, 1024)
		for {
			n, err := syscall.Read(fd, buf)
			if err != nil || n == 0 {
				break
			}
			if pct, ok := parseBattery(buf[:n]); ok {
				onBattery(pct)
			}
		}
		log.Printf("AAP session closed for %s", addr)
	}()

	return s, nil
}

// tryConnectAAP starts an AAP session for the device. Any existing session is
// replaced. Safe to call from multiple goroutines.
func (d *daemon) tryConnectAAP(addr string, productID uint16) {
	d.aapMu.Lock()
	old := d.aapSessions[addr]
	delete(d.aapSessions, addr)
	d.aapMu.Unlock()

	if old != nil {
		old.close()
	}

	session, err := startAAP(addr, productID, func(pct uint8) {
		d.updateOrAdd(addr, pct)
	})
	if err != nil {
		log.Printf("AAP %s: %v", addr, err)
		return
	}

	d.aapMu.Lock()
	d.aapSessions[addr] = session
	d.aapMu.Unlock()
	log.Printf("AAP connected %s (product 0x%04x)", addr, productID)
}

// disconnectAAP closes the AAP session for addr, if any.
func (d *daemon) disconnectAAP(addr string) {
	d.aapMu.Lock()
	s := d.aapSessions[addr]
	delete(d.aapSessions, addr)
	d.aapMu.Unlock()

	if s != nil {
		s.close()
		log.Printf("AAP disconnected %s", addr)
	}
}

// connectAppleAAP fetches Device1 properties for addr and starts AAP if it's
// an Apple headphone. Runs D-Bus calls, so must be called in its own goroutine.
func (d *daemon) connectAppleAAP(addr string) {
	devPath := addrToDevPath(addr)
	obj := d.conn.Object(bluezService, devPath)

	var modalias string
	if err := obj.Call(propsIface+".Get", 0, "org.bluez.Device1", "Modalias").Store(&modalias); err != nil {
		return
	}
	productID, ok := isAppleHeadphone(modalias)
	if !ok {
		return
	}

	var icon string
	obj.Call(propsIface+".Get", 0, "org.bluez.Device1", "Icon").Store(&icon)
	if icon != "audio-headphones" && icon != "audio-headset" {
		return
	}

	d.tryConnectAAP(addr, productID)
}
