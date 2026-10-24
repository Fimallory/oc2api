package gateway

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
)

// bunTransport puts requests on the wire the way Bun's fetch does, so the
// upstream fingerprint matches a real OpenCode client: Bun's BoringSSL
// ClientHello, HTTP/1.1 only, and Bun's header order. Go's crypto/tls and
// net/http cannot do that, and the free tier treats the difference as
// "not OpenCode" (403 FreeTierError) or as abuse (429).
type bunTransport struct {
	proxy                 *url.URL
	dialer                *net.Dialer
	responseHeaderTimeout time.Duration
	handshakeTimeout      time.Duration
}

// bunCommonHeaders lists the headers WebKit knows by name, in the order Bun's
// fetch writes them.
var bunCommonHeaders = []string{
	"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language", "Authorization", "Cache-Control",
	"Connection", "Content-Encoding", "Content-Language", "Content-Length", "Content-Type", "Cookie", "DNT",
	"Expect", "Host", "If-None-Match", "Keep-Alive", "Last-Event-Id", "Origin", "Pragma",
	"Proxy-Authorization", "Range", "Referer", "Sec-Fetch-Mode", "TE", "Upgrade-Insecure-Requests",
	"User-Agent",
}

// bunClientHello is the ClientHello of Bun 1.4.3's fetch (JA4 t13d1713h1): no
// GREASE, no padding, http/1.1 as the only ALPN protocol, X25519MLKEM768 as
// the first key share.
func bunClientHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// newBunClient builds an http.Client whose transport mimics Bun.
func newBunClient(proxy *url.URL, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &bunTransport{
			proxy:                 proxy,
			dialer:                &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second},
			responseHeaderTimeout: timeout,
			handshakeTimeout:      15 * time.Second,
		},
	}
}

func (t *bunTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("bun transport: unsupported scheme %q", req.URL.Scheme)
	}
	ctx := req.Context()
	host := req.URL.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	conn, err := t.dial(ctx, host)
	if err != nil {
		return nil, err
	}
	// The custom transport reads directly from the socket, so a cancelled
	// context has to close the connection itself.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	tlsConn := utls.UClient(conn, &utls.Config{ServerName: req.URL.Hostname()}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(bunClientHello()); err != nil {
		conn.Close()
		return nil, err
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, t.handshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, err
	}

	var body []byte
	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			conn.Close()
			return nil, err
		}
	}

	head := bunRequestHead(req, body)
	if _, err := tlsConn.Write(head); err != nil {
		conn.Close()
		return nil, err
	}
	if len(body) > 0 {
		if _, err := tlsConn.Write(body); err != nil {
			conn.Close()
			return nil, err
		}
	}

	br := bufio.NewReaderSize(tlsConn, 64<<10)
	if t.responseHeaderTimeout > 0 {
		_ = tlsConn.SetReadDeadline(time.Now().Add(t.responseHeaderTimeout))
	}
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	statusLine = strings.TrimRight(statusLine, "\r\n")
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		conn.Close()
		return nil, fmt.Errorf("bun transport: bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bun transport: bad status %q", parts[1])
	}

	header := make(http.Header)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	_ = tlsConn.SetReadDeadline(time.Time{})

	resp := &http.Response{
		Status:        statusLine,
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Request:       req,
		ContentLength: -1,
	}
	resp.Body = newBunBody(tlsConn, br, header, req.Method)
	if header.Get("Content-Length") != "" {
		if n, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil {
			resp.ContentLength = n
		}
	}
	if req.Method == http.MethodHead || status == http.StatusNoContent || status == http.StatusNotModified {
		resp.Body = io.NopCloser(strings.NewReader(""))
	}
	return resp, nil
}

