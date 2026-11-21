//go:build linux

package heartbeat

import (
	"context"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const login1 = "org.freedesktop.login1"
const loginManager = login1 + ".Manager"
const loginPath = "/org/freedesktop/login1"
const sleepSendBudget = 2 * time.Second

// WatchPower holds a logind delay inhibitor while subscribed. On a sleep
// signal it releases the lock after the bounded heartbeat attempt, and
// takes a new lock on resume. No power settings are changed.
func WatchPower(ctx context.Context) <-chan PowerObservation {
	events := make(chan PowerObservation, 1)
	go func() {
		defer close(events)
		conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
		if err != nil {
			return
		}
		defer conn.Close()
		signals := make(chan *dbus.Signal, 2)
		conn.Signal(signals)
		defer conn.RemoveSignal(signals)
		if err := conn.AddMatchSignalContext(ctx,
			dbus.WithMatchSender(login1), dbus.WithMatchObjectPath(loginPath),
			dbus.WithMatchInterface(loginManager), dbus.WithMatchMember("PrepareForSleep")); err != nil {
			return
		}
		manager := conn.Object(login1, loginPath)
		release := takeSleepDelay(ctx, manager)
		defer func() {
			if release != nil {
				release()
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				if signal == nil || signal.Name != loginManager+".PrepareForSleep" || len(signal.Body) != 1 {
					continue
				}
				sleeping, ok := signal.Body[0].(bool)
				if !ok {
					continue
				}
				at := time.Now().UTC()
				if sleeping {
					if release != nil {
						publishBeforeSleep(ctx, events, at, release)
						release = nil
						continue
					}
					select {
					case events <- PowerObservation{State: "sleeping", At: at}:
					case <-ctx.Done():
						return
					}
					continue
				}
				if release == nil {
					release = takeSleepDelay(ctx, manager)
				}
				select {
				case events <- PowerObservation{State: "awake", At: at}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events
}

func takeSleepDelay(ctx context.Context, manager dbus.BusObject) func() {
	ctx, cancel := context.WithTimeout(ctx, sleepSendBudget)
	defer cancel()
	var fd dbus.UnixFD
	err := manager.CallWithContext(ctx, loginManager+".Inhibit", 0,
		"sleep", "outpost", "Report the suspend event", "delay").Store(&fd)
	if err != nil {
		return nil
	}
	unix.CloseOnExec(int(fd))
	return func() { _ = unix.Close(int(fd)) }
}

func publishBeforeSleep(ctx context.Context, events chan<- PowerObservation, at time.Time, release func()) {
	defer release()
	deadline := at.Add(sleepSendBudget)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	event := PowerObservation{State: "sleeping", At: at, sendBy: deadline, completed: make(chan struct{})}
	select {
	case events <- event:
	case <-ctx.Done():
		return
	}
	select {
	case <-event.completed:
	case <-ctx.Done():
	}
}
