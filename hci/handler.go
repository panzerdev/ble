package hci

import (
	"fmt"
	"sync"

	log "github.com/Sirupsen/logrus"

	"github.com/currantlabs/bt/buffer"
	"github.com/currantlabs/bt/hci/cmd"
	"github.com/currantlabs/bt/hci/evt"
	"github.com/currantlabs/bt/l2cap"
)

type dispatcher struct {
	sync.Mutex
	handlers map[int]Handler
}

func (d *dispatcher) Handler(c int) Handler {
	d.Lock()
	defer d.Unlock()
	return d.handlers[c]
}

func (d *dispatcher) SetHandler(c int, f Handler) Handler {
	d.Lock()
	defer d.Unlock()
	old := d.handlers[c]
	d.handlers[c] = f
	return old
}

func (d *dispatcher) dispatch(b []byte) {
	d.Lock()
	defer d.Unlock()
	code, plen := int(b[0]), int(b[1])
	if plen != len(b[2:]) {
		log.Warnf("hci: corrupt event packet: [ % X ]", b)
	}
	if f, found := d.handlers[code]; found {
		go f.Handle(b[2:])
		return
	}
	log.Errorf("hci: unsupported event packet: [ % X ]", b)
}

func (h *hci) handleACLData(b []byte) {
	a := l2cap.Packet(b)
	h.muConns.Lock()
	c, ok := h.conns[a.Handle()]
	h.muConns.Unlock()
	if !ok {
		return
	}
	c.HandlePacket(a)
}

func (h *hci) handleCommandComplete(b []byte) {
	var e evt.CommandCompleteEvent
	if err := e.Unmarshal(b); err != nil {
		return
	}
	for i := 0; i < int(e.NumHCICommandPackets); i++ {
		h.chCmdBufs <- make([]byte, 64)
	}
	if e.CommandOpcode == 0x0000 {
		// NOP command, used for flow control purpose [Vol 2, Part E, 4.4]
		return
	}
	p, found := h.sentCmds[int(e.CommandOpcode)]
	if !found {
		log.Errorf("event: can't find the cmd for CommandCompleteEP: %v", e)
		return
	}
	p.done <- e.ReturnParameters
}

func (h *hci) handleCommandStatus(b []byte) {
	var e evt.CommandStatusEvent
	if err := e.Unmarshal(b); err != nil {
		return
	}
	for i := 0; i < int(e.NumHCICommandPackets); i++ {
		h.chCmdBufs <- make([]byte, 64)
	}
	p, found := h.sentCmds[int(e.CommandOpcode)]
	if !found {
		log.Errorf("event: can't find the cmd for CommandStatusEP: %v", e)
		return
	}
	close(p.done)
}

func (h *hci) handleLEMeta(b []byte) {
	code := int(b[0])
	if f := h.subevtHandlers.Handler(code); f != nil {
		f.Handle(b)
		return
	}
	log.Printf("Unsupported LE event: [ % X ]", b)
}

func (h *hci) handleLEConnectionComplete(b []byte) {
	e := &evt.LEConnectionCompleteEvent{}
	if err := e.Unmarshal(b); err != nil {
		return
	}

	c := l2cap.NewConn(h, h.dev, buffer.NewClient(h.pool), h.addr, e)
	h.muConns.Lock()
	h.conns[e.ConnectionHandle] = c
	h.muConns.Unlock()
	h.chConn <- c
}

func (h *hci) handleLEConnectionUpdateComplete(b []byte) {
	e := &evt.LEConnectionUpdateCompleteEvent{}
	if err := e.Unmarshal(b); err != nil {
		return
	}
}
func (h *hci) handleDisconnectionComplete(b []byte) {
	e := &evt.DisconnectionCompleteEvent{}
	if err := e.Unmarshal(b); err != nil {
		return
	}
	h.muConns.Lock()
	c, found := h.conns[e.ConnectionHandle]
	delete(h.conns, e.ConnectionHandle)
	h.muConns.Unlock()
	if !found {
		log.Errorf("conns: disconnecting an invalid handle %04X", e.ConnectionHandle)
		return
	}
	c.Disconnect()
}

func (h *hci) handleNumberOfCompletedPackets(b []byte) {
	e := &evt.NumberOfCompletedPacketsEvent{}
	if err := e.Unmarshal(b); err != nil {
		return
	}
	for i := 0; i < int(e.NumberOfHandles); i++ {
		h.muConns.Lock()
		// FIXME: check the race condition between disconnection and this event
		c, ok := h.conns[e.ConnectionHandle[i]]
		if !ok {
			h.muConns.Unlock()
			return
		}

		h.muConns.Unlock()
		// Add the HCI buffer to the per-connection list. When written buffers are acked by
		// the controller via NumberOfCompletedPackets event, we'll put them back to the pool.
		// When a connection disconnects, all the sent packets and weren't acked yet
		// will be recylecd. [Vol2, Part E 4.1.1]
		for j := 0; j < int(e.HCNumOfCompletedPackets[i]); j++ {
			c.TxBuffers().Free()
		}
	}
}

func (h *hci) handleLELongTermKeyRequest(b []byte) {
	e := &evt.LELongTermKeyRequestEvent{}
	if err := e.Unmarshal(b); err != nil {
		return
	}

	h.Send(&cmd.LELongTermKeyRequestNegativeReply{
		ConnectionHandle: e.ConnectionHandle,
	}, nil)
}

func (h *hci) handleLEAdvertisingReport(p []byte) {
	e := &evt.LEAdvertisingReportEvent{}
	if err := e.Unmarshal(p); err != nil {
		return
	}
	f := func(a [6]byte) string {
		return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", a[5], a[4], a[3], a[2], a[1], a[0])
	}
	for i := 0; i < int(e.NumReports); i++ {
		log.Printf("%d, %d, %s, %d, [% X]",
			e.EventType[i], e.AddressType[i], f(e.Address[i]), e.RSSI[i], e.Data[i])
	}
}
