// Copyright 2015 Muir Manders.  All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package goftp

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Represents a single connection to an FTP server.
type persistentConn struct {
	// control socket
	controlConn net.Conn

	// data socket (tracked so we can close it on client.Close())
	dataConn net.Conn

	// control socket read/write helpers
	reader *textproto.Reader
	writer *textproto.Writer

	config Config
	t0     time.Time

	// has this connection encountered an unrecoverable error
	broken bool

	// index of this connection (used for logging context and
	// round-roubin host selection)
	idx int

	// map of ftp features available on server
	features map[string]string
}

func (pconn *persistentConn) setControlConn(conn net.Conn) {
	pconn.controlConn = conn
	pconn.reader = textproto.NewReader(bufio.NewReader(conn))
	pconn.writer = textproto.NewWriter(bufio.NewWriter(conn))
}

func (pconn *persistentConn) close() {
	pconn.debug("closing")
	if pconn.controlConn != nil {
		pconn.controlConn.Close()
	}

	if pconn.dataConn != nil {
		pconn.dataConn.Close()
	}
}

func (pconn *persistentConn) sendCommandExpected(expected int, f string, args ...interface{}) error {
	code, msg, err := pconn.sendCommand(f, args...)
	if err != nil {
		return err
	}

	var ok bool
	switch expected {
	case ReplyGroupPositiveCompletion, ReplyGroupPreliminaryReply:
		ok = code/100 == expected
	default:
		ok = code == expected
	}

	if !ok {
		return fmt.Errorf("unexpected response: %d-%s", code, msg)
	}

	return nil
}

func (pconn *persistentConn) sendCommand(f string, args ...interface{}) (int, string, error) {
	cmd := fmt.Sprintf(f, args...)

	logName := cmd
	if strings.HasPrefix(cmd, "PASS") {
		logName = "PASS ******"
	}

	err := pconn.writer.PrintfLine(cmd)

	if err != nil {
		pconn.broken = true
		pconn.debug(`error sending command "%s": %s`, logName, err)
		return 0, "", fmt.Errorf("error writing command: %s", err)
	}

	code, msg, err := pconn.readResponse(0)
	if err != nil {
		pconn.debug(`error reading response to command "%s": %s`, logName, err)
		return 0, "", fmt.Errorf("error reading command response: %s", err)
	}

	pconn.debug("sent command %s, got %d-%s", logName, code, msg)

	return code, msg, err
}

func (pconn *persistentConn) readResponse(expectedCode int) (int, string, error) {
	pconn.controlConn.SetReadDeadline(time.Now().Add(pconn.config.Timeout))
	code, msg, err := pconn.reader.ReadResponse(expectedCode)
	if err != nil {
		pconn.broken = true
	}
	return code, msg, err
}

func (pconn *persistentConn) debug(f string, args ...interface{}) {
	if pconn.config.Logger == nil {
		return
	}

	msg := fmt.Sprintf("goftp: %.3f #%d %s\n",
		time.Now().Sub(pconn.t0).Seconds(),
		pconn.idx,
		fmt.Sprintf(f, args...),
	)

	pconn.config.Logger.Write([]byte(msg))
}

func (pconn *persistentConn) fetchFeatures() error {
	code, msg, err := pconn.sendCommand("FEAT")
	if err != nil {
		return err
	}

	if !positiveCompletionReply(code) {
		pconn.debug("server doesn't support FEAT: %d-%s", code, msg)
		return fmt.Errorf("server doesn't support FEAT (%s)", msg)
	}

	for _, line := range strings.Split(msg, "\n") {
		if line[0] == ' ' {
			parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
			if len(parts) == 1 {
				pconn.features[strings.ToUpper(parts[0])] = ""
			} else if len(parts) == 2 {
				pconn.features[strings.ToUpper(parts[0])] = parts[1]
			}
		}
	}

	return nil
}

func (pconn *persistentConn) hasFeature(name string) bool {
	_, found := pconn.features[name]
	return found
}

func (pconn *persistentConn) hasFeatureWithArg(name, arg string) bool {
	val, found := pconn.features[name]
	return found && strings.ToUpper(arg) == val
}

func (pconn *persistentConn) logIn() error {
	if pconn.config.User == "" {
		return nil
	}

	code, msg, err := pconn.sendCommand("USER %s", pconn.config.User)
	if err != nil {
		pconn.broken = true
		return err
	}

	if code == ReplyNeedPassword {
		code, msg, err = pconn.sendCommand("PASS %s", pconn.config.Password)
		if err != nil {
			return err
		}
	}

	if !positiveCompletionReply(code) {
		return fmt.Errorf("unexpected response to PASS: %d-%s", code, msg)
	}

	return nil
}

