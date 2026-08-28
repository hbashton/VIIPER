package inputlatency

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRecorderCorrelatesTerminalCommitByExactReceiveBoundary(t *testing.T) {
	session, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	receivedAt := time.Unix(1, 2)
	RecordTransportAdmitted(0x11, time.Unix(9, 9), 1, 1, 5) // unmatched polling
	RecordBrokerReceived(0x11, 7, receivedAt, 20)
	RecordTransportAdmitted(0x11, receivedAt, 8, 3, 40)

	record, err := session.Await(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != 7 || record.ReceivedAt != receivedAt ||
		record.BrokerReceivedTicks != 20 || record.TransportAdmittedTicks != 40 ||
		record.PresentationToken != 8 || record.PresentationGeneration != 3 {
		t.Fatalf("unexpected admission: %+v", record)
	}
	if _, err = session.Await(context.Background(), 7); err == nil ||
		!strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("duplicate consume result: %v", err)
	}
}

func TestRecorderFailsClosedOnAmbiguousReceiveTimestamp(t *testing.T) {
	session, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	receivedAt := time.Unix(3, 4)
	RecordBrokerReceived(0x22, 1, receivedAt, 10)
	RecordBrokerReceived(0x22, 2, receivedAt, 11)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err = session.Await(ctx, 1); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous timestamp result: %v", err)
	}
}

func TestRecorderFailsClosedOnSourceCollisionAndBadAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		record func(time.Time)
		want   string
	}{
		{
			name: "source-collision",
			record: func(at time.Time) {
				RecordBrokerReceived(0x31, 1, at, 10)
				RecordBrokerReceived(0x32, 2, at.Add(time.Nanosecond), 11)
			},
			want: "multiple DualSense sources",
		},
		{
			name: "admission-before-broker",
			record: func(at time.Time) {
				RecordBrokerReceived(0x31, 1, at, 20)
				RecordTransportAdmitted(0x31, at, 1, 1, 19)
			},
			want: "invalid terminal admission",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, err := Start()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			test.record(time.Unix(5, 6))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err = session.Await(ctx, 1); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("result=%v, want %q", err, test.want)
			}
		})
	}
}

func TestRecorderRejectsNestedSessions(t *testing.T) {
	first, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Start(); err == nil {
		second.Close()
		t.Fatal("nested recorder session succeeded")
	}
}
