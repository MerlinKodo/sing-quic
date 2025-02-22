package hysteria2

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"

	qtls "github.com/MerlinKodo/sing-quic"

	"github.com/MerlinKodo/quic-go"
	"github.com/MerlinKodo/quic-go/http3"
	hyCC "github.com/MerlinKodo/sing-quic/hysteria2/congestion"
	"github.com/MerlinKodo/sing-quic/hysteria2/internal/protocol"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/baderror"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type ServiceOptions struct {
	Context               context.Context
	Logger                logger.Logger
	BrutalDebug           bool
	SendBPS               uint64
	ReceiveBPS            uint64
	IgnoreClientBandwidth bool
	SalamanderPassword    string
	TLSConfig             *tls.Config
	UDPDisabled           bool
	Handler               ServerHandler
	MasqueradeHandler     http.Handler
	CWND                  int
}

type ServerHandler interface {
	N.TCPConnectionHandler
	N.UDPConnectionHandler
}

type Service[U comparable] struct {
	ctx                   context.Context
	logger                logger.Logger
	brutalDebug           bool
	sendBPS               uint64
	receiveBPS            uint64
	ignoreClientBandwidth bool
	salamanderPassword    string
	tlsConfig             *tls.Config
	quicConfig            *quic.Config
	userMap               map[string]U
	udpDisabled           bool
	handler               ServerHandler
	masqueradeHandler     http.Handler
	quicListener          io.Closer
	cwnd                  int
}

func NewService[U comparable](options ServiceOptions) (*Service[U], error) {
	quicConfig := &quic.Config{
		DisablePathMTUDiscovery:        !(runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "android" || runtime.GOOS == "darwin"),
		EnableDatagrams:                !options.UDPDisabled,
		MaxIncomingStreams:             1 << 60,
		InitialStreamReceiveWindow:     defaultStreamReceiveWindow,
		MaxStreamReceiveWindow:         defaultStreamReceiveWindow,
		InitialConnectionReceiveWindow: defaultConnReceiveWindow,
		MaxConnectionReceiveWindow:     defaultConnReceiveWindow,
		MaxIdleTimeout:                 defaultMaxIdleTimeout,
		KeepAlivePeriod:                defaultKeepAlivePeriod,
	}
	if options.MasqueradeHandler == nil {
		options.MasqueradeHandler = http.NotFoundHandler()
	}
	return &Service[U]{
		ctx:                   options.Context,
		logger:                options.Logger,
		brutalDebug:           options.BrutalDebug,
		sendBPS:               options.SendBPS,
		receiveBPS:            options.ReceiveBPS,
		ignoreClientBandwidth: options.IgnoreClientBandwidth,
		salamanderPassword:    options.SalamanderPassword,
		tlsConfig:             options.TLSConfig,
		quicConfig:            quicConfig,
		userMap:               make(map[string]U),
		udpDisabled:           options.UDPDisabled,
		handler:               options.Handler,
		masqueradeHandler:     options.MasqueradeHandler,
	}, nil
}

func (s *Service[U]) UpdateUsers(userList []U, passwordList []string) {
	userMap := make(map[string]U)
	for i, user := range userList {
		userMap[passwordList[i]] = user
	}
	s.userMap = userMap
}

func (s *Service[U]) Start(conn net.PacketConn) error {
	if s.salamanderPassword != "" {
		conn = NewSalamanderConn(conn, []byte(s.salamanderPassword))
	}
	err := qtls.ConfigureHTTP3(s.tlsConfig)
	if err != nil {
		return err
	}
	listener, err := qtls.Listen(conn, s.tlsConfig, s.quicConfig)
	if err != nil {
		return err
	}
	s.quicListener = listener
	go s.loopConnections(listener)
	return nil
}

func (s *Service[U]) Close() error {
	return common.Close(
		s.quicListener,
	)
}

func (s *Service[U]) loopConnections(listener qtls.Listener) {
	for {
		connection, err := listener.Accept(s.ctx)
		if err != nil {
			if E.IsClosedOrCanceled(err) {
				s.logger.Debug(E.Cause(err, "listener closed"))
			} else {
				s.logger.Error(E.Cause(err, "listener closed"))
			}
			return
		}
		go s.handleConnection(connection)
	}
}

func (s *Service[U]) handleConnection(connection quic.Connection) {
	session := &serverSession[U]{
		Service:    s,
		ctx:        s.ctx,
		quicConn:   connection,
		source:     M.SocksaddrFromNet(connection.RemoteAddr()),
		connDone:   make(chan struct{}),
		udpConnMap: make(map[uint32]*udpPacketConn),
	}
	httpServer := http3.Server{
		Handler:        session,
		StreamHijacker: session.handleStream0,
	}
	_ = httpServer.ServeQUICConn(connection)
	_ = connection.CloseWithError(0, "")
}

type serverSession[U comparable] struct {
	*Service[U]
	ctx           context.Context
	quicConn      quic.Connection
	source        M.Socksaddr
	connAccess    sync.Mutex
	connDone      chan struct{}
	connErr       error
	authenticated bool
	authUser      U
	udpAccess     sync.RWMutex
	udpConnMap    map[uint32]*udpPacketConn
}

