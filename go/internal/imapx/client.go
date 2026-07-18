// MailFerry - IMAP Migration & Sync
// High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra <andy@saputra.org>
//
// https://saputra.org
// https://github.com/ajsap/mailferry
//
// Licensed under the GNU Affero General Public License v3.0 (AGPL-3.0).
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// Contributions welcome: submit issues, feature requests and pull requests
// at https://github.com/ajsap/mailferry

// Package imapx: MailFerry's native IMAP client.
//
// One goroutine owns the socket reader and routes responses; the folder
// worker goroutine issues pipelined commands. Message bodies stream from
// the reader to the consumer chunk-by-chunk (constant memory, any size),
// and APPEND literals stream out with LITERAL+ when offered. Inactivity
// and byte-progress are watched; a stalled socket is closed so work can
// reconnect and resume from the last confirmed checkpoint.
package imapx

import (
	"bufio"
	"compress/flate"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ajsap/mailferry/v2/internal/identity"
)

// ------------------------------------------------------------- errors --

type ConnLost struct{ Msg string }

func (e *ConnLost) Error() string { return e.Msg }

type CommandErr struct {
	Name, Status, Text string
	Code               []any
}

func (e *CommandErr) Error() string { return fmt.Sprintf("%s: %s %s", e.Name, e.Status, e.Text) }

type AuthErr struct{ Text string }

func (e *AuthErr) Error() string { return "authentication failure: " + e.Text }

func IsConnLost(err error) bool {
	var c *ConnLost
	return errors.As(err, &c)
}

// ------------------------------------------------------------- pending --

type Result struct {
	Final *Response
	Data  []*Response
}

type Pending struct {
	tag   string
	name  string
	types map[string]bool
	data  []*Response
	ch    chan error
	res   Result
}

// Wait blocks until the tagged completion arrives (or the connection dies).
func (p *Pending) Wait() (Result, error) {
	err := <-p.ch
	return p.res, err
}

// ---------------------------------------------------------- body stream --

// BodyHandle streams one message body from the reader goroutine.
type BodyHandle struct {
	UID    uint32
	sizeCh chan int64
	ch     chan []byte
	errCh  chan error
	size   int64
	failed atomic.Bool
}

// WaitSize blocks until the literal size is known.
func (b *BodyHandle) WaitSize(timeout time.Duration) (int64, error) {
	select {
	case n := <-b.sizeCh:
		b.size = n
		return n, nil
	case err := <-b.errCh:
		return 0, err
	case <-time.After(timeout):
		return 0, &ConnLost{"timed out waiting for message body"}
	}
}

// Chunks yields body chunks until nil (end) or error.
func (b *BodyHandle) Chunks() (<-chan []byte, <-chan error) { return b.ch, b.errCh }

func (b *BodyHandle) fail(err error) {
	if b.failed.CompareAndSwap(false, true) {
		select {
		case b.errCh <- err:
		default:
		}
		close(b.ch)
	}
}

// -------------------------------------------------------------- client --

type Endpoint struct {
	Host     string
	Port     int
	Security string // ssl | tls | none
	User     string
	Password string
}

type SideStats interface {
	RX(n int)
	TX(n int)
	State(s string)
}

type nullSide struct{}

func (nullSide) RX(int)       {}
func (nullSide) TX(int)       {}
func (nullSide) State(string) {}

type Client struct {
	EP         Endpoint
	Timeout    time.Duration
	TLSVerify  bool
	Side       SideStats
	OwnerLabel string
	Log        func(string)
	Trace      bool // wire trace into the mailbox log (credentials redacted)
	Baseline   bool // RFC-3501-only: no LITERAL+, no COMPRESS

	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	zw       *flate.Writer // non-nil once COMPRESS=DEFLATE is active
	deflated atomic.Bool
	wmu      sync.Mutex // socket writes
	mu       sync.Mutex // pending/state
	pending  map[string]*Pending
	order    []string
	bodyFIFO []*BodyHandle
	contCh   chan *Response
	tagN     int

	Caps        map[string]bool
	Greeting    string
	selExists   uint32
	selUV       uint32
	selUIDNext  uint32
	closed      atomic.Bool
	closeErr    error
	lastAct     atomic.Int64 // unix nanos of last socket activity
	pendAppends atomic.Int32
	watchStop   chan struct{}
}

