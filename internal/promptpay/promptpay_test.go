package promptpay

import (
	"strings"
	"testing"
)

// Reference payloads produced by src/lib/promptpay.ts in the backoffice.
//
// Two implementations of one wire format is a correctness risk that a bank app
// would surface as a QR it refuses to read, so this asserts byte-for-byte
// equality rather than trusting that both followed the same spec.
func TestPayloadMatchesBackoffice(t *testing.T) {
	cases := []struct {
		id     string
		satang int64
		want   string
	}{
		{id: "0812345678", satang: 525000, want: "00020101021229370016A000000677010111011300668123456785802TH530376454075250.0063045311"},
		{id: "0812345678", satang: 0, want: "00020101021129370016A000000677010111011300668123456785802TH530376463045D82"},
		{id: "0812345678", satang: 450000, want: "00020101021229370016A000000677010111011300668123456785802TH530376454074500.006304E06E"},
		{id: "1234567890123", satang: 100, want: "00020101021229370016A000000677010111021312345678901235802TH530376454041.006304304C"},
		{id: "123456789012345", satang: 999999999, want: "00020101021229390016A00000067701011103151234567890123455802TH530376454109999999.9963044036"},
		{id: "66812345678", satang: 12345, want: "00020101021229370016A000000677010111011300668123456785802TH53037645406123.456304906E"},
		{id: "812345678", satang: 1, want: "00020101021229370016A000000677010111011300668123456785802TH530376454040.01630437B6"},
		{id: "0899999999", satang: 517000, want: "00020101021229370016A000000677010111011300668999999995802TH530376454075170.006304592E"},
	}

	for _, c := range cases {
		got, ok := Payload(c.id, c.satang)
		if !ok {
			t.Errorf("Payload(%q, %d) rejected the id", c.id, c.satang)
			continue
		}
		if got != c.want {
			t.Errorf("Payload(%q, %d)\n got  %s\n want %s", c.id, c.satang, got, c.want)
		}
	}
}

// A zero amount must produce a static payload, which is what lets a tenant pay
// in instalments: initiation method 11 rather than 12, and no amount tag.
func TestZeroAmountIsStatic(t *testing.T) {
	got, ok := Payload("0812345678", 0)
	if !ok {
		t.Fatal("Payload rejected a valid id")
	}
	if !strings.HasPrefix(got, "000201010211") {
		t.Errorf("want static initiation method 11, got %q", got[:12])
	}
	if strings.Contains(got, "5407") || strings.Contains(got, "5404") {
		t.Error("a static payload must carry no amount tag 54")
	}
}

func TestRejectsUnusableID(t *testing.T) {
	for _, id := range []string{"", "abc", "123", "0812345"} {
		if _, ok := Payload(id, 100); ok {
			t.Errorf("Payload(%q) accepted an unusable id", id)
		}
	}
}
