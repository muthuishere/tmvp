package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/secret"
	"github.com/muthuishere/tmvp/internal/secure"
)

// Serve runs one request on eng and writes redacted JSONL frames to w,
// ending with a "done" frame. Used by the socket server and by the
// in-process client mode, so both paths redact identically.
func Serve(ctx context.Context, eng *Engine, req protocol.Request, w io.Writer) {
	rw := eng.Red.Writer(w)
	var mu sync.Mutex
	write := func(f protocol.Frame) {
		b, err := json.Marshal(f)
		if err != nil {
			b, _ = json.Marshal(protocol.Frame{Type: protocol.FrameError, Error: errs.New(errs.Runtime, "unencodable frame: %v", err)})
		}
		mu.Lock()
		_, _ = rw.Write(append(b, '\n'))
		mu.Unlock()
	}
	exit := 0
	if req.Proto != 0 && req.Proto != protocol.Version {
		write(protocol.Frame{Type: protocol.FrameError, Error: errs.New(errs.Runtime, "protocol mismatch: client %d, server %d", req.Proto, protocol.Version).
			WithHint("tmvp server stop (the next call starts a matching server)")})
		exit = 1
	} else if req.Build != "" && req.Build != protocol.SelfBuild() && req.Op != protocol.OpStop && req.Op != protocol.OpStatus && req.Op != protocol.OpPing {
		write(protocol.Frame{Type: protocol.FrameError, Error: errs.New(errs.ServerStale, "tmvp server runs another build than this client").
			WithHint("tmvp server stop (the next call starts a matching server)")})
		exit = 1
	} else {
		exit = eng.Handle(ctx, req, write)
	}
	write(protocol.Frame{Type: protocol.FrameDone, Exit: exit})
	_ = rw.Flush()
}

// Options for the socket server.
type Options struct {
	Socket string        // default protocol.SocketPath()
	Idle   time.Duration // default 30m (TMVP_IDLE overrides)
}

// IdleFromEnv returns TMVP_IDLE or 30m.
func IdleFromEnv() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TMVP_IDLE")); err == nil && d > 0 {
		return d
	}
	return 30 * time.Minute
}

// ErrAlreadyRunning is returned when another server answers on the socket.
var ErrAlreadyRunning = errors.New("tmvp server already running")

// Run serves eng on a unix socket until ctx ends, server.stop, or idle.
func Run(ctx context.Context, eng *Engine, o Options) error {
	if o.Socket == "" {
		o.Socket = protocol.SocketPath()
	}
	if o.Idle <= 0 {
		o.Idle = IdleFromEnv()
	}
	dir := filepath.Dir(o.Socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Only tighten our own dedicated run dir; never chmod a directory the
	// user pointed TMVP_SOCKET into. The socket itself is created 0600 by
	// the umask in listen (no window where it is group/world-connectable),
	// and checkPeer refuses other OS users anyway.
	if filepath.Clean(dir) == filepath.Clean(protocol.RunDir()) {
		if err := SecureRunDir(dir); err != nil {
			return fmt.Errorf("secure run dir %s: %w", dir, err)
		}
	}
	// Single-flight: exactly one live server per socket. The lock is held
	// for the server's lifetime and released by the OS if it dies, so a
	// crashed server never blocks the next one.
	unlock, err := LockPath(o.Socket+".lock", false)
	if IsLocked(err) {
		return ErrAlreadyRunning
	}
	if err != nil {
		return err
	}
	defer unlock()
	if n := secret.CleanStale(15 * time.Minute); n > 0 {
		log.Printf("tmvp server: removed %d stale secret temp dir(s)", n)
	}
	if c, err := net.DialTimeout("unix", o.Socket, 300*time.Millisecond); err == nil {
		c.Close()
		return ErrAlreadyRunning
	}
	// Nobody answered and we hold the lock: whatever is there is stale
	// (only unlink after a failed connect).
	_ = os.Remove(o.Socket)
	ln, err := listen(o.Socket)
	if err != nil {
		return err
	}
	if err := secure.File(o.Socket); err != nil {
		ln.Close()
		return fmt.Errorf("secure socket %s: %w", o.Socket, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	eng.OnStop(cancel)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		tick := o.Idle / 20
		if tick < 50*time.Millisecond {
			tick = 50 * time.Millisecond
		}
		if tick > 30*time.Second {
			tick = 30 * time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if eng.Idle() >= o.Idle {
					log.Printf("tmvp server: idle %s, exiting", o.Idle)
					cancel()
					return
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConn(ctx, eng, c)
		}()
	}
	wg.Wait()
	eng.Shutdown()
	_ = os.Remove(o.Socket)
	return nil
}

func handleConn(ctx context.Context, eng *Engine, c net.Conn) {
	defer c.Close()
	if err := checkPeer(c); err != nil {
		log.Printf("tmvp server: refused connection: %v", err)
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(io.LimitReader(c, 4<<20), 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var req protocol.Request
	if err := json.Unmarshal(line, &req); err != nil {
		b, _ := json.Marshal(protocol.Frame{Type: protocol.FrameDone, Exit: 1, Error: errs.New(errs.Usage, "bad request: %v", err)})
		_, _ = c.Write(append(b, '\n'))
		return
	}
	// a client that goes away cancels its request
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		buf := make([]byte, 1)
		_, _ = c.Read(buf) // returns on close
		cancel()
	}()
	Serve(rctx, eng, req, c)
}

func listen(path string) (net.Listener, error) {
	if len(path) > 103 { // sun_path limit (108 on Linux/Windows, 104 on darwin)
		return nil, fmt.Errorf("socket path too long (%d > 103 bytes): %s; set TMVP_SOCKET or TMVP_HOME to a shorter path", len(path), path)
	}
	var ln net.Listener
	var err error
	withPrivateUmask(func() { ln, err = net.Listen("unix", path) })
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	return ln, nil
}
