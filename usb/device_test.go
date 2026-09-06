package usb

import "testing"

func TestControlTransactionClaimStructuralValidity(t *testing.T) {
	tests := []struct {
		name    string
		claim   ControlTransactionClaim
		valid   bool
		handled bool
	}{
		{name: "unhandled", claim: ControlTransactionClaim{}, valid: true},
		{name: "data", claim: ControlTransactionClaim{
			Token: 1, Generation: 1, Result: ControlTransactionData,
			ResponseLength: 4,
		}, valid: true, handled: true},
		{name: "zero-length data", claim: ControlTransactionClaim{
			Token: 1, Generation: 1, Result: ControlTransactionData,
		}},
		{name: "no data", claim: ControlTransactionClaim{
			Token: 1, Generation: 1, Result: ControlTransactionNoData,
		}, valid: true, handled: true},
		{name: "stall", claim: ControlTransactionClaim{
			Token: 1, Generation: 1, Result: ControlTransactionStall,
		}, valid: true, handled: true},
		{name: "unhandled with token", claim: ControlTransactionClaim{
			Token: 1,
		}},
		{name: "data without token", claim: ControlTransactionClaim{
			Generation: 1, Result: ControlTransactionData,
		}},
		{name: "no-data with length", claim: ControlTransactionClaim{
			Token: 1, Generation: 1, Result: ControlTransactionNoData,
			ResponseLength: 1,
		}},
		{name: "unknown result", claim: ControlTransactionClaim{
			Token: 1, Generation: 1, Result: 0xff,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.claim.Valid(); got != test.valid {
				t.Fatalf("Valid() = %t, want %t", got, test.valid)
			}
			if got := test.claim.Handled(); got != test.handled {
				t.Fatalf("Handled() = %t, want %t", got, test.handled)
			}
		})
	}
}

func TestInterruptOutTransactionClaimStructuralValidity(t *testing.T) {
	tests := []struct {
		name    string
		claim   InterruptOutTransactionClaim
		valid   bool
		handled bool
	}{
		{name: "unhandled", claim: InterruptOutTransactionClaim{}, valid: true},
		{name: "accepted", claim: InterruptOutTransactionClaim{
			Token: 1, Generation: 1, Result: InterruptOutTransactionAccepted,
		}, valid: true, handled: true},
		{name: "stall", claim: InterruptOutTransactionClaim{
			Token: 1, Generation: 1, Result: InterruptOutTransactionStall,
		}, valid: true, handled: true},
		{name: "unhandled with token", claim: InterruptOutTransactionClaim{
			Token: 1,
		}},
		{name: "accepted without token", claim: InterruptOutTransactionClaim{
			Generation: 1, Result: InterruptOutTransactionAccepted,
		}},
		{name: "accepted without generation", claim: InterruptOutTransactionClaim{
			Token: 1, Result: InterruptOutTransactionAccepted,
		}},
		{name: "unknown result", claim: InterruptOutTransactionClaim{
			Token: 1, Generation: 1, Result: 0xff,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.claim.Valid(); got != test.valid {
				t.Fatalf("Valid() = %t, want %t", got, test.valid)
			}
			if got := test.claim.Handled(); got != test.handled {
				t.Fatalf("Handled() = %t, want %t", got, test.handled)
			}
		})
	}
}
