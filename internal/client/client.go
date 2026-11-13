// Package client is the thin forwarder: it sends one Request to the local
// server (auto-starting it detached when needed) and streams Frames back.
// With TMVP_NO_SERVER=1 it runs the Engine in-process (tests, CI, debugging).
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/server"
)

// InProcess reports whether TMVP_NO_SERVER is set.
func InProcess() bool { v := os.Getenv("TMVP_NO_SERVER"); return v != "" && v != "0" }

var (
	localOnce sync.Once
	localEng  *server.Engine
)

// LocalEngine is the in-process engine (shared for the process lifetime).
func LocalEngine() *server.Engine {
	localOnce.Do(func() { localEng = server.NewEngine() })
	return localEng
}

// Do sends req and calls fn for every frame except the final "done".
// It returns the exit code from the done frame.
func Do(ctx context.Context, req protocol.Request, fn func(protocol.Frame)) (int, error) {
	req.Proto = protocol.Version
	if InProcess() {
		pr, pw := io.Pipe()
		go func() {
			server.Serve(ctx, LocalEngine(), req, pw)
			pw.Close()
		}()
		return read(pr, fn)
	}
	req.Build = protocol.SelfBuild()
	stale := false
	code, err := send(ctx, req, func(f protocol.Frame) {
		if f.Type == protocol.FrameError && f.Error != nil && f.Error.Code == errs.ServerStale {
			stale = true // nothing ran; restart the server from this binary and retry once
			return
		}
		fn(f)
	})
	if !stale || err != nil {
		return code, err
	}
	if err := restartStale(ctx); err != nil {
		return 1, err
	}
	return send(ctx, req, fn)
}

func send(ctx context.Context, req protocol.Request, fn func(protocol.Frame)) (int, error) {
	c, err := Dial(ctx, req.Op != protocol.OpStop && req.Op != protocol.OpStatus)
	if err != nil {
		return 1, err
	}
	defer c.Close()
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return 1, err
	}
	return read(c, fn)
}

// restartStale stops a server that runs another build and waits until its
// process is gone (its <sock>.lock is free); the retry's Dial then starts
// one from this binary.
func restartStale(ctx context.Context) error {
	_, _ = send(ctx, protocol.Request{Proto: protocol.Version, Op: protocol.OpStop}, func(protocol.Frame) {})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if unlock, err := server.LockPath(protocol.SocketPath()+".lock", false); err == nil {
			unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errs.New(errs.Runtime, "an older tmvp server did not stop").WithHint("tmvp server stop, then retry")
}

func read(r io.Reader, fn func(protocol.Frame)) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var f protocol.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			continue
		}
		if f.Type == protocol.FrameDone {
			if f.Error != nil {
				fn(protocol.Frame{Type: protocol.FrameError, Error: f.Error})
			}
			return f.Exit, nil
		}
		fn(f)
	}
	return 1, errs.New(errs.Runtime, "lost connection to tmvp server before the command finished").
		WithHint("in-flight reads failed; writes are never retried automatically — check `tmvp logs --writes`")
}

// ErrNotRunning is returned by Dial(autostart=false) when no server answers.
var ErrNotRunning = errors.New("tmvp server is not running")

// Dial connects to the server, starting it when autostart is set.
func Dial(ctx context.Context, autostart bool) (net.Conn, error) {
	sock := protocol.SocketPath()
	if runtime.GOOS != "windows" && len(sock) > 103 {
		return nil, errs.New(errs.Config, "socket path too long (%d > 103 bytes): %s", len(sock), sock).
			WithHint("set TMVP_SOCKET (or TMVP_HOME) to a shorter path")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err == nil {
		return c, nil
	}
	if !autostart {
		return nil, ErrNotRunning
	}
	// Single-flight auto-start: serialize spawners on a lock next to the
	// socket, then re-check — another client may have started it meanwhile.
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "start tmvp server")
	}
	unlock, err := server.LockPath(sock+".start.lock", true)
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "start tmvp server: lock")
	}
	defer unlock()
	if c, err = d.DialContext(ctx, "unix", sock); err == nil {
		return c, nil
	}
	if err := StartServer(); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "start tmvp server")
	}
	deadline := time.Now().Add(5 * time.Second)
	for wait := 10 * time.Millisecond; time.Now().Before(deadline); wait = min(wait*2, 200*time.Millisecond) {
		if c, err = d.DialContext(ctx, "unix", sock); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, errs.Wrap(errs.Runtime, err, "tmvp server did not come up").
		WithHint("see " + filepath.Join(protocol.RunDir(), "server.log"))
}

// StartServer launches `tmvp server run` detached from this process.
func StartServer() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := server.SecureRunDir(protocol.RunDir()); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(protocol.RunDir(), "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "server", "run")
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), "TMVP_SERVER_CHILD=1")
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
