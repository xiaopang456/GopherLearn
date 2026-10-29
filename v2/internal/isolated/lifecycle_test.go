package isolated

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type lifeWorker struct {
	name   string
	events *[]string
	fail   bool
}

func (w lifeWorker) Start(context.Context) error {
	*w.events = append(*w.events, "start:"+w.name)
	if w.fail {
		return errors.New("injected")
	}
	return nil
}
func (w lifeWorker) Stop(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded stop")
	}
	*w.events = append(*w.events, "stop:"+w.name)
	return nil
}
func TestPartialStartCleanupUsesFreshBoundedContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, l := newLifecycle(parent)
	var events []string
	l.closers = append(l.closers, func() error { events = append(events, "close:db"); return nil })
	if err := l.start(ctx, "one", lifeWorker{"one", &events, false}); err != nil {
		t.Fatal(err)
	}
	if err := l.start(ctx, "two", lifeWorker{"two", &events, true}); err == nil {
		t.Fatal("failure ignored")
	}
	cancel()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"start:one", "start:two", "stop:one", "close:db"}) {
		t.Fatal(events)
	}
}
func TestNoImplicitConfiguration(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("implicit config accepted")
	}
	if _, err := Load("missing-isolated-file.toml"); err == nil {
		t.Fatal("missing config accepted")
	}
}
