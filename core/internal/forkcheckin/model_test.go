package forkcheckin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func connectedAccount() Account {
	return Account{
		ID:               "account_one",
		DashboardBaseURL: "https://relay.example",
		State:            AccountStateConnected,
		Revision:         1,
		Network:          Network{Mode: NetworkModeDirect},
		TimeZone:         "Asia/Shanghai",
		RemoteUserID:     "7",
		BoundServices:    []contract.ServiceID{"service_one"},
	}
}

func TestAccountIDAcceptsOnlyPathSafeIdentifiers(t *testing.T) {
	for _, id := range []AccountID{"account_one", "a01", "abc"} {
		if err := id.Validate(); err != nil {
			t.Fatalf("Validate(%q) = %v", id, err)
		}
	}
	for _, id := range []AccountID{
		"", "ab", "Account_One", "account-one", "account one",
		"1account", "_account", "account/../other",
		AccountID(strings.Repeat("a", maxAccountIDLen+1)),
	} {
		if err := id.Validate(); err == nil {
			t.Fatalf("Validate(%q) accepted an unusable account id", id)
		}
	}
}

func TestNormalizeDashboardURLKeepsSubPathAndRefusesPlaintextRemotes(t *testing.T) {
	for _, testCase := range []struct{ in, want string }{
		{"https://relay.example", "https://relay.example"},
		{"https://relay.example/", "https://relay.example"},
		{"https://RELAY.Example/Base/", "https://relay.example/Base"},
		{"https://relay.example:8443/base", "https://relay.example:8443/base"},
		// The loopback exception mirrors the subscription issuers.
		{"http://127.0.0.1:3000/base", "http://127.0.0.1:3000/base"},
		{"http://localhost:3000", "http://localhost:3000"},
	} {
		got, err := NormalizeDashboardURL(testCase.in)
		if err != nil || got != testCase.want {
			t.Fatalf("NormalizeDashboardURL(%q) = %q, %v; want %q", testCase.in, got, err, testCase.want)
		}
	}
	for _, bad := range []string{
		"", "relay.example", "ftp://relay.example",
		"http://relay.example",
		"http://relay.example.localhost",
		"https://user:pass@relay.example",
		"https://relay.example/?tab=1",
		"https://relay.example/#top",
		"https://relay.example/base/../admin",
		"https://relay.example//base",
		" https://relay.example",
		"https://relay.example/ba se",
		"https://" + strings.Repeat("a", MaxDashboardURLLen),
	} {
		if got, err := NormalizeDashboardURL(bad); err == nil {
			t.Fatalf("NormalizeDashboardURL(%q) = %q, want an error", bad, got)
		}
	}
}

func TestValidateTimeZoneRequiresAnExplicitIANAZone(t *testing.T) {
	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York"} {
		if err := ValidateTimeZone(zone); err != nil {
			t.Fatalf("ValidateTimeZone(%q) = %v", zone, err)
		}
	}
	// "Local" would move the scheduled day whenever the host zone changes.
	for _, zone := range []string{"", "Local", "local", " UTC", "Mars/Olympus", "+08:00"} {
		if err := ValidateTimeZone(zone); err == nil {
			t.Fatalf("ValidateTimeZone(%q) accepted an unusable zone", zone)
		}
	}
}

func TestAccountValidateSeparatesDraftFromConnectedState(t *testing.T) {
	account := connectedAccount()
	if err := account.Validate(); err != nil {
		t.Fatalf("connected account: %v", err)
	}

	draft := account
	draft.State = AccountStateDraft
	draft.RemoteUserID = ""
	draft.Automatic = false
	if err := draft.Validate(); err != nil {
		t.Fatalf("draft account: %v", err)
	}

	withUser := draft
	withUser.RemoteUserID = "7"
	if err := withUser.Validate(); err == nil {
		t.Fatal("a draft account kept a remote user it never verified")
	}

	for _, state := range []AccountState{
		AccountStateConnected, AccountStateAuthRequired, AccountStateManualRequired,
	} {
		anonymous := account
		anonymous.State = state
		anonymous.RemoteUserID = ""
		anonymous.Automatic = false
		if err := anonymous.Validate(); err == nil {
			t.Fatalf("state %q was accepted without a verified remote user", state)
		}
	}

	// Automatic check-in on an unusable account would wake the scheduler
	// only to report that login is required.
	for _, state := range []AccountState{
		AccountStateAuthRequired, AccountStateManualRequired,
	} {
		automatic := account
		automatic.State = state
		automatic.Automatic = true
		if err := automatic.Validate(); err == nil {
			t.Fatalf("automatic check-in was accepted for a %q account", state)
		}
	}
}

