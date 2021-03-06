package network

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	sarama "github.com/Shopify/sarama"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/inconshreveable/log15"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/decoders"
	"github.com/stephane-martin/skewer/javascript"
	"github.com/stephane-martin/skewer/model"
	"github.com/stephane-martin/skewer/services/base"
	"github.com/stephane-martin/skewer/services/errors"
	"github.com/stephane-martin/skewer/sys/binder"
	"github.com/stephane-martin/skewer/utils"
	"github.com/stephane-martin/skewer/utils/queue"
	"github.com/stephane-martin/skewer/utils/queue/tcp"
)

var connCounter *prometheus.CounterVec
var ackCounter *prometheus.CounterVec
var messageFilterCounter *prometheus.CounterVec

func initDirectRelpRegistry() {
	base.Once.Do(func() {
		base.InitRegistry()

		// as a RELP service
		relpAnswersCounter = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "skw_relp_answers_total",
				Help: "number of RSP answers sent back to the RELP client",
			},
			[]string{"status", "client"},
		)

		relpProtocolErrorsCounter = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "skw_relp_protocol_errors_total",
				Help: "Number of RELP protocol errors",
			},
			[]string{"client"},
		)

		// as a "directrelp destination"
		ackCounter = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "skw_dest_ack_total",
				Help: "number of message acknowledgments",
			},
			[]string{"dest", "status"},
		)

		connCounter = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "skw_dest_conn_total",
				Help: "number of connections to remote service",
			},
			[]string{"dest", "status"},
		)

		messageFilterCounter = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "skw_message_filtering_total",
				Help: "number of filtered messages by status",
			},
			[]string{"status", "client", "destination"},
		)

		base.Registry.MustRegister(relpAnswersCounter, relpProtocolErrorsCounter, ackCounter, connCounter, messageFilterCounter)
	})
}

type DirectRelpService struct {
	impl           *DirectRelpServiceImpl
	fatalErrorChan chan struct{}
	fatalOnce      *sync.Once
	QueueSize      uint64
	logger         log15.Logger
	reporter       base.Reporter
	b              binder.Client
	sc             []conf.DirectRELPSourceConfig
	pc             []conf.ParserConfig
	kc             conf.KafkaDestConfig
	wg             sync.WaitGroup
	confined       bool
}

func NewDirectRelpService(env *base.ProviderEnv) (base.Provider, error) {
	initDirectRelpRegistry()
	s := DirectRelpService{
		b:        env.Binder,
		logger:   env.Logger,
		reporter: env.Reporter,
		confined: env.Confined,
	}
	s.impl = NewDirectRelpServiceImpl(env.Confined, env.Reporter, env.Binder, env.Logger)
	return &s, nil
}

func (s *DirectRelpService) Type() base.Types {
	return base.DirectRELP
}

func (s *DirectRelpService) FatalError() chan struct{} {
	return s.fatalErrorChan
}

func (s *DirectRelpService) dofatal() {
	s.fatalOnce.Do(func() { close(s.fatalErrorChan) })
}

func (s *DirectRelpService) Gather() ([]*dto.MetricFamily, error) {
	return base.Registry.Gather()
}

func (s *DirectRelpService) Start() (infos []model.ListenerInfo, err error) {
	infos = []model.ListenerInfo{}
	s.impl = NewDirectRelpServiceImpl(s.confined, s.reporter, s.b, s.logger)
	s.fatalErrorChan = make(chan struct{})
	s.fatalOnce = &sync.Once{}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			state := <-s.impl.StatusChan
			switch state {
			case FinalStopped:
				_ = s.reporter.Report([]model.ListenerInfo{})
				return

			case Stopped:
				s.impl.SetConf(s.sc, s.pc, s.kc, s.QueueSize)
				infos, err := s.impl.Start()
				if err == nil {
					err = s.reporter.Report(infos)
					if err != nil {
						s.impl.Logger.Error("Failed to report infos. Fatal error.", "error", err)
						s.dofatal()
					}
				} else {
					s.impl.Logger.Warn("The DirectRELP service has failed to start", "error", err)
					err = s.reporter.Report([]model.ListenerInfo{})
					if err != nil {
						s.impl.Logger.Error("Failed to report infos. Fatal error.", "error", err)
						s.dofatal()
					} else {
						s.impl.StopAndWait()
					}
				}

			case Waiting:
				go func() {
					time.Sleep(time.Duration(30) * time.Second)
					s.impl.EndWait()
				}()

			case Started:
			}
		}
	}()

	s.impl.StatusChan <- Stopped // trigger the RELP service to start
	return
}

