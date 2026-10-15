package taxapi

import (
	"strings"
	"testing"
)

func TestParseAttestationMemo(t *testing.T) {
	cases := []struct {
		memo    string
		want    AttestationMemo
		wantErr string
	}{
		{memo: `bryanlabs-tax:jurisdiction v1 country=CH entity="Chorus One AG" source=https://chorus.one/imprint`, want: AttestationMemo{Country: "CH", Entity: "Chorus One AG", Source: "https://chorus.one/imprint"}},
		{memo: `  bryanlabs-tax:jurisdiction   v1   country=de  `, want: AttestationMemo{Country: "DE"}},
		{memo: `bryanlabs-tax:jurisdiction v1 country=none`, want: AttestationMemo{Revoke: true}},
		{memo: `bryanlabs-tax:jurisdiction v1 country=NONE entity="Gone Ltd"`, want: AttestationMemo{Revoke: true, Entity: "Gone Ltd"}},
		{memo: `bryanlabs-tax:jurisdiction v1 entity="No Country"`, wantErr: "country is required"},
		{memo: `bryanlabs-tax:jurisdiction v1 country=XX`, wantErr: "ISO 3166-1"},
		{memo: `bryanlabs-tax:jurisdiction v1 country=CH country=DE`, wantErr: "given twice"},
		{memo: `bryanlabs-tax:jurisdiction v1 country=CH entity=Chorus One AG`, wantErr: "quote values"},
		{memo: `bryanlabs-tax:jurisdiction v1 country=CH source=chorus.one`, wantErr: "http(s) URL"},
		{memo: `bryanlabs-tax:jurisdiction v1 country=CH nation=CH`, wantErr: "unknown key"},
		{memo: `bryanlabs-tax:jurisdiction v2 country=CH`, wantErr: "format version"},
		{memo: `hello`, wantErr: "must start with"},
		{memo: `bryanlabs-tax:jurisdiction v1`, wantErr: "country is required"},
	}
	for _, c := range cases {
		got, err := ParseAttestationMemo(c.memo)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%q: want error containing %q, got %v (parsed %+v)", c.memo, c.wantErr, err, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.memo, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v want %+v", c.memo, got, c.want)
		}
	}
}

func TestOperatorAccountRoundTrip(t *testing.T) {
	// Strangelove's operator address and the account that controls it share bytes.
	op := "cosmosvaloper130mdu9a0etmeuw52qfxk73pn0ga6gawkxsrlwf"
	acct, err := OperatorAccount(op)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(acct, "cosmos1") || strings.HasPrefix(acct, "cosmosvaloper") {
		t.Fatalf("account %q", acct)
	}
	back, err := AccountOperator(acct)
	if err != nil {
		t.Fatal(err)
	}
	if back != op {
		t.Fatalf("round trip %q -> %q -> %q", op, acct, back)
	}
	if _, err := OperatorAccount(acct); err == nil {
		t.Fatal("an account address must not be accepted as an operator address")
	}
	if _, err := AccountOperator(op); err == nil {
		t.Fatal("an operator address must not be accepted as an account address")
	}
	if _, err := AccountOperator("cosmos1typo"); err == nil {
		t.Fatal("a bad checksum must fail")
	}
}

func TestValidatorStatusLabel(t *testing.T) {
	if validatorStatusLabel("BOND_STATUS_BONDED", true) != "jailed" {
		t.Fatal("jailed wins")
	}
	if validatorStatusLabel("BOND_STATUS_UNBONDING", false) != "unbonding" || validatorStatusLabel("BOND_STATUS_BONDED", false) != "bonded" || validatorStatusLabel("", false) != "unknown" {
		t.Fatal("status mapping")
	}
}
