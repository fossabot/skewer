package namespaces

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kardianos/osext"
	"github.com/stephane-martin/skewer/sys/kring"
)

type NamespacedCmd struct {
	cmd          *PluginCmd
	dumpable     bool
	storePath    string
	confPath     string
	acctPath     string
	fileDestTmpl string
	certFiles    []string
	certPaths    []string
	polldirs     []string
}

func NewNamespacedCmd(cmd *PluginCmd) *NamespacedCmd {
	return &NamespacedCmd{cmd: cmd}
}

func (c *NamespacedCmd) Dumpable(dumpable bool) *NamespacedCmd {
	c.dumpable = dumpable
	return c
}

func (c *NamespacedCmd) StorePath(path string) *NamespacedCmd {
	c.storePath = strings.TrimSpace(path)
	return c
}

func (c *NamespacedCmd) ConfPath(path string) *NamespacedCmd {
	c.confPath = strings.TrimSpace(path)
	return c
}

func (c *NamespacedCmd) AccountingPath(path string) *NamespacedCmd {
	c.acctPath = strings.TrimSpace(path)
	return c
}

func (c *NamespacedCmd) FileDestTemplate(tmpl string) *NamespacedCmd {
	c.fileDestTmpl = strings.TrimSpace(tmpl)
	return c
}

func (c *NamespacedCmd) CertFiles(list []string) *NamespacedCmd {
	c.certFiles = list
	return c
}

func (c *NamespacedCmd) CertPaths(list []string) *NamespacedCmd {
	c.certPaths = list
	return c
}

func (c *NamespacedCmd) PollDirectories(dirs []string) *NamespacedCmd {
	c.polldirs = dirs
	return c
}

type PluginCmd struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
}

func (cmd *PluginCmd) Start() error {
	return cmd.Cmd.Start()
}

func (cmd *PluginCmd) Wait() error {
	return cmd.Cmd.Wait()
}

func (cmd *PluginCmd) Kill() error {
	return cmd.Cmd.Process.Kill()
}

func (cmd *PluginCmd) Namespaced() *NamespacedCmd {
	return NewNamespacedCmd(cmd)
}

func (cmd *PluginCmd) AppendEnv(moreenvs []string) {
	cmd.Cmd.Env = append(cmd.Cmd.Env, moreenvs...)
}

func (cmd *PluginCmd) SetSysProcAttr(attrs *syscall.SysProcAttr) {
	cmd.Cmd.SysProcAttr = attrs
}

type CmdOpts struct {
	name        string
	ring        kring.Ring
	loggerHdl   uintptr
	binderHdl   uintptr
	messagePipe *os.File
	profile     bool
}

func BinderHandle(hdl uintptr) func(*CmdOpts) {
	return func(opts *CmdOpts) {
		opts.binderHdl = hdl
	}
}

func LoggerHandle(hdl uintptr) func(*CmdOpts) {
	return func(opts *CmdOpts) {
		opts.loggerHdl = hdl
	}
}

func Pipe(pipe *os.File) func(*CmdOpts) {
	return func(opts *CmdOpts) {
		opts.messagePipe = pipe
	}
}

func Profile(profile bool) func(*CmdOpts) {
	return func(opts *CmdOpts) {
		opts.profile = profile
	}
}

func SetupCmd(name string, ring kring.Ring, funcopts ...func(*CmdOpts)) (cmd *PluginCmd, err error) {
	opts := &CmdOpts{
		name: name,
		ring: ring,
	}
	for _, f := range funcopts {
		f(opts)
	}
	cmd = &PluginCmd{}
	exe, err := osext.Executable()
	if err != nil {
		return nil, err
	}
	envs := []string{"PATH=/bin:/usr/bin", fmt.Sprintf("SKEWER_SESSION=%s", opts.ring.GetSessionID().String())}
	files := []*os.File{}
	if opts.binderHdl != 0 {
		files = append(files, os.NewFile(opts.binderHdl, "binder"))
		envs = append(envs, "SKEWER_HAS_BINDER=TRUE")
	}
	if opts.loggerHdl != 0 {
		files = append(files, os.NewFile(opts.loggerHdl, "logger"))
		envs = append(envs, "SKEWER_HAS_LOGGER=TRUE")
	}
	if opts.messagePipe != nil {
		files = append(files, opts.messagePipe)
		envs = append(envs, "SKEWER_HAS_PIPE=TRUE")
	}
	if opts.profile {
		envs = append(envs, "SKEWER_PROFILE=TRUE")
	}
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	files = append(files, rPipe)
	err = opts.ring.WriteRingPass(wPipe)
	_ = wPipe.Close()
	if err != nil {
		return nil, err
	}

	cmd.Cmd = &exec.Cmd{
		Path:       exe,
		Stderr:     os.Stderr,
		ExtraFiles: files,
		Env:        envs,
		Args:       []string{name},
	}
	cmd.Stdin, err = cmd.Cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, err = cmd.Cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

type envPaths struct {
	acctParentDir     string
	fileDestParentDir string
	storePath         string
	confPath          string
	certFiles         []string
	certPaths         []string
	polldirs          []string
}

func setupEnv(paths envPaths, ttyName string) (env []string) {
	env = []string{}
	if len(ttyName) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_TTYNAME=%s", ttyName))
	}

	if len(paths.confPath) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_CONF_DIR=%s", paths.confPath))
	}

	if len(paths.storePath) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_STORE_PATH=%s", paths.storePath))
	}

	if len(paths.acctParentDir) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_ACCT_DIR=%s", paths.acctParentDir))
	}

	if len(paths.fileDestParentDir) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_FILEDEST_DIR=%s", paths.fileDestParentDir))
	}

	if len(paths.certFiles) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_CERT_FILES=%s", strings.Join(paths.certFiles, string(filepath.ListSeparator))))
	}

	if len(paths.certPaths) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_CERT_PATHS=%s", strings.Join(paths.certPaths, string(filepath.ListSeparator))))
	}

	if len(paths.polldirs) > 0 {
		env = append(env, fmt.Sprintf("SKEWER_POLLDIRS=%s", strings.Join(paths.polldirs, string(filepath.ListSeparator))))
	}

	_, err := exec.LookPath("systemctl")
	if err == nil {
		env = append(env, "SKEWER_HAVE_SYSTEMCTL=TRUE")
	}

	return env
}

type baseMountPoint struct {
	Source string
	Target string
}

type bindMountPoint struct {
	baseMountPoint
	Flags    uintptr
	ReadOnly bool
	IsDir    bool
}

type mountPoint struct {
	baseMountPoint
	Flags uintptr
	Fs    string
	Data  string
}
