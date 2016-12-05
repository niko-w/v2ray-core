package freedom

import (
	"io"
	"v2ray.com/core/app"
	"v2ray.com/core/app/dns"
	"v2ray.com/core/common/alloc"
	"v2ray.com/core/common/dice"
	"v2ray.com/core/common/errors"
	v2io "v2ray.com/core/common/io"
	"v2ray.com/core/common/loader"
	"v2ray.com/core/common/log"
	v2net "v2ray.com/core/common/net"
	"v2ray.com/core/common/retry"
	"v2ray.com/core/proxy"
	"v2ray.com/core/proxy/registry"
	"v2ray.com/core/transport/internet"
	"v2ray.com/core/transport/internet/tcp"
	"v2ray.com/core/transport/ray"
)

type FreedomConnection struct {
	domainStrategy Config_DomainStrategy
	timeout        uint32
	dns            dns.Server
	meta           *proxy.OutboundHandlerMeta
}

func NewFreedomConnection(config *Config, space app.Space, meta *proxy.OutboundHandlerMeta) *FreedomConnection {
	f := &FreedomConnection{
		domainStrategy: config.DomainStrategy,
		timeout:        config.Timeout,
		meta:           meta,
	}
	space.InitializeApplication(func() error {
		if config.DomainStrategy == Config_USE_IP {
			if !space.HasApp(dns.APP_ID) {
				return errors.New("Freedom: DNS server is not found in the space.")
			}
			f.dns = space.GetApp(dns.APP_ID).(dns.Server)
		}
		return nil
	})
	return f
}

// Private: Visible for testing.
func (v *FreedomConnection) ResolveIP(destination v2net.Destination) v2net.Destination {
	if !destination.Address.Family().IsDomain() {
		return destination
	}

	ips := v.dns.Get(destination.Address.Domain())
	if len(ips) == 0 {
		log.Info("Freedom: DNS returns nil answer. Keep domain as is.")
		return destination
	}

	ip := ips[dice.Roll(len(ips))]
	var newDest v2net.Destination
	if destination.Network == v2net.Network_TCP {
		newDest = v2net.TCPDestination(v2net.IPAddress(ip), destination.Port)
	} else {
		newDest = v2net.UDPDestination(v2net.IPAddress(ip), destination.Port)
	}
	log.Info("Freedom: Changing destination from ", destination, " to ", newDest)
	return newDest
}

func (v *FreedomConnection) Dispatch(destination v2net.Destination, payload *alloc.Buffer, ray ray.OutboundRay) {
	log.Info("Freedom: Opening connection to ", destination)

	defer payload.Release()
	defer ray.OutboundInput().Release()
	defer ray.OutboundOutput().Close()

	var conn internet.Connection
	if v.domainStrategy == Config_USE_IP && destination.Address.Family().IsDomain() {
		destination = v.ResolveIP(destination)
	}
	err := retry.ExponentialBackoff(5, 100).On(func() error {
		rawConn, err := internet.Dial(v.meta.Address, destination, v.meta.GetDialerOptions())
		if err != nil {
			return err
		}
		conn = rawConn
		return nil
	})
	if err != nil {
		log.Warning("Freedom: Failed to open connection to ", destination, ": ", err)
		return
	}
	defer conn.Close()

	input := ray.OutboundInput()
	output := ray.OutboundOutput()

	if !payload.IsEmpty() {
		conn.Write(payload.Bytes())
	}

	go func() {
		v2writer := v2io.NewAdaptiveWriter(conn)
		defer v2writer.Release()

		if err := v2io.PipeUntilEOF(input, v2writer); err != nil {
			log.Info("Freedom: Failed to transport all TCP request: ", err)
		}
		if tcpConn, ok := conn.(*tcp.RawConnection); ok {
			tcpConn.CloseWrite()
		}
	}()

	var reader io.Reader = conn

	timeout := v.timeout
	if destination.Network == v2net.Network_UDP {
		timeout = 16
	}
	if timeout > 0 {
		reader = v2net.NewTimeOutReader(timeout /* seconds */, conn)
	}

	v2reader := v2io.NewAdaptiveReader(reader)
	if err := v2io.PipeUntilEOF(v2reader, output); err != nil {
		log.Info("Freedom: Failed to transport all TCP response: ", err)
	}
	v2reader.Release()
	ray.OutboundOutput().Close()
}

type FreedomFactory struct{}

func (v *FreedomFactory) StreamCapability() v2net.NetworkList {
	return v2net.NetworkList{
		Network: []v2net.Network{v2net.Network_RawTCP},
	}
}

func (v *FreedomFactory) Create(space app.Space, config interface{}, meta *proxy.OutboundHandlerMeta) (proxy.OutboundHandler, error) {
	return NewFreedomConnection(config.(*Config), space, meta), nil
}

func init() {
	registry.MustRegisterOutboundHandlerCreator(loader.GetType(new(Config)), new(FreedomFactory))
}
