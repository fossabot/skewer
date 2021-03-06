package network

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inconshreveable/log15"
	dto "github.com/prometheus/client_model/go"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/decoders"
	"github.com/stephane-martin/skewer/model"
	"github.com/stephane-martin/skewer/services/base"
	"github.com/stephane-martin/skewer/services/errors"
	"github.com/stephane-martin/skewer/utils"
	"github.com/stephane-martin/skewer/utils/queue/tcp"
)

type TcpServerStatus int

const (
	TcpStopped TcpServerStatus = iota
	TcpStarted
)

func initTcpRegistry() {
	base.Once.Do(func() {
		base.InitRegistry()
	})
}

type TcpServiceImpl struct {
	StreamingService
	status           TcpServerStatus
	statusChan       chan TcpServerStatus
	reporter         base.Stasher
	rawMessagesQueue *tcp.Ring
	fatalErrorChan   chan struct{}
	fatalOnce        *sync.Once
}

func NewTcpService(env *base.ProviderEnv) (*TcpServiceImpl, error) {
	initTcpRegistry()
	s := TcpServiceImpl{
		status:   TcpStopped,
		reporter: env.Reporter,
	}
	s.StreamingService.init()
	s.StreamingService.BaseService.Logger = env.Logger.New("class", "TcpServer")
	s.StreamingService.BaseService.Binder = env.Binder
	s.StreamingService.handler = tcpHandler{Server: &s}
	s.StreamingService.confined = env.Confined
	return &s, nil
}

// Gather asks the TCP service to report metrics
func (s *TcpServiceImpl) Gather() ([]*dto.MetricFamily, error) {
	return base.Registry.Gather()
}

func (s *TcpServiceImpl) Type() base.Types {
	return base.TCP
}

// Start makes the TCP service start
func (s *TcpServiceImpl) Start() ([]model.ListenerInfo, error) {
	s.LockStatus()
	if s.status != TcpStopped {
		s.UnlockStatus()
		return nil, errors.ServerNotStopped
	}
	s.statusChan = make(chan TcpServerStatus, 1)
	s.fatalErrorChan = make(chan struct{})
	s.fatalOnce = &sync.Once{}

	// start listening on the required ports
	infos := s.initTCPListeners()
	if len(infos) > 0 {
		s.status = TcpStarted
		s.Listen()
		s.Logger.Info("Listening on TCP", "nb_services", len(infos))
		// start the parsers
		cpus := runtime.NumCPU()
		for i := 0; i < cpus; i++ {
			s.wg.Add(1)
			go s.parse()
		}
	} else {
		s.Logger.Debug("TCP Server not started: no listener")
		close(s.statusChan)
	}
	s.UnlockStatus()
	return infos, nil
}

func (s *TcpServiceImpl) dofatal() {
	s.fatalOnce.Do(func() { close(s.fatalErrorChan) })
}

func (s *TcpServiceImpl) FatalError() chan struct{} {
	return s.fatalErrorChan
}

// Shutdown is just Stop for the TCP service
func (s *TcpServiceImpl) Shutdown() {
	s.Stop()
}

// Stop makes the TCP service stop
func (s *TcpServiceImpl) Stop() {
	s.LockStatus()
	if s.status != TcpStarted {
		s.UnlockStatus()
		return
	}
	s.resetTCPListeners() // close the listeners. This will make Listen to return and close all current connections.
	if s.rawMessagesQueue != nil {
		s.rawMessagesQueue.Dispose()
	}
	s.wg.Wait() // wait that all goroutines have ended
	s.Logger.Debug("TcpServer goroutines have ended")

	s.status = TcpStopped
	s.statusChan <- TcpStopped
	close(s.statusChan)
	s.Logger.Debug("TCP server has stopped")
	s.UnlockStatus()
}

// SetConf configures the TCP service
//func (s *TcpServiceImpl) SetConf(sc []conf.TCPSourceConfig, pc []conf.ParserConfig, queueSize uint64, messageSize int) {
func (s *TcpServiceImpl) SetConf(c conf.BaseConfig) {
	s.BaseService.Pool = &sync.Pool{New: func() interface{} {
		return &model.RawTcpMessage{Message: make([]byte, c.Main.MaxInputMessageSize)}
	}}
	s.StreamingService.SetConf(c.TCPSource, c.Parsers, c.Main.InputQueueSize, c.Main.MaxInputMessageSize)
	s.rawMessagesQueue = tcp.NewRing(c.Main.InputQueueSize)
}

