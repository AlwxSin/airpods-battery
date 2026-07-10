package main

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/godbus/dbus/v5"
)

const (
	rootPath      = dbus.ObjectPath("/org/airpods/battery")
	batteryIface  = "org.bluez.BatteryProvider1"
	providerMgr   = "org.bluez.BatteryProviderManager1"
	adapterPath   = dbus.ObjectPath("/org/bluez/hci0")
	bluezService  = "org.bluez"
	bluezBatIface = "org.bluez.Battery1"
	objMgrIface   = "org.freedesktop.DBus.ObjectManager"
	propsIface    = "org.freedesktop.DBus.Properties"
)

type entry struct {
	objPath    dbus.ObjectPath
	devicePath dbus.ObjectPath
	percentage uint8
}

type daemon struct {
	conn        *dbus.Conn
	mu          sync.Mutex
	entries     map[string]*entry  // addr -> entry
	aapMu       sync.Mutex
	aapSessions map[string]*aapSession // addr -> session
}

// propHandler is exported per battery object for org.freedesktop.DBus.Properties.
type propHandler struct {
	d    *daemon
	addr string
}

func (p *propHandler) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface != batteryIface {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", nil)
	}
	p.d.mu.Lock()
	e := p.d.entries[p.addr]
	p.d.mu.Unlock()
	if e == nil {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
	}
	switch prop {
	case "Percentage":
		return dbus.MakeVariant(e.percentage), nil
	case "Device":
		return dbus.MakeVariant(e.devicePath), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
}

func (p *propHandler) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != batteryIface {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", nil)
	}
	p.d.mu.Lock()
	e := p.d.entries[p.addr]
	p.d.mu.Unlock()
	if e == nil {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
	}
	return map[string]dbus.Variant{
		"Percentage": dbus.MakeVariant(e.percentage),
		"Device":     dbus.MakeVariant(e.devicePath),
	}, nil
}

func (p *propHandler) Set(_, _ string, _ dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", nil)
}

// GetManagedObjects implements org.freedesktop.DBus.ObjectManager.
func (d *daemon) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make(map[dbus.ObjectPath]map[string]map[string]dbus.Variant)
	for _, e := range d.entries {
		result[e.objPath] = map[string]map[string]dbus.Variant{
			batteryIface: {
				"Percentage": dbus.MakeVariant(e.percentage),
				"Device":     dbus.MakeVariant(e.devicePath),
			},
		}
	}
	return result, nil
}

func addrToDevPath(addr string) dbus.ObjectPath {
	return dbus.ObjectPath("/org/bluez/hci0/dev_" + strings.ReplaceAll(addr, ":", "_"))
}

func addrToObjPath(addr string) dbus.ObjectPath {
	return dbus.ObjectPath(string(rootPath) + "/dev_" + strings.ReplaceAll(addr, ":", "_"))
}

func pathToAddr(path dbus.ObjectPath) string {
	s := string(path)
	idx := strings.LastIndex(s, "/dev_")
	if idx < 0 {
		return ""
	}
	return strings.ReplaceAll(s[idx+5:], "_", ":")
}

// addLocked registers a new entry. Caller must hold d.mu.
func (d *daemon) addLocked(addr string, pct uint8) {
	e := &entry{
		objPath:    addrToObjPath(addr),
		devicePath: addrToDevPath(addr),
		percentage: pct,
	}
	d.entries[addr] = e
	ph := &propHandler{d: d, addr: addr}
	if err := d.conn.Export(ph, e.objPath, propsIface); err != nil {
		log.Printf("export props %s: %v", addr, err)
	}
}

