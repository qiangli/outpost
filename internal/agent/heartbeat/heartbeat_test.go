package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAcknowledgementRequiresAcceptedHeartbeat(t *testing.T) {
	for _, code := range []int{200, 401, 403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/host/heartbeat" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing authenticated heartbeat")
				}
				w.WriteHeader(code)
			}))
			defer srv.Close()
			w := New(Config{CloudboxBase: srv.URL, AccessToken: "fixture-token", AgentName: "fixture", OnAcknowledged: func(context.Context) { calls++ }})
			err := w.postOne(context.Background())
			if code == 200 {
				if err != nil || calls != 1 {
					t.Fatal(err, calls)
				}
			} else if err == nil || calls != 0 {
				t.Fatal("unaccepted heartbeat acknowledged", err, calls)
			}
		})
	}
}

func TestPowerEventsTriggerImmediateHeartbeat(t *testing.T) {
	var got = make(chan Reachability, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reachability Reachability `json:"reachability"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		got <- body.Reachability
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	events := make(chan PowerObservation, 2)
	worker := New(Config{
		CloudboxBase: srv.URL, AgentName: "fixture", AccessToken: "fixture-token",
		PowerEvents:    events,
		ReachabilityFn: func() Reachability { return Reachability{LANSSH: "up"} },
	})
	go func() {
		defer close(done)
		if err := worker.Run(ctx); err != nil {
			t.Error(err)
		}
	}()
	defer func() { cancel(); <-done }()
	read := func(want string, at time.Time) {
		t.Helper()
		events <- PowerObservation{State: want, At: at}
		select {
		case observation := <-got:
			if observation.Power != want || observation.LANSSH != "up" || !observation.PowerAt.Equal(at) {
				t.Fatalf("power event %s: %+v", want, observation)
			}
		case <-time.After(time.Second):
			t.Fatalf("no immediate heartbeat for %s", want)
		}
	}
	read("sleeping", time.Now().UTC())
	read("awake", time.Now().UTC())
}

func TestHeartbeatOmitsPowerWithoutObservation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reachability map[string]any `json:"reachability"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		_, state := body.Reachability["power"]
		_, stamp := body.Reachability["power_observed_at"]
		if state || stamp || body.Reachability["lan_ssh"] != "up" {
			t.Errorf("unexpected wire health: %v", body.Reachability)
		}
	}))
	defer srv.Close()
	worker := New(Config{CloudboxBase: srv.URL, AgentName: "fixture", AccessToken: "fixture-token",
		ReachabilityFn: func() Reachability { return Reachability{LANSSH: "up"} }})
	if err := worker.postOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}
