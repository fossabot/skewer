package dests

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/inconshreveable/log15"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/model"
)

// TODO: metrics

type stderrDestination struct {
	logger  log15.Logger
	fatal   chan struct{}
	once    sync.Once
	ack     storeCallback
	nack    storeCallback
	permerr storeCallback
	format  string
	encoder model.Encoder
}

func NewStderrDestination(ctx context.Context, bc conf.BaseConfig, ack, nack, permerr storeCallback, logger log15.Logger) (dest Destination, err error) {
	d := &stderrDestination{
		logger:  logger,
		ack:     ack,
		nack:    nack,
		permerr: permerr,
		format:  bc.FileDest.Format,
		fatal:   make(chan struct{}),
	}

	d.encoder, err = model.NewEncoder(d.format)
	if err != nil {
		return nil, fmt.Errorf("Error getting encoder: %s", err)
	}

	return d, nil
}

func (d *stderrDestination) Send(message model.FullMessage, partitionKey string, partitionNumber int32, topic string) (err error) {
	var buf []byte
	buf, err = model.ChainEncode(d.encoder, &message, "\n")
	if err != nil {
		d.permerr(message.Uid, conf.Stderr)
		return err
	}
	_, err = os.Stderr.Write(buf)
	if err != nil {
		d.nack(message.Uid, conf.Stderr)
		d.once.Do(func() { close(d.fatal) })
		return err
	}
	d.ack(message.Uid, conf.Stderr)
	return nil
}

func (d *stderrDestination) Close() error {
	return nil
}

func (d *stderrDestination) Fatal() chan struct{} {
	return d.fatal
}