func (d *daemon) updateOrAdd(addr string, pct uint8) {
	d.mu.Lock()
	e, exists := d.entries[addr]
	if !exists {
		d.addLocked(addr, pct)
		e = d.entries[addr]
		d.mu.Unlock()
		if err := d.conn.Emit(rootPath, objMgrIface+".InterfacesAdded",
			e.objPath,
			map[string]map[string]dbus.Variant{
				batteryIface: {
					"Percentage": dbus.MakeVariant(e.percentage),
					"Device":     dbus.MakeVariant(e.devicePath),
				},
			},
		); err != nil {
			log.Printf("emit InterfacesAdded %s: %v", addr, err)
		}
		log.Printf("added %s at %d%%", addr, pct)
		return
	}
	if e.percentage == pct {
		d.mu.Unlock()
		return
	}
	e.percentage = pct
	d.mu.Unlock()
	if err := d.conn.Emit(e.objPath, propsIface+".PropertiesChanged",
		batteryIface,
		map[string]dbus.Variant{"Percentage": dbus.MakeVariant(pct)},
		[]string{},
	); err != nil {
		log.Printf("emit PropertiesChanged %s: %v", addr, err)
	}
	log.Printf("updated %s to %d%%", addr, pct)
}

func (d *daemon) removeDevice(addr string) {
	d.mu.Lock()
	_, existed := d.entries[addr]
	delete(d.entries, addr)
	d.mu.Unlock()
	// Emit even without a local entry: bluetoothd can silently miss a
	// removal, and the stale battery would otherwise survive until the
	// daemon exits.
	if err := d.conn.Emit(rootPath, objMgrIface+".InterfacesRemoved",
		addrToObjPath(addr),
		[]string{batteryIface},
	); err != nil {
		log.Printf("emit InterfacesRemoved %s: %v", addr, err)
	}
	if existed {
		log.Printf("removed %s", addr)
	}
}

func (d *daemon) enumerateDevices() {
	obj := d.conn.Object(bluezService, "/")
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := obj.Call(objMgrIface+".GetManagedObjects", 0).Store(&objects); err != nil {
		log.Printf("GetManagedObjects: %v", err)
		return
	}
	for path, ifaces := range objects {
		addr := pathToAddr(path)
		if addr == "" {
			continue
		}

		// BlueZ Battery1 (HFP or AVRCP): add to provider directly.
		if bat, ok := ifaces[bluezBatIface]; ok {
			if pctVar, ok := bat["Percentage"]; ok {
				if pct, ok := pctVar.Value().(uint8); ok {
					d.updateOrAdd(addr, pct)
				}
			}
		}

		// Device1: start AAP for connected Apple headphones.
		dev, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		connVar, ok := dev["Connected"]
		if !ok {
			continue
		}
		if connected, _ := connVar.Value().(bool); !connected {
			continue
		}
		modVar, ok := dev["Modalias"]
		if !ok {
			continue
		}
		modalias, _ := modVar.Value().(string)
		productID, ok := isAppleHeadphone(modalias)
		if !ok {
			continue
		}
		iconVar, _ := dev["Icon"]
		icon, _ := iconVar.Value().(string)
		if icon != "audio-headphones" && icon != "audio-headset" {
			continue
		}
		go d.tryConnectAAP(addr, productID)
	}
}

