package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, for the one request this provider makes (2.5.5
// SM-5, question 223: hand-written rather than the SDK, whose credential chain
// is what decision 2 refuses). It is the general algorithm, held to AWS's
// published test vectors in sigv4_test.go; it signs every header the request
// carries, so the caller sets only the headers it means to send.

const sigAlgorithm = "AWS4-HMAC-SHA256"

type credentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
}

// sign adds X-Amz-Date (and X-Amz-Security-Token for a session credential)
// and the Authorization header to req, whose body is payload.
func sign(req *http.Request, payload []byte, cred credentials, region, service string, now time.Time) {
	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if cred.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", cred.SessionToken)
	}
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}
	canonical, signed := canonicalRequest(req, payload)
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := sigAlgorithm + "\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(canonical))
	key := hmacSHA256([]byte("AWS4"+cred.SecretAccessKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", sigAlgorithm+" Credential="+cred.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
	// Go sends Host from req.Host, not from the header map.
	req.Host = req.Header.Get("Host")
	req.Header.Del("Host")
}

// canonicalRequest returns the canonical request and its signed-headers list.
func canonicalRequest(req *http.Request, payload []byte) (string, string) {
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	headers := map[string][]string{}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" {
			continue
		}
		for _, v := range vs {
			headers[lk] = append(headers[lk], strings.Join(strings.Fields(v), " "))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k + ":" + strings.Join(headers[k], ",") + "\n")
	}
	signed := strings.Join(names, ";")
	return req.Method + "\n" + path + "\n" + canonicalQuery(req.URL) + "\n" + ch.String() + "\n" + signed + "\n" + hexSHA256(payload), signed
}

func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's encoding: every byte but A-Z a-z 0-9 - _ . ~ as %XX.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
