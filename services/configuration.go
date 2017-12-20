package services

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/awnumar/memguard"
	"github.com/inconshreveable/log15"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/consul"
	"github.com/stephane-martin/skewer/sys/capabilities"
	"github.com/stephane-martin/skewer/sys/kring"
	"github.com/stephane-martin/skewer/sys/namespaces"
	"github.com/stephane-martin/skewer/utils"
)

var stdoutMu sync.Mutex
var stdoutWriter *utils.EncryptWriter

func init() {
	stdoutWriter = utils.NewEncryptWriter(os.Stdout, nil)
}

func WConf(header []byte, message []byte) (err error) {
	stdoutMu.Lock()
	err = stdoutWriter.WriteWithHeader(header, message)
	stdoutMu.Unlock()
	return err
}

type ConfigurationService struct {
	output       chan *conf.BaseConfig
	params       consul.ConnParams
	stdin        io.WriteCloser
	logger       log15.Logger
	stdinMu      *sync.Mutex
	confdir      string
	loggerHandle uintptr
	signKey      *memguard.LockedBuffer
	stdinWriter  *utils.SigWriter
}

func NewConfigurationService(signKey *memguard.LockedBuffer, childLoggerHandle uintptr, l log15.Logger) *ConfigurationService {
	c := ConfigurationService{
		loggerHandle: childLoggerHandle,
		logger:       l,
		signKey:      signKey,
		stdinMu:      &sync.Mutex{},
	}
	return &c
}

func (c *ConfigurationService) W(header []byte, message []byte) (err error) {
	c.stdinMu.Lock()
	if c.stdinWriter != nil {
		err = c.stdinWriter.WriteWithHeader(header, message)
	} else {
		err = fmt.Errorf("stdin is nil")
	}
	c.stdinMu.Unlock()
	return err
}

func (c *ConfigurationService) SetConfDir(cdir string) {
	c.confdir = cdir
}

func (c *ConfigurationService) SetConsulParams(params consul.ConnParams) {
	c.params = params
}

func (c *ConfigurationService) Stop() {
	err := c.W([]byte("stop"), utils.NOW)
	if err == nil {
		for range c.output {

		}
	} else {
		c.logger.Warn("Error asking the configuration plugin to stop", "error", err)
	}
}

func (c *ConfigurationService) Reload() {
	err := c.W([]byte("reload"), utils.NOW)
	if err != nil {
		c.logger.Warn("Error asking the configuration plugin to reload", "error", err)
	}
}

func (c *ConfigurationService) Chan() chan *conf.BaseConfig {
	return c.output
}

func (c *ConfigurationService) Start(r kring.Ring) error {
	var err error
	var cmd *exec.Cmd
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	c.output = make(chan *conf.BaseConfig)

	if capabilities.CapabilitiesSupported {
		cmd, stdin, stdout, err = setupCmd("confined-skewer-conf", r, 0, c.loggerHandle, nil, false)
		if err != nil {
			close(c.output)
			return err
		}
		err = namespaces.NewNamespacedCmd(cmd).ConfPath(c.confdir).Start()
	}
	if err != nil {
		c.logger.Warn("Starting configuration service in user namespace has failed", "error", err)
	}
	if err != nil || !capabilities.CapabilitiesSupported {
		cmd, stdin, stdout, err = setupCmd("skewer-conf", r, 0, c.loggerHandle, nil, false)
		if err != nil {
			close(c.output)
			return err
		}
		err = cmd.Start()
	}
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		close(c.output)
		return err
	}
	c.stdin = stdin
	c.stdinWriter = utils.NewSignatureWriter(stdin, c.signKey)

	startedChan := make(chan error)

	go func() {
		kill := false
		once := &sync.Once{}
		defer func() {
			c.logger.Debug("Configuration service is stopping")

			once.Do(func() {
				startedChan <- fmt.Errorf("Unexpected end of configuration service")
				close(startedChan)
			})

			if kill {
				c.logger.Warn("Killing configuration service")
				c.stdinMu.Lock()
				_ = cmd.Process.Kill()
				c.stdinMu.Unlock()
			}

			errChan := make(chan error)
			go func() {
				errChan <- cmd.Wait()
				close(errChan)
			}()

			var err error

			select {
			case err = <-errChan:
			case <-time.After(3 * time.Second):
				c.logger.Warn("Timeout: killing configuration service")
				c.stdinMu.Lock()
				_ = cmd.Process.Kill()
				c.stdinMu.Unlock()
				err = cmd.Wait()
			}

			if err == nil {
				c.logger.Debug("Configuration process has exited without providing error")
			} else {
				c.logger.Error("Configuration process has ended with error", "error", err.Error())
				if e, ok := err.(*exec.ExitError); ok {
					c.logger.Error("Configuration process stderr", "stderr", string(e.Stderr))
					status := e.Sys()
					if cstatus, ok := status.(syscall.WaitStatus); ok {
						c.logger.Error("Configuration process exit code", "code", cstatus.ExitStatus())
					}
				}
			}
			close(c.output)
		}()

		var command string
		scanner := bufio.NewScanner(stdout)
		scanner.Split(utils.PluginSplit)

		for scanner.Scan() {
			parts := strings.SplitN(scanner.Text(), " ", 2)
			command = parts[0]
			switch command {
			case "newconf":
				if len(parts) == 2 {
					newconf := conf.BaseConfig{}
					err := json.Unmarshal([]byte(parts[1]), &newconf)
					if err == nil {
						c.output <- &newconf
					} else {
						c.logger.Error("Error unmarshaling a new configuration received from child", "error", err)
						kill = true
						return
					}
				} else {
					c.logger.Error("Empty newconf message received from configuration child")
					kill = true
					return
				}
			case "started":
				c.logger.Debug("Configuration child has started")
				once.Do(func() { close(startedChan) })

			case "starterror":
				msg := ""
				if len(parts) == 2 {
					msg = parts[1]
				} else {
					msg = "Received empty starterror from child"
					kill = true
				}
				c.logger.Error(msg)
				once.Do(func() { startedChan <- fmt.Errorf(msg); close(startedChan) })
				return
			case "reloaded":
				c.logger.Debug("Configuration child has been reloaded")
			default:
				msg := "Unknown command received from configuration child"
				c.logger.Error(msg, "command", command)
				kill = true
				once.Do(func() { startedChan <- fmt.Errorf(msg + ": " + command); close(startedChan) })
				return
			}
		}
		err := scanner.Err()
		if err != nil {
			c.logger.Error("Scanner error", "error", err)
		}

	}()

	cparams, _ := json.Marshal(c.params)

	err = c.W([]byte("confdir"), []byte(c.confdir))
	if err == nil {
		err = c.W([]byte("consulparams"), cparams)
		if err == nil {
			err = c.W([]byte("start"), utils.NOW)
			if err == nil {
				err = <-startedChan
			}
		}
	}
	if err != nil {
		c.logger.Crit("Error starting configuration service", "error", err)
		c.Stop()
	}
	return err

}

