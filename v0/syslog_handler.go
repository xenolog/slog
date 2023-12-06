/*
SyslogHandler provides message delivery to syslog functionality
to any [slog.Handler] compotible handler, which able to output to an [io.Writer].
Interaction with Syslog server should be setup before [slog.Logger]/[slog.Handler] setup:

	syslogPx := mlog.NewSyslogProxy(...)
	logHandler := slog.NewJSONHandler(syslogPx.Writer(), nil)
	syslogHandler := mlog.NewSyslogHandler(syslogPx, logHandler, &SyslogHandlerOptions{...})

	if err := syslogPx.Connect("udp://1.2.3.4:514"); err != nil {
		// Handle dial to syslog server error
	}
	logger := slog.New(syslogHandler)
	logger.Info("very important message")
*/
package mlog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	logSyslog "log/syslog"
	"net"
	netURL "net/url"
	"os"
	"slices"
	"sync"
	"time"
)

const (
	severityMask = 0x07 // see log/syslog code
	facilityMask = 0xf8 // see log/syslog code
)

var allowedProto = []string{"tcp", "udp", "unix"} //nolint:gochecknoglobals

// -----------------------------------------------------------------------------
// SyslogProxy is a type that provides interaction with the syslog Proxyserver
type SyslogProxy struct {
	buf            *bytes.Buffer
	lineProcessBuf []byte
	priority       logSyslog.Priority
	tag            string
	// hostname       string
	url          string
	conn         net.Conn // connection, disconnected if nil
	useLocalTZ   bool
	timeout      time.Duration
	stderrLogger *slog.Logger
	mu           *sync.Mutex
}

// Writer returns a [io.Writer] which may be used
// to fill buffer, shich will be send to Syslog server later
func (s *SyslogProxy) Writer() io.Writer {
	return s.buf
}

// func (s *SyslogProxy) LocalBuffer() bufio.ReadWriter {
// 	return s.buf
// }

func (s *SyslogProxy) Lock() {
	s.mu.Lock()
}

func (s *SyslogProxy) Unlock() {
	s.mu.Unlock()
}

// Connect (or re-connect) to the given syslog server or socket
// url should be in one of following format:
//
//	tcp://1.2.3.4:514
//	udp://1.2.3.4:514
//	unix:///var/run/syslog
//
// if timeout is 0 the default timeout will be used
func (s *SyslogProxy) Connect(url string, timeout time.Duration) error {
	var addr, proto string

	if url == "" {
		// use previous url
		url = s.url
	}

	// parse URL
	u, err := netURL.Parse(url)
	if err != nil {
		return fmt.Errorf("%w: URL `%s` is wrong: %w", ErrSyslogURLparse, url, err)
	}
	if slices.Index(allowedProto, u.Scheme) == -1 {
		return fmt.Errorf("%w: URL `%s` is wrong: unsupported proto '%s', allowed only %v", ErrSyslogURLparse, url, u.Scheme, allowedProto)
	}
	proto = u.Scheme
	if proto == "unix" {
		addr = u.Path
	} else {
		addr = u.Host
	}
	s.url = fmt.Sprintf("%s://%s", proto, addr)

	if timeout == 0 {
		timeout = defaultDialTimeout
	}

	// dial to the Syslog server
	c, err := net.DialTimeout(proto, addr, timeout)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSyslogConnection, err)
	}

	// Set successfully connected Syslog server as destination for messages
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Disconnect()
	s.conn = c
	s.timeout = timeout
	return nil
}

func (s *SyslogProxy) IsConnected() bool {
	return s.conn != nil
}

func (s *SyslogProxy) Disconnect() {
	if s.conn != nil {
		s.conn.Close() // revive:disable:unhandled-error
		s.conn = nil
	}
}

// ProcessLines process each line of LocalBuffer by given function.
// be carefully, strongly recommended wrap this call by mutex Lock()/Unlock.
func (s *SyslogProxy) ProcessLines(processFunc func([]byte) ([]byte, error)) (err error) {
	if !s.IsConnected() {
		return fmt.Errorf("%w: not connected", ErrSyslogConnection)
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		return fmt.Errorf("%w: %w", ErrSyslogConnection, err)
	}

	// todo(sv): Should be rewriten for async usage !!!
	// all processing should have ability to execute in separated goroutine, i.e. threadsafe
	scanner := bufio.NewScanner(s.buf)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			line, err := processFunc([]byte(line))
			if err != nil {
				return err // just return err from user provided function
			}
			_, err = s.conn.Write(line)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrSyslogWrite, err)
			}
			if line[len(line)-1] != '\n' { // add EOL if not present after processing by user function
				_, err = s.conn.Write([]byte("\n")) // each line should leads by \n it is a Syslog protocol requirements
				if err != nil {
					return fmt.Errorf("%w: %w", ErrSyslogWrite, err)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrSyslogProcessHandleResult, err)
	}
	return nil
}