func makeLogger(logger log15.Logger, raw *model.RawTcpMessage) log15.Logger {
	// used to avoid to call logger.New in the critical parseOne
	return logger.New(
		"protocol", "tcp",
		"client", raw.Client,
		"local_port", raw.LocalPort,
		"unix_socket_path", raw.UnixSocketPath,
		"format", raw.Decoder.Format,
	)
}

func (s *TcpServiceImpl) parseOne(raw *model.RawTcpMessage, env *decoders.ParsersEnv, gen *utils.Generator) error {
	parser, err := env.GetParser(&raw.Decoder)

	if parser == nil || err != nil {
		makeLogger(s.Logger, raw).Error("Unknown parser")
		return nil
	}

	syslogMsgs, err := parser(raw.Message)
	if err != nil {
		base.ParsingErrorCounter.WithLabelValues("tcp", raw.Client, raw.Decoder.Format).Inc()
		makeLogger(s.Logger, raw).Info("Parsing error", "error", err)
		return nil
	}

	var syslogMsg *model.SyslogMessage
	var full *model.FullMessage

	for _, syslogMsg = range syslogMsgs {
		if syslogMsg == nil {
			continue
		}
		if raw.Client != "" {
			syslogMsg.SetProperty("skewer", "client", raw.Client)
		}
		if raw.UnixSocketPath != "" {
			syslogMsg.SetProperty("skewer", "socketpath", raw.UnixSocketPath)
		}
		if raw.LocalPort != 0 {
			syslogMsg.SetProperty("skewer", "localport", strconv.FormatInt(int64(raw.LocalPort), 10))
		}

		full = model.FullFactoryFrom(syslogMsg)
		full.Uid = gen.Uid()
		full.ConfId = raw.ConfID
		fatal, nonfatal := s.reporter.Stash(full)

		if fatal != nil {
			makeLogger(s.Logger, raw).Error("Fatal error stashing TCP message", "error", fatal)
			s.dofatal()
			return fatal
		} else if nonfatal != nil {
			makeLogger(s.Logger, raw).Warn("Non-fatal error stashing TCP message", "error", nonfatal)
		}
		model.FullFree(full)
	}
	return nil
}

// parse fetch messages from the raw queue, parse them, and push them to be sent.
func (s *TcpServiceImpl) parse() {
	defer s.wg.Done()

	env := decoders.NewParsersEnv(s.Logger)
	for _, pc := range s.ParserConfigs {
		env.AddJSParser(pc.Name, pc.Func)
	}
	gen := utils.NewGenerator()

	for {
		raw, err := s.rawMessagesQueue.Get()
		if raw == nil || err != nil {
			return
		}
		err = s.parseOne(raw, env, gen)
		s.Pool.Put(raw)
		if err != nil {
			return
		}
	}
}

type tcpHandler struct {
	Server *TcpServiceImpl
}

