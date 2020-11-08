package safestop

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

//HaltHandler - a handler function that accepts a timed context and returns an error (or nil) if
//it successfully stops from signal.
type HaltHandler func(ctx context.Context) error

//SafeStop is core of the package - it handles registering and properly triggering registered
//HaltHandler functions (in the order of instantiation) upon a registered kill signal coming
//through. HaltHandler functions will use contexts that may be canceled due to timeouts
type SafeStop struct {
	fncs map[string]HaltHandler // collection of HaltHandlers Safestop will call on a signal

	lock sync.Mutex // We need to ensure that we have atomic access to fncs

	signalsChannel chan os.Signal

	options *Opts
}

//Opts is the config for a given SafeStop
type Opts struct {
	Signals []os.Signal //defaults to SIGINT and SIGTERM
	Timeout time.Duration

	//OnError is an optional function that is called whenever
	//any handler encounters an error. It will consist of the
	//given identifer for the handler (passed at registry
	//time) and the error generated.
	OnError func(identifier string, err error)

	//OnComplete is an optional function called when all
	//HaltHandlers are called. It will pass a map of each
	//registered handler and whether it errored out.
	OnComplete func(errors map[string]error)
}

//NewSafeStop instantiates a new SafeStop as per options
func NewSafeStop(opts *Opts) *SafeStop {
	ss := &SafeStop{
		fncs:           map[string]HaltHandler{},
		lock:           sync.Mutex{},
		signalsChannel: make(chan os.Signal),
		options:        opts,
	}

	//Default to SIGINT and SIGTERM
	if len(ss.options.Signals) == 0 {
		ss.options.Signals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	}

	//Await the signals to come in - this will
	//hold forever until a registered signal occurs
	go ss.awaitSignal()

	//Now that we can handle incoming signals, register for them
	ss.registerSignals()

	return ss
}

//awaitSignal is the function that handles the triggering from an
//incoming signal. It is called within the creation of a SafeStop
//and should not be called again or you will double Handler execs
func (ss *SafeStop) awaitSignal() {
	<-ss.signalsChannel

	ss.lock.Lock()
	defer ss.lock.Unlock()

	//Setup a timeout context - all functions should finish within a set time
	ctx, cancelFunc := context.WithTimeout(context.Background(), ss.options.Timeout)
	defer cancelFunc()

	errors := map[string]error{}

	//We have been triggered - begin executing handlers
	//with timeout
	for id, handler := range ss.fncs {
		err := handler(ctx)
		errors[id] = err
		if err != nil && ss.options.OnError != nil {
			ss.options.OnError(id, err)
		}
	}

	if ss.options.OnComplete != nil {
		ss.options.OnComplete(errors)
	}
}

//RegisterHandler will add the given HaltHandler for SafeStop to manage
func (ss *SafeStop) RegisterHandler(id string, fnc HaltHandler) {
	ss.lock.Lock()
	ss.fncs[id] = fnc
	ss.lock.Unlock()
}

//registerSignals internally manages registering for each signal requested
//and then calls the registered functions upon trigger
func (ss *SafeStop) registerSignals() {
	signal.Notify(ss.signalsChannel, ss.options.Signals...)
}
