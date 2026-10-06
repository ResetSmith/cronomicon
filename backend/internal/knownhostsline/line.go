// Package knownhostsline reads and writes single known_hosts lines the way the
// runner's verifier does (SB band).
//
// Server and agent both need this and must agree with a third party: the
// golang.org/x/crypto/ssh/knownhosts loader that every run goes through. That
// loader fails the WHOLE FILE on one line it cannot read, so a line this
// package lets through must be one it accepts, and a line it accepts must be
// one this package can read back — to show it, and to take it away again.
// ssh.ParseKnownHosts is not that: it rejects a line whose comment has a space
// in it, which the loader happily trusts.
//
// A leaf: the standard library and x/crypto only, so the agent can import it.
package knownhostsline

import (
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Line is one parsed known_hosts line.
type Line struct {
	Marker string   // "" | "revoked" | "cert-authority" | whatever followed '@'
	Hosts  []string // the comma-separated host patterns, as written
	Key    ssh.PublicKey
}

// Parse reads one line: `[@marker] hosts key-type base64-key [comment…]`.
// Everything after the key is a comment and is ignored, as the loader ignores
// it. A blank line or a `#` comment is an error here; callers skip those first.
func Parse(line string) (Line, error) {
	fields := strings.Fields(line)
	var l Line
	if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
		l.Marker = fields[0][1:]
		fields = fields[1:]
	}
	if len(fields) < 3 {
		return l, errors.New("expected: host key-type base64-key")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		return l, errors.New("the key is not base64")
	}
	key, err := ssh.ParsePublicKey(blob)
	if err != nil {
		return l, errors.New("the key cannot be read")
	}
	if key.Type() != fields[1] {
		return l, errors.New("the key type does not match the key")
	}
	l.Hosts, l.Key = strings.Split(fields[0], ","), key
	return l, nil
}

// Render builds the line for one host pattern and key: no marker, no comment.
func Render(pattern string, key ssh.PublicKey) string {
	return pattern + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// IsHashed reports whether a host is a hashed entry (`|1|salt|hash`).
func IsHashed(host string) bool { return strings.HasPrefix(host, "|1|") }

// ValidPattern reports whether host is ONE host the loader will accept and
// that names exactly one host: a plain name or address, `[host]:port`, or a
// well-formed hashed entry. Wildcards and negations are valid known_hosts but
// not one host, and are refused here; so is anything the loader would choke on
// — an empty host, an unbalanced bracket, a hash with the wrong parts — because
// one such line in the file makes every connection from that runner fail.
func ValidPattern(host string) bool {
	if host == "" || len(host) > 1024 || strings.ContainsAny(host, " \t\r\n,*?!#@") {
		return false
	}
	if IsHashed(host) {
		parts := strings.Split(host, "|")
		if len(parts) != 4 || parts[2] == "" || parts[3] == "" {
			return false
		}
		_, err1 := base64.StdEncoding.DecodeString(parts[2])
		_, err2 := base64.StdEncoding.DecodeString(parts[3])
		return err1 == nil && err2 == nil
	}
	if strings.Contains(host, "|") {
		return false
	}
	if !strings.ContainsAny(host, "[]") {
		return true
	}
	// Brackets only in the one form that means something: [host]:port.
	h, p, err := net.SplitHostPort(host)
	if err != nil || h == "" || !strings.HasPrefix(host, "[") || strings.ContainsAny(h, "[]") {
		return false
	}
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

// HostPort splits a known_hosts host into the host and port the loader
// compares: `[host]:port` as written, port 22 otherwise.
func HostPort(pattern string) (host, port string) {
	if h, p, err := net.SplitHostPort(pattern); err == nil {
		return h, p
	}
	return pattern, "22"
}
