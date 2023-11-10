package qtls

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

	"github.com/MerlinKodo/quic-go"
	"github.com/MerlinKodo/quic-go/http3"
	M "github.com/sagernet/sing/common/metadata"
)

type Config interface {
	Dial(ctx context.Context, conn net.PacketConn, addr net.Addr, config *quic.Config) (quic.Connection, error)
	DialEarly(ctx context.Context, conn net.PacketConn, addr net.Addr, config *quic.Config) (quic.EarlyConnection, error)
	CreateTransport(conn net.PacketConn, quicConnPtr *quic.EarlyConnection, serverAddr M.Socksaddr, quicConfig *quic.Config, enableDatagrams bool) http.RoundTripper
}

type ServerConfig interface {
	Listen(conn net.PacketConn, config *quic.Config) (Listener, error)
	ListenEarly(conn net.PacketConn, config *quic.Config) (EarlyListener, error)
	ConfigureHTTP3()
}

type Listener interface {
	Accept(ctx context.Context) (quic.Connection, error)
	Close() error
	Addr() net.Addr
}

type EarlyListener interface {
	Accept(ctx context.Context) (quic.EarlyConnection, error)
	Close() error
	Addr() net.Addr
}

func Dial(ctx context.Context, conn net.PacketConn, addr net.Addr, tlsConfig *tls.Config, quicConfig *quic.Config) (quic.Connection, error) {
	return quic.Dial(ctx, conn, addr, tlsConfig, quicConfig)
}

func DialEarly(ctx context.Context, conn net.PacketConn, addr net.Addr, tlsConfig *tls.Config, quicConfig *quic.Config) (quic.EarlyConnection, error) {
	return quic.DialEarly(ctx, conn, addr, tlsConfig, quicConfig)
}

func CreateTransport(conn net.PacketConn, quicConnPtr *quic.EarlyConnection, serverAddr M.Socksaddr, tlsConfig *tls.Config, quicConfig *quic.Config, enableDatagrams bool) (http.RoundTripper, error) {
	return &http3.RoundTripper{
		TLSClientConfig: tlsConfig,
		QuicConfig:      quicConfig,
		EnableDatagrams: enableDatagrams,
		Dial: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlyConnection, error) {
			quicConn, err := quic.DialEarly(ctx, conn, serverAddr.UDPAddr(), tlsCfg, cfg)
			if err != nil {
				return nil, err
			}
			*quicConnPtr = quicConn
			return quicConn, nil
		},
	}, nil
}

func Listen(conn net.PacketConn, tlsConfig *tls.Config, quicConfig *quic.Config) (Listener, error) {
	return quic.Listen(conn, tlsConfig, quicConfig)
}

func ListenEarly(conn net.PacketConn, tlsConfig *tls.Config, quicConfig *quic.Config) (EarlyListener, error) {
	return quic.ListenEarly(conn, tlsConfig, quicConfig)
}

func ConfigureHTTP3(tlsConfig *tls.Config) error {
	if len(tlsConfig.NextProtos) == 0 {
		tlsConfig.NextProtos = []string{http3.NextProtoH3}
	}
	http3.ConfigureTLSConfig(tlsConfig)
	return nil
}
