// gw3-btproxy turns the Bluetooth chip of a Xiaomi Gateway 3 (ZNDMWG03LM) into an
// ESPHome-compatible Bluetooth proxy for Home Assistant.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"time"
)

var (
	version   = "dev"
	buildTime = "" // empty in reproducible builds
	verbose   bool
)

func logf(format string, a ...any) { log.Printf(format, a...) }
func debugf(format string, a ...any) {
	if verbose {
		log.Printf(format, a...)
	}
}

func main() {
	serialPath := flag.String("serial", "/dev/ttyS1", "BT chip UART")
	tcpAddr := flag.String("tcp", "", "use a TCP bridge to the UART instead (host:port), for development")
	listen := flag.String("listen", ":6053", "ESPHome API listen address")
	name := flag.String("name", "gw3-btproxy", "device name (hostname style)")
	friendly := flag.String("friendly-name", "Gateway BT Proxy", "friendly name")
	mac := flag.String("mac", "", "MAC address reported to Home Assistant (default: derived from the chip address)")
	maxConn := flag.Int("max-conn", 2, "simultaneous connections (the chip allows 2)")
	active := flag.Bool("active", false, "active scanning")
	maxMTU := flag.Uint("mtu", 247, "largest ATT MTU to offer (23 = never exchange)")
	flag.StringVar(&passkeyFile, "passkeys", passkeyFile, "file of 'MAC PIN' lines: the PIN to type in when a device asks for one")
	flag.String("tag", "", "ignored; '-tag silabs_ncp_bt' makes daemon_miio.sh treat this process as its BT app")
	flag.BoolVar(&verbose, "v", false, "debug logging")
	flag.Parse()

	debug.SetGCPercent(50)
	debug.SetMemoryLimit(6 << 20)
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	var port io.ReadWriter
	var err error
	if *tcpAddr != "" {
		port, err = net.DialTimeout("tcp", *tcpAddr, 5*time.Second)
	} else {
		port, err = openSerial(*serialPath)
	}
	if err != nil {
		log.Fatalf("open chip port: %v", err)
	}

	bg := NewBGAPI(port)
	p := NewProxy(bg, Config{MaxConn: *maxConn, Active: *active, MaxMTU: uint16(*maxMTU)})
	srv := NewServer(p, DeviceInfo{Name: *name, FriendlyName: *friendly, Model: "Xiaomi Gateway 3 (ZNDMWG03LM)", Version: version})
	go p.Run()
	if err := p.Init(); err != nil {
		log.Fatalf("chip init: %v", err)
	}
	srv.info.MAC = *mac
	if srv.info.MAC == "" {
		// The chip address equals the gateway's WiFi MAC, which other integrations already use as device id.
		// Set the locally administered bit so Home Assistant keeps this device separate.
		srv.info.MAC = macString(p.btAddr.Load() | 0x02<<40)
	}
	fmt.Fprintf(os.Stderr, "gw3-btproxy %s ready, BT %s, API id %s\n", version, macString(p.btAddr.Load()), srv.info.MAC)
	log.Fatal(srv.Serve(*listen))
}