// Request that the server enters passive mode, allowing us to connect to it.
// This lets transfers work with the client behind NAT, so you almost always
// want it. First try EPSV, then fall back to PASV.
func (pconn *persistentConn) requestPassive() (string, error) {
	var (
		startIdx   int
		endIdx     int
		port       int
		remoteHost string
	)

	// Extended PaSsiVe (same idea as PASV, but works with IPv6).
	// See http://tools.ietf.org/html/rfc2428.
	code, msg, err := pconn.sendCommand("EPSV")
	if err != nil {
		return "", err
	}

	if code != ReplyEnteringExtendedPassiveMode {
		pconn.debug("server doesn't support EPSV: %d-%s", code, msg)
		goto PASV
	}

	startIdx = strings.Index(msg, "|||")
	endIdx = strings.LastIndex(msg, "|")
	if startIdx == -1 || endIdx == -1 || startIdx+3 > endIdx {
		pconn.debug("failed parsing EPSV response: %s", msg)
		goto PASV
	}

	port, err = strconv.Atoi(msg[startIdx+3 : endIdx])
	if err != nil {
		pconn.debug("EPSV response didn't contain port: %s", msg)
		goto PASV
	}

	remoteHost, _, err = net.SplitHostPort(pconn.controlConn.RemoteAddr().String())
	if err != nil {
		pconn.debug("failed determing remote host (?)")
		goto PASV
	}

	return fmt.Sprintf("[%s]:%d", remoteHost, port), nil

PASV:
	err = pconn.sendCommandExpected(ReplyEnteringPassiveMode, "PASV")
	if err != nil {
		return "", fmt.Errorf("error requesting PASV: %s", err)
	}

	parseError := fmt.Errorf("error parsing PASV response (%s)", msg)

	// "Entering Passive Mode (162,138,208,11,223,57)."
	startIdx = strings.Index(msg, "(")
	endIdx = strings.LastIndex(msg, ")")
	if startIdx == -1 || endIdx == -1 || startIdx > endIdx {
		return "", parseError
	}

	addrParts := strings.Split(msg[startIdx+1:endIdx], ",")
	if len(addrParts) != 6 {
		return "", parseError
	}

	ip := net.ParseIP(strings.Join(addrParts[0:4], "."))
	if ip == nil {
		return "", parseError
	}

	port = 0
	for i, part := range addrParts[4:6] {
		portOctet, err := strconv.Atoi(part)
		if err != nil {
			return "", parseError
		}
		port |= portOctet << (byte(1-i) * 8)
	}

	return fmt.Sprintf("%s:%d", ip.String(), port), nil
}

func (pconn *persistentConn) openDataConn() (net.Conn, error) {
	host, err := pconn.requestPassive()
	if err != nil {
		return nil, fmt.Errorf("error requesting passive connection: %s", err)
	}

	var dc net.Conn

	if pconn.config.TLSConfig != nil && pconn.config.TLSMode == TLSImplicit {
		pconn.debug("opening TLS data connection to %s", host)
		dialer := &net.Dialer{
			Timeout: pconn.config.Timeout,
		}
		dc, err = tls.DialWithDialer(dialer, "tcp", host, pconn.config.TLSConfig)
	} else {
		pconn.debug("opening data connection to %s", host)
		dc, err = net.DialTimeout("tcp", host, pconn.config.Timeout)

		if err == nil {
			if pconn.config.TLSConfig != nil && pconn.config.TLSMode == TLSExplicit {
				pconn.debug("upgrading data connection to TLS")
				dc = tls.Client(dc, pconn.config.TLSConfig)
			}
		}
	}

	if err != nil {
		return nil, err
	}

	pconn.dataConn = dc
	return dc, nil
}

func (pconn *persistentConn) setType(t string) error {
	return pconn.sendCommandExpected(ReplyCommandOkay, "TYPE %s", t)
}

func (pconn *persistentConn) logInTLS() error {
	err := pconn.sendCommandExpected(ReplyAuthOkayNoDataNeeded, "AUTH TLS")
	if err != nil {
		return err
	}

	pconn.setControlConn(tls.Client(pconn.controlConn, pconn.config.TLSConfig))

	err = pconn.logIn()
	if err != nil {
		return err
	}

	err = pconn.sendCommandExpected(ReplyGroupPositiveCompletion, "PBSZ 0")
	if err != nil {
		return err
	}

	err = pconn.sendCommandExpected(ReplyGroupPositiveCompletion, "PROT P")
	if err != nil {
		return err
	}

	pconn.debug("successfully upgraded to TLS")

	return nil
}