func writeNewConf(ctx context.Context, updated chan *conf.BaseConfig, logger log15.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case newconf, more := <-updated:
			if more {
				confb, err := json.Marshal(newconf)
				if err == nil {
					// TODO: sign configuration
					err = WConf([]byte("newconf"), confb)
					if err != nil {
						logger.Warn("Error sending new configuration", "error", err)
					}
				} else {
					logger.Warn("Error serializing new configuration", "error", err)
				}
			} else {
				return
			}
		}
	}
}

func start(confdir string, params consul.ConnParams, r kring.Ring, logger log15.Logger) (context.CancelFunc, error) {

	if len(confdir) == 0 {
		return nil, fmt.Errorf("Configuration directory is empty")
	}
	ctx, cancel := context.WithCancel(context.Background())
	gconf, updated, err := conf.InitLoad(ctx, confdir, params, r, logger)
	if err == nil {
		confb, err := json.Marshal(gconf)
		if err == nil {
			err = utils.Chain(
				func() error { return WConf([]byte("started"), utils.NOW) },
				func() error { return WConf([]byte("newconf"), confb) },
			)
			if err != nil {
				return nil, err
			}
			go writeNewConf(ctx, updated, logger)
		} else {
			cancel()
			return nil, err
		}
	} else {
		cancel()
		return nil, err
	}
	return cancel, nil
}

func LaunchConfProvider(r kring.Ring, logger log15.Logger) error {
	if r == nil {
		return fmt.Errorf("No ring")
	}
	sigpubkey, err := r.GetSignaturePubkey()
	if err != nil {
		return err
	}
	var confdir string
	var params consul.ConnParams

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Split(utils.MakeSignSplit(sigpubkey))
	var command string
	var cancel context.CancelFunc

	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " ", 2)
		command = parts[0]

		switch command {
		case "start":
			var err error
			cancel, err = start(confdir, params, r, logger)
			if err != nil {
				_ = WConf([]byte("starterror"), []byte(err.Error()))
				return err
			}

		case "reload":
			newcancel, err := start(confdir, params, r, logger)
			if err == nil {
				if cancel != nil {
					cancel()
				}
				cancel = newcancel
				err := WConf([]byte("reloaded"), utils.NOW)
				if err != nil {
					return err
				}
			} else {
				logger.Warn("Error reloading configuration", "error", err)
			}

		case "confdir":
			if len(parts) == 2 {
				confdir = strings.TrimSpace(parts[1])
			} else {
				return fmt.Errorf("Empty confdir command")
			}
		case "consulparams":
			if len(parts) == 2 {
				newparams := consul.ConnParams{}
				err := json.Unmarshal([]byte(parts[1]), &params)
				if err == nil {
					params = newparams
				} else {
					return fmt.Errorf("Error unmarshaling consulparams received from parent: %s", err.Error())
				}
			} else {
				return fmt.Errorf("Empty consulparams command")
			}
		case "stop":
			if cancel != nil {
				cancel()
			}
			return nil
		default:
			return fmt.Errorf("Unknown command")
		}

	}
	return scanner.Err()
}
