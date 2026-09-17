package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestListResult(t *testing.T) {
	boom := errors.New("403 Forbidden")
	if err := listResult(5, nil, 5); err != nil {
		t.Errorf("all read: %v", err)
	}
	if err := listResult(0, []error{boom}, 3); err != boom || IsPartial(err) {
		t.Errorf("nothing found is a failure, not a short list: %v", err)
	}
	err := listResult(40, []error{boom, boom}, 42)
	var p *PartialError
	if !errors.As(err, &p) || p.Failed != 2 || p.Of != 42 || !errors.Is(err, boom) {
		t.Errorf("some missing: %#v", err)
	}
}

// Requests to Dell leave spaced out, and a refusal holds back every request
// not yet sent, not only the one refused.
func TestPacer(t *testing.T) {
	p := &pacer{every: 20 * time.Millisecond}
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := p.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("four requests in %s, want at least 60ms", d)
	}
	p.holdOff(100 * time.Millisecond)
	start = time.Now()
	p.wait(ctx)
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Errorf("request after a refusal left after %s", d)
	}
	cancelled, cancel := context.WithCancel(ctx)
	p.holdOff(time.Hour)
	cancel()
	if err := p.wait(cancelled); err == nil {
		t.Error("a cancelled wait returned no error")
	}
}
