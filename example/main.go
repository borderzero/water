package main

import (
	"flag"
	"fmt"
	"github.com/labulakalia/water"
	"golang.org/x/net/ipv4"
	"log"
	"net"
	"os"
	"os/exec"
)

const (
	// I use TUN interface, so only plain IP packet, no ethernet header + mtu is set to 1300
	BUFFERSIZE = 1500
	MTU        = "1300"
)

var (
	tunIP        = flag.String("tun_ip", "", "Local tun interface IP/MASK like 192.168.3.3⁄24")
	tunMask      = flag.String("tun_mask", "", "net")
	remoteIP     = flag.String("remote_server", "", "Remote server (external) IP like 8.8.8.8")
	remotePort   = flag.Int("remote_port", 4321, "UDP remotePort for communication")
	remoteSubNet = flag.String("remote_subnet", "", "Remote server sub net")
)

func runIP(name string,args ...string) {
	cmd := exec.Command(name,args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if nil != err {
		log.Fatalln("Error running /sbin/ip:", err,name,args)
	}
}

func main() {
	flag.Parse()
	// check if we have anything
	if "" == *tunIP {
		flag.Usage()
		log.Fatalln("\nlocal ip is not specified")
	}
	if "" == *remoteIP {
		flag.Usage()
		log.Fatalln("\nremote server is not specified")
	}
	// create TUN interface
	iface, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			InterfaceName: "testTun",
		},
	})
	if nil != err {
		log.Fatalln("Unable to allocate TUN interface:", err)
	}
	log.Println("Interface allocated:", iface.Name())
	// set interface parameters
	//netlink.AddrAdd()
	//runIP("/sbin/ifconfig",iface.Name(),*tunIP,*tunIP,"up")
	//runIP("/sbin/route","add","-net",*tunMask,"-iface",iface.Name())
	//runIP("sh","-c",fmt.Sprintf("route add -net %s -iface %s",*remoteSubNet,iface.Name()))
	// reslove remote addr
	remoteAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("[%s]:%v", *remoteIP, *remotePort))
	if nil != err {
		log.Fatalln("Unable to resolve remote addr:", err)
	}

	// listen to local socket
	lstnAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%v", *remotePort))
	if nil != err {
		log.Fatalln("Unable to get UDP socket:", err)
	}
	log.Println("listen udp",lstnAddr.String())
	lstnConn, err := net.ListenUDP("udp", lstnAddr)
	if nil != err {
		log.Fatalln("Unable to listen on UDP socket:", err)
	}
	defer lstnConn.Close()
	// recv in separate thread
	go func() {
		buf := make([]byte, BUFFERSIZE)
		for {
			n, addr, err := lstnConn.ReadFromUDP(buf)
			// just debug
			header, _ := ipv4.ParseHeader(buf[:n])
			log.Printf("Recv Data: %s -> %s(%s -> %s)\n",addr.String(),lstnConn.LocalAddr(),header.Src,header.Dst)
			if err != nil || n == 0 {
				fmt.Println("Error: ", err)
				continue
			}
			// write to TUN interface
			_, err = iface.Write(buf[:n])
			if err != nil {
				log.Println("write tun iface ",err)
			}
		}
	}()
	// and one more loop
	packet := make([]byte, BUFFERSIZE)
	for {
		plen, err := iface.Read(packet)
		if err != nil {
			break
		}
		// debug :)
		header, _ := ipv4.ParseHeader(packet[:plen])
		log.Printf("Write data: %s -> %s(%s -> %s)\n", lstnConn.LocalAddr(),remoteAddr ,header.Src,header.Dst)
		if header.Src.String() == header.Dst.String() {
			iface.Write(packet[:plen])
		} else {
			// real send
			lstnConn.WriteToUDP(packet[:plen], remoteAddr)
		}
	}
}