func NewClient(ep Endpoint, timeout time.Duration, tlsVerify bool, side SideStats,
	owner string, log func(string)) *Client {
	if side == nil {
		side = nullSide{}
	}
	if log == nil {
		log = func(string) {}
	}
	return &Client{EP: ep, Timeout: timeout, TLSVerify: tlsVerify, Side: side,
		OwnerLabel: owner, Log: log,
		pending: map[string]*Pending{}, contCh: make(chan *Response, 1),
		Caps: map[string]bool{}, watchStop: make(chan struct{})}
}

func (c *Client) touch() { c.lastAct.Store(time.Now().UnixNano()) }

func (c *Client) Alive() bool { return !c.closed.Load() }

func (c *Client) Has(cap string) bool { return c.Caps[strings.ToUpper(cap)] }

// Connect dials, upgrades TLS as configured and reads the greeting.
func (c *Client) Connect() error {
	c.Side.State("connect")
	addr := fmt.Sprintf("%s:%d", c.EP.Host, c.EP.Port)
	d := net.Dialer{Timeout: minDur(60*time.Second, c.Timeout)}
	var err error
	if c.EP.Security == "ssl" {
		c.conn, err = tls.DialWithDialer(&d, "tcp", addr, c.tlsConfig())
	} else {
		c.conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return &ConnLost{fmt.Sprintf("cannot connect to %s: %v", addr, err)}
	}
	c.br = bufio.NewReaderSize(&countReader{c: c}, 64<<10)
	c.bw = bufio.NewWriterSize(&countWriter{c: c}, 64<<10)
	c.touch()

	greet, err := c.readOne()
	if err != nil {
		c.Abort(err)
		return &ConnLost{"no greeting: " + err.Error()}
	}
	if greet.Status != "OK" && greet.Status != "PREAUTH" {
		c.Abort(nil)
		return &ConnLost{"unexpected greeting: " + greet.Text}
	}
	c.Greeting = greet.Text
	if len(greet.Code) > 0 {
		if s, _ := TokStr(greet.Code[0]); strings.EqualFold(s, "CAPABILITY") {
			c.setCaps(greet.Code[1:])
		}
	}
	if c.EP.Security == "tls" {
		c.Side.State("tls")
		if err := c.starttls(); err != nil {
			c.Abort(err)
			return err
		}
	}
	go c.readerLoop()
	go c.watchdog()
	if len(c.Caps) == 0 {
		if _, err := c.Cmd("CAPABILITY", ""); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: c.EP.Host, InsecureSkipVerify: !c.TLSVerify,
		MinVersion: tls.VersionTLS12}
}

// starttls runs synchronously before the reader loop starts.
func (c *Client) starttls() error {
	tag := c.nextTag()
	if err := c.writeLine(tag + " STARTTLS"); err != nil {
		return err
	}
	for {
		r, err := c.readOne()
		if err != nil {
			return &ConnLost{"STARTTLS: " + err.Error()}
		}
		if r.Tag == tag {
			if r.Status != "OK" {
				return &CommandErr{"STARTTLS", r.Status, r.Text, r.Code}
			}
			break
		}
	}
	tc := tls.Client(c.conn, c.tlsConfig())
	if err := tc.Handshake(); err != nil {
		return &ConnLost{"TLS handshake failed: " + err.Error()}
	}
	c.conn = tc
	c.br = bufio.NewReaderSize(&countReader{c: c}, 64<<10)
	c.bw = bufio.NewWriterSize(&countWriter{c: c}, 64<<10)
	c.Caps = map[string]bool{} // caps must be re-read after TLS
	return nil
}

// readOne: synchronous single-response read (pre-loop phases only).
func (c *Client) readOne() (*Response, error) {
	segs, lits, err := c.readLogicalLine(nil)
	if err != nil {
		return nil, err
	}
	return ParseLine(segs, lits)
}

func (c *Client) setCaps(tokens []any) {
	c.Caps = map[string]bool{}
	for _, t := range tokens {
		if s, ok := TokStr(t); ok {
			c.Caps[strings.ToUpper(s)] = true
		}
	}
}

func (c *Client) nextTag() string {
	c.tagN++
	return fmt.Sprintf("MF%04d", c.tagN)
}

