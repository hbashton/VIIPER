package retainedusb

import "testing"

func validTestLimits() Limits {
	return Limits{
		QueueDepth:             [3]uint8{2, 3, 4},
		BufferedRequestBytes:   40,
		MaximumControlOut:      4,
		MaximumControlResponse: 64,
		MaximumInterruptIn:     64,
		MaximumInterruptOut:    8,
		InterruptInRoute:       Route{EndpointAddress: 0x81},
		InterruptOutRoute:      Route{EndpointAddress: 0x01},
	}
}

func TestRetainedUSBLaneAndRouteShapes(t *testing.T) {
	for lane := LaneControl; lane <= LaneInterruptOut; lane++ {
		index, valid := lane.Index()
		if !valid || index != int(lane-LaneControl) {
			t.Fatalf("lane %d index = (%d, %t)", lane, index, valid)
		}
	}
	for _, lane := range []Lane{0, laneLimit, ^Lane(0)} {
		if lane.Valid() {
			t.Fatalf("invalid lane %d reported valid", lane)
		}
	}
	if !(Route{EndpointAddress: 0x81}).ValidInterruptIn() ||
		(Route{EndpointAddress: 0x81}).ValidInterruptOut() {
		t.Fatal("interrupt-IN route classification failed")
	}
	if !(Route{EndpointAddress: 0x01}).ValidInterruptOut() ||
		(Route{EndpointAddress: 0x01}).ValidInterruptIn() {
		t.Fatal("interrupt-OUT route classification failed")
	}
	for _, endpoint := range []uint8{0, 0x80, 0x71, 0xf1} {
		route := Route{EndpointAddress: endpoint}
		if route.ValidInterruptIn() || route.ValidInterruptOut() {
			t.Fatalf("reserved/zero endpoint 0x%02x reported valid", endpoint)
		}
	}
}

func TestRetainedUSBLimitsAreFixedAndBounded(t *testing.T) {
	valid := validTestLimits()
	if !valid.Valid() {
		t.Fatal("valid retained USB limits rejected")
	}
	tests := []struct {
		name   string
		mutate func(*Limits)
	}{
		{"zero queue", func(l *Limits) { l.QueueDepth[0] = 0 }},
		{"queue too deep", func(l *Limits) { l.QueueDepth[1] = MaximumQueueDepth + 1 }},
		{"byte cap", func(l *Limits) { l.BufferedRequestBytes = MaximumBufferedRequestBytes + 1 }},
		{"fixed slabs exceed budget", func(l *Limits) { l.BufferedRequestBytes-- }},
		{"zero IN maximum", func(l *Limits) { l.MaximumInterruptIn = 0 }},
		{"zero OUT maximum", func(l *Limits) { l.MaximumInterruptOut = 0 }},
		{"bad IN route", func(l *Limits) { l.InterruptInRoute.EndpointAddress = 1 }},
		{"bad OUT route", func(l *Limits) { l.InterruptOutRoute.EndpointAddress = 0x81 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if candidate.Valid() {
				t.Fatalf("invalid limits reported valid: %+v", candidate)
			}
		})
	}
}

func TestRetainedUSBTicketAndResultShapes(t *testing.T) {
	valid := Ticket{
		OwnerID: 1, Token: 2, Generation: 3,
		SessionGeneration: 4, Lane: LaneInterruptIn,
	}
	if !valid.Valid() {
		t.Fatal("valid ticket rejected")
	}
	for _, mutate := range []func(*Ticket){
		func(ticket *Ticket) { ticket.OwnerID = 0 },
		func(ticket *Ticket) { ticket.Token = 0 },
		func(ticket *Ticket) { ticket.Generation = 0 },
		func(ticket *Ticket) { ticket.SessionGeneration = 0 },
		func(ticket *Ticket) { ticket.Lane = 0 },
	} {
		candidate := valid
		mutate(&candidate)
		if candidate.Valid() {
			t.Fatalf("invalid ticket reported valid: %+v", candidate)
		}
	}
	for result := ResultPending; result <= ResultStall; result++ {
		if !result.Valid() {
			t.Fatalf("result %d rejected", result)
		}
	}
	if Result(0xff).Valid() {
		t.Fatal("unknown result reported valid")
	}
}

func TestRetainedUSBImportLeaseAndCloseReasonShapes(t *testing.T) {
	valid := ImportLease{
		AuthorityID: 1, DeviceID: 2, OwnerID: 3,
		ImportToken: 4, SessionGeneration: 5,
	}
	if !valid.Valid() {
		t.Fatal("valid retained import lease rejected")
	}
	for _, mutate := range []func(*ImportLease){
		func(lease *ImportLease) { lease.AuthorityID = 0 },
		func(lease *ImportLease) { lease.DeviceID = 0 },
		func(lease *ImportLease) { lease.OwnerID = 0 },
		func(lease *ImportLease) { lease.ImportToken = 0 },
		func(lease *ImportLease) { lease.SessionGeneration = 0 },
	} {
		candidate := valid
		mutate(&candidate)
		if candidate.Valid() {
			t.Fatalf("invalid retained import lease reported valid: %+v", candidate)
		}
	}
	for reason := ImportClosePeerDisconnect; reason <= ImportCloseInvariantFailure; reason++ {
		if !reason.Valid() {
			t.Fatalf("retained import close reason %d rejected", reason)
		}
	}
	for _, reason := range []ImportCloseReason{0, ImportCloseReason(0xff)} {
		if reason.Valid() {
			t.Fatalf("unknown retained import close reason %d reported valid", reason)
		}
	}
}