func (d *daemon) handleSignal(sig *dbus.Signal) {
	switch sig.Name {
	case propsIface + ".PropertiesChanged":
		if len(sig.Body) < 2 {
			return
		}
		iface, _ := sig.Body[0].(string)
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		addr := pathToAddr(sig.Path)
		if addr == "" {
			return
		}

		switch iface {
		case bluezBatIface:
			// HFP/AVRCP battery from BlueZ.
			pctVar, ok := changed["Percentage"]
			if !ok {
				return
			}
			pct, ok := pctVar.Value().(uint8)
			if !ok {
				return
			}
			d.updateOrAdd(addr, pct)

		case "org.bluez.Device1":
			// Device connected/disconnected.
			connVar, ok := changed["Connected"]
			if !ok {
				return
			}
			connected, _ := connVar.Value().(bool)
			if connected {
				go d.connectAppleAAP(addr)
			} else {
				d.disconnectAAP(addr)
				d.removeDevice(addr)
			}
		}

	case objMgrIface + ".InterfacesAdded":
		if len(sig.Body) < 2 {
			return
		}
		path, _ := sig.Body[0].(dbus.ObjectPath)
		ifaces, _ := sig.Body[1].(map[string]map[string]dbus.Variant)
		addr := pathToAddr(path)
		if addr == "" {
			return
		}

		// BlueZ Battery1 appeared (e.g. HFP connected during a call).
		if bat, ok := ifaces[bluezBatIface]; ok {
			if pctVar, ok := bat["Percentage"]; ok {
				if pct, ok := pctVar.Value().(uint8); ok {
					d.updateOrAdd(addr, pct)
				}
			}
		}

		// New device or device just connected — try AAP.
		if dev, ok := ifaces["org.bluez.Device1"]; ok {
			if connVar, ok := dev["Connected"]; ok {
				if connected, _ := connVar.Value().(bool); connected {
					go d.connectAppleAAP(addr)
				}
			}
		}

	case objMgrIface + ".InterfacesRemoved":
		if len(sig.Body) < 2 {
			return
		}
		path, _ := sig.Body[0].(dbus.ObjectPath)
		ifaces, _ := sig.Body[1].([]string)
		addr := pathToAddr(path)
		if addr == "" {
			return
		}

		removeBattery := false
		removeDevice := false
		for _, iface := range ifaces {
			switch iface {
			case bluezBatIface:
				removeBattery = true
			case "org.bluez.Device1":
				removeDevice = true
			}
		}

		if removeDevice {
			// Full device removal: tear down everything.
			d.disconnectAAP(addr)
			d.removeDevice(addr)
		} else if removeBattery {
			// BlueZ Battery1 removed (e.g. HFP disconnected after call).
			// Only remove from provider if AAP is NOT providing battery.
			d.aapMu.Lock()
			hasAAP := d.aapSessions[addr] != nil
			d.aapMu.Unlock()
			if !hasAAP {
				d.removeDevice(addr)
			}
		}
	}
}

func main() {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		log.Fatalf("connect system bus: %v", err)
	}
	defer conn.Close()

	d := &daemon{
		conn:        conn,
		entries:     make(map[string]*entry),
		aapSessions: make(map[string]*aapSession),
	}

	if err := conn.Export(d, rootPath, objMgrIface); err != nil {
		log.Fatalf("export object manager: %v", err)
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(propsIface),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchSender(bluezService),
	); err != nil {
		log.Fatalf("match PropertiesChanged: %v", err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(objMgrIface),
		dbus.WithMatchMember("InterfacesAdded"),
		dbus.WithMatchSender(bluezService),
	); err != nil {
		log.Fatalf("match InterfacesAdded: %v", err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(objMgrIface),
		dbus.WithMatchMember("InterfacesRemoved"),
		dbus.WithMatchSender(bluezService),
	); err != nil {
		log.Fatalf("match InterfacesRemoved: %v", err)
	}

	sigCh := make(chan *dbus.Signal, 32)
	conn.Signal(sigCh)

	d.enumerateDevices()

	bluezObj := conn.Object(bluezService, adapterPath)
	if err := bluezObj.Call(providerMgr+".RegisterBatteryProvider", 0, rootPath).Err; err != nil {
		log.Fatalf("RegisterBatteryProvider: %v", err)
	}
	log.Printf("registered with BlueZ BatteryProviderManager at %s", rootPath)

	osSig := make(chan os.Signal, 1)
	signal.Notify(osSig, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case s := <-sigCh:
			d.handleSignal(s)
		case <-osSig:
			d.aapMu.Lock()
			sessions := make([]*aapSession, 0, len(d.aapSessions))
			for _, s := range d.aapSessions {
				sessions = append(sessions, s)
			}
			d.aapMu.Unlock()
			for _, s := range sessions {
				s.close()
			}
			bluezObj.Call(providerMgr+".UnregisterBatteryProvider", 0, rootPath)
			log.Printf("unregistered, exiting")
			return
		}
	}
}
