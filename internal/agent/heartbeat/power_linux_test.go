//go:build linux

package heartbeat

import (
	"context"
	"testing"
	"time"
)

func TestSleepDelayWaitsForHeartbeatCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan PowerObservation)
	released := make(chan struct{})
	go publishBeforeSleep(ctx, events, time.Now(), func() { close(released) })
	select {
	case e := <-events:
		if e.State != "sleeping" || e.sendBy.Sub(e.At) != sleepSendBudget {
			t.Fatalf("sleep power event: %+v", e)
		}
		select {
		case <-released:
			t.Fatal("inhibitor released before the heartbeat attempt")
		default:
		}
		close(e.completed)
	case <-time.After(time.Second):
		t.Fatal("no sleep event")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("heartbeat completion did not release the inhibitor")
	}
}

func TestSleepDelayReleasesWhenWorkerIsBusy(t *testing.T) {
	events := make(chan PowerObservation)
	released := make(chan struct{})
	at := time.Now().Add(-sleepSendBudget + 25*time.Millisecond)
	go publishBeforeSleep(context.Background(), events, at, func() { close(released) })
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("inhibitor persisted beyond the heartbeat budget")
	}
}
