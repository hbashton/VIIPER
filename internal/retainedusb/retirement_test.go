package retainedusb

import "testing"

func TestImportRetirementContractRequiresExactLeaseAndKnownReason(t *testing.T) {
	request := ImportRetirementRequest{
		Lease:  ImportLease{AuthorityID: 1, DeviceID: 2, OwnerID: 3, ImportToken: 4, SessionGeneration: 5},
		Reason: ImportRetirementInputHistoryOverflow,
	}
	if !request.Valid() || request.Reason.String() != "input presentation history overflow" ||
		!RetireOwnerRequested.Valid() || !ImportCloseOwnerRequested.Valid() {
		t.Fatal("valid owner retirement rejected")
	}
	for _, invalid := range []ImportRetirementRequest{{}, {Lease: request.Lease}, {Reason: request.Reason}, {Lease: request.Lease, Reason: 255}} {
		if invalid.Valid() {
			t.Fatalf("invalid retirement accepted: %+v", invalid)
		}
	}
}
