// MailFerry — IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra
// Author: Andy Saputra <andy@saputra.org>
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This file is part of MailFerry (https://github.com/ajsap/mailferry).
// Licensed under the GNU Affero General Public License v3.0 or later;
// see the LICENSE file for details.

// Package fakeimap: an in-memory IMAP server for the automated test suite,
// with poison-message and stall behaviours for resilience regression tests.
package fakeimap

import (
	"bufio"
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Msg struct {
	UID          uint32
	Body         []byte
	Flags        map[string]bool
	InternalDate string
}

func (m *Msg) HeaderFields(wanted []string) []byte {
	end := bytes.Index(m.Body, []byte("\r\n\r\n"))
	header := m.Body
	if end >= 0 {
		header = m.Body[:end+2]
	}
	var out [][]byte
	take := false
	for _, line := range bytes.Split(header, []byte("\r\n")) {
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if take {
				out = append(out, line)
			}
			continue
		}
		name := strings.ToLower(string(bytes.SplitN(line, []byte(":"), 2)[0]))
		take = false
		for _, w := range wanted {
			if name == w {
				take = true
				break
			}
		}
		if take {
			out = append(out, line)
		}
	}
	joined := bytes.Join(out, []byte("\r\n"))
	if len(joined) > 0 {
		return append(joined, []byte("\r\n\r\n")...)
	}
	return []byte("\r\n")
}

type Folder struct {
	Name        string
	Attrs       []string
	UIDValidity uint32
	UIDNext     uint32
	Msgs        []*Msg
}

func NewFolder(name string, uidvalidity uint32, attrs ...string) *Folder {
	return &Folder{Name: name, UIDValidity: uidvalidity, UIDNext: 1, Attrs: attrs}
}

func (f *Folder) Add(body []byte, flags []string, internalDate string) *Msg {
	m := &Msg{UID: f.UIDNext, Body: body, Flags: map[string]bool{},
		InternalDate: internalDate}
	for _, fl := range flags {
		m.Flags[fl] = true
	}
	f.UIDNext++
	f.Msgs = append(f.Msgs, m)
	return m
}

func (f *Folder) bySet(set string) []*Msg {
	var out []*Msg
	for _, part := range strings.Split(set, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, ":"); i >= 0 {
			lo, _ := strconv.ParseUint(part[:i], 10, 32)
			hiS := part[i+1:]
			var hi uint64
			if hiS == "*" {
				hi = uint64(f.UIDNext)
			} else {
				hi, _ = strconv.ParseUint(hiS, 10, 32)
			}
			for _, m := range f.Msgs {
				if uint64(m.UID) >= lo && uint64(m.UID) <= hi {
					out = append(out, m)
				}
			}
		} else {
			u, _ := strconv.ParseUint(part, 10, 32)
			for _, m := range f.Msgs {
				if uint64(m.UID) == u {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

type Account struct {
	User     string
	Password string
	mu       sync.Mutex
	Folders  map[string]*Folder
	order    []string
}

func NewAccount(user, password string) *Account {
	a := &Account{User: user, Password: password, Folders: map[string]*Folder{}}
	a.AddFolder(NewFolder("INBOX", 1111))
	return a
}

func (a *Account) AddFolder(f *Folder) *Folder {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Folders[f.Name] = f
	a.order = append(a.order, f.Name)
	return f
}

func (a *Account) Folder(name string) *Folder {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.EqualFold(name, "INBOX") {
		for k, f := range a.Folders {
			if strings.EqualFold(k, "INBOX") {
				return f
			}
		}
	}
	return a.Folders[name]
}

func (a *Account) TotalMsgs() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, f := range a.Folders {
		n += len(f.Msgs)
	}
	return n
}

// Server is one fake IMAP endpoint.
type Server struct {
	Accounts map[string]*Account
	Caps     []string
	Addr     string

	AppendCount   atomic.Int64
	FetchBodyN    atomic.Int64
	KillCount     atomic.Int64
	AppendReject  []byte       // bodies containing this get "NO APPEND failed"
	AppendKill    []byte       // bodies containing this kill the connection
	AppendDelayMS atomic.Int64 // artificial per-append latency (slow-server tests)
	DropAfterOKN  atomic.Int64 // kill AFTER sending OK for the Nth append (ack-loss test)
	StallAfterN   atomic.Int64 // after N body fetches, stop responding (hung server)
	StallOnce     atomic.Bool  // clear StallAfterN after the first stall (recover next conn)
	StallCount    atomic.Int64
	CompressConns atomic.Int64 // connections that negotiated COMPRESS=DEFLATE
	storeMu       sync.Mutex
	StoreLog      []string // "folder set items" per UID STORE (flag-sync tests)
	mu            sync.Mutex
	ln            net.Listener
}

// StoreEvents returns a copy of the UID STORE audit log.
func (s *Server) StoreEvents() []string {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	return append([]string(nil), s.StoreLog...)
}

func NewServer(accounts ...*Account) *Server {
	s := &Server{Accounts: map[string]*Account{},
		Caps: []string{"IMAP4rev1", "UIDPLUS", "LITERAL+", "NAMESPACE",
			"SPECIAL-USE", "ID", "UNSELECT", "COMPRESS=DEFLATE"}}
	for _, a := range accounts {
		s.Accounts[a.User] = a
	}
	return s
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.ln = ln
	s.Addr = ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.client(conn)
		}
	}()
	return nil
}

func (s *Server) Port() int {
	_, p, _ := net.SplitHostPort(s.Addr)
	n, _ := strconv.Atoi(p)
	return n
}

func (s *Server) Stop() {
	if s.ln != nil {
		s.ln.Close()
	}
}

var litRe = regexp.MustCompile(`\{(\d+)(\+)?\}$`)
var quotedOrAtom = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|(\S+)`)

func toks(s string) []string {
	var out []string
	for _, m := range quotedOrAtom.FindAllStringSubmatch(s, -1) {
		if m[1] != "" || strings.HasPrefix(m[0], `"`) {
			v := strings.ReplaceAll(m[1], `\"`, `"`)
			out = append(out, strings.ReplaceAll(v, `\\`, `\`))
		} else {
			out = append(out, m[2])
		}
	}
	return out
}

func (s *Server) client(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	var acct *Account
	var selected *Folder
	var out io.Writer = conn
	var zw *flate.Writer
	send := func(line string) bool {
		_, err := out.Write([]byte(line + "\r\n"))
		if err == nil && zw != nil {
			err = zw.Flush()
		}
		return err == nil
	}
	sendRaw := func(b []byte) bool {
		_, err := out.Write(b)
		if err == nil && zw != nil {
			err = zw.Flush()
		}
		return err == nil
	}
	send("* OK [CAPABILITY " + strings.Join(s.Caps, " ") + "] fake server ready")
	for {
		raw, err := br.ReadString('\n')
		if err != nil {
			return
		}
		raw = strings.TrimRight(raw, "\r\n")
		var lits [][]byte
		for {
			m := litRe.FindStringSubmatchIndex(raw)
			if m == nil {
				break
			}
			n, _ := strconv.Atoi(raw[m[2]:m[3]])
			if m[4] < 0 { // synchronising literal
				send("+ OK")
			}
			buf := make([]byte, n)
			if _, err := readFull(br, buf); err != nil {
				return
			}
			lits = append(lits, buf)
			rest, err := br.ReadString('\n')
			if err != nil {
				return
			}
			raw = raw[:m[0]] + "\x00LIT\x00" + strings.TrimRight(rest, "\r\n")
		}
		parts := strings.SplitN(raw, " ", 3)
		if len(parts) < 2 {
			continue
		}
		tag, verb := parts[0], strings.ToUpper(parts[1])
		rest := ""
		if len(parts) > 2 {
			rest = parts[2]
		}
		if verb == "UID" && rest != "" {
			sub, r2, _ := strings.Cut(rest, " ")
			verb = "UID " + strings.ToUpper(sub)
			rest = r2
		}
		switch verb {
		case "CAPABILITY":
			send("* CAPABILITY " + strings.Join(s.Caps, " "))
			send(tag + " OK done")
		case "NOOP":
			send(tag + " OK done")
		case "ID":
			send("* ID NIL")
			send(tag + " OK done")
		case "NAMESPACE":
			send(`* NAMESPACE (("" "/")) NIL NIL`)
			send(tag + " OK done")
		case "COMPRESS":
			if zw != nil {
				send(tag + " NO [COMPRESSIONACTIVE] already compressed")
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(rest), "DEFLATE") {
				send(tag + " BAD unsupported compression")
				continue
			}
			send(tag + " OK DEFLATE active")
			zw, _ = flate.NewWriter(conn, 6)
			out = zw
			br = bufio.NewReader(flate.NewReader(br))
			s.CompressConns.Add(1)
		case "LOGIN":
			t := toks(rest)
			if len(t) >= 2 {
				if a := s.Accounts[t[0]]; a != nil && a.Password == t[1] {
					acct = a
					send(tag + " OK [CAPABILITY " + strings.Join(s.Caps, " ") + "] logged in")
					continue
				}
			}
			send(tag + " NO [AUTHENTICATIONFAILED] bad credentials")
		case "LOGOUT":
			send("* BYE bye")
			send(tag + " OK done")
			return
		default:
			if acct == nil {
				send(tag + " NO not authenticated")
				continue
			}
			switch verb {
			case "LIST":
				acct.mu.Lock()
				names := append([]string(nil), acct.order...)
				acct.mu.Unlock()
				for _, name := range names {
					f := acct.Folders[name]
					send(fmt.Sprintf(`* LIST (%s) "/" "%s"`, strings.Join(f.Attrs, " "), name))
				}
				send(tag + " OK done")
			case "SUBSCRIBE":
				send(tag + " OK done")
			case "CREATE":
				t := toks(rest)
				if len(t) > 0 {
					if acct.Folder(t[0]) != nil {
						send(tag + " NO [ALREADYEXISTS] exists")
					} else {
						acct.AddFolder(NewFolder(t[0], 2222+uint32(len(acct.Folders))))
						send(tag + " OK created")
					}
				}
			case "STATUS":
				t := toks(rest)
				f := acct.Folder(t[0])
				if f == nil {
					send(tag + " NO no such folder")
					continue
				}
				var size int64
				for _, m := range f.Msgs {
					size += int64(len(m.Body))
				}
				send(fmt.Sprintf(`* STATUS "%s" (MESSAGES %d UIDNEXT %d UIDVALIDITY %d)`,
					t[0], len(f.Msgs), f.UIDNext, f.UIDValidity))
				send(tag + " OK done")
			case "SELECT", "EXAMINE":
				t := toks(rest)
				f := acct.Folder(t[0])
				if f == nil {
					send(tag + " NO no such folder")
					continue
				}
				selected = f
				send(fmt.Sprintf("* %d EXISTS", len(f.Msgs)))
				send("* 0 RECENT")
				send(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`)
				send(fmt.Sprintf("* OK [UIDVALIDITY %d] ok", f.UIDValidity))
				send(fmt.Sprintf("* OK [UIDNEXT %d] ok", f.UIDNext))
				ro := "READ-WRITE"
				if verb == "EXAMINE" {
					ro = "READ-ONLY"
				}
				send(tag + " OK [" + ro + "] done")
			case "UNSELECT":
				selected = nil
				send(tag + " OK done")
			case "UID SEARCH":
				if selected == nil {
					send(tag + " NO nothing selected")
					continue
				}
				var b strings.Builder
				b.WriteString("* SEARCH")
				for _, m := range selected.Msgs {
					fmt.Fprintf(&b, " %d", m.UID)
				}
				send(b.String())
				send(tag + " OK done")
			case "UID FETCH":
				if selected == nil {
					send(tag + " NO nothing selected")
					continue
				}
				set, items, _ := strings.Cut(rest, " ")
				itemsUp := strings.ToUpper(items)
				wantHdr := strings.Contains(itemsUp, "HEADER.FIELDS")
				wantBody := strings.Contains(itemsUp, "BODY.PEEK[]") ||
					strings.Contains(itemsUp, "BODY[]")
				var fields []string
				if wantHdr {
					if mm := regexp.MustCompile(`(?i)HEADER\.FIELDS \(([^)]*)\)`).
						FindStringSubmatch(items); mm != nil {
						for _, f := range strings.Fields(mm[1]) {
							fields = append(fields, strings.ToLower(f))
						}
					}
				}
				for seq, m := range selected.bySet(set) {
					var attrs []string
					attrs = append(attrs, fmt.Sprintf("UID %d", m.UID))
					if strings.Contains(itemsUp, "FLAGS") {
						var fl []string
						for f := range m.Flags {
							fl = append(fl, f)
						}
						attrs = append(attrs, "FLAGS ("+strings.Join(fl, " ")+")")
					}
					if strings.Contains(itemsUp, "INTERNALDATE") {
						attrs = append(attrs, `INTERNALDATE "`+m.InternalDate+`"`)
					}
					if strings.Contains(itemsUp, "RFC822.SIZE") {
						attrs = append(attrs, fmt.Sprintf("RFC822.SIZE %d", len(m.Body)))
					}
					if wantHdr {
						data := m.HeaderFields(fields)
						sendRaw([]byte(fmt.Sprintf("* %d FETCH (%s BODY[HEADER.FIELDS (MESSAGE-ID DATE FROM TO SUBJECT)] {%d}\r\n",
							seq+1, strings.Join(attrs, " "), len(data))))
						sendRaw(data)
						sendRaw([]byte(")\r\n"))
					} else if wantBody {
						n := s.FetchBodyN.Add(1)
						if sa := s.StallAfterN.Load(); sa > 0 && n > sa {
							s.StallCount.Add(1)
							if s.StallOnce.Load() {
								s.StallAfterN.Store(0) // next connection recovers
							}
							time.Sleep(3600 * time.Second) // hung server: never reply
						}
						sendRaw([]byte(fmt.Sprintf("* %d FETCH (%s BODY[] {%d}\r\n",
							seq+1, strings.Join(attrs, " "), len(m.Body))))
						sendRaw(m.Body)
						sendRaw([]byte(")\r\n"))
					} else {
						send(fmt.Sprintf("* %d FETCH (%s)", seq+1, strings.Join(attrs, " ")))
					}
				}
				send(tag + " OK fetch done")
			case "UID STORE":
				if selected == nil {
					send(tag + " NO nothing selected")
					continue
				}
				set, items, _ := strings.Cut(rest, " ")
				s.storeMu.Lock()
				s.StoreLog = append(s.StoreLog, selected.Name+" "+set+" "+items)
				s.storeMu.Unlock()
				if mm := regexp.MustCompile(`\(([^)]*)\)`).FindStringSubmatch(items); mm != nil {
					for _, m := range selected.bySet(set) {
						m.Flags = map[string]bool{}
						for _, f := range strings.Fields(mm[1]) {
							m.Flags[f] = true
						}
					}
				}
				send(tag + " OK done")
			case "APPEND":
				head := strings.Split(rest, "\x00LIT\x00")[0]
				t := toks(head)
				if len(t) == 0 {
					send(tag + " BAD append")
					continue
				}
				f := acct.Folder(t[0])
				if f == nil {
					send(tag + " NO [TRYCREATE] no such folder")
					continue
				}
				var flags []string
				if mm := regexp.MustCompile(`\(([^)]*)\)`).FindStringSubmatch(head); mm != nil {
					for _, fl := range strings.Fields(mm[1]) {
						if !strings.EqualFold(fl, `\Recent`) {
							flags = append(flags, fl)
						}
					}
				}
				date := "17-Jul-2026 10:00:00 +0000"
				if dm := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).
					FindAllStringSubmatch(head, -1); len(dm) > 1 {
					date = dm[len(dm)-1][1]
				}
				var body []byte
				if len(lits) > 0 {
					body = lits[0]
				}
				if d := s.AppendDelayMS.Load(); d > 0 {
					time.Sleep(time.Duration(d) * time.Millisecond)
				}
				if len(s.AppendKill) > 0 && bytes.Contains(body, s.AppendKill) {
					s.KillCount.Add(1)
					return // content filter RSTs the line
				}
				if len(s.AppendReject) > 0 && bytes.Contains(body, s.AppendReject) {
					send(tag + " NO APPEND failed")
					continue
				}
				acct.mu.Lock()
				msg := f.Add(body, flags, date)
				acct.mu.Unlock()
				n := s.AppendCount.Add(1)
				send(fmt.Sprintf("%s OK [APPENDUID %d %d] append done", tag, f.UIDValidity, msg.UID))
				if v := s.DropAfterOKN.Load(); v > 0 && n == v {
					return // ack sent, then the line dies (dup-prevention test)
				}
			default:
				send(tag + " BAD unknown command " + verb)
			}
		}
	}
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