func (s *serverSession[U]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.Host == protocol.URLHost && r.URL.Path == protocol.URLPath {
		if s.authenticated {
			protocol.AuthResponseToHeader(w.Header(), protocol.AuthResponse{
				UDPEnabled: !s.udpDisabled,
				Rx:         s.receiveBPS,
				RxAuto:     s.ignoreClientBandwidth,
			})
			w.WriteHeader(protocol.StatusAuthOK)
			return
		}
		request := protocol.AuthRequestFromHeader(r.Header)
		user, loaded := s.userMap[request.Auth]
		if !loaded {
			s.masqueradeHandler.ServeHTTP(w, r)
			return
		}
		s.authUser = user
		s.authenticated = true
		if !s.ignoreClientBandwidth && request.Rx > 0 {
			var sendBps uint64
			if s.sendBPS > 0 && s.sendBPS < request.Rx {
				sendBps = s.sendBPS
			} else {
				sendBps = request.Rx
			}
			s.quicConn.SetCongestionControl(hyCC.NewBrutalSender(sendBps, s.brutalDebug, s.logger))
		} else {
			SetCongestionController(s.quicConn, "bbr", s.cwnd)
		}
		protocol.AuthResponseToHeader(w.Header(), protocol.AuthResponse{
			UDPEnabled: !s.udpDisabled,
			Rx:         s.receiveBPS,
			RxAuto:     s.ignoreClientBandwidth,
		})
		w.WriteHeader(protocol.StatusAuthOK)
		if s.ctx.Done() != nil {
			go func() {
				select {
				case <-s.ctx.Done():
					s.closeWithError(s.ctx.Err())
				case <-s.connDone:
				}
			}()
		}
		if !s.udpDisabled {
			go s.loopMessages()
		}
	} else {
		s.masqueradeHandler.ServeHTTP(w, r)
	}
}

func (s *serverSession[U]) handleStream0(frameType http3.FrameType, connection quic.Connection, stream quic.Stream, err error) (bool, error) {
	if !s.authenticated || err != nil {
		return false, nil
	}
	if frameType != protocol.FrameTypeTCPRequest {
		return false, nil
	}
	go func() {
		hErr := s.handleStream(stream)
		stream.CancelRead(0)
		stream.Close()
		if hErr != nil {
			stream.CancelRead(0)
			stream.Close()
			s.logger.Error(E.Cause(hErr, "handle stream request"))
		}
	}()
	return true, nil
}

func (s *serverSession[U]) handleStream(stream quic.Stream) error {
	destinationString, err := protocol.ReadTCPRequest(stream)
	if err != nil {
		return E.New("read TCP request")
	}
	ctx := auth.ContextWithUser(s.ctx, s.authUser)
	_ = s.handler.NewConnection(ctx, &serverConn{Stream: stream}, M.Metadata{
		Source:      s.source,
		Destination: M.ParseSocksaddr(destinationString),
	})
	return nil
}

func (s *serverSession[U]) closeWithError(err error) {
	s.connAccess.Lock()
	defer s.connAccess.Unlock()
	select {
	case <-s.connDone:
		return
	default:
		s.connErr = err
		close(s.connDone)
	}
	if E.IsClosedOrCanceled(err) {
		s.logger.Debug(E.Cause(err, "connection failed"))
	} else {
		s.logger.Error(E.Cause(err, "connection failed"))
	}
	_ = s.quicConn.CloseWithError(0, "")
}

type serverConn struct {
	quic.Stream
	responseWritten bool
}

func (c *serverConn) HandshakeFailure(err error) error {
	if c.responseWritten {
		return os.ErrClosed
	}
	c.responseWritten = true
	buffer := protocol.WriteTCPResponse(false, err.Error(), nil)
	defer buffer.Release()
	return common.Error(c.Stream.Write(buffer.Bytes()))
}

func (c *serverConn) HandshakeSuccess() error {
	if c.responseWritten {
		return nil
	}
	c.responseWritten = true
	buffer := protocol.WriteTCPResponse(true, "", nil)
	defer buffer.Release()
	return common.Error(c.Stream.Write(buffer.Bytes()))
}

func (c *serverConn) Read(p []byte) (n int, err error) {
	n, err = c.Stream.Read(p)
	return n, baderror.WrapQUIC(err)
}

func (c *serverConn) Write(p []byte) (n int, err error) {
	if !c.responseWritten {
		c.responseWritten = true
		buffer := protocol.WriteTCPResponse(true, "", p)
		defer buffer.Release()
		_, err = c.Stream.Write(buffer.Bytes())
		if err != nil {
			return 0, baderror.WrapQUIC(err)
		}
		return len(p), nil
	}
	n, err = c.Stream.Write(p)
	return n, baderror.WrapQUIC(err)
}

func (c *serverConn) LocalAddr() net.Addr {
	return M.Socksaddr{}
}

func (c *serverConn) RemoteAddr() net.Addr {
	return M.Socksaddr{}
}

func (c *serverConn) Close() error {
	c.Stream.CancelRead(0)
	return c.Stream.Close()
}
