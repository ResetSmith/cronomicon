package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// The address alerts link to (2.5.2 OP2.1, replacing OP-7's
// CRONOMICON_PUBLIC_URL). Nothing in the server knows the address people open
// the app at, but their browser does: the first global administrator to open
// the app records it (DetectAlertLinkAddress), as the GitLab webhook URL and
// the runner install commands already take theirs from the page. It is shown
// on Settings → Notifications, and a global administrator may change it there.
// Unset, alerts carry no link. It is checked when it is written, never at
// start, so a bad value cannot stop the server.

// AlertLinkKey is its settings row: the JSON of AlertLinkAddress.
const AlertLinkKey = "notify.alertLinkAddress"

// AlertLinkAddress is the stored address and how it got there. Source is
// "detected" (recorded from a global administrator's browser) or "set" (typed
// on Settings → Notifications); By and At say who and when. An Address of ""
// with Source "set" is a cleared one: no links, and nothing re-detects it.
type AlertLinkAddress struct {
	Address string `json:"address"`
	Source  string `json:"source,omitempty"`
	By      string `json:"by,omitempty"`
	At      string `json:"at,omitempty"`
}

// ErrInvalidAlertLinkAddress: not an absolute http(s) address with a host, or
// one carrying a query, a fragment or a user (a link is appended to it).
var ErrInvalidAlertLinkAddress = errors.New("the alert link address must be an http:// or https:// address with a host, and no query, fragment or user")

// NormalizeAlertLinkAddress trims the address and its trailing slash and
// checks it. A path is kept, for an app served under one; "" stays "".
func NormalizeAlertLinkAddress(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", ErrInvalidAlertLinkAddress
	}
	return s, nil
}

// ReadAlertLinkAddress is the stored address; the zero value when none is.
func ReadAlertLinkAddress(ctx context.Context, database *sql.DB) (AlertLinkAddress, error) {
	var raw string
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, AlertLinkKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertLinkAddress{}, nil
	}
	if err != nil {
		return AlertLinkAddress{}, fmt.Errorf("read the alert link address: %w", err)
	}
	var a AlertLinkAddress
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return AlertLinkAddress{}, fmt.Errorf("read the alert link address: %w", err)
	}
	return a, nil
}

// SetAlertLinkAddress stores an address a global administrator typed ("" clears
// it, and a cleared one stays cleared: DetectAlertLinkAddress records nothing
// over it). Recorded in Global's change log.
func SetAlertLinkAddress(ctx context.Context, database *sql.DB, address, actor string) (AlertLinkAddress, error) {
	address, err := NormalizeAlertLinkAddress(address)
	if err != nil {
		return AlertLinkAddress{}, err
	}
	a := AlertLinkAddress{Address: address, Source: "set", By: actor, At: time.Now().UTC().Format(time.RFC3339)}
	raw, err := json.Marshal(a)
	if err != nil {
		return AlertLinkAddress{}, err
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, last_modified_by = excluded.last_modified_by,
		                               last_modified_at = excluded.last_modified_at`,
		AlertLinkKey, string(raw), actor, a.At); err != nil {
		return AlertLinkAddress{}, fmt.Errorf("set the alert link address: %w", err)
	}
	detail := address
	if detail == "" {
		detail = "none: alerts carry no link"
	}
	audit(ctx, database, agencyid.Global, actor, "Notifications", "alert-link-address-set", "", detail)
	return a, nil
}

// DetectAlertLinkAddress records the address a global administrator's browser
// is at, only when none has been stored (detected, set or cleared): the first
// visit decides, and a later visit through another name (a tunnel, an internal
// address) changes nothing. recorded says whether this call stored it.
func DetectAlertLinkAddress(ctx context.Context, database *sql.DB, address, actor string) (a AlertLinkAddress, recorded bool, err error) {
	address, err = NormalizeAlertLinkAddress(address)
	if err != nil {
		return AlertLinkAddress{}, false, err
	}
	if address == "" {
		return AlertLinkAddress{}, false, ErrInvalidAlertLinkAddress
	}
	a = AlertLinkAddress{Address: address, Source: "detected", By: actor, At: time.Now().UTC().Format(time.RFC3339)}
	raw, err := json.Marshal(a)
	if err != nil {
		return AlertLinkAddress{}, false, err
	}
	res, err := database.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO NOTHING`, AlertLinkKey, string(raw), actor, a.At)
	if err != nil {
		return AlertLinkAddress{}, false, fmt.Errorf("record the alert link address: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := ReadAlertLinkAddress(ctx, database)
		return cur, false, err
	}
	audit(ctx, database, agencyid.Global, actor, "Notifications", "alert-link-address-detected", "", address)
	return a, true, nil
}
