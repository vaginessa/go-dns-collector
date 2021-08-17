package loggers

import (
	"bufio"
	"net"
	"strconv"
	"time"

	"github.com/dmachard/go-dnscollector/dnsutils"
	"github.com/dmachard/go-dnstap-protobuf"
	"github.com/dmachard/go-framestream"
	"github.com/dmachard/go-logger"
	"google.golang.org/protobuf/proto"
)

type DnstapSender struct {
	done       chan bool
	channel    chan dnsutils.DnsMessage
	config     *dnsutils.Config
	logger     *logger.Logger
	exit       chan bool
	conn       net.Conn
	remoteAddr string
	remotePort int
	sockPath   string
	identity   string
	retry      int
}

func NewDnstapSender(config *dnsutils.Config, logger *logger.Logger) *DnstapSender {
	logger.Info("generator dnstap sender - enabled")
	s := &DnstapSender{
		done:    make(chan bool),
		exit:    make(chan bool),
		channel: make(chan dnsutils.DnsMessage, 512),
		logger:  logger,
		config:  config,
	}

	s.ReadConfig()

	return s
}

func (o *DnstapSender) ReadConfig() {
	o.sockPath = o.config.Generators.Dnstap.SockPath
	o.remoteAddr = o.config.Generators.Dnstap.RemoteAddress
	o.remotePort = o.config.Generators.Dnstap.RemotePort
	o.identity = o.config.Subprocessors.ServerId
	o.retry = o.config.Generators.Dnstap.RetryInterval
}

func (o *DnstapSender) LogInfo(msg string, v ...interface{}) {
	o.logger.Info("generator dnstap sender - "+msg, v...)
}

func (o *DnstapSender) LogError(msg string, v ...interface{}) {
	o.logger.Error("generator dnstap sender - "+msg, v...)
}

func (o *DnstapSender) Channel() chan dnsutils.DnsMessage {
	return o.channel
}

func (o *DnstapSender) Stop() {
	o.LogInfo("stopping...")

	// exit to close properly
	o.exit <- true

	// read done channel and block until run is terminated
	<-o.done
	close(o.done)
}

func (o *DnstapSender) Run() {
	o.LogInfo("running in background...")

	dt := &dnstap.Dnstap{}
	frame := &framestream.Frame{}

LOOP:
	for {
	LOOP_RECONNECT:
		for {
			select {
			case <-o.exit:
				break LOOP
			default:
				var err error
				var conn net.Conn
				if len(o.sockPath) > 0 {
					o.LogInfo("connecting to unix socket %s", o.sockPath)
					conn, err = net.Dial("unix", o.sockPath)
				} else {
					o.LogInfo("connecting to remote destination")
					conn, err = net.Dial("tcp", o.remoteAddr+":"+strconv.Itoa(o.remotePort))
				}
				if err != nil {
					o.LogError("connect error: %s", err)
				}
				if conn != nil {
					o.LogInfo("connected with remote")
					o.conn = conn
					// frame stream library
					r := bufio.NewReader(conn)
					w := bufio.NewWriter(conn)
					fs := framestream.NewFstrm(r, w, conn, 5*time.Second, []byte("protobuf:dnstap.Dnstap"), true)

					// init framestream protocol
					if err := fs.InitSender(); err != nil {
						o.LogError("sender protocol initialization error %s", err)
						break
					} else {
						o.LogInfo("framestream initialized")
					}

					for {
						select {
						case dm := <-o.channel:

							dt.Reset()

							t := dnstap.Dnstap_MESSAGE
							dt.Identity = []byte(o.identity)
							dt.Version = []byte("-")
							dt.Type = &t

							mt := dnstap.Message_Type(dnstap.Message_Type_value[dm.Operation])
							sf := dnstap.SocketFamily(dnstap.SocketFamily_value[dm.Family])
							sp := dnstap.SocketProtocol(dnstap.SocketProtocol_value[dm.Protocol])
							tsec := uint64(dm.TimeSec)
							tnsec := uint32(dm.TimeNsec)
							rportint, err := strconv.Atoi(dm.ResponsePort)
							if err != nil {
								o.LogError("error to encode dnstap response port %s", err)
								continue
							}
							rport := uint32(rportint)
							qportint, err := strconv.Atoi(dm.QueryPort)
							if err != nil {
								o.LogError("error to encode dnstap query port %s", err)
								continue
							}
							qport := uint32(qportint)

							msg := &dnstap.Message{Type: &mt}

							msg.SocketFamily = &sf
							msg.SocketProtocol = &sp
							msg.QueryAddress = net.ParseIP(dm.QueryIp)
							msg.QueryPort = &qport
							msg.ResponseAddress = net.ParseIP(dm.ResponseIp)
							msg.ResponsePort = &rport

							if dm.Type == "query" {
								msg.QueryMessage = dm.Payload
								msg.QueryTimeSec = &tsec
								msg.QueryTimeNsec = &tnsec
							} else {
								msg.ResponseTimeSec = &tsec
								msg.ResponseTimeNsec = &tnsec
								msg.ResponseMessage = dm.Payload
							}

							dt.Message = msg

							data, err := proto.Marshal(dt)
							if err != nil {
								o.LogError("proto marshal error %s", err)
							}

							frame.Write(data)
							if err := fs.SendFrame(frame); err != nil {
								o.LogError("send frame error %s", err)
								break LOOP_RECONNECT
							}
						case <-o.exit:
							o.logger.Info("closing framestream")
							if err = fs.ResetSender(); err != nil {
								o.LogError("reset framestream error %s", err)
							}
							break LOOP
						}
					}

				}
				o.LogInfo("retry to connect in 5 seconds")
				time.Sleep(time.Duration(o.retry) * time.Second)
			}
		}
	}

	if o.conn != nil {
		o.LogInfo("closing tcp connection")
		o.conn.Close()
	}
	o.LogInfo("run terminated")
	o.done <- true
}
