package server

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/satori/go.uuid"
	"github.com/stephane-martin/relp2kafka/conf"
	"github.com/stephane-martin/relp2kafka/model"
	"github.com/stephane-martin/relp2kafka/store"
)

type TcpServerStatus int

const (
	TcpStopped TcpServerStatus = iota
	TcpStarted
)

type TcpServer struct {
	StoreServer
	statusMutex *sync.Mutex
	status      TcpServerStatus
	ClosedChan  chan TcpServerStatus
}

func (s *TcpServer) init() {
	s.StoreServer.init()
	s.statusMutex = &sync.Mutex{}
}

func NewTcpServer(c *conf.GConfig, st *store.MessageStore, logger log15.Logger) *TcpServer {
	s := TcpServer{}
	s.init()
	s.protocol = "tcp"
	s.stream = true
	s.Conf = *c
	s.listeners = map[int]net.Listener{}
	s.connections = map[net.Conn]bool{}
	s.logger = logger.New("class", "TcpServer")
	s.shandler = TcpHandler{Server: &s}
	s.status = TcpStopped
	s.store = st

	return &s
}

func (s *TcpServer) Start() (err error) {
	s.statusMutex.Lock()
	defer s.statusMutex.Unlock()
	if s.status != TcpStopped {
		err = ServerNotStopped
		return
	}
	s.ClosedChan = make(chan TcpServerStatus, 1)

	// start listening on the required ports
	nb := s.initListeners()
	if nb > 0 {
		s.status = TcpStarted
		s.wg.Add(1)
		go s.Listen()
		//s.storeToKafkaWg.Add(1)
		//go s.Store2Kafka()
	} else {
		s.logger.Info("TCP Server not started: no listening port")
		close(s.ClosedChan)
	}
	return

}

func (s *TcpServer) Stop() {
	s.statusMutex.Lock()
	defer s.statusMutex.Unlock()
	if s.status != TcpStarted {
		return
	}
	s.resetListeners() // close the listeners. This will make Listen to return and close all current connections.
	s.wg.Wait()        // wait that all HandleConnection goroutines have ended
	s.logger.Debug("TcpServer goroutines have ended")
	//s.store.StopSend()
	// eventually store.StartSend will end
	// after that the Store.Outputs channel will be closed (in the defer)
	// Store2Kafka will end, because of the closed Store.Outputs channel
	// The Kafka producer is then asked to close (AsyncClose in defer)
	// The child goroutine is Store2Kafka will drain the Producer Successes and Errors channels, and then return,
	// and finally storeToKafkaWg.Wait() will return
	//s.logger.Debug("Waiting for the Store to finish operations")
	//s.storeToKafkaWg.Wait()

	s.status = TcpStopped
	s.ClosedChan <- TcpStopped
	close(s.ClosedChan)
	s.logger.Info("TCP server has stopped")
}

type TcpHandler struct {
	Server *TcpServer
}

func (h TcpHandler) HandleConnection(conn net.Conn, i int) {
	s := h.Server
	s.AddConnection(conn)

	raw_messages_chan := make(chan *model.TcpUdpRawMessage)

	defer func() {
		close(raw_messages_chan)
		s.RemoveConnection(conn)
		s.wg.Done()
	}()

	var client string
	remote := conn.RemoteAddr()
	if remote != nil {
		client = strings.Split(remote.String(), ":")[0]
	}

	var local_port int
	local := conn.LocalAddr()
	if local != nil {
		s := strings.Split(local.String(), ":")
		local_port, _ = strconv.Atoi(s[len(s)-1])
	}

	logger := s.logger.New("remote", client, "local_port", local_port)
	logger.Info("New TCP client")

	// pull messages from raw_messages_chan, parse them and push them to the Store
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for m := range raw_messages_chan {
			p, err := model.Parse(m.Message, s.Conf.Syslog[i].Format, s.Conf.Syslog[i].DontParseSD)

			if err == nil {
				// todo: get rid of pointer to times
				t := time.Now()
				if p.TimeReported != nil {
					t = *p.TimeReported
				} else {
					p.TimeReported = &t
					p.TimeGenerated = &t
				}
				uid := t.Format(time.RFC3339) + m.Uid.String()
				parsed_msg := model.TcpUdpParsedMessage{
					Parsed: model.ParsedMessage{
						Fields:    p,
						Client:    m.Client,
						LocalPort: m.LocalPort,
					},
					Uid:       uid,
					ConfIndex: i,
				}
				s.store.Inputs <- &parsed_msg
			} else {
				logger.Info("Parsing error", "Message", m.Message, "error", err)
			}
		}
	}()

	// Syslog TCP server
	scanner := bufio.NewScanner(conn)
	scanner.Split(TcpSplit)

	for {
		if scanner.Scan() {
			data := scanner.Text()

			raw := model.TcpUdpRawMessage{
				RawMessage: model.RawMessage{
					Client:    client,
					LocalPort: local_port,
					Message:   data,
				},
				Uid: uuid.NewV4(),
			}
			raw_messages_chan <- &raw
		} else {
			logger.Info("Scanning the TCP stream has ended", "error", scanner.Err())
			return
		}
	}
}

func TcpSplit(data []byte, atEOF bool) (int, []byte, error) {
	trimmed_data := bytes.TrimLeft(data, " \r\n")
	if len(trimmed_data) == 0 {
		return 0, nil, nil
	}
	trimmed := len(data) - len(trimmed_data)
	if trimmed_data[0] == byte('<') {
		// non-transparent-framing
		lf := bytes.IndexByte(trimmed_data, '\n')
		if lf >= 0 {
			token := bytes.Trim(trimmed_data[0:lf], " \r\n")
			advance := trimmed + lf + 1
			return advance, token, nil
		} else {
			// data does not contain a full syslog line
			return 0, nil, nil
		}
	} else {
		// octet counting framing
		sp := bytes.IndexAny(trimmed_data, " \n")
		if sp <= 0 {
			return 0, nil, nil
		}
		datalen_s := bytes.Trim(trimmed_data[0:sp], " \r\n")
		datalen, err := strconv.Atoi(string(datalen_s))
		if err != nil {
			return 0, nil, err
		}
		advance := trimmed + sp + 1 + datalen
		if len(data) >= advance {
			token := bytes.Trim(trimmed_data[sp+1:sp+1+datalen], " \r\n")
			return advance, token, nil
		} else {
			return 0, nil, nil
		}

	}
}