func TestAccountValidateBoundsNetworkRevisionAndBindings(t *testing.T) {
	base := connectedAccount()
	for name, mutate := range map[string]func(*Account){
		"unknown state":        func(a *Account) { a.State = "signed_in" },
		"zero revision":        func(a *Account) { a.Revision = 0 },
		"negative revision":    func(a *Account) { a.Revision = -1 },
		"proxy without custom": func(a *Account) { a.Network = Network{Mode: NetworkModeDirect, ProxyURL: "socks5://p.example:1080"} },
		"custom without proxy": func(a *Account) { a.Network = Network{Mode: NetworkModeCustom} },
		"proxy with credentials": func(a *Account) {
			a.Network = Network{Mode: NetworkModeCustom, ProxyURL: "socks5://u:p@p.example:1080"}
		},
		"unknown network mode": func(a *Account) { a.Network = Network{Mode: "tunnel"} },
		"duplicate binding": func(a *Account) {
			a.BoundServices = []contract.ServiceID{"service_one", "service_one"}
		},
		"invalid service id": func(a *Account) { a.BoundServices = []contract.ServiceID{"Service_One"} },
		"too many bindings": func(a *Account) {
			a.BoundServices = make([]contract.ServiceID, MaxBoundServices+1)
			for index := range a.BoundServices {
				a.BoundServices[index] = contract.ServiceID("service_" + string(rune('a'+index%26)))
			}
		},
		"padded remote user": func(a *Account) { a.RemoteUserID = " 7" },
		"control character":  func(a *Account) { a.RemoteUserID = "7\n" },
	} {
		account := base
		mutate(&account)
		if err := account.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	custom := base
	custom.Network = Network{Mode: NetworkModeCustom, ProxyURL: "socks5://proxy.example:1080"}
	if err := custom.Validate(); err != nil {
		t.Fatalf("custom network: %v", err)
	}
	system := base
	system.Network = Network{Mode: NetworkModeSystem}
	if err := system.Validate(); err != nil {
		t.Fatalf("system network: %v", err)
	}
}

func TestIdentityIsOneSiteAndUserRegardlessOfBindings(t *testing.T) {
	account := connectedAccount()
	identity, ok := account.Identity()
	if !ok {
		t.Fatal("a connected account has no identity")
	}
	// Two accounts on the same relay are distinct records but must not
	// share a site-and-user key, or one day would check in twice.
	same := account
	same.ID = "account_two"
	same.DashboardBaseURL = "https://RELAY.example/"
	same.BoundServices = []contract.ServiceID{"service_two", "service_three"}
	sameIdentity, ok := same.Identity()
	if !ok || sameIdentity != identity {
		t.Fatalf("identity = %+v, want %+v", sameIdentity, identity)
	}
	other := account
	other.RemoteUserID = "8"
	if otherIdentity, _ := other.Identity(); otherIdentity == identity {
		t.Fatal("a different remote user shares an identity")
	}
	elsewhere := account
	elsewhere.DashboardBaseURL = "https://other.example"
	if elsewhereIdentity, _ := elsewhere.Identity(); elsewhereIdentity == identity {
		t.Fatal("a different site shares an identity")
	}

	draft := account
	draft.State = AccountStateDraft
	draft.RemoteUserID = ""
	if _, ok := draft.Identity(); ok {
		t.Fatal("a draft account claims an identity")
	}
}

func TestConfigFingerprintChangesWithEverythingThatRedirectsASession(t *testing.T) {
	base := connectedAccount()
	original := base.ConfigFingerprint()
	for name, mutate := range map[string]func(*Account){
		"address": func(a *Account) { a.DashboardBaseURL = "https://relay.example/base" },
		"network": func(a *Account) { a.Network = Network{Mode: NetworkModeSystem} },
		"proxy": func(a *Account) {
			a.Network = Network{Mode: NetworkModeCustom, ProxyURL: "socks5://proxy.example:1080"}
		},
		"remote user": func(a *Account) { a.RemoteUserID = "8" },
		"binding":     func(a *Account) { a.BoundServices = []contract.ServiceID{"service_two"} },
		"identifier":  func(a *Account) { a.ID = "account_two" },
	} {
		account := base
		mutate(&account)
		if account.ConfigFingerprint() == original {
			t.Fatalf("changing the %s did not change the fingerprint", name)
		}
	}

	// Equivalent spellings and binding order are the same configuration.
	normalized := base
	normalized.DashboardBaseURL = "https://RELAY.example/"
	if normalized.ConfigFingerprint() != original {
		t.Fatal("an equivalent address produced a different fingerprint")
	}
	reordered := base
	reordered.BoundServices = []contract.ServiceID{"service_one"}
	if reordered.ConfigFingerprint() != original {
		t.Fatal("binding order changed the fingerprint")
	}

	// Fields that do not decide where a session is sent must not invalidate
	// a running authorization.
	for name, mutate := range map[string]func(*Account){
		"revision":  func(a *Account) { a.Revision = 9 },
		"automatic": func(a *Account) { a.Automatic = true },
		"time zone": func(a *Account) { a.TimeZone = "UTC" },
	} {
		account := base
		mutate(&account)
		if account.ConfigFingerprint() != original {
			t.Fatalf("changing the %s invalidated the stored authorization", name)
		}
	}
}

func TestAccountViewPublishesNoSecretAndNoHiddenField(t *testing.T) {
	account := connectedAccount()
	account.Network = Network{Mode: NetworkModeCustom, ProxyURL: "socks5://proxy.example:1080"}
	encoded, err := json.Marshal(account.View())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"id", "dashboard_base_url", "state", "revision", "network",
		"time_zone", "automatic", "remote_user_id", "bound_services",
		"config_fingerprint",
	}
	if len(fields) != len(want) {
		t.Fatalf("published fields = %v, want exactly %v", fields, want)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing published field %q", key)
		}
	}
	// A session or proxy password must not be reachable through the DTO.
	for _, forbidden := range []string{"credential", "cookie", "session", "token", "password"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("published JSON mentions %q: %s", forbidden, encoded)
		}
	}

	// An empty binding list is published as [], so the UI never has to
	// distinguish null from empty.
	draft := connectedAccount()
	draft.State = AccountStateDraft
	draft.RemoteUserID = ""
	draft.BoundServices = nil
	encodedDraft, err := json.Marshal(draft.View())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedDraft), `"bound_services":[]`) {
		t.Fatalf("draft view = %s", encodedDraft)
	}
	if strings.Contains(string(encodedDraft), "remote_user_id") {
		t.Fatalf("a draft published an empty remote user: %s", encodedDraft)
	}
}

func TestAccountSnapshotRequiresABoundedStoredSession(t *testing.T) {
	account := connectedAccount()
	if err := (AccountSnapshot{Account: account, Credential: []byte("session")}).Validate(); err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	if err := (AccountSnapshot{Account: account}).Validate(); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("empty credential error = %v, want ErrCredentialUnavailable", err)
	}
	oversized := AccountSnapshot{Account: account, Credential: make([]byte, MaxCredentialBytes+1)}
	if err := oversized.Validate(); err == nil || errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("oversized credential error = %v", err)
	}
	invalid := AccountSnapshot{Account: Account{ID: "bad id"}, Credential: []byte("session")}
	if err := invalid.Validate(); err == nil {
		t.Fatal("a snapshot of an invalid account was accepted")
	}
}