func (h tcpHandler) HandleConnection(conn net.Conn, config conf.TCPSourceConfig) {
	s := h.Server
	s.AddConnection(conn)

	defer func() {
		if e := recover(); e != nil {
			errString := fmt.Sprintf("%s", e)
			s.Logger.Error("Scanner panicked in TCP service", "error", errString)
		}
		s.RemoveConnection(conn)
		s.wg.Done()
	}()

	client := ""
	path := ""
	remote := conn.RemoteAddr()
	localPort := ""
	var localPortInt int32

	if remote == nil {
		client = "localhost"
		path = conn.LocalAddr().String()
	} else {
		client = strings.Split(remote.String(), ":")[0]
		local := conn.LocalAddr()
		if local != nil {
			s := strings.Split(local.String(), ":")
			localPortInt, _ = utils.Atoi32(s[len(s)-1])
			if localPortInt > 0 {
				localPort = strconv.FormatInt(int64(localPortInt), 10)
			}
		}
	}
	client = strings.TrimSpace(client)
	path = strings.TrimSpace(path)

	logger := s.Logger.New("protocol", "tcp", "client", client, "local_port", localPort, "unix_socket_path", path, "format", config.Format)
	logger.Info("New client")
	base.ClientConnectionCounter.WithLabelValues("tcp", client, localPort, path).Inc()

	timeout := config.Timeout
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, s.MaxMessageSize), s.MaxMessageSize)
	if config.LineFraming {
		scanner.Split(makeLFTCPSplit(config.FrameDelimiter))
	} else {
		scanner.Split(TcpSplit)
	}
	var rawmsg *model.RawTcpMessage
	var buf []byte

	for scanner.Scan() {
		if timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(timeout))
		}
		buf = scanner.Bytes()
		if len(buf) == 0 {
			continue
		}
		if s.MaxMessageSize > 0 && len(buf) > s.MaxMessageSize {
			logger.Warn("Message too large", "max", s.MaxMessageSize, "length", len(buf))
			return
		}
		rawmsg = s.Pool.Get().(*model.RawTcpMessage)
		rawmsg.Client = client
		rawmsg.LocalPort = localPortInt
		rawmsg.UnixSocketPath = path
		rawmsg.ConfID = config.ConfID
		rawmsg.Decoder = model.DecoderConfig(config.DecoderBaseConfig)
		rawmsg.Message = rawmsg.Message[:len(buf)]
		copy(rawmsg.Message, buf)
		err := s.rawMessagesQueue.Put(rawmsg)
		if err != nil {
			// rawMessagesQueue has been disposed
			logger.Warn("Error queueing TCP raw message", "error", err)
			return
		}
		base.IncomingMsgsCounter.WithLabelValues("tcp", client, localPort, path).Inc()
	}
	logger.Info("End of TCP client connection", "error", scanner.Err())
}

func makeLFTCPSplit(delimiter string) func(d []byte, a bool) (int, []byte, error) {
	delim := []byte(delimiter)[0]
	f := func(data []byte, atEOF bool) (advance int, token []byte, eoferr error) {
		if atEOF {
			eoferr = io.EOF
		}
		trimmedData := bytes.TrimLeft(data, " \r\n")
		if len(trimmedData) == 0 {
			return 0, nil, eoferr
		}
		trimmed := len(data) - len(trimmedData)
		lf := bytes.IndexByte(trimmedData, delim)
		if lf < 1 {
			return 0, nil, eoferr
		}
		token = bytes.Trim(trimmedData[0:lf], " \r\n")
		advance = trimmed + lf + 1
		return advance, token, nil
	}
	return f
}

func getline(data []byte, trimmed int, eoferr error) (int, []byte, error) {
	lf := bytes.IndexByte(data, '\n')
	if lf == 0 {
		return 0, nil, eoferr
	}
	token := bytes.Trim(data[0:lf], " \r\n")
	return lf + trimmed + 1, token, nil
}

func TcpSplit(data []byte, atEOF bool) (advance int, token []byte, eoferr error) {
	if atEOF {
		eoferr = io.EOF
	}
	trimmedData := bytes.TrimLeft(data, " \r\n")
	if len(trimmedData) == 0 {
		return 0, nil, eoferr
	}
	trimmed := len(data) - len(trimmedData)
	if trimmedData[0] == byte('<') {
		return getline(trimmedData, trimmed, eoferr)
	}
	// octet counting framing?
	sp := bytes.IndexAny(trimmedData, " \n")
	if sp <= 0 {
		return 0, nil, eoferr
	}
	datalenStr := bytes.Trim(trimmedData[0:sp], " \r\n")
	datalen, err := strconv.Atoi(string(datalenStr))
	if err != nil {
		// the first part is not a number, so back to LF
		return getline(trimmedData, trimmed, eoferr)
	}
	advance = trimmed + sp + 1 + datalen
	if len(data) < advance {
		return 0, nil, eoferr
	}
	token = bytes.Trim(trimmedData[sp+1:sp+1+datalen], " \r\n")
	return advance, token, nil

}
