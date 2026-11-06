// Package aws is the secret store provider for AWS Secrets Manager (2.5.5
// SM-2, SM-5).
//
// A reference is a secret's name or ARN, optionally followed by "#key": with a
// key, the secret's SecretString is read as a JSON object and the key's string
// value is the value; without one, the whole SecretString is. A binary secret
// is refused.
//
// The prefix rule (LR-80) is vaultpath's shape on the secret's NAME: whole
// "/"-separated segments, no empty, "." or ".." segment. An ARN is judged on
// its name, the part after ":secret:", with the six-character suffix AWS adds
// to a full ARN removed.
//
// The credential is static and sealed per agency (SM-4): an access key id, a
// secret access key and, optionally, a session token. Nothing is read from the
// environment, an instance role or the metadata service; the client is built
// over httpx.SafeTransport, which refuses the metadata addresses.
package aws

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
)

// ID is the provider's stored value.
const ID = "aws"

// The connection's keys. Settings: region, endpoint (optional: a VPC or FIPS
// endpoint, the only address the allowed hosts bind for this provider, SM-7).
// Credential: accessKeyId, secretAccessKey, sessionToken (optional).
const (
	SettingRegion       = "region"
	SettingEndpoint     = "endpoint"
	SettingCAPEM        = "caPem" // with an endpoint only: the CA it is reached with
	CredAccessKeyID     = "accessKeyId"
	CredSecretAccessKey = "secretAccessKey"
	CredSessionToken    = "sessionToken"
)

func init() { secretstore.Register(Provider{}) }

// Provider is AWS Secrets Manager.
type Provider struct{}

func (Provider) ID() string    { return ID }
func (Provider) Label() string { return "AWS Secrets Manager" }

var (
	regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	// The characters AWS allows in a secret name.
	nameCharRE = regexp.MustCompile(`^[A-Za-z0-9/_+=.@-]+$`)
	arnRE      = regexp.MustCompile(`^arn:aws[a-z-]*:secretsmanager:([a-z0-9-]+):([0-9]{12}):secret:(.+)$`)
	suffixRE   = regexp.MustCompile(`-[A-Za-z0-9]{6}$`)
	keyRE      = regexp.MustCompile(`^[^\x00-\x1f\x7f]+$`)
)

const maxNameLen = 512

// split separates a reference into its secret id and "#key" at the LAST '#'
// ('#' is not a name character, so there is at most one in a valid ref).
func split(ref string) (id, key string) {
	if i := strings.LastIndex(ref, "#"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// name is the secret name a secret id names: the id itself, or an ARN's name
// part without the suffix of a full ARN.
func name(id string) string {
	if m := arnRE.FindStringSubmatch(id); m != nil {
		return suffixRE.ReplaceAllString(m[3], "")
	}
	return id
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{secretstore.ErrInvalid}, args...)...)
}

// segments checks a name or a prefix and returns its segments.
func segments(s, what string) ([]string, error) {
	if s == "" {
		return nil, invalid("the %s is empty", what)
	}
	if len(s) > maxNameLen {
		return nil, invalid("the %s is longer than %d characters", what, maxNameLen)
	}
	if !nameCharRE.MatchString(s) {
		return nil, invalid("%q holds a character an AWS secret name cannot (letters, digits and / _ + = . @ - only)", s)
	}
	segs := strings.Split(s, "/")
	for _, g := range segs {
		switch g {
		case "":
			return nil, invalid("%q has an empty segment (a slash at either end, or a doubled slash)", s)
		case ".", "..":
			return nil, invalid("%q has a %q segment", s, g)
		}
	}
	return segs, nil
}

// ValidateRef refuses a malformed reference as written.
func (Provider) ValidateRef(ref string) error {
	id, key := split(ref)
	if strings.Contains(ref, "#") && !keyRE.MatchString(key) {
		return invalid("%q names an empty or unprintable key after '#'", ref)
	}
	if strings.HasPrefix(id, "arn:") && !arnRE.MatchString(id) {
		return invalid("%q is not a Secrets Manager secret ARN (arn:aws:secretsmanager:<region>:<account>:secret:<name>)", id)
	}
	_, err := segments(name(id), "secret name")
	return err
}

// NormalizePrefix stores a prefix without a slash at its end ("app/" and
// "app" are one prefix: whole segments).
func (Provider) NormalizePrefix(prefix string) (string, error) {
	p := strings.TrimSuffix(prefix, "/")
	if _, err := segments(p, "prefix"); err != nil {
		return "", err
	}
	return p, nil
}

func (p Provider) UnderPrefix(ref, prefix string) (bool, error) {
	if err := p.ValidateRef(ref); err != nil {
		return false, err
	}
	id, _ := split(ref)
	ns, _ := segments(name(id), "secret name")
	xs, err := segments(strings.TrimSuffix(prefix, "/"), "prefix")
	if err != nil {
		return false, err
	}
	if len(xs) > len(ns) {
		return false, nil
	}
	for i := range xs {
		if ns[i] != xs[i] { // secret names are case-sensitive
			return false, nil
		}
	}
	return true, nil
}

func (Provider) Location(ref string) string {
	id, _ := split(ref)
	return id
}

// Address is the endpoint when the connection names one; the region's public
// endpoint is not a host an administrator chose (SM-7).
func (Provider) Address(c secretstore.Connection) string { return c.Settings[SettingEndpoint] }

func (Provider) Validate(c secretstore.Connection) error {
	if !regionRE.MatchString(c.Settings[SettingRegion]) {
		return fmt.Errorf("an AWS Secrets Manager connection needs its region (such as us-east-1)")
	}
	if ep := c.Settings[SettingEndpoint]; ep != "" {
		u, err := url.Parse(ep)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return fmt.Errorf("the endpoint must be an https:// address with no path, user or query")
		}
	}
	if pem := c.Settings[SettingCAPEM]; pem != "" {
		if c.Settings[SettingEndpoint] == "" {
			return fmt.Errorf("a CA bundle is for an endpoint of your own; the region's endpoint uses the public roots")
		}
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(pem)) {
			return fmt.Errorf("the CA bundle holds no PEM certificate")
		}
	}
	if c.Credential[CredAccessKeyID] == "" || c.Credential[CredSecretAccessKey] == "" {
		return fmt.Errorf("an AWS Secrets Manager connection needs an access key id and a secret access key: " +
			"Cronomicon never uses the server's own AWS identity")
	}
	return nil
}

func (p Provider) NewClient(c secretstore.Connection, egress httpx.EgressPolicy) (secrets.VaultClient, error) {
	if err := p.Validate(c); err != nil {
		return nil, err
	}
	endpoint := strings.TrimSuffix(c.Settings[SettingEndpoint], "/")
	if endpoint == "" {
		endpoint = "https://secretsmanager." + c.Settings[SettingRegion] + ".amazonaws.com"
	}
	cl := newClient(endpoint, c.Settings[SettingRegion], credentials{
		AccessKeyID:     c.Credential[CredAccessKeyID],
		SecretAccessKey: c.Credential[CredSecretAccessKey],
		SessionToken:    c.Credential[CredSessionToken],
	}, egress)
	if pem := c.Settings[SettingCAPEM]; pem != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(pem))
		cl.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return cl, nil
}

// ErrBinary refuses a binary secret: a run's variable is text.
var ErrBinary = errors.New("the secret is binary; Cronomicon reads text secrets only")
