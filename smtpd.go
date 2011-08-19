package main

// TODO:
//  -- send 421 to connected clients on graceful server shutdown (s3.8)
//

import (
	"bufio"
	"exec"
	"fmt"
	"log"
	"net"
	"os"
	"exp/regexp"
	"strings"
	"unicode"
)

var (
	rcptToRE   = regexp.MustCompile(`(?i)^to:\s*<(.+?)>`)
	mailFromRE = regexp.MustCompile(`(?i)^from:\s*<(.*?)>`)
)

// Server is an SMTP server.
type Server struct {
	Addr         string // TCP address to listen on, ":25" if empty
	Hostname     string // optional Hostname to announce; "" to use system hostname
	ReadTimeout  int64  // optional net.Conn.SetReadTimeout value for new connections
	WriteTimeout int64  // optional net.Conn.SetWriteTimeout value for new connections

	// OnNewConnection, if non-nil, is called on new connections.
	// If it returns non-nil, the connection is closed.
	OnNewConnection func(c Connection) os.Error

	// OnNewMail must be defined and is called when a new message beings.
	// (when a MAIL FROM line arrives)
	OnNewMail func(c Connection, from MailAddress) (Envelope, os.Error)
}

// MailAddress is defined by 
type MailAddress interface {
	Email() string    // email address, as provided
	Hostname() string // canonical hostname, lowercase
}

// Connection is implemented by the SMTP library and provided to callers
// customizing their own Servers.
type Connection interface {
	Addr() net.Addr
}

type Envelope interface {
	AddRecipient(rcpt MailAddress) os.Error
}

type BasicEnvelope struct {
	rcpts []MailAddress
}

func (e *BasicEnvelope) AddRecipient(rcpt MailAddress) os.Error {
	e.rcpts = append(e.rcpts, rcpt)
	return nil
}

// ArrivingMessage is the interface that must be implement by servers
// receiving mail.
type ArrivingMessage interface {
	AddHeaderLine(s string) os.Error
	EndHeaders() os.Error
}

func (srv *Server) hostname() string {
	if srv.Hostname != "" {
		return srv.Hostname
	}
	out, err := exec.Command("hostname").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.  If
// srv.Addr is blank, ":25" is used.
func (srv *Server) ListenAndServe() os.Error {
	addr := srv.Addr
	if addr == "" {
		addr = ":25"
	}
	ln, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	return srv.Serve(ln)
}

func (srv *Server) Serve(ln net.Listener) os.Error {
	defer ln.Close()
	for {
		rw, e := ln.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				log.Printf("smtpd: Accept error: %v", e)
				continue
			}
			return e
		}
		if srv.ReadTimeout != 0 {
			rw.SetReadTimeout(srv.ReadTimeout)
		}
		if srv.WriteTimeout != 0 {
			rw.SetWriteTimeout(srv.WriteTimeout)
		}
		sess, err := srv.newSession(rw)
		if err != nil {
			continue
		}
		go sess.serve()
	}
	panic("not reached")
}

type session struct {
	srv *Server
	rwc net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	env Envelope // current envelope, or nil

	helloType string
	helloHost string
}

func (srv *Server) newSession(rwc net.Conn) (s *session, err os.Error) {
	s = &session{
		srv: srv,
		rwc: rwc,
		br:  bufio.NewReader(rwc),
		bw:  bufio.NewWriter(rwc),
	}
	return
}

func (s *session) errorf(format string, args ...interface{}) {
	log.Printf("Client error: "+format, args...)
}

func (s *session) sendf(format string, args ...interface{}) {
	fmt.Fprintf(s.bw, format, args...)
	s.bw.Flush()
}

func (s *session) sendlinef(format string, args ...interface{}) {
	s.sendf(format+"\r\n", args...)
}

func (s *session) Addr() net.Addr {
	return s.rwc.RemoteAddr()
}