var redactLogin = regexp.MustCompile(`^(\S+ (?:LOGIN "(?:[^"\\]|\\.)*"|AUTHENTICATE \S+)) .*$`)

// trace mirrors protocol lines into the per-mailbox log when --trace is on.
// Credentials are always redacted; literal bodies are never traced.
func (c *Client) trace(dir, line string) {
	if !c.Trace || c.Log == nil {
		return
	}
	if dir == "C:" {
		if m := redactLogin.FindStringSubmatch(line); m != nil {
			line = m[1] + " ****"
		}
	}
	if len(line) > 200 {
		line = line[:200]
	}
	c.Log(dir + " " + line)
}

func (c *Client) writeLine(s string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed.Load() {
		return c.lostErr()
	}
	if _, err := c.bw.WriteString(s + "\r\n"); err != nil {
		return &ConnLost{err.Error()}
	}
	if err := c.flushWire(); err != nil {
		return &ConnLost{err.Error()}
	}
	c.trace("C:", s)
	return nil
}

// flushWire flushes the buffered writer and, when COMPRESS=DEFLATE is
// active, sync-flushes the deflate stream so the server sees the command.
// Callers hold wmu.
func (c *Client) flushWire() error {
	if err := c.bw.Flush(); err != nil {
		return err
	}
	if c.zw != nil {
		return c.zw.Flush()
	}
	return nil
}

func (c *Client) lostErr() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	return &ConnLost{"connection closed"}
}

// ---------------------------------------------------------- reader loop --

// readLogicalLine reads one physical line plus any literals. Body literals
// for FETCH BODY[] stream into the front of bodyFIFO instead of buffering.
func (c *Client) readLogicalLine(streamSink func(size int64) bool) ([]string, [][]byte, error) {
	var segs []string
	var lits [][]byte
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return nil, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		size, plus := literalTail(line)
		if size < 0 {
			segs = append(segs, line)
			return segs, lits, nil
		}
		_ = plus
		segs = append(segs, line[:strings.LastIndexByte(line, '{')])
		// stream or buffer the literal
		if streamSink != nil && strings.Contains(strings.ToUpper(line), "BODY[]") &&
			streamSink(size) {
			if err := c.streamLiteral(size); err != nil {
				return nil, nil, err
			}
			lits = append(lits, nil) // placeholder token
		} else {
			buf := make([]byte, size)
			if _, err := ioReadFull(c.br, buf); err != nil {
				return nil, nil, err
			}
			lits = append(lits, buf)
		}
	}
}

// streamLiteral pushes size bytes to the front body handle in 64KB chunks.
func (c *Client) streamLiteral(size int64) error {
	c.mu.Lock()
	var bh *BodyHandle
	if len(c.bodyFIFO) > 0 {
		bh = c.bodyFIFO[0]
		c.bodyFIFO = c.bodyFIFO[1:]
	}
	c.mu.Unlock()
	if bh == nil { // nobody expects it: drain
		return discard(c.br, size)
	}
	bh.sizeCh <- size
	left := size
	for left > 0 {
		n := int64(64 << 10)
		if n > left {
			n = left
		}
		buf := make([]byte, n)
		if _, err := ioReadFull(c.br, buf); err != nil {
			bh.fail(&ConnLost{err.Error()})
			return err
		}
		bh.ch <- buf
		left -= n
	}
	close(bh.ch)
	bh.failed.Store(true) // stream complete; fail() becomes a no-op
	return nil
}

func (c *Client) readerLoop() {
	for {
		segs, lits, err := c.readLogicalLine(func(int64) bool { return true })
		if err != nil {
			c.Abort(&ConnLost{"connection dropped/reset (EOF from server)"})
			return
		}
		if len(segs) > 0 {
			c.trace("S:", segs[0])
		}
		r, perr := ParseLine(segs, lits)
		if perr != nil {
			c.Log("protocol: " + perr.Error())
			continue
		}
		c.route(r)
	}
}

