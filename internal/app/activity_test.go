package app

import (
	"reflect"
	"sync"
	"testing"
)

func TestAccountActivityTrackerEmitsOnlyStateTransitions(t *testing.T) {
	var events []string
	tracker := NewAccountActivityTracker(func(accountID string, active bool) {
		events = append(events, accountID+":"+map[bool]string{true: "active", false: "idle"}[active])
	})

	tracker.Update("account-1", true)
	tracker.Update("account-1", true)
	tracker.Update("account-1", false)
	if !tracker.Active("account-1") {
		t.Fatal("account should remain active while one request is still running")
	}
	tracker.Update("account-1", false)
	tracker.Update("account-1", false)

	want := []string{"account-1:active", "account-1:idle"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if tracker.Active("account-1") {
		t.Fatal("account should be idle after all requests finish")
	}
}

func TestAccountActivityTrackerHandlesConcurrentRequests(t *testing.T) {
	tracker := NewAccountActivityTracker(nil)
	const requests = 50
	var started sync.WaitGroup
	var release sync.WaitGroup
	started.Add(requests)
	release.Add(1)

	var workers sync.WaitGroup
	workers.Add(requests)
	for range requests {
		go func() {
			defer workers.Done()
			tracker.Update("account-1", true)
			started.Done()
			release.Wait()
			tracker.Update("account-1", false)
		}()
	}

	started.Wait()
	if !tracker.Active("account-1") {
		t.Fatal("account should be active while concurrent requests are blocked")
	}
	release.Done()
	workers.Wait()
	if tracker.Active("account-1") {
		t.Fatal("account should be idle after concurrent requests finish")
	}
}