func (s *session) serve() {
	defer s.rwc.Close()
	if onc := s.srv.OnNewConnection; onc != nil {
		if err := onc(s); err != nil {
			// TODO: if the error implements a SMTPErrorStringer,
			// call it and send the error back
			//
			// TODO: if the user error is 554 (rejecting this connection),
			// we should relay that back too, and stick around for
			// a bit (see RFC 5321 3.1)
			return
		}
	}
	s.sendf("220 %s ESMTP gosmtpd\r\n", s.srv.hostname())
	for {
		sl, err := s.br.ReadSlice('\n')
		if err != nil {
			s.errorf("read error: %v", err)
			return
		}
		line := cmdLine(string(sl))
		if err := line.checkValid(); err != nil {
			s.sendlinef("500 %v", err)
			continue
		}

		switch line.Verb() {
		case "HELO", "EHLO":
			s.handleHello(line.Verb(), line.Arg())
		case "QUIT":
			s.sendlinef("221 2.0.0 Bye")
			return
		case "RSET":
			s.env = nil
			s.sendlinef("250 2.0.0 OK")
		case "NOOP":
			s.sendlinef("250 2.0.0 OK")
		case "MAIL":
			arg := line.Arg() // "From:<foo@bar.com>"
			m := mailFromRE.FindStringSubmatch(arg)
			if m == nil {
				s.sendlinef("501 5.1.7 Bad sender address syntax")
				continue
			}
			s.handleMailFrom(m[1])
		case "RCPT":
			s.handleRcpt(line)
		case "DATA":
			s.sendlinef("354 Go ahead")
		default:
			log.Printf("Client: %q, verb: %q", line, line.Verb())
			s.sendlinef("502 5.5.2 Error: command not recognized")
		}
	}
}

func (s *session) handleHello(greeting, host string) {
	s.helloType = greeting
	s.helloHost = host
	fmt.Fprintf(s.bw, "250-%s\r\n", s.srv.hostname())
	for _, ext := range []string{
		"250-PIPELINING",
		"250-SIZE 10240000",
		"250-ENHANCEDSTATUSCODES",
		"250-8BITMIME",
		"250 DSN",
	} {
		fmt.Fprintf(s.bw, "%s\r\n", ext)
	}
	s.bw.Flush()
}

func (s *session) handleMailFrom(email string) {
	if s.env != nil {
		s.sendlinef("503 5.5.1 Error: nested MAIL command")
		return
	}
	log.Printf("mail from: %q", email)
	cb := s.srv.OnNewMail
	if cb == nil {
		log.Printf("smtp: Server.OnNewMail is nil; rejecting MAIL FROM")
		s.sendf("451 Server.OnNewMail not configured\r\n")
		return
	}
	s.env = nil
	env, err := cb(s, addrString(email))
	if err != nil {
		log.Printf("rejecting MAIL FROM %q: %v", email, err)
		// TODO: send it back to client if warranted, like above
		return
	}
	s.env = env
	s.sendlinef("250 2.1.0 Ok")
}

func (s *session) handleRcpt(line cmdLine) {
	if s.env == nil {
		s.sendlinef("503 5.5.1 Error: need MAIL command")
		return
	}
	arg := line.Arg() // "To:<foo@bar.com>"
	m := rcptToRE.FindStringSubmatch(arg)
	if m == nil {
		s.sendlinef("501 5.1.7 Bad sender address syntax")
	}
	err := s.env.AddRecipient(addrString(m[1]))
	if err != nil {
		// TODO: don't always proxy the error to the client
		s.sendlinef("550 bad recipient: %v", err)
		return
	}
	s.sendlinef("250 2.1.0 Ok")
}

type addrString string

func (a addrString) Email() string {
	return string(a)
}

func (a addrString) Hostname() string {
	e := string(a)
	if idx := strings.Index(e, "@"); idx != -1 {
		return strings.ToLower(e[idx+1:])
	}
	return ""
}

type cmdLine string

func (cl cmdLine) checkValid() os.Error {
	if !strings.HasSuffix(string(cl), "\r\n") {
		return os.NewError(`line doesn't end in \r\n`)
	}
	// Check for verbs defined not to have an argument
	// (RFC 5321 s4.1.1)
	switch cl.Verb() {
	case "RSET", "DATA", "QUIT":
		if cl.Arg() != "" {
			return os.NewError("unexpected argument")
		}
	}
	return nil
}

func (cl cmdLine) Verb() string {
	s := string(cl)
	if idx := strings.Index(s, " "); idx != -1 {
		return strings.ToUpper(s[:idx])
	}
	return strings.ToUpper(s[:len(s)-2])
}

func (cl cmdLine) Arg() string {
	s := string(cl)
	if idx := strings.Index(s, " "); idx != -1 {
		return strings.TrimRightFunc(s[idx+1:len(s)-2], unicode.IsSpace)
	}
	return ""
}

func (cl cmdLine) String() string {
	return string(cl)
}