type SyslogProxyOptions struct {
	UseLocalTZ         bool
	Priority           logSyslog.Priority
	Tag                string
	IOBufSize          int
	LineProcessBufSize int
}

// NewSyslog setup and return [Syslog] entity.
func NewSyslogProxy(opts *SyslogProxyOptions) *SyslogProxy {
	if opts == nil {
		opts = &SyslogProxyOptions{}
	}

	// hostname, _ := os.Hostname()

	if opts.IOBufSize == 0 {
		opts.IOBufSize = InitialIOBufSize
	}
	if opts.LineProcessBufSize == 0 {
		opts.LineProcessBufSize = InitialLineProcessBufSize
	}
	if opts.Priority < 0 || opts.Priority > logSyslog.LOG_LOCAL7|logSyslog.LOG_DEBUG {
		opts.Priority = logSyslog.LOG_INFO | logSyslog.LOG_USER
	}

	if opts.Tag == "" {
		opts.Tag = os.Args[0]
	}

	buf := make([]byte, 0, opts.IOBufSize)

	s := &SyslogProxy{
		buf:            bytes.NewBuffer(buf),
		lineProcessBuf: make([]byte, opts.LineProcessBufSize),
		// hostname:       hostname,
		priority:     opts.Priority,
		tag:          opts.Tag,
		useLocalTZ:   opts.UseLocalTZ,
		stderrLogger: slog.New(NewHumanReadableHandler(os.Stderr, nil)),
		mu:           &sync.Mutex{},
	}

	return s
}

//-----------------------------------------------------------------------------

// SyslogHandler currently has no options,
// but this will change in the future and the type is reserved
// to maintain backward compatibility
type SyslogHandlerOptions struct {
	LineProcessFunc func(line []byte) ([]byte, error)
}

// SyslogHandler is a proxy Handler that ensures
// message delivery from any [slog.Handler] to the syslog server
// It should be used in conjunction with mlog.Syslog
type SyslogHandler struct {
	syslogPx        *SyslogProxy
	handler         slog.Handler
	level           slog.Level // should not be set manually. collected from uplevel slog handler
	lineProcessFunc func(line []byte) ([]byte, error)
}

func (h *SyslogHandler) Copy() *SyslogHandler {
	rv := &SyslogHandler{
		syslogPx:        h.syslogPx,
		handler:         h.handler,
		level:           h.level,
		lineProcessFunc: h.lineProcessFunc,
	}
	return rv
}

// Enabled reports whether the handler handles records at the given level. The handler ignores records whose level is lower.
// Implements [slog.Handler] interface.
func (h *SyslogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// WithAttrs returns a new HumanReadableHandler whose attributes consists of h's attributes followed by attrs.
// Implements [slog.Handler] interface.
func (h *SyslogHandler) WithAttrs(aa []slog.Attr) slog.Handler {
	hh := h.Copy()
	hh.handler = h.handler.WithAttrs(aa)
	return hh
}

// WithGroup returns a new HumanReadableHandler with the given group appended to the receiver's existing groups.
// Implements [slog.Handler] interface.
func (h *SyslogHandler) WithGroup(name string) slog.Handler {
	var rv *SyslogHandler
	if name != "" {
		rv = h.Copy()
		rv.handler = h.handler.WithGroup(name)
	} else {
		rv = h
	}
	return rv
}

// Handle handles the Record.
// It will only be called when Enabled(...) returns true.
// Implements [slog.Handler] interface.
func (h *SyslogHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic
	if !h.syslogPx.IsConnected() {
		return fmt.Errorf("%w: not connected", ErrSyslogConnection)
	}

	h.syslogPx.Lock() // should be locked before call chield's Handle() because Writer used by one
	defer h.syslogPx.Unlock()
	if err := h.handler.Handle(ctx, r); err != nil {
		return fmt.Errorf("%w: %w", ErrSyslogHandle, err)
	}

	err := h.syslogPx.ProcessLines(h.lineProcessFunc)

	return err
}

func NewSyslogHandler(syslogPx *SyslogProxy, h slog.Handler, opts *SyslogHandlerOptions) *SyslogHandler {
	if opts == nil {
		opts = &SyslogHandlerOptions{}
	}
	if opts.LineProcessFunc == nil {
		opts.LineProcessFunc = func(line []byte) ([]byte, error) {
			rv, _ := TrimTimestamp(line)
			return rv, nil // `unable to trim timestamp` is not a global error
		}
	}
	sh := &SyslogHandler{
		syslogPx:        syslogPx,
		handler:         h,
		lineProcessFunc: opts.LineProcessFunc,
	}
	for _, logLevel := range allowedLevels {
		if h.Enabled(context.TODO(), logLevel) {
			sh.level = logLevel
			break
		}
	}
	return sh
}