func (s *DirectRelpService) Shutdown() {
	s.Stop()
}

func (s *DirectRelpService) Stop() {
	s.impl.FinalStop()
	s.wg.Wait()
}

func (s *DirectRelpService) SetConf(c conf.BaseConfig) {
	s.sc = c.DirectRELPSource
	s.pc = c.Parsers
	s.kc = *c.KafkaDest
	s.QueueSize = c.Main.InputQueueSize
}

type DirectRelpServiceImpl struct {
	StreamingService
	RelpConfigs         []conf.DirectRELPSourceConfig
	kafkaConf           conf.KafkaDestConfig
	status              RelpServerStatus
	StatusChan          chan RelpServerStatus
	producer            sarama.AsyncProducer
	reporter            base.Reporter
	rawQ                *tcp.Ring
	parsedMessagesQueue *queue.MessageQueue
	parsewg             sync.WaitGroup
	configs             map[utils.MyULID]conf.DirectRELPSourceConfig
	forwarder           *ackForwarder
}

func NewDirectRelpServiceImpl(confined bool, reporter base.Reporter, b binder.Client, logger log15.Logger) *DirectRelpServiceImpl {
	s := DirectRelpServiceImpl{
		status:    Stopped,
		reporter:  reporter,
		configs:   map[utils.MyULID]conf.DirectRELPSourceConfig{},
		forwarder: newAckForwarder(),
	}
	s.StreamingService.init()
	s.StreamingService.BaseService.Logger = logger.New("class", "DirectRELPService")
	s.StreamingService.BaseService.Binder = b
	s.StreamingService.handler = DirectRelpHandler{Server: &s}
	s.StreamingService.confined = confined
	s.StatusChan = make(chan RelpServerStatus, 10)
	return &s
}

func (s *DirectRelpServiceImpl) Start() ([]model.ListenerInfo, error) {
	s.LockStatus()
	defer s.UnlockStatus()
	if s.status == FinalStopped {
		return nil, errors.ServerDefinitelyStopped
	}
	if s.status != Stopped && s.status != Waiting {
		return nil, errors.ServerNotStopped
	}

	infos := s.initTCPListeners()
	if len(infos) == 0 {
		s.Logger.Info("DirectRELP service not started: no listener")
		return infos, nil
	}

	var err error
	s.producer, err = s.kafkaConf.GetAsyncProducer(s.confined)
	if err != nil {
		connCounter.WithLabelValues("directkafka", "fail").Inc()
		s.resetTCPListeners()
		return nil, err
	}
	connCounter.WithLabelValues("directkafka", "success").Inc()

	s.Logger.Info("Listening on DirectRELP", "nb_services", len(infos))

	s.parsedMessagesQueue = queue.NewMessageQueue()
	s.rawQ = tcp.NewRing(s.QueueSize)
	s.configs = map[utils.MyULID]conf.DirectRELPSourceConfig{}

	for _, l := range s.UnixListeners {
		s.configs[l.Conf.ConfID] = conf.DirectRELPSourceConfig(l.Conf)
	}
	for _, l := range s.TcpListeners {
		s.configs[l.Conf.ConfID] = conf.DirectRELPSourceConfig(l.Conf)
	}

	s.wg.Add(1)
	go s.push2kafka()
	s.wg.Add(1)
	go s.handleKafkaResponses()

	cpus := runtime.NumCPU()
	for i := 0; i < cpus; i++ {
		s.parsewg.Add(1)
		go s.parse()
	}

	s.status = Started
	s.StatusChan <- Started

	s.Listen()
	return infos, nil
}

func (s *DirectRelpServiceImpl) Stop() {
	s.LockStatus()
	s.doStop(false, false)
	s.UnlockStatus()
}

func (s *DirectRelpServiceImpl) FinalStop() {
	s.LockStatus()
	s.doStop(true, false)
	s.UnlockStatus()
}

func (s *DirectRelpServiceImpl) StopAndWait() {
	s.LockStatus()
	s.doStop(false, true)
	s.UnlockStatus()
}

func (s *DirectRelpServiceImpl) EndWait() {
	s.LockStatus()
	if s.status != Waiting {
		s.UnlockStatus()
		return
	}
	s.status = Stopped
	s.StatusChan <- Stopped
	s.UnlockStatus()
}

