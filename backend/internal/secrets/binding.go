package secrets

import (
	"errors"
	"sync/atomic"
)

// Each sealed value is bound to where it lives (2.5.1 SG2.10, question
// 134(a)). The binding is GCM additional data in both envelope operations, so
// a value copied into another row (another agency's secret, another
// repository's token) does not open there, even with the KEK. Until 2.5.1 a
// value was bound to nothing, and anyone who could write the database could
// move one agency's sealed secret into another agency's row and have that
// agency's runs decrypt it.
//
// A secret's or an SSH key's row is bound by its table and id; it is not bound
// to its owner agency, which a repair can change (the id never changes, and
// is enough to keep the value in its row). A sealed settings column is bound
// by its table and its row (the row's key, or the singleton's condition), not
// by its column: a rotation moves a repository's webhook secret from one
// column to the other within its row.
//
// Values sealed before 2.5.1 are unbound. BindSeals re-seals every one at
// start; once it has done so without a failure it records that, and unbound
// seals are refused from then on (ErrSealUnbound), so an old unbound value
// copied into another row does not open either. That is what makes 2.5.1 a
// one-way upgrade: 2.5.0 cannot open a bound value.

// Binding names where a sealed value lives.
type Binding string

const bindingPrefix = "cronomicon/seal/v1|"

func bindingOf(table, row string) Binding { return Binding(bindingPrefix + table + "|" + row) }

// RowBinding binds a value sealed in a row of an envelope table (secrets,
// ssh_credentials) to that row.
func RowBinding(table, id string) Binding { return bindingOf(table, id) }

// The labels of the encrypted settings columns (EncryptedSettingsColumns),
// which settings code names to bind and open its tokens. A label is part of
// nothing stored, but it is how a caller finds the column's binding.
const (
	LabelGitToken                = "gitlab.token"
	LabelGitWebhookSecret        = "gitlab.webhookSecret"
	LabelGitWebhookSecretPrev    = "gitlab.webhookSecretPrevious"
	LabelLogStorageS3SecretKey   = "logStorage.s3SecretKey"
	LabelVaultRoleID             = "vault.roleId"
	LabelVaultSecretID           = "vault.secretId"
	LabelSMTPPassword            = "notifications.smtpPassword"
	LabelObservabilityBearer     = "observability.bearerToken"
	LabelAlertTargetURL          = "alerts.targetURL"
	LabelAgencyVaultRoleID       = "agencyVault.roleId"
	LabelAgencyVaultSecretID     = "agencyVault.secretId"
	sealedSettingsUnknownLabelID = "?"
)

// Binding is the binding of this column's token in the row whose key is key
// (ignored for a singleton column, which has one row).
func (sc SettingsColumn) Binding(key string) Binding {
	if sc.Key != "" {
		return bindingOf(sc.Table, key)
	}
	return bindingOf(sc.Table, sc.Where)
}

// SettingsBinding is the binding of the token of the column labelled label in
// the row whose key is key ("" for a singleton). An unknown label binds to a
// row nothing seals into, so a typo fails to open rather than opening anything.
func SettingsBinding(label, key string) Binding {
	for _, sc := range EncryptedSettingsColumns {
		if sc.Label == label {
			return sc.Binding(key)
		}
	}
	return bindingOf(sealedSettingsUnknownLabelID+label, key)
}

// ErrSealUnbound refuses a value sealed before 2.5.1 once every value has been
// bound (BindSeals): it is either one BindSeals could not reach, or one copied
// in from elsewhere.
var ErrSealUnbound = errors.New("sealed value is not bound to its row (sealed before 2.5.1, or copied from elsewhere)")

// unboundRefused is set once BindSeals has bound every value.
var unboundRefused atomic.Bool

// RefuseUnboundSeals switches the refusal of unbound seals on or off. Set by
// BindSeals; tests that need the pre-2.5.1 behaviour back reset it.
func RefuseUnboundSeals(on bool) { unboundRefused.Store(on) }

// UnboundSealsRefused reports whether unbound seals are refused.
func UnboundSealsRefused() bool { return unboundRefused.Load() }
