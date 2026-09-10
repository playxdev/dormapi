package pii

import (
	"testing"

	"github.com/playxdev/dormapi/internal/ulid"
)

func TestParityWithBackoffice(t *testing.T) {
	p, err := ParsePepper("ZGV2LW9ubHktcGlpLXBlcHBlci0zMi1ieXRlcyEhISE=")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"A7K9Q2MX":     "3JmXWWveJNFfbTC/xZyigKvo1BHnJOwmSKn+QmwpNsk=",
		"a7k9-q2mx":    "3JmXWWveJNFfbTC/xZyigKvo1BHnJOwmSKn+QmwpNsk=",
		"07K9Q2MX":     "W8dp9XbrjkG/DC0YNY/svUtPSOKso3rhio7j3oiHRUY=",
		"IL0UZZZZ":     "x2/rL0fmS0Tswvz5lE8w88WcwOPcf/MPSyC+DNxSmBM=",
		"0812345678":   "wYRNwdhERu6pUokcaC6w/cbpt50FiPGAQaV3kJWuhqA=",
		"+66812345678": "wYRNwdhERu6pUokcaC6w/cbpt50FiPGAQaV3kJWuhqA=",
	}
	for in, want := range cases {
		if got := p.Hash(ulid.NormalizeCrockford(in)); got != want {
			t.Errorf("Hash(%q) = %q, want %q", in, got, want)
		}
	}
}
