package water

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"water/winipcfg"
)

var (
	WintunStaticRequestedGUID = &windows.GUID{
		Data2: 0xFFFF,
		Data3: 0xFFFF,
		Data4: [8]byte{0xFF, 0xe9, 0x76, 0xe5, 0x8c, 0x74, 0x06, 0x3e},
	}
	WintunTunnelType = "WireGuard"
)

type WinTun struct {
	tun *NativeTun
}

func (w *WinTun) Read(p []byte) (n int, err error) {
	return w.tun.Read(p, 0)
}

func (w *WinTun) Write(p []byte) (n int, err error) {
	return w.tun.Write(p, 0)
}

func (w *WinTun) Close() error {
	return w.tun.Close()
}

func openTunDev(config Config) (*Interface, error) {
	tun, err := CreateTUNWithRequestedGUID(config.InterfaceName, WintunStaticRequestedGUID, 0)
	if err != nil {
		return nil, err
	}
	network := config.PlatformSpecificParams.Network
	ipPrefix, err := netip.ParsePrefix(network)
	if err != nil {
		return nil, err
	}
	link := winipcfg.LUID(tun.LUID())
	err = link.SetIPAddresses(ipPrefix)
	if err != nil {
		return nil, err
	}
	return &Interface{
		isTAP:           false,
		ReadWriteCloser: &WinTun{tun: tun},
		name:            config.InterfaceName,
	}, nil
}

const (
	rateMeasurementGranularity = uint64((Second / 2) / Nanosecond)
	spinloopRateThreshold      = 800000000 / 8                         // 800mbps
	spinloopDuration           = uint64(Millisecond / 80 / Nanosecond) // ~1gbit/s
)

type Event int

const (
	EventUp = 1 << iota
	EventDown
	EventMTUUpdate
)

type NativeTun struct {
	wt     *wintun.Adapter
	name   string
	handle windows.Handle

	session   wintun.Session
	readWait  windows.Handle
	events    chan Event
	running   sync.WaitGroup
	closeOnce sync.Once
	close     atomic.Value
	forcedMTU int
}

//go:linkname procyield runtime.procyield
func procyield(cycles uint32)

//go:linkname nanotime runtime.nanotime
func nanotime() int64

// CreateTUNWithRequestedGUID creates a Wintun interface with the given name and
// a requested GUID. Should a Wintun interface with the same name exist, it is reused.
func CreateTUNWithRequestedGUID(ifname string, requestedGUID *windows.GUID, mtu int) (*NativeTun, error) {
	wt, err := wintun.CreateAdapter(ifname, WintunTunnelType, requestedGUID)
	if err != nil {
		return nil, fmt.Errorf("Error creating interface: %w", err)
	}

	forcedMTU := 1420
	if mtu > 0 {
		forcedMTU = mtu
	}

	tun := &NativeTun{
		wt:        wt,
		name:      ifname,
		handle:    windows.InvalidHandle,
		events:    make(chan Event, 10),
		forcedMTU: forcedMTU,
	}

	tun.session, err = wt.StartSession(0x800000) // Ring capacity, 8 MiB
	if err != nil {
		tun.wt.Close()
		close(tun.events)
		return nil, fmt.Errorf("Error starting session: %w", err)
	}
	tun.readWait = tun.session.ReadWaitEvent()
	return tun, nil
}

func (tun *NativeTun) Name() (string, error) {
	return tun.name, nil
}

func (tun *NativeTun) File() *os.File {
	return nil
}

func (tun *NativeTun) Events() chan Event {
	return tun.events
}

func (tun *NativeTun) Close() error {
	var err error
	tun.closeOnce.Do(func() {
		tun.close.Store(true)
		windows.SetEvent(tun.readWait)
		tun.running.Wait()
		tun.session.End()
		if tun.wt != nil {
			tun.wt.Close()
		}
		close(tun.events)
	})
	return err
}

func (tun *NativeTun) MTU() (int, error) {
	return tun.forcedMTU, nil
}

// TODO: This is a temporary hack. We really need to be monitoring the interface in real time and adapting to MTU changes.
func (tun *NativeTun) ForceMTU(mtu int) {
	update := tun.forcedMTU != mtu
	tun.forcedMTU = mtu
	if update {
		tun.events <- EventMTUUpdate
	}
}

// Note: Read() and Write() assume the caller comes only from a single thread; there's no locking.

func (tun *NativeTun) Read(buff []byte, offset int) (int, error) {
	tun.running.Add(1)
	defer tun.running.Done()
retry:
	if tun.close.Load().(bool) {
		return 0, os.ErrClosed
	}
	start := nanotime()
	for {
		if tun.close.Load().(bool) {
			return 0, os.ErrClosed
		}
		packet, err := tun.session.ReceivePacket()
		switch err {
		case nil:
			packetSize := len(packet)
			copy(buff[offset:], packet)
			tun.session.ReleaseReceivePacket(packet)

			return packetSize, nil
		case windows.ERROR_NO_MORE_ITEMS:
			if uint64(nanotime()-start) >= spinloopDuration {
				windows.WaitForSingleObject(tun.readWait, windows.INFINITE)
				goto retry
			}
			procyield(1)
			continue
		case windows.ERROR_HANDLE_EOF:
			return 0, os.ErrClosed
		case windows.ERROR_INVALID_DATA:
			return 0, errors.New("Send ring corrupt")
		}
		return 0, fmt.Errorf("Read failed: %w", err)
	}
}

func (tun *NativeTun) Flush() error {
	return nil
}

func (tun *NativeTun) Write(buff []byte, offset int) (int, error) {
	tun.running.Add(1)
	defer tun.running.Done()
	if tun.close.Load().(bool) {
		return 0, os.ErrClosed
	}

	packetSize := len(buff) - offset

	packet, err := tun.session.AllocateSendPacket(packetSize)
	if err == nil {
		copy(packet, buff[offset:])
		tun.session.SendPacket(packet)
		return packetSize, nil
	}
	switch err {
	case windows.ERROR_HANDLE_EOF:
		return 0, os.ErrClosed
	case windows.ERROR_BUFFER_OVERFLOW:
		return 0, nil // Dropping when ring is full.
	}
	return 0, fmt.Errorf("Write failed: %w", err)
}

// LUID returns Windows interface instance ID.
func (tun *NativeTun) LUID() uint64 {
	tun.running.Add(1)
	defer tun.running.Done()
	if tun.close.Load().(bool) {
		return 0
	}
	return tun.wt.LUID()
}

// RunningVersion returns the running version of the Wintun driver.
func (tun *NativeTun) RunningVersion() (version uint32, err error) {
	return wintun.RunningVersion()
}