func (s *DirectRelpServiceImpl) doStop(final bool, wait bool) {
	if final && (s.status == Waiting || s.status == Stopped || s.status == FinalStopped) {
		if s.status != FinalStopped {
			s.status = FinalStopped
			s.StatusChan <- FinalStopped
			close(s.StatusChan)
		}
		return
	}

	if s.status == Stopped || s.status == FinalStopped || s.status == Waiting {
		if s.status == Stopped && wait {
			s.status = Waiting
			s.StatusChan <- Waiting
		}
		return
	}

	s.resetTCPListeners() // makes the listeners stop
	// no more message will arrive in rawMessagesQueue
	if s.rawQ != nil {
		s.rawQ.Dispose()
	}
	// the parsers consume the rest of rawMessagesQueue, then they stop
	s.parsewg.Wait() // wait that the parsers have stopped
	if s.parsedMessagesQueue != nil {
		s.parsedMessagesQueue.Dispose()
	}

	// after the parsers have stopped, we can close the queues
	s.forwarder.RemoveAll()
	// wait that all goroutines have ended
	s.wg.Wait()

	if final {
		s.status = FinalStopped
		s.StatusChan <- FinalStopped
		close(s.StatusChan)
	} else if wait {
		s.status = Waiting
		s.StatusChan <- Waiting
	} else {
		s.status = Stopped
		s.StatusChan <- Stopped
	}
}

func (s *DirectRelpServiceImpl) SetConf(sc []conf.DirectRELPSourceConfig, pc []conf.ParserConfig, kc conf.KafkaDestConfig, queueSize uint64) {
	tcpConfigs := []conf.TCPSourceConfig{}
	for _, c := range sc {
		tcpConfigs = append(tcpConfigs, conf.TCPSourceConfig(c))
	}
	s.StreamingService.SetConf(tcpConfigs, pc, queueSize, 132000)
	s.kafkaConf = kc
	s.BaseService.Pool = &sync.Pool{New: func() interface{} {
		return &model.RawTcpMessage{Message: make([]byte, 132000)}
	}}
}

func makeDRELPLogger(logger log15.Logger, raw *model.RawTcpMessage) log15.Logger {
	return logger.New(
		"protocol", "directrelp",
		"client", raw.Client,
		"local_port", raw.LocalPort,
		"unix_socket_path", raw.UnixSocketPath,
		"format", raw.Decoder.Format,
		"txnr", raw.Txnr,
	)
}

func (s *DirectRelpServiceImpl) parseOne(raw *model.RawTcpMessage, e *decoders.ParsersEnv) error {

	parser, err := e.GetParser(&raw.Decoder)
	if err != nil || parser == nil {
		s.forwarder.ForwardFail(raw.ConnID, raw.Txnr)
		makeDRELPLogger(s.Logger, raw).Crit("Unknown parser")
		return nil
	}
	syslogMsgs, err := parser(raw.Message)
	if err != nil {
		makeDRELPLogger(s.Logger, raw).Warn("Parsing error", "error", err)
		s.forwarder.ForwardFail(raw.ConnID, raw.Txnr)
		base.ParsingErrorCounter.WithLabelValues("directrelp", raw.Client, raw.Decoder.Format).Inc()
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
		if raw.LocalPort != 0 {
			syslogMsg.SetProperty("skewer", "localport", strconv.FormatInt(int64(raw.LocalPort), 10))
		}
		if raw.UnixSocketPath != "" {
			syslogMsg.SetProperty("skewer", "socketpath", raw.UnixSocketPath)
		}

		full = model.FullFactoryFrom(syslogMsg)
		full.Txnr = raw.Txnr
		full.ConfId = raw.ConfID
		full.ConnId = raw.ConnID
		_ = s.parsedMessagesQueue.Put(full)
	}
	return nil
}

func (s *DirectRelpServiceImpl) parse() {
	defer s.parsewg.Done()

	var raw *model.RawTcpMessage
	var err error
	e := decoders.NewParsersEnv(s.Logger)
	for _, pc := range s.ParserConfigs {
		e.AddJSParser(pc.Name, pc.Func)
	}

	for {
		raw, err = s.rawQ.Get()
		if err != nil {
			return
		}
		if raw == nil {
			s.Logger.Error("rawMessagesQueue returns nil, should not happen!")
			return
		}
		err = s.parseOne(raw, e)
		s.Pool.Put(raw)
		if err != nil {
			return
		}
	}
}

