// Package promptpay builds Thai QR Payment payloads.
//
// EMVCo QR Code Specification for Payment Systems, Merchant-Presented Mode, as
// profiled by the Thai Bankers' Association.
//
// This mirrors src/lib/promptpay.ts in the backoffice. Two implementations of
// one wire format is a correctness risk, so the tests here assert byte-for-byte
// equality with payloads produced by that file.
package promptpay

import (
	"fmt"
	"strings"
)

const aidPromptPay = "A000000677010111"

func tlv(tag, value string) string {
	return fmt.Sprintf("%s%02d%s", tag, len(value), value)
}

// CRC16 is CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF), which tag 63
// requires.
func CRC16(input string) string {
	crc := 0xffff
	for i := 0; i < len(input); i++ {
		crc ^= int(input[i]) << 8
		for b := 0; b < 8; b++ {
			if crc&0x8000 != 0 {
				crc = ((crc << 1) ^ 0x1021) & 0xffff
			} else {
				crc = (crc << 1) & 0xffff
			}
		}
	}
	return fmt.Sprintf("%04X", crc)
}

type targetKind int

const (
	kindMobile targetKind = iota
	kindNationalID
	kindEWallet
)

type target struct {
	kind  targetKind
	value string
}

// normalize detects what kind of PromptPay ID was entered and puts it in the
// form the spec wants. A 10-digit local mobile number becomes 0066 plus its
// last nine digits.
func normalize(raw string) (target, bool) {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()

	switch {
	case d == "":
		return target{}, false
	case len(d) == 15:
		return target{kindEWallet, d}, true
	case len(d) == 13:
		return target{kindNationalID, d}, true
	case len(d) == 10 && strings.HasPrefix(d, "0"):
		return target{kindMobile, "0066" + d[1:]}, true
	case len(d) == 11 && strings.HasPrefix(d, "66"):
		return target{kindMobile, "00" + d}, true
	case len(d) == 12 && strings.HasPrefix(d, "660"):
		return target{kindMobile, "0066" + d[3:]}, true
	case len(d) == 9:
		return target{kindMobile, "0066" + d}, true
	}
	return target{}, false
}

// Payload builds the string encoded into the QR image.
//
// A zero amount produces a *static* payload: the payer types the amount into
// their banking app. That is what makes paying an invoice in instalments
// possible — a payload with the amount embedded cannot be part-paid.
func Payload(promptPayID string, amountSatang int64) (string, bool) {
	t, ok := normalize(promptPayID)
	if !ok {
		return "", false
	}

	subTag := "01"
	switch t.kind {
	case kindNationalID:
		subTag = "02"
	case kindEWallet:
		subTag = "03"
	}

	merchant := tlv("00", aidPromptPay) + tlv(subTag, t.value)
	dynamic := amountSatang > 0

	initMethod := "11"
	if dynamic {
		initMethod = "12"
	}

	// Field order follows the de-facto Thai deployment (country before
	// currency), matching what bank apps in the wild accept.
	payload := tlv("00", "01") +
		tlv("01", initMethod) +
		tlv("29", merchant) +
		tlv("58", "TH") +
		tlv("53", "764")

	if dynamic {
		payload += tlv("54", fmt.Sprintf("%d.%02d", amountSatang/100, amountSatang%100))
	}

	payload += "6304"
	return payload + CRC16(payload), true
}