// bunRequestHead renders the request line and headers the way Bun's fetch
// writes them: common headers in WebKit's order, other headers lowercased and
// sorted, then the headers fetch adds itself.
func bunRequestHead(req *http.Request, body []byte) []byte {
	values := make(map[string]string, len(req.Header))
	for name, list := range req.Header {
		lower := strings.ToLower(name)
		if lower == "host" || lower == "connection" || lower == "content-length" || lower == "transfer-encoding" {
			continue
		}
		value := strings.Join(list, ", ")
		if previous, ok := values[lower]; ok {
			value = previous + ", " + value
		}
		values[lower] = value
	}
	if values["accept"] == "*/*" {
		delete(values, "accept")
	}
	_, hasUserAgent := values["user-agent"]
	_, hasAccept := values["accept"]
	_, hasEncoding := values["accept-encoding"]

	var b strings.Builder
	b.WriteString(req.Method + " " + req.URL.RequestURI() + " HTTP/1.1\r\n")
	written := make(map[string]bool, len(values))
	for _, name := range bunCommonHeaders {
		lower := strings.ToLower(name)
		if lower == "content-length" && body != nil {
			b.WriteString(name + ": " + strconv.Itoa(len(body)) + "\r\n")
			written[lower] = true
			continue
		}
		if value, ok := values[lower]; ok {
			b.WriteString(name + ": " + value + "\r\n")
			written[lower] = true
		}
	}
	rest := make([]string, 0, len(values))
	for name := range values {
		if !written[name] {
			rest = append(rest, name)
		}
	}
	sortStrings(rest)
	for _, name := range rest {
		b.WriteString(name + ": " + values[name] + "\r\n")
	}
	b.WriteString("Connection: keep-alive\r\n")
	if !hasUserAgent {
		b.WriteString("User-Agent: Bun/1.3.14\r\n")
	}
	if !hasAccept {
		b.WriteString("Accept: */*\r\n")
	}
	host := req.URL.Hostname()
	b.WriteString("Host: " + host + "\r\n")
	if !hasEncoding {
		b.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	}
	if body != nil && !written["content-length"] {
		b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// bunBody decodes chunked transfer encoding and gzip content encoding.
func newBunBody(conn net.Conn, br *bufio.Reader, header http.Header, method string) io.ReadCloser {
	var reader io.Reader = br
	if strings.Contains(strings.ToLower(header.Get("Transfer-Encoding")), "chunked") {
		reader = newChunkedReader(br)
	} else if n, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && n >= 0 {
		reader = io.LimitReader(br, n)
	}
	encoding := strings.ToLower(header.Get("Content-Encoding"))
	switch {
	case strings.Contains(encoding, "gzip"):
		if gz, err := gzip.NewReader(reader); err == nil {
			return &bunBody{Reader: gz, closers: []io.Closer{gz, conn}}
		}
	case strings.Contains(encoding, "br"):
		return &bunBody{Reader: brotli.NewReader(reader), closers: []io.Closer{conn}}
	case strings.Contains(encoding, "zstd"):
		if zr, err := zstd.NewReader(reader); err == nil {
			return &bunBody{Reader: zr.IOReadCloser(), closers: []io.Closer{conn}}
		}
	case strings.Contains(encoding, "deflate"):
		return &bunBody{Reader: flate.NewReader(reader), closers: []io.Closer{conn}}
	}
	return &bunBody{Reader: reader, closers: []io.Closer{conn}}
}

type bunBody struct {
	io.Reader
	closers []io.Closer
}

func (b *bunBody) Close() error {
	var first error
	for _, closer := range b.closers {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type chunkedReader struct {
	br        *bufio.Reader
	remaining int64
	done      bool
}

func newChunkedReader(br *bufio.Reader) *chunkedReader { return &chunkedReader{br: br} }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if c.remaining == 0 {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(line, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("bun transport: bad chunk size %q", line)
		}
		if size == 0 {
			c.done = true
			_, _ = c.br.ReadString('\n')
			return 0, io.EOF
		}
		c.remaining = size
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.br.Read(p)
	c.remaining -= int64(n)
	if c.remaining == 0 {
		_, _ = c.br.ReadString('\n')
	}
	if err == io.EOF && c.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (t *bunTransport) dial(ctx context.Context, addr string) (net.Conn, error) {
	if t.proxy == nil {
		return t.dialer.DialContext(ctx, "tcp", addr)
	}
	if t.proxy.Scheme == "socks5" || t.proxy.Scheme == "socks5h" {
		return t.dialSOCKS(ctx, addr)
	}
	conn, err := t.dialer.DialContext(ctx, "tcp", t.proxy.Host)
	if err != nil {
		return nil, err
	}
	auth := ""
	if t.proxy.User != nil {
		pass, _ := t.proxy.User.Password()
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(t.proxy.User.Username()+":"+pass)) + "\r\n"
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", addr, addr, auth)
	br := bufio.NewReader(conn)
	resp, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(resp, " 200 ") {
		conn.Close()
		return nil, fmt.Errorf("bun transport: proxy CONNECT failed: %s", strings.TrimSpace(resp))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return conn, nil
}

func (t *bunTransport) dialSOCKS(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := t.dialer.DialContext(ctx, "tcp", t.proxy.Host)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte{5, 1, 2}); err != nil {
		return fail(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	user := t.proxy.User.Username()
	pass, _ := t.proxy.User.Password()
	request := []byte{1, byte(len(user))}
	request = append(request, user...)
	request = append(request, byte(len(pass)))
	request = append(request, pass...)
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	if reply[1] != 0 {
		return fail(errors.New("bun transport: socks auth failed"))
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fail(err)
	}
	port, _ := strconv.Atoi(portStr)
	request = []byte{5, 1, 0, 3, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fail(err)
	}
	if head[1] != 0 {
		return fail(fmt.Errorf("bun transport: socks connect failed: %d", head[1]))
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4
	case 3:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return fail(err)
		}
		skip = int(size[0])
	case 4:
		skip = 16
	}
	if skip > 0 {
		if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
			return fail(err)
		}
	}
	return conn, nil
}