func (s *DirectRelpServiceImpl) handleKafkaResponses() {
	var succ *sarama.ProducerMessage
	var fail *sarama.ProducerError
	var more, fatal bool
	kafkaSuccChan := s.producer.Successes()
	kafkaFailChan := s.producer.Errors()
	for {
		if kafkaSuccChan == nil && kafkaFailChan == nil {
			return
		}
		select {
		case succ, more = <-kafkaSuccChan:
			if more {
				metad := succ.Metadata.(meta)
				s.forwarder.ForwardSucc(metad.ConnID, metad.Txnr)
			} else {
				kafkaSuccChan = nil
			}
		case fail, more = <-kafkaFailChan:
			if more {
				metad := fail.Msg.Metadata.(meta)
				s.forwarder.ForwardFail(metad.ConnID, metad.Txnr)
				s.Logger.Info("NACK from Kafka", "error", fail.Error(), "txnr", metad.Txnr, "topic", fail.Msg.Topic)
				fatal = model.IsFatalKafkaError(fail.Err)
			} else {
				kafkaFailChan = nil
			}

		}

		if fatal {
			s.StopAndWait()
			return
		}

	}

}

func (s *DirectRelpServiceImpl) handleResponses(conn net.Conn, connID utils.MyULID, client string, logger log15.Logger) {
	defer func() {
		s.wg.Done()
	}()

	successes := map[int32]bool{}
	failures := map[int32]bool{}
	var err error
	var ok1, ok2 bool
	var currentTxnr int32

	writeSuccess := func(txnr int32) (err error) {
		_, err = fmt.Fprintf(conn, "%d rsp 6 200 OK\n", txnr)
		return err
	}

	writeFailure := func(txnr int32) (err error) {
		_, err = fmt.Fprintf(conn, "%d rsp 6 500 KO\n", txnr)
		return err
	}

	for s.forwarder.Wait(connID) {
		currentTxnr = s.forwarder.GetSucc(connID)
		if currentTxnr != -1 {
			//logger.Debug("New success to report to client", "txnr", currentTxnr)
			_, ok1 = successes[currentTxnr]
			_, ok2 = failures[currentTxnr]
			if !ok1 && !ok2 {
				successes[currentTxnr] = true
			}
		}

		currentTxnr = s.forwarder.GetFail(connID)
		if currentTxnr != -1 {
			//logger.Debug("New failure to report to client", "txnr", currentTxnr)
			_, ok1 = successes[currentTxnr]
			_, ok2 = failures[currentTxnr]
			if !ok1 && !ok2 {
				failures[currentTxnr] = true
			}
		}

		// rsyslog expects the ACK/txnr correctly and monotonously ordered
		// so we need a bit of cooking to ensure that
	Cooking:
		for {
			next := s.forwarder.NextToCommit(connID)
			if next == -1 {
				break Cooking
			}
			//logger.Debug("Next to commit", "connid", connID, "txnr", next)
			if successes[next] {
				err = writeSuccess(next)
				if err == nil {
					//logger.Debug("ACK to client", "connid", connID, "tnxr", next)
					successes[next] = false
					relpAnswersCounter.WithLabelValues("200", client).Inc()
					ackCounter.WithLabelValues("directrelp", "ack").Inc()
				}
			} else if failures[next] {
				err = writeFailure(next)
				if err == nil {
					//logger.Debug("NACK to client", "connid", connID, "txnr", next)
					failures[next] = false
					relpAnswersCounter.WithLabelValues("500", client).Inc()
					ackCounter.WithLabelValues("directrelp", "nack").Inc()
				}
			} else {
				break Cooking
			}

			if err == nil {
				s.forwarder.Commit(connID)
			} else if err == io.EOF {
				// client is gone
				return
			} else if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				logger.Info("Timeout error writing RELP response to client", "error", err)
			} else {
				logger.Warn("Unexpected error writing RELP response to client", "error", err)
				return
			}
		}
	}
}

func (s *DirectRelpServiceImpl) push2kafka() {
	defer func() {
		s.producer.AsyncClose()
		s.wg.Done()
	}()
	envs := map[utils.MyULID]*javascript.Environment{}
	var message *model.FullMessage
	var err error

	for s.parsedMessagesQueue.Wait(0) {
		message, err = s.parsedMessagesQueue.Get()
		if message == nil || err != nil {
			// should not happen
			s.Logger.Error("Fatal error getting messages from the parsed messages queue", "error", err)
			s.StopAndWait()
			return
		}
		s.pushOne(message, &envs)
	}
}