func (c *Client) route(r *Response) {
	if r.Tag == "+" {
		select {
		case c.contCh <- r:
		default:
		}
		return
	}
	if r.Tag != "*" { // tagged completion
		c.mu.Lock()
		p := c.pending[r.Tag]
		delete(c.pending, r.Tag)
		for i, t := range c.order {
			if t == r.Tag {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		if p == nil {
			return
		}
		p.res = Result{Final: r, Data: p.data}
		if r.Status == "OK" {
			if p.name == "COMPRESS" {
				// Reader-side deflate wrap. Safe: route() runs on the reader
				// goroutine, so the swap happens before the next read; any
				// already-buffered compressed bytes stay inside the old
				// bufio.Reader, which becomes the deflate source.
				c.br = bufio.NewReaderSize(flate.NewReader(c.br), 64<<10)
				c.deflated.Store(true)
			}
			p.ch <- nil
		} else {
			var err error = &CommandErr{p.name, r.Status, r.Text, r.Code}
			if p.name == "LOGIN" || p.name == "AUTHENTICATE" {
				err = &AuthErr{r.Text}
			}
			p.ch <- err
		}
		return
	}
	// untagged
	switch r.Type {
	case "CAPABILITY":
		c.setCaps(r.Tokens)
	case "EXISTS":
		c.selExists = r.Num
	case "RECENT", "EXPUNGE", "FLAGS":
	case "BYE":
	default:
	}
	if r.Status != "" && len(r.Code) > 0 {
		if k, _ := TokStr(r.Code[0]); true {
			switch strings.ToUpper(k) {
			case "UIDVALIDITY":
				if n, ok := TokUint(r.Code[1]); ok {
					c.selUV = n
				}
			case "UIDNEXT":
				if n, ok := TokUint(r.Code[1]); ok {
					c.selUIDNext = n
				}
			case "CAPABILITY":
				c.setCaps(r.Code[1:])
			}
		}
	}
	if r.Type != "" {
		c.mu.Lock()
		for _, tag := range c.order {
			p := c.pending[tag]
			if p != nil && p.types[r.Type] {
				p.data = append(p.data, r)
				break
			}
		}
		c.mu.Unlock()
	}
}

// ------------------------------------------------------------ commands --

// CmdNowait issues a pipelined command; collect untagged types into it.
func (c *Client) CmdNowait(name, args string, types ...string) (*Pending, error) {
	tag := ""
	c.mu.Lock()
	c.tagN++
	tag = fmt.Sprintf("MF%04d", c.tagN)
	p := &Pending{tag: tag, name: name, types: map[string]bool{}, ch: make(chan error, 1)}
	for _, t := range types {
		p.types[strings.ToUpper(t)] = true
	}
	c.pending[tag] = p
	c.order = append(c.order, tag)
	c.mu.Unlock()
	line := tag + " " + name
	if args != "" {
		line += " " + args
	}
	if err := c.writeLine(line); err != nil {
		c.dropPending(tag)
		return nil, err
	}
	return p, nil
}

func (c *Client) dropPending(tag string) {
	c.mu.Lock()
	delete(c.pending, tag)
	for i, t := range c.order {
		if t == tag {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

func (c *Client) Cmd(name, args string, types ...string) (Result, error) {
	p, err := c.CmdNowait(name, args, types...)
	if err != nil {
		return Result{}, err
	}
	return p.Wait()
}

func quoteIMAP(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func (c *Client) Login() error {
	c.Side.State("auth")
	_, err := c.Cmd("LOGIN", quoteIMAP(c.EP.User)+" "+quoteIMAP(c.EP.Password))
	if err != nil {
		return err
	}
	if len(c.Caps) == 0 {
		c.Cmd("CAPABILITY", "")
	}
	c.Cmd("ID", fmt.Sprintf(`("name" %s "version" %s)`,
		quoteIMAP(identity.Product), quoteIMAP(identity.Version)))
	c.Side.State("ready")
	return nil
}

// Compressed reports whether COMPRESS=DEFLATE is active on this connection.
func (c *Client) Compressed() bool { return c.deflated.Load() }

// StartCompress negotiates COMPRESS=DEFLATE (RFC 4978) when the server
// offers it and baseline mode is off. Wire byte counters stay underneath
// the deflate layer, so RX/TX keep reporting true network bytes.
func (c *Client) StartCompress() error {
	if c.Baseline || c.deflated.Load() || !c.Has("COMPRESS=DEFLATE") {
		return nil
	}
	p, err := c.CmdNowait("COMPRESS", "DEFLATE")
	if err != nil {
		return err
	}
	if _, err := p.Wait(); err != nil {
		return err // server refused: continue uncompressed
	}
	// Reader side was wrapped by route(); now wrap the writer.
	c.wmu.Lock()
	zw, _ := flate.NewWriter(&countWriter{c: c}, 6)
	c.zw = zw
	c.bw = bufio.NewWriterSize(zw, 64<<10)
	c.wmu.Unlock()
	c.Log("COMPRESS=DEFLATE enabled")
	return nil
}

type SelectInfo struct {
	Exists      uint32
	UIDValidity uint32
	UIDNext     uint32
}

func (c *Client) Select(mailboxWire string, readonly bool) (SelectInfo, error) {
	verb := "SELECT"
	if readonly {
		verb = "EXAMINE"
	}
	c.selUV, c.selUIDNext, c.selExists = 0, 0, 0
	_, err := c.Cmd(verb, quoteIMAP(mailboxWire))
	if err != nil {
		return SelectInfo{}, err
	}
	return SelectInfo{Exists: c.selExists, UIDValidity: c.selUV, UIDNext: c.selUIDNext}, nil
}

func (c *Client) Status(mailboxWire string) (map[string]int64, error) {
	res, err := c.Cmd("STATUS", quoteIMAP(mailboxWire)+" (MESSAGES UIDNEXT UIDVALIDITY)",
		"STATUS")
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, d := range res.Data {
		for _, t := range d.Tokens {
			if list, ok := t.([]any); ok {
				for i := 0; i+1 < len(list); i += 2 {
					if k, ok := TokStr(list[i]); ok {
						if v, ok := TokInt64(list[i+1]); ok {
							out[strings.ToUpper(k)] = v
						}
					}
				}
			}
		}
	}
	return out, nil
}

func (c *Client) Create(mailboxWire string) error {
	_, err := c.Cmd("CREATE", quoteIMAP(mailboxWire))
	if err != nil {
		var ce *CommandErr
		if errors.As(err, &ce) && strings.Contains(strings.ToUpper(ce.Text), "EXISTS") {
			return nil
		}
	}
	return err
}

func (c *Client) Subscribe(mailboxWire string) { c.Cmd("SUBSCRIBE", quoteIMAP(mailboxWire)) }

type ListEntry struct {
	Attrs []string
	Delim string
	Wire  string
}

func (c *Client) ListAll() ([]ListEntry, error) {
	res, err := c.Cmd("LIST", `"" "*"`, "LIST")
	if err != nil {
		return nil, err
	}
	var out []ListEntry
	for _, d := range res.Data {
		if len(d.Tokens) < 3 {
			continue
		}
		var e ListEntry
		if attrs, ok := d.Tokens[0].([]any); ok {
			for _, a := range attrs {
				if s, ok := TokStr(a); ok {
					e.Attrs = append(e.Attrs, s)
				}
			}
		}
		if s, ok := TokStr(d.Tokens[1]); ok && !strings.EqualFold(s, "NIL") {
			e.Delim = s
		}
		if s, ok := TokStr(d.Tokens[2]); ok {
			e.Wire = s
		}
		out = append(out, e)
	}
	return out, nil
}

func (c *Client) NamespaceInfo() (prefix, delim string) {
	if !c.Has("NAMESPACE") {
		return "", ""
	}
	res, err := c.Cmd("NAMESPACE", "", "NAMESPACE")
	if err != nil {
		return "", ""
	}
	for _, d := range res.Data {
		if len(d.Tokens) == 0 {
			continue
		}
		if personal, ok := d.Tokens[0].([]any); ok && len(personal) > 0 {
			if first, ok := personal[0].([]any); ok && len(first) >= 2 {
				p, _ := TokStr(first[0])
				dl, _ := TokStr(first[1])
				return p, dl
			}
		}
	}
	return "", ""
}

// UIDSearchAll returns every UID in the selected folder.
func (c *Client) UIDSearchAll() ([]uint32, error) {
	res, err := c.Cmd("UID SEARCH", "ALL", "SEARCH")
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, d := range res.Data {
		for _, t := range d.Tokens {
			if n, ok := TokUint(t); ok {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

type Meta struct {
	UID          uint32
	Size         int64
	Flags        []string
	InternalDate string
	Header       []byte
}

// UIDFetchMeta fetches per-message metadata (+ fingerprint headers).
func (c *Client) UIDFetchMeta(set string, withHeader bool) ([]Meta, error) {
	items := "(UID FLAGS INTERNALDATE RFC822.SIZE"
	if withHeader {
		items += " BODY.PEEK[HEADER.FIELDS (MESSAGE-ID DATE FROM TO SUBJECT)]"
	}
	items += ")"
	res, err := c.Cmd("UID FETCH", set+" "+items, "FETCH")
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, d := range res.Data {
		at := FetchAttrs(d.Tokens)
		var m Meta
		if u, ok := TokUint(at["UID"]); ok {
			m.UID = u
		} else {
			continue
		}
		if n, ok := TokInt64(at["RFC822.SIZE"]); ok {
			m.Size = n
		}
		if fl, ok := at["FLAGS"].([]any); ok {
			for _, f := range fl {
				if s, ok := TokStr(f); ok {
					m.Flags = append(m.Flags, s)
				}
			}
		}
		if s, ok := TokStr(at["INTERNALDATE"]); ok {
			m.InternalDate = s
		}
		if b, ok := at["BODY[]"].([]byte); ok {
			m.Header = b
		}
		out = append(out, m)
	}
	return out, nil
}

// BodyFetch pipelines a full-body fetch; the body streams via the handle.
func (c *Client) BodyFetch(uid uint32) (*BodyHandle, error) {
	bh := &BodyHandle{UID: uid, sizeCh: make(chan int64, 1),
		ch: make(chan []byte, 8), errCh: make(chan error, 1)}
	c.mu.Lock()
	c.bodyFIFO = append(c.bodyFIFO, bh)
	c.mu.Unlock()
	_, err := c.CmdNowait("UID FETCH", fmt.Sprintf("%d (BODY.PEEK[])", uid))
	if err != nil {
		bh.fail(err)
		return bh, err
	}
	return bh, nil
}

// ------------------------------------------------------------- append --

type AppendSink struct {
	c   *Client
	p   *Pending
	err error
}

func (a *AppendSink) Write(chunk []byte) error {
	if a.err != nil {
		return a.err
	}
	a.c.wmu.Lock()
	defer a.c.wmu.Unlock()
	if _, err := a.c.bw.Write(chunk); err != nil {
		a.err = &ConnLost{err.Error()}
		return a.err
	}
	return nil
}

// Finish terminates the literal; completion is pipelined via the Pending.
func (a *AppendSink) Finish() (*Pending, error) {
	if a.err != nil {
		return nil, a.err
	}
	a.c.wmu.Lock()
	_, err := a.c.bw.WriteString("\r\n")
	if err == nil {
		err = a.c.flushWire()
	}
	a.c.wmu.Unlock()
	if err != nil {
		return nil, &ConnLost{err.Error()}
	}
	return a.p, nil
}

// AppendBegin opens a streamed APPEND. With LITERAL+ no round trip is
// needed; otherwise it waits for the continuation.
func (c *Client) AppendBegin(mailboxWire, flags, internalDate string, size int64) (*AppendSink, error) {
	c.pendAppends.Add(1)
	args := quoteIMAP(mailboxWire)
	if flags != "" {
		args += " (" + flags + ")"
	}
	if internalDate != "" {
		args += " " + quoteIMAP(internalDate)
	}
	plus := ""
	if c.Has("LITERAL+") && !c.Baseline {
		plus = "+"
	}
	c.mu.Lock()
	c.tagN++
	tag := fmt.Sprintf("MF%04d", c.tagN)
	p := &Pending{tag: tag, name: "APPEND", types: map[string]bool{}, ch: make(chan error, 1)}
	c.pending[tag] = p
	c.order = append(c.order, tag)
	c.mu.Unlock()
	line := fmt.Sprintf("%s APPEND %s {%d%s}", tag, args, size, plus)
	if err := c.writeLine(line); err != nil {
		c.dropPending(tag)
		c.pendAppends.Add(-1)
		return nil, err
	}
	if plus == "" {
		select {
		case <-c.contCh:
		case err := <-p.ch:
			c.pendAppends.Add(-1)
			if err == nil {
				err = &ConnLost{"APPEND completed before literal"}
			}
			return nil, err
		case <-time.After(c.Timeout):
			c.pendAppends.Add(-1)
			return nil, &ConnLost{"timed out waiting for APPEND continuation"}
		}
	}
	return &AppendSink{c: c, p: p}, nil
}

// AppendDone must be called after the pending resolves (bookkeeping).
func (c *Client) AppendDone() { c.pendAppends.Add(-1) }

// AppendUIDOf extracts the new UID from [APPENDUID uv uid].
func AppendUIDOf(res Result) int64 {
	if res.Final == nil || len(res.Final.Code) < 3 {
		return 0
	}
	if k, _ := TokStr(res.Final.Code[0]); strings.EqualFold(k, "APPENDUID") {
		if n, ok := TokInt64(res.Final.Code[2]); ok {
			return n
		}
	}
	return 0
}

func (c *Client) UIDStoreFlags(uid int64, flags string) error {
	_, err := c.Cmd("UID STORE", fmt.Sprintf("%d FLAGS (%s)", uid, flags))
	return err
}

func (c *Client) Noop() error { _, err := c.Cmd("NOOP", ""); return err }

func (c *Client) Logout(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		c.Cmd("LOGOUT", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	c.Abort(nil)
}

// ------------------------------------------------------------ watchdog --

func (c *Client) watchdog() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.watchStop:
			return
		case <-t.C:
			if c.closed.Load() {
				return
			}
			idle := time.Since(time.Unix(0, c.lastAct.Load()))
			limit := c.Timeout
			if c.pendAppends.Load() > 0 {
				limit = 3 * c.Timeout // large APPEND acks can be slow
			}
			if idle > limit {
				c.Log(fmt.Sprintf("watchdog: no socket activity for %ds — closing connection",
					int(idle.Seconds())))
				c.Abort(&ConnLost{fmt.Sprintf("no socket activity for %ds", int(idle.Seconds()))})
				return
			}
		}
	}
}

// Abort force-closes the connection; every waiter unwinds with the error.
func (c *Client) Abort(err error) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	if err == nil {
		err = &ConnLost{"connection closed"}
	}
	c.closeErr = err
	select {
	case <-c.watchStop:
	default:
		close(c.watchStop)
	}
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Lock()
	pend := c.pending
	c.pending = map[string]*Pending{}
	c.order = nil
	fifo := c.bodyFIFO
	c.bodyFIFO = nil
	c.mu.Unlock()
	for _, p := range pend {
		select {
		case p.ch <- err:
		default:
		}
	}
	for _, bh := range fifo {
		bh.fail(err)
	}
	c.Side.State("closed")
}

// --------------------------------------------------------- byte counting --

type countReader struct{ c *Client }

func (r *countReader) Read(p []byte) (int, error) {
	n, err := r.c.conn.Read(p)
	if n > 0 {
		r.c.Side.RX(n)
		r.c.touch()
	}
	return n, err
}

type countWriter struct{ c *Client }

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.c.conn.Write(p)
	if n > 0 {
		w.c.Side.TX(n)
		w.c.touch()
	}
	return n, err
}

// ---------------------------------------------------------------- misc --

func literalTail(line string) (int64, bool) {
	if !strings.HasSuffix(line, "}") {
		return -1, false
	}
	i := strings.LastIndexByte(line, '{')
	if i < 0 {
		return -1, false
	}
	body := line[i+1 : len(line)-1]
	plus := strings.HasSuffix(body, "+")
	if plus {
		body = body[:len(body)-1]
	}
	var n int64
	for _, ch := range body {
		if ch < '0' || ch > '9' {
			return -1, false
		}
		n = n*10 + int64(ch-'0')
	}
	if body == "" {
		return -1, false
	}
	return n, plus
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func discard(r *bufio.Reader, n int64) error {
	buf := make([]byte, 32<<10)
	for n > 0 {
		want := int64(len(buf))
		if want > n {
			want = n
		}
		got, err := r.Read(buf[:want])
		n -= int64(got)
		if err != nil {
			return err
		}
	}
	return nil
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// StaleKick: injected by the stale supervisor to force a reconnect. It is
// transport-shaped (reconnect + resume from checkpoint) but never consumes
// retry budgets and never blames a message.
type StaleKick struct{ Msg string }

func (e *StaleKick) Error() string { return e.Msg }

func IsStaleKick(err error) bool {
	var s *StaleKick
	return errors.As(err, &s)
}
