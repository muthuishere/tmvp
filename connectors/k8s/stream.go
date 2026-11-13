package k8s

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdystream "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	streamhttp "k8s.io/streaming/pkg/httpstream"

	"github.com/muthuishere/tmvp/internal/errs"
)

// Streaming (exec / port-forward) goes through client-go. client-go's SPDY
// and websocket transports ignore rest.Config.Dial, so the SPDY upgrade
// transport is built here with the target's via-aware dialer (as an
// UpgradeTransport). The websocket transports (exec over v5.channel.k8s.io,
// port-forward as SPDY-over-websocket) are only used — with SPDY as the
// fallback — when there is no via: chain, so a tunnel is never bypassed.

func shouldFallback(err error) bool {
	// client-go v0.37 raises the streaming module's UpgradeFailureError; check both.
	return httpstream.IsUpgradeFailure(err) || streamhttp.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
}

// spdyTransport returns the SPDY round tripper (auth/WrapTransport applied)
// and its upgrader, dialing through the target's Dialer.
func (c *client) spdyTransport() (http.RoundTripper, spdy.Upgrader, error) {
	tc, err := rest.TLSConfigFor(c.cfg)
	if err != nil {
		return nil, nil, errs.New(errs.Config, "kubeconfig TLS: %s", scrubHost(err.Error(), c.rc.Server))
	}
	up, err := spdystream.NewRoundTripperWithConfig(spdystream.RoundTripperConfig{
		UpgradeTransport: &http.Transport{DialContext: c.dial, TLSClientConfig: tc},
		PingPeriod:       5 * time.Second,
	})
	if err != nil {
		return nil, nil, errs.Wrap(errs.Runtime, err, "spdy")
	}
	rt, err := rest.HTTPWrappersForConfig(c.cfg, up)
	if err != nil {
		return nil, nil, errs.Wrap(errs.Runtime, err, "spdy")
	}
	return rt, up, nil
}

// ---- port-forward ----

// portForward opens one forwarded connection to pod:port. The returned
// net.Conn supports deadlines (it is a net.Pipe bridged to the data stream).
func (c *client) portForward(ctx context.Context, ns, pod string, port int) (net.Conn, error) {
	u := c.url("/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/portforward", nil)
	rt, up, err := c.spdyTransport()
	if err != nil {
		return nil, err
	}
	var d httpstream.Dialer = spdy.NewDialer(up, &http.Client{Transport: rt}, http.MethodPost, u)
	if c.direct {
		if ws, err := portforward.NewSPDYOverWebsocketDialer(u, c.cfg); err == nil {
			d = portforward.NewFallbackDialer(ws, d, shouldFallback)
		}
	}
	type res struct {
		conn httpstream.Connection
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		conn, _, err := d.Dial(portforward.PortForwardProtocolV1Name)
		ch <- res{conn, err}
	}()
	var conn httpstream.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, c.mapErr(ctx, r.err)
		}
		conn = r.conn
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, errs.New(errs.Timeout, "kubernetes port-forward: dial timed out")
	}
	h := http.Header{}
	h.Set(corev1.StreamType, corev1.StreamTypeError)
	h.Set(corev1.PortHeader, strconv.Itoa(port))
	h.Set(corev1.PortForwardRequestIDHeader, "0")
	errStream, err := conn.CreateStream(h)
	if err != nil {
		conn.Close()
		return nil, errs.Wrap(errs.Unreachable, err, "port-forward error stream")
	}
	_ = errStream.Close() // we never write to it
	h.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(h)
	if err != nil {
		conn.Close()
		return nil, errs.Wrap(errs.Unreachable, err, "port-forward data stream")
	}
	local, remote := net.Pipe()
	shut := func() {
		_ = remote.Close()
		conn.RemoveStreams(data, errStream)
		_ = conn.Close()
	}
	go func() { // server-reported failure (e.g. nothing listening on the port)
		if b, _ := io.ReadAll(io.LimitReader(errStream, 4096)); len(b) > 0 {
			shut()
		}
	}()
	go func() { _, _ = io.Copy(remote, data); shut() }()
	go func() { _, _ = io.Copy(data, remote); _ = data.Close(); shut() }()
	go func() { <-conn.CloseChan(); _ = remote.Close() }()
	return local, nil
}

// ---- exec ----

type exitStatus interface{ ExitStatus() int }

// execStream runs cmd in pod (stdout/stderr only, no tty) and returns its exit code.
func (c *client) execStream(ctx context.Context, ns, pod, container string, cmd []string, stdout, stderr io.Writer) (int, error) {
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "command": cmd}
	if container != "" {
		q.Set("container", container)
	}
	u := c.url("/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/exec", q)
	rt, up, err := c.spdyTransport()
	if err != nil {
		return 0, err
	}
	ex, err := remotecommand.NewSPDYExecutorForTransports(rt, up, http.MethodPost, u)
	if err != nil {
		return 0, errs.Wrap(errs.Runtime, err, "exec")
	}
	if c.direct {
		if ws, werr := remotecommand.NewWebSocketExecutor(c.cfg, http.MethodGet, u.String()); werr == nil {
			if fb, ferr := remotecommand.NewFallbackExecutor(ws, ex, shouldFallback); ferr == nil {
				ex = fb
			}
		}
	}
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
	var es exitStatus
	if errors.As(err, &es) {
		return es.ExitStatus(), nil
	}
	return 0, c.mapErr(ctx, err)
}
