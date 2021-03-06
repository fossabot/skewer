package dests

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/encoders"
	"github.com/stephane-martin/skewer/model"
	"github.com/stephane-martin/skewer/utils"
	"github.com/stephane-martin/skewer/utils/queue/message"
	"github.com/valyala/bytebufferpool"
)

type HTTPDestination struct {
	*baseDestination
	clt         *http.Client
	username    string
	password    string
	useragent   string
	url         *template.Template
	method      string
	contentType string
	sendQueue   *message.Ring
	wg          sync.WaitGroup
}

func NewHTTPDestination(ctx context.Context, e *Env) (Destination, error) {
	config := e.config.HTTPDest
	d := &HTTPDestination{
		baseDestination: newBaseDestination(conf.HTTP, "http", e),
		useragent:       config.UserAgent,
		method:          config.Method,
	}
	err := d.setFormat(config.Format)
	if err != nil {
		return nil, err
	}

	config.ContentType = strings.TrimSpace(strings.ToLower(config.ContentType))
	d.contentType = config.ContentType
	if config.ContentType == "auto" || config.ContentType == "" {
		if encoders.MimeTypes[d.format] != "" {
			d.contentType = encoders.MimeTypes[d.format]
		} else {
			d.contentType = "text/plain"
		}
	}

	if config.BasicAuth {
		d.username = config.Username
		d.password = config.Password
	}

	config.URL = strings.TrimSpace(config.URL)
	config.ProxyURL = strings.TrimSpace(config.ProxyURL)

	zurl, err := url.Parse(config.URL)
	if err != nil {
		return nil, err
	}
	host := zurl.Host

	tmpl, err := template.New("url").Parse(config.URL)
	if err != nil {
		return nil, err
	}
	d.url = tmpl

	if strings.HasPrefix(strings.ToLower(config.URL), "https") {
		config.TLSEnabled = true
	}
	dialer := &net.Dialer{
		Timeout: config.ConnTimeout,
	}
	if config.ConnKeepAlive {
		dialer.KeepAlive = config.ConnKeepAlivePeriod
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		IdleConnTimeout:       config.IdleConnTimeout,
		Proxy:                 nil,
		MaxIdleConns:          100,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext:           dialer.DialContext,
	}

	if config.TLSEnabled {
		tlsconfig, err := utils.NewTLSConfig(
			host,
			config.CAFile,
			config.CAPath,
			config.CertFile,
			config.KeyFile,
			config.Insecure,
			e.confined,
		)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsconfig
	}

	if len(config.ProxyURL) > 0 {
		url, err := url.Parse(config.ProxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(url)
	}

	d.clt = &http.Client{
		Transport: transport,
		Jar:       nil,
	}
	d.sendQueue = message.NewRing(4 * uint64(config.MaxIdleConnsPerHost))

	if config.Rebind > 0 {
		go func() {
			select {
			case <-ctx.Done():
				// the store service asked for stop
			case <-time.After(config.Rebind):
				e.logger.Info("HTTP destination rebind period has expired", "rebind", config.Rebind.String())
				d.dofatal()
			}
		}()
	}

	for i := 0; i < config.MaxIdleConnsPerHost; i++ {
		d.wg.Add(1)
		go d.dosend(ctx)
	}

	return d, nil
}

func (d *HTTPDestination) Close() error {
	d.sendQueue.Dispose()
	d.wg.Wait()
	// nack remaining messages
	for {
		msg, err := d.sendQueue.Get()
		if err != nil || msg == nil {
			break
		}
		d.nack(msg.Uid)
	}
	return nil
}

func (d *HTTPDestination) dosendOne(ctx context.Context, msg *model.FullMessage) (err error) {
	urlbuf := bytebufferpool.Get()
	body := bytebufferpool.Get()
	defer func() {
		bytebufferpool.Put(body)
		bytebufferpool.Put(urlbuf)
	}()
	err = d.url.Execute(urlbuf, msg.Fields)
	if err != nil {
		d.permerr(msg.Uid)
		d.logger.Warn("Error calculating target URL from template", "error", err)
		return nil
	}
	err = d.encoder(msg, body)

	if err != nil {
		d.permerr(msg.Uid)
		d.logger.Warn("Error encoding message", "error", err)
		return nil
	}
	req, err := http.NewRequest(d.method, urlbuf.String(), bytes.NewReader(body.B))
	if err != nil {
		d.permerr(msg.Uid)
		d.logger.Warn("Error preparing HTTP request", "error", err)
		return nil
	}
	req.Header.Set("Content-Type", d.contentType)
	if len(d.useragent) > 0 {
		req.Header.Set("User-Agent", d.useragent)
	}
	if len(d.username) > 0 && len(d.password) > 0 {
		req.SetBasicAuth(d.username, d.password)
	}
	req = req.WithContext(ctx)
	resp, err := d.clt.Do(req)
	if err != nil {
		// server down ?
		d.nack(msg.Uid)
		d.logger.Warn("Error sending HTTP request", "error", err)
		return err
	}
	// not interested in response body
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()

	httpStatusCounter.WithLabelValues(req.Host, strconv.FormatInt(int64(resp.StatusCode), 10)).Inc()

	if 200 <= resp.StatusCode && resp.StatusCode < 300 {
		d.ack(msg.Uid)
		return nil
	}
	if 400 <= resp.StatusCode && resp.StatusCode < 500 {
		// client-side error ??!
		d.nack(msg.Uid)
		d.logger.Warn("Client side error sending HTTP request", "code", resp.StatusCode, "status", resp.Status)
		return fmt.Errorf("HTTP error when sending message to server: code '%d', status '%s'", resp.StatusCode, resp.Status)
	}
	if 500 <= resp.StatusCode && resp.StatusCode < 600 {
		// server side error
		d.nack(msg.Uid)
		d.logger.Warn("Server side error sending HTTP request", "code", resp.StatusCode, "status", resp.Status)
		return fmt.Errorf("HTTP error when sending message to server: code '%d', status '%s'", resp.StatusCode, resp.Status)
	}
	d.nack(msg.Uid)
	d.logger.Warn("Unexpected status code sending HTTP request", "code", resp.StatusCode, "status", resp.Status)
	return fmt.Errorf("HTTP error when sending message to server: code '%d', status '%s'", resp.StatusCode, resp.Status)
}

func (d *HTTPDestination) dosend(ctx context.Context) {
	defer d.wg.Done()
	var msg *model.FullMessage
	var err error
	for {
		msg, err = d.sendQueue.Get()
		if err != nil || msg == nil {
			return
		}
		err = d.dosendOne(ctx, msg)
		model.FullFree(msg)
		if err != nil {
			d.dofatal()
			return
		}
	}
}

func (d *HTTPDestination) sendOne(msg *model.FullMessage) (err error) {
	err = d.sendQueue.Put(msg)
	if err != nil {
		// the client send queue has been disposed
		d.nack(msg.Uid)
		d.dofatal()
	}
	return err
}

func (d *HTTPDestination) Send(msgs []model.OutputMsg, partitionKey string, partitionNumber int32, topic string) (err error) {
	var i int
	var e error
	for i = range msgs {
		e = d.sendOne(msgs[i].Message)
		if e != nil {
			err = e
		}
	}
	return err
}
