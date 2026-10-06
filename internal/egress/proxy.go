package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/source"
)

type Entry struct {
	Host         string
	Port         int // 0 means 443.
	AllowPrivate bool
}

type run struct {
	token   string
	allow   []Entry
	blocked map[string]int
}

type Proxy struct {
	listen  string
	resolve func(context.Context, string, bool) ([]netip.Addr, error)
	mu      sync.Mutex
	addr    string
	runs    map[string]*run
	tunnels chan struct{}
}

func New(listen string, resolve func(context.Context, string, bool) ([]netip.Addr, error)) *Proxy {
	if resolve == nil {
		resolve = source.ResolveAllowed
	}
	return &Proxy{listen: listen, resolve: resolve, runs: make(map[string]*run), tunnels: make(chan struct{}, 256)}
}

func (p *Proxy) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

func (p *Proxy) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", p.listen)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.addr = listener.Addr().String()
	p.mu.Unlock()
	return p.serve(ctx, listener)
}

// ServeUnix serves the proxy on a unix socket (0660, group of the parent
// directory) and removes it when ctx is done.
func (p *Proxy) ServeUnix(ctx context.Context, path string) error {
	os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := os.Chmod(path, 0o660); err != nil {
		listener.Close()
		return err
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		listener.Close()
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(path, -1, int(st.Gid)); err != nil && !errors.Is(err, syscall.EPERM) {
			listener.Close()
			return err
		}
	}
	return p.serve(ctx, listener)
}

func (p *Proxy) serve(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	// All connections (pre-auth and tunnels): bounded so a run cannot
	// exhaust agentgw's fds; tunnels have their own, lower cap.
	pending := make(chan struct{}, 512)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// e.g. EMFILE: back off and keep serving rather than leave every
			// later run without network until a restart.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case pending <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		go func() {
			defer func() { <-pending }()
			p.handle(ctx, conn)
		}()
	}
}

func (p *Proxy) Register(allow []Entry) (string, func() map[string]int, func()) {
	var id [8]byte
	var token [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	if _, err := rand.Read(token[:]); err != nil {
		panic(err)
	}
	name := "run-" + hex.EncodeToString(id[:])
	r := &run{token: hex.EncodeToString(token[:]), allow: append([]Entry(nil), allow...), blocked: make(map[string]int)}
	p.mu.Lock()
	p.runs[name] = r
	addr := p.addr
	p.mu.Unlock()
	blocked := func() map[string]int {
		p.mu.Lock()
		defer p.mu.Unlock()
		copy := make(map[string]int, len(r.blocked))
		for host, count := range r.blocked {
			copy[host] = count
		}
		return copy
	}
	release := func() {
		p.mu.Lock()
		delete(p.runs, name)
		p.mu.Unlock()
	}
	return "http://" + name + ":" + r.token + "@" + addr, blocked, release
}

func (p *Proxy) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	var head []byte
	for len(head) < 8192 {
		b, err := reader.ReadByte()
		if err != nil {
			return
		}
		head = append(head, b)
		if bytes.HasSuffix(head, []byte("\r\n\r\n")) {
			break
		}
	}
	if !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		writeError(conn, http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		writeError(conn, http.StatusBadRequest)
		return
	}
	if req.Method != http.MethodConnect {
		writeError(conn, http.StatusMethodNotAllowed)
		return
	}
	user, token, ok := basic(req.Header.Get("Proxy-Authorization"))
	p.mu.Lock()
	r := p.runs[user]
	valid := ok && r != nil && subtle.ConstantTimeCompare([]byte(token), []byte(r.token)) == 1
	p.mu.Unlock()
	if !valid {
		io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"agentgw\"\r\nContent-Length: 0\r\n\r\n")
		return
	}
	host, portText, err := net.SplitHostPort(req.Host)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || host == "" || port < 1 || port > 65535 {
		p.block(r, req.Host)
		writeError(conn, http.StatusForbidden)
		return
	}
	var entry *Entry
	for i := range r.allow {
		e := &r.allow[i]
		eport := e.Port
		if eport == 0 {
			eport = 443
		}
		pattern := strings.ToLower(e.Host)
		name := strings.ToLower(host)
		if port == eport && (name == pattern || strings.HasPrefix(pattern, "*.") && strings.HasSuffix(name, pattern[1:]) && len(name) > len(pattern)-1) {
			entry = e
			break
		}
	}
	if entry == nil {
		p.block(r, net.JoinHostPort(host, portText))
		writeError(conn, http.StatusForbidden)
		return
	}
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, err := p.resolve(rctx, host, entry.AllowPrivate)
	rcancel()
	if err == nil && len(addrs) == 0 {
		err = errors.New("no address found")
	}
	if err == nil {
		for _, addr := range addrs {
			_, err = source.ResolveAllowed(ctx, addr.String(), entry.AllowPrivate)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		p.block(r, net.JoinHostPort(host, portText))
		writeError(conn, http.StatusForbidden)
		return
	}
	select {
	case p.tunnels <- struct{}{}:
		defer func() { <-p.tunnels }()
	default:
		writeError(conn, http.StatusServiceUnavailable)
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Try each checked address in order (a dual-stack name may list an
	// unreachable family first); never re-resolve.
	var upstream net.Conn
	for _, addr := range addrs {
		if upstream, err = (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(addr.String(), portText)); err == nil {
			break
		}
	}
	if err != nil {
		writeError(conn, http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { tunnel(upstream, conn, reader); done <- struct{}{} }()
	go func() { tunnel(conn, upstream, upstream); done <- struct{}{} }()
	<-done
	conn.Close()
	upstream.Close()
	<-done
}

func tunnel(dst, source net.Conn, reader io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		source.SetReadDeadline(time.Now().Add(5 * time.Minute))
		n, err := reader.Read(buf)
		if n > 0 {
			for sent := 0; sent < n; {
				dst.SetWriteDeadline(time.Now().Add(5 * time.Minute))
				written, writeErr := dst.Write(buf[sent:n])
				sent += written
				if writeErr != nil || written == 0 {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// maxBlocked caps the distinct hosts recorded per run (and so the audit rows
// and output lines); the rest are counted under "other".
const maxBlocked = 64

func (p *Proxy) block(r *run, host string) {
	// The name comes from the sandbox: keep it short and printable.
	host = strings.Map(func(c rune) rune {
		if c < 0x21 || c > 0x7e {
			return '?'
		}
		return c
	}, host)
	if len(host) > 255 {
		host = host[:255]
	}
	p.mu.Lock()
	if _, ok := r.blocked[host]; !ok && len(r.blocked) >= maxBlocked {
		host = "other"
	}
	r.blocked[host]++
	p.mu.Unlock()
}

func basic(header string) (string, string, bool) {
	scheme, encoded, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	user, token, ok := strings.Cut(string(decoded), ":")
	return user, token, ok
}

func writeError(w io.Writer, code int) {
	io.WriteString(w, "HTTP/1.1 "+strconv.Itoa(code)+" "+http.StatusText(code)+"\r\nContent-Length: 0\r\n\r\n")
}
