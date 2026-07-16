package notify

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeSMTPServer accepts a single connection and speaks the minimum SMTP needed
// to drive sendSMTPPlain -> finishSMTP to completion (greeting, EHLO, MAIL,
// RCPT, DATA, QUIT). It never touches *testing.T from its goroutine, so it is
// race-safe.
func fakeSMTPServer(t *testing.T) (host string, port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		_, _ = fmt.Fprint(conn, "220 mock ESMTP\r\n")
		inData := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					_, _ = fmt.Fprint(conn, "250 ok\r\n")
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				_, _ = fmt.Fprint(conn, "250 mock\r\n")
			case line == "DATA":
				_, _ = fmt.Fprint(conn, "354 end data with <CR><LF>.<CR><LF>\r\n")
				inData = true
			case line == "QUIT":
				_, _ = fmt.Fprint(conn, "221 bye\r\n")
				return
			default: // MAIL FROM / RCPT TO / RSET / ...
				_, _ = fmt.Fprint(conn, "250 ok\r\n")
			}
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pn, _ := strconv.Atoi(p)
	return h, pn, func() { _ = ln.Close() }
}

// TestSendSMTP_PlainSuccess drives the real plaintext transport end to end
// (sendSMTP -> sendSMTPPlain -> finishSMTP) against a fake relay, with no auth
// (an unauthenticated internal relay).
func TestSendSMTP_PlainSuccess(t *testing.T) {
	host, port, closeFn := fakeSMTPServer(t)
	defer closeFn()

	cfg := SMTPConfig{Host: host, Port: port, From: "tsm@example.com"} // UseTLS=false, no username
	msg := buildEmailMessage(cfg.From, []string{"ops@example.com"}, "Drift", "3 resources drifted")
	if err := sendSMTP(context.Background(), cfg, []string{"ops@example.com"}, msg); err != nil {
		t.Fatalf("sendSMTP over plaintext relay: %v", err)
	}
}

// TestSendSMTP_TLSDialFallbackFails exercises the UseTLS branch: the implicit
// TLS dial fails and falls back to the STARTTLS pattern, which also fails to
// connect (port bound then released), so both dial paths return an error.
func TestSendSMTP_TLSDialFallbackFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	_ = ln.Close() // free the port so subsequent dials are refused immediately

	cfg := SMTPConfig{Host: h, Port: port, From: "tsm@example.com", UseTLS: true}
	if err := sendSMTP(context.Background(), cfg, []string{"ops@example.com"}, []byte("x")); err == nil {
		t.Fatal("expected an error dialing an unreachable TLS relay")
	}
}
