package clock

import (
	"testing"
	"time"
)

func TestFakeAfterFiresOnAdvance(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)

	ch := f.After(5 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired before the clock advanced")
	default:
	}

	f.Sleep(3 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired before its deadline")
	default:
	}

	f.Sleep(2 * time.Second)
	select {
	case got := <-ch:
		if !got.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("timer fired at %v, want %v", got, start.Add(5*time.Second))
		}
	default:
		t.Fatal("timer did not fire when its deadline was reached")
	}
}
