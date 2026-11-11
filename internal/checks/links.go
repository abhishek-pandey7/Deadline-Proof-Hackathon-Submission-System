package checks

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// LinkOptions configure the link checker.
type LinkOptions struct {
	Timeout time.Duration // per link; default 5s
	// AllowPrivate lets tests reach httptest servers on 127.0.0.1. Never set
	// it in production: it would let a submitted link probe the server's network.
	AllowPrivate bool
	RootCAs      *x509.CertPool // tests trust their own TLS server
}

// LinkChecker checks that submitted links respond, without letting a link
// reach private, loopback or link-local addresses (SSRF).
type LinkChecker struct {
	client  *http.Client
	timeout time.Duration
}

var errBlockedAddress = errors.New("the link points to a private or local address")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// blocked reports whether connecting to addr could reach something private.
func blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || cgnat.Contains(addr)
}

// NewLinkChecker builds a checker. The address check runs on the resolved IP
// at connect time, so DNS tricks (rebinding) cannot get past it.
func NewLinkChecker(o LinkOptions) *LinkChecker {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	dialer := &net.Dialer{
		Timeout: o.Timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if o.AllowPrivate {
				return nil
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil || blocked(ap.Addr()) {
				return errBlockedAddress
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:               nil, // never route link checks through an environment proxy
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: o.Timeout,
		TLSClientConfig:     &tls.Config{RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12},
		MaxIdleConns:        20,
		IdleConnTimeout:     30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   o.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("redirected away from https")
			}
			return nil
		},
	}
	return &LinkChecker{client: client, timeout: o.Timeout}
}

// LinkResult is one link's outcome.
type LinkResult struct {
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CheckAll checks every link in parallel and passes only if all respond.
func (c *LinkChecker) CheckAll(ctx context.Context, links []string) Result {
	out := make([]LinkResult, len(links))
	var wg sync.WaitGroup
	for i, l := range links {
		wg.Go(func() { out[i] = c.Check(ctx, l) })
	}
	wg.Wait()
	passed := true
	for _, r := range out {
		passed = passed && r.OK
	}
	return Result{Name: CheckLinks, Passed: passed, Detail: map[string]any{"links": out}}
}

// Check sends HEAD, falling back to a one-byte GET for servers that refuse
// HEAD. A link passes on any final status below 400.
func (c *LinkChecker) Check(ctx context.Context, url string) LinkResult {
	res := LinkResult{URL: url}
	status, err := c.do(ctx, http.MethodHead, url)
	if err == nil && (status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented || status == http.StatusForbidden) {
		status, err = c.do(ctx, http.MethodGet, url)
	}
	if err != nil {
		res.Error = describe(err)
		return res
	}
	res.Status = status
	res.OK = status < 400
	if !res.OK {
		res.Error = fmt.Sprintf("the link answered %d %s", status, http.StatusText(status))
	}
	return res
}

func (c *LinkChecker) do(ctx context.Context, method, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "DeadlineCrush-LinkCheck/1.0")
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

func describe(err error) string {
	switch {
	case errors.Is(err, errBlockedAddress):
		return errBlockedAddress.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the link did not answer within 5 seconds"
	default:
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return "the link's domain does not resolve"
		}
		return "the link could not be reached"
	}
}
