//go:build linux

package heartbeat

import (
	"context"
	"time"

	"github.com/godbus/dbus/v5"
)

// WatchPower subscribes to logind's suspend and resume observations when
// available. It takes no inhibitor lock and changes no power settings.
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
			dbus.WithMatchSender("org.freedesktop.login1"),
			dbus.WithMatchObjectPath("/org/freedesktop/login1"),
			dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
			dbus.WithMatchMember("PrepareForSleep")); err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				if signal == nil || signal.Name != "org.freedesktop.login1.Manager.PrepareForSleep" || len(signal.Body) != 1 {
					continue
				}
				sleeping, ok := signal.Body[0].(bool)
				if !ok {
					continue
				}
				event := PowerObservation{State: "awake", At: time.Now().UTC()}
				if sleeping {
					event.State = "sleeping"
				}
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events
}