func (s *DirectRelpServiceImpl) pushOne(message *model.FullMessage, envs *map[utils.MyULID]*javascript.Environment) {
	defer model.FullFree(message)
	var err error

	e, haveEnv := (*envs)[message.ConfId]
	if !haveEnv {
		config, haveConfig := s.configs[message.ConfId]
		if !haveConfig {
			s.Logger.Warn("Could not find the configuration for a message", "confId", message.ConfId, "txnr", message.Txnr)
			return
		}
		(*envs)[message.ConfId] = javascript.NewFilterEnvironment(
			config.FilterFunc,
			config.TopicFunc,
			config.TopicTmpl,
			config.PartitionFunc,
			config.PartitionTmpl,
			config.PartitionNumberFunc,
			s.Logger,
		)
		e = (*envs)[message.ConfId]
	}

	topic, errs := e.Topic(message.Fields)
	for _, err = range errs {
		s.Logger.Info("Error calculating topic", "error", err, "txnr", message.Txnr)
	}
	if len(topic) == 0 {
		s.Logger.Warn("Topic or PartitionKey could not be calculated", "txnr", message.Txnr)
		s.forwarder.ForwardFail(message.ConnId, message.Txnr)
		return
	}
	partitionKey, errs := e.PartitionKey(message.Fields)
	for _, err = range errs {
		s.Logger.Info("Error calculating the partition key", "error", err, "txnr", message.Txnr)
	}
	partitionNumber, errs := e.PartitionNumber(message.Fields)
	for _, err = range errs {
		s.Logger.Info("Error calculating the partition number", "error", err, "txnr", message.Txnr)
	}

	filterResult, err := e.FilterMessage(message.Fields)
	if err != nil {
		s.Logger.Warn("Error happened filtering message", "error", err)
		return
	}

	switch filterResult {
	case javascript.DROPPED:
		s.forwarder.ForwardFail(message.ConnId, message.Txnr)
		messageFilterCounter.WithLabelValues("dropped", message.Fields.GetProperty("skewer", "client"), "directkafka").Inc()
		return
	case javascript.REJECTED:
		s.forwarder.ForwardFail(message.ConnId, message.Txnr)
		messageFilterCounter.WithLabelValues("rejected", message.Fields.GetProperty("skewer", "client"), "directkafka").Inc()
		return
	case javascript.PASS:
		messageFilterCounter.WithLabelValues("passing", message.Fields.GetProperty("skewer", "client"), "directkafka").Inc()
	default:
		s.forwarder.ForwardFail(message.ConnId, message.Txnr)
		messageFilterCounter.WithLabelValues("unknown", message.Fields.GetProperty("skewer", "client"), "directkafka").Inc()
		s.Logger.Warn("Error happened processing message", "txnr", message.Txnr, "error", err)
		return
	}

	serialized, err := message.Fields.RegularJson()

	if err != nil {
		s.Logger.Warn("Error generating Kafka message", "error", err, "txnr", message.Txnr)
		s.forwarder.ForwardFail(message.ConnId, message.Txnr)
		return
	}

	kafkaMsg := &sarama.ProducerMessage{
		Key:       sarama.StringEncoder(partitionKey),
		Partition: partitionNumber,
		Value:     sarama.ByteEncoder(serialized),
		Topic:     topic,
		Timestamp: message.Fields.GetTimeReported(),
		Metadata:  meta{Txnr: message.Txnr, ConnID: message.ConnId},
	}

	s.producer.Input() <- kafkaMsg
}

type DirectRelpHandler struct {
	Server *DirectRelpServiceImpl
}

func (h DirectRelpHandler) HandleConnection(conn net.Conn, c conf.TCPSourceConfig) {

	config := conf.DirectRELPSourceConfig(c)
	s := h.Server
	s.AddConnection(conn)
	connID := s.forwarder.AddConn()
	lport, lports, client, path := props(conn)
	l := s.Logger.New(
		"ConnID", connID,
		"protocol", "directrelp",
		"client", client,
		"local_port", lport,
		"unix_socket_path", path,
		"format", config.Format,
	)

	defer func() {
		if e := recover(); e != nil {
			errString := fmt.Sprintf("%s", e)
			l.Error("Scanner panicked in Direct RELP service", "error", errString)
		}
		l.Info("Scanning the Direct RELP stream has ended")
		s.forwarder.RemoveConn(connID)
		s.RemoveConnection(conn)
		s.wg.Done()
	}()

	s.wg.Add(1)
	go s.handleResponses(conn, connID, client, l)
	var dc model.DecoderConfig = model.DecoderConfig(config.DecoderBaseConfig)
	scan(l, s.forwarder, s.rawQ, conn, config.Timeout, config.ConfID, connID, s.MaxMessageSize, lport, dc, lports, path, client, s.Pool)
}
