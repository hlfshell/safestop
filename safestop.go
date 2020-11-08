package safestop

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"time"
)

//HaltHandler - a handler function that accepts a timed context and returns an error (or nil) if
//it successfully stops from signal.
type HaltHandler func(ctx context.Context) error

//SafeStop is core of the package - it handles registering and properly triggering registered
//HaltHandler functions (in the order of instantiation) upon a registered kill signal coming
//through. HaltHandler functions will use contexts that may be canceled due to timeouts
type SafeStop struct {
	fncs []HaltHandler // collection of HaltHandlers Safestop will call on a signal

	lock sync.Mutex // We need to ensure that we have atomic access to fncs

	signalsChannel chan os.Signal

	Options *Opts
}

//Opts is the config for a given SafeStop
type Opts struct {
	Signals    []os.Signal
	Timeout    time.Duration
	OnError    func(error) //OnError is an optional function that is called whenever any handler encounters an error
	OnComplete func()      //OnComplete is an optional function called when all HaltHandlers are called
}

//NewSafeStop instantiates a new SafeStop as per options
func NewSafeStop(opts *Opts) *SafeStop {
	ss := &SafeStop{
		fncs:           []HaltHandler{},
		lock:           sync.Mutex{},
		signalsChannel: make(chan os.Signal),
		Options:        opts,
	}

	go func() {
		<-ss.signalsChannel

		ss.lock.Lock()
		defer ss.lock.Unlock()

		//Setup a timeout context - all functions should finish within a set time
		ctx, cancelFunc := context.WithTimeout(context.Background(), ss.Options.Timeout)
		defer cancelFunc()

		//We have been triggered - begin executing handlers
		//with timeout
		for _, handler := range ss.fncs {
			err := handler(ctx)
			if err != nil && ss.Options.OnError != nil {
				ss.Options.OnError(err)
			}
		}

		if ss.Options.OnComplete != nil {
			ss.Options.OnComplete()
		}
	}()

	ss.registerSignals()

	return ss
}

//RegisterHandler will add the given HaltHandler for SafeStop to manage
func (ss *SafeStop) RegisterHandler(fnc HaltHandler) {
	ss.lock.Lock()
	ss.fncs = append(ss.fncs, fnc)
	ss.lock.Unlock()
}

//registerSignals internally manages registering for each signal requested
//and then calls the registered functions upon trigger
func (ss *SafeStop) registerSignals() {
	signal.Notify(ss.signalsChannel, ss.Options.Signals...)
}
